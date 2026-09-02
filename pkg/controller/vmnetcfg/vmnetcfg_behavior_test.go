package vmnetcfg

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	f.ippools[pool.Name] = pool.DeepCopy()
}

func (f *fakeAPIServer) seedVMNetCfg(obj *kihv1.VirtualMachineNetworkConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vmnetcfgs[obj.Namespace+"/"+obj.Name] = obj.DeepCopy()
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
	if n := e.countRequests(http.MethodGet, ippoolPath); n != 0 {
		t.Errorf("ippool get requests = %d, want 0", n)
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

	t.Run("delete", func(t *testing.T) {
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
					"10.0.0.1": "a [x]",
					"10.0.0.2": "b [y]",
				}},
			},
		})

		if err := e.controller.updateIPPoolStatus(DELETE, testNamespace, testVMName, "10.0.0.1", testNetwork, testMAC, testPoolName); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		pool := e.getStoredPool()
		if _, exists := pool.Status.IPv4.Allocated["10.0.0.1"]; exists {
			t.Error("deleted ip must be removed from allocated")
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

func TestVMNetCfgUpdateFailureReturnsError(t *testing.T) {
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
	// allocation side effects were already applied before the object update
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("lease must exist: allocation happens before the object update")
	}
}
