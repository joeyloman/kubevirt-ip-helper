package ippool

import (
	"errors"
	"sync/atomic"
	"testing"
)

// The startup gate must only count a pool once its registration attempt
// settled: counting the attempt itself would open the gate while the pool
// is not live yet, so the vmnetcfg controller could restore bindings into
// an unregistered network and new allocations could take over addresses
// whose reservations were never rebuilt.

// a registration which failed transiently (here: the bindinterface does
// not exist on the host) must not count for the startup gate: the
// requeued or resynced attempt counts once it settles instead
func TestRegisterIPPoolTransientFailureStaysUncountedDuringInit(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-t")
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate)

	pool := testPool("pool-t", "infra/net-t", 60)
	controller.scope = testNetworkScope(pool.Spec.NetworkName)
	if err := controller.indexer.Add(pool); err != nil {
		t.Fatal(err)
	}

	if _, err := controller.registerIPPool(pool); err == nil {
		t.Fatal("the registration of a pool with a missing bindinterface returned nil, want a transient error")
	}
	if startupGate.Settled() != 0 {
		t.Errorf("ippool count = %d, want 0: a transiently failed registration must stay uncounted", startupGate.Settled())
	}

	// the retried attempt must stay able to settle the gate
	if _, err := controller.registerIPPool(pool); err == nil {
		t.Fatal("the retried registration returned nil, want a transient error")
	}
	if startupGate.Settled() != 0 {
		t.Errorf("ippool count = %d after the retried attempt, want 0 until the registration settles", startupGate.Settled())
	}
}

// a pool whose projection cannot parse is definitively unregistrable: it
// counts for the startup gate so a broken object does not block the
// controller startup until it is repaired
func TestRegisterIPPoolUnparseableSubnetCountsAsHandledDuringInit(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-u")
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate)

	pool := testPool("pool-u", "infra/net-u", 60)
	controller.scope = testNetworkScope(pool.Spec.NetworkName)
	pool.Spec.IPv4Config.Subnet = "192.168.1.0/33"

	_, err := controller.registerIPPool(pool)
	if err == nil {
		t.Fatal("the registration of a pool with an unparseable subnet returned nil, want a rejection")
	}
	if !errors.Is(err, ErrPoolUnregistrable) {
		t.Errorf("error = %v, want the ErrPoolUnregistrable classification", err)
	}
	if startupGate.Settled() != 1 {
		t.Errorf("ippool count = %d, want 1: a definitively rejected pool must count as handled", startupGate.Settled())
	}
}

// once the gate counted a settled registration, later attempts of the
// same pool must not double count even if they fail
func TestRegisterIPPoolSettledCountIsIdempotent(t *testing.T) {
	var appStatus atomic.Int32
	appStatus.Store(APP_INIT)
	startupGate := newTestGate("pool-v")
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, startupGate)

	pool := testPool("pool-v", "infra/net-v", 60)
	controller.scope = testNetworkScope(pool.Spec.NetworkName)
	pool.Spec.IPv4Config.Subnet = "192.168.1.0/33"

	if _, err := controller.registerIPPool(pool); err == nil {
		t.Fatal("want the definitive rejection of the unparseable subnet")
	}
	if startupGate.Settled() != 1 {
		t.Fatalf("ippool count = %d after the settled rejection, want 1", startupGate.Settled())
	}

	// the requeued event repeats the rejected attempt: the gate keeps its
	// single count for this pool
	if _, err := controller.registerIPPool(pool); err == nil {
		t.Fatal("want the repeated rejection")
	}
	if startupGate.Settled() != 1 {
		t.Errorf("ippool count = %d after the repeated attempt, want 1", startupGate.Settled())
	}
}
