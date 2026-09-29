package ippool

// F08 regression: the era context must reach the dhcp registration, so a
// reload whose era was lost aborts before any state is taken instead of
// replacing the options of a network the shutdown is fencing - and the
// worker never hangs behind a registration which ignores the era loss.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

func TestCanceledEraReloadKeepsTheServingPool(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_RUNNING)

	oldPool := testPool("pool-cancel", "net-cancel", 60)
	newPool := testPool("pool-cancel", "net-cancel", 120)

	controller, cacheAllocator := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, nil)
	if err := cacheAllocator.Add(oldPool); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	if err := controller.dhcp.AddPool(
		context.Background(),
		"net-cancel",
		"192.168.1.1",
		"255.255.255.0",
		"192.168.1.1",
		nil,
		"",
		nil,
		nil,
		60,
		"test-fake-iface",
	); err != nil {
		t.Fatalf("registering the live dhcp pool: %v", err)
	}

	// the era context is dead (the leadership was lost or a restart was
	// abandoned): the reload must abort on it before any state is taken
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	controller.ctx = canceledCtx

	err := controller.handleIPPoolObjectChange(*oldPool, newPool)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("reload on a canceled era = %v, want the propagated context.Canceled", err)
	}

	// the serving pool keeps its registered options and the cache keeps
	// the previous object: nothing partial was published
	if pool := controller.dhcp.GetPool("net-cancel"); pool.LeaseTime != 60 {
		t.Errorf("serving lease time = %d after the canceled reload, want the old 60", pool.LeaseTime)
	}
	got, getErr := cacheAllocator.Get("pool", "net-cancel")
	if getErr != nil {
		t.Fatalf("pool missing from cache: %v", getErr)
	}
	if leaseTime := got.(kihv1.IPPool).Spec.IPv4Config.LeaseTime; leaseTime != 60 {
		t.Errorf("cache lease time = %d after the canceled reload, want the old 60", leaseTime)
	}
}
