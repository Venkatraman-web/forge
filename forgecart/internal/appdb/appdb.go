// Package appdb opens ForgeCart's own PostgreSQL connection pool.
package appdb

import (
	"context"
	"database/sql"
	"errors"
	"time"

	// Registers the "pgx" database/sql driver. pgx is already in go.mod as
	// the driver underneath GORM; this adds no new module.
	_ "github.com/jackc/pgx/v5/stdlib"
)

const startupTimeout = 5 * time.Second

// DefaultMaxConns is the pool size of a process that does not choose one.
const DefaultMaxConns = 10

// Open creates a pool of at most maxConns connections and verifies connectivity.
// The returned error is deliberately generic: driver errors can contain
// connection details.
func Open(ctx context.Context, url string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, errors.New("invalid application database configuration")
	}
	// The cap keeps a process from exhausting PostgreSQL's connection limit
	// (every process of every service shares it).
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns / 2)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, errors.New("cannot reach the application database (check " + "FORGECART_DATABASE_URL and that PostgreSQL is running)")
	}
	return db, nil
}
