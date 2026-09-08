package vmnetcfg

import (
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// A failed sync must never unwind the restore of an address which the stored
// spec already records: the guest of the existing assignment keeps using the
// address, so releasing its lease and ipam reservation would hand it to
// another vm while the durable object still claims it.

// a later nic whose pool is not in the cache must fail the sync without
// freeing the restored durable address of an earlier nic
func TestVMNetCfgFailedSyncKeepsRestoredDurableAllocation(t *testing.T) {
	e := newTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)

	// restart scenario: the lease map is empty and both nics carry already
	// persisted addresses; the pool of the second nic is not registered
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC, NetworkName: testNetwork, IPAddress: "10.0.0.1"},
		{MACAddress: testMAC2, NetworkName: "net-missing", IPAddress: "10.0.0.2"},
	}
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the missing pool of the second nic to fail the sync")
	}
	if !strings.Contains(err.Error(), "does not exists in cache") {
		t.Errorf("error = %q, want the cache miss message", err)
	}

	// the restored assignment of the first nic must stay fully applied
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("restored lease = %v, want 10.0.0.1 kept by the failed sync", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the durable address must stay reserved)", used)
	}

	// the pool status entry written during the restore must survive too
	pool := e.getStoredPool()
	if got := pool.Status.IPv4.Allocated["10.0.0.1"]; got == "" {
		t.Error("the restored allocation must stay recorded in the pool status")
	}

	// the freed address must not be handed to a competing vm: a fresh
	// allocation on the same network must skip the reserved address
	hijacker := newVMNetCfg("", "02:00:00:00:00:99")
	hijacker.Name = "vm-hijacker"
	hijacker.Spec.VMName = "vm-hijacker"
	e.seedVMNetCfg(hijacker)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, hijacker); err != nil {
		t.Fatalf("competing sync: %s", err)
	}

	e.api.mu.Lock()
	stored := e.api.vmnetcfgs[testNamespace+"/vm-hijacker"].DeepCopy()
	e.api.mu.Unlock()

	if got := stored.Spec.NetworkConfig[0].IPAddress; got != "10.0.0.2" {
		t.Errorf("competing vm received %q, want 10.0.0.2 (10.0.0.1 must stay reserved for its owner)", got)
	}
}

// a failing pool status update of the only nic must not release the restored
// durable address either: the reservation protects the running guest even
// while the status write keeps failing
func TestVMNetCfgPoolStatusFailureKeepsRestoredDurableAllocation(t *testing.T) {
	e := newTestEnv(t)

	e.addSubnet("10.0.0.1", "10.0.0.2")
	e.seedPool(nil)
	e.api.poolStatusPutCode = http.StatusInternalServerError

	vmnetcfg := newVMNetCfg("10.0.0.1", testMAC)
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the pool status failure to fail the sync")
	}
	if !strings.Contains(err.Error(), "cannot update the IPPool") {
		t.Errorf("error = %q, want the pool status rejection", err)
	}

	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("restored lease = %v, want 10.0.0.1 kept despite the status failure", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the durable address must stay reserved)", used)
	}
}

// within one sync the rollback must distinguish the allocation kinds: the
// fresh allocation of an earlier nic is unwound while the restored durable
// assignment of another nic stays applied
func TestVMNetCfgFailedSyncUnwindsOnlyFreshAllocations(t *testing.T) {
	e := newTestEnv(t)

	secondNetwork := "net-b"
	const secondPoolName = "ippool-b"

	e.addSubnet("10.0.0.1", "10.0.0.1")
	e.seedPool(nil)

	poolB := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: secondPoolName},
		Spec: kihv1.IPPoolSpec{
			NetworkName: secondNetwork,
			IPv4Config:  kihv1.IPv4Config{Subnet: testSubnet, ServerIP: "10.0.0.1"},
		},
	}
	e.seedPoolWith(poolB)
	if err := e.ipam.NewSubnet(secondNetwork, testSubnet, "10.0.0.1", "10.0.0.1"); err != nil {
		t.Fatalf("adding second subnet: %s", err)
	}

	// the first nic restores a durable address, the second nic asks for a
	// fresh one and the third nic fails on its missing pool
	vmnetcfg := newVMNetCfg("", testMAC)
	vmnetcfg.Spec.NetworkConfig = []kihv1.NetworkConfig{
		{MACAddress: testMAC, NetworkName: testNetwork, IPAddress: "10.0.0.1"},
		{MACAddress: testMAC2, NetworkName: secondNetwork},
		{MACAddress: "02:00:00:00:00:03", NetworkName: "net-missing"},
	}
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the missing pool of the third nic to fail the sync")
	}

	// the durable restore stays applied
	lease := e.dhcp.GetLease(testMAC)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.1" {
		t.Errorf("restored lease = %v, want 10.0.0.1 kept by the failed sync", lease.ClientIP)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("durable ipam used = %d, want 1", used)
	}

	// the fresh allocation of the second nic is unwound
	if e.dhcp.CheckLease(testMAC2) {
		t.Error("the fresh lease of the second nic must be released by the unwind")
	}
	if used := e.ipam.Used(secondNetwork); used != 0 {
		t.Errorf("fresh ipam used = %d, want 0 after the unwind", used)
	}

	e.api.mu.Lock()
	poolBStored := e.api.ippools[secondPoolName].DeepCopy()
	e.api.mu.Unlock()
	if got := poolBStored.Status.IPv4.Allocated["10.0.0.1"]; got != "" {
		t.Errorf("fresh status entry = %q, want removed by the unwind", got)
	}
}
