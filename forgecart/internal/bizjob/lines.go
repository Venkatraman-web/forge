package bizjob

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Querier is the part of *sql.DB the shop-side reads need.
type Querier interface {
	Queryer
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// OrderLine is one line of a paid order as the workers snapshot it. The
// currency is not carried: ReadLines checks that every line has the order's.
type OrderLine struct {
	SKU            string `json:"sku"`
	ProductName    string `json:"product_name"`
	Quantity       int    `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	LineTotalMinor int64  `json:"line_total_minor"`
}

// Lines are an order's lines in SKU order.
type Lines []OrderLine

// JSON is the argument of a worker's INSERT ... jsonb_to_recordset($n::jsonb):
// the lines read from the shop database are written to the results database as
// one statement, without a second query on the shop database.
func (l Lines) JSON() string {
	b, _ := json.Marshal(l) // plain strings and numbers: cannot fail
	return string(b)
}

// ReadLines reads the order's lines from the shop database and checks that they
// exist, are positive, use the order's currency and add up to its subtotal.
func ReadLines(ctx context.Context, q Querier, orderID string, o PaidOrder) (Lines, error) {
	rows, err := q.QueryContext(ctx, `SELECT sku, product_name, quantity, unit_price_minor, currency, line_total_minor FROM order_items WHERE order_id = $1 ORDER BY sku`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines Lines
	var sum int64
	for rows.Next() {
		var l OrderLine
		var currency string
		if err := rows.Scan(&l.SKU, &l.ProductName, &l.Quantity, &l.UnitPriceMinor, &currency, &l.LineTotalMinor); err != nil {
			return nil, err
		}
		if l.Quantity <= 0 || currency != o.Currency {
			return nil, Reject("order_items_inconsistent")
		}
		sum += l.LineTotalMinor
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 || sum != o.Subtotal {
		return nil, Reject("order_items_inconsistent")
	}
	return lines, nil
}

// ReadPaidOrder verifies the event and reads the order and its lines from the
// shop database. Nothing is written: the worker's result goes to the results
// database afterwards (see Databases).
func ReadPaidOrder(ctx context.Context, shop Querier, eventType string, in Input) (PaidOrder, Lines, error) {
	o, err := VerifyPaidOrder(ctx, shop, eventType, in)
	if err != nil {
		return PaidOrder{}, nil, err
	}
	lines, err := ReadLines(ctx, shop, in.OrderID, o)
	if err != nil {
		return PaidOrder{}, nil, err
	}
	return o, lines, nil
}
