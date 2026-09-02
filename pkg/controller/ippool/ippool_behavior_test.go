package ippool

import (
	"encoding/json"
	"net"
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

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	kihdhcp "github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	kihipam "github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"

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

	appStatus := APP_RUNNING
	ippoolCountCurrent := 0

	var cs *kihclientset.Clientset
	if srv != nil {
		var err error
		cs, err = kihclientset.NewForConfig(&rest.Config{Host: srv.URL})
		if err != nil {
			t.Fatalf("failed to create clientset for test server: %s", err.Error())
		}
	}

	c := &Controller{
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
// serves GET (stored object), PUT .../status (echo of the submitted body) and
// can be switched into failing modes.
type ippoolBehaviorRestState struct {
	mu       sync.Mutex
	pool     *kihv1.IPPool
	failGet  bool
	failPut  bool
	getCount int
	putCount int
	putPath  string
	lastBody *kihv1.IPPool
}

func ippoolBehaviorNewRestState(pool *kihv1.IPPool) *ippoolBehaviorRestState {
	return &ippoolBehaviorRestState{pool: pool}
}

func (s *ippoolBehaviorRestState) ippoolBehaviorHandler() http.Handler {
	const prefix = "/apis/kubevirtiphelper.k8s.binbash.org/v1/ippools"
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
	return mux
}

func ippoolBehaviorWriteKubeError(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(&metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Reason:   metav1.StatusReasonNotFound,
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
	*c.appStatus = APP_INIT

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

	if *c.appStatus != APP_INIT {
		t.Errorf("app status changed during init: got %d, want %d", *c.appStatus, APP_INIT)
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
	if *c.ippoolCountCurrent != 0 {
		t.Errorf("ippool count changed during init: got %d, want 0", *c.ippoolCountCurrent)
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

	if *c.appStatus != APP_RUNNING {
		t.Errorf("app status changed on no-change update: got %d, want %d", *c.appStatus, APP_RUNNING)
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
	if *c.appStatus != APP_RUNNING {
		t.Errorf("reloadable change was classified as restart: app status got %d, want %d", *c.appStatus, APP_RUNNING)
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
	if *c.appStatus != APP_RUNNING {
		t.Errorf("reloadable change was classified as restart: app status got %d, want %d", *c.appStatus, APP_RUNNING)
	}
}

func TestHandleIPPoolObjectChangeReloadWithInvalidSubnet(t *testing.T) {
	c, _, d, ca, m := ippoolBehaviorNewTestController(t, nil)

	// both old and new pool carry the same unparsable subnet, so the
	// subnet field itself is unchanged and only reload-class options
	// differ. The change therefore stays on the in-process reload
	// branch of handleIPPoolObjectChange: no DHCP server stop and no
	// netlink call are involved.
	oldPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	oldPool.Spec.IPv4Config.Subnet = "not-a-subnet"
	if err := ca.Add(oldPool); err != nil {
		t.Fatalf("failed to seed cache: %s", err.Error())
	}
	// a dhcp pool already exists for the network (as if registered while
	// the subnet was still valid); the failed re-projection must drop it
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

	newPool := ippoolBehaviorNewTestPool("pool1", "net-a")
	newPool.Spec.IPv4Config.Subnet = "not-a-subnet"
	newPool.Spec.IPv4Config.LeaseTime = 4200

	if err := c.handleIPPoolObjectChange(*oldPool, newPool); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	// createOrUpdateDHCPPool deletes the existing pool before parsing the
	// subnet, so the aborted parse leaves no dhcp pool behind while the
	// cache still receives the update
	if d.CheckPool("net-a") {
		t.Errorf("expected the dhcp pool to be dropped after the failed update")
	}
	stored, err := ca.Get("pool", "net-a")
	if err != nil {
		t.Fatalf("updated pool missing from cache: %s", err.Error())
	}
	storedPool := stored.(kihv1.IPPool)
	if storedPool.Spec.IPv4Config.Subnet != "not-a-subnet" {
		t.Errorf("cache does not hold the reloaded pool with its subnet")
	}
	if storedPool.Spec.IPv4Config.LeaseTime != 4200 {
		t.Errorf("cache does not hold the reloaded pool options")
	}
	if *c.appStatus != APP_RUNNING {
		t.Errorf("app status changed: got %d, want %d", *c.appStatus, APP_RUNNING)
	}
	// handleIPPoolObjectChange records the parse failure reported by
	// createOrUpdateDHCPPool.
	if v, ok := ippoolBehaviorMetricValue(t, m, "kubevirtiphelper_app_logs", map[string]string{"loglevel": "error"}); !ok || v != 1 {
		t.Errorf("error log status metric = %v (found %t), want 1", v, ok)
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
	if *c.appStatus != APP_RUNNING {
		t.Errorf("app status changed: got %d, want %d", *c.appStatus, APP_RUNNING)
	}
	if *c.ippoolCountCurrent != 0 {
		t.Errorf("ippool count changed: got %d, want 0", *c.ippoolCountCurrent)
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

	uPool, err := c.resetIPPoolStatus(pool)
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
		"10.10.10.20": "EXCLUDED",
		"10.10.10.21": "EXCLUDED",
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
	uPool, err := c.resetIPPoolStatus(pool)
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

	uPool, err := c.resetIPPoolStatus(ippoolBehaviorNewTestPool("pool1", "net-a"))
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

	uPool, err := c.resetIPPoolStatus(ippoolBehaviorNewTestPool("pool1", "net-a"))
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
