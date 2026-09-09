package vmnetcfg

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// The late-registration barrier differential: a recovering pool which
// registered again after the startup gate dropped its retries seeds the
// persisted ownership claims of its bindings into the fresh allocator. The
// restore of a recorded binding must reclaim its own pinned address
// idempotently, and a fresh allocation must never take it - the whole
// observed sequence must keep the original binding's persisted ip.

func barrierSeededEnv(t *testing.T) *testEnv {
	t.Helper()

	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")

	// what the recovering registration now publishes: the kept pool status
	// record of the existing binding, pinned in the allocator
	e.seedPool(map[string]string{"10.0.0.1": util.AllocationRef(testNamespace, testVMName, testMAC)})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", util.AllocationRef(testNamespace, testVMName, testMAC)); err != nil {
		t.Fatalf("seeding the registration pin: %s", err)
	}

	return e
}

// The existing binding restores after the pool recovered: its recorded
// address stays its own and the pool status record is confirmed.
func TestRestoreReclaimsThePinnedClaim(t *testing.T) {
	e := barrierSeededEnv(t)

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the restored binding must reclaim its recorded address: %s", err)
	}

	if !e.dhcp.CheckLease(testMAC) {
		t.Error("the restored binding must serve again")
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != util.AllocationRef(testNamespace, testVMName, testMAC) {
		t.Errorf("pool record = %q, want the confirmed owner", got)
	}
}

// A new vm which arrives after the recovery must not take the address of the
// recorded binding: the barrier pins it away from fresh allocations.
func TestFreshAllocationAfterRecoverySkipsThePinnedClaim(t *testing.T) {
	e := barrierSeededEnv(t)

	restored := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(restored)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, restored); err != nil {
		t.Fatalf("the restored binding must reclaim its recorded address: %s", err)
	}
	// a second vm arrives and asks for an automatic address: steady state
	// (a running application serves fresh allocations immediately; the
	// startup replay defers them, covered by the finding-4 tests)
	*e.appStatus = APP_RUNNING
	fresh := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "vm-fresh"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm-fresh"},
	}
	fresh.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{NetworkName: testNetwork, MACAddress: testMAC2},
	}
	e.seedVMNetCfg(fresh)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, fresh); err != nil {
		t.Fatalf("the fresh vm must receive a free address: %s", err)
	}

	storedFresh := e.getStoredVMNetCfg()
	if storedFresh == nil || len(storedFresh.Status.NetworkConfig) < 1 {
		t.Fatalf("the fresh vm status was not persisted: %+v", storedFresh)
	}
	if e.dhcp.CheckLease(testMAC2) == false {
		t.Fatal("the fresh vm must lease its address")
	}
	if got := e.dhcp.GetLease(testMAC2).ClientIP.String(); got != "10.0.0.2" {
		t.Errorf("fresh vm address = %q, want 10.0.0.2 (the pinned claim must be skipped)", got)
	}
}

// A foreign binding cannot steal the pinned claim: its sync fails visibly
// and the address stays owned by the recorded binding.
func TestForeignBindingCannotTakeThePinnedClaim(t *testing.T) {
	e := barrierSeededEnv(t)

	foreign := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other-ns", Name: "other-vm"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "other-vm"},
	}
	foreign.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{NetworkName: testNetwork, MACAddress: testMAC2, IPAddress: "10.0.0.1"},
	}
	e.seedVMNetCfg(foreign)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, foreign); err == nil {
		// the ipam rejection lands the nic in a sticky error status rather
		// than failing the whole sync
		storedForeign, fetchErr := e.client.KubevirtiphelperV1().VirtualMachineNetworkConfigs("other-ns").Get(context.TODO(), "other-vm", metav1.GetOptions{})
		if fetchErr != nil {
			t.Fatalf("fetching the foreign vmnetcfg: %s", fetchErr)
		}
		if len(storedForeign.Status.NetworkConfig) < 1 || storedForeign.Status.NetworkConfig[0].Status != "ERROR" {
			t.Errorf("foreign nic status = %+v, want ERROR after the rejected claim", storedForeign.Status.NetworkConfig)
		}
	} else {
		t.Fatalf("the foreign binding sync must not fail the object: %s", err)
	}

	if e.dhcp.CheckLease(testMAC2) {
		t.Error("the foreign binding must not lease the pinned address")
	}

	// the address still belongs to the recorded owner
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", util.AllocationRef(testNamespace, testVMName, testMAC)); err != nil {
		t.Errorf("the recorded owner must still reclaim: %s", err)
	}
}

// A fresh allocation while the recorded binding has not restored yet (the
// startup replay ordering) must not take the pinned address either: the
// barrier holds before the restoration runs.
func TestBarrierHoldsBeforeTheRestoration(t *testing.T) {
	e := barrierSeededEnv(t)

	// only the fresh vm exists; the recorded binding's vmnetcfg object
	// arrives later
	fresh := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "vm-fresh"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm-fresh"},
	}
	fresh.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{NetworkName: testNetwork, MACAddress: testMAC2},
	}
	e.seedVMNetCfg(fresh)

	// the recorded binding's vmnetcfg object is not processed yet at this
	// point, but the vmnetcfg controller of a running application - the
	// startup replay defers instead and is covered by the finding-4 tests
	*e.appStatus = APP_RUNNING

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, fresh); err != nil {
		t.Fatalf("the fresh vm must receive a free address: %s", err)
	}

	// the barrier decides before the recorded binding restores: the fresh
	// vm receives the unclaimed address only
	if got := e.dhcp.GetLease(testMAC2).ClientIP.String(); got != "10.0.0.2" {
		t.Errorf("fresh vm address = %q, want 10.0.0.2 before the recorded binding restored", got)
	}

	// the recorded binding still restores afterwards
	restored := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(restored)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, restored); err != nil {
		t.Fatalf("the recorded binding must restore after the fresh allocation: %s", err)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("the recorded binding must serve again")
	}
}
