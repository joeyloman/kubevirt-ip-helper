package vmnetcfg

// P2-1 regression tests: the lease-idempotent path must also repair the
// durable pool ownership record. A status write failure after the lease was
// applied kept the reservation (protected in ipam/dhcp) but left the pool
// status record missing; the next successful reconcile silently skipped it
// forever (lease-present, used=1, pool owner="").

import (
	"errors"
	"net/http"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// TestVMNetCfgLeaseIdempotencyRepairsPoolStatus: after a transient status
// write failure the recovered reconcile must rebuild the missing pool
// status entry, and a steady-state reconcile must be read-only on it.
func TestVMNetCfgLeaseIdempotencyRepairsPoolStatus(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "OK", Message: "IP address successfully allocated"},
	}
	e.seedVMNetCfg(vmnetcfg)

	// the status write of the first sync fails after the lease was applied:
	// the reservation is retained, the pool record stays missing
	e.api.poolStatusPutCode = http.StatusInternalServerError
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("want the transient status failure to fail the sync")
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Fatal("the lease must be retained")
	}
	if pool := e.getStoredPool(); len(pool.Status.IPv4.Allocated) != 0 {
		t.Fatalf("pool status must miss the record after the failure: %v", pool.Status.IPv4.Allocated)
	}

	// the recovered reconcile takes the lease-idempotent path and must
	// repair the pool ownership record
	e.api.poolStatusPutCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the recovered sync must succeed: %s", err)
	}
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated["10.0.0.1"]; got != testNamespace+"/"+testVMName+" ["+testMAC+"]" {
		t.Errorf("pool status entry = %q, want the repaired owner record", got)
	}
	if pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 0 {
		t.Errorf("used/available = %d/%d, want 1/0 after the repair", pool.Status.IPv4.Used, pool.Status.IPv4.Available)
	}

	// a steady-state reconcile of the same assignment must not keep writing
	// the pool status: the matching owner entry is confirmed read-only
	puts := e.countRequests(http.MethodPut, ippoolStatusPath)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the steady-state sync must succeed: %s", err)
	}
	if got := e.countRequests(http.MethodPut, ippoolStatusPath); got != puts {
		t.Errorf("steady-state sync must be read-only on the pool status: PUTs %d -> %d", puts, got)
	}
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("the lease must remain")
	}
}

// With the entry recorded for another owner the repair cannot converge: the
// sync must fail visibly (not serve silently under a leftover foreign claim)
// while keeping the lease and reservation intact for the retried reconcile.
func TestVMNetCfgLeaseIdempotencyForeignOwnerFailsVisibly(t *testing.T) {
	e := newTestEnv(t)
	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(map[string]string{"10.0.0.1": "other-ns/other-vm [02:00:00:00:00:99]"})
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
		t.Fatalf("occupying the claimed address: %s", err)
	}
	if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", legacyVMRef); err != nil {
		t.Fatalf("seeding our lease: %s", err)
	}

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	vmnetcfg.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{MACAddress: testMAC, NetworkName: testNetwork, Status: "OK", Message: "IP address successfully allocated"},
	}
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg)
	if err == nil {
		t.Fatal("want the foreign-owned record to fail the sync")
	}
	if !errors.Is(err, util.ErrForeignOwner) {
		t.Errorf("error = %v, want the ErrForeignOwner classification", err)
	}
	// the failed repair must not unwind the served lease or reservation
	if !e.dhcp.CheckLease(testMAC) {
		t.Error("the own lease must remain")
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1", used)
	}
	// the foreign record stays untouched
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"]; got != "other-ns/other-vm [02:00:00:00:00:99]" {
		t.Errorf("foreign record = %q, want preserved", got)
	}
}
