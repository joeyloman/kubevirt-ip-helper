package vmnetcfg

import (
	"errors"
	"net"
	"sync"
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
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

const (
	APP_INIT    = 0
	APP_RUNNING = 1
	APP_RESTART = 2
)

type Controller struct {
	indexer              cache.Indexer
	queue                workqueue.RateLimitingInterface
	informer             cache.Controller
	cache                *kihcache.CacheAllocator
	ipam                 *ipam.IPAllocator
	dhcp                 *dhcp.DHCPAllocator
	metrics              *metrics.MetricsAllocator
	kihClientset         *kihclientset.Clientset
	appStatus            *int
	vmnetcfgCountCurrent *int

	// initAttempted tracks the vmnetcfg objects which the current
	// initialization phase already handled, so an object which cannot
	// complete its sync is still counted by the startup gate
	initAttempted map[string]bool

	mutex sync.Mutex
	// deferredInitAllocations records the vmnetcfg keys whose startup
	// sync deferred a fresh allocation until the initialization replay
	// of every object settled: a pending nic must not take an address
	// whose persisted assignment is still waiting in another object's
	// spec during the startup replay
	deferredInitAllocations map[string]bool
}

func NewController(
	queue workqueue.RateLimitingInterface,
	indexer cache.Indexer,
	informer cache.Controller,
	cache *kihcache.CacheAllocator,
	ipam *ipam.IPAllocator,
	dhcp *dhcp.DHCPAllocator,
	metrics *metrics.MetricsAllocator,
	kihClientset *kihclientset.Clientset,
	appStatus *int,
	vmnetcfgCountCurrent *int,
) *Controller {
	return &Controller{
		informer:             informer,
		indexer:              indexer,
		queue:                queue,
		cache:                cache,
		ipam:                 ipam,
		dhcp:                 dhcp,
		metrics:              metrics,
		kihClientset:         kihClientset,
		appStatus:            appStatus,
		vmnetcfgCountCurrent: vmnetcfgCountCurrent,
	}
}

// markInitAttempt counts one settled VirtualMachineNetworkConfig object
// for the startup gate: its sync either completed (nics in the ERROR
// status included, their object was processed) or was definitively
// rejected. a transiently failed restore stays uncounted, so the retried
// sync can still protect the existing reservation before the vm
// controller opens new allocations; a definitively broken object still
// counts so it does not block the controller startup until it is removed.
func (c *Controller) markInitAttempt(key string) {
	if *c.appStatus != APP_INIT {
		return
	}

	if c.initAttempted == nil {
		c.initAttempted = make(map[string]bool)
	}

	if _, exists := c.initAttempted[key]; exists {
		return
	}

	c.initAttempted[key] = true

	*c.vmnetcfgCountCurrent++
}

// initSyncSettled reports whether a failed sync can never succeed on a
// retry during the initialization phase: a networkname without a live
// pool registration cannot restore its reservation until the offending
// IPPool is repaired, an invalid macaddress in the spec cannot register a
// lease at all, and an ownership conflict (the pool status records the
// claimed address for another owner) needs one of the claiming objects to
// be edited. the startup gate must not wait for such objects, while every
// other failure is transient and the retried sync must stay able to
// settle the object for the gate.
func (c *Controller) initSyncSettled(vmnetcfg *kihv1.VirtualMachineNetworkConfig, err error) bool {
	if errors.Is(err, util.ErrForeignOwner) {
		return true
	}

	for _, v := range vmnetcfg.Spec.NetworkConfig {
		if _, cacheErr := c.cache.Get("pool", v.NetworkName); cacheErr != nil {
			return true
		}

		if _, macErr := net.ParseMAC(v.MACAddress); macErr != nil {
			return true
		}
	}

	return false
}

// deferInitAllocation records one vmnetcfg key whose startup sync
// deferred a fresh allocation until the initialization replay settled:
// the controller requeues the key after the startup gate counted every
// object, so the pending nic is served by a sync which cannot overtake
// the restored assignments of the other objects anymore.
func (c *Controller) deferInitAllocation(key string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.deferredInitAllocations == nil {
		c.deferredInitAllocations = make(map[string]bool)
	}

	c.deferredInitAllocations[key] = true
}

// releaseDeferredInitAllocations drains the recorded keys after the
// initialization replay finished: every object's durable assignments are
// restored or persisted as settled by then, so the fresh allocations of
// the pending nics can no longer take a recorded address.
func (c *Controller) releaseDeferredInitAllocations() (keys []string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	for key := range c.deferredInitAllocations {
		keys = append(keys, key)
	}

	c.deferredInitAllocations = make(map[string]bool)

	return keys
}

// runDeferredInitAllocations waits until the application left its
// initialization phase - every vmnetcfg object's startup sync then
// settled, durable assignments included - and requeues the deferred keys
// so the pending nics allocate from the settled state.
func (c *Controller) runDeferredInitAllocations(stopCh chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case <-time.After(time.Second):
		}

		if *c.appStatus != APP_INIT {
			break
		}
	}

	c.requeueDeferredInitAllocations()
}

// requeueDeferredInitAllocations requeues the recorded keys as UPDATE
// events: the fresh allocations of the pending nics run through the
// regular reconciliation, which cannot overtake the restored assignments
// of the other objects anymore because the initialization replay
// finished. a key which was deleted during the initialization settles
// through the missing-object handling.
func (c *Controller) requeueDeferredInitAllocations() {
	for _, key := range c.releaseDeferredInitAllocations() {
		log.Infof("(vmnetcfg.requeueDeferredInitAllocations) requeueing %s for the deferred fresh allocation after the initialization finished", key)

		c.queue.Add(Event{key: key, action: UPDATE})
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
		log.Errorf("(vmnetcfg.sync) fetching object with key %s from store failed with %v", event.key, err)
		c.metrics.UpdateLogStatus("error")

		return
	}

	if !exists && event.action != DELETE {
		log.Warnf("(vmnetcfg.sync) VirtualMachineNetworkConfig %s does not exist anymore", event.key)
		c.metrics.UpdateLogStatus("warning")
		// the object is gone and cannot produce a sync anymore; the startup
		// gate must not wait for it
		c.markInitAttempt(event.key)

		return
	}

	switch event.action {
	case ADD, UPDATE:
		err = c.updateVirtualMachineNetworkConfig(event.action, obj.(*kihv1.VirtualMachineNetworkConfig))
		if err != nil {
			log.Errorf("(vmnetcfg.sync) failed to update vmnetcfg for %s: %s", event.key, err.Error())
			c.metrics.UpdateLogStatus("error")
		}
		// the startup gate counts a vmnetcfg as handled once its sync
		// settled, whether the settled sync was the initial ADD or a
		// resynced UPDATE: an object whose ADD failed transiently recovers
		// through the resync and must not leave the gate waiting forever.
		// vmnetcfgs with nics in the ERROR status count as well because
		// their sync completed, and a definitively rejected ADD counts so a
		// broken vmnetcfg does not block the vm controller startup forever.
		// a transiently failed restore stays uncounted instead: the
		// rate-limited retry must stay able to protect the existing
		// reservation before the vm controller opens new allocations
		if err == nil || (event.action == ADD && c.initSyncSettled(obj.(*kihv1.VirtualMachineNetworkConfig), err)) {
			c.markInitAttempt(event.key)
		}
	case DELETE:
		// an object which is gone can never produce a settled sync anymore:
		// the gate counts it so a startup-time deletion does not block the
		// controller startup forever
		c.markInitAttempt(event.key)
	}

	return
}

func (c *Controller) handleErr(err error, key interface{}) {
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

	log.Errorf("(vmnetcfg.handleErr) dropping VirtualMachineNetworkConfig %q out of the queue: %v", key, err)
	c.metrics.UpdateLogStatus("error")

	// an exhausted key can never settle through its own retries anymore:
	// the gate counts it so the app startup does not wait forever for an
	// object which keeps failing (marked exactly once via initAttempted)
	if ev, ok := key.(Event); ok {
		c.markInitAttempt(ev.key)
	}
}

func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	defer c.queue.ShutDown()
	log.Infof("(vmnetcfg.Run) starting the VirtualMachineNetworkConfig controller")

	go c.informer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		log.Errorf("(vmnetcfg.Run) timed out waiting for caches to sync")
		c.metrics.UpdateLogStatus("error")

		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	// requeue the pending nics deferred during the startup replay once
	// the initialization phase settled every object's durable
	// assignments
	go c.runDeferredInitAllocations(stopCh)

	<-stopCh
	log.Infof("(vmnetcfg.Run) stopping the VirtualMachineNetworkConfig controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}
