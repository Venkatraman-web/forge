package bizjob

import (
	"context"
	"encoding/json"
)

// OrderLine is one line of a paid order as the workers snapshot it. The
// currency is not carried: checkLines checks that every line has the order's.
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

// ReadPaidOrder verifies the event and reads the order and its lines from the
// shop database, in one statement. Nothing is written: the worker's result goes
// to the results database afterwards (see Databases).
func ReadPaidOrder(ctx context.Context, shop Queryer, eventType string, in Input) (PaidOrder, Lines, error) {
	return readPaid(ctx, shop, eventType, in, true)
}
