package vm

import (
	"context"
	"errors"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kubevirtv1 "kubevirt.io/api/core/v1"

	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
)

const (
	// vmNetCfgTerminatingRetryDelay is the pause between reconciliation
	// attempts while the old VirtualMachineNetworkConfig is still
	// terminating. The finalizer cleanup is usually sub-second; a fixed
	// short delay replaces the near-instant five-attempt budget which used
	// to drop the key and wait for the one-minute informer resync, leaving
	// the recreated vm without a lease for up to a minute.
	vmNetCfgTerminatingRetryDelay = 2 * time.Second
	// vmNetCfgTerminatingRetryLimit bounds the deferred retries. After
	// this many attempts the key falls back to the periodic resync, so a
	// permanently stuck finalizer cannot spin the worker forever.
	vmNetCfgTerminatingRetryLimit = 30
)

// errVMNetCfgTerminating marks a sync deferred because the vmnetcfg object
// is still being deleted. It is not a transient API failure: the vm
// controller retries it on a fixed short delay with a dedicated budget
// instead of consuming the rate-limited error retries.
var errVMNetCfgTerminating = errors.New("VirtualMachineNetworkConfig is being deleted")

type Controller struct {
	ctx          context.Context
	indexer      cache.Indexer
	queue        workqueue.RateLimitingInterface
	informer     cache.Controller
	cache        *kihcache.CacheAllocator
	ipam         *ipam.IPAllocator
	dhcp         *dhcp.DHCPAllocator
	metrics      *metrics.MetricsAllocator
	kihClientset *kihclientset.Clientset

	// terminatingRetryDelay is the deferred retry delay for
	// errVMNetCfgTerminating. Tests set it to zero to keep the queue
	// deterministic.
	terminatingRetryDelay time.Duration
	// terminatingDeferrals counts the deferred retries per vm key.
	deferralMutex        sync.Mutex
	terminatingDeferrals map[string]int
}

func NewController(
	ctx context.Context,
	queue workqueue.RateLimitingInterface,
	indexer cache.Indexer,
	informer cache.Controller,
	cache *kihcache.CacheAllocator,
	ipam *ipam.IPAllocator,
	dhcp *dhcp.DHCPAllocator,
	metrics *metrics.MetricsAllocator,
	kihClientset *kihclientset.Clientset,
) *Controller {
	// the API calls of a reconciliation run under the era context: a
	// canceled era (application reinit or shutdown) aborts in-flight
	// syncs instead of blocking the era join until every client timeout
	if ctx == nil {
		ctx = context.Background()
	}

	return &Controller{
		ctx:          ctx,
		informer:     informer,
		indexer:      indexer,
		queue:        queue,
		cache:        cache,
		ipam:         ipam,
		dhcp:         dhcp,
		metrics:      metrics,
		kihClientset: kihClientset,

		terminatingRetryDelay: vmNetCfgTerminatingRetryDelay,
		terminatingDeferrals:  make(map[string]int),
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
		log.Errorf("(vm.sync) fetching object with key %s from store failed with %v", event.key, err)
		c.metrics.UpdateLogStatus("error")

		return
	}

	if !exists && event.action != DELETE {
		// disabled warn logging because sometimes a vm object could already be removed when there is still an update job in the queue
		log.Debugf("(vm.sync) VirtualMachine %s does not exist anymore", event.key)

		return
	}

	switch event.action {
	case ADD, UPDATE:
		err = c.handleVirtualMachineObjectChange(obj.(*kubevirtv1.VirtualMachine))
	case DELETE:
		if exists {
			// the informer removes an object from the store before
			// delivering its delete event, so an object under this key is
			// a same-name replacement created while the deletion (or its
			// rate-limited retry) was in flight: tearing down the vmnetcfg
			// object now would destroy the replacement's network
			// configuration. the cleanup is dropped and the replacement's
			// own events manage the object.
			log.Warnf("(vm.sync) VirtualMachine %s was deleted but a same-name replacement exists, skipping the vmnetcfg cleanup", event.key)
			c.metrics.UpdateLogStatus("warning")

			return
		}

		err = c.deleteVirtualMachineNetworkConfigObject(event.vmNamespace, event.vmName)
	}

	if err != nil {
		log.Errorf("(vm.sync) %s", err)
		c.metrics.UpdateLogStatus("error")
	}

	return
}

func (c *Controller) clearTerminatingDeferral(key string) {
	c.deferralMutex.Lock()
	delete(c.terminatingDeferrals, key)
	c.deferralMutex.Unlock()
}

// deferTerminating requeues a sync whose vmnetcfg is still terminating on
// the fixed short delay and reports whether the retry budget is left. It
// deliberately bypasses the rate-limited retry count: the wait is for the
// old object's finalizer, not for this sync to stop failing, so burning the
// five transient-error retries in milliseconds and then waiting for the
// one-minute resync is exactly the gap this closes.
func (c *Controller) deferTerminating(eventKey string, queueKey interface{}) bool {
	c.deferralMutex.Lock()
	attempts := c.terminatingDeferrals[eventKey]
	if attempts >= vmNetCfgTerminatingRetryLimit {
		delete(c.terminatingDeferrals, eventKey)
		c.deferralMutex.Unlock()

		return false
	}
	c.terminatingDeferrals[eventKey] = attempts + 1
	c.deferralMutex.Unlock()

	c.queue.Forget(queueKey)

	if c.terminatingRetryDelay <= 0 {
		c.queue.Add(queueKey)
	} else {
		c.queue.AddAfter(queueKey, c.terminatingRetryDelay)
	}

	return true
}

func (c *Controller) handleErr(err error, key interface{}) {
	event, hasEvent := key.(Event)

	if err == nil {
		if hasEvent {
			c.clearTerminatingDeferral(event.key)
		}

		c.queue.Forget(key)

		return
	}

	if hasEvent && errors.Is(err, errVMNetCfgTerminating) {
		if c.deferTerminating(event.key, key) {
			return
		}

		c.queue.Forget(key)

		log.Errorf("(vm.handleErr) dropping VirtualMachine %q out of the queue after %d deferred retries while its vmnetcfg is terminating: %v",
			event.key, vmNetCfgTerminatingRetryLimit, err)
		c.metrics.UpdateLogStatus("error")

		return
	}

	if hasEvent {
		c.clearTerminatingDeferral(event.key)
	}

	if c.queue.NumRequeues(key) < 5 {
		log.Errorf("(vm.handleErr) syncing VirtualMachine %v: %v", key, err)

		c.queue.AddRateLimited(key)

		return
	}

	c.queue.Forget(key)

	log.Errorf("(vm.handleErr) dropping VirtualMachine %q out of the queue: %v", key, err)
	c.metrics.UpdateLogStatus("error")
}

func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	defer c.queue.ShutDown()
	log.Infof("(vm.Run) starting the VirtualMachine controller")

	go c.informer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		log.Errorf("Timed out waiting for caches to sync")
		c.metrics.UpdateLogStatus("error")

		return
	}

	// the workers are joined before Run returns: an in-flight sync may
	// still hold local allocator state, so a caller waiting for this era
	// to end (the EventListener join) must not observe Run returning
	// while a worker is still reconciling
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
	log.Infof("(vm.runWorker) stopping the VirtualMachine controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}
