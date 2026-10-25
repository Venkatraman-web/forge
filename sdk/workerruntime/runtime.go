// Package workerruntime runs customer-owned job handlers against Forge's
// remote worker API, using the typed client in forge/sdk/workerclient.
//
// STATUS: multi-slot lifecycle with claim-error backoff (Tasks 25.3.2.4.1 and
// 25.3.2.4.2). Run starts Config.Concurrency independent slots; each claims one
// job at a time, runs its handler under a deadline, renews the lease with a
// heartbeat and a watchdog, and reports the outcome (Complete, Fail or, on a
// clean shutdown, Release). A slot that cannot claim backs off or, for errors
// that retrying cannot fix, stops the worker (claimerr.go). Not yet built: the
// final shutdown hardening. See docs/decisions/0003-worker-runtime-v0.md.
//
// # Delivery guarantees
//
// Forge delivers jobs at least once. A handler can run more than once (a crash,
// a lost response, a lease that lapses). Handlers must be idempotent; Job.ID is
// the stable key to use.
//
// # Limits worth knowing
//
// A panic in a goroutine that a handler starts itself cannot be recovered by
// the runtime and ends the process. Network partitions and database failures
// can stop lease renewal, so software cannot guarantee that no other worker
// starts the job after the stored lease expires; the watchdog stops the
// handler before that point when it can, and the server does not enforce the
// execution deadline.
//
// # Cancellation and the safety rule
//
// A handler must stop promptly when its context is cancelled. Go cannot stop a
// goroutine that ignores cancellation. The runtime therefore will never release
// a claim, and so never let another worker take over, while a handler may still
// be performing side effects: a handler that does not return keeps its job (the
// lease keeps being renewed) and keeps its slot until the process exits.
package workerruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Venkatraman-web/forge/sdk/workerclient"
)

// Limits and defaults for Config.
const (
	// DefaultConcurrency is used when Config.Concurrency is zero.
	DefaultConcurrency = 1
	// MaxConcurrency is the most handlers one Worker runs at once.
	MaxConcurrency = 64
	// DefaultPollWait is used when Config.PollWait is zero.
	DefaultPollWait = 20 * time.Second
	// MinPollWait is the shortest accepted PollWait.
	MinPollWait = time.Second
	// DefaultShutdownGrace is used when Config.ShutdownGrace is zero.
	DefaultShutdownGrace = 30 * time.Second
	// MaxShutdownGrace bounds ShutdownGrace.
	MaxShutdownGrace = time.Hour
	// DefaultReportAttempts is used when Config.ReportAttempts is zero.
	DefaultReportAttempts = 2
	// MaxReportAttempts is the largest accepted ReportAttempts.
	MaxReportAttempts = 3
	// MinJobTimeout is the shortest accepted WithTimeout. Below a second a
	// timeout would mostly measure scheduling noise, and it matches MinPollWait,
	// the runtime's other shortest duration.
	MinJobTimeout = time.Second
	// MaxJobTypes is the most distinct job types one Worker may register: a
	// claim request lists at most this many types.
	MaxJobTypes = workerclient.MaxClaimTypes
	// MaxNameLength is the longest Config.Name.
	MaxNameLength = 40
)

// Errors returned by this package.
var (
	// ErrNoHandlers is returned by Run when no handler has been registered.
	ErrNoHandlers = errors.New("workerruntime: no handlers registered")

	// ErrShutdownIncomplete is returned by Run when a handler was still running
	// at the end of the shutdown grace period. The handler cannot be stopped; its
	// lease keeps being renewed in the background so no other worker takes the
	// job, until the handler returns or the process exits. Exiting the process
	// is the safe way to stop it: the lease then expires and Forge recovers the
	// job.
	ErrShutdownIncomplete = errors.New("workerruntime: shutdown incomplete: handlers are still running; exit the process to stop them safely")

	// ErrAlreadyRunning is returned by Handle once Run has started.
	ErrAlreadyRunning = errors.New("workerruntime: handlers must be registered before Run")
)

var (
	jobTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	namePattern    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,` + "40" + `}$`)
)

// Config configures a Worker. The zero value of every optional field selects
// its default. An explicitly set invalid value is rejected, not corrected.
type Config struct {
	// Client calls the Forge worker API. Required. Its HTTP timeout should
	// exceed PollWait (the client's default does).
	Client *workerclient.Client

	// Name labels this worker in its worker IDs and logs: letters, digits,
	// '.', '_' and '-', at most MaxNameLength characters. Default: the host
	// name with unsupported characters replaced by '-'. It is not a secret.
	Name string

	// Concurrency is how many handlers may run at once: 1 to MaxConcurrency.
	// Zero means DefaultConcurrency.
	Concurrency int

	// PollWait is how long one claim request may wait for work: a whole number
	// of seconds from MinPollWait to the claim API's maximum (25 s). Zero means
	// DefaultPollWait.
	PollWait time.Duration

	// ShutdownGrace is how long Run waits for running handlers after its
	// context is cancelled, up to MaxShutdownGrace. Zero means
	// DefaultShutdownGrace.
	ShutdownGrace time.Duration

	// ReportAttempts is the most times the runtime will send one outcome report
	// (Complete, Fail or Release) to Forge when the result is uncertain: 1 to
	// MaxReportAttempts, total, including the first. 1 means never resend. Zero
	// means DefaultReportAttempts (2). A resend repeats the same request, only
	// after an uncertain error, never after a 409 or other 4xx; because Forge
	// does not replay outcomes, a 409 that follows an uncertain first attempt
	// means the outcome is unknown.
	ReportAttempts int

	// Logger receives the runtime's events, which contain identifiers and fixed
	// words only: never handler error text, payloads or credentials. Nil means
	// slog.Default().
	Logger *slog.Logger
}

// Worker runs registered handlers. Create one with New. Handle may be called
// concurrently with itself; Run is called once.
type Worker struct {
	client         *workerclient.Client
	name           string
	instance       string // random per Worker; keeps IDs unique across restarts
	concurrency    int
	pollWait       time.Duration
	shutdownGrace  time.Duration
	reportAttempts int
	log            *slog.Logger

	wedged atomic.Int32 // handlers still running after being cancelled; see Wedged

	mu       sync.Mutex
	handlers map[string]HandlerFunc
	timeouts map[string]time.Duration // per job type, set by WithTimeout
	started  bool

	completions completer
	capNoted    sync.Map // job types whose WithTimeout exceeded the server deadline (logged once)
}

// New validates cfg and creates a Worker. It makes no network request and
// starts nothing.
func New(cfg Config) (*Worker, error) {
	if cfg.Client == nil {
		return nil, errors.New("workerruntime: Config.Client is required")
	}
	w := &Worker{
		client:         cfg.Client,
		concurrency:    cfg.Concurrency,
		pollWait:       cfg.PollWait,
		shutdownGrace:  cfg.ShutdownGrace,
		reportAttempts: cfg.ReportAttempts,
		log:            cfg.Logger,
		handlers:       make(map[string]HandlerFunc),
		timeouts:       make(map[string]time.Duration),
	}

	switch {
	case cfg.Concurrency == 0:
		w.concurrency = DefaultConcurrency
	case cfg.Concurrency < 0 || cfg.Concurrency > MaxConcurrency:
		return nil, fmt.Errorf("workerruntime: Config.Concurrency must be 1 to %d, got %d", MaxConcurrency, cfg.Concurrency)
	}

	switch {
	case cfg.PollWait == 0:
		w.pollWait = DefaultPollWait
	case cfg.PollWait < MinPollWait || cfg.PollWait > workerclient.MaxWait:
		return nil, fmt.Errorf("workerruntime: Config.PollWait must be %s to %s, got %s", MinPollWait, workerclient.MaxWait, cfg.PollWait)
	case cfg.PollWait%time.Second != 0:
		// The claim API counts whole seconds; truncating silently would hide a mistake.
		return nil, fmt.Errorf("workerruntime: Config.PollWait must be a whole number of seconds, got %s", cfg.PollWait)
	}

	switch {
	case cfg.ShutdownGrace == 0:
		w.shutdownGrace = DefaultShutdownGrace
	case cfg.ShutdownGrace < 0 || cfg.ShutdownGrace > MaxShutdownGrace:
		return nil, fmt.Errorf("workerruntime: Config.ShutdownGrace must be positive and at most %s, got %s", MaxShutdownGrace, cfg.ShutdownGrace)
	}

	switch {
	case cfg.ReportAttempts == 0:
		w.reportAttempts = DefaultReportAttempts
	case cfg.ReportAttempts < 1 || cfg.ReportAttempts > MaxReportAttempts:
		return nil, fmt.Errorf("workerruntime: Config.ReportAttempts must be 1 to %d, got %d", MaxReportAttempts, cfg.ReportAttempts)
	}

	if cfg.Name == "" {
		host, _ := os.Hostname() // an error leaves host empty; defaultName handles that
		w.name = defaultName(host)
	} else if !namePattern.MatchString(cfg.Name) || len(cfg.Name) > MaxNameLength {
		return nil, fmt.Errorf("workerruntime: Config.Name must be 1 to %d characters from A-Z a-z 0-9 . _ -", MaxNameLength)
	} else {
		w.name = cfg.Name
	}

	if w.log == nil {
		w.log = slog.Default()
	}

	inst, err := newInstanceID()
	if err != nil {
		return nil, err
	}
	w.instance = inst
	return w, nil
}

// HandlerFunc runs one job. It must stop promptly when ctx is cancelled and
// must be safe to run concurrently with other handlers. The result is encoded
// as JSON and becomes the job's public result (so it must not contain secrets);
// nil, or anything that encodes to JSON null, means no result. A returned
// error marks the attempt failed and retryable, unless it was wrapped with
// Permanent. The error's text is never sent to Forge or logged.
type HandlerFunc func(ctx context.Context, job Job) (result any, err error)

// HandleOption adjusts how one job type is run. See WithTimeout.
type HandleOption func(*handleOptions)

type handleOptions struct {
	timeout    time.Duration
	timeoutSet bool // WithTimeout(0) is a mistake, not "unset"
}

// WithTimeout limits how long one attempt of a job of this type may execute.
// When it expires the handler's context is cancelled (the same mechanism and
// the same outcome as Forge's own execution deadline): the attempt is reported
// as a retryable timeout failure, so it is retried or, with no attempts left,
// failed under the usual policy. Go cannot stop a goroutine, so a handler that
// ignores its context keeps running (see the package documentation).
//
// It can only SHORTEN the limit. Forge sends every claim the claimed job's own
// execution deadline (its submitter's options.timeout_seconds, 30 s when the
// submission omitted it, and never above the server's maximum), and the
// effective limit is the smaller of the two; a larger value changes nothing and
// is logged once per job type. The valid range is MinJobTimeout (1 s) and up.
// The clock starts when the claim response arrives. This is a worker-side
// setting for a job type; the submitter chooses the deadline of each job.
func WithTimeout(d time.Duration) HandleOption {
	return func(o *handleOptions) { o.timeout, o.timeoutSet = d, true }
}

// Handle registers h for jobs of type jobType. Types must match
// ^[a-z][a-z0-9_.-]{0,63}$, the same rule Forge applies. A Worker handles at
// most MaxJobTypes distinct types, because one claim request lists at most that
// many; a ninth registration returns an error rather than being dropped. Nil
// handlers and duplicate registrations are errors. Handle runs nothing, and
// must be called before Run: afterwards it returns ErrAlreadyRunning. Options
// such as WithTimeout are validated here, so a bad value fails at startup.
func (w *Worker) Handle(jobType string, h HandlerFunc, opts ...HandleOption) error {
	if !jobTypePattern.MatchString(jobType) {
		return fmt.Errorf("workerruntime: invalid job type %q: use lowercase letters, digits, '_', '.', '-', starting with a letter, at most 64 characters", jobType)
	}
	if h == nil {
		return fmt.Errorf("workerruntime: handler for %q is nil", jobType)
	}
	var o handleOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.timeoutSet && o.timeout < MinJobTimeout {
		return fmt.Errorf("workerruntime: WithTimeout for %q must be at least %s, got %s", jobType, MinJobTimeout, o.timeout)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.started:
		return ErrAlreadyRunning
	case w.handlers[jobType] != nil:
		return fmt.Errorf("workerruntime: a handler for %q is already registered", jobType)
	case len(w.handlers) >= MaxJobTypes:
		return fmt.Errorf("workerruntime: at most %d job types per worker (a claim lists at most %d), cannot add %q", MaxJobTypes, MaxJobTypes, jobType)
	}
	w.handlers[jobType] = h
	if o.timeoutSet {
		w.timeouts[jobType] = o.timeout
	}
	return nil
}

// types returns the registered job types in sorted order: the list a claim
// will ask for.
func (w *Worker) types() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.handlers))
	for t := range w.handlers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Run claims jobs and runs their handlers until ctx is cancelled or a slot hits
// a fatal error. It starts exactly Config.Concurrency execution slots. Each slot
// is its own loop with its own stable worker ID (Config.Name, a random
// per-Worker part, and the slot number): it claims one job for the registered
// types (a long poll of Config.PollWait), runs the handler under a per-job
// deadline taken from the claim, keeps that claim's lease alive, reports the
// outcome, and only then claims again. A busy slot never claims, and nothing is
// prefetched, so at most Concurrency handlers run at once. Slots share nothing
// mutable except the read-only handler table; each claim has its own lifecycle
// state (heartbeat, watchdog, cancellation reason, outcome).
//
// At least one handler must be registered (ErrNoHandlers). A worker can be
// started once: a second Run, also after the first has returned, returns
// ErrAlreadyRunning, and so does Handle once Run has started.
//
// Shutdown. Cancelling ctx, or a fatal error in any slot, stops every slot from
// claiming, cancels in-flight claim requests and cancels the running handlers.
// Handlers that stop are released (attempt refunded) after their heartbeat and
// watchdog are stopped. A handler that does not stop is never released or
// failed: after ShutdownGrace it is left running with its lease kept alive in
// the background, and Run returns ErrShutdownIncomplete.
//
// Run returns nil after a clean shutdown. A fatal error is returned as the
// primary error; later errors from other slots are logged, not returned, and
// never replace it. Claim errors are classified (claimerr.go): a wrong key, any
// other 4xx, an unreadable server response and client-side argument errors are
// fatal; rate limiting (honouring Retry-After), 5xx and transport failures make
// the slot back off, with a bounded jittered delay, and claim again. Unusable
// lease timings from the server are fatal too. If a handler is also left running, ErrShutdownIncomplete is joined to
// the primary error (use errors.Is to test for either).
func (w *Worker) Run(ctx context.Context) error {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return ErrAlreadyRunning
	}
	if len(w.handlers) == 0 {
		w.mu.Unlock()
		return ErrNoHandlers
	}
	w.started = true
	handlers := make(map[string]HandlerFunc, len(w.handlers))
	for t, h := range w.handlers {
		handlers[t] = h
	}
	timeouts := make(map[string]time.Duration, len(w.timeouts))
	for t, d := range w.timeouts {
		timeouts[t] = d
	}
	w.mu.Unlock()

	types := w.types()
	w.log.Info("worker_started", "slots", w.concurrency, "types", types, "poll_wait", w.pollWait.String())
	defer w.log.Info("worker_stopped")

	// runCtx ends when the caller cancels ctx or when a slot reports a fatal
	// error; every slot and every running job observes it as "shutdown".
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	var (
		mu       sync.Mutex
		primary  error
		detached bool
	)
	fatal := func(slot int, err error) {
		mu.Lock()
		if primary == nil {
			primary = err
			mu.Unlock()
			stop() // coordinated shutdown of the other slots
			return
		}
		mu.Unlock()
		w.log.Error("slot_error_after_primary_failure", "slot", slot, "category", errCategory(err))
	}

	// Slots and the claimer talk through two things: an idle slot puts its number
	// on ready, and the claimer hands it a claimed job through its inbox. The
	// claimer only ever claims as many jobs as there are idle slots, so a claimed
	// job starts at once and never waits (unrenewed) for a free slot.
	ready := make(chan int, w.concurrency)
	inbox := make([]chan delivery, w.concurrency+1)
	for slot := 1; slot <= w.concurrency; slot++ {
		inbox[slot] = make(chan delivery, 1)
	}
	var wg sync.WaitGroup
	for slot := 1; slot <= w.concurrency; slot++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			if w.slotLoop(runCtx, slot, ready, inbox[slot], handlers, timeouts, fatal) {
				mu.Lock()
				detached = true
				mu.Unlock()
			}
		}(slot)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.claimer(runCtx, types, ready, inbox, fatal)
	}()
	wg.Wait()
	// A job delivered to a slot that stopped before taking it was claimed but
	// never started: hand it back.
	for slot := 1; slot <= w.concurrency; slot++ {
		select {
		case d := <-inbox[slot]:
			w.releaseUnstarted(d.claim, "claimed_after_shutdown")
		default:
		}
	}

	mu.Lock()
	defer mu.Unlock()
	incomplete := detached || w.Wedged() > 0
	switch {
	case primary != nil && incomplete:
		return errors.Join(primary, ErrShutdownIncomplete)
	case primary != nil:
		return primary
	case incomplete:
		return ErrShutdownIncomplete
	}
	return nil
}

// delivery is a claimed job on its way from the claimer to an idle slot.
type delivery struct {
	claim      workerclient.ClaimedJob
	receivedAt time.Time // the lease clock starts when the claim response arrived
}

// claimer turns idle slots into claimed jobs. It waits for an idle slot, gathers
// every other idle slot that is waiting (up to the server's batch limit), asks
// Forge for that many jobs in ONE request, and hands one job to each of those
// slots; slots that got nothing go back to the idle pool. One request for many
// jobs costs Forge far less than one request per job. It returns when ctx ends
// or after reporting a fatal error.
func (w *Worker) claimer(ctx context.Context, types []string, ready chan int, inbox []chan delivery, fatal func(int, error)) {
	workerID := w.workerID(0)
	failures := 0 // consecutive transient claim failures
	var lastLog time.Time
	giveBack := func(slots []int) {
		for _, s := range slots {
			ready <- s // ready has room for every slot, so this never blocks
		}
	}
	for {
		var slots []int
		select {
		case s := <-ready:
			slots = append(slots, s)
		case <-ctx.Done():
			return
		}
	gather:
		for len(slots) < workerclient.MaxClaimBatch {
			select {
			case s := <-ready:
				slots = append(slots, s)
			default:
				break gather
			}
		}

		began := time.Now()
		claims, err := w.client.ClaimBatch(ctx, workerclient.ClaimBatchRequest{WorkerID: workerID, Types: types, Wait: w.pollWait, Max: len(slots)})
		receivedAt := time.Now() // the lease clock starts here, not when the long poll began
		if err != nil {
			giveBack(slots)
			if ctx.Err() != nil {
				// Shutdown cancelled the claim. In the rare case the server had
				// already committed some, nobody renews them and the reaper
				// recovers them after the lease (the attempts stay used).
				w.log.Info("claim_cancelled_by_shutdown")
				return
			}
			kind, category, retryAfter := classifyClaimError(err)
			if kind == claimFatal {
				attrs := []any{"category", category}
				if category == "malformed_response" {
					attrs = append(attrs, "note", "a claim may have been committed; it is recovered when its lease expires")
				}
				w.log.Error("claim_failed_fatal_worker_stopping", attrs...)
				fatal(0, fmt.Errorf("workerruntime: claim (not retryable: %s): %w", category, err))
				return
			}
			failures++
			wait := claimRetryDelay(failures, retryAfter)
			if lastLog.IsZero() || time.Since(lastLog) >= claimLogEvery {
				lastLog = time.Now()
				w.log.Warn("claim_failed_will_retry", "category", category, "consecutive", failures, "wait", wait.Round(time.Millisecond).String())
			}
			sleepCtx(ctx, wait) // interrupted at once by shutdown
			continue
		}
		if failures > 0 {
			w.log.Info("claim_recovered", "after_failures", failures)
			failures = 0
			lastLog = time.Time{}
		}
		if len(claims) == 0 {
			giveBack(slots)
			if idle := time.Since(began); idle < minPollGap {
				sleepCtx(ctx, minPollGap-idle) // a server answering idle at once must not cause a busy loop
			}
			continue
		}
		if ctx.Err() != nil {
			// Shutdown began while the claim was in flight. No handler ran, so
			// handing the jobs back is safe.
			for _, c := range claims {
				w.releaseUnstarted(c, "claimed_after_shutdown")
			}
			return
		}
		for i, c := range claims {
			inbox[slots[i]] <- delivery{claim: c, receivedAt: receivedAt} // the slot is idle: the inbox has room
		}
		giveBack(slots[len(claims):])
	}
}

// slotLoop is one execution slot: announce that it is idle, take the job the
// claimer hands over, run it, report, repeat. It returns when ctx ends (after
// its current job is resolved or detached) or after reporting a fatal error, and
// says whether it left a handler running (detached).
func (w *Worker) slotLoop(ctx context.Context, slot int, ready chan<- int, inbox <-chan delivery, handlers map[string]HandlerFunc, timeouts map[string]time.Duration, fatal func(int, error)) (detached bool) {
	for {
		select {
		case ready <- slot:
		case <-ctx.Done():
			return detached
		}
		var d delivery
		select {
		case d = <-inbox:
		case <-ctx.Done():
			return detached
		}
		claim, receivedAt := d.claim, d.receivedAt

		if ctx.Err() != nil {
			// Shutdown began while the claim was in flight. The handler never
			// ran, so handing the job back is safe.
			w.releaseUnstarted(claim, "claimed_after_shutdown")
			return detached
		}
		if err := checkTimings(claim); err != nil {
			w.releaseUnstarted(claim, "unusable_timings")
			fatal(slot, err)
			return detached
		}
		if t := timeouts[claim.Type]; t > 0 {
			switch {
			case t < claim.Deadline:
				claim.Deadline = t // WithTimeout shortens the server's deadline
			case t > claim.Deadline:
				if _, seen := w.capNoted.LoadOrStore(claim.Type, true); !seen {
					w.log.Warn("with_timeout_exceeds_server_deadline_ignored", "type", claim.Type, "with_timeout", t.String(), "server_deadline", claim.Deadline.String())
				}
			}
		}
		h := handlers[claim.Type]
		if h == nil {
			// Cannot happen while claims list only registered types: the server
			// returned a type that was not asked for. Fail it permanently: a
			// release would just be claimed again.
			w.failUnregistered(claim)
			continue
		}
		if w.runJob(ctx, slot, h, claim, receivedAt) {
			detached = true
			return detached // this slot is occupied by the wedged handler; do not claim again
		}
	}
}

// Wedged reports how many handlers are still running although the runtime
// cancelled them (deadline, lost or unprovable ownership, or shutdown) more
// than a short moment ago. Such a job is deliberately kept alive: its lease is
// renewed, and it is neither released nor failed, until the handler returns.
// The worker's slot stays occupied meanwhile; a handler that never returns
// holds both until the process exits. A handler that is merely slow, and has
// not been cancelled, is not counted.
func (w *Worker) Wedged() int { return int(w.wedged.Load()) }
