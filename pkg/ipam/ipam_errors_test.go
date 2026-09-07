package ipam

import (
	"errors"
	"testing"
)

// the vmnetcfg controller classifies release outcomes through errors.Is on
// the package sentinels, so the error wording must stay out of the contract.
func TestIPAMReleaseOutcomesAreSentinelErrors(t *testing.T) {
	allocator := New()
	if err := allocator.NewSubnet("net", "192.168.53.0/29", "192.168.53.1", "192.168.53.6"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	ip, err := allocator.GetIP("net", "")
	if err != nil {
		t.Fatalf("GetIP: %v", err)
	}

	if err := allocator.ReleaseIP("net", ip); err != nil {
		t.Fatalf("ReleaseIP: %v", err)
	}

	if err := allocator.ReleaseIP("net", ip); !errors.Is(err, ErrIPAlreadyFree) {
		t.Errorf("second release = %v, want ErrIPAlreadyFree", err)
	}

	if err := allocator.ReleaseIP("ghost", ip); !errors.Is(err, ErrSubnetNotFound) {
		t.Errorf("release on an unknown subnet = %v, want ErrSubnetNotFound", err)
	}

	if _, err := allocator.GetIP("ghost", ""); !errors.Is(err, ErrSubnetNotFound) {
		t.Errorf("allocation on an unknown subnet = %v, want ErrSubnetNotFound", err)
	}

	// an out-of-subnet address is always a hard error, never a release case
	if err := allocator.ReleaseIP("net", "192.168.54.4"); err == nil || errors.Is(err, ErrIPAlreadyFree) {
		t.Errorf("out-of-subnet release = %v, want a plain failing error", err)
	}

	// an empty ip is a caller error: the controllers must surface it
	if err := allocator.ReleaseIP("net", ""); err == nil || errors.Is(err, ErrIPAlreadyFree) || errors.Is(err, ErrSubnetNotFound) {
		t.Errorf("empty-ip release = %v, want a plain failing error", err)
	}
}
