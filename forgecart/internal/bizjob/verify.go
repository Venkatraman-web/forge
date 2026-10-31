package bizjob

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Venkatraman-web/forge/forgecart/internal/outbox"
)

// PaidOrder is what the common verification learns about the order.
type PaidOrder struct {
	Email    string // the order's stored customer email: never log or return it
	Currency string
	Subtotal int64
	Total    int64
}

// paidOrderQuery reads the event, the order and (when withLines) its lines in one
// statement: one round trip to the shop database instead of three. The order is
// joined with LEFT JOIN so that a missing order is still reported as such after the
// event's own checks, in the same order as before. %s is the lines column.
const paidOrderQuery = `SELECT e.event_type, e.order_id, e.payload ->> 'payment_attempt_id',
	o.status, o.inventory_status, o.paid_attempt_id, o.customer_email, o.currency, o.subtotal_minor, o.total_minor,
	%s
FROM outbox_events e LEFT JOIN orders o ON o.id = $2
WHERE e.id = $1`

// linesColumn aggregates the order's lines for the order id given by ref: the
// single read passes its parameter ($2), the batched read the unnested order id.
func linesColumn(ref string) string {
	return `COALESCE((SELECT jsonb_agg(jsonb_build_object('sku', i.sku, 'product_name', i.product_name, 'quantity', i.quantity,
		'unit_price_minor', i.unit_price_minor, 'currency', i.currency, 'line_total_minor', i.line_total_minor) ORDER BY i.sku)
		FROM order_items i WHERE i.order_id = ` + ref + `), '[]'::jsonb)`
}

const noLinesColumn = `'[]'::jsonb`

// VerifyPaidOrder performs the checks every ForgeCart business job needs before
// acting on an outbox event. It only reads, from the shop database, with one plain
// statement (no transaction and no row lock): a paid order, its payment attempt
// and its event are never changed afterwards, so nothing has to be held still
// while the worker records its result, which may be in another database.
//
//   - the event exists, is the order's paid event (or, for events written before
//     the change to one event per order, has the job's own type), and names the
//     input's order and payment attempt (the input alone is not trusted);
//   - the order exists and is paid with committed inventory;
//   - the order's recorded paying attempt is the input's payment attempt.
//
// Contradictions are *RejectError; database errors are returned unchanged.
func VerifyPaidOrder(ctx context.Context, q Queryer, eventType string, in Input) (PaidOrder, error) {
	o, _, err := readPaid(ctx, q, eventType, in, false)
	return o, err
}

// paidRow is what one read of an event, its order and (optionally) the order's lines
// returns; verify applies the checks to it. The single read (readPaid) and the
// batched Reader both produce paidRows, so they cannot disagree on the checks.
type paidRow struct {
	evType, evOrder                                  string
	evAttempt, status, invStatus, paidBy, email, ccy sql.NullString
	subtotal, total                                  sql.NullInt64
	rawLines                                         []byte
}

// fields lists the columns of paidOrderQuery in order, as Scan destinations.
func (p *paidRow) fields() []any {
	return []any{&p.evType, &p.evOrder, &p.evAttempt, &p.status, &p.invStatus, &p.paidBy, &p.email, &p.ccy, &p.subtotal, &p.total, &p.rawLines}
}

// readPaid runs paidOrderQuery and applies the checks of VerifyPaidOrder; with
// withLines it also checks the order's lines (see ReadPaidOrder).
func readPaid(ctx context.Context, q Queryer, eventType string, in Input, withLines bool) (PaidOrder, Lines, error) {
	col := noLinesColumn
	if withLines {
		col = linesColumn("$2")
	}
	var row paidRow
	err := q.QueryRowContext(ctx, fmt.Sprintf(paidOrderQuery, col), in.EventID, in.OrderID).Scan(row.fields()...)
	if errors.Is(err, sql.ErrNoRows) {
		return PaidOrder{}, nil, Reject("unknown_event")
	}
	if err != nil {
		return PaidOrder{}, nil, err
	}
	return row.verified(eventType, in, withLines)
}

// verified applies the checks to a row, in the order the contradictions are
// reported, and (withLines) checks the lines.
func (p paidRow) verified(eventType string, in Input, withLines bool) (PaidOrder, Lines, error) {
	// An order's event is "order.paid"; events written before that change carry
	// the job's own type, and are still accepted for that job.
	if p.evType != outbox.OrderPaid && p.evType != eventType {
		return PaidOrder{}, nil, Reject("wrong_event_type")
	}
	if p.evOrder != in.OrderID || p.evAttempt.String != in.PaymentAttemptID {
		return PaidOrder{}, nil, Reject("event_mismatch")
	}
	if !p.status.Valid { // the LEFT JOIN found no order
		return PaidOrder{}, nil, Reject("order_not_found")
	}
	if p.status.String != "paid" || p.invStatus.String != "committed" {
		return PaidOrder{}, nil, Reject("order_not_paid_and_committed")
	}
	// The order records which payment attempt made it paid (set in the same
	// transaction as the paid state and the event), so the payment service's
	// database is not read here.
	if !p.paidBy.Valid {
		return PaidOrder{}, nil, Reject("payment_attempt_not_found")
	}
	if p.paidBy.String != in.PaymentAttemptID {
		return PaidOrder{}, nil, Reject("payment_attempt_mismatch")
	}
	o := PaidOrder{Email: p.email.String, Currency: p.ccy.String, Subtotal: p.subtotal.Int64, Total: p.total.Int64}
	if !withLines {
		return o, nil, nil
	}
	lines, err := checkLines(p.rawLines, o)
	if err != nil {
		return PaidOrder{}, nil, err
	}
	return o, lines, nil
}

// checkLines decodes the order's lines and checks that they exist, are positive,
// use the order's currency and add up to its subtotal.
func checkLines(raw []byte, o PaidOrder) (Lines, error) {
	var rows []struct {
		OrderLine
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	var lines Lines
	var sum int64
	for _, r := range rows {
		if r.Quantity <= 0 || r.Currency != o.Currency {
			return nil, Reject("order_items_inconsistent")
		}
		sum += r.LineTotalMinor
		lines = append(lines, r.OrderLine)
	}
	if len(lines) == 0 || sum != o.Subtotal {
		return nil, Reject("order_items_inconsistent")
	}
	return lines, nil
}
