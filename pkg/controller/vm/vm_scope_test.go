package vm

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	kubevirtv1 "kubevirt.io/api/core/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

func vmScopePeer(t *testing.T, first *Controller, network string) *Controller {
	t.Helper()
	peer := newTestController(t, newTestQueue(), newTestIndexer(), nil, first.kihClientset)
	peer.scope = vmTestScope("default", network)
	return peer
}

func vmScopeAddNIC(vm *kubevirtv1.VirtualMachine, name, network, mac string) {
	vm.Spec.Template.Spec.Domain.Devices.Interfaces = append(vm.Spec.Template.Spec.Domain.Devices.Interfaces,
		kubevirtv1.Interface{Name: name, MacAddress: mac})
	vm.Spec.Template.Spec.Networks = append(vm.Spec.Template.Spec.Networks,
		kubevirtv1.Network{Name: name, NetworkSource: kubevirtv1.NetworkSource{
			Multus: &kubevirtv1.MultusNetwork{NetworkName: network},
		}})
}

func vmScopeStoredConfig(spec []kihv1.NetworkConfig, status []kihv1.NetworkConfigStatus) *kihv1.VirtualMachineNetworkConfig {
	return &kihv1.VirtualMachineNetworkConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "vm1", Namespace: "ns1", UID: "cfg-1", ResourceVersion: "1",
			Finalizers: []string{vmnetcfgFinalizer, "foreign.example/retain"}},
		Spec:   kihv1.VirtualMachineNetworkConfigSpec{VMName: "vm1", NetworkConfig: spec},
		Status: kihv1.VirtualMachineNetworkConfigStatus{NetworkConfig: status},
	}
}

func TestScopedProjectionMergesCompetingCreate(t *testing.T) {
	a, f := vmBehaviorNewTestController(t)
	b := vmScopePeer(t, a, "net-b")
	vm := multusVM("ns1", "vm1", "a", "default/net-a", "aa:bb:cc:00:00:01")
	vmScopeAddNIC(vm, "b", "default/net-b", "aa:bb:cc:00:00:02")
	var winnerErr error
	f.beforeVMNetCfgCreate = func() { winnerErr = b.handleVirtualMachineObjectChange(vm) }
	if err := a.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("losing creator did not merge: %v", err)
	}
	if winnerErr != nil {
		t.Fatalf("winning creator: %v", winnerErr)
	}
	got := f.storedVMNetCfg("ns1/vm1")
	want := []kihv1.NetworkConfig{
		testNetCfg("aa:bb:cc:00:00:02", "default/net-b", ""),
		testNetCfg("aa:bb:cc:00:00:01", "default/net-a", ""),
	}
	if got == nil || !reflect.DeepEqual(got.Spec.NetworkConfig, want) {
		t.Fatalf("both scopes must survive the create race: got %+v, want %+v", got, want)
	}
	if len(f.requestsFor(http.MethodPost, "/virtualmachinenetworkconfigs")) != 2 {
		t.Fatal("the scenario must reach competing creates, not only sequential updates")
	}
}

func TestScopedProjectionRebasesConflictWithoutLosingForeignStateOrAllocatedIP(t *testing.T) {
	a, f := vmBehaviorNewTestController(t)
	b := vmScopePeer(t, a, "net-b")
	oldA := testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "")
	foreign := testNetCfg("aa:bb:cc:00:00:02", "default/net-b", "10.1.0.11")
	foreignStatus := kihv1.NetworkConfigStatus{MACAddress: foreign.MACAddress, NetworkName: foreign.NetworkName, Status: "Error", Message: "preserve foreign diagnostic"}
	f.vmnetcfgs["ns1/vm1"] = vmScopeStoredConfig([]kihv1.NetworkConfig{foreign, oldA}, []kihv1.NetworkConfigStatus{foreignStatus})
	vm := multusVM("ns1", "vm1", "a", "default/net-a", oldA.MACAddress)
	vmScopeAddNIC(vm, "a2", "default/net-a", "aa:bb:cc:00:00:03")
	vmScopeAddNIC(vm, "b", "default/net-b", foreign.MACAddress)
	vmScopeAddNIC(vm, "b2", "default/net-b", "aa:bb:cc:00:00:04")
	var competingErr error
	f.beforeVMNetCfgUpdate = func(status bool, proposed *kihv1.VirtualMachineNetworkConfig) {
		if status {
			t.Error("expected the projection conflict on the spec resource")
		}
		if competingErr = b.handleVirtualMachineObjectChange(vm); competingErr != nil {
			return
		}
		api := a.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs("ns1")
		current, err := api.Get(context.Background(), "vm1", metav1.GetOptions{})
		if err != nil {
			competingErr = err
			return
		}
		current.Labels = map[string]string{"other-writer": "retained"}
		current.Annotations = map[string]string{"other-writer": "retained"}
		for i := range current.Spec.NetworkConfig {
			if current.Spec.NetworkConfig[i].MACAddress == oldA.MACAddress {
				current.Spec.NetworkConfig[i].IPAddress = "10.0.0.12"
			}
		}
		_, competingErr = api.Update(context.Background(), current, metav1.UpdateOptions{})
	}
	if err := a.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("projection conflict did not converge: %v", err)
	}
	if competingErr != nil {
		t.Fatalf("competing writer: %v", competingErr)
	}
	got := f.storedVMNetCfg("ns1/vm1")
	want := []kihv1.NetworkConfig{
		foreign,
		testNetCfg("aa:bb:cc:00:00:04", "default/net-b", ""),
		testNetCfg(oldA.MACAddress, "default/net-a", "10.0.0.12"),
		testNetCfg("aa:bb:cc:00:00:03", "default/net-a", ""),
	}
	if !reflect.DeepEqual(got.Spec.NetworkConfig, want) || !reflect.DeepEqual(got.Status.NetworkConfig, []kihv1.NetworkConfigStatus{foreignStatus}) {
		t.Fatalf("conflict overwrote concurrent state: %+v", got)
	}
	if got.Labels["other-writer"] != "retained" || got.Annotations["other-writer"] != "retained" || !reflect.DeepEqual(got.Finalizers, []string{vmnetcfgFinalizer, "foreign.example/retain"}) {
		t.Fatalf("foreign metadata lost: %+v", got.ObjectMeta)
	}
	if len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")) != 0 || a.dhcp.CheckLease(oldA.MACAddress) {
		t.Fatal("projection must not allocate or clean a still-desired NIC during retry")
	}
}

func TestScopedProjectionCanonicalIdentityPreservesAllocatedIP(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	mac := "aa:bb:cc:00:00:01"
	own := testNetCfg("AA-BB-CC-00-00-01", "net-a", "10.0.0.11")
	foreign := testNetCfg(mac, "default/net-b", "10.1.0.11")
	cfg := vmScopeStoredConfig([]kihv1.NetworkConfig{foreign, own}, nil)
	cfg.Namespace = "default"
	f.vmnetcfgs["default/vm1"] = cfg
	addSimpleLease(t, c.dhcp, mac, own.IPAddress, "default/vm1")
	vm := multusVM("default", "vm1", "a", "default/net-a", mac)
	if err := c.handleVirtualMachineObjectChange(vm); err != nil {
		t.Fatalf("canonical projection: %v", err)
	}
	got := f.storedVMNetCfg("default/vm1")
	want := []kihv1.NetworkConfig{foreign, testNetCfg(mac, "default/net-a", own.IPAddress)}
	if !reflect.DeepEqual(got.Spec.NetworkConfig, want) {
		t.Fatalf("canonical identity must retain only its own IP: got %+v, want %+v", got.Spec.NetworkConfig, want)
	}
	if !c.dhcp.CheckLease(mac) || len(f.requestsFor(http.MethodGet, "/ippools")) != 0 {
		t.Fatal("equivalent network/MAC spelling must not clean the existing binding")
	}
}

func TestScopedProjectionForeignOnlyIsReadOnly(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "existing"}[existing], func(t *testing.T) {
			c, f := vmBehaviorNewTestController(t)
			foreign := testNetCfg("aa:bb:cc:00:00:02", "default/net-b", "10.0.0.11")
			vm := multusVM("ns1", "vm1", "b", foreign.NetworkName, foreign.MACAddress)
			if err := c.dhcp.AddLease(foreign.MACAddress, foreign.NetworkName, foreign.IPAddress, "ns1/vm1"); err != nil {
				t.Fatal(err)
			}
			addSubnetWithOwnedIP(t, c.ipam, foreign.NetworkName, foreign.IPAddress, "ns1/vm1 ["+foreign.MACAddress+"]")
			var before *kihv1.VirtualMachineNetworkConfig
			if existing {
				before = vmScopeStoredConfig([]kihv1.NetworkConfig{foreign}, []kihv1.NetworkConfigStatus{
					{MACAddress: foreign.MACAddress, NetworkName: foreign.NetworkName, Status: "Error", Message: "untouched"},
				})
				f.vmnetcfgs["ns1/vm1"] = before.DeepCopy()
			}
			if err := c.handleVirtualMachineObjectChange(vm); err != nil {
				t.Fatal(err)
			}
			if err := c.deleteVirtualMachineNetworkConfigObject("ns1", "vm1"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.storedVMNetCfg("ns1/vm1"), before) || !c.dhcp.CheckLease(foreign.MACAddress) || c.ipam.Used(foreign.NetworkName) != 1 {
				t.Fatal("foreign-only reconciliation changed foreign object or bindings")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, req := range f.requests {
				if req.method != http.MethodGet || req.path == "/apis/kubevirtiphelper.k8s.binbash.org/v1/ippools" {
					t.Errorf("foreign-only object triggered mutation or cleanup lookup: %s %s", req.method, req.path)
				}
			}
		})
	}
}

func TestScopedProjectionCleansRemovedNICBeforeSpecAndStatusAcknowledgement(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	own := testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "10.0.0.11")
	foreign := testNetCfg("aa:bb:cc:00:00:02", "default/net-b", "10.0.0.12")
	ownStatus := kihv1.NetworkConfigStatus{MACAddress: own.MACAddress, NetworkName: own.NetworkName, Status: "Ready"}
	foreignStatus := kihv1.NetworkConfigStatus{MACAddress: "aa:bb:cc:00:00:03", NetworkName: foreign.NetworkName, Status: "Error", Message: "status-only foreign row"}
	f.vmnetcfgs["ns1/vm1"] = vmScopeStoredConfig([]kihv1.NetworkConfig{foreign, own}, []kihv1.NetworkConfigStatus{ownStatus, foreignStatus})
	addSimpleLease(t, c.dhcp, own.MACAddress, own.IPAddress, "ns1/vm1")
	addSubnetWithOwnedIP(t, c.ipam, own.NetworkName, own.IPAddress, "ns1/vm1 ["+own.MACAddress+"]")
	storePool(t, c, f, "pool-a", own.NetworkName, map[string]string{own.IPAddress: "ns1/vm1 [" + own.MACAddress + "]"})
	beforeAvailable := c.ipam.Available(own.NetworkName)
	if err := c.dhcp.AddLease(foreign.MACAddress, foreign.NetworkName, foreign.IPAddress, "ns1/vm1"); err != nil {
		t.Fatal(err)
	}
	addSubnetWithOwnedIP(t, c.ipam, foreign.NetworkName, foreign.IPAddress, "ns1/vm1 ["+foreign.MACAddress+"]")
	specObserved, statusObserved := false, false
	f.beforeVMNetCfgUpdate = func(status bool, proposed *kihv1.VirtualMachineNetworkConfig) {
		specObserved = true
		if status || c.dhcp.CheckLease(own.MACAddress) || c.ipam.Used(own.NetworkName) != 0 || len(f.storedPool("pool-a").Status.IPv4.Allocated) != 0 {
			t.Error("spec acknowledgement preceded binding cleanup")
		}
		counts := f.storedPool("pool-a").Status.IPv4
		if counts.Used != 0 || counts.Available != beforeAvailable+1 || counts.Used != c.ipam.Used(own.NetworkName) || counts.Available != c.ipam.Available(own.NetworkName) {
			t.Errorf("accounting did not reflect the last-NIC release before spec acknowledgement: %+v; allocator used=%d available=%d", counts, c.ipam.Used(own.NetworkName), c.ipam.Available(own.NetworkName))
		}
		current := f.storedVMNetCfg("ns1/vm1")
		if len(current.Spec.NetworkConfig) != 2 || !reflect.DeepEqual(current.Status.NetworkConfig, []kihv1.NetworkConfigStatus{ownStatus, foreignStatus}) {
			t.Error("cleanup lost its durable rows before spec acknowledgement")
		}
		f.mu.Lock()
		f.beforeVMNetCfgUpdate = func(status bool, proposed *kihv1.VirtualMachineNetworkConfig) {
			statusObserved = true
			current := f.storedVMNetCfg("ns1/vm1")
			if !status || !reflect.DeepEqual(current.Spec.NetworkConfig, []kihv1.NetworkConfig{foreign}) || !reflect.DeepEqual(proposed.Status.NetworkConfig, []kihv1.NetworkConfigStatus{foreignStatus}) {
				t.Error("status acknowledgement did not follow owned-only spec acknowledgement")
			}
			// A foreign status writer commits while this acknowledgement is
			// in flight. The retry must merge its new diagnostic, not restore
			// the value observed before cleanup.
			foreignStatus.Message = "updated by foreign status writer"
			current.Status.NetworkConfig = []kihv1.NetworkConfigStatus{ownStatus, foreignStatus}
			if _, err := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs("ns1").UpdateStatus(context.Background(), current, metav1.UpdateOptions{}); err != nil {
				t.Errorf("foreign status writer: %v", err)
			}
		}
		f.mu.Unlock()
	}
	if err := c.handleVirtualMachineObjectChange(multusVM("ns1", "vm1", "b", foreign.NetworkName, foreign.MACAddress)); err != nil {
		t.Fatalf("removing owned NIC: %v", err)
	}
	got := f.storedVMNetCfg("ns1/vm1")
	if !specObserved || !statusObserved || got == nil || !reflect.DeepEqual(got.Spec.NetworkConfig, []kihv1.NetworkConfig{foreign}) || !reflect.DeepEqual(got.Status.NetworkConfig, []kihv1.NetworkConfigStatus{foreignStatus}) {
		t.Fatalf("shared object did not retain foreign rows through ordered acknowledgements: %+v", got)
	}
	if !c.dhcp.CheckLease(foreign.MACAddress) || c.ipam.Used(foreign.NetworkName) != 1 || len(f.requestsFor(http.MethodDelete, "/virtualmachinenetworkconfigs/vm1")) != 0 {
		t.Fatal("removing this scope's last NIC damaged the foreign network")
	}
	counts := f.storedPool("pool-a").Status.IPv4
	if counts.Used != 0 || counts.Used != c.ipam.Used(own.NetworkName) || counts.Available != c.ipam.Available(own.NetworkName) || counts.Available != beforeAvailable+1 {
		t.Errorf("durable accounting does not reflect the last-NIC release: used=%d available=%d; allocator used=%d available=%d", counts.Used, counts.Available, c.ipam.Used(own.NetworkName), c.ipam.Available(own.NetworkName))
	}
}

type vmScopeReadIndexer struct {
	cache.Indexer
	read chan struct{}
}

func (i *vmScopeReadIndexer) GetByKey(key string) (interface{}, bool, error) {
	select {
	case i.read <- struct{}{}:
	default:
	}
	return i.Indexer.GetByKey(key)
}

func TestScopedSyncWaitsForSharedMutexBeforeFreshReads(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	indexer := &vmScopeReadIndexer{Indexer: newTestIndexer(), read: make(chan struct{}, 1)}
	c.indexer = indexer
	old := multusVM("ns1", "vm1", "old", "default/net-a", "aa:bb:cc:00:00:01")
	if err := indexer.Add(old); err != nil {
		t.Fatal(err)
	}
	shared := &sync.Mutex{}
	c.reconcileMu = shared
	shared.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(shared.Unlock) }
	defer release()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		done <- c.sync(Event{key: "ns1/vm1", action: UPDATE, vmName: "vm1", vmNamespace: "ns1"})
	}()
	<-started
	select {
	case <-indexer.read:
		t.Error("sync read the indexer before acquiring the era mutex")
	case err := <-done:
		t.Fatalf("sync escaped held mutex: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if len(f.requestsFor(http.MethodGet, "/virtualmachinenetworkconfigs/vm1")) != 0 {
		t.Error("sync read the API before acquiring the era mutex")
	}
	fresh := multusVM("ns1", "vm1", "new", "default/net-a", "aa:bb:cc:00:00:02")
	if err := indexer.Update(fresh); err != nil {
		t.Fatal(err)
	}
	foreign := testNetCfg("aa:bb:cc:00:00:03", "default/net-b", "10.1.0.11")
	f.mu.Lock()
	f.vmnetcfgs["ns1/vm1"] = vmScopeStoredConfig([]kihv1.NetworkConfig{foreign}, nil)
	f.mu.Unlock()
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(shutdownWait):
		t.Fatal("sync failed to finish after releasing the mutex")
	}
	got := f.storedVMNetCfg("ns1/vm1")
	want := []kihv1.NetworkConfig{foreign, testNetCfg("aa:bb:cc:00:00:02", "default/net-a", "")}
	if got == nil || !reflect.DeepEqual(got.Spec.NetworkConfig, want) || len(f.requestsFor(http.MethodPost, "/virtualmachinenetworkconfigs")) != 0 {
		t.Fatalf("sync did not use fresh indexer and API state after waiting: %+v", got)
	}
}

func TestScopedProjectionConflictRejectsStaleCleanupAcknowledgement(t *testing.T) {
	for _, change := range []string{"uid", "deleting", "removed-ip", "cancel"} {
		t.Run(change, func(t *testing.T) {
			c, f := vmBehaviorNewTestController(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.ctx = ctx
			own := testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "10.0.0.11")
			foreign := testNetCfg("aa:bb:cc:00:00:02", "default/net-b", "10.1.0.11")
			f.vmnetcfgs["ns1/vm1"] = vmScopeStoredConfig([]kihv1.NetworkConfig{own, foreign}, nil)
			addSimpleLease(t, c.dhcp, own.MACAddress, own.IPAddress, "ns1/vm1")
			addSubnetWithOwnedIP(t, c.ipam, own.NetworkName, own.IPAddress, "ns1/vm1 ["+own.MACAddress+"]")
			storePool(t, c, f, "pool-a", own.NetworkName, map[string]string{own.IPAddress: "ns1/vm1 [" + own.MACAddress + "]"})
			var concurrent *kihv1.VirtualMachineNetworkConfig
			cleanupWrites := 0
			f.beforeVMNetCfgUpdate = func(status bool, proposed *kihv1.VirtualMachineNetworkConfig) {
				if status {
					t.Error("expected a spec acknowledgement")
				}
				cleanupWrites = len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status"))
				current := f.storedVMNetCfg("ns1/vm1")
				switch change {
				case "uid":
					// Model the API replacing this object, independently of
					// the stale controller request currently in flight.
					current.UID = "replacement-uid"
				case "deleting":
					now := metav1.Now()
					current.DeletionTimestamp = &now
				case "removed-ip":
					current.Spec.NetworkConfig[0].IPAddress = "10.0.0.12"
				case "cancel":
					current.Annotations = map[string]string{"concurrent": "retained"}
				}
				vmBehaviorBumpResourceVersion(&current.ObjectMeta)
				f.mu.Lock()
				f.vmnetcfgs["ns1/vm1"] = current
				f.mu.Unlock()
				concurrent = current.DeepCopy()
				if change == "cancel" {
					cancel()
				}
			}
			if err := c.handleVirtualMachineObjectChange(testVM("ns1", "vm1")); err == nil {
				t.Fatal("stale cleanup acknowledgement must return an error")
			}
			if concurrent == nil || !reflect.DeepEqual(f.storedVMNetCfg("ns1/vm1"), concurrent) {
				t.Fatalf("stale projection overwrote concurrent %s state: %+v", change, f.storedVMNetCfg("ns1/vm1"))
			}
			if cleanupWrites == 0 || len(f.requestsFor(http.MethodPut, "/ippools/pool-a/status")) != cleanupWrites {
				t.Fatal("the conflict loop repeated binding cleanup rather than retrying only API intent")
			}
			if c.dhcp.CheckLease(own.MACAddress) || c.ipam.Used(own.NetworkName) != 0 {
				t.Fatal("completed cleanup was unexpectedly restored")
			}
		})
	}
}

func TestScopedCleanupPreservesSameOwnerMACOnForeignNetwork(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	own := testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "10.0.0.11")
	foreign := testNetCfg(own.MACAddress, "default/net-b", own.IPAddress)
	cfg := vmScopeStoredConfig([]kihv1.NetworkConfig{own, foreign}, nil)
	if err := c.dhcp.AddLease(foreign.MACAddress, foreign.NetworkName, foreign.IPAddress, "ns1/vm1"); err != nil {
		t.Fatal(err)
	}
	addSubnetWithOwnedIP(t, c.ipam, own.NetworkName, own.IPAddress, "ns1/vm1 ["+own.MACAddress+"]")
	addSubnetWithOwnedIP(t, c.ipam, foreign.NetworkName, foreign.IPAddress, "ns1/vm1 ["+foreign.MACAddress+"]")
	storePool(t, c, f, "pool-a", own.NetworkName, map[string]string{own.IPAddress: "ns1/vm1 [" + own.MACAddress + "]"})
	if err := c.cleanupNetworkInterface(cfg, &foreign); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	requests := len(f.requests)
	f.mu.Unlock()
	if requests != 0 || c.ipam.Used(own.NetworkName) != 1 || c.ipam.Used(foreign.NetworkName) != 1 || !c.dhcp.CheckLease(foreign.MACAddress) {
		t.Fatal("direct foreign cleanup was not a complete no-op")
	}
	if err := c.cleanupNetworkInterface(cfg, &own); err != nil {
		t.Fatal(err)
	}
	if c.ipam.Used(own.NetworkName) != 0 || c.ipam.Used(foreign.NetworkName) != 1 || !c.dhcp.CheckLease(foreign.MACAddress) {
		t.Fatal("owned cleanup destroyed the same-owner, same-MAC lease on a foreign network")
	}
	if len(f.storedPool("pool-a").Status.IPv4.Allocated) != 0 {
		t.Fatal("owned ledger record survived cleanup")
	}
}

func TestScopedProjectionResolvesRemovedStatusOnlyBindingFromOwnedLedger(t *testing.T) {
	c, f := vmBehaviorNewTestController(t)
	own := testNetCfg("aa:bb:cc:00:00:01", "default/net-a", "10.0.0.11")
	foreign := testNetCfg("aa:bb:cc:00:00:02", "default/net-b", "10.1.0.11")
	foreignStatus := kihv1.NetworkConfigStatus{MACAddress: foreign.MACAddress, NetworkName: foreign.NetworkName, Status: "Ready"}
	f.vmnetcfgs["ns1/vm1"] = vmScopeStoredConfig([]kihv1.NetworkConfig{foreign}, []kihv1.NetworkConfigStatus{
		{MACAddress: own.MACAddress, NetworkName: own.NetworkName, Status: "Error", Message: "legacy status-only binding"},
		foreignStatus,
	})
	addSimpleLease(t, c.dhcp, own.MACAddress, own.IPAddress, "ns1/vm1")
	addSubnetWithOwnedIP(t, c.ipam, own.NetworkName, own.IPAddress, "ns1/vm1 ["+own.MACAddress+"]")
	storePool(t, c, f, "pool-a", own.NetworkName, map[string]string{
		own.IPAddress: "ns1/vm1 [" + own.MACAddress + "]",
		"10.0.0.12":   "ns1/another-vm [aa:bb:cc:00:00:99]",
	})
	if err := c.handleVirtualMachineObjectChange(multusVM("ns1", "vm1", "b", foreign.NetworkName, foreign.MACAddress)); err != nil {
		t.Fatalf("status-only removal: %v", err)
	}
	got := f.storedVMNetCfg("ns1/vm1")
	if got == nil || !reflect.DeepEqual(got.Spec.NetworkConfig, []kihv1.NetworkConfig{foreign}) || !reflect.DeepEqual(got.Status.NetworkConfig, []kihv1.NetworkConfigStatus{foreignStatus}) {
		t.Fatalf("status-only cleanup damaged foreign rows or failed to acknowledge own row: %+v", got)
	}
	if c.dhcp.CheckLease(own.MACAddress) || c.ipam.Used(own.NetworkName) != 0 {
		t.Fatal("status-only row was acknowledged without releasing its resolved binding")
	}
	wantLedger := map[string]string{"10.0.0.12": "ns1/another-vm [aa:bb:cc:00:00:99]"}
	if !reflect.DeepEqual(f.storedPool("pool-a").Status.IPv4.Allocated, wantLedger) {
		t.Fatal("status-only cleanup must remove exactly its own durable reservation")
	}
}

func TestScopedLastNICRemovalDeletesEmptyConfigWithParent(t *testing.T) {
	for _, scenario := range []string{"parent-deleted", "different-parent", "UID-replaced", "unmanaged"} {
		t.Run(scenario, func(t *testing.T) {
			c, f := vmBehaviorNewTestController(t)
			c.indexer = newTestIndexer()
			vm := multusVM("ns1", "vm1", "a", "default/net-a", "02:00:00:00:00:01")
			key := "ns1/vm1"
			if err := c.indexer.Add(vm); err != nil {
				t.Fatal(err)
			}
			if err := c.sync(Event{key: key, action: ADD, vmNamespace: "ns1", vmName: "vm1"}); err != nil {
				t.Fatal(err)
			}
			created := f.storedVMNetCfg(key)
			if created == nil || len(created.Spec.NetworkConfig) != 1 || len(created.Finalizers) != 1 {
				t.Fatalf("normal projection did not create managed binding: %#v", created)
			}
			emptyVM := testVM("ns1", "vm1")
			if err := c.indexer.Update(emptyVM); err != nil {
				t.Fatal(err)
			}
			if err := c.sync(Event{key: key, action: UPDATE, vmNamespace: "ns1", vmName: "vm1"}); err != nil {
				t.Fatal(err)
			}
			empty := f.storedVMNetCfg(key)
			if empty == nil || len(empty.Spec.NetworkConfig)+len(empty.Status.NetworkConfig) != 0 || empty.DeletionTimestamp != nil {
				t.Fatalf("last-NIC projection did not leave expected empty live binding: %#v", empty)
			}
			f.mu.Lock()
			switch scenario {
			case "different-parent":
				f.vmnetcfgs[key].Spec.VMName = "another-vm"
			case "UID-replaced":
				f.vmnetcfgGetUIDOverride = "stale-uid"
			case "unmanaged":
				f.vmnetcfgs[key].Finalizers = nil
			}
			f.mu.Unlock()
			before := f.storedVMNetCfg(key)
			if err := c.indexer.Delete(emptyVM); err != nil {
				t.Fatal(err)
			}
			err := c.sync(Event{key: key, action: DELETE, vmNamespace: "ns1", vmName: "vm1"})
			if scenario == "different-parent" || scenario == "UID-replaced" {
				if err == nil {
					t.Fatal("parent/UID mismatch must reject deletion")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			got := f.storedVMNetCfg(key)
			if scenario == "parent-deleted" {
				if got != nil && got.DeletionTimestamp == nil {
					t.Fatalf("parent DELETE stranded empty managed config: %#v", got)
				}
			} else if !reflect.DeepEqual(got, before) {
				t.Fatalf("parent DELETE changed an unowned or replaced config: %#v", got)
			}
		})
	}
}
