package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/ippool"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vm"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/vmnetcfg"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
)

const (
	APP_INIT    = 0
	APP_RUNNING = 1
	APP_RESTART = 2
)

type handler struct {
	ctx                  context.Context
	kubeConfigFile       string
	kubeContext          string
	ipam                 *ipam.IPAllocator
	dhcp                 *dhcp.DHCPAllocator
	cache                *cache.CacheAllocator
	metrics              *metrics.MetricsAllocator
	ippoolEventHandler   *ippool.EventHandler
	vmnetcfgEventHandler *vmnetcfg.EventHandler
	vmEventHandler       *vm.EventHandler
	appStatus            int
}

func Register() *handler {
	return &handler{}
}

func (h *handler) Init() {
	h.kubeConfigFile = os.Getenv("KUBECONFIG")
	if h.kubeConfigFile == "" {
		homedir := os.Getenv("HOME")
		h.kubeConfigFile = filepath.Join(homedir, ".kube", "config")
	}

	h.kubeContext = os.Getenv("KUBECONTEXT")

	h.appStatus = APP_INIT
}

func (h *handler) Run() {
	var ctx context.Context
	var cancel context.CancelFunc

	ctx, cancel = context.WithCancel(context.Background())
	h.RunServices(ctx)
	h.appStatus = APP_RUNNING

	// keep the main thread alive
	for {
		time.Sleep(time.Second)
		if h.appStatus == APP_RESTART {
			cancel()
			h.metrics.Stop()

			time.Sleep(time.Second * 10)

			h.appStatus = APP_INIT
			ctx, cancel = context.WithCancel(context.Background())
			h.RunServices(ctx)
			h.appStatus = APP_RUNNING
		}
	}
}

func (h *handler) RunServices(ctx context.Context) {
	// register the new context
	h.ctx = ctx

	// initialize the ipam service
	h.ipam = ipam.New()

	// initialize the dhcp service
	h.dhcp = dhcp.New()

	// initialize the metrics service
	h.metrics = metrics.New()
	go h.metrics.Run()

	// initialize the pool cache
	h.cache = cache.New()

	// initialize the ippoolEventListener handler
	h.ippoolEventHandler = ippool.NewEventHandler(
		h.ctx,
		h.ipam,
		h.dhcp,
		h.metrics,
		h.cache,
		h.kubeConfigFile,
		h.kubeContext,
		nil,
		nil,
		&h.appStatus,
	)
	if err := h.ippoolEventHandler.Init(); err != nil {
		handleErr(err)
	}
	go h.ippoolEventHandler.EventListener()

	// give the ippool handler some time to gather all the pools and register the ipam subnets
	time.Sleep(time.Second * 10)

	// initialize the vmnetcfgEventListener handler
	h.vmnetcfgEventHandler = vmnetcfg.NewEventHandler(
		h.ctx,
		h.ipam,
		h.dhcp,
		h.metrics,
		h.cache,
		h.kubeConfigFile,
		h.kubeContext,
		nil,
		nil,
	)
	if err := h.vmnetcfgEventHandler.Init(); err != nil {
		handleErr(err)
	}
	go h.vmnetcfgEventHandler.EventListener()

	// give the vmnetcfg handler some time to settle before collecting all the vms
	time.Sleep(time.Second * 30)

	// initialize the vmEventListener handler
	h.vmEventHandler = vm.NewEventHandler(
		h.ctx,
		h.ipam,
		h.dhcp,
		h.cache,
		h.kubeConfigFile,
		h.kubeContext,
		nil,
		nil,
		nil,
	)
	if err := h.vmEventHandler.Init(); err != nil {
		handleErr(err)
	}
	go h.vmEventHandler.EventListener()
}

func (h *handler) Stop() {

}

func handleErr(err error) {
	log.Panicf("(app.handleErr) %s", err.Error())
}
