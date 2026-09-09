package vmnetcfg

// P2-5 regression tests: the existing-lease adoption path works from a
// stale snapshot. The vm and the vmnetcfg controllers run independently,
// so the nic of the reconciled snapshot can be removed concurrently: the
// adoption must not recreate the released ipam reservation for the
// removed nic, and a successor which took the freed address over must
// never be released by the stale cleanup. The interleaving is scheduled
// deterministically at the matching-lease debug message - the window
// between the lease snapshot and the guarded adoption - through the
// existing logging seam, without any production hook.

import (
	"strings"
	"sync/atomic"
	"testing"

	log "github.com/sirupsen/logrus"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// adoptRaceLogHookFunc adapts a function to the logrus hook interface.
type adoptRaceLogHookFunc func(entry *log.Entry) error

func (f adoptRaceLogHookFunc) Levels() []log.Level { return log.AllLevels }

func (f adoptRaceLogHookFunc) Fire(entry *log.Entry) error { return f(entry) }

// adoptRaceHook schedules fn inside the early window of the existing-lease
// path: it fires once at the matching-lease debug message, after the lease
// snapshot was taken but before the guarded adoption runs. The hook is
// installed on the global logger, so tests using it must not run in
// parallel; the previous level and hooks are restored afterwards.
func adoptRaceHook(t *testing.T, fn func()) {
	t.Helper()

	oldLevel := log.GetLevel()
	oldHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))

	var fired atomic.Bool
	log.AddHook(adoptRaceLogHookFunc(func(entry *log.Entry) error {
		// the message check runs first so unrelated log entries cannot
		// consume the one-shot synchronization
		if strings.Contains(entry.Message, "already exists in the leases") && fired.CompareAndSwap(false, true) {
			fn()
		}
		return nil
	}))
	log.SetLevel(log.DebugLevel)

	t.Cleanup(func() {
		log.SetLevel(oldLevel)
		log.StandardLogger().ReplaceHooks(oldHooks)
	})
}

// adoptRaceEnv seeds a fully applied binding in a one-address pool - the
// vmnetcfg nic records the address, the matching dhcp lease, the ipam
// reservation and the pool ownership entry exist - and returns the
// persisted revision of the object, so a concurrent update makes the
// reconciliation's own status write stale (409) like against the real
// apiserver.
func adoptRaceEnv(t *testing.T) (*testEnv, *kihv1.VirtualMachineNetworkConfig) {
	t.Helper()

	e := newTestEnv(t)
	*e.appStatus = APP_RUNNING
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(map[string]string{"10.0.0.1": canonicalLegacy})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", canonicalLegacy); err != nil {
		t.Fatalf("occupying the address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", legacyVMRef); err != nil {
		t.Fatalf("seeding the lease: %s", err)
	}

	vmnetcfg := legacyVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	return e, e.getStoredVMNetCfg()
}

// TestAdoptionDoesNotRecreateTheClaimAfterConcurrentCleanup: the vm
// controller cleanup of the removed nic completes between the lease
// snapshot and the adoption - the pre-fix code adopted the released
// address back into ipam, leaking a reservation no reconciliation could
// release anymore. The guarded adoption must refuse to run without the
// live lease, nothing of the removed nic may survive, and the freed
// address must be available to the next legitimate vm.
func TestAdoptionDoesNotRecreateTheClaimAfterConcurrentCleanup(t *testing.T) {
	e, vmnetcfg := adoptRaceEnv(t)

	// the concurrent cleanup completes inside the early window: the lease
	// and the reservation are released, the ownership record is removed,
	// the nic is gone from the persisted spec and the resource versions
	// advanced
	adoptRaceHook(t, func() {
		_ = e.dhcp.DeleteLeaseOwnedBy(testMAC, legacyVMRef)
		_ = e.ipam.ReleaseIP(testNetwork, "10.0.0.1")

		e.api.mu.Lock()
		e.api.ippools[testPoolName].Status.IPv4.Allocated = map[string]string{}
		bumpResourceVersion(e.api.ippools[testPoolName])
		stored := e.api.vmnetcfgs[testNamespace+"/"+testVMNetCfgName]
		stored.Spec.NetworkConfig = nil
		bumpResourceVersion(stored)
		e.api.mu.Unlock()
	})

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the raced reconciliation must converge: %s", err)
	}

	// nothing of the removed nic survives
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the removed nic must not hold a lease")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the recreated reservation must not exist)", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("pool status = %v, want no stale record", pool.Status.IPv4.Allocated)
	}

	// the stale status write was rejected: the concurrently removed nic
	// was not resurrected in the persisted spec
	if stored := e.getStoredVMNetCfg(); len(stored.Spec.NetworkConfig) != 0 {
		t.Errorf("spec networkconfig = %v, want the nic to stay removed", stored.Spec.NetworkConfig)
	}

	// the address is available to the next legitimate vm
	fresh := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "vm-fresh"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm-fresh"},
	}
	fresh.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC2, NetworkName: testNetwork},
	}
	e.seedVMNetCfg(fresh)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, fresh); err != nil {
		t.Fatalf("the next vm must receive the freed address: %s", err)
	}
	if got := e.dhcp.GetLease(testMAC2).ClientIP.String(); got != "10.0.0.1" {
		t.Errorf("the next vm lease ip = %q, want the freed 10.0.0.1", got)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (only the next vm's claim)", used)
	}
}

// TestStaleCleanupDoesNotReleaseASuccessor: the concurrent cleanup removes
// the lease and releases the address, and a successor vm already took the
// freed address over before the stale reconciliation resumes. Neither the
// adoption nor the compensating release may touch the successor's
// allocation.
func TestStaleCleanupDoesNotReleaseASuccessor(t *testing.T) {
	e, vmnetcfg := adoptRaceEnv(t)

	const successorRef = testNamespace + "/vm-fresh [" + testMAC2 + "]"

	adoptRaceHook(t, func() {
		_ = e.dhcp.DeleteLeaseOwnedBy(testMAC, legacyVMRef)
		_ = e.ipam.ReleaseIP(testNetwork, "10.0.0.1")

		// the successor's binding allocates the freed address (an
		// anonymous fresh allocation), serves its lease and records its
		// ownership
		if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
			t.Errorf("the successor taking the freed address: %s", err)
		}
		if err := e.dhcp.AddLease(testMAC2, testNetwork, "10.0.0.1", testNamespace+"/vm-fresh"); err != nil {
			t.Errorf("seeding the successor lease: %s", err)
		}

		e.api.mu.Lock()
		e.api.ippools[testPoolName].Status.IPv4.Allocated = map[string]string{"10.0.0.1": successorRef}
		e.api.mu.Unlock()
	})

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the raced reconciliation must converge: %s", err)
	}

	// the successor keeps its allocation: the stale cleanup neither
	// adopted nor released it
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the successor's claim)", used)
	}
	if !e.dhcp.CheckLease(testMAC2) {
		t.Error("the successor must keep its lease")
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != successorRef {
		t.Errorf("pool record = %q, want the successor's ownership preserved", got)
	}

	// and the removed nic's lease stays absent
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the removed nic must not hold a lease")
	}

	// the successor's own reconciliation re-establishes its claim under
	// its named identity: the stale cleanup must have left the anonymous
	// allocation untouched instead of promoting or releasing it, so this
	// sync stays idempotent instead of hitting a foreign owner
	successor := newVMNetCfg("10.0.0.1", testMAC2)
	successor.ObjectMeta.Name = "vm-fresh"
	successor.Spec.VMName = "vm-fresh"
	successor.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC2, NetworkName: testNetwork, Status: "OK", Message: "IP address successfully allocated"},
	}
	e.seedVMNetCfg(successor)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, successor); err != nil {
		t.Fatalf("the successor's own reconciliation must keep its address: %s", err)
	}
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", successorRef); err != nil {
		t.Errorf("the successor's named reclaim after its reconciliation: %s", err)
	}
}

// TestGuardedAdoptionStillPromotesAnAnonymousAllocation: without a
// concurrent cleanup the guarded adoption behaves exactly as before - a
// live lease owned by this binding justifies the promotion of an earlier
// anonymous allocation to the named owner, and the repair write rebuilds
// the ownership record.
func TestGuardedAdoptionStillPromotesAnAnonymousAllocation(t *testing.T) {
	e := newTestEnv(t)
	*e.appStatus = APP_RUNNING
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	// the anonymous allocation of a failed earlier sync plus the live
	// owned lease it left behind
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
		t.Fatalf("seeding the anonymous allocation: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", legacyVMRef); err != nil {
		t.Fatalf("seeding the lease: %s", err)
	}

	vmnetcfg := legacyVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the promotion reconcile must succeed: %s", err)
	}

	// the anonymous allocation is promoted to the named owner and the
	// record is rebuilt
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", canonicalLegacy); err != nil {
		t.Errorf("the own reclaim after the promotion: %s", err)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != canonicalLegacy {
		t.Errorf("pool record = %q, want the promoted owner record", got)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("the binding keeps serving")
	}
}
