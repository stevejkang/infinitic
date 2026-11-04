package rotator

import (
	"errors"
	"time"

	"github.com/stevejkang/infinitic/internal/anthropic"
)

// The usage endpoints throttle aggressive polling, and a 429 that is retried on every check keeps
// the account throttled. After consecutive failures an account is left alone for a growing while.
const (
	backoffBase        = time.Minute
	maxFailureBackoff  = 15 * time.Minute
	rateLimitProbeBase = 15 * time.Minute // Retry-After often overstates; probe at least this soon
	maxRateLimitWait   = time.Hour
)

// backoffFor is how long to leave an account alone after its streak-th consecutive failure.
//   - other transient failures: 1m, 2m, 4m, ... up to 15m
//   - HTTP 429: at least Retry-After and 2^streak minutes, at most a probe cap of
//     15m, 30m, then 60m, since the endpoint often lifts sooner than it advertises
func backoffFor(err error, streak int) time.Duration {
	growth := backoffBase << min(streak-1, 10)
	var limited *anthropic.RateLimitedError
	if !errors.As(err, &limited) {
		return min(growth, maxFailureBackoff)
	}
	probeCap := min(rateLimitProbeBase<<min(streak-1, 4), maxRateLimitWait)
	return min(max(limited.RetryAfter, 2*growth), probeCap)
}
