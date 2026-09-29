package ippool

// F09 regression tests: the claim sweep pinned a recorded nic under the
// claimant identity of the frozen LIST snapshot, and the re-verification
// compared only the canonical mac, the network and the address. a
// spec.vmname edit (or a same-name replacement of the claiming object)
// which landed between the LIST and the re-verification read kept the
// tuple comparison satisfied, so the registration published the old
// owner's pin: every restore and release derives the owner reference
// from the current spec.vmname, so neither the current claimant (who was
// rejected as a foreign owner of its own recorded address) nor the gone
// identity (whose binding no longer existed) could ever reclaim or
// release it, and the durable address was stranded.
//
// The fix covers both ownership paths of a recorded address. the spec
// claim records the full claimant identity (the vm name and the object
// uid) and fails the registration when the fresh read attributes the
// still-recorded nic to a different claimant: the failed attempt is
// torn down before any publication, and the retried sweep runs against
// the settled spec and pins the address under the current claimant. the
// persisted ledger record goes stale the same way when a binding edits
// its vmname, and it cannot heal by retrying (nothing rewrites it while
// the pool is unpublished): the ledger verification therefore
// re-attributes the record to the vm the fresh spec names, keeping the
// reservation continuous, and the displaced spec survivor of a
// ledger-pinned address is identity-verified as well, so an edit which
// lands mid-registration fails the attempt and the retry re-attributes.
// Dropping a pin instead would expose the still-recorded address to
// fresh allocations, and enforcing vmname immutability only here would
// silently forbid a legitimate edit.

import (
	"errors"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// TestRegistrationFailsOnTheSwappedClaimant: the binding listed as the
// claimant of the recorded nic has its spec.vmname edited to another vm
// between the frozen LIST response and the re-verification read, the nic
// tuple retained. The registration must fail before any publication, and
// the retried registration must pin the address under the current
// claimant: the snapshot identity stays rejected as a foreign owner,
// the current claimant reclaims its own recorded address, and a fresh
// allocation never receives it.
func TestRegistrationFailsOnTheSwappedClaimant(t *testing.T) {
	stored := recoveryNewPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	binding := recoveryNewVMNetCfg("default", "vm-a", "10.0.0.2", "02:00:00:00:00:10", "net-a")
	binding.UID = "uid-1"
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{binding}

	// the vmname is edited between the frozen LIST response and the
	// re-verification read: the nic tuple is retained, only the claiming
	// vm changes
	rs.vmnetcfgListHook = func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.vmnetcfgs[0] = rs.vmnetcfgs[0].DeepCopy()
		rs.vmnetcfgs[0].Spec.VMName = "vm-b"
	}

	pool := recoveryNewPool("pool1", "net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err == nil {
		t.Fatal("the registration must fail when the recorded claimant changed while the pool was unpublished")
	}
	if rs.putCount != 0 {
		t.Errorf("pool status writes = %d, want 0 (the failure precedes the publication)", rs.putCount)
	}
	if _, cacheErr := c.cache.Get("pool", "net-a"); cacheErr == nil {
		t.Error("the pool must not be published into the cache")
	}

	// the edit settled: the retried registration sweeps the current spec
	// and pins the address under the current claimant
	rs.vmnetcfgListHook = nil
	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the retried registration steps: %s", err)
	}
	if used := c.ipam.Used("net-a"); used != 1 {
		t.Errorf("ipam used after the retry = %d, want 1", used)
	}
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", util.AllocationRef("default", "vm-a", "02:00:00:00:00:10")); !errors.Is(err, ipam.ErrIPForeignOwner) {
		t.Errorf("the snapshot identity must stay rejected as a foreign owner, got err %v", err)
	}
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", util.AllocationRef("default", "vm-b", "02:00:00:00:00:10")); err != nil {
		t.Errorf("the current claimant must reclaim its own recorded address: %s", err)
	}

	// the pool's range holds exactly the recorded address, so a fresh
	// allocation can only take it if the protection missed the claim
	if ip, err := c.ipam.GetIP("net-a", ""); err == nil {
		t.Errorf("a fresh allocation must never receive the recorded address, got ip %q", ip)
	}
}

// TestRegistrationFailsOnTheReplacedClaimantObject: the claiming object
// is deleted and recreated under the same name between the frozen LIST
// response and the re-verification read (a same-name/new-uid replacement
// with the tuple and the vm retained). The pin must not be attributed to
// the snapshot of the gone object: the registration fails, and the
// retried sweep attributes the pin to the object which now records the
// nic.
func TestRegistrationFailsOnTheReplacedClaimantObject(t *testing.T) {
	stored := recoveryNewPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)
	binding := recoveryNewVMNetCfg("default", "vm-a", "10.0.0.2", "02:00:00:00:00:10", "net-a")
	binding.UID = "uid-1"
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{binding}

	// the object is replaced under the same name between the frozen LIST
	// response and the re-verification read: same tuple, same vm, new uid
	rs.vmnetcfgListHook = func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.vmnetcfgs[0] = rs.vmnetcfgs[0].DeepCopy()
		rs.vmnetcfgs[0].UID = "uid-2"
	}

	pool := recoveryNewPool("pool1", "net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err == nil {
		t.Fatal("the registration must fail when the recorded claimant object was replaced while the pool was unpublished")
	}
	if rs.putCount != 0 {
		t.Errorf("pool status writes = %d, want 0 (the failure precedes the publication)", rs.putCount)
	}
	if _, cacheErr := c.cache.Get("pool", "net-a"); cacheErr == nil {
		t.Error("the pool must not be published into the cache")
	}

	// the replacement settled: the retried registration sweeps the
	// recreated object and pins its recorded address under its identity
	rs.vmnetcfgListHook = nil
	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the retried registration steps: %s", err)
	}
	if used := c.ipam.Used("net-a"); used != 1 {
		t.Errorf("ipam used after the retry = %d, want 1", used)
	}
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", util.AllocationRef("default", "vm-a", "02:00:00:00:00:10")); err != nil {
		t.Errorf("the recreated claimant must reclaim its own recorded address: %s", err)
	}
}

// TestRegistrationReattributesTheLedgerRecordOfTheSwappedClaimant: the
// durable-address case of the review's trigger. the pool status already
// records the address under the old claimant, so the ledger pass owns
// the pin and the spec claim only records as its displaced survivor:
// the spec-path re-verification never touches it. the persisted record
// went stale when the binding's spec.vmname was edited to another vm
// with the nic tuple retained - pre-fix the tuple match kept the pin
// under the recorded identity, which neither the current claimant nor
// the gone one could ever reclaim or release. the registration must
// re-attribute the record to the current claimant: the pin and the
// republished ledger entry carry the current vm's reference, and the
// restoring binding of the current vm reclaims its own recorded
// address.
func TestRegistrationReattributesTheLedgerRecordOfTheSwappedClaimant(t *testing.T) {
	stored := recoveryNewPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.0.0.2": util.AllocationRef("default", "vm-a", "02:00:00:00:00:10"),
	}

	c, rs, _ := recoveryNewController(t, stored)
	// the vmname edit landed before the registration: every read sees
	// the settled state, and the persisted record is stale against it
	binding := recoveryNewVMNetCfg("default", "vm-a", "10.0.0.2", "02:00:00:00:00:10", "net-a")
	binding.UID = "uid-1"
	binding.Spec.VMName = "vm-b"
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{binding}

	pool := recoveryNewPool("pool1", "net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	if got, ok := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; !ok || got != util.AllocationRef("default", "vm-b", "02:00:00:00:00:10") {
		t.Errorf("republished ledger entry = %q (present %t), want the re-attributed %q",
			got, ok, util.AllocationRef("default", "vm-b", "02:00:00:00:00:10"))
	}
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", util.AllocationRef("default", "vm-a", "02:00:00:00:00:10")); !errors.Is(err, ipam.ErrIPForeignOwner) {
		t.Errorf("the recorded identity must stay rejected as a foreign owner, got err %v", err)
	}
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", util.AllocationRef("default", "vm-b", "02:00:00:00:00:10")); err != nil {
		t.Errorf("the current claimant must reclaim its own recorded address: %s", err)
	}
	if ip, err := c.ipam.GetIP("net-a", ""); err == nil {
		t.Errorf("a fresh allocation must never receive the recorded address, got ip %q", ip)
	}
}

// TestRegistrationFailsOnTheSwappedClaimantOfALedgerRecord: the pool
// status already records the address under the old claimant and the
// vmname edit lands between the registration's reads: the ledger
// verification saw the old spec, the frozen LIST snapshot carried the
// old spec, and only the displaced survivor's fresh read sees the edit.
// the registration must fail before any publication, and the retried
// registration re-attributes the record to the current claimant.
func TestRegistrationFailsOnTheSwappedClaimantOfALedgerRecord(t *testing.T) {
	stored := recoveryNewPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.0.0.2": util.AllocationRef("default", "vm-a", "02:00:00:00:00:10"),
	}

	c, rs, _ := recoveryNewController(t, stored)
	binding := recoveryNewVMNetCfg("default", "vm-a", "10.0.0.2", "02:00:00:00:00:10", "net-a")
	binding.UID = "uid-1"
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{binding}

	// the vmname is edited after the frozen LIST response was served:
	// only the reads after the list see the edit, and the recorded
	// address is decided by the ledger pass, so the displaced spec
	// survivor is the only witness of the swap
	rs.vmnetcfgListHook = func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.vmnetcfgs[0] = rs.vmnetcfgs[0].DeepCopy()
		rs.vmnetcfgs[0].Spec.VMName = "vm-b"
	}

	pool := recoveryNewPool("pool1", "net-a")

	if err := recoveryRegistrationSteps(t, c, pool); err == nil {
		t.Fatal("the registration must fail when the claimant of a ledger-recorded address changed while the pool was unpublished")
	}
	if rs.putCount != 0 {
		t.Errorf("pool status writes = %d, want 0 (the failure precedes the publication)", rs.putCount)
	}
	if _, cacheErr := c.cache.Get("pool", "net-a"); cacheErr == nil {
		t.Error("the pool must not be published into the cache")
	}

	// the edit settled: the retried registration re-attributes the
	// persisted record to the current claimant
	rs.vmnetcfgListHook = nil
	if err := recoveryRegistrationSteps(t, c, pool); err != nil {
		t.Fatalf("the retried registration steps: %s", err)
	}
	if got, ok := rs.lastBody.Status.IPv4.Allocated["10.0.0.2"]; !ok || got != util.AllocationRef("default", "vm-b", "02:00:00:00:00:10") {
		t.Errorf("republished ledger entry = %q (present %t), want the re-attributed %q",
			got, ok, util.AllocationRef("default", "vm-b", "02:00:00:00:00:10"))
	}
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", util.AllocationRef("default", "vm-a", "02:00:00:00:00:10")); !errors.Is(err, ipam.ErrIPForeignOwner) {
		t.Errorf("the recorded identity must stay rejected as a foreign owner, got err %v", err)
	}
	if _, err := c.ipam.ReclaimIP("net-a", "10.0.0.2", util.AllocationRef("default", "vm-b", "02:00:00:00:00:10")); err != nil {
		t.Errorf("the current claimant must reclaim its own recorded address: %s", err)
	}
}
