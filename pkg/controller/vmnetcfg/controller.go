package vmnetcfg

import (
	"time"

	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

type Controller struct {
	indexer       cache.Indexer
	queue         workqueue.RateLimitingInterface
	informer      cache.Controller
	vmNetCfgCache *map[string]kihv1.VirtualMachineNetworkConfig
}

func NewController(
	queue workqueue.RateLimitingInterface,
	indexer cache.Indexer,
	informer cache.Controller,
	vmNetCfgCache *map[string]kihv1.VirtualMachineNetworkConfig,
) *Controller {
	log.Infof("(vmnetcfg.NewController) start")

	return &Controller{
		informer:      informer,
		indexer:       indexer,
		queue:         queue,
		vmNetCfgCache: vmNetCfgCache,
	}
}

func (c *Controller) processNextItem() bool {
	log.Infof("(vmnetcfg.processNextItem) start")

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
	log.Infof("(vmnetcfg.sync) start")

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
		log.Infof("(vmnetcfg.sync) event sync for VirtualMachineNetworkConfig %s", obj.(*kihv1.VirtualMachineNetworkConfig).GetName())
		log.Infof("(vmnetcfg.sync) add action found!")
		// if err := allocateIPPool(obj.(*kihv1.IPPool), kviph_clientset); err != nil {
		// 	log.Errorf("(watchIPPoolEvents) error allocating ippool: %s", err.Error())
		// }
	case UPDATE:
		log.Infof("(vmnetcfg.sync) update action found!")
	case DELETE:
		log.Infof("(vmnetcfg.sync) delete action found!")
	}

	return
}

func (c *Controller) handleErr(err error, key interface{}) {
	log.Infof("(vmnetcfg.handleErr) start")

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
	log.Infof("(vmnetcfg.runWorker) start")

	for c.processNextItem() {
	}
}
