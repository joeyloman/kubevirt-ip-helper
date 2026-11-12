package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestAddToSchemeRegistersKnownTypes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, tc := range []struct {
		name string
		obj  runtime.Object
		kind string
	}{
		{"VirtualMachineNetworkConfig", &VirtualMachineNetworkConfig{}, "VirtualMachineNetworkConfig"},
		{"VirtualMachineNetworkConfigList", &VirtualMachineNetworkConfigList{}, "VirtualMachineNetworkConfigList"},
		{"IPPool", &IPPool{}, "IPPool"},
		{"IPPoolList", &IPPoolList{}, "IPPoolList"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gvks, _, err := scheme.ObjectKinds(tc.obj)
			if err != nil {
				t.Fatalf("ObjectKinds: %v", err)
			}
			if len(gvks) != 1 {
				t.Fatalf("got %d registered kinds for %s, want 1", len(gvks), tc.kind)
			}
			if gvks[0] != SchemeGroupVersion.WithKind(tc.kind) {
				t.Errorf("gvk = %v, want %v", gvks[0], SchemeGroupVersion.WithKind(tc.kind))
			}
		})
	}

	// addKnownTypes also wires the metav1 types used by generated clients
	// (status, list options) into the same group version.
	if !scheme.Recognizes(SchemeGroupVersion.WithKind("Status")) {
		t.Error("metav1.Status not registered with scheme")
	}
	if !scheme.Recognizes(SchemeGroupVersion.WithKind("ListOptions")) {
		t.Error("metav1 ListOptions not registered with scheme")
	}
}

func TestAddToSchemeDoesNotRecognizeForeignTypes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	if gvk := (schema.GroupVersion{Group: "example.com", Version: "v1"}).WithKind("Something"); scheme.Recognizes(gvk) {
		t.Errorf("unexpectedly recognized foreign GVK %v", gvk)
	}
}

func TestResourceReturnsGroupResource(t *testing.T) {
	want := schema.GroupResource{Group: "kubevirtiphelper.k8s.binbash.org", Resource: "ippools"}
	if got := Resource("ippools"); got != want {
		t.Errorf("Resource = %v, want %v", got, want)
	}
}

func TestIPPoolDeepCopyIsolatesNestedSlicesAndMaps(t *testing.T) {
	orig := &IPPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pool-a",
			Labels:      map[string]string{"app": "kubevirt-ip-helper"},
			Annotations: map[string]string{"note": "orig"},
		},
		Spec: IPPoolSpec{
			IPv4Config: IPv4Config{
				DNS:          []string{"1.1.1.1", "8.8.8.8"},
				DomainSearch: []string{"example.com"},
				NTP:          []string{"10.0.0.53"},
				Pool:         Pool{Exclude: []string{"192.168.0.10"}},
			},
		},
		Status: IPPoolStatus{
			IPv4: IPv4Status{Allocated: map[string]string{"aa:bb:cc:dd:ee:01": "192.168.0.50"}},
		},
	}

	got := orig.DeepCopy()

	orig.Labels["app"] = "mutated"
	orig.Annotations["note"] = "mutated"
	orig.Spec.IPv4Config.DNS[0] = "9.9.9.9"
	orig.Spec.IPv4Config.DomainSearch[0] = "mutated"
	orig.Spec.IPv4Config.NTP[0] = "10.0.0.99"
	orig.Spec.IPv4Config.Pool.Exclude[0] = "192.168.0.99"
	orig.Status.IPv4.Allocated["aa:bb:cc:dd:ee:01"] = "10.1.1.1"

	if got.Labels["app"] != "kubevirt-ip-helper" {
		t.Errorf("deep copy label = %q, want original value", got.Labels["app"])
	}
	if got.Annotations["note"] != "orig" {
		t.Errorf("deep copy annotation = %q, want original value", got.Annotations["note"])
	}
	if got.Spec.IPv4Config.DNS[0] != "1.1.1.1" {
		t.Errorf("deep copy dns = %v, want unchanged original", got.Spec.IPv4Config.DNS)
	}
	if got.Spec.IPv4Config.DomainSearch[0] != "example.com" {
		t.Errorf("deep copy domain search = %v, want unchanged original", got.Spec.IPv4Config.DomainSearch)
	}
	if got.Spec.IPv4Config.NTP[0] != "10.0.0.53" {
		t.Errorf("deep copy ntp = %v, want unchanged original", got.Spec.IPv4Config.NTP)
	}
	if got.Spec.IPv4Config.Pool.Exclude[0] != "192.168.0.10" {
		t.Errorf("deep copy exclude = %v, want unchanged original", got.Spec.IPv4Config.Pool.Exclude)
	}
	if got.Status.IPv4.Allocated["aa:bb:cc:dd:ee:01"] != "192.168.0.50" {
		t.Errorf("deep copy allocated = %v, want unchanged original", got.Status.IPv4.Allocated)
	}
}

func TestVirtualMachineNetworkConfigDeepCopyObjectIsolatesSlices(t *testing.T) {
	orig := &VirtualMachineNetworkConfig{
		Spec: VirtualMachineNetworkConfigSpec{
			NetworkConfig: []NetworkConfig{
				{IPAddress: "10.0.0.5", MACAddress: "aa:bb:cc:dd:ee:01", NetworkName: "net-a"},
			},
		},
		Status: VirtualMachineNetworkConfigStatus{
			NetworkConfig: []NetworkConfigStatus{
				{MACAddress: "aa:bb:cc:dd:ee:01", NetworkName: "net-a", Status: "Ready"},
			},
		},
	}

	got, ok := orig.DeepCopyObject().(*VirtualMachineNetworkConfig)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *VirtualMachineNetworkConfig", got)
	}

	orig.Spec.NetworkConfig[0].IPAddress = "10.0.0.99"
	orig.Status.NetworkConfig[0].Status = "Mutated"

	if got.Spec.NetworkConfig[0].IPAddress != "10.0.0.5" {
		t.Errorf("deep copy spec network config = %+v, want unchanged original", got.Spec.NetworkConfig)
	}
	if got.Status.NetworkConfig[0].Status != "Ready" {
		t.Errorf("deep copy status network config = %+v, want unchanged original", got.Status.NetworkConfig)
	}
}
