package ippool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
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

func newTestController(t *testing.T, queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller, appStatus *atomic.Int32, startupGate *gate.Gate) (*Controller, *kihcache.CacheAllocator) {
	t.Helper()

	scope := testNetworkScope("infra/net-a")
	for _, obj := range indexer.List() {
		if pool, ok := obj.(*kihv1.IPPool); ok {
			scope = testNetworkScope(pool.Spec.NetworkName)
			break
		}
	}
	// The fixture's API follows its selected objects unless a lifecycle test
	// supplies an independent recovery server to model informer lag.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/apis/kubevirtiphelper.k8s.binbash.org/v1/ippools/")
		if obj, exists, err := indexer.GetByKey(name); err == nil && exists {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(obj)
			return
		}
		ippoolBehaviorWriteKubeError(w, http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	client, err := kihclientset.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("creating fixture client: %v", err)
	}

	cacheAllocator := kihcache.NewCacheAllocator()
	controller := NewController(
		queue,
		indexer,
		informer,
		context.Background(),
		cacheAllocator,
		ipam.NewIPAllocator(),
		dhcp.NewDHCPAllocator(),
		metrics.NewMetricsAllocator(),
		client,
		appStatus,
		startupGate,
		nil,
		scope,
	)
	t.Cleanup(queue.ShutDown)

	return controller, cacheAllocator
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

func testNetworkScope(network string) util.NetworkScope {
	namespace, name, ok := strings.Cut(network, "/")
	if !ok {
		panic("test network must be namespace-qualified: " + network)
	}
	scope, err := util.NewNetworkScope(namespace, name)
	if err != nil {
		panic(err)
	}
	return scope
}

func testPoolMetadata(name, network string) metav1.ObjectMeta {
	scope := testNetworkScope(network)
	return metav1.ObjectMeta{Name: name, Labels: map[string]string{
		util.NetworkLabel: scope.Name(), util.NetworkNamespaceLabel: scope.Namespace(),
	}}
}

func testPool(name, network string, leaseTime int) *kihv1.IPPool {
	return &kihv1.IPPool{
		ObjectMeta: testPoolMetadata(name, network),
		Spec: kihv1.IPPoolSpec{
			NetworkName:   network,
			BindInterface: "test-fake-iface",
			IPv4Config: kihv1.IPv4Config{
				ServerIP:  "192.168.1.1",
				Subnet:    "192.168.1.0/24",
				Pool:      kihv1.Pool{Start: "192.168.1.10", End: "192.168.1.100"},
				Router:    "192.168.1.1",
				LeaseTime: leaseTime,
			},
		},
	}
}

// TestSyncDefersAddDuringRestart pins the p3 finding: the ADD path used
// to register unconditionally while the UPDATE re-registration path
// already deferred during APP_RESTART - an add processed in the window
// between the restart-triggering update and the era cancel ran a full
// registration on the dying era, which the immediately following
// teardown then tore back down. the deferred add fails the sync so the
// requeue retries it, and the next era's resync re-delivers it in any
// case.
func TestSyncDefersAddDuringRestart(t *testing.T) {
	pool := testPool("pool-defer", "infra/net-defer", 60)
	c, _, _ := recoveryNewController(t, pool)

	// the sync-level event drives the object through the indexer, and
	// the restart phase decides the deferral (the recovery harness keeps
	// the appStatus running for its own direct registration calls)
	indexer := newTestIndexer()
	if err := indexer.Add(pool); err != nil {
		t.Fatalf("indexing the pool: %v", err)
	}
	c.indexer = indexer
	var appStatus atomic.Int32
	appStatus.Store(APP_RESTART)
	c.appStatus = &appStatus

	event := Event{
		key:             "pool-defer",
		action:          ADD,
		poolName:        "pool-defer",
		poolNetworkName: "infra/net-defer",
	}

	err := c.sync(event)
	if err == nil || !strings.Contains(err.Error(), "deferring registration") {
		t.Fatalf("sync of an add during the restart = %v, want the deferral", err)
	}
	if c.dhcp.CheckPool("infra/net-defer") {
		t.Error("a dying era must not register the pool's dhcp service")
	}
	if used := c.ipam.Used("infra/net-defer"); used != 0 {
		t.Errorf("ipam used = %d, want 0: the deferred add must not register a subnet", used)
	}

	// once the new era runs, the retried add registers regularly
	appStatus.Store(APP_RUNNING)
	if err := c.sync(event); err != nil {
		t.Fatalf("the retried add after the restart must register: %v", err)
	}
	if !c.dhcp.CheckPool("infra/net-defer") {
		t.Error("the retried add must register the dhcp pool once the era runs")
	}
}

func testPoolEvent(key, action, networkName string) Event {
	return Event{key: key, action: action, poolName: key, poolNetworkName: networkName}
}

func TestProcessNextItemReturnsFalseAfterShutdown(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, nil)

	queue.ShutDown()

	if got := controller.processNextItem(); got {
		t.Errorf("processNextItem() after ShutDown returned true, want false")
	}
}

func TestProcessNextItemSucceedsForMissingIndexObject(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, nil)

	event := testPoolEvent("pool-a", ADD, "infra/net-a")
	queue.Add(event)

	if got := controller.processNextItem(); !got {
		t.Fatalf("processNextItem() returned false, want true")
	}

	if n := queue.Len(); n != 0 {
		t.Errorf("queue has %d items after a successful sync, want 0", n)
	}
}

func TestProcessNextItemRequeuesOnIndexerError(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller, _ := newTestController(t, queue, indexer, nil, &appStatus, nil)

	event := testPoolEvent("pool-b", ADD, "infra/net-b")
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
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, nil)

	key := "pool-c"
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
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, nil)

	key := "pool-d"
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
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, nil)

	key := "pool-e"
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
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, nil)

	if err := controller.sync(testPoolEvent("pool-f", ADD, "infra/net-f")); err != nil {
		t.Errorf("sync() for a missing index object returned error %v, want nil", err)
	}
}

func TestSyncReturnsIndexerError(t *testing.T) {
	var appStatus atomic.Int32
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	if err := controller.sync(testPoolEvent("pool-g", ADD, "infra/net-g")); err == nil {
		t.Fatalf("sync() returned nil, want the indexer error")
	}
}

func TestSyncDeleteSucceedsWhenPoolNotCached(t *testing.T) {
	// a DELETE snapshot for a pool that is gone from the index and unknown to
	// the cache only logs the cache lookup failure and returns without error
	var appStatus atomic.Int32
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, nil)

	if err := controller.sync(testPoolEvent("pool-h", DELETE, "infra/net-h")); err != nil {
		t.Errorf("sync(DELETE) returned error %v, want nil", err)
	}
}

func TestSyncUpdateReturnsErrorWhenPoolNotCached(t *testing.T) {
	// an UPDATE event whose pool has no live registration becomes a
	// registration attempt; in this environment the attempt fails at the
	// netlink step (no test-fake-iface), which still returns an error so
	// the queue retries instead of silently forgetting the event
	var appStatus atomic.Int32
	indexer := newTestIndexer()
	indexer.Add(testPool("pool-i", "infra/net-i", 60))
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	if err := controller.sync(testPoolEvent("pool-i", UPDATE, "infra/net-i")); err == nil {
		t.Error("sync(UPDATE) returned nil, want a rate-limited requeue error for the missing cache entry")
	}
}

func TestSyncUpdateIgnoredWhileInitializing(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	oldPool := testPool("pool-j", "infra/net-j", 60)
	newPool := testPool("pool-j", "infra/net-j", 120)

	indexer := newTestIndexer()
	indexer.Add(newPool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	listenerRepairs := 0
	controller.runListener = func(networkName string, nic string) error {
		listenerRepairs++
		return nil
	}

	if err := controller.sync(testPoolEvent("pool-j", UPDATE, "infra/net-j")); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}

	// while initializing, pool updates are deliberately ignored: the cache
	// still holds the originally registered pool, and the listener repair
	// must not open sockets during the startup replay (the registration
	// phase owns the listener lifecycle)
	if listenerRepairs != 0 {
		t.Errorf("listener repair attempts = %d, want 0 while the application initializes", listenerRepairs)
	}
	got, err := cacheAllocator.Get("pool", "infra/net-j")
	if err != nil {
		t.Fatalf("pool missing from cache: %v", err)
	}
	if leaseTime := got.(kihv1.IPPool).Spec.IPv4Config.LeaseTime; leaseTime != 60 {
		t.Errorf("cache lease time = %d after ignored update, want 60", leaseTime)
	}
}

func TestSyncUpdateSkipsIdenticalPoolWhenRunning(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)
	pool := testPool("pool-k", "infra/net-k", 60)

	indexer := newTestIndexer()
	indexer.Add(pool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	listenerRepairs := 0
	controller.runListener = func(networkName string, nic string) error {
		listenerRepairs++
		return nil
	}

	if err := controller.sync(testPoolEvent("pool-k", UPDATE, "infra/net-k")); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}

	// an identical object is a no-change: the cache keeps the original pool
	// and no dhcp pool is (re)registered
	got, err := cacheAllocator.Get("pool", "infra/net-k")
	if err != nil {
		t.Fatalf("pool missing from cache: %v", err)
	}
	if leaseTime := got.(kihv1.IPPool).Spec.IPv4Config.LeaseTime; leaseTime != 60 {
		t.Errorf("cache lease time = %d after no-change update, want 60", leaseTime)
	}
	if controller.dhcp.CheckPool("infra/net-k") {
		t.Errorf("dhcp pool registered for an identical update")
	}

	// the pool has no running listener in this fixture, so the same event
	// re-serves it through the repair seam: a died listener must not stay
	// dead until an operator edits the pool or the pod restarts
	if listenerRepairs != 1 {
		t.Errorf("listener repair attempts = %d, want 1 (the identified no-change event re-serves the listener)", listenerRepairs)
	}
}

// TestSyncUpdateListenerRepairFailsLoudly: a repair which cannot re-open
// the socket surfaces the failure so the queue retries it instead of
// silently leaving the pool unserved.
func TestSyncUpdateListenerRepairFailsLoudly(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)
	pool := testPool("pool-m2", "infra/net-m2", 60)

	indexer := newTestIndexer()
	indexer.Add(pool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	controller.runListener = func(networkName string, nic string) error {
		return errors.New("cannot bind to interface test-fake-iface: no such device")
	}

	if err := controller.sync(testPoolEvent("pool-m2", UPDATE, "infra/net-m2")); err == nil {
		t.Error("sync(UPDATE) returned nil, want the listener repair failure surfaced for the rate-limited retry")
	}
}

func TestSyncUpdateReloadsPoolWhenRunning(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)
	oldPool := testPool("pool-l", "infra/net-l", 60)
	newPool := testPool("pool-l", "infra/net-l", 120)

	indexer := newTestIndexer()
	indexer.Add(newPool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	controller.runListener = func(networkName string, nic string) error { return nil }

	if err := controller.sync(testPoolEvent("pool-l", UPDATE, "infra/net-l")); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}

	// a lease time change is reloadable: the dhcp pool is refreshed and the
	// cache now carries the updated pool
	if !controller.dhcp.CheckPool("infra/net-l") {
		t.Errorf("dhcp pool was not registered after a reloadable update")
	}
	got, err := cacheAllocator.Get("pool", "infra/net-l")
	if err != nil {
		t.Fatalf("pool missing from cache: %v", err)
	}
	if leaseTime := got.(kihv1.IPPool).Spec.IPv4Config.LeaseTime; leaseTime != 120 {
		t.Errorf("cache lease time = %d after reload, want 120", leaseTime)
	}
}

func TestRunShutsDownTheQueue(t *testing.T) {
	queue := newTestQueue()
	var appStatus atomic.Int32
	controller, _ := newTestController(t, queue, newTestIndexer(), &stubInformer{synced: true}, &appStatus, nil)

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
	var appStatus atomic.Int32
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, nil)

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
		testNetworkScope("infra/net-a"),
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

func TestEventListenerScopesInitialListAndWatchByBothLabels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type observation struct {
		watch    bool
		selector string
	}
	requests := make(chan observation, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/kubevirtiphelper.k8s.binbash.org/v1/ippools" {
			ippoolBehaviorWriteKubeError(w, http.StatusNotFound)
			return
		}
		watching := r.URL.Query().Get("watch") == "true"
		select {
		case requests <- observation{watch: watching, selector: r.URL.Query().Get("labelSelector")}:
		case <-ctx.Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if watching {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-ctx.Done()
			return
		}
		_ = json.NewEncoder(w).Encode(&kihv1.IPPoolList{
			TypeMeta: metav1.TypeMeta{APIVersion: kihv1.SchemeGroupVersion.String(), Kind: "IPPoolList"},
			ListMeta: metav1.ListMeta{ResourceVersion: "1"},
			Items:    []kihv1.IPPool{},
		})
	}))
	t.Cleanup(server.Close)
	t.Cleanup(cancel)
	client, err := kihclientset.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	scope := testNetworkScope("infra/net-a")
	handler := NewEventHandler(
		ctx, ipam.NewIPAllocator(), dhcp.NewDHCPAllocator(), metrics.NewMetricsAllocator(),
		kihcache.NewCacheAllocator(), "", "", nil, client, new(atomic.Int32), newTestGate(), scope,
	)
	done := make(chan error, 1)
	go func() { done <- handler.EventListener() }()
	deadline := time.NewTimer(shutdownWait)
	defer deadline.Stop()
	seenList, seenWatch := false, false
	for !seenList || !seenWatch {
		select {
		case request := <-requests:
			if request.watch {
				seenWatch = true
			} else {
				seenList = true
			}
			selector, err := labels.Parse(request.selector)
			if err != nil {
				t.Fatalf("invalid discovery selector: %v", err)
			}
			if !selector.Matches(labels.Set{util.NetworkLabel: "net-a", util.NetworkNamespaceLabel: "infra"}) ||
				selector.Matches(labels.Set{util.NetworkLabel: "net-a", util.NetworkNamespaceLabel: "tenant"}) ||
				selector.Matches(labels.Set{util.NetworkLabel: "net-b", util.NetworkNamespaceLabel: "infra"}) ||
				selector.Matches(labels.Set{util.NetworkLabel: "net-a"}) {
				t.Errorf("watch=%v selector does not isolate the NAD namespace: %q", request.watch, request.selector)
			}
		case <-deadline.C:
			t.Fatalf("discovery did not reach both requests: list=%v watch=%v", seenList, seenWatch)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(shutdownWait):
		t.Fatal("scoped event listener did not stop after cancellation")
	}
}

// writeTempKubeconfig writes content into a fresh file under the test's temp
// directory and returns its path. The file is removed with the test.
func writeTempKubeconfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}

	return path
}

// testKubeconfig is a minimal, self-contained kubeconfig that resolves
// without contacting any cluster.
const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: test-cluster
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test-context
  context:
    cluster: test-cluster
    user: test-user
current-context: test-context
users:
- name: test-user
  user:
    token: test-token
`

// newTestEventHandler builds an EventHandler with fresh in-memory allocators and
// the given kubeconfig settings, suitable for getKubeConfig/Init tests.
func newTestEventHandler(kubeConfig, kubeContext string) *EventHandler {
	return NewEventHandler(
		context.Background(),
		ipam.NewIPAllocator(),
		dhcp.NewDHCPAllocator(),
		metrics.NewMetricsAllocator(),
		kihcache.NewCacheAllocator(),
		kubeConfig,
		kubeContext,
		nil,
		nil,
		new(atomic.Int32),
		nil,
		testNetworkScope("infra/net-a"),
	)
}

func TestEventHandlerGetKubeConfigLoadsExplicitFile(t *testing.T) {
	handler := newTestEventHandler(writeTempKubeconfig(t, testKubeconfig), "")

	config, err := handler.getKubeConfig()
	if err != nil {
		t.Fatalf("getKubeConfig() returned error: %v", err)
	}
	if config == nil {
		t.Fatal("getKubeConfig() returned a nil config")
	}
	if config.Host != "https://127.0.0.1:6443" {
		t.Errorf("config host = %q, want https://127.0.0.1:6443", config.Host)
	}
}

func TestEventHandlerGetKubeConfigMissingFileFallsBackToInCluster(t *testing.T) {
	// outside of a real cluster in-cluster config loading always fails
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	handler := newTestEventHandler(filepath.Join(t.TempDir(), "does-not-exist"), "")

	config, err := handler.getKubeConfig()
	if err == nil {
		t.Fatal("getKubeConfig() with a missing file returned nil error, want the in-cluster config error")
	}
	if config != nil {
		t.Errorf("getKubeConfig() returned config %v alongside an error, want nil", config)
	}
}

func TestEventHandlerGetKubeConfigRejectsMalformedFile(t *testing.T) {
	handler := newTestEventHandler(writeTempKubeconfig(t, "not: [valid yaml"), "")

	if config, err := handler.getKubeConfig(); err == nil {
		t.Fatalf("getKubeConfig() with a malformed file returned nil error (config %v)", config)
	}
}

func TestEventHandlerGetKubeConfigRejectsUnknownContext(t *testing.T) {
	handler := newTestEventHandler(writeTempKubeconfig(t, testKubeconfig), "missing-context")

	if config, err := handler.getKubeConfig(); err == nil {
		t.Fatalf("getKubeConfig() with an unknown context returned nil error (config %v)", config)
	}
}

func TestEventHandlerInitSucceedsWithKubeconfig(t *testing.T) {
	handler := newTestEventHandler(writeTempKubeconfig(t, testKubeconfig), "")

	if err := handler.Init(); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if handler.kubeRestConfig == nil {
		t.Error("Init() left kubeRestConfig nil, want a rest config")
	}
	if handler.kihClientset == nil {
		t.Error("Init() left kihClientset nil, want a generated clientset")
	}
}

func TestEventHandlerInitFailsOnMalformedKubeconfig(t *testing.T) {
	handler := newTestEventHandler(writeTempKubeconfig(t, "this is: not: [valid"), "")

	if err := handler.Init(); err == nil {
		t.Fatal("Init() with a malformed kubeconfig returned nil error")
	}
	if handler.kihClientset != nil {
		t.Error("Init() must not build a clientset when the kubeconfig fails to load")
	}
}
func TestSyncAddReturnsErrorWhenPoolFailsToRegister(t *testing.T) {
	// an ADD event whose pool has an invalid subnet fails inside registerIPPool
	// before any netlink or dhcp work; sync must return the error so the
	// queue applies a rate-limited requeue. A nil error would Forget the
	// event and the successful-pool counter would never reach the target
	// count, blocking initialization forever.
	pool := testPool("pool-m", "infra/net-m", 60)
	pool.Spec.IPv4Config.Subnet = "not-a-cidr"

	indexer := newTestIndexer()
	if err := indexer.Add(pool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	var appStatus atomic.Int32
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	if err := controller.sync(testPoolEvent("pool-m", ADD, "infra/net-m")); err == nil {
		t.Error("sync(ADD) returned nil, want a rate-limited requeue error from the registration failure")
	}

	if controller.dhcp.CheckPool("infra/net-m") {
		t.Errorf("dhcp pool registered although registerIPPool failed")
	}
	if v, ok := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_app_logs", map[string]string{"loglevel": "error"}); !ok || v != 1 {
		t.Errorf("app log status gauge: got value %v found %v, want exactly 1 error entry", v, ok)
	}
}

// two IPPool objects sharing a networkname must not be able to tear down
// the live sub-resources of the registered one: the ADD of the duplicate
// is rejected before any pool state is created, so every failure path
// of the registration stays scoped to its own keys
func TestSyncAddDuplicateNetworkNameDoesNotTouchForeignState(t *testing.T) {
	var appStatus atomic.Int32
	foreignPool := testPool("pool-a", "infra/net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(foreignPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}
	if err := indexer.Add(testPool("pool-b", "infra/net-dup", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	// a foreign pool registration already owns the net-dup keys and holds
	// one live allocation
	if err := controller.ipam.NewSubnet("infra/net-dup", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the foreign ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("infra/net-dup", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the foreign live ip: %v", err)
	}
	if err := controller.dhcp.AddPool("infra/net-dup", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
		t.Fatalf("registering the foreign dhcp pool: %v", err)
	}
	if err := cacheAllocator.Add(foreignPool); err != nil {
		t.Fatalf("caching the foreign pool: %v", err)
	}

	obj, _, _ := indexer.GetByKey("pool-b")
	if cleanup, err := controller.registerIPPool(obj.(*kihv1.IPPool)); err == nil {
		t.Fatal("registerIPPool accepted an already-claimed networkname")
	} else if cleanup {
		t.Error("registerIPPool requested cleanup for an already-claimed networkname, want the foreign state untouched")
	}

	if err := controller.sync(testPoolEvent("pool-b", ADD, "infra/net-dup")); err == nil {
		t.Fatal("sync(ADD) for an already-claimed networkname returned nil, want a rejection error")
	} else if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("error = %v, want a networkname-claim rejection", err)
	}

	// the foreign registration must survive both rejections untouched
	if used := controller.ipam.Used("infra/net-dup"); used < 1 {
		t.Errorf("foreign allocation state of net-dup wiped: used=%d, want >= 1", used)
	}
	if _, err := controller.ipam.GetIP("infra/net-dup", ""); err != nil {
		t.Errorf("GetIP on the foreign subnet failed: %v, want the subnet to stay live", err)
	}
	if !controller.dhcp.CheckPool("infra/net-dup") {
		t.Error("the foreign dhcp pool was removed by the rejected duplicate ADD")
	}
	if !cacheAllocator.Check(foreignPool) {
		t.Error("the foreign pool was dropped from the cache by the rejected duplicate ADD")
	}
	if v, ok := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_app_logs", map[string]string{"loglevel": "error"}); !ok || v != 1 {
		t.Errorf("app log status gauge: got value %v found %v, want exactly 1 error entry", v, ok)
	}
}

// deleting an IPPool object which was never registered under its own
// networkname (for example a duplicate-networkname pool whose ADD was
// rejected) must not resolve to the live pool in the cache, which shares
// that networkname, and free the live pool's state
func TestSyncDeleteForeignCacheEntryKeepsLivePoolState(t *testing.T) {
	var appStatus atomic.Int32
	foreignPool := testPool("pool-a", "infra/net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(foreignPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	// a live registration owns the net-dup keys and holds one allocation
	if err := controller.ipam.NewSubnet("infra/net-dup", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the live pool's ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("infra/net-dup", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the live pool's ip: %v", err)
	}
	if err := controller.dhcp.AddPool("infra/net-dup", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
		t.Fatalf("registering the live pool's dhcp pool: %v", err)
	}
	if err := cacheAllocator.Add(foreignPool); err != nil {
		t.Fatalf("caching the live pool: %v", err)
	}
	controller.metrics.UpdateIPPoolUsed("pool-a", "192.168.1.0/24", "infra/net-dup", 1)
	controller.metrics.UpdateIPPoolAvailable("pool-a", "192.168.1.0/24", "infra/net-dup", 90)

	// pool-b was rejected at registration time and shares networkname
	// net-dup with the live pool-a; deleting it is a no-op
	if err := controller.sync(testPoolEvent("pool-b", DELETE, "infra/net-dup")); err != nil {
		t.Fatalf("sync(DELETE) for an unregistered pool returned error %v, want nil", err)
	}

	if used := controller.ipam.Used("infra/net-dup"); used < 1 {
		t.Errorf("live allocation state of net-dup wiped by the unrelated delete: used=%d, want >= 1", used)
	}
	if !controller.dhcp.CheckPool("infra/net-dup") {
		t.Error("the live pool's dhcp pool was removed by the unrelated delete")
	}
	if !cacheAllocator.Check(foreignPool) {
		t.Error("the live pool was dropped from the cache by the unrelated delete")
	}
	if v, ok := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_used", map[string]string{"ippool": "pool-a", "subnet": "192.168.1.0/24", "network": "infra/net-dup"}); !ok || v != 1 {
		t.Errorf("ippool_used metric after the unrelated delete: got value %v found %v, want 1", v, ok)
	}
}

// a delete whose networkname lookup resolves to the deleted pool itself
// must free exactly that registration: dhcp pool, ipam subnet, cache entry
// and both pool gauges
func TestSyncDeleteRegisteredPoolFreesItsState(t *testing.T) {
	var appStatus atomic.Int32
	storedPool := testPool("pool-a", "infra/net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(storedPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	if err := controller.ipam.NewSubnet("infra/net-dup", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("infra/net-dup", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the live ip: %v", err)
	}
	if err := controller.dhcp.AddPool("infra/net-dup", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
		t.Fatalf("registering the dhcp pool: %v", err)
	}
	// a lease of the deleted network, which the teardown must drop with
	// the registration (its listener is stopped first, so the network is
	// served by nobody afterwards)
	if err := controller.dhcp.AddLease("02:00:00:00:00:01", "infra/net-dup", "192.168.1.10", "ref-dup"); err != nil {
		t.Fatalf("seeding the lease of the deleted network: %v", err)
	}
	// a lease of another network, which the teardown of this pool must not touch
	if err := controller.dhcp.AddLease("02:00:00:00:00:99", "infra/net-keep", "192.168.2.50", "ref-keep"); err != nil {
		t.Fatalf("seeding the lease of another network: %v", err)
	}
	if err := cacheAllocator.Add(storedPool); err != nil {
		t.Fatalf("caching the pool: %v", err)
	}
	controller.metrics.UpdateIPPoolUsed("pool-a", "192.168.1.0/24", "infra/net-dup", 1)
	controller.metrics.UpdateIPPoolAvailable("pool-a", "192.168.1.0/24", "infra/net-dup", 90)
	if err := indexer.Delete(storedPool); err != nil {
		t.Fatal(err)
	}

	if err := controller.sync(testPoolEvent("pool-a", DELETE, "infra/net-dup")); err != nil {
		t.Fatalf("sync(DELETE) for a registered pool returned error %v, want nil", err)
	}

	if used := controller.ipam.Used("infra/net-dup"); used != 0 {
		t.Errorf("ipam allocation state after the own delete: used=%d, want 0", used)
	}
	if controller.dhcp.CheckPool("infra/net-dup") {
		t.Error("the deleted pool's dhcp pool survived the delete")
	}
	// the leases of the deleted network are dropped with the teardown
	if controller.dhcp.CheckLease("02:00:00:00:00:01") {
		t.Error("the deleted network's lease survived the delete")
	}
	// the sweep is scoped to the deleted network: the lease of another
	// network survives the teardown
	if !controller.dhcp.CheckLease("02:00:00:00:00:99") {
		t.Error("a lease of another network must survive the delete")
	}
	if lease := controller.dhcp.GetLease("02:00:00:00:00:99"); lease.PoolName != "infra/net-keep" {
		t.Errorf("the surviving lease serves network %q, want net-keep", lease.PoolName)
	}
	if cacheAllocator.Check(storedPool) {
		t.Error("the deleted pool's cache entry survived the delete")
	}
	if _, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_used", map[string]string{"ippool": "pool-a", "subnet": "192.168.1.0/24", "network": "infra/net-dup"}); found {
		t.Error("the deleted pool's ippool_used metric survived the delete")
	}
	if _, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_available", map[string]string{"ippool": "pool-a", "subnet": "192.168.1.0/24", "network": "infra/net-dup"}); found {
		t.Error("the deleted pool's ippool_available metric survived the delete")
	}
}

// An authoritative NotFound for a never-registered pool is a converged
// no-op and settles its startup snapshot key.
func TestSyncDeleteUncachedPoolSettlesAfterNotFound(t *testing.T) {
	var appStatus atomic.Int32
	startupGate := newTestGate("pool-a")

	indexer := newTestIndexer()

	// no pool is registered under net-gone in this process era
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate)

	if err := controller.sync(testPoolEvent("pool-a", DELETE, "infra/net-gone")); err != nil {
		t.Fatalf("sync(DELETE) for an uncached networkname returned error %v, want nil", err)
	}

	if v, ok := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_app_logs", map[string]string{"loglevel": "error"}); ok {
		t.Errorf("app log status gauge: got error entry %v, want none for a converged no-op delete", v)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("startup gate count = %d, want 1: a startup-time deletion must count for the gate even without a cache entry", startupGate.Settled())
	}
}

// An out-of-scope update stops local service instead of restarting the helper
// under the foreign network, even if the informer held its old key, and foreign
// allocator state is not mistaken for this helper's old registration.
func TestSyncUpdateNetworkMismatchPreservesForeignState(t *testing.T) {
	oldPool := recoveryNewPool("pool-n", "infra/net-a")
	controller, rs, _ := recoveryNewController(t, oldPool)
	if err := recoveryRegistrationSteps(t, controller, oldPool); err != nil {
		t.Fatal(err)
	}
	const foreign = "other/net-a"
	if err := controller.dhcp.AddPool(foreign, "192.168.2.1", "255.255.255.0", "192.168.2.1", nil, "", nil, nil, 60, "other-interface"); err != nil {
		t.Fatal(err)
	}
	newPool := oldPool.DeepCopy()
	newPool.Spec.NetworkName = foreign
	rs.pool = newPool
	controller.indexer = newTestIndexer()
	if err := controller.indexer.Add(newPool); err != nil {
		t.Fatal(err)
	}
	if err := controller.sync(testPoolEvent("pool-n", UPDATE, foreign)); !errors.Is(err, ErrPoolUnregistrable) {
		t.Fatalf("mismatched update = %v, want unregistrable", err)
	}
	if controller.appStatus.Load() != APP_RUNNING {
		t.Fatal("network mismatch must stop the old service without restarting into the foreign network")
	}
	if controller.cache.Check(oldPool) || controller.dhcp.CheckPool(oldPool.Spec.NetworkName) {
		t.Fatal("the old local registration survived the scope mismatch")
	}
	if !controller.dhcp.CheckPool(foreign) {
		t.Fatal("the mismatch removed foreign DHCP state")
	}
}

// a definitively-failing registration counts the pool as handled for the
// startup gate: the gate compares handled pools against the list of pool
// objects, so a rejected pool (here: duplicate networkname) must not keep
// the vmnetcfg/vm controllers from starting. the rate-limited retries of
// the failing object must not double count.
func TestSyncAddRejectedPoolCountsAsHandledDuringInit(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-b")
	foreignPool := testPool("pool-a", "infra/net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(foreignPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}
	if err := indexer.Add(testPool("pool-b", "infra/net-dup", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate)

	// a live registration owns the net-dup keys and holds one allocation
	if err := controller.ipam.NewSubnet("infra/net-dup", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the live pool's ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("infra/net-dup", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the live pool's ip: %v", err)
	}
	if err := controller.dhcp.AddPool("infra/net-dup", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
		t.Fatalf("registering the live pool's dhcp pool: %v", err)
	}
	if err := cacheAllocator.Add(foreignPool); err != nil {
		t.Fatalf("caching the live pool: %v", err)
	}

	if err := controller.sync(testPoolEvent("pool-b", ADD, "infra/net-dup")); err == nil {
		t.Fatal("sync(ADD) for an already-claimed networkname returned nil, want a rejection error")
	}
	if startupGate.Settled() != 1 {
		t.Errorf("ippool count = %d, want 1: the rejected registration must count as handled", startupGate.Settled())
	}

	// the rate-limited retries of the same event must not double count
	if err := controller.sync(testPoolEvent("pool-b", ADD, "infra/net-dup")); err == nil {
		t.Fatal("the retried sync(ADD) returned nil, want a rejection error")
	}
	if startupGate.Settled() != 1 {
		t.Errorf("ippool count = %d after the retried event, want 1", startupGate.Settled())
	}
}

// an add for an object which already vanished from the index counts as
// handled: the pool cannot produce a registration anymore and the startup
// gate must not wait for it
func TestSyncAddVanishedPoolCountsAsHandledDuringInit(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-z")
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate)

	if err := controller.sync(testPoolEvent("pool-z", ADD, "infra/net-z")); err != nil {
		t.Fatalf("sync(ADD) for a vanished pool returned error %v, want nil", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("ippool count = %d, want 1: a vanished pool must not block the startup gate", startupGate.Settled())
	}
}

// only the initialization phase counts for the startup gate: registration
// attempts in the running or restarting phases must not touch the counter
func TestMarkInitAttemptOnlyCountsDuringInit(t *testing.T) {
	for _, phase := range []int{APP_RUNNING, APP_RESTART} {
		var appStatus atomic.Int32
		appStatus.Store(int32(phase))
		startupGate := newTestGate("pool-x")
		controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate)

		controller.markInitAttempt("pool-x")

		if startupGate.Settled() != 0 {
			t.Errorf("ippool count = %d in phase %d, want 0: the gate is only evaluated during initialization", startupGate.Settled(), phase)
		}
	}
}

// an update whose pool has no live registration of its own (the first
// registration was dropped, or the lookup resolved a foreign pool which
// shares the networkname) becomes a registration attempt: a fixed
// projection comes to life with the next event. the attempt counts for
// the startup gate only once it settles, so a transiently failing
// re-registration (here: the bindinterface is missing on the host) must
// keep the gate waiting for its retry.
func TestSyncUpdateAttemptsRegistrationForUnregisteredPool(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-r")
	indexer := newTestIndexer()
	if err := indexer.Add(testPool("pool-r", "infra/net-fresh", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate)

	event := testPoolEvent("pool-r", UPDATE, "infra/net-fresh")

	if err := controller.sync(event); err != nil {
		if strings.Contains(err.Error(), "does not exists in cache") {
			t.Errorf("the update resolved to the cache-miss invariant instead of attempting registration: %v", err)
		}
	}
	if startupGate.Settled() != 0 {
		t.Errorf("ippool count = %d, want 0: a transiently failed re-registration must stay uncounted", startupGate.Settled())
	}

	// the retried event must not double count
	if err := controller.sync(event); err != nil {
		if strings.Contains(err.Error(), "does not exists in cache") {
			t.Errorf("the retried update fell back to the cache-miss invariant: %v", err)
		}
	}
	if startupGate.Settled() != 0 {
		t.Errorf("ippool count = %d after the retried event, want 0 until the registration settles", startupGate.Settled())
	}

}

// an update for a pool whose networkname is claimed by a LIVE pool must
// not resolve to the live pool as the old configuration: that would tear
// the whole application down for an unrelated update. the update re-
// attempts the registration of its own pool instead, which the duplicate
// networkname check rejects without touching the live state.
func TestSyncUpdateForeignCacheEntryDoesNotCascadeRestart(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)
	foreignPool := testPool("pool-a", "infra/net-shared", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(foreignPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}
	if err := indexer.Add(testPool("pool-b", "infra/net-shared", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	// a live registration owns the net-shared keys and holds one allocation
	if err := controller.ipam.NewSubnet("infra/net-shared", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the live pool's ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("infra/net-shared", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the live pool's ip: %v", err)
	}
	if err := controller.dhcp.AddPool("infra/net-shared", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
		t.Fatalf("registering the live pool's dhcp pool: %v", err)
	}
	if err := cacheAllocator.Add(foreignPool); err != nil {
		t.Fatalf("caching the live pool: %v", err)
	}

	if err := controller.sync(testPoolEvent("pool-b", UPDATE, "infra/net-shared")); err == nil {
		t.Fatal("sync(UPDATE) for a pool with a claimed networkname returned nil, want a rejection error")
	} else if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("error = %v, want the duplicate networkname rejection", err)
	}

	if appStatus.Load() != APP_RUNNING {
		t.Errorf("app status = %d after the rejected update, want %d: the foreign registration must not start an application restart", appStatus.Load(), APP_RUNNING)
	}
	if used := controller.ipam.Used("infra/net-shared"); used < 1 {
		t.Errorf("live allocation of net-shared wiped: used=%d, want >= 1", used)
	}
	if !controller.dhcp.CheckPool("infra/net-shared") {
		t.Error("the live pool's dhcp pool was removed by the unrelated update")
	}
	if !cacheAllocator.Check(foreignPool) {
		t.Error("the live pool was dropped from the cache by the unrelated update")
	}
}

// TestSyncUpdateListenerRepairAlreadyRunningConverges: a repair which loses
// the race against a concurrent registration (the queue keys items by
// event, not by pool, so an in-flight ADD and a resync UPDATE of the same
// network stay distinct items) must not surface a failure - the pool is
// serving, which is exactly what the repair wanted.
func TestSyncUpdateListenerRepairAlreadyRunningConverges(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)
	pool := testPool("pool-n2", "infra/net-n2", 60)

	indexer := newTestIndexer()
	indexer.Add(pool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	controller.runListener = func(networkName string, nic string) error {
		return fmt.Errorf("%w: network %s", dhcp.ErrServerAlreadyRunning, networkName)
	}

	if err := controller.sync(testPoolEvent("pool-n2", UPDATE, "infra/net-n2")); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil for the converged already-running repair", err)
	}
}

// Scope loss is actionable during startup, not a swallowed ordinary
// configuration update. Repeated resyncs cannot publish a foreign pool.
func TestSyncNetworkMismatchDuringInitCannotDoubleRegister(t *testing.T) {
	oldPool := recoveryNewPool("pool-n", "infra/net-a")
	controller, rs, _ := recoveryNewController(t, oldPool)
	controller.appStatus.Store(APP_INIT)
	controller.gate = newTestGate(oldPool.Name)
	if err := recoveryRegistrationSteps(t, controller, oldPool); err != nil {
		t.Fatal(err)
	}
	renamed := oldPool.DeepCopy()
	renamed.Spec.NetworkName = "infra/net-new"
	rs.pool = renamed
	controller.indexer = newTestIndexer()
	if err := controller.indexer.Add(renamed); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []int32{APP_INIT, APP_RUNNING} {
		controller.appStatus.Store(phase)
		if err := controller.sync(testPoolEvent(oldPool.Name, UPDATE, renamed.Spec.NetworkName)); !errors.Is(err, ErrPoolUnregistrable) {
			t.Fatalf("phase %d mismatch = %v, want unregistrable", phase, err)
		}
		if controller.dhcp.CheckPool(oldPool.Spec.NetworkName) || controller.dhcp.CheckPool(renamed.Spec.NetworkName) || controller.cache.Check(oldPool) || controller.cache.Check(renamed) {
			t.Fatal("out-of-scope resync retained or double-registered the pool")
		}
		if controller.appStatus.Load() != phase {
			t.Fatal("scope mismatch initiated an application restart")
		}
	}
}

// A deletion event carries the final, possibly foreign network name;
// teardown must still find the local registration by object identity.
func TestSyncDeleteOfRenamedPoolTearsDownRecordedRegistration(t *testing.T) {
	oldPool := recoveryNewPool("pool-d", "infra/net-a")
	controller, rs, _ := recoveryNewController(t, oldPool)
	if err := recoveryRegistrationSteps(t, controller, oldPool); err != nil {
		t.Fatal(err)
	}
	controller.indexer = newTestIndexer()
	rs.pool = nil
	if err := controller.sync(testPoolEvent(oldPool.Name, DELETE, "infra/net-new")); err != nil {
		t.Fatal(err)
	}
	if controller.dhcp.CheckPool(oldPool.Spec.NetworkName) || controller.cache.Check(oldPool) {
		t.Fatal("the deletion leaked the registration under the old network key")
	}
}

// Run must not return while a worker is still syncing: the EventListener
// join waits for Run, so an early return would let the restart flow
// observe a stopped era while a reconciler still registers or tears down
// pool state (nic addresses, dhcp listeners)
func TestRunJoinsTheInFlightSyncBeforeReturning(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		// the in-flight sync stays blocked until the test releases it
		<-release
		ippoolBehaviorWriteKubeError(w, http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	// the blocked handler must always drain, also on the failure path:
	// the server cleanup of the test waits for outstanding requests
	var releaseOnce sync.Once
	releaseSync := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSync()

	queue := newTestQueue()
	indexer := newTestIndexer()
	if err := indexer.Add(testPool("pool-j", "infra/net-j", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	var appStatus atomic.Int32
	controller, _ := newTestController(t, queue, indexer, &stubInformer{synced: true}, &appStatus, nil)
	cs, err := kihclientset.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}
	controller.kihClientset = cs

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		controller.Run(1, stop)
		close(done)
	}()

	queue.Add(testPoolEvent("pool-j", ADD, "infra/net-j"))
	<-started

	close(stop)

	// the worker is blocked mid-sync: Run must stay up while it runs
	select {
	case <-done:
		t.Fatal("Run returned while the worker was still syncing")
	case <-time.After(200 * time.Millisecond):
	}

	releaseSync()

	select {
	case <-done:
	case <-time.After(shutdownWait):
		t.Fatal("Run did not return after the in-flight sync finished")
	}
}

// a pool of the startup snapshot which was deleted before the informer
// started generates no event at all: Run must settle it through the
// store reconcile after the cache sync, or the membership gate would wait
// for it forever (a count-based gate hid this case by letting unrelated
// objects substitute)
func TestRunSettlesSnapshotKeysDeletedBeforeTheInformerStarted(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)

	// the store holds another pool but not the snapshot's pool-gone: its
	// deletion happened between the startup LIST and the informer start
	indexer := newTestIndexer()
	if err := indexer.Add(testPool("pool-live", "infra/net-live", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	startupGate := newTestGate("pool-gone", "pool-live")
	controller, _ := newTestController(t, newTestQueue(), indexer, &stubInformer{synced: true}, &appStatus, startupGate)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		controller.Run(1, stop)
		close(done)
	}()

	deadline := time.NewTimer(shutdownWait)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for startupGate.Settled() == 0 {
		select {
		case <-tick.C:
		case <-deadline.C:
			close(stop)
			<-done
			t.Fatal("the deleted snapshot key never settled after its API verification")
		}
	}
	close(stop)

	select {
	case <-done:
	case <-time.After(shutdownWait):
		t.Fatal("Run did not return after the stop channel was closed")
	}

	// the never-observed deletion settled through the store reconcile;
	// pool-live settles through its own events in production, so it stays
	// pending here
	for _, key := range startupGate.Unsettled() {
		if key == "pool-gone" {
			t.Errorf("the never-observed deletion %s stayed unsettled: Run must settle it through the store reconcile", key)
		}
	}
}

// I05 regression: one bind interface serves one pool. a second pool on an
// interface another pool already serves would share the broadcast segment
// with it, and every socket of the interface receives both pools' traffic
// (the so_reuseport delivery semantics depend on the deployment kernel,
// so which pool answers a request is not deterministic). the second pool
// is rejected before any of its sub-resources exist, the rejection is
// unregistrable (the startup gate counts the pool instead of retrying the
// conflict forever), and the first registration stays untouched.
func TestRegisterIPPoolRejectsDuplicateBindInterface(t *testing.T) {
	pool := recoveryNewPool("pool-b", "infra/net-b")
	controller, _, _ := recoveryNewController(t, pool)
	controller.appStatus.Store(APP_INIT)
	controller.gate = newTestGate(pool.Name)
	// Preserve the defensive allocator guard without making one scoped
	// controller discover and register a second network.
	if err := controller.dhcp.AddPool("infra/net-a", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, pool.Spec.BindInterface); err != nil {
		t.Fatal(err)
	}
	if cleanup, err := controller.registerIPPool(pool); !errors.Is(err, ErrPoolUnregistrable) || cleanup {
		t.Fatalf("interface conflict = (%v, %v), want definitive rejection before mutation", cleanup, err)
	}
	if controller.dhcp.CheckPool(pool.Spec.NetworkName) || controller.cache.Check(pool) || controller.ipam.Used(pool.Spec.NetworkName) != 0 {
		t.Fatal("the rejected pool published local state")
	}
	if !controller.dhcp.CheckPool("infra/net-a") || !controller.gate.Open() {
		t.Fatal("interface conflict must preserve the incumbent and settle the rejected pool")
	}
}

func TestSyncSelectorLossPreservesLedgerAndRestorationReplays(t *testing.T) {
	for _, action := range []string{DELETE, UPDATE} {
		t.Run(action, func(t *testing.T) {
			pool := recoveryNewPool("pool1", "infra/net-a")
			const mac = "02:aa:bb:cc:dd:01"
			ref := util.AllocationRef("tenant", "vm", mac)
			pool.Status.IPv4.Allocated = map[string]string{"10.0.0.2": ref}
			c, rs, _ := recoveryNewController(t, pool)
			rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
				recoveryNewVMNetCfg("tenant", "vm", "10.0.0.2", mac, pool.Spec.NetworkName),
			}
			if err := recoveryRegistrationSteps(t, c, pool); err != nil {
				t.Fatal(err)
			}
			if err := c.dhcp.AddLease(mac, pool.Spec.NetworkName, "10.0.0.2", "tenant/vm"); err != nil {
				t.Fatal(err)
			}
			c.indexer = newTestIndexer()
			before := rs.pool.DeepCopy()
			writes := rs.putCount
			delete(rs.pool.Labels, util.NetworkNamespaceLabel)
			if err := c.sync(testPoolEvent(pool.Name, action, pool.Spec.NetworkName)); !errors.Is(err, ErrPoolUnregistrable) {
				t.Fatalf("selector loss = %v, want unregistrable", err)
			}
			if rs.putCount != writes || rs.pool.Status.IPv4.Allocated["10.0.0.2"] != ref || !rs.pool.Status.LastUpdate.Equal(&before.Status.LastUpdate) {
				t.Fatal("selector loss mutated the durable reservation ledger")
			}
			if c.cache.Check(pool) || c.dhcp.CheckPool(pool.Spec.NetworkName) || c.dhcp.CheckLease(mac) || c.ipam.Used(pool.Spec.NetworkName) != 0 {
				t.Fatal("selector loss retained local serving state")
			}
			rs.pool.Labels[util.NetworkNamespaceLabel] = "infra"
			if err := c.indexer.Add(rs.pool.DeepCopy()); err != nil {
				t.Fatal(err)
			}
			if err := c.sync(testPoolEvent(pool.Name, ADD, pool.Spec.NetworkName)); err != nil {
				t.Fatalf("restored selector replay: %v", err)
			}
			if !c.cache.Check(pool) || !c.dhcp.CheckPool(pool.Spec.NetworkName) {
				t.Fatal("label restoration did not restore service")
			}
			if _, err := c.ipam.GetIP(pool.Spec.NetworkName, ""); err == nil {
				t.Fatal("restoration offered the surviving owner's address to a new allocation")
			}
			if _, err := c.ipam.ReclaimIP(pool.Spec.NetworkName, "10.0.0.2", ref); err != nil {
				t.Fatalf("surviving owner cannot reclaim its reservation: %v", err)
			}
		})
	}
}

func TestSyncDisappearanceAPIErrorRetainsRegistrationAndRetries(t *testing.T) {
	for _, action := range []string{DELETE, ADD} {
		t.Run(action, func(t *testing.T) {
			pool := recoveryNewPool("pool1", "infra/net-a")
			c, rs, _ := recoveryNewController(t, pool)
			if err := recoveryRegistrationSteps(t, c, pool); err != nil {
				t.Fatal(err)
			}
			c.indexer = newTestIndexer()
			c.queue = newTestQueue()
			t.Cleanup(c.queue.ShutDown)
			c.appStatus.Store(APP_INIT)
			c.gate = newTestGate(pool.Name)
			rs.getStatus = http.StatusInternalServerError
			event := testPoolEvent(pool.Name, action, pool.Spec.NetworkName)
			for range 6 {
				err := c.sync(event)
				if err == nil || errors.Is(err, ErrPoolUnregistrable) {
					t.Fatalf("unverified disappearance = %v, want retryable", err)
				}
				c.handleErr(err, event)
			}
			if c.gate.Settled() != 0 || !c.cache.Check(pool) || !c.dhcp.CheckPool(pool.Spec.NetworkName) {
				t.Fatal("exhausted API errors settled the gate or removed the live registration")
			}
			rs.getStatus = 0
			rs.pool = nil
			if err := c.sync(event); err != nil {
				t.Fatalf("retry after authoritative deletion: %v", err)
			}
			if !c.gate.Open() || c.cache.Check(pool) || c.dhcp.CheckPool(pool.Spec.NetworkName) {
				t.Fatal("NotFound did not settle and clean up the deleted registration")
			}
		})
	}
}

func TestSyncVerificationDoesNotDiscoverLivePool(t *testing.T) {
	pool := recoveryNewPool("pool1", "infra/net-a")
	c, rs, _ := recoveryNewController(t, pool)
	c.appStatus.Store(APP_INIT)
	c.indexer = newTestIndexer()
	for _, action := range []string{DELETE, ADD} {
		if err := c.sync(testPoolEvent(pool.Name, action, pool.Spec.NetworkName)); err == nil || errors.Is(err, ErrPoolUnregistrable) {
			t.Fatalf("live but unobserved pool = %v, want retryable informer wait", err)
		}
	}
	if c.cache.Check(pool) || c.dhcp.CheckPool(pool.Spec.NetworkName) || rs.putCount != 0 || c.gate.Settled() != 0 || len(c.indexer.List()) != 0 {
		t.Fatal("unfiltered verification response entered discovery or durable state")
	}
	if err := c.indexer.Add(pool.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := c.sync(testPoolEvent(pool.Name, ADD, pool.Spec.NetworkName)); err != nil {
		t.Fatal(err)
	}
	if !c.cache.Check(pool) || !c.gate.Open() {
		t.Fatal("selected informer delivery did not register and settle the pool")
	}
}

func TestSyncDisappearanceUIDReplacementAndStaleDeleteKeepCurrentPool(t *testing.T) {
	pool := recoveryNewPool("pool1", "infra/net-a")
	pool.UID = "old"
	c, rs, _ := recoveryNewController(t, pool)
	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatal(err)
	}
	const oldMAC = "02:00:00:00:00:01"
	if err := c.dhcp.AddLease(oldMAC, pool.Spec.NetworkName, "10.0.0.2", "tenant/old"); err != nil {
		t.Fatal(err)
	}
	replacement := recoveryNewPool(pool.Name, pool.Spec.NetworkName)
	replacement.UID = "new"
	replacement.Spec.IPv4Config.Pool.Start = "10.0.0.3"
	replacement.Spec.IPv4Config.Pool.End = "10.0.0.3"
	rs.pool = replacement
	c.indexer = newTestIndexer()
	// The old DELETE arrives before the selected informer has the replacement.
	if err := c.sync(testPoolEvent(pool.Name, DELETE, pool.Spec.NetworkName)); err == nil {
		t.Fatal("replacement must wait for selected discovery")
	}
	if c.cache.Check(pool) || c.dhcp.CheckLease(oldMAC) {
		t.Fatal("the predecessor's allocator or lease survived its UID replacement")
	}
	if err := c.indexer.Add(replacement.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := c.sync(testPoolEvent(pool.Name, ADD, pool.Spec.NetworkName)); err != nil {
		t.Fatal(err)
	}
	const newMAC = "02:00:00:00:00:02"
	if ip, err := c.ipam.GetIP(pool.Spec.NetworkName, ""); err != nil || ip != "10.0.0.3" {
		t.Fatalf("replacement range = %q, %v", ip, err)
	}
	if err := c.dhcp.AddLease(newMAC, pool.Spec.NetworkName, "10.0.0.3", "tenant/new"); err != nil {
		t.Fatal(err)
	}
	if err := c.sync(testPoolEvent(pool.Name, DELETE, pool.Spec.NetworkName)); err != nil {
		t.Fatalf("stale DELETE against selected replacement: %v", err)
	}
	cached, err := c.cache.Get("pool", pool.Spec.NetworkName)
	if err != nil || cached.(kihv1.IPPool).UID != replacement.UID || !c.dhcp.CheckLease(newMAC) || c.ipam.Used(pool.Spec.NetworkName) != 1 {
		t.Fatal("stale deletion tore down the current replacement")
	}
}
