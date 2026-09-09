package vmnetcfg

// P2-3 regression tests: during an ip change the old address must never
// stay free in ipam while the transition is half-done. The cleanup releases
// the ipam mark before the pool status record is removed, so a failing
// status write must re-mark the reservation (fail closed) instead of
// leaving the address reissuable to a fresh allocation.

import (
	"net/http"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestVMNetCfgOldAddressCleanupStatusFailureReMarks: the status delete fails
// after the old address was released - the reservation is re-marked, stays
// non-reissuable, and the converged retry accounts for both the retained
// old mark and the new address.
func TestVMNetCfgOldAddressCleanupStatusFailureReMarks(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(map[string]string{"10.0.0.1": canonicalLegacy})
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
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

	// the status write fails after the old address was released
	e.api.poolStatusPutCode = http.StatusInternalServerError
	err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg)
	if err == nil {
		t.Fatal("want the status failure to fail the sync")
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the old lease must be gone (the transition was started)")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Fatalf("ipam used = %d, want 1: the released address must be re-marked", used)
	}
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err == nil {
		t.Fatal("the re-marked old address must not be reissuable")
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != canonicalLegacy {
		t.Errorf("old status entry = %q, want preserved after the failed delete", got)
	}

	// the retried sync converges: the new address is allocated and served
	// while the old mark and record stay held (fail-closed until a restart
	// rebuilds the status from the specs)
	e.api.poolStatusPutCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the retried sync must converge: %s", err)
	}
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.2" {
		t.Fatalf("lease = %v, want the new 10.0.0.2", lease)
	}
	if used := e.ipam.Used(testNetwork); used != 2 {
		t.Errorf("ipam used = %d, want 2 (retained old mark + new address)", used)
	}
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err == nil {
		t.Error("the old address must not be reissuable after the converged retry")
	}
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated["10.0.0.2"]; got != canonicalLegacy {
		t.Errorf("new status entry = %q, want recorded", got)
	}
	if _, exists := pool.Status.IPv4.Allocated["10.0.0.1"]; !exists {
		t.Error("old status entry must be retained (fail closed)")
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
