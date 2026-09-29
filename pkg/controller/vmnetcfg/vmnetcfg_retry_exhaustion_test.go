package vmnetcfg

import (
	"net/http"
	"testing"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// R03 boundary regression: a force-deleted binding (its finalizers stripped
// externally, its object gone) whose cleanup fails against a failing api is
// retried only through the rate-limited requeue budget of handleErr - no
// object remains which a resync could ever deliver again. once the budget
// is exhausted the key is forgotten and the cleanup is never retried, so
// whatever the failed attempts could not converge stays until the next
// process era rebuilds the ledger: the deleting order releases the dhcp
// lease and the ipam claim before the durable ledger delete, so exactly
// the persisted pool record is stranded and keeps the address reserved
// against every fresh allocation. this pins the current contract; whether
// restart-dependent recovery meets the operational sla is the decision the
// reliability review reserves (R03), not something this test changes.

// TestRetryExhaustedCleanupKeepsCapacityReservedUntilEraRebuild force-deletes
// a served binding while the pool status writes fail, exhausts the retry
// budget through the real queue, then recovers the api without restarting
// the process: the cleanup is not retried (the key was forgotten, the queue
// is empty and no object can produce another event), the stranded ledger
// record keeps the address reserved - a successor binding's allocation is
// rejected as foreign-owned and fully unwound - and only the next era's
// registration revalidation can release the capacity.
func TestRetryExhaustedCleanupKeepsCapacityReservedUntilEraRebuild(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	// the binding converges first: its address is served and recorded
	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the initial allocation must converge: %v", err)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Fatalf("lease = %v, want the served 10.0.0.1", lease.ClientIP)
	}
	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	if owner, ok := storedPoolRecord(e, "10.0.0.1"); !ok || owner != ownerRef {
		t.Fatalf("ledger record = %q (present %v), want the binding's reference %q", owner, ok, ownerRef)
	}

	// the binding is force-deleted (its finalizers stripped externally, the
	// object gone at once) while the pool status writes fail: the tombstone
	// delete event is the only remaining reference to its reservations
	tombstone := e.getStoredVMNetCfg()
	e.api.mu.Lock()
	delete(e.api.vmnetcfgs, testNamespace+"/"+testVMNetCfgName)
	e.api.mu.Unlock()
	e.api.poolStatusPutCode = http.StatusInternalServerError

	key := testNamespace + "/" + testVMNetCfgName
	event := Event{key: key, action: DELETE, vmnetcfg: tombstone}
	cleanupPutBaseline := e.countRequests(http.MethodPut, ippoolStatusPath)

	// drive the requeue budget through the real queue: the initial attempt
	// plus the five rate-limited retries each release the (already gone)
	// lease and claim and fail the durable ledger delete, and the final
	// failure forgets the key
	e.queue.Add(event)
	for i := 0; e.queue.Len() > 0; i++ {
		if i > 10 {
			t.Fatal("the retry budget must be exhausted, not looped")
		}
		if !e.controller.processNextItem() {
			t.Fatal("the queue shut down while the retry budget was being exhausted")
		}
	}

	attempts := e.countRequests(http.MethodPut, ippoolStatusPath) - cleanupPutBaseline
	if attempts != 6 {
		t.Errorf("ledger delete attempts = %d, want 6 (the initial attempt plus five rate-limited retries)", attempts)
	}

	// the exhausted key is forgotten and the queue is empty: no reconciliation
	// of the gone object can ever arrive again
	if n := e.queue.Len(); n != 0 {
		t.Errorf("queue length after the exhausted retries = %d, want 0", n)
	}
	if n := e.queue.NumRequeues(event); n != 0 {
		t.Errorf("requeue count of the dropped key = %d, want 0 (the key was forgotten)", n)
	}

	// the deleting order released the lease and the claim before the failed
	// durable delete: exactly the persisted ledger record is stranded
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the lease must be released by the cleanup attempts")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the claim was released before the ledger delete)", used)
	}
	if owner, ok := storedPoolRecord(e, "10.0.0.1"); !ok || owner != ownerRef {
		t.Errorf("ledger record = %q (present %v), want the stranded record of the deleted binding %q", owner, ok, ownerRef)
	}

	// the api recovers without restarting the process: the cleanup is not
	// retried, because the forgotten key is the only driver it ever had
	e.api.poolStatusPutCode = 0
	if attemptsAfter := e.countRequests(http.MethodPut, ippoolStatusPath) - cleanupPutBaseline; attemptsAfter != attempts {
		t.Errorf("ledger delete attempts after the api recovered = %d, want %d (no retry may run)", attemptsAfter, attempts)
	}

	// the stranded record keeps the capacity reserved: a successor binding's
	// fresh allocation of the released address is rejected as foreign-owned
	// and fully unwound, so nothing can silently take over the deleted
	// binding's durable ownership until the next era rebuilds the ledger
	successor := successorVMNetCfg()
	e.seedVMNetCfg(successor)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, successor); err == nil {
		t.Fatal("the stranded ledger record must reject a successor's allocation of the reserved address")
	}
	if e.dhcp.CheckLease(testMAC2) {
		t.Error("the successor must not serve the address reserved by the stranded record")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the rejected successor's claim is unwound)", used)
	}
	if owner, ok := storedPoolRecord(e, "10.0.0.1"); !ok || owner != ownerRef {
		t.Errorf("ledger record = %q (present %v), want the deleted binding's stranded ownership %q untouched", owner, ok, ownerRef)
	}
}
