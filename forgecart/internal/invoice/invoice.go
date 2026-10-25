// Package invoice implements the invoice.generate business job: issue one
// immutable demo invoice (header plus line snapshots) for a paid order, exactly
// once per outbox event. It is not a PDF and not tax-compliant e-invoicing.
package invoice

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

const EventType = "invoice.generate"

type Input = bizjob.Input
type RejectError = bizjob.RejectError

func reject(code string) error { return bizjob.Reject(code) }

// Result is what a successful run reports to Forge: identifiers only (no
// customer details).
type Result struct {
	InvoiceID     string `json:"invoice_id"`
	InvoiceNumber string `json:"invoice_number"`
	OrderID       string `json:"order_id"`
	Duplicate     bool   `json:"duplicate"`
}

type Generator struct{ dbs bizjob.Databases }

func New(dbs bizjob.Databases) *Generator { return &Generator{dbs: dbs} }

// Handler adapts a Generator to the worker runtime.
func Handler(g *Generator) workerruntime.HandlerFunc { return bizjob.Handler(g.Generate) }

var effect = bizjob.Effect[Result]{
	Find: func(ctx context.Context, q bizjob.Queryer, eventID string) (*bizjob.Stored[Result], error) {
		var r Result
		var attemptID string
		err := q.QueryRowContext(ctx, `SELECT id, invoice_number, order_id, payment_attempt_id FROM invoices WHERE event_id = $1`, eventID).
			Scan(&r.InvoiceID, &r.InvoiceNumber, &r.OrderID, &attemptID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return &bizjob.Stored[Result]{Result: r, OrderID: r.OrderID, PaymentAttemptID: attemptID}, err
	},
	Duplicate:    func(r Result) Result { r.Duplicate = true; return r },
	ConflictCode: "conflicts_with_issued_invoice",
	// The invoice number is derived from the order, so a second invoice for an
	// order violates invoices_number_key before invoices_order_key.
	OrderConstraints: []string{"invoices_order_key", "invoices_number_key"},
	OtherEventCode:   "order_already_invoiced_by_other_event",
}

// numberFor derives the invoice number from the order ID, so it is
// deterministic and unique per order (one invoice per order).
func numberFor(orderID string) string {
	return "INV-" + strings.ToUpper(strings.TrimPrefix(orderID, "ord_"))
}

// Generate verifies the event against the shop database and issues the invoice
// in one transaction on the results database.
//
// It is safe to call any number of times, concurrently, for the same event:
// UNIQUE(event_id) lets exactly one insert win, and every other delivery
// reports the existing invoice as a duplicate if and only if it was issued for
// the same order and payment attempt.
func (g *Generator) Generate(ctx context.Context, in Input) (Result, error) {
	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	if res, found, err := effect.Existing(ctx, g.dbs.Results, in); found || err != nil {
		return res, err
	}

	order, lines, err := bizjob.ReadPaidOrder(ctx, g.dbs.Shop, EventType, in)
	if err != nil {
		return Result{}, err
	}
	if order.Total < order.Subtotal {
		return Result{}, reject("order_items_inconsistent")
	}

	var rb [12]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return Result{}, err
	}
	id := "inv_" + hex.EncodeToString(rb[:])
	number := numberFor(in.OrderID)

	tx, err := g.dbs.Results.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	if dup, inserted, err := effect.Insert(ctx, g.dbs.Results, tx, in, `
		INSERT INTO invoices (id, invoice_number, event_id, order_id, payment_attempt_id, customer_email, currency, subtotal_minor, total_minor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (event_id) DO NOTHING`,
		id, number, in.EventID, in.OrderID, in.PaymentAttemptID, order.Email, order.Currency, order.Subtotal, order.Total); !inserted {
		return dup, err
	}

	// Snapshot the lines in SKU order, in the same transaction.
	ins, err := tx.ExecContext(ctx, `
		INSERT INTO invoice_items (invoice_id, line_number, sku, product_name, quantity, unit_price_minor, currency, line_total_minor)
		SELECT $1, row_number() OVER (ORDER BY sku), sku, product_name, quantity, unit_price_minor, $3, line_total_minor
		FROM jsonb_to_recordset($2::jsonb) AS l(sku text, product_name text, quantity int, unit_price_minor bigint, line_total_minor bigint)`,
		id, lines.JSON(), order.Currency)
	if err != nil {
		return Result{}, err
	}
	if n, _ := ins.RowsAffected(); int(n) != len(lines) {
		return Result{}, errors.New("invoice: line snapshot count differs from the verified lines; retry")
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{InvoiceID: id, InvoiceNumber: number, OrderID: in.OrderID}, nil
}
