package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Venkatraman-web/forge/forgecart/internal/outbox"
	"github.com/Venkatraman-web/forge/sdk/forgeclient"
)

// Config holds validated dispatcher settings (see config.LoadDispatcher).
type Config struct {
	ForgeURL     string
	ForgeAPIKey  string // Forge's submit key; never logged
	PollInterval time.Duration
	BatchSize    int
	Lease        time.Duration
	HTTPTimeout  time.Duration
	MaxAttempts  int
}

const (
	backoffBase = time.Second
	backoffCap  = 5 * time.Minute
	// dbOpTimeout bounds each state-changing database call.
	dbOpTimeout = 5 * time.Second
	// shutdownGrace is how long in-flight work may keep running after a stop
	// request before its HTTP request is cancelled.
	shutdownGrace = 5 * time.Second
	// consecutivePermanentLimit: this many permanent rejections in a row with no
	// success between them is treated as a systematic problem, not bad events.
	consecutivePermanentLimit = 5
	// unchargedDelay is how soon an event is due again after a Forge-wide
	// failure. It is short and fixed (not exponential) because the event is not
	// at fault: once the operator fixes Forge or the configuration it should
	// flow again without waiting out a long backoff.
	unchargedDelay = 10 * time.Second
	// Backpressure (Forge answered queue_full). The pause honours Forge's
	// Retry-After; these bound it. defaultBackpressureDelay applies when the
	// header is missing or invalid (an older Forge), minBackpressureDelay stops a
	// tiny hint from becoming a tight loop, and jitter spreads the retries of
	// many events (and of several dispatchers) so they do not return together.
	defaultBackpressureDelay = 5 * time.Second
	minBackpressureDelay     = time.Second

	// defaultAdmissionBusyDelay applies when an admission_busy answer carries no
	// valid Retry-After. Forge sends one second.
	defaultAdmissionBusyDelay = time.Second
)

// ErrFatal wraps configuration-wide failures that stop the dispatcher.
var ErrFatal = errors.New("dispatcher: stopping on a configuration-wide failure")

type Dispatcher struct {
	cfg        Config
	store      *outbox.Store
	forge      *forgeClient
	log        *slog.Logger
	permStreak atomic.Int32
	// pauseUntil (unix nanoseconds) is when claiming may resume after a
	// queue_full answer. While Forge is full, nothing is gained by claiming and
	// submitting more events; waiting is the backpressure.
	pauseUntil atomic.Int64

	fatalOnce sync.Once
	fatalErr  error
	stopClaim context.CancelFunc
}

func New(cfg Config, store *outbox.Store, log *slog.Logger) (*Dispatcher, error) {
	fc, err := newForgeClient(cfg.ForgeURL, cfg.ForgeAPIKey, cfg.HTTPTimeout)
	if err != nil {
		return nil, err
	}
	return &Dispatcher{cfg: cfg, store: store, forge: fc, log: log}, nil
}

// Run claims and publishes events until ctx is cancelled (graceful stop: no new
// claims, in-flight events get shutdownGrace to finish) or a fatal error
// occurs. It returns nil on a requested stop.
//
// Each round claims up to BatchSize events, records an attempt for each
// concurrently (so no event waits behind another while its lease runs down), and
// then sends the ready ones to Forge together in ONE batch request, because a
// single submission costs Forge one durable commit and that commit rate is what
// limits submissions. No database transaction is open during any HTTP call.
func (d *Dispatcher) Run(ctx context.Context) error {
	claimCtx, stop := context.WithCancel(ctx)
	defer stop()
	d.stopClaim = stop

	// workCtx outlives ctx by shutdownGrace so a stop request does not abort
	// requests that are about to finish; after that they are cancelled.
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	var inflight sync.WaitGroup
	go func() {
		<-claimCtx.Done()
		select {
		case <-time.After(shutdownGrace):
			cancelWork()
		case <-workCtx.Done():
		}
	}()

	dbDelay := d.cfg.PollInterval
	for claimCtx.Err() == nil {
		if wait := time.Until(time.Unix(0, d.pauseUntil.Load())); wait > 0 {
			d.log.Info("claiming paused: Forge queue is full", "op", "backpressure", "outcome", "paused", "pause_ms", wait.Milliseconds())
			if !sleep(claimCtx, wait) {
				break
			}
			continue
		}
		events, err := d.store.Claim(claimCtx, d.cfg.BatchSize, d.cfg.Lease)
		if err != nil {
			if claimCtx.Err() != nil {
				break
			}
			d.log.Error("claim failed", "op", "claim", "outcome", "database_error")
			if !sleep(claimCtx, dbDelay) {
				break
			}
			dbDelay = min(dbDelay*2, 30*time.Second)
			continue
		}
		dbDelay = d.cfg.PollInterval
		if len(events) > 0 {
			inflight.Add(1)
			go func() {
				defer inflight.Done()
				d.publishGroup(workCtx, events)
			}()
		}
		inflight.Wait() // round barrier: finish this batch, then re-check for stop
		if len(events) < d.cfg.BatchSize && !sleep(claimCtx, d.cfg.PollInterval) {
			break
		}
	}
	inflight.Wait()
	d.log.Info("dispatcher stopped", "reason", map[bool]string{true: "fatal", false: "requested"}[d.fatalErr != nil])
	return d.fatalErr
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (d *Dispatcher) fatal(code string) {
	d.fatalOnce.Do(func() {
		d.fatalErr = fmt.Errorf("%w (%s)", ErrFatal, code)
		d.stopClaim()
	})
}

// backoff is capped exponential backoff with jitter (50-100% of the step).
func backoff(attempt int) time.Duration {
	step := backoffBase
	for i := 1; i < attempt && step < backoffCap; i++ {
		step *= 2
	}
	step = min(step, backoffCap)
	return step/2 + rand.N(step/2+1)
}

// prepared is a claimed event that passed its checks and whose attempt has been
// recorded, so it is ready to be sent to Forge.
type prepared struct {
	e    outbox.Event
	reqs []forgeclient.SubmitRequest // the Forge jobs of this event (four for an order.paid event)
	n    int                         // attempts recorded so far, including this one
	log  *slog.Logger
}

// publishGroup publishes the events of one claim round. Each event is checked
// on its own, then ONE statement records the attempt for all that are ready, and
// they go to Forge in one batch request (at most forgeclient.MaxBatchSize). The
// batch is all or nothing in Forge, so one answer applies to every event in it.
// If Forge rejects the batch as invalid (it names one bad item), nothing was
// stored, and each event is sent on its own instead, so only the bad event fails.
func (d *Dispatcher) publishGroup(ctx context.Context, events []outbox.Event) {
	group := d.prepareGroup(events)
	// One Forge request holds at most forgeclient.MaxBatchSize jobs. A request
	// always carries WHOLE events: the jobs of one event are never split across
	// two requests, so an event is either published or not.
	for len(group) > 0 {
		n, jobs := 0, 0
		for n < len(group) && (n == 0 || jobs+len(group[n].reqs) <= forgeclient.MaxBatchSize) {
			jobs += len(group[n].reqs)
			n++
		}
		d.sendBatch(ctx, group[:n])
		group = group[n:]
	}
}

// prepareGroup validates the events and records one attempt for every event that
// is ready, in a single database statement. Events that need no sending
// (rejected, exhausted, lease lost) are handled here and left out of the result.
func (d *Dispatcher) prepareGroup(events []outbox.Event) []prepared {
	var checked []prepared
	var owned []outbox.Owned
	for _, e := range events {
		log := d.log.With("event_id", e.ID, "event_type", e.Type)
		reqs, err := buildRequests(e.ID, e.Type, e.Payload)
		if err != nil {
			d.fail(log, e, "invalid_payload")
			continue
		}
		// The retry budget counts only attempts charged to the event itself:
		// attempts that ended in a Forge-wide failure or a shutdown are excluded.
		if e.Attempts-e.UnchargedAttempts >= d.cfg.MaxAttempts {
			d.fail(log, e, "attempts_exhausted")
			continue
		}
		checked = append(checked, prepared{e: e, reqs: reqs, log: log})
		owned = append(owned, outbox.Owned{ID: e.ID, Token: e.ClaimToken})
	}
	if len(checked) == 0 {
		return nil
	}
	// Count the attempts immediately before the request. A crash between this
	// commit and the HTTP send still consumes an attempt.
	c, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	attempts, err := d.store.StartAttempts(c, owned)
	if err != nil {
		d.log.Error("could not record attempts", "op", "start_attempt", "outcome", "database_error", "events", len(checked))
		return nil // nothing was sent; the leases expire and the events are reclaimed
	}
	ready := checked[:0]
	for _, p := range checked {
		n, ok := attempts[p.e.ID]
		if !ok {
			p.log.Warn("lease lost before sending", "op", "start_attempt", "outcome", "lease_lost")
			continue
		}
		p.n = n
		ready = append(ready, p)
	}
	return ready
}

// sendBatch submits the jobs of prepared events together in one request.
func (d *Dispatcher) sendBatch(ctx context.Context, group []prepared) {
	if len(group) == 1 {
		d.settle(ctx, group[0], d.forge.submitJobs(ctx, group[0].reqs))
		return
	}
	var reqs []forgeclient.SubmitRequest
	for _, p := range group {
		reqs = append(reqs, p.reqs...)
	}
	out := d.forge.submitJobs(ctx, reqs)
	if out.kind == outcomePermanent {
		// One event made Forge refuse the whole batch. Nothing was stored; find
		// the bad one by sending each event's jobs alone (the same recorded attempt).
		d.log.Warn("batch rejected; sending its events one by one", "op", "submit_batch", "outcome", out.code, "events", len(group))
		each(group, func(p prepared) { d.settle(ctx, p, d.forge.submitJobs(ctx, p.reqs)) })
		return
	}
	if out.kind == outcomeAccepted {
		d.permStreak.Store(0)
		d.markPublishedGroup(group)
		return
	}
	each(group, func(p prepared) { d.settle(ctx, p, out) })
}

// markPublishedGroup records acceptance of a whole batch in one statement. Forge
// ALREADY has these jobs, so a failure to record is an uncertain outcome, never a
// rejection: events not recorded stay leased until the lease expires and are
// submitted again (a possible duplicate that the jobs must tolerate via event_id).
func (d *Dispatcher) markPublishedGroup(group []prepared) {
	owned := make([]outbox.Owned, len(group))
	for i, p := range group {
		owned[i] = outbox.Owned{ID: p.e.ID, Token: p.e.ClaimToken}
	}
	for try := 0; try < 3; try++ {
		c, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
		marked, err := d.store.MarkPublishedBatch(c, owned)
		cancel()
		if err != nil {
			time.Sleep(time.Duration(try+1) * 200 * time.Millisecond)
			continue
		}
		for _, p := range group {
			if marked[p.e.ID] {
				p.log.Info("published", "op", "mark_published", "outcome", "published", "attempt", p.n)
			} else {
				p.log.Warn("Forge accepted the job but the lease was lost; a duplicate submission is possible", "op", "mark_published", "outcome", "accepted_but_lease_lost", "attempt", p.n)
			}
		}
		return
	}
	for _, p := range group {
		p.log.Error("Forge accepted the job but recording it failed; a duplicate submission is possible", "op", "mark_published", "outcome", "accepted_but_unrecorded", "attempt", p.n)
	}
}

// each runs f for every event of the group at the same time and waits for all.
// Recording the outcomes one after another would cost one durable commit each, in
// sequence (about 3 ms apiece on a network disk, which capped the dispatcher near
// 300 events per second); done together, the database flushes them as one group.
// The group is at most forgeclient.MaxBatchSize, so this is bounded.
func each(group []prepared, f func(prepared)) {
	var wg sync.WaitGroup
	for _, p := range group {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f(p)
		}()
	}
	wg.Wait()
}

// settle applies Forge's answer to one event: published, rescheduled, failed, or
// a reason to stop the dispatcher.
func (d *Dispatcher) settle(ctx context.Context, p prepared, out outcome) {
	e, n, log := p.e, p.n, p.log
	interrupted := ctx.Err() != nil && out.kind == outcomeRetry
	if interrupted {
		out.code = "interrupted_by_shutdown"
	}
	used := n - e.UnchargedAttempts // budget used including this attempt
	switch out.kind {
	case outcomeAccepted:
		d.permStreak.Store(0)
		d.markPublished(log, e, n)
	case outcomeBackpressure:
		d.backpressure(log, e, n, out)
	case outcomeAdmissionBusy:
		d.admissionBusy(log, e, n, out)
	case outcomeRetry:
		if interrupted {
			// Our own shutdown, not a publication failure: uncharged. The
			// outcome stays uncertain (the job may exist).
			d.reschedule(log, e, n, backoff(1), out.code, true)
			return
		}
		d.retry(log, e, n, used, out)
	case outcomePermanent:
		if d.permStreak.Add(1) >= consecutivePermanentLimit {
			log.Error("too many consecutive rejections: treating as a configuration problem", "op", "submit", "outcome", out.code)
			d.reschedule(log, e, n, unchargedDelay, "systematic_rejection", true)
			d.fatal("systematic_rejection")
			return
		}
		d.fail(log, e, out.code)
	default: // outcomeFatal
		log.Error("configuration-wide failure; stopping", "op", "submit", "outcome", out.code)
		d.reschedule(log, e, n, unchargedDelay, out.code, true)
		d.fatal(out.code)
	}
}

// markPublished records acceptance. Forge ALREADY has the job here, so a
// failure to record it is an uncertain outcome, never a rejection: the event
// stays leased until the lease expires and will be submitted again (a possible
// duplicate that Forge jobs must tolerate via event_id).
func (d *Dispatcher) markPublished(log *slog.Logger, e outbox.Event, attempts int) {
	var err error
	for i := 0; i < 3; i++ {
		_, err = d.dbCall(func(c context.Context) (int, error) { return 0, d.store.MarkPublished(c, e.ID, e.ClaimToken) })
		if err == nil {
			log.Info("published", "op", "mark_published", "outcome", "published", "attempt", attempts)
			return
		}
		if errors.Is(err, outbox.ErrLeaseLost) {
			log.Warn("Forge accepted the job but the lease was lost; a duplicate submission is possible", "op", "mark_published", "outcome", "accepted_but_lease_lost", "attempt", attempts)
			return
		}
		time.Sleep(time.Duration(i+1) * 200 * time.Millisecond)
	}
	log.Error("Forge accepted the job but recording it failed; a duplicate submission is possible", "op", "mark_published", "outcome", "accepted_but_unrecorded", "attempt", attempts)
}

// backpressureDelay turns Forge's Retry-After hint into a retry delay: the hint
// (or the default when it is absent) is the minimum, plus up to 50% jitter, and
// never more than maxRetryAfter.
func backpressureDelay(retryAfter time.Duration) time.Duration {
	base := retryAfter
	if base <= 0 {
		base = defaultBackpressureDelay
	}
	base = max(base, minBackpressureDelay)
	return min(base+rand.N(base/2+1), maxRetryAfter)
}

// backpressure handles a queue_full answer. Forge stored nothing, so the event
// is simply not published yet: it stays pending, is rescheduled as an UNCHARGED
// attempt (it did nothing wrong, so no number of full-queue answers can use up
// its retry budget or fail it), and claiming pauses for the same delay so the
// dispatcher does not keep submitting other events into a full queue. Genuine
// problems are unaffected: every other outcome still takes its own path.
func (d *Dispatcher) backpressure(log *slog.Logger, e outbox.Event, attempts int, out outcome) {
	delay := backpressureDelay(out.retryAfter)
	d.pauseClaims(delay)
	d.reschedule(log, e, attempts, delay, out.code, true)
}

// admissionBusyDelay turns Forge's Retry-After hint into this event's retry
// delay: never less than the hint or one second, whichever is larger (one second
// when the hint is missing), plus up to
// 50% jitter so rejected events do not all come back at the same instant, and
// never more than maxRetryAfter.
func admissionBusyDelay(retryAfter time.Duration) time.Duration {
	base := retryAfter
	if base <= 0 {
		base = defaultAdmissionBusyDelay
	}
	base = max(base, minBackpressureDelay) // a tiny hint must not become a tight loop
	return min(base+rand.N(base/2+1), maxRetryAfter)
}

// admissionBusy handles an admission_busy answer. Forge stored nothing, so the
// event stays pending and is rescheduled as an UNCHARGED attempt: any number of
// busy answers can neither use up its retry budget nor fail it. Unlike
// backpressure, claiming is NOT paused: the answer says Forge is momentarily
// busy with other submissions, not that nothing will be accepted, and holding
// back unrelated events would only lower throughput. A tight loop is still
// impossible: each rejected event is due again no earlier than the advertised
// delay, and claiming itself keeps its normal poll interval and batch size.
func (d *Dispatcher) admissionBusy(log *slog.Logger, e outbox.Event, attempts int, out outcome) {
	d.reschedule(log, e, attempts, admissionBusyDelay(out.retryAfter), out.code, true)
}

// pauseClaims postpones claiming until at least delay from now. The later of
// two overlapping pauses wins.
func (d *Dispatcher) pauseClaims(delay time.Duration) {
	until := time.Now().Add(delay).UnixNano()
	for {
		cur := d.pauseUntil.Load()
		if until <= cur || d.pauseUntil.CompareAndSwap(cur, until) {
			return
		}
	}
}

func (d *Dispatcher) retry(log *slog.Logger, e outbox.Event, attempts, used int, out outcome) {
	if used >= d.cfg.MaxAttempts {
		d.fail(log, e, "attempts_exhausted")
		return
	}
	delay := backoff(used)
	if out.retryAfter > delay {
		delay = out.retryAfter
	}
	d.reschedule(log, e, attempts, delay, out.code, false)
}

// reschedule records a retry. uncharged marks the attempt as not belonging to
// the event's retry budget (Forge-wide failure or shutdown). If the database
// call fails, nothing is recorded and the attempt stays charged: that is the
// conservative direction, and lease expiry recovers the event.
func (d *Dispatcher) reschedule(log *slog.Logger, e outbox.Event, attempts int, delay time.Duration, code string, uncharged bool) {
	_, err := d.dbCall(func(c context.Context) (int, error) {
		if uncharged {
			return 0, d.store.RescheduleUncharged(c, e.ID, e.ClaimToken, delay, code)
		}
		return 0, d.store.RescheduleAfter(c, e.ID, e.ClaimToken, delay, code)
	})
	switch {
	case err == nil:
		log.Info("rescheduled", "op", "reschedule", "outcome", code, "attempt", attempts, "uncharged", uncharged, "retry_in_ms", delay.Milliseconds())
	case errors.Is(err, outbox.ErrLeaseLost):
		log.Warn("lease lost; retry not recorded", "op", "reschedule", "outcome", "lease_lost")
	default:
		log.Error("retry not recorded; lease expiry will recover the event", "op", "reschedule", "outcome", "database_error")
	}
}

func (d *Dispatcher) fail(log *slog.Logger, e outbox.Event, code string) {
	_, err := d.dbCall(func(c context.Context) (int, error) { return 0, d.store.MarkFailed(c, e.ID, e.ClaimToken, code) })
	switch {
	case err == nil:
		log.Error("event failed permanently", "op", "mark_failed", "outcome", code)
	case errors.Is(err, outbox.ErrLeaseLost):
		log.Warn("lease lost; failure not recorded", "op", "mark_failed", "outcome", "lease_lost")
	default:
		log.Error("failure not recorded; lease expiry will recover the event", "op", "mark_failed", "outcome", "database_error")
	}
}

// dbCall runs a state-changing database call under its own deadline, detached
// from shutdown so that an outcome already known can still be recorded.
func (d *Dispatcher) dbCall(f func(context.Context) (int, error)) (int, error) {
	c, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	return f(c)
}
