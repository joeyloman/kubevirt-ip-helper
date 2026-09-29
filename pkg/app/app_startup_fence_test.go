package app

// F06 regression tests: the shutdown of a service era must fence the
// serving state first and then join every mutation producer - the
// controller event listeners AND the startup producer which spawns them -
// before the host state is cleaned and the leadership lease is released.
// The startup producer registers itself in the era join before it can
// create any state, so a shutdown which lands while a startup is still
// running (initial or restarted) never observes a zero-count Wait, and no
// startup work runs at all once the era was canceled.
//
// The scenarios mirror the review's acceptance list: a cancellation
// before the first controller listener was spawned (the zero-count Wait),
// the same during a restarted startup (driven through the real client-go
// election of Run), and the no-new-work gate for an era which was already
// dead when RunServices was entered. The live-listener variant of the
// fence (a held listener goroutine blocking the release) is pinned by the
// F05 regressions in app_test.go; the resolver-blocked worker variant
// needs the bounded-DNS seam of the F08 work and stays there.

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
)

// awaitCondition polls cond until it holds or the deadline expires.
func awaitCondition(t *testing.T, cond func() bool, what string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached within the deadline: %s", what)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestOnStoppedLeadingJoinsTheStartupProducerBeforeTheRelease pins the F06
// producer barrier: the stopped-leading shutdown fences the allocator
// while the startup producer is still running, but it must not release
// the lease (or clean the host state) until the producer returned. The
// pre-fix code joined only the listeners the producer had already
// spawned, so a cancellation before the first spawn observed a zero-count
// Wait and released the lease while the producer was still building the
// era behind the teardown.
func TestOnStoppedLeadingJoinsTheStartupProducerBeforeTheRelease(t *testing.T) {
	lock := &fakeElectionLock{record: resourcelock.LeaderElectionRecord{HolderIdentity: "test-leader"}}
	h := fakeElectionHandler(t, lock)
	era := &eraState{appStatus: &atomic.Int32{}, dhcp: dhcp.New()}

	// the startup producer has not spawned a single controller listener
	// yet (its era gather is blocked): without the producer barrier this
	// is exactly the zero-count Wait of the review's scenario
	producerHold := make(chan struct{})
	eraStored := make(chan struct{})
	h.runServices = func(ctx context.Context) error {
		h.era.Store(era)
		close(eraStored)
		<-producerHold

		return nil
	}

	producerCtx, producerCancel := context.WithCancel(context.Background())
	defer producerCancel()
	producerDone := make(chan struct{})
	go func() {
		h.runEraServices(producerCtx)
		close(producerDone)
	}()
	<-eraStored

	stopped := make(chan struct{})
	go func() {
		h.onStoppedLeading()
		close(stopped)
	}()

	// the fence has closed the allocator while the producer is still
	// running, but the producer is joined before the release: no lease
	// read or write may become observable yet
	awaitAllocatorClosed(t, era)
	select {
	case <-producerDone:
		t.Fatal("the startup producer exited while its hold is still closed")
	default:
	}
	lock.mu.Lock()
	gets, updates := lock.gets, lock.updates
	lock.mu.Unlock()
	if gets != 0 || updates != 0 {
		t.Fatalf("lease calls = %d gets / %d updates while the startup producer still runs, want 0/0: the release must join the producer first",
			gets, updates)
	}

	// the producer returns: only now may the shutdown complete and the
	// lease pass to a standby
	close(producerHold)
	<-producerDone
	<-stopped

	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.gets != 1 || lock.updates != 1 {
		t.Errorf("lease calls = %d gets / %d updates after the shutdown, want the single identity-checked release",
			lock.gets, lock.updates)
	}
	if lock.record.HolderIdentity != "" {
		t.Errorf("holder = %q after the shutdown, want cleared for the standby", lock.record.HolderIdentity)
	}
}

// TestRunServicesRunsNoStartupWorkOnACanceledEra pins the second half of
// the F06 fix: once the era context is dead, the startup must not run any
// new work. The leadership may already have been lost - the shutdown path
// fenced the serving state and cleaned the host state - so a canceled
// startup must not even publish its half-built era over the previous one,
// whose teardown already completed.
func TestRunServicesRunsNoStartupWorkOnACanceledEra(t *testing.T) {
	t.Run("a canceled restart does not publish a new era", func(t *testing.T) {
		h := fakeElectionHandler(t, &fakeElectionLock{})
		previous := &eraState{appStatus: &atomic.Int32{}, dhcp: dhcp.New()}
		h.era.Store(previous)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := h.RunServices(ctx); err != nil {
			t.Fatalf("RunServices on a canceled era = %v, want the graceful nil of the canceled-gather contract", err)
		}
		if h.era.Load() != previous {
			t.Fatal("RunServices on a canceled era published a new era, want the previous one untouched after its teardown")
		}
	})

	t.Run("a canceled first startup publishes no era at all", func(t *testing.T) {
		h := fakeElectionHandler(t, &fakeElectionLock{})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := h.RunServices(ctx); err != nil {
			t.Fatalf("RunServices on a canceled era = %v, want the graceful nil of the canceled-gather contract", err)
		}
		if h.era.Load() != nil {
			t.Fatal("RunServices on a canceled era published an era, want none")
		}
	})
}

// TestCanceledRestartStartupJoinsTheProducerBeforeTheRelease drives the
// real client-go election of Run through the review's "repeat during
// restarted startup" scenario: the running era requests an application
// restart, the leading loop fences and joins it, and the leadership is
// lost while the restarted startup is still gathering (before its first
// controller listener was spawned). The stopped-leading shutdown must
// fence the restarted era's allocator first, then join the startup
// producer, and only then release the lease - the pre-fix code released
// the lease while the producer was still building the replacement era.
func TestCanceledRestartStartupJoinsTheProducerBeforeTheRelease(t *testing.T) {
	clearInClusterEnv(t)
	t.Setenv("METRICS_PORT", freeTCPPort(t))

	oldBackoff := restartBackoff
	restartBackoff = 50 * time.Millisecond
	t.Cleanup(func() { restartBackoff = oldBackoff })

	lock := &fakeElectionLock{}
	h := &handler{
		leaderId:       "test-leader",
		kubeConfigFile: filepath.Join(t.TempDir(), "does-not-exist"),
		listenerWg:     &sync.WaitGroup{},
		lock:           lock,
	}

	firstEra := &eraState{appStatus: &atomic.Int32{}, dhcp: dhcp.New()}
	restartEra := &eraState{appStatus: &atomic.Int32{}, dhcp: dhcp.New()}

	// the first era serves; its controller listener flips the era into
	// APP_RESTART when the test triggers it (mirroring the ippool
	// controller's restart request) and is joined by the restart teardown
	// before the rebuild begins
	restartTrigger := make(chan struct{})
	producerHold := make(chan struct{})
	restarted := make(chan struct{})
	var calls atomic.Int32
	h.runServices = func(ctx context.Context) error {
		switch calls.Add(1) {
		case 1:
			h.era.Store(firstEra)
			h.listenerWg.Add(1)
			go func() {
				defer h.listenerWg.Done()
				<-restartTrigger
				firstEra.appStatus.Store(APP_RESTART)
			}()

			return nil
		case 2:
			// the restarted startup: the new era is published (so its
			// allocator is fenceable), no controller listener was spawned
			// yet, and the producer itself blocks in its startup gather
			h.era.Store(restartEra)
			close(restarted)
			<-producerHold

			return nil
		default:
			t.Error("the era was rebuilt after the canceled startup")

			return nil
		}
	}

	mainCtx, cancel := context.WithCancel(context.Background())
	runReturned := make(chan struct{})
	go func() {
		h.Run(mainCtx)
		close(runReturned)
	}()

	// the first era runs, then flips into the restart: the leading loop
	// fences it, joins its listener, backs off and starts the rebuild
	awaitCondition(t, func() bool { return firstEra.appStatus.Load() == APP_RUNNING },
		"the first era to run")
	close(restartTrigger)
	<-restarted

	// the leadership is lost while the restarted startup is still
	// gathering: the shutdown fences the restarted era's allocator
	// first...
	cancel()

	awaitAllocatorClosed(t, restartEra)

	// ...but the lease must keep naming this process while the startup
	// producer is still running: a renewal write preserves the holder,
	// only the release clears it, and the release must wait for the
	// producer join
	time.Sleep(100 * time.Millisecond)
	lock.mu.Lock()
	holder := lock.record.HolderIdentity
	lock.mu.Unlock()
	if holder != "test-leader" {
		t.Fatalf("holder = %q while the restarted startup producer still runs, want this process: the release must join the producer first",
			holder)
	}
	select {
	case <-runReturned:
		t.Fatal("Run returned while the startup producer still runs")
	default:
	}

	// the producer returns: the leading loop exits on the canceled
	// context, the shutdown completes and only then the release clears
	// the holder
	close(producerHold)
	<-runReturned

	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.record.HolderIdentity != "" {
		t.Errorf("holder = %q after the graceful shutdown, want cleared by the explicit release",
			lock.record.HolderIdentity)
	}
	if calls.Load() != 2 {
		t.Errorf("era builds = %d, want exactly two (the initial era and the canceled restart), no startup work after the cancellation",
			calls.Load())
	}
}
