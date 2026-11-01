package host_test

import (
	"testing"
	"time"

	"github.com/EliasLd/packrat/internal/host"
)

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "seconds", raw: "30", want: 30 * time.Second},
		{name: "padded seconds", raw: " 7 ", want: 7 * time.Second},
		{name: "http date counts as unknown", raw: "Wed, 21 Oct 2026 07:28:00 GMT", want: 0},
		{name: "garbage", raw: "soon", want: 0},
		{name: "negative", raw: "-5", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := host.ParseRetryAfter(tt.raw)
			if got != tt.want {
				t.Fatalf("ParseRetryAfter(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestLimitWaitPolicy(t *testing.T) {
	policy := host.DefaultWaitPolicy()
	tests := []struct {
		name       string
		retryAfter time.Duration
		attempt    int
		streak     int
		want       time.Duration
		giveUp     bool
	}{
		{name: "host delay honored as is", retryAfter: 30 * time.Second, attempt: 1, streak: 1, want: 30 * time.Second},
		{name: "host delay below the floor clamps up", retryAfter: 500 * time.Millisecond, attempt: 1, streak: 1, want: 2 * time.Second},
		{name: "unknown delay ladders by attempt", retryAfter: 0, attempt: 2, streak: 1, want: 15 * time.Second},
		{name: "ladder tops out at the cap", retryAfter: 0, attempt: 9, streak: 1, want: policy.Max},
		{name: "long host delay abandons the link", retryAfter: 5 * time.Minute, attempt: 1, streak: 1, giveUp: true},
		{name: "third limit in a row abandons the link", retryAfter: 0, attempt: 3, streak: 3, giveUp: true},
		{name: "streak dominates any delay", retryAfter: 10 * time.Second, attempt: 3, streak: 4, giveUp: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, giveUp := policy.Wait(tt.retryAfter, tt.attempt, tt.streak)
			if giveUp != tt.giveUp || got != tt.want {
				t.Fatalf("Wait(%v, %d, %d) = (%v, %v), want (%v, %v)",
					tt.retryAfter, tt.attempt, tt.streak, got, giveUp, tt.want, tt.giveUp)
			}
		})
	}
}

// TestDefaultWaitPolicy pins the shared defaults: the FuckingFast-tuned
// values every host starts from, so a refactor cannot silently retune the
// existing resolver.
func TestDefaultWaitPolicy(t *testing.T) {
	policy := host.DefaultWaitPolicy()
	if policy.Min != 2*time.Second {
		t.Fatalf("Min = %v, want 2s", policy.Min)
	}
	if policy.Max != time.Minute {
		t.Fatalf("Max = %v, want 1m", policy.Max)
	}
	if policy.LadderBase != 5*time.Second {
		t.Fatalf("LadderBase = %v, want 5s", policy.LadderBase)
	}
	if policy.GiveUp != 3 {
		t.Fatalf("GiveUp = %d, want 3", policy.GiveUp)
	}
}
