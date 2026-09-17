package vm

import (
	"context"
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
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

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
	scope        util.NetworkScope
	reconcileMu  *sync.Mutex
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
	scope util.NetworkScope,
	reconcileMu *sync.Mutex,
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
		scope:        scope,
		reconcileMu:  reconcileMu,
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
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	if err := c.ctx.Err(); err != nil {
		return err
	}

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

func (c *Controller) handleErr(err error, key interface{}) {
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
