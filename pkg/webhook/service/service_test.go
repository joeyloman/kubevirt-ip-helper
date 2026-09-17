package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strings"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func vmnetcfg(namespace string, name string, vmName string, nics ...kihv1.NetworkConfig) *kihv1.VirtualMachineNetworkConfig {
	return &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: kihv1.VirtualMachineNetworkConfigSpec{
			VMName:        vmName,
			NetworkConfig: nics,
		},
	}
}

// Exercise admission against its real REST list path, without a live API.
func admissionTestHandler(t *testing.T, pools *kihv1.IPPoolList, configs *kihv1.VirtualMachineNetworkConfigList, lookupFailure bool) *Handler {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RawQuery != "" {
			t.Errorf("admission discovery must remain an unfiltered GET: %s %s", r.Method, r.URL)
		}
		if lookupFailure {
			http.Error(w, "API unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case vmNetCfgAPIPath + "/ippools":
			_ = json.NewEncoder(w).Encode(pools)
		case vmNetCfgAPIPath + "/virtualmachinenetworkconfigs":
			_ = json.NewEncoder(w).Encode(configs)
		case vmNetCfgAPIPath + "/namespaces/tenant/virtualmachinenetworkconfigs":
			list := &kihv1.VirtualMachineNetworkConfigList{}
			for _, obj := range configs.Items {
				if obj.Namespace == "tenant" {
					list.Items = append(list.Items, obj)
				}
			}
			_ = json.NewEncoder(w).Encode(list)
		default:
			t.Errorf("unexpected admission lookup %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{clientset: clientset}
}

func vmNetCfgReview(t *testing.T, operation admissionv1.Operation, obj, old *kihv1.VirtualMachineNetworkConfig) *admissionv1.AdmissionReview {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	request := &admissionv1.AdmissionRequest{
		UID:       "admission-test",
		Operation: operation,
		Namespace: obj.Namespace,
		Name:      obj.Name,
		Object:    runtime.RawExtension{Raw: raw},
	}
	if old != nil {
		request.OldObject.Raw, err = json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
	}
	return &admissionv1.AdmissionReview{Request: request}
}

func TestValidateVmNetCfgDelta(t *testing.T) {
	own := kihv1.NetworkConfig{NetworkName: "infra/net-a", MACAddress: "02:00:00:00:00:01", IPAddress: "192.168.11.110"}
	badMAC := kihv1.NetworkConfig{NetworkName: "infra/net-b", MACAddress: "01:00:5e:00:00:01"}
	badIP := kihv1.NetworkConfig{NetworkName: "infra/net-b", MACAddress: "02:00:00:00:00:02", IPAddress: "192.168.11.250"}
	duplicate := kihv1.NetworkConfig{NetworkName: "infra/net-b", MACAddress: "02:00:00:00:00:03"}
	cfg := func(rows ...kihv1.NetworkConfig) *kihv1.VirtualMachineNetworkConfig {
		return vmnetcfg("tenant", "shared", "vm", rows...)
	}
	old := cfg(own, badMAC, badIP, duplicate)
	updatedOwn := own
	updatedOwn.IPAddress = "192.168.11.120"
	invalidOwn := own
	invalidOwn.IPAddress = "192.168.11.250"
	changedForeign := badIP
	changedForeign.IPAddress = "192.168.11.251"
	changedNetwork := badIP
	changedNetwork.NetworkName = "infra/net-a"
	changedMAC := badIP
	changedMAC.MACAddress = "02:00:00:00:00:04"
	metadataOnly := old.DeepCopy()
	metadataOnly.Labels = map[string]string{"kept": "value"}
	metadataOnly.Finalizers = []string{"cleanup"}
	ownerMAC := cfg(badMAC)
	ownerMAC.Spec.VMName = "new-owner"
	ownerIP := cfg(badIP)
	ownerIP.Spec.VMName = "new-owner"
	ownerDuplicate := cfg(duplicate)
	ownerDuplicate.Spec.VMName = "new-owner"
	emptyOwner := cfg(badMAC)
	emptyOwner.Spec.VMName = ""
	sharedMAC := own
	sharedMAC.NetworkName = "infra/net-b"

	pools := &kihv1.IPPoolList{Items: []kihv1.IPPool{
		*testPool("pool-a", "infra/net-a", "192.168.11.100", "192.168.11.166"),
		*testPool("pool-b", "infra/net-b", "192.168.11.100", "192.168.11.166"),
	}}
	conflict := duplicate
	conflict.NetworkName = "infra/net-c"
	configs := &kihv1.VirtualMachineNetworkConfigList{Items: []kihv1.VirtualMachineNetworkConfig{
		*vmnetcfg("other-tenant", "distinct-tenant", "vm", own),
		*old,
		*vmnetcfg("tenant", "distinct", "vm", conflict),
		*vmnetcfg("tenant", "distinct-new-owner", "new-owner", conflict),
	}}
	h := admissionTestHandler(t, pools, configs, false)
	tests := []struct {
		name      string
		operation admissionv1.Operation
		old       *kihv1.VirtualMachineNetworkConfig
		obj       *kihv1.VirtualMachineNetworkConfig
		allowed   bool
	}{
		{"valid own allocation despite all unchanged foreign violations", admissionv1.Update, old, cfg(updatedOwn, badMAC, badIP, duplicate), true},
		{"own row removal despite unchanged foreign violations", admissionv1.Update, old, cfg(badMAC, badIP, duplicate), true},
		{"invalid row removal", admissionv1.Update, old, cfg(own, badIP, duplicate), true},
		{"metadata acknowledgement despite unchanged foreign violations", admissionv1.Update, old, metadataOnly, true},
		{"reorder uses complete rows not positions", admissionv1.Update, old, cfg(duplicate, badIP, own, badMAC), true},
		{"removing one existing invalid duplicate", admissionv1.Update, cfg(badMAC, badMAC), cfg(badMAC), true},
		{"extra duplicate invalid MAC is new", admissionv1.Update, old, cfg(own, badMAC, badIP, duplicate, badMAC), false},
		{"extra duplicate invalid IP is new", admissionv1.Update, old, cfg(own, badMAC, badIP, duplicate, badIP), false},
		{"extra duplicate cross-object claim is new", admissionv1.Update, old, cfg(own, badMAC, badIP, duplicate, duplicate), false},
		{"modified own invalid IP is checked", admissionv1.Update, old, cfg(invalidOwn, badMAC, badIP, duplicate), false},
		{"modified foreign IP is checked", admissionv1.Update, old, cfg(own, badMAC, changedForeign, duplicate), false},
		{"network-only edit is a changed row", admissionv1.Update, cfg(badIP), cfg(changedNetwork), false},
		{"MAC-only edit is a changed row", admissionv1.Update, cfg(badIP), cfg(changedMAC), false},
		{"owner change revalidates MAC", admissionv1.Update, cfg(badMAC), ownerMAC, false},
		{"owner change revalidates IP", admissionv1.Update, cfg(badIP), ownerIP, false},
		{"owner change revalidates cross-object identity", admissionv1.Update, cfg(duplicate), ownerDuplicate, false},
		{"clearing owner is exempt as before", admissionv1.Update, cfg(badMAC), emptyOwner, true},
		{"CREATE validates rows even with OldObject", admissionv1.Create, old, old, false},
		{"CREATE without owner is exempt as before", admissionv1.Create, nil, emptyOwner, true},
		{"missing OldObject cannot exempt invalid rows", admissionv1.Update, nil, cfg(badMAC), false},
		{"same MAC across networks inside shared object is valid", admissionv1.Create, nil, cfg(own, sharedMAC), true},
		{"valid duplicate insertion does not invent an intra-object guard", admissionv1.Update, cfg(own), cfg(own, own), true},
		{"last row removal needs no revalidation", admissionv1.Update, cfg(badMAC), cfg(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := h.validateVmNetCfg(vmNetCfgReview(t, tt.operation, tt.obj, tt.old))
			if response.Allowed != tt.allowed {
				t.Fatalf("allowed=%v, want %v: %+v", response.Allowed, tt.allowed, response.Result)
			}
		})
	}
}

func TestValidateVmNetCfgQualifiedNetworks(t *testing.T) {
	pools := &kihv1.IPPoolList{Items: []kihv1.IPPool{
		*testPool("pool", "tenant/net-a", "192.168.11.100", "192.168.11.166"),
		*testPool("shared-pool", "infra/net-b", "192.168.11.100", "192.168.11.166"),
	}}
	h := admissionTestHandler(t, pools, &kihv1.VirtualMachineNetworkConfigList{}, false)
	tests := []struct {
		name      string
		namespace string
		network   string
		ip        string
		allowed   bool
	}{
		{"qualified range enforced", "tenant", "tenant/net-a", "192.168.11.250", false},
		{"bare equivalent range enforced", "tenant", "net-a", "192.168.11.250", false},
		{"shared NAD range enforced from tenant", "tenant", "infra/net-b", "192.168.11.250", false},
		{"bare network is not implicitly infrastructure scoped", "tenant", "net-b", "192.168.11.250", true},
		{"valid bare range", "tenant", "net-a", "192.168.11.110", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := vmnetcfg(tt.namespace, "shared", "vm", kihv1.NetworkConfig{
				NetworkName: tt.network, MACAddress: "02:00:00:00:00:01", IPAddress: tt.ip,
			})
			response := h.validateVmNetCfg(vmNetCfgReview(t, admissionv1.Create, obj, nil))
			if response.Allowed != tt.allowed {
				t.Fatalf("allowed=%v, want %v: %+v", response.Allowed, tt.allowed, response.Result)
			}
		})
	}
}

func TestValidateIPPoolGlobalDeletionGate(t *testing.T) {
	tests := []struct {
		name          string
		network       string
		statusOnly    bool
		lookupFailure bool
		allowed       bool
	}{
		{"same network tenant reference blocks", "infra/net-a", false, false, false},
		{"other network same owner MAC does not block", "infra/net-b", false, false, true},
		{"ambiguous tenant reference blocks", "", false, false, false},
		{"lookup failure cannot prove orphan", "infra/net-b", false, true, false},
		{"status rows and finalizers are not spec references", "infra/net-a", true, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool("pool", "infra/net-a", "192.168.11.100", "192.168.11.166")
			pool.Status.IPv4.Allocated = map[string]string{"192.168.11.110": "tenant/vm [02:00:00:00:00:01]"}
			obj := vmnetcfg("tenant", "shared", "vm", kihv1.NetworkConfig{
				NetworkName: tt.network, MACAddress: "02:00:00:00:00:01",
			})
			now := metav1.Now()
			obj.DeletionTimestamp = &now
			obj.Finalizers = []string{"cleanup"}
			if tt.statusOnly {
				obj.Spec.NetworkConfig = nil
				obj.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{
					NetworkName: tt.network, MACAddress: "02:00:00:00:00:01",
				}}
			}
			h := admissionTestHandler(t, &kihv1.IPPoolList{}, &kihv1.VirtualMachineNetworkConfigList{
				Items: []kihv1.VirtualMachineNetworkConfig{*obj},
			}, tt.lookupFailure)
			response := h.validateIPPool(&admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{UID: "delete-test", Operation: admissionv1.Delete},
			}, pool)
			if response.Allowed != tt.allowed {
				t.Fatalf("allowed=%v, want %v: %+v", response.Allowed, tt.allowed, response.Result)
			}
		})
	}
}

func TestParseAllocationRef(t *testing.T) {
	tests := []struct {
		name      string
		ref       string
		namespace string
		vmName    string
		hwAddr    string
		ok        bool
	}{
		{"canonical reference", "default/cirros-vm1 [02:7b:d9:84:8f:e5]", "default", "cirros-vm1", "02:7b:d9:84:8f:e5", true},
		{"dash spelling is canonicalized", "default/vm-1 [02-7b-d9-84-8f-e5]", "default", "vm-1", "02:7b:d9:84:8f:e5", true},
		{"uppercase spelling is canonicalized", "DEFAULT/vm-1 [02:7B:D9:84:8F:E5]", "DEFAULT", "vm-1", "02:7b:d9:84:8f:e5", true},
		{"reference without a macaddress", "default/vm-1", "", "", "", false},
		{"reference with an unparseable macaddress", "default/vm-1 [not-a-mac]", "", "", "", false},
		{"owner without a namespace", "vmname [02:7b:d9:84:8f:e5]", "", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			namespace, vmName, hwAddr, ok := parseAllocationRef(tt.ref)

			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}

			if namespace != tt.namespace || vmName != tt.vmName || hwAddr != tt.hwAddr {
				t.Fatalf("parsed (%q, %q, %q), want (%q, %q, %q)", namespace, vmName, hwAddr, tt.namespace, tt.vmName, tt.hwAddr)
			}
		})
	}
}

func TestIPPoolRecordsNetworkOwnership(t *testing.T) {
	const mac = "02:7b:d9:84:8f:e5"
	tests := []struct {
		name        string
		poolNetwork string
		namespace   string
		vmName      string
		network     string
		mac         string
		deleting    bool
		blocked     bool
	}{
		{"same network bare row", "tenant/net-a", "tenant", "vm", "net-a", mac, false, true},
		{"same network qualified row", "infra/net-a", "tenant", "vm", "infra/net-a", mac, false, true},
		{"canonical mac spelling", "tenant/net-a", "tenant", "vm", "net-a", "02-7B-D9-84-8F-E5", false, true},
		{"different network same owner mac", "tenant/net-a", "tenant", "vm", "net-b", mac, false, false},
		{"bare row resolves in tenant namespace", "infra/net-a", "tenant", "vm", "net-a", mac, false, false},
		{"different namespace", "infra/net-a", "other", "vm", "infra/net-a", mac, false, false},
		{"different vm", "tenant/net-a", "tenant", "other-vm", "net-a", mac, false, false},
		{"different mac", "tenant/net-a", "tenant", "vm", "net-a", "02:00:00:00:00:01", false, false},
		{"deleting object still blocks", "tenant/net-a", "tenant", "vm", "net-a", mac, true, true},
		{"empty pool network", "", "tenant", "vm", "net-b", mac, false, true},
		{"bare cluster scoped pool network", "net-a", "tenant", "vm", "net-b", mac, false, true},
		{"malformed pool network", "tenant/net-a/extra", "tenant", "vm", "net-b", mac, false, true},
		{"empty row network", "tenant/net-a", "tenant", "vm", "", mac, false, true},
		{"malformed row network", "tenant/net-a", "tenant", "vm", "tenant/net-a/extra", mac, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := vmnetcfg(tt.namespace, "config", tt.vmName,
				kihv1.NetworkConfig{NetworkName: tt.network, MACAddress: tt.mac})
			if tt.deleting {
				now := metav1.Now()
				obj.DeletionTimestamp = &now
			}
			index := buildAllocationOwnerIndex(&kihv1.VirtualMachineNetworkConfigList{
				Items: []kihv1.VirtualMachineNetworkConfig{*obj},
			})
			blocking, orphaned := evaluateIPPoolRecords(
				map[string]string{"192.168.10.63": "tenant/vm [" + mac + "]"},
				tt.poolNetwork, index, true)
			if tt.blocked {
				if len(blocking) != 1 || len(orphaned) != 0 {
					t.Fatalf("live or ambiguous record must block: blocking=%v orphaned=%v", blocking, orphaned)
				}
			} else if len(blocking) != 0 || len(orphaned) != 1 {
				t.Fatalf("another binding must not keep this allocation live: blocking=%v orphaned=%v", blocking, orphaned)
			}
		})
	}
}

// TestEvaluateIPPoolRecords covers the orphan-aware deletion gate: only a
// record whose owner tuple is backed by a live object blocks, and every
// unprovably orphaned record blocks.
func TestEvaluateIPPoolRecords(t *testing.T) {
	list := &kihv1.VirtualMachineNetworkConfigList{
		Items: []kihv1.VirtualMachineNetworkConfig{
			*vmnetcfg("default", "cirros-vm1", "cirros-vm1",
				kihv1.NetworkConfig{MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "net-a"}),
		},
	}
	index := buildAllocationOwnerIndex(list)

	allocated := map[string]string{
		"192.168.10.63":  "default/cirros-vm1 [02:7b:d9:84:8f:e5]",
		"192.168.10.99":  "default/gone-vm [02:00:00:00:00:33]",
		"192.168.10.100": "EXCLUDED",
		"192.168.10.101": "unparseable",
	}

	blocking, orphaned := evaluateIPPoolRecords(allocated, "default/net-a", index, true)

	if len(blocking) != 2 {
		t.Fatalf("blocking = %v, want exactly the live and the unparseable records", blocking)
	}

	if !strings.Contains(blocking[0], "192.168.10.101") {
		t.Fatalf("the unparseable record must block and be reported first (sorted by ip): %v", blocking)
	}

	if !strings.Contains(blocking[1], "192.168.10.63") || !strings.Contains(blocking[1], "default/cirros-vm1") {
		t.Fatalf("the live record must block and name its recording object: %v", blocking)
	}

	if len(orphaned) != 1 || !strings.Contains(orphaned[0], "192.168.10.99") || !strings.Contains(orphaned[0], "gone-vm") {
		t.Fatalf("the orphaned record must be reported and must not block: %v", orphaned)
	}

	// an unavailable index keeps every non-EXCLUDED record blocking
	blocking, orphaned = evaluateIPPoolRecords(allocated, "default/net-a", index, false)

	if len(blocking) != 3 || len(orphaned) != 0 {
		t.Fatalf("with an unavailable index every record must block: blocking=%v orphaned=%v", blocking, orphaned)
	}

	for _, entry := range blocking {
		if strings.Contains(entry, "EXCLUDED") {
			t.Fatalf("an EXCLUDED record must never block: %v", blocking)
		}
	}
}

// TestFindRecordedTuple covers the duplicate (vmname, macaddress) guard:
// the same pair claimed by another object is denied on any network, while
// a different vmname, the object itself, and empty macaddresses pass.
func TestFindRecordedTuple(t *testing.T) {
	list := &kihv1.VirtualMachineNetworkConfigList{
		Items: []kihv1.VirtualMachineNetworkConfig{
			*vmnetcfg("default", "cirros-vm1", "cirros-vm1",
				kihv1.NetworkConfig{MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "kubevirt-public/management"}),
		},
	}

	tests := []struct {
		name    string
		obj     *kihv1.VirtualMachineNetworkConfig
		denied  bool
		wantSub string
	}{
		{
			"the same vm and mac in another object",
			vmnetcfg("default", "dup-cfg", "cirros-vm1",
				kihv1.NetworkConfig{IPAddress: "192.168.10.150", MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "kubevirt-public/management"}),
			true,
			"default/cirros-vm1",
		},
		{
			"the same vm and mac on a different network",
			vmnetcfg("default", "dup-cfg", "cirros-vm1",
				kihv1.NetworkConfig{IPAddress: "192.168.11.140", MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "kubevirt-public/storage"}),
			true,
			"kubevirt-public/management",
		},
		{
			"a different vmname claiming the same mac stays admissible",
			vmnetcfg("default", "foreign-cfg", "other-vm",
				kihv1.NetworkConfig{MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "kubevirt-public/management"}),
			false,
			"",
		},
		{
			"the object never conflicts with itself",
			vmnetcfg("default", "cirros-vm1", "cirros-vm1",
				kihv1.NetworkConfig{MACAddress: "02:7b:d9:84:8f:e5", NetworkName: "kubevirt-public/management"}),
			false,
			"",
		},
		{
			"an empty macaddress is skipped",
			vmnetcfg("default", "macless-cfg", "cirros-vm1",
				kihv1.NetworkConfig{IPAddress: "192.168.10.150"}),
			false,
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			denied := findRecordedTuple(tt.obj, list)

			if (denied != nil) != tt.denied {
				t.Fatalf("denied = %v, want %v", denied != nil, tt.denied)
			}

			if denied != nil && !strings.Contains(*denied, tt.wantSub) {
				t.Fatalf("message %q does not contain %q", *denied, tt.wantSub)
			}
		})
	}
}

func testPool(name string, networkName string, start string, end string) *kihv1.IPPool {
	return &kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: kihv1.IPPoolSpec{
			NetworkName: networkName,
			IPv4Config: kihv1.IPv4Config{
				Subnet:   "192.168.11.0/24",
				ServerIP: "192.168.11.9",
				Pool:     kihv1.Pool{Start: start, End: end},
			},
		},
	}
}

// TestCheckNICIPAddress covers the ipaddress range guard of the vmnetcfg
// admission check, including the two deliberate allowances: a network
// without a pool (the vm-before-pool ordering) and a pool whose range does
// not parse (the ippool controller's own rejection).
func TestCheckNICIPAddress(t *testing.T) {
	pool := testPool("storage", "kubevirt-public/storage", "192.168.11.100", "192.168.11.166")
	brokenRange := testPool("broken-pool", "kubevirt-public/storage", "192.168.11.abc", "192.168.11.166")

	tests := []struct {
		name   string
		nc     kihv1.NetworkConfig
		pool   *kihv1.IPPool
		denied bool
	}{
		{"an ip inside the range", kihv1.NetworkConfig{IPAddress: "192.168.11.120", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/storage"}, pool, false},
		{"the range start itself", kihv1.NetworkConfig{IPAddress: "192.168.11.100", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/storage"}, pool, false},
		{"the range end itself", kihv1.NetworkConfig{IPAddress: "192.168.11.166", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/storage"}, pool, false},
		{"an ip above the range", kihv1.NetworkConfig{IPAddress: "192.168.11.200", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/storage"}, pool, true},
		{"an ip below the range", kihv1.NetworkConfig{IPAddress: "192.168.11.99", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/storage"}, pool, true},
		{"an ip outside the subnet", kihv1.NetworkConfig{IPAddress: "10.0.0.5", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/storage"}, pool, true},
		{"a network without a pool", kihv1.NetworkConfig{IPAddress: "10.99.0.10", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/nonexistent"}, nil, false},
		{"a pool whose range does not parse", kihv1.NetworkConfig{IPAddress: "192.168.11.120", MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/storage"}, brokenRange, false},
		{"an empty ipaddress is skipped", kihv1.NetworkConfig{MACAddress: "02:00:00:00:00:01", NetworkName: "kubevirt-public/storage"}, pool, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			denied := checkNICIPAddress(tt.nc, tt.pool)

			if (denied != nil) != tt.denied {
				t.Fatalf("denied = %v, want %v", denied, tt.denied)
			}
		})
	}
}

// TestEvaluateIPPoolSpec covers the ippool spec guard. every rejected case
// mirrors a bound of the controller's own registration validation, and the
// off-subnet serverip is deliberately allowed because the controller
// registers it.
func TestEvaluateIPPoolSpec(t *testing.T) {
	tests := []struct {
		name         string
		cfg          kihv1.IPv4Config
		wantProblems []string
	}{
		{
			"a valid configuration",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20", Exclude: []string{"192.168.16.15"}}},
			nil,
		},
		{
			"an off-subnet serverip is deliberately allowed",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "10.9.9.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20"}},
			nil,
		},
		{
			"a subnet length the crd schema accepts but the controller cannot register",
			kihv1.IPv4Config{Subnet: "192.168.16.0/33", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20"}},
			[]string{`the subnet "192.168.16.0/33" does not parse as an ipv4 prefix`},
		},
		{
			"a pool start outside the subnet",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.15.255", End: "192.168.16.20"}},
			[]string{"the pool start 192.168.15.255 is not within the subnet 192.168.16.0/24"},
		},
		{
			"a pool end before its start",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.20", End: "192.168.16.10"}},
			[]string{"the pool end 192.168.16.10 lies before the pool start 192.168.16.20"},
		},
		{
			"a pool end equal to the broadcast address",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.255"}},
			[]string{"the pool end 192.168.16.255 equals the broadcast address 192.168.16.255 of the subnet 192.168.16.0/24"},
		},
		{
			"an exclude address outside the pool range",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20", Exclude: []string{"192.168.16.200"}}},
			[]string{"the exclude address 192.168.16.200 is not within the pool range 192.168.16.10..192.168.16.20"},
		},
		{
			"an exclude address equal to the broadcast address",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "192.168.16.9", Pool: kihv1.Pool{Start: "192.168.16.10", End: "192.168.16.20", Exclude: []string{"192.168.16.255"}}},
			[]string{"the exclude address 192.168.16.255 equals the broadcast address 192.168.16.255 of the subnet 192.168.16.0/24", "the exclude address 192.168.16.255 is not within the pool range 192.168.16.10..192.168.16.20"},
		},
		{
			"a range larger than the cap",
			kihv1.IPv4Config{Subnet: "10.20.0.0/15", ServerIP: "10.20.0.9", Pool: kihv1.Pool{Start: "10.20.0.1", End: "10.21.255.254"}},
			[]string{"the pool range 10.20.0.1 - 10.21.255.254 is larger than the maximum of 65536 addresses"},
		},
		{
			"every problem of a broken projection is reported together and sorted",
			kihv1.IPv4Config{Subnet: "192.168.16.0/24", ServerIP: "10.9.9.9x", Pool: kihv1.Pool{Start: "192.168.16.20", End: "192.168.16.10"}},
			[]string{"the pool end 192.168.16.10 lies before the pool start 192.168.16.20", `the serverip "10.9.9.9x" does not parse as an ipv4 address`},
		},
		{
			"an empty configuration stays the controller's business",
			kihv1.IPv4Config{},
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := evaluateIPPoolSpec(tt.cfg)

			if len(problems) != len(tt.wantProblems) {
				t.Fatalf("problems = %v, want %v", problems, tt.wantProblems)
			}

			if !sort.StringsAreSorted(problems) {
				t.Fatalf("the problems must be sorted for a deterministic message: %v", problems)
			}

			for i, want := range tt.wantProblems {
				if problems[i] != want {
					t.Fatalf("problems[%d] = %q, want %q", i, problems[i], want)
				}
			}
		})
	}
}

// TestCheckNICMACAddress covers the source-address guard: every multicast
// address and the broadcast address are denied, unicast addresses pass,
// and an empty macaddress is skipped.
func TestCheckNICMACAddress(t *testing.T) {
	tests := []struct {
		name   string
		nc     kihv1.NetworkConfig
		denied bool
	}{
		{"a locally administered unicast address", kihv1.NetworkConfig{MACAddress: "02:7b:d9:00:00:61", NetworkName: "net-a"}, false},
		{"a globally unique unicast address", kihv1.NetworkConfig{MACAddress: "52:54:00:12:34:56", NetworkName: "net-a"}, false},
		{"an ipv4 multicast address", kihv1.NetworkConfig{MACAddress: "01:00:5e:00:00:99", NetworkName: "net-a"}, true},
		{"an odd first octet is multicast", kihv1.NetworkConfig{MACAddress: "03:00:00:00:00:99", NetworkName: "net-a"}, true},
		{"the broadcast address", kihv1.NetworkConfig{MACAddress: "ff:ff:ff:ff:ff:ff", NetworkName: "net-a"}, true},
		{"an empty macaddress is skipped", kihv1.NetworkConfig{IPAddress: "192.168.11.120", NetworkName: "net-a"}, false},
		{"an unparseable macaddress", kihv1.NetworkConfig{MACAddress: "02:7b:d9:00:00", NetworkName: "net-a"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			denied := checkNICMACAddress(tt.nc)

			if (denied != nil) != tt.denied {
				t.Fatalf("denied = %v, want %v", denied, tt.denied)
			}
		})
	}
}

func TestIPv4BroadcastAndRangeLen(t *testing.T) {
	tests := []struct {
		subnet    string
		broadcast string
	}{
		{"192.168.16.0/24", "192.168.16.255"},
		{"10.20.0.0/16", "10.20.255.255"},
		{"10.20.0.0/30", "10.20.0.3"},
		{"192.168.16.0/31", "192.168.16.1"},
		{"192.168.16.128/25", "192.168.16.255"},
	}

	for _, tt := range tests {
		prefix, err := netip.ParsePrefix(tt.subnet)
		if err != nil {
			t.Fatalf("subnet %s does not parse: %s", tt.subnet, err)
		}

		if got := ipv4Broadcast(prefix).String(); got != tt.broadcast {
			t.Fatalf("broadcast of %s = %s, want %s", tt.subnet, got, tt.broadcast)
		}
	}

	start, _ := netip.ParseAddr("192.168.16.10")
	end, _ := netip.ParseAddr("192.168.16.20")
	if got := ipv4RangeLen(start, end); got != 11 {
		t.Fatalf("rangeLen of an 11-address range = %d, want 11", got)
	}

	start, _ = netip.ParseAddr("10.20.0.1")
	end, _ = netip.ParseAddr("10.21.255.254")
	if got := ipv4RangeLen(start, end); got != 131070 {
		t.Fatalf("rangeLen of the oversized range = %d, want 131070", got)
	}
}
