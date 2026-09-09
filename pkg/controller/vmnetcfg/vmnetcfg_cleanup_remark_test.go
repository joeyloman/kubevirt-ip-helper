package vmnetcfg

// P2-2 regression tests: an interrupted address transition must recover
// the owner's networking. The cleanup un-records the pool status entry
// before it releases anything locally, so a failed status write leaves
// the lease, the ipam claim and the record fully consistent: the owner
// keeps serving, the retried cleanup converges, and no sticky error
// status bricks the vm behind a re-marked anonymous reservation.

import (
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// TestVMNetCfgOldAddressCleanupStatusFailureStaysConsistent: the status
// delete fails before any local release - the old lease, the old claim and
// the old record stay fully intact, and the retried sync completes the
// transition to the recorded new address.
func TestVMNetCfgOldAddressCleanupStatusFailureStaysConsistent(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(map[string]string{"10.0.0.1": canonicalLegacy})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", canonicalLegacy); err != nil {
		t.Fatalf("occupying the old address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", legacyVMRef); err != nil {
		t.Fatalf("seeding the old lease: %s", err)
	}

	// the vm controller already recorded the new address in the spec: the
	// sync must clean up the old assignment (10.0.0.1) and allocate the new
	vmnetcfg := newVMNetCfg("10.0.0.2", testMAC)
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "OK", Message: "IP address successfully allocated"},
	}
	e.seedVMNetCfg(vmnetcfg)

	// the status write fails
	e.api.poolStatusPutCode = http.StatusInternalServerError
	err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg)
	if err == nil {
		t.Fatal("want the status failure to fail the sync")
	}

	// nothing was released: the owner keeps serving the old address
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Fatalf("lease = %v, want the old 10.0.0.1 kept (the cleanup aborted before touching it)", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the old claim stays held, not re-marked anonymously)", used)
	}
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", canonicalLegacy); err != nil {
		t.Error("the old address must still belong to its owner (idempotent own reclaim)")
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != canonicalLegacy {
		t.Errorf("old status entry = %q, want preserved after the failed delete", got)
	}

	// the retried sync converges: the new address is allocated and served
	// and the old one is honestly freed by the completed cleanup
	e.api.poolStatusPutCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the retried sync must converge: %s", err)
	}
	lease = e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.2" {
		t.Fatalf("lease = %v, want the new 10.0.0.2", lease)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (old released, new held)", used)
	}
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
		t.Errorf("the freed old address must be reissuable after the converged cleanup: %s", err)
	}
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated["10.0.0.2"]; got != canonicalLegacy {
		t.Errorf("new status entry = %q, want recorded for the owner", got)
	}
	if _, exists := pool.Status.IPv4.Allocated["10.0.0.1"]; exists {
		t.Error("old status entry must be gone after the converged cleanup")
	}
}

// The reviewer's brick scenario: a one-address pool serves an existing vm,
// the desired ip is cleared to request automatic allocation again, and the
// pool status un-record fails transiently. The vm must keep serving until
// the api recovers, and then rebind its address - never a sticky error
// status with a used one-address pool and no lease.
func TestVMNetCfgClearedAddressRecoversAfterCleanupFailure(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(map[string]string{"10.0.0.1": canonicalLegacy})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", canonicalLegacy); err != nil {
		t.Fatalf("occupying the address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", legacyVMRef); err != nil {
		t.Fatalf("seeding the lease: %s", err)
	}

	// the desired ip was cleared: the binding requests automatic allocation
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "OK", Message: "IP address successfully allocated"},
	}
	e.seedVMNetCfg(vmnetcfg)

	// steady state: the cleared-address reset is a live transition of a
	// running application (the startup replay defers fresh allocations
	// instead, covered by the finding-4 tests)
	*e.appStatus = APP_RUNNING

	// the transient cleanup failure during the address reset
	e.api.poolStatusPutCode = http.StatusInternalServerError
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err == nil {
		t.Fatal("want the status failure to fail the sync")
	}

	// fail closed without bricking: the vm still serves
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Fatalf("lease = %v, want the address kept through the failed reset", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Fatalf("ipam used = %d, want 1 (the own claim, not an anonymous re-mark)", used)
	}
	stored := e.getStoredVMNetCfg()
	if got := stored.Status.NetworkConfig[0].Status; got != "OK" {
		t.Errorf("nic status = %q, want the preserved OK (no sticky error)", got)
	}

	// the api recovers and the binding reclaims its address
	e.api.poolStatusPutCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the recovered sync must succeed: %s", err)
	}
	lease = e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Fatalf("lease = %v, want the rebound 10.0.0.1 (the one-address pool hands it back to its owner)", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 after the recovery", used)
	}
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated["10.0.0.1"]; got != canonicalLegacy {
		t.Errorf("status entry = %q, want the owner record rebuilt", got)
	}
	stored = e.getStoredVMNetCfg()
	if got := stored.Status.NetworkConfig[0].Status; got != "OK" {
		t.Errorf("nic status after recovery = %q, want OK", got)
	}
}

// the deletion path keeps its existing semantics: a foreign status entry is
// left for its owner while the finalizer still converges
func TestVMNetCfgDeleteForeignStatusEntryStillConverges(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
		t.Fatalf("occupying the address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", legacyVMRef); err != nil {
		t.Fatalf("seeding the lease: %s", err)
	}
	e.seedPool(map[string]string{"10.0.0.1": "other-ns/other-vm [02:00:00:00:00:99]"})

	now := metav1.Now()
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	vmnetcfg.ObjectMeta.DeletionTimestamp = &now
	vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper"}
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the deletion with a foreign status entry must complete: %s", err)
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (own reservation released)", used)
	}
	if stored := e.getStoredVMNetCfg(); len(stored.ObjectMeta.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed", stored.ObjectMeta.Finalizers)
	}
}
