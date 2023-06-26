package vm

import (
	"time"

	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

type Controller struct {
	indexer  cache.Indexer
	queue    workqueue.RateLimitingInterface
	informer cache.Controller
}

func NewController(queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller) *Controller {
	log.Infof("(vm.NewController) start")

	return &Controller{
		informer: informer,
		indexer:  indexer,
		queue:    queue,
	}
}

func (c *Controller) processNextItem() bool {
	log.Infof("(vm.processNextItem) start")

	event, quit := c.queue.Get()
	if quit {
		return false
	}

	defer c.queue.Done(event)

	err := c.sync(event.(Event))
	c.handleErr(err, event)

	return true
}

func (c *Controller) sync(event Event) error {
	log.Infof("(vm.sync) start")

	obj, exists, err := c.indexer.GetByKey(event.key)
	if err != nil {
		log.Errorf("(vm.sync) fetching object with key %s from store failed with %v", event.key, err)

		return err
	}

	if exists {
		log.Infof("(vm.sync) event sync for VM %s", obj.(*kubevirtv1.VirtualMachine).GetName())

		switch event.action {
		case ADD:
			log.Infof("(vm.sync) add action found!")
			getNetworkDetails(obj.(*kubevirtv1.VirtualMachine))
		case UPDATE:
			log.Infof("(vm.sync) update action found!")
		case DELETE:
			log.Infof("(vm.sync) delete action found!")
		}
	} else {
		log.Infof("(vm.sync) VM %s does not exist anymore", event.key)
	}

	return nil
}

func (c *Controller) handleErr(err error, key interface{}) {
	log.Infof("(vm.handleErr) start")

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
	log.Infof("(vm.runWorker) start")

	for c.processNextItem() {
	}
}
