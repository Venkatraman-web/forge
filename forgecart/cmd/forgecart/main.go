// Command forgecart is the ForgeCart reference application's catalog service.
//
//	forgecart        serve the HTTP API
//	forgecart seed   insert the starter catalog (safe to repeat), then exit
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Venkatraman-web/forge/forgecart/internal/appdb"
	"github.com/Venkatraman-web/forge/forgecart/internal/catalog"
	"github.com/Venkatraman-web/forge/forgecart/internal/config"
	"github.com/Venkatraman-web/forge/forgecart/internal/httpapi"
	"github.com/Venkatraman-web/forge/forgecart/internal/logging"
	"github.com/Venkatraman-web/forge/forgecart/internal/payments"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "forgecart:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "seed") {
		return errors.New("usage: forgecart [seed]")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := appdb.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer db.Close()
	log, err := logging.New(os.Stdout)
	if err != nil {
		return err
	}

	if len(args) == 1 {
		n, err := catalog.Seed(ctx, db)
		if err != nil {
			return errors.New("seeding failed (has the migration been applied?)")
		}
		log.Info("seed complete", "inserted", n)
		return nil
	}

	paySvc := payments.NewService(db, payments.NewClient(cfg.PaymentURL, cfg.PaymentKey))
	api := httpapi.New(db, log, paySvc)
	paySvc.OnPaymentCall, paySvc.OnSweep, paySvc.OnExpire = api.ObservePaymentCall, api.ObserveSweep, api.ObserveExpire
	// The sweeper repairs payments recorded by the payment service but never applied to
	// their order; see package payments. One goroutine, stopped with the process.
	sweepDone := make(chan struct{})
	if cfg.SweepInterval > 0 {
		go func() {
			defer close(sweepDone)
			paySvc.RunSweeper(ctx, cfg.SweepInterval, cfg.SweepAge, log)
		}()
	} else {
		close(sweepDone)
	}
	// The expirer cancels orders that stay unpaid and returns their stock.
	expireDone := make(chan struct{})
	if cfg.ExpiryAfter > 0 {
		go func() {
			defer close(expireDone)
			paySvc.RunExpirer(ctx, cfg.ExpiryInterval, cfg.ExpiryAfter, log)
		}()
	} else {
		close(expireDone)
	}
	srv := httpapi.NewServer(cfg.Addr, api.Handler())
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("forgecart listening", "addr", cfg.Addr)
	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" {
		// Its own listener and key: the numbers describe the business and must
		// not be readable through the shop's public address.
		metricsSrv = httpapi.NewServer(cfg.MetricsAddr, api.MetricsHandler(cfg.MetricsKey))
		go func() { errc <- metricsSrv.ListenAndServe() }()
		log.Info("forgecart metrics listening", "addr", cfg.MetricsAddr)
	}

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("http shutdown incomplete")
	}
	<-sweepDone // the sweeper and the expirer use the database: wait for them before closing it
	<-expireDone
	log.Info("forgecart stopped")
	return nil
}
