package vmnetcfg

// P1-1 regression tests: the startup gate must settle for every object
// exactly once - a recovered UPDATE settles an object whose initial ADD
// failed, a startup-time deletion settles an object which can never sync
// anymore, and an exhausted retry settles a key which keeps failing.

import (
	"errors"
	"net/http"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// An object whose first ADD fails transiently and recovers through a
// resynced UPDATE must settle the gate on that successful UPDATE.
func TestSyncUpdateSuccessSettlesStartupGate(t *testing.T) {
	e, controller, count := newGateTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)
	if err := controller.indexer.Add(vmnetcfg); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}
	key := testNamespace + "/" + testVMNetCfgName

	// the initial ADD fails transiently and stays uncounted
	e.api.poolStatusPutCode = http.StatusInternalServerError
	if err := controller.sync(Event{key: key, action: ADD}); err == nil {
		t.Fatal("want the transient status failure to fail the sync")
	}
	if *count != 0 {
		t.Fatalf("gate count = %d after the failed ADD, want 0", *count)
	}

	// the recovery arrives as a resync UPDATE and must settle the gate
	e.api.poolStatusPutCode = 0
	if err := controller.sync(Event{key: key, action: UPDATE}); err != nil {
		t.Fatalf("the recovered UPDATE sync failed: %s", err)
	}
	if *count != 1 {
		t.Fatalf("gate count = %d after the recovered UPDATE, want 1", *count)
	}

	// a further sync of the settled object must not double count
	if err := controller.sync(Event{key: key, action: UPDATE}); err != nil {
		t.Fatalf("the repeated sync failed: %s", err)
	}
	if *count != 1 {
		t.Errorf("gate count = %d after the repeated sync, want 1", *count)
	}
}

// An object deleted during startup - before its sync ever settled - must
// count so the startup gate does not wait for a key which can never sync.
func TestSyncDeleteSettlesUncountedStartupObject(t *testing.T) {
	_, controller, count := newGateTestEnv(t)
	key := testNamespace + "/" + testVMNetCfgName

	if err := controller.sync(Event{key: key, action: DELETE}); err != nil {
		t.Fatalf("the delete sync failed: %s", err)
	}
	if *count != 1 {
		t.Errorf("gate count = %d after the deleted object, want 1", *count)
	}
}

// A key dropped after the retry threshold must settle the gate: it can
// never settle through its own retries anymore, so the gate waiting for it
// would block the whole controller startup forever.
func TestHandleErrDropSettlesStartupCount(t *testing.T) {
	_, controller, count := newGateTestEnv(t)
	key := testNamespace + "/" + testVMNetCfgName
	syncErr := errors.New("persistent failure")

	// the first five failures are rate limited and requeued
	for i := 0; i < 5; i++ {
		controller.queue.Add(Event{key: key, action: ADD})
		item, quit := controller.queue.Get()
		if quit {
			t.Fatalf("queue was shut down while getting the item")
		}
		controller.handleErr(syncErr, item)
		controller.queue.Done(item)
	}
	if *count != 0 {
		t.Fatalf("gate count = %d after five requeues, want 0", *count)
	}

	// the next failure exceeds the retry threshold: the key is dropped and
	// must settle the startup gate exactly once
	controller.queue.Add(Event{key: key, action: ADD})
	item, quit := controller.queue.Get()
	if quit {
		t.Fatalf("queue was shut down while getting the item")
	}
	controller.handleErr(syncErr, item)
	controller.queue.Done(item)

	if *count != 1 {
		t.Errorf("gate count = %d after the dropped key, want 1", *count)
	}
}

// A transient failure (pool status write 500) on one interface must leave
// every interface of the object protected when the sync fails, and the
// protection must survive the retry-exhaustion settle: the gate may open,
// but no recorded address becomes reissuable.
func TestGateDropSettleKeepsInterfacesProtected(t *testing.T) {
	e, controller, count := newGateTestEnv(t)
	seedHealthyPool(e)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testVMNetCfgName},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName: testVMName,
			NetworkConfig: []kihv1.NetworkConfig{
				{IPAddress: "10.0.0.1", MACAddress: "02:00:00:00:00:01", NetworkName: testNetwork},
				{IPAddress: "10.0.1.2", MACAddress: "02:00:00:00:00:02", NetworkName: healthyNet2},
			},
		},
	}
	e.seedVMNetCfg(vmnetcfg)
	if err := controller.indexer.Add(vmnetcfg); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}
	key := testNamespace + "/" + testVMNetCfgName

	// every pool status write fails transiently: the sync still processes
	// the later interface and retains its durable reservation
	e.api.poolStatusPutCode = http.StatusInternalServerError
	if err := controller.sync(Event{key: key, action: ADD}); err == nil {
		t.Fatal("want the transient status failure to fail the sync")
	}
	if *count != 0 {
		t.Fatalf("gate count = %d after the transient failure, want 0", *count)
	}
	if !e.dhcp.CheckLease("02:00:00:00:00:02") {
		t.Fatal("the later interface must stay restored after the failed sync")
	}
	if used := e.ipam.Used(healthyNet2); used != 1 {
		t.Fatalf("healthy network used = %d, want 1", used)
	}
	if _, err := e.ipam.GetIP(healthyNet2, "10.0.1.2"); err == nil {
		t.Fatal("the recorded address must not be reissuable")
	}

	// the key exhausts its retries and is dropped: the gate settles, but
	// the recorded addresses stay protected
	syncErr := errors.New("persistent failure")
	for i := 0; i < 5; i++ {
		controller.queue.Add(Event{key: key, action: ADD})
		item, quit := controller.queue.Get()
		if quit {
			t.Fatalf("queue was shut down while getting the item")
		}
		controller.handleErr(syncErr, item)
		controller.queue.Done(item)
	}
	controller.queue.Add(Event{key: key, action: ADD})
	item, quit := controller.queue.Get()
	if quit {
		t.Fatalf("queue was shut down while getting the item")
	}
	controller.handleErr(syncErr, item)
	controller.queue.Done(item)

	if *count != 1 {
		t.Fatalf("gate count = %d after the drop, want 1", *count)
	}
	if !e.dhcp.CheckLease("02:00:00:00:00:02") {
		t.Error("the later interface's lease must survive the gate settle")
	}
	if used := e.ipam.Used(healthyNet2); used != 1 {
		t.Errorf("healthy network used = %d after the gate settle, want 1", used)
	}
	if _, err := e.ipam.GetIP(healthyNet2, "10.0.1.2"); err == nil {
		t.Error("the recorded address must not become reissuable after the gate settle")
	}
}
