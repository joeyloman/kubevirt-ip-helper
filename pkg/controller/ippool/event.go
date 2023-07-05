package ippool

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	log "github.com/sirupsen/logrus"
)

const (
	ADD    = "add"
	UPDATE = "update"
	DELETE = "delete"
)

type EventHandler struct {
	ctx            context.Context
	ipam           *ipam.IPAllocator
	ipPoolCache    map[string]kihv1.IPPool
	kubeConfig     string
	kubeContext    string
	kubeRestConfig *rest.Config
	kihClientset   *kihclientset.Clientset
}

type Event struct {
	key    string
	action string
}

func NewEventHandler(
	ctx context.Context,
	ipam *ipam.IPAllocator,
	ipPoolCache map[string]kihv1.IPPool,
	kubeConfig string,
	kubeContext string,
	kubeRestConfig *rest.Config,
	kihClientset *kihclientset.Clientset,
) *EventHandler {
	//log.Infof("(ippool.NewEventHandler) start")

	return &EventHandler{
		ctx:            ctx,
		ipam:           ipam,
		ipPoolCache:    ipPoolCache,
		kubeConfig:     kubeConfig,
		kubeContext:    kubeContext,
		kubeRestConfig: kubeRestConfig,
		kihClientset:   kihClientset,
	}
}

func (e *EventHandler) Init() (err error) {
	//log.Infof("(ippool.Init) start")

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
	//log.Infof("(ippool.getKubeConfig) start")

	if e.kubeConfig == "" {
		return rest.InClusterConfig()
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: e.kubeConfig},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: e.kubeContext},
	).ClientConfig()
}

func (e *EventHandler) EventListener() (err error) {
	log.Infof("(ippool.EventListener) starting IPPool event listener")

	vmWatcher := cache.NewListWatchFromClient(e.kihClientset.KubevirtiphelperV1().RESTClient(), "ippools", corev1.NamespaceAll, fields.Everything())

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(vmWatcher, &kihv1.IPPool{}, 0, cache.ResourceEventHandlerFuncs{
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

	controller := NewController(queue, indexer, informer, e.ipPoolCache, e.ipam, e.kihClientset)
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	select {}
}
