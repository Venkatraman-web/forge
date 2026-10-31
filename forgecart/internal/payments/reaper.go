package payments

import (
	"context"
	"log/slog"
	"time"

	"github.com/Venkatraman-web/forge/forgecart/internal/inventory"
)

// reapBatch is how many reservations one reaper pass looks at.
const reapBatch = 2000

// releaseChunk is how many reservations one release transaction gives back: a few hundred keeps the
// product locks it holds short, and one commit per chunk instead of one per reservation is what lets
// the reaper clear tens of thousands left by an outage in minutes.
const releaseChunk = 200

// ReapReservations makes one full sweep over the stock reservations that stayed 'reserved' longer than
// age. The checkout saga leaves one behind when the process dies between the reservation and the order,
// and a declined or expired order leaves one behind when the release after its state change failed
// (docs/decisions/0005-inventory-service-and-checkout-saga.md). For each:
//
//   - the order is paid: mark the reservation committed (the reaper stops looking at it);
//   - the order is pending payment: leave it, it still holds the stock legitimately;
//   - the order does not exist, or is payment_failed or cancelled: release the stock.
//
// It walks the list in batches with a cursor, so a long list of reservations it can do nothing about
// (orders awaiting payment) cannot hide the ones behind them, and returns what it did.
func (s *Service) ReapReservations(ctx context.Context, age time.Duration) (checked, committed, released int, err error) {
	var cursor inventory.Reserved
	for {
		batch, err := s.inv.Stale(ctx, age, cursor, reapBatch)
		if err != nil {
			return checked, committed, released, err
		}
		if len(batch) == 0 {
			return checked, committed, released, nil
		}
		cursor = batch[len(batch)-1]
		checked += len(batch)
		ids := make([]string, len(batch))
		for i, r := range batch {
			ids[i] = r.OrderID
		}
		status := make(map[string]string, len(ids))
		rows, err := s.db.QueryContext(ctx, `SELECT id, status FROM orders WHERE id = ANY($1::text[])`, ids)
		if err != nil {
			return checked, committed, released, err
		}
		for rows.Next() {
			var id, st string
			if err := rows.Scan(&id, &st); err != nil {
				rows.Close()
				return checked, committed, released, err
			}
			status[id] = st
		}
		if err := rows.Err(); err != nil {
			return checked, committed, released, err
		}
		rows.Close()
		var paid, toRelease []string
		for _, id := range ids {
			switch status[id] {
			case "paid":
				paid = append(paid, id)
			case "pending_payment":
			default: // no such order, payment_failed or cancelled
				toRelease = append(toRelease, id)
			}
		}
		for len(toRelease) > 0 {
			n := min(releaseChunk, len(toRelease))
			done, err := s.inv.ReleaseMany(ctx, toRelease[:n])
			if err != nil {
				return checked, committed, released, err
			}
			released += done
			toRelease = toRelease[n:]
		}
		if len(paid) > 0 {
			n, err := s.inv.Commit(ctx, paid)
			if err != nil {
				return checked, committed, released, err
			}
			committed += n
		}
		if len(batch) < reapBatch {
			return checked, committed, released, nil
		}
	}
}

// RunReaper calls ReapReservations every interval until ctx ends.
func (s *Service) RunReaper(ctx context.Context, interval, age time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		checked, committed, released, err := s.ReapReservations(ctx, age)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("reservation reaper failed", "cause", sweepErrClass(err), "checked", checked, "committed", committed, "released", released)
		case released > 0:
			log.Info("reservation reaper released stock", "released", released, "committed", committed, "checked", checked)
		}
	}
}
