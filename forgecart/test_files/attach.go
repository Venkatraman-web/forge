package main

// "Attach" mode: the same Env/Trial machinery as the sandbox, pointed at a
// ForgeCart + Forge deployment that ALREADY RUNS (for example on other
// machines). Nothing is built, started or stopped. Between trials the harness
// waits for Forge to go idle and then EMPTIES the ForgeCart business tables and
// resets stock, so every trial starts from the same state. That erase is the
// one destructive thing attach mode does, which is why the command demands an
// explicit confirmation flag and prints which database host it will erase.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AttachConfig names the running deployment. The keys and the database URL are
// secrets: they come from the environment and are never written to results.
type AttachConfig struct {
	CartURL     string
	ForgeURL    string
	OperatorURL string
	APIKey      string // Forge's submit key (reads job state for the observer)
	OperatorKey string
	AppDBURL    string // ForgeCart's application database; its business tables get emptied
	// ResultsDBURL is the workers' separate results database, if the deployment has one
	// (FORGECART_RESULTS_DATABASE_URL on the workers). Empty = the results are in AppDBURL.
	ResultsDBURL string
	// PaymentsDBURL is the payment service's database (its attempts and Razorpay links).
	PaymentsDBURL string
}

// businessTables are emptied between trials; products (the catalogue) is kept
// and only its stock is reset.
var businessTables = []string{"razorpay_links", "order_items", "orders", "payment_attempts", "outbox_events", "inventory_reconciliations",
	"invoice_items", "invoices", "notification_deliveries", "fulfillment_items", "fulfillments"}

// resultTables are the workers' result tables; in a split deployment they live in
// the results database.
var resultTables = []string{"inventory_reconciliations", "invoice_items", "invoices", "notification_deliveries", "fulfillment_items", "fulfillments"}

// NewAttachedSandbox creates a Sandbox that manages nothing: URLs and keys come
// from the AttachConfig, and there are no processes, containers or logs.
func NewAttachedSandbox(repo, runDir, runID string, a AttachConfig) (*Sandbox, error) {
	logDir := filepath.Join(runDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	return &Sandbox{RunID: runID, Project: "attached-" + runID, Repo: repo, RunDir: runDir, LogDir: logDir,
		BinDir: filepath.Join(runDir, "bin"), apiKey: a.APIKey, attach: &a, procs: map[string]*Proc{}}, nil
}

// DBHost is the database host:port shown in logs (never the credentials).
func (a AttachConfig) DBHost() string {
	u, err := url.Parse(a.AppDBURL)
	if err != nil {
		return "(unparseable)"
	}
	return u.Host + u.Path
}

// resetAttached waits until Forge is idle, empties the business tables and
// resets stock. Forge's own database is never touched.
func (s *Sandbox) resetAttached(ctx context.Context, cfg SandboxConfig) error {
	a := s.attach
	f := &remoteForge{cfg: RemoteConfig{OperatorURL: a.OperatorURL}, opKey: a.OperatorKey, hc: &http.Client{Timeout: 15 * time.Second}}
	f.op = &opClient{url: strings.TrimRight(a.OperatorURL, "/") + "/v1/queues", auth: "Bearer " + a.OperatorKey, hc: f.hc}
	if _, ok := f.waitIdle(ctx, 180*time.Second); !ok {
		return fmt.Errorf("Forge still has queued or running jobs after 180 s; refusing to reset the ForgeCart tables underneath them")
	}
	db, err := sql.Open("pgx", a.AppDBURL)
	if err != nil {
		return fmt.Errorf("open ForgeCart database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// Let a dispatcher that is still publishing the previous trial's last events finish.
	time.Sleep(2 * time.Second)
	if _, err := db.ExecContext(ctx, "TRUNCATE "+strings.Join(businessTables, ", ")+" CASCADE"); err != nil {
		return fmt.Errorf("empty ForgeCart tables: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("UPDATE products SET inventory_quantity = %d, updated_at = now()", cfg.Inventory)); err != nil {
		return fmt.Errorf("reset stock: %w", err)
	}
	pdb, err := sql.Open("pgx", a.PaymentsDBURL)
	if err != nil {
		return fmt.Errorf("open payment database: %w", err)
	}
	defer pdb.Close()
	if _, err := pdb.ExecContext(ctx, "TRUNCATE payment_attempts, razorpay_links"); err != nil {
		return fmt.Errorf("empty payment tables: %w", err)
	}
	if a.ResultsDBURL != "" {
		rdb, err := sql.Open("pgx", a.ResultsDBURL)
		if err != nil {
			return fmt.Errorf("open results database: %w", err)
		}
		defer rdb.Close()
		if _, err := rdb.ExecContext(ctx, "TRUNCATE "+strings.Join(resultTables, ", ")); err != nil {
			return fmt.Errorf("empty results tables: %w", err)
		}
	}
	return nil
}
