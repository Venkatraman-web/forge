// Package workerapp is the shared main() of ForgeCart's worker commands: load
// configuration, open the ForgeCart database, connect to Forge's worker API and
// run one registered handler until SIGINT/SIGTERM.
package workerapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Venkatraman-web/forge/forgecart/internal/appdb"
	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/forgecart/internal/config"
	"github.com/Venkatraman-web/forge/forgecart/internal/logging"
	"github.com/Venkatraman-web/forge/sdk/workerclient"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

// Main runs a worker for one job type and exits the process on failure.
func Main(command, defaultName, jobType string, newHandler func(bizjob.Databases) workerruntime.HandlerFunc) {
	if err := run(defaultName, jobType, newHandler); err != nil {
		fmt.Fprintln(os.Stderr, command+":", err)
		os.Exit(1)
	}
}

func run(defaultName, jobType string, newHandler func(bizjob.Databases) workerruntime.HandlerFunc) error {
	cfg, err := config.LoadWorker(defaultName)
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
	dbs := bizjob.Databases{Shop: db, Results: db}
	if cfg.ResultsDatabaseURL != "" {
		if dbs.Results, err = appdb.Open(ctx, cfg.ResultsDatabaseURL, appdb.DefaultMaxConns); err != nil {
			return fmt.Errorf("results database: %w", err)
		}
		defer dbs.Results.Close()
	}

	client, err := workerclient.New(workerclient.Config{BaseURL: cfg.ForgeURL, APIKey: cfg.APIKey})
	if err != nil {
		return errors.New("invalid Forge worker API settings (check " + config.ForgeWorkerURLEnv + " and " + config.ForgeWorkerKeyEnv + ")")
	}
	log, err := logging.New(os.Stdout)
	if err != nil {
		return err
	}
	w, err := workerruntime.New(workerruntime.Config{Client: client, Name: cfg.Name, Concurrency: cfg.Concurrency, PollWait: cfg.PollWait, ShutdownGrace: cfg.ShutdownGrace, Logger: log})
	if err != nil {
		return fmt.Errorf("worker setup: %w", err)
	}
	var opts []workerruntime.HandleOption
	if cfg.JobTimeout > 0 {
		opts = append(opts, workerruntime.WithTimeout(cfg.JobTimeout))
	}
	handler := newHandler(dbs)
	if cfg.MetricsAddr != "" {
		// Its own listener and key: the numbers describe the business, like the shop's.
		wm := newWorkerMetrics(jobType, cfg.Concurrency, dbs.Shop, dbs.Results)
		handler = wm.wrap(handler)
		srv := &http.Server{Addr: cfg.MetricsAddr, Handler: wm.handler(cfg.MetricsKey), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("worker metrics listener stopped", "cause", "listen")
			}
		}()
		defer srv.Close()
	}
	if err := w.Handle(jobType, handler, opts...); err != nil {
		return fmt.Errorf("register handler: %w", err)
	}
	log.Info("worker started", "job_type", jobType, "concurrency", cfg.Concurrency,
		"poll_wait", cfg.PollWait.String(), "shutdown_grace", cfg.ShutdownGrace.String(), "job_timeout", cfg.JobTimeout.String())
	go reportWedged(ctx, w, log)
	return w.Run(ctx)
}

// reportWedged logs, whenever it changes, how many job slots are held by
// handlers that ignored cancellation. Go cannot stop such a handler, so its slot
// stays busy until it returns; without this line it would go unnoticed.
func reportWedged(ctx context.Context, w *workerruntime.Worker, log *slog.Logger) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	last := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := w.Wedged(); n != last {
				log.Warn("worker slots held by handlers that ignored cancellation", "wedged_slots", n)
				last = n
			}
		}
	}
}
