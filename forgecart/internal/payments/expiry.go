package payments

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// An order that is created but never paid holds its stock (status pending_payment, inventory
// reserved) for ever. The expirer cancels such orders after the operator's expiry time and
// returns their stock.
//
// It must not simply flip the order: a payment may be in flight, or a Razorpay customer may
// still be able to pay. So for each stale order it first asks the payment service to close the
// order's payment (Client.cancel). The service answers with the order's final payment if there
// already is one (a payment that succeeded, or was declined: that is applied, as the sweeper
// would), or records a decline that closes the order, after which no payment for it is accepted
// (for Razorpay it also cancels a link that could still be paid). Only then is the order
// cancelled here. Closing is idempotent, so a crash anywhere in between is repaired by the
// next pass.
const (
	expiryBatch      = 100 // orders per query
	expiryMaxBatches = 20  // batches per pass, so a large backlog is worked off over several passes
)

// ExpireStale runs one pass. It reports how many stale orders it looked at, how many it
// cancelled, and how many it instead completed because the payment service already had a final
// payment for them.
func (s *Service) ExpireStale(ctx context.Context, expiry time.Duration) (checked, expired, repaired int, err error) {
	var lastAt time.Time
	var lastID string
	for batch := 0; batch < expiryMaxBatches; batch++ {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, total_minor, currency, created_at FROM orders
			WHERE status = 'pending_payment' AND created_at < now() - $1::interval AND (created_at, id) > ($2::timestamptz, $3::text)
			ORDER BY created_at, id LIMIT $4`, expiry.String(), lastAt, lastID, expiryBatch)
		if err != nil {
			return checked, expired, repaired, err
		}
		type stale struct {
			id       string
			total    int64
			currency string
			created  time.Time
		}
		var orders []stale
		for rows.Next() {
			var o stale
			if err := rows.Scan(&o.id, &o.total, &o.currency, &o.created); err != nil {
				rows.Close()
				return checked, expired, repaired, err
			}
			orders = append(orders, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return checked, expired, repaired, err
		}
		for _, o := range orders {
			lastAt, lastID = o.created, o.id
			checked++
			pr, err := s.client.cancel(ctx, payRequest{OrderID: o.id, AmountMinor: o.total, Currency: o.currency})
			if err != nil {
				if errors.Is(err, errMismatch) {
					continue // the two services disagree about this order: leave it for a person
				}
				return checked, expired, repaired, err
			}
			applied, err := s.applyFinal(ctx, o.id, pr.AttemptID, pr.Outcome, pr.Expired)
			if err != nil {
				return checked, expired, repaired, err
			}
			switch {
			case applied && pr.Expired:
				expired++
			case applied:
				repaired++
			}
		}
		if len(orders) < expiryBatch {
			break
		}
	}
	return checked, expired, repaired, nil
}

// RunExpirer runs ExpireStale every interval until ctx is cancelled. One goroutine, one pass at
// a time; several shop processes may run it, because every step is idempotent.
func (s *Service) RunExpirer(ctx context.Context, interval, expiry time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		checked, expired, repaired, err := s.ExpireStale(ctx, expiry)
		if s.OnExpire != nil && ctx.Err() == nil {
			s.OnExpire(expired, repaired, err)
		}
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("order expiry failed", "cause", sweepErrClass(err), "checked", checked, "expired", expired)
		case expired > 0 || repaired > 0:
			log.Info("order expiry", "expired", expired, "completed_from_payment_service", repaired, "checked", checked)
		}
	}
}
