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
