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

	"github.com/joeyloman/kubevirt-ip-helper/pkg/kubevirtiphelper"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/vm"

	kviphv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kviphclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
)

func watchEvents(kubevirt_kubecli kubecli.KubevirtClient, kviph_clientset *kviphclientset.Clientset, k8s_clientset *kubernetes.Clientset) {
	var watchEventTimeout int = 0 // in seconds (skip all events for the first 30 secs)

	log.Infof("(watchEvents) start watching the ippool, VirtualMachineNetworkConfig and vm events ..")

	// toggle watchEventsActivated after 10 secs
	var watchEventsActivated bool = false
	time.AfterFunc(time.Duration(watchEventTimeout)*time.Second, func() { watchEventsActivated = true })

	// do the eventwatch stuff for ippools
	watchlistIPPools := cache.NewListWatchFromClient(kviph_clientset.KubevirtiphelperV1().RESTClient(), "ippools", corev1.NamespaceAll,
		fields.Everything())

	_, controllerIPPools := cache.NewInformer(
		watchlistIPPools,
		&kviphv1.IPPool{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				log.Debugf("(watchIPPoolEvents) entering the eventwatch AddFunc ..")

				if watchEventsActivated {
					// allocate the new IPPool
					if err := kubevirtiphelper.AllocateIPPool(obj.(*kviphv1.IPPool), kviph_clientset); err != nil {
						log.Errorf("(watchIPPoolEvents) error allocating ippool: %s", err.Error())
					}
				} else {
					log.Debugf("(watchIPPoolEvents) not activated yet, object action not executed")
				}
			},
			DeleteFunc: func(obj interface{}) {
				log.Debugf("(watchIPPoolEvents) entering the eventwatch DeleteFunc ..")

				if watchEventsActivated {
					// remove the IPPool
					if err := kubevirtiphelper.RemoveIPPool(obj.(*kviphv1.IPPool), kviph_clientset); err != nil {
						log.Errorf("(watchIPPoolEvents) error removing ippool: %s", err.Error())
					}
				} else {
					log.Debugf("(watchIPPoolEvents) not activated yet, object action not executed")
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				log.Debugf("(watchIPPoolEvents) entering the eventwatch UpdateFunc ..")

				if watchEventsActivated {
					// update the IPPool
					if err := kubevirtiphelper.UpdateIPPool(oldObj.(*kviphv1.IPPool), newObj.(*kviphv1.IPPool), kviph_clientset); err != nil {
						log.Errorf("(watchIPPoolEvents) error removing ippool: %s", err.Error())
					}
				} else {
					log.Debugf("(watchIPPoolEvents) not activated yet, object action not executed")
				}
			},
		},
	)

	// do the eventwatch stuff for VirtualMachineNetworkConfigs
	watchlistVirtualMachineNetworkConfigs := cache.NewListWatchFromClient(kviph_clientset.KubevirtiphelperV1().RESTClient(), "VirtualMachineNetworkConfigs", corev1.NamespaceAll,
		fields.Everything())

	_, controllerVirtualMachineNetworkConfigs := cache.NewInformer(
		watchlistVirtualMachineNetworkConfigs,
		&kviphv1.VirtualMachineNetworkConfig{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				log.Debugf("(watchVirtualMachineNetworkConfigEvents) entering the eventwatch AddFunc ..")

				if watchEventsActivated {
					// allocate the new VirtualMachineNetworkConfig
					if err := kubevirtiphelper.AllocateVirtualMachineNetworkConfig(obj.(*kviphv1.VirtualMachineNetworkConfig), kviph_clientset); err != nil {
						log.Errorf("(watchVirtualMachineNetworkConfigEvents) error allocating VirtualMachineNetworkConfig: %s", err.Error())
					}
				} else {
					log.Debugf("(watchVirtualMachineNetworkConfigEvents) not activated yet, object action not executed")
				}
			},
			DeleteFunc: func(obj interface{}) {
				log.Debugf("(watchVirtualMachineNetworkConfigEvents) entering the eventwatch DeleteFunc ..")

				if watchEventsActivated {
					// remove the VirtualMachineNetworkConfig
					if err := kubevirtiphelper.RemoveVirtualMachineNetworkConfig(obj.(*kviphv1.VirtualMachineNetworkConfig), kviph_clientset); err != nil {
						log.Errorf("(watchVirtualMachineNetworkConfigEvents) error removing VirtualMachineNetworkConfig: %s", err.Error())
					}
				} else {
					log.Debugf("(watchVirtualMachineNetworkConfigEvents) not activated yet, object action not executed")
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				log.Debugf("(watchVirtualMachineNetworkConfigEvents) entering the eventwatch UpdateFunc ..")

				if watchEventsActivated {
					// update the VirtualMachineNetworkConfig
					if err := kubevirtiphelper.UpdateVirtualMachineNetworkConfig(oldObj.(*kviphv1.VirtualMachineNetworkConfig), newObj.(*kviphv1.VirtualMachineNetworkConfig), kviph_clientset); err != nil {
						log.Errorf("(watchVirtualMachineNetworkConfigEvents) error removing VirtualMachineNetworkConfig: %s", err.Error())
					}
				} else {
					log.Debugf("(watchVirtualMachineNetworkConfigEvents) not activated yet, object action not executed")
				}
			},
		},
	)

	// do the eventwatch stuff for configmaps so we can detect new clusters
	watchlistVirtualMachines := cache.NewListWatchFromClient(kubevirt_kubecli.RestClient(), "virtualmachines", corev1.NamespaceAll, fields.Everything())

	_, controllerVirtualMachines := cache.NewInformer(
		watchlistVirtualMachines,
		&kubevirtV1.VirtualMachine{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				log.Infof("(watchVirtualMachineEvents) entering the eventwatch AddFunc ..")
				//log.Infof("(watchVirtualMachineEvents) add VM obj: [%+v]", obj)
				if watchEventsActivated {
					vm.GetNetworkDetails(obj.(*kubevirtV1.VirtualMachine))
				} else {
					log.Debugf("(watchVirtualMachineEvents) not activated yet, object action not executed")
				}
			},
			DeleteFunc: func(obj interface{}) {
				log.Infof("(watchVirtualMachineEvents) entering the eventwatch DeleteFunc ..")
				//log.Infof("(watchVirtualMachineEvents) delete VM obj: [%+v]", obj)
				if watchEventsActivated {
					vm.GetNetworkDetails(obj.(*kubevirtV1.VirtualMachine))
				} else {
					log.Debugf("(watchVirtualMachineEvents) not activated yet, object action not executed")
				}

			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				log.Infof("(watchVirtualMachineEvents) entering the eventwatch UpdateFunc ..")
				//log.Infof("(watchVirtualMachineEvents) update VM old obj: [%+v] / new obj: [%+v]", oldObj, newObj)
				if watchEventsActivated {
					vm.GetNetworkDetails(oldObj.(*kubevirtV1.VirtualMachine))
					vm.GetNetworkDetails(newObj.(*kubevirtV1.VirtualMachine))
				} else {
					log.Debugf("(watchVirtualMachineEvents) not activated yet, object action not executed")
				}
			},
		},
	)

	stop := make(chan struct{})
	defer close(stop)
	go controllerIPPools.Run(stop)
	go controllerVirtualMachineNetworkConfigs.Run(stop)
	go controllerVirtualMachines.Run(stop)

	for {
		time.Sleep(time.Second)
	}
}
