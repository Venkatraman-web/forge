// Package outbox holds ForgeCart's transactional outbox: the row a paid order
// writes (this file) and the dispatcher's claims on pending rows (claims.go).
package outbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

// OrderPaid is the one event a paid order writes. The dispatcher turns it into
// one Forge job per entry of JobTypes when it publishes.
const OrderPaid = "order.paid"

// JobTypes are the background jobs a paid order needs, in the order the
// dispatcher submits them. They are the Forge job types of the two workers:
// the inventory reconciliation, and "order.process", which records the invoice, the
// fulfillment plan and the confirmation notification of the order in one job
// (before 2026-10-30 each of those three was a job of its own).
var JobTypes = []string{
	"inventory.reconcile",
	"order.process",
}

// payload is deliberately tiny and holds no customer data, prices or secrets:
// workers load what they need from ForgeCart's database by order ID.
type payload struct {
	OrderID          string `json:"order_id"`
	PaymentAttemptID string `json:"payment_attempt_id"`
}

// PaidOrderEvent is the outbox row a paid order writes.
type PaidOrderEvent struct {
	ID      string
	Type    string
	Payload string // JSON
}

// NewPaidOrderEvent makes the order's one event. The caller inserts it in the same
// statement that marks the order paid, so the order is paid and the event exists, or
// neither is committed; a duplicate (order, type) violates the unique constraint and
// fails that statement.
func NewPaidOrderEvent(orderID, paymentAttemptID string) (PaidOrderEvent, error) {
	body, err := json.Marshal(payload{OrderID: orderID, PaymentAttemptID: paymentAttemptID})
	if err != nil {
		return PaidOrderEvent{}, err
	}
	var rb [12]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return PaidOrderEvent{}, err
	}
	return PaidOrderEvent{ID: "evt_" + hex.EncodeToString(rb[:]), Type: OrderPaid, Payload: string(body)}, nil
}
