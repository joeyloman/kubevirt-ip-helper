package vmnetcfg

// P2-4 regression tests: the startup replay must be two-phase. A pending
// nic (persisted without an ip right before the restart) must not allocate
// during the initialization replay, because the recorded assignments of
// the other objects are still waiting for their own sync - especially when
// the pool status lost the record of an existing assignment (its record
// write failed before the restart), so the registration seeding cannot
// pin the address either. The replay restores every recorded assignment
// first; the deferred fresh allocations only run through the requeued
// reconciliation after the initialization finished.

import (
	"context"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// replayTwoPhaseEnv builds the upgrade scenario: vm-b carries the recorded
// assignment 10.0.0.1 in its spec only (no lease, no pool status record, no
// ipam claim - all lost with the previous process), vm-a carries the
// pending nic without an address. Nothing pins 10.0.0.1 except vm-b's
// object.
func replayTwoPhaseEnv(t *testing.T) *testEnv {
	t.Helper()

	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	recorded := newVMNetCfg("10.0.0.1", testMAC)
	recorded.ObjectMeta.Name = "vm-b"
	recorded.Spec.VMName = "vm-b"
	recorded.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{IPAddress: "10.0.0.1", MACAddress: testMAC, NetworkName: testNetwork},
	}
	e.seedVMNetCfg(recorded)

	pending := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "vm-a"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm-a"},
	}
	pending.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC2, NetworkName: testNetwork},
	}
	e.seedVMNetCfg(pending)

	return e
}

// The reviewer's probe as a fixed differential: vm-a's pending nic syncs
// first (arbitrary startup order) and must not take 10.0.0.1; vm-b's
// recorded assignment restores afterwards and owns it; the deferred
// allocation runs after the replay and takes the genuinely free address.
func TestStartupReplayAllocatesOnlyAfterEveryAssignmentRestored(t *testing.T) {
	e := replayTwoPhaseEnv(t)
	recorded := newVMNetCfg("10.0.0.1", testMAC)
	recorded.ObjectMeta.Name = "vm-b"
	recorded.Spec.VMName = "vm-b"
	recorded.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{IPAddress: "10.0.0.1", MACAddress: testMAC, NetworkName: testNetwork},
	}
	pending := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "vm-a"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm-a"},
	}
	pending.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC2, NetworkName: testNetwork},
	}

	// phase 1, replay order: the pending object syncs first
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, pending); err != nil {
		t.Fatalf("the deferred sync of the pending nic must succeed: %s", err)
	}
	if e.dhcp.CheckLease(testMAC2) {
		t.Error("the pending nic must not hold a lease during the replay")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 during the replay (nothing allocated)", used)
	}
	deferred := e.controller.releaseDeferredInitAllocations()
	if len(deferred) != 1 || deferred[0] != testNamespace+"/vm-a" {
		t.Fatalf("deferred keys = %v, want [default/vm-a]", deferred)
	}

	// phase 1 continued: the recorded assignment restores
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, recorded); err != nil {
		t.Fatalf("the recorded assignment must restore: %s", err)
	}
	if got := e.dhcp.GetLease(testMAC).ClientIP.String(); got != "10.0.0.1" {
		t.Errorf("vm-b lease ip = %q, want its recorded 10.0.0.1 (never 'already allocated')", got)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 after the restore", used)
	}

	// phase 2: the replay finished and the deferred key requeues
	*e.appStatus = APP_RUNNING
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, pending); err != nil {
		t.Fatalf("the requeued pending nic must allocate: %s", err)
	}
	if got := e.dhcp.GetLease(testMAC2).ClientIP.String(); got != "10.0.0.2" {
		t.Errorf("vm-a lease ip = %q, want the free 10.0.0.2", got)
	}
	if used := e.ipam.Used(testNetwork); used != 2 {
		t.Errorf("ipam used = %d, want 2 after the two-phase replay", used)
	}
}

// The requeue path: the deferred key is delivered as an UPDATE event which
// the running controller reconciles like every other event.
func TestDeferredKeysRequeueThroughTheQueue(t *testing.T) {
	e := replayTwoPhaseEnv(t)
	pending := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "vm-a"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm-a"},
	}
	pending.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC2, NetworkName: testNetwork},
	}
	if err := e.controller.indexer.Add(pending); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}

	// the pending object is processed with a real queue: the sync defers
	key := testNamespace + "/vm-a"
	if err := e.controller.sync(Event{key: key, action: ADD}); err != nil {
		t.Fatalf("the deferred sync must succeed: %s", err)
	}

	// the initialization finished: the wake requeues the deferred key
	*e.appStatus = APP_RUNNING
	e.controller.requeueDeferredInitAllocations()

	// the queued UPDATE event allocates
	if !e.controller.processNextItem() {
		t.Fatal("the requeued event must be processable")
	}
	// the requeued event allocated and committed durably: assert the API
	// object (the indexer entry is not updated by a direct sync)
	storedAPI, fetchErr := e.client.KubevirtiphelperV1().VirtualMachineNetworkConfigs(testNamespace).Get(context.TODO(), "vm-a", metav1.GetOptions{})
	if fetchErr != nil {
		t.Fatalf("fetching the synced object: %s", fetchErr)
	}

	// the requeued event allocated and committed durably: assert the API
	// object (the indexer entry is not updated by a direct sync); the
	// auto allocation hands out a free address in unspecified order, the
	// deterministic specific-address ordering is pinned by the startup
	// replay differential test below
	if got := storedAPI.Spec.NetworkConfig[0].IPAddress; got == "" {
		t.Errorf("vm-a spec ip = %q, want an allocated address after the requeue", got)
	}

	// the requeue drained the deferred set: a second requeue is a no-op
	if keys := e.controller.releaseDeferredInitAllocations(); len(keys) != 0 {
		t.Errorf("deferred keys after the requeue = %v, want empty", keys)
	}
}

// A recorded vmnetcfg whose recorded-address restore fails transiently
// during the replay is retried before the deferred allocations run: the
// failing sync stays uncounted and the pending nic does not jump the gate.
func TestPendingNicNeverOvertakesAFailedRestore(t *testing.T) {
	e := replayTwoPhaseEnv(t)
	pending := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "vm-a"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm-a"},
	}
	pending.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC2, NetworkName: testNetwork},
	}
	if err := e.controller.indexer.Add(pending); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}

	// the pending nic defers first
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, pending); err != nil {
		t.Fatalf("the deferred sync must succeed: %s", err)
	}

	// the recorded vmnetcfg is seeded with a lease-based steady state
	// whose pool status write fails transiently
	e.api.poolStatusPutCode = http.StatusInternalServerError
	recorded := newVMNetCfg("10.0.0.1", testMAC)
	recorded.ObjectMeta.Name = "vm-b"
	recorded.Spec.VMName = "vm-b"
	recorded.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{IPAddress: "10.0.0.1", MACAddress: testMAC, NetworkName: testNetwork},
	}
	e.seedVMNetCfg(recorded)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, recorded); err == nil {
		t.Fatal("want the transient restore failure to fail the sync")
	}

	// the restore of the recorded address is still retried and succeeds
	// before any deferred allocation consumed the address
	e.api.poolStatusPutCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, recorded); err != nil {
		t.Fatalf("the retried restore must succeed: %s", err)
	}
	if got := e.dhcp.GetLease(testMAC).ClientIP.String(); got != "10.0.0.1" {
		t.Errorf("vm-b lease ip = %q, want 10.0.0.1", got)
	}

	// the deferred allocation runs after the restore and takes the free
	// address only
	*e.appStatus = APP_RUNNING
	e.controller.requeueDeferredInitAllocations()
	if err := e.controller.sync(Event{key: testNamespace + "/vm-a", action: UPDATE}); err != nil {
		t.Fatalf("the requeued pending nic must allocate: %s", err)
	}
	if got := e.dhcp.GetLease(testMAC2).ClientIP.String(); got != "10.0.0.2" {
		t.Errorf("vm-a lease ip = %q, want 10.0.0.2", got)
	}
}

func TestDeferredObjectSettlesTheStartupGate(t *testing.T) {
	e := replayTwoPhaseEnv(t)
	pending := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "vm-a"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm-a"},
	}
	pending.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC2, NetworkName: testNetwork},
	}
	if err := e.controller.indexer.Add(pending); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}

	// a gate-wired controller so the settled count of the deferred sync
	// is observable
	count := 0
	controller := NewController(newTestQueue(), newTestIndexer(), nil, e.cache, e.ipam, e.dhcp, e.metrics, e.client, e.appStatus, &count)

	if err := controller.sync(Event{key: testNamespace + "/vm-a", action: ADD}); err != nil {
		t.Fatalf("the deferred sync must succeed: %s", err)
	}

	// the sync of the deferred object counted as handled: the startup
	// gate is not blocked by the deferred allocation
	if count != 1 {
		t.Errorf("gate count = %d, want 1 after the deferred sync settled", count)
	}
}
