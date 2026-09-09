package ippool

// P1-1 regression tests: the startup gate must settle for every pool
// exactly once - a startup-time deletion settles a pool which could never
// register, and an exhausted retry settles a key which keeps failing.

import (
	"errors"
	"testing"
)

// A pool deleted during startup - before its registration ever settled -
// must count so the startup gate does not wait for a pool which can never
// register.
func TestSyncDeleteSettlesUnregisteredPoolStartup(t *testing.T) {
	appStatus := APP_INIT
	var countCurrent int
	controller, _ := newTestController(t, newTestQueue(), newTestIndexer(), nil, &appStatus, &countCurrent)

	if err := controller.sync(Event{key: "ippool-x", action: DELETE, poolName: "ippool-x", poolNetworkName: "net-x"}); err != nil {
		t.Fatalf("the delete sync failed: %s", err)
	}
	if countCurrent != 1 {
		t.Errorf("ippool count = %d after the deleted pool, want 1", countCurrent)
	}
}

// A key dropped after the retry threshold must settle the gate: it can
// never settle through its own retries anymore, so the gate waiting for it
// would block the whole controller startup forever.
func TestHandleErrDropSettlesStartupCount(t *testing.T) {
	appStatus := APP_INIT
	var countCurrent int
	queue := newTestQueue()
	controller, _ := newTestController(t, queue, newTestIndexer(), nil, &appStatus, &countCurrent)

	key := "pool-e"
	syncErr := errors.New("persistent failure")

	// the first five failures are rate limited and requeued
	for i := 0; i < 5; i++ {
		queue.Add(Event{key: key, action: ADD, poolName: key})
		item, quit := queue.Get()
		if quit {
			t.Fatalf("queue was shut down while getting the item")
		}
		controller.handleErr(syncErr, item)
		queue.Done(item)
	}
	if countCurrent != 0 {
		t.Fatalf("ippool count = %d after five requeues, want 0", countCurrent)
	}

	// the next failure exceeds the retry threshold: the key is dropped and
	// must settle the startup gate exactly once
	queue.Add(Event{key: key, action: ADD, poolName: key})
	item, quit := queue.Get()
	if quit {
		t.Fatalf("queue was shut down while getting the item")
	}
	controller.handleErr(syncErr, item)
	queue.Done(item)

	if countCurrent != 1 {
		t.Errorf("ippool count = %d after the dropped key, want 1", countCurrent)
	}
}
