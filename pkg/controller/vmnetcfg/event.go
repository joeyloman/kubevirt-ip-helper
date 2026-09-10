package vmnetcfg

import (
	"context"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ippoolstatus"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

const (
	// the add/delete tokens are the ledger mutations of ippoolstatus; the
	// aliases keep the event values and the status-write switch from
	// drifting apart
	ADD    = ippoolstatus.EventAdd
	UPDATE = "update"
	DELETE = ippoolstatus.EventDelete
)

// resyncPeriod re-delivers every watched object periodically: objects
// whose earlier event was dropped (a transient sync failure, or a network
// which only became live later) get another reconciliation chance without
// any external event or pod restart.
const resyncPeriod = time.Minute

type EventHandler struct {
	ctx                  context.Context
	ipam                 *ipam.IPAllocator
	dhcp                 *dhcp.DHCPAllocator
	metrics              *metrics.MetricsAllocator
	cache                *kihcache.CacheAllocator
	kubeConfig           string
	kubeContext          string
	kubeRestConfig       *rest.Config
	kihClientset         *kihclientset.Clientset
	appStatus            *atomic.Int32
	vmnetcfgCountCurrent *atomic.Int32
}

type Event struct {
	key    string
	action string
}

func NewEventHandler(
	ctx context.Context,
	ipam *ipam.IPAllocator,
	dhcp *dhcp.DHCPAllocator,
	metrics *metrics.MetricsAllocator,
	cache *kihcache.CacheAllocator,
	kubeConfig string,
	kubeContext string,
	kubeRestConfig *rest.Config,
	kihClientset *kihclientset.Clientset,
	appStatus *atomic.Int32,
	vmnetcfgCountCurrent *atomic.Int32,
) *EventHandler {
	return &EventHandler{
		ctx:                  ctx,
		ipam:                 ipam,
		dhcp:                 dhcp,
		metrics:              metrics,
		cache:                cache,
		kubeConfig:           kubeConfig,
		kubeContext:          kubeContext,
		kubeRestConfig:       kubeRestConfig,
		kihClientset:         kihClientset,
		appStatus:            appStatus,
		vmnetcfgCountCurrent: vmnetcfgCountCurrent,
	}
}

func (e *EventHandler) Init() (err error) {
	e.kubeRestConfig, err = e.getKubeConfig()
	if err != nil {
		return
	}

	e.kihClientset, err = kihclientset.NewForConfig(e.kubeRestConfig)
	if err != nil {
		return
	}

	return
}

func (e *EventHandler) getKubeConfig() (config *rest.Config, err error) {
	// bound every controller api call: a hang against the api must not
	// wedge the reconcilers behind an unresponsive transport
	const configTimeout = 30 * time.Second

	if !util.FileExists(e.kubeConfig) {
		if config, err = rest.InClusterConfig(); err != nil {
			return
		}
		config.Timeout = configTimeout

		return
	}

	config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: e.kubeConfig},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: e.kubeContext},
	).ClientConfig()
	if err == nil {
		config.Timeout = configTimeout
	}

	return
}

func (e *EventHandler) EventListener() (err error) {
	log.Infof("(vmnetcfg.EventListener) starting the VirtualMachineNetworkConfig event listener")

	vmWatcher := cache.NewListWatchFromClient(e.kihClientset.KubevirtiphelperV1().RESTClient(), "virtualmachinenetworkconfigs", corev1.NamespaceAll, fields.Everything())

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(vmWatcher, &kihv1.VirtualMachineNetworkConfig{}, resyncPeriod, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(Event{
					key:    key,
					action: ADD,
				})
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(Event{
					key:    key,
					action: UPDATE,
				})
			}
		},
		DeleteFunc: func(obj interface{}) {
			vmnetcfg, isVMNetCfg := util.UnwrapTombstone(obj).(*kihv1.VirtualMachineNetworkConfig)
			if !isVMNetCfg {
				return
			}

			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(vmnetcfg)
			if err == nil {
				queue.Add(Event{
					key:    key,
					action: DELETE,
				})
			}
		},
	}, cache.Indexers{})

	controller := NewController(e.ctx, queue, indexer, informer, e.cache, e.ipam, e.dhcp, e.metrics, e.kihClientset, e.appStatus, e.vmnetcfgCountCurrent)
	stop := make(chan struct{})

	// join the controller on shutdown: EventListener only returns after
	// Controller.Run has fully stopped (its worker has drained the queue), so
	// the application restart flow can wait for the old generation to be gone
	done := make(chan struct{})
	go func() {
		controller.Run(1, stop)
		close(done)
	}()

	select {
	case <-e.ctx.Done():
		log.Infof("(vmnetcfg.EventListener) stopping the VirtualMachineNetworkConfig event listener")
		close(stop)
		<-done
		return
	}
}
