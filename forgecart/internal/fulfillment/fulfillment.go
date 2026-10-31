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

	_, lines, err := p.dbs.ReadPaidOrder(ctx, EventType, in)
	if err != nil {
		return Result{}, err
	}

	var rb [12]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return Result{}, err
	}
	id := "ful_" + hex.EncodeToString(rb[:])

	// One statement writes the fulfillment plan and its line snapshot (SKU order): it
	// is atomic by itself, and its affected-row count is the number of lines.
	if dup, inserted, err := effect.Write(ctx, p.dbs.Results, in, int64(len(lines)), `
		WITH h AS (
			INSERT INTO fulfillments (id, event_id, order_id, payment_attempt_id, status)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (event_id) DO NOTHING RETURNING id
		)
		INSERT INTO fulfillment_items (fulfillment_id, line_number, sku, product_name, quantity)
		SELECT h.id, row_number() OVER (ORDER BY l.sku), l.sku, l.product_name, l.quantity
		FROM h, jsonb_to_recordset($6::jsonb) AS l(sku text, product_name text, quantity int)`,
		id, in.EventID, in.OrderID, in.PaymentAttemptID, StatusPrepared, lines.JSON()); !inserted {
		return dup, err
	}
	return Result{FulfillmentID: id, OrderID: in.OrderID, Status: StatusPrepared, Items: len(lines)}, nil
}
