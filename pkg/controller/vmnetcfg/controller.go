package vmnetcfg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	log "github.com/sirupsen/logrus"

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
	appStatus    *atomic.Int32
	scope        util.NetworkScope
	reconcileMu  *sync.Mutex

	// gate is the startup membership gate of this era: its snapshot holds
	// the exact keys of the startup LIST, and markInitAttempt settles a
	// key once its sync settled. a vmnetcfg created after the snapshot
	// settles a key which is not part of it, so it can never substitute
	// for an unvisited pre-existing object the way a plain count would
	// let it
	gate *gate.Gate

	// verifyVM reports whether the VirtualMachine of a given namespace
	// and name exists. it is an indirection over the kubevirt client so
	// the orphan sweep is testable without a live cluster (the same seam
	// shape as the ippool controller's ledger revalidation). a nil seam
	// fails closed: no vm existence is verified, so nothing is swept.
	verifyVM func(namespace string, name string) (bool, error)

	mutex sync.Mutex
	// deferredInitAllocations records the vmnetcfg keys whose startup
	// sync deferred a fresh allocation until the initialization replay
	// of every object settled: a pending nic must not take an address
	// whose persisted assignment is still waiting in another object's
	// spec during the startup replay
	deferredInitAllocations map[string]bool

	// pendingUnwinds records the ledger deletions whose compensating or
	// unwind attempt failed while the nic was concurrently removed: the
	// tuple cannot be reconstructed from the spec anymore, so the
	// reconciliations of the owning object replay them. guarded by mutex
	// like deferredInitAllocations
	pendingUnwinds map[string][]pendingLedgerDelete
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
	appStatus *atomic.Int32,
	startupGate *gate.Gate,
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
		appStatus:    appStatus,
		gate:         startupGate,
		scope:        scope,
		reconcileMu:  reconcileMu,
	}
}

// markInitAttempt settles one VirtualMachineNetworkConfig object for the
// startup gate: its sync either completed (nics in the ERROR status
// included, their object was processed) or was definitively rejected.
// a transiently failed restore stays unsettled, so the retried sync can
// still protect the existing reservation before the vm controller opens
// new allocations; a definitively broken object still settles so it does
// not block the controller startup until it is removed.
func (c *Controller) markInitAttempt(key string) {
	if c.appStatus.Load() != APP_INIT || c.gate == nil {
		return
	}

	c.gate.Settle(key)
}

// errNicPoolMissing marks a per-interface restore failure whose networkname
// has no live pool registration: the interface cannot restore its
// reservation until the offending IPPool is repaired, so a sync failing
// with it can never succeed on a retry and settles the startup gate.
var errNicPoolMissing = errors.New("networkname has no registered pool")

// errNicMacInvalid marks a per-interface restore failure whose macaddress
// cannot parse: the interface can never register a lease until the spec is
// corrected, so a sync failing with it settles the startup gate as well.
var errNicMacInvalid = errors.New("invalid macaddress")

// initSyncSettled reports whether a failed sync can never succeed on a
// retry during the initialization phase. the classification follows the
// recorded per-interface failure itself (the sentinel-wrapped restore
// errors of updateVirtualMachineNetworkConfig), never a re-scan of the
// spec: a transient failure on one interface must keep the object
// uncounted even when an unrelated interface of the same object is
// permanently broken, or the gate would open while the transient restore
// is still pending. an ownership conflict (the pool status records the
// claimed address for another owner) needs one of the claiming objects to
// be edited, a networkname without a live pool registration cannot
// restore its reservation until the offending IPPool is repaired, and an
// invalid macaddress in the spec cannot register a lease at all. every
// other failure is transient and the retried sync must stay able to
// settle the object for the gate.
func (c *Controller) initSyncSettled(err error) bool {
	return errors.Is(err, util.ErrForeignOwner) ||
		errors.Is(err, errNicPoolMissing) ||
		errors.Is(err, errNicMacInvalid)
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

// rememberPendingUnwind records a ledger deletion which failed while its
// nic was concurrently removed: the retried reconciliation of the owning
// object cannot reconstruct the tuple from the spec anymore (the removal
// is durable by then), so this record is the only thing which keeps the
// owner-validated deletion reachable.
func (c *Controller) rememberPendingUnwind(key string, entry pendingLedgerDelete) {
	if !c.scope.Owns(entry.namespace, entry.networkName) {
		return
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.pendingUnwinds == nil {
		c.pendingUnwinds = make(map[string][]pendingLedgerDelete)
	}

	c.pendingUnwinds[key] = append(c.pendingUnwinds[key], entry)
}

// drainPendingUnwinds replays the recorded ledger deletions of a deleted
// object one last time and drops them. a force-delete strips the
// finalizers externally, so the object can disappear while its replay is
// still recorded, and no reconciliation of it will ever arrive again -
// the resident entries would keep the map bound and the ledger records
// stranded until the next process era. each entry gets one final
// owner-validated attempt; whatever still fails is dropped with a warning.
// The next era's pool registration revalidates the persisted ledger.
func (c *Controller) drainPendingUnwinds(key string) {
	c.mutex.Lock()
	pending := c.pendingUnwinds[key]
	delete(c.pendingUnwinds, key)
	c.mutex.Unlock()

	for _, entry := range pending {
		if !c.scope.Owns(entry.namespace, entry.networkName) {
			continue
		}
		err := c.updateIPPoolStatus(
			DELETE,
			entry.namespace,
			entry.vmName,
			entry.ip,
			entry.networkName,
			entry.macAddress,
			entry.poolName,
		)
		if err == nil {
			log.Warnf("(vmnetcfg.drainPendingUnwinds) [%s] removed the pending ledger record of ip %s in pool %s after the deletion",
				key, entry.ip, entry.poolName)
			c.metrics.UpdateLogStatus("warning")

			continue
		}

		if errors.Is(err, util.ErrForeignOwner) || apierrors.IsNotFound(err) {
			log.Warnf("(vmnetcfg.drainPendingUnwinds) [%s] the pending ledger record of ip %s in pool %s converged: %s",
				key, entry.ip, entry.poolName, err)
			c.metrics.UpdateLogStatus("warning")

			continue
		}

		log.Warnf("(vmnetcfg.drainPendingUnwinds) [%s] dropping the pending ledger record of ip %s in pool %s after the deletion, the next era's registration revalidates the ledger: %s",
			key, entry.ip, entry.poolName, err)
		c.metrics.UpdateLogStatus("warning")
	}
}

// retryPendingUnwinds replays the failed ledger deletions of an object at
// the start of its reconciliation. a deletion which converged - the
// record is gone, a foreign owner recorded the address in the meantime,
// or the pool itself is gone with its ledger - is dropped from the
// pending list; a transiently failing one stays recorded and fails the
// reconciliation, so the rate-limited retry and the resync keep replaying
// it. without this replay the orphaned record would block a later
// binding's ledger write until the next pool registration revalidates
// the persisted ledger.
func (c *Controller) retryPendingUnwinds(vmnetcfg *kihv1.VirtualMachineNetworkConfig) error {
	key := fmt.Sprintf("%s/%s", vmnetcfg.Namespace, vmnetcfg.Name)

	c.mutex.Lock()
	pending := c.pendingUnwinds[key]
	delete(c.pendingUnwinds, key)
	c.mutex.Unlock()

	if len(pending) == 0 {
		return nil
	}

	var retryErr error

	for _, entry := range pending {
		if !c.scope.Owns(entry.namespace, entry.networkName) {
			continue
		}
		err := c.updateIPPoolStatus(
			DELETE,
			entry.namespace,
			entry.vmName,
			entry.ip,
			entry.networkName,
			entry.macAddress,
			entry.poolName,
		)
		if err == nil {
			log.Warnf("(vmnetcfg.retryPendingUnwinds) [%s/%s] removed the pending ledger record of ip %s in pool %s",
				vmnetcfg.Namespace, vmnetcfg.Name, entry.ip, entry.poolName)
			c.metrics.UpdateLogStatus("warning")

			continue
		}

		if errors.Is(err, util.ErrForeignOwner) || apierrors.IsNotFound(err) {
			// the record belongs to another owner now, or the pool is gone
			// with its ledger: converged, nothing left to replay
			log.Warnf("(vmnetcfg.retryPendingUnwinds) [%s/%s] the pending ledger record of ip %s in pool %s converged: %s",
				vmnetcfg.Namespace, vmnetcfg.Name, entry.ip, entry.poolName, err)
			c.metrics.UpdateLogStatus("warning")

			continue
		}

		log.Errorf("(vmnetcfg.retryPendingUnwinds) [%s/%s] cannot remove the pending ledger record of ip %s in pool %s: %s",
			vmnetcfg.Namespace, vmnetcfg.Name, entry.ip, entry.poolName, err)
		c.metrics.UpdateLogStatus("error")

		c.rememberPendingUnwind(key, entry)

		if retryErr == nil {
			retryErr = err
		}
	}

	return retryErr
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

		if c.appStatus.Load() != APP_INIT {
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
		// the startup gate settles a vmnetcfg once its sync settled,
		// whether the settled sync was the initial ADD or a resynced
		// UPDATE: an object whose ADD failed transiently recovers through
		// the resync and must not leave the gate waiting forever.
		// vmnetcfgs with nics in the ERROR status settle as well because
		// their sync completed, and a definitively rejected sync settles
		// on either action so a broken vmnetcfg does not block the vm
		// controller startup forever: the settled classification is
		// definitive (a foreign ownership conflict needs an edit, a
		// networkname without a live pool needs its IPPool repaired, an
		// unusable macaddress needs a spec correction), so no retry of
		// the same object can protect an additional reservation - waiting
		// for the retry exhaustion would only delay the gate. a
		// transiently failed restore stays unsettled instead: the
		// rate-limited retry must stay able to protect the existing
		// reservation before the vm controller opens new allocations
		if err == nil || c.initSyncSettled(err) {
			c.markInitAttempt(event.key)
		}
	case DELETE:
		// an object which is gone can never produce a settled sync anymore:
		// the gate settles it so a startup-time deletion does not block the
		// controller startup forever
		c.markInitAttempt(event.key)

		// A queued deletion can outlive a same-name replacement. Verify
		// absence under the era mutex before draining the old pending work.
		namespace, name, splitErr := cache.SplitMetaNamespaceKey(event.key)
		if splitErr != nil {
			return splitErr
		}
		c.reconcileMu.Lock()
		live, readErr := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(namespace).Get(c.ctx, name, metav1.GetOptions{})
		if readErr == nil {
			c.reconcileMu.Unlock()
			return c.updateVirtualMachineNetworkConfig(UPDATE, live)
		}
		if !apierrors.IsNotFound(readErr) {
			c.reconcileMu.Unlock()
			return readErr
		}
		c.drainPendingUnwinds(event.key)
		c.reconcileMu.Unlock()
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
	// the gate settles it so the app startup does not wait forever for an
	// object which keeps failing
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

	// settle the snapshot keys whose object the informer never observed:
	// a vmnetcfg deleted between the startup LIST and the informer start
	// generates no event at all, so without this reconciliation the
	// membership gate would wait for it forever (a count-based gate hid
	// this case by letting unrelated objects substitute). the cache sync
	// guarantees the store holds the complete initial list, so a snapshot
	// key which is absent from it was deleted before the informer started
	// and can never settle otherwise
	if c.gate != nil {
		for _, key := range c.gate.Unsettled() {
			if _, exists, getErr := c.indexer.GetByKey(key); getErr == nil && !exists {
				log.Warnf("(vmnetcfg.Run) VirtualMachineNetworkConfig %s of the startup snapshot was deleted before the informer started, settling it for the startup gate",
					key)
				c.metrics.UpdateLogStatus("warning")

				c.markInitAttempt(key)
			}
		}
	}

	// the workers are joined before Run returns: the queue shutdown below
	// makes a worker which waits on the empty queue return, and a worker
	// which is mid-reconciliation finishes its in-flight item first, so an
	// event listener waiting for Run has really seen the last
	// reconciliation of the old generation when it proceeds
	var workerWg sync.WaitGroup
	for i := 0; i < workers; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			wait.Until(c.runWorker, time.Second, stopCh)
		}()
	}

	// requeue the pending nics deferred during the startup replay once
	// the initialization phase settled every object's durable
	// assignments
	go c.runDeferredInitAllocations(stopCh)

	<-stopCh
	log.Infof("(vmnetcfg.Run) stopping the VirtualMachineNetworkConfig controller")

	// shut the queue down before joining the workers: the shutdown is
	// what makes a worker blocked on the empty queue return, so it must
	// happen first or the join below would wait forever on it
	c.queue.ShutDown()
	workerWg.Wait()
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}
