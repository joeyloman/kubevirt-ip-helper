package scheduler

// F14 regression tests: the renewal scheduler reassigned a global ticker
// and recursively started another scheduler on every tick, so each
// renewal leaked its predecessor's goroutine - blocked forever on a
// stopped ticker whose quit channel nobody closes - and short
// certificate lifetimes accumulated waiters quickly. the SIGTERM path
// canceled the process context and exited immediately, interrupting
// in-flight admission requests instead of draining them, and a renewal
// racing the shutdown could restart the admission server under the
// drain.
//
// the loop now runs on the process context, owns one local timer per
// wait, disarms it and returns on cancellation, and restarts the
// admission server only after a successful renewal. the tests drive the
// loop through a manually fired timer factory (the injected clock of
// the review): fifty renewals run without real waiting, the goroutine
// count stays flat, cancellation disarms the pending timer and ends the
// loop, and a tick arriving after the cancellation is inert.

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

// fakeRenewalHandler counts the renewals and fails the first failUntil
// of them on demand.
type fakeRenewalHandler struct {
	mu        sync.Mutex
	expiryErr bool
	expiry    time.Time
	failUntil int
	runCalls  int
}

func (f *fakeRenewalHandler) GetCertExpireDate() (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.expiryErr {
		return time.Time{}, errors.New("the expiry is scripted to be undeterminable")
	}

	return f.expiry, nil
}

func (f *fakeRenewalHandler) Run(certRenewalPeriod int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runCalls++
	if f.runCalls <= f.failUntil {
		return errors.New("the renewal is scripted to fail")
	}

	return nil
}

func (f *fakeRenewalHandler) renewals() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.runCalls
}

// fakeServerHandler records the admission server restarts without
// binding a listener.
type fakeServerHandler struct {
	mu        sync.Mutex
	stopCalls int
	runCalls  int
}

func (f *fakeServerHandler) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stopCalls++

	return nil
}

// timerAt returns the idx-th timer if the loop has created it yet.
func (f *fakeTimerFactory) timerAt(idx int) *fakeTimer {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.timers) > idx {
		return f.timers[idx]
	}

	return nil
}

func (f *fakeServerHandler) Run() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runCalls++
}

func (f *fakeServerHandler) restarts() (stops int, runs int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.stopCalls, f.runCalls
}

// fakeTimer is one manually fired timer of the injected clock.
type fakeTimer struct {
	fire    chan time.Time
	stopped chan struct{}
	once    sync.Once
	delay   time.Duration
}

// fakeTimerFactory hands out the timers the loop waits on. the test
// fires them in order, so many renewals run without real waiting.
type fakeTimerFactory struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

func (f *fakeTimerFactory) newTimer(d time.Duration) (<-chan time.Time, func()) {
	ft := &fakeTimer{
		fire:    make(chan time.Time, 1),
		stopped: make(chan struct{}),
		delay:   d,
	}
	f.mu.Lock()
	f.timers = append(f.timers, ft)
	f.mu.Unlock()

	return ft.fire, func() {
		ft.once.Do(func() { close(ft.stopped) })
	}
}

// waitForTimer returns the idx-th timer the loop created, waiting for
// the loop to create it.
func (f *fakeTimerFactory) waitForTimer(t *testing.T, idx int) *fakeTimer {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.timers)
		var ft *fakeTimer
		if n > idx {
			ft = f.timers[idx]
		}
		f.mu.Unlock()
		if ft != nil {
			return ft
		}
		if time.Now().After(deadline) {
			t.Fatalf("the loop did not create timer %d within 10s (created %d)", idx, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// fireTimer fires the idx-th timer.
func (f *fakeTimerFactory) fireTimer(t *testing.T, idx int) {
	t.Helper()

	ft := f.waitForTimer(t, idx)
	select {
	case ft.fire <- time.Now():
	default:
		t.Fatalf("timer %d was fired twice", idx)
	}
}

// TestRenewalLoopDrivesRenewalsWithoutAccumulating: fifty fired ticks
// drive fifty renewals and fifty listener restarts, and the loop's one
// goroutine is the only worker it owns - the recursive design leaked a
// goroutine and a ticker per tick. cancellation disarms the pending
// timer and ends the loop, and a tick arriving after the cancellation
// is inert: no renewal, no server restart.
func TestRenewalLoopDrivesRenewalsWithoutAccumulating(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cHandler := &fakeRenewalHandler{expiry: time.Now().Add(24 * time.Hour)}
	sHandler := &fakeServerHandler{}
	factory := &fakeTimerFactory{}

	go runRenewalLoop(ctx, cHandler, sHandler, 60, factory.newTimer)

	// the baseline is taken once the loop is parked on its first timer
	factory.waitForTimer(t, 0)
	base := runtime.NumGoroutine()

	for i := range 50 {
		factory.fireTimer(t, i)
	}

	// all fifty renewals ran and the loop parked on the next timer
	deadline := time.Now().Add(10 * time.Second)
	for cHandler.renewals() < 50 || factory.timerAt(50) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the renewals stalled at %d of 50", cHandler.renewals())
		}
		time.Sleep(time.Millisecond)
	}

	after := runtime.NumGoroutine()
	if after > base+5 {
		t.Errorf("the goroutine count grew from %d to %d across 50 renewals, the loop leaks its waiters", base, after)
	}

	stops, runs := sHandler.restarts()
	if runs != 50 || stops != 50 {
		t.Errorf("the listener was stopped %d times and restarted %d times, want 50/50 (one per successful renewal)", stops, runs)
	}

	// cancellation disarms the pending timer and the loop goroutine
	// exits
	pending := factory.waitForTimer(t, 50)
	cancel()
	select {
	case <-pending.stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not disarm its pending timer on cancellation")
	}

	deadline = time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > base+5 {
		if time.Now().After(deadline) {
			t.Fatalf("the loop goroutine did not exit on cancellation (goroutines %d, baseline %d)", runtime.NumGoroutine(), base)
		}
		time.Sleep(time.Millisecond)
	}

	// a tick arriving after the cancellation is inert
	select {
	case pending.fire <- time.Now():
	default:
		t.Fatal("the pending timer channel could not be fed")
	}
	time.Sleep(200 * time.Millisecond)
	if renewals := cHandler.renewals(); renewals != 50 {
		t.Errorf("a renewal ran after the cancellation: %d renewals, want 50", renewals)
	}
	if stops, runs := sHandler.restarts(); runs != 50 || stops != 50 {
		t.Errorf("the server was touched after the cancellation: stops=%d runs=%d, want 50/50", stops, runs)
	}
}

// TestFailedRenewalKeepsTheServerAndTheLoop: a failed renewal keeps the
// serving listener (F13) and the loop stays alive for the retry, which
// the next tick delivers.
func TestFailedRenewalKeepsTheServerAndTheLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cHandler := &fakeRenewalHandler{
		expiry:    time.Now().Add(24 * time.Hour),
		failUntil: 1,
	}
	sHandler := &fakeServerHandler{}
	factory := &fakeTimerFactory{}

	go runRenewalLoop(ctx, cHandler, sHandler, 60, factory.newTimer)

	factory.fireTimer(t, 0)
	deadline := time.Now().Add(10 * time.Second)
	for cHandler.renewals() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the first tick did not run a renewal")
		}
		time.Sleep(time.Millisecond)
	}
	if stops, runs := sHandler.restarts(); stops != 0 || runs != 0 {
		t.Errorf("the failed renewal restarted the server: stops=%d runs=%d, want 0/0", stops, runs)
	}

	// the loop survived the failure: the next tick retries and restarts
	factory.fireTimer(t, 1)
	deadline = time.Now().Add(10 * time.Second)
	for {
		stops, runs := sHandler.restarts()
		if cHandler.renewals() >= 2 && stops >= 1 && runs >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retry did not restart the server: renewals=%d stops=%d runs=%d", cHandler.renewals(), stops, runs)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestUndeterminableExpiryRechecksAtTheMinimumInterval: when the expiry
// cannot be determined (F13) the loop re-checks at the minimum interval
// instead of killing the process, before and after the renewal.
func TestUndeterminableExpiryRechecksAtTheMinimumInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cHandler := &fakeRenewalHandler{expiryErr: true}
	sHandler := &fakeServerHandler{}
	factory := &fakeTimerFactory{}

	go runRenewalLoop(ctx, cHandler, sHandler, 60, factory.newTimer)

	first := factory.waitForTimer(t, 0)
	if first.delay != time.Minute {
		t.Errorf("the first re-check delay = %s, want the minimum interval of 1m", first.delay)
	}

	factory.fireTimer(t, 0)
	second := factory.waitForTimer(t, 1)
	if second.delay != time.Minute {
		t.Errorf("the post-renewal re-check delay = %s, want the minimum interval of 1m", second.delay)
	}
}

// blockingStopServer blocks inside Stop until released, modeling the
// drained server whose drain outlives the shutdown signal.
type blockingStopServer struct {
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	runCalls int
}

func (b *blockingStopServer) Stop() error {
	b.once.Do(func() { close(b.entered) })
	<-b.release

	return nil
}

func (b *blockingStopServer) Run() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.runCalls++
}

func (b *blockingStopServer) runs() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.runCalls
}

// TestShutdownDuringTheRestartDrainDoesNotRelisten: the shutdown signal
// can land while the loop is inside the drain of a successful renewal,
// after the select it left and before the re-listen. the re-listen is
// the one action which must never follow the cancellation: the loop
// re-checks the context between the drain and the restart, so the
// server stays down and no scheduler restart happens after the
// shutdown.
func TestShutdownDuringTheRestartDrainDoesNotRelisten(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cHandler := &fakeRenewalHandler{expiry: time.Now().Add(24 * time.Hour)}
	sHandler := &blockingStopServer{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	factory := &fakeTimerFactory{}

	go runRenewalLoop(ctx, cHandler, sHandler, 60, factory.newTimer)

	factory.fireTimer(t, 0)

	// the loop is now inside the drain of the renewal's restart
	<-sHandler.entered
	cancel()

	// the drain completes after the shutdown: the loop must return at
	// the re-listen guard instead of binding the listener again
	close(sHandler.release)
	time.Sleep(200 * time.Millisecond)
	if runs := sHandler.runs(); runs != 0 {
		t.Errorf("the server was re-listened after the shutdown: %d restarts, want 0", runs)
	}
	if factory.timerAt(1) != nil {
		t.Error("the loop scheduled another wait after the shutdown")
	}
}

// TestRenewalDelayMinutes: the delay comes from the fresh expiry minus
// the renewal period plus the one valid minute, clamped at the minimum
// interval of one minute.
func TestRenewalDelayMinutes(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		expiry time.Time
		period int64
		want   int64
	}{
		{"far future expiry", now.Add(125 * time.Minute), 60, 66},
		{"expiry inside the renewal period", now.Add(30 * time.Minute), 60, 1},
		{"already expired", now, 60, 1},
		{"whole-minute boundary", now.Add(61 * time.Minute), 60, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renewalDelayMinutes(tc.expiry, now, tc.period); got != tc.want {
				t.Errorf("renewalDelayMinutes(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}
