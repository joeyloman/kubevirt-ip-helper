package vmnetcfg

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	prom "github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
)

const (
	testVMNetCfgName = "vm-test"
	testNamespace    = "default"
	testVMName       = "vm-test"
	testPoolName     = "ippool-test"
	testNetwork      = "net-test"
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
	t          *testing.T
	srv        *httptest.Server
	api        *fakeAPIServer
	client     *kihclientset.Clientset
	cache      *kihcache.CacheAllocator
	ipam       *ipam.IPAllocator
	dhcp       *dhcp.DHCPAllocator
	metrics    *metrics.MetricsAllocator
	controller *Controller
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

	appStatus := APP_INIT
	count := 0
	e := &testEnv{
		t:       t,
		srv:     srv,
		api:     api,
		client:  client,
		cache:   kihcache.NewCacheAllocator(),
		ipam:    ipam.NewIPAllocator(),
		dhcp:    dhcp.NewDHCPAllocator(),
		metrics: metrics.NewMetricsAllocator(),
	}
	e.controller = NewController(nil, nil, nil, e.cache, e.ipam, e.dhcp, e.metrics, e.client, &appStatus, &count)

	return e
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
	if err := e.cache.Add(pool); err != nil {
		e.t.Fatalf("seeding pool cache: %s", err)
	}
	e.api.seedPool(pool)
}

// seedPool registers a default pool with the given status allocation map.
func (e *testEnv) seedPool(allocated map[string]string) *kihv1.IPPool {
	e.t.Helper()
	pool := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: testPoolName},
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
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
			return
		}
		name := p[1]
		sub := ""
		if len(p) > 2 {
			sub = p[2]
		}
		f.handleIPPool(w, r, name, sub)
	case "namespaces":
		if len(p) < 4 || p[2] != "virtualmachinenetworkconfigs" {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
			return
		}
		ns := p[1]
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
	switch {
	case r.Method == http.MethodGet && sub == "":
		f.mu.Lock()
		obj, ok := f.vmnetcfgs[key]
		f.mu.Unlock()
		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource")
			return
		}
		f.writeVMNetCfg(w, obj)
	case r.Method == http.MethodPut && sub == "":
		f.mu.Lock()
		failCode := f.vmnetcfgPutCode
		f.mu.Unlock()
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
		f.vmnetcfgs[key] = obj.DeepCopy()
		f.mu.Unlock()
		f.writeVMNetCfg(w, &obj)
	case r.Method == http.MethodPut && sub == "status":
		var obj kihv1.VirtualMachineNetworkConfig
		if err := decodeBody(r, &obj); err != nil {
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
			return
		}
		f.mu.Lock()
		f.vmnetcfgs[key] = obj.DeepCopy()
		f.mu.Unlock()
		f.writeVMNetCfg(w, &obj)
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

func TestVMNetCfgStickyError(t *testing.T) {
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
	if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 1 {
		t.Errorf("status update requests = %d, want 1", n)
	}
}

func TestVMNetCfgExistingLeaseIdempotency(t *testing.T) {
	e := newTestEnv(t)
	// deliberately no ipam subnet: if the controller tried to allocate it would error
	e.seedPool(nil)
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
	if got := stored.Status.NetworkConfig[0]; got.Status != "OK" || got.Message != "IP address successfully allocated" {
		t.Errorf("status = %+v, want preserved OK", got)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("existing lease must be preserved")
	}
	if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 0 {
		t.Errorf("main update requests = %d, want 0", n)
	}
	if n := e.countRequests(http.MethodGet, ippoolPath); n != 1 {
		t.Errorf("ippool get requests = %d, want 1 (the idempotent path verifies the ownership record)", n)
	}
	if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 1 {
		t.Errorf("status update requests = %d, want 1", n)
	}
}

func TestVMNetCfgRequestedIPTransition(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
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

	if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 1 {
		t.Errorf("main update requests = %d, want 1", n)
	}
	if n := e.countRequests(http.MethodPut, ippoolStatusPath); n != 2 {
		t.Errorf("ippool status update requests = %d, want 2 (delete + add)", n)
	}
}

func TestVMNetCfgDeletionFinalizerCleanup(t *testing.T) {
	seedCleanupState := func(e *testEnv) *kihv1.VirtualMachineNetworkConfig {
		e.addSubnet("10.0.0.1", "10.0.0.1")
		if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
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
		if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 1 {
			t.Errorf("main update requests = %d, want 1", n)
		}
		if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 0 {
			t.Errorf("status update requests = %d, want 0", n)
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
		if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 0 {
			t.Errorf("main update requests = %d, want 0", n)
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
		if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 1 {
			t.Errorf("main update requests = %d, want 1", n)
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

		// the lease was released, but the failed status delete re-marks the
		// reservation (fail closed): the address stays non-reissuable while
		// the deletion is half-done and the vm's pod may still be terminating
		if e.dhcp.CheckLease(testMAC) {
			t.Error("the own lease was released before the status update failed")
		}
		if got := e.ipam.Used(testNetwork); got != 1 {
			t.Errorf("ipam used = %d, want 1: the released address must be re-marked until the retry", got)
		}
		if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got == "" {
			t.Error("the own status entry must remain for the retrying cleanup")
		}

		// the retried cleanup converges: the re-marked reservation is
		// released, the status entry removed and the finalizer gone
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
		if got := stored.Status.NetworkConfig[0]; got.Status != "ERROR" ||
			got.Message != "vmnetcfg was manually created after this program was (re)started, preventing possible ip hijack" {
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
		// the passed object is mutated in place
		if got := vmnetcfg.Status.NetworkConfig[0].Status; got != "ERROR" {
			t.Errorf("mutated status = %q, want ERROR", got)
		}
		if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 1 {
			t.Errorf("status update requests = %d, want 1", n)
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
		if n := e.countRequests(http.MethodPut, ippoolStatusPath); n != 1 {
			t.Errorf("status update requests = %d, want 1", n)
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
		if n := e.countRequests(http.MethodGet, ippoolPath); n != 1 {
			t.Errorf("get requests = %d, want 1", n)
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
		seedEmptyPool(e)
		e.api.conflictPath = ippoolStatusPath
		e.api.conflictCount = 1

		if err := e.controller.updateIPPoolStatus(ADD, testNamespace, testVMName, "10.0.0.2", testNetwork, testMAC, testPoolName); err != nil {
			t.Fatalf("unexpected error after conflict retry: %s", err)
		}

		if n := e.countRequests(http.MethodPut, ippoolStatusPath); n != 2 {
			t.Errorf("status update requests = %d, want 2 (one conflict, one success)", n)
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
		if pool.ObjectMeta.ResourceVersion == "1" {
			t.Errorf("resourceVersion = %q, want a version bumped by the conflict and the write", pool.ObjectMeta.ResourceVersion)
		}
	})

	t.Run("non-conflict error returns immediately", func(t *testing.T) {
		e := newTestEnv(t)
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
	if n := e.totalRequests(); n != 0 {
		t.Errorf("requests = %d, want 0 (early return before client call)", n)
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

// A failing object update must roll back the allocation side effects: DHCP
// may not keep serving an address the durable vmnetcfg object never recorded.
func TestVMNetCfgUpdateFailureRollsBackAllocations(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	e.api.vmnetcfgPutCode = http.StatusInternalServerError
	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want error on failed object update")
	}
	if !strings.Contains(err.Error(), "cannot update VirtualMachineNetworkConfig object") {
		t.Errorf("error = %q, want update prefix", err)
	}
	// the failed main update must not be followed by a status update
	if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 0 {
		t.Errorf("status update requests = %d, want 0", n)
	}
	// the allocation side effects are reverted with the durable update gone
	if e.dhcp.CheckLease(testMAC) {
		t.Error("lease must be rolled back when the object update fails")
	}
	if got := e.ipam.Used(testNetwork); got != 0 {
		t.Errorf("ipam used = %d, want 0 after the rollback", got)
	}
	pool := e.getStoredPool()
	if len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("ippool status allocations = %v, want empty after the rollback", pool.Status.IPv4.Allocated)
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

// a failing pool status update of a later nic must unwind the applied
// allocations of the earlier nics too: leaving them registered while the
// durable object never received the addresses leaks the ipam claims when
// the object is deleted without spec ips to clean up from
func TestVMNetCfgLaterNICPoolStatusFailureUnwindsEarlierNICs(t *testing.T) {
	e := newTestEnv(t)

	secondNetwork := "net-b"
	const secondPoolName = "ippool-b"

	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	poolB := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: secondPoolName},
		Spec: kihv1.IPPoolSpec{
			NetworkName: secondNetwork,
			IPv4Config:  kihv1.IPv4Config{Subnet: testSubnet, ServerIP: "10.0.0.1"},
		},
		Status: kihv1.IPPoolStatus{
			IPv4: kihv1.IPv4Status{Allocated: map[string]string{
				"10.0.0.2": "another/vm [02:00:00:00:00:02]",
			}},
		},
	}
	e.seedPoolWith(poolB)
	if err := e.ipam.NewSubnet(secondNetwork, testSubnet, "10.0.0.1", "10.0.0.2"); err != nil {
		t.Fatalf("adding second subnet: %s", err)
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC, NetworkName: testNetwork},
		{MACAddress: testMAC2, NetworkName: secondNetwork, IPAddress: "10.0.0.2"},
	}
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the pool status failure of the second nic to fail the sync")
	}
	if !strings.Contains(err.Error(), "cannot update the IPPool") {
		t.Errorf("error = %q, want the pool status rejection", err)
	}

	// nothing of the sync stayed applied when the second nic failed: the
	// first nic was already registered when the failure hit
	if e.dhcp.CheckLease(testMAC) || e.dhcp.CheckLease(testMAC2) {
		t.Error("leases of earlier and failing interfaces must be released")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("first nic's ipam allocation = %d used, want 0 after the unwind", used)
	}
	if used := e.ipam.Used(secondNetwork); used != 0 {
		t.Errorf("failing nic's ipam allocation = %d used, want 0 after the unwind", used)
	}

	e.api.mu.Lock()
	poolA := e.api.ippools[testPoolName].DeepCopy()
	poolBStored := e.api.ippools[secondPoolName].DeepCopy()
	e.api.mu.Unlock()

	if got := poolA.Status.IPv4.Allocated["10.0.0.1"]; got != "" {
		t.Errorf("first nic's status entry = %q, want removed by the unwind", got)
	}
	if got := poolBStored.Status.IPv4.Allocated["10.0.0.2"]; got != "another/vm [02:00:00:00:00:02]" {
		t.Errorf("foreign entry of the second pool = %q, want preserved", got)
	}

	// the unwound sync must not have touched the durable object
	if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 0 {
		t.Errorf("vmnetcfg updates = %d, want 0 (the failure is pre-commit)", n)
	}
	if n := e.countRequests(http.MethodPut, vmnetcfgStatusPath); n != 0 {
		t.Errorf("vmnetcfg status updates = %d, want 0 (the failure is pre-commit)", n)
	}
}

// a pool lookup failing after an earlier nic was applied must unwind the
// earlier allocation as well
func TestVMNetCfgLaterNICPoolLookupFailureUnwindsEarlierNICs(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC, NetworkName: testNetwork},
		{MACAddress: testMAC2, NetworkName: "net-missing"},
	}
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the pool lookup failure of the second nic to fail the sync")
	}
	if !strings.Contains(err.Error(), "does not exists in cache") {
		t.Errorf("error = %q, want the cache miss message", err)
	}

	if e.dhcp.CheckLease(testMAC) {
		t.Error("the earlier nic's lease must be released by the unwind")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("the earlier nic's ipam allocation = %d used, want 0 after the unwind", used)
	}

	e.api.mu.Lock()
	poolA := e.api.ippools[testPoolName].DeepCopy()
	e.api.mu.Unlock()
	if got := poolA.Status.IPv4.Allocated["10.0.0.1"]; got != "" {
		t.Errorf("the earlier nic's status entry = %q, want removed by the unwind", got)
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
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
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
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
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
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
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
	if v, ok := e.metricValue(metricIPPoolUsed, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 0 {
		t.Errorf("used gauge after cleanup = %v (present %v), want 0", v, ok)
	}
	if v, ok := e.metricValue(metricIPPoolAvail, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 2 {
		t.Errorf("available gauge after cleanup = %v (present %v), want 2", v, ok)
	}
}

// the rollback of a failed durable update must publish accounting which
// reflects ipam after the releases: a status write that runs before the
// releases persisted used/available of the still-held address
func TestVMNetCfgRollbackPublishesSettledAccounting(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)
	e.api.vmnetcfgPutCode = http.StatusInternalServerError

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the failed durable update to fail the sync")
	}
	if !strings.Contains(err.Error(), "cannot update VirtualMachineNetworkConfig object") {
		t.Errorf("error = %q, want the update prefix", err)
	}

	// the unwind released the allocation side effects...
	if e.dhcp.CheckLease(testMAC) {
		t.Error("lease must be rolled back when the object update fails")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 after the rollback", used)
	}

	// ...and the persisted accounting must reflect the settled state
	pool := e.getStoredPool()
	if len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("allocations = %v, want empty after the rollback", pool.Status.IPv4.Allocated)
	}
	if pool.Status.IPv4.Used != 0 {
		t.Errorf("persisted used = %d, want 0 (counters must be computed after the releases)", pool.Status.IPv4.Used)
	}
	if pool.Status.IPv4.Available != 2 {
		t.Errorf("persisted available = %d, want 2", pool.Status.IPv4.Available)
	}

	if v, ok := e.metricValue(metricIPPoolUsed, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 0 {
		t.Errorf("used gauge after rollback = %v (present %v), want 0", v, ok)
	}
	if v, ok := e.metricValue(metricIPPoolAvail, map[string]string{"ippool": testPoolName, "subnet": testSubnet, "network": testNetwork}); !ok || v != 2 {
		t.Errorf("available gauge after rollback = %v (present %v), want 2", v, ok)
	}
}
