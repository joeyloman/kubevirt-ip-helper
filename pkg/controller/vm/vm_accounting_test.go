package vm

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/client-go/rest"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
)

func TestScopedLiveRemovalAccountingIsBestEffort(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusOnly bool
		noIP       bool
	}{
		{name: "allocated-spec-ignores-failed-refresh"},
		{name: "status-only-ignores-failed-refresh", statusOnly: true},
		{name: "unassigned-spec-ignores-failed-refresh", noIP: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := vmBehaviorNewTestController(t)
			own := testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "10.0.0.11")
			foreign := testNetCfg("aa:bb:cc:00:00:02", "default/net-b", "10.0.0.12")
			ownRef := "ns1/vm1 [" + own.MACAddress + "]"
			foreignRef := "ns1/vm1 [" + foreign.MACAddress + "]"
			ownStatus := kihv1.NetworkConfigStatus{MACAddress: own.MACAddress, NetworkName: own.NetworkName, Status: "Ready"}
			foreignStatus := kihv1.NetworkConfigStatus{MACAddress: foreign.MACAddress, NetworkName: foreign.NetworkName, Status: "Error", Message: "foreign diagnostic"}
			spec := []kihv1.NetworkConfig{foreign}
			if !tc.statusOnly {
				row := own
				if tc.noIP {
					row.IPAddress = ""
				}
				spec = append(spec, row)
			}
			original := vmScopeStoredConfig(spec, []kihv1.NetworkConfigStatus{ownStatus, foreignStatus})
			f.vmnetcfgs["ns1/vm1"] = original.DeepCopy()
			addSimpleLease(t, c.dhcp, own.MACAddress, own.IPAddress, "ns1/vm1")
			addSubnetWithOwnedIP(t, c.ipam, own.NetworkName, own.IPAddress, ownRef)
			storePool(t, c, f, "pool-a", own.NetworkName, map[string]string{own.IPAddress: ownRef})
			if err := c.dhcp.AddLease(foreign.MACAddress, foreign.NetworkName, foreign.IPAddress, "ns1/vm1"); err != nil {
				t.Fatal(err)
			}
			addSubnetWithOwnedIP(t, c.ipam, foreign.NetworkName, foreign.IPAddress, foreignRef)
			storePool(t, c, f, "pool-b", foreign.NetworkName, map[string]string{foreign.IPAddress: foreignRef})
			foreignPool := f.storedPool("pool-b")
			foreignLease := c.dhcp.GetLease(foreign.MACAddress)
			beforeAvailable := c.ipam.Available(own.NetworkName)

			// Reject only the accounting persistence after local release. The
			// preceding durable un-record still reaches the real fake API.
			// Keep rejecting throughout reconciliation.
			var rejected atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/ippools/pool-a/status") && c.ipam.Used(own.NetworkName) == 0 {
					rejected.Store(true)
					writeAPIError(w, http.StatusInternalServerError, "injected post-release accounting failure")
					return
				}
				f.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			client, err := kihclientset.NewForConfig(&rest.Config{Host: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			c.kihClientset = client

			assertReleased := func() {
				t.Helper()
				pool := f.storedPool("pool-a")
				if c.dhcp.CheckLease(own.MACAddress) || c.ipam.Used(own.NetworkName) != 0 || len(c.ipam.IPsOwnedBy(own.NetworkName, ownRef)) != 0 || len(pool.Status.IPv4.Allocated) != 0 {
					t.Error("removed NIC still has a lease, allocator reservation, or durable ledger tuple")
				}
			}
			assertForeign := func() {
				t.Helper()
				if !reflect.DeepEqual(f.storedPool("pool-b"), foreignPool) || !reflect.DeepEqual(c.dhcp.GetLease(foreign.MACAddress), foreignLease) || c.ipam.Used(foreign.NetworkName) != 1 || !reflect.DeepEqual(c.ipam.IPsOwnedBy(foreign.NetworkName, foreignRef), []string{foreign.IPAddress}) {
					t.Error("owned NIC removal changed foreign ledger, lease, or allocation")
				}
			}
			var acknowledgements atomic.Int32
			f.beforeVMNetCfgUpdate = func(_ bool, _ *kihv1.VirtualMachineNetworkConfig) {
				acknowledgements.Add(1)
				assertReleased()
				assertForeign()
			}
			vm := multusVM("ns1", "vm1", "b", foreign.NetworkName, foreign.MACAddress)
			err = c.handleVirtualMachineObjectChange(vm)
			if !rejected.Load() || err != nil {
				t.Fatalf("accounting failure must not block cleanup: rejected=%t error=%v", rejected.Load(), err)
			}
			assertReleased()
			assertForeign()
			counts := f.storedPool("pool-a").Status.IPv4
			if counts.Used != 1 || counts.Available != beforeAvailable {
				t.Fatalf("failed post-release refresh changed stored counters: %+v", counts)
			}
			got := f.storedVMNetCfg("ns1/vm1")
			if acknowledgements.Load() == 0 || got == nil || !reflect.DeepEqual(got.Spec.NetworkConfig, []kihv1.NetworkConfig{foreign}) || !reflect.DeepEqual(got.Status.NetworkConfig, []kihv1.NetworkConfigStatus{foreignStatus}) || !reflect.DeepEqual(got.Finalizers, original.Finalizers) {
				t.Fatalf("cleanup did not acknowledge only owned rows while retaining the shared object: %+v", got)
			}
		})
	}
}
