package vmnetcfg

// P1.2 regression tests: the vm controller releases the state of a removed
// nic (lease, claim, ledger entry) before its durable spec update lands.
// A vmnetcfg sync which read the object before that update would restore
// the nic's claim into a spec which no longer references it - an orphan
// lease, claim and ledger entry which no reconciliation ever cleans (the
// finalizer iterates only the present spec nics) and which every later
// registration re-pins as authoritative. The post-bind re-verification
// must unwind the recreated state and drop the vanished nic from the
// pending commit.

import (
	"net/http"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// The fresh read sees an owned NIC, then a competing writer removes it
// after allocation. The post-bind fence must unwind the newly served tuple.
func TestVerifyClaimedNicsUnwindsVanishedNic(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	// the stale snapshot this sync reads: the nic is still part of the spec
	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	e.api.poolPutHook = func() {
		e.api.mu.Lock()
		defer e.api.mu.Unlock()
		e.api.poolPutHook = nil
		stored := e.api.vmnetcfgs[testNamespace+"/"+testVMNetCfgName]
		stored.Spec.NetworkConfig = nil
		bumpResourceVersion(stored)
	}

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("the stale owned decision must requeue after unwinding")
	}

	// the recreated state was unwound: nothing served, nothing recorded
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the restored lease of the vanished nic must be unwound")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the restored claim must be unwound)", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("pool status allocations = %v, want empty", pool.Status.IPv4.Allocated)
	}

	// the pending commit must not write the vanished nic back into the spec
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 0 {
		t.Errorf("the vanished nic must not be committed back into the spec, got %v", stored.Spec.NetworkConfig)
	}
}

// TestVerifyClaimedNicsKeepsLiveNic: the live object still carries the nic
// (the normal case), so the sync commits the claim exactly like before.
func TestVerifyClaimedNicsKeepsLiveNic(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if !e.dhcp.CheckLease(testMAC) {
		t.Fatal("lease must exist for a nic which stayed recorded")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1", used)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != "10.0.0.1" {
		t.Errorf("spec = %v, want the allocated address committed", stored.Spec.NetworkConfig)
	}
}

// TestVerifyClaimedNicsKeepsDurableRestore: a durable restore (the spec
// records the address) whose live object still carries the nic is kept
// applied - the verification must not unwind the restored assignment of a
// binding which still exists.
func TestVerifyClaimedNicsKeepsDurableRestore(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if !e.dhcp.CheckLease(testMAC) {
		t.Fatal("the restored lease of a still-recorded nic must stay")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the restored claim must stay)", used)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != "10.0.0.1" {
		t.Errorf("spec = %v, want the durable restore kept", stored.Spec.NetworkConfig)
	}
}

// TestVerifyFailureStillUnwindsContestedClaims: the pre-commit verification
// of the claimed nics fails its re-read, so the pending commit is lost. The
// contested claim of this sync (the pool status records its address for
// another owner) must still be unwound - the retried sync takes the
// lease-based repair path for the nic, which never unwinds a contested
// claim, so skipping the rollback would leave the lease serving an address
// whose ledger record belongs to another owner forever.
func TestVerifyFailureStillUnwindsContestedClaims(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	// the ledger records the reclaimed address for another owner while the
	// allocator is free: the re-claim succeeds, the lease is served and the
	// record write rejects the claim as contested
	e.seedPool(map[string]string{"10.0.0.1": "other-ns/other-vm [02:00:00:00:00:99]"})

	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{IPAddress: "10.0.0.1", MACAddress: testMAC, NetworkName: testNetwork},
		{MACAddress: testMAC2, NetworkName: testNetwork},
	}
	e.seedVMNetCfg(vmnetcfg)

	// Initial fresh read succeeds; verification fails only after binding.
	e.api.poolPutHook = func() {
		e.api.mu.Lock()
		defer e.api.mu.Unlock()
		e.api.poolPutHook = nil
		e.api.vmnetcfgGetCode = http.StatusInternalServerError
	}

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("want the failed verification to fail the sync")
	}

	// the contested claim of the first nic is unwound
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the contested lease must be deleted by the rollback")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (only the quarantined claim of the second nic kept)", used)
	}

	// the uncontested allocation of the second nic stays quarantined
	if !e.dhcp.CheckLease(testMAC2) {
		t.Error("the served lease of the uncontested second nic must stay quarantined")
	}

	// the foreign ledger record was never clobbered, and the quarantined
	// record of the second nic is kept for the retried sync
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated["10.0.0.1"]; got != "other-ns/other-vm [02:00:00:00:00:99]" {
		t.Errorf("allocated[10.0.0.1] = %q, want the foreign owner preserved", got)
	}
	if got := pool.Status.IPv4.Allocated["10.0.0.2"]; got != testNamespace+"/"+testVMName+" ["+testMAC2+"]" {
		t.Errorf("allocated[10.0.0.2] = %q, want the quarantined record kept", got)
	}

	// the failure is pre-commit: the durable object was never written
	if n := e.countRequests("PUT", vmnetcfgMainPath); n != 0 {
		t.Errorf("vmnetcfg updates = %d, want 0", n)
	}
}
