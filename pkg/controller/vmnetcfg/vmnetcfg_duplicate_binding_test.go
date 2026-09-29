package vmnetcfg

// R10 fixtures and regressions of duplicate same-owner bindings: two
// stored VirtualMachineNetworkConfigs of the same namespace and the same
// spec vmname which record the same macaddress (admission is
// list-before-admit and fail-open, so concurrent creates and legacy
// alternative spellings both reach the store). the dhcp allocator keys
// its lease map on the parsed macaddress and the ownership layers key on
// the name-based owner reference (namespace/vmname), so the two objects
// are indistinguishable to the controller: honoring both contradictory
// specs oscillated the one lease between their addresses on every resync
// while both reported status OK.
//
// the deterministic ownership rule is: THE LIVE LEASE IS THE AUTHORITY.
// a binding whose requested assignment differs from its own live lease
// while a live sibling of the same vm records the lease's tuple in its
// spec is contested: it keeps a stable ERROR naming the sibling and the
// incumbent address, and the lease, the claim and the ledger record stay
// untouched - a possibly served address is never released to resolve a
// contest. deleting one of two agreeing duplicates must not free their
// shared address under the survivor's feet either: the survivor records
// the tuple, so the state stays with it and releases when it goes.

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

const (
	dupVMName = "vm-dup"

	dupBindingA = "binding-a"
	dupBindingB = "binding-b"

	// the same address in two spellings: the controller canonicalizes
	// the macaddress of a lease, so the two objects share one lease
	dupMACColon = "02:00:00:00:00:0a"
	dupMACDash  = "02-00-00-00-00-0A"

	dupIncumbentIP  = "10.0.0.5"
	dupChallengerIP = "10.0.0.6"
)

// newDuplicateBinding builds one binding of the duplicate-vm pair: both
// objects live in the test namespace and carry the same spec vmname.
func newDuplicateBinding(name string, ip string, mac string) *kihv1.VirtualMachineNetworkConfig {
	return &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      name,
		},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName: dupVMName,
			NetworkConfig: []kihv1.NetworkConfig{
				{IPAddress: ip, MACAddress: mac, NetworkName: testNetwork},
			},
		},
	}
}

// seedBinding stores a binding in the fake apiserver and in the
// informer store the controller enumerates for its siblings.
func (e *testEnv) seedBinding(obj *kihv1.VirtualMachineNetworkConfig) {
	e.t.Helper()

	e.seedVMNetCfg(obj)
	if err := e.indexer.Add(obj.DeepCopy()); err != nil {
		e.t.Fatalf("seeding the informer store with %s: %s", obj.Name, err)
	}
}

// storedBinding reads one binding back from the fake apiserver.
func (e *testEnv) storedBinding(name string) *kihv1.VirtualMachineNetworkConfig {
	e.t.Helper()

	e.api.mu.Lock()
	defer e.api.mu.Unlock()

	obj, ok := e.api.vmnetcfgs[testNamespace+"/"+name]
	if !ok {
		e.t.Fatalf("vmnetcfg %s not present in fake server", testNamespace+"/"+name)
	}

	return obj.DeepCopy()
}

// dropBinding removes a binding like the informer and the apiserver do
// once a deletion converged: no sibling lookup may see it anymore.
func (e *testEnv) dropBinding(name string) {
	e.t.Helper()

	e.api.mu.Lock()
	delete(e.api.vmnetcfgs, testNamespace+"/"+name)
	e.api.mu.Unlock()

	obj := &kihv1.VirtualMachineNetworkConfig{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name}}
	if err := e.indexer.Delete(obj); err != nil {
		e.t.Fatalf("dropping %s from the informer store: %s", name, err)
	}
}

// syncBinding reconciles one stored binding like a resync does.
func (e *testEnv) syncBinding(name string) error {
	e.t.Helper()

	return e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.storedBinding(name))
}

// markBindingDeleting seeds the deletion of one binding: the object
// carries the cleanup finalizer and a deletionTimestamp, so its
// reconciliation runs the finalizer cleanup.
func (e *testEnv) markBindingDeleting(name string) *kihv1.VirtualMachineNetworkConfig {
	e.t.Helper()

	stored := e.storedBinding(name)
	now := metav1.Now()
	stored.ObjectMeta.DeletionTimestamp = &now
	stored.ObjectMeta.Finalizers = []string{vmnetcfgCleanupFinalizer}
	e.seedVMNetCfg(stored)

	return stored
}

// assertOneLease asserts that exactly the given address is served for
// the shared macaddress and that its reservation layers did not churn:
// one ipam claim and one ledger record under the name-based owner
// reference of the duplicate vm.
func (e *testEnv) assertOneLease(t *testing.T, ip string) {
	t.Helper()

	lease := e.dhcp.GetLease(dupMACColon)
	if lease.ClientIP == nil || lease.ClientIP.String() != ip {
		t.Errorf("lease = %v, want it to keep serving %s", lease.ClientIP, ip)
	}
	if used := e.ipam.Used(testNetwork); used != 1 {
		t.Errorf("ipam used = %d, want exactly the one live reservation", used)
	}
	if ref, ok := e.getStoredPool().Status.IPv4.Allocated[ip]; !ok || ref != "default/"+dupVMName+" ["+dupMACColon+"]" {
		t.Errorf("ledger = %v, want exactly the one record of %s owned by %s", e.getStoredPool().Status.IPv4.Allocated, ip, dupVMName)
	}
}

// bindingNic returns the single status entry of one stored binding.
func (e *testEnv) bindingNic(t *testing.T, name string) kihv1.NetworkConfigStatus {
	t.Helper()

	stored := e.storedBinding(name)
	if len(stored.Status.NetworkConfig) != 1 {
		t.Fatalf("binding %s carries %d status entries, want one", name, len(stored.Status.NetworkConfig))
	}

	return stored.Status.NetworkConfig[0]
}

// TestConcurrentlyAdmittedDuplicatesConvergeDeterministically is the R10
// controller fixture (fixtures 2 and 3): two bindings of the same vm and
// macaddress were both admitted and both are stored - one requests the
// incumbent address explicitly, the other a different one. whoever
// reconciles first owns the live lease; the other is contested and must
// hold a stable ERROR naming the sibling and the incumbent address while
// the lease, the claim and the ledger record never move. the pre-fix
// behavior honored both contradictory specs: every resync ran the
// destructive migration cleanup of the other binding's address and
// re-allocated the own one, so the lease oscillated between the two
// addresses with full claim and record churn while both statuses stayed
// OK.
func TestConcurrentlyAdmittedDuplicatesConvergeDeterministically(t *testing.T) {
	for _, tt := range []struct {
		name   string
		first  string
		second string
	}{
		{"the incumbent binding syncs first", dupBindingA, dupBindingB},
		{"the challenger binding syncs first", dupBindingB, dupBindingA},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.appStatus.Store(APP_RUNNING)
			e.addSubnet(dupIncumbentIP, dupChallengerIP)
			e.seedPool(nil)

			e.seedBinding(newDuplicateBinding(dupBindingA, dupIncumbentIP, dupMACColon))
			e.seedBinding(newDuplicateBinding(dupBindingB, dupChallengerIP, dupMACDash))

			owner, challenger := tt.first, tt.second
			ownerIP, challengerIP := dupIncumbentIP, dupChallengerIP
			if owner == dupBindingB {
				ownerIP, challengerIP = dupChallengerIP, dupIncumbentIP
			}

			if err := e.syncBinding(tt.first); err != nil {
				t.Fatalf("first sync of %s: %v", tt.first, err)
			}
			if err := e.syncBinding(tt.second); err != nil {
				t.Fatalf("second sync of %s: %v", tt.second, err)
			}

			e.assertOneLease(t, ownerIP)

			if nic := e.bindingNic(t, owner); nic.Status != "OK" {
				t.Errorf("owner status = %+v, want OK", nic)
			}

			nic := e.bindingNic(t, challenger)
			if nic.Status != "ERROR" {
				t.Errorf("challenger status = %+v, want the stable ERROR of the contested binding", nic)
			}
			if !strings.Contains(nic.Message, owner) || !strings.Contains(nic.Message, ownerIP) {
				t.Errorf("challenger message = %q, want it to name the sibling %s and the incumbent address %s", nic.Message, owner, ownerIP)
			}
			if stored := e.storedBinding(challenger); stored.Spec.NetworkConfig[0].IPAddress != challengerIP {
				t.Errorf("challenger spec = %+v, want its explicit request %s untouched", stored.Spec.NetworkConfig, challengerIP)
			}

			// alternating resyncs in both orders must not move the lease,
			// the claim, the record or either verdict
			challengerMessage := nic.Message
			for _, round := range [][2]string{{challenger, owner}, {owner, challenger}, {challenger, owner}} {
				for _, name := range round {
					if err := e.syncBinding(name); err != nil {
						t.Fatalf("resync of %s: %v", name, err)
					}
				}

				e.assertOneLease(t, ownerIP)

				if nic := e.bindingNic(t, owner); nic.Status != "OK" {
					t.Errorf("owner status after resyncs = %+v, want OK", nic)
				}
				nic = e.bindingNic(t, challenger)
				if nic.Status != "ERROR" || nic.Message != challengerMessage {
					t.Errorf("challenger status after resyncs = %+v, want the identical stable ERROR %q", nic, challengerMessage)
				}
			}
		})
	}
}

// TestContestedBindingRecoversWhenSiblingDeleted pins the recovery of
// the contested binding: deleting the incumbent releases its address
// (the challenger records a different one, so nothing is held for it)
// and the challenger's next sync converges to its own requested address
// with the ERROR cleared.
func TestContestedBindingRecoversWhenSiblingDeleted(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet(dupIncumbentIP, dupChallengerIP)
	e.seedPool(nil)

	e.seedBinding(newDuplicateBinding(dupBindingA, dupIncumbentIP, dupMACColon))
	e.seedBinding(newDuplicateBinding(dupBindingB, dupChallengerIP, dupMACDash))

	if err := e.syncBinding(dupBindingA); err != nil {
		t.Fatalf("owner sync: %v", err)
	}
	if err := e.syncBinding(dupBindingB); err != nil {
		t.Fatalf("challenger sync: %v", err)
	}
	if nic := e.bindingNic(t, dupBindingB); nic.Status != "ERROR" {
		t.Fatalf("challenger status = %+v, want the contested ERROR before the recovery", nic)
	}

	// clear the conflict: the incumbent binding is deleted and converges
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.markBindingDeleting(dupBindingA)); err != nil {
		t.Fatalf("deletion sync of the incumbent: %v", err)
	}
	if stored := e.storedBinding(dupBindingA); len(stored.ObjectMeta.Finalizers) != 0 {
		t.Errorf("incumbent finalizers = %v, want the deletion to converge", stored.ObjectMeta.Finalizers)
	}
	e.dropBinding(dupBindingA)

	// the challenger re-attempts its failed interface and converges
	if err := e.syncBinding(dupBindingB); err != nil {
		t.Fatalf("challenger recovery sync: %v", err)
	}

	e.assertOneLease(t, dupChallengerIP)
	if nic := e.bindingNic(t, dupBindingB); nic.Status != "OK" {
		t.Errorf("challenger status = %+v, want OK after the conflict cleared", nic)
	}
	if stored := e.storedBinding(dupBindingB); stored.Spec.NetworkConfig[0].IPAddress != dupChallengerIP {
		t.Errorf("challenger spec = %+v, want its own requested address", stored.Spec.NetworkConfig)
	}
}

// TestEmptyRequestedDuplicatesConvergeThroughAdoption pins the unchanged
// F02 behavior for duplicates: two bindings without an explicit address
// do not contest anything - the second one adopts the lease of the first
// and both converge on the same address with one reservation.
func TestEmptyRequestedDuplicatesConvergeThroughAdoption(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet(dupIncumbentIP, dupIncumbentIP)
	e.seedPool(nil)

	e.seedBinding(newDuplicateBinding(dupBindingA, "", dupMACColon))
	e.seedBinding(newDuplicateBinding(dupBindingB, "", dupMACDash))

	if err := e.syncBinding(dupBindingA); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := e.syncBinding(dupBindingB); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	// both bindings record the same address and both are OK: the second
	// one adopted the quarantined-style lease of the first instead of
	// contesting it or allocating a second address
	for _, name := range []string{dupBindingA, dupBindingB} {
		stored := e.storedBinding(name)
		if stored.Spec.NetworkConfig[0].IPAddress != dupIncumbentIP {
			t.Errorf("binding %s spec = %+v, want the adopted %s", name, stored.Spec.NetworkConfig, dupIncumbentIP)
		}
		if nic := e.bindingNic(t, name); nic.Status != "OK" {
			t.Errorf("binding %s status = %+v, want OK", name, nic)
		}
	}

	e.assertOneLease(t, dupIncumbentIP)
}

// TestDeletingOneAgreeingDuplicateKeepsTheSurvivorsAddress is the R10
// deletion fixture (fixture 4): both bindings converged on the identical
// recorded address - the agreed state a contested binding reaches when
// its sibling is edited to agree. deleting one of them must not release
// the shared address under the survivor's feet: the survivor records the
// tuple, so the lease, the claim and the ledger record stay with it and
// remain unavailable to a foreign binding until the survivor goes too.
func TestDeletingOneAgreeingDuplicateKeepsTheSurvivorsAddress(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	e.addSubnet(dupIncumbentIP, dupIncumbentIP)
	e.seedPool(nil)

	e.seedBinding(newDuplicateBinding(dupBindingA, dupIncumbentIP, dupMACColon))
	e.seedBinding(newDuplicateBinding(dupBindingB, dupIncumbentIP, dupMACDash))

	if err := e.syncBinding(dupBindingA); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := e.syncBinding(dupBindingB); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	for _, name := range []string{dupBindingA, dupBindingB} {
		if nic := e.bindingNic(t, name); nic.Status != "OK" {
			t.Fatalf("binding %s status = %+v, want the agreed OK state", name, nic)
		}
	}

	// delete one of the two agreeing duplicates: its cleanup must leave
	// the shared state to the survivor and still converge
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.markBindingDeleting(dupBindingA)); err != nil {
		t.Fatalf("deletion sync: %v", err)
	}
	if stored := e.storedBinding(dupBindingA); len(stored.ObjectMeta.Finalizers) != 0 {
		t.Errorf("deleted binding finalizers = %v, want the deletion to converge", stored.ObjectMeta.Finalizers)
	}

	// the possibly served address must not become allocatable to a
	// foreign binding while the survivor still records it
	if ip, err := e.ipam.AllocateIP(testNetwork, "default/other-vm"); err == nil {
		t.Fatalf("a foreign binding received %s: the address of the surviving binding became allocatable", ip)
	}
	e.assertOneLease(t, dupIncumbentIP)

	// the survivor keeps serving and its resync stays converged
	if err := e.syncBinding(dupBindingB); err != nil {
		t.Fatalf("survivor resync: %v", err)
	}
	if nic := e.bindingNic(t, dupBindingB); nic.Status != "OK" {
		t.Errorf("survivor status = %+v, want OK", nic)
	}

	// once the survivor is deleted as well - and the deleted duplicate is
	// gone - the address releases for real
	e.dropBinding(dupBindingA)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, e.markBindingDeleting(dupBindingB)); err != nil {
		t.Fatalf("survivor deletion sync: %v", err)
	}

	if e.dhcp.CheckLease(dupMACColon) {
		t.Error("the lease must be gone once the last binding recording the tuple is deleted")
	}
	if used := e.ipam.Used(testNetwork); used != 0 {
		t.Errorf("ipam used = %d, want the released reservation", used)
	}
	if len(e.getStoredPool().Status.IPv4.Allocated) != 0 {
		t.Errorf("ledger = %v, want it empty after the last binding is deleted", e.getStoredPool().Status.IPv4.Allocated)
	}
	if ip, err := e.ipam.AllocateIP(testNetwork, "default/other-vm"); err != nil || ip != dupIncumbentIP {
		t.Errorf("foreign allocation = (%s, %v), want the released %s", ip, err, dupIncumbentIP)
	}
}
