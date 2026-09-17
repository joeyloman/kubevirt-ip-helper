package vm

import (
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

// the delete handler must derive the vmnetcfg cleanup for every delayed
// deletion the informer reports: a tombstone whose payload is no longer a
// VirtualMachine (a stale final state after a relist) would otherwise be
// dropped silently, stranding the vmnetcfg object and its allocations
// forever while the vm controller keeps reporting a healthy queue.
func TestEnqueueVirtualMachineDeleteDerivesTombstoneCleanup(t *testing.T) {
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	defer queue.ShutDown()

	handler := &EventHandler{scope: vmTestScope("default", "net-a"), reconcileMu: &sync.Mutex{}}

	// a tombstone with a live payload names the object from the object
	handler.enqueueVirtualMachineDelete(queue, cache.DeletedFinalStateUnknown{
		Key: "tenant-a/vm-old",
		Obj: &kubevirtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "vm-old"},
		},
	})

	if queue.Len() != 1 {
		t.Fatalf("queue length = %d, want 1 after a live tombstone payload", queue.Len())
	}

	item, _ := queue.Get()
	event := item.(Event)
	queue.Done(item)

	if event.action != DELETE || event.key != "tenant-a/vm-old" ||
		event.vmNamespace != "tenant-a" || event.vmName != "vm-old" {
		t.Errorf("event = %+v, want the payload-named delete", event)
	}

	// a tombstone whose payload is no longer a VirtualMachine still
	// produces the cleanup, named from the tombstone key
	handler.enqueueVirtualMachineDelete(queue, cache.DeletedFinalStateUnknown{
		Key: "tenant-b/vm-lost",
		Obj: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-b", Name: "vm-lost"},
		},
	})

	if queue.Len() != 1 {
		t.Fatal("a tombstone with a foreign payload must still enqueue the cleanup")
	}

	item, _ = queue.Get()
	event = item.(Event)
	queue.Done(item)

	if event.action != DELETE || event.key != "tenant-b/vm-lost" ||
		event.vmNamespace != "tenant-b" || event.vmName != "vm-lost" {
		t.Errorf("event = %+v, want the key-derived delete", event)
	}

	// a tombstone with a nil payload derives the cleanup from the key as well
	handler.enqueueVirtualMachineDelete(queue, cache.DeletedFinalStateUnknown{
		Key: "tenant-c/vm-gone",
	})

	if queue.Len() != 1 {
		t.Fatal("a tombstone with a nil payload must still enqueue the cleanup")
	}

	item, _ = queue.Get()
	event = item.(Event)
	queue.Done(item)

	if event.action != DELETE || event.key != "tenant-c/vm-gone" ||
		event.vmNamespace != "tenant-c" || event.vmName != "vm-gone" {
		t.Errorf("event = %+v, want the key-derived delete", event)
	}

	// a foreign object which is not a tombstone stays dropped
	handler.enqueueVirtualMachineDelete(queue, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-d", Name: "cm-foreign"},
	})

	if queue.Len() != 0 {
		t.Errorf("queue length = %d, want 0: foreign objects must stay dropped", queue.Len())
	}
}
