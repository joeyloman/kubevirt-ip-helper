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
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
)

const (
	ADD    = "add"
	UPDATE = "update"
	DELETE = "delete"
)

type Handler struct {
	ctx            context.Context
	vmNetCfgCache  map[string]kihv1.VirtualMachineNetworkConfig
	kubeConfig     string
	kubeContext    string
	kubeRestConfig *rest.Config
	kihClientset   *kihclientset.Clientset
}

type Event struct {
	key    string
	action string
}

func NewHandler(
	ctx context.Context,
	vmNetCfgCache map[string]kihv1.VirtualMachineNetworkConfig,
	kubeConfig string,
	kubeContext string,
	kubeRestConfig *rest.Config,
	kihClientset *kihclientset.Clientset,
) *Handler {
	log.Infof("(vmnetcfg.NewHandler) start")

	return &Handler{
		ctx:            ctx,
		vmNetCfgCache:  vmNetCfgCache,
		kubeConfig:     kubeConfig,
		kubeContext:    kubeContext,
		kubeRestConfig: kubeRestConfig,
		kihClientset:   kihClientset,
	}
}

func (h *Handler) Init() (err error) {
	log.Infof("(vmnetcfg.Init) start")

	h.kubeRestConfig, err = h.getKubeConfig()
	if err != nil {
		return
	}

	h.kihClientset, err = kihclientset.NewForConfig(h.kubeRestConfig)
	if err != nil {
		return
	}

	return
}

func (h *Handler) getKubeConfig() (config *rest.Config, err error) {
	log.Infof("(vmnetcfg.getKubeConfig) start")

	if h.kubeConfig == "" {
		return rest.InClusterConfig()
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: h.kubeConfig},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: h.kubeContext},
	).ClientConfig()
}

func (h *Handler) EventListener() (err error) {
	log.Infof("(vmnetcfg.EventListener) start")

	vmWatcher := cache.NewListWatchFromClient(h.kihClientset.KubevirtiphelperV1().RESTClient(), "virtualmachinenetworkconfigs", corev1.NamespaceAll, fields.Everything())

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

	controller := NewController(queue, indexer, informer, h.vmNetCfgCache)
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	select {}
}
