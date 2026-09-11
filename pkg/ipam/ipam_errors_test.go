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

	// an unparseable ip can never hold an allocation: the release carries
	// the ErrIPInvalid sentinel like ReleaseIPOwnedBy, so the
	// sentinel-classifying cleanup callers converge it instead of
	// retrying it forever
	if err := allocator.ReleaseIP("net", "not-an-ip"); !errors.Is(err, ErrIPInvalid) {
		t.Errorf("unparseable-ip release = %v, want ErrIPInvalid", err)
	}
}

// a non-canonical address spelling must never strand a reservation: the
// v4-in-v6 spelling is classified by the cidr gate (ErrIPNotInCidr,
// which the cleanup paths treat as converged) and leaves the reservation
// intact, while the release lookup itself runs on the canonical key -
// the same Unmap form every mutator stores - so the gate and the lookup
// can never diverge on a spelling which passes both.
func TestReleaseIPCanonicalizesTheAddressLookup(t *testing.T) {
	allocator := New()
	if err := allocator.NewSubnet("net", "192.168.53.0/29", "192.168.53.1", "192.168.53.6"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	ip, err := allocator.GetIP("net", "")
	if err != nil {
		t.Fatalf("GetIP: %v", err)
	}

	// the v4-in-v6 spelling is stopped by the cidr gate and the
	// reservation survives the attempt
	spelling := "::ffff:" + ip
	if err := allocator.ReleaseIP("net", spelling); !errors.Is(err, ErrIPNotInCidr) {
		t.Fatalf("ReleaseIP of the v4-in-v6 spelling = %v, want the ErrIPNotInCidr classification of the cidr gate", err)
	}
	if used := allocator.Used("net"); used != 1 {
		t.Errorf("used after the rejected spelling = %d, want the reservation intact", used)
	}

	// the canonical spelling releases it
	if err := allocator.ReleaseIP("net", ip); err != nil {
		t.Fatalf("ReleaseIP of the canonical spelling: %v", err)
	}
	if used := allocator.Used("net"); used != 0 {
		t.Errorf("used after the canonical release = %d, want 0", used)
	}
}
