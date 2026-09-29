package dhcp

// F08 regression tests: the ntp resolution of a pool registration must be
// bounded and cancelable. the resolution used to run on
// context.Background(), so a blackholing resolver held the single ippool
// worker - and with it the era join of a shutdown - forever, and a
// canceled era still published whatever the resolution had produced so
// far. the registration now runs under the caller's context with one
// aggregate budget: cancellation and budget exhaustion abort it before
// any state is taken, so nothing partial is published and the hang is
// bounded.

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// parkingResolver returns a resolver whose dial parks until its context
// is done or the release channel closes, mirroring a resolver which
// honors cancellation. entered closes once a lookup started.
func parkingResolver(entered chan struct{}, release chan struct{}) *net.Resolver {
	var once sync.Once

	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			once.Do(func() { close(entered) })
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return nil, errors.New("the parked resolver was released without a connection")
			}
		},
	}
}

// boundedAddPoolResult waits for the AddPool result with a hard test
// deadline, so a registration which ignores its cancellation or budget
// fails the test instead of hanging the suite.
func boundedAddPoolResult(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("AddPool did not return although its context was canceled or its budget exhausted")

		return nil
	}
}

// TestAddPoolAbortsTheResolutionWhenTheEraIsCanceled pins the F08
// cancellation semantics: the registration runs under the caller's
// context, so canceling it aborts the resolution and publishes nothing -
// not even the entries which already resolved.
func TestAddPoolAbortsTheResolutionWhenTheEraIsCanceled(t *testing.T) {
	t.Run("a canceled registration publishes no pool", func(t *testing.T) {
		a := NewDHCPAllocator()
		entered := make(chan struct{})
		release := make(chan struct{})
		a.SetResolver(parkingResolver(entered, release))

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- a.AddPool(ctx, "net-cancel", "192.168.0.1", "255.255.255.0", "192.168.0.254", nil, "", nil, []string{"cancel-ntp.invalid"}, 60, "eth0")
		}()
		<-entered
		cancel()

		err := boundedAddPoolResult(t, done)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("AddPool on a canceled era = %v, want the propagated context.Canceled", err)
		}
		if a.CheckPool("net-cancel") {
			t.Error("the canceled registration published a pool, want no state taken")
		}
		close(release)
	})

	t.Run("a canceled replacement keeps the serving pool", func(t *testing.T) {
		a := newTestPooledAllocator(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		a.SetResolver(parkingResolver(entered, release))

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- a.AddPool(ctx, "pool1", "192.168.0.1", "255.255.255.0", "192.168.0.254", []string{"1.1.1.1", "8.8.8.8"}, "example.com", []string{"example.com"}, []string{"replacement-ntp.invalid"}, 120, "eth0")
		}()
		<-entered
		cancel()

		err := boundedAddPoolResult(t, done)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("AddPool on a canceled era = %v, want the propagated context.Canceled", err)
		}
		// the serving pool must keep its registered options: cancellation
		// is not a failed lookup whose partial result may be published
		pool := a.GetPool("pool1")
		if pool.LeaseTime != 3600 {
			t.Errorf("serving lease time = %d after the canceled replacement, want the old 3600", pool.LeaseTime)
		}
		close(release)
	})
}

// TestAddPoolResolutionBudgetBoundsTheHang pins the F08 aggregate budget:
// even against a resolver which never answers, the registration returns
// within the budget and publishes nothing.
func TestAddPoolResolutionBudgetBoundsTheHang(t *testing.T) {
	oldBudget := ntpResolutionBudget
	ntpResolutionBudget = 100 * time.Millisecond
	t.Cleanup(func() { ntpResolutionBudget = oldBudget })

	a := NewDHCPAllocator()
	entered := make(chan struct{})
	release := make(chan struct{})
	a.SetResolver(parkingResolver(entered, release))

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- a.AddPool(context.Background(), "net-budget", "192.168.0.1", "255.255.255.0", "192.168.0.254", nil, "", nil, []string{"hanging-ntp.invalid"}, 60, "eth0")
	}()
	<-entered

	err := boundedAddPoolResult(t, done)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AddPool against a parked resolver = %v, want the budget's DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the resolution returned after %s, want it bounded near the budget", elapsed)
	}
	if a.CheckPool("net-budget") {
		t.Error("the budget-exhausted registration published a pool, want no state taken")
	}
	close(release)
}
