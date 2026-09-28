package ippool

import (
	"sync/atomic"
	"testing"
)

// the uid generation guard of the delete path: the cache is keyed by the
// networkname, so a pool which is deleted and recreated under the same
// name resolves the replacement's live registration, and a retried stale
// delete would tear it down (its DHCP listener, its ipam subnet, its dhcp
// pool and its cache entry) until the replacement's resync re-registers
// them. a delete whose tombstone uid differs from the cached registration
// is skipped; a matching uid tears the live registration down exactly
// like before, and an empty tombstone uid (a tombstone whose metadata was
// degraded, for example a DeletedFinalStateUnknown delivery) falls back
// to the name-only behavior instead of reading as a mismatch.
func TestSyncDeleteSkipsCleanupForSameNameReplacement(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-d")
	controller, cache := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate)

	// the replacement's live registration
	pool := testPool("pool-d", "net-d", 60)
	pool.ObjectMeta.UID = "replacement-uid"
	if err := cache.Add(pool); err != nil {
		t.Fatalf("seeding the replacement's cache entry: %v", err)
	}
	if err := controller.ipam.NewSubnet("net-d", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("seeding the replacement's subnet: %v", err)
	}
	controller.registeredPools = map[string]string{"pool-d": "net-d"}

	// the deleted generation's tombstone: same name and networkname, a
	// different uid
	deleted := Event{key: "pool-d", action: DELETE, poolName: "pool-d", poolNetworkName: "net-d", poolUID: "deleted-uid"}
	if err := controller.sync(deleted); err != nil {
		t.Fatalf("the stale delete sync failed: %v", err)
	}

	if _, err := cache.Get("pool", "net-d"); err != nil {
		t.Errorf("the replacement's cache entry was removed by the stale delete: %v", err)
	}
	if net, live := controller.registeredPools["pool-d"]; !live || net != "net-d" {
		t.Errorf("the replacement's registration record was removed by the stale delete: registeredPools[pool-d] = %q, live=%v", net, live)
	}
	if err := controller.ipam.NewSubnet("net-d", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err == nil {
		t.Error("the replacement's ipam subnet was deleted by the stale delete")
	}
}

// the paired control: with the guard's condition satisfied (the tombstone
// uid matches the cached registration), the same fixture tears the live
// registration down - which is what makes the skip assertions above
// meaningful.
func TestSyncDeleteWithMatchingUidTearsDownLiveRegistration(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-d")
	controller, cache := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate)

	pool := testPool("pool-d", "net-d", 60)
	pool.ObjectMeta.UID = "live-uid"
	if err := cache.Add(pool); err != nil {
		t.Fatalf("seeding the live registration: %v", err)
	}
	if err := controller.ipam.NewSubnet("net-d", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("seeding the subnet: %v", err)
	}
	controller.registeredPools = map[string]string{"pool-d": "net-d"}

	own := Event{key: "pool-d", action: DELETE, poolName: "pool-d", poolNetworkName: "net-d", poolUID: "live-uid"}
	if err := controller.sync(own); err != nil {
		t.Fatalf("the delete sync failed: %v", err)
	}

	if _, err := cache.Get("pool", "net-d"); err == nil {
		t.Error("the live registration survived its own delete")
	}
	if _, live := controller.registeredPools["pool-d"]; live {
		t.Error("the registration record survived its own delete")
	}
	if err := controller.ipam.NewSubnet("net-d", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Errorf("the ipam subnet survived its own delete: %v", err)
	}
}

// an unidentifiable generation must not read as a mismatch: an empty
// tombstone uid falls back to the name-only behavior, so the deletion
// still tears the live registration down.
func TestSyncDeleteWithUnknownUidTearsDownLiveRegistration(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-d")
	controller, cache := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate)

	pool := testPool("pool-d", "net-d", 60)
	pool.ObjectMeta.UID = "live-uid"
	if err := cache.Add(pool); err != nil {
		t.Fatalf("seeding the live registration: %v", err)
	}
	if err := controller.ipam.NewSubnet("net-d", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("seeding the subnet: %v", err)
	}
	controller.registeredPools = map[string]string{"pool-d": "net-d"}

	deleted := Event{key: "pool-d", action: DELETE, poolName: "pool-d", poolNetworkName: "net-d", poolUID: ""}
	if err := controller.sync(deleted); err != nil {
		t.Fatalf("the delete sync failed: %v", err)
	}

	if _, err := cache.Get("pool", "net-d"); err == nil {
		t.Error("the live registration survived a deletion with an unidentifiable generation")
	}
}

// the renamed-pool delete path resolves the live registration under the
// recorded old networkname; a same-name replacement registered under a
// new networkname must not be torn down by the old generation's delete.
func TestSyncRenamedPoolDeleteSkipsCleanupForSameNameReplacement(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-d")
	controller, cache := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate)

	// the replacement registered under the NEW networkname while the old
	// generation's delete was in flight
	replacement := testPool("pool-d", "net-new", 60)
	replacement.ObjectMeta.UID = "replacement-uid"
	if err := cache.Add(replacement); err != nil {
		t.Fatalf("seeding the replacement's cache entry: %v", err)
	}
	if err := controller.ipam.NewSubnet("net-new", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err != nil {
		t.Fatalf("seeding the replacement's subnet: %v", err)
	}
	controller.registeredPools = map[string]string{"pool-d": "net-new"}

	// the old generation's delete: its recorded networkname differs from
	// the replacement's, so the renamed-pool path resolves the live
	// registration
	deleted := Event{key: "pool-d", action: DELETE, poolName: "pool-d", poolNetworkName: "net-old", poolUID: "deleted-uid"}
	if err := controller.sync(deleted); err != nil {
		t.Fatalf("the stale delete sync failed: %v", err)
	}

	if _, err := cache.Get("pool", "net-new"); err != nil {
		t.Errorf("the replacement's cache entry was removed by the stale renamed-pool delete: %v", err)
	}
	if err := controller.ipam.NewSubnet("net-new", "192.168.1.0/24", "192.168.1.10", "192.168.1.100"); err == nil {
		t.Error("the replacement's ipam subnet was deleted by the stale renamed-pool delete")
	}
}
