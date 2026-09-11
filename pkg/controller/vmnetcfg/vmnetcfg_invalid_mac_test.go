package vmnetcfg

// P0-2 regression tests: an unusable macaddress must be rejected before the
// address is claimed, so a later correction can still be served instead of
// being blocked forever by a reservation the invalid object consumed.

import (
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// An omitted/empty macaddress with a requested ip must not consume the
// reservation; once the macaddress is corrected the interface converges.
func TestVMNetCfgInvalidMacDoesNotConsumeReservationAndRecovers(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("10.0.0.1", "")
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("want the invalid macaddress to fail the sync")
	}
	// the invalid identity must not have consumed anything
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (no reservation for an unusable identity)", used)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease may exist for the invalid macaddress")
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("pool status allocations = %v, want empty", pool.Status.IPv4.Allocated)
	}
	if stored := e.getStoredVMNetCfg(); stored.Spec.NetworkConfig[0].IPAddress != "10.0.0.1" {
		t.Errorf("spec ip = %q, want the requested address kept", stored.Spec.NetworkConfig[0].IPAddress)
	}

	// the macaddress is corrected: the retried sync must now serve the
	// requested address instead of failing on its own stale claim. the api
	// object reflects the correction (the informer event and the durable
	// object are the same state a real controller sees)
	vmnetcfg.Spec.NetworkConfig[0].MACAddress = testMAC
	e.seedVMNetCfg(vmnetcfg)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the corrected interface must converge: %s", err)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Fatal("lease must exist after the macaddress correction")
	}
	if got := e.dhcp.GetLease(testMAC).ClientIP.String(); got != "10.0.0.1" {
		t.Errorf("lease ip = %s, want the requested 10.0.0.1", got)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1", used)
	}
	stored := e.getStoredVMNetCfg()
	if got := stored.Status.NetworkConfig[0]; got.Status != "OK" || got.MACAddress != testMAC {
		t.Errorf("status = %+v, want OK for the corrected macaddress", got)
	}
}

// A06 deletion-path regression: an unusable macaddress can never own a
// dhcp lease, so the deletion cleanup of such a nic converges instead of
// wedging the object in the terminating state - the helper finalizer is
// removed and a later healthy nic of the same object is still cleaned up
// (the pre-fix behavior stopped at the deterministic parse failure and
// left both finalizers and the healthy nic's lease, reservation and
// ledger record behind).
func TestVMNetCfgInvalidMacDoesNotWedgeTheDeletionCleanup(t *testing.T) {
	e := newTestEnv(t)

	// the healthy nic of the deleting object holds a fully applied binding
	e.addSubnet("10.0.0.1", "10.0.0.2")
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", testNamespace+"/"+testVMName+" ["+testMAC+"]"); err != nil {
		e.t.Fatalf("seeding the healthy claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.2", testNamespace+"/"+testVMName); err != nil {
		e.t.Fatalf("seeding the healthy lease: %s", err)
	}
	e.seedPool(map[string]string{"10.0.0.2": testNamespace + "/" + testVMName + " [" + testMAC + "]"})

	// the deleting object carries the malformed nic first and the healthy
	// nic second: the cleanup must not stop at the former
	now := metav1.Now()
	vmnetcfg := newVMNetCfg("", "")
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{IPAddress: "10.0.0.1", MACAddress: "not-a-mac-address", NetworkName: testNetwork},
		{IPAddress: "10.0.0.2", MACAddress: testMAC, NetworkName: testNetwork},
	}
	vmnetcfg.ObjectMeta.DeletionTimestamp = &now
	vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper"}
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the deletion cleanup must converge past the malformed nic: %s", err)
	}

	// the finalizer is removed although the first nic could not be parsed
	stored := e.getStoredVMNetCfg()
	if len(stored.ObjectMeta.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want empty (the malformed nic must not wedge the deletion)", stored.ObjectMeta.Finalizers)
	}

	// the later healthy nic is fully cleaned
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the healthy nic's lease must be deleted")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the healthy nic's claim is released)", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("pool status = %v, want the healthy nic's record removed", pool.Status.IPv4.Allocated)
	}
}
