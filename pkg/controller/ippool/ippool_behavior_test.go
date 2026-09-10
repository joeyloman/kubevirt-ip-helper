package ippool

import (
	"context"
	"encoding/json"
	"errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	kihdhcp "github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	kihipam "github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"

	prom "github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// ippoolBehaviorNewTestPool builds an IPPool whose IPv4 options are all literal IPv4 addresses,
// so no external DNS resolution is needed anywhere in the DHCP projection.
func ippoolBehaviorNewTestPool(name, network string) *kihv1.IPPool {
	return &kihv1.IPPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kubevirtiphelper.k8s.binbash.org/v1",
			Kind:       "IPPool",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: kihv1.IPPoolSpec{
			NetworkName:   network,
			BindInterface: "eth-test",
			IPv4Config: kihv1.IPv4Config{
				ServerIP: "10.10.10.1",
				Subnet:   "10.10.10.0/24",
				Pool: kihv1.Pool{
					Start:   "10.10.10.10",
					End:     "10.10.10.50",
					Exclude: []string{"10.10.10.20"},
				},
				Router:       "10.10.10.254",
				DNS:          []string{"10.10.10.2", "10.10.10.3"},
				DomainName:   "example.local",
				DomainSearch: []string{"example.local"},
				NTP:          []string{"10.10.10.4"},
				LeaseTime:    3600,
			},
		},
	}
}

// ippoolBehaviorNewTestController wires a Controller with real in-memory ipam, dhcp, cache and
// metrics allocators, plus (when srv is given) a real typed clientset pointed at
// the httptest server. informer/indexer/queue are unused by the functions under
// test and stay nil. The appStatus starts at APP_RUNNING.
func ippoolBehaviorNewTestController(t *testing.T, srv *httptest.Server) (*Controller, *kihipam.IPAllocator, *kihdhcp.DHCPAllocator, *kihcache.CacheAllocator, *metrics.MetricsAllocator) {
	t.Helper()

	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)
	var ippoolCountCurrent atomic.Int32

	var cs *kihclientset.Clientset
	if srv != nil {
		var err error
		cs, err = kihclientset.NewForConfig(&rest.Config{Host: srv.URL})
		if err != nil {
			t.Fatalf("failed to create clientset for test server: %s", err.Error())
		}
	}

	c := &Controller{
		ctx:                context.Background(),
		cache:              kihcache.New(),
		ipam:               kihipam.New(),
		dhcp:               kihdhcp.New(),
		metrics:            metrics.New(),
		kihClientset:       cs,
		appStatus:          &appStatus,
		ippoolCountCurrent: &ippoolCountCurrent,
	}

	return c, c.ipam, c.dhcp, c.cache, c.metrics
}

// ippoolBehaviorAssertDHCPPoolOptions verifies that the DHCP pool registered for network
// carries exactly the projected options derived from an IPPool spec.
func ippoolBehaviorAssertDHCPPoolOptions(t *testing.T, d *kihdhcp.DHCPAllocator, network, serverIP, subnetMask, router string, dns, ntp []net.IP, domainName string, domainSearch []string, leaseTime int, nic string) {
	t.Helper()

	pool := d.GetPool(network)
	if len(pool.ServerIP) == 0 {
		t.Fatalf("expected a dhcp pool to be registered for network %q", network)
	}
	if !pool.ServerIP.Equal(net.ParseIP(serverIP)) {
		t.Errorf("server ip: got %q, want %q", pool.ServerIP.String(), serverIP)
	}
	// DHCPPool.SubnetMask is a net.IPMask whose String() is hexadecimal
	// (e.g. ffffff00); compare the mask bytes the same way the dhcp
	// package builds them from the dotted-quad representation.
	if !reflect.DeepEqual(pool.SubnetMask, net.IPMask(net.ParseIP(subnetMask).To4())) {
		t.Errorf("subnet mask: got %s, want %s", net.IP(pool.SubnetMask).String(), subnetMask)
	}
	if !pool.Router.Equal(net.ParseIP(router)) {
		t.Errorf("router: got %q, want %q", pool.Router.String(), router)
	}
	if !reflect.DeepEqual(pool.DNS, dns) {
		t.Errorf("dns: got %v, want %v", pool.DNS, dns)
	}
	if pool.DomainName != domainName {
		t.Errorf("domain name: got %q, want %q", pool.DomainName, domainName)
	}
	if !reflect.DeepEqual(pool.DomainSearch, domainSearch) {
		t.Errorf("domain search: got %v, want %v", pool.DomainSearch, domainSearch)
	}
	if !reflect.DeepEqual(pool.NTP, ntp) {
		t.Errorf("ntp: got %v, want %v", pool.NTP, ntp)
	}
	if pool.LeaseTime != leaseTime {
		t.Errorf("lease time: got %d, want %d", pool.LeaseTime, leaseTime)
	}
	if pool.Nic != nic {
		t.Errorf("nic: got %q, want %q", pool.Nic, nic)
	}
}

// ippoolBehaviorRestState backs a minimal fake API server for the typed IPPool client: it
// serves GET (stored object), PUT .../status (echo of the submitted body), the
// cluster-wide VirtualMachineNetworkConfig LIST the claim protection takes its
// authoritative snapshot from, and can be switched into failing modes.
type ippoolBehaviorRestState struct {
	mu       sync.Mutex
	pool     *kihv1.IPPool
	failGet  bool
	failPut  bool
	getCount int
	putCount int
	putPath  string
	lastBody *kihv1.IPPool
	// vmnetcfgs backs the cluster-wide list of the claim protection sweep
	// and its per-object re-verification reads
	vmnetcfgs []*kihv1.VirtualMachineNetworkConfig
	// failVMNetCfgList switches the list into its failure mode so a
	// registration cannot obtain its claim snapshot
	failVMNetCfgList bool
	listCount        int
	// failVMNetCfgGet switches the per-object claim re-verification into
	// its failure mode so an unverifiable claim must fail the registration
	failVMNetCfgGet  bool
	vmnetcfgGetCount int
	// vmnetcfgListHook runs after the list response was served: the
	// concurrency regressions use it to complete a concurrent cleanup
	// between the frozen list snapshot and the re-verification reads
	vmnetcfgListHook func()
}

func ippoolBehaviorNewRestState(pool *kihv1.IPPool) *ippoolBehaviorRestState {
	return &ippoolBehaviorRestState{pool: pool}
}

func (s *ippoolBehaviorRestState) ippoolBehaviorHandler() http.Handler {
	const prefix = "/apis/kubevirtiphelper.k8s.binbash.org/v1/ippools"
	const vmPrefix = "/apis/kubevirtiphelper.k8s.binbash.org/v1/virtualmachinenetworkconfigs"
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		restPath := strings.TrimPrefix(r.URL.Path, prefix)

		s.mu.Lock()
		defer s.mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			s.getCount++
			if s.failGet {
				ippoolBehaviorWriteKubeError(w, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(s.pool); err != nil {
				// the client is gone; nothing sensible to write
				return
			}
		case http.MethodPut:
			if !strings.HasSuffix(restPath, "/status") {
				ippoolBehaviorWriteKubeError(w, http.StatusNotFound)
				return
			}
			s.putCount++
			s.putPath = restPath
			if s.failPut {
				ippoolBehaviorWriteKubeError(w, http.StatusNotFound)
				return
			}
			var in kihv1.IPPool
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				ippoolBehaviorWriteKubeError(w, http.StatusBadRequest)
				return
			}
			s.lastBody = &in
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(&in); err != nil {
				return
			}
		default:
			ippoolBehaviorWriteKubeError(w, http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc(vmPrefix, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			ippoolBehaviorWriteKubeError(w, http.StatusMethodNotAllowed)
			return
		}

		list := &kihv1.VirtualMachineNetworkConfigList{
			TypeMeta: metav1.TypeMeta{APIVersion: kihv1.SchemeGroupVersion.String(), Kind: "VirtualMachineNetworkConfigList"},
		}

		hook := func() {}

		s.mu.Lock()
		s.listCount++
		if s.failVMNetCfgList {
			s.mu.Unlock()
			ippoolBehaviorWriteKubeError(w, http.StatusNotFound)
			return
		}
		for _, obj := range s.vmnetcfgs {
			list.Items = append(list.Items, *obj.DeepCopy())
		}
		hook = s.vmnetcfgListHook
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)

		if hook != nil {
			// the frozen snapshot was served: complete the interleaved
			// concurrent cleanup before the re-verification reads arrive.
			// the hook runs without the state lock and takes care of its
			// own locking
			hook()
		}
	})
	// the per-object re-verification read of the claim sweep: the
	// namespaced GET of one claiming object
	// (/apis/.../v1/namespaces/{ns}/virtualmachinenetworkconfigs/{name})
	mux.HandleFunc("/apis/kubevirtiphelper.k8s.binbash.org/v1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(segments) != 7 || segments[3] != "namespaces" || segments[5] != "virtualmachinenetworkconfigs" {
			ippoolBehaviorWriteKubeError(w, http.StatusNotFound)
			return
		}

		if r.Method != http.MethodGet {
			ippoolBehaviorWriteKubeError(w, http.StatusMethodNotAllowed)
			return
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		s.vmnetcfgGetCount++
		if s.failVMNetCfgGet {
			ippoolBehaviorWriteKubeError(w, http.StatusInternalServerError)
			return
		}

		for _, obj := range s.vmnetcfgs {
			if obj.Namespace == segments[4] && obj.Name == segments[6] {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(obj.DeepCopy())
				return
			}
		}

		ippoolBehaviorWriteKubeError(w, http.StatusNotFound)
	})
	return mux
}

func ippoolBehaviorWriteKubeError(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	reason := metav1.StatusReasonInternalError
	if code == http.StatusNotFound {
		reason = metav1.StatusReasonNotFound
	} else if code == http.StatusMethodNotAllowed {
		reason = metav1.StatusReasonMethodNotAllowed
	}

	_ = json.NewEncoder(w).Encode(&metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Reason:   reason,
		Message:  http.StatusText(code),
		Code:     int32(code),
	})
}

// ippoolBehaviorGatherMetrics scrapes the (unexported) prometheus registry of a metrics
// allocator via reflect, avoiding any change to production code.
func ippoolBehaviorGatherMetrics(t *testing.T, m *metrics.MetricsAllocator) []*dto.MetricFamily {
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

func ippoolBehaviorMetricValue(t *testing.T, m *metrics.MetricsAllocator, familyName string, labels map[string]string) (value float64, found bool) {
	t.Helper()

	for _, family := range ippoolBehaviorGatherMetrics(t, m) {
		if family.GetName() != familyName {
			continue
		}
		for _, metric := range family.GetMetric() {
			match := true
			for name, want := range labels {
				if !ippoolBehaviorMetricHasLabel(metric.GetLabel(), name, want) {
					match = false
					break
				}
			}
			if match {
				return metric.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func ippoolBehaviorMetricHasLabel(pairs []*dto.LabelPair, name, want string) bool {
	for _, pair := range pairs {
		if pair.GetName() == name && pair.GetValue() == want {
			return true
		}
	}
	return false
}

func TestHandleIPPoolObjectChangeAppInitIgnoresUpdate(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)
	c.appStatus.Store(APP_INIT)

	oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	oldPool.Status.LastUpdate = metav1.Now()
	if err := ca.Add(oldPool); err != nil {
		t.Fatalf("failed to seed cache: %s", err.Error())
	}
	cached := *oldPool

	newPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	// restart-class fields
	newPool.Spec.IPv4Config.ServerIP = "10.10.10.99"
	newPool.Spec.IPv4Config.Subnet = "10.20.0.0/16"
	// reload-class field
	newPool.Spec.IPv4Config.LeaseTime = 9999

	if err := c.handleIPPoolObjectChange(*oldPool, newPool); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if c.appStatus.Load() != APP_INIT {
		t.Errorf("app status changed during init: got %d, want %d", c.appStatus.Load(), APP_INIT)
	}
	if d.CheckPool("net-a") {
		t.Errorf("a dhcp pool was created although updates are ignored during init")
	}
	got, err := ca.Get("pool", "net-a")
	if err != nil {
		t.Fatalf("cached pool missing: %s", err.Error())
	}
	if !reflect.DeepEqual(got.(kihv1.IPPool), cached) {
		t.Errorf("cache was modified during init, want the unchanged cached pool")
	}
	if c.ippoolCountCurrent.Load() != 0 {
		t.Errorf("ippool count changed during init: got %d, want 0", c.ippoolCountCurrent.Load())
	}
}

func TestHandleIPPoolObjectChangeNoChangeKeepsState(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)

	oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	oldPool.Status.IPv4.Used = 7
	if err := ca.Add(oldPool); err != nil {
		t.Fatalf("failed to seed cache: %s", err.Error())
	}
	cached := *oldPool
	if err := c.createOrUpdateDHCPPool(oldPool); err != nil {
		t.Fatalf("failed to seed dhcp pool: %s", err.Error())
	}
	before := d.GetPool("net-a")

	// identical spec, different status: no pool option changed
	newPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	newPool.Status.IPv4.Used = 99

	if err := c.handleIPPoolObjectChange(*oldPool, newPool); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("app status changed on no-change update: got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}
	got, err := ca.Get("pool", "net-a")
	if err != nil {
		t.Fatalf("cached pool missing: %s", err.Error())
	}
	if !reflect.DeepEqual(got.(kihv1.IPPool), cached) {
		t.Errorf("no-change update must not refresh the cache with the new object")
	}
	if !reflect.DeepEqual(d.GetPool("net-a"), before) {
		t.Errorf("no-change update modified the dhcp pool")
	}
}

func TestHandleIPPoolObjectChangeReloadUpdatesPoolAndCache(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)

	oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := ca.Add(oldPool); err != nil {
		t.Fatalf("failed to seed cache: %s", err.Error())
	}
	if err := c.createOrUpdateDHCPPool(oldPool); err != nil {
		t.Fatalf("failed to seed dhcp pool: %s", err.Error())
	}

	newPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	newPool.Spec.IPv4Config.LeaseTime = 4200
	newPool.Spec.IPv4Config.DomainName = "corp.example.com"
	newPool.Spec.IPv4Config.DNS = []string{"10.10.10.5"}
	newPool.Spec.IPv4Config.DomainSearch = []string{"corp.example.com", "example.com"}
	newPool.Spec.IPv4Config.NTP = []string{"10.10.10.6", "10.10.10.7"}

	if err := c.handleIPPoolObjectChange(*oldPool, newPool); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	// A restart-class change would have flipped appStatus to APP_RESTART and
	// returned before touching the cache; staying APP_RUNNING with a refreshed
	// cache proves the change was classified as reloadable.
	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("reloadable change was classified as restart: app status got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}

	ippoolBehaviorAssertDHCPPoolOptions(t, d, "net-a",
		"10.10.10.1", "255.255.255.0", "10.10.10.254",
		[]net.IP{net.ParseIP("10.10.10.5")},
		[]net.IP{net.ParseIP("10.10.10.6"), net.ParseIP("10.10.10.7")},
		"corp.example.com",
		[]string{"corp.example.com", "example.com"},
		4200, "eth-test")

	stored, err := ca.Get("pool", "net-a")
	if err != nil {
		t.Fatalf("updated pool missing from cache: %s", err.Error())
	}
	storedPool := stored.(kihv1.IPPool)
	if storedPool.Spec.IPv4Config.LeaseTime != 4200 {
		t.Errorf("cached lease time: got %d, want 4200", storedPool.Spec.IPv4Config.LeaseTime)
	}
	if storedPool.Spec.IPv4Config.DomainName != "corp.example.com" {
		t.Errorf("cached domain name: got %q, want %q", storedPool.Spec.IPv4Config.DomainName, "corp.example.com")
	}
	if !reflect.DeepEqual(storedPool.Spec.IPv4Config.DNS, []string{"10.10.10.5"}) {
		t.Errorf("cached dns: got %v, want %v", storedPool.Spec.IPv4Config.DNS, []string{"10.10.10.5"})
	}
	if !reflect.DeepEqual(storedPool.Spec.IPv4Config.DomainSearch, []string{"corp.example.com", "example.com"}) {
		t.Errorf("cached domain search: got %v", storedPool.Spec.IPv4Config.DomainSearch)
	}
	if !reflect.DeepEqual(storedPool.Spec.IPv4Config.NTP, []string{"10.10.10.6", "10.10.10.7"}) {
		t.Errorf("cached ntp: got %v", storedPool.Spec.IPv4Config.NTP)
	}
}

func TestHandleIPPoolObjectChangeReloadAddsNewCacheEntry(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)

	oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	newPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	newPool.Spec.IPv4Config.LeaseTime = 4200

	// cache deliberately empty: the pool is not known yet
	if err := c.handleIPPoolObjectChange(*oldPool, newPool); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if !ca.Check(newPool) {
		t.Errorf("expected the reloaded pool to be added to the cache")
	}
	stored, err := ca.Get("pool", "net-a")
	if err != nil {
		t.Fatalf("updated pool missing from cache: %s", err.Error())
	}
	if stored.(kihv1.IPPool).Spec.IPv4Config.LeaseTime != 4200 {
		t.Errorf("cache does not hold the reloaded pool")
	}
	if !d.CheckPool("net-a") {
		t.Errorf("expected a dhcp pool to be created for the reloaded network")
	}
	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("reloadable change was classified as restart: app status got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}
}

// A rejected reload must not destroy the active dhcp pool and must keep the
// previously cached valid configuration; the error escapes so the queue can
// retry the update.
func TestHandleIPPoolObjectChangeRejectedInvalidSubnet(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)

	// both pool versions carry the same (invalid) subnet textually: the
	// unparseable-subnet guard rejects every update carrying such a
	// projection before the change even classifies as reload or restart,
	// so serving state must remain untouched
	if err := d.AddPool(
		"net-a",
		"10.10.10.1",
		"255.255.255.0",
		"10.10.10.254",
		[]string{"10.10.10.2", "10.10.10.3"},
		"example.local",
		[]string{"example.local"},
		[]string{"10.10.10.4"},
		3600,
		"eth-test",
	); err != nil {
		t.Fatalf("failed to seed dhcp pool: %s", err.Error())
	}
	oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	oldPool.Spec.IPv4Config.Subnet = "not-a-subnet"
	oldPool.Spec.IPv4Config.LeaseTime = 3600
	if err := ca.Add(oldPool); err != nil {
		t.Fatalf("failed to seed cache: %s", err.Error())
	}

	newPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	newPool.Spec.IPv4Config.Subnet = "not-a-subnet"
	newPool.Spec.IPv4Config.LeaseTime = 4200

	err := c.handleIPPoolObjectChange(*oldPool, newPool)
	if err == nil {
		t.Fatal("reload with an invalid subnet must return an error")
	}
	if !strings.Contains(err.Error(), "not-a-subnet") {
		t.Error("the returned error must name the invalid subnet projection")
	}

	// the active dhcp pool must survive the rejected update
	if !d.CheckPool("net-a") {
		t.Errorf("the active dhcp pool was deleted although the replacement was rejected")
	}
	stored, err := ca.Get("pool", "net-a")
	if err != nil {
		t.Fatalf("the valid pool is missing from cache: %s", err.Error())
	}
	storedPool := stored.(kihv1.IPPool)
	if storedPool.Spec.IPv4Config.Subnet != "not-a-subnet" {
		t.Errorf("cache holds subnet %q, want the previously cached entry preserved", storedPool.Spec.IPv4Config.Subnet)
	}
	if storedPool.Spec.IPv4Config.LeaseTime != 3600 {
		t.Errorf("cache lease time = %d, want the previously cached 3600, not the rejected 4200", storedPool.Spec.IPv4Config.LeaseTime)
	}
	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("app status changed: got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}
}

// an update which would restart the application must be rejected while the
// new subnet does not parse: the crd schema accepts lengths up to two
// digits, so spellings such as 10.10.10.0/33 reach the controller. the
// registered configuration must keep serving and the restored spec must
// reconcile as a no-change update afterwards, without a restart cycle.
func TestHandleIPPoolObjectChangeRejectsUnparseableSubnetUpdate(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)

	oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := ca.Add(oldPool); err != nil {
		t.Fatalf("failed to cache the registered pool: %s", err.Error())
	}

	if err := d.AddPool(
		"net-a",
		"10.10.10.1",
		"255.255.255.0",
		"10.10.10.254",
		[]string{"10.10.10.2", "10.10.10.3"},
		"example.local",
		[]string{"example.local"},
		[]string{"10.10.10.4"},
		3600,
		"eth-test",
	); err != nil {
		t.Fatalf("failed to seed the active dhcp pool: %s", err.Error())
	}

	newPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	newPool.Spec.IPv4Config.Subnet = "10.10.10.0/33"

	if err := c.handleIPPoolObjectChange(*oldPool, newPool); err == nil {
		t.Fatal("handleIPPoolObjectChange accepted an unparseable subnet update")
	}

	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("the rejected update started an application restart: app status got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}
	if plc := d.CheckPool("net-a"); !plc {
		t.Error("the rejected update removed the active dhcp pool")
	}
	if !ca.Check(oldPool) {
		t.Error("the rejected update touched the cache")
	}

	// restoring the previously registered subnet reconciles as no-change:
	// the registration keeps serving without a restart cycle
	restored := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := c.handleIPPoolObjectChange(*oldPool, restored); err != nil {
		t.Errorf("handleIPPoolObjectChange rejected the restored spec: %v", err)
	}
	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("the restored spec started an application restart: app status got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}
	if !d.CheckPool("net-a") {
		t.Error("the restored spec removed the active dhcp pool")
	}
}

// the pre-teardown validation must classify every projection which can
// never register, not only the unparseable subnet: an out-of-range start,
// a reversed range or the broadcast as end would drain the live dhcp pool
// and the registration afterwards, leaving the network unserved while the
// leader crash-loops or logs forever
func TestHandleIPPoolObjectChangeRejectsUnregistrableRangeUpdate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(pool *kihv1.IPPool)
	}{
		{"start outside the subnet", func(pool *kihv1.IPPool) { pool.Spec.IPv4Config.Pool.Start = "192.168.9.9" }},
		{"reversed range", func(pool *kihv1.IPPool) {
			pool.Spec.IPv4Config.Pool.Start = "10.10.10.50"
			pool.Spec.IPv4Config.Pool.End = "10.10.10.10"
		}},
		{"broadcast as end", func(pool *kihv1.IPPool) { pool.Spec.IPv4Config.Pool.End = "10.10.10.255" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)

			oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
			if err := ca.Add(oldPool); err != nil {
				t.Fatalf("failed to cache the registered pool: %s", err.Error())
			}

			if err := d.AddPool(
				"net-a",
				"10.10.10.1",
				"255.255.255.0",
				"10.10.10.254",
				[]string{"10.10.10.2", "10.10.10.3"},
				"example.local",
				[]string{"example.local"},
				[]string{"10.10.10.4"},
				3600,
				"eth-test",
			); err != nil {
				t.Fatalf("failed to seed the active dhcp pool: %s", err.Error())
			}

			newPool := ippoolBehaviorNewTestPool("pool1", "net-a")
			tc.mutate(newPool)

			if err := c.handleIPPoolObjectChange(*oldPool, newPool); err == nil {
				t.Fatal("handleIPPoolObjectChange accepted an unregistrable range update")
			}
			if c.appStatus.Load() != APP_RUNNING {
				t.Errorf("the rejected update started an application restart: app status got %d, want %d", c.appStatus.Load(), APP_RUNNING)
			}
			if !d.CheckPool("net-a") {
				t.Error("the rejected update removed the active dhcp pool")
			}
			if !ca.Check(oldPool) {
				t.Error("the rejected update touched the cache")
			}
		})
	}
}

func TestCreateOrUpdateDHCPPoolProjectsOptions(t *testing.T) {
	c, _, d, _, _ := ippoolBehaviorNewTestController(t, nil)

	pool := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := c.createOrUpdateDHCPPool(pool); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	ippoolBehaviorAssertDHCPPoolOptions(t, d, "net-a",
		"10.10.10.1", "255.255.255.0", "10.10.10.254",
		[]net.IP{net.ParseIP("10.10.10.2"), net.ParseIP("10.10.10.3")},
		[]net.IP{net.ParseIP("10.10.10.4")},
		"example.local",
		[]string{"example.local"},
		3600, "eth-test")

	// re-registering the same network replaces the existing pool entry
	pool.Spec.IPv4Config.LeaseTime = 1800
	pool.Spec.IPv4Config.DNS = []string{"10.10.10.9"}
	if err := c.createOrUpdateDHCPPool(pool); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	ippoolBehaviorAssertDHCPPoolOptions(t, d, "net-a",
		"10.10.10.1", "255.255.255.0", "10.10.10.254",
		[]net.IP{net.ParseIP("10.10.10.9")},
		[]net.IP{net.ParseIP("10.10.10.4")},
		"example.local",
		[]string{"example.local"},
		1800, "eth-test")
}

func TestCreateOrUpdateDHCPPoolRejectsInvalidSubnet(t *testing.T) {
	c, _, d, _, _ := ippoolBehaviorNewTestController(t, nil)

	pool := ippoolBehaviorNewTestPool("pool1", "net-a")
	pool.Spec.IPv4Config.Subnet = "not-a-subnet"

	if err := c.createOrUpdateDHCPPool(pool); err == nil {
		t.Fatal("expected the subnet parse error")
	}
	if d.CheckPool("net-a") {
		t.Errorf("no pool should be registered after the failed subnet parse")
	}
}

func TestRegisterIPPoolValidatesSubnetBeforeNetlink(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)

	pool := ippoolBehaviorNewTestPool("pool1", "net-a")
	pool.Spec.IPv4Config.Subnet = "300.1.2.0/24"

	cleanup, err := c.registerIPPool(pool)
	if err == nil {
		t.Fatalf("expected a subnet parse error")
	}
	if cleanup {
		t.Errorf("cleanup must stay false when validation fails before any sub-resource is created")
	}
	if d.CheckPool("net-a") {
		t.Errorf("dhcp pool must not be registered when the subnet is invalid")
	}
	if ca.Check(pool) {
		t.Errorf("pool must not be cached when the subnet is invalid")
	}
	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("app status changed: got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}
	if c.ippoolCountCurrent.Load() != 0 {
		t.Errorf("ippool count changed: got %d, want 0", c.ippoolCountCurrent.Load())
	}
}

func TestResetIPPoolStatusReconstructsStatus(t *testing.T) {
	stored := ippoolBehaviorNewTestPool("pool1", "net-a")
	stored.Status.LastUpdate = metav1.NewTime(time.Unix(1700000000, 0))
	stored.Status.LastUpdateBeforeStart = metav1.NewTime(time.Unix(1699999999, 0))
	stored.Status.IPv4.Allocated = map[string]string{"10.10.10.99": "USED"}
	stored.Status.IPv4.Used = 3
	stored.Status.IPv4.Available = 38
	prevLastUpdate := stored.Status.LastUpdate

	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, alloc, _, _, _ := ippoolBehaviorNewTestController(t, srv)
	if err := alloc.NewSubnet("net-a", "10.10.10.0/24", "10.10.10.10", "10.10.10.50"); err != nil {
		t.Fatalf("failed to register subnet: %s", err.Error())
	}
	if _, err := alloc.GetIP("net-a", "10.10.10.10"); err != nil {
		t.Fatalf("failed to allocate an ip: %s", err.Error())
	}

	pool := ippoolBehaviorNewTestPool("pool1", "net-a")
	pool.Spec.IPv4Config.Pool.Exclude = []string{"10.10.10.20", "10.10.10.21"}

	uPool, err := c.resetIPPoolStatus(pool, map[string]string{
		"10.10.10.10": "default/vm-a [02:00:00:00:00:01]",
		"10.10.10.30": "USED",
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	if uPool == nil {
		t.Fatalf("expected the updated pool to be returned")
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.getCount != 1 {
		t.Errorf("expected exactly one GET, got %d", rs.getCount)
	}
	if rs.putCount != 1 {
		t.Errorf("expected exactly one status PUT, got %d", rs.putCount)
	}
	if rs.putPath != "/pool1/status" {
		t.Errorf("status PUT path: got %q, want %q", rs.putPath, "/pool1/status")
	}

	upd := rs.lastBody
	if upd == nil {
		t.Fatalf("the status update was never received")
	}
	if upd.Status.LastUpdate.IsZero() {
		t.Errorf("LastUpdate must be refreshed to the current time")
	}
	if !upd.Status.LastUpdateBeforeStart.Time.Equal(prevLastUpdate.Time) {
		t.Errorf("LastUpdateBeforeStart must preserve the previous LastUpdate, got %v, want %v",
			upd.Status.LastUpdateBeforeStart.Time, prevLastUpdate.Time)
	}
	wantAllocated := map[string]string{
		"10.10.10.10": "default/vm-a [02:00:00:00:00:01]",
		"10.10.10.20": "EXCLUDED",
		"10.10.10.21": "EXCLUDED",
		"10.10.10.30": "USED",
	}
	if !reflect.DeepEqual(upd.Status.IPv4.Allocated, wantAllocated) {
		t.Errorf("allocated map: got %v, want %v", upd.Status.IPv4.Allocated, wantAllocated)
	}
	if upd.Status.IPv4.Used != 1 {
		t.Errorf("used: got %d, want 1 (from ipam)", upd.Status.IPv4.Used)
	}
	if upd.Status.IPv4.Available != 40 {
		t.Errorf("available: got %d, want 40 (from ipam)", upd.Status.IPv4.Available)
	}
}

func TestResetIPPoolStatusFirstStartSetsLastUpdateBeforeStart(t *testing.T) {
	rs := ippoolBehaviorNewRestState(ippoolBehaviorNewTestPool("pool1", "net-a")) // zero status timestamps
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)

	pool := ippoolBehaviorNewTestPool("pool1", "net-a")
	uPool, err := c.resetIPPoolStatus(pool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	if uPool == nil {
		t.Fatalf("expected the updated pool to be returned")
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	upd := rs.lastBody
	if upd == nil {
		t.Fatalf("the status update was never received")
	}
	if upd.Status.LastUpdateBeforeStart.IsZero() {
		t.Errorf("LastUpdateBeforeStart must be set on the first start")
	}
	if upd.Status.LastUpdate.IsZero() {
		t.Errorf("LastUpdate must be set on the first start")
	}
	if upd.Status.LastUpdateBeforeStart.Time.After(upd.Status.LastUpdate.Time) {
		t.Errorf("LastUpdateBeforeStart must not be after LastUpdate")
	}
}

func TestResetIPPoolStatusGetErrorIsReturned(t *testing.T) {
	rs := ippoolBehaviorNewRestState(ippoolBehaviorNewTestPool("pool1", "net-a"))
	rs.failGet = true
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)

	uPool, err := c.resetIPPoolStatus(ippoolBehaviorNewTestPool("pool1", "net-a"), nil)
	if err == nil {
		t.Fatalf("expected the GET failure to be returned")
	}
	if uPool != nil {
		t.Errorf("expected a nil pool when the GET fails")
	}
}

func TestResetIPPoolStatusUpdateStatusErrorIsReturned(t *testing.T) {
	rs := ippoolBehaviorNewRestState(ippoolBehaviorNewTestPool("pool1", "net-a"))
	rs.failPut = true
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)

	uPool, err := c.resetIPPoolStatus(ippoolBehaviorNewTestPool("pool1", "net-a"), nil)
	if err == nil {
		t.Fatalf("expected the status update failure to be returned")
	}
	if uPool == nil {
		t.Error("generated UpdateStatus client should return its allocated result object on error")
	}
}

func TestResetIPPoolMetricsSetsGaugesFromAPI(t *testing.T) {
	stored := ippoolBehaviorNewTestPool("pool1", "net-a")
	stored.Status.IPv4.Used = 7
	stored.Status.IPv4.Available = 93

	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, _, _, _, m := ippoolBehaviorNewTestController(t, srv)

	if err := c.resetIPPoolMetrics(ippoolBehaviorNewTestPool("pool1", "net-a")); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	labels := map[string]string{
		"ippool":  "pool1",
		"subnet":  "10.10.10.0/24",
		"network": "net-a",
	}
	if v, ok := ippoolBehaviorMetricValue(t, m, "kubevirtiphelper_ippool_used", labels); !ok || v != 7 {
		t.Errorf("ippool used gauge: got value %v found %v, want 7", v, ok)
	}
	if v, ok := ippoolBehaviorMetricValue(t, m, "kubevirtiphelper_ippool_available", labels); !ok || v != 93 {
		t.Errorf("ippool available gauge: got value %v found %v, want 93", v, ok)
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.getCount != 1 {
		t.Errorf("expected exactly one GET, got %d", rs.getCount)
	}
}

func TestResetIPPoolMetricsGetErrorIsReturned(t *testing.T) {
	rs := ippoolBehaviorNewRestState(ippoolBehaviorNewTestPool("pool1", "net-a"))
	rs.failGet = true
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)

	if err := c.resetIPPoolMetrics(ippoolBehaviorNewTestPool("pool1", "net-a")); err == nil {
		t.Fatalf("expected the GET failure to be returned")
	}
}

// A v6 subnet parses as a prefix but can never be registered: the
// projection validation must reject it before the bind-interface
// mutation, the dhcp pool or the listener exist, so no compensating
// cleanup (which would rebuild the same malformed address string it
// should remove) is needed.
func TestRegisterIPPoolRejectsIPv6BeforeAnyMutation(t *testing.T) {
	c, ipam, dhcp, cache, _ := ippoolBehaviorNewTestController(t, nil)

	var nicMutated bool
	orig := network.AddIpToNic
	network.AddIpToNic = func(nic string, ip4 string) error {
		nicMutated = true

		return nil
	}
	t.Cleanup(func() {
		network.AddIpToNic = orig
	})

	pool := ippoolBehaviorNewTestPool("pool-v6", "net-v6")
	pool.Spec.IPv4Config.Subnet = "2001:db8::/64"
	pool.Spec.IPv4Config.Pool.Start = "2001:db8::1"
	pool.Spec.IPv4Config.Pool.End = "2001:db8::2"

	cleanup, err := c.registerIPPool(pool)
	if err == nil {
		t.Fatal("the v6 subnet registration returned nil, want rejection")
	}
	if cleanup {
		t.Error("cleanup flag = true, want false: nothing was applied yet")
	}
	if !errors.Is(err, ErrPoolUnregistrable) {
		t.Errorf("rejection = %v, want the ErrPoolUnregistrable classification", err)
	}
	if nicMutated {
		t.Error("the bind interface must not be mutated for an unregistrable projection")
	}
	if dhcp.CheckPool(pool.Spec.NetworkName) {
		t.Error("no dhcp pool may exist for an unregistrable projection")
	}
	if ipam.Used(pool.Spec.NetworkName) != 0 {
		t.Error("no ipam subnet may exist for an unregistrable projection")
	}
	if cache.Check(pool) {
		t.Error("no cache entry may exist for an unregistrable projection")
	}
}

// The update path must reject an unregistrable projection before the
// reload teardown: a v6 subnet would otherwise delete the live dhcp pool
// and mask it to an all-ones v4 prefix or "<nil>" before any later check
// could reject it.
func TestHandleIPPoolObjectChangeRejectsIPv6KeepsLiveState(t *testing.T) {
	c, _, dhcp, cache, _ := ippoolBehaviorNewTestController(t, nil)
	c.appStatus.Store(APP_RUNNING)

	oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := c.createOrUpdateDHCPPool(oldPool); err != nil {
		t.Fatalf("seeding the live dhcp pool: %s", err)
	}
	if err := cache.Add(oldPool); err != nil {
		t.Fatalf("seeding the cache: %s", err)
	}

	newPool := oldPool.DeepCopy()
	newPool.Spec.IPv4Config.Subnet = "2001:db8::/64"
	newPool.Spec.IPv4Config.Pool.Start = "2001:db8::1"
	newPool.Spec.IPv4Config.Pool.End = "2001:db8::2"

	if err := c.handleIPPoolObjectChange(*oldPool, newPool); err == nil {
		t.Fatal("the v6 update returned nil, want rejection")
	}

	if !dhcp.CheckPool(oldPool.Spec.NetworkName) {
		t.Error("the live dhcp pool must survive a rejected projection update")
	}
	if !cache.Check(oldPool) {
		t.Error("the cached pool must survive a rejected projection update")
	}
}

// TestRegisterIPPoolValidatesExcludeEntriesBeforeNetlink pins the review
// finding: an exclude entry which the exclude pass could never reclaim
// (outside the pool range, the subnet or the broadcast) used to fail the
// registration only after the nic address, the dhcp pool and its listener
// were already live, so every resync tore the half-built registration
// down and rebuilt it forever. the entry must be rejected as an
// unregistrable projection before any mutation.
func TestRegisterIPPoolValidatesExcludeEntriesBeforeNetlink(t *testing.T) {
	cases := []struct {
		name    string
		exclude []string
	}{
		{"outside the pool range", []string{"10.10.10.200"}},
		{"outside the subnet", []string{"192.168.8.8"}},
		{"the broadcast address", []string{"10.10.10.255"}},
		{"unparseable", []string{"not-an-ip"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)

			pool := ippoolBehaviorNewTestPool("pool1", "net-a")
			pool.Spec.IPv4Config.Pool.Exclude = tc.exclude

			cleanup, err := c.registerIPPool(pool)
			if err == nil {
				t.Fatal("the unclaimable exclude entry must fail the registration")
			}
			if !errors.Is(err, ErrPoolUnregistrable) {
				t.Errorf("error = %v, want the ErrPoolUnregistrable classification so the startup gate counts the pool", err)
			}
			if cleanup {
				t.Error("cleanup flag = true, want false: the rejection must not tear down state it never created")
			}

			// the rejection happened before any mutation
			if d.CheckPool("net-a") {
				t.Error("no dhcp pool may exist after the pre-mutation rejection")
			}
			if ca.Check(pool) {
				t.Error("no cache entry may exist after the pre-mutation rejection")
			}
			if used := c.ipam.Used("net-a"); used != 0 {
				t.Errorf("ipam used = %d, want 0 (the rejection must precede the subnet registration)", used)
			}
		})
	}
}

// the update path must reject an unclaimable exclude entry before the
// restart teardown: the restart would drain the live services of the
// whole application and the re-registration of the next era would then
// fail at the same exclude entry forever, leaving the network unserved
// until the object is repaired by hand
func TestHandleIPPoolObjectChangeRejectsUnclaimableExcludeUpdate(t *testing.T) {
	cases := []struct {
		name    string
		exclude []string
	}{
		{"outside the pool range", []string{"10.10.10.20", "10.10.10.200"}},
		{"outside the subnet", []string{"10.10.10.20", "192.168.8.8"}},
		{"the broadcast address", []string{"10.10.10.20", "10.10.10.255"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)

			oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
			if err := ca.Add(oldPool); err != nil {
				t.Fatalf("failed to cache the registered pool: %s", err.Error())
			}

			if err := d.AddPool(
				"net-a",
				"10.10.10.1",
				"255.255.255.0",
				"10.10.10.254",
				[]string{"10.10.10.2", "10.10.10.3"},
				"example.local",
				[]string{"example.local"},
				[]string{"10.10.10.4"},
				3600,
				"eth-test",
			); err != nil {
				t.Fatalf("failed to seed the active dhcp pool: %s", err.Error())
			}

			newPool := ippoolBehaviorNewTestPool("pool1", "net-a")
			newPool.Spec.IPv4Config.Pool.Exclude = tc.exclude

			if err := c.handleIPPoolObjectChange(*oldPool, newPool); err == nil {
				t.Fatal("handleIPPoolObjectChange accepted an unclaimable exclude entry")
			}
			if c.appStatus.Load() != APP_RUNNING {
				t.Errorf("the rejected update started an application restart: app status got %d, want %d", c.appStatus.Load(), APP_RUNNING)
			}
			if !d.CheckPool("net-a") {
				t.Error("the rejected update removed the active dhcp pool")
			}
			if !ca.Check(oldPool) {
				t.Error("the rejected update touched the cache")
			}
		})
	}
}

// an exclude entry which the persisted ledger records for a live binding
// can never converge: the re-registration of the next era rejects it up
// front, so the update must be refused before the restart teardown as
// well - otherwise a working network is drained by an edit which can
// never succeed. the ledger is only consulted when the exclude entries
// actually changed.
func TestHandleIPPoolObjectChangeRejectsExcludeOverlappingLiveClaim(t *testing.T) {
	stored := ippoolBehaviorNewTestPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.10.10.20": kihipam.ExcludedOwner,
		"10.10.10.30": "default/vm-test [02:00:00:00:00:01]",
	}
	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	t.Cleanup(srv.Close)

	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, srv)

	oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := ca.Add(oldPool); err != nil {
		t.Fatalf("failed to cache the registered pool: %s", err.Error())
	}

	if err := d.AddPool(
		"net-a",
		"10.10.10.1",
		"255.255.255.0",
		"10.10.10.254",
		[]string{"10.10.10.2", "10.10.10.3"},
		"example.local",
		[]string{"example.local"},
		[]string{"10.10.10.4"},
		3600,
		"eth-test",
	); err != nil {
		t.Fatalf("failed to seed the active dhcp pool: %s", err.Error())
	}

	// the unchanged exclude list reconciles as no-change without
	// consulting the ledger
	unchanged := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := c.handleIPPoolObjectChange(*oldPool, unchanged); err != nil {
		t.Fatalf("the unchanged exclude list must reconcile as no-change: %v", err)
	}
	if got := rs.getCount; got != 0 {
		t.Errorf("ledger reads = %d for unchanged exclude entries, want 0", got)
	}

	// adding the claimed address to the exclude list is rejected before
	// the teardown
	newPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	newPool.Spec.IPv4Config.Pool.Exclude = []string{"10.10.10.20", "10.10.10.30"}

	if err := c.handleIPPoolObjectChange(*oldPool, newPool); err == nil {
		t.Fatal("handleIPPoolObjectChange accepted an exclude entry overlapping a live ledger claim")
	}
	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("the rejected update started an application restart: app status got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}
	if !d.CheckPool("net-a") {
		t.Error("the rejected update removed the active dhcp pool")
	}
	if !ca.Check(oldPool) {
		t.Error("the rejected update touched the cache")
	}
	if got := rs.getCount; got == 0 {
		t.Error("the ledger conflict check never consulted the pool status")
	}
}
