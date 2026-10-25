package workerruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Venkatraman-web/forge/sdk/workerclient"
)

// cause says why a running handler's context was cancelled, if it was. The
// runtime records it when it cancels, so classification never depends on error
// text or on guessing from the error.
type cause int

const (
	causeNone      cause = iota // not cancelled by the runtime
	causeDeadline               // the execution deadline passed
	causeLost                   // Forge says the claim is no longer ours
	causeSelfFence              // no renewal succeeded within the safety margin
	causeShutdown               // Run's context was cancelled
)

// action is what the runtime must do next about a finished handler.
type action int

const (
	actionNone     action = iota // report nothing: the claim is gone or unprovable
	actionComplete               // workerclient.Complete
	actionFail                   // workerclient.Fail
	actionRelease                // workerclient.Release (cooperative shutdown)
)

// outcome is the pure result of classify.
type outcome struct {
	action  action
	result  json.RawMessage      // actionComplete only; nil means no result
	failure workerclient.Failure // actionFail only
	// reason is a fixed word for logs ("completed", "failed_retryable", ...).
	reason string
}

// classify decides what to report for a handler that has stopped. It is pure:
// no clock, no network, no logging. result and err are what the handler
// returned; panicked is true when the runtime recovered a panic (the panic
// value is never an input, so it can never be leaked); c is why the runtime
// cancelled the handler, if it did.
//
//	lost or self-fenced          -> nothing (even if the handler succeeded)
//	panic                        -> Fail{KindPanic}, retryable
//	nil error, deadline cause    -> Fail{KindTimeout}, retryable (never a success)
//	nil error                    -> Complete(result), or a permanent KindError
//	                                failure if the result cannot be reported
//	Canceled error, shutdown     -> Release
//	Permanent error              -> Fail{KindError, Permanent}
//	any error, deadline cause    -> Fail{KindTimeout}, retryable
//	any other error              -> Fail{KindError}, retryable
func classify(result any, err error, panicked bool, c cause) outcome {
	if c == causeLost || c == causeSelfFence {
		return outcome{action: actionNone, reason: "claim_not_ours"}
	}
	if panicked {
		return outcome{action: actionFail, failure: workerclient.Failure{Kind: workerclient.KindPanic}, reason: "failed_panic"}
	}
	if c == causeDeadline && err == nil {
		// The execution deadline fired, so this attempt overran. A late "success"
		// is not accepted as one: the work may be half-done or duplicated by the
		// retry the server will schedule.
		return outcome{action: actionFail, failure: workerclient.Failure{Kind: workerclient.KindTimeout}, reason: "failed_timeout"}
	}
	if err == nil {
		raw, encErr := encodeResult(result)
		if encErr != nil {
			// A result that cannot be reported is a bug in the handler; sending
			// the same result again would fail the same way.
			return outcome{action: actionFail, failure: workerclient.Failure{Kind: workerclient.KindError, Permanent: true}, reason: "failed_result_invalid"}
		}
		return outcome{action: actionComplete, result: raw, reason: "completed"}
	}
	if c == causeShutdown && errors.Is(err, context.Canceled) {
		return outcome{action: actionRelease, reason: "released_shutdown"}
	}
	switch {
	case isPermanent(err):
		return outcome{action: actionFail, failure: workerclient.Failure{Kind: workerclient.KindError, Permanent: true}, reason: "failed_permanent"}
	case c == causeDeadline:
		return outcome{action: actionFail, failure: workerclient.Failure{Kind: workerclient.KindTimeout}, reason: "failed_timeout"}
	}
	return outcome{action: actionFail, failure: workerclient.Failure{Kind: workerclient.KindError}, reason: "failed_retryable"}
}

var (
	errResultTooLarge  = fmt.Errorf("workerruntime: result is larger than %d bytes", workerclient.MaxResultBytes)
	errResultNUL       = errors.New("workerruntime: result contains U+0000, which Forge cannot store")
	errResultAmbiguous = errors.New("workerruntime: result mixes MarshalJSON and MarshalText values and contains an escaped U+FFFD, so it cannot be verified as unaltered; simplify the result type")
	errResultUTF8      = errors.New("workerruntime: result contains text that is not valid UTF-8, which Forge cannot store unaltered")
)

// encodeResult encodes a handler's return value for workerclient.Complete.
//
// nil, and any value that encodes to JSON null (a nil pointer, map or slice),
// means "no result" and returns nil, matching how the server treats null. Other
// values are encoded with encoding/json (HTML characters are not escaped), so
// objects, arrays, strings, numbers, booleans and nested structures keep their
// meaning. It fails, rather than altering the value, when the value cannot be
// encoded, the encoding exceeds workerclient.MaxResultBytes, or any string or
// object key contains U+0000 (PostgreSQL jsonb cannot store it), or any string
// or key is not valid UTF-8 (encoding/json would silently replace it with
// U+FFFD; see utf8check.go for how this is detected, and its one residual
// limit). Errors never include the offending data.
func encodeResult(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	var scan utf8Scan
	if scan.invalid(reflect.ValueOf(v), 0) {
		return nil, errResultUTF8
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("workerruntime: result cannot be encoded as JSON: %w", err)
	}
	raw := bytes.TrimSpace(buf.Bytes()) // Encode appends a newline
	if bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	// Invalid UTF-8 that came from a MarshalJSON method is copied through as
	// raw bytes; invalid UTF-8 from a TextMarshaler became a \ufffd escape.
	if !utf8.Valid(raw) {
		return nil, errResultUTF8
	}
	if hasFFFDEscape(raw) {
		switch {
		case !scan.sawJSONMarshaler:
			return nil, errResultUTF8 // nothing but the encoder can have written the escape
		case scan.sawTextMarshaler:
			return nil, errResultAmbiguous // see utf8check.go: fail closed
		}
	}
	if len(raw) > workerclient.MaxResultBytes {
		return nil, errResultTooLarge
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("workerruntime: result cannot be encoded as JSON: %w", err)
	}
	if hasNUL(decoded) {
		return nil, errResultNUL
	}
	return json.RawMessage(raw), nil
}

// hasNUL reports whether any string or object key in v contains U+0000.
func hasNUL(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.ContainsRune(x, 0)
	case []any:
		for _, e := range x {
			if hasNUL(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if strings.ContainsRune(k, 0) || hasNUL(e) {
				return true
			}
		}
	}
	return false
}

// --- worker IDs ---------------------------------------------------------

var invalidNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// defaultName turns a host name into a valid Config.Name.
func defaultName(host string) string {
	name := strings.Trim(invalidNameChars.ReplaceAllString(host, "-"), "-")
	if len(name) > MaxNameLength {
		name = strings.Trim(name[:MaxNameLength], "-")
	}
	if name == "" {
		return "worker"
	}
	return name
}

// newInstanceID returns 8 random hex characters from crypto/rand. It is
// chosen once per Worker so that a restarted process (which may reuse a process
// ID and host name) never shares worker IDs with its predecessor.
func newInstanceID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("workerruntime: generate worker instance id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// workerID returns the worker_id for execution slot `slot` (1-based):
// "<name>-<instance>-<slot>". With a name of at most MaxNameLength characters,
// an 8-character instance and a slot up to MaxConcurrency the result is 12 to
// 53 characters from the allowed set, inside the server's 8-64 limit. Each slot
// gets a distinct ID. Uniqueness is for readable logs; the claim generation is
// what actually fences.
func (w *Worker) workerID(slot int) string {
	return fmt.Sprintf("%s-%s-%d", w.name, w.instance, slot)
}
