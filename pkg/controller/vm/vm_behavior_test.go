package vm

import (
	"context"
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

	log "github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	kubevirtv1 "kubevirt.io/api/core/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

const vmnetcfgFinalizer = "kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup"

// ---------------------------------------------------------------------------
// Fixtures and helpers
// ---------------------------------------------------------------------------
func vmTestScope(namespace, name string) util.NetworkScope {
	scope, err := util.NewNetworkScope(namespace, name)
	if err != nil {
		panic(err)
	}
	return scope
}

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
		ctx:          context.Background(),
		cache:        kihcache.NewCacheAllocator(),
		ipam:         ipam.NewIPAllocator(),
		dhcp:         dhcp.NewDHCPAllocator(),
		metrics:      metrics.NewMetricsAllocator(),
		kihClientset: cs,
		scope:        vmTestScope("default", "net-a"),
		reconcileMu:  &sync.Mutex{},
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
	if err := alloc.AddLease(mac, "default/net-a", ip, ref); err != nil {
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

// addSubnetWithOwnedIP registers an ipam subnet and reserves the given ip
// as a named allocation of the owner: a binding's own live allocation is
// a named reservation (the owner-validated cleanups release exactly that
// state), while an anonymous allocation models a foreign or successor
// state which no cleanup of this owner may free.
func addSubnetWithOwnedIP(t *testing.T, alloc *ipam.IPAllocator, name, ip, ownerRef string) {
	t.Helper()
	if err := alloc.NewSubnet(name, "10.0.0.0/24", "10.0.0.10", "10.0.0.12"); err != nil {
		t.Fatalf("adding subnet %s: %v", name, err)
	}
	if _, err := alloc.ReclaimIP(name, ip, ownerRef); err != nil {
		t.Fatalf("reserving ip %s in %s for %s: %v", ip, name, ownerRef, err)
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
	vmnetcfgGetUIDOverride   string // when set, GETs serve this UID instead of the stored one (a stale pre-replacement read)
	vmnetcfgCreateStatus     int
	vmnetcfgCreateErr        string
	vmnetcfgUpdateStatus     int
	vmnetcfgUpdateErr        string
	vmnetcfgDeleteStatus     int
	vmnetcfgDeleteErr        string
	ippoolGetStatus          int
	ippoolGetErr             string
	ippoolListStatus         int // when set, the cluster-scoped list fails
	ippoolListErr            string
	ippoolStatusConflicts    int // consecutive 409s before a successful status update
	ippoolStatusUpdateStatus int
	ippoolStatusUpdateErr    string
	// Hooks run outside mu, allowing a second real HTTP client to commit
	// between another controller's read and write.
	beforeVMNetCfgCreate func()
	beforeVMNetCfgUpdate func(status bool, proposed *kihv1.VirtualMachineNetworkConfig)
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
	status := len(segs) == 8 && segs[7] == "status"
	obj := &kihv1.VirtualMachineNetworkConfig{}
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		if err := json.Unmarshal(body, obj); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if r.Method == http.MethodPost {
			key = ns + "/" + obj.Name
		}
	}

	// Detach each hook before invoking it: nested real client requests must
	// proceed normally while the outer request remains suspended.
	f.mu.Lock()
	createHook, updateHook := f.beforeVMNetCfgCreate, f.beforeVMNetCfgUpdate
	if r.Method == http.MethodPost {
		f.beforeVMNetCfgCreate = nil
	}
	if r.Method == http.MethodPut {
		f.beforeVMNetCfgUpdate = nil
	}
	f.mu.Unlock()
	if r.Method == http.MethodPost && createHook != nil {
		createHook()
	}
	if r.Method == http.MethodPut && updateHook != nil {
		updateHook(status, obj.DeepCopy())
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	existing, found := f.vmnetcfgs[key]
	if found && existing.ResourceVersion == "" {
		existing.ResourceVersion = "1"
	}
	switch r.Method {
	case http.MethodGet:
		if f.vmnetcfgGetStatus != 0 {
			writeAPIError(w, f.vmnetcfgGetStatus, f.vmnetcfgGetErr)
			return
		}
		if !found {
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("virtualmachinenetworkconfigs %q not found", name))
			return
		}
		served := existing.DeepCopy()
		if f.vmnetcfgGetUIDOverride != "" {
			served.UID = types.UID(f.vmnetcfgGetUIDOverride)
		}
		vmBehaviorWriteJSON(w, http.StatusOK, served)
	case http.MethodPost:
		if f.vmnetcfgCreateStatus != 0 {
			writeAPIError(w, f.vmnetcfgCreateStatus, f.vmnetcfgCreateErr)
			return
		}
		if found {
			vmBehaviorWriteJSON(w, http.StatusConflict, metav1.Status{
				TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
				Status:   metav1.StatusFailure, Reason: metav1.StatusReasonAlreadyExists,
				Message: "virtualmachinenetworkconfigs already exists", Code: http.StatusConflict,
			})
			return
		}
		obj.ResourceVersion = "1"
		obj.UID = types.UID("cfg-" + obj.Name)
		obj.Status = kihv1.VirtualMachineNetworkConfigStatus{}
		f.vmnetcfgs[key] = obj
		vmBehaviorWriteJSON(w, http.StatusCreated, obj)
	case http.MethodPut:
		if f.vmnetcfgUpdateStatus != 0 {
			writeAPIError(w, f.vmnetcfgUpdateStatus, f.vmnetcfgUpdateErr)
			return
		}
		if !found {
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("virtualmachinenetworkconfigs %q not found", name))
			return
		}
		if obj.Name != name || obj.Namespace != ns {
			writeAPIError(w, http.StatusBadRequest, "the object identity does not match the requested object")
			return
		}
		if obj.ResourceVersion == "" || obj.ResourceVersion != existing.ResourceVersion || obj.UID != existing.UID {
			writeAPIError(w, http.StatusConflict, "the object has been modified")
			return
		}
		if status {
			next := existing.DeepCopy()
			next.Status = obj.Status
			obj = next
		} else {
			obj.Status = existing.DeepCopy().Status
		}
		vmBehaviorBumpResourceVersion(&obj.ObjectMeta)
		f.vmnetcfgs[key] = obj
		vmBehaviorWriteJSON(w, http.StatusOK, obj)
	case http.MethodDelete:
		if f.vmnetcfgDeleteStatus != 0 {
			writeAPIError(w, f.vmnetcfgDeleteStatus, f.vmnetcfgDeleteErr)
			return
		}
		opts := &metav1.DeleteOptions{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, opts); err != nil {
				writeAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if opts.Preconditions != nil && opts.Preconditions.UID != nil && found &&
			existing.UID != *opts.Preconditions.UID {
			writeAPIError(w, http.StatusConflict, "the UID in the precondition does not match the UID in record")
			return
		}
		delete(f.vmnetcfgs, key)
		writeAPISuccess(w)
	default:
		f.t.Errorf("unexpected method %s for %s", r.Method, r.URL.Path)
	}
}

func (f *fakeAPI) handlePool(w http.ResponseWriter, r *http.Request, segs []string, body []byte) {
	// a cluster-scoped list (/apis/<group>/<version>/ippools) is served
	// for the callback api-verify of the cleanup paths: it decides whether
	// a cache-missed pool is truly gone (its ledger died with it) or
	// merely missed the cache. without the route the fake panics on
	// segs[4] and the clientset retries for ~10s
	if len(segs) == 4 && r.Method == http.MethodGet {
		f.mu.Lock()
		listStatus := f.ippoolListStatus
		listErr := f.ippoolListErr
		f.mu.Unlock()
		if listStatus != 0 {
			writeAPIError(w, listStatus, listErr)
			return
		}

		f.mu.Lock()
		list := &kihv1.IPPoolList{}
		for _, pool := range f.pools {
			list.Items = append(list.Items, *pool.DeepCopy())
		}
		f.mu.Unlock()
		vmBehaviorWriteJSON(w, http.StatusOK, list)

		return
	}

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
	addSubnetWithOwnedIP(t, c.ipam, networkName, oldIP, "ns1/vm1 ["+oldMAC+"]")
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

// TestHandleVirtualMachineObjectChangeSkipsTerminatingObject pins the
// replacement race of a deleted virtual machine: the vm controller deletes
// the vmnetcfg object of a deleted vm, and a same-name replacement
// created while that deletion is still running finds the doomed object on
// its own create-or-update lookup. configuring the replacement through it
// would write the replacement's spec into the object whose finalizer
// cleanup is in flight, and that cleanup releases the leases, the ipam
// claims and the ledger records of the nics it finds in the spec before
// it deletes the object - the replacement then serves without its
// reservations until its own resync re-creates the vmnetcfg. the sync is
// deferred with a retriable error instead, and the retried sync creates
// the replacement's own object once the doomed one is gone.
func TestHandleVirtualMachineObjectChangeSkipsTerminatingObject(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	// the object of a deleted virtual machine is still terminating: its
	// finalizer cleanup has not completed yet
	now := metav1.Now()
	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "vm1",
			Namespace:         "ns1",
			DeletionTimestamp: &now,
			Finalizers:        []string{vmnetcfgFinalizer},
		},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	f.mu.Unlock()

	// the replacement advertises its own interface
	vm := multusVM("ns1", "vm1", "net1", "default/net-a", "aa:bb:cc:00:00:02")

	err := c.handleVirtualMachineObjectChange(vm)
	if err == nil {
		t.Fatal("expected the sync of a terminating vmnetcfg object to be deferred")
	}

	// the doomed object must not be configured: no spec update of it
	if n := len(f.requestsFor(http.MethodPut, "/virtualmachinenetworkconfigs/vm1")); n != 0 {
		t.Errorf("expected no update of the terminating object, got %d", n)
	}

	// the deferred sync converges once the object is gone: the retried
	// sync creates the replacement's own object
	f.mu.Lock()
	delete(f.vmnetcfgs, "ns1/vm1")
	f.mu.Unlock()

	if err := c.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("the retried sync of the gone object must create the replacement's object: %v", err)
	}

	creates := f.requestsFor(http.MethodPost, "/virtualmachinenetworkconfigs")
	if len(creates) != 1 {
		t.Fatalf("expected 1 create after the doomed object is gone, got %d", len(creates))
	}
	var created kihv1.VirtualMachineNetworkConfig
	if err := json.Unmarshal(creates[0].body, &created); err != nil {
		t.Fatalf("decoding create body: %v", err)
	}
	if len(created.Spec.NetworkConfig) != 1 || created.Spec.NetworkConfig[0] != testNetCfg("aa:bb:cc:00:00:02", "default/net-a", "") {
		t.Errorf("the replacement must get its own network config, got %+v", created.Spec.NetworkConfig)
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

	// the pool must be cached: the interface cleanup of the replaced nic
	// now fails (and aborts before the object update) on a pool cache miss,
	// so this test needs the cleanup to reach the durable update
	storePool(t, c, f, "pool-a", "default/net-a", map[string]string{
		"10.0.0.42": "ns1/vm1 [aa:bb:cc:00:00:01]",
	})
	if err := c.ipam.NewSubnet("default/net-a", "10.0.0.0/24", "10.0.0.10", "10.0.0.50"); err != nil {
		t.Fatalf("registering cleanup allocator: %v", err)
	}

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
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the injected VMNetCfg API failure after successful cleanup, got %v", err)
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
		Spec: kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1", NetworkConfig: []kihv1.NetworkConfig{
			testNetCfg("aa:bb:cc:00:00:01", "default/net-a", ""),
		}},
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
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1", NetworkConfig: []kihv1.NetworkConfig{
			testNetCfg("aa:bb:cc:00:00:01", "default/net-a", ""),
		}},
	}
	f.mu.Unlock()

	err := c.deleteVirtualMachineNetworkConfigObject("ns1", "vm1")
	if err == nil || !strings.Contains(err.Error(), "cannot delete VirtualMachineNetworkConfig object for vm") {
		t.Fatalf("expected wrapped delete error, got %v", err)
	}
}

func TestCheckVirtualMachineNetworkConfigObject(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	obj, exists, err := c.checkVirtualMachineNetworkConfigObject("ns1", "missing")
	if err != nil {
		t.Fatalf("expected no error for missing vmnetcfg, got %v", err)
	}
	if exists || obj != nil {
		t.Error("expected exists=false and nil object for missing vmnetcfg")
	}

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"}}
	f.mu.Unlock()

	obj, exists, err = c.checkVirtualMachineNetworkConfigObject("ns1", "vm1")
	if err != nil {
		t.Fatalf("expected no error for existing vmnetcfg, got %v", err)
	}
	if !exists || obj == nil {
		t.Error("expected exists=true and the observed object for existing vmnetcfg")
	}

	// a transient get failure must surface as an error, never as absence
	f.vmnetcfgGetStatus = http.StatusInternalServerError
	f.vmnetcfgGetErr = "boom"
	if _, _, err = c.checkVirtualMachineNetworkConfigObject("ns1", "vm1"); err == nil {
		t.Error("expected error for failing vmnetcfg get")
	}
}

// a transient failure of the preflight get must not be mistaken for a missing
// object: the deletion returns an error so the rate-limited retry re-runs it,
// no delete reaches the api and the binding stays retained
func TestDeleteVirtualMachineNetworkConfigObjectPropagatesGetError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	f.vmnetcfgGetStatus = http.StatusInternalServerError
	f.vmnetcfgGetErr = "boom"

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"}}
	f.mu.Unlock()

	err := c.deleteVirtualMachineNetworkConfigObject("ns1", "vm1")
	if err == nil || !strings.Contains(err.Error(), "cannot check VirtualMachineNetworkConfig object for vm") {
		t.Fatalf("expected wrapped get error, got %v", err)
	}
	if n := len(f.requestsFor(http.MethodDelete, "/virtualmachinenetworkconfigs/vm1")); n != 0 {
		t.Errorf("expected no delete after failing get, got %d", n)
	}
	if f.storedVMNetCfg("ns1/vm1") == nil {
		t.Error("expected vmnetcfg to be retained after failing get")
	}
}

// the delete must be conditioned on the uid the preflight get observed, so a
// same-name replacement landing between get and delete is rejected by the
// apiserver instead of destroyed
func TestDeleteVirtualMachineNetworkConfigObjectSendsUIDPrecondition(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1", UID: types.UID("1111-2222-3333")},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1", NetworkConfig: []kihv1.NetworkConfig{
			testNetCfg("aa:bb:cc:00:00:01", "default/net-a", ""),
		}},
	}
	f.mu.Unlock()

	if err := c.deleteVirtualMachineNetworkConfigObject("ns1", "vm1"); err != nil {
		t.Fatalf("deleteVirtualMachineNetworkConfigObject: %v", err)
	}

	deletes := f.requestsFor(http.MethodDelete, "/virtualmachinenetworkconfigs/vm1")
	if len(deletes) != 1 {
		t.Fatalf("expected 1 delete, got %d", len(deletes))
	}
	opts := &metav1.DeleteOptions{}
	if err := json.Unmarshal(deletes[0].body, opts); err != nil {
		t.Fatalf("decoding delete options from request body: %v", err)
	}
	if opts.Preconditions == nil || opts.Preconditions.UID == nil {
		t.Fatalf("expected a uid precondition on the delete, got %+v", opts.Preconditions)
	}
	if *opts.Preconditions.UID != types.UID("1111-2222-3333") {
		t.Errorf("expected precondition uid 1111-2222-3333, got %s", *opts.Preconditions.UID)
	}
	if f.storedVMNetCfg("ns1/vm1") != nil {
		t.Error("expected vmnetcfg to be gone after delete")
	}
}

// a same-name replacement created between the preflight get and the delete
// must survive: the uid precondition no longer matches the stored object, the
// apiserver rejects the delete with a conflict and the retried sync converges
// through the informer-store replacement guard instead
func TestDeleteVirtualMachineNetworkConfigObjectSparesReplacementOnUIDConflict(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	// the store already holds the replacement (uid-b) while the get serves
	// the stale pre-replacement object (uid-a)
	f.vmnetcfgGetUIDOverride = "uid-a"

	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1", UID: types.UID("uid-b")},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1", NetworkConfig: []kihv1.NetworkConfig{
			testNetCfg("aa:bb:cc:00:00:01", "default/net-a", ""),
		}},
	}
	f.mu.Unlock()

	err := c.deleteVirtualMachineNetworkConfigObject("ns1", "vm1")
	if err == nil || !strings.Contains(err.Error(), "cannot delete VirtualMachineNetworkConfig object for vm") {
		t.Fatalf("expected wrapped conflict error, got %v", err)
	}

	// the replacement must still be there, unharmed
	stored := f.storedVMNetCfg("ns1/vm1")
	if stored == nil {
		t.Fatal("expected the replacement vmnetcfg to survive the preconditioned delete")
	}
	if stored.UID != types.UID("uid-b") {
		t.Errorf("expected the replacement uid uid-b to be intact, got %s", stored.UID)
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
	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, "ns1/vm1 ["+mac+"]")
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

	// a converged cleanup writes the pool status twice: the durable
	// un-record and the post-release count republish
	statusUpdates := f.requestsFor(http.MethodPut, "/ippools/pool-a/status")
	if len(statusUpdates) != 2 {
		t.Fatalf("expected 2 pool status updates (un-record and count republish), got %d", len(statusUpdates))
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
	// the republish persists the post-release accounting: the address is
	// free again, so the stored counts must match the live allocator
	if pool.Status.IPv4.Used != 0 || pool.Status.IPv4.Available != 3 {
		t.Errorf("expected used=0 available=3 after the republish, got used=%d available=%d",
			pool.Status.IPv4.Used, pool.Status.IPv4.Available)
	}
}

func TestCleanupNetworkInterfaceFailsWhenPoolUnknown(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	networkName := "default/net-a"
	ip := "10.0.0.11"

	addSimpleLease(t, c.dhcp, mac, ip, "ns1/vm1")
	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, "ns1/vm1 ["+mac+"]")

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}

	// the pool exists in the api but missed the cache: the status entry
	// cannot be un-recorded, so the cleanup must report failure (proceeding
	// silently would orphan the record forever), not a converged success
	f.mu.Lock()
	f.pools["pool-a"] = &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", ResourceVersion: "1"},
		Spec:       kihv1.IPPoolSpec{NetworkName: networkName},
		Status: kihv1.IPPoolStatus{
			IPv4: kihv1.IPv4Status{Allocated: map[string]string{ip: "ns1/vm1 [" + mac + "]"}},
		},
	}
	f.mu.Unlock()

	err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip})
	if err == nil || !strings.Contains(err.Error(), "does not exists in cache") {
		t.Fatalf("expected pool cache miss error, got %v", err)
	}
	if n := len(f.requestsFor(http.MethodPut, "/status")); n != 0 {
		t.Errorf("expected no pool status update when the pool misses the cache, got %d", n)
	}
	// the un-record failed before any local release: the address stays
	// fully intact for the retried cleanup
	if !c.dhcp.CheckLease(mac) {
		t.Error("expected the lease still registered after the failed un-record")
	}
	if used := c.ipam.Used(networkName); used != 1 {
		t.Errorf("expected the ipam claim kept after the failed un-record, used=%d", used)
	}

	// once the pool is cached the retried cleanup converges: the releases
	// are idempotent and the record is removed
	storePool(t, c, f, "pool-a", networkName, map[string]string{
		ip: "ns1/vm1 [" + mac + "]",
	})
	if err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip}); err != nil {
		t.Fatalf("retried cleanupNetworkInterface: %v", err)
	}
	if c.dhcp.CheckLease(mac) {
		t.Error("expected dhcp lease to be deleted")
	}
	if n := len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")); n != 2 {
		t.Errorf("expected 2 pool status updates after the retry (un-record and count republish), got %d", n)
	}
	if pool := f.storedPool("pool-a"); pool != nil {
		if _, stillThere := pool.Status.IPv4.Allocated[ip]; stillThere {
			t.Errorf("expected %s removed from allocations after the retry, got %v", ip, pool.Status.IPv4.Allocated)
		}
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
	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, "ns1/vm1 ["+mac+"]")
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
}

// A04 regression: a successor lease is not proof that this nic's
// bookkeeping completed. when the own ledger entry survived (a failed or
// lost un-record of an earlier attempt, or a hand-edited record) while a
// successor already serves the address, the cleanup must still un-record
// the own orphan - otherwise it blocks the successor's own ledger write
// forever and every registration re-pins the address to the ghost owner -
// but never touch the successor's lease or reservation.
func TestCleanupNetworkInterfaceUnrecordsOwnOrphanUnderSuccessorLease(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	successorMac := "aa:bb:cc:00:00:02"
	networkName := "default/net-a"
	ip := "10.0.0.11"

	// this nic's own ledger entry survived while its local state is gone
	storePool(t, c, f, "pool-a", networkName, map[string]string{
		ip: "ns1/vm1 [" + mac + "]",
	})
	// a successor vm already serves the address live
	if err := c.dhcp.AddLease(successorMac, networkName, ip, "ns1/vm2"); err != nil {
		t.Fatalf("successor lease: %v", err)
	}
	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, "ns1/vm2 ["+successorMac+"]")

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	netCfg := testNetCfg(mac, networkName, ip)

	if err := c.cleanupNetworkInterface(vmnetcfg, &netCfg); err != nil {
		t.Fatalf("cleanup under a successor lease must converge: %v", err)
	}

	// the own orphan ledger entry is removed: the successor's pending
	// ledger write can succeed and the registration re-pins nothing
	pool := f.storedPool("pool-a")
	if _, exists := pool.Status.IPv4.Allocated[ip]; exists {
		t.Errorf("expected the own orphan entry removed, got %v", pool.Status.IPv4.Allocated)
	}

	// the successor's live state is untouched
	if !c.dhcp.CheckLease(successorMac) {
		t.Error("expected the successor's dhcp lease preserved")
	}
	if used := c.ipam.Used(networkName); used != 1 {
		t.Errorf("expected the successor's ipam allocation preserved, used=%d", used)
	}

	// the replay converges without resurrecting anything
	if err := c.cleanupNetworkInterface(vmnetcfg, &netCfg); err != nil {
		t.Fatalf("retried cleanup: %v", err)
	}
	if _, exists := f.storedPool("pool-a").Status.IPv4.Allocated[ip]; exists {
		t.Errorf("expected the replay to stay converged, got %v", f.storedPool("pool-a").Status.IPv4.Allocated)
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

// a pool status entry which does not match this nic's owner reference
// (a legacy spelling written by an older revision or a hand-edited record)
// must not pin the interface state: the foreign ledger entry stays
// untouched, but the owner-validated lease deletion and ipam release still
// run, so the cleanup converges instead of leaking the lease and the
// reservation of a removed nic
func TestCleanupNetworkInterfaceReleasesOwnStateUnderForeignLedgerEntry(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	networkName := "default/net-a"
	ip := "10.0.0.11"
	foreignRef := "ns1/vm2 [aa:bb:cc:00:00:99]"

	// the live state belongs to this vm: named reservation and lease
	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, "ns1/vm1 ["+mac+"]")
	if err := c.dhcp.AddLease(mac, networkName, ip, "ns1/vm1"); err != nil {
		t.Fatalf("own lease: %v", err)
	}
	// the ledger records the address under a foreign reference
	storePool(t, c, f, "pool-a", networkName, map[string]string{ip: foreignRef})

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}

	if err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip}); err != nil {
		t.Fatalf("cleanup under a foreign ledger entry: %v", err)
	}

	// the live state of the removed nic is released
	if c.dhcp.CheckLease(mac) {
		t.Error("the own lease must be released although the ledger entry is foreign")
	}
	if used := c.ipam.Used(networkName); used != 0 {
		t.Errorf("ipam used = %d, want 0: the own reservation must be released", used)
	}

	// The foreign ledger entry stays intact.
	pool := f.storedPool("pool-a")
	if got := pool.Status.IPv4.Allocated[ip]; got != foreignRef {
		t.Errorf("pool record = %q, want the foreign entry kept", got)
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

	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, "ns1/vm1 ["+mac+"]")
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

// A retry with an already-recorded reference and current accounting must be
// a no-op instead of failing with the duplicate-IP error.
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
	addSubnetWithIP(t, c.ipam, "net-a", "10.0.0.11")
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
	addSubnetWithIP(t, c.ipam, "net-a", "10.0.0.11")
	f.ippoolStatusUpdateStatus = http.StatusInternalServerError
	f.ippoolStatusUpdateErr = "boom"

	err := c.updateIPPoolStatus(ADD, "ns1", "vm1", "10.0.0.11", "net-a", "aa:bb:cc:00:00:01", "pool-a")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the injected pool status API failure, got %v", err)
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
	addSubnetWithOwnedIP(t, c.ipam, networkName, oldIP, "ns1/vm1 ["+oldMAC+"]")
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
	if err := c.ipam.NewSubnet("default/net-a", "10.0.0.0/24", "10.0.0.10", "10.0.0.12"); err != nil {
		t.Fatalf("registering empty subnet: %v", err)
	}

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	// The allocator is registered but this binding has no lease or claim.
	if err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: "aa:bb:cc:00:00:01", NetworkName: "default/net-a", IPAddress: "10.0.0.11"}); err != nil {
		t.Fatalf("cleanupNetworkInterface: %v", err)
	}

	if c.dhcp.CheckLease("aa:bb:cc:00:00:01") {
		t.Error("expected no lease after cleanup")
	}
	pool := f.storedPool("pool-a")
	if len(pool.Status.IPv4.Allocated) != 0 || pool.Status.IPv4.Used != 0 || pool.Status.IPv4.Available != 3 {
		t.Errorf("cleanup without local bindings must leave an empty, accurately counted pool: %+v", pool.Status.IPv4)
	}
}

// when the pool exists but its status cannot be updated the cleanup aborts
// so the sync retries: the durable un-record runs before any local release
// (mirroring the vmnetcfg live path), so this interface's address is never
// locally freed while its ownership record is still written - a retried
// cleanup converges from a fully intact state
func TestCleanupNetworkInterfacePropagatesPoolStatusError(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	networkName := "default/net-a"
	ip := "10.0.0.11"

	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, "ns1/vm1 ["+mac+"]")
	storePool(t, c, f, "pool-a", networkName, map[string]string{ip: "ns1/vm1 [" + mac + "]"})
	if err := c.dhcp.AddLease(mac, networkName, ip, "ns1/vm1"); err != nil {
		t.Fatalf("seeding lease: %v", err)
	}
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

	// the un-record failed, so the lease and the claim must be fully
	// intact: the address was never locally freed while its ownership
	// record is still written, and the retried cleanup converges from a
	// consistent state instead of leaving a ghost ledger entry behind
	if !c.dhcp.CheckLease(mac) {
		t.Error("expected the lease to stay registered after the failed un-record")
	}
	if used := c.ipam.Used(networkName); used != 1 {
		t.Errorf("expected the ipam allocation kept after the failed un-record, used=%d", used)
	}
	if n := len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")); n != 1 {
		t.Errorf("expected 1 pool status attempt, got %d", n)
	}
}

// TestCleanupNetworkInterfaceUnrecordsBeforeReleasing: on the success path
// the durable un-record happens before the local releases (mirroring the
// vmnetcfg live path), and the lease and claim are gone once the cleanup
// converged. the un-record persists the pre-release accounting, the
// post-release republish persists the counts of the live allocator.
func TestCleanupNetworkInterfaceUnrecordsBeforeReleasing(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:02"
	networkName := "default/net-a"
	ip := "10.0.0.11"

	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, "ns1/vm1 ["+mac+"]")
	storePool(t, c, f, "pool-a", networkName, map[string]string{ip: "ns1/vm1 [" + mac + "]"})
	if err := c.dhcp.AddLease(mac, networkName, ip, "ns1/vm1"); err != nil {
		t.Fatalf("seeding lease: %v", err)
	}

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	if err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip}); err != nil {
		t.Fatalf("cleanupNetworkInterface: %v", err)
	}

	if c.dhcp.CheckLease(mac) {
		t.Error("the lease must be released after a converged cleanup")
	}
	if used := c.ipam.Used(networkName); used != 0 {
		t.Errorf("ipam used = %d, want 0 after a converged cleanup", used)
	}
	puts := f.requestsFor(http.MethodPut, "/ippools/pool-a/status")
	if len(puts) != 2 {
		t.Fatalf("expected the un-record and the post-release republish, got %d puts", len(puts))
	}
	var unrecorded, republished kihv1.IPPool
	if err := json.Unmarshal(puts[0].body, &unrecorded); err != nil {
		t.Fatalf("decoding the un-record body: %v", err)
	}
	if err := json.Unmarshal(puts[1].body, &republished); err != nil {
		t.Fatalf("decoding the republish body: %v", err)
	}
	if unrecorded.Status.IPv4.Used != 1 {
		t.Errorf("the un-record must persist the pre-release accounting, got used=%d", unrecorded.Status.IPv4.Used)
	}
	if republished.Status.IPv4.Used != 0 || republished.Status.IPv4.Available != 3 {
		t.Errorf("the republish must persist the post-release accounting, got used=%d available=%d",
			republished.Status.IPv4.Used, republished.Status.IPv4.Available)
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

// vmBehaviorLogHookFunc adapts a function to the logrus hook interface.
type vmBehaviorLogHookFunc func(entry *log.Entry) error

func (f vmBehaviorLogHookFunc) Levels() []log.Level { return log.AllLevels }

func (f vmBehaviorLogHookFunc) Fire(entry *log.Entry) error { return f(entry) }

// TestCleanupNetworkInterfaceDoesNotReleaseTheSuccessorAfterDelayedResume:
// the delayed-cleanup regression. The cleanup of a removed nic passes its
// lease snapshot check and pauses right before its ipam release; inside
// that window the removed nic's own binding notices the vanished lease
// and compensates by releasing its claim, and the successor binding
// obtains the freed address as a named reservation with its lease and
// ownership record. The resumed cleanup must not free the successor's
// allocation: the release is owner-validated, so a reservation which no
// longer carries this nic's owner reference stays untouched and the
// cleanup converges on the foreign ownership record instead.
func TestCleanupNetworkInterfaceDoesNotReleaseTheSuccessorAfterDelayedResume(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	macA := "aa:bb:cc:00:00:01"
	macB := "aa:bb:cc:00:00:02"
	networkName := "default/net-a"
	ip := "10.0.0.11"
	ownerA := "ns1/vm1 [" + macA + "]"
	ownerB := "ns1/vm2 [" + macB + "]"

	// binding A is fully applied: named reservation, lease, record
	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, ownerA)
	addSimpleLease(t, c.dhcp, macA, ip, "ns1/vm1")
	storePool(t, c, f, "pool-a", networkName, map[string]string{ip: ownerA})

	vmnetcfgA := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        "vm1",
			NetworkConfig: []kihv1.NetworkConfig{testNetCfg(macA, networkName, ip)},
		},
	}

	// the interleaving runs inside the window between the cleanup's lease
	// snapshot check and its ipam release, scheduled deterministically at
	// the pre-release debug message of the real cleanup function
	oldLevel := log.GetLevel()
	oldHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	fired := false
	log.AddHook(vmBehaviorLogHookFunc(func(entry *log.Entry) error {
		if strings.Contains(entry.Message, "releasing the ipam reservation") && !fired {
			fired = true

			// the removed nic's binding notices the vanished lease and
			// compensates: the owner-validated release of its own claim
			// and the owner-checked removal of its record
			if err := c.ipam.ReleaseIPOwnedBy(networkName, ip, ownerA); err != nil {
				t.Errorf("the binding's compensating release: %v", err)
			}
			if err := c.updateIPPoolStatus(DELETE, "ns1", "vm1", ip, networkName, macA, "pool-a"); err != nil {
				t.Errorf("the binding's record removal: %v", err)
			}

			// the successor binding obtains the freed address as a named
			// reservation with its lease and its ownership record
			if _, err := c.ipam.ReclaimIP(networkName, ip, ownerB); err != nil {
				t.Errorf("the successor taking the freed address: %v", err)
			}
			if err := c.dhcp.AddLease(macB, networkName, ip, "ns1/vm2"); err != nil {
				t.Errorf("the successor lease: %v", err)
			}
			if err := c.updateIPPoolStatus(ADD, "ns1", "vm2", ip, networkName, macB, "pool-a"); err != nil {
				t.Errorf("the successor record: %v", err)
			}
		}
		return nil
	}))
	log.SetLevel(log.DebugLevel)

	t.Cleanup(func() {
		log.SetLevel(oldLevel)
		log.StandardLogger().ReplaceHooks(oldHooks)
	})

	netCfg := testNetCfg(macA, networkName, ip)
	if err := c.cleanupNetworkInterface(vmnetcfgA, &netCfg); err != nil {
		t.Fatalf("the delayed cleanup must converge: %v", err)
	}

	// the successor keeps its whole allocation
	if used := c.ipam.Used(networkName); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the successor's reservation preserved)", used)
	}
	if !c.dhcp.CheckLease(macB) {
		t.Error("the successor's dhcp lease must stay")
	}
	if got := f.storedPool("pool-a").Status.IPv4.Allocated[ip]; got != ownerB {
		t.Errorf("pool record = %q, want the successor's ownership preserved", got)
	}

	// nothing of the removed nic survives
	if c.dhcp.CheckLease(macA) {
		t.Error("the removed nic must not hold a lease")
	}
	// the successor's exact address is not handed to a third vm
	if _, err := c.ipam.GetIP(networkName, ip); err == nil {
		t.Error("the preserved reservation must not be allocatable")
	}
}

// TestCleanupNetworkInterfaceConvergesWhenPoolDeleted: the pool is gone
// from the api and missed the cache, so its status ledger died with it -
// the cleanup converges (releasing lease and claim) instead of failing
// forever over a record which can no longer exist.
func TestCleanupNetworkInterfaceConvergesWhenPoolDeleted(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:03"
	networkName := "default/net-deleted"
	c.scope = vmTestScope("default", "net-deleted")
	ip := "10.0.0.11"

	if err := c.dhcp.AddLease(mac, networkName, ip, "ns1/vm1"); err != nil {
		t.Fatalf("adding lease: %v", err)
	}
	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, "ns1/vm1 ["+mac+"]")

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}

	// neither the cache nor the api knows the pool: the api-verify (an
	// empty list) classifies it as deleted - its ledger record went with
	// it, so there is nothing left to un-record
	if err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip}); err != nil {
		t.Fatalf("cleanupNetworkInterface for a deleted pool: %v", err)
	}

	if c.dhcp.CheckLease(mac) {
		t.Error("the lease of a deleted pool must be released")
	}
	if used := c.ipam.Used(networkName); used != 0 {
		t.Errorf("ipam used = %d, want 0 after the converged cleanup", used)
	}
	if n := len(f.requestsFor(http.MethodPut, "/status")); n != 0 {
		t.Errorf("expected no pool status update for a deleted pool, got %d", n)
	}
}

// TestCleanupNetworkInterfaceFailsClosedWhenListFails: the api-verify
// itself fails, so the pool may still exist with a live ledger entry -
// the cleanup fails conservatively with the state fully intact instead of
// releasing an address whose ownership record is still written.
func TestCleanupNetworkInterfaceFailsClosedWhenListFails(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:04"
	networkName := "default/net-a"
	ip := "10.0.0.11"

	addSimpleLease(t, c.dhcp, mac, ip, "ns1/vm1")
	addSubnetWithOwnedIP(t, c.ipam, networkName, ip, "ns1/vm1 ["+mac+"]")
	f.ippoolListStatus = http.StatusInternalServerError
	f.ippoolListErr = "boom"

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: networkName, IPAddress: ip})
	if err == nil {
		t.Fatal("expected the list failure to fail the cleanup conservatively")
	}
	if !c.dhcp.CheckLease(mac) {
		t.Error("the lease must stay registered when the api-verify fails")
	}
	if used := c.ipam.Used(networkName); used != 1 {
		t.Errorf("ipam used = %d, want 1 (no release on an unverifiable pool)", used)
	}
}
