package app

import (
	"time"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	log "github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"

	kubevirtV1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
)

func watchEvents(kubevirt_kubecli kubecli.KubevirtClient, k8s_clientset *kubernetes.Clientset) {
	log.Infof("(watchEvents) start watching the vm events ..")

	// do the eventwatch stuff for configmaps so we can detect new clusters
	//watchlistVirtualMachines := cache.NewListWatchFromClient(k8s_clientset.CoreV1().RESTClient(), "virtualmachines", corev1.NamespaceAll, fields.Everything())
	watchlistVirtualMachines := cache.NewListWatchFromClient(kubevirt_kubecli.RestClient(), "virtualmachines", corev1.NamespaceAll, fields.Everything())

	//bla := kubevirt_kubecli.VirtualMachine(corev1.NamespaceAll)

	_, controllerVirtualMachines := cache.NewInformer(
		watchlistVirtualMachines,
		&kubevirtV1.VirtualMachine{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				log.Infof("(watchVirtualMachineEvents) entering the eventwatch AddFunc ..")
				log.Infof("(watchVirtualMachineEvents) add VM obj: [%+v]", obj)

			},
			DeleteFunc: func(obj interface{}) {
				log.Infof("(watchVirtualMachineEvents) entering the eventwatch DeleteFunc ..")
				log.Infof("(watchVirtualMachineEvents) delete VM obj: [%+v]", obj)

			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				log.Infof("(watchVirtualMachineEvents) entering the eventwatch UpdateFunc ..")
				log.Infof("(watchVirtualMachineEvents) update VM old obj: [%+v] / new obj:", oldObj, newObj)
			},
		},
	)

	stop := make(chan struct{})
	defer close(stop)
	go controllerVirtualMachines.Run(stop)

	for {
		time.Sleep(time.Second)
	}
}
