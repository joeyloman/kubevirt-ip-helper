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

	// an out-of-subnet address was never allocated by ipam: a release
	// classifies through ErrIPNotInCidr and cleanup must converge on it,
	// while it never silently passes as a free-already release
	if err := allocator.ReleaseIP("net", "192.168.54.4"); !errors.Is(err, ErrIPNotInCidr) {
		t.Errorf("out-of-subnet release = %v, want ErrIPNotInCidr", err)
	}

	// an empty ip is a caller error: the controllers must surface it
	if err := allocator.ReleaseIP("net", ""); err == nil || errors.Is(err, ErrIPAlreadyFree) || errors.Is(err, ErrSubnetNotFound) {
		t.Errorf("empty-ip release = %v, want a plain failing error", err)
	}
}
