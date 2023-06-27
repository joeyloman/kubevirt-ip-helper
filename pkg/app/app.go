package app

import (
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"

	"context"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/ippool"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vm"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vmnetcfg"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

type handler struct {
	ctx                   context.Context
	ipPoolCache           map[string]kihv1.IPPool
	vmNetCfgCache         map[string]kihv1.VirtualMachineNetworkConfig
	ippoolEventListener   *ippool.EventListener
	vmnetcfgEventListener *vmnetcfg.EventListener
	vmEventListener       *vm.EventListener
}

func Register(ctx context.Context) *handler {
	log.Infof("(app.NewEventListeners) start")

	return &handler{
		ctx: ctx,
	}
}

func (h *handler) Run(ctx context.Context) {
	log.Infof("(app.Run) start")

	// TODO: flag parse

	// temp
	homedir := os.Getenv("HOME")
	kubeconfig_file := filepath.Join(homedir, ".kube", "config")

	// initialize the pool and vm network config caches
	h.ipPoolCache = make(map[string]kihv1.IPPool)
	h.vmNetCfgCache = make(map[string]kihv1.VirtualMachineNetworkConfig)

	// initialize the ippoolEventListener handler
	h.ippoolEventListener = ippool.NewEventListener(ctx, &h.ipPoolCache, kubeconfig_file, "", nil, nil)
	if err := h.ippoolEventListener.Init(); err != nil {
		handleErr(err)
	}
	go h.ippoolEventListener.Listener()

	// initialize the vmnetcfgEventListener handler
	h.vmnetcfgEventListener = vmnetcfg.NewEventListener(ctx, &h.vmNetCfgCache, kubeconfig_file, "", nil, nil)
	if err := h.vmnetcfgEventListener.Init(); err != nil {
		handleErr(err)
	}
	go h.vmnetcfgEventListener.Listener()

	// initialize the vmEventListener handler
	h.vmEventListener = vm.NewEventListener(ctx, &h.vmNetCfgCache, kubeconfig_file, "", nil, nil)
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
