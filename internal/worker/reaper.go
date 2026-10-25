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

// reaperErrorLogEvery limits how often a failing reaper pass is logged.
const reaperErrorLogEvery = 30 * time.Second

// maxRetryDelayLimit keeps the configured retry delay cap far below any
// overflow or day-long wait that would look like a lost job.
const maxRetryDelayLimit = 24 * time.Hour

// Reaper recovers jobs whose workers stopped renewing their leases: a job
// claimed by a worker that crashed or lost its network must not stay running
// forever.
type Reaper struct {
	store    store.Store
	interval time.Duration
	batch    int
	backoff  time.Duration
	wg       sync.WaitGroup
	metrics  *metrics.Metrics // optional; nil records nothing
}

// UseMetrics makes the reaper count its recoveries. Call it before Start.
func (r *Reaper) UseMetrics(m *metrics.Metrics) { r.metrics = m }

// NewReaper validates its settings and creates a reaper. It starts nothing.
// backoff is how long a recovered job waits before it can be claimed again; it
// is for work lost to a dead worker, not a handler retry policy.
func NewReaper(jobStore store.Store, interval time.Duration, batch int, backoff time.Duration) (*Reaper, error) {
	switch {
	case jobStore == nil:
		return nil, errors.New("reaper needs a store")
	case interval <= 0:
		return nil, fmt.Errorf("reaper interval must be positive, got %s", interval)
	case batch < 1:
		return nil, fmt.Errorf("reaper batch size must be at least 1, got %d", batch)
	case backoff <= 0:
		return nil, fmt.Errorf("recovery backoff must be positive, got %s", backoff)
	}
	return &Reaper{store: jobStore, interval: interval, batch: batch, backoff: backoff}, nil
}

// Start launches the reaper loop and returns immediately. Call it once.
func (r *Reaper) Start(ctx context.Context) {
	r.wg.Add(1)
	go r.run(ctx)
}

// Wait blocks until the loop has exited (after its context is cancelled).
func (r *Reaper) Wait() { r.wg.Wait() }

// WaitFor is Wait with a time limit; it reports whether the loop exited.
func (r *Reaper) WaitFor(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		r.Wait()
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

// ValidateLeaseTiming checks the relationship the lease protocol depends on:
// heartbeat < safety margin < lease. It runs in every execution mode, because
// the timings also describe remote claims.
func ValidateLeaseTiming(lease, heartbeat, safety time.Duration) error {
	switch {
	case lease <= 0 || heartbeat <= 0 || safety <= 0:
		return fmt.Errorf("lease duration, heartbeat interval and safety margin must all be positive, got %s, %s, %s", lease, heartbeat, safety)
	case heartbeat >= safety:
		return fmt.Errorf("heartbeat interval (%s) must be shorter than the lease safety margin (%s)", heartbeat, safety)
	case safety >= lease:
		return fmt.Errorf("lease safety margin (%s) must be shorter than the lease duration (%s)", safety, lease)
	}
	return nil
}

// ValidateRetryDelays checks the retry backoff bounds.
func ValidateRetryDelays(base, max time.Duration) error {
	switch {
	case base <= 0 || max <= 0:
		return fmt.Errorf("retry base delay and maximum delay must be positive, got %s and %s", base, max)
	case base > max:
		return fmt.Errorf("retry base delay (%s) must not exceed the maximum delay (%s)", base, max)
	case max > maxRetryDelayLimit:
		return fmt.Errorf("retry maximum delay (%s) must not exceed %s", max, maxRetryDelayLimit)
	}
	return nil
}

// run recovers jobs whose workers stopped renewing their leases. One
// loop per process; passes never overlap. The SQL is safe if several processes
// ever run it at once.
func (r *Reaper) run(ctx context.Context) {
	defer r.wg.Done()
	var lastErrLog time.Time
	pass := func() {
		rctx, cancel := context.WithTimeout(ctx, r.interval+5*time.Second)
		res, err := r.store.ReapExpired(rctx, r.batch, r.backoff)
		cancel()
		switch {
		case err != nil && ctx.Err() != nil:
			// shutting down; the pass was interrupted
		case err != nil:
			// A database outage would otherwise log on every pass.
			if time.Since(lastErrLog) > reaperErrorLogEvery {
				lastErrLog = time.Now()
				log.Printf("reaper: pass failed (further failures are logged at most every %s): %s", reaperErrorLogEvery, DescribeDBError(err))
			}
		case res.Requeued > 0 || res.Exhausted > 0:
			r.metrics.Reaped(res.Requeued, res.Exhausted)
			log.Printf("reaper: recovered %d job(s) with expired leases (%d queued again, %d failed with no attempts left)",
				res.Requeued+res.Exhausted, res.Requeued, res.Exhausted)
		}
	}

	pass() // right away: leases that expired while Forge was down
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass()
		}
	}
}
