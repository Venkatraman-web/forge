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

func NewService(db *sql.DB, client *Client) *Service { return &Service{db: db, client: client} }

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

// applyPaid marks the order paid and writes its one outbox event in the same
// transaction: either the order is paid and the event exists, or neither is
// committed. Stock was already deducted at checkout; it is only marked committed.
func (s *Service) applyPaid(ctx context.Context, orderID, attemptID string) (applied bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE orders SET status = 'paid', inventory_status = 'committed', paid_attempt_id = $2, updated_at = now()
		WHERE id = $1 AND status = 'pending_payment' AND inventory_status = 'reserved'`, orderID, attemptID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if err := outbox.InsertPaidOrderEvent(ctx, tx, orderID, attemptID); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	tx.Rollback()
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

// applyDeclined moves the order to payment_failed and returns its reserved
// stock, in one transaction. The state change only matches once, so the stock is
// returned exactly once.
func (s *Service) applyDeclined(ctx context.Context, orderID string) (applied bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE orders SET status = 'payment_failed', inventory_status = 'released', updated_at = now()
		WHERE id = $1 AND status = 'pending_payment' AND inventory_status = 'reserved'`, orderID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if err := releaseStock(ctx, tx, orderID); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	tx.Rollback()
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = $1`, orderID).Scan(&status); err != nil {
		return false, err
	}
	if status == "payment_failed" {
		return false, nil
	}
	return false, errOrderConflict
}

// applyCancelled moves an expired, unpaid order to cancelled and returns its reserved stock,
// in one transaction (like applyDeclined; the state change only matches once).
func (s *Service) applyCancelled(ctx context.Context, orderID string) (applied bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE orders SET status = 'cancelled', inventory_status = 'released', updated_at = now()
		WHERE id = $1 AND status = 'pending_payment' AND inventory_status = 'reserved'`, orderID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if err := releaseStock(ctx, tx, orderID); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	tx.Rollback()
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = $1`, orderID).Scan(&status); err != nil {
		return false, err
	}
	if status == "cancelled" {
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

// releaseStock returns the order's reserved quantities to inventory. It runs
// in the same transaction as the state change that moved the order from
// reserved to released. Products are updated in SKU order, the same order
// checkout locks them in, to avoid lock-order deadlocks.
func releaseStock(ctx context.Context, tx *sql.Tx, orderID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT product_id, quantity FROM order_items WHERE order_id = $1 ORDER BY sku`, orderID)
	if err != nil {
		return err
	}
	type line struct {
		productID int64
		qty       int
	}
	var lines []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.productID, &l.qty); err != nil {
			rows.Close()
			return err
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if len(lines) == 0 {
		return errors.New("order has no items to release")
	}
	for _, l := range lines {
		res, err := tx.ExecContext(ctx, `UPDATE products SET inventory_quantity = inventory_quantity + $2, updated_at = now() WHERE id = $1`, l.productID, l.qty)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("product row missing during stock release")
		}
	}
	return nil
}
