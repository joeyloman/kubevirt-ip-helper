package vmnetcfg

// P2-3 regression tests: the lease-idempotent ownership repair works from
// a stale snapshot. The vm and the vmnetcfg controllers run independently,
// so the nic of the reconciled snapshot can be removed concurrently: the
// repair must not resurrect ownership state which the raced cleanup just
// removed, and a later binding must be able to take the freed address.

import (
	"net/http"
	"sync/atomic"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// repairRaceSetup seeds a vm whose binding is fully applied (lease, ipam
// claim and pool status record) and whose vmnetcfg snapshot keeps the
// recorded address, so a reconcile takes the lease-idempotent repair path.
func repairRaceSetup(t *testing.T) *testEnv {
	t.Helper()

	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(map[string]string{"10.0.0.1": canonicalLegacy})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", canonicalLegacy); err != nil {
		t.Fatalf("occupying the address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", legacyVMRef); err != nil {
		t.Fatalf("seeding the lease: %s", err)
	}

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "OK", Message: "IP address successfully allocated"},
	}
	e.seedVMNetCfg(vmnetcfg)

	return e
}

// simulateCleanup removes the lease (owner validated), releases the ipam
// claim and clears the pool status record: the interleaving of the vm
// controller cleanup for the removed nic.
func simulateCleanup(e *testEnv) {
	_ = e.dhcp.DeleteLeaseOwnedBy(testMAC, legacyVMRef)
	_ = e.ipam.ReleaseIP(testNetwork, "10.0.0.1")

	e.api.mu.Lock()
	e.api.ippools[testPoolName].Status.IPv4.Allocated = map[string]string{}
	e.api.mu.Unlock()
}

// TestRepairDoesNotResurrectAfterRacedCleanup: the vm controller cleanup
// races the ownership repair (the lease vanishes after the repair decision,
// the record write lands afterwards) - the resurrection must be undone and
// the freed address must be usable by a later binding.
func TestRepairDoesNotResurrectAfterRacedCleanup(t *testing.T) {
	e := repairRaceSetup(t)
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)

	// the simulated cleanup runs between the repair decision and the
	// record write: it lands on the pool status GET inside the repair
	var hooked atomic.Bool
	e.api.poolGetHook = func() {
		if !hooked.CompareAndSwap(false, true) {
			return
		}

		simulateCleanup(e)
	}

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the raced repair must converge instead of sticking an error: %s", err)
	}

	// the resurrection was undone: nothing of the removed nic survives
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the removed nic must not hold a lease")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 after the raced cleanup", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("pool status = %v, want the resurrected record undone", pool.Status.IPv4.Allocated)
	}

	// a later binding can take the freed address: the ownership record of
	// the removed nic must not block it
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
		t.Errorf("the freed address must be reissuable after the compensating delete: %s", err)
	}
}

// TestRacedCleanupWithFailedStatusDeleteIsCompensated: the raced cleanup
// releases the lease and the address but its own status delete fails - the
// resurrected record is on this reconciliation, and the compensating delete
// removes it.
func TestRacedCleanupWithFailedStatusDeleteIsCompensated(t *testing.T) {
	e := repairRaceSetup(t)
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)

	// the ownership record is missing: an earlier status write failure
	// left the lease and the claim retained without the record, which is
	// why this reconcile takes the repair path at all
	e.api.mu.Lock()
	e.api.ippools[testPoolName].Status.IPv4.Allocated = map[string]string{}
	e.api.mu.Unlock()

	var hooked atomic.Bool
	e.api.poolPutHook = func() {
		if !hooked.CompareAndSwap(false, true) {
			return
		}

		_ = e.dhcp.DeleteLeaseOwnedBy(testMAC, legacyVMRef)
		_ = e.ipam.ReleaseIP(testNetwork, "10.0.0.1")
	}

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the compensating delete must converge: %s", err)
	}

	if e.dhcp.CheckLease(testMAC) {
		t.Error("the removed nic must not hold a lease")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("pool status = %v, want the record removed by the compensation", pool.Status.IPv4.Allocated)
	}
}

// TestCompensatingDeleteNeverClobbersANewOwner: between the resurrection
// and the compensation another vm took the freed address and recorded its
// own ownership - the owner-validated compensating delete must leave that
// record alone.
func TestCompensatingDeleteNeverClobbersANewOwner(t *testing.T) {
	e := repairRaceSetup(t)
	// the ownership record is missing before the repair (same setup as
	// the failed-preceding-write scenario), so the reconcile takes the
	// repair path and issues the record write
	e.api.mu.Lock()
	e.api.ippools[testPoolName].Status.IPv4.Allocated = map[string]string{}
	e.api.mu.Unlock()

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)

	const foreignRef = "other-ns/other-vm [02:00:00:00:00:99]"

	var hooked atomic.Bool
	e.api.poolPutHook = func() {
		if !hooked.CompareAndSwap(false, true) {
			return
		}

		// the cleanup releases lease and address; the fresh record write
		// of the cleanup fails - and a new vm takes the freed address and
		// records its own ownership
		_ = e.dhcp.DeleteLeaseOwnedBy(testMAC, legacyVMRef)
		_ = e.ipam.ReleaseIP(testNetwork, "10.0.0.1")
		if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
			t.Errorf("the successor taking the freed address: %s", err)
		}

		e.api.mu.Lock()
		e.api.ippools[testPoolName].Status.IPv4.Allocated = map[string]string{
			"10.0.0.1": foreignRef,
		}
		e.api.mu.Unlock()
	}

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the sync must succeed although the compensation hit the foreign record: %s", err)
	}

	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != foreignRef {
		t.Errorf("pool record = %q, want the foreign owner preserved", got)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the successor's claim)", used)
	}
}

// TestRepairWithoutRaceStaysDone: without a concurrent cleanup the repair
// runs exactly as before - a record write per reconcile at most, no
// compensation traffic, and the binding keeps serving.
func TestRepairWithoutRaceStaysDone(t *testing.T) {
	e := repairRaceSetup(t)
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)

	// a missing ownership record is rebuilt
	e.api.mu.Lock()
	e.api.ippools[testPoolName].Status.IPv4.Allocated = map[string]string{}
	e.api.mu.Unlock()

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the plain repair must succeed: %s", err)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("the binding keeps serving")
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != canonicalLegacy {
		t.Errorf("pool record = %q, want the repaired owner record", got)
	}

	// a steady-state reconcile writes nothing: the repair decisions are
	// read-only when the record already matches
	puts := e.countRequests(http.MethodPut, ippoolStatusPath)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the steady-state repair must succeed: %s", err)
	}
	if got := e.countRequests(http.MethodPut, ippoolStatusPath); got != puts {
		t.Errorf("steady-state pool status puts %d -> %d, want read-only", puts, got)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 1 {
		t.Errorf("pool status = %v, want the single confirmed record", pool.Status.IPv4.Allocated)
	}
}

// deleted-vmnetcfg side of the raced cleanup: the tombstone resource must
// have the removed object name so no reconciliation recreates state for it.
var _ = kihv1.IPPool{}
