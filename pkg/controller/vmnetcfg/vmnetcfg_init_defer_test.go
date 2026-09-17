package vmnetcfg

import (
	"context"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// a pending nic (lost spec write) whose lease and claim are still live
// must keep its reached state during the initialization replay: the
// mismatch branch may not tear down an address whose pool-status record
// write was the lost step, because the replay rebuilds ownership from that
// record and a freed address could be handed out a second time while the
// original guest keeps using it. the cleanup and fresh allocation belong to
// the post-gate requeue instead.
func TestInitPendingNicWithLiveLeaseKeepsReachedState(t *testing.T) {
	e := newTestEnv(t)

	// a controller on the environment's shared app status so the test can
	// flip the initialization phase after the deferral assertions
	startupGate := newTestGate(testNamespace + "/" + testVMNetCfgName)
	controller := NewController(context.Background(), newTestQueue(), newTestIndexer(), nil, e.cache, e.ipam, e.dhcp, e.metrics, e.client, e.appStatus, startupGate, e.scope, e.reconcileMu)

	e.addSubnet("10.0.0.1", "10.0.0.2")

	ownerRef := util.AllocationRef(testNamespace, testVMName, testMAC)
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", ownerRef); err != nil {
		t.Fatalf("seeding the claim: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
		t.Fatalf("seeding the lease: %s", err)
	}
	e.seedPool(nil)

	// the crash window: lease, claim and status record survived, the spec
	// write was lost with the restart
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Status = kihv1.VirtualMachineNetworkConfigStatus{
		NetworkConfig: []kihv1.NetworkConfigStatus{
			{MACAddress: testMAC, NetworkName: testNetwork, Status: "OK"},
		},
	}
	e.seedVMNetCfg(vmnetcfg)
	if err := controller.indexer.Add(vmnetcfg); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}

	event := Event{key: testNamespace + "/" + testVMNetCfgName, action: ADD}

	if err := controller.sync(event); err != nil {
		t.Fatalf("the deferred sync must not fail: %s", err)
	}
	// the deferred sync settles the object for the gate (it is requeued
	// once the gate opened); settling must not double count
	if startupGate.Settled() != 1 {
		t.Errorf("gate count = %d, want 1: the deferred sync settles the object once", startupGate.Settled())
	}
	if !e.dhcp.CheckLease(testMAC) || e.dhcp.GetLease(testMAC).ClientIP.String() != "10.0.0.1" {
		t.Fatal("the reached lease was torn down during the initialization replay")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1: the reached claim must survive the replay", used)
	}
	if stored := e.getStoredVMNetCfg(); len(stored.Status.NetworkConfig) != 1 || stored.Status.NetworkConfig[0].MACAddress != testMAC {
		t.Errorf("the persisted status record was rewritten during the replay: %+v", stored.Status.NetworkConfig)
	}

	// a repeated replay sync must stay deferred and must not hand the
	// reached address out a second time
	if err := controller.sync(event); err != nil {
		t.Fatalf("the repeated deferred sync must not fail: %s", err)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1: the replay must not allocate a second address", used)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("gate count = %d after the repeated deferred sync, want 1", startupGate.Settled())
	}

	// after the gate the object converges: the requeue performs the same
	// cleanup and allocation the deferred branch skipped
	e.appStatus.Store(APP_RUNNING)
	if err := controller.sync(event); err != nil {
		t.Fatalf("the post-gate sync failed: %s", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("gate count = %d, want 1 after the settled post-gate sync", startupGate.Settled())
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 1 || stored.Spec.NetworkConfig[0].IPAddress == "" {
		t.Errorf("the spec did not settle after the gate: %+v", stored.Spec.NetworkConfig)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 after convergence", used)
	}
}
