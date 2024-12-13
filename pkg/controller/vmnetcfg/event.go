package vmnetcfg

import (
	"context"

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
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

const (
	ADD    = "add"
	UPDATE = "update"
	DELETE = "delete"
)

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
	appStatus            *int
	vmnetcfgCountCurrent *int
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
	appStatus *int,
	vmnetcfgCountCurrent *int,
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
	if !util.FileExists(e.kubeConfig) {
		return rest.InClusterConfig()
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: e.kubeConfig},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: e.kubeContext},
	).ClientConfig()
}

func (e *EventHandler) EventListener() (err error) {
	log.Infof("(vmnetcfg.EventListener) starting the VirtualMachineNetworkConfig event listener")

	vmWatcher := cache.NewListWatchFromClient(e.kihClientset.KubevirtiphelperV1().RESTClient(), "virtualmachinenetworkconfigs", corev1.NamespaceAll, fields.Everything())

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(vmWatcher, &kihv1.VirtualMachineNetworkConfig{}, 0, cache.ResourceEventHandlerFuncs{
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
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(Event{
					key:    key,
					action: DELETE,
				})
			}
		},
	}, cache.Indexers{})

	controller := NewController(queue, indexer, informer, e.cache, e.ipam, e.dhcp, e.metrics, e.kihClientset, e.appStatus, e.vmnetcfgCountCurrent)
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	select {
	case <-e.ctx.Done():
		log.Infof("(vmnetcfg.EventListener) stopping the VirtualMachineNetworkConfig event listener")
		return
	}
}
