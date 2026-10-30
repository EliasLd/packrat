package download

import (
	"context"
	"sync"
)

// Gate blocks download work while paused. Pause/Resume are idempotent and
// safe for concurrent use with Wait.
type Gate struct {
	mu sync.Mutex
	// released is a closed channel while work may proceed, and an open
	// one while paused; every waiter selects on the same channel snapshot.
	released chan struct{}
	pauses   uint64 // grows once per engaged pause; callers watch it for changes
}

// NewGate starts unpaused.
func NewGate() *Gate {
	released := make(chan struct{})
	close(released)
	return &Gate{released: released}
}

// Pause blocks all waiters until Resume; pausing twice is a no-op.
func (g *Gate) Pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.released:
		g.released = make(chan struct{})
		g.pauses++
	default:
	}
}

// Resume lets waiters through; resuming an unpaused gate is a no-op.
func (g *Gate) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.released:
	default:
		close(g.released)
	}
}

// Wait blocks while the gate is paused and returns the context error when
// the context ends during the wait.
func (g *Gate) Wait(ctx context.Context) error {
	g.mu.Lock()
	released := g.released
	g.mu.Unlock()
	select {
	case <-released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Paused reports whether the gate currently blocks waiters.
func (g *Gate) Paused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.released:
		return false
	default:
		return true
	}
}

// Pauses reports how many times a pause was engaged. Callers snapshot it
// before a wait and compare afterwards to detect "the gate was paused
// meanwhile" without ever confusing one pause for another.
func (g *Gate) Pauses() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pauses
}
