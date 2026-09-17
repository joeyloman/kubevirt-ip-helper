package vmnetcfg

// Regression tests of the two review findings that remained in the
// shared-object race handling of the vm and vmnetcfg controllers:
//
//   - a spec write which lands between the pre-commit verification GET and
//     the commit Update invalidates the verification verdict: the commit
//     fails with a resourceVersion conflict whose failure branch must
//     re-verify and unwind the claimed nics the newer spec removed, or
//     their freshly recreated lease/claim/ledger record survives with no
//     reconciliation ever iterating them again (the vm controller never
//     re-runs its own cleanup after its update succeeded).
//   - a pending ledger unwind whose replay fails again must keep the
//     finalizers of a deleting object: the deletion cleanup iterates only
//     the nics of the present spec, so dropping the pending entry with the
//     finalizers would strand the ledger record until the next process
//     era.

import (
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// TestCommitConflictUnwindsVanishedClaimedNic: the verification GET still
// sees the nic (so the pre-commit barrier passes), then the vm controller's
// spec update lands and the commit Update fails with a resourceVersion
// conflict. The conflict branch must re-verify: the vanished nic's freshly
// allocated lease, claim and ledger record are unwound instead of being
// left behind orphaned (without the re-verification the rollback would
// only quarantine them, because the allocation was uncontested).
func TestCommitConflictUnwindsVanishedClaimedNic(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	// the stale snapshot this sync reads: the nic is still part of the spec
	// and has no address yet, so the sync freshly allocates and claims it
	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	// the competing write lands between the verification GET and the
	// commit: the stored object drops the nic (the vm controller's spec
	// update) and the commit PUT is rejected with a conflict
	e.api.vmnetcfgPutConflict = 1
	e.api.vmnetcfgPutConflictFn = func(obj *kihv1.VirtualMachineNetworkConfig) {
		obj.Spec.NetworkConfig = nil
	}

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("the conflicting commit must surface its error")
	}

	// the re-verification unwound the freshly claimed nic: nothing served,
	// nothing claimed, nothing recorded
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the lease of the vanished nic must be unwound after the conflicting commit")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the claim of the vanished nic must be unwound)", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("pool status allocations = %v, want empty (the ledger record must be unwound)", pool.Status.IPv4.Allocated)
	}

	// the competing spec survived: the vanished nic was not written back
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 0 {
		t.Errorf("the vanished nic must not be committed back into the spec, got %v", stored.Spec.NetworkConfig)
	}
}

// TestDeletionKeepsFinalizersWhilePendingUnwindFails: a deleting object
// carries a pending ledger unwind whose replay fails transiently again.
// The object's spec has no nics left (the nic of the pending unwind was
// removed, which is exactly why the unwind is pending), so the deletion
// cleanup itself would succeed and remove the finalizers - only the guard
// keeps them until the replay converges. Without the guard the pending
// entry is dropped together with the finalizers and the stranded ledger
// record blocks a later binding of the address for the whole era.
func TestDeletionKeepsFinalizersWhilePendingUnwindFails(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	now := metav1.Now()
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = nil
	vmnetcfg.ObjectMeta.DeletionTimestamp = &now
	vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper"}
	e.seedVMNetCfg(vmnetcfg)

	// the pending ledger unwind of the already removed nic: its tuple is
	// not reconstructible from the spec anymore, so the replay is the only
	// path which keeps the record reachable
	e.controller.rememberPendingUnwind(testNamespace+"/"+testVMNetCfgName, pendingLedgerDelete{
		namespace:   testNamespace,
		vmName:      testVMName,
		ip:          "10.0.0.1",
		networkName: testNetwork,
		macAddress:  testMAC,
		poolName:    testPoolName,
	})

	// the replay fails transiently (an api blip on the pool status write)
	e.api.poolStatusPutCode = http.StatusInternalServerError

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err == nil {
		t.Fatal("the deletion must keep the finalizers while the pending unwind cannot converge")
	}

	// the finalizers survived and the pending entry stayed remembered
	stored := e.getStoredVMNetCfg()
	if len(stored.ObjectMeta.Finalizers) == 0 {
		t.Error("the finalizers must survive a failing pending unwind replay")
	}
	e.controller.mutex.Lock()
	pending := len(e.controller.pendingUnwinds[testNamespace+"/"+testVMNetCfgName])
	e.controller.mutex.Unlock()
	if pending != 1 {
		t.Errorf("pending unwind entries = %d, want 1 (the failed replay must be re-remembered)", pending)
	}

	// the api recovers: the retried deletion replays the unwind, converges
	// and completes the cleanup
	e.api.poolStatusPutCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, stored); err != nil {
		t.Fatalf("the recovered deletion must converge: %s", err)
	}
	stored = e.getStoredVMNetCfg()
	if len(stored.ObjectMeta.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed after the converged replay", stored.ObjectMeta.Finalizers)
	}
	e.controller.mutex.Lock()
	pending = len(e.controller.pendingUnwinds[testNamespace+"/"+testVMNetCfgName])
	e.controller.mutex.Unlock()
	if pending != 0 {
		t.Errorf("pending unwind entries after the convergence = %d, want 0", pending)
	}
}
