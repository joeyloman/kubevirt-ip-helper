package vmnetcfg

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

const networkCleanupFinalizer = "kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup"

// Each helper has private allocators and a private era mutex, but talks to the
// same API. No shared allocator accidentally serializes the two networks.
func networkPeer(t *testing.T, shared *testEnv, name string) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.api = shared.api
	e.client = shared.client
	e.scope = testNetworkScope(t, testNamespace, name)
	e.controller = NewController(context.Background(), e.queue, e.indexer, nil, e.cache, e.ipam, e.dhcp, e.metrics, e.client, e.appStatus, nil, e.scope, e.reconcileMu)
	e.appStatus.Store(APP_RUNNING)
	return e
}

func seedNetworkPool(t *testing.T, e *testEnv, name string, allocated map[string]string) {
	t.Helper()
	if err := e.ipam.NewSubnet(e.scope.NetworkName(), testSubnet, "10.0.0.1", "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	e.seedPoolWith(&kihv1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{util.NetworkLabel: e.scope.Name(), util.NetworkNamespaceLabel: e.scope.Namespace()}},
		Spec:       kihv1.IPPoolSpec{NetworkName: e.scope.NetworkName(), IPv4Config: kihv1.IPv4Config{Subnet: testSubnet, ServerIP: "10.0.0.1"}},
		Status:     kihv1.IPPoolStatus{IPv4: kihv1.IPv4Status{Allocated: allocated}},
	})
}

func TestVMNetCfgForeignOnlySettlesWithoutWrites(t *testing.T) {
	e, controller, startupGate := newGateTestEnv(t)
	obj := newVMNetCfg("not-an-ip", "not-a-mac")
	obj.Spec.NetworkConfig[0].NetworkName = "elsewhere/net-test"
	obj.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: "net-other", MACAddress: testMAC2, Status: "ERROR", Message: "foreign bytes"}}
	e.seedVMNetCfg(obj)
	if err := controller.indexer.Add(obj); err != nil {
		t.Fatal(err)
	}
	if err := controller.sync(Event{key: testNamespace + "/" + testVMNetCfgName, action: ADD}); err != nil {
		t.Fatal(err)
	}
	if startupGate.Settled() != 1 {
		t.Fatal("foreign-only snapshot did not settle")
	}
	if got := e.getStoredVMNetCfg(); !reflect.DeepEqual(got, obj) {
		t.Fatalf("foreign-only object changed: %#v", got)
	}
	if e.countRequests(http.MethodPut, vmnetcfgMainPath)+e.countRequests(http.MethodPut, vmnetcfgStatusPath)+e.countRequests(http.MethodGet, apiPrefix+"/ippools") != 0 {
		t.Fatal("foreign-only reconcile wrote the object or inspected allocation ledgers")
	}
}

func TestVMNetCfgTwoNetworksRebaseInterleavedCommits(t *testing.T) {
	a := newTestEnv(t)
	a.appStatus.Store(APP_RUNNING)
	b := networkPeer(t, a, "net-other")
	seedNetworkPool(t, a, testPoolName, nil)
	seedNetworkPool(t, b, "pool-other", nil)
	obj := newVMNetCfg("", testMAC)
	foreignInvalid := kihv1.NetworkConfig{NetworkName: "elsewhere/invalid", MACAddress: "KEEP-MAC", IPAddress: "KEEP-IP"}
	obj.Spec.NetworkConfig = append([]kihv1.NetworkConfig{foreignInvalid}, obj.Spec.NetworkConfig...)
	obj.Spec.NetworkConfig = append(obj.Spec.NetworkConfig, kihv1.NetworkConfig{NetworkName: b.scope.NetworkName(), MACAddress: testMAC})
	foreignStatus := kihv1.NetworkConfigStatus{NetworkName: "elsewhere/status-only", MACAddress: "KEEP", Status: "ERROR", Message: "KEEP MESSAGE"}
	obj.Status.NetworkConfig = []kihv1.NetworkConfigStatus{foreignStatus}
	obj.Annotations = map[string]string{"unrelated": "keep"}
	obj.Finalizers = []string{networkCleanupFinalizer, "other/finalizer"}
	a.seedVMNetCfg(obj)
	interleaved := make(chan struct{})
	a.api.vmnetcfgBeforePut = func(sub string) {
		if sub != "" {
			return
		}
		a.api.mu.Lock()
		a.api.vmnetcfgBeforePut = nil
		a.api.mu.Unlock()
		close(interleaved)
		if err := b.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err != nil {
			t.Errorf("second helper commit: %s", err)
		}
	}
	if err := a.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err != nil {
		t.Fatal(err)
	}
	select {
	case <-interleaved:
	default:
		t.Fatal("did not exercise competing helper write")
	}
	stored := a.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 3 || len(stored.Status.NetworkConfig) != 3 {
		t.Fatalf("interleaved commits lost or duplicated rows: spec=%v status=%v", stored.Spec.NetworkConfig, stored.Status.NetworkConfig)
	}
	if !reflect.DeepEqual(stored.Spec.NetworkConfig[0], foreignInvalid) || !reflect.DeepEqual(stored.Status.NetworkConfig[0], foreignStatus) {
		t.Fatalf("foreign rows changed: spec=%v status=%v", stored.Spec.NetworkConfig, stored.Status.NetworkConfig)
	}
	for _, helper := range []struct {
		env      *testEnv
		poolName string
	}{{a, testPoolName}, {b, "pool-other"}} {
		e := helper.env
		network := e.scope.NetworkName()
		rows := e.scope.FilterSpec(stored.Namespace, stored.Spec.NetworkConfig)
		statuses := e.scope.FilterStatus(stored.Namespace, stored.Status.NetworkConfig)
		if len(rows) != 1 || len(statuses) != 1 {
			t.Fatalf("network %s lost or duplicated its commit: %v %v", network, rows, statuses)
		}
		if rows[0].MACAddress != testMAC || rows[0].NetworkName != network || statuses[0].MACAddress != testMAC || statuses[0].NetworkName != network || statuses[0].Status != "OK" {
			t.Errorf("network %s committed the wrong binding: %v %v", network, rows, statuses)
		}
		// AllocateIP ranges over a map: either address in this two-address
		// pool is valid, but every projection must retain the same binding.
		storedIP := rows[0].IPAddress
		if storedIP != "10.0.0.1" && storedIP != "10.0.0.2" {
			t.Fatalf("network %s committed IP %q outside its allocation range", network, storedIP)
		}
		lease := e.dhcp.GetLease(testMAC)
		if lease.ClientIP == nil || lease.ClientIP.String() != storedIP || lease.PoolName != network || lease.Reference != testNamespace+"/"+testVMName {
			t.Errorf("network %s lease does not match committed IP %s and owner: %+v", network, storedIP, lease)
		}
		owner := util.AllocationRef(testNamespace, testVMName, testMAC)
		if claims := e.ipam.IPsOwnedBy(network, owner); len(claims) != 1 || claims[0] != storedIP {
			t.Errorf("network %s named IPAM claims = %v, want only %s for %s", network, claims, storedIP, owner)
		}
		if used, available, exists := e.ipam.UsageCounts(network); !exists || used != 1 || available != 1 {
			t.Errorf("network %s IPAM accounting = %d/%d (registered %v), want 1/1", network, used, available, exists)
		}
		pool, err := e.client.KubevirtiphelperV1().IPPools().Get(context.Background(), helper.poolName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(pool.Status.IPv4.Allocated, map[string]string{storedIP: owner}) || pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 1 {
			t.Errorf("network %s durable allocation does not match committed binding and 1/1 accounting: %+v", network, pool.Status.IPv4)
		}
		for _, metric := range []string{metricIPPoolUsed, metricIPPoolAvail} {
			if value, present := e.metricValue(metric, map[string]string{"ippool": helper.poolName, "subnet": testSubnet, "network": network}); !present || value != 1 {
				t.Errorf("network %s metric %s = %v (present %v), want 1", network, metric, value, present)
			}
		}
		labels := map[string]string{"vm": testNamespace + "/" + testVMNetCfgName, "network": network, "mac": testMAC, "ip": storedIP, "status": "OK"}
		if value, present := e.metricValue(metricVMNetCfgStatus, labels); !present || value != 1 {
			t.Errorf("network %s binding metric = %v (present %v), want %v with value 1", network, value, present, labels)
		}
		if count := e.countMetricsByLabel(metricVMNetCfgStatus, "vm", testNamespace+"/"+testVMNetCfgName); count != 1 {
			t.Errorf("network %s binding metric series = %d, want only its own committed binding", network, count)
		}
	}
	if !reflect.DeepEqual(stored.Annotations, obj.Annotations) || !reflect.DeepEqual(stored.Finalizers, obj.Finalizers) {
		t.Fatal("competing commit lost unrelated metadata")
	}
	if a.countRequests(http.MethodPut, ippoolStatusPath) != 1 || a.countRequests(http.MethodPut, apiPrefix+"/ippools/pool-other/status") != 1 {
		t.Fatal("API conflict repeated pool allocation side effects")
	}
}

func TestVMNetCfgStatusConflictPreservesForeignProgress(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	seedNetworkPool(t, e, testPoolName, nil)
	obj := newVMNetCfg("10.0.0.1", testMAC)
	foreign := kihv1.NetworkConfigStatus{NetworkName: "default/net-other", MACAddress: testMAC, Status: "ERROR", Message: "new foreign progress"}
	e.seedVMNetCfg(obj)
	e.api.vmnetcfgStatusPutConflict = 1
	e.api.vmnetcfgStatusPutConflictFn = func(current *kihv1.VirtualMachineNetworkConfig) {
		current.Status.NetworkConfig = append(current.Status.NetworkConfig, foreign)
		current.Annotations = map[string]string{"other-helper": "keep"}
	}
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err != nil {
		t.Fatal(err)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Status.NetworkConfig) != 2 || !reflect.DeepEqual(stored.Status.NetworkConfig[0], foreign) || stored.Status.NetworkConfig[1].Status != "OK" || stored.Annotations["other-helper"] != "keep" {
		t.Fatalf("status rebase lost progress: %#v", stored)
	}
	if e.countRequests(http.MethodPut, ippoolStatusPath) != 1 {
		t.Fatal("status conflict replayed ledger mutations")
	}
}

func TestVMNetCfgDeletionAcknowledgesOnlyOwnedRows(t *testing.T) {
	a := newTestEnv(t)
	b := networkPeer(t, a, "net-other")
	owner := util.AllocationRef(testNamespace, testVMName, testMAC)
	seedNetworkPool(t, a, testPoolName, map[string]string{"10.0.0.1": owner})
	seedNetworkPool(t, b, "pool-other", map[string]string{"10.0.0.1": owner})
	for _, e := range []*testEnv{a, b} {
		if _, err := e.ipam.ReclaimIP(e.scope.NetworkName(), "10.0.0.1", owner); err != nil {
			t.Fatal(err)
		}
		if err := e.dhcp.AddLease(testMAC, e.scope.NetworkName(), "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
			t.Fatal(err)
		}
	}
	obj := newVMNetCfg("10.0.0.1", testMAC)
	now := metav1.Now()
	obj.DeletionTimestamp = &now
	obj.Finalizers = []string{networkCleanupFinalizer, "unrelated/keep"}
	foreign := kihv1.NetworkConfig{NetworkName: b.scope.NetworkName(), MACAddress: testMAC, IPAddress: "10.0.0.1"}
	foreignStatus := kihv1.NetworkConfigStatus{NetworkName: b.scope.NetworkName(), MACAddress: testMAC, Status: "OK"}
	obj.Spec.NetworkConfig = append(obj.Spec.NetworkConfig, foreign)
	obj.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: testNetwork, MACAddress: testMAC, Status: "OK"}, foreignStatus}
	a.seedVMNetCfg(obj)
	if err := a.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err != nil {
		t.Fatal(err)
	}
	stored := a.getStoredVMNetCfg()
	if !reflect.DeepEqual(stored.Spec.NetworkConfig, []kihv1.NetworkConfig{foreign}) || !reflect.DeepEqual(stored.Status.NetworkConfig, []kihv1.NetworkConfigStatus{foreignStatus}) || !reflect.DeepEqual(stored.Finalizers, obj.Finalizers) {
		t.Fatalf("first helper acknowledged foreign cleanup: %#v", stored)
	}
	if a.ipam.Used(testNetwork) != 0 || a.dhcp.CheckLease(testMAC) || b.ipam.Used(b.scope.NetworkName()) != 1 || !b.dhcp.CheckLease(testMAC) {
		t.Fatal("first helper cleanup crossed the network boundary")
	}
	if err := b.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err != nil {
		t.Fatal(err)
	}
	stored = a.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig)+len(stored.Status.NetworkConfig) != 0 || !reflect.DeepEqual(stored.Finalizers, []string{"unrelated/keep"}) {
		t.Fatalf("last helper did not complete deletion: %#v", stored)
	}
	if b.ipam.Used(b.scope.NetworkName()) != 0 || b.dhcp.CheckLease(testMAC) {
		t.Fatal("last helper acknowledged before cleanup")
	}
}

func TestVMNetCfgDeletionFinalizerRequiresBothArraysEmpty(t *testing.T) {
	for _, kind := range []string{"foreign-spec-only", "foreign-status-only", "empty"} {
		t.Run(kind, func(t *testing.T) {
			e := newTestEnv(t)
			obj := newVMNetCfg("", testMAC)
			obj.Spec.NetworkConfig = nil
			now := metav1.Now()
			obj.DeletionTimestamp = &now
			obj.Finalizers = []string{networkCleanupFinalizer, "kubevirtiphelper", "unrelated/keep"}
			if kind == "foreign-spec-only" {
				obj.Spec.NetworkConfig = []kihv1.NetworkConfig{{NetworkName: "default/net-other", MACAddress: testMAC}}
			}
			if kind == "foreign-status-only" {
				obj.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: "default/net-other", MACAddress: testMAC, Status: "ERROR"}}
			}
			e.seedVMNetCfg(obj)
			if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err != nil {
				t.Fatal(err)
			}
			stored := e.getStoredVMNetCfg()
			want := obj.Finalizers
			if kind == "empty" {
				want = []string{"unrelated/keep"}
			}
			if !reflect.DeepEqual(stored.Finalizers, want) || !reflect.DeepEqual(stored.Spec, obj.Spec) || !reflect.DeepEqual(stored.Status, obj.Status) {
				t.Fatalf("finalizer/row preservation: %#v", stored)
			}
		})
	}
}

func TestVMNetCfgDeletionRecoversAfterSpecAcknowledgementCrash(t *testing.T) {
	e := newTestEnv(t)
	owner := util.AllocationRef(testNamespace, testVMName, testMAC)
	seedNetworkPool(t, e, testPoolName, map[string]string{"10.0.0.1": owner})
	obj := newVMNetCfg("10.0.0.1", testMAC)
	now := metav1.Now()
	obj.DeletionTimestamp = &now
	obj.Finalizers = []string{networkCleanupFinalizer}
	obj.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: testNetwork, MACAddress: testMAC, Status: "OK"}}
	e.seedVMNetCfg(obj)
	e.api.vmnetcfgStatusPutCode = http.StatusInternalServerError
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err == nil {
		t.Fatal("failed status acknowledgement must be retried")
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig) != 0 || len(stored.Status.NetworkConfig) != 1 || len(stored.Finalizers) != 1 || len(e.getStoredPool().Status.IPv4.Allocated) != 0 {
		t.Fatalf("crash boundary lost cleanup evidence: %#v", stored)
	}
	// A new era has no local lease, claims or pending unwind state.
	restarted := networkPeer(t, e, "net-test")
	restarted.addSubnet("10.0.0.1", "10.0.0.2")
	restarted.seedPoolWith(e.getStoredPool())
	e.api.vmnetcfgStatusPutCode = 0
	if err := restarted.controller.updateVirtualMachineNetworkConfig(UPDATE, stored); err != nil {
		t.Fatal(err)
	}
	stored = e.getStoredVMNetCfg()
	if len(stored.Spec.NetworkConfig)+len(stored.Status.NetworkConfig)+len(stored.Finalizers) != 0 {
		t.Fatalf("restart did not finish status-only acknowledgement: %#v", stored)
	}
}

func TestVMNetCfgStatusOnlyDeletionRecoversOwnerLedger(t *testing.T) {
	e := newTestEnv(t)
	owner := util.AllocationRef(testNamespace, testVMName, testMAC)
	otherOwner := util.AllocationRef(testNamespace, "other-vm", testMAC)
	seedNetworkPool(t, e, testPoolName, map[string]string{"10.0.0.1": owner, "10.0.0.2": otherOwner})
	obj := newVMNetCfg("", testMAC)
	obj.Spec.NetworkConfig = nil
	obj.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: testNetwork, MACAddress: testMAC, Status: "ERROR"}}
	now := metav1.Now()
	obj.DeletionTimestamp = &now
	obj.Finalizers = []string{networkCleanupFinalizer}
	e.seedVMNetCfg(obj)
	e.api.poolListCode = http.StatusServiceUnavailable
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err == nil {
		t.Fatal("unavailable ledger inventory must not acknowledge a status-only binding")
	}
	if got := e.getStoredVMNetCfg(); !reflect.DeepEqual(got, obj) {
		t.Fatal("failed owner-ledger discovery discarded cleanup evidence")
	}
	e.api.poolListCode = 0
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err != nil {
		t.Fatal(err)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated; !reflect.DeepEqual(got, map[string]string{"10.0.0.2": otherOwner}) {
		t.Fatalf("status-only recovery touched another owner or left its own record: %v", got)
	}
	if got := e.getStoredVMNetCfg(); len(got.Status.NetworkConfig)+len(got.Finalizers) != 0 {
		t.Fatalf("status-only recovery did not acknowledge: %#v", got)
	}
}

func TestVMNetCfgMutexWaitRefreshesOwnedRows(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	seedNetworkPool(t, e, testPoolName, nil)
	stale := newVMNetCfg("", testMAC)
	e.seedVMNetCfg(stale)
	e.reconcileMu.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(e.reconcileMu.Unlock) }
	t.Cleanup(release)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- e.controller.updateVirtualMachineNetworkConfig(UPDATE, stale)
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("reconciliation passed the held era mutex: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if e.totalRequests() != 0 {
		t.Fatal("reconciliation read API before acquiring the era mutex")
	}
	current := stale.DeepCopy()
	current.Spec.NetworkConfig = []kihv1.NetworkConfig{{NetworkName: "default/net-other", MACAddress: testMAC}}
	e.seedVMNetCfg(current)
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(shutdownWait):
		t.Fatal("reconciliation did not finish after mutex release")
	}
	if e.dhcp.CheckLease(testMAC) || e.ipam.Used(testNetwork) != 0 || len(e.getStoredPool().Status.IPv4.Allocated) != 0 {
		t.Fatal("resumed reconciliation allocated the stale owned row")
	}
	if got := e.getStoredVMNetCfg(); !reflect.DeepEqual(got, current) {
		t.Fatal("resumed reconciliation overwrote fresh foreign intent")
	}
}

func TestVMNetCfgLiveStatusCommitRejectsDeletionRace(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	seedNetworkPool(t, e, testPoolName, nil)
	obj := newVMNetCfg("10.0.0.1", testMAC)
	obj.Finalizers = []string{networkCleanupFinalizer}
	e.seedVMNetCfg(obj)
	e.api.vmnetcfgStatusPutConflict = 1
	e.api.vmnetcfgStatusPutConflictFn = func(current *kihv1.VirtualMachineNetworkConfig) {
		now := metav1.Now()
		current.DeletionTimestamp = &now
	}
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err == nil {
		t.Fatal("stale live status decision must return for reconciliation after deletion")
	}
	if got := e.getStoredVMNetCfg(); got.DeletionTimestamp == nil || len(got.Status.NetworkConfig) != 0 || len(got.Finalizers) != 1 {
		t.Fatalf("stale live status committed across deletion: %#v", got)
	}
}

func TestVMNetCfgStatusOnlyDeletionRecoversIPAMOnlyClaims(t *testing.T) {
	e := newTestEnv(t)
	seedNetworkPool(t, e, testPoolName, nil)
	owner := util.AllocationRef(testNamespace, testVMName, testMAC)
	foreignOwner := util.AllocationRef(testNamespace, "other-vm", testMAC)
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.1", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ipam.ReclaimIP(testNetwork, "10.0.0.2", foreignOwner); err != nil {
		t.Fatal(err)
	}
	obj := newDeletingVMNetCfg(nil)
	obj.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: testNetwork, MACAddress: testMAC, Status: "ERROR"}}
	e.seedVMNetCfg(obj)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, obj); err != nil {
		t.Fatal(err)
	}
	if e.ipam.Used(testNetwork) != 1 {
		t.Fatal("status-only cleanup failed to release own claim or released foreign claim")
	}
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.2"); err == nil {
		t.Fatal("foreign claim became reissuable")
	}
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.1"); err != nil {
		t.Fatalf("own IPAM-only claim was not released: %s", err)
	}
	stored := e.getStoredVMNetCfg()
	if len(stored.Status.NetworkConfig)+len(stored.Finalizers) != 0 {
		t.Fatalf("IPAM-only cleanup was not acknowledged: %#v", stored)
	}
}

func TestVMNetCfgNetworkReferencesResolveInObjectNamespace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace string
		reference string
		owned     bool
	}{
		{name: "bare-local", namespace: testNamespace, reference: "net-test", owned: true},
		{name: "qualified-tenant", namespace: "tenant-a", reference: testNetwork, owned: true},
		{name: "bare-tenant-is-foreign", namespace: "tenant-a", reference: "net-test", owned: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.appStatus.Store(APP_RUNNING)
			seedNetworkPool(t, e, testPoolName, nil)
			obj := newVMNetCfg("", testMAC)
			obj.Namespace = tc.namespace
			obj.Spec.NetworkConfig[0].NetworkName = tc.reference
			e.seedVMNetCfg(obj)
			if err := e.controller.updateVirtualMachineNetworkConfig(ADD, obj); err != nil {
				t.Fatal(err)
			}
			if e.dhcp.CheckLease(testMAC) != tc.owned {
				t.Fatalf("lease ownership for %s/%s disagrees with object namespace", tc.namespace, tc.reference)
			}
			stored, err := e.client.KubevirtiphelperV1().VirtualMachineNetworkConfigs(tc.namespace).Get(context.Background(), obj.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.owned {
				if len(stored.Spec.NetworkConfig) != 1 || len(stored.Status.NetworkConfig) != 1 {
					t.Fatalf("owned binding rows missing: %+v", stored)
				}
				row := stored.Spec.NetworkConfig[0]
				if (row.IPAddress != "10.0.0.1" && row.IPAddress != "10.0.0.2") || row.NetworkName != testNetwork || row.MACAddress != testMAC {
					t.Fatalf("owned reference did not resolve to an in-pool canonical binding: %+v", row)
				}
				status := stored.Status.NetworkConfig[0]
				if status.NetworkName != testNetwork || status.MACAddress != testMAC || status.Status != "OK" {
					t.Fatalf("owned status disagrees with the binding: %+v", status)
				}
				owner := util.AllocationRef(tc.namespace, obj.Spec.VMName, testMAC)
				if !e.dhcp.HasOwnedLease(testMAC, tc.namespace+"/"+obj.Spec.VMName, testNetwork, row.IPAddress) ||
					!reflect.DeepEqual(e.ipam.IPsOwnedBy(testNetwork, owner), []string{row.IPAddress}) {
					t.Fatal("namespace-qualified owner or address disagrees across lease and claim")
				}
				pool := e.getStoredPool()
				if !reflect.DeepEqual(pool.Status.IPv4.Allocated, map[string]string{row.IPAddress: owner}) ||
					pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 1 ||
					e.ipam.Used(testNetwork) != 1 || e.ipam.Available(testNetwork) != 1 {
					t.Fatalf("owned allocation ledger or accounting disagrees: %+v", pool.Status.IPv4)
				}
			} else if !reflect.DeepEqual(stored.Spec, obj.Spec) || len(stored.Status.NetworkConfig) != 0 ||
				e.ipam.Used(testNetwork) != 0 || len(e.getStoredPool().Status.IPv4.Allocated) != 0 {
				t.Fatal("foreign bare reference was rewritten or allocated")
			}
		})
	}
}

func TestVMNetCfgQueuedDeleteReconcilesCurrentReplacement(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	seedNetworkPool(t, e, testPoolName, nil)
	replacement := newVMNetCfg("10.0.0.1", testMAC)
	replacement.UID = "replacement"
	e.seedVMNetCfg(replacement)
	if err := e.controller.sync(Event{key: testNamespace + "/" + testVMNetCfgName, action: DELETE}); err != nil {
		t.Fatal(err)
	}
	stored := e.getStoredVMNetCfg()
	if stored.UID != replacement.UID || stored.DeletionTimestamp != nil || len(stored.Spec.NetworkConfig) != 1 || len(stored.Status.NetworkConfig) != 1 || stored.Status.NetworkConfig[0].Status != "OK" {
		t.Fatalf("queued delete did not reconcile the current replacement: %#v", stored)
	}
	if !e.dhcp.CheckLease(testMAC) || e.ipam.Used(testNetwork) != 1 || e.getStoredPool().Status.IPv4.Allocated["10.0.0.1"] != util.AllocationRef(testNamespace, testVMName, testMAC) {
		t.Fatal("queued delete lost the replacement's binding")
	}
	if e.countRequests(http.MethodDelete, vmnetcfgMainPath) != 0 {
		t.Fatal("queued delete attempted to delete the replacement")
	}
}

func TestVMNetCfgFreshReadRejectsReplacedUIDBeforeAllocation(t *testing.T) {
	e := newTestEnv(t)
	e.appStatus.Store(APP_RUNNING)
	seedNetworkPool(t, e, testPoolName, nil)
	stale := newVMNetCfg("", testMAC)
	stale.UID = "old"
	replacement := stale.DeepCopy()
	replacement.UID = "replacement"
	e.seedVMNetCfg(replacement)
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, stale); err == nil {
		t.Fatal("stale UID must invalidate the allocation decision")
	}
	if e.dhcp.CheckLease(testMAC) || e.ipam.Used(testNetwork) != 0 || len(e.getStoredPool().Status.IPv4.Allocated) != 0 {
		t.Fatal("stale UID caused allocation side effects")
	}
	if got := e.getStoredVMNetCfg(); !reflect.DeepEqual(got, replacement) {
		t.Fatalf("stale UID changed replacement: %#v", got)
	}
}

func disjointMACConfigs(t *testing.T, statusOnlySibling bool) (*testEnv, *kihv1.VirtualMachineNetworkConfig, *kihv1.VirtualMachineNetworkConfig) {
	t.Helper()
	e := newTestEnv(t)
	seedNetworkPool(t, e, testPoolName, map[string]string{
		"10.0.0.1": util.AllocationRef(testNamespace, testVMName, testMAC),
		"10.0.0.2": util.AllocationRef(testNamespace, testVMName, testMAC2),
	})
	for _, binding := range []struct{ ip, mac string }{{"10.0.0.1", testMAC}, {"10.0.0.2", testMAC2}} {
		if _, err := e.ipam.ReclaimIP(testNetwork, binding.ip, util.AllocationRef(testNamespace, testVMName, binding.mac)); err != nil {
			t.Fatal(err)
		}
		if err := e.dhcp.AddLease(binding.mac, testNetwork, binding.ip, testNamespace+"/"+testVMName); err != nil {
			t.Fatal(err)
		}
	}
	deleting := newDeletingVMNetCfg([]kihv1.NetworkConfig{{NetworkName: testNetwork, MACAddress: testMAC, IPAddress: "10.0.0.1"}})
	deleting.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: testNetwork, MACAddress: testMAC, Status: "OK"}}
	e.seedVMNetCfg(deleting)
	sibling := newVMNetCfg("10.0.0.2", testMAC2)
	sibling.Name = "another-config-for-same-vm"
	sibling.UID = "sibling"
	if statusOnlySibling {
		sibling.Spec.NetworkConfig = nil
		sibling.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: testNetwork, MACAddress: testMAC2, Status: "ERROR"}}
	}
	e.seedVMNetCfg(sibling)
	return e, deleting, sibling
}

func assertSiblingBindingPreserved(t *testing.T, e *testEnv, sibling *kihv1.VirtualMachineNetworkConfig) {
	t.Helper()
	owner := util.AllocationRef(testNamespace, testVMName, testMAC2)
	pool := e.getStoredPool()
	if !reflect.DeepEqual(pool.Status.IPv4.Allocated, map[string]string{"10.0.0.2": owner}) || pool.Status.IPv4.Used != 1 || pool.Status.IPv4.Available != 1 {
		t.Fatalf("same-VM sibling reservation or accounting changed: %+v", pool.Status.IPv4)
	}
	lease := e.dhcp.GetLease(testMAC2)
	if lease.ClientIP == nil || lease.ClientIP.String() != "10.0.0.2" || lease.Reference != testNamespace+"/"+testVMName || lease.PoolName != testNetwork {
		t.Fatalf("same-VM sibling lease changed: %+v", lease)
	}
	if e.ipam.Used(testNetwork) != 1 || e.dhcp.CheckLease(testMAC) {
		t.Fatal("cleanup did not exclusively release the deleting config's binding")
	}
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.2"); err == nil {
		t.Fatal("same-VM sibling claim became reissuable")
	}
	e.api.mu.Lock()
	storedSibling := e.api.vmnetcfgs[sibling.Namespace+"/"+sibling.Name].DeepCopy()
	e.api.mu.Unlock()
	if !reflect.DeepEqual(storedSibling, sibling) {
		t.Fatalf("cleanup rewrote sibling config: %#v", storedSibling)
	}
	if stored := e.getStoredVMNetCfg(); len(stored.Spec.NetworkConfig)+len(stored.Status.NetworkConfig)+len(stored.Finalizers) != 0 {
		t.Fatalf("own cleanup did not complete: %#v", stored)
	}
}

func TestVMNetCfgDeletionPreservesAnotherConfigForSameVM(t *testing.T) {
	for _, reference := range []string{"spec", "status-only"} {
		t.Run(reference, func(t *testing.T) {
			e, deleting, sibling := disjointMACConfigs(t, reference == "status-only")
			if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, deleting); err != nil {
				t.Fatal(err)
			}
			assertSiblingBindingPreserved(t, e, sibling)
		})
	}
}

func TestVMNetCfgDeletionBlocksWhenSiblingReferenceLookupFails(t *testing.T) {
	e, deleting, sibling := disjointMACConfigs(t, false)
	e.api.vmnetcfgListCode = http.StatusServiceUnavailable
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, deleting); err == nil {
		t.Fatal("ambiguous same-VM ledger recovery must fail closed when sibling lookup fails")
	}
	stored := e.getStoredVMNetCfg()
	if !reflect.DeepEqual(stored.Spec, deleting.Spec) || !reflect.DeepEqual(stored.Status, deleting.Status) || !reflect.DeepEqual(stored.Finalizers, deleting.Finalizers) {
		t.Fatalf("failed sibling lookup acknowledged ambiguous cleanup: %#v", stored)
	}
	if got := e.getStoredPool().Status.IPv4.Allocated["10.0.0.2"]; got != util.AllocationRef(testNamespace, testVMName, testMAC2) || !e.dhcp.CheckLease(testMAC2) {
		t.Fatal("failed sibling lookup destroyed another config's binding")
	}
	if _, err := e.ipam.GetIP(testNetwork, "10.0.0.2"); err == nil {
		t.Fatal("failed sibling lookup released another config's claim")
	}
	e.api.mu.Lock()
	e.api.vmnetcfgListCode = 0
	e.api.mu.Unlock()
	if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, stored); err != nil {
		t.Fatal(err)
	}
	assertSiblingBindingPreserved(t, e, sibling)
}

func TestVMNetCfgStatusOnlyCleanupPreservesTransferredBinding(t *testing.T) {
	for _, localOnly := range []bool{false, true} {
		name := "durable-ledger"
		if localOnly {
			name = "local-only"
		}
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			e.appStatus.Store(APP_RUNNING)
			owner := util.AllocationRef(testNamespace, testVMName, testMAC)
			foreign := util.AllocationRef(testNamespace, "unrelated-vm", testMAC2)
			seedNetworkPool(t, e, testPoolName, map[string]string{"10.0.0.1": owner, "10.0.0.2": foreign})
			pool := e.getStoredPool()
			pool.Spec.IPv4Config.ServerIP = "10.0.0.254"
			pool.Spec.IPv4Config.Router = "10.0.0.254"
			pool.Spec.IPv4Config.Pool.Start = "10.0.0.1"
			pool.Spec.IPv4Config.Pool.End = "10.0.0.2"
			pool.Status.IPv4.Used, pool.Status.IPv4.Available = 2, 0
			if err := e.cache.Upsert(pool); err != nil {
				t.Fatal(err)
			}
			e.api.seedPool(pool)
			for ip, ref := range map[string]string{"10.0.0.1": owner, "10.0.0.2": foreign} {
				if _, err := e.ipam.ReclaimIP(testNetwork, ip, ref); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.dhcp.AddLease(testMAC, testNetwork, "10.0.0.1", testNamespace+"/"+testVMName); err != nil {
				t.Fatal(err)
			}
			if err := e.dhcp.AddLease(testMAC2, testNetwork, "10.0.0.2", testNamespace+"/unrelated-vm"); err != nil {
				t.Fatal(err)
			}
			old := newDeletingVMNetCfg([]kihv1.NetworkConfig{{NetworkName: testNetwork, MACAddress: testMAC, IPAddress: "10.0.0.1"}})
			old.UID = "old-config"
			old.Status.NetworkConfig = []kihv1.NetworkConfigStatus{{NetworkName: testNetwork, MACAddress: testMAC, Status: "OK"}}
			e.seedVMNetCfg(old)
			e.api.vmnetcfgStatusPutCode = http.StatusInternalServerError
			if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, old); err == nil {
				t.Fatal("expected interrupted status acknowledgement")
			}
			intermediate := e.getStoredVMNetCfg()
			if len(intermediate.Spec.NetworkConfig) != 0 || len(intermediate.Status.NetworkConfig) != 1 || len(intermediate.Finalizers) != 1 {
				t.Fatalf("not at spec-ack/status-pending boundary: %#v", intermediate)
			}
			e.api.vmnetcfgStatusPutCode = 0
			successor := newVMNetCfg("10.0.0.1", testMAC)
			successor.Name, successor.UID = "successor-config", "successor-config-uid"
			e.seedVMNetCfg(successor)
			if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, successor); err != nil {
				t.Fatal(err)
			}
			client := e.client.KubevirtiphelperV1().VirtualMachineNetworkConfigs(testNamespace)
			before, err := client.Get(e.controller.ctx, successor.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !e.dhcp.HasOwnedLease(testMAC, testNamespace+"/"+testVMName, testNetwork, "10.0.0.1") {
				t.Fatal("successor not restored")
			}
			wantLedger := map[string]string{"10.0.0.1": owner, "10.0.0.2": foreign}
			if localOnly {
				delete(wantLedger, "10.0.0.1")
				pool = e.getStoredPool()
				delete(pool.Status.IPv4.Allocated, "10.0.0.1")
				e.api.seedPool(pool)
			}
			if err := e.controller.updateVirtualMachineNetworkConfig(UPDATE, intermediate); err != nil {
				t.Fatal(err)
			}
			after, err := client.Get(e.controller.ctx, successor.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("old cleanup rewrote successor object: before=%#v after=%#v", before, after)
			}
			if got := e.getStoredPool().Status.IPv4.Allocated; !reflect.DeepEqual(got, wantLedger) {
				t.Errorf("old status-only cleanup changed ledger: got=%v want=%v", got, wantLedger)
			}
			if !e.dhcp.HasOwnedLease(testMAC, testNamespace+"/"+testVMName, testNetwork, "10.0.0.1") || !reflect.DeepEqual(e.ipam.IPsOwnedBy(testNetwork, owner), []string{"10.0.0.1"}) {
				t.Error("old status-only cleanup destroyed successor lease/claim")
			}
			if !e.dhcp.HasOwnedLease(testMAC2, testNamespace+"/unrelated-vm", testNetwork, "10.0.0.2") || !reflect.DeepEqual(e.ipam.IPsOwnedBy(testNetwork, foreign), []string{"10.0.0.2"}) {
				t.Error("unrelated binding changed")
			}
			if got := e.getStoredVMNetCfg(); len(got.Spec.NetworkConfig)+len(got.Status.NetworkConfig)+len(got.Finalizers) != 0 {
				t.Errorf("old acknowledgements did not finish: %#v", got)
			}
		})
	}
}
