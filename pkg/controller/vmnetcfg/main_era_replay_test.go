package vmnetcfg

// Regression tests for the upgrade contract: state as persisted by main
// (v0.9.1) replayed through this controller. main wrote pool status entries
// "namespace/vmname [<mac>]" and vmnetcfg specs with the same spelling; the
// branch canonicalizes the mac spelling. These tests pin that a rebuild from
// main-era state preserves the recorded addresses, normalizes spellings and
// converges to a single owner - the invariant a production upgrade of a
// static-DHCP/etcd-adjacent deployment rests on.

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

const (
	legacyVMRef        = testNamespace + "/" + testVMName
	nonCanonicalMAC    = "02-00-00-00-00-01"
	nonCanonicalLegacy = legacyVMRef + " [" + nonCanonicalMAC + "]"
	canonicalLegacy    = legacyVMRef + " [" + testMAC + "]"
)

// legacyVMNetCfg builds a vmnetcfg exactly as main left it: spec records the
// ip/mac/networkname, status carries the OK entry, finalizer "kubevirtiphelper".
func legacyVMNetCfg(ip string, mac string) *kihv1.VirtualMachineNetworkConfig {
	vmnetcfg := newVMNetCfg(ip, mac)
	vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper"}
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: mac, NetworkName: testNetwork, Status: "OK", Message: "IP address successfully allocated"},
	}
	return vmnetcfg
}

// A main-era reservation (canonical mac spelling) must replay error-free and
// keep the same ip for the same owner: no release, no reallocation, no
// owner rewrite.
func TestMainEraReplayPreservesAddress(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(map[string]string{"10.0.0.1": canonicalLegacy})
	vmnetcfg := legacyVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("replay must not fail: %s", err)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Fatal("lease for the legacy vm must exist after replay")
	}
	if got := e.dhcp.GetLease(testMAC).ClientIP.String(); got != "10.0.0.1" {
		t.Fatalf("lease ip = %s, want the legacy 10.0.0.1", got)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Fatalf("ipam used = %d, want 1", used)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != canonicalLegacy {
		t.Errorf("allocated[10.0.0.1] = %q, want legacy ref %q", got, canonicalLegacy)
	}
	if got := e.getStoredVMNetCfg().Spec.NetworkConfig[0].IPAddress; got != "10.0.0.1" {
		t.Errorf("spec ip = %q, want 10.0.0.1", got)
	}
}

// After the registration wipe (status.allocated = excludes only) the replay
// must rebuild the entry for the same ip with the same owner identity.
func TestMainEraReplayAfterStatusWipeRebuildsEntry(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)
	vmnetcfg := legacyVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("replay must not fail: %s", err)
	}
	if got := e.dhcp.GetLease(testMAC).ClientIP.String(); got != "10.0.0.1" {
		t.Fatalf("lease ip = %s, want 10.0.0.1", got)
	}
	stored := e.getStoredPool()
	if got := stored.Status.IPv4.Allocated["10.0.0.1"]; got != canonicalLegacy {
		t.Errorf("allocated[10.0.0.1] = %q, want canonical ref %q", got, canonicalLegacy)
	}
	if stored.Status.IPv4.Used != 1 || stored.Status.IPv4.Available != 1 {
		t.Errorf("used/available = %d/%d, want 1/1", stored.Status.IPv4.Used, stored.Status.IPv4.Available)
	}
}

// A legacy vmnetcfg whose mac was persisted in dash spelling must still
// replay with the same ip and normalize lease/status identity.
func TestMainEraReplayNormalizesNonCanonicalMAC(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)
	vmnetcfg := legacyVMNetCfg("10.0.0.1", nonCanonicalMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("replay must not fail for a non-canonical legacy mac: %s", err)
	}
	if !e.dhcp.CheckLease(nonCanonicalMAC) || !e.dhcp.CheckLease(testMAC) {
		t.Fatal("lease must resolve under the raw and the canonical mac spelling")
	}
	if got := e.dhcp.GetLease(nonCanonicalMAC).ClientIP.String(); got != "10.0.0.1" {
		t.Fatalf("lease ip = %s, want 10.0.0.1", got)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Fatalf("ipam used = %d, want 1", used)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != legacyVMRef+" ["+testMAC+"]" {
		t.Errorf("allocated[10.0.0.1] = %q, want canonical ref", got)
	}
}

// Deleting a legacy vm must release the address and remove the main-era
// status entry, with legacy finalizer names.
func TestMainEraDeleteReleases(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
		t.Fatalf("occupying ip: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", legacyVMRef); err != nil {
		t.Fatalf("seeding lease: %s", err)
	}
	e.seedPool(map[string]string{"10.0.0.1": canonicalLegacy})

	now := metav1.Now()
	vmnetcfg := legacyVMNetCfg("10.0.0.1", testMAC)
	vmnetcfg.ObjectMeta.DeletionTimestamp = &now
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("legacy delete must not fail: %s", err)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("lease must be released")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("allocated = %v, want empty", pool.Status.IPv4.Allocated)
	}
	if stored := e.getStoredVMNetCfg(); len(stored.ObjectMeta.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed", stored.ObjectMeta.Finalizers)
	}
}

// The same deletion with a dash-spelled spec mac must also release after the
// branch normalized the status entry.
func TestMainEraDeleteNonCanonicalAfterNormalization(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
		t.Fatalf("occupying ip: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", legacyVMRef); err != nil {
		t.Fatalf("seeding lease: %s", err)
	}
	e.seedPool(map[string]string{"10.0.0.1": canonicalLegacy})

	now := metav1.Now()
	vmnetcfg := legacyVMNetCfg("10.0.0.1", nonCanonicalMAC)
	vmnetcfg.ObjectMeta.DeletionTimestamp = &now
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("delete with legacy mac spelling must not fail: %s", err)
	}
	if e.dhcp.CheckLease(nonCanonicalMAC) {
		t.Error("lease must be released")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("allocated = %v, want empty", pool.Status.IPv4.Allocated)
	}
}

// A main-era inconsistency (pool status records an ip for one vm while a
// second vmnetcfg claims the same address) must converge to exactly one
// served owner regardless of replay order - never a released address or a
// second served owner. The claimants use distinct macs so the status-conflict
// logic decides the winner, not the duplicate-mac guard.
func TestMainEraForeignConflictConvergesToSingleOwner(t *testing.T) {
	build := func(recordedOwnerFirst bool) {
		t.Helper()

		e := newTestEnv(t)
		e.addSubnet("10.0.0.1", "10.0.0.2")
		e.seedPool(map[string]string{"10.0.0.1": "default/other-vm [" + testMAC + "]"})

		recorded := newVMNetCfg("10.0.0.1", testMAC)
		recorded.ObjectMeta.Name = "other-vm"
		recorded.Spec.VMName = "other-vm"

		claimant := legacyVMNetCfg("10.0.0.1", testMAC2)
		e.seedVMNetCfg(recorded)
		e.seedVMNetCfg(claimant)

		replay := func(obj *kihv1.VirtualMachineNetworkConfig) error {
			return e.controller.updateVirtualMachineNetworkConfig(ADD, obj)
		}

		if recordedOwnerFirst {
			if err := replay(recorded); err != nil {
				t.Fatalf("recorded owner replay must succeed: %s", err)
			}
			_ = replay(claimant) // second claimant fails at ipam GetIP; may error
		} else {
			if err := replay(claimant); err == nil {
				t.Fatal("conflicting claimant must not succeed while the entry is foreign")
			}
			if err := replay(recorded); err != nil {
				t.Fatalf("recorded owner replay must succeed: %s", err)
			}
		}

		// convergent end state: exactly one served owner, ip still claimed
		if used := e.ipam.Used(testNetwork); used != 1 {
			t.Fatalf("ipam used = %d, want exactly 1 (the recorded owner)", used)
		}
		lease := e.dhcp.GetLease(testMAC)
		if lease.ClientIP == nil || lease.Reference != "default/other-vm" {
			t.Fatalf("lease = %+v, want served for default/other-vm", lease)
		}
		if e.dhcp.CheckLease(testMAC2) {
			t.Error("conflicting claimant must not hold a lease")
		}
		if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != "default/other-vm ["+testMAC+"]" {
			t.Errorf("allocated[10.0.0.1] = %q, want the recorded owner preserved", got)
		}
	}

	t.Run("recorded owner replayed first", func(t *testing.T) { build(true) })
	t.Run("conflicting claimant replayed first", func(t *testing.T) { build(false) })
}
