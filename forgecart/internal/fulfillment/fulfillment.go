// Package fulfillment implements the fulfillment.prepare business job: record a
// SIMULATED fulfillment plan (header plus item snapshots) for a paid order,
// exactly once per outbox event. It ships nothing and changes no stock.
package fulfillment

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

const (
	EventType      = "fulfillment.prepare"
	StatusPrepared = "prepared"
)

type Input = bizjob.Input

// Result is what a successful run reports to Forge.
type Result struct {
	FulfillmentID string `json:"fulfillment_id"`
	OrderID       string `json:"order_id"`
	Status        string `json:"status"`
	Items         int    `json:"items"`
	Duplicate     bool   `json:"duplicate"`
}

type Preparer struct{ dbs bizjob.Databases }

func New(dbs bizjob.Databases) *Preparer { return &Preparer{dbs: dbs} }

// Handler adapts a Preparer to the worker runtime.
func Handler(p *Preparer) workerruntime.HandlerFunc { return bizjob.Handler(p.Prepare) }

var effect = bizjob.Effect[Result]{
	Find: func(ctx context.Context, q bizjob.Queryer, eventID string) (*bizjob.Stored[Result], error) {
		var r Result
		var attemptID string
		err := q.QueryRowContext(ctx, `
			SELECT f.id, f.order_id, f.status, f.payment_attempt_id, (SELECT count(*) FROM fulfillment_items i WHERE i.fulfillment_id = f.id)
			FROM fulfillments f WHERE f.event_id = $1`, eventID).Scan(&r.FulfillmentID, &r.OrderID, &r.Status, &attemptID, &r.Items)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return &bizjob.Stored[Result]{Result: r, OrderID: r.OrderID, PaymentAttemptID: attemptID}, err
	},
	Duplicate:        func(r Result) Result { r.Duplicate = true; return r },
	ConflictCode:     "conflicts_with_prepared_plan",
	OrderConstraints: []string{"fulfillments_order_key"},
	OtherEventCode:   "order_already_prepared_by_other_event",
}

// Prepare verifies the event against the shop database and records the plan in
// one transaction on the results database. Its only writes are the new
// fulfillments and fulfillment_items rows.
//
// Safe to call any number of times, concurrently, for the same event:
// UNIQUE(event_id) lets exactly one insert win and everyone else reports the
// stored plan as a duplicate if (and only if) it matches.
func (p *Preparer) Prepare(ctx context.Context, in Input) (Result, error) {
	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	if res, found, err := effect.Existing(ctx, p.dbs.Results, in); found || err != nil {
		return res, err
	}

	_, lines, err := bizjob.ReadPaidOrder(ctx, p.dbs.Shop, EventType, in)
	if err != nil {
		return Result{}, err
	}

	var rb [12]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return Result{}, err
	}
	id := "ful_" + hex.EncodeToString(rb[:])

	tx, err := p.dbs.Results.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	if dup, inserted, err := effect.Insert(ctx, p.dbs.Results, tx, in, `
		INSERT INTO fulfillments (id, event_id, order_id, payment_attempt_id, status)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (event_id) DO NOTHING`,
		id, in.EventID, in.OrderID, in.PaymentAttemptID, StatusPrepared); !inserted {
		return dup, err
	}

	// Snapshot the lines read from the order (SKU order), in the same transaction.
	ins, err := tx.ExecContext(ctx, `
		INSERT INTO fulfillment_items (fulfillment_id, line_number, sku, product_name, quantity)
		SELECT $1, row_number() OVER (ORDER BY sku), sku, product_name, quantity
		FROM jsonb_to_recordset($2::jsonb) AS l(sku text, product_name text, quantity int)`, id, lines.JSON())
	if err != nil {
		return Result{}, err
	}
	if c, _ := ins.RowsAffected(); int(c) != len(lines) {
		return Result{}, errors.New("fulfillment: item snapshot count differs from the verified lines; retry")
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{FulfillmentID: id, OrderID: in.OrderID, Status: StatusPrepared, Items: len(lines)}, nil
}
