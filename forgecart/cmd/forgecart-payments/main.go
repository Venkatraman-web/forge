// Command forgecart-payments is ForgeCart's payment service: its own process and
// database, called by the shop (order service) over HTTP.
//
//	forgecart-payments    serve the HTTP API
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
	"github.com/Venkatraman-web/forge/forgecart/internal/config"
	"github.com/Venkatraman-web/forge/forgecart/internal/logging"
	"github.com/Venkatraman-web/forge/forgecart/internal/paymentsvc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forgecart-payments:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadPaymentService()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := appdb.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return errors.New("cannot reach the payment database (check " + config.PaymentsDatabaseURLEnv + " and that PostgreSQL is running)")
	}
	defer db.Close()
	log, err := logging.New(os.Stdout)
	if err != nil {
		return err
	}

	svc := paymentsvc.NewService(ctx, db, cfg.Payment)
	api := paymentsvc.NewAPI(db, svc, cfg.APIKey, log)
	srv := paymentsvc.NewServer(cfg.Addr, api.Handler())
	errc := make(chan error, 3)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("forgecart payment service listening", "addr", cfg.Addr, "provider", cfg.Payment.Provider)
	// With Razorpay, the poller settles links that neither the webhook nor the customer asked about.
	pollDone := make(chan struct{})
	if cfg.Payment.Provider == paymentsvc.ProviderRazorpay && cfg.PollInterval > 0 {
		svc.OnPoll = api.ObservePoll
		go func() {
			defer close(pollDone)
			svc.RunPoller(ctx, cfg.PollInterval, cfg.PollBatch, 2*time.Hour, log)
		}()
	} else {
		close(pollDone)
	}
	var webhookSrv *http.Server
	if cfg.WebhookAddr != "" {
		// The one listener the internet has to reach; requests are accepted only with Razorpay's signature.
		webhookSrv = paymentsvc.NewServer(cfg.WebhookAddr, paymentsvc.NewWebhook(api, cfg.WebhookSecret).Handler())
		go func() { errc <- webhookSrv.ListenAndServe() }()
		log.Info("forgecart payment service webhook listening", "addr", cfg.WebhookAddr)
	}
	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" {
		// Its own listener and key: the numbers describe the business.
		metricsSrv = paymentsvc.NewServer(cfg.MetricsAddr, api.MetricsHandler(cfg.MetricsKey))
		go func() { errc <- metricsSrv.ListenAndServe() }()
		log.Info("forgecart payment service metrics listening", "addr", cfg.MetricsAddr)
	}

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if webhookSrv != nil {
		_ = webhookSrv.Shutdown(shutdownCtx)
	}
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("http shutdown incomplete")
	}
	<-pollDone // the poller uses the database: wait for it before closing it
	log.Info("forgecart payment service stopped")
	return nil
}
