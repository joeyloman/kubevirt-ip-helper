package vm

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

	kubevirtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

const (
	ADD    = "add"
	UPDATE = "update"
	DELETE = "delete"
)

type EventListener struct {
	ctx            context.Context
	vmNetCfgCache  *map[string]kihv1.VirtualMachineNetworkConfig
	kubeConfig     string
	kubeContext    string
	kubeRestConfig *rest.Config
	kcli           kubecli.KubevirtClient
}

type Event struct {
	key       string
	action    string
	vmname    string
	namespace string
}

func NewEventListener(
	ctx context.Context,
	vmNetCfgCache *map[string]kihv1.VirtualMachineNetworkConfig,
	kubeConfig string,
	kubeContext string,
	kubeRestConfig *rest.Config,
	kcli kubecli.KubevirtClient,
) *EventListener {
	log.Infof("(vm.NewEventListener) start")

	return &EventListener{
		ctx:            ctx,
		vmNetCfgCache:  vmNetCfgCache,
		kubeConfig:     kubeConfig,
		kubeContext:    kubeContext,
		kubeRestConfig: kubeRestConfig,
		kcli:           kcli,
	}
}

func (e *EventListener) Init() (err error) {
	log.Infof("(vm.Init) start")

	e.kubeRestConfig, err = e.getKubeConfig()
	if err != nil {
		return
	}

	e.kcli, err = kubecli.GetKubevirtClientFromRESTConfig(e.kubeRestConfig)
	if err != nil {
		return
	}

	return
}

func (e *EventListener) getKubeConfig() (config *rest.Config, err error) {
	log.Infof("(vm.getKubeConfig) start")

	if e.kubeConfig == "" {
		return rest.InClusterConfig()
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: e.kubeConfig},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: e.kubeContext},
	).ClientConfig()
}

func (e *EventListener) Listener() (err error) {
	log.Infof("(vm.Listener) start")

	vmWatcher := cache.NewListWatchFromClient(e.kcli.RestClient(), "virtualmachines", corev1.NamespaceAll, fields.Everything())

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(vmWatcher, &kubevirtv1.VirtualMachine{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(Event{
					key:       key,
					action:    ADD,
					vmname:    obj.(*kubevirtv1.VirtualMachine).GetName(),
					namespace: obj.(*kubevirtv1.VirtualMachine).GetNamespace(),
				})
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(Event{
					key:       key,
					action:    UPDATE,
					vmname:    new.(*kubevirtv1.VirtualMachine).GetName(),
					namespace: new.(*kubevirtv1.VirtualMachine).GetNamespace(),
				})
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(Event{
					key:       key,
					action:    DELETE,
					vmname:    obj.(*kubevirtv1.VirtualMachine).GetName(),
					namespace: obj.(*kubevirtv1.VirtualMachine).GetNamespace(),
				})
			}
		},
	}, cache.Indexers{})

	controller := NewController(queue, indexer, informer, e.vmNetCfgCache)
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	select {}
}
