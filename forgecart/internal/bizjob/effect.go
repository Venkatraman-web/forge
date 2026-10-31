package bizjob

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"
)

// Queryer is the part of *sql.DB and *sql.Tx a lookup needs.
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Stored is what a worker's lookup returns for an event that already has an
// effect record: the typed result to hand back, plus the two business
// identifiers that decide whether a redelivery matches it.
type Stored[R any] struct {
	Result           R
	OrderID          string
	PaymentAttemptID string
}

// Effect holds the worker-specific parts of "record this event's effect at most
// once". The control flow around them is the same for every worker:
//
//	Existing  - fast path before any transaction: an event that already has a
//	            record is a duplicate (same order and payment attempt) or a
//	            conflict (anything else).
//	Write     - run the worker's INSERT ... ON CONFLICT (event_id) DO NOTHING as
//	            one statement and classify what happened.
//
// Verification, the SQL, the typed records and the comparison of stored values
// stay in the worker; this type only classifies outcomes.
type Effect[R any] struct {
	// Find returns the record for an event ID, or nil if there is none.
	Find func(ctx context.Context, q Queryer, eventID string) (*Stored[R], error)
	// Duplicate marks a stored result as the answer to a redelivery.
	Duplicate func(R) R
	// ConflictCode is the rejection for an event ID whose stored record belongs
	// to a different order or payment attempt.
	ConflictCode string
	// OrderConstraints are the unique constraints that make an order have at most
	// one effect. PostgreSQL reports whichever violated index it checks first, so
	// list every constraint that covers the order (for example invoices_number_key
	// as well as invoices_order_key, because the number is derived from the order).
	OrderConstraints []string
	// OtherEventCode is the rejection when another event already owns the order.
	OtherEventCode string
}

func (e Effect[R]) resolve(s *Stored[R], in Input) (R, error) {
	if s.OrderID != in.OrderID || s.PaymentAttemptID != in.PaymentAttemptID {
		var zero R
		return zero, Reject(e.ConflictCode)
	}
	return e.Duplicate(s.Result), nil
}

// Existing is the fast path. found reports whether the event already had a
// record, in which case res is the duplicate result or err is the conflict
// rejection; the stored record is never touched. A lookup failure is returned
// as an ordinary (retryable) error.
func (e Effect[R]) Existing(ctx context.Context, q Queryer, in Input) (res R, found bool, err error) {
	s, err := e.Find(ctx, q, in.EventID)
	if err != nil || s == nil {
		return res, false, err
	}
	res, err = e.resolve(s, in)
	return res, true, err
}

// Write runs the worker's write as ONE statement on db, outside any explicit
// transaction (a statement is atomic by itself, so a header row and its item rows
// written by one statement with data-modifying CTEs commit together or not at all;
// that saves the BEGIN and COMMIT round trips). The statement must end in ON
// CONFLICT (event_id) DO NOTHING on the header row and report in its affected-row
// count wantRows when the header was written and 0 when it was not, and Write
// classifies the outcome:
//
//   - wantRows affected: inserted is true;
//   - zero rows: a concurrent delivery of the same event committed first;
//   - a unique violation on one of OrderConstraints: either that concurrent
//     delivery (the record for this event exists) or another event that already
//     owns the order (it does not);
//   - anything else, including a unique violation on any other constraint, is a
//     database error and is returned unchanged, so Forge retries it. It is never
//     turned into a duplicate or a business rejection.
//
// Whenever inserted is false nothing was written, and the record is read back
// through db to decide between duplicate and conflict.
func (e Effect[R]) Write(ctx context.Context, db *sql.DB, in Input, wantRows int64, query string, args ...any) (res R, inserted bool, err error) {
	r, err := db.ExecContext(ctx, query, args...)
	if err == nil {
		n, rerr := r.RowsAffected()
		switch {
		case rerr != nil:
			return res, false, rerr
		case n == wantRows:
			return res, true, nil
		case n != 0:
			return res, false, errors.New("bizjob: write affected an unexpected number of rows")
		}
		return e.afterConflict(ctx, db, in, nil)
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" && slices.Contains(e.OrderConstraints, pe.ConstraintName) {
		return e.afterConflict(ctx, db, in, Reject(e.OtherEventCode))
	}
	return res, false, err
}

// afterConflict reads the record back. If it exists it decides (duplicate or
// conflict). If it does not: with a nil notFound (zero rows inserted) the
// conflicting row vanished, which a retry resolves; otherwise notFound is the
// rejection for "another event owns this order".
func (e Effect[R]) afterConflict(ctx context.Context, db *sql.DB, in Input, notFound error) (res R, inserted bool, err error) {
	s, err := e.Find(ctx, db, in.EventID)
	if err != nil {
		return res, false, err
	}
	if s == nil {
		if notFound == nil {
			notFound = errors.New("bizjob: conflicting insert vanished; retry")
		}
		return res, false, notFound
	}
	res, err = e.resolve(s, in)
	return res, false, err
}
