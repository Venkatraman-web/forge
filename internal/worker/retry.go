package worker

import (
	"math/rand/v2"
	"time"

	"github.com/Venkatraman-web/forge/internal/jobs"
)

// retryJitter is the +/- fraction applied to each delay, so jobs that failed
// together (for example when a dependency went down) do not all retry at the
// same instant.
const retryJitter = 0.2

// The safe public error for each kind of failed attempt. The text is fixed and
// server-owned: handler error text is untrusted and is never stored.
var (
	infoError   = jobs.ErrorInfo{Code: "execution_failed", Message: "Job execution failed."}
	infoTimeout = jobs.ErrorInfo{Code: "timeout", Message: "Job exceeded the execution time limit."}
	infoPanic   = jobs.ErrorInfo{Code: "execution_failed", Message: "Handler panicked during execution."}
)

// FailureInfo maps a failure kind reported by a remote worker ("error",
// "timeout" or "panic") to the fixed public error. ok is false for any other
// value, so arbitrary text can never become a persisted error.
func FailureInfo(kind string) (info jobs.ErrorInfo, ok bool) {
	switch kind {
	case "error":
		return infoError, true
	case "timeout":
		return infoTimeout, true
	case "panic":
		return infoPanic, true
	}
	return jobs.ErrorInfo{}, false
}

// RetryDelay is the wait before the next attempt after attempt number
// `attempt` (1 for the first execution) failed:
//
//	nominal = min(base * 2^(attempt-1), max)
//	delay   = min(nominal * (1 + j), max),  j uniform in [-0.2, +0.2]
//
// So with the defaults (2s, 5m) the nominal waits are 2s, 4s, 8s, ... and the
// result never exceeds max (once capped, delays are 80%-100% of max). The
// doubling stops at the cap, so a large attempt number cannot overflow.
// math/rand/v2's top-level functions are safe for concurrent use.
func RetryDelay(attempt int, base, max time.Duration) time.Duration {
	nominal := base
	for i := 1; i < attempt && nominal < max; i++ {
		nominal *= 2 // nominal < max before doubling, so this cannot overflow for sane max
	}
	if nominal > max {
		nominal = max
	}
	factor := 1 + retryJitter*(2*rand.Float64()-1)
	d := time.Duration(float64(nominal) * factor)
	if d > max {
		d = max
	}
	if d < 0 {
		d = 0
	}
	return d
}
