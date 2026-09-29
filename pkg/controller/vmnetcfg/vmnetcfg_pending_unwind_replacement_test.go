package vmnetcfg

// R02 regression fixtures: the pending ledger unwinds of a deleted binding
// are keyed by the object name only, and the pool ledger records its
// allocations by the name-based owner reference (namespace/vmname
// [macaddress]), so a replacement under the same key which reuses the
// identity of the deleted generation is indistinguishable from it. the
// ordinary reconciliation of the successor replays the recorded unwind of
// the dead generation before its own work (the delete-event replacement
// guard only protects the tombstone replay, not this one), and the fixtures
// prove the three observable outcomes:
//
//   - a successor which reuses the vmname and macaddress of the dead
//     generation keeps its live ledger record: the replay of a dead
//     generation must never destroy the durable ownership of an address the
//     successor serves (a process loss in that window would hand the served
//     address to a competing binding in the next era).
//   - a successor of another vm is never touched by the replay, while the
//     dead generation's own record still converges away - that cleanup is
//     the entire purpose of the pending unwind facility.
//   - a successor of another vm which freshly allocates the freed address
//     is unblocked by the replay of the dead generation's record: the
//     generation validation must not block the address until the next era.

import (
	"net/http"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// deadGenerationEnv builds the R02 starting state: binding default/vm-test
// (uid "generation-a") freshly allocated an address, its binding commit
// failed, and the compensating ledger delete of the rollback failed as well
// (the 500 injection arms itself only after the allocation's own ledger add
// landed), so the controller recorded a pending ledger unwind while the
// ledger record of the dead generation survives. the api then recovers and
// the caller proceeds with the deletion of the binding.
func deadGenerationEnv(t *testing.T, start, end string) (e *testEnv, old *kihv1.VirtualMachineNetworkConfig, oldIP, key string) {
	t.Helper()

	e = newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet(start, end)
	e.seedPool(nil)

	key = testNamespace + "/" + testVMNetCfgName

	old = newVMNetCfg("", testMAC)
	old.ObjectMeta.UID = "generation-a"
	e.seedVMNetCfg(old)

	// the binding commit of the dead generation fails for good, while the
	// pool status api fails only from the compensating delete onwards:
	// the allocation's own ledger add must land so the rollback has a
	// real record whose deletion can fail
	e.api.vmnetcfgPutCode = http.StatusInternalServerError
	e.api.poolPutHook = func() {
		e.api.mu.Lock()
		defer e.api.mu.Unlock()
		e.api.poolStatusPutCode = http.StatusInternalServerError
	}

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, old); err == nil {
		t.Fatal("the failed commit must fail the dead generation's sync")
	}

	// the dead generation's record survived its own compensating unwind
	pool := e.getStoredPool()
	if len(pool.Status.IPv4.Allocated) != 1 {
		t.Fatalf("the ledger must hold exactly the dead generation's record, got %v", pool.Status.IPv4.Allocated)
	}
	for ip := range pool.Status.IPv4.Allocated {
		oldIP = ip
	}
	if owner := pool.Status.IPv4.Allocated[oldIP]; owner != util.AllocationRef(testNamespace, testVMName, testMAC) {
		t.Fatalf("the dead generation's record must name its own owner, got %q", owner)
	}

	// ...and its tuple stayed reachable as a pending unwind: the claim was
	// released by the rollback, nothing is served, only the record and its
	// replay bookkeeping remain
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Fatalf("the rollback must have released the dead generation's claim, used = %d", used)
	}
	if got := pendingUnwindCount(e, key); got != 1 {
		t.Fatalf("the failed compensating delete must be recorded as a pending unwind, got %d entries", got)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Fatal("the dead generation never published a lease, the address was never served")
	}

	// the api recovers: the remaining failures are injected per sync by
	// the tests themselves
	e.api.vmnetcfgPutCode = 0
	e.api.mu.Lock()
	e.api.poolStatusPutCode = 0
	e.api.poolPutHook = nil
	e.api.mu.Unlock()

	return e, old, oldIP, key
}

// pendingUnwindCount reads the number of pending unwind entries recorded
// for a key under the controller mutex.
func pendingUnwindCount(e *testEnv, key string) int {
	e.controller.mutex.Lock()
	defer e.controller.mutex.Unlock()

	return len(e.controller.pendingUnwinds[key])
}

// replaceUnderDeletedKey deletes the dead generation's binding while the
// given replacement already exists under the same key: the informer store
// holds the replacement when the delete event is processed, so the
// delete-event replacement guard skips the unwind drain and the dead
// generation's entry stays recorded for the ordinary reconciliations of
// the successor.
func replaceUnderDeletedKey(t *testing.T, e *testEnv, old *kihv1.VirtualMachineNetworkConfig, replacement *kihv1.VirtualMachineNetworkConfig, key string) {
	t.Helper()

	e.seedVMNetCfg(replacement)
	e.indexer.Add(replacement)

	if err := e.controller.sync(Event{key: key, action: DELETE, vmnetcfg: old.DeepCopy()}); err != nil {
		t.Fatalf("the delete sync of the replaced key failed: %s", err)
	}

	if got := pendingUnwindCount(e, key); got != 1 {
		t.Fatalf("the replacement guard must keep the dead generation's entry resident, got %d", got)
	}
}

// failPoolStatusPutsAfterTheFirst arms the one-shot api blip of the
// successor's reconciliations: the first pool status write of the sync
// still lands, every later one fails, so the replay attempt and the
// successor's own write can be ordered deterministically within one sync.
func failPoolStatusPutsAfterTheFirst(e *testEnv) {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()

	e.api.poolPutHook = func() {
		e.api.mu.Lock()
		defer e.api.mu.Unlock()
		if e.api.poolStatusPutCode == 0 {
			e.api.poolStatusPutCode = http.StatusInternalServerError
		}
	}
}

// recoverPoolStatusPutsAfterTheBlip arms the one-shot api blip of a
// successor sync whose replay of the dead generation's unwind must fail:
// the replay's own pool status write fails, every later write of the same
// sync lands, so the successor still establishes its durable state while
// the dead entry stays pending.
func recoverPoolStatusPutsAfterTheBlip(e *testEnv) {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()

	e.api.poolPutHook = func() {
		e.api.mu.Lock()
		defer e.api.mu.Unlock()
		if e.api.poolStatusPutCode != 0 {
			e.api.poolStatusPutCode = 0
		}
	}
}

// TestReplayedPendingUnwindSparesSameIdentityReplacement: the successor
// under the deleted key reuses the vmname and macaddress of the dead
// generation (the vm was recreated with the same identity), so its ledger
// record is indistinguishable from the dead generation's. its first sync
// restores the recorded address while the replay of the dead generation's
// unwind fails transiently again, so the successor serves the address with
// its durable record in place while the dead entry is still pending; the
// next reconciliation replays the old unwind against the healthy api. the
// replay must not remove the successor's live record.
func TestReplayedPendingUnwindSparesSameIdentityReplacement(t *testing.T) {
	e, old, oldIP, key := deadGenerationEnv(t, "10.0.0.1", "10.0.0.1")

	replacement := newVMNetCfg(oldIP, testMAC)
	replacement.ObjectMeta.UID = "generation-b"
	replaceUnderDeletedKey(t, e, old, replacement, key)

	// the successor's first reconciliation runs while the pool status api
	// would fail the replayed unwind again (the blip which recorded the
	// entry in the first place): the generation validation aborts the
	// replay of the dead generation before any mutation, so the successor
	// establishes its serving state cleanly and its reconciliation does
	// not fail on the dead generation's bookkeeping
	e.api.mu.Lock()
	e.api.poolStatusPutCode = http.StatusInternalServerError
	e.api.mu.Unlock()
	recoverPoolStatusPutsAfterTheBlip(e)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("the successor's reconciliation must not fail on the dead generation's pending unwind: %s", err)
	}

	// the successor is serving its restored address, its durable record is
	// in place, and the dead generation's entry is still pending
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != oldIP || lease.Reference != testNamespace+"/"+testVMName {
		t.Fatalf("the successor must serve its restored address %s, got lease %+v", oldIP, lease)
	}
	if owner, ok := e.getStoredPool().Status.IPv4.Allocated[oldIP]; !ok || owner != util.AllocationRef(testNamespace, testVMName, testMAC) {
		t.Fatalf("the successor's durable record must be in place while the replay is still pending, got %q", owner)
	}
	if got := pendingUnwindCount(e, key); got != 1 {
		t.Fatalf("the transiently failed replay must keep the dead generation's entry recorded, got %d", got)
	}

	// the retried reconciliation (the later repair) runs against the
	// healthy api: the dead generation's unwind replays first, the
	// successor's own ownership repair runs after it
	e.api.mu.Lock()
	e.api.poolStatusPutCode = 0
	e.api.mu.Unlock()
	failPoolStatusPutsAfterTheFirst(e)

	syncErr := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg())

	// the successor's durable ownership must survive the replay of the
	// dead generation's unwind: the ledger record is the only
	// reconstructible ownership evidence of the served address
	if _, ok := e.getStoredPool().Status.IPv4.Allocated[oldIP]; !ok {
		t.Error("the replayed unwind of the dead generation removed the successor's live ledger record while its address stays served: a process loss in this state would hand the served address to a competing binding in the next era")
	}
	if syncErr != nil {
		t.Errorf("the successor's reconciliation must not fail on the dead generation's pending unwind: %v", syncErr)
	}

	// the successor keeps serving exactly as before
	lease = e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != oldIP || lease.Reference != testNamespace+"/"+testVMName {
		t.Errorf("the successor must keep serving its restored address %s, got lease %+v", oldIP, lease)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("the successor's claim must stay held, used = %d", used)
	}

	// the dead generation's entry is not the successor's work: it stays
	// recorded until this key is deleted, and the delete-event drain of
	// the successor replays it against the then-current ledger
	if got := pendingUnwindCount(e, key); got != 1 {
		t.Errorf("the dead generation's entry must stay recorded until the key is deleted, got %d", got)
	}
}

// TestReplayedPendingUnwindConvergesAroundForeignSuccessor: the successor
// belongs to another vm, so its ledger records name another owner and the
// owner-validated replay of the dead generation's entry can never remove
// them. the dead generation's own record must still converge away through
// the replay - blocking it would strand the address until the next era,
// which is exactly what the pending unwind facility exists to prevent.
func TestReplayedPendingUnwindConvergesAroundForeignSuccessor(t *testing.T) {
	e, old, oldIP, key := deadGenerationEnv(t, "10.0.0.1", "10.0.0.2")

	// the true successor requests the other address of the pool under its
	// own identity (the binding key is reused, the vm is not)
	successorIP := "10.0.0.2"
	if oldIP == successorIP {
		successorIP = "10.0.0.1"
	}
	successorRef := util.AllocationRef(testNamespace, "vm-successor", testMAC2)

	replacement := newVMNetCfg(successorIP, testMAC2)
	replacement.Spec.VMName = "vm-successor"
	replacement.ObjectMeta.UID = "generation-b"
	replaceUnderDeletedKey(t, e, old, replacement, key)

	// the successor's first sync establishes its serving state while the
	// replay of the dead generation's unwind fails transiently again
	e.api.mu.Lock()
	e.api.poolStatusPutCode = http.StatusInternalServerError
	e.api.mu.Unlock()
	recoverPoolStatusPutsAfterTheBlip(e)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, e.getStoredVMNetCfg()); err == nil {
		t.Fatal("the transiently failing unwind replay must defer its failure to the retried sync")
	}

	lease := e.dhcp.GetLease(testMAC2)
	if lease.ClientIP == nil || lease.ClientIP.String() != successorIP || lease.Reference != testNamespace+"/vm-successor" {
		t.Fatalf("the successor must serve its own address %s, got lease %+v", successorIP, lease)
	}
	allocated := e.getStoredPool().Status.IPv4.Allocated
	if owner, ok := allocated[successorIP]; !ok || owner != successorRef {
		t.Fatalf("the successor's record must be present under its own owner, got %q", owner)
	}
	if _, ok := allocated[oldIP]; !ok {
		t.Fatalf("the dead generation's record for %s must still be pending its replay", oldIP)
	}
	if got := pendingUnwindCount(e, key); got != 1 {
		t.Fatalf("the transiently failed replay must keep the dead generation's entry recorded, got %d", got)
	}

	// the retried reconciliation runs against the healthy api: the replay
	// converges the dead generation's record, the successor is untouched
	e.api.mu.Lock()
	e.api.poolStatusPutCode = 0
	e.api.mu.Unlock()
	e.api.mu.Lock()
	e.api.poolPutHook = nil
	e.api.mu.Unlock()

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("the successor's reconciliation must converge: %s", err)
	}

	allocated = e.getStoredPool().Status.IPv4.Allocated
	if _, ok := allocated[oldIP]; ok {
		t.Errorf("the dead generation's record for %s must converge away through the replay", oldIP)
	}
	if owner, ok := allocated[successorIP]; !ok || owner != successorRef {
		t.Errorf("the replay must not touch the successor's record, got %q", owner)
	}
	lease = e.dhcp.GetLease(testMAC2)
	if lease.ClientIP == nil || lease.ClientIP.String() != successorIP || lease.Reference != testNamespace+"/vm-successor" {
		t.Errorf("the successor must keep serving its own address, got lease %+v", lease)
	}
	if got := pendingUnwindCount(e, key); got != 0 {
		t.Errorf("the converged entry must be dropped, got %d", got)
	}
}

// TestReplayedPendingUnwindUnblocksForeignSuccessorAddress: the successor
// of another vm freshly allocates the only address of the pool - the very
// address whose dead-generation record is still pending its replay. the
// dead record must not block the true successor: the replay converges it
// first and the successor's allocation of the freed address succeeds in
// the same reconciliation.
func TestReplayedPendingUnwindUnblocksForeignSuccessorAddress(t *testing.T) {
	e, old, oldIP, key := deadGenerationEnv(t, "10.0.0.1", "10.0.0.1")

	successorRef := util.AllocationRef(testNamespace, "vm-successor", testMAC2)

	replacement := newVMNetCfg("", testMAC2)
	replacement.Spec.VMName = "vm-successor"
	replacement.ObjectMeta.UID = "generation-b"
	replaceUnderDeletedKey(t, e, old, replacement, key)

	// the successor's first sync replays the dead generation's unwind
	// while the pool status write fails again, and its fresh allocation of
	// the only free address is rejected by the dead generation's record:
	// nothing is served and nothing is claimed while the record stays
	e.api.mu.Lock()
	e.api.poolStatusPutCode = http.StatusInternalServerError
	e.api.mu.Unlock()
	recoverPoolStatusPutsAfterTheBlip(e)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, e.getStoredVMNetCfg()); err == nil {
		t.Fatal("the sync must fail while the dead generation's record blocks the address and the replay cannot converge it")
	}

	if e.dhcp.CheckLease(testMAC2) {
		t.Error("the successor must not serve an address whose record belongs to the dead generation")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("the contested allocation must be unwound, used = %d", used)
	}
	if owner, ok := e.getStoredPool().Status.IPv4.Allocated[oldIP]; !ok || owner != util.AllocationRef(testNamespace, testVMName, testMAC) {
		t.Fatalf("the dead generation's record must still name its own owner, got %q", owner)
	}
	if got := pendingUnwindCount(e, key); got != 1 {
		t.Fatalf("the transiently failed replay must keep the dead generation's entry recorded, got %d", got)
	}

	// the retried reconciliation runs against the healthy api: the replay
	// converges the dead record first, the successor's fresh allocation of
	// the freed address completes afterwards in the same sync
	e.api.mu.Lock()
	e.api.poolStatusPutCode = 0
	e.api.mu.Unlock()
	e.api.mu.Lock()
	e.api.poolPutHook = nil
	e.api.mu.Unlock()

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("the successor's reconciliation must converge once the dead record is replayed: %s", err)
	}

	lease := e.dhcp.GetLease(testMAC2)
	if lease.ClientIP == nil || lease.ClientIP.String() != oldIP || lease.Reference != testNamespace+"/vm-successor" {
		t.Errorf("the successor must serve the freed address %s under its own identity, got lease %+v", oldIP, lease)
	}
	if owner, ok := e.getStoredPool().Status.IPv4.Allocated[oldIP]; !ok || owner != successorRef {
		t.Errorf("the freed address must be recorded under the successor's owner, got %q", owner)
	}
	if got := pendingUnwindCount(e, key); got != 0 {
		t.Errorf("the converged entry must be dropped, got %d", got)
	}
	if nic := e.getStoredVMNetCfg().Spec.NetworkConfig[0]; nic.IPAddress != oldIP {
		t.Errorf("the successor's committed spec must record the assignment, got %+v", nic)
	}
}
