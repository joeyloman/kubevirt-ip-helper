package ippool

// F07 regression tests: a reloadable option update must never remove the
// last usable serving configuration before its replacement is published.
// the reload used to delete the live dhcp pool before AddPool resolved the
// replacement's ntp hostnames, so every request of that window found a
// pool-absent registry and was nacked - a valid renewal needlessly lost
// its still-valid address. the replacement must be built and published
// atomically: the live pool keeps answering until the swap, and a
// replacement which AddPool rejects leaves the serving state untouched.

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

// blockingResolver returns a resolver whose dial parks until release is
// closed and then fails the lookup: the reload under test blocks inside
// the replacement's ntp resolution, which is exactly the window the
// removed delete used to open. entered closes once the resolution started.
func blockingResolver(entered chan struct{}, release chan struct{}) *net.Resolver {
	var once sync.Once

	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			once.Do(func() { close(entered) })
			<-release

			return nil, errors.New("the blocked resolver was released without a connection")
		},
	}
}

// TestReloadKeepsTheServingPoolRegisteredWhileTheReplacementResolves pins
// the F07 serving continuity: while the replacement of a reloadable option
// update is still resolving its ntp hostnames, the network's live pool
// must remain registered with the old options. the pre-fix code deleted
// the pool before the resolution, so the registry was pool-absent for the
// whole resolution and a valid renewal of that window was nacked.
func TestReloadKeepsTheServingPoolRegisteredWhileTheReplacementResolves(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	oldPool := testPool("pool-gap", "net-gap", 60)
	oldPool.Spec.IPv4Config.NTP = []string{"192.168.1.1"}
	newPool := testPool("pool-gap", "net-gap", 120)
	// a hostname entry makes the replacement resolve before it is
	// installed, and the blocked resolver parks the reload there
	newPool.Spec.IPv4Config.NTP = []string{"gap-ntp.invalid"}

	indexer := newTestIndexer()
	if err := indexer.Add(newPool); err != nil {
		t.Fatalf("indexing the updated pool: %v", err)
	}

	controller, cacheAllocator := newTestController(t, newTestQueue(), indexer, nil, &appStatus, nil)
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	// the live registration of the old options, as a serving era holds it
	if err := controller.dhcp.AddPool(
		"net-gap",
		"192.168.1.1",
		"255.255.255.0",
		"192.168.1.1",
		nil,
		"",
		nil,
		[]string{"192.168.1.1"},
		60,
		"test-fake-iface",
	); err != nil {
		t.Fatalf("registering the live dhcp pool: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	controller.dhcp.SetResolver(blockingResolver(entered, release))

	done := make(chan error, 1)
	go func() {
		done <- controller.handleIPPoolObjectChange(*oldPool, newPool)
	}()
	<-entered

	// the replacement is still resolving: the live pool must still be
	// registered with the old options - the pre-fix code had already
	// deleted it at this point, and every request of the window was
	// nacked against the pool-absent registry
	if !controller.dhcp.CheckPool("net-gap") {
		t.Fatal("the reload unregistered the serving pool while the replacement resolves")
	}
	if pool := controller.dhcp.GetPool("net-gap"); pool.LeaseTime != 60 {
		t.Fatalf("serving lease time = %d while the replacement resolves, want the old 60", pool.LeaseTime)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the blocked reload did not converge: %v", err)
	}

	// the replacement is published now: the same network serves the new
	// options and the cache carries the updated object
	if pool := controller.dhcp.GetPool("net-gap"); pool.LeaseTime != 120 {
		t.Errorf("serving lease time = %d after the reload, want the new 120", pool.LeaseTime)
	}
	got, err := cacheAllocator.Get("pool", "net-gap")
	if err != nil {
		t.Fatalf("pool missing from cache: %v", err)
	}
	if leaseTime := got.(kihv1.IPPool).Spec.IPv4Config.LeaseTime; leaseTime != 120 {
		t.Errorf("cache lease time = %d after the reload, want 120", leaseTime)
	}
}

// TestRejectedReplacementKeepsTheServingPool pins the F07 rejection
// continuity: a replacement whose address projection fails AddPool's own
// validation (the backstop behind the up-front admission) must leave the
// previously registered pool serving. the pre-fix code had already
// deleted the live pool when the rejection surfaced - with its delete
// error only logged - so a rejected replacement destroyed the working
// configuration.
func TestRejectedReplacementKeepsTheServingPool(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, nil)
	if err := controller.dhcp.AddPool(
		"net-gap2",
		"192.168.1.1",
		"255.255.255.0",
		"192.168.1.1",
		[]string{"1.1.1.1"},
		"",
		nil,
		nil,
		60,
		"test-fake-iface",
	); err != nil {
		t.Fatalf("registering the live dhcp pool: %v", err)
	}

	// the projection defect passes the subnet checks of the reload but is
	// rejected by AddPool's own validation
	rejected := testPool("pool-gap2", "net-gap2", 120)
	rejected.Spec.IPv4Config.DNS = []string{"dns.example.invalid"}

	if err := controller.createOrUpdateDHCPPool(rejected); err == nil {
		t.Fatal("the reload accepted an invalid dns projection, want the rejection")
	}

	// the rejected replacement must leave the serving configuration
	// untouched: the network keeps its registered pool and its options
	if !controller.dhcp.CheckPool("net-gap2") {
		t.Fatal("the rejected replacement unregistered the serving pool")
	}
	pool := controller.dhcp.GetPool("net-gap2")
	if pool.LeaseTime != 60 {
		t.Errorf("serving lease time = %d after the rejected replacement, want the old 60", pool.LeaseTime)
	}
	if got := pool.DNS; len(got) != 1 || !got[0].Equal(net.ParseIP("1.1.1.1")) {
		t.Errorf("serving dns = %v after the rejected replacement, want [1.1.1.1]", got)
	}
}
