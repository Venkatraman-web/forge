// Package bizjob holds what ForgeCart's business workers share: the job input
// contract, the "permanent rejection" error and the adapter that classifies
// errors for the worker runtime.
package bizjob

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

var (
	eventIDPattern   = regexp.MustCompile(`^evt_[0-9a-f]{24}$`)
	orderIDPattern   = regexp.MustCompile(`^ord_[0-9a-f]{24}$`)
	attemptIDPattern = regexp.MustCompile(`^pay_[0-9a-f]{24}$`)
)

// Databases are the two databases a business worker touches. It reads the paid
// order from Shop and records its effect (invoice, fulfillment plan, ...) in
// Results. They are the same pool unless FORGECART_RESULTS_DATABASE_URL is set,
// which moves the workers' writes off the shop database so checkout and the
// workers stop competing for one machine. The read and the write are therefore
// two separate steps, not one transaction: a redelivery is made safe by
// UNIQUE(event_id) on the result row, not by a lock held across both.
type Databases struct {
	Shop, Results *sql.DB
	// Reads, when set, batches the workers' shop reads (see Reader); when nil each
	// job reads on its own. The worker process starts it (Reader.Run).
	Reads *Reader
}

// ReadPaidOrder verifies the event and reads the order and its lines from the shop
// database, through the Reader when there is one.
func (d Databases) ReadPaidOrder(ctx context.Context, eventType string, in Input) (PaidOrder, Lines, error) {
	if d.Reads != nil {
		return d.Reads.read(ctx, eventType, in, true)
	}
	return ReadPaidOrder(ctx, d.Shop, eventType, in)
}

// VerifyPaidOrder is the same without the lines.
func (d Databases) VerifyPaidOrder(ctx context.Context, eventType string, in Input) (PaidOrder, error) {
	if d.Reads != nil {
		o, _, err := d.Reads.read(ctx, eventType, in, false)
		return o, err
	}
	return VerifyPaidOrder(ctx, d.Shop, eventType, in)
}

// Input is the job input the dispatcher submits for every event type.
type Input struct {
	EventID          string `json:"event_id"`
	OrderID          string `json:"order_id"`
	PaymentAttemptID string `json:"payment_attempt_id"`
}

// Validate checks the identifier shapes only.
func (in Input) Validate() error {
	if !eventIDPattern.MatchString(in.EventID) || !orderIDPattern.MatchString(in.OrderID) || !attemptIDPattern.MatchString(in.PaymentAttemptID) {
		return Reject("invalid_input")
	}
	return nil
}

// RejectError means the job can never succeed as submitted (bad input, or data
// that contradicts the event). Retrying will not help. Any other error is a
// temporary infrastructure failure.
type RejectError struct{ Code string }

func (e *RejectError) Error() string { return "rejected: " + e.Code }

func Reject(code string) error { return &RejectError{Code: code} }

// Handler adapts a business function to the worker runtime:
//
//   - malformed input and RejectError are permanent (Forge fails the job at once);
//   - every other error (database down, timeout, cancellation) is returned as is,
//     so Forge retries it, and on shutdown the runtime releases the job.
//
// Error text is never put in the result or logged here.
func Handler[R any](run func(context.Context, Input) (R, error)) workerruntime.HandlerFunc {
	return func(ctx context.Context, job workerruntime.Job) (any, error) {
		var in Input
		dec := json.NewDecoder(bytes.NewReader(job.Input))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, workerruntime.Permanent(Reject("invalid_input"))
		}
		if _, err := dec.Token(); err == nil {
			return nil, workerruntime.Permanent(Reject("invalid_input"))
		}
		res, err := run(ctx, in)
		var re *RejectError
		if errors.As(err, &re) {
			return nil, workerruntime.Permanent(err)
		}
		if err != nil {
			return nil, err
		}
		return res, nil
	}
}

// NewID returns prefix followed by 24 random hexadecimal characters, the shape of
// every ForgeCart identifier ("inv_", "ful_", "ntf_", ...).
func NewID(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
