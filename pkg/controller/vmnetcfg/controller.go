package vmnetcfg

import (
	"time"

	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
)

type Controller struct {
	indexer     cache.Indexer
	queue       workqueue.RateLimitingInterface
	informer    cache.Controller
	ipam        *ipam.IPAllocator
	dhcp        *dhcp.DHCPAllocator
	ipPoolCache map[string]kihv1.IPPool
}

func NewController(
	queue workqueue.RateLimitingInterface,
	indexer cache.Indexer,
	informer cache.Controller,
	ipam *ipam.IPAllocator,
	dhcp *dhcp.DHCPAllocator,
	ipPoolCache map[string]kihv1.IPPool,
) *Controller {
	//log.Infof("(vmnetcfg.NewController) start")

	return &Controller{
		informer:    informer,
		indexer:     indexer,
		queue:       queue,
		ipam:        ipam,
		dhcp:        dhcp,
		ipPoolCache: ipPoolCache,
	}
}

func (c *Controller) processNextItem() bool {
	//log.Infof("(vmnetcfg.processNextItem) start")

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
	//log.Infof("(vmnetcfg.sync) start")

	obj, exists, err := c.indexer.GetByKey(event.key)
	if err != nil {
		log.Errorf("(vmnetcfg.sync) fetching object with key %s from store failed with %v", event.key, err)

		return
	}

	if !exists && event.action != DELETE {
		log.Infof("(vmnetcfg.sync) VirtualMachineNetworkConfig %s does not exist anymore", event.key)

		return
	}

	switch event.action {
	case ADD:
		log.Infof("(vmnetcfg.sync) add action found!")
		err := c.registerVirtualMachineNetworkConfig(obj.(*kihv1.VirtualMachineNetworkConfig))
		if err != nil {
			log.Errorf("(vmnetcfg.sync) failed to allocate new vmnetcfg for %s: %s", obj.(*kihv1.VirtualMachineNetworkConfig).GetName(), err.Error())
		}
		c.dhcp.Usage()
	case UPDATE:
		log.Infof("(vmnetcfg.sync) update action found!")
	case DELETE:
		log.Infof("(vmnetcfg.sync) delete action found!")
		c.dhcp.Usage()
	}

	return
}

func (c *Controller) handleErr(err error, key interface{}) {
	//log.Infof("(vmnetcfg.handleErr) start")

	if err == nil {
		c.queue.Forget(key)

		return
	}

	if c.queue.NumRequeues(key) < 5 {
		log.Errorf("(vmnetcfg.handleErr) syncing VirtualMachineNetworkConfig %v: %v", key, err)

		c.queue.AddRateLimited(key)

		return
	}

	c.queue.Forget(key)

	log.Infof("(vmnetcfg.handleErr) dropping VirtualMachineNetworkConfig %q out of the queue: %v", key, err)
}

func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	defer c.queue.ShutDown()
	log.Infof("(vmnetcfg.Run) starting VirtualMachineNetworkConfig controller")

	go c.informer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		log.Errorf("(vmnetcfg.Run) timed out waiting for caches to sync")

		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	log.Infof("(vmnetcfg.Run) stopping VirtualMachineNetworkConfig controller")
}

func (c *Controller) runWorker() {
	//log.Infof("(vmnetcfg.runWorker) start")

	for c.processNextItem() {
	}
}
