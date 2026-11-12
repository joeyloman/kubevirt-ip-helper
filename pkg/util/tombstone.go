package util

import "k8s.io/client-go/tools/cache"

// UnwrapTombstone resolves the informer object for delete handlers: delayed
// deletions arrive as cache.DeletedFinalStateUnknown instead of the object.
func UnwrapTombstone(obj interface{}) interface{} {
	if tombstone, isTombstone := obj.(cache.DeletedFinalStateUnknown); isTombstone {
		return tombstone.Obj
	}

	return obj
}
