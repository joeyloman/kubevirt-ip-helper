package vmnetcfg

import (
	"net/http"
	"strings"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// A failed sync must never unwind the restore of an address which the stored
// spec already records: the guest of the existing assignment keeps using the
// address, so releasing its lease and ipam reservation would hand it to
// another vm while the durable object still claims it.

// A later invalid owned MAC must not free an earlier durable binding.
func TestVMNetCfgFailedSyncKeepsRestoredDurableAllocation(t *testing.T) {
	e := newTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	// restart scenario: the lease map is empty and both nics carry already
	// persisted addresses; the second MAC is malformed
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC, NetworkName: testNetwork, IPAddress: "10.0.0.1"},
		{MACAddress: "not-a-mac", NetworkName: testNetwork, IPAddress: "10.0.0.2"},
	}
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the invalid second MAC to fail the sync")
	}

	// the restored assignment of the first nic must stay fully applied
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("restored lease = %v, want 10.0.0.1 kept by the failed sync", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the durable address must stay reserved)", used)
	}

	// the pool status entry written during the restore must survive too
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated["10.0.0.1"]; got == "" {
		t.Error("the restored allocation must stay recorded in the pool status")
	}

	// the freed address must not be handed to a competing vm: a fresh
	// allocation on the same network must skip the reserved address. the
	// competing sync runs in the steady state; the startup replay defers
	// fresh allocations instead (covered by the finding-4 tests)
	e.appStatus.Store(APP_RUNNING)

	hijacker := newVMNetCfg("", "02:00:00:00:00:99")
	hijacker.Name = "vm-hijacker"
	hijacker.Spec.VMName = "vm-hijacker"
	e.seedVMNetCfg(hijacker)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, hijacker); err != nil {
		t.Fatalf("competing sync: %s", err)
	}

	e.api.mu.Lock()
	stored := e.api.vmnetcfgs[testNamespace+"/vm-hijacker"].DeepCopy()
	e.api.mu.Unlock()

	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
		t.Errorf("competing vm received %q, want 10.0.0.2 (10.0.0.1 must stay reserved for its owner)", got)
	}
}

// a failing pool status update of the only nic must not release the restored
// durable address either: the reservation protects the running guest even
// while the status write keeps failing
func TestVMNetCfgPoolStatusFailureKeepsRestoredDurableAllocation(t *testing.T) {
	e := newTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)
	e.api.poolStatusPutCode = http.StatusInternalServerError

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the pool status failure to fail the sync")
	}
	if !strings.Contains(err.Error(), "cannot update the IPPool") {
		t.Errorf("error = %q, want the pool status rejection", err)
	}

	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("restored lease = %v, want 10.0.0.1 kept despite the status failure", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the durable address must stay reserved)", used)
	}
}

// within one sync the rollback must distinguish the allocation kinds: the
// fresh allocation of an earlier nic stays quarantined (its lease may
// already have been served to the guest; the retried sync adopts it into
// the durable object), while the restored durable assignment of another
// nic stays applied as well
func TestVMNetCfgFailedSyncQuarantinesFreshAllocations(t *testing.T) {
	e := newTestEnv(t)
	// steady state: a running application's sync failure quarantines only
	// the fresh allocations of this sync
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	// The first NIC restores, the second allocates, and the third fails.
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC, NetworkName: testNetwork, IPAddress: "10.0.0.1"},
		{MACAddress: testMAC2, NetworkName: testNetwork},
		{MACAddress: "not-a-mac", NetworkName: testNetwork},
	}
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the invalid third MAC to fail the sync")
	}

	// the durable restore stays applied
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("restored lease = %v, want 10.0.0.1 kept by the failed sync", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 2 {
		t.Errorf("ipam used = %d, want durable and quarantined claims", used)
	}

	// the fresh allocation of the second nic stays quarantined: its lease
	// may already have been served, so it is kept (lease, claim and record)
	// until the retried sync adopts it
	if freshLease := e.dhcp.GetLease(testMAC2); freshLease.ClientIP == nil || freshLease.ClientIP.String() != "10.0.0.2" {
		t.Errorf("fresh lease = %v, want the quarantined lease kept", freshLease.ClientIP)
	}

	e.api.mu.Lock()
	poolBStored := e.api.ippools[testPoolName].DeepCopy()
	e.api.mu.Unlock()
	if got := poolBStored.Status.IPv4.Allocated["10.0.0.2"]; got == "" {
		t.Error("the quarantined status record of the second nic must be kept")
	}
}

// the contested unwind tolerates its converged foreign-owner outcomes: the
// ledger record of a contested address belongs to another owner by
// definition and a concurrently reassigned lease is not this binding's to
// delete, so neither may raise the error level while the own claim is still
// released
func TestContestedRollbackClassifiesForeignOwnerOutcomesAsConverged(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(map[string]string{"10.0.0.1": "other-ns/other-vm [02:00:00:00:00:99]"})

	vmnetcfg := newVMNetCfg("", testMAC)
	ownRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"

	// the state a contested rollback reverts: a claim under this binding's
	// owner reference, while the lease was reassigned to another owner and
	// the ledger records the address for another owner as well
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", ownRef); err != nil {
		t.Fatalf("seeding the own claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", "other-ns/other-vm"); err != nil {
		t.Fatalf("seeding the reassigned lease: %s", err)
	}

	e.controller.rollbackAppliedAllocations(vmnetcfg, []allocatedNetworkConfig{
		{macAddress: testMAC, networkName: testNetwork, ipAddress: "10.0.0.1", poolName: testPoolName, contested: true},
	})

	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the contested claim is released)", used)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.Reference != "other-ns/other-vm" {
		t.Errorf("reassigned lease reference = %q, want the foreign owner preserved", lease.Reference)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != "other-ns/other-vm [02:00:00:00:00:99]" {
		t.Errorf("foreign record = %q, want preserved", got)
	}

	// the converged outcomes must not raise the error level
	if v, ok := e.metricValue(metricAppLogs, map[string]string{"loglevel": "error"}); ok && v != 0 {
		t.Errorf("error log metric = %v, want none: the foreign-owner outcomes of the contested unwind are converged", v)
	}
}

// an allocation whose dhcp lease could not be registered was never served:
// its claim must be released directly through releaseOwnClaim (the branch
// the failed lease registration takes), because no lease, no ledger record
// and no spec entry references the address anymore and no later
// reconciliation could detect a quarantined claim again. the branch itself
// only runs when the lease registration fails after the pre-validation,
// which a second lease writer or a diverging mac spelling would cause.
func TestUndeliveredClaimIsReleasedNotQuarantined(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	ownRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", ownRef); err != nil {
		t.Fatalf("seeding the undelivered claim: %s", err)
	}

	e.controller.releaseOwnClaim(testNetwork, "10.0.0.1", ownRef)

	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the undelivered claim is released, not quarantined)", used)
	}

	// the converged outcomes stay tolerated: an already-free address and a
	// foreign owner are not failures of the release
	e.controller.releaseOwnClaim(testNetwork, "10.0.0.1", ownRef)

	foreignRef := testNamespace + "/other-vm [02:00:00:00:00:99]"
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", foreignRef); err != nil {
		t.Fatalf("seeding the successor's claim: %s", err)
	}
	e.controller.releaseOwnClaim(testNetwork, "10.0.0.1", ownRef)

	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the successor's claim is never released)", used)
	}
	if v, ok := e.metricValue(metricAppLogs, map[string]string{"loglevel": "error"}); ok && v != 0 {
		t.Errorf("error log metric = %v, want none: the converged releases are not errors", v)
	}
}
