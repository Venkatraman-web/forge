// Package catalog reads the product catalog from ForgeCart's database.
package catalog

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"regexp"
)

// SKUPattern mirrors the products_sku_format database constraint, so
// malformed SKUs and cursors are rejected before reaching the database.
var SKUPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{2,63}$`)

var ErrNotFound = errors.New("catalog: product not found")

// Product is the public shape of a catalog row. Prices are integer minor
// units (USD cents).
type Product struct {
	SKU       string `json:"sku"`
	Name      string `json:"name"`
	UnitPrice int64  `json:"unit_price_minor"`
	Currency  string `json:"currency"`
	Available int    `json:"available_inventory"`
	Active    bool   `json:"active"`
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const columns = `sku, name, unit_price_minor, currency, inventory_quantity, active`

func scan(row interface{ Scan(...any) error }) (Product, error) {
	var p Product
	err := row.Scan(&p.SKU, &p.Name, &p.UnitPrice, &p.Currency, &p.Available, &p.Active)
	return p, err
}

// List returns up to limit products ordered by SKU, starting after the given
// SKU (keyset pagination: stable even if rows are added between pages).
func (s *Store) List(ctx context.Context, after string, limit int) ([]Product, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+columns+` FROM products WHERE sku > $1 ORDER BY sku LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Product{}
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, sku string) (Product, error) {
	p, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM products WHERE sku = $1`, sku))
	if errors.Is(err, sql.ErrNoRows) {
		return Product{}, ErrNotFound
	}
	return p, err
}

//go:embed seed.sql
var seedSQL string

// Seed inserts the starter catalog. It is idempotent and never touches
// existing rows. It returns how many rows were newly inserted.
func Seed(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, seedSQL)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
