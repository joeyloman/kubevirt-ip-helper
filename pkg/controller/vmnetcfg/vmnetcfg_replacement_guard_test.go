package vmnetcfg

import (
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// the same-name replacement guard of the delete-event replay: a delete
// event whose key still holds a live object is a same-name replacement
// created while the deletion (or its rate-limited retry) was in flight.
// the owner checks of the release path are name-based, so replaying the
// tombstone would tear down the replacement's live lease, claim and
// ledger entry; the pending unwind records of the key carry the old
// generation's owner, so draining them would discard the successor's own
// records with the dead generation's. the replay is skipped and the
// replacement's own events manage the object.
func TestVMNetCfgDeleteWithSameNameReplacementSkipsReplay(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	e.seedPool(map[string]string{"10.0.0.2": ownerRef})

	// the replacement's live lease, served under the owner ref the
	// tombstone shares (the dhcp lease reference carries no mac suffix)
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.2", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the replacement's lease: %s", err)
	}

	key := testNamespace + "/" + testVMNetCfgName
	e.controller.rememberPendingUnwind(key, pendingLedgerDelete{
		namespace:   testNamespace,
		vmName:      testVMName,
		ip:          "10.0.0.2",
		networkName: testNetwork,
		macAddress:  testMAC,
		poolName:    testPoolName,
	})

	// the replacement still lives under the deleted event's key
	e.indexer.Add(testVMNetCfg([]kihv1.NetworkConfig{{
		MACAddress:  testMAC,
		NetworkName: testNetwork,
		IPAddress:   "10.0.0.2",
	}}))

	tombstone := testVMNetCfg([]kihv1.NetworkConfig{{
		MACAddress:  testMAC,
		NetworkName: testNetwork,
		IPAddress:   "10.0.0.2",
	}})
	if err := e.controller.sync(Event{key: key, action: DELETE, vmnetcfg: tombstone}); err != nil {
		t.Fatalf("the delete sync failed: %s", err)
	}

	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.2" || lease.Reference != testNamespace+"/"+testVMName {
		t.Errorf("the replacement's lease was torn down by the tombstone replay: %+v", lease)
	}

	e.controller.mutex.Lock()
	pending := len(e.controller.pendingUnwinds[key])
	e.controller.mutex.Unlock()
	if pending != 1 {
		t.Errorf("pending unwind entries after the skipped replay = %d, want 1: the successor's own record must survive", pending)
	}

	pool := e.getStoredPool()
	if _, still := pool.Status.IPv4.Allocated["10.0.0.2"]; !still {
		t.Error("the replacement's ledger record was removed by the tombstone replay")
	}
}

// the guard must not swallow the regular deletion: with the object gone
// from the index, the same delete event replays the release and the
// unwind drain exactly like before.
func TestVMNetCfgDeleteWithoutReplacementReplaysRelease(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	e.seedPool(map[string]string{"10.0.0.2": ownerRef})

	// the dhcp lease reference carries no mac suffix, matching the
	// production AddLease callers
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.2", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the deleted binding's lease: %s", err)
	}

	key := testNamespace + "/" + testVMNetCfgName
	tombstone := testVMNetCfg([]kihv1.NetworkConfig{{
		MACAddress:  testMAC,
		NetworkName: testNetwork,
		IPAddress:   "10.0.0.2",
	}})
	if err := e.controller.sync(Event{key: key, action: DELETE, vmnetcfg: tombstone}); err != nil {
		t.Fatalf("the delete sync failed: %s", err)
	}

	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP != nil {
		t.Errorf("the deleted binding's lease survived the release: %+v", lease)
	}

	pool := e.getStoredPool()
	if _, still := pool.Status.IPv4.Allocated["10.0.0.2"]; still {
		t.Error("the deleted binding's ledger record survived the release")
	}
}
