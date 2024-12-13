package ippool

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
)

const (
	APP_INIT    = 0
	APP_RUNNING = 1
	APP_RESTART = 2
)

type Controller struct {
	indexer            cache.Indexer
	queue              workqueue.RateLimitingInterface
	informer           cache.Controller
	ctx                context.Context
	cache              *kihcache.CacheAllocator
	ipam               *ipam.IPAllocator
	dhcp               *dhcp.DHCPAllocator
	metrics            *metrics.MetricsAllocator
	kihClientset       *kihclientset.Clientset
	appStatus          *int
	ippoolCountCurrent *int
}

func NewController(
	queue workqueue.RateLimitingInterface,
	indexer cache.Indexer,
	informer cache.Controller,
	ctx context.Context,
	cache *kihcache.CacheAllocator,
	ipam *ipam.IPAllocator,
	dhcp *dhcp.DHCPAllocator,
	metrics *metrics.MetricsAllocator,
	kihClientset *kihclientset.Clientset,
	appStatus *int,
	ippoolCountCurrent *int,
) *Controller {
	return &Controller{
		informer:           informer,
		indexer:            indexer,
		queue:              queue,
		ctx:                ctx,
		cache:              cache,
		ipam:               ipam,
		dhcp:               dhcp,
		metrics:            metrics,
		kihClientset:       kihClientset,
		appStatus:          appStatus,
		ippoolCountCurrent: ippoolCountCurrent,
	}
}

func (c *Controller) processNextItem() bool {
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
	obj, exists, err := c.indexer.GetByKey(event.key)
	if err != nil {
		log.Errorf("(ippool.sync) fetching object with key %s from store failed with %v", event.key, err)
		c.metrics.UpdateLogStatus("error")

		return
	}

	if !exists && event.action != DELETE {
		log.Warnf("(ippool.sync) IPPool %s does not exist anymore", event.key)
		c.metrics.UpdateLogStatus("warning")

		return
	}

	switch event.action {
	case ADD:
		cleanup, err := c.registerIPPool(obj.(*kihv1.IPPool))
		if err != nil {
			log.Errorf("(ippool.sync) failed to allocate new pool for %s: %s", event.poolName, err.Error())
			c.metrics.UpdateLogStatus("error")

			if cleanup {
				if err := c.cleanupIPPoolObjects(obj.(*kihv1.IPPool)); err != nil {
					log.Errorf("(ippool.sync) failed to cleanup pool %s: %s", event.poolName, err.Error())
					c.metrics.UpdateLogStatus("error")
				}
			}
		}
	case UPDATE:
		pool, err := c.cache.Get("pool", event.poolNetworkName)
		if err != nil {
			log.Errorf("(ippool.sync) %s", err)
			c.metrics.UpdateLogStatus("error")
		} else {
			oldPool := pool.(kihv1.IPPool)
			err := c.handleIPPoolObjectChange(oldPool, obj.(*kihv1.IPPool))
			if err != nil {
				log.Errorf("(ippool.sync) failed to handle IPPool update for %s: %s", event.poolName, err.Error())
				c.metrics.UpdateLogStatus("error")
			}
		}
	case DELETE:
		pool, err := c.cache.Get("pool", event.poolNetworkName)
		if err != nil {
			log.Errorf("(ippool.sync) %s", err)
			c.metrics.UpdateLogStatus("error")
		} else {
			p := pool.(kihv1.IPPool)
			if err := c.cleanupIPPoolObjects(&p); err != nil {
				log.Errorf("(ippool.sync) failed to cleanup pool %s: %s", event.poolName, err.Error())
				c.metrics.UpdateLogStatus("error")
			}
		}

		// decreasing the ippoolCountCurrent is not necessary during application initialization, because:
		// if the ippool is ok, then it's already initialized, the counter should still match to proceed the startup phase
		// if the ippool is not ok, the counter has a mismatch and the application should be restarted
	}

	return
}

func (c *Controller) handleErr(err error, key interface{}) {
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

	log.Errorf("(ippool.handleErr) dropping IPPool %q out of the queue: %v", key, err)
	c.metrics.UpdateLogStatus("error")
}

func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	defer c.queue.ShutDown()
	log.Infof("(ippool.Run) starting the IPPool controller")

	go c.informer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		log.Errorf("(ippool.Run) timed out waiting for caches to sync")
		c.metrics.UpdateLogStatus("error")

		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	log.Infof("(ippool.Run) stopping the IPPool controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}
