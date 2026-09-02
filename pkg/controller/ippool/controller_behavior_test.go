package ippool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func newTestController(t *testing.T, queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller, appStatus *int, ippoolCountCurrent *int) (*Controller, *kihcache.CacheAllocator) {
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
		ippoolCountCurrent,
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

func testPoolEvent(key, action, networkName string) Event {
	return Event{key: key, action: action, poolName: key, poolNetworkName: networkName}
}

func TestProcessNextItemReturnsFalseAfterShutdown(t *testing.T) {
	queue := newTestQueue()
	var appStatus int
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, new(int))

	queue.ShutDown()

	if got := controller.processNextItem(); got {
		t.Errorf("processNextItem() after ShutDown returned true, want false")
	}
}

func TestProcessNextItemSucceedsForMissingIndexObject(t *testing.T) {
	queue := newTestQueue()
	var appStatus int
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, new(int))

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
	var appStatus int
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller, _ := newTestController(t, queue, indexer, nil, &appStatus, new(int))

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
	var appStatus int
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, new(int))

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
	var appStatus int
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, new(int))

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
	var appStatus int
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, new(int))

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
	var appStatus int
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, new(int))

	if err := controller.sync(testPoolEvent("pool-f", ADD, "net-f")); err != nil {
		t.Errorf("sync() for a missing index object returned error %v, want nil", err)
	}
}

func TestSyncReturnsIndexerError(t *testing.T) {
	var appStatus int
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))

	if err := controller.sync(testPoolEvent("pool-g", ADD, "net-g")); err == nil {
		t.Fatalf("sync() returned nil, want the indexer error")
	}
}

func TestSyncDeleteSucceedsWhenPoolNotCached(t *testing.T) {
	// a DELETE snapshot for a pool that is gone from the index and unknown to
	// the cache only logs the cache lookup failure and returns without error
	var appStatus int
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, new(int))

	if err := controller.sync(testPoolEvent("pool-h", DELETE, "net-h")); err != nil {
		t.Errorf("sync(DELETE) returned error %v, want nil", err)
	}
}

func TestSyncUpdateSucceedsWhenPoolNotCached(t *testing.T) {
	// an UPDATE event with an index entry but no cache entry logs the cache
	// miss and returns without error
	var appStatus int
	indexer := newTestIndexer()
	indexer.Add(testPool("pool-i", "net-i", 60))
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))

	if err := controller.sync(testPoolEvent("pool-i", UPDATE, "net-i")); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}
}

func TestSyncUpdateIgnoredWhileInitializing(t *testing.T) {
	appStatus := APP_INIT
	oldPool := testPool("pool-j", "net-j", 60)
	newPool := testPool("pool-j", "net-j", 120)

	indexer := newTestIndexer()
	indexer.Add(newPool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	if err := controller.sync(testPoolEvent("pool-j", UPDATE, "net-j")); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}

	// while initializing, pool updates are deliberately ignored: the cache
	// still holds the originally registered pool
	got, err := cacheAllocator.Get("pool", "net-j")
	if err != nil {
		t.Fatalf("pool missing from cache: %v", err)
	}
	if leaseTime := got.(kihv1.IPPool).Spec.IPv4Config.LeaseTime; leaseTime != 60 {
		t.Errorf("cache lease time = %d after ignored update, want 60", leaseTime)
	}
}

func TestSyncUpdateSkipsIdenticalPoolWhenRunning(t *testing.T) {
	appStatus := APP_RUNNING
	pool := testPool("pool-k", "net-k", 60)

	indexer := newTestIndexer()
	indexer.Add(pool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))
	if err := cacheAllocator.Add(pool); err != nil {
		t.Fatalf("seeding cache: %v", err)
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
}

func TestSyncUpdateReloadsPoolWhenRunning(t *testing.T) {
	appStatus := APP_RUNNING
	oldPool := testPool("pool-l", "net-l", 60)
	newPool := testPool("pool-l", "net-l", 120)

	indexer := newTestIndexer()
	indexer.Add(newPool)

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

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
	var appStatus int
	controller, _ := newTestController(t, queue, newTestIndexer(), &stubInformer{synced: true}, &appStatus, new(int))

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
	var appStatus int
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, new(int))

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
		new(int),
		new(int),
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

func TestSyncAddReturnsNilWhenPoolFailsToRegister(t *testing.T) {
	// an ADD event whose pool has an invalid subnet fails inside registerIPPool
	// before any netlink or dhcp work; sync logs the failure and reports no
	// error to the queue machinery.
	pool := testPool("pool-m", "net-m", 60)
	pool.Spec.IPv4Config.Subnet = "not-a-cidr"

	indexer := newTestIndexer()
	if err := indexer.Add(pool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	var appStatus int
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))

	if err := controller.sync(testPoolEvent("pool-m", ADD, "net-m")); err != nil {
		t.Errorf("sync(ADD) returned error %v, want nil", err)
	}

	if controller.dhcp.CheckPool("net-m") {
		t.Errorf("dhcp pool registered although registerIPPool failed")
	}
	if v, ok := ippoolBehaviorMetricValue(t, controller.metrics, "kubevirtiphelper_app_logs", map[string]string{"loglevel": "error"}); !ok || v != 1 {
		t.Errorf("app log status gauge: got value %v found %v, want exactly 1 error entry", v, ok)
	}
}
