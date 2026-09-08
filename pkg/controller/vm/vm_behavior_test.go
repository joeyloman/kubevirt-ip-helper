package vm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	kubevirtv1 "kubevirt.io/api/core/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
)

const vmnetcfgFinalizer = "kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup"

// ---------------------------------------------------------------------------
// Fixtures and helpers
// ---------------------------------------------------------------------------

// vmBehaviorNewTestController builds a Controller wired to a real generated clientset that
// talks to an in-process fake API server, plus fresh in-memory allocators. Each
// test gets its own controller and server so no state leaks between tests.
func vmBehaviorNewTestController(t *testing.T) (*Controller, *fakeAPI) {
	t.Helper()

	f := &fakeAPI{
		t:         t,
		vmnetcfgs: map[string]*kihv1.VirtualMachineNetworkConfig{},
		pools:     map[string]*kihv1.IPPool{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.ServeHTTP))
	t.Cleanup(f.server.Close)

	cs, err := kihclientset.NewForConfig(&rest.Config{Host: f.server.URL})
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}

	return &Controller{
		cache:        kihcache.NewCacheAllocator(),
		ipam:         ipam.NewIPAllocator(),
		dhcp:         dhcp.NewDHCPAllocator(),
		metrics:      metrics.NewMetricsAllocator(),
		kihClientset: cs,
	}, f
}

func testVM(ns, name string) *kubevirtv1.VirtualMachine {
	return &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: kubevirtv1.VirtualMachineSpec{
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{},
		},
	}
}

// multusVM builds a VM with a single multus-backed interface matching a single
// multus network. An empty mac leaves the interface without an explicit MAC.
func multusVM(ns, name, nicName, networkName, mac string) *kubevirtv1.VirtualMachine {
	vm := testVM(ns, name)
	vm.Spec.Template.Spec.Domain.Devices.Interfaces = []kubevirtv1.Interface{{Name: nicName, MacAddress: mac}}
	vm.Spec.Template.Spec.Networks = []kubevirtv1.Network{
		{Name: nicName, NetworkSource: kubevirtv1.NetworkSource{Multus: &kubevirtv1.MultusNetwork{NetworkName: networkName}}},
	}
	return vm
}

func testNetCfg(mac, networkName, ip string) kihv1.NetworkConfig {
	return kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip}
}

// addSimpleLease registers a dhcp lease for mac belonging to ref.
func addSimpleLease(t *testing.T, alloc *dhcp.DHCPAllocator, mac, ip, ref string) {
	t.Helper()
	if err := alloc.AddLease(mac, "test-pool", ip, ref); err != nil {
		t.Fatalf("adding lease for %s: %v", mac, err)
	}
}

// addSubnetWithIP registers an ipam subnet and allocates the given ip in it.
func addSubnetWithIP(t *testing.T, alloc *ipam.IPAllocator, name, ip string) {
	t.Helper()
	if err := alloc.NewSubnet(name, "10.0.0.0/24", "10.0.0.10", "10.0.0.12"); err != nil {
		t.Fatalf("adding subnet %s: %v", name, err)
	}
	if _, err := alloc.GetIP(name, ip); err != nil {
		t.Fatalf("allocating ip %s in %s: %v", ip, name, err)
	}
}

const (
	// vmBehaviorCompetingAllocationIP is the sentinel address a conflict
	// injects into the pool status, simulating an allocation of another
	// writer whose entry must survive any subsequent retried write
	vmBehaviorCompetingAllocationIP = "10.99.0.99"
)

// vmBehaviorBumpResourceVersion mimics the apiserver increasing the
// resourceVersion whenever an object is written.
func vmBehaviorBumpResourceVersion(meta *metav1.ObjectMeta) {
	rv, _ := strconv.Atoi(meta.ResourceVersion)
	meta.ResourceVersion = strconv.Itoa(rv + 1)
}

func storePool(t *testing.T, c *Controller, f *fakeAPI, name, networkName string, allocated map[string]string) {
	t.Helper()
	pool := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"},
		Spec:       kihv1.IPPoolSpec{NetworkName: networkName},
		Status: kihv1.IPPoolStatus{
			IPv4: kihv1.IPv4Status{Allocated: allocated},
		},
	}
	if err := c.cache.Add(pool); err != nil {
		t.Fatalf("adding pool to cache: %v", err)
	}
	f.mu.Lock()
	f.pools[name] = pool
	f.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Fake API server (real generated client drives it over HTTP)
// ---------------------------------------------------------------------------

type apiRequest struct {
	method string
	path   string
	body   []byte
}

type fakeAPI struct {
	t *testing.T

	mu        sync.Mutex
	server    *httptest.Server
	vmnetcfgs map[string]*kihv1.VirtualMachineNetworkConfig // "ns/name"
	pools     map[string]*kihv1.IPPool                      // pool name
	requests  []apiRequest

	// response override knobs; 0 means default behavior
	vmnetcfgGetStatus        int
	vmnetcfgGetErr           string
	vmnetcfgCreateStatus     int
	vmnetcfgCreateErr        string
	vmnetcfgUpdateStatus     int
	vmnetcfgUpdateErr        string
	vmnetcfgDeleteStatus     int
	vmnetcfgDeleteErr        string
	ippoolGetStatus          int
	ippoolGetErr             string
	ippoolStatusConflicts    int // consecutive 409s before a successful status update
	ippoolStatusUpdateStatus int
	ippoolStatusUpdateErr    string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("reading request body: %v", err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, apiRequest{method: r.Method, path: r.URL.Path, body: body})
	f.mu.Unlock()

	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(segs) >= 6 && segs[3] == "namespaces" && segs[5] == "virtualmachinenetworkconfigs":
		f.handleVMNetCfg(w, r, segs[4], segs, body)
	case len(segs) >= 4 && segs[3] == "ippools":
		f.handlePool(w, r, segs, body)
	default:
		f.t.Errorf("unexpected request path %s", r.URL.Path)
		writeAPIError(w, http.StatusNotFound, "route not found")
	}
}

func (f *fakeAPI) handleVMNetCfg(w http.ResponseWriter, r *http.Request, ns string, segs []string, body []byte) {
	name := ""
	if len(segs) >= 7 {
		name = segs[6]
	}
	key := ns + "/" + name

	f.mu.Lock()
	existing, found := f.vmnetcfgs[key]
	if found && existing.ObjectMeta.ResourceVersion == "" {
		// direct-seeded fixtures are normalized to a first version so the
		// fake can work like a versioned apiserver
		existing.ObjectMeta.ResourceVersion = "1"
	}
	f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		if f.vmnetcfgGetStatus != 0 {
			writeAPIError(w, f.vmnetcfgGetStatus, f.vmnetcfgGetErr)
			return
		}
		if !found {
			writeAPIError(w, http.StatusNotFound,
				fmt.Sprintf("virtualmachinenetworkconfigs.kubevirtiphelper.k8s.binbash.org %q not found", name))
			return
		}
		vmBehaviorWriteJSON(w, http.StatusOK, existing)
	case http.MethodPost:
		if f.vmnetcfgCreateStatus != 0 {
			writeAPIError(w, f.vmnetcfgCreateStatus, f.vmnetcfgCreateErr)
			return
		}
		obj := &kihv1.VirtualMachineNetworkConfig{}
		if err := json.Unmarshal(body, obj); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		f.mu.Lock()
		f.vmnetcfgs[key] = obj
		f.mu.Unlock()
		vmBehaviorWriteJSON(w, http.StatusCreated, obj)
	case http.MethodPut:
		if f.vmnetcfgUpdateStatus != 0 {
			writeAPIError(w, f.vmnetcfgUpdateStatus, f.vmnetcfgUpdateErr)
			return
		}
		obj := &kihv1.VirtualMachineNetworkConfig{}
		if err := json.Unmarshal(body, obj); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		f.mu.Lock()
		stored, found := f.vmnetcfgs[key]
		f.mu.Unlock()
		if !found {
			writeAPIError(w, http.StatusNotFound,
				fmt.Sprintf("virtualmachinenetworkconfigs.kubevirtiphelper.k8s.binbash.org %q not found", name))
			return
		}

		// a write must be based on the latest stored version and match the
		// requested object identity
		if obj.ObjectMeta.Name != name || obj.ObjectMeta.Namespace != ns {
			writeAPIError(w, http.StatusBadRequest, "the object identity does not match the requested object")
			return
		}
		if submittedRV := obj.ObjectMeta.ResourceVersion; submittedRV == "" || stored.ObjectMeta.ResourceVersion != submittedRV {
			writeAPIError(w, http.StatusConflict,
				fmt.Sprintf("Operation cannot be fulfilled on virtualmachinenetworkconfigs %q: the object has been modified; please apply your changes to the latest version and try again", name))
			return
		}

		vmBehaviorBumpResourceVersion(&obj.ObjectMeta)
		f.mu.Lock()
		f.vmnetcfgs[key] = obj
		f.mu.Unlock()
		vmBehaviorWriteJSON(w, http.StatusOK, obj)
	case http.MethodDelete:
		if f.vmnetcfgDeleteStatus != 0 {
			writeAPIError(w, f.vmnetcfgDeleteStatus, f.vmnetcfgDeleteErr)
			return
		}
		f.mu.Lock()
		delete(f.vmnetcfgs, key)
		f.mu.Unlock()
		writeAPISuccess(w)
	default:
		f.t.Errorf("unexpected method %s for %s", r.Method, r.URL.Path)
	}
}

func (f *fakeAPI) handlePool(w http.ResponseWriter, r *http.Request, segs []string, body []byte) {
	name := segs[4]
	sub := ""
	if len(segs) >= 6 {
		sub = segs[5]
	}

	switch {
	case r.Method == http.MethodGet:
		if f.ippoolGetStatus != 0 {
			writeAPIError(w, f.ippoolGetStatus, f.ippoolGetErr)
			return
		}
		f.mu.Lock()
		pool, found := f.pools[name]
		f.mu.Unlock()
		if !found {
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("ippools %q not found", name))
			return
		}
		vmBehaviorWriteJSON(w, http.StatusOK, pool)
	case r.Method == http.MethodPut && sub == "status":
		if f.ippoolStatusConflicts > 0 {
			f.mu.Lock()
			f.ippoolStatusConflicts--
			// mimic a competing writer: the stored version advances and a
			// foreign allocation appears in the status, so a retried stale
			// write cannot pass the fake
			if stored, found := f.pools[name]; found {
				vmBehaviorBumpResourceVersion(&stored.ObjectMeta)
				if stored.Status.IPv4.Allocated == nil {
					stored.Status.IPv4.Allocated = map[string]string{}
				}
				stored.Status.IPv4.Allocated[vmBehaviorCompetingAllocationIP] = "other-writer [aa:11:22:33:44:55]"
			}
			f.mu.Unlock()
			writeAPIError(w, http.StatusConflict,
				fmt.Sprintf("Operation cannot be fulfilled on ippools %q: the object has been modified; please apply your changes to the latest version and try again", name))
			return
		}
		if f.ippoolStatusUpdateStatus != 0 {
			writeAPIError(w, f.ippoolStatusUpdateStatus, f.ippoolStatusUpdateErr)
			return
		}
		obj := &kihv1.IPPool{}
		if err := json.Unmarshal(body, obj); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}

		if obj.ObjectMeta.Name != name {
			writeAPIError(w, http.StatusBadRequest, "the object name does not match the requested object")
			return
		}

		f.mu.Lock()
		stored, found := f.pools[name]
		if !found {
			f.mu.Unlock()
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("ippools %q not found", name))
			return
		}
		// a write must be based on the latest stored version, like a real
		// apiserver works; missing versions are rejected as stale
		if submittedRV := obj.ObjectMeta.ResourceVersion; submittedRV == "" || stored.ObjectMeta.ResourceVersion != submittedRV {
			f.mu.Unlock()
			writeAPIError(w, http.StatusConflict,
				fmt.Sprintf("Operation cannot be fulfilled on ippools %q: the object has been modified; please apply your changes to the latest version and try again", name))
			return
		}
		vmBehaviorBumpResourceVersion(&obj.ObjectMeta)
		f.pools[name] = obj
		f.mu.Unlock()
		vmBehaviorWriteJSON(w, http.StatusOK, obj)
	default:
		f.t.Errorf("unexpected request to ippool endpoint: %s %s", r.Method, r.URL.Path)
	}
}

func vmBehaviorWriteJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPISuccess(w http.ResponseWriter) {
	vmBehaviorWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusSuccess,
	})
}

func writeAPIError(w http.ResponseWriter, code int, msg string) {
	vmBehaviorWriteJSON(w, code, metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Message:  msg,
		Code:     int32(code),
	})
}

func (f *fakeAPI) requestsFor(method, pathSuffix string) []apiRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []apiRequest
	for _, req := range f.requests {
		if req.method == method && strings.HasSuffix(req.path, pathSuffix) {
			out = append(out, req)
		}
	}
	return out
}

func (f *fakeAPI) storedPool(name string) *kihv1.IPPool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.pools[name]; ok {
		return p.DeepCopy()
	}
	return nil
}

func (f *fakeAPI) storedVMNetCfg(key string) *kihv1.VirtualMachineNetworkConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.vmnetcfgs[key]
	if !ok {
		return nil
	}
	if o.ObjectMeta.ResourceVersion == "" {
		// direct-seeded fixtures are normalized to a first version so a
		// write against this object behaves like against a versioned apiserver
		o.ObjectMeta.ResourceVersion = "1"
	}
	return o.DeepCopy()
}

// ---------------------------------------------------------------------------
// getNetworkConfigs: multus filtering, matching, MAC selection
// ---------------------------------------------------------------------------

func TestGetNetworkConfigsFiltersNonMultusAndUnmatched(t *testing.T) {
	c, _ := vmBehaviorNewTestController(t)

	vm := testVM("ns1", "vm1")
	vm.Spec.Template.Spec.Domain.Devices.Interfaces = []kubevirtv1.Interface{
		{Name: "pod", MacAddress: "aa:bb:cc:00:00:00"},    // pod net (Multus nil) -> skipped
		{Name: "net1", MacAddress: "aa:bb:cc:00:00:01"},   // multus with MAC -> kept
		{Name: "orphan", MacAddress: "aa:bb:cc:00:00:02"}, // no matching network -> skipped
		{Name: "net2", MacAddress: "aa:bb:cc:00:00:03"},   // multus without network name -> skipped
	}
	vm.Spec.Template.Spec.Networks = []kubevirtv1.Network{
		{Name: "pod", NetworkSource: kubevirtv1.NetworkSource{Pod: &kubevirtv1.PodNetwork{}}},
		{Name: "net1", NetworkSource: kubevirtv1.NetworkSource{Multus: &kubevirtv1.MultusNetwork{NetworkName: "default/net-a"}}},
		{Name: "net2", NetworkSource: kubevirtv1.NetworkSource{Multus: &kubevirtv1.MultusNetwork{}}},
		{Name: "unused", NetworkSource: kubevirtv1.NetworkSource{Multus: &kubevirtv1.MultusNetwork{NetworkName: "default/net-b"}}}, // no matching interface -> skipped
	}

	got, err := c.getNetworkConfigs(vm, nil)
	if err != nil {
		t.Fatalf("getNetworkConfigs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 network config, got %d: %+v", len(got), got)
	}
	if got[0] != testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "") {
		t.Errorf("unexpected network config: %+v", got[0])
	}
}

func TestGetNetworkConfigsUsesExplicitMacAddress(t *testing.T) {
	c, _ := vmBehaviorNewTestController(t)

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:01")

	got, err := c.getNetworkConfigs(vm, nil)
	if err != nil {
		t.Fatalf("getNetworkConfigs: %v", err)
	}
	if len(got) != 1 || got[0].MACAddress != "aa:bb:cc:00:00:01" || got[0].NetworkName != "default/net-a" {
		t.Fatalf("expected explicit MAC config, got %+v", got)
	}
}

func TestGetNetworkConfigsUsesHarvesterMacAddress(t *testing.T) {
	c, _ := vmBehaviorNewTestController(t)

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "")
	vm.ObjectMeta.Annotations = map[string]string{
		"harvesterhci.io/mac-address": `{"net1":"aa:bb:cc:11:22:33"}`,
	}

	got, err := c.getNetworkConfigs(vm, nil)
	if err != nil {
		t.Fatalf("getNetworkConfigs: %v", err)
	}
	if len(got) != 1 || got[0].MACAddress != "aa:bb:cc:11:22:33" {
		t.Fatalf("expected MAC from harvester annotation, got %+v", got)
	}
}

func TestGetNetworkConfigsSkipsWhenNoMacAvailable(t *testing.T) {
	c, _ := vmBehaviorNewTestController(t)

	cases := map[string]*kubevirtv1.VirtualMachine{
		"no mac and no annotation": multusVM("ns1", "vm1", "net1", "default/net-a", ""),
		"unrelated annotation": func() *kubevirtv1.VirtualMachine {
			vm := multusVM("ns1", "vm1", "net1", "default/net-a", "")
			vm.ObjectMeta.Annotations = map[string]string{
				"harvesterhci.io/mac-address": `{"othernet":"aa:bb:cc:11:22:99"}`,
			}
			return vm
		}(),
		"malformed annotation": func() *kubevirtv1.VirtualMachine {
			vm := multusVM("ns1", "vm1", "net1", "default/net-a", "")
			vm.ObjectMeta.Annotations = map[string]string{
				"harvesterhci.io/mac-address": `not json`,
			}
			return vm
		}(),
	}

	for name, vm := range cases {
		got, err := c.getNetworkConfigs(vm, nil)
		if err != nil {
			t.Fatalf("%s: getNetworkConfigs: %v", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: expected no network configs, got %+v", name, got)
		}
	}
}

func TestGetNetworkConfigsPreservesIPFromExistingConfig(t *testing.T) {
	c, _ := vmBehaviorNewTestController(t)

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:01")
	cur := []kihv1.NetworkConfig{testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "10.0.0.42")}

	got, err := c.getNetworkConfigs(vm, cur)
	if err != nil {
		t.Fatalf("getNetworkConfigs: %v", err)
	}
	if len(got) != 1 || got[0].IPAddress != "10.0.0.42" {
		t.Fatalf("expected existing IP preserved, got %+v", got)
	}
}

func TestGetNetworkConfigsRejectsForeignDHCPLease(t *testing.T) {
	c, _ := vmBehaviorNewTestController(t)
	addSimpleLease(t, c.dhcp, "aa:bb:cc:00:00:01", "10.0.0.42", "otherns/othervm")

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:01")

	_, err := c.getNetworkConfigs(vm, nil)
	if err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Fatalf("expected lease ownership error, got %v", err)
	}
}

func TestGetNetworkConfigsAcceptsOwnDHCPLease(t *testing.T) {
	c, _ := vmBehaviorNewTestController(t)
	addSimpleLease(t, c.dhcp, "aa:bb:cc:00:00:01", "10.0.0.42", "ns1/vm1")

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:01")

	got, err := c.getNetworkConfigs(vm, nil)
	if err != nil {
		t.Fatalf("getNetworkConfigs: %v", err)
	}
	if len(got) != 1 || got[0].MACAddress != "aa:bb:cc:00:00:01" {
		t.Fatalf("expected config for own lease, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// handleVirtualMachineObjectChange: dispatch between create and update
// ---------------------------------------------------------------------------

func TestHandleVirtualMachineObjectChangeCreatesWhenMissing(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:01")

	if err := c.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("handleVirtualMachineObjectChange: %v", err)
	}

	creates := f.requestsFor(http.MethodPost, "/virtualmachinenetworkconfigs")
	if len(creates) != 1 {
		t.Fatalf("expected 1 create, got %d", len(creates))
	}
	var created kihv1.VirtualMachineNetworkConfig
	if err := json.Unmarshal(creates[0].body, &created); err != nil {
		t.Fatalf("decoding create body: %v", err)
	}
	if created.Name != "vm1" || created.Namespace != "ns1" {
		t.Errorf("unexpected object identity: %s/%s", created.Namespace, created.Name)
	}
	if created.Spec.VMName != "vm1" {
		t.Errorf("expected spec.vmname vm1, got %q", created.Spec.VMName)
	}
	if len(created.Finalizers) != 1 || created.Finalizers[0] != vmnetcfgFinalizer {
		t.Errorf("expected finalizer %q, got %v", vmnetcfgFinalizer, created.Finalizers)
	}
	if len(created.Spec.NetworkConfig) != 1 || created.Spec.NetworkConfig[0] != testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "") {
		t.Errorf("unexpected network config: %+v", created.Spec.NetworkConfig)
	}
}

func TestHandleVirtualMachineObjectChangePropagatesGetError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	f.vmnetcfgGetStatus = http.StatusInternalServerError
	f.vmnetcfgGetErr = "boom"

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:01")

	err := c.handleVirtualMachineObjectChange(vm)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected get error to propagate, got %v", err)
	}
	if n := len(f.requestsFor(http.MethodPost, "/virtualmachinenetworkconfigs")); n != 0 {
		t.Errorf("expected no create after get error, got %d", n)
	}
}

func TestHandleVirtualMachineObjectChangeUpdatesExisting(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	oldMAC := "aa:bb:cc:00:00:01"
	newMAC := "aa:bb:cc:00:00:02"
	networkName := "default/net-a"
	oldIP := "10.0.0.11"

	// Existing vmnetcfg with the old interface; the VM now advertises a new MAC.
	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        "vm1",
			NetworkConfig: []kihv1.NetworkConfig{testNetCfg(oldMAC, networkName, oldIP)},
		},
	}
	f.mu.Unlock()

	// Lease, ip allocation and pool backing the old interface so cleanup can complete.
	addSimpleLease(t, c.dhcp, oldMAC, oldIP, "ns1/vm1")
	addSubnetWithIP(t, c.ipam, networkName, oldIP)
	storePool(t, c, f, "pool-a", networkName, map[string]string{
		oldIP:       "ns1/vm1 [" + oldMAC + "]",
		"10.0.0.12": "other",
	})

	vm := multusVM("ns1", "vm1", "net1", networkName, newMAC)

	if err := c.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("handleVirtualMachineObjectChange: %v", err)
	}

	updates := f.requestsFor(http.MethodPut, "/virtualmachinenetworkconfigs/vm1")
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	var updated kihv1.VirtualMachineNetworkConfig
	if err := json.Unmarshal(updates[0].body, &updated); err != nil {
		t.Fatalf("decoding update body: %v", err)
	}
	if len(updated.Spec.NetworkConfig) != 1 || updated.Spec.NetworkConfig[0] != testNetCfg(newMAC, networkName, "") {
		t.Errorf("expected updated network config, got %+v", updated.Spec.NetworkConfig)
	}

	// The mismatched old interface must have been cleaned up everywhere.
	if c.dhcp.CheckLease(oldMAC) {
		t.Error("expected old dhcp lease to be deleted")
	}
	if used := c.ipam.Used(networkName); used != 0 {
		t.Errorf("expected ip released, used=%d", used)
	}
	pool := f.storedPool("pool-a")
	if pool == nil {
		t.Fatal("expected pool to remain stored")
	}
	if _, stillThere := pool.Status.IPv4.Allocated[oldIP]; stillThere {
		t.Errorf("expected %s removed from pool allocations, got %v", oldIP, pool.Status.IPv4.Allocated)
	}
}

// ---------------------------------------------------------------------------
// createVirtualMachineNetworkConfigObject
// ---------------------------------------------------------------------------

func TestCreateVirtualMachineNetworkConfigObjectSkipsWithoutNetworks(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	vm := testVM("ns1", "vm1") // no interfaces at all

	if err := c.createVirtualMachineNetworkConfigObject(vm); err != nil {
		t.Fatalf("createVirtualMachineNetworkConfigObject: %v", err)
	}
	if n := len(f.requestsFor(http.MethodPost, "/virtualmachinenetworkconfigs")); n != 0 {
		t.Errorf("expected no create without networks, got %d", n)
	}
}

func TestCreateVirtualMachineNetworkConfigObjectPropagatesConfigError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	addSimpleLease(t, c.dhcp, "aa:bb:cc:00:00:01", "10.0.0.42", "otherns/othervm")

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:01")

	err := c.createVirtualMachineNetworkConfigObject(vm)
	if err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Fatalf("expected lease ownership error, got %v", err)
	}
	if n := len(f.requestsFor(http.MethodPost, "/virtualmachinenetworkconfigs")); n != 0 {
		t.Errorf("expected no create after config error, got %d", n)
	}
}

func TestCreateVirtualMachineNetworkConfigObjectPropagatesCreateError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	f.vmnetcfgCreateStatus = http.StatusInternalServerError
	f.vmnetcfgCreateErr = "boom"

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:01")

	err := c.createVirtualMachineNetworkConfigObject(vm)
	if err == nil || !strings.Contains(err.Error(), "cannot create VirtualMachineNetworkConfig object for vm") {
		t.Fatalf("expected wrapped create error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// updateVirtualMachineNetworkConfigObject
// ---------------------------------------------------------------------------

func TestUpdateVirtualMachineNetworkConfigObjectIdempotent(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        "vm1",
			NetworkConfig: []kihv1.NetworkConfig{testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "10.0.0.42")},
		},
	}
	f.mu.Unlock()

	// VM matches the stored config exactly: MAC, network and IP all preserved.
	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:01")

	existing := f.storedVMNetCfg("ns1/vm1")
	if err := c.updateVirtualMachineNetworkConfigObject(vm, existing); err != nil {
		t.Fatalf("updateVirtualMachineNetworkConfigObject: %v", err)
	}
	if n := len(f.requestsFor(http.MethodPut, "/virtualmachinenetworkconfigs/vm1")); n != 0 {
		t.Errorf("expected no update when nothing changed, got %d", n)
	}
	if n := len(f.requestsFor(http.MethodPut, "/status")); n != 0 {
		t.Errorf("expected no pool status update when nothing changed, got %d", n)
	}
}

func TestUpdateVirtualMachineNetworkConfigObjectPropagatesConfigError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	f.mu.Unlock()
	addSimpleLease(t, c.dhcp, "aa:bb:cc:00:00:01", "10.0.0.42", "otherns/othervm")

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:01")

	existing := f.storedVMNetCfg("ns1/vm1")
	err := c.updateVirtualMachineNetworkConfigObject(vm, existing)
	if err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Fatalf("expected lease ownership error, got %v", err)
	}
	if n := len(f.requestsFor(http.MethodPut, "/virtualmachinenetworkconfigs/vm1")); n != 0 {
		t.Errorf("expected no update after config error, got %d", n)
	}
}

func TestUpdateVirtualMachineNetworkConfigObjectPropagatesUpdateError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	f.vmnetcfgUpdateStatus = http.StatusInternalServerError
	f.vmnetcfgUpdateErr = "boom"

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        "vm1",
			NetworkConfig: []kihv1.NetworkConfig{testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "10.0.0.42")},
		},
	}
	f.mu.Unlock()

	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:02")

	existing := f.storedVMNetCfg("ns1/vm1")
	err := c.updateVirtualMachineNetworkConfigObject(vm, existing)
	if err == nil || !strings.Contains(err.Error(), "cannot update VirtualMachineNetworkConfig object for vm") {
		t.Fatalf("expected wrapped update error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// deleteVirtualMachineNetworkConfigObject / checkVirtualMachineNetworkConfigObject
// ---------------------------------------------------------------------------

func TestDeleteVirtualMachineNetworkConfigObject(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	f.mu.Unlock()

	if err := c.deleteVirtualMachineNetworkConfigObject("ns1", "vm1"); err != nil {
		t.Fatalf("deleteVirtualMachineNetworkConfigObject: %v", err)
	}
	if n := len(f.requestsFor(http.MethodDelete, "/virtualmachinenetworkconfigs/vm1")); n != 1 {
		t.Errorf("expected 1 delete, got %d", n)
	}
	if f.storedVMNetCfg("ns1/vm1") != nil {
		t.Error("expected vmnetcfg to be gone after delete")
	}
}

func TestDeleteVirtualMachineNetworkConfigObjectSkipsWhenMissing(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	if err := c.deleteVirtualMachineNetworkConfigObject("ns1", "vm1"); err != nil {
		t.Fatalf("deleteVirtualMachineNetworkConfigObject: %v", err)
	}
	if n := len(f.requestsFor(http.MethodDelete, "/virtualmachinenetworkconfigs/vm1")); n != 0 {
		t.Errorf("expected no delete for missing object, got %d", n)
	}
}

func TestDeleteVirtualMachineNetworkConfigObjectPropagatesDeleteError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	f.vmnetcfgDeleteStatus = http.StatusInternalServerError
	f.vmnetcfgDeleteErr = "boom"

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"}}
	f.mu.Unlock()

	err := c.deleteVirtualMachineNetworkConfigObject("ns1", "vm1")
	if err == nil || !strings.Contains(err.Error(), "cannot delete VirtualMachineNetworkConfig object for vm") {
		t.Fatalf("expected wrapped delete error, got %v", err)
	}
}

func TestCheckVirtualMachineNetworkConfigObject(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	if c.checkVirtualMachineNetworkConfigObject("ns1", "missing") {
		t.Error("expected false for missing vmnetcfg")
	}

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"}}
	f.mu.Unlock()

	if !c.checkVirtualMachineNetworkConfigObject("ns1", "vm1") {
		t.Error("expected true for existing vmnetcfg")
	}
}

// ---------------------------------------------------------------------------
// cleanupNetworkInterface
// ---------------------------------------------------------------------------

func TestCleanupNetworkInterfaceReleasesAllState(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	networkName := "default/net-a"
	ip := "10.0.0.11"

	addSimpleLease(t, c.dhcp, mac, ip, "ns1/vm1")
	addSubnetWithIP(t, c.ipam, networkName, ip)
	storePool(t, c, f, "pool-a", networkName, map[string]string{
		ip:          "ns1/vm1 [" + mac + "]",
		"10.0.0.12": "other",
	})

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	if err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip}); err != nil {
		t.Fatalf("cleanupNetworkInterface: %v", err)
	}

	if c.dhcp.CheckLease(mac) {
		t.Error("expected dhcp lease to be deleted")
	}
	if used := c.ipam.Used(networkName); used != 0 {
		t.Errorf("expected ip released, used=%d", used)
	}

	statusUpdates := f.requestsFor(http.MethodPut, "/ippools/pool-a/status")
	if len(statusUpdates) != 1 {
		t.Fatalf("expected 1 pool status update, got %d", len(statusUpdates))
	}
	pool := f.storedPool("pool-a")
	if pool == nil {
		t.Fatal("expected pool to remain stored")
	}
	if _, stillThere := pool.Status.IPv4.Allocated[ip]; stillThere {
		t.Errorf("expected %s removed from allocations, got %v", ip, pool.Status.IPv4.Allocated)
	}
	if _, kept := pool.Status.IPv4.Allocated["10.0.0.12"]; !kept {
		t.Errorf("expected unrelated allocation kept, got %v", pool.Status.IPv4.Allocated)
	}
	if pool.Status.LastUpdate.IsZero() {
		t.Error("expected LastUpdate to be set")
	}
}

func TestCleanupNetworkInterfaceSkipsPoolStatusWhenPoolUnknown(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	networkName := "default/net-a"
	ip := "10.0.0.11"

	addSimpleLease(t, c.dhcp, mac, ip, "ns1/vm1")
	addSubnetWithIP(t, c.ipam, networkName, ip)

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	if err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip}); err != nil {
		t.Fatalf("cleanupNetworkInterface: %v", err)
	}

	if c.dhcp.CheckLease(mac) {
		t.Error("expected dhcp lease to be deleted")
	}
	if n := len(f.requestsFor(http.MethodPut, "/status")); n != 0 {
		t.Errorf("expected no pool status update when pool is unknown, got %d", n)
	}
}

// a retried cleanup must not free an ip a successor vm owns: the first
// attempt releases the interface state, the durable update fails, and by
// the time the cleanup replays the address was acquired by another vm
func TestCleanupNetworkInterfaceSkipsSuccessorAllocationOnRetry(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	successorMac := "aa:bb:cc:00:00:02"
	networkName := "default/net-a"
	ip := "10.0.0.11"

	if err := c.dhcp.AddLease(mac, networkName, ip, "ns1/vm1"); err != nil {
		t.Fatalf("own lease: %v", err)
	}
	addSubnetWithIP(t, c.ipam, networkName, ip)
	storePool(t, c, f, "pool-a", networkName, map[string]string{
		ip: "ns1/vm1 [" + mac + "]",
	})

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	netCfg := testNetCfg(mac, networkName, ip)

	if err := c.cleanupNetworkInterface(vmnetcfg, &netCfg); err != nil {
		t.Fatalf("first cleanup: %v", err)
	}

	// the durable update of the vmnetcfg object failed, so in the meantime
	// a successor vm acquired the freed address
	if _, err := c.ipam.GetIP(networkName, ip); err != nil {
		t.Fatalf("successor allocating %s: %v", ip, err)
	}
	if err := c.dhcp.AddLease(successorMac, networkName, ip, "ns1/vm2"); err != nil {
		t.Fatalf("successor lease: %v", err)
	}

	// the successor's vmnetcfg controller recorded its allocation in the
	// pool status while our stale entry was already removed
	f.mu.Lock()
	f.pools["pool-a"].Status.IPv4.Allocated = map[string]string{
		ip: "ns1/vm2 [" + successorMac + "]",
	}
	f.mu.Unlock()

	// the replay must converge instead of freeing the successor's claim
	if err := c.cleanupNetworkInterface(vmnetcfg, &netCfg); err != nil {
		t.Fatalf("retried cleanup: %v", err)
	}

	if used := c.ipam.Used(networkName); used != 1 {
		t.Errorf("expected the successor's ipam allocation preserved, used=%d", used)
	}
	if !c.dhcp.CheckLease(successorMac) {
		t.Error("expected the successor's dhcp lease preserved")
	}

	pool := f.storedPool("pool-a")
	if got := pool.Status.IPv4.Allocated[ip]; got != "ns1/vm2 ["+successorMac+"]" {
		t.Errorf("expected the successor's status entry preserved, got %q", got)
	}
	if n := len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")); n != 1 {
		t.Errorf("expected the replay to write no pool status, got %d puts", n)
	}
}

// a mac reassigned to another vm must not be released by a cleanup: the
// whole interface state belongs to the successor by then and the own
// reservation is gone
func TestCleanupNetworkInterfaceLeavesReassignedMac(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	networkName := "default/net-a"
	ip := "10.0.0.11"

	// the interface state belongs to a successor vm which took over the mac
	if err := c.dhcp.AddLease(mac, networkName, ip, "ns1/vm2"); err != nil {
		t.Fatalf("successor lease: %v", err)
	}
	addSubnetWithIP(t, c.ipam, networkName, ip)
	storePool(t, c, f, "pool-a", networkName, map[string]string{
		ip: "ns1/vm2 [" + mac + "]",
	})

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}

	err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip})
	if err != nil {
		t.Fatalf("cleanup of a reassigned mac must converge: %v", err)
	}

	if !c.dhcp.CheckLease(mac) {
		t.Error("expected the successor's dhcp lease preserved")
	}
	if used := c.ipam.Used(networkName); used != 1 {
		t.Errorf("expected the successor's ipam allocation preserved, used=%d", used)
	}

	pool := f.storedPool("pool-a")
	if got := pool.Status.IPv4.Allocated[ip]; got != "ns1/vm2 ["+mac+"]" {
		t.Errorf("expected the successor's status entry preserved, got %q", got)
	}
}

// a lease under another networkname holding the same numeric ip holds no
// claim on this network's allocation: the own release must proceed while
// the foreign lease stays untouched
func TestCleanupNetworkInterfaceReleasesAcrossForeignNetworkLease(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	networkName := "default/net-a"
	otherNetworkName := "default/net-b"
	ip := "10.0.0.11"

	addSubnetWithIP(t, c.ipam, networkName, ip)
	storePool(t, c, f, "pool-a", networkName, map[string]string{
		ip: "ns1/vm1 [" + mac + "]",
	})

	// a second network serves the same numeric address space and its lease
	// for that address exists while this network's reservation is still live
	if err := c.dhcp.AddLease("aa:bb:cc:00:00:02", otherNetworkName, ip, "ns1/vm2"); err != nil {
		t.Fatalf("foreign network lease: %v", err)
	}

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}

	err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip})
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if used := c.ipam.Used(networkName); used != 0 {
		t.Errorf("expected the own ipam allocation released, used=%d", used)
	}
	if !c.dhcp.CheckLease("aa:bb:cc:00:00:02") {
		t.Error("expected the other network's dhcp lease preserved")
	}

	pool := f.storedPool("pool-a")
	if _, stillThere := pool.Status.IPv4.Allocated[ip]; stillThere {
		t.Errorf("expected own status entry removed, got %v", pool.Status.IPv4.Allocated)
	}
}

// ---------------------------------------------------------------------------
// updateIPPoolStatus
// ---------------------------------------------------------------------------

func TestUpdateIPPoolStatusAdd(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	storePool(t, c, f, "pool-a", "net-a", map[string]string{"10.0.0.12": "other"})
	addSubnetWithIP(t, c.ipam, "net-a", "10.0.0.11")

	if err := c.updateIPPoolStatus(ADD, "ns1", "vm1", "10.0.0.11", "net-a", "aa:bb:cc:00:00:01", "pool-a"); err != nil {
		t.Fatalf("updateIPPoolStatus: %v", err)
	}

	pool := f.storedPool("pool-a")
	if pool == nil {
		t.Fatal("expected pool to remain stored")
	}
	want := map[string]string{
		"10.0.0.12": "other",
		"10.0.0.11": "ns1/vm1 [aa:bb:cc:00:00:01]",
	}
	if !reflect.DeepEqual(pool.Status.IPv4.Allocated, want) {
		t.Errorf("allocated mismatch:\n got %v\nwant %v", pool.Status.IPv4.Allocated, want)
	}
	if pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 2 {
		t.Errorf("expected used=1 available=2, got used=%d available=%d", pool.Status.IPv4.Used, pool.Status.IPv4.Available)
	}
	if pool.Status.LastUpdate.IsZero() {
		t.Error("expected LastUpdate to be set")
	}
}

func TestUpdateIPPoolStatusAddRejectsDuplicateIP(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	storePool(t, c, f, "pool-a", "net-a", map[string]string{"10.0.0.11": "someone else"})
	addSubnetWithIP(t, c.ipam, "net-a", "10.0.0.12")

	err := c.updateIPPoolStatus(ADD, "ns1", "vm1", "10.0.0.11", "net-a", "aa:bb:cc:00:00:01", "pool-a")
	if err == nil || !strings.Contains(err.Error(), "already found in IPPool status") {
		t.Fatalf("expected duplicate ip error, got %v", err)
	}
	if n := len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")); n != 0 {
		t.Errorf("expected no status update for duplicate ip, got %d", n)
	}
}

// a re-try of an allocation whose reference is already recorded must be a
// no-op instead of failing with the duplicate-ip error
func TestUpdateIPPoolStatusAddIsIdempotentForSameOwner(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	storePool(t, c, f, "pool-a", "net-a", map[string]string{
		"10.0.0.11": "ns1/vm1 [aa:bb:cc:00:00:01]",
	})
	addSubnetWithIP(t, c.ipam, "net-a", "10.0.0.11")

	if err := c.updateIPPoolStatus(ADD, "ns1", "vm1", "10.0.0.11", "net-a", "AA-BB-CC-00-00-01", "pool-a"); err != nil {
		t.Fatalf("re-adding the same allocation must succeed: %v", err)
	}
	if n := len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")); n != 0 {
		t.Errorf("expected no status update for an already recorded identical owner, got %d", n)
	}
}

func TestUpdateIPPoolStatusDelete(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	storePool(t, c, f, "pool-a", "net-a", map[string]string{
		"10.0.0.11": "ns1/vm1 [aa:bb:cc:00:00:01]",
		"10.0.0.12": "other",
	})
	addSubnetWithIP(t, c.ipam, "net-a", "10.0.0.11")

	if err := c.updateIPPoolStatus(DELETE, "ns1", "vm1", "10.0.0.11", "net-a", "aa:bb:cc:00:00:01", "pool-a"); err != nil {
		t.Fatalf("updateIPPoolStatus: %v", err)
	}

	pool := f.storedPool("pool-a")
	if pool == nil {
		t.Fatal("expected pool to remain stored")
	}
	want := map[string]string{"10.0.0.12": "other"}
	if !reflect.DeepEqual(pool.Status.IPv4.Allocated, want) {
		t.Errorf("allocated mismatch:\n got %v\nwant %v", pool.Status.IPv4.Allocated, want)
	}
}

func TestUpdateIPPoolStatusGetError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	f.ippoolGetStatus = http.StatusInternalServerError
	f.ippoolGetErr = "boom"

	err := c.updateIPPoolStatus(ADD, "ns1", "vm1", "10.0.0.11", "net-a", "aa:bb:cc:00:00:01", "pool-a")
	if err == nil || !strings.Contains(err.Error(), "cannot get IPPool pool-a") {
		t.Fatalf("expected get error, got %v", err)
	}
	if n := len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")); n != 0 {
		t.Errorf("expected no status update after get error, got %d", n)
	}
}

func TestUpdateIPPoolStatusRetriesOnConflict(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	storePool(t, c, f, "pool-a", "net-a", map[string]string{})
	f.ippoolStatusConflicts = 1

	if err := c.updateIPPoolStatus(ADD, "ns1", "vm1", "10.0.0.11", "net-a", "aa:bb:cc:00:00:01", "pool-a"); err != nil {
		t.Fatalf("updateIPPoolStatus: %v", err)
	}
	if n := len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")); n != 2 {
		t.Errorf("expected 2 status attempts after one conflict, got %d", n)
	}

	// the retry re-reads the pool: the sentinel allocation of the competing
	// writer must have survived the merge and the successful put replayed
	// with the advanced resourceVersion
	pool := f.storedPool("pool-a")
	if pool == nil {
		t.Fatal("expected pool to remain stored")
	}
	if got := pool.Status.IPv4.Allocated["10.0.0.11"]; got != "ns1/vm1 [aa:bb:cc:00:00:01]" {
		t.Errorf("allocated[10.0.0.11] = %q, want the retried allocation", got)
	}
	if got := pool.Status.IPv4.Allocated[vmBehaviorCompetingAllocationIP]; !strings.HasPrefix(got, "other-writer") {
		t.Errorf("allocated[%s] = %q, want the competing writer sentinel preserved", vmBehaviorCompetingAllocationIP, got)
	}
	if pool.ObjectMeta.ResourceVersion == "1" {
		t.Errorf("resourceVersion = %q, want a version advanced for the successful write", pool.ObjectMeta.ResourceVersion)
	}
}

func TestUpdateIPPoolStatusPropagatesUpdateError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	storePool(t, c, f, "pool-a", "net-a", map[string]string{})
	f.ippoolStatusUpdateStatus = http.StatusInternalServerError
	f.ippoolStatusUpdateErr = "boom"

	err := c.updateIPPoolStatus(ADD, "ns1", "vm1", "10.0.0.11", "net-a", "aa:bb:cc:00:00:01", "pool-a")
	if err == nil || !strings.Contains(err.Error(), "cannot update status of IPPool pool-a") {
		t.Fatalf("expected wrapped update error, got %v", err)
	}
}

func TestUpdateVirtualMachineNetworkConfigObjectKeepsMatchingInterface(t *testing.T) {
	// The VM gains a second NIC while the first one is unchanged. The
	// matching interface must NOT be cleaned up; only the vmnetcfg object
	// is updated with the new interface list.
	c, f := vmBehaviorNewTestController(t)

	oldMAC := "aa:bb:cc:00:00:01"
	networkName := "default/net-a"
	oldIP := "10.0.0.11"

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        "vm1",
			NetworkConfig: []kihv1.NetworkConfig{testNetCfg(oldMAC, networkName, oldIP)},
		},
	}
	f.mu.Unlock()

	// the old interface has a lease owned by this vm so getNetworkConfigs
	// accepts it; no pool or ipam state so cleanup must not even trigger
	addSimpleLease(t, c.dhcp, oldMAC, oldIP, "ns1/vm1")

	vm := multusVM("ns1", "vm1", "net1", networkName, oldMAC)
	vm.Spec.Template.Spec.Domain.Devices.Interfaces = append(
		vm.Spec.Template.Spec.Domain.Devices.Interfaces,
		kubevirtv1.Interface{Name: "net2", MacAddress: "aa:bb:cc:00:00:02"},
	)
	vm.Spec.Template.Spec.Networks = append(
		vm.Spec.Template.Spec.Networks,
		kubevirtv1.Network{Name: "net2", NetworkSource: kubevirtv1.NetworkSource{Multus: &kubevirtv1.MultusNetwork{NetworkName: networkName}}},
	)

	existing := f.storedVMNetCfg("ns1/vm1")
	if err := c.updateVirtualMachineNetworkConfigObject(vm, existing); err != nil {
		t.Fatalf("updateVirtualMachineNetworkConfigObject: %v", err)
	}

	updates := f.requestsFor(http.MethodPut, "/virtualmachinenetworkconfigs/vm1")
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	var updated kihv1.VirtualMachineNetworkConfig
	if err := json.Unmarshal(updates[0].body, &updated); err != nil {
		t.Fatalf("decoding update body: %v", err)
	}
	want := []kihv1.NetworkConfig{
		testNetCfg(oldMAC, networkName, oldIP),
		testNetCfg("aa:bb:cc:00:00:02", networkName, ""),
	}
	if !reflect.DeepEqual(updated.Spec.NetworkConfig, want) {
		t.Errorf("network configs mismatch:\n got %v\nwant %v", updated.Spec.NetworkConfig, want)
	}

	// the matching interface must not be cleaned up: its lease survives
	if !c.dhcp.CheckLease(oldMAC) {
		t.Error("expected the lease of the matching interface to be kept")
	}
	if n := len(f.requestsFor(http.MethodPut, "/status")); n != 0 {
		t.Errorf("expected no pool status updates when nothing is removed, got %d", n)
	}
}

func TestUpdateVirtualMachineNetworkConfigObjectRemovesAllInterfaces(t *testing.T) {
	// The VM loses every interface: the controller must clean up all
	// previously tracked interfaces and persist an empty network config.
	c, f := vmBehaviorNewTestController(t)

	oldMAC := "aa:bb:cc:00:00:01"
	networkName := "default/net-a"
	oldIP := "10.0.0.11"

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        "vm1",
			NetworkConfig: []kihv1.NetworkConfig{testNetCfg(oldMAC, networkName, oldIP)},
		},
	}
	f.mu.Unlock()

	addSimpleLease(t, c.dhcp, oldMAC, oldIP, "ns1/vm1")
	addSubnetWithIP(t, c.ipam, networkName, oldIP)
	storePool(t, c, f, "pool-a", networkName, map[string]string{oldIP: "ns1/vm1 [" + oldMAC + "]"})

	existing := f.storedVMNetCfg("ns1/vm1")
	if err := c.updateVirtualMachineNetworkConfigObject(testVM("ns1", "vm1"), existing); err != nil {
		t.Fatalf("updateVirtualMachineNetworkConfigObject: %v", err)
	}

	updates := f.requestsFor(http.MethodPut, "/virtualmachinenetworkconfigs/vm1")
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	var updated kihv1.VirtualMachineNetworkConfig
	if err := json.Unmarshal(updates[0].body, &updated); err != nil {
		t.Fatalf("decoding update body: %v", err)
	}
	if len(updated.Spec.NetworkConfig) != 0 {
		t.Errorf("expected empty network config, got %+v", updated.Spec.NetworkConfig)
	}

	// the only interface was torn down everywhere
	if c.dhcp.CheckLease(oldMAC) {
		t.Error("expected dhcp lease to be deleted")
	}
	if used := c.ipam.Used(networkName); used != 0 {
		t.Errorf("expected ip released, used=%d", used)
	}
	if pool := f.storedPool("pool-a"); pool != nil {
		if _, stillThere := pool.Status.IPv4.Allocated[oldIP]; stillThere {
			t.Errorf("expected %s removed from pool allocations, got %v", oldIP, pool.Status.IPv4.Allocated)
		}
	}
}

// cleaning an interface that has neither a dhcp lease nor an ipam
// allocation must converge and still finish the pool bookkeeping
func TestCleanupNetworkInterfaceConvergesWithoutLeaseAndIP(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	storePool(t, c, f, "pool-a", "default/net-a", map[string]string{})

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	// no lease registered for the mac, no subnet registered for the network
	if err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: "aa:bb:cc:00:00:01", NetworkName: "default/net-a", IPAddress: "10.0.0.11"}); err != nil {
		t.Fatalf("cleanupNetworkInterface: %v", err)
	}

	if c.dhcp.CheckLease("aa:bb:cc:00:00:01") {
		t.Error("expected no lease after cleanup")
	}
	if n := len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")); n != 1 {
		t.Errorf("expected 1 pool status update, got %d", n)
	}
}

// when the pool exists but its status cannot be updated the cleanup aborts
// so the sync retries: the release always happens first, so the retry
// converges instead of repeating a destructive step
func TestCleanupNetworkInterfacePropagatesPoolStatusError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	networkName := "default/net-a"
	ip := "10.0.0.11"

	addSubnetWithIP(t, c.ipam, networkName, ip)
	storePool(t, c, f, "pool-a", networkName, map[string]string{ip: "ns1/vm1 [" + mac + "]"})
	f.ippoolStatusUpdateStatus = http.StatusInternalServerError
	f.ippoolStatusUpdateErr = "boom"

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip})
	if err == nil {
		t.Fatal("expected the pool status error to propagate")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to carry the underlying failure", err)
	}

	// the release finished before the abort: a retry converges instead of
	// repeating a destructive step
	if c.dhcp.CheckLease(mac) {
		t.Error("expected the lease to be released before the abort")
	}
	if used := c.ipam.Used(networkName); used != 0 {
		t.Errorf("expected the ipam allocation released before the abort, used=%d", used)
	}
	if n := len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")); n != 1 {
		t.Errorf("expected 1 pool status attempt, got %d", n)
	}
}

func TestUpdateIPPoolStatusUnknownEventRejected(t *testing.T) {
	// an event other than add/delete is a programmer error: it must be
	// rejected instead of rebuilding the allocation map from scratch,
	// which would erase every live allocation entry
	c, f := vmBehaviorNewTestController(t)

	storePool(t, c, f, "pool-a", "net-a", map[string]string{"10.0.0.11": "ns1/vm1 [aa:bb:cc:00:00:01]"})
	seeded := f.storedPool("pool-a")

	err := c.updateIPPoolStatus("bogus", "ns1", "vm1", "10.0.0.11", "net-a", "aa:bb:cc:00:00:01", "pool-a")
	if err == nil {
		t.Fatal("expected an unknown ippool status event to be rejected")
	} else if !strings.Contains(err.Error(), "unsupported ippool status event") {
		t.Errorf("error = %v, want an unsupported-event rejection", err)
	}

	pool := f.storedPool("pool-a")
	if pool == nil {
		t.Fatal("expected pool to remain stored")
	}
	if !reflect.DeepEqual(pool.Status.IPv4.Allocated, seeded.Status.IPv4.Allocated) {
		t.Errorf("allocations changed to %v, want unchanged %v", pool.Status.IPv4.Allocated, seeded.Status.IPv4.Allocated)
	}
	if !pool.Status.LastUpdate.Time.Equal(seeded.Status.LastUpdate.Time) {
		t.Error("LastUpdate changed for a rejected event, status must stay untouched")
	}
	if n := len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")); n != 0 {
		t.Errorf("expected no pool status attempts for an unknown event, got %d", n)
	}
}
