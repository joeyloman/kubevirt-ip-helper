package ippool

import (
	"context"
	"time"

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
	log "github.com/sirupsen/logrus"
)

const (
	ADD    = "add"
	UPDATE = "update"
	DELETE = "delete"
)

// resyncPeriod re-delivers every watched object periodically: objects
// whose earlier event was dropped (a transient registration failure, or a
// network which only became live later) get another reconciliation chance
// without any external event or pod restart.
const resyncPeriod = time.Minute

type EventHandler struct {
	ctx                context.Context
	ipam               *ipam.IPAllocator
	dhcp               *dhcp.DHCPAllocator
	metrics            *metrics.MetricsAllocator
	cache              *kihcache.CacheAllocator
	kubeConfig         string
	kubeContext        string
	kubeRestConfig     *rest.Config
	kihClientset       *kihclientset.Clientset
	appStatus          *int
	ippoolCountCurrent *int
}

type Event struct {
	key                string
	action             string
	poolName           string
	poolNetworkName    string
	oldPoolNetworkName string
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
	ippoolCountCurrent *int,
) *EventHandler {
	return &EventHandler{
		ctx:                ctx,
		ipam:               ipam,
		dhcp:               dhcp,
		metrics:            metrics,
		cache:              cache,
		kubeConfig:         kubeConfig,
		kubeContext:        kubeContext,
		kubeRestConfig:     kubeRestConfig,
		kihClientset:       kihClientset,
		appStatus:          appStatus,
		ippoolCountCurrent: ippoolCountCurrent,
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
	log.Infof("(ippool.EventListener) starting the IPPool event listener")

	vmWatcher := cache.NewListWatchFromClient(e.kihClientset.KubevirtiphelperV1().RESTClient(), "ippools", corev1.NamespaceAll, fields.Everything())

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(vmWatcher, &kihv1.IPPool{}, resyncPeriod, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(Event{
					key:             key,
					action:          ADD,
					poolName:        obj.(*kihv1.IPPool).ObjectMeta.Name,
					poolNetworkName: obj.(*kihv1.IPPool).Spec.NetworkName,
				})
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(Event{
					key:                key,
					action:             UPDATE,
					poolName:           new.(*kihv1.IPPool).ObjectMeta.Name,
					poolNetworkName:    new.(*kihv1.IPPool).Spec.NetworkName,
					oldPoolNetworkName: old.(*kihv1.IPPool).Spec.NetworkName,
				})
			}
		},
		DeleteFunc: func(obj interface{}) {
			pool, isPool := util.UnwrapTombstone(obj).(*kihv1.IPPool)
			if !isPool {
				return
			}

			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pool)
			if err == nil {
				queue.Add(Event{
					key:             key,
					action:          DELETE,
					poolName:        pool.ObjectMeta.Name,
					poolNetworkName: pool.Spec.NetworkName,
				})
			}
		},
	}, cache.Indexers{})

	controller := NewController(queue, indexer, informer, e.ctx, e.cache, e.ipam, e.dhcp, e.metrics, e.kihClientset, e.appStatus, e.ippoolCountCurrent)
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	select {
	case <-e.ctx.Done():
		log.Infof("(ippool.EventListener) stopping the IPPool event listener")
		return
	}
}
