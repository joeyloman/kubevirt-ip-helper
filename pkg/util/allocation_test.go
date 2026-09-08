package util

import (
	"errors"
	"fmt"
	"testing"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
)

// the controllers treat provably absent allocations as converged cleanup
// state so a retried or non-existing allocation cannot stick finalizers.
func TestIsAlreadyReleased(t *testing.T) {
	if err := ipam.ErrSubnetNotFound; !IsAlreadyReleased(fmt.Errorf("net: %w", err)) {
		t.Errorf("subnet without allocation state = %v, want classified as released", err)
	}
	if err := ipam.ErrIPAlreadyFree; !IsAlreadyReleased(fmt.Errorf("ip: %w", err)) {
		t.Errorf("address without live allocation = %v, want classified as released", err)
	}
	if err := ipam.ErrIPNotInCidr; !IsAlreadyReleased(fmt.Errorf("given ip 10.0.0.9 is not cidr 10.0.0.0/29: %w", err)) {
		t.Errorf("address outside the subnet = %v, want classified as released", err)
	}

	if IsAlreadyReleased(errors.New("boom")) {
		t.Error("an unrelated error must not classify as released")
	}
	if IsAlreadyReleased(nil) {
		t.Error("nil must not classify as released")
	}
}
