package vmnetcfg

import (
	"errors"
	"fmt"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	ipam "github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
)

// Edge regressions of the claim ownership boundary: the corrected binding
// of a vm retakes the ownerless protection pin the registration sweep made
// for its unusable-mac claim, a pin of another vm or an unattributed pin
// stays rejected, a binding's fresh allocation is a named reservation the
// delayed cleanup can release owner-validated, and the ip-change cleanup
// never frees a successor's claim.

// TestCorrectedMacRetakesItsAttributedPin: the registration protection
// pins the recorded address of a claim whose macaddress was unusable at
// registration time ownerlessly, attributed to the claiming vm. Once the
// user corrects the macaddress, the binding of that vm must retake its
// own pin through the claimant reclaim: the lease is restored, the status
// becomes OK instead of a sticky ERROR, and the ledger record is rebuilt
// under the canonical owner.
func TestCorrectedMacRetakesItsAttributedPin(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	// the pin the registration sweep made for the unusable-mac claim of
	// this vm: ownerless, attributed to the vm-level reference
	if err := e.ipam.ProtectIP(testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the attributed pin: %s", err)
	}

	// the user supplied the valid macaddress
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the corrected binding must recover its recorded address: %s", err)
	}

	// the binding serves its lease again and its nic status is OK
	if got := e.dhcp.GetLease(testMAC).ClientIP.String(); got != "10.0.0.1" {
		t.Errorf("the corrected binding's lease ip = %q, want its recorded 10.0.0.1", got)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Status.NetworkConfig) != 1 || stored.Status.NetworkConfig[0].Status != "OK" {
		t.Errorf("nic status = %+v, want OK (no sticky ERROR for the own protection pin)", stored.Status.NetworkConfig)
	}

	// the ledger record is rebuilt under the canonical owner and the
	// reservation now carries the binding's identity
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != canonicalLegacy {
		t.Errorf("pool record = %q, want the canonical owner %q", got, canonicalLegacy)
	}
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", canonicalLegacy); err != nil {
		t.Errorf("the promoted claim must belong to the binding: %s", err)
	}
}

// A pin which is attributed to another vm, and an unattributed pin (the
// conservative protection of an unknown historical reference), are never
// adopted: the binding fails visibly with a sticky ERROR and keeps no
// lease, while the protected address stays reserved.
func TestForeignAndUnattributedPinsAreNotAdopted(t *testing.T) {
	for name, attribution := range map[string]string{
		"attributed to another vm": "default/other-vm",
		"unattributed":             "",
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			e.appStatus.Store(APP_RUNNING)
			e.addSubnet("10.0.0.1", "10.0.0.1")
			e.seedPool(nil)

			if err := e.ipam.ProtectIP(testNetwork, "10.0.0.1", attribution); err != nil {
				t.Fatalf("seeding the pin: %s", err)
			}

			vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
			e.seedVMNetCfg(vmnetcfg)

			// the foreign-owner rejection is a visible nic ERROR, not a
			// sync failure
			if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
				t.Fatalf("the rejected reclaim must surface as a nic status, not a sync error: %s", err)
			}

			if e.dhcp.CheckLease(testMAC) {
				t.Error("the binding must not serve a lease for a pin it cannot take")
			}
			stored := e.getStoredVMNetCfg()
			if len(stored.Status.NetworkConfig) != 1 || stored.Status.NetworkConfig[0].Status != "ERROR" {
				t.Errorf("nic status = %+v, want the visible foreign-owner ERROR", stored.Status.NetworkConfig)
			}

			// the protected address stays reserved and unclaimable
			if used := e.ipam.Used(testNetwork); used != 1 {
				t.Errorf("ipam used = %d, want 1 (the pin stays protected)", used)
			}
			if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", canonicalLegacy); err == nil {
				t.Error("the pin must stay unclaimable by the rejected binding")
			}
		})
	}
}

// TestFreshAllocationIsANamedReservation: a binding's fresh
// auto-allocation carries the binding's owner identity from the moment it
// exists, so the owner-validated cleanup of the own nic releases exactly
// it while no other owner can displace or release it.
func TestFreshAllocationIsANamedReservation(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the fresh allocation: %s", err)
	}

	// the reservation carries the binding's identity: the own reclaim is
	// idempotent and a foreign release is rejected
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", canonicalLegacy); err != nil {
		t.Errorf("the own reclaim of the fresh allocation: %s", err)
	}
	foreignRef := fmt.Sprintf("%s/other-vm [02:00:00:00:00:99]", testNamespace)
	if err := e.ipam.ReleaseIPOwnedBy(testNetwork, "10.0.0.1", foreignRef); !errors.Is(err, ipam.ErrIPForeignOwner) {
		t.Errorf("a foreign release of the named reservation = %v, want ErrIPForeignOwner", err)
	}

	// the owner-validated cleanup of the own nic releases exactly it
	stored := e.getStoredVMNetCfg()
	netCfg := &kihv1.NetworkConfig{
		MACAddress:  testMAC,
		NetworkName: testNetwork,
		IPAddress:   "10.0.0.1",
	}
	if err := e.controller.cleanupNetworkInterface(stored, netCfg, false); err != nil {
		t.Fatalf("the own cleanup must release its named reservation: %s", err)
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used after the own cleanup = %d, want 0", used)
	}
}

// TestIpChangeCleanupLeavesTheSuccessorClaim: the ip-change cleanup
// un-records its own ledger entry and then releases the old address - but
// only while the reservation still carries its own owner reference. A
// successor which took the address over in the meantime keeps its claim,
// and the cleanup converges.
func TestIpChangeCleanupLeavesTheSuccessorClaim(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	// the own ledger record still exists while the claim was taken over
	// by a successor (the removed nic's binding already compensated)
	e.seedPool(map[string]string{"10.0.0.1": canonicalLegacy})
	successorRef := fmt.Sprintf("%s/vm-successor [02:00:00:00:00:09]", testNamespace)
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", successorRef); err != nil {
		t.Fatalf("seeding the successor's claim: %s", err)
	}

	vmnetcfg := legacyVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	netCfg := &kihv1.NetworkConfig{
		MACAddress:  testMAC,
		NetworkName: testNetwork,
		IPAddress:   "10.0.0.1",
	}
	if err := e.controller.cleanupNetworkInterface(vmnetcfg, netCfg, false); err != nil {
		t.Fatalf("the cleanup must converge on the successor's claim: %s", err)
	}

	// the successor keeps its claim and the own record was removed
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the successor's claim preserved)", used)
	}
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", successorRef); err != nil {
		t.Errorf("the successor's own reclaim after the cleanup: %s", err)
	}
	if _, ok := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; ok {
		t.Errorf("pool status = %v, want the own record removed", e.getStoredPool().Status.IPv4.Allocated)
	}
}
