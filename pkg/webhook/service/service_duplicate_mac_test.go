package service

// R10 fixtures and regressions of the duplicate (vmname, macaddress)
// admission guard: the check compares the recorded spellings, so a
// canonically equivalent duplicate (dash or uppercase spelling of the
// same address) passes it although the helper controller keys its lease
// map on the parsed macaddress and cannot distinguish the two objects.
// the guard itself stays deliberately list-before-admit and fail-open:
// two admissions which interleave in the list window both pass (fixture
// 3, the accepted TOCTOU boundary), and the converged duplicates are
// handled deterministically by the controller instead.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// admissionFakeAPI is a minimal stateful apiserver for the vmnetcfg
// admission path: it serves the namespace list of the duplicate check.
// the stored objects model what earlier admissions persisted - the fake
// never stores an admitted object itself, because the real apiserver
// only writes it after the webhook allowed the request. that property
// is what reproduces the list-before-admit window of two concurrent
// creates: neither object is visible to the list of either request.
type admissionFakeAPI struct {
	mu        sync.Mutex
	vmnetcfgs map[string]*kihv1.VirtualMachineNetworkConfig
}

func newAdmissionFakeAPI() *admissionFakeAPI {
	return &admissionFakeAPI{vmnetcfgs: map[string]*kihv1.VirtualMachineNetworkConfig{}}
}

func (f *admissionFakeAPI) store(obj *kihv1.VirtualMachineNetworkConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if obj.ObjectMeta.ResourceVersion == "" {
		obj.ObjectMeta.ResourceVersion = "1"
	}
	f.vmnetcfgs[obj.Namespace+"/"+obj.Name] = obj.DeepCopy()
}

func (f *admissionFakeAPI) clientset(t *testing.T) *kubernetes.Clientset {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet ||
			!strings.HasPrefix(r.URL.Path, vmNetCfgAPIPath+"/namespaces/") ||
			!strings.HasSuffix(r.URL.Path, "/virtualmachinenetworkconfigs") {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		ns := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, vmNetCfgAPIPath+"/namespaces/"), "/"), "/")[0]

		list := &kihv1.VirtualMachineNetworkConfigList{}
		f.mu.Lock()
		for _, obj := range f.vmnetcfgs {
			if obj.Namespace == ns {
				list.Items = append(list.Items, *obj.DeepCopy())
			}
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(srv.Close)

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("building clientset: %v", err)
	}

	return clientset
}

// admitVMNetCfg drives one vmnetcfg create through the admission
// handler and returns its verdict.
func admitVMNetCfg(t *testing.T, h *Handler, obj *kihv1.VirtualMachineNetworkConfig, uid string) *admissionv1.AdmissionResponse {
	t.Helper()

	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshaling the admitted object: %v", err)
	}

	body, err := json.Marshal(&admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:    types.UID(uid),
			Object: runtime.RawExtension{Raw: raw},
		},
	})
	if err != nil {
		t.Fatalf("marshaling the admission review: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/validate-vmnetcfg", bytes.NewBuffer(body))
	rec := httptest.NewRecorder()

	h.validateVmNetCfgAdmission(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(rec.Body).Decode(ar); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if ar.Response == nil {
		t.Fatal("response carries no AdmissionResponse")
	}

	return ar.Response
}

// TestVmNetCfgAdmissionDeniesCanonicalEquivalentDuplicateMAC is the R10
// admission fixture: binding-a already records (vm-x, 02:00:00:00:00:0a)
// and binding-b claims the same pair with the dash/uppercase spelling of
// the same address. the helper controller keys its lease on the parsed
// macaddress, so the two objects are indistinguishable to it on any
// network: the duplicate must be denied exactly like the raw-equal one,
// while a different vmname claiming the macaddress stays admissible (the
// deliberate same-vmname-only scope) and the identical object never
// conflicts with itself (the object-identity exemption).
func TestVmNetCfgAdmissionDeniesCanonicalEquivalentDuplicateMAC(t *testing.T) {
	api := newAdmissionFakeAPI()
	api.store(vmnetcfg("default", "binding-a", "vm-x",
		kihv1.NetworkConfig{MACAddress: "02:00:00:00:00:0a", NetworkName: "net-a"}))

	h := &Handler{clientset: api.clientset(t)}

	tests := []struct {
		name   string
		obj    *kihv1.VirtualMachineNetworkConfig
		denied bool
		wantIn string
	}{
		{
			name: "the same vm and mac in a dash spelling",
			obj: vmnetcfg("default", "binding-b", "vm-x",
				kihv1.NetworkConfig{MACAddress: "02-00-00-00-00-0A", NetworkName: "net-a"}),
			denied: true,
			wantIn: "VirtualMachineNetworkConfig default/binding-a",
		},
		{
			name: "the same vm and mac in an uppercase spelling",
			obj: vmnetcfg("default", "binding-c", "vm-x",
				kihv1.NetworkConfig{MACAddress: "02:00:00:00:00:0A", NetworkName: "net-b"}),
			denied: true,
			wantIn: "VirtualMachineNetworkConfig default/binding-a",
		},
		{
			name: "the same vm and mac in the identical spelling",
			obj: vmnetcfg("default", "binding-d", "vm-x",
				kihv1.NetworkConfig{MACAddress: "02:00:00:00:00:0a", NetworkName: "net-a"}),
			denied: true,
			wantIn: "VirtualMachineNetworkConfig default/binding-a",
		},
		{
			name: "a different vmname claiming the same mac stays admissible",
			obj: vmnetcfg("default", "foreign-cfg", "vm-y",
				kihv1.NetworkConfig{MACAddress: "02:00:00:00:00:0a", NetworkName: "net-a"}),
			denied: false,
		},
		{
			name: "the object never conflicts with itself",
			obj: vmnetcfg("default", "binding-a", "vm-x",
				kihv1.NetworkConfig{MACAddress: "02:00:00:00:00:0a", NetworkName: "net-a"}),
			denied: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := admitVMNetCfg(t, h, tt.obj, "uid-"+tt.name)

			if resp.Allowed == tt.denied {
				t.Fatalf("allowed = %v, want denied = %v (result: %v)", resp.Allowed, tt.denied, resp.Result)
			}

			if tt.denied {
				if resp.Result == nil || !strings.Contains(resp.Result.Message, tt.wantIn) {
					t.Fatalf("denial message = %v, want it to name the recorded object %q", resp.Result, tt.wantIn)
				}
			}
		})
	}
}

// TestVmNetCfgAdmissionDuplicateWindowAdmitsBoth is the accepted R10
// TOCTOU boundary (fixture 3): the duplicate check is list-before-admit
// and the apiserver stores an object only after its admission, so two
// creates of the same (vmname, macaddress) pair which interleave in the
// list window both pass - the admission LIST is deliberately not a lock
// and the webhook fails open. the helper controller owns the converged
// duplicates deterministically once both are stored.
func TestVmNetCfgAdmissionDuplicateWindowAdmitsBoth(t *testing.T) {
	api := newAdmissionFakeAPI()
	h := &Handler{clientset: api.clientset(t)}

	pair := kihv1.NetworkConfig{MACAddress: "02:00:00:00:00:0a", NetworkName: "net-a"}

	for _, name := range []string{"binding-a", "binding-b"} {
		resp := admitVMNetCfg(t, h, vmnetcfg("default", name, "vm-x", pair), "uid-"+name)

		if !resp.Allowed {
			t.Fatalf("the concurrently created %s was denied with %v: the list-before-admit window is the accepted admission boundary, the controller handles the converged duplicates", name, resp.Result)
		}
	}
}
