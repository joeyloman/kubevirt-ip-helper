package ippool

// P1-1 regression tests: the startup gate must settle for every pool
// exactly once - a startup-time deletion settles a pool which could never
// register, and an exhausted retry settles a key which keeps failing.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
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

// The NewSubnet call-site classification is the only thing preventing a
// typo'd range from wedging the startup gate forever: a deterministic
// range rejection must come out unregistrable, while the retryable
// already-exists conflict and plain transient errors stay unchanged.
func TestSubnetRegistrationErrorClassification(t *testing.T) {
	cases := []struct {
		name              string
		err               error
		wantUnregistrable bool
		wantText          string
	}{
		{
			name:              "invalid-range",
			err:               fmt.Errorf("start address 10.0.1.1 is not within subnet 10.0.0.0/24 range: %w", ipam.ErrSubnetInvalid),
			wantUnregistrable: true,
			wantText:          "cannot be registered",
		},
		{
			name:     "duplicate-conflict",
			err:      errors.New("network net-x already exists"),
			wantText: "already exists",
		},
		{
			name:     "plain-transient",
			err:      errors.New("boom"),
			wantText: "boom",
		},
	}
	for _, tc := range cases {
		got := subnetRegistrationError("net-x", tc.err)
		if isUnregistrable := errors.Is(got, ErrPoolUnregistrable); isUnregistrable != tc.wantUnregistrable {
			t.Errorf("%s: errors.Is(ErrPoolUnregistrable) = %v, want %v (%v)", tc.name, isUnregistrable, tc.wantUnregistrable, got)
		}
		if !strings.Contains(got.Error(), tc.wantText) {
			t.Errorf("%s: error = %q, want text %q", tc.name, got.Error(), tc.wantText)
		}
		if !strings.Contains(got.Error(), "net-x") {
			t.Errorf("%s: error = %q, want the network name", tc.name, got.Error())
		}
	}
}
