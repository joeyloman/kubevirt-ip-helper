package vmnetcfg

import (
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	missingNetName = testNetwork
	missingNetSub  = "10.0.2.0/29"
	healthyNet2    = "default/net-test-2"
	healthySub2    = "10.0.1.0/29"
)

func seedHealthyPool(e *testEnv) {
	e.t.Helper()
	if err := e.ipam.NewSubnet(healthyNet2, healthySub2, "10.0.1.1", "10.0.1.2"); err != nil {
		e.t.Fatal(err)
	}
	e.seedPoolWith(&kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ippool-test-2"},
		Spec:       kihv1.IPPoolSpec{NetworkName: healthyNet2, IPv4Config: kihv1.IPv4Config{Subnet: healthySub2, ServerIP: "10.0.1.1"}},
	})
}

func mixedVMNetCfg() *kihv1.VirtualMachineNetworkConfig {
	obj := newVMNetCfg("10.0.2.5", testMAC)
	obj.Spec.NetworkConfig = append(obj.Spec.NetworkConfig, kihv1.NetworkConfig{IPAddress: "10.0.1.2", MACAddress: testMAC2, NetworkName: healthyNet2})
	return obj
}

// A missing pool on A cannot stop B from protecting its durable assignment.
func TestVMNetCfgMissingPoolDoesNotBlockLaterDurableInterface(t *testing.T) {
	a := newTestEnv(t)
	b := networkPeer(t, a, "net-test-2")
	b.appStatus.Store(APP_INIT)
	seedHealthyPool(b)
	obj := mixedVMNetCfg()
	a.seedVMNetCfg(obj)
	if err := a.controller.updateVirtualMachineNetworkConfig(ADD, obj); err == nil {
		t.Fatal("owned missing pool must fail its own helper")
	}
	if err := b.controller.updateVirtualMachineNetworkConfig(ADD, obj); err != nil {
		t.Fatal(err)
	}
	if lease := b.dhcp.GetLease(testMAC2); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.1.2" {
		t.Fatalf("healthy helper failed to restore: %+v", lease)
	}
	if _, err := b.ipam.GetIP(healthyNet2, "10.0.1.2"); err == nil {
		t.Fatal("healthy durable address became reissuable")
	}
	if a.dhcp.CheckLease(testMAC) || a.dhcp.CheckLease(testMAC2) || b.dhcp.CheckLease(testMAC) {
		t.Fatal("helper crossed the network ownership boundary")
	}
	if got := a.getStoredVMNetCfg().Spec.NetworkConfig[0]; got != obj.Spec.NetworkConfig[0] {
		t.Fatalf("missing foreign row changed: %+v", got)
	}
}

func TestVMNetCfgMissingPoolInterfaceRecoversOncePoolRegisters(t *testing.T) {
	e := newTestEnv(t)
	obj := mixedVMNetCfg()
	e.seedVMNetCfg(obj)
	if err := e.controller.updateVirtualMachineNetworkConfig(ADD, obj); err == nil {
		t.Fatal("missing owned pool must fail")
	}
	if err := e.ipam.NewSubnet(missingNetName, missingNetSub, "10.0.2.5", "10.0.2.6"); err != nil {
		t.Fatal(err)
	}
	e.seedPoolWith(&kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ippool-test-1"},
		Spec:       kihv1.IPPoolSpec{NetworkName: missingNetName, IPv4Config: kihv1.IPv4Config{Subnet: missingNetSub, ServerIP: "10.0.2.1"}},
	})
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err != nil {
		t.Fatal(err)
	}
	if lease := e.dhcp.GetLease(testMAC); lease.ClientIP == nil || lease.ClientIP.String() != "10.0.2.5" {
		t.Fatalf("registered pool failed to restore owner: %+v", lease)
	}
	if _, err := e.ipam.GetIP(missingNetName, "10.0.2.5"); err == nil {
		t.Fatal("restored address became reissuable")
	}
	stored := e.getStoredVMNetCfg()
	statuses := e.scope.FilterStatus(stored.Namespace, stored.Status.NetworkConfig)
	if len(statuses) != 1 || statuses[0].Status != "OK" || e.dhcp.CheckLease(testMAC2) {
		t.Fatalf("recovery failed or touched foreign NIC: %v", statuses)
	}
}

func TestVMNetCfgStartupGateMissingPoolKeepsLaterInterfacesProtected(t *testing.T) {
	e, controller, startupGate := newGateTestEnv(t)
	b := networkPeer(t, e, "net-test-2")
	seedHealthyPool(b)
	obj := mixedVMNetCfg()
	e.seedVMNetCfg(obj)
	if err := b.controller.updateVirtualMachineNetworkConfig(ADD, obj); err != nil {
		t.Fatal(err)
	}
	if err := controller.indexer.Add(obj); err != nil {
		t.Fatal(err)
	}
	if err := controller.sync(Event{key: testNamespace + "/" + testVMNetCfgName, action: ADD}); err == nil {
		t.Fatal("owned missing pool must fail")
	}
	if startupGate.Settled() != 1 {
		t.Fatal("missing-pool startup object did not settle")
	}
	if b.ipam.Used(healthyNet2) != 1 || !b.dhcp.CheckLease(testMAC2) {
		t.Fatal("other helper's durable binding changed when the gate settled")
	}
}
