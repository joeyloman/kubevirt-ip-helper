package vmnetcfg

// P0-2 regression tests: an unusable macaddress must be rejected before the
// address is claimed, so a later correction can still be served instead of
// being blocked forever by a reservation the invalid object consumed.

import (
	"testing"
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
	// requested address instead of failing on its own stale claim
	vmnetcfg.Spec.NetworkConfig[0].MACAddress = testMAC
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
