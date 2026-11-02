package util

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// UnwrapTombstone must survive plain objects, resolve tombstones back to
// their inner object, and never invent a typed object out of garbage.
func TestUnwrapTombstone(t *testing.T) {
	obj := &metav1.ObjectMeta{Name: "obj-a"}

	if got := UnwrapTombstone(obj); got != obj {
		t.Errorf("UnwrapTombstone changed a plain object: %#v", got)
	}

	tombstone := cache.DeletedFinalStateUnknown{Key: "default/obj-a", Obj: obj}
	if got := UnwrapTombstone(tombstone); got != obj {
		t.Errorf("UnwrapTombstone did not resolve the tombstone: %#v", got)
	}

	if got, isObj := UnwrapTombstone("garbage").(*metav1.ObjectMeta); isObj {
		t.Errorf("foreign object typed as *metav1.ObjectMeta: %#v", got)
	}
}
