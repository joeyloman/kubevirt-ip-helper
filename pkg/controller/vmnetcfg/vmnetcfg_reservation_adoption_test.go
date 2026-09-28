package vmnetcfg

// F02 regression tests: a pending nic (an empty spec address) which still
// carries the binding's own reservation must adopt that reservation
// instead of routing it through a destructive cleanup or allocating a
// second address. Two states reach the adoption:
//
//   - the held reservation of a sync whose binding commit failed or could
//     not be resolved (the F01 keep state): the ipam claim and the ledger
//     record survived while the spec assignment never landed, and nothing
//     was served. the retried sync adopts the held address, so a
//     competing allocation can never take it and the stranded reservation
//     converges into the durable spec instead of leaking for the era.
//   - the quarantined lease of an earlier sync whose commit failed before
//     the spec recorded the address: the lease may already have been
//     ACKed, so it keeps serving continuously while the sync adopts its
//     address into the durable spec. the destructive mismatch cleanup
//     would release a possibly served address into a window where a
//     competing allocation takes it over.
//
// An explicit requested-address or network change never adopts: it
// records a different address or network and keeps its regular migration
// cleanup (pinned by the address-change tests).

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// holdUnreadableCommit drives a fresh allocation into the F01 keep state:
// the binding commit fails and the authoritative reread fails alike, so
// the claim and the ledger record stay held while nothing is served. it
// returns the held address read from the ledger.
func holdUnreadableCommit(t *testing.T, e *testEnv) string {
	t.Helper()

	e.api.vmnetcfgPutCode = http.StatusInternalServerError
	e.api.vmnetcfgGetFailFrom = 2

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("want the failed commit to fail the sync")
	}

	if e.dhcp.CheckLease(testMAC) {
		t.Fatal("the keep state must not serve anything")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Fatalf("ipam used = %d, want 1 (the held reservation)", used)
	}

	pool := e.getStoredPool()
	if len(pool.Status.IPv4.Allocated) != 1 {
		t.Fatalf("ledger records = %v, want exactly the held reservation", pool.Status.IPv4.Allocated)
	}
	heldIP := ""
	for ip := range pool.Status.IPv4.Allocated {
		heldIP = ip
	}

	e.api.vmnetcfgPutCode = 0
	e.api.vmnetcfgGetFailFrom = 0

	return heldIP
}

// TestHeldReservationIsAdoptedNotReallocated retries the keep state in a
// one-address pool while a competing allocation attempts the address
// mid-sync: the reservation stays continuously claimed, so the competitor
// must fail and the retried sync must commit and publish the held address.
// the pre-adoption behavior allocated a fresh address here, which failed
// against the still-held claim and left the reservation stranded.
func TestHeldReservationIsAdoptedNotReallocated(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	heldIP := holdUnreadableCommit(t, e)

	// the competing allocation attempts the address while the retried
	// sync reconciles: the adopted reservation keeps it claimed
	var hooked, competitorTookAddress atomic.Bool
	e.api.poolGetHook = func() {
		if hooked.CompareAndSwap(false, true) {
			if _, err := e.ipam.AllocateIP(testNetwork, "other-ns/other-vm"); err == nil {
				competitorTookAddress.Store(true)
			}
		}
	}

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("retried sync: %v", err)
	}

	if !hooked.Load() {
		t.Fatal("the competing allocation must run during the retried sync")
	}
	if competitorTookAddress.Load() {
		t.Error("a competing allocation must never receive the held reservation")
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != heldIP {
		t.Errorf("lease = %v, want the adopted %s published after the commit", lease.ClientIP, heldIP)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != heldIP {
		t.Errorf("stored spec = %+v, want the adopted address committed", stored.Spec.NetworkConfig)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the adopted reservation, no second claim)", used)
	}
}

// TestHeldReservationAdoptionKeepsTheSameAddressWithFreeAddressesLeft is
// the two-address variant: the allocator hands out the first free address
// by unspecified map iteration order, so the held address is read from the
// ledger and the retried sync must persist exactly it - never flip to the
// other free address while the first stays held for the rest of the era.
func TestHeldReservationAdoptionKeepsTheSameAddressWithFreeAddressesLeft(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	heldIP := holdUnreadableCommit(t, e)
	otherIP := "10.0.0.1"
	if heldIP == "10.0.0.1" {
		otherIP = "10.0.0.2"
	}

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("retried sync: %v", err)
	}

	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != heldIP {
		t.Errorf("lease = %v, want the adopted %s, never the other %s", lease.ClientIP, heldIP, otherIP)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != heldIP {
		t.Errorf("stored spec = %+v, want the adopted address committed", stored.Spec.NetworkConfig)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (no second address consumed)", used)
	}
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated[heldIP]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
		t.Errorf("ledger record of the adopted address = %q, want the owner record", got)
	}
	if _, exists := pool.Status.IPv4.Allocated[otherIP]; exists {
		t.Errorf("ledger records = %v, want no second record for %s", pool.Status.IPv4.Allocated, otherIP)
	}
}

// TestQuarantinedLeaseIsAdoptedNotReleased retries a pending nic whose own
// lease still serves the address of a quarantined allocation: the lease
// must stay continuously present while the sync adopts its address, and a
// competing allocation mid-sync must never succeed. the pre-adoption
// mismatch cleanup released the served address first, which handed it to
// the competitor and failed the sync against the exhausted one-address
// pool.
func TestQuarantinedLeaseIsAdoptedNotReleased(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")

	ownRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	e.seedPool(map[string]string{"10.0.0.1": ownRef})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", ownRef); err != nil {
		t.Fatalf("seeding the quarantined claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the quarantined lease: %s", err)
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	// the competing allocation attempts the address mid-sync while the
	// lease must keep serving continuously
	var hooked, competitorTookAddress, leaseServedThroughout atomic.Bool
	leaseServedThroughout.Store(true)
	e.api.poolGetHook = func() {
		if hooked.CompareAndSwap(false, true) {
			if !e.dhcp.CheckLease(testMAC) {
				leaseServedThroughout.Store(false)
			}
			if _, err := e.ipam.AllocateIP(testNetwork, "other-ns/other-vm"); err == nil {
				competitorTookAddress.Store(true)
			}
		}
	}

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the adoption must converge the quarantined lease: %v", err)
	}

	if !hooked.Load() {
		t.Fatal("the competing allocation must run during the sync")
	}
	if !leaseServedThroughout.Load() {
		t.Error("the quarantined lease must keep serving continuously through the adoption")
	}
	if competitorTookAddress.Load() {
		t.Error("a competing allocation must never receive the served address")
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("lease = %v, want the continuously serving 10.0.0.1", lease.ClientIP)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != "10.0.0.1" {
		t.Errorf("stored spec = %+v, want the adopted lease address committed", stored.Spec.NetworkConfig)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the claim was never released)", used)
	}
	// the adoption released and re-recorded nothing: the own ledger record
	// was only confirmed read-only
	if n := e.countRequests(http.MethodPut, ippoolStatusPath); n != 0 {
		t.Errorf("pool status writes = %d, want 0 (nothing was released or re-recorded)", n)
	}
}

// TestQuarantinedLeaseAdoptionKeepsTheSameAddressWithFreeAddressesLeft is
// the two-address variant of the lease adoption: the served address is
// read from the lease (the allocator's free-address order is unspecified)
// and the retried sync must persist exactly it, never flip to the other
// free address through a release window.
func TestQuarantinedLeaseAdoptionKeepsTheSameAddressWithFreeAddressesLeft(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the quarantined lease: %s", err)
	}
	servedIP := e.dhcp.GetLease(testMAC).ClientIP.String()
	otherIP := "10.0.0.1"
	if servedIP == "10.0.0.1" {
		otherIP = "10.0.0.2"
	}

	e.seedPool(map[string]string{servedIP: ownRef})
	if _, err := e.ipam.ReclaimIP(testNetwork, servedIP, ownRef); err != nil {
		t.Fatalf("seeding the quarantined claim: %s", err)
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the adoption must converge the quarantined lease: %v", err)
	}

	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != servedIP {
		t.Errorf("lease = %v, want the adopted %s, never the other %s", lease.ClientIP, servedIP, otherIP)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != servedIP {
		t.Errorf("stored spec = %+v, want the adopted lease address committed", stored.Spec.NetworkConfig)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (no second address consumed)", used)
	}
	if n := e.countRequests(http.MethodPut, ippoolStatusPath); n != 0 {
		t.Errorf("pool status writes = %d, want 0 (nothing was released or re-recorded)", n)
	}
}
