package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/ippool"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vm"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vmnetcfg"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

type handler struct {
	ctx                  context.Context
	ipam                 *ipam.IPAllocator
	dhcp                 *dhcp.DHCPAllocator
	ipPoolCache          map[string]kihv1.IPPool
	ippoolEventHandler   *ippool.EventHandler
	vmnetcfgEventHandler *vmnetcfg.EventHandler
	vmEventHandler       *vm.EventHandler
}

func Register(ctx context.Context) *handler {
	//log.Infof("(app.NewEventListeners) start")

	return &handler{
		ctx: ctx,
	}
}

func (h *handler) Run() {
	//log.Infof("(app.Run) start")

	// TODO: flag parse kubeconfig and context

	// temp
	homedir := os.Getenv("HOME")
	kubeconfig_file := filepath.Join(homedir, ".kube", "config")

	// initialize the ipam service
	h.ipam = ipam.New()

	// initialize the dhcp service
	h.dhcp = dhcp.New()

	// initialize the pool cache
	h.ipPoolCache = make(map[string]kihv1.IPPool)

	// initialize the ippoolEventListener handler
	h.ippoolEventHandler = ippool.NewEventHandler(
		h.ctx,
		h.ipam,
		h.ipPoolCache,
		kubeconfig_file,
		"",
		nil,
		nil,
	)
	if err := h.ippoolEventHandler.Init(); err != nil {
		handleErr(err)
	}
	go h.ippoolEventHandler.EventListener()

	// initialize the vmnetcfgEventListener handler
	h.vmnetcfgEventHandler = vmnetcfg.NewEventHandler(
		h.ctx,
		h.ipam,
		h.dhcp,
		h.ipPoolCache,
		kubeconfig_file,
		"",
		nil,
		nil,
	)
	if err := h.vmnetcfgEventHandler.Init(); err != nil {
		handleErr(err)
	}
	go h.vmnetcfgEventHandler.EventListener()

	// initialize the vmEventListener handler
	h.vmEventHandler = vm.NewEventHandler(
		h.ctx,
		h.dhcp,
		kubeconfig_file,
		"",
		nil,
		nil,
		nil,
	)
	if err := h.vmEventHandler.Init(); err != nil {
		handleErr(err)
	}
	go h.vmEventHandler.EventListener()

	// TODO: pass interfaces
	h.dhcp.Run()

	// TODO: replace with DHCP service
	for {
		time.Sleep(time.Second)
	}
}

func handleErr(err error) {
	log.Errorf("(app.handleErr) %s", err.Error())
}
