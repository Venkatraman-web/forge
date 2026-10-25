package payments

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// The order and the payment are in two databases, so a crash between the payment
// service recording an attempt and Pay applying it to the order leaves a recorded
// payment and a pending order. Repeating the request repairs it, but a client that
// never comes back would leave the order (and its reserved stock) stuck. The sweeper
// is the repair that does not depend on the client: it walks the payment service's
// final attempts in the order they were completed, from a cursor kept in the shop's
// database, and applies those whose order is still pending exactly as Pay would.
// Walking payments, not pending orders, keeps the repair time independent of how
// many orders were abandoned unpaid. Applying is idempotent, so it is harmless if
// Pay or another shop process gets there first.
const (
	sweepPage     = 500 // attempts per request; the payment service's limit
	sweepMaxPages = 20  // pages per pass, so one pass stays short after a long pause
)

// Sweep runs one pass: it examines the final attempts completed more than lag ago
// that are past the cursor, applies the unapplied ones, and moves the cursor. It
// reports how many attempts it examined and how many orders it repaired.
func (s *Service) Sweep(ctx context.Context, lag time.Duration) (checked, repaired int, err error) {
	var at time.Time
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT completed_at, attempt_id FROM payment_sweep_cursor`).Scan(&at, &id); err != nil {
		return 0, 0, err
	}
	for page := 0; page < sweepMaxPages; page++ {
		finals, err := s.client.finals(ctx, at, id, lag, sweepPage)
		if err != nil {
			return checked, repaired, err
		}
		if len(finals) == 0 {
			break
		}
		orderIDs := make([]string, len(finals))
		for i, f := range finals {
			orderIDs[i] = f.OrderID
		}
		pending, err := s.pendingAmong(ctx, orderIDs)
		if err != nil {
			return checked, repaired, err
		}
		for _, f := range finals {
			if !pending[f.OrderID] {
				continue
			}
			applied, err := s.applyFinal(ctx, f.OrderID, f.AttemptID, f.Outcome, f.Expired)
			if err != nil {
				return checked, repaired, err
			}
			if applied {
				repaired++
			}
		}
		last := finals[len(finals)-1]
		at, id = last.CompletedAt, last.AttemptID
		if _, err := s.db.ExecContext(ctx, `UPDATE payment_sweep_cursor SET completed_at = $1, attempt_id = $2`, at, id); err != nil {
			return checked, repaired, err
		}
		checked += len(finals)
		if len(finals) < sweepPage {
			break
		}
	}
	return checked, repaired, nil
}

// pendingAmong returns which of the orders are still pending payment.
func (s *Service) pendingAmong(ctx context.Context, ids []string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM orders WHERE id = ANY($1::text[]) AND status = 'pending_payment'`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// RunSweeper sweeps every interval until ctx is cancelled. It is one goroutine and
// one pass at a time.
func (s *Service) RunSweeper(ctx context.Context, interval, lag time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		checked, repaired, err := s.Sweep(ctx, lag)
		if s.OnSweep != nil && ctx.Err() == nil {
			s.OnSweep(repaired, err)
		}
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("payment sweep failed", "cause", sweepErrClass(err), "checked", checked)
		case repaired > 0:
			// A repair means an earlier request died between the two steps: worth seeing.
			log.Warn("payment sweep applied payments that were recorded but never applied", "repaired", repaired, "checked", checked)
		}
	}
}

func sweepErrClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout_or_canceled"
	}
	if errors.Is(err, ErrUnavailable) {
		return "payment_service_unavailable"
	}
	return "error"
}
