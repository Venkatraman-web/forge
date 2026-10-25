package workerclient

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors for errors.Is. An *Error response matches the sentinel for
// its HTTP status; see Error.
var (
	// ErrOwnershipLost: the server answered 409. The database says this claim
	// is not current: another worker or the reaper has taken the job, it
	// already finished, or the worker ID or generation is wrong. The caller
	// must stop work on the claim and make no further calls for it.
	//
	// It is also what a repeated Complete, Fail or Release gets after the first
	// attempt committed but its response was lost. The server cannot tell the
	// two cases apart, and neither can the SDK.
	ErrOwnershipLost = errors.New("forge: claim is no longer current")

	// ErrUnauthorized: the server answered 401 (missing or wrong API key).
	ErrUnauthorized = errors.New("forge: unauthorized")

	// ErrRateLimited: the server answered 429 because too many claim requests
	// were already waiting. Error.RetryAfter holds the server's Retry-After.
	ErrRateLimited = errors.New("forge: too many waiting claim requests")

	// ErrInvalidRequest: the server answered another 4xx (400, 405, 413). The
	// request was rejected before anything changed. It is a bug in the caller.
	ErrInvalidRequest = errors.New("forge: request rejected as invalid")

	// ErrUnavailable: the server answered 5xx, usually 503 because the
	// database could not confirm the operation. The operation may or may not
	// have been applied; see IsUncertain.
	ErrUnavailable = errors.New("forge: service unavailable")

	// ErrMalformedResponse: the server answered, but the status or body did not
	// match the documented contract (wrong shape, unexpected status, redirect,
	// or oversized body). For a request that mutates state, the server may
	// already have applied it; see IsUncertain.
	ErrMalformedResponse = errors.New("forge: unexpected response from server")
)

// Error is an error response from the Forge worker API. Its fields come from
// the server's JSON error body, so treat Message as untrusted text: the SDK
// strips control characters and shortens it, nothing more.
//
// errors.Is(err, ErrOwnershipLost) and the other sentinels match by Status:
// 401 ErrUnauthorized, 409 ErrOwnershipLost, 429 ErrRateLimited, other 4xx
// ErrInvalidRequest, 5xx ErrUnavailable.
type Error struct {
	Status     int           // HTTP status code
	Code       string        // server error code such as "ownership_lost", may be empty
	Message    string        // short server message, may be empty
	RetryAfter time.Duration // from the Retry-After header; zero if absent or not in seconds
	Op         string        // "claim", "renew", "complete", "fail" or "release"
}

func (e *Error) Error() string {
	s := fmt.Sprintf("forge: %s: HTTP %d", e.Op, e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Is lets errors.Is match the sentinel errors above.
func (e *Error) Is(target error) bool {
	switch {
	case target == ErrUnauthorized:
		return e.Status == 401
	case target == ErrOwnershipLost:
		return e.Status == 409
	case target == ErrRateLimited:
		return e.Status == 429
	case target == ErrInvalidRequest:
		return e.Status >= 400 && e.Status < 500 && e.Status != 401 && e.Status != 409 && e.Status != 429
	case target == ErrUnavailable:
		return e.Status >= 500
	}
	return false
}

// TransportError means the request could not be completed: a connection
// failure, a timeout, or cancellation of the context, possibly after the
// server had already received and applied it. Unwrap returns the underlying
// error, so errors.Is(err, context.Canceled) and
// errors.Is(err, context.DeadlineExceeded) work.
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string { return fmt.Sprintf("forge: %s: %v", e.Op, e.Err) }
func (e *TransportError) Unwrap() error { return e.Err }

// responseError reports a response that broke the contract.
type responseError struct {
	op, detail string
}

func (e *responseError) Error() string {
	return fmt.Sprintf("forge: %s: unexpected response: %s", e.op, e.detail)
}
func (e *responseError) Is(target error) bool { return target == ErrMalformedResponse }

// IsUncertain reports whether the outcome of the operation is unknown: the
// server may have applied it even though the call did not return success.
// That is true for transport failures (including timeouts and cancellation
// after the request was sent), for 5xx responses, and for malformed responses.
//
// The SDK never retries by itself and cannot resolve the uncertainty. The
// server does not replay operations: a repeat of Complete, Fail or Release
// that follows a committed original gets ErrOwnershipLost, which looks the
// same as a genuine loss. A caller that gets an uncertain result should treat
// the outcome as unknown and stop acting on the claim, rather than assume the
// operation failed.
//
// Errors raised before anything was sent (invalid arguments) are not
// uncertain.
func IsUncertain(err error) bool {
	var te *TransportError
	if errors.As(err, &te) {
		return true
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Status >= 500
	}
	return errors.Is(err, ErrMalformedResponse)
}

// IsOwnershipLost is shorthand for errors.Is(err, ErrOwnershipLost).
func IsOwnershipLost(err error) bool { return errors.Is(err, ErrOwnershipLost) }
