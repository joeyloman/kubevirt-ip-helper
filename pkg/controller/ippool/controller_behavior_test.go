package ippool

import (
	"context"
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
	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"
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
		nil,
		appStatus,
		startupGate,
		nil,
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

func testPool(name, network string, leaseTime int) *kihv1.IPPool {
	return &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
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
	pool := testPool("pool-defer", "net-defer", 60)
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
		poolNetworkName: "net-defer",
	}

	err := c.sync(event)
	if err == nil || !strings.Contains(err.Error(), "deferring registration") {
		t.Fatalf("sync of an add during the restart = %v, want the deferral", err)
	}
	if c.dhcp.CheckPool("net-defer") {
		t.Error("a dying era must not register the pool's dhcp service")
	}
	if used := c.ipam.Used("net-defer"); used != 0 {
		t.Errorf("ipam used = %d, want 0: the deferred add must not register a subnet", used)
	}

	// once the new era runs, the retried add registers regularly
	appStatus.Store(APP_RUNNING)
	if err := c.sync(event); err != nil {
		t.Fatalf("the retried add after the restart must register: %v", err)
	}
	if !c.dhcp.CheckPool("net-defer") {
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

	event := testPoolEvent("pool-a", ADD, "net-a")
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

	event := testPoolEvent("pool-b", ADD, "net-b")
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

	if err := controller.sync(testPoolEvent("pool-f", ADD, "net-f")); err != nil {
		t.Errorf("sync() for a missing index object returned error %v, want nil", err)
	}
}

func TestSyncReturnsIndexerError(t *testing.T) {
	var appStatus atomic.Int32
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	if err := controller.sync(testPoolEvent("pool-g", ADD, "net-g")); err == nil {
		t.Fatalf("sync() returned nil, want the indexer error")
	}
}

func TestSyncDeleteSucceedsWhenPoolNotCached(t *testing.T) {
	// a DELETE snapshot for a pool that is gone from the index and unknown to
	// the cache only logs the cache lookup failure and returns without error
	var appStatus atomic.Int32
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, nil)

	if err := controller.sync(testPoolEvent("pool-h", DELETE, "net-h")); err != nil {
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
	indexer.Add(testPool("pool-i", "net-i", 60))
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	if err := controller.sync(testPoolEvent("pool-i", UPDATE, "net-i")); err == nil {
		t.Error("sync(UPDATE) returned nil, want a rate-limited requeue error for the missing cache entry")
	}
}

func TestSyncUpdateIgnoredWhileInitializing(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	oldPool := testPool("pool-j", "net-j", 60)
	newPool := testPool("pool-j", "net-j", 120)

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

	if err := controller.sync(testPoolEvent("pool-j", UPDATE, "net-j")); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}

	// while initializing, pool updates are deliberately ignored: the cache
	// still holds the originally registered pool, and the listener repair
	// must not open sockets during the startup replay (the registration
	// phase owns the listener lifecycle)
	if listenerRepairs != 0 {
		t.Errorf("listener repair attempts = %d, want 0 while the application initializes", listenerRepairs)
	}
	got, err := cacheAllocator.Get("pool", "net-j")
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
	pool := testPool("pool-k", "net-k", 60)

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

	if err := controller.sync(testPoolEvent("pool-k", UPDATE, "net-k")); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}

	// an identical object is a no-change: the cache keeps the original pool
	// and no dhcp pool is (re)registered
	got, err := cacheAllocator.Get("pool", "net-k")
	if err != nil {
		t.Fatalf("pool missing from cache: %v", err)
	}
	if leaseTime := got.(kihv1.IPPool).Spec.IPv4Config.LeaseTime; leaseTime != 60 {
		t.Errorf("cache lease time = %d after no-change update, want 60", leaseTime)
	}
	if controller.dhcp.CheckPool("net-k") {
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
	pool := testPool("pool-m2", "net-m2", 60)

	indexer := newTestIndexer()
	indexer.Add(pool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	controller.runListener = func(networkName string, nic string) error {
		return errors.New("cannot bind to interface test-fake-iface: no such device")
	}

	if err := controller.sync(testPoolEvent("pool-m2", UPDATE, "net-m2")); err == nil {
		t.Error("sync(UPDATE) returned nil, want the listener repair failure surfaced for the rate-limited retry")
	}
}

func TestSyncUpdateReloadsPoolWhenRunning(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)
	oldPool := testPool("pool-l", "net-l", 60)
	newPool := testPool("pool-l", "net-l", 120)

	indexer := newTestIndexer()
	indexer.Add(newPool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	controller.runListener = func(networkName string, nic string) error { return nil }

	if err := controller.sync(testPoolEvent("pool-l", UPDATE, "net-l")); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}

	// a lease time change is reloadable: the dhcp pool is refreshed and the
	// cache now carries the updated pool
	if !controller.dhcp.CheckPool("net-l") {
		t.Errorf("dhcp pool was not registered after a reloadable update")
	}
	got, err := cacheAllocator.Get("pool", "net-l")
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
	pool := testPool("pool-m", "net-m", 60)
	pool.Spec.IPv4Config.Subnet = "not-a-cidr"

	indexer := newTestIndexer()
	if err := indexer.Add(pool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	var appStatus atomic.Int32
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	if err := controller.sync(testPoolEvent("pool-m", ADD, "net-m")); err == nil {
		t.Error("sync(ADD) returned nil, want a rate-limited requeue error from the registration failure")
	}

	if controller.dhcp.CheckPool("net-m") {
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
	foreignPool := testPool("pool-a", "net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(foreignPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}
	if err := indexer.Add(testPool("pool-b", "net-dup", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	// a foreign pool registration already owns the net-dup keys and holds
	// one live allocation
	if err := controller.ipam.NewSubnet("net-dup", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the foreign ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("net-dup", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the foreign live ip: %v", err)
	}
	if err := controller.dhcp.AddPool("net-dup", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
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

	if err := controller.sync(testPoolEvent("pool-b", ADD, "net-dup")); err == nil {
		t.Fatal("sync(ADD) for an already-claimed networkname returned nil, want a rejection error")
	} else if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("error = %v, want a networkname-claim rejection", err)
	}

	// the foreign registration must survive both rejections untouched
	if used := controller.ipam.Used("net-dup"); used < 1 {
		t.Errorf("foreign allocation state of net-dup wiped: used=%d, want >= 1", used)
	}
	if _, err := controller.ipam.GetIP("net-dup", ""); err != nil {
		t.Errorf("GetIP on the foreign subnet failed: %v, want the subnet to stay live", err)
	}
	if !controller.dhcp.CheckPool("net-dup") {
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
	foreignPool := testPool("pool-a", "net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(foreignPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	// a live registration owns the net-dup keys and holds one allocation
	if err := controller.ipam.NewSubnet("net-dup", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the live pool's ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("net-dup", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the live pool's ip: %v", err)
	}
	if err := controller.dhcp.AddPool("net-dup", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
		t.Fatalf("registering the live pool's dhcp pool: %v", err)
	}
	if err := cacheAllocator.Add(foreignPool); err != nil {
		t.Fatalf("caching the live pool: %v", err)
	}
	controller.metrics.UpdateIPPoolUsed("pool-a", "192.168.1.0/24", "net-dup", 1)
	controller.metrics.UpdateIPPoolAvailable("pool-a", "192.168.1.0/24", "net-dup", 90)

	// pool-b was rejected at registration time and shares networkname
	// net-dup with the live pool-a; deleting it is a no-op
	if err := controller.sync(testPoolEvent("pool-b", DELETE, "net-dup")); err != nil {
		t.Fatalf("sync(DELETE) for an unregistered pool returned error %v, want nil", err)
	}

	if used := controller.ipam.Used("net-dup"); used < 1 {
		t.Errorf("live allocation state of net-dup wiped by the unrelated delete: used=%d, want >= 1", used)
	}
	if !controller.dhcp.CheckPool("net-dup") {
		t.Error("the live pool's dhcp pool was removed by the unrelated delete")
	}
	if !cacheAllocator.Check(foreignPool) {
		t.Error("the live pool was dropped from the cache by the unrelated delete")
	}
	if v, ok := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_used", map[string]string{"ippool": "pool-a", "subnet": "192.168.1.0/24", "network": "net-dup"}); !ok || v != 1 {
		t.Errorf("ippool_used metric after the unrelated delete: got value %v found %v, want 1", v, ok)
	}
	if v, ok := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_app_logs", map[string]string{"loglevel": "warning"}); !ok || v != 1 {
		t.Errorf("app log status gauge: got value %v found %v, want exactly 1 warning entry", v, ok)
	}
}

// a delete whose networkname lookup resolves to the deleted pool itself
// must free exactly that registration: dhcp pool, ipam subnet, cache entry
// and both pool gauges
func TestSyncDeleteRegisteredPoolFreesItsState(t *testing.T) {
	var appStatus atomic.Int32
	storedPool := testPool("pool-a", "net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(storedPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	if err := controller.ipam.NewSubnet("net-dup", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("net-dup", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the live ip: %v", err)
	}
	if err := controller.dhcp.AddPool("net-dup", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
		t.Fatalf("registering the dhcp pool: %v", err)
	}
	// a lease of the deleted network, which the teardown must drop with
	// the registration (its listener is stopped first, so the network is
	// served by nobody afterwards)
	if err := controller.dhcp.AddLease("02:00:00:00:00:01", "net-dup", "192.168.1.10", "ref-dup"); err != nil {
		t.Fatalf("seeding the lease of the deleted network: %v", err)
	}
	// a lease of another network, which the teardown of this pool must not touch
	if err := controller.dhcp.AddLease("02:00:00:00:00:99", "net-keep", "192.168.2.50", "ref-keep"); err != nil {
		t.Fatalf("seeding the lease of another network: %v", err)
	}
	if err := cacheAllocator.Add(storedPool); err != nil {
		t.Fatalf("caching the pool: %v", err)
	}
	controller.metrics.UpdateIPPoolUsed("pool-a", "192.168.1.0/24", "net-dup", 1)
	controller.metrics.UpdateIPPoolAvailable("pool-a", "192.168.1.0/24", "net-dup", 90)

	if err := controller.sync(testPoolEvent("pool-a", DELETE, "net-dup")); err != nil {
		t.Fatalf("sync(DELETE) for a registered pool returned error %v, want nil", err)
	}

	if used := controller.ipam.Used("net-dup"); used != 0 {
		t.Errorf("ipam allocation state after the own delete: used=%d, want 0", used)
	}
	if controller.dhcp.CheckPool("net-dup") {
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
	if lease := controller.dhcp.GetLease("02:00:00:00:00:99"); lease.PoolName != "net-keep" {
		t.Errorf("the surviving lease serves network %q, want net-keep", lease.PoolName)
	}
	if cacheAllocator.Check(storedPool) {
		t.Error("the deleted pool's cache entry survived the delete")
	}
	if _, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_used", map[string]string{"ippool": "pool-a", "subnet": "192.168.1.0/24", "network": "net-dup"}); found {
		t.Error("the deleted pool's ippool_used metric survived the delete")
	}
	if _, found := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_ippool_available", map[string]string{"ippool": "pool-a", "subnet": "192.168.1.0/24", "network": "net-dup"}); found {
		t.Error("the deleted pool's ippool_available metric survived the delete")
	}
}

// deleting an IPPool object whose networkname resolves to no cache entry
// at all (its registration was rejected, or failed and was torn back down)
// is the converged outcome of a never-registered pool, not a failure: the
// delete is a no-op which reports a warning like the name-mismatch case,
// and the pool still counts for the startup gate so a startup-time
// deletion cannot block the controller startup
func TestSyncDeleteUncachedNetworkNameReportsWarning(t *testing.T) {
	var appStatus atomic.Int32
	startupGate := newTestGate("pool-a")

	indexer := newTestIndexer()

	// no pool is registered under net-gone in this process era
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate)

	if err := controller.sync(testPoolEvent("pool-a", DELETE, "net-gone")); err != nil {
		t.Fatalf("sync(DELETE) for an uncached networkname returned error %v, want nil", err)
	}

	if v, ok := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_app_logs", map[string]string{"loglevel": "error"}); ok {
		t.Errorf("app log status gauge: got error entry %v, want none for a converged no-op delete", v)
	}
	if v, ok := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_app_logs", map[string]string{"loglevel": "warning"}); !ok || v != 1 {
		t.Errorf("app log status gauge: got value %v found %v, want exactly 1 warning entry", v, ok)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("startup gate count = %d, want 1: a startup-time deletion must count for the gate even without a cache entry", startupGate.Settled())
	}
}

// a pool whose networkname changed keeps its cache entry under the old key:
// its update event must still reach the restart handling through it
func TestSyncUpdateReachesRestartAfterNetworkNameChange(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)
	oldPool := testPool("pool-n", "net-old", 60)
	newPool := testPool("pool-n", "net-new", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(newPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	event := testPoolEvent("pool-n", UPDATE, "net-new")
	event.oldPoolNetworkName = "net-old"

	if err := controller.sync(event); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if appStatus.Load() != APP_RESTART {
		t.Errorf("app status = %d, want %d after a networkname change", appStatus.Load(), APP_RESTART)
	}
}

// renaming a pool into a networkname which a live registration already
// claims would tear the whole application down and then fail during the
// re-registration: that update must be rejected before any teardown, so
// the currently registered configuration keeps serving
func TestSyncUpdateRejectsNetworkNameChangeToClaimedNetwork(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)
	oldPool := testPool("pool-n", "net-old", 60)
	newPool := testPool("pool-n", "net-claimed", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(newPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	// a live registration already owns the target networkname
	if err := controller.dhcp.AddPool("net-claimed", "192.168.2.1", "255.255.255.0", "192.168.2.1", nil, "", nil, nil, 60, "test-fake-iface-2"); err != nil {
		t.Fatalf("registering the claimant dhcp pool: %v", err)
	}

	event := testPoolEvent("pool-n", UPDATE, "net-claimed")
	event.oldPoolNetworkName = "net-old"

	if err := controller.sync(event); err == nil {
		t.Fatal("sync(UPDATE) accepted a networkname change into an already claimed networkname")
	}

	if appStatus.Load() != APP_RUNNING {
		t.Errorf("the rejected rename started an application restart: app status got %d, want %d", appStatus.Load(), APP_RUNNING)
	}
	if !cacheAllocator.Check(oldPool) {
		t.Error("the rejected rename dropped the live registration from the cache")
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
	foreignPool := testPool("pool-a", "net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(foreignPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}
	if err := indexer.Add(testPool("pool-b", "net-dup", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate)

	// a live registration owns the net-dup keys and holds one allocation
	if err := controller.ipam.NewSubnet("net-dup", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the live pool's ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("net-dup", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the live pool's ip: %v", err)
	}
	if err := controller.dhcp.AddPool("net-dup", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
		t.Fatalf("registering the live pool's dhcp pool: %v", err)
	}
	if err := cacheAllocator.Add(foreignPool); err != nil {
		t.Fatalf("caching the live pool: %v", err)
	}

	if err := controller.sync(testPoolEvent("pool-b", ADD, "net-dup")); err == nil {
		t.Fatal("sync(ADD) for an already-claimed networkname returned nil, want a rejection error")
	}
	if startupGate.Settled() != 1 {
		t.Errorf("ippool count = %d, want 1: the rejected registration must count as handled", startupGate.Settled())
	}

	// the rate-limited retries of the same event must not double count
	if err := controller.sync(testPoolEvent("pool-b", ADD, "net-dup")); err == nil {
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

	if err := controller.sync(testPoolEvent("pool-z", ADD, "net-z")); err != nil {
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
	if err := indexer.Add(testPool("pool-r", "net-fresh", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate)

	event := testPoolEvent("pool-r", UPDATE, "net-fresh")

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
	foreignPool := testPool("pool-a", "net-shared", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(foreignPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}
	if err := indexer.Add(testPool("pool-b", "net-shared", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)

	// a live registration owns the net-shared keys and holds one allocation
	if err := controller.ipam.NewSubnet("net-shared", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the live pool's ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("net-shared", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the live pool's ip: %v", err)
	}
	if err := controller.dhcp.AddPool("net-shared", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
		t.Fatalf("registering the live pool's dhcp pool: %v", err)
	}
	if err := cacheAllocator.Add(foreignPool); err != nil {
		t.Fatalf("caching the live pool: %v", err)
	}

	if err := controller.sync(testPoolEvent("pool-b", UPDATE, "net-shared")); err == nil {
		t.Fatal("sync(UPDATE) for a pool with a claimed networkname returned nil, want a rejection error")
	} else if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("error = %v, want the duplicate networkname rejection", err)
	}

	if appStatus.Load() != APP_RUNNING {
		t.Errorf("app status = %d after the rejected update, want %d: the foreign registration must not start an application restart", appStatus.Load(), APP_RUNNING)
	}
	if used := controller.ipam.Used("net-shared"); used < 1 {
		t.Errorf("live allocation of net-shared wiped: used=%d, want >= 1", used)
	}
	if !controller.dhcp.CheckPool("net-shared") {
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
	pool := testPool("pool-n2", "net-n2", 60)

	indexer := newTestIndexer()
	indexer.Add(pool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	controller.runListener = func(networkName string, nic string) error {
		return fmt.Errorf("%w: network %s", dhcp.ErrServerAlreadyRunning, networkName)
	}

	if err := controller.sync(testPoolEvent("pool-n2", UPDATE, "net-n2")); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil for the converged already-running repair", err)
	}
}

// TestSyncUpdateResyncReroutesSwallowedNetworkNameChange pins the review
// finding: a networkname change which arrives while the application is
// initializing is ignored, but the registration keeps serving under the
// old networkname. the resync update which follows (old==new networkname)
// used to misread the pool as never-registered and re-register it a
// second time under the new name, leaving two live registrations of the
// same pool - the stale one under the old networkname could never be
// cleaned by a later event again. the resync must route the rename
// through the regular change handling instead, which tears the old
// registration down through the restart flow.
func TestSyncUpdateResyncReroutesSwallowedNetworkNameChange(t *testing.T) {
	stubNicMutation(t)

	// the pool status the registration consults survives at the api
	stored := testPool("pool-n", "net-old", 60)
	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	t.Cleanup(srv.Close)

	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-n")

	oldSpec := testPool("pool-n", "net-old", 60)
	newSpec := testPool("pool-n", "net-new", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(oldSpec); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate)
	cs, err := kihclientset.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}
	controller.kihClientset = cs
	// the listener seam keeps the registration off the host network
	controller.runListener = func(networkName string, nic string) error {
		return nil
	}

	// the startup registration settles under the old networkname
	if err := controller.sync(testPoolEvent("pool-n", ADD, "net-old")); err != nil {
		t.Fatalf("the startup registration failed: %v", err)
	}
	if !cacheAllocator.Check(oldSpec) {
		t.Fatal("the startup registration did not publish the pool under the old networkname")
	}
	if net, live := controller.registeredPools["pool-n"]; !live || net != "net-old" {
		t.Fatalf("the settled registration was not recorded: registeredPools[pool-n] = %q, live=%v", net, live)
	}

	// the rename arrives while the application is initializing: the update
	// is ignored, the registration keeps serving under the old networkname.
	// the rename is persisted, so the api serves the new spec from now on
	// (exactly like a cluster where the object was updated).
	if err := indexer.Update(newSpec); err != nil {
		t.Fatalf("applying the rename to the index: %v", err)
	}
	rs.pool = newSpec.DeepCopy()
	renameEvent := testPoolEvent("pool-n", UPDATE, "net-new")
	renameEvent.oldPoolNetworkName = "net-old"
	if err := controller.sync(renameEvent); err != nil {
		t.Fatalf("the initializing application must ignore the rename update: %v", err)
	}
	if appStatus.Load() != APP_INIT {
		t.Fatalf("app status = %d, want %d: the swallowed rename must not restart the initializing application", appStatus.Load(), APP_INIT)
	}

	// a resync update (old==new networkname) must not double-register
	// while the application is initializing either: it is routed through
	// the change handling, which ignores it until the application runs
	resyncEvent := testPoolEvent("pool-n", UPDATE, "net-new")
	if err := controller.sync(resyncEvent); err != nil {
		t.Fatalf("the resync during initialization returned an error: %v", err)
	}
	if controller.dhcp.CheckPool("net-new") || cacheAllocator.Check(newSpec) {
		t.Fatal("the resync during initialization double-registered the pool")
	}

	// once the application runs, the resync routes the swallowed rename
	// through the restart flow instead of registering a second time
	appStatus.Store(APP_RUNNING)
	if err := controller.sync(resyncEvent); err != nil {
		t.Fatalf("the resync of the swallowed rename returned an error: %v", err)
	}
	if appStatus.Load() != APP_RESTART {
		t.Errorf("app status = %d, want %d: the swallowed rename must take the restart flow", appStatus.Load(), APP_RESTART)
	}

	// no second registration exists: the new networkname has no live
	// sub-resources, the old registration is the one the era restart
	// tears down
	if controller.dhcp.CheckPool("net-new") {
		t.Error("the resync registered a second dhcp pool under the new networkname")
	}
	if cacheAllocator.Check(newSpec) {
		t.Error("the resync published a second cache entry under the new networkname")
	}
	if controller.ipam.Used("net-new") != 0 {
		t.Error("the resync registered a second ipam subnet under the new networkname")
	}
	if !controller.dhcp.CheckPool("net-old") {
		t.Error("the old registration must stay live until the era restart tears it down")
	}
	if net, live := controller.registeredPools["pool-n"]; !live || net != "net-old" {
		t.Errorf("the restart flow must keep the recorded registration untouched: registeredPools[pool-n] = %q, live=%v", net, live)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("ippool count = %d, want 1 (a single settled registration)", startupGate.Settled())
	}
}

// TestSyncDeleteOfRenamedPoolTearsDownTheRecordedRegistration pins the
// review finding: a pool which was renamed while the application was
// initializing keeps its live registration under the OLD networkname
// (the rename is swallowed during APP_INIT). when the object is then
// deleted, the event carries only the final networkname, so the delete
// path used to report "never registered; skipping cleanup" and leak the
// whole registration: the dhcp pool, the ipam subnet, the cache entry,
// the nic address and the registeredPools record all stayed behind. the
// deletion must resolve the recorded networkname (exactly like the
// update path) and tear the live registration down.
func TestSyncDeleteOfRenamedPoolTearsDownTheRecordedRegistration(t *testing.T) {
	stubNicMutation(t)

	// the cleanup releases the server ip from the bind interface: record
	// the seam so the teardown of the old registration is observable
	var removedNicIPs []string
	origRemove := network.RemoveIpFromNic
	network.RemoveIpFromNic = func(nic string, ip4 string) error {
		removedNicIPs = append(removedNicIPs, nic+" "+ip4)

		return nil
	}
	t.Cleanup(func() {
		network.RemoveIpFromNic = origRemove
	})

	// the pool status the registration consults survives at the api
	stored := testPool("pool-d", "net-old", 60)
	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	t.Cleanup(srv.Close)

	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-d")

	oldSpec := testPool("pool-d", "net-old", 60)
	newSpec := testPool("pool-d", "net-new", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(oldSpec); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate)
	cs, err := kihclientset.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}
	controller.kihClientset = cs
	// the listener seam keeps the registration off the host network
	controller.runListener = func(networkName string, nic string) error {
		return nil
	}

	// the startup registration settles under the old networkname
	if err := controller.sync(testPoolEvent("pool-d", ADD, "net-old")); err != nil {
		t.Fatalf("the startup registration failed: %v", err)
	}
	if net, live := controller.registeredPools["pool-d"]; !live || net != "net-old" {
		t.Fatalf("the settled registration was not recorded: registeredPools[pool-d] = %q, live=%v", net, live)
	}

	// the rename arrives while the application is initializing and is
	// swallowed; the rename is persisted, so the api serves the new spec
	// from now on
	if err := indexer.Update(newSpec); err != nil {
		t.Fatalf("applying the rename to the index: %v", err)
	}
	rs.pool = newSpec.DeepCopy()
	renameEvent := testPoolEvent("pool-d", UPDATE, "net-new")
	renameEvent.oldPoolNetworkName = "net-old"
	if err := controller.sync(renameEvent); err != nil {
		t.Fatalf("the initializing application must ignore the rename update: %v", err)
	}

	// the renamed object is deleted: the event carries only the final
	// networkname, and the handler must converge on it
	if err := indexer.Delete(newSpec); err != nil {
		t.Fatalf("removing the deleted object from the index: %v", err)
	}
	if err := controller.sync(testPoolEvent("pool-d", DELETE, "net-new")); err != nil {
		t.Fatalf("the deletion of the renamed pool returned an error: %v", err)
	}

	// the registration under the old networkname is fully torn down
	if controller.dhcp.CheckPool("net-old") {
		t.Error("the dhcp pool of the old registration survived the deletion")
	}
	if used := controller.ipam.Used("net-old"); used != 0 {
		t.Errorf("ipam used of net-old = %d, want 0 (the subnet must be deleted)", used)
	}
	if cacheAllocator.Check(oldSpec) {
		t.Error("the cache entry of the old registration survived the deletion")
	}
	if _, live := controller.registeredPools["pool-d"]; live {
		t.Error("the registeredPools record of the renamed pool survived the deletion")
	}
	if len(removedNicIPs) == 0 {
		t.Error("the cleanup never removed the server ip from the bind interface")
	} else if removedNicIPs[0] != "test-fake-iface 192.168.1.1/24" {
		t.Errorf("removed nic ip = %q, want the server ip of the old registration", removedNicIPs[0])
	}

	// no registration ever existed under the new networkname
	if controller.dhcp.CheckPool("net-new") {
		t.Error("a dhcp pool exists under the new networkname")
	}
	if cacheAllocator.Check(newSpec) {
		t.Error("a cache entry exists under the new networkname")
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
	if err := indexer.Add(testPool("pool-j", "net-j", 60)); err != nil {
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

	queue.Add(testPoolEvent("pool-j", ADD, "net-j"))
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
	if err := indexer.Add(testPool("pool-live", "net-live", 60)); err != nil {
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
	stubNicMutation(t)

	// the live registration of pool-a on the shared nic
	live := testPool("pool-a", "net-a", 60)
	rs := ippoolBehaviorNewRestState(live)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	t.Cleanup(srv.Close)

	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-a", "pool-b")

	indexer := newTestIndexer()
	if err := indexer.Add(live); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}
	if err := indexer.Add(testPool("pool-b", "net-b", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, startupGate)
	cs, err := kihclientset.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}
	controller.kihClientset = cs
	controller.runListener = func(networkName string, nic string) error {
		return nil
	}

	// pool-a registers on the shared nic
	if err := controller.sync(testPoolEvent("pool-a", ADD, "net-a")); err != nil {
		t.Fatalf("the first registration failed: %v", err)
	}
	if !controller.dhcp.CheckPool("net-a") {
		t.Fatal("the first registration did not create its dhcp pool")
	}

	// the api serves the second pool's object for its claim protection
	rs.mu.Lock()
	rs.pool = testPool("pool-b", "net-b", 60)
	rs.mu.Unlock()

	// pool-b claims the SAME bindinterface with another network: the
	// registration must reject it definitively
	err = controller.sync(testPoolEvent("pool-b", ADD, "net-b"))
	if err == nil {
		t.Fatal("the duplicate bindinterface registration returned nil, want a rejection")
	}
	if !errors.Is(err, ErrPoolUnregistrable) {
		t.Errorf("error = %v, want the ErrPoolUnregistrable classification", err)
	}

	// none of pool-b's sub-resources exist
	if controller.dhcp.CheckPool("net-b") {
		t.Error("a dhcp pool was created for the rejected duplicate pool")
	}
	if controller.ipam.Used("net-b") != 0 {
		t.Error("an ipam subnet was created for the rejected duplicate pool")
	}
	if cacheAllocator.Check(testPool("pool-b", "net-b", 60)) {
		t.Error("a cache entry was published for the rejected duplicate pool")
	}

	// the first registration is untouched and both pools settled the gate
	// (the rejection is unregistrable, so it counts as handled)
	if !controller.dhcp.CheckPool("net-a") {
		t.Error("the live registration of the first pool must stay untouched")
	}
	if !startupGate.Open() {
		t.Errorf("gate settled = %d out of %d, want the snapshot complete (the rejected pool counts as handled)",
			startupGate.Settled(), startupGate.Target())
	}
}
