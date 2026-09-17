package ippool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/gate"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

const (
	APP_INIT    = 0
	APP_RUNNING = 1
	APP_RESTART = 2
)

// A disappeared selected object has no informer resync to retry it. Keep
// verification failures queued until the API can distinguish loss from deletion.
var errPoolDisappearanceUnverified = errors.New("IPPool disappearance remains unverified")

type Controller struct {
	indexer      cache.Indexer
	queue        workqueue.RateLimitingInterface
	informer     cache.Controller
	ctx          context.Context
	cache        *kihcache.CacheAllocator
	ipam         *ipam.IPAllocator
	dhcp         *dhcp.DHCPAllocator
	metrics      *metrics.MetricsAllocator
	kihClientset *kihclientset.Clientset
	appStatus    *atomic.Int32
	scope        util.NetworkScope

	// gate is the startup membership gate of this era: its snapshot holds
	// the exact keys of the startup LIST, and markInitAttempt settles a
	// key once its registration attempt settled. a pool created after the
	// snapshot settles a key which is not part of it, so it can never
	// substitute for an unvisited pre-existing pool the way a plain count
	// would let it
	gate *gate.Gate

	// verifyVM reports whether the VirtualMachine of a given namespace
	// and name exists. it is an indirection over the kubevirt client so
	// the ledger revalidation of the claim protection is testable without
	// a live cluster (the same seam shape as runListener): production
	// controllers verify through the kubevirt api, tests substitute a
	// stub. a nil seam fails closed (the claim stays protected)
	verifyVM func(namespace string, name string) (bool, error)

	// runListener opens the dhcp listener of a pool. it is an indirection
	// over dhcp.Run so the listener start of a registration and the
	// listener repair are testable without a host interface (the same
	// seam shape as network.AddIpToNic/RemoveIpFromNic): production
	// controllers default to dhcp.Run, tests substitute a nil-returning
	// stub
	runListener func(networkName string, nic string) error
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
	startupGate *gate.Gate,
	verifyVM func(namespace string, name string) (bool, error),
	scope util.NetworkScope,
) *Controller {
	return &Controller{
		informer:     informer,
		indexer:      indexer,
		queue:        queue,
		ctx:          ctx,
		cache:        cache,
		ipam:         ipam,
		dhcp:         dhcp,
		metrics:      metrics,
		kihClientset: kihClientset,
		appStatus:    appStatus,
		gate:         startupGate,
		verifyVM:     verifyVM,
		scope:        scope,
	}
}

// markInitAttempt settles one IPPool object for the startup gate: its
// registration is either live or definitively rejected (the pool object
// can also be gone already). settling only settled pools keeps the
// vmnetcfg controller from restoring bindings into pools which are not
// registered yet, while rejected pools still settle so a broken object
// does not block the controller startup until it is removed first.
func (c *Controller) markInitAttempt(name string) {
	if c.appStatus.Load() != APP_INIT || c.gate == nil {
		return
	}

	c.gate.Settle(name)
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

	// A filtered watch DELETE (including a tombstone), or an absent cache
	// entry, is not proof of deletion. Verify existence without the selector;
	// an unavailable API must retain the registration for a later retry.
	if event.action == DELETE || !exists {
		if c.kihClientset == nil {
			return fmt.Errorf("%w: no API client for IPPool %s", errPoolDisappearanceUnverified, event.poolName)
		}
		current, getErr := c.kihClientset.KubevirtiphelperV1().IPPools().Get(c.ctx, event.poolName, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			if err := c.removeLocalRegistration(event.poolName); err != nil {
				return err
			}
			c.markInitAttempt(event.poolName)
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("%w: IPPool %s: %v", errPoolDisappearanceUnverified, event.poolName, getErr)
		}
		if !c.scope.MatchesPool(current) {
			if err := c.removeLocalRegistration(event.poolName); err != nil {
				return err
			}
			// Only a key already present in the selected startup snapshot
			// can settle here. The verification response is never discovered.
			c.markInitAttempt(event.poolName)
			return fmt.Errorf("live IPPool %s no longer matches network %s; local registration removed, durable claims retained: %w",
				current.Name, c.scope.NetworkName(), ErrPoolUnregistrable)
		}
		// Retire a predecessor seen during disappearance verification, but
		// never discover its replacement through this unfiltered GET.
		if installed, cacheErr := c.cache.Get("pool", c.scope.NetworkName()); cacheErr == nil {
			pool := installed.(kihv1.IPPool)
			if pool.Name == current.Name && pool.UID != current.UID {
				if err := c.cleanupIPPoolObjects(&pool); err != nil {
					return err
				}
			}
		}
		if !exists || obj.(*kihv1.IPPool).UID != current.UID || !c.scope.MatchesPool(obj.(*kihv1.IPPool)) {
			return fmt.Errorf("%w: waiting for selected informer observation of live IPPool %s",
				errPoolDisappearanceUnverified, current.Name)
		}
		event.action = UPDATE
	}

	current := obj.(*kihv1.IPPool)
	if !c.scope.MatchesPool(current) {
		if err := c.removeLocalRegistration(current.Name); err != nil {
			return err
		}
		if c.selectedPool(current) {
			c.markInitAttempt(current.Name)
		}
		return c.poolIdentityError(current)
	}
	event.poolName = current.Name
	event.poolNetworkName = current.Spec.NetworkName
	switch event.action {
	case ADD:
		// a dying era must not register a pool it is about to tear down
		// (the same fence the UPDATE re-registration path applies): the
		// registration would re-add the server ip to the nic and open the
		// dhcp listener after (or during) the application teardown. the
		// deferred add fails the sync so the requeue retries it, and the
		// next era's resync re-delivers it in any case
		if c.appStatus.Load() == APP_RESTART {
			log.Warnf("(ippool.sync) deferring registration of pool %s while the application is reinitializing", event.poolName)
			return fmt.Errorf("deferring registration of pool %s while the application is reinitializing", event.poolName)
		}
		err = c.registerPoolWithTeardown(obj.(*kihv1.IPPool), "failed to allocate new pool for")
	case UPDATE:
		pool, poolErr := c.cache.Get("pool", event.poolNetworkName)
		if poolErr != nil || pool.(kihv1.IPPool).Name != event.poolName {
			// The cache does not resolve to THIS pool: it has no live
			// registration, or another pool already claims the network. The
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
	}

	return
}

// removeLocalRegistration deliberately has no API writes: selector loss must
// leave the still-live pool's reservation ledger available for label restoration.
func (c *Controller) removeLocalRegistration(name string) error {
	cached, err := c.cache.Get("pool", c.scope.NetworkName())
	if err != nil {
		return nil
	}
	pool := cached.(kihv1.IPPool)
	if pool.Name != name {
		return nil
	}
	return c.cleanupIPPoolObjects(&pool)
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
	if errors.Is(err, errPoolDisappearanceUnverified) {
		c.queue.AddAfter(key, resyncPeriod)
		return
	}

	log.Errorf("(ippool.handleErr) dropping IPPool %q out of the queue: %v", key, err)
	c.metrics.UpdateLogStatus("error")
	// an exhausted key can never settle through its own retries anymore:
	// the gate settles it so the app startup does not wait forever for an
	// object which keeps failing
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

	// The selected initial LIST may omit a startup key after deletion OR
	// selector loss. Queue the same fresh-read disappearance reconciliation
	// as a watch DELETE; a missing index entry cannot settle it by itself.
	if c.gate != nil {
		for _, key := range c.gate.Unsettled() {
			if _, exists, getErr := c.indexer.GetByKey(key); getErr == nil && !exists {
				c.queue.Add(Event{key: key, action: DELETE, poolName: key})
			}
		}
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
