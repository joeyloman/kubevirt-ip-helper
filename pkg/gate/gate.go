// Package gate tracks the settlement of an exact startup snapshot: the
// application records the keys of the objects its startup LIST saw and the
// controllers settle each key once its reconciliation settled. the gate
// opens only when every key of the snapshot settled, so an object created
// after the snapshot can never substitute for an unvisited pre-existing
// one (a count-based gate opens on cardinality alone: a post-snapshot
// object increments the count and lets the startup proceed while a
// pre-existing object was never visited, its durable state unprotected).
package gate

import "sync"

type Gate struct {
	mu      sync.Mutex
	target  map[string]struct{}
	settled map[string]struct{}
}

// New returns a gate with an empty snapshot: it stays closed until
// SetTarget records the startup snapshot (an empty snapshot opens
// immediately, like a zero target of a count-based gate).
func New() *Gate {
	return &Gate{
		target:  make(map[string]struct{}),
		settled: make(map[string]struct{}),
	}
}

// SetTarget records the startup snapshot: exactly these keys must settle
// before the gate opens. it is called once, after the startup LIST
// succeeded and before the event listeners start.
func (g *Gate) SetTarget(keys []string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.target = make(map[string]struct{}, len(keys))

	for _, key := range keys {
		g.target[key] = struct{}{}
	}
}

// Settle records one settled key. it is idempotent, and a key outside the
// snapshot is recorded but never lets the gate open: a post-snapshot
// object cannot substitute for an unvisited snapshot object.
func (g *Gate) Settle(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.settled[key] = struct{}{}
}

// Open reports whether every key of the startup snapshot settled.
func (g *Gate) Open() bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	for key := range g.target {
		if _, ok := g.settled[key]; !ok {
			return false
		}
	}

	return true
}

// Settled returns how many snapshot keys settled: the progress number of
// the startup logs.
func (g *Gate) Settled() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	count := 0

	for key := range g.target {
		if _, ok := g.settled[key]; ok {
			count++
		}
	}

	return count
}

// Target returns the size of the startup snapshot.
func (g *Gate) Target() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.target)
}

// Unsettled returns the snapshot keys which have not settled yet: the
// controller startup reconcile settles the ones whose object the informer
// never observed (deleted between the startup LIST and the informer
// start, so no event will ever settle them).
func (g *Gate) Unsettled() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	keys := make([]string, 0, len(g.target))

	for key := range g.target {
		if _, ok := g.settled[key]; !ok {
			keys = append(keys, key)
		}
	}

	return keys
}
