package ipam

import (
	"errors"
	"testing"
)

// Tests for the owner-aware reclaim contract which backs the registration
// seeding barrier: a recovering pool pins the persisted ownership claims of
// its bindings before fresh allocations can run, the restoring binding
// reclaims its own recorded address idempotently, and every foreign claim
// is rejected instead of being silently taken.

func reclaimTestAllocator(t *testing.T) *IPAllocator {
	t.Helper()

	a := NewIPAllocator()
	if err := a.NewSubnet("net", "192.168.99.0/24", "192.168.99.1", "192.168.99.3"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	return a
}

func TestReclaimIPAllocatesFreeAddressUnderOwner(t *testing.T) {
	a := reclaimTestAllocator(t)

	ip, err := a.ReclaimIP("net", "192.168.99.1", "ns/vm [02:00:00:00:00:01]")
	if err != nil {
		t.Fatalf("ReclaimIP: %v", err)
	}
	if ip != "192.168.99.1" {
		t.Errorf("reclaimed ip = %q, want the requested address", ip)
	}
	if got := a.Used("net"); got != 1 {
		t.Errorf("used = %d, want 1", got)
	}

	// the own reclaim stays idempotent
	if _, err := a.ReclaimIP("net", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); err != nil {
		t.Errorf("idempotent reclaim = %v, want nil", err)
	}

	// a foreign reclaim is rejected with a classifiable error
	if _, err := a.ReclaimIP("net", "192.168.99.1", "ns/other [02:00:00:00:00:02]"); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("foreign reclaim = %v, want ErrIPForeignOwner", err)
	}

	// a fresh allocation must never take the owned address
	if _, err := a.GetIP("net", ""); err != nil {
		t.Fatalf("allocating the next free address: %v", err)
	}
	if _, err := a.GetIP("net", ""); err != nil {
		t.Fatalf("allocating the third address: %v", err)
	}
	if used := a.Used("net"); used != 3 {
		t.Errorf("used = %d, want 3 (the own claim started the count)", used)
	}
}

func TestReclaimIPRejectsAnonymousAndExcludedOwners(t *testing.T) {
	a := reclaimTestAllocator(t)

	// a plain auto-allocation carries no reclaim identity and must not be
	// adopted by a nameless binding
	if _, err := a.GetIP("net", "192.168.99.1"); err != nil {
		t.Fatalf("allocating: %v", err)
	}
	if _, err := a.ReclaimIP("net", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("reclaim of an anonymous allocation = %v, want ErrIPForeignOwner", err)
	}

	// the exclude pseudo-owner can never be claimed by a vm binding
	if _, err := a.ReclaimIP("net", "192.168.99.2", ExcludedOwner); err != nil {
		t.Fatalf("excluding: %v", err)
	}
	if _, err := a.ReclaimIP("net", "192.168.99.2", "ns/vm [02:00:00:00:00:01]"); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("reclaim of an excluded address = %v, want ErrIPForeignOwner", err)
	}
}

func TestReclaimIPValidation(t *testing.T) {
	a := reclaimTestAllocator(t)

	if _, err := a.ReclaimIP("net", "192.168.99.1", ""); err == nil {
		t.Error("empty owner must be rejected")
	}
	if _, err := a.ReclaimIP("ghost", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); !errors.Is(err, ErrSubnetNotFound) {
		t.Errorf("unknown subnet = %v, want ErrSubnetNotFound", err)
	}
	if _, err := a.ReclaimIP("net", "192.168.98.1", "ns/vm [02:00:00:00:00:01]"); err == nil {
		t.Error("address outside the pool range must be rejected")
	}
	if _, err := a.ReclaimIP("net", "192.168.99.255", "ns/vm [02:00:00:00:00:01]"); err == nil {
		t.Error("the broadcast address must be rejected")
	}
}

func TestAdoptIPPromotesVerifiedAnonymousAllocation(t *testing.T) {
	a := reclaimTestAllocator(t)

	// the earlier sync of this binding allocated anonymously (its durable
	// write failed afterwards): the lease-verified adopt promotes it
	if _, err := a.GetIP("net", "192.168.99.1"); err != nil {
		t.Fatalf("allocating: %v", err)
	}
	if err := a.AdoptIP("net", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); err != nil {
		t.Fatalf("AdoptIP: %v", err)
	}
	if err := a.AdoptIP("net", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); err != nil {
		t.Errorf("idempotent adopt = %v, want nil", err)
	}

	// a foreign named owner cannot adopt it
	if err := a.AdoptIP("net", "192.168.99.1", "ns/other [02:00:00:00:00:02]"); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("foreign adopt = %v, want ErrIPForeignOwner", err)
	}

	// a foreign named owner cannot reclaim it either
	if _, err := a.ReclaimIP("net", "192.168.99.1", "ns/other [02:00:00:00:00:02]"); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("foreign reclaim = %v, want ErrIPForeignOwner", err)
	}
}

func TestAdoptIPClaimsFreeAddressAfterClaimLoss(t *testing.T) {
	a := reclaimTestAllocator(t)

	// a binding whose lease survived the restart but whose allocator claim
	// was lost can adopt its address while the allocator knows nothing
	if err := a.AdoptIP("net", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); err != nil {
		t.Fatalf("AdoptIP on the free address: %v", err)
	}
	if got := a.Used("net"); got != 1 {
		t.Errorf("used = %d, want 1 after the adopt", got)
	}
}

func TestReleaseIPForgetsTheOwner(t *testing.T) {
	a := reclaimTestAllocator(t)

	if _, err := a.ReclaimIP("net", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); err != nil {
		t.Fatalf("ReclaimIP: %v", err)
	}
	if err := a.ReleaseIP("net", "192.168.99.1"); err != nil {
		t.Fatalf("ReleaseIP: %v", err)
	}

	// the released address is free again: a different owner may take it
	if _, err := a.ReclaimIP("net", "192.168.99.1", "ns/successor [02:00:00:00:00:09]"); err != nil {
		t.Errorf("reclaim after the release = %v, want nil", err)
	}
}

// The compensating release of a raced cleanup is owner-validated: it only
// frees the address while the reservation still carries this owner's
// reference, so an allocation a successor took over in the meantime (a
// fresh anonymous allocation or another owner's named reclaim) survives
// the stale cleanup, and every already-released outcome is converged.
func TestReleaseIPOwnedByOnlyReleasesTheOwnClaim(t *testing.T) {
	a := reclaimTestAllocator(t)

	if _, err := a.ReclaimIP("net", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); err != nil {
		t.Fatalf("ReclaimIP: %v", err)
	}

	// a foreign identity must not release the claim
	if err := a.ReleaseIPOwnedBy("net", "192.168.99.1", "ns/other [02:00:00:00:00:02]"); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("foreign release = %v, want ErrIPForeignOwner", err)
	}
	if got := a.Used("net"); got != 1 {
		t.Errorf("used = %d, want 1 after the rejected foreign release", got)
	}

	// the own release frees the address and forgets the owner
	if err := a.ReleaseIPOwnedBy("net", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); err != nil {
		t.Fatalf("own release: %v", err)
	}
	if got := a.Used("net"); got != 0 {
		t.Errorf("used = %d, want 0 after the own release", got)
	}
	if _, err := a.ReclaimIP("net", "192.168.99.1", "ns/successor [02:00:00:00:00:09]"); err != nil {
		t.Errorf("successor reclaim after the release = %v, want nil", err)
	}

	// a successor's anonymous allocation survives the stale owner's cleanup
	if err := a.ReleaseIPOwnedBy("net", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("release of an anonymous successor allocation = %v, want ErrIPForeignOwner", err)
	}
	if got := a.Used("net"); got != 1 {
		t.Errorf("used = %d, want 1 (the successor keeps the address)", got)
	}
}

func TestReleaseIPOwnedByConvergedOutcomes(t *testing.T) {
	a := reclaimTestAllocator(t)

	// an already-free address is converged
	if err := a.ReleaseIPOwnedBy("net", "192.168.99.2", "ns/vm [02:00:00:00:00:01]"); !errors.Is(err, ErrIPAlreadyFree) {
		t.Errorf("release of a free address = %v, want ErrIPAlreadyFree", err)
	}

	// an address outside the registered subnet was never allocated
	if err := a.ReleaseIPOwnedBy("net", "10.0.0.1", "ns/vm [02:00:00:00:00:01]"); !errors.Is(err, ErrIPNotInCidr) {
		t.Errorf("release outside the subnet = %v, want ErrIPNotInCidr", err)
	}

	// a subnet without allocation state has nothing to release
	if err := a.ReleaseIPOwnedBy("gone", "192.168.99.1", "ns/vm [02:00:00:00:00:01]"); !errors.Is(err, ErrSubnetNotFound) {
		t.Errorf("release without a subnet = %v, want ErrSubnetNotFound", err)
	}

	// an empty owner identity is a caller error, not a converged release
	if err := a.ReleaseIPOwnedBy("net", "192.168.99.1", ""); err == nil {
		t.Error("release with an empty owner must fail")
	}
}

// A binding's fresh auto-allocation is a named reservation from the
// moment it exists: the delayed cleanup of a removed nic can release it
// through the owner-validated release, while no other owner can displace
// it and the binding's own restore stays idempotent.
func TestAllocateIPNamesTheFreshAllocation(t *testing.T) {
	a := reclaimTestAllocator(t)

	const owner = "ns/vm [02:00:00:00:00:01]"

	ip, err := a.AllocateIP("net", owner)
	if err != nil {
		t.Fatalf("AllocateIP: %v", err)
	}
	if ip != "192.168.99.1" {
		t.Errorf("allocated ip = %q, want the first free address", ip)
	}

	// the reservation carries the binding's identity
	if _, err := a.ReclaimIP("net", ip, owner); err != nil {
		t.Errorf("own reclaim after the named allocation: %v", err)
	}
	if _, err := a.ReclaimIP("net", ip, "ns/other [02:00:00:00:00:02]"); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("foreign reclaim = %v, want ErrIPForeignOwner", err)
	}
	if err := a.ReleaseIPOwnedBy("net", ip, owner); err != nil {
		t.Errorf("owner-validated release of the named allocation: %v", err)
	}

	// an empty owner identity is a caller error
	if _, err := a.AllocateIP("net", ""); err == nil {
		t.Error("AllocateIP with an empty owner must fail")
	}
	if _, err := a.AllocateIP("gone", owner); !errors.Is(err, ErrSubnetNotFound) {
		t.Errorf("AllocateIP without a subnet = %v, want ErrSubnetNotFound", err)
	}
}

// The ownerless protection pin blocks every fresh allocation and every
// binding while recording which object's claim it protects: the binding
// of that vm retakes its own pin through the claimant reclaim once its
// identity is corrected, and nothing else can ever take it.
func TestProtectIPAndReclaimIPClaimant(t *testing.T) {
	a := reclaimTestAllocator(t)

	const (
		vmRef        = "ns/vm"
		correctedMac = "02:00:00:00:00:01"
	)
	owner := "ns/vm [" + correctedMac + "]"

	if err := a.ProtectIP("net", "192.168.99.2", vmRef); err != nil {
		t.Fatalf("ProtectIP: %v", err)
	}

	// the pin blocks allocations and reclaims of every identity
	if _, err := a.GetIP("net", "192.168.99.2"); err == nil {
		t.Error("a fresh allocation must not take the pinned address")
	}
	if _, err := a.ReclaimIP("net", "192.168.99.2", owner); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("plain reclaim of the own pin = %v, want ErrIPForeignOwner", err)
	}

	// the claimant of the attribution retakes its own pin
	if _, err := a.ReclaimIPClaimant("net", "192.168.99.2", owner, vmRef); err != nil {
		t.Fatalf("the corrected binding retaking its attributed pin: %v", err)
	}
	// the promotion is durable and idempotent
	if _, err := a.ReclaimIP("net", "192.168.99.2", owner); err != nil {
		t.Errorf("own reclaim after the promotion: %v", err)
	}

	// an unattributed pin is never reclaimable, and a foreign claimant
	// cannot take a pin attributed to another vm
	if err := a.ProtectIP("net", "192.168.99.3", ""); err != nil {
		t.Fatalf("unattributed ProtectIP: %v", err)
	}
	if _, err := a.ReclaimIPClaimant("net", "192.168.99.3", owner, vmRef); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("claimant reclaim of an unattributed pin = %v, want ErrIPForeignOwner", err)
	}

	if err := a.ProtectIP("net", "192.168.99.1", "ns/other-vm"); err != nil {
		t.Fatalf("ProtectIP of another vm's claim: %v", err)
	}
	if _, err := a.ReclaimIPClaimant("net", "192.168.99.1", owner, vmRef); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("claimant reclaim of a foreign-attributed pin = %v, want ErrIPForeignOwner", err)
	}

	// a released pin forgets its attribution: a later claimant cannot
	// promote a re-pinned anonymous state it does not own
	if err := a.ReleaseIP("net", "192.168.99.1"); err != nil {
		t.Fatalf("releasing the foreign-attributed pin: %v", err)
	}
	if _, err := a.GetIP("net", "192.168.99.1"); err != nil {
		t.Fatalf("re-allocating the released address anonymously: %v", err)
	}
	if _, err := a.ReclaimIPClaimant("net", "192.168.99.1", owner, "ns/other-vm"); !errors.Is(err, ErrIPForeignOwner) {
		t.Errorf("claimant reclaim of a stale attribution = %v, want ErrIPForeignOwner", err)
	}
}
