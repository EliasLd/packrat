package host

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The limit machinery shared by every host resolver lives here: the wait
// seam, the wait policy for rate-limit answers, and the classification of a
// failed exchange into a typed RateLimitedError. Hosts consume it with their
// own policy values so each tunes itself without forking the code.

// Sleeper waits for d, interruptible through ctx. Split from the clock so
// tests can assert scheduled waits without sleeping.
type Sleeper func(ctx context.Context, d time.Duration) error

// SleepFor waits d, interruptible through ctx, without adding jitter (the
// callers decide pacing and jitter themselves).
func SleepFor(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// WaitPolicy decides the pause a rate-limit answer deserves and whether the
// link should be abandoned instead. Waits requested by the host are clamped
// into [Min, Max]; a Retry-After above Max, or GiveUp limiting answers in a
// row inside one Resolve, abandons the link instead of camping on a host
// that asked for minutes.
type WaitPolicy struct {
	Min        time.Duration // floor for any imposed wait
	Max        time.Duration // ceiling; a Retry-After above it gives up
	LadderBase time.Duration // first rung when the host sent no delay
	GiveUp     int           // consecutive limiting answers that abandon a link
}

// DefaultWaitPolicy carries the values tuned against FuckingFast: short
// blips wait at least 2s, at most a minute; unknown delays ladder from 5s;
// three limiting answers in a row give up.
func DefaultWaitPolicy() WaitPolicy {
	return WaitPolicy{
		Min:        2 * time.Second,
		Max:        time.Minute,
		LadderBase: 5 * time.Second,
		GiveUp:     3,
	}
}

// Wait decides the pause after a rate-limit answer and whether the link
// should be abandoned instead. A Retry-After wins over the ladder; a streak
// of limiting answers (attempt failures, not resets) triggers the give-up
// so a long block ends fast and visibly.
func (p WaitPolicy) Wait(retryAfter time.Duration, attempt, streak int) (time.Duration, bool) {
	if retryAfter > p.Max || streak >= p.GiveUp {
		return 0, true
	}
	if retryAfter > 0 {
		return p.Clamp(retryAfter), false
	}
	wait := p.LadderBase
	for i := 1; i < attempt; i++ {
		wait *= 3
	}
	return p.Clamp(wait), false
}

// Clamp pins d into [Min, Max].
func (p WaitPolicy) Clamp(d time.Duration) time.Duration {
	if d < p.Min {
		return p.Min
	}
	if d > p.Max {
		return p.Max
	}
	return d
}

// ParseRetryAfter reads a Retry-After header; only the seconds form is
// supported, HTTP-date responses count as unknown.
func ParseRetryAfter(v string) (time.Duration, bool) {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// limitPhrases are the markers a rate-limit or IP-block page leaves in its
// body even when it answers status 200. Deliberately short and specific,
// so ordinary page copy never reads as limiting.
var limitPhrases = []string{"rate limit", "rate-limit", "ratelimit", "too many requests"}

// hasLimitPhrase reports whether the body looks like a rate-limit page.
func hasLimitPhrase(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, phrase := range limitPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// ClassifyLimit reports a "back off" signal from a failed exchange, or nil
// when nothing about it looks limiting. The response carrying a Retry-After
// header always wins, then the explicit limit statuses, then the body sniff
// as the last resort for app-level limits that answer status 200.
func ClassifyLimit(status int, retryAfter string, body []byte) *RateLimitedError {
	if ra, ok := ParseRetryAfter(retryAfter); ok {
		return &RateLimitedError{RetryAfter: ra, Detail: excerpted(fmt.Sprintf("status %d", status), body)}
	}
	if status == http.StatusTooManyRequests || status == http.StatusForbidden || status == http.StatusServiceUnavailable {
		return &RateLimitedError{Detail: excerpted(fmt.Sprintf("status %d", status), body)}
	}
	if len(body) > 0 && hasLimitPhrase(body) {
		return &RateLimitedError{Detail: excerpted("page suggests rate limiting", body)}
	}
	return nil
}

// excerpted appends a bounded, single-line body excerpt to a limit detail,
// so the error says what the host actually served: block and challenge
// pages name themselves in the head of the document, quota answers in
// their copy. The excerpt stops after 160 bytes, unbalanced on purpose.
func excerpted(detail string, body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if s == "" {
		return detail
	}
	const max = 160
	if len(s) > max {
		s = s[:max] + "..."
	}
	return fmt.Sprintf("%s; body: %q", detail, s)
}
