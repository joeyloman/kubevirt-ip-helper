package vmnetcfg

// F04 regression tests: finalization must not depend on the very pool
// registration that the allocation blocks. a pool object can exist
// without a cache entry - its spec is unregistrable, or its registration
// is definitively rejected because an exclude entry conflicts with the
// persisted record of a live binding: the registration requires the
// release of that claim, while the binding's finalization used to
// require the registration (the cleanup refused to un-record through
// anything but the cache, so both objects pinned each other forever).
// the durable cleanup now resolves the pool object through the api and
// runs the owner-validated ledger removal against it directly: the
// terminating binding converges, its record stops blocking the
// registration, and the pool can register once the conflicting claim is
// gone (pinned on the ippool side). the write preserves the counters of
// a network the allocator does not know, so it cannot corrupt the
// durable status of the unregistered pool.

import (
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// seedUnregisteredPool registers a pool object with the fake api only -
// no cache entry and no subnet, the shape of a pool whose registration
// is blocked - carrying the given ledger records, an exclude entry which
// conflicts with the binding's own recorded address, and serving-state
// counters which the cleanup write must preserve.
func seedUnregisteredPool(t *testing.T, e *testEnv, allocated map[string]string) *kihv1.IPPool {
	t.Helper()

	pool := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: testPoolName},
		Spec: kihv1.IPPoolSpec{
			NetworkName: testNetwork,
			IPv4Config: kihv1.IPv4Config{
				ServerIP: "10.0.0.1",
				Subnet:   testSubnet,
				Pool: kihv1.Pool{
					Start:   "10.0.0.1",
					End:     "10.0.0.6",
					Exclude: []string{"10.0.0.2"},
				},
			},
		},
		Status: kihv1.IPPoolStatus{
			IPv4: kihv1.IPv4Status{Allocated: allocated, Used: 3, Available: 2},
		},
	}
	e.api.seedPool(pool)

	return pool
}

// TestVMNetCfgDeletionFinalizesAgainstAnUnregisteredPool is the review's
// deadlock fixture: the terminating binding holds the record of the very
// address the pool's exclude entry claims, so the registration rejects
// definitively while the binding cannot finalize. without repairing the
// pool spec and without its registration, the deletion must converge:
// the record is removed through the api-resolved pool object and the
// finalizers come off. the pre-fix cleanup failed on the missing cache
// entry and pinned both objects forever.
func TestVMNetCfgDeletionFinalizesAgainstAnUnregisteredPool(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	seedUnregisteredPool(t, e, map[string]string{"10.0.0.2": ownerRef})

	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "10.0.0.2", MACAddress: testMAC, NetworkName: testNetwork},
	})
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the deletion must finalize against the unregistered pool: %s", err)
	}

	pool := e.getStoredPool()
	if got, still := pool.Status.IPv4.Allocated["10.0.0.2"]; still {
		t.Errorf("the ledger record survived the deletion: %q", got)
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed so the object can terminate", final.Finalizers)
	}

	// the write never touched the serving-state counters of the pool:
	// the allocator does not know the network, so recomputing them would
	// have reported zero
	if got := pool.Status.IPv4.Used; got != 3 {
		t.Errorf("Used = %d, want the persisted 3 preserved", got)
	}
	if got := pool.Status.IPv4.Available; got != 2 {
		t.Errorf("Available = %d, want the persisted 2 preserved", got)
	}
	if e.ipam.HasSubnet(testNetwork) {
		t.Error("the fixture must keep the network unregistered")
	}
}

// TestVMNetCfgDeletionFailsClosedWhileThePoolCannotBeVerified: the
// cleanup may only skip the un-record when the pool object is
// verifiably gone. while the api list fails, the record might live in a
// pool object, so the deletion keeps the finalizers and the record - a
// failed lookup is never a converged one. once the api answers again the
// same object finalizes without any repair.
func TestVMNetCfgDeletionFailsClosedWhileThePoolCannotBeVerified(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	seedUnregisteredPool(t, e, map[string]string{"10.0.0.2": ownerRef})

	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "10.0.0.2", MACAddress: testMAC, NetworkName: testNetwork},
	})
	e.seedVMNetCfg(vmnetcfg)

	e.api.ippoolListCode = http.StatusInternalServerError
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err == nil {
		t.Fatal("the unverifiable pool must fail the deletion")
	}
	if _, still := e.getStoredPool().Status.IPv4.Allocated["10.0.0.2"]; !still {
		t.Fatal("the ledger record must survive the failed verification")
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 1 {
		t.Fatalf("finalizers = %v, want kept while the pool cannot be verified", final.Finalizers)
	}

	e.api.ippoolListCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.getStoredVMNetCfg()); err != nil {
		t.Fatalf("the deletion must finalize once the api answers: %s", err)
	}
	if _, still := e.getStoredPool().Status.IPv4.Allocated["10.0.0.2"]; still {
		t.Error("the ledger record survived the converged deletion")
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed after the recovery", final.Finalizers)
	}
}

// TestVMNetCfgDeletionLeavesTheForeignRecordOfAnUnregisteredPool: the
// owner validation must not regress for api-resolved pools. a record of
// another owner is not this binding's to remove, so the deleting
// cleanup reports it, keeps it and still finalizes.
func TestVMNetCfgDeletionLeavesTheForeignRecordOfAnUnregisteredPool(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)

	foreignRef := util.AllocationRef(testNamespace, "other-vm", testMAC2)
	seedUnregisteredPool(t, e, map[string]string{"10.0.0.2": foreignRef})

	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "10.0.0.2", MACAddress: testMAC, NetworkName: testNetwork},
	})
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the foreign record must not block the deletion: %s", err)
	}

	pool := e.getStoredPool()
	if got, still := pool.Status.IPv4.Allocated["10.0.0.2"]; !still || got != foreignRef {
		t.Errorf("allocated[10.0.0.2] = %q (present %v), want the foreign record untouched", got, still)
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed: the foreign record is not this binding's to remove", final.Finalizers)
	}
}

// TestVMNetCfgDeletionCleansEveryPoolClaimingTheNetwork: several pool
// objects may claim the same network, and the record can live in any of
// them. the cleanup removes it from every match - a no-op where the
// record is absent - instead of assuming the first match holds it.
func TestVMNetCfgDeletionCleansEveryPoolClaimingTheNetwork(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	seedUnregisteredPool(t, e, nil)

	// a second pool object claims the same network and holds the record
	claimant := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ippool-claimant"},
		Spec: kihv1.IPPoolSpec{
			NetworkName: testNetwork,
			IPv4Config: kihv1.IPv4Config{
				ServerIP: "10.0.0.1",
				Subnet:   testSubnet,
				Pool:     kihv1.Pool{Start: "10.0.0.1", End: "10.0.0.6"},
			},
		},
		Status: kihv1.IPPoolStatus{
			IPv4: kihv1.IPv4Status{Allocated: map[string]string{"10.0.0.2": ownerRef}, Used: 1, Available: 4},
		},
	}
	e.api.seedPool(claimant)

	vmnetcfg := newDeletingVMNetCfg([]kihv1.NetworkConfig{
		{IPAddress: "10.0.0.2", MACAddress: testMAC, NetworkName: testNetwork},
	})
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the deletion must finalize against every claimant: %s", err)
	}

	e.api.mu.Lock()
	if _, still := e.api.ippools["ippool-claimant"].Status.IPv4.Allocated["10.0.0.2"]; still {
		t.Error("the record of the second claimant survived the deletion")
	}
	if got := e.api.ippools["ippool-claimant"].Status.IPv4.Used; got != 1 {
		t.Errorf("the second claimant's Used = %d, want the persisted 1 preserved", got)
	}
	if got := e.api.ippools[testPoolName].Status.IPv4.Used; got != 3 {
		t.Errorf("the first claimant's Used = %d, want the persisted 3 preserved", got)
	}
	e.api.mu.Unlock()

	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed after every claimant was cleaned", final.Finalizers)
	}
}
