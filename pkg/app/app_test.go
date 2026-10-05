package app

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

func TestFilterIPPoolsByVLAN(t *testing.T) {
	newPool := func(name string, annotations map[string]string) v1.IPPool {
		return v1.IPPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Annotations: annotations,
			},
		}
	}

	pools := []v1.IPPool{
		newPool("vlan10-pool", map[string]string{"kubevirtiphelper/vlan-id": "10"}),
		newPool("vlan20-pool", map[string]string{"kubevirtiphelper/vlan-id": "20"}),
		newPool("no-annotation-pool", nil),
		newPool("other-annotation-pool", map[string]string{"foo": "bar"}),
	}

	tests := []struct {
		name    string
		vlanID  string
		wantLen int
		want    []string
	}{
		{
			name:    "matches the pool with the same vlan-id value",
			vlanID:  "10",
			wantLen: 1,
			want:    []string{"vlan10-pool"},
		},
		{
			name:    "matches a different pool for a different vlan-id value",
			vlanID:  "20",
			wantLen: 1,
			want:    []string{"vlan20-pool"},
		},
		{
			name:    "matches nothing when no pool has that vlan-id value",
			vlanID:  "30",
			wantLen: 0,
		},
		{
			name:    "empty vlanID does not match pools without the annotation",
			vlanID:  "",
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterIPPoolsByVLAN(pools, tt.vlanID)
			if len(got) != tt.wantLen {
				t.Fatalf("filterIPPoolsByVLAN() returned %d pools, want %d", len(got), tt.wantLen)
			}

			for i, name := range tt.want {
				if got[i].Name != name {
					t.Errorf("filterIPPoolsByVLAN()[%d].Name = %q, want %q", i, got[i].Name, name)
				}
			}
		})
	}
}

func TestFilterIPPoolsByVLANNoPools(t *testing.T) {
	got := filterIPPoolsByVLAN(nil, "10")
	if len(got) != 0 {
		t.Fatalf("filterIPPoolsByVLAN(nil, ...) = %v, want empty", got)
	}
}
