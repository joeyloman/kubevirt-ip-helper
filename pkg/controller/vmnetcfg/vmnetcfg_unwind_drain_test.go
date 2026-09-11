package vmnetcfg

import (
	"net/http"
	"testing"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// The delete-event drain regression tests: a force-delete strips the
// finalizers externally, so the object can disappear while its pending
// ledger unwind is still recorded, and no reconciliation of it will ever
// arrive again - the resident entries kept the map bound and the ledger
// records stranded until the next process era. The delete event replays
// each recorded deletion one last time and drops it.

func TestVMNetCfgDeleteEventDrainsPendingUnwinds(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	e.seedPool(map[string]string{"10.0.0.2": ownerRef})

	key := testNamespace + "/" + testVMNetCfgName
	e.controller.rememberPendingUnwind(key, pendingLedgerDelete{
		namespace:   testNamespace,
		vmName:      testVMName,
		ip:          "10.0.0.2",
		networkName: testNetwork,
		macAddress:  testMAC,
		poolName:    testPoolName,
	})

	// the object was force-deleted (its finalizers stripped externally):
	// the delete event is the last reference to its pending unwind
	if err := e.controller.sync(Event{key: key, action: DELETE}); err != nil {
		t.Fatalf("the delete sync failed: %s", err)
	}

	// the final attempt removed the ledger record
	pool := e.getStoredPool()
	if got, still := pool.Status.IPv4.Allocated["10.0.0.2"]; still {
		t.Errorf("the drained ledger record must be removed from the pool status, still recorded as %q", got)
	}

	e.controller.mutex.Lock()
	pending := len(e.controller.pendingUnwinds[key])
	e.controller.mutex.Unlock()
	if pending != 0 {
		t.Errorf("pending entries after the drain = %d, want 0: the delete event must drop them", pending)
	}
}

// a transiently failing final attempt must not keep the entry resident
// either: the object is gone, no retry can ever arrive, and the next
// era's pool registration revalidates the persisted ledger - the entry is
// dropped with a warning instead.
func TestVMNetCfgDeleteEventDropsUnconvergedPendingUnwinds(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	e.seedPool(map[string]string{"10.0.0.2": ownerRef})
	e.api.poolStatusPutCode = http.StatusInternalServerError

	key := testNamespace + "/" + testVMNetCfgName
	e.controller.rememberPendingUnwind(key, pendingLedgerDelete{
		namespace:   testNamespace,
		vmName:      testVMName,
		ip:          "10.0.0.2",
		networkName: testNetwork,
		macAddress:  testMAC,
		poolName:    testPoolName,
	})

	if err := e.controller.sync(Event{key: key, action: DELETE}); err != nil {
		t.Fatalf("the delete sync must not fail on the dropped unwind: %s", err)
	}

	e.controller.mutex.Lock()
	pending := len(e.controller.pendingUnwinds[key])
	e.controller.mutex.Unlock()
	if pending != 0 {
		t.Errorf("pending entries after the failed drain = %d, want 0: the entry must be dropped, not re-recorded", pending)
	}
	if n := e.countMetricsByLabel(metricAppLogs, "loglevel", "warning"); n == 0 {
		t.Error("the dropped unwind must be surfaced as a warning")
	}

	// the record itself stays until the next era's registration sweep
	// revalidates it - the drain never fabricates a converged state
	pool := e.getStoredPool()
	if _, still := pool.Status.IPv4.Allocated["10.0.0.2"]; !still {
		t.Error("the failed ledger delete must leave the record to the next era's revalidation")
	}
}
