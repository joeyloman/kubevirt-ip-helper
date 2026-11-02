package ippool

import (
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The registration seeding barrier: a pool which registers again after the
// startup gate dropped its retries (an UPDATE resync recovery) must pin the
// persisted ownership claims of its bindings in the fresh allocator before
// it becomes visible to fresh allocations. A fresh vm snapshot must never
// take an address whose binding still exists.

func TestProtectPersistedClaimsPinsThePersistedOwner(t *testing.T) {
	stored := ippoolBehaviorNewTestPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.10.10.10": "default/vm-a [02:00:00:00:00:01]",
		"10.10.10.99": "EXCLUDED",
	}

	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)
	pool := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := c.ipam.NewSubnet("net-a", "10.10.10.0/24", "10.10.10.10", "10.10.10.50"); err != nil {
		t.Fatalf("registering the subnet: %s", err.Error())
	}

	claims, err := c.protectPersistedClaims(pool)
	if err != nil {
		t.Fatalf("protectPersistedClaims: %s", err.Error())
	}
	if claims["10.10.10.10"] != "default/vm-a [02:00:00:00:00:01]" {
		t.Errorf("claims = %v, want the pinned owner reference for 10.10.10.10", claims)
	}
	if _, carries := claims["10.10.10.99"]; carries {
		t.Errorf("claims = %v, the exclude entry is rederived from the spec and must not be a claim", claims)
	}

	// a fresh allocation must never hand out the pinned address: the full
	// sweep of the pool range drains everything but the pin (the range
	// holds 41 addresses and one is pinned)
	for i := 0; i < 40; i++ {
		if _, err := c.ipam.GetIP("net-a", ""); err != nil {
			t.Fatalf("allocation %d of the fresh range failed: %s", i+1, err.Error())
		}
	}
	if _, err := c.ipam.GetIP("net-a", ""); err == nil {
		t.Error("the pool must be exhausted after the fresh allocations plus the pin")
	}
	if used := c.ipam.Used("net-a"); used != 41 {
		t.Errorf("used = %d, want 41 (40 fresh allocations and the pinned claim)", used)
	}
	if _, err := c.ipam.ReclaimIP("net-a", "10.10.10.10", "default/vm-b [02:00:00:00:00:09]"); err == nil {
		t.Error("the foreign reclaim of the pinned address must fail")
	}

	// the restoring binding of the recorded owner reclaims idempotently
	if _, err := c.ipam.ReclaimIP("net-a", "10.10.10.10", "default/vm-a [02:00:00:00:00:01]"); err != nil {
		t.Errorf("the own reclaim of the pinned address: %s", err.Error())
	}
}

func TestProtectPersistedClaimsUnparseableRecordProtectsUnconditionally(t *testing.T) {
	stored := ippoolBehaviorNewTestPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		"10.10.10.10": "USED",
	}

	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)
	pool := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := c.ipam.NewSubnet("net-a", "10.10.10.0/24", "10.10.10.10", "10.10.10.50"); err != nil {
		t.Fatalf("registering the subnet: %s", err.Error())
	}

	claims, err := c.protectPersistedClaims(pool)
	if err != nil {
		t.Fatalf("protectPersistedClaims: %s", err.Error())
	}
	if claims["10.10.10.10"] != "USED" {
		t.Errorf("claims = %v, want the original record kept for the republish", claims)
	}

	// the protected address must not be handed out to a fresh allocation
	if _, err := c.ipam.GetIP("net-a", "10.10.10.10"); err == nil {
		t.Error("the unconditionally protected address must not be reissuable")
	}
}

func TestProtectPersistedClaimsFailsOnForeignRecording(t *testing.T) {
	stored := ippoolBehaviorNewTestPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		// a live conflict recorded by another owner: publishing the
		// allocator would silently drop or offer a claimed address
		"10.10.10.10": "default/vm-a [02:00:00:00:00:01]",
	}

	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)
	pool := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := c.ipam.NewSubnet("net-a", "10.10.10.0/24", "10.10.10.10", "10.10.10.50"); err != nil {
		t.Fatalf("registering the subnet: %s", err.Error())
	}

	// a competing plain allocation holds the address first: the pinned
	// claim cannot be honored and the registration must fail loudly
	if _, err := c.ipam.GetIP("net-a", "10.10.10.10"); err != nil {
		t.Fatalf("seeding the competing allocation: %s", err.Error())
	}

	if _, err := c.protectPersistedClaims(pool); err == nil {
		t.Fatal("protectPersistedClaims must fail when a claim fights a live allocation")
	}
}

func TestProtectPersistedClaimsGettingTheStatusFailsTheRegistration(t *testing.T) {
	stored := ippoolBehaviorNewTestPool("pool1", "net-a")

	rs := ippoolBehaviorNewRestState(stored)
	rs.failGet = true
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)
	pool := ippoolBehaviorNewTestPool("pool1", "net-a")
	if err := c.ipam.NewSubnet("net-a", "10.10.10.0/24", "10.10.10.10", "10.10.10.50"); err != nil {
		t.Fatalf("registering the subnet: %s", err.Error())
	}

	if _, err := c.protectPersistedClaims(pool); err == nil {
		t.Fatal("without the persisted status the registration must fail instead of publishing an unprotected allocator")
	}
}

func TestProtectPersistedClaimsRepublishesTheDurableRecords(t *testing.T) {
	stored := ippoolBehaviorNewTestPool("pool1", "net-a")
	stored.Status.LastUpdate = metav1.Now()
	stored.Status.IPv4.Allocated = map[string]string{
		"10.10.10.10": "default/vm-a [02:00:00:00:00:01]",
		"10.10.10.60": "default/vm-x [02:00:00:00:00:0a]",
		"10.10.10.99": "EXCLUDED",
	}
	// 10.10.10.60 lies outside the pool range (start .10, end .50): its pin
	// is skipped and its durable record is republished
	pool := ippoolBehaviorNewTestPool("pool1", "net-a")
	pool.Spec.IPv4Config.Pool.Exclude = []string{"10.10.10.20"}

	rs := ippoolBehaviorNewRestState(stored)
	srv := httptest.NewServer(rs.ippoolBehaviorHandler())
	defer srv.Close()

	c, _, _, _, _ := ippoolBehaviorNewTestController(t, srv)
	if err := c.ipam.NewSubnet("net-a", "10.10.10.0/24", "10.10.10.10", "10.10.10.50"); err != nil {
		t.Fatalf("registering the subnet: %s", err.Error())
	}

	claims, err := c.protectPersistedClaims(pool)
	if err != nil {
		t.Fatalf("protectPersistedClaims: %s", err.Error())
	}

	uPool, err := c.resetIPPoolStatus(pool, claims)
	if err != nil {
		t.Fatalf("resetIPPoolStatus: %s", err.Error())
	}

	if got := uPool.Status.IPv4.Allocated["10.10.10.10"]; got != "default/vm-a [02:00:00:00:00:01]" {
		t.Errorf("pinned claim record = %q, want the owner reference", got)
	}
	if got := uPool.Status.IPv4.Allocated["10.10.10.60"]; got != "default/vm-x [02:00:00:00:00:0a]" {
		t.Errorf("out-of-range record = %q, want the durable record kept", got)
	}
	if got := uPool.Status.IPv4.Allocated["10.10.10.20"]; got != "EXCLUDED" {
		t.Errorf("exclude record = %q, want the rederived exclude", got)
	}
	if _, carries := uPool.Status.IPv4.Allocated["10.10.10.99"]; carries {
		t.Errorf("the exclude record of the previous registration must not survive: %v", uPool.Status.IPv4.Allocated)
	}
	if uPool.Status.IPv4.Used != 1 {
		t.Errorf("used = %d, want 1 (only the pinned claim)", uPool.Status.IPv4.Used)
	}
}
