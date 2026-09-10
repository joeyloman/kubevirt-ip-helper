package ippool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	appStatus          *atomic.Int32
	ippoolCountCurrent *atomic.Int32

	// initAttempted tracks the IPPool objects which the current
	// initialization phase already handled, so a pool which definitively
	// cannot register is still counted by the startup gate
	initAttempted map[string]bool

	// runListener opens the dhcp listener of a pool. it is an indirection
	// over dhcp.Run so the listener start of a registration and the
	// listener repair are testable without a host interface (the same
	// seam shape as network.AddIpToNic/RemoveIpFromNic): production
	// controllers default to dhcp.Run, tests substitute a nil-returning
	// stub
	runListener func(networkName string, nic string) error

	// registeredPools records the networkname each pool NAME holds its
	// live registration of this era under. the cache is keyed by the
	// networkname alone, so this record is the only way an update event
	// which no longer carries the registered networkname (a rename
	// swallowed while the application was initializing, re-delivered by a
	// resync with old==new) can find the live registration it must tear
	// down instead of registering the pool a second time
	registeredPools map[string]string
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
	appStatus *atomic.Int32,
	ippoolCountCurrent *atomic.Int32,
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
	if c.appStatus.Load() != APP_INIT {
		return
	}

	if c.initAttempted == nil {
		c.initAttempted = make(map[string]bool)
	}

	if _, exists := c.initAttempted[name]; exists {
		return
	}

	c.initAttempted[name] = true

	c.ippoolCountCurrent.Add(1)
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
			// a dying era must not re-register: registerPoolWithTeardown
			// would re-add the server ip to the nic and re-open the dhcp
			// listener after (or during) the application teardown, and the
			// new era's registration would then collide with the stale
			// address deterministically forever
			if c.appStatus.Load() == APP_RESTART {
				log.Warnf("(ippool.sync) deferring re-registration of pool %s while the application is reinitializing", event.poolName)
				return fmt.Errorf("deferring re-registration of pool %s while the application is reinitializing", event.poolName)
			}

			// the pool can nevertheless own a live registration under a
			// networkname which this event does not carry: its rename
			// arrived while the application was initializing (updates are
			// ignored then), so the registration kept serving under the
			// old networkname while the object - and every resync update
			// with it - already carries the new one. re-registering would
			// create a SECOND live registration of the same pool (two dhcp
			// listeners on the same segment, and a stale registration
			// under the old networkname whose state no later event can
			// clean anymore), so the rename is routed through the regular
			// change handling, which tears the old registration down
			// through the restart flow instead
			if registeredNet, live := c.registeredPools[event.poolName]; live && registeredNet != event.poolNetworkName {
				if oldPool, oldErr := c.cache.Get("pool", registeredNet); oldErr == nil && oldPool.(kihv1.IPPool).Name == event.poolName {
					err = c.handleIPPoolObjectChange(oldPool.(kihv1.IPPool), obj.(*kihv1.IPPool))
					if err != nil {
						log.Errorf("(ippool.sync) failed to handle the deferred networkname change of pool %s: %s", event.poolName, err.Error())
						c.metrics.UpdateLogStatus("error")
					}

					return err
				}

				// the recorded registration is not live anymore (its cache
				// entry was released with it): fall through to the
				// re-registration attempt
				log.Warnf("(ippool.sync) the recorded registration of pool %s under networkname %s is not live anymore, re-registering it",
					event.poolName, registeredNet)
			}

			err = c.registerPoolWithTeardown(obj.(*kihv1.IPPool), "failed to register unregistered pool")

			return err
		}
		oldPool := pool.(kihv1.IPPool)
		err = c.handleIPPoolObjectChange(oldPool, obj.(*kihv1.IPPool))
		if err != nil {
			log.Errorf("(ippool.sync) failed to handle IPPool update for %s: %s", event.poolName, err.Error())
			c.metrics.UpdateLogStatus("error")
		}

		// a pool whose dhcp listener died after its registration (its
		// socket error was surfaced and deregistered by the serve wrapper)
		// is re-served by the next event or resync: the registration state
		// (server ip on the nic, dhcp pool, ipam subnet) is still live, so
		// only the listener needs to be re-opened. the repair runs only
		// while the application serves: during the startup replay
		// (APP_INIT) and the reinitialization teardown (APP_RESTART) the
		// listener lifecycle belongs to the era transitions, whose fresh
		// registration re-opens the sockets. the controller runs one sync
		// worker, but the queue keys items by event rather than by pool, so
		// an in-flight ADD and a resync UPDATE of the same network stay
		// distinct items: the duplicate-run rejection is therefore
		// classified as the converged outcome instead of a failure
		if err == nil && c.appStatus.Load() == APP_RUNNING && !c.dhcp.IsRunning(obj.(*kihv1.IPPool).Spec.NetworkName) {
			runListener := c.runListener
			if runListener == nil {
				runListener = c.dhcp.Run
			}

			if runErr := runListener(obj.(*kihv1.IPPool).Spec.NetworkName, obj.(*kihv1.IPPool).Spec.BindInterface); runErr != nil {
				if errors.Is(runErr, dhcp.ErrServerAlreadyRunning) {
					// a concurrent registration (or a repair which just
					// won the race) serves the pool already: converged,
					// nothing to retry
					log.Warnf("(ippool.sync) the DHCP listener of pool %s is already running, nothing to repair", event.poolName)
					c.metrics.UpdateLogStatus("warning")
				} else {
					log.Errorf("(ippool.sync) failed to restore the DHCP listener of pool %s: %s", event.poolName, runErr.Error())
					c.metrics.UpdateLogStatus("error")

					err = runErr
				}
			} else {
				log.Warnf("(ippool.sync) restored the DHCP listener of pool %s after its unexpected termination", event.poolName)
				c.metrics.UpdateLogStatus("warning")
			}
		}
	case DELETE:
		// a pool which is deleted can never settle a registration for the
		// gate anymore (it may have failed its attempts during startup):
		// count it so a startup-time deletion does not block the controller
		// startup forever. counted pools are deduplicated by name.
		c.markInitAttempt(event.poolName)

		pool, poolErr := c.cache.Get("pool", event.poolNetworkName)
		if poolErr != nil {
			// no live registration exists under this networkname: the pool
			// was never registered in this process era (its ADD was
			// rejected, or its attempts failed and a partial registration
			// was torn back down), so the deletion has no live state to
			// clean up. this is the converged outcome of a never-registered
			// pool, not a failure, so it is reported like the name-mismatch
			// case below instead of counting as an error.
			log.Warnf("(ippool.sync) IPPool %s [networkname %s] was never registered; skipping cleanup of the live state",
				event.poolName, event.poolNetworkName)
			c.metrics.UpdateLogStatus("warning")

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

	// the workers are joined before Run returns: an in-flight sync may
	// still be registering or tearing down pool state (nic addresses,
	// dhcp listeners), so a caller waiting for this era to end (the
	// EventListener join) must not observe Run returning while a worker
	// is still reconciling
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(c.runWorker, time.Second, stopCh)
		}()
	}

	<-stopCh
	// shut the queue down before joining the workers: one blocked in
	// queue.Get is only released by the shutdown, so waiting first would
	// deadlock (the deferred shutdown stays as the early-return safety)
	c.queue.ShutDown()
	wg.Wait()
	log.Infof("(ippool.Run) stopping the IPPool controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}
