package ipam_test

import (
	"errors"
	"testing"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// TestReleaseIPUnparseableAddressIsUnusableIdentity pins the sentinel
// contract of the plain release from the consumer side: a release of an
// unparseable address classifies as an unusable identity, so the cleanup
// callers converge it instead of requeueing it forever.
func TestReleaseIPUnparseableAddressIsUnusableIdentity(t *testing.T) {
	allocator := ipam.New()
	if err := allocator.NewSubnet("net", "192.168.53.0/29", "192.168.53.1", "192.168.53.6"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	err := allocator.ReleaseIP("net", "not-an-ip")
	if !errors.Is(err, ipam.ErrIPInvalid) {
		t.Errorf("release of an unparseable address = %v, want ErrIPInvalid", err)
	}
	if !util.IsUnusableIdentity(err) {
		t.Errorf("IsUnusableIdentity of an unparseable release = false, want the release to classify as converged: %v", err)
	}
}
