package vmnetcfg

import (
	"errors"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// The orphan sweep regression tests: the vm controller is the only writer
// which deletes the vmnetcfg when its VirtualMachine is deleted, and a vm
// deleted while no controller was watching produces no event any restart
// could replay (the fresh informer lists only what exists). Such a binding
// used to keep its finalizer, its recorded binding kept the ledger owner
// alive, and its address stayed allocated to a deleted vm until someone
// deleted the object by hand. The sweep routes a controller-managed binding
// whose vm answers NotFound on the authoritative api into the regular
// deletion flow.

// newOrphanVMNetCfg builds a controller-managed binding: the cleanup
// finalizer is only put on the object by the vm controller, and the uid is
// what the delete precondition must carry.
func newOrphanVMNetCfg() *kihv1.VirtualMachineNetworkConfig {
	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	vmnetcfg.UID = "1111-2222-3333"
	vmnetcfg.Finalizers = []string{vmnetcfgCleanupFinalizer}

	return vmnetcfg
}

// seedStrandedBindingState reproduces the live state a down-time orphan
// holds: a claimed address, a served lease and a persisted ledger record,
// none of which any vm event would ever release anymore.
func seedStrandedBindingState(e *testEnv) {
	e.t.Helper()
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	ownerRef := testNamespace + "/" + testVMName + " [" + testMAC + "]"
	e.seedPool(map[string]string{"10.0.0.1": ownerRef})
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", ownerRef); err != nil {
		e.t.Fatalf("seeding the stranded claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		e.t.Fatalf("seeding the stranded lease: %s", err)
	}
}

func TestVMNetCfgOrphanSweepReleasesTheStrandedBinding(t *testing.T) {
	e := newTestEnv(t)
	seedStrandedBindingState(e)

	// the vm is definitively gone: it was deleted while no controller
	// was watching, so no event of it exists anymore
	e.controller.verifyVM = func(namespace string, name string) (bool, error) {
		if namespace != testNamespace || name != testVMName {
			t.Errorf("the vm verification queried %s/%s, want %s/%s", namespace, name, testNamespace, testVMName)
		}

		return false, nil
	}

	vmnetcfg := newOrphanVMNetCfg()
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the orphan sweep must not fail the sync: %s", err)
	}

	// the sweep routed the binding into the deletion flow instead of
	// running the regular reconciliation: exactly one delete, no spec
	// write, and the delete is preconditioned on the delivered uid
	if n := e.countRequests(http.MethodDelete, vmnetcfgMainPath); n != 1 {
		t.Fatalf("delete requests = %d, want exactly 1", n)
	}
	if n := e.countRequests(http.MethodPut, vmnetcfgMainPath); n != 0 {
		t.Errorf("spec writes = %d, want 0: the swept binding must not be reconciled", n)
	}
	e.api.mu.Lock()
	deletes := append([]metav1.DeleteOptions(nil), e.api.vmnetcfgDeletes...)
	e.api.mu.Unlock()
	if len(deletes) != 1 {
		t.Fatalf("recorded delete options = %d, want 1", len(deletes))
	}
	if deletes[0].Preconditions == nil || deletes[0].Preconditions.UID == nil {
		t.Fatalf("the delete must carry the uid precondition, got %+v", deletes[0].Preconditions)
	}
	if got := string(*deletes[0].Preconditions.UID); got != "1111-2222-3333" {
		t.Errorf("precondition uid = %q, want the delivered 1111-2222-3333", got)
	}

	// like the real apiserver, the finalizer keeps the object alive with
	// a deletionTimestamp: the finalizer cleanup owns the removal
	stored := e.getStoredVMNetCfg()
	if stored.DeletionTimestamp == nil {
		t.Fatal("the swept binding must carry a deletionTimestamp after the delete")
	}

	// the deletion event re-enters the sync and runs the finalizer
	// cleanup: the stranded reservations are released and the object can
	// be removed
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, stored); err != nil {
		t.Fatalf("the finalizer cleanup of the swept binding failed: %s", err)
	}

	pool := e.getStoredPool()
	if _, still := pool.Status.IPv4.Allocated["10.0.0.1"]; still {
		t.Error("the ledger record of the swept binding must be released")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want the stranded claim released", used)
	}
	if e.dhcp.CheckLease(testMAC) {
		t.Error("the lease of the swept binding must be released")
	}
	if final := e.getStoredVMNetCfg(); len(final.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want removed so the object can be deleted", final.Finalizers)
	}
}

// a live vm must never have its binding swept: the vm controller rebuilds
// the vmnetcfg of a recreated vm, but deleting the live binding's object
// first would needlessly release and re-take its reservation.
func TestVMNetCfgOrphanSweepSkipsTheLiveVM(t *testing.T) {
	e := newTestEnv(t)
	seedStrandedBindingState(e)

	e.controller.verifyVM = func(namespace string, name string) (bool, error) {
		return true, nil
	}

	vmnetcfg := newOrphanVMNetCfg()
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if n := e.countRequests(http.MethodDelete, vmnetcfgMainPath); n != 0 {
		t.Errorf("delete requests = %d, want 0 for a live vm", n)
	}
	if stored := e.getStoredVMNetCfg(); stored.DeletionTimestamp != nil {
		t.Error("the binding of a live vm must not be marked for deletion")
	}
}

// a manually created vmnetcfg carries no cleanup finalizer, so the sweep
// never touches it: only the vm controller's own objects are known to be
// orphanable.
func TestVMNetCfgOrphanSweepSkipsManualObjects(t *testing.T) {
	e := newTestEnv(t)
	seedStrandedBindingState(e)

	e.controller.verifyVM = func(namespace string, name string) (bool, error) {
		return false, nil
	}

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if n := e.countRequests(http.MethodDelete, vmnetcfgMainPath); n != 0 {
		t.Errorf("delete requests = %d, want 0 for a manual object", n)
	}
	if stored := e.getStoredVMNetCfg(); stored.DeletionTimestamp != nil {
		t.Error("a manual vmnetcfg must not be marked for deletion")
	}
}

// a transient verification failure must not fail the reconciliation of a
// possibly live binding: the sweep is best-effort and the next resync
// retries it.
func TestVMNetCfgOrphanSweepSkipsOnTransientVerificationFailure(t *testing.T) {
	e := newTestEnv(t)
	seedStrandedBindingState(e)

	e.controller.verifyVM = func(namespace string, name string) (bool, error) {
		return false, errors.New("api read failed")
	}

	vmnetcfg := newOrphanVMNetCfg()
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the transient verification failure must not fail the sync: %s", err)
	}

	if n := e.countRequests(http.MethodDelete, vmnetcfgMainPath); n != 0 {
		t.Errorf("delete requests = %d, want 0 on an unverified vm", n)
	}
	if n := e.countMetricsByLabel(metricAppLogs, "loglevel", "warning"); n == 0 {
		t.Error("the skipped sweep must surface the transient verification failure as a warning")
	}
}

// without a verifier the sweep fails closed: nothing is swept, which keeps
// every controller without a kubevirt api (and the test default) on the
// safe side.
func TestVMNetCfgOrphanSweepFailsClosedWithoutVerifier(t *testing.T) {
	e := newTestEnv(t)
	seedStrandedBindingState(e)

	// no verifyVM seam is set: the controller fails closed

	vmnetcfg := newOrphanVMNetCfg()
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if n := e.countRequests(http.MethodDelete, vmnetcfgMainPath); n != 0 {
		t.Errorf("delete requests = %d, want 0 without a verifier", n)
	}
}

// the delete is preconditioned on the delivered uid: a replacement object
// created between the informer delivery and the delete is rejected by the
// apiserver instead of destroyed, and the sweep lets the regular
// reconciliation proceed.
func TestVMNetCfgOrphanSweepNeverDestroysAReplacement(t *testing.T) {
	e := newTestEnv(t)
	seedStrandedBindingState(e)

	e.controller.verifyVM = func(namespace string, name string) (bool, error) {
		e.api.mu.Lock()
		stored := e.api.vmnetcfgs[testNamespace+"/"+testVMNetCfgName]
		stored.UID = "9999-8888-7777"
		bumpResourceVersion(stored)
		e.api.mu.Unlock()
		return false, nil
	}

	// The replacement lands after the entrypoint's fresh read, inside VM
	// verification, so the delete must still enforce its UID precondition.
	vmnetcfg := newOrphanVMNetCfg()
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("replacement must invalidate the old owned decision")
	}

	// the precondition mismatch was rejected with a conflict: the stored
	// replacement survives untouched
	if n := e.countRequests(http.MethodDelete, vmnetcfgMainPath); n != 1 {
		t.Fatalf("delete requests = %d, want the attempted 1", n)
	}
	final := e.getStoredVMNetCfg()
	if final.DeletionTimestamp != nil {
		t.Error("the replacement object must not be marked for deletion")
	}
	if final.UID != "9999-8888-7777" {
		t.Errorf("stored uid = %q, want the replacement's 9999-8888-7777", string(final.UID))
	}
	if n := e.countMetricsByLabel(metricAppLogs, "loglevel", "error"); n == 0 {
		t.Error("the rejected delete must surface the failure as an error")
	}
}

func TestVMNetCfgEmptyManagedOrphanRecoversOnResync(t *testing.T) {
	e := newTestEnv(t)
	obj := newOrphanVMNetCfg()
	obj.Spec.NetworkConfig = nil
	e.seedVMNetCfg(obj)
	if err := e.indexer.Add(obj); err != nil {
		t.Fatal(err)
	}
	event := Event{key: testNamespace + "/" + testVMNetCfgName, action: UPDATE}
	parentExists := true
	var verificationErr error
	e.controller.verifyVM = func(namespace, name string) (bool, error) {
		if namespace != obj.Namespace || name != obj.Spec.VMName {
			t.Fatalf("wrong parent queried: %s/%s", namespace, name)
		}
		return parentExists, verificationErr
	}
	// A live parent and an uncertain parent both retain the empty config.
	for _, parentError := range []error{nil, errors.New("temporary parent read failure")} {
		verificationErr = parentError
		if err := e.controller.sync(event); err != nil {
			t.Fatal(err)
		}
		if got := e.getStoredVMNetCfg(); got.DeletionTimestamp != nil {
			t.Fatal("unverified orphan was deleted")
		}
		parentExists = false
	}
	verificationErr = nil
	e.api.vmnetcfgDeleteStatus = http.StatusInternalServerError
	if err := e.controller.sync(event); err != nil {
		t.Fatal(err)
	}
	if got := e.getStoredVMNetCfg(); got.DeletionTimestamp != nil {
		t.Fatal("failed delete unexpectedly succeeded")
	}
	e.api.vmnetcfgDeleteStatus = 0
	if err := e.controller.sync(event); err != nil {
		t.Fatal(err)
	}
	deleting := e.getStoredVMNetCfg()
	if deleting.DeletionTimestamp == nil {
		t.Fatal("normal resync stranded the empty managed orphan")
	}
	if err := e.indexer.Update(deleting); err != nil {
		t.Fatal(err)
	}
	if err := e.controller.sync(event); err != nil {
		t.Fatal(err)
	}
	if got := e.getStoredVMNetCfg(); len(got.Finalizers) != 0 {
		t.Fatalf("empty orphan cleanup retained finalizers: %v", got.Finalizers)
	}
}

func TestVMNetCfgEmptyOrphanSweepPreservesUnownedAndReplacementConfigs(t *testing.T) {
	for _, scenario := range []string{"manual", "foreign-spec", "foreign-status", "UID-replaced"} {
		t.Run(scenario, func(t *testing.T) {
			e := newTestEnv(t)
			obj := newOrphanVMNetCfg()
			obj.Spec.NetworkConfig = nil
			switch scenario {
			case "manual":
				obj.Finalizers = nil
			case "foreign-spec":
				obj.Spec.NetworkConfig = []kihv1.NetworkConfig{{NetworkName: "default/foreign", MACAddress: testMAC}}
			case "foreign-status":
				obj.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: "default/foreign", MACAddress: testMAC}}
			}
			e.seedVMNetCfg(obj)
			if err := e.indexer.Add(obj); err != nil {
				t.Fatal(err)
			}
			e.controller.verifyVM = func(_, _ string) (bool, error) {
				if scenario == "UID-replaced" {
					e.api.mu.Lock()
					e.api.vmnetcfgs[testNamespace+"/"+testVMNetCfgName].UID = "successor"
					e.api.mu.Unlock()
				}
				return false, nil
			}
			if err := e.controller.sync(Event{key: testNamespace + "/" + testVMNetCfgName, action: UPDATE}); err != nil {
				t.Fatal(err)
			}
			got := e.getStoredVMNetCfg()
			if got.DeletionTimestamp != nil {
				t.Fatalf("orphan sweep deleted %s config: %#v", scenario, got)
			}
			if scenario == "UID-replaced" && got.UID != "successor" {
				t.Fatalf("replacement UID changed: %s", got.UID)
			}
		})
	}
}

// a failed delete must not wedge the binding either: the sweep stays
// best-effort, the regular reconciliation proceeds and the next resync
// retries the delete.
func TestVMNetCfgOrphanSweepRetriesAFailedDelete(t *testing.T) {
	e := newTestEnv(t)
	seedStrandedBindingState(e)
	e.api.vmnetcfgDeleteStatus = http.StatusInternalServerError

	e.controller.verifyVM = func(namespace string, name string) (bool, error) {
		return false, nil
	}

	vmnetcfg := newOrphanVMNetCfg()
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err != nil {
		t.Fatalf("the failed delete must not fail the sync: %s", err)
	}

	if n := e.countRequests(http.MethodDelete, vmnetcfgMainPath); n != 1 {
		t.Fatalf("delete requests = %d, want the attempted 1", n)
	}
	if stored := e.getStoredVMNetCfg(); stored.DeletionTimestamp != nil {
		t.Error("the object must survive a failed delete")
	}
	if n := e.countMetricsByLabel(metricAppLogs, "loglevel", "error"); n == 0 {
		t.Error("the failed delete must surface the failure as an error")
	}
}
