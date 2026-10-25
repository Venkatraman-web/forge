// Package notification implements the notification.send business job: record a
// SIMULATED order-confirmation delivery for a paid order, exactly once per
// outbox event. Nothing is ever sent to an email provider.
package notification

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
	EventType  = "notification.send"
	template   = "order_confirmation"
	StatusSent = "simulated_sent"
)

type Input = bizjob.Input

// Result is what a successful run reports to Forge. It deliberately has no
// recipient field.
type Result struct {
	DeliveryID string `json:"delivery_id"`
	OrderID    string `json:"order_id"`
	Status     string `json:"status"`
	Duplicate  bool   `json:"duplicate"`
}

type Notifier struct{ dbs bizjob.Databases }

func New(dbs bizjob.Databases) *Notifier { return &Notifier{dbs: dbs} }

// Handler adapts a Notifier to the worker runtime.
func Handler(n *Notifier) workerruntime.HandlerFunc { return bizjob.Handler(n.Send) }

var effect = bizjob.Effect[Result]{
	Find: func(ctx context.Context, q bizjob.Queryer, eventID string) (*bizjob.Stored[Result], error) {
		var r Result
		var attemptID string
		err := q.QueryRowContext(ctx, `SELECT id, order_id, status, payment_attempt_id FROM notification_deliveries WHERE event_id = $1`, eventID).
			Scan(&r.DeliveryID, &r.OrderID, &r.Status, &attemptID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return &bizjob.Stored[Result]{Result: r, OrderID: r.OrderID, PaymentAttemptID: attemptID}, err
	},
	Duplicate:        func(r Result) Result { r.Duplicate = true; return r },
	ConflictCode:     "conflicts_with_recorded_delivery",
	OrderConstraints: []string{"notification_deliveries_order_template_key"},
	OtherEventCode:   "order_already_confirmed_by_other_event",
}

// Send verifies the event against the shop database and records the simulated
// delivery in one transaction on the results database.
//
// Safe to call any number of times, concurrently, for the same event:
// UNIQUE(event_id) lets exactly one insert win and everyone else reports the
// stored delivery as a duplicate if (and only if) it matches.
func (n *Notifier) Send(ctx context.Context, in Input) (Result, error) {
	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	if res, found, err := effect.Existing(ctx, n.dbs.Results, in); found || err != nil {
		return res, err
	}

	// The confirmation needs the order's lines: ReadPaidOrder checks that they
	// exist and add up.
	order, _, err := bizjob.ReadPaidOrder(ctx, n.dbs.Shop, EventType, in)
	if err != nil {
		return Result{}, err
	}
	if order.Email == "" {
		return Result{}, bizjob.Reject("order_has_no_recipient")
	}

	var rb [12]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return Result{}, err
	}
	id := "ntf_" + hex.EncodeToString(rb[:])

	tx, err := n.dbs.Results.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	if dup, inserted, err := effect.Insert(ctx, n.dbs.Results, tx, in, `
		INSERT INTO notification_deliveries (id, event_id, order_id, payment_attempt_id, template, recipient_email, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (event_id) DO NOTHING`,
		id, in.EventID, in.OrderID, in.PaymentAttemptID, template, order.Email, StatusSent); !inserted {
		return dup, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{DeliveryID: id, OrderID: in.OrderID, Status: StatusSent}, nil
}
