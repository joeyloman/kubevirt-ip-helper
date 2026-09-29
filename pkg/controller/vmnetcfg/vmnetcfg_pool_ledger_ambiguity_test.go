package vmnetcfg

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// R01 regression tests: the pool ledger ADD of a fresh allocation is the
// first durable write of the ownership protocol, and its response can be
// lost while the write actually committed - the ambiguous-commit boundary
// the binding PUT regression already covers for the commit, reproduced
// here for the ledger. the concurrent removal of the binding's nic (the vm
// controller's spec update landing mid-call) must not turn that ambiguity
// into a served address or a stranded reservation: nothing is ACK-eligible
// before the binding commit (F01), so the failed sync fully unwinds the
// unpublished allocation owner-validated, and whatever replay the stale
// tuple still needs can never touch a successor which took the address
// over in the meantime.

const (
	// successorVMNetCfgName and successorVMName identify the second binding
	// of the registered test network: its object name and vm identity are
	// distinct from the first binding's, so its ownership can never be
	// validated by the removed binding's allocation reference
	successorVMNetCfgName = "vm-successor"
	successorVMName       = "vm-successor"
)

// successorVMNetCfg builds the successor binding with an unassigned nic of
// its own mac on the registered test network.
func successorVMNetCfg() *kihv1.VirtualMachineNetworkConfig {
	return &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      successorVMNetCfgName,
		},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName: successorVMName,
			NetworkConfig: []kihv1.NetworkConfig{
				{MACAddress: testMAC2, NetworkName: testNetwork},
			},
		},
	}
}

// successorOwnerRef is the successor's canonical allocation reference.
func successorOwnerRef() string {
	return util.AllocationRef(testNamespace, successorVMName, testMAC2)
}

// firstOwnerRef is the removed binding's canonical allocation reference.
func firstOwnerRef() string {
	return util.AllocationRef(testNamespace, testVMName, testMAC)
}

// storedPoolRecord returns the ledger record of the address, if any.
func storedPoolRecord(e *testEnv, ip string) (string, bool) {
	record, ok := e.getStoredPool().Status.IPv4.Allocated[ip]
	return record, ok
}

// removeNicFromStoredVMNetCfg applies the vm controller's durable spec
// update inside an interleaving hook: the nic disappears from the stored
// object while the controller is still mid-call.
func removeNicFromStoredVMNetCfg(e *testEnv, mac string, network string) {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	obj, ok := e.api.vmnetcfgs[testNamespace+"/"+testVMNetCfgName]
	if !ok {
		return
	}
	kept := obj.Spec.NetworkConfig[:0]
	for _, v := range obj.Spec.NetworkConfig {
		if !(v.MACAddress == mac && v.NetworkName == network) {
			kept = append(kept, v)
		}
	}
	obj.Spec.NetworkConfig = kept
}

// onceInterleave wraps an interleaving hook so it runs exactly once: the
// fake fires the pool hooks after every served request, but the concurrent
// removal happens exactly one time, mid-call. the returned flag observes
// the completion, so the test can wait for the durable removal before it
// replays the retried sync (the hook runs on the fake's server goroutine).
func onceInterleave(fn func()) (hook func(), fired *atomic.Bool) {
	fired = &atomic.Bool{}
	return func() {
		if fired.CompareAndSwap(false, true) {
			fn()
		}
	}, fired
}

// TestCommittedLedgerWriteWithConcurrentNicRemovalNeverServes loses the
// response of a pool ledger ADD which actually committed, while the nic it
// was recorded for is removed durably mid-call: the address of an
// uncommitted binding assignment is never ACK-eligible (the lease
// publication waits for the binding commit), so the rollback must fully
// unwind the ambiguous allocation - release the claim and compensate the
// committed record - instead of serving it or stranding it. the retried
// sync still holds the stale spec copy (the requeue beats the informer
// delivery of the removal), so it re-allocates the address and the
// pre-commit verification must unwind that recreation against the live
// object; the converged address is then served to a successor binding,
// which a further resync of the removed binding must leave untouched.
func TestCommittedLedgerWriteWithConcurrentNicRemovalNeverServes(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	// the ledger ADD commits but its response is lost; while the controller
	// is inside the call, the vm controller's spec update removes the nic
	hook, removalDone := onceInterleave(func() {
		removeNicFromStoredVMNetCfg(e, testMAC, testNetwork)
	})
	e.api.poolPutDropConn = 1
	e.api.poolPutHook = hook

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("the lost ledger response must fail the sync")
	} else if !strings.Contains(err.Error(), "cannot update the IPPool") {
		t.Errorf("sync error = %q, want the ledger rejection", err)
	}

	// the durable removal must land before the retried sync replays
	waitFor(t, func() bool { return removalDone.Load() }, "the mid-call nic removal")

	// the ADD committed (resource version advanced from its seed) and the
	// rollback compensated it with a second committed delete: the record is
	// converged, the claim is released and nothing was ever served
	if rv := e.getStoredPool().ResourceVersion; rv != "3" {
		t.Errorf("stored pool resourceVersion = %q, want 3 (the committed ADD and its compensating delete)", rv)
	}
	if _, still := storedPoolRecord(e, "10.0.0.1"); still {
		t.Error("the committed ledger record must be removed by the compensating delete of the rollback")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 after the unwind", used)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the removed nic's address must never be ACK-eligible")
	}

	// the retried sync still sees the removed nic in its stale spec copy:
	// it re-allocates the address, and the pre-commit verification must
	// unwind the recreation against the live object instead of serving or
	// committing it
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the stale retry must converge against the live object: %v", err)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the stale retry must not publish a lease for the removed nic")
	}
	if _, still := storedPoolRecord(e, "10.0.0.1"); still {
		t.Error("the stale retry must not leave a ledger record of the removed nic")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 after the stale retry unwound its recreation", used)
	}

	// the converged address is served to a successor binding
	successor := successorVMNetCfg()
	e.seedVMNetCfg(successor)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, successor); err != nil {
		t.Fatalf("the successor binding must receive the converged address: %v", err)
	}
	if lease := e.dhcp.GetLease(testMAC2); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("successor lease = %v, want the converged 10.0.0.1", lease.ClientIP)
	}
	if owner, ok := storedPoolRecord(e, "10.0.0.1"); !ok || owner != successorOwnerRef() {
		t.Errorf("ledger record = %q (present %v), want the successor's reference %q", owner, ok, successorOwnerRef())
	}

	// a further resync of the removed binding must not unwind the successor
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("the converged binding must resync cleanly: %v", err)
	}
	if owner, ok := storedPoolRecord(e, "10.0.0.1"); !ok || owner != successorOwnerRef() {
		t.Errorf("ledger record after the resync = %q (present %v), want the successor's ownership untouched", owner, ok)
	}
	if !e.dhcp.CheckLease(testMAC2) {
		t.Error("the successor's lease must stay served across the removed binding's resync")
	}
}

// TestFailedLedgerWriteWithConcurrentNicRemovalSparesSuccessorOwnership
// fails the pool ledger ADD outright (nothing committed) while the nic is
// removed mid-call, and fails the compensating delete of the rollback as
// well: the vanished nic's tuple left the spec, so the unwound record is
// remembered as a pending ledger delete and only the object's own
// reconciliation can replay it. a successor binding takes the converged
// address in the meantime - the replay must recognize the foreign owner,
// converge and never touch the successor's record, lease or claim.
func TestFailedLedgerWriteWithConcurrentNicRemovalSparesSuccessorOwnership(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	// both the ledger ADD and the compensating delete of the rollback fail
	// while the api answers errors; the nic is removed durably mid-call
	hook, removalDone := onceInterleave(func() {
		removeNicFromStoredVMNetCfg(e, testMAC, testNetwork)
	})
	e.api.poolStatusPutCode = http.StatusInternalServerError
	e.api.poolPutHook = hook

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("the failing ledger write must fail the sync")
	}

	waitFor(t, func() bool { return removalDone.Load() }, "the mid-call nic removal")

	// nothing committed: no record exists, the claim is released, nothing
	// was served, and the tuple of the failed compensation stays reachable
	// through the pending unwinds of the object
	if rv := e.getStoredPool().ResourceVersion; rv != "1" {
		t.Errorf("stored pool resourceVersion = %q, want the seed 1 (no ledger write committed)", rv)
	}
	if _, still := storedPoolRecord(e, "10.0.0.1"); still {
		t.Error("no ledger record may exist when the ADD failed outright")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 after the rollback", used)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the removed nic's address must never be ACK-eligible")
	}
	key := testNamespace + "/" + testVMNetCfgName
	e.controller.mutex.Lock()
	pending := len(e.controller.pendingUnwinds[key])
	e.controller.mutex.Unlock()
	if pending != 1 {
		t.Errorf("pending ledger deletes = %d, want 1 (the failed compensation of the vanished nic)", pending)
	}

	// the api recovers and a successor binding takes the converged address
	e.api.poolStatusPutCode = 0
	successor := successorVMNetCfg()
	e.seedVMNetCfg(successor)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, successor); err != nil {
		t.Fatalf("the successor binding must receive the converged address: %v", err)
	}
	if lease := e.dhcp.GetLease(testMAC2); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("successor lease = %v, want the converged 10.0.0.1", lease.ClientIP)
	}

	// the stale replay of the removed binding still sees the removed nic in
	// its spec copy: the pending delete replays first and must converge on
	// the successor's foreign record without touching it, and the stale nic
	// can neither take the successor's claimed address nor unwind it
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the stale replay must converge against the successor's ownership: %v", err)
	}
	e.controller.mutex.Lock()
	pending = len(e.controller.pendingUnwinds[key])
	e.controller.mutex.Unlock()
	if pending != 0 {
		t.Errorf("pending ledger deletes after the replay = %d, want 0 (the foreign owner converged the entry)", pending)
	}
	if owner, ok := storedPoolRecord(e, "10.0.0.1"); !ok || owner != successorOwnerRef() {
		t.Errorf("ledger record after the replay = %q (present %v), want the successor's ownership untouched", owner, ok)
	}
	if !e.dhcp.CheckLease(testMAC2) {
		t.Error("the successor's lease must stay served across the stale replay")
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the stale replay must not publish a lease for the removed nic")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (only the successor's claim)", used)
	}
}

// TestStrandedCommittedLedgerRecordConvergesThroughItsOwnReplay strands a
// committed ledger record whose response was lost while its nic was
// removed mid-call, and fails the compensating delete of the rollback: the
// record stays durable under the vanished binding's owner, so no successor
// can take the address until the object's own reconciliation replays the
// pending delete (the owner matches, the record is removed) - only then
// does the address return to the pool.
func TestStrandedCommittedLedgerRecordConvergesThroughItsOwnReplay(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	// the ledger ADD commits but its response is lost; the nic is removed
	// durably mid-call and the compensating delete of the rollback fails
	hook, removalDone := onceInterleave(func() {
		removeNicFromStoredVMNetCfg(e, testMAC, testNetwork)
	})
	e.api.poolPutDropConn = 1
	e.api.poolStatusPutCode = http.StatusInternalServerError
	e.api.poolPutHook = hook

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("the lost ledger response must fail the sync")
	}

	waitFor(t, func() bool { return removalDone.Load() }, "the mid-call nic removal")

	// the ADD committed and its compensation failed: the record stays
	// stranded under the vanished binding's owner, the claim is released,
	// nothing was served, and the tuple stays reachable through the pending
	// unwinds
	if rv := e.getStoredPool().ResourceVersion; rv != "2" {
		t.Errorf("stored pool resourceVersion = %q, want 2 (the committed ADD only)", rv)
	}
	if owner, ok := storedPoolRecord(e, "10.0.0.1"); !ok || owner != firstOwnerRef() {
		t.Errorf("ledger record = %q (present %v), want the stranded record of the vanished binding %q", owner, ok, firstOwnerRef())
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 after the rollback released the claim", used)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the removed nic's address must never be ACK-eligible")
	}
	key := testNamespace + "/" + testVMNetCfgName
	e.controller.mutex.Lock()
	pending := len(e.controller.pendingUnwinds[key])
	e.controller.mutex.Unlock()
	if pending != 1 {
		t.Errorf("pending ledger deletes = %d, want 1 (the failed compensation of the committed record)", pending)
	}

	// the api recovers, but the stranded record still pins the address: a
	// successor's fresh allocation is rejected as foreign-owned and fully
	// unwound, so the durable record cannot be silently taken over
	e.api.poolStatusPutCode = 0
	successor := successorVMNetCfg()
	e.seedVMNetCfg(successor)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, successor); err == nil {
		t.Fatal("the stranded record must reject a successor's allocation of the pinned address")
	}
	if e.dhcp.CheckLease(testMAC2) {
		t.Error("the successor must not serve the address pinned by the stranded record")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the rejected successor's claim is unwound)", used)
	}

	// the object's own reconciliation replays the pending delete: the owner
	// matches, the stranded record is removed and the entry converges
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 0 {
		t.Fatalf("stored spec = %+v, want the durable nic removal", stored.Spec.NetworkConfig)
	}
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, stored); err != nil {
		t.Fatalf("the replay of the stranded record must converge: %v", err)
	}
	e.controller.mutex.Lock()
	pending = len(e.controller.pendingUnwinds[key])
	e.controller.mutex.Unlock()
	if pending != 0 {
		t.Errorf("pending ledger deletes after the replay = %d, want 0", pending)
	}
	if _, still := storedPoolRecord(e, "10.0.0.1"); still {
		t.Error("the replay must remove the stranded record of the vanished binding")
	}

	// the converged address is served to the successor
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, successor); err != nil {
		t.Fatalf("the successor binding must receive the converged address: %v", err)
	}
	if lease := e.dhcp.GetLease(testMAC2); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("successor lease = %v, want the converged 10.0.0.1", lease.ClientIP)
	}
	if owner, ok := storedPoolRecord(e, "10.0.0.1"); !ok || owner != successorOwnerRef() {
		t.Errorf("ledger record = %q (present %v), want the successor's reference %q", owner, ok, successorOwnerRef())
	}
}
