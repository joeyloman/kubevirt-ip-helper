package util

import (
	"reflect"
	"strings"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func networkScopeForTest(t *testing.T, namespace, name string) NetworkScope {
	t.Helper()
	scope, err := NewNetworkScope(namespace, name)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestNetworkScopeRejectsInvalidIdentity(t *testing.T) {
	for _, invalid := range []string{"", "UPPER", "a.b", "a/b", "-a", "a-", " a", strings.Repeat("a", 64)} {
		if _, err := NewNetworkScope("infra", invalid); err == nil {
			t.Errorf("accepted invalid network name %q", invalid)
		}
		if _, err := NewNetworkScope(invalid, "management"); err == nil {
			t.Errorf("accepted invalid namespace %q", invalid)
		}
	}
	name := strings.Repeat("a", 63)
	scope := networkScopeForTest(t, name, name)
	if scope.LeaseName() != "kubevirt-ip-helper-lock-"+name {
		t.Fatal("maximum-length identity truncated or changed the Lease destination")
	}
}

func TestNetworkScopeOwnershipAndDiscovery(t *testing.T) {
	scope := networkScopeForTest(t, "infra", "management")
	for _, tc := range []struct {
		namespace, reference string
		want                 bool
	}{
		{"tenant", "infra/management", true},
		{"infra", "management", true},
		{"tenant", "management", false},
		{"infra", "tenant/management", false},
		{"infra", "infra/storage", false},
		{"infra", "infra/management/extra", false},
		{"infra", "", false},
	} {
		if got := scope.Owns(tc.namespace, tc.reference); got != tc.want {
			t.Errorf("Owns(%q, %q) = %v, want %v", tc.namespace, tc.reference, got, tc.want)
		}
	}
	selector, err := labels.Parse(scope.Selector())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                 string
		poolLabels           map[string]string
		reference            string
		discovered, accepted bool
	}{
		{"own", map[string]string{NetworkLabel: "management", NetworkNamespaceLabel: "infra"}, "infra/management", true, true},
		{"same name elsewhere", map[string]string{NetworkLabel: "management", NetworkNamespaceLabel: "tenant"}, "tenant/management", false, false},
		{"other name", map[string]string{NetworkLabel: "storage", NetworkNamespaceLabel: "infra"}, "infra/storage", false, false},
		{"missing namespace label", map[string]string{NetworkLabel: "management"}, "infra/management", false, false},
		{"unlabelled", nil, "infra/management", false, false},
		{"selected bare reference", map[string]string{NetworkLabel: "management", NetworkNamespaceLabel: "infra"}, "management", true, false},
		{"selected wrong reference", map[string]string{NetworkLabel: "management", NetworkNamespaceLabel: "infra"}, "infra/storage", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := &kihv1.IPPool{ObjectMeta: metav1.ObjectMeta{Labels: tc.poolLabels}, Spec: kihv1.IPPoolSpec{NetworkName: tc.reference}}
			if got := selector.Matches(labels.Set(pool.Labels)); got != tc.discovered {
				t.Fatalf("discovery = %v, want %v", got, tc.discovered)
			}
			if got := scope.MatchesPool(pool); got != tc.accepted {
				t.Fatalf("pool acceptance = %v, want %v", got, tc.accepted)
			}
		})
	}
	if scope.MatchesPool(nil) {
		t.Fatal("nil pool accepted")
	}
	var zero NetworkScope
	if zero.Owns("", "") || zero.Owns("infra", "infra/management") || zero.MatchesPool(&kihv1.IPPool{}) {
		t.Fatal("zero scope acquired ownership")
	}
	zeroSelector, err := labels.Parse(zero.Selector())
	if err != nil {
		t.Fatal(err)
	}
	if zeroSelector.Matches(labels.Set{}) || zeroSelector.Matches(labels.Set{NetworkLabel: "management", NetworkNamespaceLabel: "infra"}) {
		t.Fatal("zero scope selector widened discovery")
	}
}

func TestQualifyNetworkName(t *testing.T) {
	for _, tc := range []struct{ namespace, reference, want string }{
		{"tenant", "management", "tenant/management"},
		{"tenant", "infra/management", "infra/management"},
		{"", "infra/management", "infra/management"},
		{"", "management", ""},
		{"tenant", "", ""},
		{"tenant", "/management", ""},
		{"tenant", "infra/", ""},
		{"tenant", "infra/management/extra", ""},
		{"tenant", " infra/management", ""},
		{"tenant", "infra/MANAGEMENT", ""},
		{"tenant", "infra/manage.ment", ""},
		{"INVALID", "management", ""},
	} {
		if got := QualifyNetworkName(tc.namespace, tc.reference); got != tc.want {
			t.Errorf("QualifyNetworkName(%q, %q) = %q, want %q", tc.namespace, tc.reference, got, tc.want)
		}
	}
}

func TestNetworkScopeSpecMergePreservesForeignRows(t *testing.T) {
	scope := networkScopeForTest(t, "infra", "management")
	foreignBare := kihv1.NetworkConfig{NetworkName: "management", MACAddress: "malformed", IPAddress: "not-an-ip"}
	foreignQualified := kihv1.NetworkConfig{NetworkName: "infra/storage", MACAddress: "02:00:00:00:00:01", IPAddress: "10.2.0.5"}
	own := kihv1.NetworkConfig{NetworkName: "infra/management", MACAddress: foreignQualified.MACAddress, IPAddress: "10.1.0.5"}
	current := make([]kihv1.NetworkConfig, 4, 12)
	copy(current, []kihv1.NetworkConfig{foreignBare, own, foreignQualified, own})
	before := append([]kihv1.NetworkConfig(nil), current...)
	replacement := []kihv1.NetworkConfig{{NetworkName: own.NetworkName, MACAddress: own.MACAddress, IPAddress: "10.1.0.6"}}
	merged := scope.MergeSpec("tenant", current, replacement)
	want := []kihv1.NetworkConfig{foreignBare, replacement[0], foreignQualified}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("merge = %+v, want %+v", merged, want)
	}
	if !reflect.DeepEqual(current, before) {
		t.Fatal("merge mutated source rows")
	}
	if got := scope.FilterSpec("tenant", merged); !reflect.DeepEqual(got, replacement) {
		t.Fatalf("owned rows = %+v", got)
	}
	if got := scope.MergeSpec("tenant", merged, nil); !reflect.DeepEqual(got, []kihv1.NetworkConfig{foreignBare, foreignQualified}) {
		t.Fatalf("own cleanup changed foreign rows: %+v", got)
	}
	// An accidental foreign replacement cannot erase or duplicate that network.
	if got := scope.MergeSpec("tenant", current, []kihv1.NetworkConfig{foreignQualified}); !reflect.DeepEqual(got, []kihv1.NetworkConfig{foreignBare, foreignQualified}) {
		t.Fatalf("foreign replacement gained ownership: %+v", got)
	}
	if got := scope.MergeSpec("infra", []kihv1.NetworkConfig{{NetworkName: "management"}}, replacement); !reflect.DeepEqual(got, replacement) {
		t.Fatalf("bare local owned row was not replaced: %+v", got)
	}
}

func TestNetworkScopeStatusMergePreservesForeignOnlyErrors(t *testing.T) {
	scope := networkScopeForTest(t, "infra", "management")
	foreign := kihv1.NetworkConfigStatus{NetworkName: "management", MACAddress: "bad", Status: "error", Message: "preserve this exact foreign diagnostic"}
	own := kihv1.NetworkConfigStatus{NetworkName: scope.NetworkName(), MACAddress: "02:00:00:00:00:01", Status: "pending"}
	other := kihv1.NetworkConfigStatus{NetworkName: "infra/storage", MACAddress: own.MACAddress, Status: "bound"}
	current := []kihv1.NetworkConfigStatus{foreign, own, other}
	before := append([]kihv1.NetworkConfigStatus(nil), current...)
	replacement := []kihv1.NetworkConfigStatus{{NetworkName: own.NetworkName, MACAddress: own.MACAddress, Status: "bound"}}
	merged := scope.MergeStatus("tenant", current, replacement)
	if !reflect.DeepEqual(merged, []kihv1.NetworkConfigStatus{foreign, replacement[0], other}) {
		t.Fatalf("status merge changed foreign rows: %+v", merged)
	}
	if !reflect.DeepEqual(current, before) {
		t.Fatal("status merge mutated source rows")
	}
	if got := scope.FilterStatus("tenant", current); !reflect.DeepEqual(got, []kihv1.NetworkConfigStatus{own}) {
		t.Fatalf("owned statuses = %+v", got)
	}
	if got := scope.MergeStatus("tenant", merged, nil); !reflect.DeepEqual(got, []kihv1.NetworkConfigStatus{foreign, other}) {
		t.Fatalf("status cleanup changed foreign rows: %+v", got)
	}
}

func TestNetworkScopeMergesRebaseIndependentNetworks(t *testing.T) {
	a := networkScopeForTest(t, "infra", "management")
	b := networkScopeForTest(t, "infra", "storage")
	current := []kihv1.NetworkConfig{{NetworkName: a.NetworkName(), IPAddress: "10.1.0.5"}, {NetworkName: b.NetworkName(), IPAddress: "10.2.0.5"}}
	newA := []kihv1.NetworkConfig{{NetworkName: a.NetworkName(), IPAddress: "10.1.0.6"}}
	newB := []kihv1.NetworkConfig{{NetworkName: b.NetworkName(), IPAddress: "10.2.0.6"}}
	ab := b.MergeSpec("tenant", a.MergeSpec("tenant", current, newA), newB)
	ba := a.MergeSpec("tenant", b.MergeSpec("tenant", current, newB), newA)
	want := []kihv1.NetworkConfig{newA[0], newB[0]}
	if !reflect.DeepEqual(ab, want) || !reflect.DeepEqual(ba, want) {
		t.Fatalf("independent rebase lost a network: A/B=%+v B/A=%+v", ab, ba)
	}
	if got := a.MergeSpec("tenant", newB, newA); !reflect.DeepEqual(got, []kihv1.NetworkConfig{newB[0], newA[0]}) {
		t.Fatalf("first owned projection lost existing network: %+v", got)
	}
}

func TestNetworkScopeEmptyAndZeroMerges(t *testing.T) {
	scope := networkScopeForTest(t, "infra", "management")
	if scope.MergeSpec("tenant", nil, nil) != nil || scope.MergeStatus("tenant", nil, nil) != nil {
		t.Fatal("unchanged nil rows became nonnil")
	}
	emptySpec := []kihv1.NetworkConfig{}
	emptyStatus := []kihv1.NetworkConfigStatus{}
	if scope.MergeSpec("tenant", emptySpec, nil) == nil || scope.MergeStatus("tenant", emptyStatus, nil) == nil {
		t.Fatal("unchanged empty rows became nil")
	}
	var zero NetworkScope
	spec := []kihv1.NetworkConfig{{NetworkName: scope.NetworkName(), IPAddress: "10.1.0.5"}}
	status := []kihv1.NetworkConfigStatus{{NetworkName: scope.NetworkName(), Status: "bound"}}
	if !reflect.DeepEqual(zero.MergeSpec("tenant", spec, spec), spec) || !reflect.DeepEqual(zero.MergeStatus("tenant", status, status), status) {
		t.Fatal("zero scope modified existing rows")
	}
	if len(zero.FilterSpec("tenant", spec)) != 0 || len(zero.FilterStatus("tenant", status)) != 0 {
		t.Fatal("zero scope selected rows")
	}
}
