package ippool

// Recovery regression tests: a pool which registers again after the
// startup gate dropped its retries (an UPDATE resync recovery of an
// application already in APP_RUNNING) must protect every durable claim of
// its network before the registration publishes the allocator. The
// durable claims come from the ownership ledger the pool status survived
// with and from the recorded assignments of the vmnetcfg objects - the
// ledger alone is not a complete inventory, because main could persist a
// vmnetcfg assignment after the pool status write failed. The spec claims
// admit through the same rules the binding replay applies (a hijack
// guarded request never claims, an established assignment outranks a bare
// request), they are pinned in the allocator only - the restoring binding
// writes its own ledger entry - and every pin is re-verified against a
// fresh read of its object before the pool is published, so a claim whose
// nic was removed while the pool was still unpublished is dropped instead
// of being published as an orphan record.
//
// The tests execute the complete production registration path through the
// same registerPoolWithTeardown the sync runs: the up-front validation,
// the allocator registration, the exclude pass, the claim protection, the
// status rebuild, the metrics reset and the cache publication, and the
// teardown of a failed attempt. The host-level steps - adding the server
// ip to the bind interface and starting the dhcp listener - run through
// their test seams (a stubbed nic mutation and a no-op listener), so the
// fixtures exercise the real registration sequence without touching host
// interfaces or opening privileged listeners. The binding restoration is
// exercised through the exact primitives the vmnetcfg binding path runs
// (the owner-validated reclaim of the recorded address, the dhcp lease
// registration and the fresh auto-allocation); the controller-level
// reconciliation against the published state is covered by the
// vmnetcfg-side recovery tests.

import (
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// recoveryNewPool builds the one-address pool of the review scenario: the
// range holds exactly 10.0.0.2, so a fresh allocation can only take the
// recorded address if the protection missed its claim.
func recoveryNewPool(name, network string) *kihv1.IPPool {
	return &kihv1.IPPool{
		ObjectMeta: testPoolMetadata(name, network),
		Spec: kihv1.IPPoolSpec{
			NetworkName:   network,
			BindInterface: "eth-test",
			IPv4Config: kihv1.IPv4Config{
				ServerIP: "10.0.0.1",
				Subnet:   "10.0.0.0/29",
				Pool:     kihv1.Pool{Start: "10.0.0.2", End: "10.0.0.2"},
			},
		},
	}
}

// recoveryRegistrationSteps runs the complete production registration of
// the pool through the same registerPoolWithTeardown wrapper the sync
// uses: the up-front validation, the nic address and the dhcp pool and
// its listener (through the test seams recoveryNewController installs),
// the allocator registration, the claim protection, the status rebuild,
// the metrics reset and the cache publication. A failed attempt is torn
// back down by the same wrapper, so a retried registration starts from a
// clean registration state exactly like the production recovery.
func recoveryRegistrationSteps(t *testing.T, c *Controller, pool *kihv1.IPPool) error {
	t.Helper()

	return c.registerPoolWithTeardown(pool, "the recovery registration of")
}

// recoveryNewController wires a controller against the rest state and
// returns the state so the test can seed its fixtures.
func recoveryNewController(t *testing.T, stored *kihv1.IPPool) (*Controller, *ippoolBehaviorRestState, *httptest.Server) {
	t.Helper()

	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	t.Cleanup(srv.Close)

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)
	c.scope = testNetworkScope(stored.Spec.NetworkName)

	// the host-level steps of the registration run through their test
	// seams: the nic address add is stubbed off the host interfaces and
	// the dhcp listener stays a no-op, so the recovery fixtures run the
	// real registration sequence without privileged operations
	stubNicMutation(t)
	origRemove := network.RemoveIpFromNic
	network.RemoveIpFromNic = func(nic string, ip4 string) error { return nil }
	t.Cleanup(func() { network.RemoveIpFromNic = origRemove })
	c.runListener = func(networkName string, nic string) error {
		return nil
	}

	return c, rs, srv
}

// recoveryNewVMNetCfg builds a vmnetcfg whose spec records the given
// assignment for the network, like main persisted it.
func recoveryNewVMNetCfg(namespace, name, ip, mac, network string) *kihv1.VirtualMachineNetworkConfig {
	return &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName: name,
			NetworkConfig: []kihv1.NetworkConfig{
				{IPAddress: ip, MACAddress: mac, NetworkName: network},
			},
		},
		Status: kihv1.VirtualMachineNetworkConfigStatus{
			NetworkConfig: []kihv1.NetworkConfigStatus{
				{MACAddress: mac, NetworkName: network, Status: "OK", Message: "IP address successfully allocated"},
			},
		},
	}
}

// TestRegistrationNormalizesTheLegacyStatusReference: a main-era pool
// status entry carries the mac address in a legacy spelling (hyphens and
// uppercase). The protection must pin and republish the claim under the
// canonical reference the restoring binding constructs, so the binding
// reclaims its own recorded address idempotently instead of being rejected
// as a foreign owner of its own lease.
func TestRegistrationNormalizesTheLegacyStatusReference(t *testing.T) {
	const (
		legacyNamespace = "default"
		legacyVMName    = "vm-test"
		legacyMAC       = "02-AA-BB-CC-DD-01"
	)

	// the raw historical reference exactly as main wrote it into the pool
	// status - deliberately not built through util.AllocationRef, which
	// would canonicalize the spelling before the test starts
	legacyRef := legacyNamespace + "/" + legacyVMName + " [" + legacyMAC + "]"
	canonicalRef := util.AllocationRef(legacyNamespace, legacyVMName, legacyMAC)

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{"10.0.0.2": legacyRef}

	c, rs, _ := recoveryNewController(t, stored)
	pool := recoveryNewPool("pool1", "infra/net-a")

	err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the claim is pinned and republished under the canonical spelling:
	// both the written ledger and the published pool must carry the
	// canonical reference, otherwise the same logical owner is rejected
	// as a foreign owner of its own lease
	if got := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; got != canonicalRef {
		t.Errorf("republished ledger entry = %q, want the canonical reference %q", got, canonicalRef)
	}
	if got := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; got == legacyRef {
		t.Error("the raw legacy spelling must not be republished")
	}

	// the published pool carries the canonical record
	published, pubErr := c.cache.Get("pool", "infra/net-a")
	if pubErr != nil {
		t.Fatalf("the registered pool must be published: %s", pubErr)
	}
	if got := published.(kihv1.IPPool).Status.IPv4.Allocated["10.0.0.2"]; got != canonicalRef {
		t.Errorf("published ledger entry = %q, want the canonical reference %q", got, canonicalRef)
	}

	// the binding restoration through the production primitives: the
	// owner-validated reclaim of the recorded address succeeds, the lease
	// is served, and the ownership check of the record write agrees
	// (a matching entry is confirmed read-only)
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", canonicalRef); err != nil {
		t.Errorf("the restoring binding reclaiming its own recorded address: %s", err)
	}
	if err := c.dhcp.AddLease(legacyMAC, "infra/net-a", "10.0.0.2", legacyNamespace+"/"+legacyVMName); err != nil {
		t.Errorf("restoring the dhcp lease: %s", err)
	}
	if !c.dhcp.CheckLease(legacyMAC) {
		t.Error("the restored binding must serve its lease")
	}

	// a genuinely different owner is still rejected
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", util.AllocationRef("default", "vm-other", "02:00:00:00:00:99")); err == nil {
		t.Error("a different owner must not reclaim the protected claim")
	}
}

// TestRegistrationProtectsTheSpecOnlyClaim: the review's late-recovery
// differential. The existing vmnetcfg persistently claims the only pool
// address while the pool status carries no entry for it (a historical
// partial write). The recovering registration must pin the spec claim
// before the publication, so a fresh allocation cannot take the address
// and the original binding restores it afterwards. The pin lives in the
// allocator only: the binding writes its own ledger entry when it
// reclaims the address, so no unverified claim is ever published as an
// authoritative record.
func TestRegistrationProtectsTheSpecOnlyClaim(t *testing.T) {
	const (
		oldNamespace = "default"
		oldVMName    = "vm-old"
		oldMAC       = "02:00:00:00:00:10"
	)

	oldRef := util.AllocationRef(oldNamespace, oldVMName, oldMAC)

	// the ledger lost the record: the pool status survived without the
	// entry of the existing binding
	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		recoveryNewVMNetCfg(oldNamespace, oldVMName, "10.0.0.2", oldMAC, "infra/net-a"),
	}
	pool := recoveryNewPool("pool1", "infra/net-a")

	err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the spec claim is pinned in the allocator but not republished: the
	// restoring binding writes the ledger entry itself, so a stale pin
	// can never survive as an authoritative record
	if got, ok := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; ok {
		t.Errorf("republished ledger entry = %q, want none before the binding restored", got)
	}
	if used := c.ipam.Used("infra/net-a"); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the spec claim is pinned)", used)
	}

	// a new vm requesting an automatic address must not receive the
	// existing address: the one-address pool is exhausted by the pin
	// (the application is past its startup gate here - the controller's
	// appStatus is APP_RUNNING - so no global deferral masks this)
	if _, err := c.ipam.GetIP("infra/net-a", ""); err == nil {
		t.Error("a fresh allocation must not receive the existing vm's address")
	}

	// the original binding restores its own address and dhcp lease through
	// the production primitives: its reclaim is idempotent against the
	// pin and its own record write rebuilds the ledger entry
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", oldRef); err != nil {
		t.Errorf("the original binding restoring its recorded address: %s", err)
	}
	if err := c.dhcp.AddLease(oldMAC, "infra/net-a", "10.0.0.2", oldNamespace+"/"+oldVMName); err != nil {
		t.Errorf("restoring the dhcp lease: %s", err)
	}
	if got := c.dhcp.GetLease(oldMAC).ClientIP.String(); got != "10.0.0.2" {
		t.Errorf("the restored lease ip = %q, want the recorded 10.0.0.2", got)
	}

	// a foreign binding cannot take the protected claim
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", util.AllocationRef("other-ns", "vm-other", "02:00:00:00:00:99")); err == nil {
		t.Error("a foreign binding must not reclaim the protected claim")
	}
}

// TestRegistrationSweepCoversNamespacesAndMalformedNics: the claim sweep
// must cover every namespace and every nic of the network, must not stop
// at a malformed earlier nic, must keep the exclude pass authoritative,
// and must skip out-of-range claims without publishing them.
func TestRegistrationSweepCoversNamespacesAndMalformedNics(t *testing.T) {
	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Spec.IPv4Config.Pool.Start = "10.0.0.2"
	stored.Spec.IPv4Config.Pool.End = "10.0.0.5"
	stored.Spec.IPv4Config.Pool.Exclude = []string{"10.0.0.4"}
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)

	// ns-a/vm-a: the malformed nic comes first and must not stop the
	// protection of the later healthy nic
	malformedFirst := recoveryNewVMNetCfg("ns-a", "vm-a", "10.0.0.3", "not-a-mac", "infra/net-a")
	malformedFirst.Spec.NetworkConfig = append(malformedFirst.Spec.NetworkConfig, kihv1.NetworkConfig{
		IPAddress: "10.0.0.2", MACAddress: "02:00:00:00:00:11", NetworkName: "infra/net-a",
	})

	// ns-b/vm-b: a claim from another namespace of the same network
	foreignNamespace := recoveryNewVMNetCfg("ns-b", "vm-b", "10.0.0.5", "02:00:00:00:00:12", "infra/net-a")

	// ns-c/vm-c: a claim on an excluded address - the exclude pass wins
	excludedClaim := recoveryNewVMNetCfg("ns-c", "vm-c", "10.0.0.4", "02:00:00:00:00:13", "infra/net-a")

	// ns-d/vm-d: an out-of-range claim which the allocator can never hand
	// out, and a nic of another network which is not this pool's business
	outOfRange := recoveryNewVMNetCfg("ns-d", "vm-d", "10.0.0.99", "02:00:00:00:00:14", "infra/net-a")
	outOfRange.Spec.NetworkConfig = append(outOfRange.Spec.NetworkConfig, kihv1.NetworkConfig{
		IPAddress: "10.0.1.2", MACAddress: "02:00:00:00:00:15", NetworkName: "infra/net-other",
	})

	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{malformedFirst, foreignNamespace, excludedClaim, outOfRange}

	pool := stored.DeepCopy()

	err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the healthy claim after the malformed nic is pinned under its owner
	healthyRef := util.AllocationRef("ns-a", "vm-a", "02:00:00:00:00:11")
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", healthyRef); err != nil {
		t.Errorf("the healthy nic's own reclaim against its pin: %s", err)
	}
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", util.AllocationRef("ns-a", "vm-a", "02:00:00:00:00:99")); err == nil {
		t.Error("a different owner must not reclaim the healthy nic's pin")
	}

	// the malformed nic's address is protected without an owner identity:
	// neither a fresh allocation nor any binding can take it, and only the
	// binding of its own vm can retake it once the macaddress is corrected
	if _, err := c.ipam.GetIP("infra/net-a", "10.0.0.3"); err == nil {
		t.Error("the malformed nic's address must not be handable to a fresh allocation")
	}
	correctedRef := util.AllocationRef("ns-a", "vm-a", "02:00:00:00:00:20")
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.3", correctedRef); err == nil {
		t.Error("a plain reclaim must not take the ownerless pin")
	}
	if _, err := c.ipam.ReclaimIPClaimant("infra/net-a", "10.0.0.3", correctedRef, "ns-a/vm-a"); err != nil {
		t.Errorf("the corrected binding of the claiming vm retaking its pin: %s", err)
	}

	// the claim of the other namespace is pinned under its owner
	foreignRef := util.AllocationRef("ns-b", "vm-b", "02:00:00:00:00:12")
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.5", foreignRef); err != nil {
		t.Errorf("the other namespace's own reclaim against its pin: %s", err)
	}

	// the excluded address stays reserved for the exclude pass
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.4", util.AllocationRef("ns-c", "vm-c", "02:00:00:00:00:13")); err == nil {
		t.Error("a binding must not reclaim an excluded address")
	}

	// the out-of-range claim is neither pinned nor published
	if _, err := c.ipam.GetIP("infra/net-a", "10.0.0.99"); err == nil {
		t.Error("an out-of-range claim must not be pinned")
	}

	// the accounting: the four in-range addresses are all reserved (two
	// owner pins, one ownerless pin, one exclude)
	if used := c.ipam.Used("infra/net-a"); used != 4 {
		t.Errorf("ipam used = %d, want 4", used)
	}
	if _, err := c.ipam.GetIP("infra/net-a", ""); err == nil {
		t.Error("the pool must be exhausted after the protection")
	}

	// the republished ledger carries only the exclude entry: the spec
	// pins live in the allocator until their bindings restore and write
	// their own records
	ledger := rs.lastBody.Status.IPv4.Allocated
	if got := ledger["10.0.0.4"]; got != ipam.ExcludedOwner {
		t.Errorf("ledger entry of the excluded address = %q, want %q", got, ipam.ExcludedOwner)
	}
	if len(ledger) != 1 {
		t.Errorf("ledger = %v, want exactly the exclude entry", ledger)
	}
}

// TestRegistrationWithoutTheClaimSnapshotDoesNotPublish: when the
// authoritative vmnetcfg snapshot cannot be obtained, the registration
// must fail before the status rebuild and the cache publication - an
// unseen claim would otherwise be handed to a fresh allocation. The pool
// stays unregistered, so no allocator state is exposed at all.
func TestRegistrationWithoutTheClaimSnapshotDoesNotPublish(t *testing.T) {
	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	rs.failVMNetCfgList = true
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		recoveryNewVMNetCfg("default", "vm-old", "10.0.0.2", "02:00:00:00:00:10", "infra/net-a"),
	}
	pool := recoveryNewPool("pool1", "infra/net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err == nil {
		t.Fatal("the registration must fail when the claim snapshot cannot be obtained")
	}

	// nothing was published: no status write, no cached pool
	if rs.putCount != 0 {
		t.Errorf("pool status writes = %d, want 0 (the failure precedes the publication)", rs.putCount)
	}
	if _, cacheErr := c.cache.Get("pool", "infra/net-a"); cacheErr == nil {
		t.Error("the pool must not be published into the cache")
	}

	// the retry re-runs the whole protection once the snapshot is
	// available: the failed attempt was torn down by the same
	// registerPoolWithTeardown the steps helper runs (its allocator step
	// removed the subnet), so the retry starts from a fresh registration
	// state
	rs.failVMNetCfgList = false
	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the retried registration steps: %s", err)
	}
	oldRef := util.AllocationRef("default", "vm-old", "02:00:00:00:00:10")
	if used := c.ipam.Used("infra/net-a"); used != 1 {
		t.Errorf("ipam used after the retry = %d, want 1 (the spec claim is pinned)", used)
	}
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", oldRef); err != nil {
		t.Errorf("the pinned claim of the retry must belong to its recorded owner: %s", err)
	}
	if _, err := c.cache.Get("pool", "infra/net-a"); err != nil {
		t.Errorf("the retried registration must publish the pool: %s", err)
	}
}

// recoveryNewStatuslessVMNetCfg builds a vmnetcfg whose spec records the
// given address for the network while carrying no status at all, like a
// manually created object which the binding controller never processed.
func recoveryNewStatuslessVMNetCfg(namespace, name, ip, mac, network string, createdAgo time.Duration) *kihv1.VirtualMachineNetworkConfig {
	obj := recoveryNewVMNetCfg(namespace, name, ip, mac, network)
	obj.Status.NetworkConfig = nil
	obj.CreationTimestamp = metav1.NewTime(time.Now().Add(-createdAgo))

	return obj
}

// TestRegistrationDoesNotHonorTheHijackGuardedClaim: a status-less
// vmnetcfg created while the previous process era was already serving is
// the object the binding replay's hijack guard rejects - the sweep must
// not reserve its requested address either, otherwise the rejected
// request preempts the established assignment which actually owns the
// address and the established vm loses its ip to a reservation nothing
// can ever restore. The guarded request appears first in the LIST, so a
// first-wins sweep would take its claim.
func TestRegistrationDoesNotHonorTheHijackGuardedClaim(t *testing.T) {
	const (
		oldNamespace = "default"
		oldVMName    = "vm-old"
		oldMAC       = "02:00:00:00:00:10"
	)

	oldRef := util.AllocationRef(oldNamespace, oldVMName, oldMAC)
	guardedRef := util.AllocationRef("default", "aaa-new", "02:00:00:00:00:20")

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{}
	// the previous era recorded its last status update an hour ago: the
	// guarded object was created half an hour after it, inside the
	// interval the binding replay's hijack guard rejects
	stored.Status.LastUpdate = metav1.NewTime(time.Now().Add(-time.Hour))

	c, rs, _ := recoveryNewController(t, stored)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		// the guarded request appears first in the LIST
		recoveryNewStatuslessVMNetCfg("default", "aaa-new", "10.0.0.2", "02:00:00:00:00:20", "infra/net-a", 30*time.Minute),
		// the established vm carries an OK status and records the same
		// address, but its ledger entry was lost
		recoveryNewVMNetCfg(oldNamespace, oldVMName, "10.0.0.2", oldMAC, "infra/net-a"),
	}
	pool := recoveryNewPool("pool1", "infra/net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the established assignment owns the pin; the guarded request never
	// claimed anything
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", oldRef); err != nil {
		t.Errorf("the established vm reclaiming its recorded address: %s", err)
	}
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", guardedRef); err == nil {
		t.Error("the hijack guarded request must not own the pin")
	}
}

// The guarded request alone claims nothing: like the binding replay, the
// sweep leaves its requested address unassigned, so the address stays
// available exactly as before the guard rejected the object.
func TestRegistrationLeavesTheGuardedRequestUnclaimed(t *testing.T) {
	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{}
	stored.Status.LastUpdate = metav1.NewTime(time.Now().Add(-time.Hour))

	c, rs, _ := recoveryNewController(t, stored)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		recoveryNewStatuslessVMNetCfg("default", "aaa-new", "10.0.0.2", "02:00:00:00:00:20", "infra/net-a", 30*time.Minute),
	}
	pool := recoveryNewPool("pool1", "infra/net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	if used := c.ipam.Used("infra/net-a"); used != 0 {
		t.Errorf("ipam used = %d, want 0 (a guarded request claims nothing)", used)
	}
	if ip, err := c.ipam.GetIP("infra/net-a", ""); err != nil || ip != "10.0.0.2" {
		t.Errorf("the unclaimed address must stay available, got ip %q err %v", ip, err)
	}
}

// TestRegistrationPrefersTheEstablishedAssignmentRegardlessOfListOrder:
// two admitted claimants record the same address - one carries the status
// entry of an assignment the binding controller already established, the
// other is a bare request without any status. The list returns the bare
// request first, but the established assignment must win the pin: a
// first-wins sweep would hand the live vm's address to the request and
// the established vm would be rejected as a foreign owner of its own
// recorded address.
func TestRegistrationPrefersTheEstablishedAssignmentRegardlessOfListOrder(t *testing.T) {
	const (
		estNamespace = "default"
		estVMName    = "vm-old"
		estMAC       = "02:00:00:00:00:10"
	)

	estRef := util.AllocationRef(estNamespace, estVMName, estMAC)
	requestRef := util.AllocationRef("default", "vm-req", "02:00:00:00:00:20")

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		// the bare request appears first in the LIST and is admitted (its
		// object predates the last status update, so no hijack guard
		// applies)
		recoveryNewStatuslessVMNetCfg("default", "vm-req", "10.0.0.2", "02:00:00:00:00:20", "infra/net-a", 2*time.Hour),
		recoveryNewVMNetCfg(estNamespace, estVMName, "10.0.0.2", estMAC, "infra/net-a"),
	}
	pool := recoveryNewPool("pool1", "infra/net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", estRef); err != nil {
		t.Errorf("the established assignment must own the pin: %s", err)
	}
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", requestRef); err == nil {
		t.Error("the bare request must not own the pin")
	}
}

// TestRegistrationDropsTheStaleSpecClaim: the LIST snapshot captures a
// spec-only claim, and the vm cleanup completes the nic's removal while
// the pool is still unpublished (the frozen snapshot stays served). The
// re-verification read catches the removed nic, so the pin is dropped
// instead of being published: the address stays available to the next
// legitimate vm and no orphan record survives which a fresh helper
// restart would treat as authoritative and reserve again.
func TestRegistrationDropsTheStaleSpecClaim(t *testing.T) {
	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		recoveryNewVMNetCfg("default", "vm-old", "10.0.0.2", "02:00:00:00:00:10", "infra/net-a"),
	}

	// the concurrent vm cleanup completes between the frozen LIST
	// response and the re-verification reads: the nic is gone from the
	// persisted spec
	rs.vmnetcfgListHook = func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.vmnetcfgs[0] = rs.vmnetcfgs[0].DeepCopy()
		rs.vmnetcfgs[0].Spec.NetworkConfig = nil
	}

	pool := recoveryNewPool("pool1", "infra/net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the stale pin was dropped: nothing is reserved and nothing was
	// published for the removed nic
	if used := c.ipam.Used("infra/net-a"); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the stale pin was dropped)", used)
	}
	if got, ok := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; ok {
		t.Errorf("republished ledger entry = %q, want no orphan record", got)
	}

	// the address is available to the next legitimate vm
	if ip, err := c.ipam.GetIP("infra/net-a", ""); err != nil || ip != "10.0.0.2" {
		t.Errorf("the freed address must be allocatable, got ip %q err %v", ip, err)
	}
}

// A claiming object which is deleted while the pool is still unpublished
// leaves a stale claim as well: the re-verification read reports the
// object as gone and the pin is dropped.
func TestRegistrationDropsTheClaimOfTheDeletedObject(t *testing.T) {
	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		recoveryNewVMNetCfg("default", "vm-old", "10.0.0.2", "02:00:00:00:00:10", "infra/net-a"),
	}
	rs.vmnetcfgListHook = func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.vmnetcfgs = nil
	}

	pool := recoveryNewPool("pool1", "infra/net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	if used := c.ipam.Used("infra/net-a"); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the claim of the deleted object was dropped)", used)
	}
}

// A claim whose object cannot be re-verified must fail the registration
// before any publication instead of publishing a pin nobody vouches for
// anymore; the retried registration converges once the read succeeds.
func TestRegistrationFailsOnUnverifiableClaim(t *testing.T) {
	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	rs.failVMNetCfgGet = true
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		recoveryNewVMNetCfg("default", "vm-old", "10.0.0.2", "02:00:00:00:00:10", "infra/net-a"),
	}
	pool := recoveryNewPool("pool1", "infra/net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err == nil {
		t.Fatal("the registration must fail when a pinned claim cannot be verified")
	}
	if rs.putCount != 0 {
		t.Errorf("pool status writes = %d, want 0 (the failure precedes the publication)", rs.putCount)
	}
	if _, cacheErr := c.cache.Get("pool", "infra/net-a"); cacheErr == nil {
		t.Error("the pool must not be published into the cache")
	}

	// the retry converges once the verification read succeeds again: the
	// failed attempt was torn down by the same wrapper, so the retry
	// starts from a fresh registration state
	rs.failVMNetCfgGet = false
	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the retried registration steps: %s", err)
	}
	if used := c.ipam.Used("infra/net-a"); used != 1 {
		t.Errorf("ipam used after the retry = %d, want 1", used)
	}
}

// TestRegistrationAttributesTheUnusableMacClaim: the recorded address of
// a claim with an unusable macaddress is pinned without an owner identity
// (no valid mac means no owner reference) and attributed to its claiming
// vm, so the corrected binding of that vm retakes its own pin, while an
// unparseable ledger reference stays an unattributed pin which no binding
// can ever reclaim.
func TestRegistrationAttributesTheUnusableMacClaim(t *testing.T) {
	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Spec.IPv4Config.Pool.Start = "10.0.0.2"
	stored.Spec.IPv4Config.Pool.End = "10.0.0.3"
	// an unparseable historical ledger reference keeps its conservative
	// protection: pinned ownerlessly and unattributed
	stored.Status.IPv4.Allocated = map[string]string{"10.0.0.2": "garbage"}

	c, rs, _ := recoveryNewController(t, stored)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		recoveryNewVMNetCfg("default", "vm-broken", "10.0.0.3", "not-a-mac", "infra/net-a"),
	}
	pool := stored.DeepCopy()

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the unparseable ledger pin: protected, republished verbatim and
	// never reclaimable by any binding
	if got := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; got != "garbage" {
		t.Errorf("the unparseable ledger entry must be republished verbatim, got %q", got)
	}
	if _, err := c.ipam.ReclaimIPClaimant("infra/net-a", "10.0.0.2", util.AllocationRef("default", "anyone", "02:00:00:00:00:99"), "default/anyone"); err == nil {
		t.Error("an unattributed pin must stay unreclaimable")
	}

	// the unusable-mac spec pin: attributed to its vm, retaken by the
	// corrected binding of that vm only
	correctedRef := util.AllocationRef("default", "vm-broken", "02:00:00:00:00:30")
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.3", correctedRef); err == nil {
		t.Error("a plain reclaim must not take the ownerless pin")
	}
	if _, err := c.ipam.ReclaimIPClaimant("infra/net-a", "10.0.0.3", correctedRef, "default/other-vm"); err == nil {
		t.Error("a foreign claimant must not take the attributed pin")
	}
	if _, err := c.ipam.ReclaimIPClaimant("infra/net-a", "10.0.0.3", correctedRef, "default/vm-broken"); err != nil {
		t.Errorf("the corrected binding of the claiming vm retaking its pin: %s", err)
	}
}

// TestRegisterIPPoolRejectsExcludeOverlappingLiveClaim pins the P2.6
// finding: an exclude entry which the persisted ledger records for a live
// binding is a configuration conflict which can never converge, so the
// registration must reject it as unregistrable BEFORE any host, dhcp or
// allocator mutation. without the up-front rejection, the exclude pass
// claims the address as EXCLUDED first and the claim protection of the
// same address fails the registration forever - each resync tearing the
// half-built registration down and rebuilding it.
func TestRegisterIPPoolRejectsExcludeOverlappingLiveClaim(t *testing.T) {
	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{"10.0.0.2": "default/vm-test [02:00:00:00:00:01]"}

	c, rs, _ := recoveryNewController(t, stored)
	// the recorded owner is genuinely live: its vmnetcfg still records
	// the binding, so the exclude entry is a real never-converging
	// conflict. (a stale record whose owner is authoritatively gone is
	// revalidated away by the admission check instead - see
	// TestRegisterIPPoolAdmitsExcludeOverStaleLedgerRecord below.)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		recoveryNewVMNetCfg("default", "vm-test", "10.0.0.2", "02:00:00:00:00:01", "infra/net-a"),
	}

	pool := recoveryNewPool("pool1", "infra/net-a")
	pool.Spec.IPv4Config.Pool.Exclude = []string{"10.0.0.2"}

	cleanup, err := c.registerIPPool(pool)
	if err == nil {
		t.Fatal("the overlapping exclude must fail the registration")
	}
	if !errors.Is(err, ErrPoolUnregistrable) {
		t.Errorf("error = %v, want ErrPoolUnregistrable so the startup gate counts the pool", err)
	}
	if cleanup {
		t.Error("cleanup flag = true, want false: the rejection must not run any teardown of state it never created")
	}

	// the rejection happened before any mutation: nothing of the pool is
	// registered anywhere
	if c.dhcp.CheckPool("infra/net-a") {
		t.Error("no dhcp pool may exist after the pre-mutation rejection")
	}
	// Used discriminates a missing subnet (0) from a registered subnet
	// whose single address the exclude pass claimed (1): the probe is
	// read-only, unlike an allocation attempt
	if used := c.ipam.Used("infra/net-a"); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the rejection must precede the subnet registration)", used)
	}
}

// TestRegisterIPPoolAdmitsExcludeOverStaleLedgerRecord pins the review
// finding: an exclude entry which the persisted ledger records for an
// owner whose binding is authoritatively gone (the helper was down while
// the vm was deleted, so no cleanup un-recorded it) is not a conflict -
// the same registration's claim protection would drop the stale record,
// so the up-front check must revalidate the recorded owner instead of
// rejecting the pool as unregistrable forever (the rejection settles the
// startup gate and never retries, leaving the network without a dhcp
// server until the status is edited by hand).
func TestRegisterIPPoolAdmitsExcludeOverStaleLedgerRecord(t *testing.T) {
	const (
		ownerNamespace = "default"
		ownerVMName    = "vm-gone"
		ownerMAC       = "02:00:00:00:00:50"
	)

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.0.0.2": util.AllocationRef(ownerNamespace, ownerVMName, ownerMAC),
	}

	c, rs, _ := recoveryNewController(t, stored)
	// the vmnetcfg is gone and the vm is gone too: the record is stale
	c.verifyVM = func(namespace string, name string) (bool, error) {
		if namespace != ownerNamespace || name != ownerVMName {
			t.Errorf("the vm verification queried %s/%s, want %s/%s", namespace, name, ownerNamespace, ownerVMName)
		}

		return false, nil
	}

	pool := recoveryNewPool("pool1", "infra/net-a")
	pool.Spec.IPv4Config.Pool.Exclude = []string{"10.0.0.2"}

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the stale ledger record must not block the exclude entry: %s", err)
	}

	// the exclude pass claimed the address and the stale record was
	// dropped by the claim protection instead of being republished for
	// its gone owner
	if got := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; got != ipam.ExcludedOwner {
		t.Errorf("allocated[10.0.0.2] = %q, want the EXCLUDED owner of the exclude pass", got)
	}

	// the excluded address is not available to a fresh allocation
	if ip, err := c.ipam.GetIP("infra/net-a", ""); err == nil {
		t.Errorf("the excluded address must stay unallocatable, got ip %q", ip)
	}
}

// TestRegisterIPPoolRetriesExcludeOverUnverifiableOwner pins the transient
// classification of the exclude admission: the owner revalidation runs
// against the api, and an owner whose liveness cannot be established (the
// vmnetcfg read fails transiently) is neither a definitive conflict nor a
// stale record. wrapping that state in ErrPoolUnregistrable would settle
// the startup gate on the first failed read - leaving the network without
// a dhcp server and telling the operator to hand-edit a healthy object -
// although the resync re-runs the admission and admits the entry as soon
// as the api read succeeds. the registration returns a plain retriable
// error and the gate stays open instead.
func TestRegisterIPPoolRetriesExcludeOverUnverifiableOwner(t *testing.T) {
	const (
		ownerNamespace = "default"
		ownerVMName    = "vm-unreachable"
		ownerMAC       = "02:00:00:00:00:60"
	)

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.0.0.2": util.AllocationRef(ownerNamespace, ownerVMName, ownerMAC),
	}

	c, rs, _ := recoveryNewController(t, stored)
	// the vmnetcfg read of the owner revalidation fails transiently
	rs.failVMNetCfgGet = true

	pool := recoveryNewPool("pool1", "infra/net-a")
	pool.Spec.IPv4Config.Pool.Exclude = []string{"10.0.0.2"}

	// the startup replay is active, so the gate classification of the
	// registration attempt is observable
	c.appStatus.Store(APP_INIT)

	cleanup, err := c.registerIPPool(pool)
	if err == nil {
		t.Fatal("an unverifiable owner must not silently admit the exclude entry")
	}
	if errors.Is(err, ErrPoolUnregistrable) {
		t.Errorf("error = %v, the unverified owner is a transient state and must not carry the definitive ErrPoolUnregistrable classification", err)
	}
	if cleanup {
		t.Error("cleanup flag = true, want false: the failed verification must not run any teardown of state it never created")
	}

	// the transient failure stays uncounted: the retried registration
	// must still be able to settle the pool for the startup gate once
	// the api read succeeds
	if c.gate.Settled() != 0 {
		t.Errorf("ippool gate settled = %d, want 0 for a transiently failed registration", c.gate.Settled())
	}

	// the next registration attempt runs against a healed api: the owner
	// is authoritatively gone (no vmnetcfg, no vm), so the stale record
	// is revalidated away and the exclude entry is admitted
	rs.failVMNetCfgGet = false
	c.verifyVM = func(namespace string, name string) (bool, error) {
		if namespace != ownerNamespace || name != ownerVMName {
			t.Errorf("the vm verification queried %s/%s, want %s/%s", namespace, name, ownerNamespace, ownerVMName)
		}

		return false, nil
	}

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the healed revalidation must admit the exclude entry: %s", err)
	}
	if got := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; got != ipam.ExcludedOwner {
		t.Errorf("allocated[10.0.0.2] = %q, want the EXCLUDED owner after the healed admission", got)
	}

	// the settled registration counts for the gate exactly once the
	// transient state healed
	if c.gate.Settled() != 1 {
		t.Errorf("ippool gate settled = %d after the healed admission, want 1", c.gate.Settled())
	}
}

// A02 trigger 1 regression: the mac spelling of a live owner drifts
// between the LIST snapshot and the per-claim re-verification read
// (02-AA-BB-CC-DD-01 -> 02:aa:bb:cc:dd:01, e.g. an in-flight edit which
// only reformats the address). the unchanged logical owner must not be
// classified as removed: dropping its pin would publish the address as
// free although the same nic still records it, and a fresh allocation
// could double-bind it.
func TestRegistrationKeepsThePinOfAMacSpellingDrift(t *testing.T) {
	const (
		driftNamespace = "default"
		driftVMName    = "vm-drift"
	)

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)

	// the LIST snapshot carries the legacy spelling, like the spec of a
	// vm which was created before the canonical normalization existed
	drifted := recoveryNewVMNetCfg(driftNamespace, driftVMName, "10.0.0.2", "02-AA-BB-CC-DD-01", "infra/net-a")
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{drifted}

	// between the frozen LIST snapshot and the re-verification reads the
	// object's mac is reformatted to the canonical spelling: the logical
	// owner is unchanged
	rs.vmnetcfgListHook = func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()

		reformatted := recoveryNewVMNetCfg(driftNamespace, driftVMName, "10.0.0.2", "02:aa:bb:cc:dd:01", "infra/net-a")
		rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{reformatted}
	}

	pool := recoveryNewPool("pool1", "infra/net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the drifted owner keeps its pin: the address is not available to a
	// fresh allocation
	if used := c.ipam.Used("infra/net-a"); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the drifted owner keeps its pin)", used)
	}
	if ip, err := c.ipam.GetIP("infra/net-a", ""); err == nil {
		t.Errorf("the recorded address must stay unavailable to a fresh allocation, got ip %q err %v", ip, err)
	}

	// the canonical owner reclaims its own address idempotently
	ownerRef := util.AllocationRef(driftNamespace, driftVMName, "02:aa:bb:cc:dd:01")
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", ownerRef); err != nil {
		t.Errorf("the drifted vm reclaiming its recorded address under the canonical spelling: %s", err)
	}
}

// A02 trigger 2 regression: two admitted claimants record the same
// address, the winner's pin displaces the survivor, and the winner is
// dropped during the re-verification (its object was deleted between the
// LIST snapshot and the fresh read). the survivor must be promoted back:
// without the promotion the pool publishes the address as free although
// the survivor's live object still records it, and a fresh allocation
// takes it over.
func TestRegistrationPromotesTheSurvivorOfADroppedWinner(t *testing.T) {
	const (
		winnerNamespace = "default"
		winnerVMName    = "vm-winner"
		winnerMAC       = "02:00:00:00:00:30"
		survivorMAC     = "02:00:00:00:00:31"
	)

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		// both claimants are established assignments without a ledger
		// entry (a historical partial write lost both records)
		recoveryNewVMNetCfg(winnerNamespace, winnerVMName, "10.0.0.2", winnerMAC, "infra/net-a"),
		recoveryNewVMNetCfg(winnerNamespace, "vm-survivor", "10.0.0.2", survivorMAC, "infra/net-a"),
	}

	// the winner disappears between the frozen LIST snapshot and the
	// re-verification reads: its pin is dropped, the survivor remains
	rs.vmnetcfgListHook = func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()

		rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
			recoveryNewVMNetCfg(winnerNamespace, "vm-survivor", "10.0.0.2", survivorMAC, "infra/net-a"),
		}
	}

	pool := recoveryNewPool("pool1", "infra/net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the promoted survivor owns the pin: the address is not available
	// to a fresh allocation
	if used := c.ipam.Used("infra/net-a"); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the promoted survivor owns the pin)", used)
	}
	if ip, err := c.ipam.GetIP("infra/net-a", ""); err == nil {
		t.Errorf("the survivor's recorded address must stay unavailable to a fresh allocation, got ip %q err %v", ip, err)
	}

	// the survivor reclaims its own address idempotently, the dropped
	// winner does not
	survivorRef := util.AllocationRef(winnerNamespace, "vm-survivor", survivorMAC)
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", survivorRef); err != nil {
		t.Errorf("the promoted survivor reclaiming its recorded address: %s", err)
	}
	winnerRef := util.AllocationRef(winnerNamespace, winnerVMName, winnerMAC)
	if _, err := c.ipam.ReclaimIP("infra/net-a", "10.0.0.2", winnerRef); err == nil {
		t.Error("the dropped winner must not own the pin anymore")
	}
}

// A03 regression: a ledger entry whose owning vmnetcfg still exists but
// positively removed the binding (the nic edit already landed, the pool
// status write of the cleanup was skipped) must not be resurrected by the
// registration: the previous behavior republished and pinned the record,
// permanently consuming the address although no reconciliation is left
// which could ever release it.
func TestRegistrationDropsTheLedgerRecordOfAPositivelyRemovedOwner(t *testing.T) {
	const (
		ownerNamespace = "default"
		ownerVMName    = "vm-moved"
		ownerMAC       = "02:00:00:00:00:40"
	)

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.0.0.2": util.AllocationRef(ownerNamespace, ownerVMName, ownerMAC),
	}

	c, rs, _ := recoveryNewController(t, stored)

	// the owner's object exists, but its spec records the binding on
	// another network: the nic of this pool was removed
	moved := recoveryNewVMNetCfg(ownerNamespace, ownerVMName, "10.9.9.9", ownerMAC, "infra/net-b")
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{moved}

	pool := recoveryNewPool("pool1", "infra/net-a")

	err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the stale record is neither pinned nor republished
	if got, republished := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; republished {
		t.Errorf("the record of the positively removed owner must not be republished, got %q", got)
	}
	if used := c.ipam.Used("infra/net-a"); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the stale record pins nothing)", used)
	}
	if ip, err := c.ipam.GetIP("infra/net-a", ""); err != nil || ip != "10.0.0.2" {
		t.Errorf("the reclaimed address must be available to a fresh allocation, got ip %q err %v", ip, err)
	}
}

// A03 regression: a ledger entry whose vmnetcfg AND virtualmachine are
// both authoritatively gone is a orphan record (a hand-edited status, a
// pre-fix residue): the registration must drop it instead of pinning it
// forever. the vm existence decides, because a live vm's controller
// recreates its vmnetcfg and the recreated binding reclaims the address.
func TestRegistrationDropsTheLedgerRecordOfAGoneVM(t *testing.T) {
	const (
		ownerNamespace = "default"
		ownerVMName    = "vm-gone"
		ownerMAC       = "02:00:00:00:00:41"
	)

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.0.0.2": util.AllocationRef(ownerNamespace, ownerVMName, ownerMAC),
	}

	c, rs, _ := recoveryNewController(t, stored)

	// no vmnetcfg exists and the vm is gone too: the authoritative absence
	c.verifyVM = func(namespace string, name string) (bool, error) {
		if namespace != ownerNamespace || name != ownerVMName {
			t.Errorf("the vm verification queried %s/%s, want %s/%s", namespace, name, ownerNamespace, ownerVMName)
		}

		return false, nil
	}

	pool := recoveryNewPool("pool1", "infra/net-a")

	err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	if got, republished := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; republished {
		t.Errorf("the record of the gone vm must not be republished, got %q", got)
	}
	if used := c.ipam.Used("infra/net-a"); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the orphan record pins nothing)", used)
	}
}

// A03 regression: a ledger entry whose vmnetcfg is gone while its
// virtualmachine still lives is NOT an authoritative absence - the vm
// controller recreates the vmnetcfg and the recreated binding reclaims
// the address - so the record keeps its protection (fail closed).
func TestRegistrationKeepsTheLedgerRecordOfAReconstructibleOwner(t *testing.T) {
	const (
		ownerNamespace = "default"
		ownerVMName    = "vm-live"
		ownerMAC       = "02:00:00:00:00:42"
	)

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.0.0.2": util.AllocationRef(ownerNamespace, ownerVMName, ownerMAC),
	}

	c, rs, _ := recoveryNewController(t, stored)

	// no vmnetcfg exists, but the vm is live and will reconstruct it
	c.verifyVM = func(namespace string, name string) (bool, error) {
		return true, nil
	}

	pool := recoveryNewPool("pool1", "infra/net-a")

	err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	if ref, republished := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; !republished {
		t.Errorf("the record of the live vm must be republished, got nothing")
	} else if ref != util.AllocationRef(ownerNamespace, ownerVMName, ownerMAC) {
		t.Errorf("the republished record = %q, want the canonical owner reference", ref)
	}
	if used := c.ipam.Used("infra/net-a"); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the reconstructible owner keeps its pin)", used)
	}
	if ip, err := c.ipam.GetIP("infra/net-a", ""); err == nil {
		t.Errorf("the recorded address must stay unavailable to a fresh allocation, got ip %q", ip)
	}
}

// A03 regression: an owner whose vmnetcfg cannot be read (a transient api
// error) keeps its record: a claim the guest may still hold must not be
// dropped because one api read failed, and the next registration
// revalidates it again.
func TestRegistrationKeepsTheLedgerRecordOfAnUnverifiableOwner(t *testing.T) {
	const (
		ownerNamespace = "default"
		ownerVMName    = "vm-unreadable"
		ownerMAC       = "02:00:00:00:00:43"
	)

	stored := recoveryNewPool("pool1", "infra/net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.0.0.2": util.AllocationRef(ownerNamespace, ownerVMName, ownerMAC),
	}

	c, rs, _ := recoveryNewController(t, stored)

	// the owner's object cannot be read at all
	rs.failVMNetCfgGet = true

	pool := recoveryNewPool("pool1", "infra/net-a")

	err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	if _, republished := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; !republished {
		t.Errorf("the record of the unverifiable owner must stay republished (fail closed)")
	}
	if used := c.ipam.Used("infra/net-a"); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the unverifiable owner keeps its pin)", used)
	}
}

func TestRegistrationRejectsUnselectedAndMismatchedPoolsBeforeMutation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*kihv1.IPPool)
	}{
		{"missing network label", func(p *kihv1.IPPool) { delete(p.Labels, util.NetworkLabel) }},
		{"missing namespace label", func(p *kihv1.IPPool) { delete(p.Labels, util.NetworkNamespaceLabel) }},
		{"same NAD name in another namespace", func(p *kihv1.IPPool) {
			p.Labels[util.NetworkNamespaceLabel] = "other"
			p.Spec.NetworkName = "other/net-a"
		}},
		{"selected foreign spec", func(p *kihv1.IPPool) { p.Spec.NetworkName = "other/net-a" }},
		{"selected bare spec", func(p *kihv1.IPPool) { p.Spec.NetworkName = "net-a" }},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/fresh-api", func(t *testing.T) {
			pool := recoveryNewPool("pool1", "infra/net-a")
			c, rs, _ := recoveryNewController(t, pool.DeepCopy())
			c.appStatus.Store(APP_INIT)
			tc.mutate(rs.pool)
			pool = rs.pool.DeepCopy()
			before := rs.pool.DeepCopy()
			hostWrites := 0
			network.AddIpToNic = func(string, string) error { hostWrites++; return nil }
			cleanup, err := c.registerIPPool(pool)
			if !errors.Is(err, ErrPoolUnregistrable) || cleanup {
				t.Fatalf("scope rejection = (%v, %v), want definitive rejection before mutation", cleanup, err)
			}
			if hostWrites != 0 || rs.putCount != 0 || !reflect.DeepEqual(rs.pool, before) || c.cache.Check(pool) || c.dhcp.CheckPool("infra/net-a") {
				t.Fatal("scope rejection mutated host, API, cache, or DHCP state")
			}
			if err := c.ipam.NewSubnet("infra/net-a", "10.0.0.0/29", "10.0.0.2", "10.0.0.2"); err != nil {
				t.Fatalf("rejected pool left an allocator registration: %v", err)
			}
		})
	}
}

func TestRegistrationRechecksSelectorBeforePublishingProtectedClaims(t *testing.T) {
	pool := recoveryNewPool("pool1", "infra/net-a")
	c, rs, _ := recoveryNewController(t, pool.DeepCopy())
	rs.vmnetcfgListHook = func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		delete(rs.pool.Labels, util.NetworkNamespaceLabel)
		rs.pool.Status.IPv4.Allocated = map[string]string{"10.0.0.2": "retained-ledger"}
	}
	if err := recoveryRegistrationSteps(t, c, pool); err == nil {
		t.Fatal("registration committed after its API selector changed")
	}
	if rs.putCount != 0 || rs.pool.Status.IPv4.Allocated["10.0.0.2"] != "retained-ledger" {
		t.Fatal("stale registration overwrote the live object's durable ledger")
	}
	if c.cache.Check(pool) || c.dhcp.CheckPool(pool.Spec.NetworkName) {
		t.Fatal("failed selector revalidation published a local registration")
	}
	if err := c.ipam.NewSubnet(pool.Spec.NetworkName, "10.0.0.0/29", "10.0.0.2", "10.0.0.2"); err != nil {
		t.Fatalf("failed registration leaked its allocator: %v", err)
	}
}

func TestRegistrationQualifiesLegacyBareNICInObjectNamespace(t *testing.T) {
	for _, namespace := range []string{"infra", "tenant"} {
		for _, ledger := range []bool{false, true} {
			name := namespace + "/spec-only"
			if ledger {
				name = namespace + "/ledger"
			}
			t.Run(name, func(t *testing.T) {
				pool := recoveryNewPool("pool1", "infra/net-a")
				const mac = "02-AA-BB-CC-DD-01"
				ref := util.AllocationRef(namespace, "vm", mac)
				if ledger {
					pool.Status.IPv4.Allocated = map[string]string{"10.0.0.2": ref}
				}
				c, rs, _ := recoveryNewController(t, pool)
				// Deliberately legacy bare spelling: it belongs to infra only
				// when the VMNetCfg itself is in infra.
				rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
					recoveryNewVMNetCfg(namespace, "vm", "10.0.0.2", mac, "net-a"),
				}
				rs.vmnetcfgs[0].Status.NetworkConfig[0].MACAddress = "02:aa:bb:cc:dd:01"
				if err := recoveryRegistrationSteps(t, c, pool); err != nil {
					t.Fatal(err)
				}
				if namespace == "infra" {
					if _, err := c.ipam.GetIP(pool.Spec.NetworkName, ""); err == nil {
						t.Fatal("local legacy NIC lost protection")
					}
					if _, err := c.ipam.ReclaimIP(pool.Spec.NetworkName, "10.0.0.2", ref); err != nil {
						t.Fatalf("canonical owner cannot restore legacy NIC: %v", err)
					}
					if ledger && rs.pool.Status.IPv4.Allocated["10.0.0.2"] != ref {
						t.Fatal("local legacy ledger record was dropped")
					}
				} else {
					if _, present := rs.pool.Status.IPv4.Allocated["10.0.0.2"]; present {
						t.Fatal("foreign namespace's bare NIC kept this pool's stale ledger record")
					}
					if ip, err := c.ipam.GetIP(pool.Spec.NetworkName, ""); err != nil || ip != "10.0.0.2" {
						t.Fatalf("foreign bare NIC reserved this network's address: %q, %v", ip, err)
					}
				}
			})
		}
	}
}

func TestRegistrationForeignSameMACStatusCannotBypassHijackGuard(t *testing.T) {
	pool := recoveryNewPool("pool1", "infra/net-a")
	pool.Status.LastUpdate = metav1.NewTime(time.Now().Add(-time.Hour))
	c, rs, _ := recoveryNewController(t, pool)
	vm := recoveryNewStatuslessVMNetCfg("tenant", "vm", "10.0.0.2", "02-AA-BB-CC-DD-01", pool.Spec.NetworkName, 30*time.Minute)
	vm.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{NetworkName: "net-a", MACAddress: "02:aa:bb:cc:dd:01", Status: "OK"},
	}
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{vm}
	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatal(err)
	}
	if ip, err := c.ipam.GetIP(pool.Spec.NetworkName, ""); err != nil || ip != "10.0.0.2" {
		t.Fatalf("foreign same-MAC status exempted a guarded request: %q, %v", ip, err)
	}
}

func TestRegistrationForeignSameMACStatusCannotEstablishClaim(t *testing.T) {
	pool := recoveryNewPool("pool1", "infra/net-a")
	c, rs, _ := recoveryNewController(t, pool)
	request := recoveryNewStatuslessVMNetCfg("tenant", "aaa-request", "10.0.0.2", "02-AA-BB-CC-DD-01", pool.Spec.NetworkName, 2*time.Hour)
	request.Status.NetworkConfig = []kihv1.NetworkConfigStatus{
		{NetworkName: "net-a", MACAddress: "02:aa:bb:cc:dd:01", Status: "OK"},
	}
	established := recoveryNewVMNetCfg("tenant", "established", "10.0.0.2", "02:00:00:00:00:02", pool.Spec.NetworkName)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{request, established}
	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ipam.ReclaimIP(pool.Spec.NetworkName, "10.0.0.2", util.AllocationRef("tenant", "established", "02:00:00:00:00:02")); err != nil {
		t.Fatalf("foreign status outranked the established owned assignment: %v", err)
	}
	if _, err := c.ipam.ReclaimIP(pool.Spec.NetworkName, "10.0.0.2", util.AllocationRef("tenant", "aaa-request", "02:aa:bb:cc:dd:01")); err == nil {
		t.Fatal("request adopted its foreign same-MAC status as an owned assignment")
	}
}

func TestRegistrationOwnedStatusMatchesCanonicalMAC(t *testing.T) {
	pool := recoveryNewPool("pool1", "infra/net-a")
	c, rs, _ := recoveryNewController(t, pool)
	vm := recoveryNewVMNetCfg("infra", "vm", "10.0.0.2", "02-AA-BB-CC-DD-01", "net-a")
	vm.Status.NetworkConfig[0].MACAddress = "02:aa:bb:cc:dd:01"
	vm.Status.NetworkConfig[0].NetworkName = pool.Spec.NetworkName
	vm.Status.NetworkConfig[0].Status = "ERROR"
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{vm}
	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatal(err)
	}
	if ip, err := c.ipam.GetIP(pool.Spec.NetworkName, ""); err != nil || ip != "10.0.0.2" {
		t.Fatalf("MAC spelling drift hid the owned ERROR status and reserved a rejected claim: %q, %v", ip, err)
	}
}
