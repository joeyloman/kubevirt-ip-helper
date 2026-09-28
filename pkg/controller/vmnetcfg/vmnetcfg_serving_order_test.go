package vmnetcfg

// F01 regression tests: the lease publication of a fresh allocation must
// wait for the complete durable ownership protocol. the pool ledger record
// (the durable ownership) is written first, the binding commit records the
// matching spec assignment, and only then does the deferred AddLease make
// the address ACK-eligible - the dhcp server answers a request purely from
// its in-memory lease map, so a lease which does not exist cannot produce
// an ACK. a guest can therefore never receive an address which a process
// loss would leave unreconstructible: before the commit nothing was served
// (so releasing is duplicate-safe), and after the commit both durable
// records exist, which the pool registration rebuilds the reservation from
// (see TestRegistrationKeepsTheLedgerRecordOfAReconstructibleOwner and
// TestRegistrationDropsTheLedgerRecordOfAPositivelyRemovedOwner in
import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// waitFor polls the condition until it holds or the deadline expires.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(2 * time.Millisecond)
	}

	t.Fatalf("condition not reached within the deadline: %s", what)
}

// TestFreshAllocationWaitsForTheDurableCommit parks the binding commit of a
// fresh allocation after its pool ledger record was already written: while
// the commit is parked, the durable ownership exists but the stored spec
// does not record the assignment yet, and the address must not be
// ACK-eligible. this pins the F01 ordering against both the old publish
// order (lease before the ledger write) and the naive swap (lease after the
// ledger write but before the commit): serving eligibility waits for the
// full protocol, because a crash before the commit would otherwise free the
// address through the recovery while the guest keeps it.
func TestFreshAllocationWaitsForTheDurableCommit(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	block := make(chan struct{})
	e.api.blockVMNetCfgPut = block

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	done := make(chan error, 1)
	go func() {
		done <- e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	}()

	// wait until the durable ownership record of the fresh allocation is
	// written: from that moment the sync can only be at (or behind) the
	// verification and the parked commit
	waitFor(t, func() bool {
		return e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"] != ""
	}, "the pool ledger record of the fresh allocation")

	// the ledger record exists, but the assignment is not durable yet:
	// nothing may be ACK-eligible
	if e.dhcp.CheckLease(testMAC) {
		t.Fatal("the lease must not exist while the binding commit is parked")
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
		t.Errorf("ledger record = %q, want the owner reference of this binding", got)
	}

	// release the commit: the assignment becomes durable and exactly then
	// the deferred publication serves the address
	close(block)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("parked sync: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked sync did not finish after the commit was released")
	}

	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("lease = %v, want the published 10.0.0.1 lease", lease.ClientIP)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != "10.0.0.1" {
		t.Errorf("stored spec = %+v, want the committed assignment", stored.Spec.NetworkConfig)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1", used)
	}
}

// TestParkedLedgerWriteKeepsTheAddressUnserved fails the pool ledger write
// of a fresh allocation: without the durable ownership record the lease is
// never published and the reservation is released by the rollback, so no
// address can be ACKed which no durable object records. the retry converges
// once the write works again.
func TestParkedLedgerWriteKeepsTheAddressUnserved(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	e.api.poolStatusPutCode = http.StatusInternalServerError

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the pool ledger failure to fail the sync")
	}
	if !strings.Contains(err.Error(), "cannot update the IPPool") {
		t.Errorf("error = %q, want the ledger rejection", err)
	}

	// nothing was served and nothing was recorded: the claim is released
	// and no lease exists
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must exist while the durable ownership is missing")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 after the rollback", used)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != "" {
		t.Errorf("ledger record = %q, want none (the write failed)", got)
	}

	// the retry converges once the ledger write works again
	e.api.poolStatusPutCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("retried sync: %v", err)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("lease = %v, want the published 10.0.0.1 lease after the retry", lease.ClientIP)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != "10.0.0.1" {
		t.Errorf("stored spec = %+v, want the committed assignment after the retry", stored.Spec.NetworkConfig)
	}
}

// TestUnreadableCommitOutcomeKeepsReservationsUnserved fails the binding
// commit and the authoritative reread alike: the controller cannot know
// whether the write landed, so the reservations of the sync stay held
// without serving anything - a write which landed must never be destroyed.
// the claim and the record of the unresolved assignment are bounded by the
// era (the registration sweep revalidates the ledger), and the retried sync
// converges on a fresh address while the held reservation keeps protecting
// the possibly committed one.
func TestUnreadableCommitOutcomeKeepsReservationsUnserved(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)
	e.api.vmnetcfgPutCode = http.StatusInternalServerError
	// the pre-commit verification GET is served, the reread of the failed
	// commit fails
	e.api.vmnetcfgGetFailFrom = 2

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the failed commit to fail the sync")
	}
	if !strings.Contains(err.Error(), "cannot update VirtualMachineNetworkConfig object") {
		t.Errorf("error = %q, want the commit failure", err)
	}

	// the outcome is unreadable: nothing is served, but the claim and the
	// ledger record of the possibly committed assignment stay held. the
	// allocator hands out the first free address of the pool range, whose
	// map iteration order is unspecified, so the held address is read from
	// the ledger instead of being assumed
	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must exist while the commit outcome is unreadable")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the held reservation of the possibly committed assignment)", used)
	}
	pool := e.getStoredPool()
	if len(pool.Status.IPv4.Allocated) != 1 {
		t.Fatalf("ledger records = %v, want exactly the held record of the possibly committed assignment", pool.Status.IPv4.Allocated)
	}
	heldIP := ""
	for ip := range pool.Status.IPv4.Allocated {
		heldIP = ip
	}
	if got := pool.Status.IPv4.Allocated[heldIP]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
		t.Errorf("held ledger record = %q, want the owner reference of this binding", got)
	}

	// once the API answers again the retried sync converges on the other
	// address while the held reservation keeps protecting the possibly
	// committed one
	e.api.vmnetcfgPutCode = 0
	e.api.vmnetcfgGetFailFrom = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("retried sync: %v", err)
	}

	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() == heldIP {
		t.Fatalf("lease = %v, want the freshly allocated other address after the retry", lease.ClientIP)
	}
	freshIP := lease.ClientIP.String()
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress != freshIP {
		t.Errorf("stored spec = %+v, want the second address committed after the retry", stored.Spec.NetworkConfig)
	}
	if used := e.ipam.Used(testNetwork); used != 2 {
		t.Errorf("ipam used = %d, want 2 (the new claim plus the held reservation)", used)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated[heldIP]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
		t.Errorf("ledger record of the held reservation = %q, want it kept for the rest of the era", got)
	}
}

// TestGoneBindingUnwindsItsUncommittedAllocation removes the binding while
// its commit fails: the reread proves definitively that no assignment of
// this sync can be recorded anymore (whatever landed was released by the
// deletion cleanup), so the unpublished allocation is fully unwound instead
// of leaking its claim and record with no object left to reconcile.
func TestGoneBindingUnwindsItsUncommittedAllocation(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)
	e.api.vmnetcfgPutCode = http.StatusInternalServerError
	e.api.vmnetcfgPutHook = func() {
		e.api.mu.Lock()
		delete(e.api.vmnetcfgs, testNamespace+"/"+testVMNetCfgName)
		e.api.mu.Unlock()
	}

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the failed commit to fail the sync")
	}

	if e.dhcp.CheckLease(testMAC) {
		t.Error("no lease must exist for the gone binding")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 after the unwind", used)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != "" {
		t.Errorf("ledger record = %q, want the unwound record removed", got)
	}
}

// TestCommittedAllocationProtectsTheAddressAgainstCompetingBindings proves
// the protection leg of the F01 regression: once a binding's assignment is
// committed and published, both durable records exist, so a competing
// binding which reconciles first can never receive the served address -
// within the era the claim holds it, and across eras the registration sweep
// re-pins it from the ledger record and the spec entry.
func TestCommittedAllocationProtectsTheAddressAgainstCompetingBindings(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	// the first binding commits and publishes its assignment; the
	// allocator hands out the first free address of the pool range, whose
	// map iteration order is unspecified, so the served address is read
	// from the lease instead of being assumed
	first := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(first)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, first); err != nil {
		t.Fatalf("first binding: %v", err)
	}
	firstLease := e.dhcp.GetLease(testMAC)
	if firstLease.ClientIP == nil {
		t.Fatalf("first lease = %v, want a published lease", firstLease.ClientIP)
	}
	servedIP := firstLease.ClientIP.String()
	otherIP := "10.0.0.1"
	if servedIP == "10.0.0.1" {
		otherIP = "10.0.0.2"
	}

	// a competing binding of another vm reconciles: the served address is
	// claimed and recorded, so the competitor must receive the other one
	second := newVMNetCfg("", testMAC2)
	second.Name = testVMNetCfgName + "-other"
	second.Spec.VMName = testVMName + "-other"
	e.seedVMNetCfg(second)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, second); err != nil {
		t.Fatalf("second binding: %v", err)
	}

	if lease := e.dhcp.GetLease(testMAC2); lease.ClientIP == nil || lease.ClientIP.String() != otherIP {
		t.Errorf("second lease = %v, want the free %s, never the served %s", lease.ClientIP, otherIP, servedIP)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != servedIP {
		t.Errorf("first lease = %v, want %s preserved", lease.ClientIP, servedIP)
	}
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated[servedIP]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
		t.Errorf("ledger record of the served address = %q, want the first binding's owner", got)
	}
	if got := pool.Status.IPv4.Allocated[otherIP]; got != testNamespace+"/"+testVMName+"-other ["+testMAC2+"]" {
		t.Errorf("ledger record of the second address = %q, want the second binding's owner", got)
	}
}
