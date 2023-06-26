package ippool

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
	indexer  cache.Indexer
	queue    workqueue.RateLimitingInterface
	informer cache.Controller
}

func NewController(queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller) *Controller {
	log.Infof("(ippool.NewController) start")

	return &Controller{
		informer: informer,
		indexer:  indexer,
		queue:    queue,
	}
}

func (c *Controller) processNextItem() bool {
	log.Infof("(ippool.processNextItem) start")

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
	log.Infof("(ippool.sync) start")

	obj, exists, err := c.indexer.GetByKey(event.key)
	if err != nil {
		log.Errorf("(ippool.sync) fetching object with key %s from store failed with %v", event.key, err)

		return err
	}

	if exists {
		log.Infof("(ippool.sync) event sync for IPPool %s", obj.(*kihv1.IPPool).GetName())

		switch event.action {
		case ADD:
			log.Infof("(ippool.sync) add action found!")
			// if err := allocateIPPool(obj.(*kihv1.IPPool), kviph_clientset); err != nil {
			// 	log.Errorf("(watchIPPoolEvents) error allocating ippool: %s", err.Error())
			// }
		case UPDATE:
			log.Infof("(ippool.sync) update action found!")
		case DELETE:
			log.Infof("(ippool.sync) delete action found!")
		}
	} else {
		log.Infof("(ippool.sync) VM %s does not exist anymore", event.key)
	}

	return nil
}

func (c *Controller) handleErr(err error, key interface{}) {
	log.Infof("(ippool.handleErr) start")

	if err == nil {
		c.queue.Forget(key)

		return
	}

	if c.queue.NumRequeues(key) < 5 {
		log.Errorf("(ippool.handleErr) syncing IPPool %v: %v", key, err)

		c.queue.AddRateLimited(key)

		return
	}

	c.queue.Forget(key)

	log.Infof("(ippool.handleErr) dropping IPPool %q out of the queue: %v", key, err)
}

func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	defer c.queue.ShutDown()
	log.Infof("(ippool.Run) starting IPPool controller")

	go c.informer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		log.Errorf("(ippool.Run) timed out waiting for caches to sync")

		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	log.Infof("(ippool.Run) stopping IPPool controller")
}

func (c *Controller) runWorker() {
	log.Infof("(ippool.runWorker) start")

	for c.processNextItem() {
	}
}
