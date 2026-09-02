package vm

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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
)

// shutdownWait is the bounded amount of time a test waits for a controller
// goroutine to observe a queue or context shutdown before failing.
const shutdownWait = 5 * time.Second

const (
	// vmnetcfgNotFoundJSON is a metav1.Status response whose message contains
	// "not found", which the controller checks when deciding whether to
	// create a vmnetcfg object.
	vmnetcfgNotFoundJSON = `{"apiVersion":"v1","kind":"Status","status":"Failure","message":"virtualmachinenetworkconfigs.kubevirtiphelper.k8s.binbash.org \"vm-test\" not found","reason":"NotFound","code":404}`

	vmnetcfgBodyJSON = `{"apiVersion":"kubevirtiphelper.k8s.binbash.org/v1","kind":"VirtualMachineNetworkConfig","metadata":{"name":"vm-test","namespace":"default","resourceVersion":"1"},"spec":{"vmname":"vm-test"}}`

	serverErrorJSON = `{"apiVersion":"v1","kind":"Status","status":"Failure","message":"boom","reason":"InternalError","code":500}`
)

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

func newTestController(t *testing.T, queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller, kihClientset *kihclientset.Clientset) *Controller {
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
	)
	t.Cleanup(queue.ShutDown)

	return controller
}

func testEvent(action string) Event {
	return Event{key: "default/vm-test", action: action, vmName: "vm-test", vmNamespace: "default"}
}

func testVirtualMachine(withNIC bool) *kubevirtv1.VirtualMachine {
	vm := &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-test", Namespace: "default"},
		Spec: kubevirtv1.VirtualMachineSpec{
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{},
		},
	}

	if withNIC {
		vm.Spec.Template.Spec.Domain.Devices.Interfaces = []kubevirtv1.Interface{
			{Name: "nic0", MacAddress: "02:00:00:00:00:01"},
		}
		vm.Spec.Template.Spec.Networks = []kubevirtv1.Network{
			{Name: "nic0", NetworkSource: kubevirtv1.NetworkSource{Multus: &kubevirtv1.MultusNetwork{NetworkName: "default/net"}}},
		}
	}

	return vm
}

// requestLog records the requests a fake API server received.
type requestLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *requestLog) record(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, r.Method+" "+r.URL.Path)
}

func (l *requestLog) seenRequest(method, pathSuffix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, req := range l.seen {
		if strings.HasPrefix(req, method+" ") && strings.HasSuffix(req, pathSuffix) {
			return true
		}
	}

	return false
}

func newFakeServer(t *testing.T, log *requestLog, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	return server
}

func newTestClientset(t *testing.T, server *httptest.Server) *kihclientset.Clientset {
	t.Helper()

	client, err := kihclientset.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}

	return client
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func TestProcessNextItemReturnsFalseAfterShutdown(t *testing.T) {
	queue := newTestQueue()
	controller := newTestController(t, queue, newTestIndexer(), nil, nil)

	queue.ShutDown()

	if got := controller.processNextItem(); got {
		t.Errorf("processNextItem() after ShutDown returned true, want false")
	}
}

func TestProcessNextItemSucceedsForMissingIndexObject(t *testing.T) {
	queue := newTestQueue()
	controller := newTestController(t, queue, newTestIndexer(), nil, nil)

	event := testEvent(ADD)
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
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller := newTestController(t, queue, indexer, nil, nil)

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
	controller := newTestController(t, queue, newTestIndexer(), nil, nil)

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
	controller := newTestController(t, queue, newTestIndexer(), nil, nil)

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
	controller := newTestController(t, queue, newTestIndexer(), nil, nil)

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
	controller := newTestController(t, newTestQueue(), newTestIndexer(), nil, nil)

	if err := controller.sync(testEvent(ADD)); err != nil {
		t.Errorf("sync() for a missing index object returned error %v, want nil", err)
	}
}

func TestSyncReturnsIndexerError(t *testing.T) {
	indexer := &failingIndexer{Indexer: newTestIndexer(), err: errors.New("store unavailable")}
	controller := newTestController(t, newTestQueue(), indexer, nil, nil)

	if err := controller.sync(testEvent(ADD)); err == nil {
		t.Fatalf("sync() returned nil, want the indexer error")
	}
}

func TestSyncUpdateSucceedsWhenServerErrors(t *testing.T) {
	// an UPDATE event whose API lookup fails logs the failure and returns
	// without error: handler errors never escape sync
	var log requestLog
	server := newFakeServer(t, &log, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, serverErrorJSON)
	})

	indexer := newTestIndexer()
	indexer.Add(testVirtualMachine(false))
	controller := newTestController(t, newTestQueue(), indexer, nil, newTestClientset(t, server))

	if err := controller.sync(testEvent(UPDATE)); err != nil {
		t.Errorf("sync(UPDATE) returned error %v, want nil", err)
	}

	if !log.seenRequest("GET", "/virtualmachinenetworkconfigs/vm-test") {
		t.Errorf("expected the API lookup for the vmnetcfg object")
	}
}

func TestSyncDeleteSnapshotSkipsDeletedObject(t *testing.T) {
	// a DELETE snapshot whose vmnetcfg object is already gone (API returns
	// not found) must not trigger a second delete request
	var log requestLog
	server := newFakeServer(t, &log, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, vmnetcfgNotFoundJSON)
	})

	controller := newTestController(t, newTestQueue(), newTestIndexer(), nil, newTestClientset(t, server))

	if err := controller.sync(testEvent(DELETE)); err != nil {
		t.Errorf("sync(DELETE) returned error %v, want nil", err)
	}

	if !log.seenRequest("GET", "/virtualmachinenetworkconfigs/vm-test") {
		t.Errorf("expected the controller to check whether the vmnetcfg object still exists")
	}
	if log.seenRequest("DELETE", "/virtualmachinenetworkconfigs/vm-test") {
		t.Errorf("delete was attempted although the vmnetcfg object no longer exists")
	}
}

func TestSyncDeleteSnapshotDeletesReferencedObject(t *testing.T) {
	// a DELETE snapshot whose vmnetcfg object still exists leads to a delete
	// attempt; a failing delete is logged but does not escape sync
	var log requestLog
	server := newFakeServer(t, &log, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, vmnetcfgBodyJSON)
		default:
			writeJSON(w, http.StatusNotFound, vmnetcfgNotFoundJSON)
		}
	})

	controller := newTestController(t, newTestQueue(), newTestIndexer(), nil, newTestClientset(t, server))

	if err := controller.sync(testEvent(DELETE)); err != nil {
		t.Errorf("sync(DELETE) returned error %v, want nil", err)
	}

	if !log.seenRequest("GET", "/virtualmachinenetworkconfigs/vm-test") {
		t.Errorf("expected the controller to check whether the vmnetcfg object still exists")
	}
	if !log.seenRequest("DELETE", "/virtualmachinenetworkconfigs/vm-test") {
		t.Errorf("expected a delete request for the referenced vmnetcfg object")
	}
}

func TestSyncAddSkipsCreateWithoutNetworkConfig(t *testing.T) {
	// an ADD event for a VM without interfaces results in no network
	// configuration, so no vmnetcfg object is created
	var log requestLog
	server := newFakeServer(t, &log, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, vmnetcfgNotFoundJSON)
	})

	indexer := newTestIndexer()
	indexer.Add(testVirtualMachine(false))
	controller := newTestController(t, newTestQueue(), indexer, nil, newTestClientset(t, server))

	if err := controller.sync(testEvent(ADD)); err != nil {
		t.Errorf("sync(ADD) returned error %v, want nil", err)
	}

	if log.seenRequest("POST", "/virtualmachinenetworkconfigs") {
		t.Errorf("a vmnetcfg object was created although the vm has no network configuration")
	}
}

func TestSyncAddCreateFailureSucceeds(t *testing.T) {
	// an ADD event for a VM with a NIC leads to a create attempt; a failing
	// create is logged but does not escape sync
	var log requestLog
	server := newFakeServer(t, &log, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// unknown vmnetcfg object: controller must decide to create
			writeJSON(w, http.StatusNotFound, vmnetcfgNotFoundJSON)
		default:
			// create request fails
			writeJSON(w, http.StatusInternalServerError, serverErrorJSON)
		}
	})

	indexer := newTestIndexer()
	indexer.Add(testVirtualMachine(true))
	controller := newTestController(t, newTestQueue(), indexer, nil, newTestClientset(t, server))

	if err := controller.sync(testEvent(ADD)); err != nil {
		t.Errorf("sync(ADD) returned error %v, want nil", err)
	}

	if !log.seenRequest("GET", "/virtualmachinenetworkconfigs/vm-test") {
		t.Errorf("expected the API lookup for the vmnetcfg object")
	}
	if !log.seenRequest("POST", "/virtualmachinenetworkconfigs") {
		t.Errorf("expected a create request for the vmnetcfg object")
	}
}

func TestRunShutsDownTheQueue(t *testing.T) {
	queue := newTestQueue()
	controller := newTestController(t, queue, newTestIndexer(), &stubInformer{synced: true}, nil)

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
	controller := newTestController(t, queue, newTestIndexer(), nil, nil)

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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	client, err := kihclientset.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}

	kubevirtClient, err := kubecli.GetKubevirtClientFromRESTConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("creating kubevirt client: %v", err)
	}

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
		client,
		kubevirtClient,
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

// ---------------------------------------------------------------------------
// EventHandler kubeconfig discovery (getKubeConfig / Init)
// ---------------------------------------------------------------------------

// writeTempKubeconfig writes a minimal kubeconfig pointing at serverURL with the
// given current-context and returns its path. Each test gets its own temp dir so
// files never collide.
func writeTempKubeconfig(t *testing.T, serverURL, currentContext string) string {
	t.Helper()

	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: %s
users:
- name: test
  user: {}
`, serverURL, currentContext)

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing temp kubeconfig: %v", err)
	}

	return path
}

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
		nil,
	)
}

func TestGetKubeConfigLoadsTempKubeconfig(t *testing.T) {
	// a valid kubeconfig file must produce a rest config pointing at the
	// configured API server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(server.Close)

	handler := newTestEventHandler(writeTempKubeconfig(t, server.URL, "test"), "")

	cfg, err := handler.getKubeConfig()
	if err != nil {
		t.Fatalf("getKubeConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected a rest config, got nil")
	}
	if cfg.Host != server.URL {
		t.Errorf("expected host %s, got %s", server.URL, cfg.Host)
	}
}

func TestGetKubeConfigMissingFileFallsBackToInCluster(t *testing.T) {
	// when the kubeconfig file does not exist the handler falls back to
	// rest.InClusterConfig; without in-cluster env vars that must fail
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	handler := newTestEventHandler(filepath.Join(t.TempDir(), "does-not-exist"), "")

	if _, err := handler.getKubeConfig(); err == nil {
		t.Fatal("expected an error from the in-cluster fallback without in-cluster env")
	}
}

func TestGetKubeConfigMalformedFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("this is not a kubeconfig: {"), 0o600); err != nil {
		t.Fatalf("writing temp kubeconfig: %v", err)
	}

	handler := newTestEventHandler(path, "")

	if _, err := handler.getKubeConfig(); err == nil {
		t.Fatal("expected an error for a malformed kubeconfig")
	}
}

func TestGetKubeConfigUnknownContextFails(t *testing.T) {
	// an explicitly requested kubeconfig context that does not exist must
	// surface as an error instead of silently using another context
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(server.Close)

	handler := newTestEventHandler(writeTempKubeconfig(t, server.URL, "test"), "missing-context")

	if _, err := handler.getKubeConfig(); err == nil {
		t.Fatal("expected an error for an unknown kubeconfig context")
	}
}

func TestInitSucceedsWithTempKubeconfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(server.Close)

	handler := newTestEventHandler(writeTempKubeconfig(t, server.URL, "test"), "")

	if err := handler.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if handler.kubeRestConfig == nil || handler.kubeRestConfig.Host != server.URL {
		t.Errorf("expected kubeRestConfig pointing at %s", server.URL)
	}
	if handler.kihClientset == nil {
		t.Error("expected kihClientset to be initialized")
	}
	if handler.kcli == nil {
		t.Error("expected the kubevirt client to be initialized")
	}
}

func TestInitFailsForMissingKubeconfig(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	handler := newTestEventHandler(filepath.Join(t.TempDir(), "does-not-exist"), "")

	if err := handler.Init(); err == nil {
		t.Fatal("expected Init to fail without a kubeconfig or in-cluster env")
	}
	if handler.kihClientset != nil {
		t.Error("expected kihClientset to stay nil after a failed Init")
	}
}

func TestInitFailsForMalformedKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatalf("writing temp kubeconfig: %v", err)
	}

	handler := newTestEventHandler(path, "")

	if err := handler.Init(); err == nil {
		t.Fatal("expected Init to fail for a malformed kubeconfig")
	}
	if handler.kihClientset != nil {
		t.Error("expected kihClientset to stay nil after a failed Init")
	}
}
