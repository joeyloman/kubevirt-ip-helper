package vm

import (
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// the divergent-tuple release of the nic cleanup: a quarantined allocation
// (a vmnetcfg sync whose durable object update failed after the lease, the
// claim and the ledger entry were applied) leaves the mac's live lease
// serving a tuple which the spec does not record anymore. the cleanup
// captures the own live lease before the by-mac deletion and releases the
// divergent tuple's claim and ledger entry through the same owner-validated
// flow, mirroring the vmnetcfg controller's capturedLease handling - the
// by-mac deletion alone would orphan the address for the rest of the era.
func TestCleanupNetworkInterfaceReleasesDivergentLeaseTuple(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)

	mac := "aa:bb:cc:00:00:01"
	specNetwork := "default/net-a"
	specIP := "10.0.0.11"
	servedNetwork := "default/net-b"
	servedIP := "10.0.0.12"

	// the quarantined allocation: the mac's live lease serves a tuple the
	// spec does not record (the dhcp lease reference carries no mac suffix)
	if err := c.dhcp.AddLease(mac, servedNetwork, servedIP, "ns1/vm1"); err != nil {
		t.Fatalf("adding the divergent lease: %v", err)
	}
	addSubnetWithOwnedIP(t, c.ipam, servedNetwork, servedIP, "ns1/vm1 ["+mac+"]")
	storePool(t, c, f, "pool-b", servedNetwork, map[string]string{
		servedIP: "ns1/vm1 [" + mac + "]",
	})

	// the spec tuple's own recorded state
	addSubnetWithOwnedIP(t, c.ipam, specNetwork, specIP, "ns1/vm1 ["+mac+"]")
	storePool(t, c, f, "pool-a", specNetwork, map[string]string{
		specIP: "ns1/vm1 [" + mac + "]",
	})

	vmnetcfg := &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1"},
		Spec:       kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1"},
	}
	if err := c.cleanupNetworkInterface(vmnetcfg, &kihv1.NetworkConfig{MACAddress: mac, NetworkName: specNetwork, IPAddress: specIP}); err != nil {
		t.Fatalf("cleanupNetworkInterface: %v", err)
	}

	if c.dhcp.CheckLease(mac) {
		t.Error("expected the divergent dhcp lease to be deleted")
	}
	if used := c.ipam.Used(specNetwork); used != 0 {
		t.Errorf("expected the spec ip released, used=%d", used)
	}
	if used := c.ipam.Used(servedNetwork); used != 0 {
		t.Errorf("expected the divergent tuple's claim released, used=%d", used)
	}

	poolB := f.storedPool("pool-b")
	if poolB == nil {
		t.Fatal("expected pool-b to remain stored")
	}
	if _, stillThere := poolB.Status.IPv4.Allocated[servedIP]; stillThere {
		t.Errorf("expected the divergent tuple's ledger record removed, got %v", poolB.Status.IPv4.Allocated)
	}

	poolA := f.storedPool("pool-a")
	if poolA == nil {
		t.Fatal("expected pool-a to remain stored")
	}
	if _, stillThere := poolA.Status.IPv4.Allocated[specIP]; stillThere {
		t.Errorf("expected the spec tuple's ledger record removed, got %v", poolA.Status.IPv4.Allocated)
	}
}
