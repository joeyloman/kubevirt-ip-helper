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

	// initAttempted tracks the IPPool objects which the current
	// initialization phase already handled, so a pool which definitively
	// cannot register is still counted by the startup gate
	initAttempted map[string]bool
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

// markInitAttempt counts one settled IPPool object for the startup gate:
// its registration is either live or definitively rejected (the pool
// object can also be gone already). counting only settled pools keeps the
// vmnetcfg controller from restoring bindings into pools which are not
// registered yet, while rejected pools still count so a broken object
// does not block the controller startup until it is removed first.
func (c *Controller) markInitAttempt(name string) {
	if *c.appStatus != APP_INIT {
		return
	}

	if c.initAttempted == nil {
		c.initAttempted = make(map[string]bool)
	}

	if _, exists := c.initAttempted[name]; exists {
		return
	}

	c.initAttempted[name] = true

	*c.ippoolCountCurrent++
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
		c.markInitAttempt(event.poolName)

		return
	}

	switch event.action {
	case ADD:
		err = c.registerPoolWithTeardown(obj.(*kihv1.IPPool), "failed to allocate new pool for")
	case UPDATE:
		pool, poolErr := c.cache.Get("pool", event.poolNetworkName)
		if poolErr != nil && event.oldPoolNetworkName != "" && event.oldPoolNetworkName != event.poolNetworkName {
			// the networkname changed: the cache still holds the pool under
			// the old key, so the restart handling sees the old configuration
			pool, poolErr = c.cache.Get("pool", event.oldPoolNetworkName)
		}

		if poolErr != nil || pool.(kihv1.IPPool).Name != event.poolName {
			// neither cache key resolves to THIS pool: the pool has no live
			// registration (its first attempt was dropped, or the lookup
			// resolved a different pool which shares the networkname). the
			// update becomes a re-registration attempt instead, so a fixed
			// projection comes to life with the next event without a pod
			// restart. a still-unregistrable projection keeps failing and a
			// claimed networkname is rejected without touching the live
			// state of the pool which owns it. a partially applied
			// registration is torn back down, so the retried attempt is
			// not rejected by the leftover sub-resources of its own
			// previous attempt.
			err = c.registerPoolWithTeardown(obj.(*kihv1.IPPool), "failed to register unregistered pool")

			return err
		}
		oldPool := pool.(kihv1.IPPool)
		err = c.handleIPPoolObjectChange(oldPool, obj.(*kihv1.IPPool))
		if err != nil {
			log.Errorf("(ippool.sync) failed to handle IPPool update for %s: %s", event.poolName, err.Error())
			c.metrics.UpdateLogStatus("error")
		}
	case DELETE:
		// a pool which is deleted can never settle a registration for the
		// gate anymore (it may have failed its attempts during startup):
		// count it so a startup-time deletion does not block the controller
		// startup forever. counted pools are deduplicated by name.
		c.markInitAttempt(event.poolName)

		pool, poolErr := c.cache.Get("pool", event.poolNetworkName)
		if poolErr != nil {
			log.Errorf("(ippool.sync) %s", poolErr)
			c.metrics.UpdateLogStatus("error")

			return
		}

		p := pool.(kihv1.IPPool)
		if p.Name != event.poolName {
			// the cache is keyed by the networkname, so this lookup returns
			// the pool which lives under the deleted object's networkname.
			// a pool which was never registered under its own networkname
			// (for example one whose ADD was rejected because a live pool
			// already claims it) therefore resolves to that live pool.
			// freeing the live pool's registration because an unrelated
			// object was deleted is incorrect, so this delete stays a no-op.
			log.Warnf("(ippool.sync) IPPool %s [networkname %s] was never registered; skipping cleanup of the live state",
				event.poolName, event.poolNetworkName)
			c.metrics.UpdateLogStatus("warning")

			return
		}
		if err = c.cleanupIPPoolObjects(&p); err != nil {
			log.Errorf("(ippool.sync) failed to cleanup pool %s: %s", event.poolName, err.Error())
			c.metrics.UpdateLogStatus("error")
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

	// an exhausted key can never settle through its own retries anymore:
	// the gate counts it so the app startup does not wait forever for an
	// object which keeps failing (marked exactly once via initAttempted)
	if ev, ok := key.(Event); ok {
		c.markInitAttempt(ev.poolName)
	}
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
