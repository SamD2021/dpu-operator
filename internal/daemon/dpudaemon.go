package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	cni100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/go-logr/logr"
	pb2 "github.com/openshift/dpu-operator/dpu-api/gen"
	"github.com/openshift/dpu-operator/dpu-cni/pkgs/cniserver"
	"github.com/openshift/dpu-operator/dpu-cni/pkgs/cnitypes"
	"github.com/openshift/dpu-operator/dpu-cni/pkgs/networkfn"
	deviceplugin "github.com/openshift/dpu-operator/internal/daemon/device-plugin"
	"github.com/openshift/dpu-operator/internal/daemon/plugin"
	sfcreconciler "github.com/openshift/dpu-operator/internal/daemon/sfc-reconciler"
	"github.com/openshift/dpu-operator/internal/utils"
	pb "github.com/opiproject/opi-api/network/evpn-gw/v1alpha1/gen/go"
	"google.golang.org/grpc"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

type DpuDaemon struct {
	pb.UnimplementedBridgePortServiceServer
	pb2.UnimplementedDeviceServiceServer

	vsp           plugin.VendorPlugin
	dp            deviceplugin.DevicePlugin
	log           logr.Logger
	server        *grpc.Server
	cniserver     *cniserver.Server
	manager       ctrl.Manager
	macStore      map[string][]string
	wg            sync.WaitGroup
	startedWg     sync.WaitGroup
	cancelManager context.CancelFunc
	done          chan error
	config        *rest.Config
	pathManager   utils.PathManager
}

func (s *DpuDaemon) CreateBridgePort(context context.Context, bpr *pb.CreateBridgePortRequest) (*pb.BridgePort, error) {
	s.log.Info("Passing CreateBridgePort", "name", bpr.BridgePort.Name)
	return s.vsp.CreateBridgePort(bpr)
}

func (s *DpuDaemon) DeleteBridgePort(context context.Context, bpr *pb.DeleteBridgePortRequest) (*emptypb.Empty, error) {
	s.log.Info("Passing DeleteBridgePort", "name", bpr.Name)
	err := s.vsp.DeleteBridgePort(bpr)
	return &emptypb.Empty{}, err
}

func NewDpuDaemon(vsp plugin.VendorPlugin, dp deviceplugin.DevicePlugin, config *rest.Config, opts ...func(*DpuDaemon)) *DpuDaemon {
	d := &DpuDaemon{
		vsp:         vsp,
		dp:          dp,
		pathManager: *utils.NewPathManager("/"),
		log:         ctrl.Log.WithName("DpuDaemon"),
		macStore:    make(map[string][]string),
		done:        make(chan error, 5),
		config:      config,
	}

	for _, opt := range opts {
		opt(d)
	}

	return d
}

func WithPathManager(pathManager utils.PathManager) func(*DpuDaemon) {
	return func(d *DpuDaemon) {
		d.pathManager = pathManager
	}
}

func (d *DpuDaemon) cniCmdNfAddHandler(req *cnitypes.PodRequest) (*cni100.Result, error) {
	d.log.Info("cniCmdNfAddHandler")
	res, err := networkfn.CmdAdd(req)
	if err != nil {
		return nil, fmt.Errorf("SRIOV manager failed in add handler: %v", err)
	}

	d.macStore[req.Netns] = append(d.macStore[req.Netns], req.CNIConf.MAC)
	if len(d.macStore[req.Netns]) == 2 {
		d.log.Info("cniCmdNfAddHandler", "req.Netns", req.Netns)
		macs := d.macStore[req.Netns]
		d.vsp.CreateNetworkFunction(macs[0], macs[1])
	}
	d.log.Info("cniCmdNfAddHandler CmdAdd succeeded")
	return res, nil
}

func (d *DpuDaemon) cniCmdNfDelHandler(req *cnitypes.PodRequest) (*cni100.Result, error) {
	d.log.Info("cniCmdNfDelHandler")
	err := networkfn.CmdDel(req)
	if err != nil {
		return nil, errors.New("SRIOV manager failed in del handler")
	}

	macs := d.macStore[req.Netns]

	if len(macs) == 2 {
		d.log.Info("cniCmdNfDelHandler", "req.Netns", req.Netns)
		d.vsp.DeleteNetworkFunction(macs[0], macs[1])
	}

	d.macStore[req.Netns] = macs[:len(macs)-1]

	d.log.Info("cniCmdNfDelHandler CmdDel succeeded")
	return nil, nil
}

func (d *DpuDaemon) Listen() (net.Listener, error) {
	d.startedWg.Add(1)
	d.log.Info("Starting DpuDaemon")
	d.setupReconcilers()

	addr, port, err := d.vsp.Start()
	if err != nil {
		d.log.Error(err, "Failed to get addr:port from VendorPlugin")
		return nil, err
	}

	d.server = grpc.NewServer()
	pb.RegisterBridgePortServiceServer(d.server, d)

	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", addr, port))
	if err != nil {
		d.log.Error(err, "Failed to start listening on", "addr", addr, "port", port)
		return lis, err
	}
	d.log.Info("server listening", "address", lis.Addr())

	add := func(r *cnitypes.PodRequest) (*cni100.Result, error) {
		return d.cniCmdNfAddHandler(r)
	}
	del := func(r *cnitypes.PodRequest) (*cni100.Result, error) {
		return d.cniCmdNfDelHandler(r)
	}

	d.cniserver = cniserver.NewCNIServer(add, del, cniserver.WithPathManager(d.pathManager))

	return lis, err
}

func (d *DpuDaemon) ListenAndServe() error {
	listener, err := d.Listen()

	if err != nil {
		d.log.Error(err, "Failed to listen")
		return err
	}

	return d.Serve(listener)
}

func (d *DpuDaemon) Serve(listener net.Listener) error {

	d.wg.Add(1)
	go func() {
		d.log.Info("Starting OPI server")
		d.server = grpc.NewServer()
		pb.RegisterBridgePortServiceServer(d.server, d)
		if err := d.server.Serve(listener); err != nil {
			d.done <- fmt.Errorf("Failed to start serving: %v", err)
		} else {
			d.done <- nil
		}
		d.log.Info("Stopping OPI server")
		d.wg.Done()
	}()

	d.wg.Add(1)
	go func() {
		d.log.Info("Starting Device Plugin server")
		if err := d.dp.ListenAndServe(); err != nil {
			d.done <- err
		} else {
			d.done <- nil
		}
		d.log.Info("Stopping Device Plugin server")
		d.wg.Done()
	}()

	d.wg.Add(1)
	go func() {
		d.log.Info("Starting CNI server")
		if err := d.cniserver.ListenAndServe(); err != nil {
			d.done <- err
		} else {
			d.done <- nil
		}
		d.log.Info("Stopping CNI server")
		d.wg.Done()
	}()

	ctx, cancelManager := context.WithCancel(ctrl.SetupSignalHandler())
	d.wg.Add(1)
	go func() {
		d.log.Info("Starting manager")
		if err := d.manager.Start(ctx); err != nil {
			d.done <- err
		} else {
			d.done <- nil
		}
		d.log.Info("Stopping manager")
		d.wg.Done()
	}()
	d.cancelManager = cancelManager

	// Block on any go routines writing to the done channel when an error occurs or they
	// are forced to exit.
	err := <-d.done

	d.cancelManager()
	d.dp.Stop()
	d.cniserver.Shutdown(context.TODO())
	d.server.Stop()
	d.wg.Wait()
	d.startedWg.Done()
	return err
}

func (d *DpuDaemon) Stop() {
	d.done <- nil
	d.startedWg.Wait()
}

func (d *DpuDaemon) setupReconcilers() {
	if d.manager == nil {
		t := time.Duration(0)

		mgr, err := ctrl.NewManager(d.config, ctrl.Options{
			Scheme: scheme,
			NewCache: func(config *rest.Config, opts cache.Options) (cache.Cache, error) {
				opts.DefaultNamespaces = map[string]cache.Config{
					"openshift-dpu-operator": {},
				}
				return cache.New(config, opts)
			},
			// A timout needs to be specified, or else the mananger will wait indefinitely on stop()
			GracefulShutdownTimeout: &t,
		})
		if err != nil {
			d.log.Error(err, "unable to start manager")
		}

		sfcReconciler := &sfcreconciler.SfcReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}

		if err = sfcReconciler.SetupWithManager(mgr); err != nil {
			d.log.Error(err, "unable to create controller", "controller", "ServiceFunctionChain")
		}
		d.manager = mgr
	}
}
