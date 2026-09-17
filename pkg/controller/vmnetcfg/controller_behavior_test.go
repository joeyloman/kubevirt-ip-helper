package vmnetcfg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/gate"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
)

// newTestGate builds a startup gate whose snapshot holds the given keys:
// the settle assertions of the tests observe the membership contract, so
// the key a test syncs must be part of the snapshot for its settlement to
// count.
func newTestGate(keys ...string) *gate.Gate {
	startupGate := gate.New()
	startupGate.SetTarget(keys)

	return startupGate
}

// shutdownWait is the bounded amount of time a test waits for a controller
// goroutine to observe a queue or context shutdown before failing.
const shutdownWait = 5 * time.Second

// newTestQueue returns a rate limiting queue whose rate limiter adds items
// synchronously (zero delay), so queue length and requeue counts can be
// asserted immediately after handleErr without sleeping.
func newTestQueue() workqueue.RateLimitingInterface {
	return workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(0, 0))
}

func newTestIndexer() cache.Indexer {
	return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
}

// failingIndexer is a cache.Indexer whose GetByKey always fails, simulating a
// broken underlying store.
type failingIndexer struct {
	cache.Indexer
	err error
}

func (f *failingIndexer) GetByKey(key string) (interface{}, bool, error) {
	return nil, false, f.err
}

// stubInformer implements cache.Controller without touching any cluster.
type stubInformer struct {
	synced bool
}

func (s *stubInformer) Run(stopCh <-chan struct{})      {}
func (s *stubInformer) HasSynced() bool                 { return s.synced }
func (s *stubInformer) LastSyncResourceVersion() string { return "" }

func newTestController(t *testing.T, queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller, appStatus *atomic.Int32, startupGate *gate.Gate, kihClientset *kihclientset.Clientset) *Controller {
	t.Helper()
	if kihClientset == nil {
		e := newTestEnv(t)
		for _, obj := range indexer.List() {
			e.seedVMNetCfg(obj.(*kihv1.VirtualMachineNetworkConfig))
		}
		kihClientset = e.client
	}

	controller := NewController(
		context.Background(),
		queue,
		indexer,
		informer,
		kihcache.NewCacheAllocator(),
		ipam.NewIPAllocator(),
		dhcp.NewDHCPAllocator(),
		metrics.NewMetricsAllocator(),
		kihClientset,
		appStatus,
		startupGate,
		testNetworkScope(t, testNamespace, "net-test"),
		&sync.Mutex{},
	)
	t.Cleanup(queue.ShutDown)

	return controller
}

// newUnavailableClientset returns a generated clientset pointing at a local
// server that answers every request with an error, without contacting a real
// cluster.
func newUnavailableClientset(t *testing.T) *kihclientset.Clientset {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	client, err := kihclientset.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}

	return client
}

func testEvent(action string) Event {
	return Event{key: "default/vm-test", action: action}
}

func testVMNetCfg(networkConfigs []kihv1.NetworkConfig) *kihv1.VirtualMachineNetworkConfig {
	return &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-test", Namespace: "default"},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        "vm-test",
			NetworkConfig: networkConfigs,
		},
	}
}

func TestProcessNextItemReturnsFalseAfterShutdown(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, startupGate, nil)

	queue.ShutDown()

	if got := controller.processNextItem(); got {
		t.Errorf("processNextItem() after ShutDown returned true, want false")
	}
}

func TestProcessNextItemSucceedsForMissingIndexObject(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, startupGate, nil)

	event := testEvent(ADD)
	queue.Add(event)

	if got := controller.processNextItem(); !got {
		t.Fatalf("processNextItem() returned false, want true")
	}

	if n := queue.Len(); n != 0 {
		t.Errorf("queue has %d items after a successful sync, want 0", n)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("counter for a missing index object: got %d, want 1; the vanished object counts as handled so it cannot block the startup gate", startupGate.Settled())
	}
}

func TestProcessNextItemRequeuesOnIndexerError(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller := newTestController(t, queue, indexer, nil, &appStatus, startupGate, nil)

	event := testEvent(ADD)
	queue.Add(event)

	if got := controller.processNextItem(); !got {
		t.Fatalf("processNextItem() returned false, want true")
	}

	if n := queue.Len(); n != 1 {
		t.Errorf("queue has %d items after a sync error, want 1 (rate limited requeue)", n)
	}
}

func TestHandleErrForgetsOnSuccess(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, startupGate, nil)

	key := "default/vm-test"
	queue.Add(key)
	item, quit := queue.Get()
	if quit {
		t.Fatalf("queue was shut down while getting the item")
	}

	controller.handleErr(nil, item)
	queue.Done(item)

	if n := queue.Len(); n != 0 {
		t.Errorf("queue has %d items after a successful sync, want 0", n)
	}
	if n := queue.NumRequeues(key); n != 0 {
		t.Errorf("NumRequeues after a successful sync = %d, want 0", n)
	}
}

func TestHandleErrRateLimitsOnFailure(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, startupGate, nil)

	key := "default/vm-test"
	queue.Add(key)
	item, quit := queue.Get()
	if quit {
		t.Fatalf("queue was shut down while getting the item")
	}

	controller.handleErr(errors.New("boom"), item)
	queue.Done(item)

	if n := queue.Len(); n != 1 {
		t.Errorf("queue has %d items after an error, want 1 (rate limited requeue)", n)
	}
	if n := queue.NumRequeues(key); n != 1 {
		t.Errorf("NumRequeues after one error = %d, want 1", n)
	}
}

func TestHandleErrDropsAfterMaxRequeues(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, startupGate, nil)

	key := "default/vm-test"
	syncErr := errors.New("persistent failure")

	// the first five failures are rate limited and requeued
	for i := 0; i < 5; i++ {
		queue.Add(key)
		item, quit := queue.Get()
		if quit {
			t.Fatalf("queue was shut down while getting the item")
		}

		controller.handleErr(syncErr, item)
		queue.Done(item)

		if n := queue.Len(); n != 1 {
			t.Fatalf("iteration %d: queue has %d items, want 1", i, n)
		}
	}

	// the next failure exceeds the retry threshold and is dropped
	queue.Add(key)
	item, quit := queue.Get()
	if quit {
		t.Fatalf("queue was shut down while getting the item")
	}
	controller.handleErr(syncErr, item)
	queue.Done(item)

	if n := queue.Len(); n != 0 {
		t.Errorf("queue has %d items after dropping the item, want 0", n)
	}
	if n := queue.NumRequeues(key); n != 0 {
		t.Errorf("NumRequeues after Forget = %d, want 0", n)
	}
}

func TestSyncReturnsNilForMissingIndexObject(t *testing.T) {
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	appStatus.Store(APP_INIT)
	controller := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate, nil)

	if err := controller.sync(testEvent(ADD)); err != nil {
		t.Errorf("sync() for a missing index object returned error %v, want nil", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("counter for a missing index object: got %d, want 1; the vanished object counts as handled so it cannot block the startup gate", startupGate.Settled())
	}
}

func TestSyncReturnsIndexerError(t *testing.T) {
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	appStatus.Store(APP_INIT)
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate, nil)

	if err := controller.sync(testEvent(ADD)); err == nil {
		t.Fatalf("sync() returned nil, want the indexer error")
	}
}

func TestSyncDeleteCountsObjectForStartupGate(t *testing.T) {
	// a delete event for an object that still exists in the index settles
	// the startup gate: the object can never produce a settled sync again
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	appStatus.Store(APP_INIT)
	indexer := newTestIndexer()
	indexer.Add(testVMNetCfg(nil))
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate, nil)

	if err := controller.sync(testEvent(DELETE)); err != nil {
		t.Errorf("sync(DELETE) returned error %v, want nil", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("counter = %d after a delete event, want 1 (the object settles the gate exactly once)", startupGate.Settled())
	}
}

func TestSyncDeleteSnapshotCountsObjectForStartupGate(t *testing.T) {
	// a delete snapshot (object already gone from the index) settles the
	// startup gate too: the object can never sync anymore
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	appStatus.Store(APP_INIT)
	controller := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate, nil)

	if err := controller.sync(testEvent(DELETE)); err != nil {
		t.Errorf("sync(DELETE) returned error %v, want nil", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("counter = %d after a delete snapshot, want 1", startupGate.Settled())
	}
}

func TestSyncAddIncrementsCounterWhileInitializing(t *testing.T) {
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	appStatus.Store(APP_INIT)
	indexer := newTestIndexer()
	indexer.Add(testVMNetCfg(nil))
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate, nil)

	if err := controller.sync(testEvent(ADD)); err != nil {
		t.Errorf("sync(ADD) returned error %v, want nil", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("counter = %d after an add while initializing, want 1", startupGate.Settled())
	}
}

func TestSyncAddDoesNotCountWhileRunning(t *testing.T) {
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	appStatus.Store(APP_RUNNING)
	indexer := newTestIndexer()
	indexer.Add(testVMNetCfg(nil))
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate, nil)

	if err := controller.sync(testEvent(ADD)); err != nil {
		t.Errorf("sync(ADD) returned error %v, want nil", err)
	}
	if startupGate.Settled() != 0 {
		t.Errorf("counter = %d after an add while running, want 0", startupGate.Settled())
	}
}

func TestSyncUpdateSuccessCountsForStartupGate(t *testing.T) {
	// a successful sync settles the startup gate also when it arrived as a
	// resynced UPDATE: an object whose initial ADD failed transiently must
	// not leave the gate waiting forever after its recovery
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	appStatus.Store(APP_INIT)
	indexer := newTestIndexer()
	indexer.Add(testVMNetCfg(nil))
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate, nil)

	if err := controller.sync(testEvent(UPDATE)); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("counter = %d after an update while initializing, want 1", startupGate.Settled())
	}
}

// a failed update returns the error so the queue requeues the event
// rate-limited; during the initialization phase the object still counts
// as handled for the startup gate exactly once: a vmnetcfg which cannot
// complete its sync (its networkname has no live registration) must not
// block the vm controller startup forever, and the doubled attempts of
// the requeue must not overcount past the target
func TestSyncAddFailureCountsAsHandledForStartupGate(t *testing.T) {
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	appStatus.Store(APP_INIT)
	indexer := newTestIndexer()
	indexer.Add(testVMNetCfg([]kihv1.NetworkConfig{
		{MACAddress: "02:00:00:00:00:01", NetworkName: testNetwork},
	}))
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate, nil)

	if err := controller.sync(testEvent(ADD)); err == nil {
		t.Error("sync(ADD) returned nil, want a rate-limited requeue error for the failed update")
	}
	if startupGate.Settled() != 1 {
		t.Errorf("counter = %d after a failed update, want 1: the startup gate counts handled objects once", startupGate.Settled())
	}

	// the rate-limited retry of the same event must not double count
	if err := controller.sync(testEvent(ADD)); err == nil {
		t.Fatal("the retried sync(ADD) returned nil, want a sync error")
	}
	if startupGate.Settled() != 1 {
		t.Errorf("counter = %d after the retried event, want 1", startupGate.Settled())
	}
}

func TestRunShutsDownTheQueue(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	controller := newTestController(t, queue, newTestIndexer(), &stubInformer{synced: true}, &appStatus, startupGate, nil)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		controller.Run(1, stop)
		close(done)
	}()

	close(stop)

	select {
	case <-done:
	case <-time.After(shutdownWait):
		t.Fatal("Run did not return after the stop channel was closed")
	}

	if !queue.ShuttingDown() {
		t.Errorf("queue was not shut down after Run returned")
	}
}

// Run joins its workers before returning: an event listener which waits for
// Run must have seen the last in-flight reconciliation of the old
// generation, so a worker which is mid-reconciliation finishes its item
// first instead of mutating state behind the new era's back
func TestRunJoinsTheInFlightSyncBeforeReturning(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)

	// the pool status write parks until the test releases it, so the worker
	// is deterministically inside its in-flight reconciliation. the release
	// runs as a cleanup as well: a parked handler would otherwise block the
	// httptest server shutdown forever when an earlier assertion fails
	block := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(block) }) }
	t.Cleanup(unblock)
	e.api.blockPoolStatusPut = block

	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	queue := newTestQueue()
	indexer := newTestIndexer()
	if err := indexer.Add(vmnetcfg); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}

	controller := NewController(
		context.Background(),
		queue,
		indexer,
		&stubInformer{synced: true},
		e.cache,
		e.ipam,
		e.dhcp,
		e.metrics,
		e.client,
		e.appStatus,
		nil,
		e.scope,
		e.reconcileMu,
	)
	queue.Add(Event{key: testNamespace + "/" + testVMNetCfgName, action: ADD})

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		controller.Run(1, stop)
		close(done)
	}()

	// wait until the worker parks inside the blocked pool status write
	deadline := time.Now().Add(shutdownWait)
	for e.countRequests(http.MethodPut, ippoolStatusPath) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker never reached the blocked pool status write")
		}
		time.Sleep(time.Millisecond)
	}

	close(stop)

	// Run must not return while the in-flight sync is still parked
	select {
	case <-done:
		t.Fatal("Run returned before its in-flight worker sync finished")
	case <-time.After(time.Second):
	}

	// releasing the parked write lets the sync finish, and only then Run
	// returns: the join really waited for the last reconciliation
	unblock()

	select {
	case <-done:
	case <-time.After(shutdownWait):
		t.Fatal("Run did not return after the in-flight sync finished")
	}

	if got := e.getStoredVMNetCfg().Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" {
		t.Errorf("spec ip = %q, want the in-flight sync committed before Run returned", got)
	}
	if !queue.ShuttingDown() {
		t.Errorf("queue was not shut down after Run returned")
	}
}

func TestRunWorkerExitsWhenQueueShutsDown(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	startupGate := newTestGate("default/vm-test")
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, startupGate, nil)

	queue.ShutDown()

	done := make(chan struct{})
	go func() {
		controller.runWorker()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(shutdownWait):
		t.Fatal("runWorker did not exit after the queue was shut down")
	}
}

func TestEventListenerStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	handler := NewEventHandler(
		ctx,
		ipam.NewIPAllocator(),
		dhcp.NewDHCPAllocator(),
		metrics.NewMetricsAllocator(),
		kihcache.NewCacheAllocator(),
		"",
		"",
		nil,
		newUnavailableClientset(t),
		new(atomic.Int32),
		nil,
		testNetworkScope(t, testNamespace, "net-test"),
		&sync.Mutex{},
	)

	done := make(chan error, 1)
	go func() {
		done <- handler.EventListener()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("EventListener returned error %v, want nil", err)
		}
	case <-time.After(shutdownWait):
		t.Fatal("EventListener did not return after context cancellation")
	}
}

// a vmnetcfg of the startup snapshot which was deleted before the informer
// started generates no event at all: Run must settle it through the store
// reconcile after the cache sync, or the membership gate would wait for it
// forever (a count-based gate hid this case by letting unrelated objects
// substitute)
func TestRunSettlesSnapshotKeysDeletedBeforeTheInformerStarted(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)

	// the store holds another vmnetcfg but not the snapshot's default/vm-gone:
	// its deletion happened between the startup LIST and the informer start
	indexer := newTestIndexer()
	if err := indexer.Add(testVMNetCfg(nil)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	startupGate := newTestGate("default/vm-gone", "default/vm-test")
	controller := newTestController(t, newTestQueue(), indexer, &stubInformer{synced: true}, &appStatus, startupGate, nil)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		controller.Run(1, stop)
		close(done)
	}()

	close(stop)

	select {
	case <-done:
	case <-time.After(shutdownWait):
		t.Fatal("Run did not return after the stop channel was closed")
	}

	// the never-observed deletion settled through the store reconcile;
	// default/vm-test settles through its own events in production, so it
	// stays pending here
	for _, key := range startupGate.Unsettled() {
		if key == "default/vm-gone" {
			t.Errorf("the never-observed deletion %s stayed unsettled: Run must settle it through the store reconcile", key)
		}
	}
}
