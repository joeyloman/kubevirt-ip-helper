package vmnetcfg

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

func hijackTestPool(lastUpdateBeforeStart time.Time, lastUpdate time.Time) *kihv1.IPPool {
	return &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: testPoolName},
		Spec: kihv1.IPPoolSpec{
			NetworkName: testNetwork,
			IPv4Config:  kihv1.IPv4Config{Subnet: testSubnet, ServerIP: "10.0.0.1"},
		},
		Status: kihv1.IPPoolStatus{
			LastUpdateBeforeStart: metav1.NewTime(lastUpdateBeforeStart),
			LastUpdate:            metav1.NewTime(lastUpdate),
		},
	}
}

// TestVMNetCfgControllerManagedObjectIsNeverHijacked pins the fast-recreate
// regression: the vm controller creates the replacement vmnetcfg while
// other bindings keep mutating the pool status. The object is
// controller-managed (cleanup finalizer) and was created after this era
// started, so it must allocate and publish its lease even though a pool
// status write lands after its creationTimestamp.
func TestVMNetCfgControllerManagedObjectIsNeverHijacked(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	base := time.Now()
	e.controller.eraStart = base
	e.seedPoolWith(hijackTestPool(base, base.Add(2*time.Second)))

	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.ObjectMeta.Finalizers = []string{vmnetcfgCleanupFinalizer}
	vmnetcfg.ObjectMeta.CreationTimestamp = metav1.NewTime(base.Add(time.Second))
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the controller-managed replacement must sync: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	got := stored.Status.NetworkConfig[0]
	if got.Status != "OK" {
		t.Fatalf("status = %q message = %q, want OK", got.Status, got.Message)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("the replacement must publish its dhcp lease")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 for the published lease", used)
	}
}

// TestVMNetCfgManualObjectCreatedWhileRunningIsNotHijacked pins the era
// fence independently of the controller finalizer: a manual object created
// after this era started is handled by the normal ownership checks of the
// allocation path, not rejected as a downtime hijack.
func TestVMNetCfgManualObjectCreatedWhileRunningIsNotHijacked(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	base := time.Now()
	e.controller.eraStart = base
	e.seedPoolWith(hijackTestPool(base, base.Add(2*time.Second)))

	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.ObjectMeta.CreationTimestamp = metav1.NewTime(base.Add(time.Second))
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("a manual object created while this era runs must sync: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if got := stored.Status.NetworkConfig[0]; got.Status != "OK" {
		t.Errorf("status = %+v, want OK for an object created while the helper runs", got)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("the object created while the helper runs must publish its lease")
	}
}

// TestVMNetCfgManualDowntimeObjectIsStillHijacked keeps the guard's purpose:
// an object without the controller finalizer, created before this era
// started and after the previous era's last status update, may have been
// created while the helper was down and stays rejected.
func TestVMNetCfgManualDowntimeObjectIsStillHijacked(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	base := time.Now()
	e.controller.eraStart = base.Add(5 * time.Minute)
	e.seedPoolWith(hijackTestPool(base, base.Add(2*time.Minute)))

	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.ObjectMeta.CreationTimestamp = metav1.NewTime(base.Add(time.Minute))
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	got := stored.Status.NetworkConfig[0]
	if got.Status != "ERROR" || got.Message != hijackErrorStatusMessage {
		t.Errorf("status = %+v, want the hijack ERROR", got)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("a guarded manual object must not publish a lease")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 for a guarded manual object", used)
	}
}

// TestVMNetCfgControllerManagedHijackMarkerHeals pins the upgrade path:
// an object which the racy legacy timestamp comparison already marked with
// the terminal hijack ERROR is re-attempted on the next resync because it
// carries the controller finalizer.
func TestVMNetCfgControllerManagedHijackMarkerHeals(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.ObjectMeta.Finalizers = []string{vmnetcfgCleanupFinalizer}
	vmnetcfg.ObjectMeta.CreationTimestamp = metav1.NewTime(time.Now())
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{
		MACAddress:  testMAC,
		NetworkName: testNetwork,
		Status:      "ERROR",
		Message:     hijackErrorStatusMessage,
	}}
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the controller-managed object must heal: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	got := stored.Status.NetworkConfig[0]
	if got.Status != "OK" {
		t.Errorf("status = %q message = %q, want OK after the healing re-attempt", got.Status, got.Message)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("the healed object must publish its dhcp lease")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 after the healing re-attempt", used)
	}
}

// TestVMNetCfgDeleteReplayReleasesNonShadowedNics pins the filtered
// delete-event replay: a same-name replacement with a different MAC is a
// different owner identity, so the old generation's lease, claim and ledger
// entry are released while the replacement's live state and its shadowed
// pending unwind stay untouched.
func TestVMNetCfgDeleteReplayReleasesNonShadowedNics(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.4")

	const (
		oldIP  = "10.0.0.1"
		newIP  = "10.0.0.2"
		oldMAC = testMAC
		newMAC = testMAC2
	)

	oldOwner := util.AllocationRef(testNamespace, testVMName, oldMAC)
	newOwner := util.AllocationRef(testNamespace, testVMName, newMAC)
	e.seedPool(map[string]string{oldIP: oldOwner, newIP: newOwner})

	if _, err := e.ipam.ReclaimIP(testNetwork, oldIP, oldOwner); err != nil {
		t.Fatalf("seeding the old claim: %s", err)
	}
	if _, err := e.ipam.ReclaimIP(testNetwork, newIP, newOwner); err != nil {
		t.Fatalf("seeding the replacement claim: %s", err)
	}
	if err := e.dhcp.AddLease(oldMAC, testNetwork, oldIP, testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the old lease: %s", err)
	}
	if err := e.dhcp.AddLease(newMAC, testNetwork, newIP, testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the replacement lease: %s", err)
	}

	replacement := newVMNetCfg(newIP, newMAC)
	replacement.ObjectMeta.UID = "generation-b"
	if err := e.indexer.Add(replacement); err != nil {
		t.Fatalf("seeding the replacement in the indexer: %s", err)
	}

	tombstone := newVMNetCfg(oldIP, oldMAC)
	tombstone.ObjectMeta.UID = "generation-a"

	key := testNamespace + "/" + testVMNetCfgName
	e.controller.rememberPendingUnwind(key, pendingLedgerDelete{
		namespace:   testNamespace,
		vmName:      testVMName,
		ip:          oldIP,
		networkName: testNetwork,
		macAddress:  oldMAC,
		poolName:    testPoolName,
		uid:         tombstone.UID,
	})
	e.controller.rememberPendingUnwind(key, pendingLedgerDelete{
		namespace:   testNamespace,
		vmName:      testVMName,
		ip:          newIP,
		networkName: testNetwork,
		macAddress:  newMAC,
		poolName:    testPoolName,
		uid:         tombstone.UID,
	})

	if err := e.controller.sync(Event{key: key, action: DELETE, vmnetcfg: tombstone}); err != nil {
		t.Fatalf("the filtered delete replay failed: %s", err)
	}

	if e.dhcp.CheckLease(oldMAC) {
		t.Error("the old generation's lease must be released")
	}
	if _, err := e.ipam.ReclaimIP(testNetwork, oldIP, oldOwner); err != nil {
		t.Errorf("the old generation's claim must be released, reclaim failed: %s", err)
	}
	if _, err := e.ipam.ReclaimIP(testNetwork, newIP, newOwner); err != nil {
		t.Errorf("the replacement's claim must stay held: %s", err)
	}

	allocated := e.getStoredPool().Status.IPv4.Allocated
	if _, ok := allocated[oldIP]; ok {
		t.Errorf("the old generation's ledger record must be removed, got %v", allocated)
	}
	if owner := allocated[newIP]; owner != newOwner {
		t.Errorf("the replacement's ledger record = %q, want %q", owner, newOwner)
	}

	lease := e.dhcp.GetLease(newMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != newIP || lease.Reference != testNamespace+"/"+testVMName {
		t.Errorf("the replacement's lease must stay intact, got %+v", lease)
	}

	e.controller.mutex.Lock()
	pending := append([]pendingLedgerDelete(nil), e.controller.pendingUnwinds[key]...)
	e.controller.mutex.Unlock()
	if len(pending) != 1 || pending[0].macAddress != newMAC {
		t.Errorf("pending unwinds = %+v, want only the replacement's shadowed entry", pending)
	}

	if got := e.queue.Len(); got != 1 {
		t.Errorf("queue length after the replay = %d, want 1 (the replacement's requeue)", got)
	}
}
