package vmnetcfg

// P0-1 regression tests: an interface whose network has no registered pool
// must not block the restoration of the other interfaces of the same
// vmnetcfg, and the startup gate must not open with a valid assignment on a
// registered pool left unprotected. The durable spec entry of the pool-less
// interface stays untouched and its assignment is restored by a resynced
// retry once the pool registers.

import (
	"strings"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	missingNetName = "net-missing"
	missingNetSub  = "10.0.2.0/29"
	healthyNet2    = "net-test-2"
	healthySub2    = "10.0.1.0/29"
)

// seedHealthyPool registers the second (healthy) pool with its ipam subnet in
// the behavior environment.
func seedHealthyPool(e *testEnv) {
	e.t.Helper()

	if err := e.ipam.NewSubnet(healthyNet2, healthySub2, "10.0.1.1", "10.0.1.2"); err != nil {
		e.t.Fatalf("adding healthy subnet: %s", err)
	}
	pool2 := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ippool-test-2"},
		Spec: kihv1.IPPoolSpec{
			NetworkName: healthyNet2,
			IPv4Config:  kihv1.IPv4Config{Subnet: healthySub2, ServerIP: "10.0.1.1"},
		},
	}
	e.seedPoolWith(pool2)
}

// mixedVMNetCfg builds a vmnetcfg with a pool-less first interface and a
// durable second interface on the healthy pool.
func mixedVMNetCfg() *kihv1.VirtualMachineNetworkConfig {
	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testVMNetCfgName},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName: testVMName,
			NetworkConfig: []kihv1.NetworkConfig{
				{IPAddress: "10.0.2.5", MACAddress: "02:00:00:00:00:01", NetworkName: missingNetName},
				{IPAddress: "10.0.1.2", MACAddress: "02:00:00:00:00:02", NetworkName: healthyNet2},
			},
		},
	}
	return vmnetcfg
}

// The pool-less interface must fail the sync but the later durable interface
// stays restored and its recorded address is not reissuable.
func TestVMNetCfgMissingPoolDoesNotBlockLaterDurableInterface(t *testing.T) {
	e := newTestEnv(t)
	seedHealthyPool(e)
	vmnetcfg := mixedVMNetCfg()
	e.seedVMNetCfg(vmnetcfg)

	err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg)
	if err == nil {
		t.Fatal("want the pool lookup failure to fail the sync")
	}
	if !strings.Contains(err.Error(), "does not exists in cache") {
		t.Errorf("error = %q, want the cache miss message", err)
	}

	// the later durable assignment is protected by this same sync
	if !e.dhcp.CheckLease("02:00:00:00:00:02") {
		t.Fatal("the later interface's lease must be restored")
	}
	if got := e.dhcp.GetLease("02:00:00:00:00:02").ClientIP.String(); got != "10.0.1.2" {
		t.Errorf("lease ip = %s, want 10.0.1.2", got)
	}
	if used := e.ipam.Used(healthyNet2); used != 1 {
		t.Errorf("healthy network used = %d, want 1", used)
	}
	if _, err := e.ipam.GetIP(healthyNet2, "10.0.1.2"); err == nil {
		t.Fatal("the recorded address must not be reissuable while the vm still claims it")
	}
	pool2 := e.api.ippools["ippool-test-2"].DeepCopy()
	if got := pool2.Status.IPv4.Allocated["10.0.1.2"]; got != legacyVMRef+" [02:00:00:00:00:02]" {
		t.Errorf("allocated[10.0.1.2] = %q, want the recorded owner", got)
	}

	// the pool-less interface got nothing: no lease, no allocation
	if e.dhcp.CheckLease("02:00:00:00:00:01") {
		t.Error("the pool-less interface must not receive a lease")
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 2 {
		t.Fatalf("spec networkconfig = %d entries, want both preserved", len(stored.Spec.NetworkConfig))
	}
	if got := stored.Spec.NetworkConfig[0]; got.IPAddress != "10.0.2.5" || got.NetworkName != missingNetName {
		t.Errorf("pool-less spec entry = %+v, want the durable assignment preserved", got)
	}
}

// Once the missing pool registers, the resynced retry restores the
// pool-less interface and the object converges.
func TestVMNetCfgMissingPoolInterfaceRecoversOncePoolRegisters(t *testing.T) {
	e := newTestEnv(t)
	seedHealthyPool(e)
	vmnetcfg := mixedVMNetCfg()
	e.seedVMNetCfg(vmnetcfg)

	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, vmnetcfg); err == nil {
		t.Fatal("want the first sync to fail while the pool is missing")
	}

	// the pool for the first interface becomes live (registration equivalent)
	if err := e.ipam.NewSubnet(missingNetName, missingNetSub, "10.0.2.5", "10.0.2.6"); err != nil {
		t.Fatalf("adding missing-net subnet: %s", err)
	}
	pool1 := &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ippool-test-1"},
		Spec: kihv1.IPPoolSpec{
			NetworkName: missingNetName,
			IPv4Config:  kihv1.IPv4Config{Subnet: missingNetSub, ServerIP: "10.0.2.1"},
		},
	}
	e.seedPoolWith(pool1)

	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, vmnetcfg); err != nil {
		t.Fatalf("the recovered sync must succeed: %s", err)
	}

	if !e.dhcp.CheckLease("02:00:00:00:00:01") {
		t.Fatal("the pool-less interface must get its lease after the pool registered")
	}
	if got := e.dhcp.GetLease("02:00:00:00:00:01").ClientIP.String(); got != "10.0.2.5" {
		t.Errorf("lease ip = %s, want the durable 10.0.2.5", got)
	}
	if used := e.ipam.Used(missingNetName); used != 1 {
		t.Errorf("missing-net used = %d, want 1", used)
	}
	if _, err := e.ipam.GetIP(missingNetName, "10.0.2.5"); err == nil {
		t.Fatal("the restored address must not be reissuable")
	}
	// the interface made it into the status as OK
	stored := e.getStoredVMNetCfg()
	for _, st := range stored.Status.NetworkConfig {
		if st.NetworkName == missingNetName {
			if st.Status != "OK" {
				t.Errorf("recovered interface status = %q, want OK", st.Status)
			}
			return
		}
	}
	t.Fatal("recovered interface status entry missing after convergence")
}

// The startup gate counts a pool-less object as settled (a pool which is not
// registered cannot be served or reissued from), but only after the later
// interface on a registered pool is protected by this same sync.
func TestVMNetCfgStartupGateMissingPoolKeepsLaterInterfacesProtected(t *testing.T) {
	e, controller, count := newGateTestEnv(t)
	seedHealthyPool(e)
	vmnetcfg := mixedVMNetCfg()
	e.seedVMNetCfg(vmnetcfg)
	if err := controller.indexer.Add(vmnetcfg); err != nil {
		t.Fatalf("seeding indexer: %s", err)
	}

	err := controller.sync(Event{key: testNamespace + "/" + testVMNetCfgName, action: ADD})
	if err == nil {
		t.Fatal("want the sync to fail while the pool is missing")
	}
	if *count != 1 {
		t.Fatalf("gate count = %d, want 1 (settled classification for the missing pool)", *count)
	}
	if used := e.ipam.Used(healthyNet2); used != 1 {
		t.Fatalf("healthy network used = %d right after the gate opened, want 1 (later interface protected)", used)
	}
	if !e.dhcp.CheckLease("02:00:00:00:00:02") {
		t.Fatal("later interface lease must exist when the gate opens")
	}
}
