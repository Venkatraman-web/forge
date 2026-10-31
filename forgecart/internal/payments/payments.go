// Package payments is the order service's side of payment: it asks the payment
// service to take the money (a separate process with its own database, see
// package paymentsvc) and then applies the answer to the order in one local
// transaction. It holds no payment data of its own.
package payments

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"

	"github.com/Venkatraman-web/forge/forgecart/internal/inventory"
	"github.com/Venkatraman-web/forge/forgecart/internal/orders"
	"github.com/Venkatraman-web/forge/forgecart/internal/outbox"
)

var orderIDPattern = regexp.MustCompile(`^ord_[0-9a-f]{24}$`)

// ValidOrderID reports whether id has the shape of an order ID.
func ValidOrderID(id string) bool { return orderIDPattern.MatchString(id) }

// Result is the public response for a payment attempt. OrderStatus and
// InventoryStatus are the order's state after this call has applied the outcome.
//
// With Razorpay, a payment that is still waiting for the customer is answered
// with Outcome "pending" and a PaymentURL; nothing is recorded yet, so there is
// no attempt ID, attempt number or completion time.
type Result struct {
	OrderID           string    `json:"order_id"`
	AttemptID         string    `json:"payment_attempt_id,omitempty"`
	AttemptNumber     int       `json:"attempt_number,omitempty"`
	Outcome           string    `json:"outcome"`
	ProviderReference *string   `json:"provider_reference,omitempty"`
	PaymentURL        string    `json:"payment_url,omitempty"`
	OrderStatus       string    `json:"order_status"`
	InventoryStatus   string    `json:"inventory_status"`
	CompletedAt       time.Time `json:"completed_at,omitzero"`
	Replayed          bool      `json:"replayed"`
}

type Service struct {
	db     *sql.DB
	inv    *inventory.Store // gives stock back when an order is declined, expired or cancelled
	client *Client
	// OnPaymentCall and OnSweep are optional observers (metrics); set them before the
	// service is used.
	OnPaymentCall func(result string, took time.Duration)
	OnSweep       func(repaired int, err error)
	OnExpire      func(expired, repaired int, err error)
}

// callResult names how a call to the payment service ended, for metrics. The set is fixed.
func callResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errFinal):
		return "final"
	case errors.Is(err, errMismatch):
		return "mismatch"
	case errors.Is(err, ErrProvider):
		return "provider_error"
	case errors.Is(err, ErrInterrupted):
		return "interrupted"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	}
	return "rejected"
}

func NewService(db *sql.DB, inv *inventory.Store, client *Client) *Service {
	return &Service{db: db, inv: inv, client: client}
}

func notPayable(status string) *orders.Error {
	return &orders.Error{Kind: orders.KindConflict, Code: "order_not_payable", Message: "The order is not awaiting payment (status: " + status + ")."}
}

var errOrderNotFound = &orders.Error{Kind: orders.KindNotFound, Code: "order_not_found", Message: "No order with that ID."}

// errOrderConflict is a payment the payment service recorded for an order that can
// no longer accept it. It should not happen (the order only leaves pending_payment
// through a payment), so it is reported loudly instead of being papered over.
var errOrderConflict = &orders.Error{Kind: orders.KindConflict, Code: "order_payment_conflict", Message: "The payment could not be applied to the order."}

// Pay performs one logical payment attempt for an order.
//
//  1. Read the order: it must exist, and its amount is what is charged.
//  2. Ask the payment service, with the shopper's idempotency key. The service
//     replays an attempt it already recorded for (order, key), so asking again is
//     always safe.
//  3. Apply the answer to the order in one transaction here: a succeeded payment
//     marks the order paid and writes its outbox event; a declined one releases
//     the stock. Temporary failures and pending payments change nothing.
//
// The order and the payment are in two databases, so a crash between steps 2 and
// 3 leaves a payment recorded and the order unchanged. Repeating the request with
// the same key repeats step 2 (the same answer) and completes step 3; applying an
// outcome twice is harmless because each update only matches a pending order.
func (s *Service) Pay(ctx context.Context, orderID, key string) (Result, error) {
	var status, inv, currency string
	var total int64
	err := s.db.QueryRowContext(ctx, `SELECT status, inventory_status, total_minor, currency FROM orders WHERE id = $1`, orderID).
		Scan(&status, &inv, &total, &currency)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, errOrderNotFound
	}
	if err != nil {
		return Result{}, err
	}

	called := time.Now()
	pr, err := s.client.pay(ctx, key, payRequest{OrderID: orderID, AmountMinor: total, Currency: currency})
	if s.OnPaymentCall != nil {
		s.OnPaymentCall(callResult(err), time.Since(called))
	}
	switch {
	case errors.Is(err, errFinal):
		// Another request finished this order's payment first. Report where the order is now.
		_ = s.db.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = $1`, orderID).Scan(&status)
		return Result{}, notPayable(status)
	case errors.Is(err, errMismatch):
		return Result{}, errOrderConflict
	case err != nil:
		return Result{}, err
	}

	res := Result{OrderID: orderID, AttemptID: pr.AttemptID, AttemptNumber: pr.AttemptNumber, Outcome: pr.Outcome,
		ProviderReference: pr.ProviderReference, PaymentURL: pr.PaymentURL, OrderStatus: status, InventoryStatus: inv,
		CompletedAt: pr.CompletedAt.UTC(), Replayed: pr.Replayed}
	switch pr.Outcome {
	case OutcomeSucceeded:
		_, err = s.applyPaid(ctx, orderID, pr.AttemptID)
		res.OrderStatus, res.InventoryStatus = "paid", "committed"
	case OutcomeDeclined:
		_, err = s.applyDeclined(ctx, orderID)
		res.OrderStatus, res.InventoryStatus = "payment_failed", "released"
	}
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// applyPaid marks the order paid and writes its one outbox event in ONE statement,
// which is atomic by itself: either the order is paid and the event exists, or
// neither is committed. The event is inserted only for the row the update matched.
// Stock was already deducted at checkout; it is only marked committed.
func (s *Service) applyPaid(ctx context.Context, orderID, attemptID string) (applied bool, err error) {
	ev, err := outbox.NewPaidOrderEvent(orderID, attemptID)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `
		WITH paid AS (
			UPDATE orders SET status = 'paid', inventory_status = 'committed', paid_attempt_id = $2, updated_at = now()
			WHERE id = $1 AND status = 'pending_payment' AND inventory_status = 'reserved'
			RETURNING id
		)
		INSERT INTO outbox_events (id, order_id, event_type, payload)
		SELECT $3, paid.id, $4, $5::jsonb FROM paid`, orderID, attemptID, ev.ID, ev.Type, ev.Payload)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	// Not pending: fine if an earlier call (a replay, or a concurrent request with
	// the same key) already applied this very attempt.
	var status string
	var paidBy sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT status, paid_attempt_id FROM orders WHERE id = $1`, orderID).Scan(&status, &paidBy); err != nil {
		return false, err
	}
	if status == "paid" && paidBy.String == attemptID {
		return false, nil
	}
	return false, errOrderConflict
}

// applyDeclined moves the order to payment_failed and gives its reserved stock back.
func (s *Service) applyDeclined(ctx context.Context, orderID string) (applied bool, err error) {
	return s.applyReleased(ctx, orderID, "payment_failed")
}

// applyCancelled moves an expired, unpaid order to cancelled and gives its reserved stock back.
func (s *Service) applyCancelled(ctx context.Context, orderID string) (applied bool, err error) {
	return s.applyReleased(ctx, orderID, "cancelled")
}

// applyReleased moves a pending order to status (payment_failed or cancelled) and then releases its
// reservation in the inventory database. The state change comes first and only matches once; the
// release is idempotent. They are two transactions in two databases (see
// docs/decisions/0005-inventory-service-and-checkout-saga.md): if the release fails, the order is
// already marked and the reaper gives the stock back; and a repeat of this call for an order already in
// the target state tries the release again, so a retry or the sweeper completes it.
func (s *Service) applyReleased(ctx context.Context, orderID, status string) (applied bool, err error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE orders SET status = $2, inventory_status = 'released', updated_at = now()
		WHERE id = $1 AND status = 'pending_payment' AND inventory_status = 'reserved'`, orderID, status)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		_ = s.inv.Release(ctx, orderID)
		return true, nil
	}
	var current string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = $1`, orderID).Scan(&current); err != nil {
		return false, err
	}
	if current == status {
		_ = s.inv.Release(ctx, orderID)
		return false, nil
	}
	return false, errOrderConflict
}

// applyFinal applies a final payment the payment service reports for a pending order:
// succeeded marks it paid, an expiry decline cancels it, any other decline fails it.
func (s *Service) applyFinal(ctx context.Context, orderID, attemptID, outcome string, expired bool) (bool, error) {
	switch {
	case outcome == OutcomeSucceeded:
		return s.applyPaid(ctx, orderID, attemptID)
	case outcome == OutcomeDeclined && expired:
		return s.applyCancelled(ctx, orderID)
	case outcome == OutcomeDeclined:
		return s.applyDeclined(ctx, orderID)
	}
	return false, nil
}
