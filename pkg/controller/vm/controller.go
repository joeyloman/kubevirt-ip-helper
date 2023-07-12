package vm

import (
	"time"

	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
)

type Controller struct {
	indexer      cache.Indexer
	queue        workqueue.RateLimitingInterface
	informer     cache.Controller
	dhcp         *dhcp.DHCPAllocator
	kihClientset *kihclientset.Clientset
}

func NewController(
	queue workqueue.RateLimitingInterface,
	indexer cache.Indexer,
	informer cache.Controller,
	dhcp *dhcp.DHCPAllocator,
	kihClientset *kihclientset.Clientset,
) *Controller {
	//log.Infof("(vm.NewController) start")

	return &Controller{
		informer:     informer,
		indexer:      indexer,
		queue:        queue,
		dhcp:         dhcp,
		kihClientset: kihClientset,
	}
}

func (c *Controller) processNextItem() bool {
	//log.Infof("(vm.processNextItem) start")

	event, quit := c.queue.Get()
	if quit {
		return false
	}

	defer c.queue.Done(event)

	err := c.sync(event.(Event))
	c.handleErr(err, event)

	return true
}

func (c *Controller) sync(event Event) (err error) {
	//log.Infof("(vm.sync) start")

	obj, exists, err := c.indexer.GetByKey(event.key)
	if err != nil {
		log.Errorf("(vm.sync) fetching object with key %s from store failed with %v", event.key, err)

		return
	}

	if !exists && event.action != DELETE {
		log.Infof("(vm.sync) VM %s does not exist anymore", event.key)

		return
	}

	//log.Infof("(vm.sync) event sync for VirtualMachine %s", obj.(*kubevirtv1.VirtualMachine).GetName())
	log.Infof("(vm.sync) processing event for VirtualMachine [%s/%s]", event.vmNamespace, event.vmName)

	switch event.action {
	case ADD:
		log.Infof("(vm.sync) add action found!")

		err := c.createVirtualMachineNetworkConfigObject(obj.(*kubevirtv1.VirtualMachine))
		if err != nil {
			log.Errorf("(vm.sync) %s", err)
		}
		c.dhcp.Usage()
	case UPDATE:
		log.Infof("(vm.sync) update action found!")

		err := c.updateVirtualMachineNetworkConfigObject(obj.(*kubevirtv1.VirtualMachine))
		if err != nil {
			log.Errorf("(vm.sync) %s", err)
		}
		c.dhcp.Usage()
	case DELETE:
		log.Infof("(vm.sync) delete action found!")

		err := c.deleteVirtualMachineNetworkConfigObject(event.vmName, event.vmNamespace)
		if err != nil {
			log.Errorf("(vm.sync) %s", err)
		}
		c.dhcp.Usage()
	}

	return
}

func (c *Controller) handleErr(err error, key interface{}) {
	//log.Infof("(vm.handleErr) start")

	if err == nil {
		c.queue.Forget(key)

		return
	}

	if c.queue.NumRequeues(key) < 5 {
		log.Errorf("(vm.handleErr) syncing VirtualMachine %v: %v", key, err)

		c.queue.AddRateLimited(key)

		return
	}

	c.queue.Forget(key)

	log.Infof("(vm.handleErr) dropping VirtualMachine %q out of the queue: %v", key, err)
}

func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	defer c.queue.ShutDown()
	log.Infof("(vm.Run) starting VirtualMachine controller")

	go c.informer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		log.Errorf("Timed out waiting for caches to sync")

		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	log.Infof("(vm.runWorker) stopping VirtualMachine controller")
}

func (c *Controller) runWorker() {
	//log.Infof("(vm.runWorker) start")

	for c.processNextItem() {
	}
}
