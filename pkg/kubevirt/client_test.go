package kubevirt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

const virtualMachinePath = "/apis/kubevirt.io/v1/namespaces/default/virtualmachines/vm-test"

func TestVirtualMachineUsesV1(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != virtualMachinePath {
			t.Errorf("request path = %q, want %q", r.URL.Path, virtualMachinePath)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(kubevirtv1.VirtualMachine{
			TypeMeta:   metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine"},
			ObjectMeta: metav1.ObjectMeta{Name: "vm-test", Namespace: "default"},
		})
	}))
	defer server.Close()

	client, err := NewClient(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	vm, err := client.VirtualMachine(context.Background(), "default", "vm-test")
	if err != nil {
		t.Fatalf("VirtualMachine: %v", err)
	}
	if vm.Name != "vm-test" || vm.Namespace != "default" {
		t.Errorf("VirtualMachine = %s/%s, want default/vm-test", vm.Namespace, vm.Name)
	}
}

func TestVirtualMachineNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(metav1.Status{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
			Status:   metav1.StatusFailure,
			Message:  `virtualmachines.kubevirt.io "vm-test" not found`,
			Reason:   metav1.StatusReasonNotFound,
			Code:     http.StatusNotFound,
		})
	}))
	defer server.Close()

	client, err := NewClient(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.VirtualMachine(context.Background(), "default", "vm-test"); !apierrors.IsNotFound(err) {
		t.Fatalf("VirtualMachine error = %v, want a NotFound error", err)
	}
}

func TestVirtualMachineListWatchUsesV1(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kubevirt.io/v1/virtualmachines" {
			t.Errorf("request path = %q, want /apis/kubevirt.io/v1/virtualmachines", r.URL.Path)
		}
		if got := r.URL.Query().Get("resourceVersion"); got != "42" {
			t.Errorf("resourceVersion = %q, want 42", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "ADDED",
				"object": kubevirtv1.VirtualMachine{
					TypeMeta:   metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine"},
					ObjectMeta: metav1.ObjectMeta{Name: "vm-test", Namespace: "default"},
				},
			})
			return
		}

		_ = json.NewEncoder(w).Encode(kubevirtv1.VirtualMachineList{
			TypeMeta: metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineList"},
			Items: []kubevirtv1.VirtualMachine{{
				TypeMeta:   metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine"},
				ObjectMeta: metav1.ObjectMeta{Name: "vm-test", Namespace: "default"},
			}},
		})
	}))
	defer server.Close()

	client, err := NewClient(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	listWatch := client.VirtualMachineListWatch()

	obj, err := listWatch.List(metav1.ListOptions{ResourceVersion: "42"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	list, ok := obj.(*kubevirtv1.VirtualMachineList)
	if !ok {
		t.Fatalf("List returned %T, want *kubevirtv1.VirtualMachineList", obj)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "vm-test" {
		t.Errorf("List items = %v, want one vm-test", list.Items)
	}

	watcher, err := listWatch.Watch(metav1.ListOptions{ResourceVersion: "42"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer watcher.Stop()

	select {
	case event, ok := <-watcher.ResultChan():
		if !ok {
			t.Fatal("watch channel closed without an event")
		}
		vm, ok := event.Object.(*kubevirtv1.VirtualMachine)
		if !ok || vm.Name != "vm-test" {
			t.Errorf("watch event object = %#v, want vm-test", event.Object)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a watch event")
	}
}
