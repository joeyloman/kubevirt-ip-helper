package vmnetcfg

import (
	"errors"
	"net/http"
	"testing"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// The startup gate must only count a vmnetcfg once its initial sync
// settled: counting a failed restore as handled would open the vm
// controller while an existing reservation is still unprotected, so a new
// allocation could take over an address whose owning guest keeps using it.

// newGateTestEnv wires a controller with a real indexer to the behavior
// test environment, so sync() can be exercised with the fake API server
// and the startup gate counters of a private initialization phase.
func newGateTestEnv(t *testing.T) (*testEnv, *Controller, *int) {
	t.Helper()

	e := newTestEnv(t)

	appStatus := APP_INIT
	count := 0
	controller := NewController(
		newTestQueue(),
		newTestIndexer(),
		nil,
		e.cache,
		e.ipam,
		e.dhcp,
		e.metrics,
		e.client,
		&appStatus,
		&count,
	)

	return e, controller, &count
}

// a transiently failed restore must stay uncounted: the retried sync
// settles the gate only after the reservation is actually rebuilt
func TestSyncAddTransientFailureStaysUncountedUntilTheRestoreSucceeds(t *testing.T) {
	e, controller, count := newGateTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(vmnetcfg)
	if err := controller.indexer.Add(vmnetcfg); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}

	event := Event{key: testNamespace + "/" + testVMNetCfgName, action: ADD}

	// the pool status write fails transiently
	e.api.poolStatusPutCode = http.StatusInternalServerError

	if err := controller.sync(event); err == nil {
		t.Fatal("want the transient status failure to fail the sync")
	}
	if *count != 0 {
		t.Errorf("gate count = %d, want 0: a transiently failed restore must stay uncounted", *count)
	}

	// the retried sync rebuilds the reservation and settles the gate
	e.api.poolStatusPutCode = 0

	if err := controller.sync(event); err != nil {
		t.Fatalf("the retried sync failed: %s", err)
	}
	if *count != 1 {
		t.Errorf("gate count = %d, want 1 after the settled restore", *count)
	}

	// a further sync of the settled object must not double count
	if err := controller.sync(event); err != nil {
		t.Fatalf("the repeated sync failed: %s", err)
	}
	if *count != 1 {
		t.Errorf("gate count = %d after the repeated sync, want 1", *count)
	}
}

// an ownership conflict is definitive: the pool status records the claimed
// address for another owner, so no retry can settle the object and the
// gate must count it as handled instead of blocking the vm controller
func TestSyncAddOwnershipConflictCountsAsHandledDuringInit(t *testing.T) {
	e, controller, count := newGateTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(map[string]string{"10.0.0.1": "other-ns/other-vm [02:00:00:00:00:99]"})

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)
	if err := controller.indexer.Add(vmnetcfg); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}

	err := controller.sync(Event{key: testNamespace + "/" + testVMNetCfgName, action: ADD})
	if err == nil {
		t.Fatal("want the ownership conflict to fail the sync")
	}
	if !errors.Is(err, util.ErrForeignOwner) {
		t.Errorf("error = %v, want the util.ErrForeignOwner classification", err)
	}
	if *count != 1 {
		t.Errorf("gate count = %d, want 1: a definitively rejected claim must count as handled", *count)
	}
}

// an invalid macaddress in the spec can never register a lease, so the
// gate must count the object as handled instead of waiting for a repair
func TestSyncAddInvalidMacCountsAsHandledDuringInit(t *testing.T) {
	e, controller, count := newGateTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("10.0.0.1", "not-a-mac-address")
	e.seedVMNetCfg(vmnetcfg)
	if err := controller.indexer.Add(vmnetcfg); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}

	err := controller.sync(Event{key: testNamespace + "/" + testVMNetCfgName, action: ADD})
	if err == nil {
		t.Fatal("want the invalid macaddress to fail the sync")
	}
	if *count != 1 {
		t.Errorf("gate count = %d, want 1: a definitively broken object must count as handled", *count)
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0: an unusable macaddress must not consume a reservation", used)
	}
}
