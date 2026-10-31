// Package inventory is ForgeCart's stock service. It owns the inventory database: the product
// catalogue, the stock of every product, and the stock reservations of the checkout saga
// (docs/decisions/0005-inventory-service-and-checkout-saga.md).
//
// Stock moves in two steps that the shop's checkout and payment code drive: Reserve takes the
// stock for one order, and Release gives it back. Both are idempotent: the reservation row
// (primary key: the order id) makes reserving twice for one order take the stock once, and Release
// only acts on a reservation that is still 'reserved'. Each is one short transaction on the
// inventory database, with the product rows locked in SKU order (the same order every time, which
// prevents lock-order deadlocks) and held only until that transaction commits.
package inventory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrConflict means a reservation could not be made on what the caller read: not enough stock, or
// the price, currency or availability of a product changed after the read. Nothing was taken.
var ErrConflict = errors.New("inventory: the stock, price or availability changed")

// Product is a catalogue row with its current stock. Available is advisory when read through
// Lookup: only Reserve decides.
type Product struct {
	ID             int64
	SKU            string
	Name           string
	UnitPriceMinor int64
	Currency       string
	Available      int
	Active         bool
}

// Line is one product of a reservation. UnitPriceMinor and Currency are what the caller read and
// priced the order with: Reserve refuses if they no longer match.
type Line struct {
	ProductID      int64  `json:"product_id"`
	SKU            string `json:"sku"`
	Quantity       int    `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	Currency       string `json:"currency"`
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Lookup reads the products with the given SKUs, without locking anything. A SKU that does not
// exist is simply missing from the result.
func (s *Store) Lookup(ctx context.Context, skus []string) (map[string]Product, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, sku, name, unit_price_minor, currency, inventory_quantity, active
		FROM products WHERE sku = ANY($1::text[])`, skus)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Product, len(skus))
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.SKU, &p.Name, &p.UnitPriceMinor, &p.Currency, &p.Available, &p.Active); err != nil {
			return nil, err
		}
		out[p.SKU] = p
	}
	return out, rows.Err()
}

// Reserve takes the stock of lines (which must be in SKU order) for orderID. It is idempotent: a
// second call for the same order id returns nil without taking anything again. It returns
// ErrConflict when a guard fails, with nothing taken.
func (s *Store) Reserve(ctx context.Context, orderID string, lines []Line) error {
	payload, err := json.Marshal(lines)
	if err != nil {
		return err
	}
	ids := make([]int64, len(lines))
	for i, l := range lines {
		ids[i] = l.ProductID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	res, err := tx.ExecContext(ctx, `
		INSERT INTO stock_reservations (order_id, lines) VALUES ($1, $2::jsonb)
		ON CONFLICT (order_id) DO NOTHING`, orderID, string(payload))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// A reservation for this order id exists already: its stock was taken by an earlier call.
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM stock_reservations WHERE order_id = $1`, orderID).Scan(&status); err != nil {
			return err
		}
		if status != "reserved" {
			return fmt.Errorf("inventory: the reservation of %s is %s", orderID, status)
		}
		return tx.Commit()
	}
	if err := lockInSKUOrder(ctx, tx, ids); err != nil {
		return err
	}
	// The guard re-checks stock, price, currency and availability on the locked rows.
	res, err = tx.ExecContext(ctx, `
		UPDATE products p SET inventory_quantity = p.inventory_quantity - l.quantity, updated_at = now()
		FROM jsonb_to_recordset($1::jsonb) AS l(product_id bigint, quantity int, unit_price_minor bigint, currency text)
		WHERE p.id = l.product_id AND p.inventory_quantity >= l.quantity AND p.active
		  AND p.unit_price_minor = l.unit_price_minor AND p.currency = l.currency`, string(payload))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); int(n) != len(lines) {
		return ErrConflict // the deferred rollback removes the reservation row too
	}
	return tx.Commit()
}

// Release gives the stock of orderID's reservation back and marks it released. It is idempotent:
// when there is no reservation, or it is released or committed already, it does nothing.
func (s *Store) Release(ctx context.Context, orderID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, `
		UPDATE stock_reservations SET status = 'released', released_at = now()
		WHERE order_id = $1 AND status = 'reserved' RETURNING lines`, orderID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var lines []Line
	if err := json.Unmarshal(raw, &lines); err != nil {
		return err
	}
	ids := make([]int64, len(lines))
	for i, l := range lines {
		ids[i] = l.ProductID
	}
	if err := lockInSKUOrder(ctx, tx, ids); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE products p SET inventory_quantity = p.inventory_quantity + l.quantity, updated_at = now()
		FROM jsonb_to_recordset($1::jsonb) AS l(product_id bigint, quantity int)
		WHERE p.id = l.product_id`, string(raw))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); int(n) != len(lines) {
		return errors.New("inventory: a product row is missing during release")
	}
	return tx.Commit()
}

// ReleaseMany is Release for many orders at once: one transaction, one commit. It releases the
// reservations that are still 'reserved', locks every product it touches once, in SKU order (the
// order every other transaction uses, so it cannot deadlock with them), gives back the summed
// quantities, and returns how many reservations it released. The caller keeps the list short
// (a few hundred): the transaction holds those product locks until it commits.
func (s *Store) ReleaseMany(ctx context.Context, orderIDs []string) (int, error) {
	if len(orderIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		UPDATE stock_reservations SET status = 'released', released_at = now()
		WHERE order_id = ANY($1::text[]) AND status = 'reserved' RETURNING lines`, orderIDs)
	if err != nil {
		return 0, err
	}
	released := 0
	back := map[int64]int{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return 0, err
		}
		var lines []Line
		if err := json.Unmarshal(raw, &lines); err != nil {
			rows.Close()
			return 0, err
		}
		for _, l := range lines {
			back[l.ProductID] += l.Quantity
		}
		released++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if released == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(back))
	type add struct {
		ProductID int64 `json:"product_id"`
		Quantity  int   `json:"quantity"`
	}
	adds := make([]add, 0, len(back))
	for id, q := range back {
		ids = append(ids, id)
		adds = append(adds, add{id, q})
	}
	if err := lockInSKUOrder(ctx, tx, ids); err != nil {
		return 0, err
	}
	payload, err := json.Marshal(adds)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE products p SET inventory_quantity = p.inventory_quantity + l.quantity, updated_at = now()
		FROM jsonb_to_recordset($1::jsonb) AS l(product_id bigint, quantity int)
		WHERE p.id = l.product_id`, string(payload))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); int(n) != len(adds) {
		return 0, errors.New("inventory: a product row is missing during release")
	}
	return released, tx.Commit()
}

// Commit marks the reservations of the given orders (known to be paid) as committed, so that the
// reaper stops looking at them, and returns how many it changed. A reservation that is not
// 'reserved' is left alone.
func (s *Store) Commit(ctx context.Context, orderIDs []string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE stock_reservations SET status = 'committed'
		WHERE order_id = ANY($1::text[]) AND status = 'reserved'`, orderIDs)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// Reserved is a reservation still in state 'reserved'.
type Reserved struct {
	OrderID   string
	CreatedAt time.Time
}

// Stale returns the reservations that are still 'reserved' and older than age, in (created_at,
// order id) order, starting after the cursor (the zero Reserved starts from the beginning), at most
// limit of them. The reaper walks the whole list with it, so reservations it can do nothing about
// (their order is still waiting for payment) never hide the ones behind them.
func (s *Store) Stale(ctx context.Context, age time.Duration, after Reserved, limit int) ([]Reserved, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT order_id, created_at FROM stock_reservations
		WHERE status = 'reserved' AND created_at < now() - make_interval(secs => $1::float8)
		  AND (created_at, order_id) > ($2::timestamptz, $3::text)
		ORDER BY created_at, order_id LIMIT $4`, age.Seconds(), after.CreatedAt, after.OrderID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reserved
	for rows.Next() {
		var r Reserved
		if err := rows.Scan(&r.OrderID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// lockInSKUOrder locks the product rows with FOR UPDATE in SKU order, the same order for every
// caller, so two transactions can never wait for each other's rows.
func lockInSKUOrder(ctx context.Context, tx *sql.Tx, ids []int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM products WHERE id = ANY($1::bigint[]) ORDER BY sku FOR UPDATE`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() { // the locks are taken as the rows are produced; read them all
	}
	return rows.Err()
}
