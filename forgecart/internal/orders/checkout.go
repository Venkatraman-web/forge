package orders

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"time"
)

// Order is the public order shape. Item prices and names are the snapshots
// stored at checkout, not current catalog values.
type Order struct {
	ID              string      `json:"order_id"`
	Status          string      `json:"status"`
	InventoryStatus string      `json:"inventory_status"`
	Currency        string      `json:"currency"`
	SubtotalMinor   int64       `json:"subtotal_minor"`
	TotalMinor      int64       `json:"total_minor"`
	Items           []OrderItem `json:"items"`
	CreatedAt       time.Time   `json:"created_at"`
	Replayed        bool        `json:"replayed"`
}

type OrderItem struct {
	SKU            string `json:"sku"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	LineTotalMinor int64  `json:"line_total_minor"`
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func newOrderID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ord_" + hex.EncodeToString(b[:]), nil
}

type lockedProduct struct {
	id        int64
	sku       string
	name      string
	price     int64
	currency  string
	available int
	active    bool
}

// Checkout creates the order and reserves stock in ONE transaction, or
// returns the existing order when key was already used for the same request.
//
// Order of work, and why:
//  1. Insert the order row first with ON CONFLICT DO NOTHING on the unique
//     idempotency key. A concurrent request with the same key blocks right
//     here until the first transaction commits or rolls back, before touching
//     any product row; afterwards it sees the committed order (replay) or
//     takes over (if the first one rolled back). A replay is therefore never
//     confused with "out of stock".
//  2. Lock the product rows with FOR UPDATE in SKU order (the same order for
//     every checkout, which prevents lock-order deadlocks).
//  3. Check stock and currency on the locked rows, price the order, fill in the
//     order, insert item snapshots, and decrement stock with a conditional
//     UPDATE. Any failure rolls the whole transaction back, including the
//     idempotency row from step 1.
//
// If Commit itself fails the outcome is unknown; retrying with the same key is
// safe because the unique key decides whether the order exists.
func (s *Store) Checkout(ctx context.Context, key string, req Request) (Order, error) {
	fingerprint := req.Fingerprint()
	id, err := newOrderID()
	if err != nil {
		return Order{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil) // READ COMMITTED; row locks do the protecting
	if err != nil {
		return Order{}, err
	}
	defer tx.Rollback() // no-op after Commit

	res, err := tx.ExecContext(ctx, `
		INSERT INTO orders (id, idempotency_key, request_fingerprint, customer_email, status, inventory_status, currency, subtotal_minor, total_minor)
		VALUES ($1, $2, $3, $4, 'pending_payment', 'reserved', $5, 0, 0)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		id, key, fingerprint, req.Email, SupportedCcy)
	if err != nil {
		return Order{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		tx.Rollback()
		return s.replay(ctx, key, fingerprint)
	}

	skus := make([]string, len(req.Items))
	for i, it := range req.Items {
		skus[i] = it.SKU
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, sku, name, unit_price_minor, currency, inventory_quantity, active
		FROM products WHERE sku = ANY($1::text[]) ORDER BY sku FOR UPDATE`, skus)
	if err != nil {
		return Order{}, err
	}
	products := map[string]lockedProduct{}
	for rows.Next() {
		var p lockedProduct
		if err := rows.Scan(&p.id, &p.sku, &p.name, &p.price, &p.currency, &p.available, &p.active); err != nil {
			rows.Close()
			return Order{}, err
		}
		products[p.sku] = p
	}
	if err := rows.Err(); err != nil {
		return Order{}, err
	}
	rows.Close()

	// Validate every line before changing anything. Items are in SKU order,
	// so the reported failure is deterministic.
	items := make([]OrderItem, len(req.Items))
	var subtotal int64
	currency := ""
	for i, it := range req.Items {
		p, ok := products[it.SKU]
		if !ok || !p.active {
			return Order{}, &Error{KindNotFound, "product_not_found", "Product " + it.SKU + " is not available."}
		}
		if currency == "" {
			currency = p.currency
		} else if p.currency != currency {
			return Order{}, &Error{KindConflict, "mixed_currency", "Products in one order must share a currency."}
		}
		if p.currency != SupportedCcy {
			return Order{}, &Error{KindConflict, "unsupported_currency", "Only " + SupportedCcy + " orders are supported."}
		}
		if p.available < it.Quantity {
			return Order{}, &Error{KindConflict, "insufficient_inventory", "Not enough stock for " + it.SKU + "."}
		}
		line, ok := mulInt64(int64(it.Quantity), p.price)
		if !ok {
			return Order{}, invalid("order_too_large", "The order total is too large.")
		}
		if subtotal, ok = addInt64(subtotal, line); !ok {
			return Order{}, invalid("order_too_large", "The order total is too large.")
		}
		items[i] = OrderItem{SKU: p.sku, Name: p.name, Quantity: it.Quantity, UnitPriceMinor: p.price, LineTotalMinor: line}
	}

	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		UPDATE orders SET currency = $2, subtotal_minor = $3, total_minor = $3, updated_at = now()
		WHERE id = $1 RETURNING created_at`, id, currency, subtotal).Scan(&createdAt)
	if err != nil {
		return Order{}, err
	}
	for _, it := range items {
		p := products[it.SKU]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO order_items (order_id, product_id, sku, product_name, quantity, unit_price_minor, currency, line_total_minor)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, p.id, it.SKU, it.Name, it.Quantity, it.UnitPriceMinor, currency, it.LineTotalMinor); err != nil {
			return Order{}, err
		}
		// Conditional decrement: even though the row is locked, never trust a
		// read for the write. Zero rows would mean the guard failed.
		dec, err := tx.ExecContext(ctx, `
			UPDATE products SET inventory_quantity = inventory_quantity - $2, updated_at = now()
			WHERE id = $1 AND inventory_quantity >= $2`, p.id, it.Quantity)
		if err != nil {
			return Order{}, err
		}
		if n, _ := dec.RowsAffected(); n != 1 {
			return Order{}, &Error{KindConflict, "insufficient_inventory", "Not enough stock for " + it.SKU + "."}
		}
	}
	if err := tx.Commit(); err != nil {
		return Order{}, err
	}
	return Order{ID: id, Status: "pending_payment", InventoryStatus: "reserved", Currency: currency,
		SubtotalMinor: subtotal, TotalMinor: subtotal, Items: items, CreatedAt: createdAt.UTC()}, nil
}

// replay returns the order already stored under key, if it was made for the
// same request. It reads committed data: the conflicting insert only returns
// "no row" once the other transaction has committed.
func (s *Store) replay(ctx context.Context, key, fingerprint string) (Order, error) {
	var o Order
	var stored string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, request_fingerprint, status, inventory_status, currency, subtotal_minor, total_minor, created_at
		FROM orders WHERE idempotency_key = $1`, key).
		Scan(&o.ID, &stored, &o.Status, &o.InventoryStatus, &o.Currency, &o.SubtotalMinor, &o.TotalMinor, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// The other transaction rolled back after our insert was skipped; ask the client to retry.
		return Order{}, &Error{KindConflict, "retry", "The request raced with another using the same key; retry."}
	}
	if err != nil {
		return Order{}, err
	}
	if stored != fingerprint {
		return Order{}, &Error{KindConflict, "idempotency_key_reused", "This Idempotency-Key was already used for a different request."}
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT sku, product_name, quantity, unit_price_minor, line_total_minor
		FROM order_items WHERE order_id = $1 ORDER BY sku`, o.ID)
	if err != nil {
		return Order{}, err
	}
	defer rows.Close()
	o.Items = []OrderItem{}
	for rows.Next() {
		var it OrderItem
		if err := rows.Scan(&it.SKU, &it.Name, &it.Quantity, &it.UnitPriceMinor, &it.LineTotalMinor); err != nil {
			return Order{}, err
		}
		o.Items = append(o.Items, it)
	}
	o.CreatedAt = o.CreatedAt.UTC()
	o.Replayed = true
	return o, rows.Err()
}

func mulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxInt64/b {
		return 0, false
	}
	return a * b, true
}

func addInt64(a, b int64) (int64, bool) {
	if a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}
