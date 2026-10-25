package workerruntime

import (
	"errors"
	"math/rand/v2"
	"time"

	"github.com/Venkatraman-web/forge/sdk/workerclient"
)

// Claim-error policy. A slot that cannot obtain a job decides, from the typed
// error the client returned, between three outcomes:
//
//	shutdown   the runtime's context is done: the slot stops quietly; this is
//	           not an operational failure (handled by the caller, not here).
//	fatal      repeating the request cannot help, so the worker stops:
//	             ErrUnauthorized         wrong or revoked key
//	             ErrInvalidRequest       any other 4xx: wrong URL (404), a job
//	                                     type the server does not enable (400),
//	                                     an unsupported method or size
//	             ErrMalformedResponse    the server answered, but not with the
//	                                     documented claim contract (wrong URL,
//	                                     proxy page, incompatible version)
//	             any other error         the client rejected the arguments
//	                                     before sending anything: a bug
//	transient  wait, then claim again (no limit on the number of retries; the
//	           delay is bounded and shutdown interrupts it):
//	             ErrRateLimited          the server asked us to slow down
//	             ErrUnavailable          5xx, typically the database
//	             *TransportError         connection failure or client timeout
//
// Ambiguous outcomes. A claim request that failed with a 5xx, a transport
// error or an unreadable 200 may still have committed in Forge: a job can be
// running under this worker without the worker knowing. Nothing renews such a
// claim, so Forge's reaper recovers it when the lease expires and the attempt
// stays used. Backing off and claiming again is therefore safe (it cannot
// double-run anything) but not free, and retrying never proves that the first
// request was rolled back. A malformed response is treated as fatal rather than
// retried precisely because each retry could commit, and lose, another claim.
// The backoff paces claims only; outcome reports (Complete, Fail, Release) are
// never retried by this code.

const (
	// claimBackoffBase and claimBackoffMax bound the exponential wait between
	// claim attempts after consecutive transient failures.
	claimBackoffBase = time.Second
	claimBackoffMax  = 30 * time.Second
	// maxRetryAfter is the longest Retry-After the runtime honours. The client
	// already rejects absurd values; anything above this is treated as
	// unusable and the exponential schedule is used instead, so a bad server
	// value cannot park a slot for an hour.
	maxRetryAfter = 5 * time.Minute
	// claimLogEvery limits how often a persistent claim failure is logged per slot.
	claimLogEvery = 30 * time.Second
)

// claimErrorKind says what a slot does about a claim error.
type claimErrorKind int

const (
	claimFatal claimErrorKind = iota
	claimTransient
)

// classifyClaimError applies the policy above to an error from Claim made
// while the runtime was still active. category is a fixed word for logs, and
// retryAfter is the server's Retry-After for a rate limit (zero otherwise).
func classifyClaimError(err error) (kind claimErrorKind, category string, retryAfter time.Duration) {
	var te *workerclient.TransportError
	var se *workerclient.Error
	switch {
	case errors.Is(err, workerclient.ErrUnauthorized):
		return claimFatal, "unauthorized", 0
	case errors.Is(err, workerclient.ErrInvalidRequest):
		return claimFatal, "invalid_request", 0
	case errors.Is(err, workerclient.ErrMalformedResponse):
		return claimFatal, "malformed_response", 0
	case errors.Is(err, workerclient.ErrRateLimited):
		if errors.As(err, &se) {
			retryAfter = se.RetryAfter
		}
		return claimTransient, "rate_limited", retryAfter
	case errors.Is(err, workerclient.ErrUnavailable):
		return claimTransient, "unavailable", 0
	case errors.As(err, &te):
		return claimTransient, "transport", 0
	}
	return claimFatal, "client_error", 0
}

// claimRetryDelay is how long a slot waits after its failures-th consecutive
// transient claim failure (1 for the first).
//
// With a usable Retry-After (0 < d <= maxRetryAfter) the wait is d plus a small
// random extra, so it is never shorter than the server asked and the slots of
// one worker do not all return at the same instant. The extra is up to a
// quarter of d, at least 250 ms and at most 5 s.
//
// Otherwise the wait is exponential: claimBackoffBase doubling per failure up
// to claimBackoffMax, then reduced by random jitter to between half and all of
// that value, so it is never zero and never above the maximum.
func claimRetryDelay(failures int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 && retryAfter <= maxRetryAfter {
		extra := retryAfter / 4
		if extra < 250*time.Millisecond {
			extra = 250 * time.Millisecond
		}
		if extra > 5*time.Second {
			extra = 5 * time.Second
		}
		return retryAfter + time.Duration(rand.Int64N(int64(extra)+1))
	}
	nominal := claimBackoffBase
	for i := 1; i < failures && nominal < claimBackoffMax; i++ {
		nominal *= 2
	}
	if nominal > claimBackoffMax {
		nominal = claimBackoffMax
	}
	half := nominal / 2
	return half + time.Duration(rand.Int64N(int64(nominal-half)+1))
}
