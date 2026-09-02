package vmnetcfg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
)

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

func newTestController(t *testing.T, queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller, appStatus *int, vmnetcfgCountCurrent *int, kihClientset *kihclientset.Clientset) *Controller {
	t.Helper()

	controller := NewController(
		queue,
		indexer,
		informer,
		kihcache.NewCacheAllocator(),
		ipam.NewIPAllocator(),
		dhcp.NewDHCPAllocator(),
		metrics.NewMetricsAllocator(),
		kihClientset,
		appStatus,
		vmnetcfgCountCurrent,
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
	var appStatus, countCurrent int
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, &countCurrent, nil)

	queue.ShutDown()

	if got := controller.processNextItem(); got {
		t.Errorf("processNextItem() after ShutDown returned true, want false")
	}
}

func TestProcessNextItemSucceedsForMissingIndexObject(t *testing.T) {
	queue := newTestQueue()
	var appStatus, countCurrent int
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, &countCurrent, nil)

	event := testEvent(ADD)
	queue.Add(event)

	if got := controller.processNextItem(); !got {
		t.Fatalf("processNextItem() returned false, want true")
	}

	if n := queue.Len(); n != 0 {
		t.Errorf("queue has %d items after a successful sync, want 0", n)
	}
	if countCurrent != 0 {
		t.Errorf("counter incremented for a missing index object, want 0, got %d", countCurrent)
	}
}

func TestProcessNextItemRequeuesOnIndexerError(t *testing.T) {
	queue := newTestQueue()
	var appStatus, countCurrent int
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller := newTestController(t, queue, indexer, nil, &appStatus, &countCurrent, nil)

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
	var appStatus, countCurrent int
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, &countCurrent, nil)

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
	var appStatus, countCurrent int
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, &countCurrent, nil)

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
	var appStatus, countCurrent int
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, &countCurrent, nil)

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
	appStatus := APP_INIT
	countCurrent := 0
	controller := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, &countCurrent, nil)

	if err := controller.sync(testEvent(ADD)); err != nil {
		t.Errorf("sync() for a missing index object returned error %v, want nil", err)
	}
	if countCurrent != 0 {
		t.Errorf("counter incremented for a missing index object, want 0, got %d", countCurrent)
	}
}

func TestSyncReturnsIndexerError(t *testing.T) {
	appStatus := APP_INIT
	countCurrent := 0
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, &countCurrent, nil)

	if err := controller.sync(testEvent(ADD)); err == nil {
		t.Fatalf("sync() returned nil, want the indexer error")
	}
}

func TestSyncDeleteIsANoopWithPresentObject(t *testing.T) {
	// the controller has no DELETE branch: a delete event for an object that
	// still exists in the index is processed without any side effects
	appStatus := APP_INIT
	countCurrent := 0
	indexer := newTestIndexer()
	indexer.Add(testVMNetCfg(nil))
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, &countCurrent, nil)

	if err := controller.sync(testEvent(DELETE)); err != nil {
		t.Errorf("sync(DELETE) returned error %v, want nil", err)
	}
	if countCurrent != 0 {
		t.Errorf("counter incremented for a delete event, want 0, got %d", countCurrent)
	}
}

func TestSyncDeleteSnapshotIsANoop(t *testing.T) {
	// a delete snapshot (object already gone from the index) is also a
	// no-op
	appStatus := APP_INIT
	countCurrent := 0
	controller := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, &countCurrent, nil)

	if err := controller.sync(testEvent(DELETE)); err != nil {
		t.Errorf("sync(DELETE) returned error %v, want nil", err)
	}
	if countCurrent != 0 {
		t.Errorf("counter incremented for a delete snapshot, want 0, got %d", countCurrent)
	}
}

func TestSyncAddIncrementsCounterWhileInitializing(t *testing.T) {
	appStatus := APP_INIT
	countCurrent := 0
	indexer := newTestIndexer()
	indexer.Add(testVMNetCfg(nil))
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, &countCurrent, nil)

	if err := controller.sync(testEvent(ADD)); err != nil {
		t.Errorf("sync(ADD) returned error %v, want nil", err)
	}
	if countCurrent != 1 {
		t.Errorf("counter = %d after an add while initializing, want 1", countCurrent)
	}
}

func TestSyncAddDoesNotCountWhileRunning(t *testing.T) {
	appStatus := APP_RUNNING
	countCurrent := 0
	indexer := newTestIndexer()
	indexer.Add(testVMNetCfg(nil))
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, &countCurrent, nil)

	if err := controller.sync(testEvent(ADD)); err != nil {
		t.Errorf("sync(ADD) returned error %v, want nil", err)
	}
	if countCurrent != 0 {
		t.Errorf("counter = %d after an add while running, want 0", countCurrent)
	}
}

func TestSyncUpdateDoesNotIncrementCounterWhileInitializing(t *testing.T) {
	appStatus := APP_INIT
	countCurrent := 0
	indexer := newTestIndexer()
	indexer.Add(testVMNetCfg(nil))
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, &countCurrent, nil)

	if err := controller.sync(testEvent(UPDATE)); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}
	if countCurrent != 0 {
		t.Errorf("counter = %d after an update while initializing, want 0", countCurrent)
	}
}

func TestSyncAddCountsEvenWhenUpdateFails(t *testing.T) {
	// objects are counted even when their update fails, otherwise the
	// application would never become operational
	appStatus := APP_INIT
	countCurrent := 0
	indexer := newTestIndexer()
	indexer.Add(testVMNetCfg([]kihv1.NetworkConfig{
		{MACAddress: "02:00:00:00:00:01", NetworkName: "missing-net"},
	}))
	controller := newTestController(t, newTestQueue(), indexer, nil, &appStatus, &countCurrent, nil)

	if err := controller.sync(testEvent(ADD)); err != nil {
		t.Errorf("sync(ADD) returned error %v, want nil", err)
	}
	if countCurrent != 1 {
		t.Errorf("counter = %d after a failed update while initializing, want 1", countCurrent)
	}
}

func TestRunShutsDownTheQueue(t *testing.T) {
	queue := newTestQueue()
	var appStatus, countCurrent int
	controller := newTestController(t, queue, newTestIndexer(), &stubInformer{synced: true}, &appStatus, &countCurrent, nil)

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

func TestRunWorkerExitsWhenQueueShutsDown(t *testing.T) {
	queue := newTestQueue()
	var appStatus, countCurrent int
	controller := newTestController(t, queue, newTestIndexer(), nil, &appStatus, &countCurrent, nil)

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
		new(int),
		new(int),
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
