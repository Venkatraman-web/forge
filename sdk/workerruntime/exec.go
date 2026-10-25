package workerruntime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Venkatraman-web/forge/sdk/workerclient"
)

const (
	// reportTimeout bounds one outcome request. Outcome requests use a context
	// detached from shutdown, because the outcomes worth reporting often happen
	// exactly when Run's context has ended.
	reportTimeout = 10 * time.Second
	// reportPause separates two attempts of one outcome request.
	reportPause = 300 * time.Millisecond
	// wedgedAfter is how long a handler may keep running after being cancelled
	// before it counts as wedged.
	wedgedAfter = 2 * time.Second
	// wedgedLogEvery repeats the wedged log line.
	wedgedLogEvery = 30 * time.Second
	// minPollGap is the shortest time between idle claims.
	minPollGap = 500 * time.Millisecond
)

// Why the runtime cancelled a handler (the context cause, for debugging).
var (
	errCauseDeadline  = errors.New("workerruntime: execution deadline reached")
	errCauseShutdown  = errors.New("workerruntime: worker shutting down")
	errCauseLost      = errors.New("workerruntime: claim no longer owned")
	errCauseSelfFence = errors.New("workerruntime: lease can no longer be trusted")
)

func causeErr(c cause) error {
	switch c {
	case causeDeadline:
		return errCauseDeadline
	case causeShutdown:
		return errCauseShutdown
	case causeLost:
		return errCauseLost
	}
	return errCauseSelfFence
}

// checkTimings refuses a claim whose lease timings could not be honoured
// safely: heartbeat < safety margin < lease, and a positive deadline. The
// runtime does not invent replacements.
func checkTimings(c workerclient.ClaimedJob) error {
	if c.HeartbeatInterval <= 0 || c.SafetyMargin <= c.HeartbeatInterval || c.Lease <= c.SafetyMargin || c.Deadline <= 0 {
		return errors.New("workerruntime: the server sent lease timings that cannot be honoured safely (need heartbeat < safety margin < lease, deadline > 0)")
	}
	return nil
}

// handlerResult is what the handler goroutine hands back.
type handlerResult struct {
	result   any
	err      error
	panicked bool
}

// jobRun is the state of one claimed job. Everything the heartbeat, the
// watchdog, the deadline timer, shutdown and the handler goroutine decide is
// recorded here under mu, so they cannot overwrite each other.
type jobRun struct {
	w     *Worker
	claim workerclient.ClaimedJob // private: carries the generation and worker ID
	job   Job                     // the handler's view
	log   *slog.Logger

	cancelH   context.CancelCauseFunc // cancels the handler's context
	leaseCtx  context.Context         // lives until the outcome is decided
	stopLease context.CancelFunc
	wg        sync.WaitGroup // heartbeat and watchdog
	wdStop    chan struct{}  // closed when the handler returns
	wdOnce    sync.Once
	cleanup   []func() // stops timers and the shutdown hook

	mu         sync.Mutex
	returned   bool  // the handler goroutine has finished
	lost       bool  // Forge said the claim is no longer ours (409 on renew)
	selfFenced bool  // no renewal succeeded within the safety margin
	first      cause // first of causeShutdown / causeDeadline to fire
	cancelled  bool  // some cancellation has been issued
	wedged     bool
	lastOK     time.Time // monotonic: the claim response arrival, or the latest renewal REQUEST send time
}

// runJob runs one claimed job to a decided outcome. It returns true only when
// the handler was still running at the end of ShutdownGrace: the job is then
// detached, its heartbeat keeps running and a background goroutine will finish
// the job when (if) the handler returns.
func (w *Worker) runJob(ctx context.Context, slot int, h HandlerFunc, claim workerclient.ClaimedJob, receivedAt time.Time) (detached bool) {
	jr := &jobRun{w: w, claim: claim, job: newJob(claim), wdStop: make(chan struct{}), lastOK: receivedAt}
	jr.log = w.log.With("slot", slot, "job_id", claim.JobID, "type", claim.Type, "attempt", claim.Attempt, "max_attempts", claim.MaxAttempts)
	hctx, cancelH := context.WithCancelCause(context.Background())
	jr.cancelH = cancelH
	jr.leaseCtx, jr.stopLease = context.WithCancel(context.Background())

	stopShutdownHook := context.AfterFunc(ctx, func() { jr.fire(causeShutdown) })
	remaining := claim.Deadline - time.Since(receivedAt)
	if remaining < 0 {
		remaining = 0
	}
	deadlineTimer := time.AfterFunc(remaining, func() { jr.fire(causeDeadline) })
	jr.cleanup = []func(){func() { stopShutdownHook() }, func() { deadlineTimer.Stop() }, func() { cancelH(nil) }}

	jr.log.Info("job_started", "deadline", claim.Deadline.String())
	jr.wg.Add(2)
	go jr.heartbeat()
	go jr.watchdog()

	done := make(chan handlerResult, 1)
	go func() {
		var r handlerResult
		completed := false
		defer func() {
			if !completed {
				recover() // a panic: the value is deliberately discarded
				r = handlerResult{panicked: true}
			}
			jr.mu.Lock()
			jr.returned = true
			// Freshness check at the moment of return. If the process was paused
			// (SIGSTOP, a VM freeze, a long GC or scheduling stall) its timers
			// all fire together on resume, and the handler can return before the
			// watchdog's next tick. The monotonic clock kept running meanwhile, so
			// "no successful renewal for longer than the safety margin" is still
			// visible here, and the claim is treated exactly as the watchdog would
			// have: self-fenced, nothing is reported.
			staleOnReturn := false
			if !jr.lost && !jr.selfFenced && time.Since(jr.lastOK) > jr.claim.SafetyMargin {
				jr.selfFenced = true
				staleOnReturn = true
			}
			if jr.wedged {
				jr.wedged = false
				w.wedged.Add(-1)
			}
			jr.mu.Unlock()
			if staleOnReturn {
				jr.log.Warn("self_fenced_on_handler_return", "reason", "no successful renewal within the safety margin")
			}
			jr.wdOnce.Do(func() { close(jr.wdStop) })
			done <- r
		}()
		r.result, r.err = h(hctx, jr.job)
		completed = true
	}()

	var res handlerResult
	select {
	case res = <-done:
	case <-ctx.Done():
		// Shutdown: the handler was cancelled by the hook above. Give it the
		// grace period to return.
		timer := time.NewTimer(w.shutdownGrace)
		select {
		case res = <-done:
			timer.Stop()
		case <-timer.C:
			jr.markWedged()
			jr.log.Error("shutdown_grace_expired_handler_still_running")
			// Detach: the heartbeat (still bound to leaseCtx) keeps the lease
			// alive; this goroutine finishes the job if the handler returns.
			go func() { jr.resolve(<-done) }()
			return true
		}
	}
	jr.resolve(res)
	return false
}

// fire records a cancellation reason and cancels the handler's context. Lost
// and self-fence override everything else when the outcome is classified; of
// shutdown and deadline the first to fire is kept. Once the handler has
// returned, nothing can change its outcome, so late signals are ignored.
func (jr *jobRun) fire(c cause) {
	jr.mu.Lock()
	if jr.returned {
		jr.mu.Unlock()
		if c == causeLost {
			jr.log.Info("renewal_conflict_after_handler_returned")
		}
		return
	}
	switch c {
	case causeLost:
		jr.lost = true
	case causeSelfFence:
		jr.selfFenced = true
	default:
		if jr.first == causeNone {
			jr.first = c
		}
	}
	firstCancel := !jr.cancelled
	jr.cancelled = true
	jr.mu.Unlock()

	jr.cancelH(causeErr(c))
	switch c {
	case causeLost:
		jr.log.Warn("ownership_lost_handler_cancelled")
	case causeSelfFence:
		jr.log.Warn("self_fenced_handler_cancelled")
	case causeDeadline:
		jr.log.Warn("deadline_reached_handler_cancelled")
	case causeShutdown:
		jr.log.Info("shutdown_handler_cancelled")
	}
	if firstCancel {
		time.AfterFunc(wedgedAfter, jr.markWedged)
	}
}

// causeNow is the cancellation reason as of now; after the handler returns it
// no longer changes.
func (jr *jobRun) causeNow() cause {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	switch {
	case jr.lost:
		return causeLost
	case jr.selfFenced:
		return causeSelfFence
	}
	return jr.first
}

// markWedged counts the handler as wedged if it is still running (it was
// cancelled and has not returned) and repeats the warning while that holds.
func (jr *jobRun) markWedged() {
	jr.mu.Lock()
	if jr.returned {
		jr.mu.Unlock()
		return
	}
	if !jr.wedged {
		jr.wedged = true
		jr.w.wedged.Add(1)
	}
	jr.mu.Unlock()
	jr.log.Error("handler_ignoring_cancellation_lease_kept_alive", "hint", "exit the process to stop it safely")
	time.AfterFunc(wedgedLogEvery, jr.markWedged)
}

// heartbeat renews the lease every HeartbeatInterval until the outcome is
// decided. It keeps trying after a self-fence (a success is proof the claim is
// still ours and keeps another worker away from a still-running handler); it
// stops only when Forge says the claim is gone.
func (jr *jobRun) heartbeat() {
	defer jr.wg.Done()
	interval := jr.claim.HeartbeatInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-jr.leaseCtx.Done():
			return
		case <-ticker.C:
		}
		sentAt := time.Now()
		rctx, cancel := context.WithTimeout(jr.leaseCtx, interval)
		_, err := jr.w.client.Renew(rctx, jr.claim)
		cancel()
		switch {
		case err == nil:
			jr.mu.Lock()
			if sentAt.After(jr.lastOK) {
				jr.lastOK = sentAt // the request send time: the server stamped the lease no earlier
			}
			jr.mu.Unlock()
			if failures > 0 {
				jr.log.Info("renewal_recovered", "failures", failures)
				failures = 0
			}
		case workerclient.IsOwnershipLost(err):
			jr.fire(causeLost)
			return
		case jr.leaseCtx.Err() != nil:
			return
		default:
			failures++
			if failures == 1 || failures%5 == 0 {
				jr.log.Warn("renewal_failed_will_retry", "consecutive", failures, "category", errCategory(err))
			}
		}
	}
}

// watchdog cancels the handler (self-fence) when no renewal has succeeded for
// SafetyMargin. It reads only the local monotonic clock, so a renewal call that
// hangs cannot stop it. It ends when the handler returns.
func (jr *jobRun) watchdog() {
	defer jr.wg.Done()
	margin := jr.claim.SafetyMargin
	tick := margin / 20
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	if tick > 250*time.Millisecond {
		tick = 250 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-jr.leaseCtx.Done():
			return
		case <-jr.wdStop:
			return
		case <-ticker.C:
		}
		jr.mu.Lock()
		silent := time.Since(jr.lastOK)
		returned := jr.returned
		jr.mu.Unlock()
		if returned {
			return
		}
		if silent > margin {
			jr.fire(causeSelfFence)
			return
		}
	}
}

// finishLease stops the heartbeat and watchdog and waits for them.
func (jr *jobRun) finishLease() {
	jr.stopLease()
	jr.wg.Wait()
}

// resolve decides and reports the outcome of a handler that has returned. The
// heartbeat keeps running while a completion or failure is reported, so the
// lease cannot lapse mid-report; a renewal conflict at that point cannot change
// the outcome (fire ignores it). A cooperative release is the exception: the
// heartbeat and watchdog are stopped and joined first.
func (jr *jobRun) resolve(res handlerResult) {
	defer func() {
		for _, f := range jr.cleanup {
			f()
		}
	}()
	c := jr.causeNow()
	o := classify(res.result, res.err, res.panicked, c)
	switch o.action {
	case actionNone:
		jr.finishLease()
		jr.log.Warn("no_outcome_reported", "reason", o.reason)
	case actionComplete:
		r := jr.w.report(func(ctx context.Context) error {
			return jr.w.complete(ctx, jr.claim, o.result)
		})
		jr.finishLease()
		jr.logReport("job_completed", o.reason, r)
	case actionFail:
		var fr workerclient.FailResult
		r := jr.w.report(func(ctx context.Context) error {
			var err error
			fr, err = jr.w.client.Fail(ctx, jr.claim, o.failure)
			return err
		})
		jr.finishLease()
		if r.status == reportConfirmed {
			jr.log.Info("job_failure_recorded", "reason", o.reason, "kind", string(o.failure.Kind), "permanent", o.failure.Permanent,
				"requeued", fr.Requeued, "retry_in", fr.RetryIn.String(), "report_attempts", r.attempts)
		} else {
			jr.logReport("job_failure_not_recorded", o.reason, r)
		}
	case actionRelease:
		jr.finishLease() // stopped and joined BEFORE the release
		r := jr.w.report(func(ctx context.Context) error {
			return jr.w.client.Release(ctx, jr.claim)
		})
		jr.logReport("job_released", o.reason, r)
	}
}

func (jr *jobRun) logReport(msg, reason string, r reportResult) {
	level := jr.log.Info
	if r.status != reportConfirmed {
		level = jr.log.Warn
		msg += "_unconfirmed"
	}
	level(msg, "reason", reason, "outcome", r.status, "report_attempts", r.attempts)
}

// Outcome-report results: fixed words for logs.
const (
	reportConfirmed     = "confirmed"       // the server confirmed it
	reportOwnershipLost = "ownership_lost"  // 409: not applied, the claim is not current
	reportUnknownOrLost = "unknown_or_lost" // 409 after an uncertain earlier attempt: it may have been applied
	reportUnconfirmed   = "unconfirmed"     // still uncertain after the last attempt
	reportRejected      = "rejected"        // a 4xx or other definite refusal
)

type reportResult struct {
	status   string
	attempts int
}

// report sends one outcome request, up to Config.ReportAttempts times in total.
// It repeats the SAME request, and only after an error for which
// workerclient.IsUncertain is true; never after a 409 or other 4xx. Forge does
// not replay operations, so a 409 that follows an uncertain attempt is
// reported as unknown_or_lost, never as success or failure, and the operation
// is never swapped for a different one.
func (w *Worker) report(call func(ctx context.Context) error) reportResult {
	uncertainBefore := false
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		err := call(ctx)
		cancel()
		switch {
		case err == nil:
			return reportResult{reportConfirmed, attempt}
		case workerclient.IsOwnershipLost(err):
			if uncertainBefore {
				return reportResult{reportUnknownOrLost, attempt}
			}
			return reportResult{reportOwnershipLost, attempt}
		case workerclient.IsUncertain(err):
			uncertainBefore = true
			if attempt >= w.reportAttempts {
				return reportResult{reportUnconfirmed, attempt}
			}
			time.Sleep(reportPause)
		default:
			return reportResult{reportRejected, attempt}
		}
	}
}

// releaseUnstarted hands back a claim whose handler never ran.
func (w *Worker) releaseUnstarted(c workerclient.ClaimedJob, why string) {
	r := w.report(func(ctx context.Context) error { return w.client.Release(ctx, c) })
	lvl := w.log.Info
	if r.status != reportConfirmed {
		lvl = w.log.Warn
	}
	lvl("claim_released_unstarted", "job_id", c.JobID, "type", c.Type, "why", why, "outcome", r.status, "report_attempts", r.attempts)
}

// failUnregistered fails a claim whose type has no handler, permanently.
func (w *Worker) failUnregistered(c workerclient.ClaimedJob) {
	r := w.report(func(ctx context.Context) error {
		_, err := w.client.Fail(ctx, c, workerclient.Failure{Kind: workerclient.KindError, Permanent: true})
		return err
	})
	w.log.Error("claim_for_unregistered_type_failed_permanently", "job_id", c.JobID, "type", c.Type, "outcome", r.status)
}

// errCategory names a client error with a fixed word, so logs never carry
// text from the server or the network.
func errCategory(err error) string {
	var te *workerclient.TransportError
	switch {
	case errors.Is(err, workerclient.ErrOwnershipLost):
		return "ownership_lost"
	case errors.Is(err, workerclient.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, workerclient.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, workerclient.ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, workerclient.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, workerclient.ErrMalformedResponse):
		return "malformed_response"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &te):
		return "transport"
	}
	return "other"
}

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
