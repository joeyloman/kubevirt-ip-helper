package ippool

// R08 regression tests: the registration reserved only the exclude
// entries, so a serverip or router which lies inside the allocation range
// was handed to a fresh guest by a fresh allocation while the pool also
// answered the address as its own dhcp identity. the registration (and
// the update preflight, exactly like F10's occupied-interface guard) now
// rejects an in-range infrastructure address unless the spec excludes it
// explicitly, before any listener, ipam or cache mutation.

import (
	"errors"
	"net/http/httptest"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"
)

// infrastructureAddressTestPool returns a pool whose server ip and router
// sit outside the allocation range, the only registrable shape.
func infrastructureAddressTestPool(name, network string) *kihv1.IPPool {
	pool := ippoolBehaviorNewTestPool(name, network)
	pool.Spec.IPv4Config.Pool = kihv1.Pool{
		Start:   "10.10.10.10",
		End:     "10.10.10.50",
		Exclude: []string{"10.10.10.20"},
	}

	return pool
}

// recordNicAdds stubs the netlink addition off the host interfaces and
// records every call, so the tests observe whether a rejected
// registration mutated the nic at all.
func recordNicAdds(t *testing.T) *[]string {
	t.Helper()

	var added []string
	orig := network.AddIpToNic
	network.AddIpToNic = func(nic string, ip4 string) error {
		added = append(added, nic+"/"+ip4)

		return nil
	}
	t.Cleanup(func() {
		network.AddIpToNic = orig
	})

	return &added
}

// TestRegisterIPPoolRejectsInRangeServerIP: a server ip inside the
// allocation range without an exclude entry must be rejected as
// unregistrable before any mutation: no listener, no cache entry, no ipam
// state and no nic change.
func TestRegisterIPPoolRejectsInRangeServerIP(t *testing.T) {
	c, ipam, d, ca, _ := ippoolBehaviorNewTestController(t, nil)
	adds := recordNicAdds(t)

	pool := infrastructureAddressTestPool("pool-infra", "net-infra")
	pool.Spec.IPv4Config.ServerIP = "10.10.10.30"

	cleanup, err := c.registerIPPool(pool)
	if err == nil {
		t.Fatal("the in-range serverip registration returned nil, want rejection")
	}
	if !errors.Is(err, ErrPoolUnregistrable) {
		t.Errorf("rejection = %v, want the ErrPoolUnregistrable classification", err)
	}
	if cleanup {
		t.Error("cleanup flag = true, want false: nothing was applied yet")
	}
	if d.CheckPool(pool.Spec.NetworkName) {
		t.Error("no dhcp pool may exist for a rejected infrastructure address")
	}
	if ipam.Used(pool.Spec.NetworkName) != 0 {
		t.Error("no ipam state may exist for a rejected infrastructure address")
	}
	if ca.Check(pool) {
		t.Error("no cache entry may exist for a rejected infrastructure address")
	}
	if len(*adds) != 0 {
		t.Errorf("the rejected registration mutated the nic: %v", *adds)
	}
}

// TestRegisterIPPoolRejectsInRangeRouter: the router is served on the
// wire like the server ip, so an in-range router without an exclude entry
// is rejected the same way.
func TestRegisterIPPoolRejectsInRangeRouter(t *testing.T) {
	c, ipam, d, ca, _ := ippoolBehaviorNewTestController(t, nil)
	stubNicMutation(t)

	pool := infrastructureAddressTestPool("pool-infra", "net-infra")
	pool.Spec.IPv4Config.Router = "10.10.10.40"

	cleanup, err := c.registerIPPool(pool)
	if err == nil {
		t.Fatal("the in-range router registration returned nil, want rejection")
	}
	if !errors.Is(err, ErrPoolUnregistrable) {
		t.Errorf("rejection = %v, want the ErrPoolUnregistrable classification", err)
	}
	if cleanup {
		t.Error("cleanup flag = true, want false: nothing was applied yet")
	}
	if d.CheckPool(pool.Spec.NetworkName) {
		t.Error("no dhcp pool may exist for a rejected infrastructure address")
	}
	if ipam.Used(pool.Spec.NetworkName) != 0 {
		t.Error("no ipam state may exist for a rejected infrastructure address")
	}
	if ca.Check(pool) {
		t.Error("no cache entry may exist for a rejected infrastructure address")
	}
}

// TestRegisterIPPoolExcludesTheInRangeServerIP: the same pool with the
// in-range server ip added to the exclude list registers, and the
// excluded infrastructure address is provably unallocatable: exhausting
// the range hands out every other address and never the server ip.
func TestRegisterIPPoolExcludesTheInRangeServerIP(t *testing.T) {
	stubNicMutation(t)

	stored := infrastructureAddressTestPool("pool-infra", "net-infra")
	stored.Spec.IPv4Config.Pool = kihv1.Pool{
		Start:   "10.10.10.10",
		End:     "10.10.10.12",
		Exclude: []string{"10.10.10.11"},
	}
	stored.Spec.IPv4Config.ServerIP = "10.10.10.11"
	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	t.Cleanup(srv.Close)

	c, ipam, d, ca, _ := ippoolBehaviorNewTestController(t, srv)
	c.runListener = func(networkName string, nic string) error {
		return nil
	}

	pool := stored.DeepCopy()
	if _, err := c.registerIPPool(pool); err != nil {
		t.Fatalf("the excluded in-range serverip must register: %s", err)
	}
	if !d.CheckPool(pool.Spec.NetworkName) {
		t.Error("the excluded in-range serverip did not register its dhcp pool")
	}
	if !ca.Check(pool) {
		t.Error("the excluded in-range serverip did not register its cache entry")
	}

	// exhausting the range proves the infrastructure address is reserved:
	// every allocation returns one of the two remaining addresses and the
	// third allocation fails instead of ever handing out the server ip
	allocated := map[string]bool{}
	for range 2 {
		ip, err := ipam.AllocateIP(pool.Spec.NetworkName, "test-vm")
		if err != nil {
			t.Fatalf("a fresh allocation of the exhausted-range pool failed early: %s", err)
		}
		if ip == pool.Spec.IPv4Config.ServerIP {
			t.Fatalf("a fresh allocation received the excluded infrastructure address %s", ip)
		}
		allocated[ip] = true
	}
	if len(allocated) != 2 {
		t.Errorf("the range allocations = %v, want the two non-excluded addresses", allocated)
	}
	if _, err := ipam.AllocateIP(pool.Spec.NetworkName, "test-vm"); err == nil {
		t.Error("the exhausted range kept allocating, want the exclusion to be the reservation")
	}
}

// TestUpdateRejectsMovingTheServerIPIntoTheRange: an edit which moves the
// server ip of a registered pool into the allocation range must be
// rejected by the preflight before any teardown: the application stays
// running, the listener keeps serving and no nic address is released.
func TestUpdateRejectsMovingTheServerIPIntoTheRange(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)
	removals := recordNicRemovals(t)

	pool := infrastructureAddressTestPool("pool-infra", "net-infra")
	occupiedInterfaceSeedPool(t, d, "net-infra", "eth-test")
	if err := ca.Add(pool); err != nil {
		t.Fatalf("caching the pool: %s", err.Error())
	}

	// the deterministically invalid edit: the server ip moves inside the
	// allocation range without an exclude entry
	edited := pool.DeepCopy()
	edited.Spec.IPv4Config.ServerIP = "10.10.10.30"

	if err := c.handleIPPoolObjectChange(*pool, edited); err == nil {
		t.Fatal("handleIPPoolObjectChange accepted moving the server ip into the range")
	}
	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("the rejected update started an application restart: app status got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}
	if !d.CheckPool("net-infra") {
		t.Error("the rejected update removed a serving dhcp pool")
	}
	if !ca.Check(pool) {
		t.Error("the rejected update touched the cache")
	}
	if len(*removals) != 0 {
		t.Errorf("the rejected update released nic addresses: %v", *removals)
	}
}

// TestUpdateRejectsMovingTheRouterIntoTheRange: the router is part of the
// same preflight, so an in-range router edit is refused the same way.
func TestUpdateRejectsMovingTheRouterIntoTheRange(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)
	removals := recordNicRemovals(t)

	pool := infrastructureAddressTestPool("pool-infra", "net-infra")
	occupiedInterfaceSeedPool(t, d, "net-infra", "eth-test")
	if err := ca.Add(pool); err != nil {
		t.Fatalf("caching the pool: %s", err.Error())
	}

	edited := pool.DeepCopy()
	edited.Spec.IPv4Config.Router = "10.10.10.40"

	if err := c.handleIPPoolObjectChange(*pool, edited); err == nil {
		t.Fatal("handleIPPoolObjectChange accepted moving the router into the range")
	}
	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("the rejected update started an application restart: app status got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}
	if !d.CheckPool("net-infra") {
		t.Error("the rejected update removed a serving dhcp pool")
	}
	if len(*removals) != 0 {
		t.Errorf("the rejected update released nic addresses: %v", *removals)
	}
}
