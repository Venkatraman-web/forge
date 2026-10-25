package workerruntime

import (
	"encoding/json"
	"fmt"

	"github.com/Venkatraman-web/forge/sdk/workerclient"
)

// Job is what a handler sees. It deliberately has no claim generation and no
// worker ID: those are the runtime's proof of ownership and a handler has no
// business with them.
type Job struct {
	// ID is stable across all attempts of the job. Use it as the idempotency key
	// for side effects, because a job can run more than once.
	ID string
	// Type is the job type the handler was registered for.
	Type string
	// Input is the job's input exactly as submitted: any valid JSON. The slice
	// belongs to this Job; changing it does not affect the runtime.
	Input json.RawMessage
	// Attempt is 1 for the first execution.
	Attempt int
	// MaxAttempts is the job's attempt budget.
	MaxAttempts int
}

// Decode unmarshals the job's input into v (a pointer), like json.Unmarshal.
// Unknown fields are ignored; use a json.Decoder on Input for strict decoding.
// The returned error describes the mismatch between the input and v without
// quoting the input. A handler that gets a Decode error will usually return
// Permanent(err), since the same input will not decode on a retry.
func (j Job) Decode(v any) error {
	if err := json.Unmarshal(j.Input, v); err != nil {
		return fmt.Errorf("workerruntime: job %s: input does not match the expected shape: %w", j.ID, err)
	}
	return nil
}

// newJob builds the handler's view of a claim. It copies the input so the
// handler cannot alter the runtime's copy, and drops the ownership fields,
// which the runtime keeps separately (in the workerclient.ClaimedJob).
func newJob(c workerclient.ClaimedJob) Job {
	in := make(json.RawMessage, len(c.Input))
	copy(in, c.Input)
	return Job{ID: c.JobID, Type: c.Type, Input: in, Attempt: c.Attempt, MaxAttempts: c.MaxAttempts}
}

// permanentError marks an error that retrying cannot fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as a failure that retrying cannot fix, for example input
// that can never be processed. Forge then fails the job at once instead of
// scheduling another attempt. Any error not marked this way is treated as
// possibly transient and retried while attempts remain. Permanent(nil) is nil.
// The wrapper is transparent to errors.Is and errors.As. Only the mark is used:
// the error's text is never sent to Forge or logged.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// isPermanent reports whether err, or any error it wraps, was marked with
// Permanent.
func isPermanent(err error) bool {
	for err != nil {
		if _, ok := err.(permanentError); ok {
			return true
		}
		switch x := err.(type) {
		case interface{ Unwrap() error }:
			err = x.Unwrap()
		case interface{ Unwrap() []error }:
			for _, e := range x.Unwrap() {
				if isPermanent(e) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}
