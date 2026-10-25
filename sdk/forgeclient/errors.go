package forgeclient

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Sentinel errors for errors.Is. An *Error response matches the sentinel for
// its HTTP status and server error code; see Error.Is.
var (
	// ErrQueueFull: Forge's queued-job limit is reached (503 or 429 with the
	// code queue_full). Nothing was stored. Wait at least Error.RetryAfter (add
	// jitter) and submit again; do not count it against a retry budget.
	ErrQueueFull = errors.New("forge: job queue is full")

	// ErrAdmissionBusy: Forge is limiting how many submissions it processes at
	// once (503 or 429 with the code admission_busy). Nothing was stored, and
	// the queue may well have room. Retry after Error.RetryAfter; it is
	// momentary, so only this submission needs to wait.
	ErrAdmissionBusy = errors.New("forge: admission is busy")

	// ErrInvalidOption: an option was rejected (400 invalid_option), for example
	// MaxAttempts or TimeoutSeconds outside what this server allows. Nothing was
	// stored. Retrying the same request will not help.
	ErrInvalidOption = errors.New("forge: invalid job option")

	// ErrIdempotencyConflict: the IdempotencyKey was already used for a job with
	// different content (409 idempotency_key_conflict). Nothing was stored. Use a
	// new key for a different job, or send the original content again.
	ErrIdempotencyConflict = errors.New("forge: idempotency key already used for a different job")

	// ErrUnauthorized: Forge rejected the API key (401 or 403): it is missing,
	// wrong or revoked. Nothing was stored. Retrying the same request will not
	// help; fix the key.
	ErrUnauthorized = errors.New("forge: unauthorized")

	// ErrNotFound: no job has that ID (404).
	ErrNotFound = errors.New("forge: not found")

	// ErrRateLimited: a 429 that is not queue_full or admission_busy.
	ErrRateLimited = errors.New("forge: rate limited")

	// ErrInvalidRequest: another 4xx (400, 405, 413 ...). The request was
	// rejected before anything was stored; retrying it unchanged will not help.
	ErrInvalidRequest = errors.New("forge: request rejected as invalid")

	// ErrUnavailable: a 5xx other than the two backpressure answers, for
	// example a database failure. A submission may or may not have been stored;
	// see IsUncertain.
	ErrUnavailable = errors.New("forge: service unavailable")

	// ErrMalformedResponse: the server answered, but the status or body did not
	// match the documented contract (a redirect, an unexpected status, a body
	// that is not a Forge job, or one that is too large). A submission may
	// already have been stored; see IsUncertain.
	ErrMalformedResponse = errors.New("forge: unexpected response from server")
)

// Error is an HTTP response from the Forge submission API that was not the
// expected success. Code and Message come from the server's JSON error body, so
// treat them as untrusted text: the SDK strips control characters and shortens
// them, nothing more. Status is always the HTTP status that arrived, even for
// a response that broke the contract (then Code is empty and errors.Is matches
// ErrMalformedResponse).
type Error struct {
	Status     int           // HTTP status code
	Code       string        // server error code such as "queue_full"; may be empty
	Message    string        // short server message; may be empty
	RetryAfter time.Duration // from the Retry-After header; zero if absent or invalid
	Op         string        // "submit", "submit_batch", "get" or "list"
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
	backpressure := e.Status == http.StatusServiceUnavailable || e.Status == http.StatusTooManyRequests
	switch target {
	case ErrQueueFull:
		return backpressure && e.Code == "queue_full"
	case ErrAdmissionBusy:
		return backpressure && e.Code == "admission_busy"
	case ErrInvalidOption:
		return e.Status == http.StatusBadRequest && e.Code == "invalid_option"
	case ErrIdempotencyConflict:
		return e.Status == http.StatusConflict && e.Code == "idempotency_key_conflict"
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrRateLimited:
		return e.Status == http.StatusTooManyRequests && e.Code != "queue_full" && e.Code != "admission_busy"
	case ErrInvalidRequest:
		return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusNotFound && e.Status != http.StatusTooManyRequests
	case ErrUnavailable:
		return e.Status >= 500 && e.Status < 600 && e.Code != "queue_full" && e.Code != "admission_busy"
	case ErrMalformedResponse:
		return e.Status < 400 || e.Status >= 600
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

// IsBackpressure reports whether err is one of Forge's two "slow down" answers
// (ErrQueueFull or ErrAdmissionBusy). Both guarantee that nothing was stored.
func IsBackpressure(err error) bool {
	return errors.Is(err, ErrQueueFull) || errors.Is(err, ErrAdmissionBusy)
}

// RetryAfter returns the server's Retry-After hint carried by err, if any.
// It is the minimum wait Forge asks for; add jitter so many callers do not come
// back at the same instant.
func RetryAfter(err error) (time.Duration, bool) {
	var e *Error
	if errors.As(err, &e) && e.RetryAfter > 0 {
		return e.RetryAfter, true
	}
	return 0, false
}

// IsUncertain reports whether a Submit may have stored the job even though it
// returned an error. That is true for transport failures (a timeout or lost
// connection after the request was sent), for 5xx answers other than the two
// backpressure answers, and for responses that broke the contract.
//
// Without an IdempotencyKey a repeat is NOT safe: submitting again after an
// uncertain result may create a second job. With one, repeating the same
// Submit (same key, same content) is safe: it returns the job stored by the
// attempt that got through. SubmitBatch has no key, so after an uncertain batch
// make the handler idempotent on a stable identifier carried in the job input,
// or check with List before repeating.
//
// Definite rejections (backpressure, invalid options or requests, not found)
// and errors raised before anything was sent are not uncertain.
func IsUncertain(err error) bool {
	var te *TransportError
	if errors.As(err, &te) {
		return true
	}
	var e *Error
	if errors.As(err, &e) {
		return errors.Is(e, ErrUnavailable) || errors.Is(e, ErrMalformedResponse)
	}
	return false
}
