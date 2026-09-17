package ippool

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/gate"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	"kubevirt.io/client-go/kubecli"

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
	ctx            context.Context
	ipam           *ipam.IPAllocator
	dhcp           *dhcp.DHCPAllocator
	metrics        *metrics.MetricsAllocator
	cache          *kihcache.CacheAllocator
	kubeConfig     string
	kubeContext    string
	kubeRestConfig *rest.Config
	kihClientset   *kihclientset.Clientset
	kcli           kubecli.KubevirtClient
	appStatus      *atomic.Int32
	startupGate    *gate.Gate
	scope          util.NetworkScope
}

type Event struct {
	key             string
	action          string
	poolName        string
	poolNetworkName string
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
	startupGate *gate.Gate,
	scope util.NetworkScope,
) *EventHandler {
	return &EventHandler{
		ctx:            ctx,
		ipam:           ipam,
		dhcp:           dhcp,
		metrics:        metrics,
		cache:          cache,
		kubeConfig:     kubeConfig,
		kubeContext:    kubeContext,
		kubeRestConfig: kubeRestConfig,
		kihClientset:   kihClientset,
		appStatus:      appStatus,
		startupGate:    startupGate,
		scope:          scope,
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

	// the kubevirt client serves the one-shot VirtualMachine existence
	// checks of the ledger revalidation (the 30s bound of the config
	// stays: unlike the informer clients there is no watch connection
	// whose long-poll a timeout would tear down)
	e.kcli, err = kubecli.GetKubevirtClientFromRESTConfig(e.kubeRestConfig)
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
	if e.scope.NetworkName() == "" {
		return fmt.Errorf("IPPool discovery requires a network scope")
	}
	log.Infof("(ippool.EventListener) starting the IPPool event listener")

	poolWatcher := cache.NewFilteredListWatchFromClient(e.kihClientset.KubevirtiphelperV1().RESTClient(), "ippools", corev1.NamespaceAll, func(options *metav1.ListOptions) {
		options.LabelSelector = e.scope.Selector()
	})

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(poolWatcher, &kihv1.IPPool{}, resyncPeriod, cache.ResourceEventHandlerFuncs{
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
					key:             key,
					action:          UPDATE,
					poolName:        new.(*kihv1.IPPool).ObjectMeta.Name,
					poolNetworkName: new.(*kihv1.IPPool).Spec.NetworkName,
				})
			}
		},
		DeleteFunc: func(obj interface{}) {
			// Tombstones need only their key: the fresh existence read in
			// sync determines whether the object was deleted or deselected.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				_, name, splitErr := cache.SplitMetaNamespaceKey(key)
				if splitErr != nil {
					return
				}
				queue.Add(Event{
					key:      key,
					action:   DELETE,
					poolName: name,
				})
			}
		},
	}, cache.Indexers{})

	// the ledger revalidation of the claim protection verifies the
	// VirtualMachine of a claim whose vmnetcfg is gone through the
	// kubevirt api: only a vm which is gone as well is the authoritative
	// absence which may drop the durable record (a live vm reconstructs
	// its vmnetcfg, so its claim stays protected)
	verifyVM := func(namespace string, name string) (bool, error) {
		_, err := e.kcli.VirtualMachine(namespace).Get(name, &metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}

			return false, err
		}

		return true, nil
	}

	controller := NewController(queue, indexer, informer, e.ctx, e.cache, e.ipam, e.dhcp, e.metrics, e.kihClientset, e.appStatus, e.startupGate, verifyVM, e.scope)
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
		log.Infof("(ippool.EventListener) stopping the IPPool event listener")
		close(stop)
		<-done
		return
	}
}
