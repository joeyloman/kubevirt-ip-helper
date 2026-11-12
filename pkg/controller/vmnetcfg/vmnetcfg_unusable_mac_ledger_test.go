package vmnetcfg

// Regression test for the admission-outage leak: an admission webhook down
// (failurePolicy Ignore) lets a vmnetcfg through whose macaddress is
// parseable but unusable as a source address (the multicast bit set). the
// sync registers the binding and the pool ledger records its allocation,
// but no dhcp lease is ever served for it and no spec write-back records
// the ip, so the deletion cleanup found an empty spec address and no
// capturable lease and left the ledger entry resident until the next era
// rebuild reclaimed it. the cleanup must release the own unrecorded ledger
// entries directly instead.

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const multicastMAC = "01:00:5e:00:00:01"

func TestVMNetCfgUnusableMacDeletionReleasesItsLedgerEntry(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	// the admission-outage object: a parseable but unusable macaddress
	// which the sync registers and allocates for
	e.appStatus.Store(APP_RUNNING)
	vmnetcfg := newVMNetCfg("", multicastMAC)
	e.seedVMNetCfg(vmnetcfg)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the multicast macaddress is admitted by the controller: %s", err)
	}

	// the binding holds a ledger allocation and a registration lease;
	// neither the spec nor any served address records the ip for a
	// macaddress which can never source a guest interface
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Fatalf("ipam used = %d, want the registered allocation", used)
	}
	if !e.dhcp.CheckLease(multicastMAC) {
		t.Fatal("the registration must reserve a lease for the binding")
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 1 {
		t.Fatalf("pool status = %v, want the binding's allocation", pool.Status.IPv4.Allocated)
	}

	// the object is deleted: the cleanup must converge AND free the
	// unrecorded ledger entry, not leave it stranded until the era rebuild
	now := metav1.Now()
	vmnetcfg.ObjectMeta.DeletionTimestamp = &now
	vmnetcfg.ObjectMeta.Finalizers = []string{"kubevirtiphelper"}
	e.seedVMNetCfg(vmnetcfg)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the deletion cleanup must converge: %s", err)
	}

	stored := e.getStoredVMNetCfg()
	if len(stored.ObjectMeta.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want empty (the cleanup must converge)", stored.ObjectMeta.Finalizers)
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the unrecorded allocation is released)", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("pool status = %v, want the unusable binding's entry removed", pool.Status.IPv4.Allocated)
	}
}

// A manually created vmnetcfg carries no cleanup finalizer, so its deletion
// produces only a DELETE event: the release must replay from the tombstone
// the informer delivered, or the lease, the claim and the ledger entry stay
// resident until the next era rebuild reclaims them.
func TestVMNetCfgManualDeletionReplaysTheReleaseFromTheTombstone(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	e.appStatus.Store(APP_RUNNING)
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the binding must converge: %s", err)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Fatalf("ipam used = %d, want the allocated claim", used)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Fatal("the lease must exist before the deletion")
	}

	// the object is gone (no finalizer carried the deletion): the DELETE
	// sync replays the release from the tombstone
	if err := e.controller.sync(Event{key: testNamespace + "/" + testVMName, action: DELETE, vmnetcfg: vmnetcfg}); err != nil {
		t.Fatalf("the tombstone replay must converge: %s", err)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the lease must be deleted by the replay")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the claim is released)", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("pool status = %v, want the ledger entry removed", pool.Status.IPv4.Allocated)
	}

	// the replay is idempotent: a converged release (a controller-managed
	// deletion whose finalizer cleanup already ran) replays as a no-op
	if err := e.controller.sync(Event{key: testNamespace + "/" + testVMName, action: DELETE, vmnetcfg: vmnetcfg}); err != nil {
		t.Fatalf("the replay of a converged release must stay a no-op: %s", err)
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the replay must not re-allocate)", used)
	}
}

// A binding whose macaddress can never serve a lease (admitted through an
// admission outage) records its allocation in the ledger only: the deletion
// replay must free that unrecorded entry through the owner-validated sweep.
func TestVMNetCfgManualDeletionOfAnUnusableMacReleasesTheLedgerEntry(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	e.appStatus.Store(APP_RUNNING)
	vmnetcfg := newVMNetCfg("", multicastMAC)
	e.seedVMNetCfg(vmnetcfg)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the multicast macaddress is admitted by the controller: %s", err)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Fatalf("ipam used = %d, want the registered allocation", used)
	}

	if err := e.controller.sync(Event{key: testNamespace + "/" + testVMName, action: DELETE, vmnetcfg: vmnetcfg}); err != nil {
		t.Fatalf("the tombstone replay must converge: %s", err)
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the unrecorded allocation is released)", used)
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Errorf("pool status = %v, want the unusable binding's entry removed", pool.Status.IPv4.Allocated)
	}
}
