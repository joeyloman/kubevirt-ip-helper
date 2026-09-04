package ippool

import (
	"testing"

	"k8s.io/client-go/tools/cache"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// delayed deletions arrive as tombstones; the delete-event construction must
// survive them instead of panicking on the direct type assertion.
func TestUnwrapTombstone(t *testing.T) {
	pool := &kihv1.IPPool{}
	pool.Name = "pool-a"
	pool.Spec.NetworkName = "net-a"

	if got := unwrapTombstone(pool); got != pool {
		t.Errorf("unwrapTombstone changed a plain object: %#v", got)
	}

	tombstone := cache.DeletedFinalStateUnknown{Key: "default/pool-a", Obj: pool}
	if got := unwrapTombstone(tombstone); got != pool {
		t.Errorf("unwrapTombstone did not resolve the tombstone: %#v", got)
	}

	if got, isPool := unwrapTombstone("garbage").(*kihv1.IPPool); isPool {
		t.Errorf("foreign object typed as *IPPool: %#v", got)
	}
}
