package vmnetcfg

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/gate"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// The startup gate must only settle a vmnetcfg once its initial sync
// settled: settling a failed restore as handled would open the vm
// controller while an existing reservation is still unprotected, so a new
// allocation could take over an address whose owning guest keeps using it.

// newGateTestEnv wires a controller with a real indexer to the behavior
// test environment, so sync() can be exercised with the fake API server
// and the startup gate of a private initialization phase. the gate
// snapshot holds the test object's key, so the settle assertions observe
// the membership contract.
func newGateTestEnv(t *testing.T) (*testEnv, *Controller, *gate.Gate) {
	t.Helper()

	e := newTestEnv(t)

	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate(testNamespace + "/" + testVMNetCfgName)
	controller := NewController(
		context.Background(),
		newTestQueue(),
		newTestIndexer(),
		nil,
		e.cache,
		e.ipam,
		e.dhcp,
		e.metrics,
		e.client,
		&appStatus,
		startupGate,
		e.scope,
		e.reconcileMu,
	)

	return e, controller, startupGate
}

// a transiently failed restore must stay uncounted: the retried sync
// settles the gate only after the reservation is actually rebuilt
func TestSyncAddTransientFailureStaysUncountedUntilTheRestoreSucceeds(t *testing.T) {
	e, controller, startupGate := newGateTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	// the recorded address restores during the initialization: this is
	// the transient-failure-retry contract of the restore path itself
	// (a pending nic without a recorded address defers instead and is
	// covered by the finding-4 replay tests)
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
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
	if startupGate.Settled() != 0 {
		t.Errorf("gate count = %d, want 0: a transiently failed restore must stay uncounted", startupGate.Settled())
	}

	// the retried sync rebuilds the reservation and settles the gate
	e.api.poolStatusPutCode = 0

	if err := controller.sync(event); err != nil {
		t.Fatalf("the retried sync failed: %s", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("gate count = %d, want 1 after the settled restore", startupGate.Settled())
	}

	// a further sync of the settled object must not double count
	if err := controller.sync(event); err != nil {
		t.Fatalf("the repeated sync failed: %s", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("gate count = %d after the repeated sync, want 1", startupGate.Settled())
	}
}

// an ownership conflict is definitive: the pool status records the claimed
// address for another owner, so no retry can settle the object and the
// gate must count it as handled instead of blocking the vm controller
func TestSyncAddOwnershipConflictCountsAsHandledDuringInit(t *testing.T) {
	e, controller, startupGate := newGateTestEnv(t)

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
	if startupGate.Settled() != 1 {
		t.Errorf("gate count = %d, want 1: a definitively rejected claim must count as handled", startupGate.Settled())
	}
}

// an invalid macaddress in the spec can never register a lease, so the
// gate must count the object as handled instead of waiting for a repair
func TestSyncAddInvalidMacCountsAsHandledDuringInit(t *testing.T) {
	e, controller, startupGate := newGateTestEnv(t)

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
	if startupGate.Settled() != 1 {
		t.Errorf("gate count = %d, want 1: a definitively broken object must count as handled", startupGate.Settled())
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0: an unusable macaddress must not consume a reservation", used)
	}
}

// a transient failure on one interface must keep the object uncounted even
// when an unrelated interface of the same object is permanently broken: the
// settled classification follows the recorded per-interface failure, never a
// re-scan of the spec, or the gate would open while the transient restore is
// still pending and the deferred fresh allocations of other objects could
// take its address
func TestSyncAddMixedFailureClassifiesOnTheRecordedFailure(t *testing.T) {
	e, controller, startupGate := newGateTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	// the first interface fails transiently (its pool status write), the
	// second has an invalid MAC on the same owned network
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{IPAddress: "10.0.0.1", MACAddress: testMAC, NetworkName: testNetwork},
		{MACAddress: "not-a-mac-address", NetworkName: testNetwork},
	}
	e.seedVMNetCfg(vmnetcfg)
	if err := controller.indexer.Add(vmnetcfg); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}
	key := testNamespace + "/" + testVMNetCfgName

	// the transient failure is recorded first, so the permanently broken
	// second interface must not settle the object
	e.api.poolStatusPutCode = http.StatusInternalServerError
	if err := controller.sync(Event{key: key, action: ADD}); err == nil {
		t.Fatal("want the mixed-failure sync to fail")
	}
	if startupGate.Settled() != 0 {
		t.Fatalf("gate count = %d, want 0: the recorded transient failure keeps the object uncounted although another interface is permanently broken", startupGate.Settled())
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Fatal("the transiently failed interface keeps its lease protected for the retry")
	}

	// once the transient failure healed, the remaining permanent failure
	// settles the object for the gate
	e.api.poolStatusPutCode = 0
	if err := controller.sync(Event{key: key, action: ADD}); err == nil {
		t.Fatal("want the invalid-MAC interface to keep failing the sync")
	} else if !errors.Is(err, errNicMacInvalid) {
		t.Fatalf("error = %v, want the remaining permanent invalid-MAC failure", err)
	}
	if startupGate.Settled() != 1 {
		t.Fatalf("gate count = %d, want 1: the invalid-MAC interface settles the gate", startupGate.Settled())
	}
}

// C02 regression: a definitively rejected sync settles the startup gate
// on the UPDATE action as well. an object which is first delivered as a
// resynced UPDATE during the initialization (e.g. written by the vm
// controller while the gate is open) and fails with a settled-class error
// must not wait for the full rate-limited retry exhaustion before the
// gate counts it - the classification is definitive, so no retry of the
// same object can protect an additional reservation.
func TestSyncUpdateSettledErrorCountsAsHandledDuringInit(t *testing.T) {
	e, controller, startupGate := newGateTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(map[string]string{"10.0.0.1": "other-ns/other-vm [02:00:00:00:00:99]"})

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)
	if err := controller.indexer.Add(vmnetcfg); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}

	err := controller.sync(Event{key: testNamespace + "/" + testVMNetCfgName, action: UPDATE})
	if err == nil {
		t.Fatal("want the ownership conflict to fail the sync")
	}
	if !errors.Is(err, util.ErrForeignOwner) {
		t.Errorf("error = %v, want the util.ErrForeignOwner classification", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("gate settled = %d, want 1: the definitively rejected UPDATE must settle the gate without waiting for the retry exhaustion", startupGate.Settled())
	}
}
