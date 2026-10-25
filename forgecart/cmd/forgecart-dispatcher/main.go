// Command forgecart-dispatcher publishes ForgeCart's outbox events to Forge as
// jobs. It is a separate process from the ForgeCart API; run as many replicas
// as you like against the same database.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Venkatraman-web/forge/forgecart/internal/appdb"
	"github.com/Venkatraman-web/forge/forgecart/internal/config"
	"github.com/Venkatraman-web/forge/forgecart/internal/dispatcher"
	"github.com/Venkatraman-web/forge/forgecart/internal/logging"
	"github.com/Venkatraman-web/forge/forgecart/internal/outbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forgecart-dispatcher:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadDispatcher()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := appdb.Open(ctx, cfg.DatabaseURL, appdb.DefaultMaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	log, err := logging.New(os.Stdout)
	if err != nil {
		return err
	}
	d, err := dispatcher.New(cfg.Dispatcher, outbox.NewStore(db), log)
	if err != nil {
		return err
	}
	log.Info("dispatcher started", "batch_size", cfg.Dispatcher.BatchSize, "lease", cfg.Dispatcher.Lease.String(), "poll", cfg.Dispatcher.PollInterval.String())
	if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
