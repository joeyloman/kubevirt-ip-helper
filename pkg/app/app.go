package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	goipam "github.com/metal-stack/go-ipam"
	log "github.com/sirupsen/logrus"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/ippool"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vm"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vmnetcfg"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

type handler struct {
	ctx                 context.Context
	ipam                goipam.Ipamer
	ipPoolCache         map[string]kihv1.IPPool
	vmNetCfgCache       map[string]kihv1.VirtualMachineNetworkConfig
	ippoolEventListener *ippool.EventListener
	vmnetcfgHandler     *vmnetcfg.Handler
	vmEventListener     *vm.EventListener
}

func Register(ctx context.Context) *handler {
	log.Infof("(app.NewEventListeners) start")

	return &handler{
		ctx: ctx,
	}
}

func (h *handler) Run() {
	log.Infof("(app.Run) start")

	// TODO: flag parse kubeconfig and

	// temp
	homedir := os.Getenv("HOME")
	kubeconfig_file := filepath.Join(homedir, ".kube", "config")

	// initialize the ipamer
	h.ipam = goipam.New(h.ctx)

	// initialize the pool and vm network config caches
	h.ipPoolCache = make(map[string]kihv1.IPPool)
	h.vmNetCfgCache = make(map[string]kihv1.VirtualMachineNetworkConfig)

	// initialize the ippoolEventListener handler
	h.ippoolEventListener = ippool.NewEventListener(h.ctx, &h.ipam, h.ipPoolCache, kubeconfig_file, "", nil, nil)
	if err := h.ippoolEventListener.Init(); err != nil {
		handleErr(err)
	}
	go h.ippoolEventListener.Listener()

	// initialize the vmnetcfgEventListener handler
	h.vmnetcfgHandler = vmnetcfg.NewHandler(h.ctx, h.vmNetCfgCache, kubeconfig_file, "", nil, nil)
	if err := h.vmnetcfgHandler.Init(); err != nil {
		handleErr(err)
	}
	go h.vmnetcfgHandler.EventListener()

	// initialize the vmEventListener handler
	h.vmEventListener = vm.NewEventListener(h.ctx, h.vmNetCfgCache, kubeconfig_file, "", nil, nil, nil)
	if err := h.vmEventListener.Init(); err != nil {
		handleErr(err)
	}
	go h.vmEventListener.Listener()

	// TODO: replace with DHCP service
	for {
		time.Sleep(time.Second)
	}
}

func handleErr(err error) {
	log.Errorf("(app.handleErr) %s", err.Error())
}
