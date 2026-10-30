package fuckingfast

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// defaultRequestInterval keeps the resolver from bursting: no two requests
// through this host start closer than this. A 73-link run spends two
// requests per link, so pacing doubles the resolve phase from seconds to
// roughly its duration in links — polite without being slow.
const defaultRequestInterval = 500 * time.Millisecond

// sleeper waits for d, interruptible through ctx. Split from the clock so
// tests can assert scheduled waits without sleeping.
type sleeper func(ctx context.Context, d time.Duration) error

// requestLimiter serializes requests so the gap between any two starts is
// at least minInterval (with jitter). One Host instance owns one limiter,
// so pacing covers page fetches, download posts, internal retries and the
// engine's relink path alike.
type requestLimiter struct {
	mu          sync.Mutex
	minInterval time.Duration
	next        time.Time // earliest request start not yet granted
	now         func() time.Time
	sleep       sleeper
}

// sleepFor waits d, interruptible through ctx, without adding jitter (the
// callers decide pacing and jitter themselves).
func sleepFor(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func newRequestLimiter() *requestLimiter {
	return &requestLimiter{
		minInterval: defaultRequestInterval,
		now:         time.Now,
		sleep:       sleepFor,
	}
}

// wait grants the caller the right to send one request now or, when a
// request was granted too recently, sleeps until its slot.
func (l *requestLimiter) wait(ctx context.Context) error {
	if l.minInterval <= 0 {
		return nil
	}

	l.mu.Lock()
	start := l.now()
	if start.Before(l.next) {
		start = l.next
	}
	l.next = start.Add(l.gap())
	l.mu.Unlock()

	if d := start.Sub(l.now()); d > 0 {
		return l.sleep(ctx, d)
	}
	return nil
}

// gap is the interval with ±25% jitter: a uniform rhythm is the most
// machine-detectable signal we could send this host.
func (l *requestLimiter) gap() time.Duration {
	base := l.minInterval
	return base - base/4 + time.Duration(rand.Int63n(int64(base/2+1)))
}

// WithMinRequestInterval overrides the pacing between consecutive requests
// through this host. Zero or negative disables pacing (tests, diagnostics).
func WithMinRequestInterval(d time.Duration) Option {
	return func(h *Host) { h.limiter.minInterval = d }
}

// WithClock replaces the limiter's clock (tests).
func WithClock(now func() time.Time) Option {
	return func(h *Host) { h.limiter.now = now }
}

// WithSleep replaces how waits are performed (tests).
func WithSleep(s sleeper) Option { return func(h *Host) { h.limiter.sleep = s } }
