package dhcp

import (
	"fmt"
	"testing"
)

// the lease identity must be the canonical colon form of the mac address:
// a hyphen or uppercase spelling of the same address must resolve to the
// same lease and a second spelling must be rejected as duplicate.
func TestDHCPLeaseIdentityIsCanonical(t *testing.T) {
	handler := New()
	if err := handler.AddLease("aa-bb-cc-dd-ee-ff", "net-a", "10.10.10.20", "default/testvm"); err != nil {
		t.Fatalf("failed to add the hyphen-form lease: %s", err)
	}

	if !handler.CheckLease("aa:bb:cc:dd:ee:ff") {
		t.Error("hyphen-form lease not resolvable in canonical colon form")
	}
	if !handler.CheckLease("AA-BB-CC-DD-EE-FF") {
		t.Error("uppercase hyphen-form lease not resolvable in canonical colon form")
	}

	if got := handler.GetLease("AA:BB:CC:DD:EE:FF"); got.ClientIP.String() != "10.10.10.20" {
		t.Errorf("lease client ip = %q, want 10.10.10.20", got.ClientIP.String())
	}

	if err := handler.AddLease("aa:bb:cc:dd:ee:ff", "net-a", "10.10.10.21", "default/othervm"); err == nil {
		t.Fatal("duplicate spelling of the lease was accepted")
	} else if got := fmt.Sprintf("%s", err); got != "lease for hwaddr aa:bb:cc:dd:ee:ff already exists" {
		t.Errorf("duplicate error = %q, want the canonical spelling message", got)
	}

	if err := handler.DeleteLease("AA-BB-CC-DD-EE-FF"); err != nil {
		t.Fatalf("the canonical lease must be deletable via another spelling: %s", err)
	}
	if handler.CheckLease("aa:bb:cc:dd:ee:ff") {
		t.Error("lease still exists after deleting it through another spelling")
	}
}
