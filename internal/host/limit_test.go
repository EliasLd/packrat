package host_test

import (
	"strings"
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

// TestClassifyLimit pins what a limit error carries: the status, and —
// when a page was served — a bounded body excerpt, since the body is what
// tells a Cloudflare challenge apart from a plain quota message.
func TestClassifyLimit(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		retryAfter string
		body       string
		want       string
	}{
		{name: "plain status", status: 429, body: "", want: "status 429"},
		{
			name:   "status with a served page",
			status: 403,
			body:   "<!DOCTYPE html> <title>Attention Required!</title>",
			want:   `status 403; body: "<!DOCTYPE html> <title>Attention Required!</title>"`,
		},
		{name: "host delay with a page", status: 200, retryAfter: "5", body: "come back soon", want: `status 200; body: "come back soon"`},
		{name: "page text limit carries its proof", status: 200, body: "<p>Too many requests</p>", want: `page suggests rate limiting; body: "<p>Too many requests</p>"`},
		{
			name:   "long bodies truncate",
			status: 403,
			body:   strings.Repeat("a ", 200),
			want:   `status 403; body: "` + strings.Repeat("a ", 80) + `..."`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := host.ClassifyLimit(tt.status, tt.retryAfter, []byte(tt.body))
			if got == nil {
				t.Fatalf("ClassifyLimit() = nil, want detail %q", tt.want)
			}
			if got.Detail != tt.want {
				t.Fatalf("ClassifyLimit() detail = %q, want %q", got.Detail, tt.want)
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
