package vmnetcfg

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// The deleted-lease tuple regression tests: the deleting cleanup deletes
// the lease by mac unconditionally, but a quarantined allocation (a sync
// whose durable object update failed after the lease was already served)
// keeps its claim and its ledger record under a tuple which the present
// spec does not record anymore, and a nic which moved networks keeps its
// pre-move tuple in the lease. The by-mac deletion used to remove the last
// reference to that tuple while its claim and ledger entry survived the
// deletion of the object - orphaning the address for the rest of the era,
// because no reconciliation ever iterates a tuple which neither the spec
// nor any lease records. The deleting cleanup captures the tuple of the
// own live lease before the deletion and releases its reservations through
// the same owner-validated flow.

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
// reservation of a quarantined allocation: the spec tuple of the nic is
// empty (the durable object update failed, so the address never made it
// into the spec), while its lease, claim and ledger record are live. the
// deleting cleanup must release all three layers of the tuple the lease
// served, not only the lease itself.
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
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: testNetwork, MACAddress: testMAC, Status: "OK"}}
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
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 1 || len(final.Spec.NetworkConfig) != 1 || len(final.Status.NetworkConfig) != 0 {
		t.Errorf("foreign post-move spec must block finalization while the old status acknowledges: %#v", final)
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
