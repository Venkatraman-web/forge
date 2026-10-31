// Package postpay implements the order.process business job: record, for one paid
// order, its invoice, its fulfillment plan and its confirmation notification, exactly
// once per outbox event. It does what the invoice.generate, fulfillment.prepare and
// notification.send jobs do, in one job: one claim and one completion in Forge, one
// read of the order, and (in the usual case) one statement and one commit on the
// results database instead of three of each.
//
// The three effects keep their own tables and their own UNIQUE(event_id) rows, so
// the integrity rules are those of the separate jobs. The combined statement is used
// only when none of the three rows exists yet. Anything else (a duplicate delivery, a
// row left by one of the separate jobs, a conflicting payment attempt, an order that
// another event already owns) is handed to the three separate steps, which are each
// idempotent and classify duplicates and conflicts exactly as before.
package postpay

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/forgecart/internal/fulfillment"
	"github.com/Venkatraman-web/forge/forgecart/internal/invoice"
	"github.com/Venkatraman-web/forge/forgecart/internal/notification"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

const EventType = "order.process"

type Input = bizjob.Input

// Result is what a successful run reports to Forge: identifiers only (no customer
// details).
type Result struct {
	OrderID       string `json:"order_id"`
	InvoiceID     string `json:"invoice_id"`
	InvoiceNumber string `json:"invoice_number"`
	FulfillmentID string `json:"fulfillment_id"`
	DeliveryID    string `json:"delivery_id"`
	Items         int    `json:"items"`
	Duplicate     bool   `json:"duplicate"`
}

type Processor struct {
	dbs bizjob.Databases
	inv *invoice.Generator
	ful *fulfillment.Preparer
	ntf *notification.Notifier
}

func New(dbs bizjob.Databases) *Processor {
	return &Processor{dbs: dbs, inv: invoice.New(dbs), ful: fulfillment.New(dbs), ntf: notification.New(dbs)}
}

// Handler adapts a Processor to the worker runtime.
func Handler(p *Processor) workerruntime.HandlerFunc { return bizjob.Handler(p.Process) }

// writeAll writes the three effects and their line snapshots in ONE statement. A
// statement is atomic by itself: the invoice, the fulfillment plan and the
// notification, with their items, commit together or not at all. Each header row is
// ON CONFLICT (event_id) DO NOTHING, and the statement reports how many rows of each
// kind it wrote.
const writeAll = `
	WITH lines AS (
		SELECT * FROM jsonb_to_recordset($10::jsonb)
		AS l(sku text, product_name text, quantity int, unit_price_minor bigint, line_total_minor bigint)
	), inv AS (
		INSERT INTO invoices (id, invoice_number, event_id, order_id, payment_attempt_id, customer_email, currency, subtotal_minor, total_minor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (event_id) DO NOTHING RETURNING id
	), inv_items AS (
		INSERT INTO invoice_items (invoice_id, line_number, sku, product_name, quantity, unit_price_minor, currency, line_total_minor)
		SELECT inv.id, row_number() OVER (ORDER BY l.sku), l.sku, l.product_name, l.quantity, l.unit_price_minor, $7, l.line_total_minor
		FROM inv, lines l RETURNING 1
	), ful AS (
		INSERT INTO fulfillments (id, event_id, order_id, payment_attempt_id, status)
		VALUES ($11, $3, $4, $5, $12) ON CONFLICT (event_id) DO NOTHING RETURNING id
	), ful_items AS (
		INSERT INTO fulfillment_items (fulfillment_id, line_number, sku, product_name, quantity)
		SELECT ful.id, row_number() OVER (ORDER BY l.sku), l.sku, l.product_name, l.quantity
		FROM ful, lines l RETURNING 1
	), ntf AS (
		INSERT INTO notification_deliveries (id, event_id, order_id, payment_attempt_id, template, recipient_email, status)
		VALUES ($13, $3, $4, $5, $14, $6, $15) ON CONFLICT (event_id) DO NOTHING RETURNING id
	)
	SELECT (SELECT count(*) FROM inv), (SELECT count(*) FROM ful), (SELECT count(*) FROM ntf),
	       (SELECT count(*) FROM inv_items), (SELECT count(*) FROM ful_items)`

// Process records the order's three effects. It is safe to call any number of
// times, concurrently, for the same event.
func (p *Processor) Process(ctx context.Context, in Input) (Result, error) {
	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	// The usual case: nothing is recorded for this event yet.
	var existing int
	if err := p.dbs.Results.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM invoices WHERE event_id = $1)
		     + (SELECT count(*) FROM fulfillments WHERE event_id = $1)
		     + (SELECT count(*) FROM notification_deliveries WHERE event_id = $1)`, in.EventID).Scan(&existing); err != nil {
		return Result{}, err
	}
	if existing == 0 {
		if res, done, err := p.recordAll(ctx, in); done || err != nil {
			return res, err
		}
	}
	return p.recordEach(ctx, in)
}

// recordAll runs the combined statement. done is true when every row of the three
// effects was written by it; false (with no error) means the caller must settle the
// event with the three separate steps.
func (p *Processor) recordAll(ctx context.Context, in Input) (res Result, done bool, err error) {
	order, lines, err := p.dbs.ReadPaidOrder(ctx, EventType, in)
	if err != nil {
		return Result{}, false, err
	}
	// Conditions under which one of the separate jobs rejects while the others
	// still record their effect: leave them to the separate steps.
	if order.Total < order.Subtotal || order.Email == "" {
		return Result{}, false, nil
	}
	var invID, fulID, ntfID string
	for _, x := range []struct {
		prefix string
		id     *string
	}{{"inv_", &invID}, {"ful_", &fulID}, {"ntf_", &ntfID}} {
		if *x.id, err = bizjob.NewID(x.prefix); err != nil {
			return Result{}, false, err
		}
	}
	number := invoice.NumberFor(in.OrderID)
	var nInv, nFul, nNtf, nInvItems, nFulItems int
	err = p.dbs.Results.QueryRowContext(ctx, writeAll,
		invID, number, in.EventID, in.OrderID, in.PaymentAttemptID, order.Email, order.Currency, order.Subtotal, order.Total, lines.JSON(),
		fulID, fulfillment.StatusPrepared, ntfID, notification.Template, notification.StatusSent,
	).Scan(&nInv, &nFul, &nNtf, &nInvItems, &nFulItems)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		// Another event owns one of the order's rows (or a concurrent delivery got
		// there first): nothing was written; the separate steps classify it.
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if nInv != 1 || nFul != 1 || nNtf != 1 || nInvItems != len(lines) || nFulItems != len(lines) {
		// Some rows already existed (a concurrent delivery, or a row left by a
		// separate job): the separate steps complete and classify the rest.
		return Result{}, false, nil
	}
	return Result{OrderID: in.OrderID, InvoiceID: invID, InvoiceNumber: number, FulfillmentID: fulID, DeliveryID: ntfID, Items: len(lines)}, true, nil
}

// recordEach runs the three separate steps. Each is idempotent and applies its own
// rules, so a duplicate delivery, a partly recorded event and a conflict are all
// settled by them. Every step is attempted. If one fails with an infrastructure
// error the job is retried (the steps already done are then duplicates); otherwise
// the first rejection is the job's rejection.
func (p *Processor) recordEach(ctx context.Context, in Input) (Result, error) {
	inv, errInv := p.inv.Generate(ctx, in)
	ful, errFul := p.ful.Prepare(ctx, in)
	ntf, errNtf := p.ntf.Send(ctx, in)
	var rejected error
	for _, err := range []error{errInv, errFul, errNtf} {
		var re *bizjob.RejectError
		switch {
		case err == nil:
		case errors.As(err, &re):
			if rejected == nil {
				rejected = err
			}
		default:
			return Result{}, err
		}
	}
	if rejected != nil {
		return Result{}, rejected
	}
	return Result{
		OrderID: in.OrderID, InvoiceID: inv.InvoiceID, InvoiceNumber: inv.InvoiceNumber,
		FulfillmentID: ful.FulfillmentID, DeliveryID: ntf.DeliveryID, Items: ful.Items,
		Duplicate: inv.Duplicate && ful.Duplicate && ntf.Duplicate,
	}, nil
}
