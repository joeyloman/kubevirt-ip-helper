package gate

import "testing"

// the gate opens exactly when every key of the startup snapshot settled:
// a post-snapshot key must never open it (the cardinality substitution of
// a count-based gate), a missing snapshot key must keep it closed, and an
// empty snapshot opens immediately (a startup without objects of the
// gated kind).
func TestGateOpensOnlyOnSnapshotMembership(t *testing.T) {
	g := New()

	if !g.Open() {
		t.Errorf("an empty snapshot must open the gate immediately")
	}

	g.SetTarget([]string{"default/vm-old", "default/vm-new"})

	if g.Open() {
		t.Errorf("a fresh snapshot must keep the gate closed")
	}

	// a post-snapshot object settles first: it must not substitute for
	// the unvisited snapshot objects
	g.Settle("default/vm-created-after-snapshot")

	if g.Open() {
		t.Errorf("a post-snapshot key opened the gate while both snapshot keys are unvisited")
	}
	if got, want := g.Settled(), 0; got != want {
		t.Errorf("settled snapshot keys: got %d, want %d", got, want)
	}

	g.Settle("default/vm-old")

	if g.Open() {
		t.Errorf("one of two snapshot keys settled must keep the gate closed")
	}

	// settling is idempotent: the retry of the same object must not
	// count twice
	g.Settle("default/vm-old")
	g.Settle("default/vm-new")

	if !g.Open() {
		t.Errorf("every snapshot key settled must open the gate")
	}
	if got, want := g.Settled(), 2; got != want {
		t.Errorf("settled snapshot keys: got %d, want %d", got, want)
	}
	if got, want := g.Target(), 2; got != want {
		t.Errorf("snapshot size: got %d, want %d", got, want)
	}
}

// Unsettled reports exactly the snapshot keys which have not settled, so
// the controller startup reconcile can settle the ones whose object the
// informer never observed.
func TestGateUnsettledReportsTheWaitingSnapshotKeys(t *testing.T) {
	g := New()
	g.SetTarget([]string{"pool-a", "pool-b", "pool-c"})

	g.Settle("pool-b")

	unsettled := g.Unsettled()
	if len(unsettled) != 2 {
		t.Fatalf("unsettled snapshot keys: got %v, want the two keys pool-a and pool-c", unsettled)
	}

	seen := map[string]bool{}
	for _, key := range unsettled {
		seen[key] = true
	}
	if !seen["pool-a"] || !seen["pool-c"] {
		t.Errorf("unsettled snapshot keys: got %v, want pool-a and pool-c", unsettled)
	}

	// a settled key outside the snapshot must not leak into the report
	g.Settle("pool-not-in-snapshot")

	if got := len(g.Unsettled()); got != 2 {
		t.Errorf("unsettled snapshot keys after settling a non-snapshot key: got %d, want 2", got)
	}
}
