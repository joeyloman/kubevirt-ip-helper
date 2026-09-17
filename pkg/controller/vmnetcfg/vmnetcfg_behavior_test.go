package vmnetcfg

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	prom "github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

const (
	testVMNetCfgName = "vm-test"
	testNamespace    = "default"
	testVMName       = "vm-test"
	testPoolName     = "ippool-test"
	testNetwork      = "default/net-test"
	testSubnet       = "10.0.0.0/29"

	testMAC  = "02:00:00:00:00:01"
	testMAC2 = "02:00:00:00:00:02"
)

const (
	// competingAllocationIP is the sentinel address a conflict injects into
	// the pool status, simulating the allocation of another writer whose
	// entry must survive any subsequent retry
	competingAllocationIP = "10.99.0.99"
)

const (
	apiPrefix = "/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

var (
	vmnetcfgMainPath   = apiPrefix + "/namespaces/" + testNamespace + "/virtualmachinenetworkconfigs/" + testVMNetCfgName
	vmnetcfgStatusPath = vmnetcfgMainPath + "/status"
	ippoolPath         = apiPrefix + "/ippools/" + testPoolName
	ippoolStatusPath   = ippoolPath + "/status"
)

const (
	metricAppLogs        = "kubevirtiphelper_app_logs"
	metricIPPoolUsed     = "kubevirtiphelper_ippool_used"
	metricIPPoolAvail    = "kubevirtiphelper_ippool_available"
	metricVMNetCfgStatus = "kubevirtiphelper_vmnetcfg_status"
)

// testEnv bundles the in-memory allocators, the metrics registry and a controller
// wired to a real generated clientset backed by an httptest fake API server.
type testEnv struct {
	t           *testing.T
	srv         *httptest.Server
	api         *fakeAPIServer
	client      *kihclientset.Clientset
	cache       *kihcache.CacheAllocator
	ipam        *ipam.IPAllocator
	dhcp        *dhcp.DHCPAllocator
	metrics     *metrics.MetricsAllocator
	indexer     cache.Indexer
	controller  *Controller
	scope       util.NetworkScope
	reconcileMu *sync.Mutex
	// queue exposes the workqueue of the controller so the deferred-key
	// requeue tests can process requeued events through processNextItem
	queue workqueue.RateLimitingInterface
	// appStatus exposes the application phase so tests can switch
	// between the startup replay (APP_INIT, fresh allocations deferred)
	// and the steady state (APP_RUNNING)
	appStatus *atomic.Int32
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	api := newFakeAPIServer()
	srv := httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	t.Cleanup(srv.Close)

	client, err := kihclientset.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("creating clientset: %s", err)
	}

	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	e := &testEnv{
		t:           t,
		srv:         srv,
		api:         api,
		client:      client,
		cache:       kihcache.NewCacheAllocator(),
		ipam:        ipam.NewIPAllocator(),
		dhcp:        dhcp.NewDHCPAllocator(),
		metrics:     metrics.NewMetricsAllocator(),
		scope:       testNetworkScope(t, testNamespace, "net-test"),
		reconcileMu: &sync.Mutex{},
	}
	// a real queue and indexer so the startup-replay tests can drive
	// requeued events through the controller
	e.indexer = newTestIndexer()
	e.queue = newTestQueue()
	e.controller = NewController(context.Background(), e.queue, e.indexer, nil, e.cache, e.ipam, e.dhcp, e.metrics, e.client, &appStatus, nil, e.scope, e.reconcileMu)
	e.appStatus = &appStatus
	t.Cleanup(e.queue.ShutDown)

	return e
}

func testNetworkScope(t *testing.T, namespace, name string) util.NetworkScope {
	t.Helper()
	scope, err := util.NewNetworkScope(namespace, name)
	if err != nil {
		t.Fatalf("creating network scope: %s", err)
	}
	return scope
}

// addSubnet registers the test subnet in the ipam allocator. The range must be a
// strict subset of the subnet (never the broadcast address).
func (e *testEnv) addSubnet(start, end string) {
	e.t.Helper()
	if err := e.ipam.NewSubnet(testNetwork, testSubnet, start, end); err != nil {
		e.t.Fatalf("adding subnet: %s", err)
	}
}

// seedPoolWith registers the pool in both the in-memory cache (the controller
// type-asserts the cached value) and the fake API server.
func (e *testEnv) seedPoolWith(pool *kihv1.IPPool) {
	e.t.Helper()
	if pool.Labels == nil {
		namespace, name, qualified := strings.Cut(pool.Spec.NetworkName, "/")
		if qualified {
			pool.Labels = map[string]string{util.NetworkLabel: name, util.NetworkNamespaceLabel: namespace}
		}
	}
	if err := e.cache.Add(pool); err != nil {
		e.t.Fatalf("seeding pool cache: %s", err)
	}
	e.api.seedPool(pool)
}

// seedPool registers a default pool with the given status allocation map.
func (e *testEnv) seedPool(allocated map[string]string) *kihv1.IPPool {
	e.t.Helper()
	pool := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: testPoolName, Labels: map[string]string{util.NetworkLabel: e.scope.Name(), util.NetworkNamespaceLabel: e.scope.Namespace()}},
		Spec: kihv1.IPPoolSpec{
			NetworkName: testNetwork,
			IPv4Config:  kihv1.IPv4Config{Subnet: testSubnet, ServerIP: "10.0.0.1"},
		},
		Status: kihv1.IPPoolStatus{
			IPv4: kihv1.IPv4Status{Allocated: allocated},
		},
	}
	e.seedPoolWith(pool)
	return pool
}

func (e *testEnv) seedVMNetCfg(obj *kihv1.VirtualMachineNetworkConfig) {
	e.t.Helper()
	e.api.seedVMNetCfg(obj)
}

func (e *testEnv) getStoredVMNetCfg() *kihv1.VirtualMachineNetworkConfig {
	e.t.Helper()
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	obj, ok := e.api.vmnetcfgs[testNamespace+"/"+testVMNetCfgName]
	if !ok {
		e.t.Fatalf("vmnetcfg %s not present in fake server", testNamespace+"/"+testVMNetCfgName)
	}
	return obj.DeepCopy()
}

func (e *testEnv) getStoredPool() *kihv1.IPPool {
	e.t.Helper()
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	pool, ok := e.api.ippools[testPoolName]
	if !ok {
		e.t.Fatalf("ippool %s not present in fake server", testPoolName)
	}
	return pool.DeepCopy()
}

func (e *testEnv) countRequests(method, path string) int {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	n := 0
	for _, r := range e.api.requests {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func (e *testEnv) totalRequests() int {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	return len(e.api.requests)
}

func (e *testEnv) countMetricsByLabel(name, labelName, labelValue string) int {
	e.t.Helper()
	count := 0
	for _, mf := range vmnetcfgBehaviorGatherMetrics(e.t, e.metrics) {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == labelName && lp.GetValue() == labelValue {
					count++
					break
				}
			}
		}
	}
	return count
}

func (e *testEnv) metricValue(name string, labels map[string]string) (float64, bool) {
	e.t.Helper()
	for _, mf := range vmnetcfgBehaviorGatherMetrics(e.t, e.metrics) {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if metricLabelsEqual(m, labels) {
				if g := m.GetGauge(); g != nil {
					return g.GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// vmnetcfgBehaviorGatherMetrics scrapes the (unexported) prometheus registry of a
// metrics allocator via reflect, avoiding any change to production code.
func vmnetcfgBehaviorGatherMetrics(t *testing.T, m *metrics.MetricsAllocator) []*dto.MetricFamily {
	t.Helper()

	registryField := reflect.ValueOf(m).Elem().FieldByName("registry")
	if !registryField.CanAddr() {
		t.Fatal("metrics registry field is not addressable")
	}
	registry := *(**prom.Registry)(unsafe.Pointer(registryField.UnsafeAddr()))

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %s", err.Error())
	}
	return families
}

func metricLabelsEqual(m *dto.Metric, want map[string]string) bool {
	if len(m.GetLabel()) != len(want) {
		return false
	}
	for _, lp := range m.GetLabel() {
		if v, ok := want[lp.GetName()]; !ok || lp.GetValue() != v {
			return false
		}
	}
	return true
}

func newVMNetCfg(ip string, mac string) *kihv1.VirtualMachineNetworkConfig {
	return &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      testVMNetCfgName,
		},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName: testVMName,
			NetworkConfig: []kihv1.NetworkConfig{
				{IPAddress: ip, MACAddress: mac, NetworkName: testNetwork},
			},
		},
	}
}

type reqRecord struct {
	method string
	path   string
}

type fakeAPIServer struct {
	mu                sync.Mutex
	vmnetcfgs         map[string]*kihv1.VirtualMachineNetworkConfig
	ippools           map[string]*kihv1.IPPool
	requests          []reqRecord
	conflictPath      string
	conflictCount     int
	poolStatusPutCode int
	vmnetcfgPutCode   int
	// vmnetcfgPutDropConn commits every vmnetcfg PUT but closes the
	// connection before a response byte is written: the client observes
	// a lost response (EOF) for a write the server actually applied,
	// which is the boundary an Update error does not prove a non-commit
	vmnetcfgPutDropConn bool
	// vmnetcfgPutConflict answers the next vmnetcfg main-resource PUTs
	// with a resourceVersion conflict after applying the competing write
	// the conflict implies: vmnetcfgPutConflictFn mutates the stored
	// object first (the vm controller's spec update landing between the
	// pre-commit verification GET and the commit), so the retried stale
	// write can never pass. the commit-conflict regression uses it to
	// place a spec write inside the verification-to-commit window.
	vmnetcfgPutConflict         int
	vmnetcfgPutConflictFn       func(obj *kihv1.VirtualMachineNetworkConfig)
	vmnetcfgStatusPutCode       int
	vmnetcfgStatusPutConflict   int
	vmnetcfgStatusPutConflictFn func(obj *kihv1.VirtualMachineNetworkConfig)
	// Before-write hooks run without the server mutex, allowing another
	// helper to finish an actual API reconciliation inside the commit window.
	vmnetcfgBeforePut func(subresource string)
	poolListCode      int
	// vmnetcfgGetCode fails the vmnetcfg GET requests while set, so the
	// pre-commit verification of the claimed nics can be made to fail
	vmnetcfgGetCode  int
	vmnetcfgListCode int
	// vmnetcfgDeleteStatus fails the vmnetcfg DELETE requests while set,
	// so the orphan sweep's delete can be made to fail
	vmnetcfgDeleteStatus int
	// vmnetcfgDeletes records the DeleteOptions body of every vmnetcfg
	// DELETE the fake served, so tests can assert the uid precondition
	vmnetcfgDeletes []metav1.DeleteOptions
	// blockPoolStatusPut, when non-nil, parks every pool status PUT until
	// the channel is closed: the Run-join test holds the worker inside its
	// in-flight reconciliation deterministically
	blockPoolStatusPut chan struct{}
	// optional single-shot interleaving hooks for the concurrency
	// regression tests: they run after the pool request was processed,
	// simulating a concurrent vm controller cleanup acting between the
	// reconciliation steps. the hooks are read under the mutex and are
	// responsible for their own locking and once semantics.
	poolGetHook func()
	poolPutHook func()
	// Runs after decoding a pool status PUT and before resourceVersion
	// validation, allowing an owner-specific competing ledger commit.
	poolBeforeStatusPut func(submitted *kihv1.IPPool)
}

func newFakeAPIServer() *fakeAPIServer {
	return &fakeAPIServer{
		vmnetcfgs: map[string]*kihv1.VirtualMachineNetworkConfig{},
		ippools:   map[string]*kihv1.IPPool{},
	}
}

func (f *fakeAPIServer) seedPool(pool *kihv1.IPPool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pool.ObjectMeta.ResourceVersion == "" {
		pool.ObjectMeta.ResourceVersion = "1"
	}
	f.ippools[pool.Name] = pool.DeepCopy()
}

// runPoolInterleaveHooks fires the optional interleaving hooks after the
// ippool handler served its request.
func (f *fakeAPIServer) runPoolInterleaveHooks(method string, path string) {
	f.mu.Lock()
	getHook, putHook := f.poolGetHook, f.poolPutHook
	f.mu.Unlock()

	if getHook != nil && method == http.MethodGet {
		getHook()
	}

	if putHook != nil && method == http.MethodPut && strings.HasSuffix(path, "/status") {
		putHook()
	}
}

func (f *fakeAPIServer) seedVMNetCfg(obj *kihv1.VirtualMachineNetworkConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if obj.ObjectMeta.ResourceVersion == "" {
		obj.ObjectMeta.ResourceVersion = "1"
	}
	f.vmnetcfgs[obj.Namespace+"/"+obj.Name] = obj.DeepCopy()
}

// bumpResourceVersion mimics the apiserver increasing the resourceVersion
// whenever an object is written.
func bumpResourceVersion(meta metav1.Object) {
	rv, _ := strconv.Atoi(meta.GetResourceVersion())
	meta.SetResourceVersion(strconv.Itoa(rv + 1))
}

func (f *fakeAPIServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, reqRecord{method: r.Method, path: r.URL.Path})
	f.mu.Unlock()

	if !strings.HasPrefix(r.URL.Path, apiPrefix) {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
		return
	}

	p := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, apiPrefix), "/"), "/")
	switch p[0] {
	case "ippools":
		if len(p) < 2 {
			// the cluster-wide list serves the deletion-path pool
			// verification: it decides whether a cache-missed pool is
			// truly gone (its ledger died with it) or merely missed the
			// cache
			f.mu.Lock()
			if f.poolListCode != 0 {
				code := f.poolListCode
				f.mu.Unlock()
				writeStatus(w, code, metav1.StatusReasonInternalError, "pool list unavailable")
				return
			}
			list := &kihv1.IPPoolList{}
			for _, pool := range f.ippools {
				list.Items = append(list.Items, *pool.DeepCopy())
			}
			f.mu.Unlock()
			writeJSON(w, list)

			return
		}
		name := p[1]
		sub := ""
		if len(p) > 2 {
			sub = p[2]
		}
		f.handleIPPool(w, r, name, sub)
		f.runPoolInterleaveHooks(r.Method, r.URL.Path)
	case "namespaces":
		if len(p) < 3 || p[2] != "virtualmachinenetworkconfigs" {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
			return
		}
		ns := p[1]
		if len(p) == 3 && r.Method == http.MethodGet {
			f.mu.Lock()
			if f.vmnetcfgListCode != 0 {
				code := f.vmnetcfgListCode
				f.mu.Unlock()
				writeStatus(w, code, metav1.StatusReasonInternalError, "VMNetCfg list unavailable")
				return
			}
			list := &kihv1.VirtualMachineNetworkConfigList{}
			for _, obj := range f.vmnetcfgs {
				if obj.Namespace == ns {
					list.Items = append(list.Items, *obj.DeepCopy())
				}
			}
			f.mu.Unlock()
			writeJSON(w, list)
			return
		}
		if len(p) < 4 {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "object name required")
			return
		}
		name := p[3]
		sub := ""
		if len(p) > 4 {
			sub = p[4]
		}
		f.handleVMNetCfg(w, r, ns, name, sub)
	default:
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
	}
}

func (f *fakeAPIServer) handleIPPool(w http.ResponseWriter, r *http.Request, name, sub string) {
	switch {
	case r.Method == http.MethodGet && sub == "":
		f.mu.Lock()
		pool, ok := f.ippools[name]
		f.mu.Unlock()
		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
			return
		}
		f.writePool(w, pool)
	case r.Method == http.MethodPut && sub == "status":
		f.mu.Lock()
		block := f.blockPoolStatusPut
		f.mu.Unlock()
		if block != nil {
			<-block
		}
		f.mu.Lock()
		conflict := f.conflictPath == r.URL.Path && f.conflictCount > 0
		if conflict {
			f.conflictCount--
		}
		failCode := f.poolStatusPutCode
		f.mu.Unlock()
		if conflict {
			// mimic a competing writer: the stored version advances and a
			// foreign allocation appears in the status, so a retried stale
			// write cannot pass the fake
			f.mu.Lock()
			if pool, found := f.ippools[name]; found {
				bumpResourceVersion(pool)
				if pool.Status.IPv4.Allocated == nil {
					pool.Status.IPv4.Allocated = map[string]string{}
				}
				pool.Status.IPv4.Allocated[competingAllocationIP] = "other-writer [aa:11:22:33:44:55]"
			}
			f.mu.Unlock()
			writeStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "please apply your changes to the latest version and try again")
			return
		}
		if failCode != 0 {
			writeStatus(w, failCode, metav1.StatusReasonInternalError, "boom")
			return
		}
		var pool kihv1.IPPool
		if err := decodeBody(r, &pool); err != nil {
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
			return
		}
		f.mu.Lock()
		beforePut := f.poolBeforeStatusPut
		f.mu.Unlock()
		if beforePut != nil {
			beforePut(&pool)
		}
		f.mu.Lock()
		stored, found := f.ippools[name]
		if !found {
			f.mu.Unlock()
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
			return
		}
		// reject writes that are not based on the latest stored version or
		// whose body does not match the requested object identity
		if submitted := pool.ObjectMeta.ResourceVersion; submitted == "" || stored.ObjectMeta.ResourceVersion != submitted {
			f.mu.Unlock()
			writeStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "please apply your changes to the latest version and try again")
			return
		}
		if pool.ObjectMeta.Name != name {
			f.mu.Unlock()
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, "the object name does not match the requested object")
			return
		}
		bumpResourceVersion(&pool)
		f.ippools[name] = pool.DeepCopy()
		f.mu.Unlock()
		f.writePool(w, &pool)
	case r.Method == http.MethodPut && sub == "":
		var pool kihv1.IPPool
		if err := decodeBody(r, &pool); err != nil {
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
			return
		}
		f.mu.Lock()
		if pool.ObjectMeta.Name != name {
			f.mu.Unlock()
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, "the object name does not match the requested object")
			return
		}
		bumpResourceVersion(&pool)
		f.ippools[name] = pool.DeepCopy()
		f.mu.Unlock()
		f.writePool(w, &pool)
	default:
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
	}
}

func (f *fakeAPIServer) handleVMNetCfg(w http.ResponseWriter, r *http.Request, ns, name, sub string) {
	key := ns + "/" + name
	if r.Method == http.MethodPut {
		f.mu.Lock()
		hook := f.vmnetcfgBeforePut
		f.mu.Unlock()
		if hook != nil {
			hook(sub)
		}
	}
	switch {
	case r.Method == http.MethodGet && sub == "":
		f.mu.Lock()
		obj, ok := f.vmnetcfgs[key]
		if ok {
			obj = obj.DeepCopy()
		}
		failCode := f.vmnetcfgGetCode
		f.mu.Unlock()
		if failCode != 0 {
			writeStatus(w, failCode, metav1.StatusReasonInternalError, "boom")
			return
		}
		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
			return
		}
		f.writeVMNetCfg(w, obj)
	case r.Method == http.MethodPut && sub == "":
		f.mu.Lock()
		failCode := f.vmnetcfgPutCode
		dropConn := f.vmnetcfgPutDropConn
		conflict := f.vmnetcfgPutConflict > 0
		if conflict {
			f.vmnetcfgPutConflict--
			// apply the competing write the conflict implies before the
			// rejection: the stored object advances like the real apiserver
			// would have advanced it, so a retried stale write cannot pass
			if f.vmnetcfgPutConflictFn != nil {
				if stored, found := f.vmnetcfgs[key]; found {
					f.vmnetcfgPutConflictFn(stored)
					bumpResourceVersion(stored)
				}
			}
		}
		f.mu.Unlock()
		if conflict {
			writeStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "please apply your changes to the latest version and try again")
			return
		}
		if failCode != 0 {
			writeStatus(w, failCode, metav1.StatusReasonInternalError, "boom")
			return
		}
		var obj kihv1.VirtualMachineNetworkConfig
		if err := decodeBody(r, &obj); err != nil {
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
			return
		}
		f.mu.Lock()
		stored, found := f.vmnetcfgs[key]
		if !found {
			f.mu.Unlock()
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "object not found")
			return
		}
		if obj.ResourceVersion != stored.ResourceVersion {
			f.mu.Unlock()
			writeStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "stale resourceVersion")
			return
		}
		obj.Status = *stored.Status.DeepCopy()
		bumpResourceVersion(&obj)
		f.vmnetcfgs[key] = obj.DeepCopy()
		f.mu.Unlock()
		if dropConn {
			// the write is committed: lose the response instead, so the
			// client cannot know whether its update was applied
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
		}
		f.writeVMNetCfg(w, &obj)
	case r.Method == http.MethodPut && sub == "status":
		f.mu.Lock()
		failCode := f.vmnetcfgStatusPutCode
		conflict := f.vmnetcfgStatusPutConflict > 0
		if conflict {
			f.vmnetcfgStatusPutConflict--
			if stored := f.vmnetcfgs[key]; stored != nil {
				if f.vmnetcfgStatusPutConflictFn != nil {
					f.vmnetcfgStatusPutConflictFn(stored)
				}
				bumpResourceVersion(stored)
			}
		}
		f.mu.Unlock()
		if conflict {
			writeStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "stale resourceVersion")
			return
		}
		if failCode != 0 {
			writeStatus(w, failCode, metav1.StatusReasonInternalError, "status unavailable")
			return
		}
		var obj kihv1.VirtualMachineNetworkConfig
		if err := decodeBody(r, &obj); err != nil {
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
			return
		}
		f.mu.Lock()
		stored, found := f.vmnetcfgs[key]
		if !found {
			f.mu.Unlock()
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
			return
		}
		// Both subresources enforce resourceVersion preconditions.
		if obj.ResourceVersion != stored.ResourceVersion {
			f.mu.Unlock()
			writeStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "please apply your changes to the latest version and try again")
			return
		}
		// a status subresource write only replaces the status of the
		// stored object: it must not silently restore a concurrently
		// removed spec or metadata
		bumpResourceVersion(stored)
		stored.Status = *obj.Status.DeepCopy()
		response := stored.DeepCopy()
		f.mu.Unlock()
		f.writeVMNetCfg(w, response)
	case r.Method == http.MethodDelete && sub == "":
		f.mu.Lock()
		obj, ok := f.vmnetcfgs[key]
		failCode := f.vmnetcfgDeleteStatus
		f.mu.Unlock()
		if failCode != 0 {
			writeStatus(w, failCode, metav1.StatusReasonInternalError, "boom")
			return
		}
		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
			return
		}
		// a uid precondition must match the stored object, like a real
		// apiserver: a same-name replacement created after the caller's
		// snapshot is rejected with a conflict instead of being destroyed
		opts := metav1.DeleteOptions{}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, readErr.Error())
			return
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &opts); err != nil {
				writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
				return
			}
		}
		f.mu.Lock()
		f.vmnetcfgDeletes = append(f.vmnetcfgDeletes, opts)
		preconditionMismatch := opts.Preconditions != nil && opts.Preconditions.UID != nil && obj.UID != *opts.Preconditions.UID
		if opts.Preconditions != nil && opts.Preconditions.ResourceVersion != nil && obj.ResourceVersion != *opts.Preconditions.ResourceVersion {
			preconditionMismatch = true
		}
		if !preconditionMismatch && len(obj.Finalizers) > 0 {
			// like the real apiserver, a finalizer-carrying object only
			// gets its deletionTimestamp: the finalizer cleanup owns the
			// actual removal
			now := metav1.Now()
			obj.ObjectMeta.DeletionTimestamp = &now
			bumpResourceVersion(&obj.ObjectMeta)
			f.vmnetcfgs[key] = obj.DeepCopy()
		} else if !preconditionMismatch {
			delete(f.vmnetcfgs, key)
		}
		f.mu.Unlock()
		if preconditionMismatch {
			writeStatus(w, http.StatusConflict, metav1.StatusReasonConflict,
				"Operation cannot be fulfilled on virtualmachinenetworkconfigs: the UID in the precondition does not match the UID in record")
			return
		}
		if obj.ObjectMeta.DeletionTimestamp != nil {
			f.writeVMNetCfg(w, obj)
			return
		}

		// a finalizer-free object is removed at once, like the real
		// apiserver answers a delete with a success status
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(&metav1.Status{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
			Status:   metav1.StatusSuccess,
		})
	default:
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
	}
}

func (f *fakeAPIServer) writePool(w http.ResponseWriter, pool *kihv1.IPPool) {
	out := pool.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: kihv1.SchemeGroupVersion.String(), Kind: "IPPool"}
	writeJSON(w, out)
}

func (f *fakeAPIServer) writeVMNetCfg(w http.ResponseWriter, obj *kihv1.VirtualMachineNetworkConfig) {
	out := obj.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: kihv1.SchemeGroupVersion.String(), Kind: "VirtualMachineNetworkConfig"}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, obj interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(obj); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	st := &metav1.Status{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Status",
		},
		Status:  metav1.StatusFailure,
		Reason:  reason,
		Message: message,
		Code:    int32(code),
	}
	_ = json.NewEncoder(w).Encode(st)
}

func decodeBody(r *http.Request, obj interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(obj)
}

func TestVMNetCfgFreshAllocation(t *testing.T) {
	t.Run("automatic", func(t *testing.T) {
		e := newTestEnv(t)
		// steady state: a running application serves fresh allocations
		// immediately; the startup replay defers them instead
		e.appStatus.Store(APP_RUNNING)
		e.addSubnet("10.0.0.1", "10.0.0.1")
		e.seedPool(nil)
		vmnetcfg := newVMNetCfg("", testMAC)
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		stored := e.getStoredVMNetCfg()
		if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" {
			t.Errorf("spec ip = %q, want 10.0.0.1", got)
		}
		if got := stored.Status.NetworkConfig[0]; got.Status != "OK" || got.Message != "IP address successfully allocated" {
			t.Errorf("status = %+v, want OK", got)
		}

		if !e.dhcp.CheckLease(testMAC) {
			t.Fatal("lease was not added")
		}
		lease := e.dhcp.GetLease(testMAC)
		if lease.ClientIP.String() != "10.0.0.1" || lease.Reference != testNamespace+"/"+testVMName {
			t.Errorf("lease = %+v, want ip 10.0.0.1 ref %s/%s", lease, testNamespace, testVMName)
		}

		pool := e.getStoredPool()
		if got := pool.Status.IPv4.Allocated["10.0.0.1"]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
			t.Errorf("allocated = %q, want %s/%s [%s]", got, testNamespace, testVMName, testMAC)
		}
		if pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 0 {
			t.Errorf("used/available = %d/%d, want 1/0", pool.Status.IPv4.Used, pool.Status.IPv4.Available)
		}
		if pool.Status.LastUpdate.Time.IsZero() {
			t.Error("pool lastupdate was not set")
		}

		if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 1 {
			t.Errorf("main update requests = %d, want 1", n)
		}
		if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 1 {
			t.Errorf("status update requests = %d, want 1", n)
		}
		if n := e.countRequests(http.MethodPut, ippoolStatusPath); n != 1 {
			t.Errorf("ippool status update requests = %d, want 1", n)
		}

		if v, ok := e.metricValue(metricIPPoolUsed, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 1 {
			t.Errorf("ippool used metric = %v (present %v), want 1", v, ok)
		}
		if v, ok := e.metricValue(metricIPPoolAvail, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 0 {
			t.Errorf("ippool available metric = %v (present %v), want 0", v, ok)
		}
		if v, ok := e.metricValue(metricVMNetCfgStatus, map[string]string{
			"vm": testNamespace + "/" + testVMNetCfgName, "network": testNetwork, "mac": testMAC, "ip": "10.0.0.1", "status": "OK",
		}); !ok || v != 1 {
			t.Errorf("vmnetcfg status metric = %v (present %v), want 1", v, ok)
		}
	})

	t.Run("requested", func(t *testing.T) {
		e := newTestEnv(t)
		e.addSubnet("10.0.0.1", "10.0.0.1")
		e.seedPool(nil)
		vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		stored := e.getStoredVMNetCfg()
		if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" {
			t.Errorf("spec ip = %q, want requested 10.0.0.1", got)
		}
		if got := stored.Status.NetworkConfig[0].Status; got != "OK" {
			t.Errorf("status = %q, want OK", got)
		}
		lease := e.dhcp.GetLease(testMAC)
		if lease.ClientIP.String() != "10.0.0.1" {
			t.Errorf("lease ip = %s, want 10.0.0.1", lease.ClientIP.String())
		}
	})
}

func TestVMNetCfgOwnershipRejection(t *testing.T) {
	e := newTestEnv(t)
	e.seedPool(nil)
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.5", "other-ns/other-vm"); err != nil {
		t.Fatalf("seeding lease: %s", err)
	}
	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Status.NetworkConfig[0]; got.Status != "ERROR" || got.Message != "macaddress belongs to another vm" {
		t.Errorf("status = %+v, want ERROR macaddress belongs to another vm", got)
	}
	// the foreign lease must be untouched
	if lease := e.dhcp.GetLease(testMAC); lease.Reference != "other-ns/other-vm" {
		t.Errorf("lease reference = %q, want other-ns/other-vm", lease.Reference)
	}
	// no allocation writes: no main update, no ippool interaction
	if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 0 {
		t.Errorf("main update requests = %d, want 0", n)
	}
	if n := e.countRequests(http.MethodGet, ippoolPath); n != 0 {
		t.Errorf("ippool get requests = %d, want 0", n)
	}
	if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 1 {
		t.Errorf("status update requests = %d, want 1", n)
	}
	if v, ok := e.metricValue(metricAppLogs, map[string]string{"loglevel": "error"}); !ok || v < 1 {
		t.Errorf("error log metric = %v (present %v), want >= 1", v, ok)
	}
}

// TestVMNetCfgStickyErrorDuringStartupReplay pins that a nic in the ERROR
// status keeps its reached state untouched while the application is still
// in the startup replay: the ADD sync and the APP_INIT UPDATEs must not
// allocate or tear down for an interface which the gate already counted.
func TestVMNetCfgStickyErrorDuringStartupReplay(t *testing.T) {
	e := newTestEnv(t)
	e.seedPool(nil)
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "ERROR", Message: "ipam error: no more ips left in network net-test"},
	}
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if len(stored.Status.NetworkConfig) != 1 {
		t.Fatalf("status entries = %d, want 1", len(stored.Status.NetworkConfig))
	}
	if got := stored.Status.NetworkConfig[0]; got.Status != "ERROR" || got.Message != "ipam error: no more ips left in network net-test" {
		t.Errorf("status = %+v, want sticky ERROR", got)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must be created for a sticky error nic")
	}
	if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 0 {
		t.Errorf("main update requests = %d, want 0", n)
	}
	// the rebuilt status equals the persisted one: the churn guard skips
	// the write instead of bumping the resourceVersion once per resync
	if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 0 {
		t.Errorf("status update requests = %d, want 0 (unchanged status must not be rewritten)", n)
	}
}

// TestVMNetCfgErrorRetryHealsOnUpdate pins the steady-state retry of a
// transient ERROR: once the underlying cause is gone (the pool freed the
// address), the next resynced UPDATE restores the interface instead of
// keeping the sticky error forever.
func TestVMNetCfgErrorRetryHealsOnUpdate(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "ERROR", Message: "ipam error: no more ips left in network net-test"},
	}
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Status.NetworkConfig[0]; got.Status != "OK" {
		t.Errorf("status = %+v, want OK after the healed retry", got)
	}
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" {
		t.Errorf("spec ip = %q, want the freed 10.0.0.1", got)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("lease ip = %s, want 10.0.0.1", lease.ClientIP.String())
	}
}

// TestVMNetCfgErrorRetryRecordsFreshError pins that a re-attempted ERROR
// nic which still fails records the new failure instead of escalating or
// leaking state.
func TestVMNetCfgErrorRetryRecordsFreshError(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	// a one-address pool which is already taken: the retried allocation
	// must fail again, recording a fresh ERROR
	e.addSubnet("10.0.0.1", "10.0.0.1")
	if _, err := e.ipam.AllocateIP(testNetwork, "other-owner"); err != nil {
		t.Fatalf("occupying the only address: %s", err)
	}
	e.seedPool(nil)
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "ERROR", Message: "ipam error: no more ips left in network net-test"},
	}
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Status.NetworkConfig[0]; got.Status != "ERROR" || !strings.Contains(got.Message, "no more ips left") {
		t.Errorf("status = %+v, want a fresh ERROR", got)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must be created for a still-failing retry")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want the single foreign allocation only", used)
	}
}

// TestVMNetCfgErrorRetryHealsWithIntactLease pins the lease-idempotent
// retry of a transient ERROR: the failed interface already holds its
// lease, its claim and its pool ownership record (an earlier sync failed
// after the allocation was applied), so the re-attempt verifies the
// binding and repairs the record - and it must publish the success status
// of this sync instead of carrying the previous ERROR entry over
// verbatim, which left the status and its metric stuck on ERROR for an
// interface which serves, while every resync re-attempted it again.
func TestVMNetCfgErrorRetryHealsWithIntactLease(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")

	// the binding is fully applied: the claim, the lease and the pool
	// ownership record all exist
	ownerRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", ownerRef); err != nil {
		t.Fatalf("claiming the address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the lease: %s", err)
	}
	e.seedPool(map[string]string{"10.0.0.1": ownerRef})

	// only the published status still records the failure of the
	// earlier sync
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "ERROR", Message: "the IPPool status could not be updated"},
	}
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if len(stored.Status.NetworkConfig) != 1 {
		t.Fatalf("status entries = %d, want 1", len(stored.Status.NetworkConfig))
	}
	if got := stored.Status.NetworkConfig[0]; got.Status != "OK" {
		t.Errorf("status = %+v, want OK for the re-attempted interface which serves", got)
	}
	if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 1 {
		t.Errorf("status update requests = %d, want 1", n)
	}

	// the verified binding keeps its address, its lease and its record
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" {
		t.Errorf("spec ip = %q, want the verified 10.0.0.1", got)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("the verified lease must remain")
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != ownerRef {
		t.Errorf("pool status entry = %q, want the intact owner record", got)
	}

	// the metric follows the published status: the ERROR series is gone
	wantLabel := map[string]string{
		"vm":      testNamespace + "/" + testVMNetCfgName,
		"network": testNetwork,
		"mac":     testMAC,
		"ip":      "10.0.0.1",
		"status":  "OK",
	}
	if v, ok := e.metricValue(metricVMNetCfgStatus, wantLabel); !ok || v != 1 {
		t.Errorf("metric for %v = %v (present %v), want 1", wantLabel, v, ok)
	}
	if n := e.countMetricsByLabel(metricVMNetCfgStatus, "status", "ERROR"); n != 0 {
		t.Errorf("ERROR status metric series = %d, want 0 after the healed re-attempt", n)
	}
}

// TestVMNetCfgErrorSkipHijackMarker pins that the terminal hijack rejection
// is never re-attempted: an object which was created while the operator was
// down must stay unrserved even though the steady-state retries re-run
// every other error.
func TestVMNetCfgErrorSkipHijackMarker(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	// Historical persisted marker: changing the production constant must not
	// silently make old rejected assignments eligible for allocation.
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "ERROR", Message: "vmnetcfg was manually created after this program was (re)started, preventing possible ip hijack"},
	}
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Status.NetworkConfig[0]; got.Status != "ERROR" || got.Message != vmnetcfg.Status.NetworkConfig[0].Message {
		t.Errorf("status = %+v, want the preserved hijack ERROR", got)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must be created for a hijack-marked nic")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 for the hijack-marked nic", used)
	}
}

func TestVMNetCfgExistingLeaseIdempotency(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)
	owner := util.AllocationRef(testNamespace, testVMName, testMAC)
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", owner); err != nil {
		t.Fatalf("seeding existing claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding lease: %s", err)
	}
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "OK", Message: "IP address successfully allocated"},
	}
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if !reflect.DeepEqual(stored.Spec, vmnetcfg.Spec) || !reflect.DeepEqual(stored.Status, vmnetcfg.Status) {
		t.Errorf("existing binding projection changed: spec=%+v status=%+v", stored.Spec, stored.Status)
	}
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" || lease.Reference != testNamespace+"/"+testVMName || lease.PoolName != testNetwork {
		t.Fatalf("existing lease changed: %+v", lease)
	}
	pool := e.getStoredPool()
	if !reflect.DeepEqual(pool.Status.IPv4.Allocated, map[string]string{"10.0.0.1": owner}) || pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 1 {
		t.Fatalf("existing binding ledger was not repaired: %+v", pool.Status.IPv4)
	}
	if e.ipam.Used(testNetwork) != 1 || e.ipam.Available(testNetwork) != 1 {
		t.Fatal("existing binding consumed another address")
	}
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, stored); err != nil {
		t.Fatal(err)
	}
	if got := e.getStoredVMNetCfg(); !reflect.DeepEqual(got, stored) {
		t.Fatalf("idempotent replay rewrote stored config: %#v", got)
	}
	if got := e.getStoredPool(); !reflect.DeepEqual(got, pool) {
		t.Fatalf("idempotent replay rewrote repaired pool: %#v", got)
	}
	if v, ok := e.metricValue(metricVMNetCfgStatus, map[string]string{"vm": testNamespace + "/" + testVMNetCfgName, "network": testNetwork, "mac": testMAC, "ip": "10.0.0.1", "status": "OK"}); !ok || v != 1 {
		t.Errorf("existing binding status metric = %v (present %v), want 1", v, ok)
	}
}

func TestVMNetCfgRequestedIPTransition(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", "default/vm-test [02:00:00:00:00:01]"); err != nil {
		t.Fatalf("occupying old ip: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding lease: %s", err)
	}
	e.seedPool(map[string]string{"10.0.0.1": testNamespace + "/" + testVMName + " [" + testMAC + "]"})

	vmnetcfg := newVMNetCfg("10.0.0.2", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
		t.Errorf("spec ip = %q, want 10.0.0.2", got)
	}
	if got := stored.Status.NetworkConfig[0]; got.Status != "OK" || got.Message != "IP address successfully allocated" {
		t.Errorf("status = %+v, want OK", got)
	}
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP.String() != "10.0.0.2" || lease.Reference != testNamespace+"/"+testVMName {
		t.Errorf("lease = %+v, want new ip 10.0.0.2", lease)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("used = %d, want 1", used)
	}
	if avail := e.ipam.Available(testNetwork); avail != 1 {
		t.Errorf("available = %d, want 1", avail)
	}

	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated["10.0.0.2"]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
		t.Errorf("allocated[10.0.0.2] = %q, want ref", got)
	}
	if _, exists := pool.Status.IPv4.Allocated["10.0.0.1"]; exists {
		t.Error("old ip must be removed from the pool status")
	}
	if pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 1 {
		t.Errorf("used/available = %d/%d, want 1/1", pool.Status.IPv4.Used, pool.Status.IPv4.Available)
	}

}

// A new network helper must leave the old helper's binding untouched.
func TestVMNetCfgNetworkMovePreservesForeignAllocation(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	old := networkPeer(t, e, "net-old")

	// the old network: the mac's live lease still serves its address there
	if err := old.ipam.NewSubnet("default/net-old", "10.0.2.0/29", "10.0.2.1", "10.0.2.1"); err != nil {
		t.Fatalf("adding the old subnet: %s", err)
	}
	ownRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	poolOld := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ippool-old"},
		Spec: kihv1.IPPoolSpec{
			NetworkName: "default/net-old",
			IPv4Config:  kihv1.IPv4Config{Subnet: "10.0.2.0/29", ServerIP: "10.0.2.1"},
		},
		Status: kihv1.IPPoolStatus{
			IPv4: kihv1.IPv4Status{Allocated: map[string]string{"10.0.2.1": ownRef}},
		},
	}
	old.seedPoolWith(poolOld)
	if _, err := old.ipam.ReclaimIP("default/net-old", "10.0.2.1", ownRef); err != nil {
		t.Fatalf("seeding the old claim: %s", err)
	}
	if err := old.dhcp.AddLease(testMAC, "default/net-old", "10.0.2.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the old lease: %s", err)
	}

	// the new network: the spec moved the nic to it and asks for a fresh
	// allocation
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the network move must converge: %s", err)
	}

	if used := old.ipam.Used("default/net-old"); used != 1 || !old.dhcp.CheckLease(testMAC) {
		t.Errorf("old network binding changed: used=%d", used)
	}
	oldPool := e.api.ippools["ippool-old"].DeepCopy()
	if got := oldPool.Status.IPv4.Allocated["10.0.2.1"]; got != ownRef {
		t.Errorf("old pool owner = %q, want preserved", got)
	}

	// the nic serves its fresh allocation of the new network
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" || lease.PoolName != testNetwork {
		t.Errorf("lease = %+v, want a fresh 10.0.0.1 lease of %s", lease, testNetwork)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("new network used = %d, want 1", used)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
		t.Errorf("allocated[10.0.0.1] = %q, want the new owner record", got)
	}
	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0]; got.IPAddress != "10.0.0.1" || got.NetworkName != testNetwork {
		t.Errorf("spec entry = %+v, want the fresh allocation of the new network committed", got)
	}
}

func TestVMNetCfgDeletionFinalizerCleanup(t *testing.T) {
	seedCleanupState := func(e *testEnv) *kihv1.VirtualMachineNetworkConfig {
		e.addSubnet("10.0.0.1", "10.0.0.1")
		if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", "default/vm-test [02:00:00:00:00:01]"); err != nil {
			e.t.Fatalf("occupying ip: %s", err)
		}
		if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
			e.t.Fatalf("seeding lease: %s", err)
		}
		e.seedPool(map[string]string{"10.0.0.1": testNamespace + "/" + testVMName + " [" + testMAC + "]"})

		now := metav1.Now()
		vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
		vmnetcfg.ObjectMeta.DeletionTimestamp = &now
		return vmnetcfg
	}

	t.Run("removes finalizers and releases resources", func(t *testing.T) {
		e := newTestEnv(t)
		vmnetcfg := seedCleanupState(e)
		vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper"}
		e.seedVMNetCfg(vmnetcfg)
		e.metrics.UpdateVmNetCfgStatus(testNamespace+"/"+testVMNetCfgName, testNetwork, testMAC, "10.0.0.1", "OK")
		if n := e.countMetricsByLabel(metricVMNetCfgStatus, "vm", testNamespace+"/"+testVMNetCfgName); n != 1 {
			t.Fatalf("expected 1 seeded vmnetcfg status metric, got %d", n)
		}

		if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		stored := e.getStoredVMNetCfg()
		if len(stored.ObjectMeta.Finalizers) != 0 {
			t.Errorf("finalizers = %v, want empty", stored.ObjectMeta.Finalizers)
		}
		if e.dhcp.CheckLease(testMAC) {
			t.Error("lease must be deleted")
		}
		if used := e.ipam.Used(testNetwork); used != 0 {
			t.Errorf("used = %d, want 0", used)
		}
		pool := e.getStoredPool()
		if len(pool.Status.IPv4.Allocated) != 0 {
			t.Errorf("allocated = %v, want empty", pool.Status.IPv4.Allocated)
		}
		if n := e.countMetricsByLabel(metricVMNetCfgStatus, "vm", testNamespace+"/"+testVMNetCfgName); n != 0 {
			t.Errorf("vmnetcfg status metric series = %d, want 0 after deletion", n)
		}
	})

	t.Run("unchanged finalizers skip the object update", func(t *testing.T) {
		e := newTestEnv(t)
		vmnetcfg := seedCleanupState(e)
		vmnetcfg.ObjectMeta.Finalizers = []string{"external-keep"}
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		stored := e.getStoredVMNetCfg()
		if len(stored.ObjectMeta.Finalizers) != 1 || stored.ObjectMeta.Finalizers[0] != "external-keep" {
			t.Errorf("finalizers = %v, want external-keep", stored.ObjectMeta.Finalizers)
		}
		// cleanup still ran
		if e.dhcp.CheckLease(testMAC) {
			t.Error("lease must be deleted during cleanup")
		}
	})
	t.Run("a foreign allocation is left to its owner and the finalizer completes", func(t *testing.T) {
		e := newTestEnv(t)
		vmnetcfg := seedCleanupState(e)
		// the leased address is meanwhile owned by another vm: the cleanup
		// must leave that allocation untouched and still finish, so a
		// deleting vmnetcfg cannot hang in the terminating state forever
		if err := e.dhcp.DeleteLease(testMAC); err != nil {
			e.t.Fatalf("replacing the seeded lease: %s", err)
		}
		if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/other-vm"); err != nil {
			e.t.Fatalf("seeding foreign lease: %s", err)
		}
		vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup"}
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		stored := e.getStoredVMNetCfg()
		if len(stored.ObjectMeta.Finalizers) != 0 {
			t.Errorf("finalizers = %v, want empty (a foreign allocation must not strand the finalizer)", stored.ObjectMeta.Finalizers)
		}

		// the foreign allocation survives the cleanup attempt
		lease := e.dhcp.GetLease(testMAC)
		if lease.Reference != testNamespace+"/other-vm" {
			t.Errorf("lease reference = %q, want the foreign owner preserved", lease.Reference)
		}
		if got := e.ipam.Used(testNetwork); got != 1 {
			t.Errorf("ipam used = %d, want the foreign-owned address still allocated", got)
		}

		// the own ippool status entry must still be removed
		if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
			t.Errorf("allocated = %v, want the own allocation removed", pool.Status.IPv4.Allocated)
		}
	})

	t.Run("a foreign owner of the recorded ip keeps it out of the ipam release", func(t *testing.T) {
		e := newTestEnv(t)
		vmnetcfg := seedCleanupState(e)
		// the recorded ip is meanwhile leased to another vm through a
		// different mac: only the ipam release is skipped, everything else
		// of this interface is cleaned and the finalizer completes
		if err := e.dhcp.DeleteLease(testMAC); err != nil {
			e.t.Fatalf("clearing the seeded lease: %s", err)
		}
		if err := e.dhcp.AddLease("aa:bb:cc:00:00:99", testNetwork, "10.0.0.1", testNamespace+"/other-vm"); err != nil {
			e.t.Fatalf("seeding foreign ip lease: %s", err)
		}
		vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup"}
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		if got := e.ipam.Used(testNetwork); got != 1 {
			t.Errorf("ipam used = %d, want the foreign-leased address still allocated", got)
		}
		if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
			t.Errorf("allocated = %v, want the own allocation removed", pool.Status.IPv4.Allocated)
		}
		stored := e.getStoredVMNetCfg()
		if len(stored.ObjectMeta.Finalizers) != 0 {
			t.Errorf("finalizers = %v, want empty", stored.ObjectMeta.Finalizers)
		}
	})

	t.Run("a foreign status entry is left and the finalizer completes", func(t *testing.T) {
		e := newTestEnv(t)
		vmnetcfg := seedCleanupState(e)
		// the status entry was meanwhile overwritten by another writer, so
		// the deletion must not remove it and must still finish
		e.api.mu.Lock()
		e.api.ippools[testPoolName].Status.IPv4.Allocated = map[string]string{
			"10.0.0.1": testNamespace + "/other-vm [aa:bb:cc:00:00:97]",
		}
		e.api.mu.Unlock()

		vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup"}
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		// the foreign status entry survives
		pool := e.getStoredPool()
		if got := pool.Status.IPv4.Allocated["10.0.0.1"]; got != testNamespace+"/other-vm [aa:bb:cc:00:00:97]" {
			t.Errorf("allocated[10.0.0.1] = %q, want the foreign entry preserved", got)
		}
		stored := e.getStoredVMNetCfg()
		if len(stored.ObjectMeta.Finalizers) != 0 {
			t.Errorf("finalizers = %v, want empty", stored.ObjectMeta.Finalizers)
		}

		// the own lease and ipam allocation are still released, so no
		// address serves a deleted vm
		if e.dhcp.CheckLease(testMAC) {
			t.Error("the own lease must still be deleted")
		}
		if got := e.ipam.Used(testNetwork); got != 0 {
			t.Errorf("ipam used = %d, want the own allocation released", got)
		}
	})

	t.Run("a transient failure still keeps the finalizers for a retry", func(t *testing.T) {
		e := newTestEnv(t)
		vmnetcfg := seedCleanupState(e)
		vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup"}
		e.seedVMNetCfg(vmnetcfg)

		// the ippool status update fails transiently: the stale status
		// entry cannot be removed, so the cleanup must fail and retry later
		e.api.poolStatusPutCode = http.StatusInternalServerError

		err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg)
		if err == nil {
			t.Fatal("want error for a transient cleanup failure")
		}

		stored := e.getStoredVMNetCfg()
		if len(stored.ObjectMeta.Finalizers) == 0 {
			t.Error("finalizers must remain so the failed cleanup is retried")
		}
		if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 0 {
			t.Errorf("main update requests = %d, want 0 (the finalizer removal must not happen)", n)
		}

		// Ledger removal precedes local release: a failed durable cleanup
		// keeps the served binding and its spec row reachable for a retry.
		if !e.dhcp.CheckLease(testMAC) {
			t.Error("the own lease must remain until ledger cleanup succeeds")
		}
		if got := e.ipam.Used(testNetwork); got != 1 {
			t.Errorf("ipam used = %d, want the own address protected for the retry", got)
		}
		if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got == "" {
			t.Error("the own status entry must remain for the retrying cleanup")
		}

		// the retried cleanup converges: the stale status entry is removed
		// and the finalizer completes
		e.api.poolStatusPutCode = 0
		if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
			t.Fatalf("the retried cleanup must converge: %s", err)
		}
		if used := e.ipam.Used(testNetwork); used != 0 {
			t.Errorf("ipam used = %d after the retry, want 0", used)
		}
		if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
			t.Errorf("allocated = %v after the retry, want empty", pool.Status.IPv4.Allocated)
		}
		if stored := e.getStoredVMNetCfg(); len(stored.ObjectMeta.Finalizers) != 0 {
			t.Errorf("finalizers = %v after the retry, want removed", stored.ObjectMeta.Finalizers)
		}
	})
}

func TestVMNetCfgStartupTimestampGate(t *testing.T) {
	t.Run("manual creation between restart boundaries is rejected", func(t *testing.T) {
		e := newTestEnv(t)
		base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		pool := &kihv1.IPPool{
			ObjectMeta: metav1.ObjectMeta{Name: testPoolName},
			Spec: kihv1.IPPoolSpec{
				NetworkName: testNetwork,
				IPv4Config:  kihv1.IPv4Config{Subnet: testSubnet},
			},
			Status: kihv1.IPPoolStatus{
				LastUpdateBeforeStart: metav1.NewTime(base),
				LastUpdate:            metav1.NewTime(base.Add(10 * time.Minute)),
			},
		}
		e.seedPoolWith(pool)
		vmnetcfg := newVMNetCfg("", testMAC)
		vmnetcfg.ObjectMeta.CreationTimestamp = metav1.NewTime(base.Add(5 * time.Minute))
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		stored := e.getStoredVMNetCfg()
		if got := stored.Status.NetworkConfig[0]; got.Status != "ERROR" || got.Message != hijackErrorStatusMessage {
			t.Errorf("status = %+v, want startup hijack ERROR", got)
		}
		if e.dhcp.CheckLease(testMAC) {
			t.Error("no lease must be created for a hijack-guarded nic")
		}
		if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 0 {
			t.Errorf("main update requests = %d, want 0", n)
		}
		if n := e.countRequests(http.MethodGet, ippoolPath); n != 0 {
			t.Errorf("ippool get requests = %d, want 0", n)
		}
	})

	t.Run("creation before the restart window allocates normally", func(t *testing.T) {
		e := newTestEnv(t)
		e.addSubnet("10.0.0.1", "10.0.0.1")
		base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		pool := &kihv1.IPPool{
			ObjectMeta: metav1.ObjectMeta{Name: testPoolName},
			Spec: kihv1.IPPoolSpec{
				NetworkName: testNetwork,
				IPv4Config:  kihv1.IPv4Config{Subnet: testSubnet},
			},
			Status: kihv1.IPPoolStatus{
				LastUpdateBeforeStart: metav1.NewTime(base.Add(10 * time.Minute)),
				LastUpdate:            metav1.NewTime(base.Add(20 * time.Minute)),
			},
		}
		e.seedPoolWith(pool)
		vmnetcfg := newVMNetCfg("", testMAC)
		vmnetcfg.ObjectMeta.CreationTimestamp = metav1.NewTime(base.Add(5 * time.Minute))
		e.seedVMNetCfg(vmnetcfg)

		// steady state: the timestamp gate rejects only the objects
		// created inside the restart window; in the running application
		// the pending nic of a pre-restart object allocates normally
		e.appStatus.Store(APP_RUNNING)

		if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		stored := e.getStoredVMNetCfg()
		if got := stored.Status.NetworkConfig[0].Status; got != "OK" {
			t.Errorf("status = %q, want OK", got)
		}
		if !e.dhcp.CheckLease(testMAC) {
			t.Error("lease must be created")
		}
	})

	t.Run("the startup replay defers the legitimate pending nic", func(t *testing.T) {
		e := newTestEnv(t)
		e.addSubnet("10.0.0.1", "10.0.0.1")
		base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		pool := &kihv1.IPPool{
			ObjectMeta: metav1.ObjectMeta{Name: testPoolName},
			Spec: kihv1.IPPoolSpec{
				NetworkName: testNetwork,
				IPv4Config:  kihv1.IPv4Config{Subnet: testSubnet},
			},
			Status: kihv1.IPPoolStatus{
				LastUpdateBeforeStart: metav1.NewTime(base.Add(10 * time.Minute)),
				LastUpdate:            metav1.NewTime(base.Add(20 * time.Minute)),
			},
		}
		e.seedPoolWith(pool)
		vmnetcfg := newVMNetCfg("", testMAC)
		vmnetcfg.ObjectMeta.CreationTimestamp = metav1.NewTime(base.Add(5 * time.Minute))
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		// the allocation is deferred: nothing is served or recorded yet
		if e.dhcp.CheckLease(testMAC) {
			t.Error("the deferred pending nic must not hold a lease")
		}
		if used := e.ipam.Used(testNetwork); used != 0 {
			t.Errorf("ipam used = %d, want 0 until the replay finished", used)
		}
		deferred := e.controller.releaseDeferredInitAllocations()
		if len(deferred) != 1 || deferred[0] != testNamespace+"/"+testVMNetCfgName {
			t.Errorf("deferred keys = %v, want the vmnetcfg key", deferred)
		}
	})
}

func TestVMNetCfgStatusAndMetricsProjection(t *testing.T) {
	t.Run("update status", func(t *testing.T) {
		e := newTestEnv(t)
		vmnetcfg := newVMNetCfg("", testMAC)
		e.seedVMNetCfg(vmnetcfg)
		status := &kihv1.VirtualMachineNetworkConfigStatus{
			NetworkConfig: []kihv1.NetworkConfigStatus{
				{MACAddress: testMAC, NetworkName: testNetwork, Status: "ERROR", Message: "boom"},
			},
		}

		if err := e.controller.updateVirtualMachineNetworkConfigStatus(vmnetcfg, status); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		stored := e.getStoredVMNetCfg()
		if len(stored.Status.NetworkConfig) != 1 || stored.Status.NetworkConfig[0].Status != "ERROR" || stored.Status.NetworkConfig[0].Message != "boom" {
			t.Errorf("stored status = %+v, want ERROR boom", stored.Status.NetworkConfig)
		}
		if !reflect.DeepEqual(stored.Spec, vmnetcfg.Spec) || !reflect.DeepEqual(stored.Finalizers, vmnetcfg.Finalizers) {
			t.Fatal("status projection changed spec or finalizers")
		}
		if err := e.controller.updateVirtualMachineNetworkConfigStatus(stored, status); err != nil {
			t.Fatal(err)
		}
		if got := e.getStoredVMNetCfg(); !reflect.DeepEqual(got, stored) {
			t.Fatalf("unchanged status projection rewrote the object: %#v", got)
		}
	})

	t.Run("metrics projection", func(t *testing.T) {
		e := newTestEnv(t)
		vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
		vmnetcfg.Spec.NetworkConfig = append(vmnetcfg.Spec.NetworkConfig,
			kihv1.NetworkConfig{IPAddress: "10.0.0.2", MACAddress: testMAC2, NetworkName: testNetwork})
		vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
			{MACAddress: testMAC, NetworkName: testNetwork, Status: "OK", Message: "IP address successfully allocated"},
			{MACAddress: testMAC2, NetworkName: testNetwork, Status: "ERROR", Message: "something"},
			{MACAddress: "02:00:00:00:00:03", NetworkName: testNetwork, Status: "OK", Message: "no matching spec entry"},
		}
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfigMetrics(testNamespace, testVMNetCfgName); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		labelsFor := func(vm, mac, ip, status string) map[string]string {
			return map[string]string{"vm": vm, "network": testNetwork, "mac": mac, "ip": ip, "status": status}
		}
		wantLabel := labelsFor(testNamespace+"/"+testVMNetCfgName, testMAC, "10.0.0.1", "OK")
		if v, ok := e.metricValue(metricVMNetCfgStatus, wantLabel); !ok || v != 1 {
			t.Errorf("metric for %v = %v (present %v), want 1", wantLabel, v, ok)
		}
		wantLabel2 := labelsFor(testNamespace+"/"+testVMNetCfgName, testMAC2, "10.0.0.2", "ERROR")
		if v, ok := e.metricValue(metricVMNetCfgStatus, wantLabel2); !ok || v != 1 {
			t.Errorf("metric for %v = %v (present %v), want 1", wantLabel2, v, ok)
		}
		// the orphaned status entry has no spec counterpart and must not be projected
		if n := e.countMetricsByLabel(metricVMNetCfgStatus, "vm", testNamespace+"/"+testVMNetCfgName); n != 2 {
			t.Errorf("vmnetcfg status metric series = %d, want 2", n)
		}
	})

	t.Run("metrics mac labels use the canonical colon spelling", func(t *testing.T) {
		e := newTestEnv(t)
		// an uppercase hyphenated spelling from the vm spec must project
		// into the canonical colon form, identical to the ippool status
		// owner references
		const foreignSpelling = "02-BB-CC-DD-EE-FF"
		vmnetcfg := newVMNetCfg("10.0.0.9", foreignSpelling)
		vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
			{MACAddress: foreignSpelling, NetworkName: testNetwork, Status: "OK", Message: "IP address successfully allocated"},
		}
		e.seedVMNetCfg(vmnetcfg)

		if err := e.controller.updateVirtualMachineNetworkConfigMetrics(testNamespace, testVMNetCfgName); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		wantLabel := map[string]string{
			"vm":      testNamespace + "/" + testVMNetCfgName,
			"network": testNetwork,
			"mac":     "02:bb:cc:dd:ee:ff",
			"ip":      "10.0.0.9",
			"status":  "OK",
		}
		if v, ok := e.metricValue(metricVMNetCfgStatus, wantLabel); !ok || v != 1 {
			t.Errorf("metric for %v = %v (present %v), want the canonical mac label", wantLabel, v, ok)
		}
	})
}

func TestUpdateIPPoolStatusBranches(t *testing.T) {
	seedEmptyPool := func(e *testEnv) {
		e.seedPoolWith(&kihv1.IPPool{
			ObjectMeta: metav1.ObjectMeta{Name: testPoolName},
			Spec: kihv1.IPPoolSpec{
				NetworkName: testNetwork,
				IPv4Config:  kihv1.IPv4Config{Subnet: testSubnet},
			},
			Status: kihv1.IPPoolStatus{
				IPv4: kihv1.IPv4Status{Allocated: map[string]string{}},
			},
		})
	}

	t.Run("add", func(t *testing.T) {
		e := newTestEnv(t)
		e.addSubnet("10.0.0.1", "10.0.0.1")
		if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
			t.Fatalf("occupying ip: %s", err)
		}
		seedEmptyPool(e)

		if err := e.controller.updateIPPoolStatus(ADD, testNamespace, testVMName, "10.0.0.2", testNetwork, testMAC, testPoolName); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		pool := e.getStoredPool()
		if got := pool.Status.IPv4.Allocated["10.0.0.2"]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
			t.Errorf("allocated[10.0.0.2] = %q, want ref", got)
		}
		if pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 0 {
			t.Errorf("used/available = %d/%d, want 1/0", pool.Status.IPv4.Used, pool.Status.IPv4.Available)
		}
		if pool.Status.LastUpdate.Time.IsZero() {
			t.Error("lastupdate must be set")
		}
	})

	t.Run("duplicate add is rejected without a write", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedPoolWith(&kihv1.IPPool{
			ObjectMeta: metav1.ObjectMeta{Name: testPoolName},
			Spec:       kihv1.IPPoolSpec{NetworkName: testNetwork},
			Status: kihv1.IPPoolStatus{
				IPv4: kihv1.IPv4Status{Allocated: map[string]string{"10.0.0.2": "someone"}},
			},
		})

		err := e.controller.updateIPPoolStatus(ADD, testNamespace, testVMName, "10.0.0.2", testNetwork, testMAC, testPoolName)
		if err == nil {
			t.Fatal("want error for duplicate allocation")
		}
		if !strings.Contains(err.Error(), "already found in IPPool status") {
			t.Errorf("error = %q, want duplicate message", err)
		}
		if n := e.countRequests(http.MethodPut, ippoolStatusPath); n != 0 {
			t.Errorf("status update requests = %d, want 0", n)
		}
	})

	t.Run("foreign-owner delete is rejected and kept", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedPoolWith(&kihv1.IPPool{
			ObjectMeta: metav1.ObjectMeta{Name: testPoolName},
			Spec:       kihv1.IPPoolSpec{NetworkName: testNetwork},
			Status: kihv1.IPPoolStatus{
				IPv4: kihv1.IPv4Status{Allocated: map[string]string{
					"10.0.0.1": testNamespace + "/other-vm [" + testMAC + "]",
					"10.0.0.2": testNamespace + "/vm-test [" + testMAC + "]",
				}},
			},
		})

		// removing an allocation reference the caller does not own must fail
		// without a write: the address was meanwhile reassigned
		err := e.controller.updateIPPoolStatus(DELETE, testNamespace, testVMName, "10.0.0.1", testNetwork, testMAC, testPoolName)
		if err == nil {
			t.Fatal("want error for a foreign-owner delete")
		}
		if !strings.Contains(err.Error(), "belongs to") {
			t.Errorf("error = %q, want foreign-owner message", err)
		}
		if e.countRequests(http.MethodPut, ippoolStatusPath) != 0 {
			t.Error("status update requests = 0 wanted, the foreign allocation must not be overwritten")
		}

		pool := e.getStoredPool()
		if got := pool.Status.IPv4.Allocated["10.0.0.1"]; got != testNamespace+"/other-vm ["+testMAC+"]" {
			t.Errorf("allocated[10.0.0.1] = %q, want the foreign reference preserved", got)
		}
		if got := pool.Status.IPv4.Allocated["10.0.0.2"]; got != testNamespace+"/vm-test ["+testMAC+"]" {
			t.Errorf("allocated[10.0.0.2] = %q, want it untouched", got)
		}
	})

	t.Run("own allocation is removed", func(t *testing.T) {
		e := newTestEnv(t)
		e.addSubnet("10.0.0.1", "10.0.0.1")
		if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
			t.Fatalf("occupying ip: %s", err)
		}
		e.seedPoolWith(&kihv1.IPPool{
			ObjectMeta: metav1.ObjectMeta{Name: testPoolName},
			Spec:       kihv1.IPPoolSpec{NetworkName: testNetwork},
			Status: kihv1.IPPoolStatus{
				IPv4: kihv1.IPv4Status{Allocated: map[string]string{
					"10.0.0.1": testNamespace + "/vm-test [" + testMAC + "]",
					"10.0.0.2": "b [y]",
				}},
			},
		})

		if err := e.controller.updateIPPoolStatus(DELETE, testNamespace, testVMName, "10.0.0.1", testNetwork, testMAC, testPoolName); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		pool := e.getStoredPool()
		if _, exists := pool.Status.IPv4.Allocated["10.0.0.1"]; exists {
			t.Error("the owned allocation must be removed from allocated")
		}
		if got := pool.Status.IPv4.Allocated["10.0.0.2"]; got != "b [y]" {
			t.Errorf("remaining allocation = %q, want b [y]", got)
		}
		if pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 0 {
			t.Errorf("used/available = %d/%d, want 1/0 (ipam state)", pool.Status.IPv4.Used, pool.Status.IPv4.Available)
		}
	})

	t.Run("conflict then success retries once", func(t *testing.T) {
		e := newTestEnv(t)
		e.addSubnet("10.0.0.1", "10.0.0.2")
		seedEmptyPool(e)
		e.api.conflictPath = ippoolStatusPath
		e.api.conflictCount = 1

		if err := e.controller.updateIPPoolStatus(ADD, testNamespace, testVMName, "10.0.0.2", testNetwork, testMAC, testPoolName); err != nil {
			t.Fatalf("unexpected error after conflict retry: %s", err)
		}

		pool := e.getStoredPool()
		if got := pool.Status.IPv4.Allocated["10.0.0.2"]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
			t.Errorf("allocated[10.0.0.2] = %q, want ref", got)
		}
		// the retried write must be based on the re-read state: the sentinel
		// allocation of the competing writer survived the merge
		if got := pool.Status.IPv4.Allocated[competingAllocationIP]; !strings.HasPrefix(got, "other-writer") {
			t.Errorf("allocated[%s] = %q, want the competing writer sentinel preserved", competingAllocationIP, got)
		}
	})

	t.Run("non-conflict error returns immediately", func(t *testing.T) {
		e := newTestEnv(t)
		e.addSubnet("10.0.0.1", "10.0.0.2")
		seedEmptyPool(e)
		e.api.poolStatusPutCode = http.StatusInternalServerError

		err := e.controller.updateIPPoolStatus(ADD, testNamespace, testVMName, "10.0.0.2", testNetwork, testMAC, testPoolName)
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "cannot update status of IPPool") {
			t.Errorf("error = %q, want update status prefix", err)
		}
		if n := e.countRequests(http.MethodPut, ippoolStatusPath); n != 1 {
			t.Errorf("status update requests = %d, want 1 (no retry)", n)
		}
	})

	t.Run("get error", func(t *testing.T) {
		e := newTestEnv(t)
		// pool not seeded -> 404 on get
		err := e.controller.updateIPPoolStatus(ADD, testNamespace, testVMName, "10.0.0.2", testNetwork, testMAC, testPoolName)
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "cannot get IPPool") {
			t.Errorf("error = %q, want cannot get prefix", err)
		}
	})
}

func TestVMNetCfgMissingPoolReturnsError(t *testing.T) {
	e := newTestEnv(t)
	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want error for missing pool")
	}
	if !strings.Contains(err.Error(), "does not exists in cache") {
		t.Errorf("error = %q, want cache miss message", err)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must be created")
	}
}

func TestVMNetCfgIPAMErrorSetsErrorStatus(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	// 10.0.0.9 is outside the 10.0.0.0/29 cidr
	vmnetcfg := newVMNetCfg("10.0.0.9", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if len(stored.Status.NetworkConfig) != 1 {
		t.Fatalf("stored status = %+v, want one IPAM rejection", stored.Status.NetworkConfig)
	}
	if got := stored.Status.NetworkConfig[0]; got.Status != "ERROR" || !strings.Contains(got.Message, "given ip 10.0.0.9 is not cidr") {
		t.Errorf("status = %+v, want ERROR with ipam message", got)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must be created on ipam error")
	}
	if v, ok := e.metricValue(metricAppLogs, map[string]string{"loglevel": "error"}); !ok || v < 1 {
		t.Errorf("error log metric = %v (present %v), want >= 1", v, ok)
	}
}

// A failing object commit must not tear down the fresh allocation: the
// dhcp server may already have acked the address to its guest, so
// releasing it would invite a reissue (duplicate ip). the lease, claim and
// status record stay quarantined and converge through the retried sync
// which adopts them into the durable spec.
func TestVMNetCfgUpdateFailureQuarantinesServedAllocation(t *testing.T) {
	e := newTestEnv(t)
	// steady state: the failed-commit rollback of a fresh allocation
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	e.api.vmnetcfgPutCode = http.StatusInternalServerError
	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want error on failed object update")
	}
	// the failed main update must not be followed by a status update
	if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 0 {
		t.Errorf("status update requests = %d, want 0", n)
	}

	// the served allocation stays quarantined: lease, claim and record
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Fatalf("lease = %v, want the quarantined lease kept", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want the quarantined claim kept", used)
	}
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated["10.0.0.1"]; got == "" {
		t.Errorf("status record = %q, want the quarantined record kept", pool.Status.IPv4.Allocated)
	}

	// the retried sync converges: the quarantined lease of the pending nic
	// is adopted into the durable object
	e.api.vmnetcfgPutCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("retried sync: %v", err)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != "10.0.0.1" {
		t.Errorf("stored spec = %+v, want the quarantined address adopted", stored.Spec.NetworkConfig)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("lease must survive the converging sync")
	}
}

// The committed-write half of the same boundary: the apiserver can apply
// the spec update and still lose the response, so the Update error the
// controller observes does not prove the write was not committed. The
// rollback must behave identically to the uncommitted case - nothing that
// could be durable is released - so the committed address is never handed
// to a second vm while the first object's stored spec claims it, and the
// retried sync converges on the object the server already committed.
func TestVMNetCfgCommittedUpdateWithLostResponseQuarantinesTheAllocation(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	// every PUT is committed but its response is lost: the client sees an
	// EOF for a write the server applied
	e.api.vmnetcfgPutDropConn = true
	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want error on the lost response of the object update")
	}
	// the failed main update must not be followed by a status update
	if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 0 {
		t.Errorf("status update requests = %d, want 0", n)
	}

	// the server committed the write despite the lost response: the
	// durable spec already records the allocation
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != "10.0.0.1" {
		t.Fatalf("stored spec = %+v, want the committed allocation recorded", stored.Spec.NetworkConfig)
	}

	// the quarantine is identical to the uncommitted case: the lease, the
	// claim and the status record stay held under this binding's identity
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Fatalf("lease = %v, want the quarantined lease kept", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want the quarantined claim kept", used)
	}
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated["10.0.0.1"]; got == "" {
		t.Errorf("status record = %q, want the quarantined record kept", pool.Status.IPv4.Allocated)
	}
	// the one-address pool is exhausted by the quarantine: a second vm
	// cannot receive the committed-but-unconfirmed address
	if _, err := e.ipam.AllocateIP(testNetwork, "other-ns/other-vm"); err == nil {
		t.Error("a second vm must not receive the committed address")
	}

	// the retried sync converges on the committed object: the durable
	// assignment is verified, its ownership repaired and the missing
	// success status published, without any second allocation
	e.api.vmnetcfgPutDropConn = false
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("retried sync: %v", err)
	}
	stored = e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != "10.0.0.1" {
		t.Errorf("stored spec = %+v, want the committed assignment unchanged", stored.Spec.NetworkConfig)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("lease must survive the converging sync")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used after the retry = %d, want 1 (no second allocation)", used)
	}
	if len(stored.Status.NetworkConfig) != 1 {
		t.Fatalf("stored status = %+v, want one recovered binding", stored.Status.NetworkConfig)
	}
	if got := stored.Status.NetworkConfig[0]; got.Status != "OK" || got.MACAddress != testMAC {
		t.Errorf("stored status = %+v, want the synthesized OK entry of the recovered binding", got)
	}
}

// the live path must still abort when the interface looks protected: the
// delete validation under the dhcp lock reassigns the lease decision,
// which aborts the sync instead of cutting the allocation of another vm.
func TestVMNetCfgCleanupAbortsOnForeignLeaseWhileLive(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedPool(nil)
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", "other-ns/other-vm"); err != nil {
		e.t.Fatalf("seeding foreign lease: %s", err)
	}

	netCfg := kihv1.NetworkConfig{MACAddress: testMAC, NetworkName: testNetwork, IPAddress: "10.0.0.1"}
	err := e.controller.cleanupNetworkInterface(vmnetcfg, &netCfg, false)
	if err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Fatalf("cleanup = %v, want the foreign-owner abort for a live vmnetcfg", err)
	}

	// the foreign allocation must be untouched
	if lease := e.dhcp.GetLease(testMAC); lease.Reference != "other-ns/other-vm" {
		t.Errorf("lease reference = %q, want the foreign owner preserved", lease.Reference)
	}

	// the abort must not have triggered any removal, let alone a status
	// write: with a foreign lease even our own status entry is protected
	if n := e.countRequests(http.MethodPut, ippoolStatusPath); n != 0 {
		t.Errorf("ippool status updates = %d, want 0 (the abort happens before any removal)", n)
	}
	if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 0 {
		t.Errorf("vmnetcfg updates = %d, want 0", n)
	}
}

// a failing pool status update of a later nic unwinds its contested claim
// while the applied fresh allocation of an earlier nic stays quarantined:
// its lease may already have been served to the guest, so releasing it
// would reissue an address the guest still uses; the retried sync
// converges it through the regular cleanup
func TestVMNetCfgLaterNICPoolStatusFailureQuarantinesEarlierNIC(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)
	firstOwner := util.AllocationRef(testNamespace, testVMName, testMAC)
	secondOwner := util.AllocationRef(testNamespace, testVMName, testMAC2)
	foreignOwner := util.AllocationRef("another", "vm", testMAC2)
	var firstCommittedIP, secondAttemptedIP string
	e.api.poolBeforeStatusPut = func(submitted *kihv1.IPPool) {
		for ip, owner := range submitted.Status.IPv4.Allocated {
			if owner != secondOwner {
				continue
			}
			e.api.mu.Lock()
			defer e.api.mu.Unlock()
			e.api.poolBeforeStatusPut = nil
			secondAttemptedIP = ip
			stored := e.api.ippools[testPoolName]
			for committedIP, committedOwner := range stored.Status.IPv4.Allocated {
				if committedOwner == firstOwner {
					firstCommittedIP = committedIP
				}
			}
			// The second NIC has reached its actual ledger PUT. A
			// competing owner wins that address before this write commits.
			stored.Status.IPv4.Allocated[ip] = foreignOwner
			bumpResourceVersion(stored)
			return
		}
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC, NetworkName: testNetwork},
		{MACAddress: testMAC2, NetworkName: testNetwork},
	}
	e.seedVMNetCfg(vmnetcfg)
	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the second NIC's contested ledger write to fail reconciliation")
	}
	e.api.mu.Lock()
	firstIP, secondIP := firstCommittedIP, secondAttemptedIP
	e.api.mu.Unlock()
	if firstIP == "" || secondIP == "" || firstIP == secondIP {
		t.Fatalf("did not exercise first committed allocation followed by second ledger PUT: first=%q second=%q", firstIP, secondIP)
	}
	if !strings.Contains(err.Error(), secondIP) || !errors.Is(err, util.ErrForeignOwner) {
		t.Fatalf("reconciliation failed outside the injected second-NIC ownership conflict: %s", err)
	}

	// The first automatic allocation was served but never committed into
	// spec. It must stay quarantined under the actual allocated identity.
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != firstIP || lease.PoolName != testNetwork || lease.Reference != testNamespace+"/"+testVMName {
		t.Fatalf("first NIC lost its quarantined binding at %s: %+v", firstIP, lease)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Fatalf("ipam used = %d, want only the first quarantined claim", used)
	}
	if _, err := e.ipam.GetIP(testNetwork, firstIP); err == nil {
		t.Fatal("first NIC's quarantined address became reissuable")
	}
	if e.dhcp.CheckLease(testMAC2) {
		t.Fatal("the contested second NIC's lease was not unwound")
	}
	pool := e.getStoredPool()
	if !reflect.DeepEqual(pool.Status.IPv4.Allocated, map[string]string{firstIP: firstOwner, secondIP: foreignOwner}) {
		t.Fatalf("quarantined or foreign ledger ownership changed: %v", pool.Status.IPv4.Allocated)
	}
	if pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 1 {
		t.Fatalf("post-unwind accounting = %+v, want one quarantined claim", pool.Status.IPv4)
	}
	stored := e.getStoredVMNetCfg()
	if !reflect.DeepEqual(stored.Spec, vmnetcfg.Spec) || !reflect.DeepEqual(stored.Status, vmnetcfg.Status) {
		t.Fatalf("failed reconciliation committed either automatic allocation: %#v", stored)
	}
	if _, err := e.ipam.GetIP(testNetwork, secondIP); err != nil {
		t.Fatalf("the contested second NIC's claim was not released: %s", err)
	}
}

// A later owned NIC failure must not free an earlier served allocation.
func TestVMNetCfgLaterNICInvalidMACQuarantinesEarlierNIC(t *testing.T) {
	e := newTestEnv(t)
	// steady state: a running application's sync failure quarantines the
	// fresh allocations of the same sync
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC, NetworkName: testNetwork},
		{MACAddress: "not-a-mac", NetworkName: testNetwork},
	}
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the invalid MAC of the second nic to fail the sync")
	}

	// the earlier nic's served allocation stays quarantined
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("the earlier nic's lease = %v, want the quarantined lease kept", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("the earlier nic's ipam allocation = %d used, want the quarantined claim kept", used)
	}

	e.api.mu.Lock()
	poolA := e.api.ippools[testPoolName].DeepCopy()
	e.api.mu.Unlock()
	if got := poolA.Status.IPv4.Allocated["10.0.0.1"]; got == "" {
		t.Error("the quarantined status record of the earlier nic must be kept")
	}

	if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 0 {
		t.Errorf("vmnetcfg updates = %d, want 0 (the failure is pre-commit)", n)
	}
}

// during finalizer cleanup a same numeric lease in another network must
// not skip the own ipam release: the reservations are network-scoped, so
// the cleanup releases its own network's allocation and leaves the
// foreign lease untouched
func TestVMNetCfgDeletionReleasesAcrossForeignNetworkLease(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(map[string]string{"10.0.0.1": "default/vm-test [02:00:00:00:00:01]"})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", "default/vm-test [02:00:00:00:00:01]"); err != nil {
		t.Fatalf("allocating the own reservation: %s", err)
	}

	// the own dhcp lease is already gone after a first cleanup attempt,
	// while a foreign network serves the same numeric address to another
	// owner
	if err := e.dhcp.AddLease(testMAC2, "net-other", "10.0.0.1", "ns-other/vm-b"); err != nil {
		t.Fatalf("foreign network lease: %s", err)
	}

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	netCfg := kihv1.NetworkConfig{MACAddress: testMAC, NetworkName: testNetwork, IPAddress: "10.0.0.1"}

	if err := e.controller.cleanupNetworkInterface(vmnetcfg, &netCfg, true); err != nil {
		t.Fatalf("cleanup = %v, want the own release to proceed despite the foreign lease", err)
	}

	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the own reservation is released)", used)
	}
	if lease := e.dhcp.GetLease(testMAC2); lease.Reference != "ns-other/vm-b" {
		t.Errorf("foreign lease reference = %q, want the other network's owner preserved", lease.Reference)
	}

	pool := e.getStoredPool()
	if got, stillThere := pool.Status.IPv4.Allocated["10.0.0.1"]; stillThere {
		t.Errorf("own status entry = %q, want removed so the finalizer converges", got)
	}
}

// the live cleanup of an old interface address must not abort on a same
// numeric lease of another network: the lookup is network-scoped, so the
// own old lease and reservation are released within their own network
func TestVMNetCfgOldAddressCleanupIgnoresForeignNetworkLease(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(map[string]string{"10.0.0.1": "default/vm-test [02:00:00:00:00:01]"})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", "default/vm-test [02:00:00:00:00:01]"); err != nil {
		t.Fatalf("allocating the own reservation: %s", err)
	}

	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", "default/vm-test"); err != nil {
		t.Fatalf("own lease: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC2, "net-other", "10.0.0.1", "ns-other/vm-b"); err != nil {
		t.Fatalf("foreign network lease: %s", err)
	}

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	netCfg := kihv1.NetworkConfig{MACAddress: testMAC, NetworkName: testNetwork, IPAddress: "10.0.0.1"}

	if err := e.controller.cleanupNetworkInterface(vmnetcfg, &netCfg, false); err != nil {
		t.Fatalf("cleanup = %v, want the own old-address cleanup to proceed", err)
	}

	if e.dhcp.CheckLease(testMAC) {
		t.Error("expected the own old lease released")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the own reservation is released)", used)
	}
	if lease := e.dhcp.GetLease(testMAC2); lease.Reference != "ns-other/vm-b" {
		t.Errorf("foreign lease reference = %q, want the other network's owner preserved", lease.Reference)
	}
}

// a rejected, never-allocated address must not make the vmnetcfg
// undeletable: the cleanup converges on the provably missing allocation
// and removes the finalizers
func TestVMNetCfgDeletionConvergesOnNeverAllocatedAddress(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.4")
	e.seedPool(nil)

	// 10.0.0.9 is outside the 10.0.0.0/29 subnet: the allocation pass
	// registers the error status and never allocates anything
	vmnetcfg := newVMNetCfg("10.0.0.9", testMAC)
	e.seedVMNetCfg(vmnetcfg)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error on the allocation pass: %s", err)
	}

	// mark the stored object for deletion like the controller would see it
	stored := e.getStoredVMNetCfg()
	now := metav1.Now()
	stored.ObjectMeta.DeletionTimestamp = &now
	stored.ObjectMeta.Finalizers = []string{"kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup"}
	e.seedVMNetCfg(stored)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, stored); err != nil {
		t.Fatalf("deletion pass = %v, want convergence on the never-allocated address", err)
	}

	final := e.getStoredVMNetCfg()
	if len(final.ObjectMeta.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed so the object can be deleted", final.ObjectMeta.Finalizers)
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want nothing allocated", used)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must exist")
	}
}

// deleting the recorded allocation must republish the pool accounting:
// the gauges of a pool whose last allocation was cleaned stay stale
// otherwise, because status-only ippool writes do not repair them
func TestVMNetCfgDeletionRefreshesPoolMetrics(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(map[string]string{"10.0.0.1": "default/vm-test [02:00:00:00:00:01]"})
	e.api.mu.Lock()
	e.api.ippools[testPoolName].Status.IPv4.Used = 1
	e.api.ippools[testPoolName].Status.IPv4.Available = 1
	e.api.mu.Unlock()
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", "default/vm-test [02:00:00:00:00:01]"); err != nil {
		t.Fatalf("allocating the recorded address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding lease: %s", err)
	}
	e.metrics.UpdateIPPoolUsed(testPoolName, testSubnet, testNetwork, 1)
	e.metrics.UpdateIPPoolAvailable(testPoolName, testSubnet, testNetwork, 1)

	// the used gauge matches the live allocation before the cleanup
	if v, ok := e.metricValue(metricIPPoolUsed, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 1 {
		t.Fatalf("used gauge before cleanup = %v (present %v), want 1", v, ok)
	}

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	now := metav1.Now()
	vmnetcfg.ObjectMeta.DeletionTimestamp = &now
	vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup"}
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("deletion pass = %v", err)
	}

	pool := e.getStoredPool()
	if got, stillThere := pool.Status.IPv4.Allocated["10.0.0.1"]; stillThere {
		t.Errorf("released allocation = %q, want removed from the status", got)
	}
	if pool.Status.IPv4.Used != 0 || pool.Status.IPv4.Available != 2 {
		t.Errorf("durable accounting after cleanup = used %d available %d, want 0/2", pool.Status.IPv4.Used, pool.Status.IPv4.Available)
	}
	if e.ipam.Used(testNetwork) != 0 || e.dhcp.CheckLease(testMAC) {
		t.Error("cleanup acknowledged while a local binding remains")
	}
	if stored := e.getStoredVMNetCfg(); len(stored.Spec.NetworkConfig)+len(stored.Status.NetworkConfig)+len(stored.Finalizers) != 0 {
		t.Fatalf("completed cleanup was not acknowledged: %#v", stored)
	}
	if v, ok := e.metricValue(metricIPPoolUsed, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 0 {
		t.Errorf("used gauge after cleanup = %v (present %v), want 0", v, ok)
	}
	if v, ok := e.metricValue(metricIPPoolAvail, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 2 {
		t.Errorf("available gauge after cleanup = %v (present %v), want 2", v, ok)
	}
}

// the quarantined rollback publishes nothing: the accounting written while
// applying the allocation already matches the kept lease, the kept claim
// and the kept status record (no phantom release in ledger or gauges)
func TestVMNetCfgQuarantinedRollbackKeepsAccountingConsistent(t *testing.T) {
	e := newTestEnv(t)
	// steady state: the failed-commit rollback of a fresh allocation
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)
	e.api.vmnetcfgPutCode = http.StatusInternalServerError

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the failed durable update to fail the sync")
	}

	// the allocation stays applied and its persisted accounting reflects it
	if !e.dhcp.CheckLease(testMAC) {
		t.Fatal("the lease must stay quarantined after the failed update")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Fatalf("ipam used = %d, want the quarantined claim kept", used)
	}

	// the ledger must record the address the lease actually holds; the
	// allocator hands out the first free address of the pool range, whose
	// map iteration order is unspecified, so the assertion keys on the
	// lease identity instead of a fixed address
	allocatedIP := e.dhcp.GetLease(testMAC).ClientIP.String()
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated[allocatedIP]; got != util.AllocationRef(testNamespace, testVMName, testMAC) {
		t.Fatalf("allocations = %v, want the quarantined record kept under its owner", pool.Status.IPv4.Allocated)
	}
	if pool.Status.IPv4.Used != 1 {
		t.Errorf("persisted used = %d, want 1 (the ledger must match the kept claim)", pool.Status.IPv4.Used)
	}
	if pool.Status.IPv4.Available != 1 {
		t.Errorf("persisted available = %d, want 1", pool.Status.IPv4.Available)
	}

	if v, ok := e.metricValue(metricIPPoolUsed, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 1 {
		t.Errorf("used gauge after quarantine = %v (present %v), want 1", v, ok)
	}
	if v, ok := e.metricValue(metricIPPoolAvail, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 1 {
		t.Errorf("available gauge after quarantine = %v (present %v), want 1", v, ok)
	}
}

// Identical numeric addresses and MACs remain distinct across helpers.
func TestVMNetCfgSameIPNetworkMovePreservesForeignLease(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	old := networkPeer(t, e, "net-old")

	// the old network: the mac's live lease serves the very same numeric
	// address there (two isolated networks with identical subnets)
	if err := old.ipam.NewSubnet("default/net-old", "10.0.0.0/29", "10.0.0.1", "10.0.0.1"); err != nil {
		t.Fatalf("adding the old subnet: %s", err)
	}
	ownRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	poolOld := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ippool-old"},
		Spec: kihv1.IPPoolSpec{
			NetworkName: "default/net-old",
			IPv4Config:  kihv1.IPv4Config{Subnet: "10.0.0.0/29", ServerIP: "10.0.0.1"},
		},
		Status: kihv1.IPPoolStatus{
			IPv4: kihv1.IPv4Status{Allocated: map[string]string{"10.0.0.1": ownRef}},
		},
	}
	old.seedPoolWith(poolOld)
	if _, err := old.ipam.ReclaimIP("default/net-old", "10.0.0.1", ownRef); err != nil {
		t.Fatalf("seeding the old claim: %s", err)
	}
	if err := old.dhcp.AddLease(testMAC, "default/net-old", "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the old lease: %s", err)
	}

	// the new network: the spec moved the nic to it and asks for the same
	// address
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the same-ip network move must converge: %s", err)
	}

	if used := old.ipam.Used("default/net-old"); used != 1 || !old.dhcp.CheckLease(testMAC) {
		t.Errorf("old network binding changed: used=%d", used)
	}
	oldPool := e.api.ippools["ippool-old"].DeepCopy()
	if got := oldPool.Status.IPv4.Allocated["10.0.0.1"]; got != ownRef {
		t.Errorf("old pool owner = %q, want preserved", got)
	}

	// the nic serves the address of the NEW network now: the lease carries
	// the new pool, the new network's allocator holds the claim and the
	// new pool's ledger records the owner
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" || lease.PoolName != testNetwork {
		t.Errorf("lease = %+v, want a 10.0.0.1 lease of %s (the migrated pool)", lease, testNetwork)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("new network used = %d, want 1", used)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != ownRef {
		t.Errorf("allocated[10.0.0.1] = %q, want the new owner record", got)
	}
	stored := e.getStoredVMNetCfg()
	if got := stored.Spec.NetworkConfig[0]; got.IPAddress != "10.0.0.1" || got.NetworkName != testNetwork {
		t.Errorf("spec entry = %+v, want the same address committed on the new network", got)
	}
}

// Accepting an ambiguous committed write must not retain an allocation when
// the live owned row instead requests a different address.
func TestVMNetCfgChangedOwnedAddressUnwindsStaleAllocation(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)
	obj := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(obj)
	var allocatedIP, requestedIP string
	e.api.vmnetcfgPutConflict = 1
	e.api.vmnetcfgPutConflictFn = func(current *kihv1.VirtualMachineNetworkConfig) {
		lease := e.dhcp.GetLease(testMAC)
		if lease.ClientIP == nil {
			t.Error("conflicting write did not encounter an applied allocation")
			return
		}
		allocatedIP = lease.ClientIP.String()
		requestedIP = "10.0.0.1"
		if allocatedIP == requestedIP {
			requestedIP = "10.0.0.2"
		}
		current.Spec.NetworkConfig[0].IPAddress = requestedIP
	}
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err == nil {
		t.Fatal("changed owned address must force a fresh reconciliation")
	}
	if allocatedIP == "" || requestedIP == "" || allocatedIP == requestedIP {
		t.Fatal("did not exercise a conflicting owned address change")
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != requestedIP || len(stored.Status.NetworkConfig) != 0 {
		t.Fatalf("stale allocation overwrote the current intent: %+v", stored)
	}
	if e.dhcp.CheckLease(testMAC) || e.ipam.Used(testNetwork) != 0 || e.ipam.Available(testNetwork) != 2 {
		t.Fatal("unwanted stale lease or claim survived")
	}
	pool := e.getStoredPool()
	if len(pool.Status.IPv4.Allocated) != 0 || pool.Status.IPv4.Used != 0 || pool.Status.IPv4.Available != 2 {
		t.Fatalf("stale ledger or accounting survived: %+v", pool.Status.IPv4)
	}
}
