package ippool

// F10 regression tests: the update preflight validated the projection,
// the range, the excludes and the networkname, but not the
// bindinterface. an update which moved a registered pool onto the
// bindinterface of another registered pool passed the preflight,
// stopped its own healthy listener, released its server ip from the nic
// and reinitialized the whole application - and the re-registration of
// the next era rejected the pool forever, because the registration's
// interface-ownership check saw the interface occupied by the other
// pool. a deterministically invalid edit interrupted previously healthy
// service across the era.
//
// The fix reuses the registration's ownership check in the update
// preflight, before any stop or nic mutation. only an interface which
// actually changes is examined: the pool's own live registration claims
// its old interface, so a simultaneous networkname edit must never be
// read as a foreign claim on the unchanged interface (the restart tears
// the old registration down before the new one claims it).

import (
	"context"
	"testing"

	kihdhcp "github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"
)

// recordNicRemovals stubs the netlink removal off the host interfaces
// and records every call, so the tests observe whether a rejected
// update released any nic address.
func recordNicRemovals(t *testing.T) *[]string {
	t.Helper()

	var removed []string
	orig := network.RemoveIpFromNic
	network.RemoveIpFromNic = func(nic string, ip4 string) error {
		removed = append(removed, nic+"/"+ip4)

		return nil
	}
	t.Cleanup(func() {
		network.RemoveIpFromNic = orig
	})

	return &removed
}

// occupiedInterfaceSeedPool installs the dhcp pool of a registered
// network on its own interface, the state a completed registration
// leaves behind.
func occupiedInterfaceSeedPool(t *testing.T, d *kihdhcp.DHCPAllocator, networkName string, nic string) {
	t.Helper()

	if err := d.AddPool(
		context.Background(),
		networkName,
		"10.10.10.1",
		"255.255.255.0",
		"10.10.10.254",
		[]string{"10.10.10.2", "10.10.10.3"},
		"example.local",
		[]string{"example.local"},
		[]string{"10.10.10.4"},
		3600,
		nic,
	); err != nil {
		t.Fatalf("seeding the dhcp pool of network %s: %s", networkName, err.Error())
	}
}

// TestUpdateRejectsTheOccupiedBindInterface: pools a and b serve on
// their own interfaces. editing a's bindinterface to b's interface must
// be rejected before any teardown: both registrations keep serving, the
// application stays running and no nic address is released.
func TestUpdateRejectsTheOccupiedBindInterface(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)
	removals := recordNicRemovals(t)

	poolA := ippoolBehaviorNewTestPool("pool-a", "net-a")
	poolA.Spec.BindInterface = "eth-a"
	poolB := ippoolBehaviorNewTestPool("pool-b", "net-b")
	poolB.Spec.BindInterface = "eth-b"

	occupiedInterfaceSeedPool(t, d, "net-a", "eth-a")
	occupiedInterfaceSeedPool(t, d, "net-b", "eth-b")
	if err := ca.Add(poolA); err != nil {
		t.Fatalf("caching pool a: %s", err.Error())
	}
	if err := ca.Add(poolB); err != nil {
		t.Fatalf("caching pool b: %s", err.Error())
	}

	// the deterministically invalid edit: pool a moves onto the
	// interface pool b serves on, the tuple otherwise unchanged
	edited := poolA.DeepCopy()
	edited.Spec.BindInterface = "eth-b"

	if err := c.handleIPPoolObjectChange(*poolA, edited); err == nil {
		t.Fatal("handleIPPoolObjectChange accepted an update onto the occupied bindinterface")
	}
	if c.appStatus.Load() != APP_RUNNING {
		t.Errorf("the rejected update started an application restart: app status got %d, want %d", c.appStatus.Load(), APP_RUNNING)
	}
	if !d.CheckPool("net-a") || !d.CheckPool("net-b") {
		t.Error("the rejected update removed a serving dhcp pool")
	}
	if !ca.Check(poolA) || !ca.Check(poolB) {
		t.Error("the rejected update touched the cache")
	}
	if len(*removals) != 0 {
		t.Errorf("the rejected update released nic addresses: %v", *removals)
	}
}

// TestUpdateAcceptsTheMoveToAFreeInterface: the same edit corrected to
// a free interface must proceed through the restart: the moving pool's
// listener stops, its server ip leaves the old interface, the other
// pool keeps serving and the application enters its reinitialization.
func TestUpdateAcceptsTheMoveToAFreeInterface(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)
	removals := recordNicRemovals(t)

	poolA := ippoolBehaviorNewTestPool("pool-a", "net-a")
	poolA.Spec.BindInterface = "eth-a"
	poolB := ippoolBehaviorNewTestPool("pool-b", "net-b")
	poolB.Spec.BindInterface = "eth-b"

	occupiedInterfaceSeedPool(t, d, "net-a", "eth-a")
	occupiedInterfaceSeedPool(t, d, "net-b", "eth-b")
	if err := ca.Add(poolA); err != nil {
		t.Fatalf("caching pool a: %s", err.Error())
	}
	if err := ca.Add(poolB); err != nil {
		t.Fatalf("caching pool b: %s", err.Error())
	}

	edited := poolA.DeepCopy()
	edited.Spec.BindInterface = "eth-c"

	if err := c.handleIPPoolObjectChange(*poolA, edited); err != nil {
		t.Fatalf("the corrected edit must proceed: %s", err)
	}
	if c.appStatus.Load() != APP_RESTART {
		t.Errorf("the intended interface move must reinitialize the application: app status got %d, want %d", c.appStatus.Load(), APP_RESTART)
	}
	// the pool registry entry survives the restart path: dhcp.Stop tears
	// the listener down and the application reinitialization unregisters
	// the pool itself, so the registration stays observable until then
	if !d.CheckPool("net-a") {
		t.Error("the restart path unregistered the moving pool ahead of the reinitialization")
	}
	if !d.CheckPool("net-b") {
		t.Error("the restart of pool a removed pool b's listener")
	}
	if len(*removals) != 1 || (*removals)[0] != "eth-a/10.10.10.1/24" {
		t.Errorf("the server ip release of the old interface = %v, want exactly [eth-a/10.10.10.1/24]", *removals)
	}
}

// TestUpdateMovesTheNetworkOnTheSameInterface: a simultaneous
// networkname edit keeps the pool on its own interface. the pool's own
// live registration claims that interface under the old network, and
// the preflight must not read it as a foreign claim: the move proceeds
// through the restart, which tears the old registration down before the
// next era's registration claims the interface under the new network.
func TestUpdateMovesTheNetworkOnTheSameInterface(t *testing.T) {
	c, _, d, ca, _ := ippoolBehaviorNewTestController(t, nil)
	removals := recordNicRemovals(t)

	poolA := ippoolBehaviorNewTestPool("pool-a", "net-a")
	poolA.Spec.BindInterface = "eth-a"
	poolB := ippoolBehaviorNewTestPool("pool-b", "net-b")
	poolB.Spec.BindInterface = "eth-b"

	occupiedInterfaceSeedPool(t, d, "net-a", "eth-a")
	occupiedInterfaceSeedPool(t, d, "net-b", "eth-b")
	if err := ca.Add(poolA); err != nil {
		t.Fatalf("caching pool a: %s", err.Error())
	}
	if err := ca.Add(poolB); err != nil {
		t.Fatalf("caching pool b: %s", err.Error())
	}

	edited := poolA.DeepCopy()
	edited.Spec.NetworkName = "net-c"

	if err := c.handleIPPoolObjectChange(*poolA, edited); err != nil {
		t.Fatalf("the networkname move on the pool's own interface must proceed: %s", err)
	}
	if c.appStatus.Load() != APP_RESTART {
		t.Errorf("the intended network move must reinitialize the application: app status got %d, want %d", c.appStatus.Load(), APP_RESTART)
	}
	// the pool registry entry survives the restart path: dhcp.Stop tears
	// the listener down and the application reinitialization unregisters
	// the pool itself, so the registration stays observable until then
	if !d.CheckPool("net-a") {
		t.Error("the restart path unregistered the moving pool ahead of the reinitialization")
	}
	if !d.CheckPool("net-b") {
		t.Error("the restart of pool a removed pool b's listener")
	}
	if len(*removals) != 1 || (*removals)[0] != "eth-a/10.10.10.1/24" {
		t.Errorf("the server ip release of the old interface = %v, want exactly [eth-a/10.10.10.1/24]", *removals)
	}
}
