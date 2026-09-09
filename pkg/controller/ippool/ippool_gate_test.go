package ippool

import (
	"errors"
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
	appStatus := APP_INIT
	var countCurrent int
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, &countCurrent)

	pool := testPool("pool-t", "net-t", 60)

	if _, err := controller.registerIPPool(pool); err == nil {
		t.Fatal("the registration of a pool with a missing bindinterface returned nil, want a transient error")
	}
	if countCurrent != 0 {
		t.Errorf("ippool count = %d, want 0: a transiently failed registration must stay uncounted", countCurrent)
	}

	// the retried attempt must stay able to settle the gate
	if _, err := controller.registerIPPool(pool); err == nil {
		t.Fatal("the retried registration returned nil, want a transient error")
	}
	if countCurrent != 0 {
		t.Errorf("ippool count = %d after the retried attempt, want 0 until the registration settles", countCurrent)
	}
}

// a pool whose projection cannot parse is definitively unregistrable: it
// counts for the startup gate so a broken object does not block the
// controller startup until it is repaired
func TestRegisterIPPoolUnparseableSubnetCountsAsHandledDuringInit(t *testing.T) {
	appStatus := APP_INIT
	var countCurrent int
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, &countCurrent)

	pool := testPool("pool-u", "net-u", 60)
	pool.Spec.IPv4Config.Subnet = "192.168.1.0/33"

	_, err := controller.registerIPPool(pool)
	if err == nil {
		t.Fatal("the registration of a pool with an unparseable subnet returned nil, want a rejection")
	}
	if !errors.Is(err, ErrPoolUnregistrable) {
		t.Errorf("error = %v, want the ErrPoolUnregistrable classification", err)
	}
	if countCurrent != 1 {
		t.Errorf("ippool count = %d, want 1: a definitively rejected pool must count as handled", countCurrent)
	}
}

// once the gate counted a settled registration, later attempts of the
// same pool must not double count even if they fail
func TestRegisterIPPoolSettledCountIsIdempotent(t *testing.T) {
	appStatus := APP_INIT
	var countCurrent int
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, &countCurrent)

	pool := testPool("pool-v", "net-v", 60)
	pool.Spec.IPv4Config.Subnet = "192.168.1.0/33"

	if _, err := controller.registerIPPool(pool); err == nil {
		t.Fatal("want the definitive rejection of the unparseable subnet")
	}
	if countCurrent != 1 {
		t.Fatalf("ippool count = %d after the settled rejection, want 1", countCurrent)
	}

	// the requeued event repeats the rejected attempt: the gate keeps its
	// single count for this pool
	if _, err := controller.registerIPPool(pool); err == nil {
		t.Fatal("want the repeated rejection")
	}
	if countCurrent != 1 {
		t.Errorf("ippool count = %d after the repeated attempt, want 1", countCurrent)
	}
}
