package fuckingfast_test

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/EliasLd/packrat/internal/host/fuckingfast"
)

// paceClock is a frozen clock; the tests assert scheduled waits, not wall
// time, so time never has to move.
func paceClock(now time.Time) func() time.Time { return func() time.Time { return now } }

// recordingSleep stands in for real waiting and records every sleep.
// Emitters call it from concurrent goroutines, so it is locked.
type recordingSleep struct {
	mu    sync.Mutex
	waits []time.Duration
	err   error // returned when set, making a wait fail
}

func (r *recordingSleep) sleep(ctx context.Context, d time.Duration) error {
	if r.err != nil {
		return r.err
	}
	r.mu.Lock()
	r.waits = append(r.waits, d)
	r.mu.Unlock()
	return ctx.Err()
}

// all returns the waits in scheduling order.
func (r *recordingSleep) all() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration{}, r.waits...)
}

func pacedHost(t *testing.T, interval time.Duration, client *fakeClient, s *recordingSleep) *fuckingfast.Host {
	t.Helper()
	h, err := fuckingfast.New(
		fuckingfast.WithHTTPClient(client),
		fuckingfast.WithBaseDelay(time.Millisecond),
		fuckingfast.WithMinRequestInterval(interval),
		fuckingfast.WithClock(paceClock(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))),
		fuckingfast.WithSleep(s.sleep),
	)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return h
}

func TestResolverPacesRequests(t *testing.T) {
	pageURL, err := url.Parse(pageURLText)
	if err != nil {
		t.Fatalf("test url: %v", err)
	}

	t.Run("the first request fires immediately, later ones are spaced", func(t *testing.T) {
		client := happyHandler(t)
		s := &recordingSleep{}
		h := pacedHost(t, 500*time.Millisecond, client, s)

		if _, err := h.Resolve(context.Background(), pageURL); err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		// two requests: immediate start, then one spaced wait; the slot
		// carries the interval with ±25 percent jitter
		waits := s.all()
		if len(waits) != 1 {
			t.Fatalf("waits = %v, want exactly one spaced wait", waits)
		}
		if waits[0] < 375*time.Millisecond || waits[0] > 625*time.Millisecond {
			t.Fatalf("wait = %v, want 500ms ±25%%", waits[0])
		}
	})

	t.Run("successive resolutions share the limiter over one host", func(t *testing.T) {
		client := happyHandler(t)
		s := &recordingSleep{}
		h := pacedHost(t, 500*time.Millisecond, client, s)

		for i := 0; i < 2; i++ {
			if _, err := h.Resolve(context.Background(), pageURL); err != nil {
				t.Fatalf("Resolve() error: %v", err)
			}
		}
		// four requests, three waits: only the very first request is free
		if waits := s.all(); len(waits) != 3 {
			t.Fatalf("waits = %v, want three spaced waits", waits)
		}
	})

	t.Run("spacing is disabled with a zero interval", func(t *testing.T) {
		client := happyHandler(t)
		s := &recordingSleep{}
		h := pacedHost(t, 0, client, s)

		if _, err := h.Resolve(context.Background(), pageURL); err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if waits := s.all(); len(waits) != 0 {
			t.Fatalf("waits = %v, want a disabled limiter to never sleep", waits)
		}
	})

	t.Run("a canceled context stops a pending slot", func(t *testing.T) {
		client := happyHandler(t)
		canceled := &recordingSleep{err: context.Canceled}
		h := pacedHost(t, 500*time.Millisecond, client, canceled)

		_, err := h.Resolve(context.Background(), pageURL)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Resolve() error = %v, want context.Canceled", err)
		}
	})

	t.Run("concurrent resolutions stay spaced without racing", func(t *testing.T) {
		h := pacedHost(t, 500*time.Millisecond, happyHandler(t), &recordingSleep{})

		const workers = 12
		errs := make(chan error, workers)
		for i := 0; i < workers; i++ {
			go func() { errs <- tryResolve(h, pageURL) }()
		}
		for i := 0; i < workers; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("concurrent Resolve() error: %v", err)
			}
		}
	})
}

// tryResolve runs one resolution; used to fan out concurrent checks.
func tryResolve(h *fuckingfast.Host, u *url.URL) error {
	_, err := h.Resolve(context.Background(), u)
	return err
}
