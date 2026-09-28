package vmnetcfg

import (
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// The deleted-lease tuple regression tests: the deleting cleanup deletes
// the lease by mac unconditionally, but a divergent lease (a lease whose
// served tuple the present spec does not record anymore - a nic which
// moved networks keeps its pre-move tuple in the lease) can hold a claim
// and a ledger record under that very tuple. The by-mac deletion used to
// remove the last reference to that tuple while its claim and ledger
// entry survived the deletion of the object - orphaning the address for
// the rest of the era, because no reconciliation ever iterates a tuple
// which neither the spec nor any lease records. The deleting cleanup
// captures the tuple of the own live lease before the deletion and
// releases its reservations through the same owner-validated flow.

// newDeletingVMNetCfg builds a controller-managed binding which the
// apiserver marked for deletion.
func newDeletingVMNetCfg(netCfgs []kihv1.NetworkConfig) *kihv1.VirtualMachineNetworkConfig {
	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  testNamespace,
			Name:       testVMNetCfgName,
			UID:        "1111-2222-3333",
			Finalizers: []string{vmnetcfgCleanupFinalizer},
		},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        testVMName,
			NetworkConfig: netCfgs,
		},
	}
	now := metav1.Now()
	vmnetcfg.ObjectMeta.DeletionTimestamp = &now

	return vmnetcfg
}

// TestVMNetCfgDeletionReleasesTheQuarantinedAllocation pins the orphaned
// reservation of a divergent lease: the spec tuple of the nic is empty
// while its lease, claim and ledger record are live (the address the
// lease serves never made it into the spec). the deleting cleanup must
// release all three layers of the tuple the lease served, not only the
// lease itself.
func TestVMNetCfgDeletionReleasesTheQuarantinedAllocation(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	// the quarantined state: a served lease, a claimed address and a
	// persisted ledger record, none of which the spec records
	e.seedPool(map[string]string{"10.0.0.2": ownerRef})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", ownerRef); err != nil {
		t.Fatalf("seeding the quarantined claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.2", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the quarantined lease: %s", err)
	}

	// the spec tuple is empty: the allocation never became durable
	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "", MACAddress: testMAC, NetworkName: testNetwork},
	})
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the deleting cleanup of the quarantined allocation failed: %s", err)
	}

	// every layer of the tuple the lease served is released
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the lease of the quarantined allocation must be released")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want the quarantined claim released", used)
	}
	pool := e.getStoredPool()
	if got, still := pool.Status.IPv4.Allocated["10.0.0.2"]; still {
		t.Errorf("the ledger record of the quarantined allocation survived the deletion: %q", got)
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed so the object can be deleted", final.Finalizers)
	}
}

// TestVMNetCfgDeletionReleasesThePreMoveTuple pins the same gap for a nic
// which moved networks: the spec records the new tuple, the lease still
// serves the pre-move tuple of the old network. the deleting cleanup must
// release the old network's claim and ledger record as well, or the old
// address stays allocated to a deleted binding for the rest of the era.
func TestVMNetCfgDeletionReleasesThePreMoveTuple(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	// the pre-move state on the old network: lease, claim, ledger record
	e.seedPool(map[string]string{"10.0.0.2": ownerRef})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", ownerRef); err != nil {
		t.Fatalf("seeding the pre-move claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.2", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the pre-move lease: %s", err)
	}

	// the spec records the post-move tuple of another network
	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "10.0.1.5", MACAddress: testMAC, NetworkName: "net-moved"},
	})
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the deleting cleanup of the moved nic failed: %s", err)
	}

	if e.dhcp.CheckLease(testMAC) {
		t.Error("the pre-move lease must be released")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used of the old network = %d, want the pre-move claim released", used)
	}
	pool := e.getStoredPool()
	if got, still := pool.Status.IPv4.Allocated["10.0.0.2"]; still {
		t.Errorf("the ledger record of the old network survived the deletion: %q", got)
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed so the object can be deleted", final.Finalizers)
	}
}

// a foreign lease is never captured: its tuple belongs to another owner,
// so the deleting cleanup leaves its reservations to that owner's own
// reconciliation instead of releasing them.
func TestVMNetCfgDeletionLeavesTheForeignLeaseTuple(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	foreignRef := util.AllocationRef(testNamespace, "other-vm", testMAC2)
	e.seedPool(map[string]string{"10.0.0.2": foreignRef})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", foreignRef); err != nil {
		t.Fatalf("seeding the foreign claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.2", testNamespace+"/other-vm"); err != nil {
		t.Fatalf("seeding the foreign lease: %s", err)
	}

	// the spec tuple is empty and the mac's lease is foreign: nothing of
	// the foreign tuple may be released
	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "", MACAddress: testMAC, NetworkName: testNetwork},
	})
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the deleting cleanup failed: %s", err)
	}

	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1: the foreign claim must stay with its owner", used)
	}
	pool := e.getStoredPool()
	if got, still := pool.Status.IPv4.Allocated["10.0.0.2"]; !still || got != foreignRef {
		t.Errorf("allocated[10.0.0.2] = %q (present %v), want the foreign record untouched", got, still)
	}
}

// TestVMNetCfgDivergentTupleDeletionRetriesUntilTheRecordConverges pins
// the F03 gap: the deleting cleanup of a divergent tuple releases the
// lease and the claim before it removes the tuple's ledger record, and
// the captured tuple is unreconstructible once the by-mac lease deletion
// ran - neither the spec nor any lease records it anymore. a transient
// failure of that record removal must therefore stay reachable: the
// failure records the captured tuple as a pending unwind, the retried
// deletion replays the owner-validated record removal before it removes
// the finalizers (the deletion guard), and the record never survives the
// deletion of the object. the pre-fix retry finalized here with the
// record stranded, blocking a later binding of the address for the era
// (a process loss between the attempts is covered by the next era's
// pool registration, which drops the record of a positively removed
// binding - pinned by the ippool recovery tests).
func TestVMNetCfgDivergentTupleDeletionRetriesUntilTheRecordConverges(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	e.seedPool(map[string]string{"10.0.0.2": ownerRef})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", ownerRef); err != nil {
		t.Fatalf("seeding the quarantined claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.2", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the quarantined lease: %s", err)
	}

	// the spec tuple is empty: the recorded removal must converge through
	// the pending unwind, because no spec entry can reach it anymore
	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "", MACAddress: testMAC, NetworkName: testNetwork},
	})
	e.seedVMNetCfg(vmnetcfg)

	// the record removal of the captured tuple fails transiently
	e.api.poolStatusPutCode = http.StatusInternalServerError
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err == nil {
		t.Fatal("the failed record removal must fail the deletion")
	}

	// the destructive half of the cleanup already ran: the tuple is only
	// reachable through the recorded pending unwind now
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the lease must be released by the first deletion attempt")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want the claim released by the first attempt", used)
	}
	if _, still := e.getStoredPool().Status.IPv4.Allocated["10.0.0.2"]; !still {
		t.Fatal("the ledger record of the captured tuple must survive the failed removal")
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 1 {
		t.Fatalf("finalizers = %v, want kept until the record removal converges", final.Finalizers)
	}

	// the retry converges the record removal before it finalizes
	e.api.poolStatusPutCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("the retried deletion must converge: %s", err)
	}
	pool := e.getStoredPool()
	if got, still := pool.Status.IPv4.Allocated["10.0.0.2"]; still {
		t.Errorf("the ledger record survived the retried deletion: %q", got)
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed after the record converged", final.Finalizers)
	}
}

// TestVMNetCfgDivergentTupleUnwindNeverTouchesTheSuccessorClaim: after
// the failed record removal released the captured tuple's claim, a
// successor may take the address over while the stale record still names
// the deleting binding. the replayed removal is owner-validated on the
// ledger layer alone: it removes only the stale record of this binding,
// never the successor's claim.
func TestVMNetCfgDivergentTupleUnwindNeverTouchesTheSuccessorClaim(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	e.seedPool(map[string]string{"10.0.0.2": ownerRef})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", ownerRef); err != nil {
		t.Fatalf("seeding the quarantined claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.2", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the quarantined lease: %s", err)
	}

	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "", MACAddress: testMAC, NetworkName: testNetwork},
	})
	e.seedVMNetCfg(vmnetcfg)

	// the first attempt releases the live state and fails the record
	e.api.poolStatusPutCode = http.StatusInternalServerError
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err == nil {
		t.Fatal("the failed record removal must fail the deletion")
	}

	// a successor takes the released address over while the stale record
	// still names the deleting binding (its own ledger write would be
	// rejected as foreign - the stale record blocks it until the replay)
	successorRef := util.AllocationRef("other-ns", "other-vm", testMAC2)
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", successorRef); err != nil {
		t.Fatalf("seeding the successor claim: %s", err)
	}

	e.api.poolStatusPutCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("the retried deletion must converge: %s", err)
	}

	// only the stale record went away; the successor keeps the claim
	pool := e.getStoredPool()
	if got, still := pool.Status.IPv4.Allocated["10.0.0.2"]; still {
		t.Errorf("the stale ledger record survived the retried deletion: %q", got)
	}
	if ip, found := e.ipam.IPOwnedBy(testNetwork, successorRef); !found || ip != "10.0.0.2" {
		t.Errorf("successor claim = %s (found %v), want the untouched 10.0.0.2", ip, found)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1: only the successor's claim remains", used)
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed after the record converged", final.Finalizers)
	}
}

// TestVMNetCfgDivergentTupleUnwindConvergesWhenThePoolDied: a captured
// tuple whose pool could not be resolved at record time (the api was
// unreachable while the recursive cleanup failed) is recorded without a
// pool name. when the pool object dies before the replay - it takes its
// whole status ledger with it - the recorded removal is converged
// without any write: the replay must classify it through the api and
// release the finalizers instead of pinning them on a pending entry
// whose record does not exist anymore (a resolution which cannot tell a
// dead pool from an unreachable api would pin until the next process
// era).
func TestVMNetCfgDivergentTupleUnwindConvergesWhenThePoolDied(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	e.seedPool(map[string]string{"10.0.0.2": ownerRef})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", ownerRef); err != nil {
		t.Fatalf("seeding the quarantined claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.2", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the quarantined lease: %s", err)
	}

	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "", MACAddress: testMAC, NetworkName: testNetwork},
	})
	e.seedVMNetCfg(vmnetcfg)

	// the pool object exists but is not cached, and the api list fails:
	// the deleting cleanup releases the live state of the captured
	// tuple, cannot verify the pool and records the tuple without a
	// pool name
	if err := e.cache.Delete("pool", testNetwork); err != nil {
		t.Fatalf("uncaching the pool: %s", err)
	}
	e.api.ippoolListCode = http.StatusInternalServerError
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err == nil {
		t.Fatal("the unverifiable pool must fail the deletion")
	}
	if _, still := e.getStoredPool().Status.IPv4.Allocated["10.0.0.2"]; !still {
		t.Fatal("the ledger record must survive the failed removal")
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 1 {
		t.Fatalf("finalizers = %v, want kept until the record removal converges", final.Finalizers)
	}

	// the pool object dies before the retry and the api recovers: its
	// ledger is gone with it
	e.api.mu.Lock()
	delete(e.api.ippools, testPoolName)
	e.api.mu.Unlock()
	e.api.ippoolListCode = 0

	// the replay classifies the pool as verifiably gone and the deletion
	// finalizes
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("the retried deletion must converge after the pool died: %s", err)
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed: the pool took its ledger with it", final.Finalizers)
	}
}

// TestVMNetCfgDivergentTupleUnwindProceedsAgainstTheUnregisteredPool:
// the recorded tuple of a divergent cleanup is replayed against the
// api-resolved pool object even while the pool is not cached (F04):
// durable cleanup must not wait for the serving registration, because
// the registration can be blocked by the very record the replay removes
// (an exclude entry conflicting with the recorded claim). the write
// recomputes the counters from the live allocator - the subnet of this
// network is registered, only the pool object is not - so the persisted
// status converges to the serving state instead of being pinned on a
// fail-closed entry.
func TestVMNetCfgDivergentTupleUnwindProceedsAgainstTheUnregisteredPool(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	e.seedPool(map[string]string{"10.0.0.2": ownerRef})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", ownerRef); err != nil {
		t.Fatalf("seeding the quarantined claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.2", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the quarantined lease: %s", err)
	}

	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "", MACAddress: testMAC, NetworkName: testNetwork},
	})
	e.seedVMNetCfg(vmnetcfg)

	// the pool object exists but is not cached, and the api list fails:
	// the first attempt records the tuple without a pool name
	if err := e.cache.Delete("pool", testNetwork); err != nil {
		t.Fatalf("uncaching the pool: %s", err)
	}
	e.api.ippoolListCode = http.StatusInternalServerError
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err == nil {
		t.Fatal("the unverifiable pool must fail the deletion")
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 1 {
		t.Fatalf("finalizers = %v, want kept until the replay converges", final.Finalizers)
	}

	// the api recovers while the pool still exists uncached: the replay
	// resolves the pool name through the api list and removes the record
	// without any registration of the pool
	e.api.ippoolListCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("the replay must proceed against the unregistered pool: %s", err)
	}
	pool := e.getStoredPool()
	if got, still := pool.Status.IPv4.Allocated["10.0.0.2"]; still {
		t.Errorf("the ledger record survived the replay: %q", got)
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed after the replay converged", final.Finalizers)
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0: the claim was released by the first attempt", used)
	}
	if got := pool.Status.IPv4.Used; got != 0 {
		t.Errorf("Used = %d, want 0 recomputed from the live allocator of the registered subnet", got)
	}
}
