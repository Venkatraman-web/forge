package orders

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Venkatraman-web/forge/forgecart/internal/inventory"
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

type Store struct {
	db  *sql.DB
	inv *inventory.Store
}

func NewStore(db *sql.DB, inv *inventory.Store) *Store { return &Store{db: db, inv: inv} }

func newOrderID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ord_" + hex.EncodeToString(b[:]), nil
}

// maxStaleRetries is how many times a checkout starts again from a fresh read after the inventory
// service refused a reservation because what the checkout had read changed.
const maxStaleRetries = 2

// Checkout creates the order and reserves its stock, or returns the existing order when key was
// already used for the same request. It is a saga of two local transactions, one in the inventory
// database and one in this database (docs/decisions/0005-inventory-service-and-checkout-saga.md):
//
//  1. Read the products from the inventory service (no locks) and validate and price the order.
//  2. Reserve the stock for the order id (a short transaction in the inventory database). A refusal
//     because of stock is the end: no order exists. A refusal because the price or availability
//     changed after the read starts the checkout again from a fresh read.
//  3. Create the order and its items in ONE statement (one commit). If the idempotency key was
//     already used, or the statement fails, the reservation is released (the compensation); for a
//     reused key the existing order is returned.
//
// If the process dies between 2 and 3 the stock stays reserved for an order that never existed; the
// reaper (package payments) releases it after a grace period. If the reply is lost after 3, the
// client retries with the same key and gets the order back, so a retry is always safe.
func (s *Store) Checkout(ctx context.Context, key string, req Request) (Order, error) {
	return s.checkout(ctx, key, req, 0)
}

func (s *Store) checkout(ctx context.Context, key string, req Request, attempt int) (Order, error) {
	fingerprint := req.Fingerprint()
	id, err := newOrderID()
	if err != nil {
		return Order{}, err
	}

	skus := make([]string, len(req.Items))
	for i, it := range req.Items {
		skus[i] = it.SKU
	}
	products, err := s.inv.Lookup(ctx, skus)
	if err != nil {
		return Order{}, err
	}

	// Validate every line before changing anything. Items are in SKU order,
	// so the reported failure is deterministic.
	items := make([]OrderItem, len(req.Items))
	lines := make([]inventory.Line, len(req.Items))
	var subtotal int64
	currency := ""
	for i, it := range req.Items {
		p, ok := products[it.SKU]
		if !ok || !p.Active {
			return Order{}, &Error{KindNotFound, "product_not_found", "Product " + it.SKU + " is not available."}
		}
		if currency == "" {
			currency = p.Currency
		} else if p.Currency != currency {
			return Order{}, &Error{KindConflict, "mixed_currency", "Products in one order must share a currency."}
		}
		if p.Currency != SupportedCcy {
			return Order{}, &Error{KindConflict, "unsupported_currency", "Only " + SupportedCcy + " orders are supported."}
		}
		if p.Available < it.Quantity {
			return Order{}, &Error{KindConflict, "insufficient_inventory", "Not enough stock for " + it.SKU + "."}
		}
		line, ok := mulInt64(int64(it.Quantity), p.UnitPriceMinor)
		if !ok {
			return Order{}, invalid("order_too_large", "The order total is too large.")
		}
		if subtotal, ok = addInt64(subtotal, line); !ok {
			return Order{}, invalid("order_too_large", "The order total is too large.")
		}
		items[i] = OrderItem{SKU: p.SKU, Name: p.Name, Quantity: it.Quantity, UnitPriceMinor: p.UnitPriceMinor, LineTotalMinor: line}
		lines[i] = inventory.Line{ProductID: p.ID, SKU: p.SKU, Quantity: it.Quantity, UnitPriceMinor: p.UnitPriceMinor, Currency: p.Currency}
	}

	if err := s.inv.Reserve(ctx, id, lines); err != nil {
		if errors.Is(err, inventory.ErrConflict) {
			if attempt < maxStaleRetries {
				return s.checkout(ctx, key, req, attempt+1)
			}
			return Order{}, &Error{KindConflict, "insufficient_inventory", "Not enough stock."}
		}
		return Order{}, err
	}

	type itemRow struct {
		ProductID      int64  `json:"product_id"`
		SKU            string `json:"sku"`
		ProductName    string `json:"product_name"`
		Quantity       int    `json:"quantity"`
		UnitPriceMinor int64  `json:"unit_price_minor"`
		LineTotalMinor int64  `json:"line_total_minor"`
	}
	rowsJSON := make([]itemRow, len(items))
	for i, it := range items {
		rowsJSON[i] = itemRow{lines[i].ProductID, it.SKU, it.Name, it.Quantity, it.UnitPriceMinor, it.LineTotalMinor}
	}
	payload, err := json.Marshal(rowsJSON)
	if err != nil {
		s.release(ctx, id)
		return Order{}, err
	}
	// One statement creates the order and its items: atomic by itself, one commit. ON CONFLICT on the
	// idempotency key leaves no row when the key was used already.
	var inserted int
	var createdAt sql.NullTime
	err = s.db.QueryRowContext(ctx, `
		WITH o AS (
			INSERT INTO orders (id, idempotency_key, request_fingerprint, customer_email, status, inventory_status, currency, subtotal_minor, total_minor)
			VALUES ($1, $2, $3, $4, 'pending_payment', 'reserved', $5, $6, $6)
			ON CONFLICT (idempotency_key) DO NOTHING RETURNING created_at
		), i AS (
			INSERT INTO order_items (order_id, product_id, sku, product_name, quantity, unit_price_minor, currency, line_total_minor)
			SELECT $1, product_id, sku, product_name, quantity, unit_price_minor, $5, line_total_minor
			FROM o, jsonb_to_recordset($7::jsonb)
			AS l(product_id bigint, sku text, product_name text, quantity int, unit_price_minor bigint, line_total_minor bigint)
		)
		SELECT (SELECT count(*) FROM o), (SELECT created_at FROM o)`,
		id, key, fingerprint, req.Email, currency, subtotal, string(payload)).Scan(&inserted, &createdAt)
	if err != nil {
		// An error does not prove the insert failed: the client may have gone away while the
		// statement still committed, and releasing now would leave an order without its stock.
		// Keep the reservation; the reaper releases it once it is old enough if no order exists.
		// The one case that proves nothing was written is a connection that could not be made at
		// all (the database refused or was unreachable): the statement was never sent.
		var connectErr *pgconn.ConnectError
		if errors.As(err, &connectErr) {
			s.release(ctx, id)
		}
		return Order{}, err
	}
	if inserted == 0 {
		// The key was used by an earlier request: give our reservation back and answer with that order.
		s.release(ctx, id)
		return s.replay(ctx, key, fingerprint)
	}
	return Order{ID: id, Status: "pending_payment", InventoryStatus: "reserved", Currency: currency,
		SubtotalMinor: subtotal, TotalMinor: subtotal, Items: items, CreatedAt: createdAt.Time.UTC()}, nil
}

// release gives a reservation back after a failed or duplicate checkout. It must not depend on the
// request context (the request may be what failed), and a failure here is left to the reaper.
func (s *Store) release(ctx context.Context, orderID string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = s.inv.Release(rctx, orderID)
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
