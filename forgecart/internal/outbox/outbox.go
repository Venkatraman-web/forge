// Package outbox writes ForgeCart's transactional outbox rows. It only
// inserts; reading, claiming and publishing events belongs to the dispatcher
// task, which has not been written.
package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// OrderPaid is the one event a paid order writes. The dispatcher turns it into
// one Forge job per entry of JobTypes when it publishes.
const OrderPaid = "order.paid"

// JobTypes are the background jobs a paid order needs, in the order the
// dispatcher submits them. They are the Forge job types of the four workers.
var JobTypes = []string{
	"inventory.reconcile",
	"fulfillment.prepare",
	"invoice.generate",
	"notification.send",
}

// payload is deliberately tiny and holds no customer data, prices or secrets:
// workers load what they need from ForgeCart's database by order ID.
type payload struct {
	OrderID          string `json:"order_id"`
	PaymentAttemptID string `json:"payment_attempt_id"`
}

// InsertPaidOrderEvent inserts the order's one pending event inside the caller's
// transaction, so it commits or rolls back together with the payment. The insert
// must create exactly one row; a duplicate (order, type) violates the unique
// constraint and fails the transaction.
func InsertPaidOrderEvent(ctx context.Context, tx *sql.Tx, orderID, paymentAttemptID string) error {
	body, err := json.Marshal(payload{OrderID: orderID, PaymentAttemptID: paymentAttemptID})
	if err != nil {
		return err
	}
	var rb [12]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_events (id, order_id, event_type, payload)
		VALUES ($1, $2, $3, $4::jsonb)`,
		"evt_"+hex.EncodeToString(rb[:]), orderID, OrderPaid, string(body))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("outbox insert affected %d rows", n)
	}
	return nil
}
