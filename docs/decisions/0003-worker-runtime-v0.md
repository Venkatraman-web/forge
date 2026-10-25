# Design 0003 — Go Worker Runtime v0 (Task 25.3.2.1)

Status: **implemented (note added 2026-10-22).** `sdk/workerruntime` exists and follows this design: `New`, `Handle`, `Run`, slots (`Concurrency`), heartbeat and watchdog, claim backoff, graceful release, `Wedged()`, `ErrShutdownIncomplete`. Additions since the design: the `Handle` option `WithTimeout` (see section 11, item 6). Not implemented: the server-side follow-ups of section 9 (for example execution-deadline enforcement), and no per-type concurrency or `Stats()` (ADR 0004). The sections below are the original design; where they differ from the source, the source is authoritative. **Later note:** the in-process ("embedded") execution mode and the built-in `dataset_validate` handler discussed in this record were removed on 2026-10-22; Forge now always runs with customer-run remote workers. References to them below are historical. **Later note (2026-10-23):** the runtime no longer claims per slot: one claimer per worker claims as many jobs as there are idle slots in a single request (`ClaimBatch`) and hands one to each slot. Slot behaviour, heartbeats, fencing and shutdown release are unchanged.

Builds on [0001](0001-execution-architecture.md) (architecture), [0002](0002-remote-worker-api-v0.md) (remote worker API) and the typed client in `sdk/workerclient/` (Task 25.3.1). The Go code in this document was **proposed API when written; the implemented API is in `sdk/workerruntime` and is authoritative**.

## 1. What exists today (verified in source)

**Typed client (`sdk/workerclient`).** `New(Config{BaseURL, APIKey, HTTPClient})`. Methods, each one HTTP request with no retries:

| Method | Returns | Notes |
|---|---|---|
| `Claim(ctx, ClaimRequest{WorkerID, Types, Wait})` | `(ClaimedJob, found bool, error)` | `found=false` is an idle answer (204). `Wait` 0-25 s, whole seconds. 1-8 types. |
| `Renew(ctx, job)` | `(lease time.Duration, error)` | Server picks the lease length. |
| `Complete(ctx, job, json.RawMessage)` | `error` | Result is pre-encoded JSON, at most 256 KiB, no U+0000. |
| `Fail(ctx, job, Failure{Kind, Permanent})` | `(FailResult{Requeued, RetryIn}, error)` | Kinds `error`, `timeout`, `panic`. Attempt comes from `job`. No free text exists in the protocol. |
| `Release(ctx, job)` | `error` | Refunds the attempt; generation unchanged. |

`ClaimedJob` carries `JobID, Type, Input, Attempt, MaxAttempts, ClaimGeneration, WorkerID` and the timings `Lease, HeartbeatInterval, SafetyMargin, Deadline` (fractions kept). Errors: `ErrOwnershipLost` (409), `ErrUnauthorized` (401), `ErrRateLimited` (429, `Error.RetryAfter`), `ErrInvalidRequest` (other 4xx), `ErrUnavailable` (5xx), `ErrMalformedResponse`, `*TransportError` (also unwraps to context errors). `IsUncertain(err)` is true for transport errors, 5xx and malformed responses.

**Server behaviour the runtime depends on.**
- The claim is a real atomic PostgreSQL claim; the lease starts when the database commits it. A claim response lost in transit leaves a running job nobody renews: the reaper recovers it after the lease and **the attempt stays consumed**.
- Renew, Complete, Fail and Release are fenced on job ID + `status='running'` + `claim_generation` + `worker_id`. There is no lease-deadline condition: an overdue lease alone does not revoke ownership, a *committed reaper transition* does.
- Defaults: lease 15 s, heartbeat 5 s, safety margin 10 s (startup rule: heartbeat < margin < lease), fixed execution deadline 30 s (advisory; the server never enforces it).
- Operations are not replayed. After a committed Complete/Fail/Release, a repeat gets 409, indistinguishable from a genuine loss. A 503 can hide a committed write (observed in earlier validation).
- Retry policy (attempt budget, backoff, jitter) lives in the server (`worker.RetryDelay`, `Store.FailAttempt`). The server sizes the backoff from the worker-supplied `attempt` (section 8).

**The embedded worker is the reference.** `internal/worker/pool.go` and `lease.go` implement the lifecycle this runtime should mirror: per-claim heartbeat plus an independent watchdog on the local monotonic clock that cancels the handler if no renewal succeeded within the safety margin (measured from when the request was *sent*); cancel causes distinguish lost ownership, self-fence and shutdown; panics are recovered and discarded; handler error text is never logged or stored; a cooperative handler interrupted by shutdown is released with a refunded attempt; a handler that ignores cancellation is never released. The remote runtime differs where the network changes the problem (sections 5-7).

## 2. Goals and non-goals

Goals: a small Go API to register handlers and run them against Forge; correct lease, outcome and shutdown behaviour built only on the typed client; no second retry scheduler; no premature release.

Non-goals (v0): workflows, middleware/plugins, per-type concurrency limits, scheduling, cancellation from the API, a CLI, metrics backends, TLS or auth beyond the client's, changes to the server.

## 3. Proposed public API

New package `github.com/Venkatraman-web/forge/sdk/workerruntime` (package name `workerruntime`). It imports `sdk/workerclient` and the standard library only (`log/slog` for logging; no new module).

```go
// PROPOSED API - not implemented.

// HandlerFunc runs one job. It must stop promptly when ctx is cancelled.
// The returned value is JSON-encoded as the job's result; nil means "no result".
type HandlerFunc func(ctx context.Context, job Job) (result any, err error)

// Job is what a handler sees. It deliberately omits the fencing token and worker ID.
type Job struct {
    ID          string          // stable across attempts: use as the idempotency key
    Type        string
    Input       json.RawMessage // exactly as submitted
    Attempt     int             // 1 for the first execution
    MaxAttempts int
}
func (j Job) Decode(v any) error // json.Unmarshal(j.Input, v); strictness is the caller's choice via v

// Permanent marks an error as one that retrying cannot fix. The error text is
// never sent to Forge or logged; only the marker is used.
func Permanent(err error) error

type Config struct {
    Client        *workerclient.Client // required
    Name          string               // default: hostname; letters, digits, . _ -; at most 40 chars
    Concurrency   int                  // default 1; 1..64 handlers at a time
    PollWait      time.Duration        // default 20s; 1s..25s
    ShutdownGrace time.Duration        // default 30s: how long Run waits for handlers after ctx is cancelled
    ReportAttempts int                 // default 2 total attempts (one resend of an uncertain Complete/Fail/Release); 1..3. See section 7.
    Logger        *slog.Logger         // default: discard. Never receives handler error text or payloads.
}

type Worker struct{ /* unexported */ }
func New(cfg Config) (*Worker, error)             // validates; starts nothing
func (w *Worker) Handle(jobType string, h HandlerFunc) error // before Run; 1..8 distinct types
func (w *Worker) Run(ctx context.Context) error   // blocks; call once
func (w *Worker) Wedged() int                     // handlers that ignored cancellation and have not returned

var ErrShutdownIncomplete error // Run's result when handlers were still running at the grace deadline
```

Illustrative use:

```go
// PROPOSED API - not implemented.
client, _ := workerclient.New(workerclient.Config{BaseURL: os.Getenv("FORGE_URL"), APIKey: os.Getenv("FORGE_KEY")})
w, _ := workerruntime.New(workerruntime.Config{Client: client, Name: "billing", Concurrency: 4})

w.Handle("order.send", func(ctx context.Context, job workerruntime.Job) (any, error) {
    var in struct{ OrderID string `json:"order_id"` }
    if err := job.Decode(&in); err != nil {
        return nil, workerruntime.Permanent(err) // malformed input never gets better
    }
    if err := sendOrder(ctx, in.OrderID, job.ID /* idempotency key */); err != nil {
        return nil, err // retryable
    }
    return map[string]any{"sent": true}, nil
})

ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
if err := w.Run(ctx); err != nil { log.Fatal(err) } // nil only after a clean drain
```

Design choices and why:
- **Types come from `Handle`.** The runtime claims only the types it can run, so "claimed a job with no handler" cannot occur in normal operation. This caps a worker at 8 handler types (the claim limit); more types means more workers.
- **`Job` hides generation and worker ID.** Handlers must not touch the fencing token; the job ID is the idempotency key and `Attempt` is informational.
- **`result any`** is marshalled by the runtime with `encoding/json`. The alternative (`json.RawMessage`) pushes encoding into every handler.
- **`Permanent(err)`** mirrors `handlers.Permanent` in the embedded code. It is the only way a handler influences classification; there is no free-text failure channel because the protocol has none.
- **`ReportAttempts`** is the one non-obvious knob; its default and semantics need a decision (section 7, open question 1).
- The runtime owns a `workerclient.Client` the caller built, so TLS, proxies and timeouts stay the caller's choice. Its `HTTPClient.Timeout` must exceed `PollWait` (the default 45 s does).

## 4. Handler contract and failure mapping

Handlers may run concurrently (up to `Concurrency`), each in its own goroutine, so they must be safe for concurrent use. A handler **must** watch `ctx`: it is cancelled for four reasons, recorded as the context cause (internal): execution deadline passed, ownership lost, self-fence (no renewal within the safety margin), shutdown. The runtime cannot kill a goroutine; a handler that ignores `ctx` is handled by section 6.

| Handler outcome | Condition | Reported via `workerclient` |
|---|---|---|
| returns `nil` error | any, except that a fired deadline makes it a timeout (row below) | `Complete(result)`. A result that cannot be JSON-encoded, exceeds 256 KiB, contains U+0000 or contains invalid UTF-8 is a handler bug: reported as `Fail{KindError, Permanent: true}`, logged by kind only |
| returns error marked `Permanent` | not cancelled by lost/self-fence | `Fail{KindError, Permanent: true}` |
| returns any other error | cause none | `Fail{KindError}` (retryable) |
| returns (any result, including success), cause = deadline | the runtime's execution deadline fired; a late success is not accepted as one | `Fail{KindTimeout}` (retryable) |
| panics | recovered in the handler goroutine; value and stack discarded | `Fail{KindPanic}` (retryable) |
| returns `context.Canceled`-matching error, cause = shutdown | the claim is still held | `Release` (section 6), no failure recorded |
| returns, cause = ownership lost or self-fence | | nothing is reported (the claim is gone or unprovable); logged |

**Late results.** A handler's eventual return is reported only while its claim is still valid. If ownership was lost or the claim was self-fenced at any point before the handler returned, nothing is reported for that claim, however the handler eventually returns (including with a successful result). Of the remaining cases the outcome is decided once, from the cancellation reason recorded when the handler returned; a later renewal conflict, shutdown or deadline cannot change it.

Notes:
- Classification uses `errors.Is`/`errors.As` and context causes, never error text. The error text is never sent (the protocol cannot carry it) and never logged.
- **Unknown job type** (server returned a type with no handler, a protocol violation): `Fail{KindError, Permanent: true}`, matching the embedded worker's terminal "unregistered type" failure. Releasing instead would let the same worker re-claim it in a loop.
- A permanent failure does not consume extra attempts; the server decides terminal state and `attempts_exhausted`.
- The runtime never computes or sends a retry delay. `RetryIn` from `FailResult` is only logged.

## 5. Per-job lifecycle

Each claimed job has up to three goroutines: the **handler** goroutine, a **heartbeat** goroutine and a **watchdog** timer. A slot goroutine owns the job until it is fully resolved.

```
slot: Claim ──found──▶ start timers ─▶ handler goroutine ──returns──▶ classify ──▶ report ──▶ stop heartbeat ──▶ next claim
                         │                  ▲  ▲
                 heartbeat: Renew every HeartbeatInterval ── 409 ──▶ cancel(lost)
                 watchdog:  no Renew success for SafetyMargin ─────▶ cancel(self-fence)
                 deadline:  Deadline elapsed ──────────────────────▶ cancel(deadline)
                 shutdown:  ctx cancelled ─────────────────────────▶ cancel(shutdown)
```

**Time bases (the one place the network changes the embedded design).** The embedded pool measures the lease from the moment it *sent* the claim. For a remote long poll that is wrong: a claim that waited 20 s for work would look 20 s stale and self-fence at once. The lease actually starts when the server commits the claim, a moment before the response arrives. So the runtime sets `lastOK` = **the time the claim response was received** (monotonic clock). That can overestimate the remaining lease by at most the response transit time; the safety slack `Lease - SafetyMargin` (5 s by default) must cover it. This assumption is recorded as a server guarantee to keep (`SafetyMargin < Lease`) and a candidate for a server hint (section 9).

**Heartbeat.** First renewal at `HeartbeatInterval` after the claim response, then every interval. Each request gets its own timeout of one interval so a hung call cannot stall the schedule. Success sets `lastOK` to the time the request was **sent** (not when the reply arrived), as the embedded code does. Results:
- `nil`: continue.
- `ErrOwnershipLost`: cancel the handler with cause *lost*, stop the heartbeat, report nothing.
- anything else (including `ErrUnauthorized`, 5xx, transport): the claim may still be ours; keep trying every interval, log rate-limited (first and every fifth consecutive failure).

**Watchdog.** Independent of any in-flight request: a timer checks the monotonic clock (tick = `SafetyMargin/20`, clamped to 10-250 ms, as in the embedded code). If `now - lastOK > SafetyMargin` it cancels the handler with cause *self-fence*. Heartbeat attempts continue afterwards (section 6, wedged handlers).

**Deadline.** The handler context gets `context.WithTimeoutCause(Deadline, errDeadline)` measured from the claim response. The server never enforces it (section 9), so this is the only enforcement.

**Return-time freshness check (Task 25.3.2.5).** A process that is paused (SIGSTOP, VM freeze) resumes with the watchdog tick and the handler's own timer both due; the handler may return first. So at the moment the handler returns, the runtime itself compares the monotonic clock with `lastOK`; if the safety margin has passed it marks the claim self-fenced and reports nothing. Found by a SIGSTOP/SIGCONT trial in which a stale `Complete` was sent once in six runs (the server fenced it with 409; the new owner was unharmed). After the fix: 0 stale reports in 32 trials.

**Outcome phase.** When the handler returns, the runtime classifies and reports. Unlike the embedded pool, the heartbeat is **not** stopped first: it keeps renewing until the outcome is decided (or abandoned), so the lease cannot lapse while a slow Complete is in flight. The embedded code stops the heartbeat first so a renewal 409 cannot be mistaken for a lost claim during completion; the remote runtime gets the same safety by phase: once the handler has returned, heartbeat and watchdog results can no longer cancel anything or change the outcome path, they only keep the lease alive. A late renewal 409 after a committed Complete is expected and ignored.

**Outcome handling.** Each report uses a context detached from shutdown with its own timeout (proposed 10 s). Results:
- success: log the outcome (`completed`, `requeued` + `RetryIn`, `failed`, `released`).
- `ErrOwnershipLost`: nothing was applied, **or** an earlier attempt of the same request was applied (section 7). Log `outcome_unknown_or_lost`; do not repeat.
- `IsUncertain`: governed by `ReportAttempts` (section 7). If unresolved, log `outcome_unconfirmed` and stop renewing: the lease will expire and the reaper will recover the job (the attempt stays consumed). The runtime never claims success it was not told about.
- any other error (`ErrInvalidRequest`, `ErrUnauthorized`): a bug or configuration problem; log and stop.

## 6. Shutdown, release, and handlers that ignore cancellation

**Shutdown trigger.** Cancelling the context passed to `Run` begins shutdown. There is no second signal path.

**Sequence.**
1. Stop claiming. Slots that are idle or waiting in a long poll abandon the poll (the in-flight `Claim` is cancelled; this is fast, at the cost of the small window where the server committed a claim whose answer the client dropped: lease recovery handles it, the attempt is consumed. Setting a short `PollWait` narrows nothing: the window is the response transit time, independent of the wait).
2. A claim response that arrives after shutdown began is **not** started. Its handler never ran, so releasing it is safe: `Release` (bounded timeout), log `released_unstarted`.
3. Cancel every active handler's context with cause *shutdown*.
4. Wait for handler goroutines until `ShutdownGrace` elapses.
5. For each handler that returned: if it returned a `context.Canceled`-matching error and the claim is still held (not lost, not self-fenced), stop its heartbeat and call `Release` (attempt refunded). If it returned success despite the cancel, `Complete` as usual. Any other error follows the normal failure mapping (consumes an attempt, which is safe).
6. `Run` returns `nil` only when every job is resolved (completed, failed, released, or lost) and no goroutine of the runtime remains.

**Release safety rule (mandatory).** `Release` is called only when *all* of these hold: the handler goroutine has returned; it returned because of the shutdown cause; heartbeat and watchdog for that job have stopped; the claim was not lost or self-fenced. If any condition is false the runtime does not release.

**Handlers that ignore cancellation ("wedged").** Go cannot stop them. The hazard: if the runtime stops renewing, the lease expires, the reaper requeues the job and a second worker runs it while the first handler may still be performing side effects. The rule forbids that, so the runtime does the following:
- The runtime **keeps renewing** the lease of any job whose handler goroutine is still running, whatever the reason it should have stopped (deadline, self-fence, shutdown). Wedged jobs are counted (`Wedged()`) and logged at error level every 30 s with job ID and age.
- The slot stays occupied (the concurrency bound is honest) and the job is **not** released, failed or reported.
- At the shutdown grace deadline `Run` returns `ErrShutdownIncomplete` (with the count). The renewal goroutines for wedged handlers deliberately outlive `Run`: they are the only thing keeping other workers away from a still-running handler, and they end when the handler returns or the process exits. They are the one intentional exception to "no goroutine survives Run".
- **The operator's remedy is to exit the process.** That kills the handler and stops renewals together, so the lease expires and the reaper recovers the job: safe by construction. `Run`'s documentation and the error message say so.
- If a wedged handler's renewal returns 409 (the job was reclaimed anyway, for example because renewals failed for a whole lease), concurrent execution has already happened; the runtime logs it and stops renewing. It is a loud, bounded failure of the guarantee, not a hidden one.

The trade-off is explicit: a handler that never returns holds its job and a slot indefinitely. The missing server-side tool is execution-deadline enforcement (section 9); adding it would turn "hold forever" into "hold up to a maximum, then allow concurrent execution", which is a policy choice for the reviewer, not something the runtime can decide alone.

## 7. Retry, ambiguous outcomes and repetition

- **Retry policy stays on the server.** The runtime reports `retryable` and a kind; the server picks the delay, decides requeue versus terminal, and applies `attempts_exhausted`. No local retry scheduler, no local backoff for jobs.
- **Attempt value.** `Fail` sends `job.Attempt` from the claim (the SDK derives it; handlers cannot set it). The server uses the worker-supplied value to size the backoff and does not check it against the stored attempt. A modified client can shorten its own job's delay; it cannot change the retry decision.
- **Ownership conflicts (409)** during an outcome report are final for that claim: stop, log, never repeat.
- **What a repeat can and cannot do.** Complete, Fail and Release are single fenced transitions. Repeating one cannot corrupt state: if the first committed the repeat gets 409; if it did not, the repeat applies. What a repeat **cannot** do is establish the original outcome, because 409 is ambiguous (genuine loss versus already applied). The embedded worker already resends fenced writes up to three times on database errors with the same ambiguity.
- **Decision for v0 (approved in review, supersedes the conservative draft).** `ReportAttempts` defaults to **2** total attempts (accepted range 1-3; 1 means never resend). A resend repeats the *same* request, only for `IsUncertain` errors, never after a 4xx or 409, with 200-500 ms pauses and a total budget below one lease, and a following 409 is reported as `outcome_unknown_or_lost`, never as success or failure. Cost of resending: when the first attempt had committed, the final status stays unknowable. Cost of not resending: every transient 503/timeout makes the job run again after the lease expires (a duplicate execution). The server change in section 9 (replayable Complete) would remove the ambiguity for the most important case.
- **Claim errors (implemented in 25.3.2.4.2, `claimerr.go`).** A failed claim is never repeated as the same request; the slot waits and issues a new claim. *Fatal* (the worker stops, other slots are shut down through the coordinator, the typed error is preserved): `ErrUnauthorized`, `ErrInvalidRequest` (every other 4xx, including a wrong URL), `ErrMalformedResponse` (not Forge's claim contract; each retry could commit and lose another claim) and client-side argument errors. *Transient* (per-slot backoff, no retry limit): `ErrRateLimited` (wait at least `Retry-After` when it is 0 < d <= 5 min, plus 250 ms to 5 s of jitter; otherwise the exponential schedule), `ErrUnavailable` and `*TransportError` (1 s doubling to 30 s, jittered to between half and all of the value). Shutdown interrupts every wait. The failure counter is per slot and resets on any valid claim answer, including an idle 204, which never causes backoff. Persistent failures are logged at most every 30 s per slot, and recovery is logged. A claim that failed with a 5xx, a transport error or an unreadable 200 may have committed; it is recovered by lease expiry and the attempt stays used.
- **Duplicate execution.** The runtime makes no exactly-once promise. Handlers run at-least-once: after a crash, a self-fence, an unconfirmed outcome, a lost claim response, or a wedged handler's lease lapsing. Handlers must be idempotent, keyed on `Job.ID`.

## 8. Concurrency and claiming

- `Concurrency` slots, each a goroutine that loops claim → run → resolve. A slot claims only when it is free, so the runtime never holds more jobs than it can run: no prefetch, no local queue.
- Each slot has its own worker ID `<Name>-<instance>-<slot>` where `<instance>` is 8 random hex characters per `Worker`; the total must match the server's `^[A-Za-z0-9._-]{8,64}$`. Uniqueness is not a security property (the generation fences), but distinct IDs make logs and the `worker_id` column readable.
- Server load: each idle slot holds one long poll (default 20 s). The server caps simultaneous waiting polls (`FORGE_WORKER_MAX_POLLS`, default 64), so a fleet whose total `Concurrency` exceeds that sees 429; the runtime honours `Retry-After`. Operators size one number against the other.
- Claims from one worker ask for all registered types at once; the server gives the oldest due job among them, so there is no per-type fairness or limit in v0.
- Shared state is minimal: a counter of active/wedged jobs behind a mutex or atomics. No handler state is shared by the runtime.

## 9. Missing server-side guarantees (follow-ups, none done here)

1. **Execution-deadline enforcement.** Nothing server-side bounds a handler's runtime while it keeps renewing. Smallest change: `RenewLease` also requires `attempt_started_at + max_runtime > now()`, with a distinct response (for example 409 `deadline_exceeded`) so it is not confused with a reaped claim. Needs a policy decision (see section 6).
2. **Backoff from a worker-supplied attempt.** Smallest hardening: compute the delay inside `Store.FailAttempt`'s SQL from the persisted `attempt` (base/max as parameters, jitter with `random()`), and drop the `attempt` request field (or ignore it). No schema change.
3. **Replayable outcomes.** Making a repeat of Complete answer 200 when the job is already `succeeded` with `claim_generation` equal to the request's generation needs no schema change (a later claim would have raised the generation) and would let the runtime resend Complete safely and know the outcome. Fail and Release have no such marker; they would need a small recorded outcome per generation. This is the real fix for the ambiguity in section 7.
4. **Claim-response loss.** A hint in the claim response (for example the server-side remaining lease) would replace the assumption "response time is close to commit time". Optionally the server could release a claim whose response write failed.
5. **Release abuse.** A worker that claims and releases forever burns no attempts. A per-job release counter or a cool-down is a future server decision.
6. **Per-service authorization** and key scoping remain the product-level prerequisite from 0001/0002.

## 10. Implementation plan

Dependency-ordered; each task leaves the repository buildable and is validated with a scratch harness against a disposable PostgreSQL and a fake server, with no repository test files.

1. **25.3.2.2 - Public API surface, no network.** Package `workerruntime`: `Config` validation and defaults, `New`, `Handle` (limits, duplicate types, type format), `Job`/`Decode`, `HandlerFunc`, `Permanent`, worker-ID generation, and the two pure functions the rest depends on: error classification (handler outcome + context cause → failure kind/permanence/release/none) and result encoding/validation. `Run` exists but returns an error "not implemented". *Done when:* `go build/vet` pass; a harness drives classification (including `errors.Is` wrapping, panics are not testable yet) and configuration validation through the public API only.
2. **25.3.2.3 - Single-job lifecycle, reachable through a one-slot `Run`.** Handler goroutine with panic recovery and deadline; heartbeat and watchdog with the time bases above; cause-tracked cancellation; outcome reporting with `ReportAttempts`; wedged-handler hold. `Run` here is deliberately minimal: one slot, claim → run → resolve, fixed 1 s claim backoff, context cancel stops after the current job. *Why this boundary differs from a "lifecycle without a loop" split:* a harness outside the repository can only reach exported API, so the lifecycle must be driven through `Run`; keeping that `Run` one-slot keeps the review small. *Done when:* against the real server and a fake server: success, retryable and permanent failure, timeout, panic, renewal 409 mid-run, self-fence with a stalled renewal, uncertain Complete, handler that ignores cancellation (wedged, lease kept), no goroutine growth.
3. **25.3.2.4 - Polling loop and concurrency.** *(Multi-slot concurrency delivered in 25.3.2.4.1: `Concurrency` slots, per-slot stable worker IDs, coordinated shutdown; claim backoff, `Retry-After` and the fatal-error policy remain for 25.3.2.4.2.)* `Concurrency` slots, per-slot worker IDs, claim backoff with jitter, `Retry-After`, fatal configuration errors (401/400 from claim) stopping the whole worker, bounded claiming. *Done when:* N concurrent handlers never exceed N, no job is run twice concurrently, 429/503/outage behaviour verified, idle slots cost only their long polls.
4. **25.3.2.5 - Graceful shutdown and hardening.** Full shutdown sequence (abandon polls, release-unstarted, cancel causes, grace, release of cooperative handlers, `ErrShutdownIncomplete`, `Wedged()`), release safety rule, goroutine accounting (only wedged-guardian goroutines survive `Run`), end-to-end runs against the real server including crash (`SIGKILL`), `SIGSTOP`/`SIGCONT`, database outage, and Forge restart. Documentation: README section and the operator guidance in section 6. *Done when:* the scenarios in 25.1 and 23 are reproduced through the SDK path with matching results.

Each later task may also touch README/CLAUDE.md for its own limitations. Server follow-ups (section 9) are separate tasks and none blocks the runtime; item 3 would change the recommended `ReportAttempts` default.

## 11. Open questions for review

1. ~~`ReportAttempts` default~~ - resolved in review: 2 total attempts, range 1-3.
2. **Wedged handlers hold forever.** Accept "hold and tell the operator to exit", or require server-side deadline enforcement first?
3. **Package name and location** (`sdk/workerruntime`).
4. **`log/slog`** as the only logging hook, versus a tiny logging interface.
5. **Expose `ClaimGeneration` on `Job`?** Current answer no; handlers use `Job.ID`.
6. **Per-handler execution limits** (a shorter deadline for a given type) were omitted from v0; `workerruntime.WithTimeout` (a `Handle` option that can only shorten the server's deadline) now provides them. See the README, "Job execution timeouts".
7. **Whether a claim wait of 20 s is the right default** against the server's 25 s cap and a proxy's idle timeout.

No question blocks task 25.3.2.2.
