// Package reconcile implements the inventory.reconcile business job: verify
// that a paid order's stock reservation is consistent and record the result
// exactly once per outbox event. It reads orders and never changes stock.
package reconcile

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
)

const EventType = "inventory.reconcile"

// Input is the job input the dispatcher submits (shared by all ForgeCart workers).
type Input = bizjob.Input

// RejectError means the job can never succeed as submitted; see bizjob.
type RejectError = bizjob.RejectError

func reject(code string) error { return bizjob.Reject(code) }

// Result is what a successful run reports to Forge: counts only, no customer data.
type Result struct {
	Outcome      string `json:"outcome"`
	ItemsChecked int    `json:"items_checked"`
	UnitsChecked int    `json:"units_checked"`
	Duplicate    bool   `json:"duplicate"`
}

type Reconciler struct{ dbs bizjob.Databases }

func New(dbs bizjob.Databases) *Reconciler { return &Reconciler{dbs: dbs} }

var effect = bizjob.Effect[Result]{
	Find: func(ctx context.Context, q bizjob.Queryer, eventID string) (*bizjob.Stored[Result], error) {
		var s bizjob.Stored[Result]
		err := q.QueryRowContext(ctx, `SELECT order_id, payment_attempt_id, outcome, items_checked, units_checked FROM inventory_reconciliations WHERE event_id = $1`, eventID).
			Scan(&s.OrderID, &s.PaymentAttemptID, &s.Result.Outcome, &s.Result.ItemsChecked, &s.Result.UnitsChecked)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return &s, err
	},
	Duplicate:        func(r Result) Result { r.Duplicate = true; return r },
	ConflictCode:     "conflicts_with_recorded_event",
	OrderConstraints: []string{"inventory_reconciliations_order_key"},
	OtherEventCode:   "order_already_reconciled_by_other_event",
}

// Reconcile verifies the order on the shop database (reads only) and records
// the result with one insert on the results database.
//
// It is safe to call any number of times, concurrently, for the same event:
// the primary key on event_id lets exactly one insert win, and every other
// delivery sees that row and reports success if (and only if) it describes the
// same order and payment attempt.
func (r *Reconciler) Reconcile(ctx context.Context, in Input) (Result, error) {
	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	// Fast path for redeliveries: no transaction needed.
	if res, found, err := effect.Existing(ctx, r.dbs.Results, in); found || err != nil {
		return res, err
	}

	order, err := bizjob.VerifyPaidOrder(ctx, r.dbs.Shop, EventType, in)
	if err != nil {
		return Result{}, err
	}
	subtotal := order.Subtotal

	// Inspect the reserved lines. Read-only: stock is neither deducted nor restored.
	var items, units int
	var lines int64
	var badStock int
	err = r.dbs.Shop.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(oi.quantity), 0), COALESCE(sum(oi.line_total_minor), 0),
		       count(*) FILTER (WHERE p.inventory_quantity < 0 OR oi.quantity <= 0)
		FROM order_items oi JOIN products p ON p.id = oi.product_id WHERE oi.order_id = $1`, in.OrderID).Scan(&items, &units, &lines, &badStock)
	if err != nil {
		return Result{}, err
	}
	if items == 0 || lines != subtotal || badStock != 0 {
		return Result{}, reject("order_items_inconsistent")
	}

	tx, err := r.dbs.Results.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	if dup, inserted, err := effect.Insert(ctx, r.dbs.Results, tx, in, `
		INSERT INTO inventory_reconciliations (event_id, order_id, payment_attempt_id, outcome, items_checked, units_checked)
		VALUES ($1, $2, $3, 'verified', $4, $5) ON CONFLICT (event_id) DO NOTHING`,
		in.EventID, in.OrderID, in.PaymentAttemptID, items, units); !inserted {
		return dup, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{Outcome: "verified", ItemsChecked: items, UnitsChecked: units}, nil
}
