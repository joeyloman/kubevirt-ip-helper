package vm

import (
	"testing"

	kubevirtv1 "kubevirt.io/api/core/v1"

	"k8s.io/client-go/tools/cache"
)

// delayed deletions arrive as tombstones; the delete-event construction must
// survive them instead of panicking on the direct type assertion.
func TestUnwrapTombstone(t *testing.T) {
	vm := &kubevirtv1.VirtualMachine{}
	vm.Name = "vm-a"
	vm.Namespace = "default"

	if got := unwrapTombstone(vm); got != vm {
		t.Errorf("unwrapTombstone changed a plain object: %#v", got)
	}

	tombstone := cache.DeletedFinalStateUnknown{Key: "default/vm-a", Obj: vm}
	if got := unwrapTombstone(tombstone); got != vm {
		t.Errorf("unwrapTombstone did not resolve the tombstone: %#v", got)
	}

	if got, isVM := unwrapTombstone("garbage").(*kubevirtv1.VirtualMachine); isVM {
		t.Errorf("foreign object typed as *VirtualMachine: %#v", got)
	}
}
