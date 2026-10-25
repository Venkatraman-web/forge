package bizjob

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Venkatraman-web/forge/forgecart/internal/outbox"
)

// PaidOrder is what the common verification learns about the order.
type PaidOrder struct {
	Email    string // the order's stored customer email: never log or return it
	Currency string
	Subtotal int64
	Total    int64
}

// VerifyPaidOrder performs the checks every ForgeCart business job needs before
// acting on an outbox event. It only reads, from the shop database, with plain
// statements (no transaction and no row lock): a paid order, its payment attempt
// and its event are never changed afterwards, so nothing has to be held still
// while the worker records its result, which may be in another database.
//
//   - the event exists, is the order's paid event (or, for events written before
//     the change to one event per order, has the job's own type), and names the
//     input's order and payment attempt (the input alone is not trusted);
//   - the order exists and is paid with committed inventory;
//   - the order's recorded paying attempt is the input's payment attempt.
//
// Contradictions are *RejectError; database errors are returned unchanged.
func VerifyPaidOrder(ctx context.Context, q Queryer, eventType string, in Input) (PaidOrder, error) {
	var evType, evOrder string
	var evAttempt sql.NullString
	err := q.QueryRowContext(ctx, `SELECT event_type, order_id, payload ->> 'payment_attempt_id' FROM outbox_events WHERE id = $1`, in.EventID).Scan(&evType, &evOrder, &evAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return PaidOrder{}, Reject("unknown_event")
	}
	if err != nil {
		return PaidOrder{}, err
	}
	// An order's event is "order.paid"; events written before that change carry
	// the job's own type, and are still accepted for that job.
	if evType != outbox.OrderPaid && evType != eventType {
		return PaidOrder{}, Reject("wrong_event_type")
	}
	if evOrder != in.OrderID || evAttempt.String != in.PaymentAttemptID {
		return PaidOrder{}, Reject("event_mismatch")
	}

	var status, invStatus string
	var paidBy sql.NullString
	var o PaidOrder
	err = q.QueryRowContext(ctx, `SELECT status, inventory_status, paid_attempt_id, customer_email, currency, subtotal_minor, total_minor FROM orders WHERE id = $1`, in.OrderID).
		Scan(&status, &invStatus, &paidBy, &o.Email, &o.Currency, &o.Subtotal, &o.Total)
	if errors.Is(err, sql.ErrNoRows) {
		return PaidOrder{}, Reject("order_not_found")
	}
	if err != nil {
		return PaidOrder{}, err
	}
	if status != "paid" || invStatus != "committed" {
		return PaidOrder{}, Reject("order_not_paid_and_committed")
	}
	// The order records which payment attempt made it paid (set in the same
	// transaction as the paid state and the event), so the payment service's
	// database is not read here.
	if !paidBy.Valid {
		return PaidOrder{}, Reject("payment_attempt_not_found")
	}
	if paidBy.String != in.PaymentAttemptID {
		return PaidOrder{}, Reject("payment_attempt_mismatch")
	}
	return o, nil
}
