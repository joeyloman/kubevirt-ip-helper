package ippool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestSyncUpdateReturnsErrorWhenPoolNotCached(t *testing.T) {
	// an UPDATE event whose pool is missing from the cache is an invariant
	// violation: it must return the error so the queue retries, instead of
	// silently forgetting the event
	var appStatus int
	indexer := newTestIndexer()
	indexer.Add(testPool("pool-i", "net-i", 60))
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))

	if err := controller.sync(testPoolEvent("pool-i", UPDATE, "net-i")); err == nil {
		t.Error("sync(UPDATE) returned nil, want a rate-limited requeue error for the missing cache entry")
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

	var appStatus int
	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))

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
	var appStatus int
	foreignPool := testPool("pool-a", "net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(foreignPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}
	if err := indexer.Add(testPool("pool-b", "net-dup", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))

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
	var appStatus int
	foreignPool := testPool("pool-a", "net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(foreignPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))

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
	var appStatus int
	storedPool := testPool("pool-a", "net-dup", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(storedPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))

	if err := controller.ipam.NewSubnet("net-dup", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("registering the ipam subnet: %v", err)
	}
	if _, err := controller.ipam.GetIP("net-dup", "192.168.1.10"); err != nil {
		t.Fatalf("allocating the live ip: %v", err)
	}
	if err := controller.dhcp.AddPool("net-dup", "192.168.1.1", "255.255.255.0", "192.168.1.1", nil, "", nil, nil, 60, "test-fake-iface"); err != nil {
		t.Fatalf("registering the dhcp pool: %v", err)
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

// a pool whose networkname changed keeps its cache entry under the old key:
// its update event must still reach the restart handling through it
func TestSyncUpdateReachesRestartAfterNetworkNameChange(t *testing.T) {
	appStatus := APP_RUNNING
	oldPool := testPool("pool-n", "net-old", 60)
	newPool := testPool("pool-n", "net-new", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(newPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	event := testPoolEvent("pool-n", UPDATE, "net-new")
	event.oldPoolNetworkName = "net-old"

	if err := controller.sync(event); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if appStatus != APP_RESTART {
		t.Errorf("app status = %d, want %d after a networkname change", appStatus, APP_RESTART)
	}
}

// renaming a pool into a networkname which a live registration already
// claims would tear the whole application down and then fail during the
// re-registration: that update must be rejected before any teardown, so
// the currently registered configuration keeps serving
func TestSyncUpdateRejectsNetworkNameChangeToClaimedNetwork(t *testing.T) {
	appStatus := APP_RUNNING
	oldPool := testPool("pool-n", "net-old", 60)
	newPool := testPool("pool-n", "net-claimed", 60)

	indexer := newTestIndexer()
	if err := indexer.Add(newPool); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, new(int))
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

	if appStatus != APP_RUNNING {
		t.Errorf("the rejected rename started an application restart: app status got %d, want %d", appStatus, APP_RUNNING)
	}
	if !cacheAllocator.Check(oldPool) {
		t.Error("the rejected rename dropped the live registration from the cache")
	}
}
