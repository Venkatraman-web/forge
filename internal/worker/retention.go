package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Venkatraman-web/forge/internal/metrics"
	"github.com/Venkatraman-web/forge/internal/store"
)

// MinRetention keeps a typo (seconds instead of days) from deleting results a
// client has not had a chance to read.
const MinRetention = time.Minute

const (
	// purgeBatch is how many jobs one DELETE removes: small enough to stay a short
	// statement, large enough that a backlog is cleared quickly.
	purgeBatch = 1000
	// purgeMaxBatches bounds one pass (100,000 jobs), so a large backlog is worked off
	// over several passes and the loop still notices shutdown.
	purgeMaxBatches = 100
)

// Retention deletes finished jobs after the operator's retention time. Without it
// every job stays in the database forever: its row is both the queue entry and the
// record a client reads with GET /v1/jobs/{id}, so nothing else would ever delete it.
//
// What a deleted job loses: GET /v1/jobs/{id} answers 404 for it, and its
// Idempotency-Key is forgotten (the same key submitted again creates a new job). Queued
// and running jobs are never deleted.
type Retention struct {
	store     store.Store
	interval  time.Duration
	succeeded time.Duration // 0 keeps succeeded jobs forever
	failed    time.Duration // 0 keeps failed jobs forever
	wg        sync.WaitGroup
	metrics   *metrics.Metrics // optional; nil records nothing
}

// UseMetrics makes the purge count what it deletes. Call it before Start.
func (r *Retention) UseMetrics(m *metrics.Metrics) { r.metrics = m }

// NewRetention validates its settings and creates the purge. It starts nothing. A
// retention of 0 keeps that status forever; at least one must be set, otherwise there
// is nothing to run.
func NewRetention(jobStore store.Store, interval, succeeded, failed time.Duration) (*Retention, error) {
	switch {
	case jobStore == nil:
		return nil, errors.New("retention needs a store")
	case interval <= 0:
		return nil, fmt.Errorf("retention interval must be positive, got %s", interval)
	case succeeded < 0 || failed < 0 || (succeeded > 0 && succeeded < MinRetention) || (failed > 0 && failed < MinRetention):
		return nil, fmt.Errorf("a retention time must be 0 (keep forever) or at least %s", MinRetention)
	case succeeded == 0 && failed == 0:
		return nil, errors.New("retention needs a time for succeeded or failed jobs")
	}
	return &Retention{store: jobStore, interval: interval, succeeded: succeeded, failed: failed}, nil
}

// Start launches the loop and returns immediately. Call it once.
func (r *Retention) Start(ctx context.Context) {
	r.wg.Add(1)
	go r.run(ctx)
}

// WaitFor waits for the loop to exit (after its context is cancelled) and reports
// whether it did within d.
func (r *Retention) WaitFor(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// pass deletes batches until a batch comes back short or the pass limit is reached.
func (r *Retention) pass(ctx context.Context) (store.PurgeResult, error) {
	var total store.PurgeResult
	for i := 0; i < purgeMaxBatches; i++ {
		var succeededBefore, failedBefore *time.Time
		now := time.Now()
		if r.succeeded > 0 {
			t := now.Add(-r.succeeded)
			succeededBefore = &t
		}
		if r.failed > 0 {
			t := now.Add(-r.failed)
			failedBefore = &t
		}
		res, err := r.store.PurgeFinished(ctx, succeededBefore, failedBefore, purgeBatch)
		total.Succeeded += res.Succeeded
		total.Failed += res.Failed
		if err != nil {
			return total, err
		}
		if res.Succeeded+res.Failed < purgeBatch || ctx.Err() != nil {
			break
		}
	}
	return total, nil
}

// run is one loop per process; passes never overlap. The SQL is safe if several
// processes run it at once.
func (r *Retention) run(ctx context.Context) {
	defer r.wg.Done()
	var lastErrLog time.Time
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		res, err := r.pass(ctx)
		if res.Succeeded+res.Failed > 0 {
			r.metrics.Purged(res.Succeeded, res.Failed)
			log.Printf("retention: deleted %d finished job(s) (%d succeeded, %d failed)", res.Succeeded+res.Failed, res.Succeeded, res.Failed)
		}
		if err != nil && ctx.Err() == nil && time.Since(lastErrLog) > reaperErrorLogEvery {
			lastErrLog = time.Now()
			log.Printf("retention: pass failed (further failures are logged at most every %s): %s", reaperErrorLogEvery, DescribeDBError(err))
		}
	}
}
