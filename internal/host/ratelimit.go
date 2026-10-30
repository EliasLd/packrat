package host

import (
	"errors"
	"fmt"
	"time"
)

// ErrRateLimited reports a host refusing work because too many requests
// arrived: it asks for a pause, not for retries of the same intensity.
// Check for it with errors.Is; read the delay with errors.As through
// RateLimitedError.
var ErrRateLimited = errors.New("host rate limit")

// RateLimitedError carries what a rate-limit response revealed: the delay
// the host asked for (zero when it sent no Retry-After) and a short
// description of the signal that fired.
type RateLimitedError struct {
	RetryAfter time.Duration
	Detail     string
}

func (e *RateLimitedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s (%s): retry after %s", ErrRateLimited, e.Detail, e.RetryAfter)
	}
	return fmt.Sprintf("%s (%s)", ErrRateLimited, e.Detail)
}

// Unwrap ties the type to the sentinel so errors.Is(ErrRateLimited) holds.
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }
