package ippool

// Recovery regression tests: a pool which registers again after the
// startup gate dropped its retries (an UPDATE resync recovery of an
// application already in APP_RUNNING) must protect every durable claim of
// its network before the registration publishes the allocator. The
// durable claims come from the ownership ledger the pool status survived
// with and from the recorded assignments of the vmnetcfg objects - the
// ledger alone is not a complete inventory, because main could persist a
// vmnetcfg assignment after the pool status write failed.
//
// The tests execute the real allocator- and state-publication steps of
// registerIPPool: the subnet registration, the exclude pass, the claim
// protection, the status rebuild, the metrics reset and the cache
// publication. The host-level steps of the production registration -
// adding the server ip to the bind interface and starting the dhcp
// listener - stay outside the test boundary (they modify host interfaces
// and open privileged listeners); nothing the claim protection depends on
// is simulated. The binding restoration is exercised through the exact
// primitives the vmnetcfg binding path runs (the owner-validated reclaim
// of the recorded address, the dhcp lease registration and the fresh
// auto-allocation); the controller-level reconciliation against the
// published state is covered by the vmnetcfg-side recovery tests.

import (
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// recoveryNewPool builds the one-address pool of the review scenario: the
// range holds exactly 10.0.0.2, so a fresh allocation can only take the
// recorded address if the protection missed its claim.
func recoveryNewPool(name, network string) *kihv1.IPPool {
	return &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
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

// recoveryRegistrationSteps executes the allocator- and state-publication
// steps of registerIPPool for a validated pool projection. It returns the
// protection error instead of failing the test, so the discovery-failure
// regression can assert that the sequence aborts before the publication.
func recoveryRegistrationSteps(t *testing.T, c *Controller, pool *kihv1.IPPool) (map[string]string, error) {
	t.Helper()

	// register the new subnet in ipam
	if err := c.ipam.NewSubnet(
		pool.Spec.NetworkName,
		pool.Spec.IPv4Config.Subnet,
		pool.Spec.IPv4Config.Pool.Start,
		pool.Spec.IPv4Config.Pool.End,
	); err != nil {
		t.Fatalf("registering the subnet: %s", err)
	}

	// mark the exclude ips as used
	for _, v := range pool.Spec.IPv4Config.Pool.Exclude {
		if _, err := c.ipam.ReclaimIP(pool.Spec.NetworkName, v, ipam.ExcludedOwner); err != nil {
			t.Fatalf("excluding ip %s: %s", v, err)
		}
	}

	// pin the persisted claims of the pool before it becomes visible to
	// fresh allocations
	protectedClaims, err := c.protectPersistedClaims(pool)
	if err != nil {
		return nil, err
	}

	// rebuild the pool status after restarting the process
	rPool, err := c.resetIPPoolStatus(pool, protectedClaims)
	if err != nil {
		t.Fatalf("rebuilding the pool status: %s", err)
	}

	// reset the pool metrics after restarting the process
	if err := c.resetIPPoolMetrics(pool); err != nil {
		t.Fatalf("resetting the pool metrics: %s", err)
	}

	// publish the pool: this is the point from which fresh allocations
	// can see the allocator
	if err := c.cache.Add(rPool); err != nil {
		t.Fatalf("publishing the pool into the cache: %s", err)
	}

	return protectedClaims, nil
}

// recoveryNewController wires a controller against the rest state and
// returns the state so the test can seed its fixtures.
func recoveryNewController(t *testing.T, stored *kihv1.IPPool) (*Controller, *ippoolBehaviorRestState, *httptest.Server) {
	t.Helper()

	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	t.Cleanup(srv.Close)

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)

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

	stored := recoveryNewPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{"10.0.0.2": legacyRef}

	c, rs, _ := recoveryNewController(t, stored)
	pool := recoveryNewPool("pool1", "net-a")

	claims, err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the claim is pinned and republished under the canonical spelling:
	// both representations must agree with the restoring binding's
	// identity, otherwise the same logical owner is rejected as a foreign
	// owner
	if got := claims["10.0.0.2"]; got != canonicalRef {
		t.Errorf("protected claim = %q, want the canonical reference %q", got, canonicalRef)
	}
	if got := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; got != canonicalRef {
		t.Errorf("republished ledger entry = %q, want the canonical reference %q", got, canonicalRef)
	}
	if got := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; got == legacyRef {
		t.Error("the raw legacy spelling must not be republished")
	}

	// the published pool carries the canonical record
	published, pubErr := c.cache.Get("pool", "net-a")
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
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", canonicalRef); err != nil {
		t.Errorf("the restoring binding reclaiming its own recorded address: %s", err)
	}
	if err := c.dhcp.AddLease(legacyMAC, "net-a", "10.0.0.2", legacyNamespace+"/"+legacyVMName); err != nil {
		t.Errorf("restoring the dhcp lease: %s", err)
	}
	if !c.dhcp.CheckLease(legacyMAC) {
		t.Error("the restored binding must serve its lease")
	}

	// a genuinely different owner is still rejected
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", util.AllocationRef("default", "vm-other", "02:00:00:00:00:99")); err == nil {
		t.Error("a different owner must not reclaim the protected claim")
	}
}

// TestRegistrationProtectsTheSpecOnlyClaim: the review's late-recovery
// differential. The existing vmnetcfg persistently claims the only pool
// address while the pool status carries no entry for it (a historical
// partial write). The recovering registration must pin the spec claim
// before the publication, so a fresh allocation cannot take the address
// and the original binding restores it afterwards.
func TestRegistrationProtectsTheSpecOnlyClaim(t *testing.T) {
	const (
		oldNamespace = "default"
		oldVMName    = "vm-old"
		oldMAC       = "02:00:00:00:00:10"
	)

	oldRef := util.AllocationRef(oldNamespace, oldVMName, oldMAC)

	// the ledger lost the record: the pool status survived without the
	// entry of the existing binding
	stored := recoveryNewPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		recoveryNewVMNetCfg(oldNamespace, oldVMName, "10.0.0.2", oldMAC, "net-a"),
	}
	pool := recoveryNewPool("pool1", "net-a")

	claims, err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the spec claim is pinned and republished under its canonical owner
	if got := claims["10.0.0.2"]; got != oldRef {
		t.Errorf("protected claim = %q, want %q", got, oldRef)
	}
	if got := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; got != oldRef {
		t.Errorf("republished ledger entry = %q, want %q", got, oldRef)
	}
	if used := c.ipam.Used("net-a"); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the spec claim is pinned)", used)
	}

	// a new vm requesting an automatic address must not receive the
	// existing address: the one-address pool is exhausted by the pin
	// (the application is past its startup gate here - the controller's
	// appStatus is APP_RUNNING - so no global deferral masks this)
	if _, err := c.ipam.GetIP("net-a", ""); err == nil {
		t.Error("a fresh allocation must not receive the existing vm's address")
	}

	// the original binding restores its own address and dhcp lease through
	// the production primitives
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", oldRef); err != nil {
		t.Errorf("the original binding restoring its recorded address: %s", err)
	}
	if err := c.dhcp.AddLease(oldMAC, "net-a", "10.0.0.2", oldNamespace+"/"+oldVMName); err != nil {
		t.Errorf("restoring the dhcp lease: %s", err)
	}
	if got := c.dhcp.GetLease(oldMAC).ClientIP.String(); got != "10.0.0.2" {
		t.Errorf("the restored lease ip = %q, want the recorded 10.0.0.2", got)
	}

	// a foreign binding cannot take the protected claim
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", util.AllocationRef("other-ns", "vm-other", "02:00:00:00:00:99")); err == nil {
		t.Error("a foreign binding must not reclaim the protected claim")
	}
}

// TestRegistrationSweepCoversNamespacesAndMalformedNics: the claim sweep
// must cover every namespace and every nic of the network, must not stop
// at a malformed earlier nic, must keep the exclude pass authoritative,
// and must skip out-of-range claims without publishing them.
func TestRegistrationSweepCoversNamespacesAndMalformedNics(t *testing.T) {
	stored := recoveryNewPool("pool1", "net-a")
	stored.Spec.IPv4Config.Pool.Start = "10.0.0.2"
	stored.Spec.IPv4Config.Pool.End = "10.0.0.5"
	stored.Spec.IPv4Config.Pool.Exclude = []string{"10.0.0.4"}
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)

	// ns-a/vm-a: the malformed nic comes first and must not stop the
	// protection of the later healthy nic
	malformedFirst := recoveryNewVMNetCfg("ns-a", "vm-a", "10.0.0.3", "not-a-mac", "net-a")
	malformedFirst.Spec.NetworkConfig = append(malformedFirst.Spec.NetworkConfig, kihv1.NetworkConfig{
		IPAddress: "10.0.0.2", MACAddress: "02:00:00:00:00:11", NetworkName: "net-a",
	})

	// ns-b/vm-b: a claim from another namespace of the same network
	foreignNamespace := recoveryNewVMNetCfg("ns-b", "vm-b", "10.0.0.5", "02:00:00:00:00:12", "net-a")

	// ns-c/vm-c: a claim on an excluded address - the exclude pass wins
	excludedClaim := recoveryNewVMNetCfg("ns-c", "vm-c", "10.0.0.4", "02:00:00:00:00:13", "net-a")

	// ns-d/vm-d: an out-of-range claim which the allocator can never hand
	// out, and a nic of another network which is not this pool's business
	outOfRange := recoveryNewVMNetCfg("ns-d", "vm-d", "10.0.0.99", "02:00:00:00:00:14", "net-a")
	outOfRange.Spec.NetworkConfig = append(outOfRange.Spec.NetworkConfig, kihv1.NetworkConfig{
		IPAddress: "10.0.1.2", MACAddress: "02:00:00:00:00:15", NetworkName: "net-other",
	})

	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{malformedFirst, foreignNamespace, excludedClaim, outOfRange}

	pool := stored.DeepCopy()

	claims, err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the healthy claim after the malformed nic is pinned under its owner
	healthyRef := util.AllocationRef("ns-a", "vm-a", "02:00:00:00:00:11")
	if got := claims["10.0.0.2"]; got != healthyRef {
		t.Errorf("protected claim of the later healthy nic = %q, want %q", got, healthyRef)
	}
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", healthyRef); err != nil {
		t.Errorf("the healthy nic's own reclaim: %s", err)
	}

	// the malformed nic's address is protected without an owner identity:
	// neither a fresh allocation nor any binding can take it
	if _, err := c.ipam.GetIP("net-a", "10.0.0.3"); err == nil {
		t.Error("the malformed nic's address must not be handable to a fresh allocation")
	}
	if _, ok := claims["10.0.0.3"]; ok {
		t.Error("an ownerless pin carries no ledger entry")
	}

	// the claim of the other namespace is pinned under its owner
	foreignRef := util.AllocationRef("ns-b", "vm-b", "02:00:00:00:00:12")
	if got := claims["10.0.0.5"]; got != foreignRef {
		t.Errorf("protected claim of the other namespace = %q, want %q", got, foreignRef)
	}

	// the excluded address stays reserved for the exclude pass
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.4", util.AllocationRef("ns-c", "vm-c", "02:00:00:00:00:13")); err == nil {
		t.Error("a binding must not reclaim an excluded address")
	}

	// the out-of-range claim is neither pinned nor published
	if _, ok := claims["10.0.0.99"]; ok {
		t.Error("an out-of-range claim must not be published")
	}
	if _, ok := claims["10.0.1.2"]; ok {
		t.Error("a claim of another network must not be published")
	}

	// the accounting: the four in-range addresses are all reserved (two
	// owner pins, one ownerless pin, one exclude)
	if used := c.ipam.Used("net-a"); used != 4 {
		t.Errorf("ipam used = %d, want 4", used)
	}
	if _, err := c.ipam.GetIP("net-a", ""); err == nil {
		t.Error("the pool must be exhausted after the protection")
	}

	// the republished ledger carries the owner pins and the exclude
	// entry, but no entry for the ownerless pin or the skipped claims
	ledger := rs.lastBody.Status.IPv4.Allocated
	if got := ledger["10.0.0.2"]; got != healthyRef {
		t.Errorf("ledger entry of the healthy nic = %q, want %q", got, healthyRef)
	}
	if got := ledger["10.0.0.5"]; got != foreignRef {
		t.Errorf("ledger entry of the other namespace = %q, want %q", got, foreignRef)
	}
	if got := ledger["10.0.0.4"]; got != ipam.ExcludedOwner {
		t.Errorf("ledger entry of the excluded address = %q, want %q", got, ipam.ExcludedOwner)
	}
	if len(ledger) != 3 {
		t.Errorf("ledger = %v, want exactly the three publishable entries", ledger)
	}
}

// TestRegistrationWithoutTheClaimSnapshotDoesNotPublish: when the
// authoritative vmnetcfg snapshot cannot be obtained, the registration
// must fail before the status rebuild and the cache publication - an
// unseen claim would otherwise be handed to a fresh allocation. The pool
// stays unregistered, so no allocator state is exposed at all.
func TestRegistrationWithoutTheClaimSnapshotDoesNotPublish(t *testing.T) {
	stored := recoveryNewPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	rs.failVMNetCfgList = true
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		recoveryNewVMNetCfg("default", "vm-old", "10.0.0.2", "02:00:00:00:00:10", "net-a"),
	}
	pool := recoveryNewPool("pool1", "net-a")

	if _, err := recoveryRegistrationSteps(t, c, pool); err == nil {
		t.Fatal("the registration must fail when the claim snapshot cannot be obtained")
	}

	// nothing was published: no status write, no cached pool
	if rs.putCount != 0 {
		t.Errorf("pool status writes = %d, want 0 (the failure precedes the publication)", rs.putCount)
	}
	if _, cacheErr := c.cache.Get("pool", "net-a"); cacheErr == nil {
		t.Error("the pool must not be published into the cache")
	}

	// the retry re-runs the whole protection once the snapshot is
	// available: the failed attempt was torn down like the production
	// registerPoolWithTeardown does (its allocator step removed the
	// subnet), so the retry starts from a fresh registration state
	c.ipam.DeleteSubnet("net-a")
	rs.failVMNetCfgList = false
	claims, err := recoveryRegistrationSteps(t, c, pool)
	if err != nil {
		t.Fatalf("the retried registration steps: %s", err)
	}
	oldRef := util.AllocationRef("default", "vm-old", "02:00:00:00:00:10")
	if got := claims["10.0.0.2"]; got != oldRef {
		t.Errorf("protected claim of the retry = %q, want %q", got, oldRef)
	}
	if _, err := c.cache.Get("pool", "net-a"); err != nil {
		t.Errorf("the retried registration must publish the pool: %s", err)
	}
}
