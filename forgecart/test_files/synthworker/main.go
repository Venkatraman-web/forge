// Command synthworker is the worker used by the Forge-isolated capacity
// experiment (E-1). It runs the real SDK runtime (forge/sdk/workerruntime)
// against Forge's worker API with a synthetic handler: it decodes the job input,
// waits a fixed time, and returns a fixed result. It touches no database, calls
// no other service and has no business logic, so what is measured is Forge's
// claim, lease, completion and scheduling path.
//
// Settings (environment, all required unless a default is shown):
//
//	SW_URL          Forge worker API base URL
//	SW_KEY          worker API key
//	SW_NAME         worker name (letters, digits, . _ -)
//	SW_CONCURRENCY  slots in this process (default 4)
//	SW_HANDLER_MS   handler duration in milliseconds (default 25)
//	SW_TYPES        comma-separated job types to handle
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Venkatraman-web/forge/sdk/workerclient"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

type input struct {
	Seq int    `json:"seq"`
	Run string `json:"run"`
	Pad string `json:"pad"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "synthworker:", err)
		os.Exit(1)
	}
}

func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative whole number", name)
	}
	return n, nil
}

func run() error {
	conc, err := envInt("SW_CONCURRENCY", 4)
	if err != nil {
		return err
	}
	ms, err := envInt("SW_HANDLER_MS", 25)
	if err != nil {
		return err
	}
	var types []string
	for _, t := range strings.Split(os.Getenv("SW_TYPES"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}
	client, err := workerclient.New(workerclient.Config{BaseURL: os.Getenv("SW_URL"), APIKey: os.Getenv("SW_KEY")})
	if err != nil {
		return fmt.Errorf("invalid SW_URL or SW_KEY: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	w, err := workerruntime.New(workerruntime.Config{Client: client, Name: os.Getenv("SW_NAME"), Concurrency: conc, Logger: log})
	if err != nil {
		return err
	}
	delay := time.Duration(ms) * time.Millisecond
	handler := func(ctx context.Context, job workerruntime.Job) (any, error) {
		var in input
		if err := job.Decode(&in); err != nil {
			return nil, workerruntime.Permanent(err)
		}
		if delay > 0 {
			t := time.NewTimer(delay)
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return map[string]bool{"ok": true}, nil
	}
	for _, t := range types {
		if err := w.Handle(t, handler); err != nil {
			return fmt.Errorf("register %s: %w", t, err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("synthworker configured", "types", types, "concurrency", conc, "handler_ms", ms)
	return w.Run(ctx)
}
