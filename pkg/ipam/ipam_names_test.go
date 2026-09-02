package ipam

import (
	"testing"
)

// a repeated subnet name must be rejected while keeping the allocation
// bitmap of the original registration; silently replacing it would drop
// live addresses from accounting and reissue them to other clients.
func TestIPAMRejectsDuplicateSubnetNames(t *testing.T) {
	allocator := New()
	if err := allocator.NewSubnet("default/net", "10.10.10.0/29", "10.10.10.1", "10.10.10.6"); err != nil {
		t.Fatalf("first registration failed: %s", err)
	}

	firstIP, err := allocator.GetIP("default/net", "")
	if err != nil {
		t.Fatalf("allocation failed: %s", err)
	}

	err = allocator.NewSubnet("default/net", "10.20.20.0/29", "10.20.20.1", "10.20.20.6")
	if err == nil || err.Error() != "network default/net already exists" {
		t.Fatalf("duplicate registration error = %v, want network default/net already exists", err)
	}

	// the original allocation state must survive the rejection
	followUpIP, err := allocator.GetIP("default/net", "")
	if err != nil {
		t.Fatalf("allocation after the rejected duplicate failed: %s", err)
	}
	if followUpIP == firstIP {
		t.Errorf("allocation %q reissued the already occupied first address", followUpIP)
	}
	if got := allocator.Used("default/net"); got != 2 {
		t.Errorf("used = %d, want 2 (original allocation kept, none added)", got)
	}
}
