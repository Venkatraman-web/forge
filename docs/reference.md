# Forge reference: architecture, configuration and API

> The short introduction is the [README](../README.md); a map of every folder and file is [project-map.md](project-map.md). This page is the long reference: how Forge works inside, every setting, every endpoint, the SDKs, metrics and security notes. Some sections still read like a development diary (task numbers, phases); the code and the numbers are current, the wording is not always tidy.

**A Go-based background-job execution and observability platform for developers.**

Forge is intended to let developers submit background jobs from their applications, execute those jobs through workers, and inspect job execution and background-processing health across their registered services. The long-term product vision includes service registration, tenant isolation, a developer dashboard, operational metrics, and self-hosted deployment. Those capabilities are not all implemented today.

> **Project overview and measured results:** [`docs/project-overview.md`](project-overview.md).

> **How it works and why (components, failure cases, measured limits, interview questions):** [`docs/architecture.md`](architecture.md).

> **Run Forge yourself on AWS (Docker), try it, secure it, load-test it:** [`docs/deploy-on-aws.md`](deploy-on-aws.md); the smallest complete application is [`examples/hello`](../examples/hello/main.go).

> **Current status (reconciled with the code on 2026-10-22):** Forge is a locally validated engine, not a production service. Jobs are stored in PostgreSQL (GORM v2, schema managed by versioned Atlas migrations) and the `jobs` table is the durable queue. A claim is fenced and leased, a heartbeat renews the lease, a reaper recovers jobs whose worker died, and failures are retried with backoff up to a per-job attempt limit. Jobs run in the customer's own worker processes, built with the Go worker SDK, which claim them through Forge's worker API; Forge never runs customer code itself. Submission is bounded: an exact queued-job limit and a bounded-admission gate return distinct, retryable `503` responses. An opt-in operator endpoint reports queue status. Validation is manual and scratch-based, and the measured numbers are single-host observations, not capacity guarantees. **Do not expose Forge to the public internet:** all three APIs (submission, worker, operator) are protected by their own single shared static key over plain HTTP, which separates roles but is not user or tenant authentication.

## Testing

There are no automated tests in the repository (see [architecture.md](architecture.md), section 7).

## Quickstart with Docker (Forge, PostgreSQL and a first job)

This runs the Forge server and its database in containers, then runs a ten-line worker and a job submission from Go. You need Docker (with Compose) and Go 1.24 or newer.

```bash
cp .env.example .env
# edit .env: replace CHANGE_ME_before_first_start in POSTGRES_PASSWORD and FORGE_DATABASE_URL,
# and set FORGE_API_KEY, FORGE_WORKER_API_KEY and FORGE_OPERATOR_API_KEY to three different
# values of at least 32 characters, for example the output of: openssl rand -hex 24

docker compose --profile forge up -d --build     # PostgreSQL, one-shot migration, Forge
curl http://127.0.0.1:8080/health                 # {"status":"ok"}
```

Forge runs no handlers itself: jobs run in workers you write. To see jobs flow end to end, run the reference application in [`forgecart/`](../forgecart/README.md); to write your own, follow the pattern below.

What runs: `postgres` (the job store, data in a named volume), `migrate` (applies `migrations/` with a pinned Atlas image and exits), and `forge` (a 34 MB image running as an unprivileged user, in `remote` mode). Forge accepts only the job types in `FORGE_REMOTE_JOB_TYPES` (default `hello.greet`; set your own in `.env`). All three ports (8080 submission, 8081 worker, 8082 operator) are published on `127.0.0.1` only. Add `--profile observability` to also start Prometheus and the Grafana dashboard (see Metrics); in this setup Prometheus reaches Forge over the compose network and needs no extra settings. Plain `docker compose up` still starts only PostgreSQL, for running Forge from source.

For your own service, copy the shape of the ForgeCart workers (`forgecart/internal/workerapp`) and its dispatcher (`forgecart/internal/dispatcher`): the worker half uses `sdk/workerruntime`, the submitting half uses `sdk/forgeclient`, and they run in different processes. Starting Forge without `FORGE_WORKER_API_KEY` stops it with a clear message (the container then restarts in a loop until the key is set).

Limits of this packaging: the quickstart uses one static key per role and plain HTTP (several named, rotatable keys are available, see `FORGE_KEYS_FILE` below; there is no TLS), so keep it on one trusted machine; it is a single Forge process (running several against one database is unvalidated); the image is built locally and not published to a registry; it is not an installer or a hosted service.

## Current implementation

Implemented (each item is described in the sections below or in `docs/decisions/`):

- REST API using Go's standard library (`net/http`): submit a job, read one, list recent jobs, health.
- PostgreSQL job store (GORM v2) with Atlas-managed, versioned migrations (seven so far: jobs table, execution schema, sharded queue counter, per-job timeout, and the hand-written submit, batch-submit and counter-shard functions); `AutoMigrate` is never used.
- PostgreSQL-backed queue: workers claim due jobs with `FOR UPDATE SKIP LOCKED`, fenced by a claim generation; leases, heartbeats, self-fencing and a reaper recover abandoned jobs (at-least-once execution).
- Retries: retryable versus permanent failures, exponential backoff with jitter, a per-job attempt limit (`options.max_attempts`, default 3), and the terminal `attempts_exhausted` state.
- Execution timeouts: a per-job timeout per attempt (`options.timeout_seconds`, default 30 s, capped by `FORGE_MAX_JOB_TIMEOUT`), delivered to workers with each claim, plus the worker-side per-job-type `WithTimeout`.
- Workers are customer-run processes (built with `sdk/workerruntime`) that claim jobs through the worker API; Forge runs no handlers of its own.
- Public Go SDK: `sdk/forgeclient` (submit and inspect jobs, with typed backpressure errors), `sdk/workerclient` (typed worker API client) and `sdk/workerruntime` (runs your handlers: concurrency slots, per-job-type `WithTimeout`, graceful shutdown); the remote worker API they talk to.
- Backpressure: an exact global queued-job limit (`queue_full`, `Retry-After: 5`) and bounded admission (`admission_busy`, `Retry-After: 1`); the exact queue counter is maintained by database triggers.
- Opt-in operator endpoint `GET /v1/queues` and a Prometheus `GET /metrics` (own listener and key): queue depth and age per job type, submissions by result, job events, reaper recoveries, admission and database pool state.
- A reference application, **ForgeCart** (`forgecart/`), and a scenario-based load-test and regression suite (`forgecart/test_files/`).
- Manual end-to-end validation, recorded in this file, the ADRs and `forgecart/test_files/report.md`.

Not implemented (do not assume): per-user authentication or multi-tenant isolation (Forge is single-tenant by design: run one deployment, with its own database and keys, per team or application; inside a deployment any submit key can read every job), service registration, service registration, a dashboard, latency histograms, per-job-type timeout or retry policy, server-side enforcement of the execution timeout, per-type queue limits, cancellation, delayed or scheduled jobs through the API, exactly-once execution, and any measured production capacity guarantee.

Job records and queued work persist across Forge restarts. A job that was *running* when its worker died is recovered after its lease expires (at-least-once: the handler may run again).

## Current runtime architecture

```text
Client --POST /v1/jobs--> HTTP API --> Job service --INSERT (queued, if below the limit)--> PostgreSQL jobs table
                                                                                              ^
Worker processes (customer-run, via the worker API) --claim / complete / fail (conditional SQL)--+
   |  run the customer's handler under a timeout
   +--> handler code in the worker, not in Forge

Client --GET /v1/jobs/{id}--> HTTP API --> PostgreSQL jobs table
```

The jobs table is the only queue: there is no in-memory backlog. A worker claims one due job in a single SQL statement (the job becomes `running` with a new `claim_generation`, an incremented `attempt`, its unique worker ID, and a lease), runs the handler while a heartbeat renews the lease, then records `succeeded` or `failed` with a write that only matches while that exact claim is still current. If it matches nothing (`ErrLostOwnership`), the worker discards its result instead of overwriting the newer state. A claim request that finds no work waits and re-checks the database every `FORGE_POLL_INTERVAL` (default 1 s) until its wait ends.

**Lease, heartbeat and reaper (Task 23).** The lease says "this worker is alive and working on the job until time T". While a handler runs, the worker renews the lease every `FORGE_HEARTBEAT_INTERVAL` with a write that only matches the current claim. A separate watchdog timer on the worker's own monotonic clock cancels the handler ("self-fence") if no renewal has succeeded for `FORGE_LEASE_SAFETY_MARGIN`; it does not depend on the database answering. A reaper goroutine (every `FORGE_REAPER_INTERVAL`, one atomic `FOR UPDATE SKIP LOCKED` statement, safe with several reapers) looks for `running` jobs whose lease deadline has passed. It increments `claim_generation`, which is what revokes the old owner, and then either puts the job back to `queued` (claimable after `FORGE_RECOVERY_BACKOFF`, error `lease_expired`) or, when `attempt` has reached `max_attempts`, marks it `failed` with `attempts_exhausted`. Renew, complete and fail are all fenced on `status='running'` plus the claim generation and worker ID; a write that matches nothing means ownership was lost, and the worker then writes nothing. `claim_generation` is separate from `attempt`: attempt counts executions, the generation is the fencing token and also increases on every reaper transition. 

**Workers.** Forge starts no workers and has no handler registry: it does not execute jobs itself. Customer-run workers execute them and talk to the worker API described next; the reaper runs inside Forge. Submissions are checked against the declared job types (`FORGE_CONFIG_FILE`, or `FORGE_REMOTE_JOB_TYPES`): unknown types are rejected, and the input need only be valid JSON. Claiming filters by job type inside the SQL statement (`Store.Claim` takes the types; an empty list is an error, never "all types"). An earlier in-process mode with a built-in `dataset_validate` handler was removed; `FORGE_EXECUTION_MODE` other than `remote` and `FORGE_WORKERS` are now startup errors.

**Remote worker API: claim, renew, complete, fail and release endpoints (Tasks 25.2.2 to 25.2.6).** In remote mode Forge also starts a second HTTP server on `FORGE_WORKER_ADDR`, separate from the submission/status API so that only this port needs to be reachable by workers (and so it can allow long requests). Every request must carry `Authorization: Bearer $FORGE_WORKER_API_KEY`; anything else gets an identical `401` before any database access, on every path. **This single shared key is a development stand-in: it has no per-customer or per-service scope, no rotation or revocation, no brute-force throttling, and the server speaks plain HTTP (put TLS in front of it). Do not expose this endpoint publicly as a production or multi-tenant service.** The operations implemented are claim, renew, complete, fail and release. Claim is

```
POST /v1/worker/claim
{"worker_id": "billing-host1-4821", "types": ["order.send"], "wait_seconds": 20}
```

`worker_id` is 8-64 characters from `A-Za-z0-9._-`; `types` lists 1-8 job types from `FORGE_REMOTE_JOB_TYPES`; `wait_seconds` is optional, 0-25 (default 0). Forge runs the atomic PostgreSQL claim, filtered to those types, and answers `200` with `job_id`, `type`, `input`, `attempt`, `max_attempts`, `claim_generation`, `worker_id`, and the timing values `lease_seconds`, `heartbeat_interval_seconds`, `safety_margin_seconds`, `deadline_seconds`; or `204 No Content` if nothing eligible appeared within `wait_seconds` (a long poll that re-checks the database every `FORGE_POLL_INTERVAL`). Errors use the usual `{"error":{...}}` shape: `400` invalid request, `401` unauthenticated, `405`, `413`, `429` too many waiting polls (with `Retry-After`), `503` when the database cannot be reached (never a `204`). A waiting claim ends at once when the client disconnects or Forge shuts down. The worker server's write timeout is 40 s (25 s maximum wait plus margin); a client or proxy in front must allow at least that.

**Claiming several jobs in one request.** Add `"max_jobs": N` (1 to 20) to the claim request and the answer becomes `{"jobs": [ ... ]}`, each entry shaped like the single-job answer above (its own `claim_generation`, all under the request's `worker_id`). Forge takes up to N eligible jobs in **one statement**, so the scan of the queue head and the counter update are shared; the batch also stops growing once the jobs' inputs add up to about 512 KiB (the first job is always returned, however large), so the answer stays small. Fewer jobs than asked for may come back, and `204` still means nothing was available. Every job of a batch is renewed, completed, failed or released individually and is fenced individually, exactly like a single claim; if the response is lost, each lease expires and the reaper recovers each job. Without `max_jobs` nothing changes, so older workers keep working. `sdk/workerclient` offers `ClaimBatch`, and `sdk/workerruntime` now uses it: one claimer per worker claims as many jobs as there are idle slots in a single request and hands one to each slot (it never claims more than the idle slots, so no claimed job waits unrenewed). Measured locally (database limited to 2 CPUs, 48 slots): database time spent on claiming fell from 2.23 ms to 0.35 ms per job and the completed rate rose from 889 to 1,452 jobs/s; see `docs/benchmarks-aws.md` for the real-disk measurement (if present).

Renew keeps a claim alive:

```
POST /v1/worker/jobs/{job_id}/renew
{"worker_id": "billing-host1-4821", "claim_generation": 4}
```

`job_id` must look like `job_` plus 16 hex characters; `claim_generation` is a whole number of at least 1, both taken from the claim response. The client cannot choose the lease length: unknown fields are rejected and the server extends the lease by `FORGE_LEASE_DURATION`. The reply is `200 {"lease_seconds": 15}` after the database confirmed the renewal. The update only matches while the job is `running` under exactly that generation and worker, so a stale generation, a different worker, a finished or requeued job and an unknown job ID all get the same `409 ownership_lost`; the worker must stop its handler and make no further calls for that claim. A database problem is `503`, never `409`: it does not prove ownership is lost, and the worker should keep trying until its safety margin runs out. An overdue lease alone does not revoke ownership: a late renewal still succeeds until the reaper has committed a new generation, and the database's row lock decides which of a racing renewal and reaper pass wins. Other errors: `400` invalid request, `401`, `405`, `413`.

Complete reports success:

```
POST /v1/worker/jobs/{job_id}/complete
{"worker_id": "billing-host1-4821", "claim_generation": 4, "result": {"sent": true}}
```

`result` is optional. It may be any valid JSON value of at most 256 KiB (the whole body may be up to 1 MiB); a missing field or `null` stores no result; a string or object key containing U+0000 is rejected with `400` because PostgreSQL's jsonb cannot store it; a larger result gets `413`. The result becomes readable through `GET /v1/jobs/{id}`, so workers must not put secrets in it. The reply is `200 {"outcome": "succeeded"}` only after the store confirmed the write. It uses a fenced statement (`Store.Complete`): it matches only a job that is `running` under exactly that generation and worker, then marks it `succeeded`, stores the result and finish time, and clears the error, lease and worker. Anything else (stale generation, other worker, queued, finished or unknown job) is `409 ownership_lost`, and the row is untouched. A repeated completion therefore also gets `409`; Forge does not replay successes. `503` means the outcome is **not confirmed**: the job may or may not have been completed (in testing, a request that timed out against a frozen database was committed when the database resumed), so a worker should resend the same request a few times and treat a later `409` as "stop, outcome unknown" rather than as proof that it failed. Other errors: `400`, `401`, `405`, `413`.

Fail reports that the attempt failed:

```
POST /v1/worker/jobs/{job_id}/fail
{"worker_id": "billing-host1-4821", "claim_generation": 4, "attempt": 1, "retryable": true, "kind": "error"}
```

All five fields are required and nothing else is accepted: no free text, no retry delay. `retryable` must be a real JSON boolean (a missing field is rejected, never read as "permanent"); `kind` is exactly `error`, `timeout` or `panic` and selects a fixed public error (`execution_failed` / `timeout` / `execution_failed` with the panic message), the text Forge stores for that kind; `attempt` (1-20, from the claim response) is used only to size the backoff. Forge computes the delay itself with the server-wide backoff (`FORGE_RETRY_BASE_DELAY` and `FORGE_RETRY_MAX_DELAY`, doubling per attempt, +/-20% jitter) and applies it with the database clock, and `Store.FailAttempt` decides in one fenced statement, from the persisted `attempt` and `max_attempts`, between two outcomes: `200 {"outcome": "requeued", "retry_in_seconds": 2.1}` (the job is `queued` again, claimable after that delay, with the generation unchanged until the next claim) and `200 {"outcome": "failed"}` (terminal: `retryable: false` keeps its own error, a retryable failure with no attempts left becomes `attempts_exhausted`). A stale generation, another worker, or a job that is not running gets `409 ownership_lost` and is untouched; a database problem is `503`. As with completion, `503` means the outcome is **not confirmed**: in testing a request that timed out against a frozen database was committed when the database resumed, and the repeat then got `409`. Other errors: `400`, `401`, `405`, `413`.

Release hands a job back, for example when a worker shuts down cleanly:

```
POST /v1/worker/jobs/{job_id}/release
{"worker_id": "billing-host1-4821", "claim_generation": 4}
```

It uses a fenced statement (`Store.Release`): a job that is still `running` under exactly that generation and worker becomes `queued` and claimable at once, the interrupted attempt is refunded (`attempt - 1`, never below 0), worker and lease are cleared, an error recorded by an earlier attempt is kept, and `claim_generation` is **not** lowered, so the next claim gets a higher one and the released claim can never be valid again. `200 {"outcome": "released"}` is the only response that confirms a release. A stale generation, another worker, or a job that is not running gets `409 ownership_lost` and is untouched; so does a repeat after a release whose response was lost (the job is no longer running and the store cannot tell the cases apart). `503` means unconfirmed: in testing, a request that timed out against a frozen database was committed when the database resumed. Other errors: `400`, `401`, `405`, `413`. **Workers must call release only after the handler has returned and renewals have stopped** (the server cannot check this; releasing while the handler still runs lets another worker execute the job at the same time). Because the attempt is refunded, a worker that releases every job it claims makes no progress and burns no attempts; v0 does not limit that. A worker that crashes or just disappears does not release: the lease expires, the reaper recovers the job, and the attempt stays consumed.

All five worker operations now exist, and the shared development key is not production authentication, so the endpoints remain suitable for protocol testing only.

**Go client package (Task 25.3.1).** `github.com/Venkatraman-web/forge/sdk/workerclient` is a typed HTTP client for these five operations and nothing more: `workerclient.New(Config{BaseURL, APIKey, HTTPClient})`, then `Claim` (returns `found=false` with no error when the queue is idle), `Renew`, `Complete`, `Fail` and `Release`. It is not a worker runtime: it runs no handlers, sends no heartbeats, and makes exactly one HTTP request per call, with no retries of any kind. It sends `Authorization: Bearer <key>` on every request, never follows redirects (so the key cannot be forwarded to another host), keeps fractional timings as `time.Duration`, bounds response sizes, and never puts the key in an error. Failures are typed: `errors.Is(err, workerclient.ErrOwnershipLost)` (409), `ErrUnauthorized` (401), `ErrRateLimited` (429, with `Retry-After` on `*workerclient.Error`), `ErrInvalidRequest` (other 4xx), `ErrUnavailable` (5xx), `ErrMalformedResponse`, and `*workerclient.TransportError` for connection errors, timeouts and context cancellation. `workerclient.IsUncertain(err)` is true when the operation may have been applied even though the call did not succeed (transport errors, 5xx, malformed responses); the server does not replay operations, so a repeat of a committed Complete, Fail or Release gets `ErrOwnershipLost`, which looks like a genuine loss. `Fail` takes the attempt from the `ClaimedJob` and never computes or sends a retry delay; the server still trusts that attempt number when sizing the backoff, which is a known server-side hardening item. The SDK does not make the server production-safe; use `https://` through a TLS proxy outside local development.

**Worker runtime (Tasks 25.3.2.3 to 25.3.2.5).** `github.com/Venkatraman-web/forge/sdk/workerruntime` runs your handlers: `New(Config)`, `Handle(jobType, handler, opts...)` (the option `WithTimeout` is described below), `Run(ctx)`. `Run` starts `Concurrency` (1 to 64) independent slots; each slot has its own stable worker ID and claims one job at a time for the registered types, so at most that many handlers run at once and nothing is prefetched. Each claim is run under the claim's deadline, kept alive with its own heartbeat and watchdog, and reported through the client (success: Complete; error: retryable Fail; `Permanent(err)`: non-retryable Fail; panic: Fail `panic`; deadline: Fail `timeout`; clean shutdown: Release with the attempt refunded). A claim that was lost or self-fenced is never reported, even if its handler later returns a result. It never sends or logs handler error text or panic values. A handler must stop when its context is cancelled: one that ignores cancellation keeps its job (the lease keeps being renewed, nothing is released or failed) and its slot until it returns or the process exits, and `Wedged()` counts such handlers across all slots. Claim errors are classified: a wrong key (401), any other 4xx, a response that is not Forge's claim contract, or a client-side argument error is fatal and shuts every slot down (the error stays typed through `errors.Is`); rate limiting (429, waiting at least the server's `Retry-After`), 5xx and connection failures make that slot back off (1 s doubling to 30 s with jitter, no retry limit, interrupted at once by shutdown) and claim again, while other slots carry on. A lost claim response may leave a running job that Forge recovers when its lease expires. The server does not enforce the execution deadline. When a handler returns, the runtime first checks the monotonic clock again: if no renewal succeeded within the safety margin (for example the process was paused and the watchdog has not yet run), the result is discarded and nothing is reported. `ShutdownGrace` bounds how long handlers get; `Run` may still take up to the bounded outcome-report time (`ReportAttempts` requests of up to 10 s each) after that to finish reporting a Complete or Release, and returns `ErrShutdownIncomplete` if a handler is still running. A release whose response is lost is logged as `unconfirmed` or `unknown_or_lost`, never as failed; the job is then either already `queued` (attempt refunded) or recovered by the reaper (attempt consumed). Crash (SIGKILL) recovery relies on the reaper: execution stays at-least-once. A `Worker` can be started once. Design: `docs/decisions/0003-worker-runtime-v0.md`.

### Job execution timeouts

Every job has an **execution timeout**: the longest **one attempt** may run before the handler's context is cancelled (it is not the total time across retries). The submitter chooses it per job with `options.timeout_seconds` on `POST /v1/jobs` (see "Job options"); the default is **30 seconds**, and the server caps it with `FORGE_MAX_JOB_TIMEOUT`. Forge stores the value on the job (`jobs.timeout_seconds`) and sends it to the worker as `deadline_seconds` in every claim, so retries and recovered jobs keep it. The runtime enforces it: when it passes, `ctx` is cancelled (cause `workerruntime: execution deadline reached`) and the attempt is reported as a retryable `timeout` failure, so the job is retried with backoff or, with no attempts left, fails with `attempts_exhausted`. The server does not enforce the deadline itself.

A worker can additionally **shorten** it for one job type with `workerruntime.WithTimeout`:

```go
err := w.Handle("report.render", renderReport, workerruntime.WithTimeout(10*time.Second))
// err is non-nil if the value is invalid; check it at startup
```

- **Effective limit.** The smaller of the claimed job's own timeout and `WithTimeout`. `WithTimeout` can never extend a job's timeout: a larger value is accepted, ignored, and logged once per job type as `with_timeout_exceeds_server_deadline_ignored`.
- **Valid `WithTimeout` values.** `workerruntime.MinJobTimeout` (1 s) and up; `WithTimeout(0)` and negative values are rejected by `Handle` with `workerruntime: WithTimeout for "<type>" must be at least 1s, got <value>`. Omitting the option leaves the job's own timeout in force.
- **Clock and outcome.** The clock starts when the claim response arrives. Expiry follows the existing retry and job-state policy. Other running jobs and slots are unaffected.
- **No forced stop.** Go cannot stop a goroutine: a handler that ignores `ctx` keeps running, keeps its slot and its lease (counted by `Wedged()`), and its late success is discarded rather than reported as success.
- **Not the same as** the HTTP client timeout, the claim long-poll wait (`PollWait`), database timeouts, the lease, heartbeat and safety-margin settings, or the shutdown grace period (`ShutdownGrace`). A long timeout does not need a long lease: the lease is renewed for as long as the worker is alive.
- **Still not available:** the server does not enforce the timeout itself (a handler that ignores `ctx` can outlive it while its worker keeps renewing the lease), there is no per-job-type timeout policy or default, and the hard ceiling of 3600 seconds is a conservative placeholder until the maximum job duration is decided (ADR 0004, D-3).
- **Validation record.** Scratch programs using only the public SDK against a real Forge confirmed: the 30 s default (claim deadline 30s, cancelled at about 30 s); a 3 s job cancelled at about 3 s on each of its 3 attempts with the timeout unchanged after retries and after crash recovery; jobs finishing within their timeout succeeding; a 12 s job unaffected while a 3 s job timed out; `WithTimeout` shortening but not extending; invalid values and values above the server maximum rejected with nothing stored; a raised `FORGE_MAX_JOB_TIMEOUT` accepting and enforcing a 40 s timeout and delivering a 90 s deadline; and the migration applied over existing rows. This is manual validation, not an automated regression suite.

### Configuring worker concurrency

How many jobs one worker process runs at once is the public `Concurrency` field of `workerruntime.Config`. There is no separate environment variable or configuration file for it in the SDK: your program decides where the number comes from.

```go
client, _ := workerclient.New(workerclient.Config{BaseURL: "http://127.0.0.1:8081", APIKey: key})
w, err := workerruntime.New(workerruntime.Config{
    Client:      client,
    Name:        "billing",
    Concurrency: 8, // at most 8 handlers at the same time in this process
})
if err != nil {
    log.Fatal(err) // a bad value is reported here, at startup
}
w.Handle("invoice.generate", generateInvoice)
err = w.Run(ctx)
```

- **Default and range.** Omit `Concurrency` (or set 0) for `workerruntime.DefaultConcurrency` (1). Valid values are 1 to `workerruntime.MaxConcurrency` (64). Anything else makes `New` return `workerruntime: Config.Concurrency must be 1 to 64, got N`; nothing is clamped silently.
- **How the limit is enforced.** `Run` starts exactly that many slots. A slot claims a job only while it is free and gives itself back only after the job is resolved (completed, failed, retried, released at shutdown), so a running job never frees its slot early. A handler that ignores cancellation keeps its slot until it returns (`Wedged()` counts such handlers).
- **Per process, not per deployment.** `Concurrency` limits one process. Total capacity is the number of worker processes you run times their `Concurrency`; the SDK does not start or count replicas, that is a deployment decision. Each idle slot holds one long poll, and the server caps simultaneous polls (`FORGE_WORKER_MAX_POLLS`, default 64), so size the fleet's total `Concurrency` against that number.
- **Not the queue limit.** `FORGE_MAX_QUEUED` limits how many jobs may wait; `Concurrency` limits how many run. They are independent. Handlers must be safe to run concurrently, since up to `Concurrency` of them share the process.
- **ForgeCart's workers** read their own variable, `FORGECART_WORKER_CONCURRENCY` (default 4, 1 to 8); that range is ForgeCart's choice, narrower than the SDK's.
- **Validation record.** A scratch program using only this public API checked (against a real Forge): the default of 1; limits of 4, 16 and 64 with 200 to 640 queued jobs, where the number of simultaneously running handlers equalled the limit and never exceeded it; rejection of -1, 65 and 1000; retryable errors, panics and permanent errors without leaking slots; clean shutdown releasing running jobs with the attempt refunded; and slots held by handlers that ignore cancellation. This is manual validation, not an automated regression suite.

**Retries (Task 24.2).** When a handler returns an error, the worker classifies it and calls `FailAttempt`; the database then decides, in one statement and from the persisted `attempt` and `max_attempts` (default 3), whether the job goes back to `queued` or becomes `failed`. Rules: an error wrapped with `handlers.Permanent(err)` fails the job at once; a handler timeout (`timeout`) and any other handler error (`execution_failed`) are retried while attempts remain; cancellation during shutdown releases the job (see below); database errors are never treated as handler failures (an unconfirmed outcome leaves the job `running` and the lease/reaper recover it); a write that matches no row (`ErrLostOwnership`) is discarded. A retry waits `delay = min(nominal * (1 + j), max)` with `nominal = min(FORGE_RETRY_BASE_DELAY * 2^(attempt-1), FORGE_RETRY_MAX_DELAY)` and jitter `j` uniform in [-0.2, +0.2], so with the defaults the nominal waits are 2s, 4s, 8s, ... capped at 5 minutes. The wait is added to the database's `now()`, so `next_run_at` does not depend on the application host's clock. `attempt` is never reset; the next claim increments it and `claim_generation`. When the last attempt fails the job ends `failed` with `attempts_exhausted` and a message that includes the last safe error. If a `FailAttempt` commits but its reply is lost, the repeated write reports lost ownership, so Forge cannot always tell "committed" from "stale"; it then writes nothing more.

**Failure logging (Task 24.3.2).** The text of a handler error is never logged, because it may hold input fragments or secrets (marking an error permanent does not make its text safe). Logs carry job ID, type, claim generation, worker ID, attempt/max attempts, an allowlisted kind (`error`, `permanent`, `timeout`, `cancelled`, `panic`), the safe error code, and whether the outcome was saved. Database errors are logged as a fixed description (for example `connection refused`, `timed out` or `database error (SQLSTATE 57P01)`), not as raw driver text.

**Graceful shutdown (Task 25.1).** On SIGINT/SIGTERM Forge stops the HTTP server, then cancels the workers: they stop claiming and running handlers are told to stop. When a cooperative handler returns `context.Canceled` because of this, the heartbeat has already been joined and the worker calls `Store.Release`, a fenced statement that returns the job to `queued` (claimable at once, worker and lease cleared) and refunds the interrupted attempt (`attempt - 1`); `claim_generation` is never reduced, so the next claim gets a higher generation. Other outcomes keep their normal handling: a handler error, timeout or panic follows the retry path, a handler that finished anyway keeps its result, and a claim that was already lost writes nothing. Shutdown waits at most 30 seconds (after the HTTP server's 10 s) for workers. A handler that ignores cancellation is not released while it runs (that could execute the job twice at once); if it is still running at the deadline Forge exits with an error, leaves the database open, and the reaper recovers the job after its lease expires. A crash (SIGKILL) is not a graceful release: the attempt stays consumed. If a release cannot be saved (for example the database is down) Forge says so, exits normally, and the reaper recovers the job once the lease expires, also without refund.

This gives **at-least-once** execution: a recovered job's handler can run again, so handlers must tolerate that. The fence protects Forge's rows, not external side effects, and Go cannot kill a handler that ignores its context (its lease keeps being renewed while it runs). All components still run in one Forge process.

## Database architecture

Phase 1 established this architecture:

- **PostgreSQL** stores jobs persistently.
- **GORM v2** provides routine ORM operations and record mapping.
- **Atlas with the GORM provider** generates versioned SQL migration files from the desired GORM model schema.
- **Atlas CLI** validates, applies, and tracks versioned schema migrations.
- **Explicit transactions and locking/conditional updates** remain important for concurrency-sensitive job state changes. GORM does not automatically prevent deadlocks or make a transaction correct.
- The existing `internal/store.Store` interface remains the boundary between API/worker code and the persistence implementation.

The intended schema workflow is:

1. Update the GORM model to describe the desired schema.
2. Generate a versioned migration using Atlas.
3. Review the generated SQL and commit the migration files.
4. Apply pending migrations deliberately with the Atlas CLI.
5. Start Forge against the migrated database.

We will **not** rely on GORM `AutoMigrate` at application startup as the versioned production migration workflow. Generated migration SQL remains reviewable and version-controlled. Atlas is a schema migration tool, not a PostgreSQL hosting service; the database can run locally in Docker during development.

### Database artifacts and the Store contract

Atlas is configured (see below) and `migrations/` holds the only migration history: the first Atlas-generated migration, which was applied to the local Compose database. The earlier hand-written SQL files have been removed.

The `store.Store` interface now takes a `context.Context` on every method and returns errors: `store.ErrNotFound` (HTTP 404) means a job does not exist, while any other error is a storage failure (HTTP 500 with a generic message). `PostgresStore` implements the contract with GORM against the `jobs` table. It takes an already-initialised `*gorm.DB`, never calls `AutoMigrate` (the schema belongs to the versioned migration files), and is the store Forge uses at runtime. There is no in-memory fallback: if PostgreSQL is unreachable at startup, Forge exits with an error.

## Phase 1: Persistence foundation

**Phase 1 is complete.** It was defined by these outcomes, all validated manually (see "Phase 1 validation record"):

- the store interface accepts contexts and distinguishes missing records from database errors;
- a GORM-backed implementation satisfies that interface;
- Atlas can generate, validate, and apply versioned migrations from the GORM schema;
- PostgreSQL can be started locally through a documented Docker Compose setup;
- Forge uses PostgreSQL explicitly, without silently falling back to memory;
- manual validation confirms that a completed job remains readable after Forge restarts.

Phase 1 did **not** implement durable queue delivery (a PostgreSQL job row alone did not make the then in-process queue durable), retries, leases/recovery, multi-tenancy, the dashboard, or performance claims. Phase 2's job is to make execution reliable across crashes and independent processes. Its first task is design-only and will compare mature Go job-queue frameworks with a broker-based design before a queue technology is treated as final.

## Build-versus-adopt policy

Forge should not rebuild commodity infrastructure without a clear reason. Before implementing a queue engine, scheduler, retry system, worker pool, observability backend, or other substantial subsystem, evaluate maintained solutions that may already provide the required reliability and operational behavior. Compare actual fit, maturity, failure semantics, performance, security, license, dependencies, operational burden, and limitations. Prefer adopting a proven component when it improves reliability or development speed without undermining Forge's product goals.

Build custom code for Forge-specific behavior that differentiates the platform or is not adequately covered by the selected component. Do not assume RabbitMQ plus a custom outbox/worker engine is automatically superior to a mature Go job framework. Phase 2 Task 17 must compare these approaches (including their storage and operational trade-offs), recommend one, and explain which components Forge should build versus reuse before implementation begins. The final decision must be grounded in current documentation and the actual repository, not feature-name comparisons alone. For any future hosted deployment, respect the user's $0 database-hosting budget: verify current free-tier limits and never enable a paid resource without explicit approval.

## Schema migrations with Atlas

**Roles.** GORM describes the *desired* table; Atlas *plans and applies* the changes. The struct `JobRecord` in [`internal/store/postgres.go`](../internal/store/postgres.go) is the single definition of the `jobs` table, including its CHECK constraints and index (written as `gorm:"..."` tags). Nothing calls `AutoMigrate`; the application never changes the schema by itself.

**How the model becomes a schema.** [`atlas.hcl`](../atlas.hcl) runs the GORM provider (`go tool atlas-provider-gorm load --path ./internal/store --dialect postgres`), which prints the desired schema as SQL. For planning, Atlas starts a disposable empty PostgreSQL 16 container (`docker://postgres/16/dev`), loads the existing migrations into it, compares the result with the desired schema, and writes the difference as a new file. This dev container is separate from the Compose database and is discarded afterwards. It needs working Docker access.

**Prerequisites.** Docker access, and the [Atlas CLI](https://atlasgo.io/getting-started#installation) on your `PATH` (`atlas version`). Use the non-community build: the community build rejects `data.external_schema`, which the GORM provider needs. Validation here used the tagged release v1.3.0 (`https://release.ariga.io/atlas/atlas-linux-amd64-v1.3.0`); the `-latest` download reported a canary build. The GORM provider is pinned in `go.mod` as a Go tool, which is why `go.mod` now says `go 1.24`.

**Change the schema:**

```bash
# 1. edit the JobRecord tags, then
atlas migrate diff <short_name> --env local   # writes migrations/<timestamp>_<short_name>.sql and updates atlas.sum
# 2. READ the generated SQL; commit the .sql file and atlas.sum together
atlas migrate validate --env local            # checks files and checksums
atlas migrate diff check --env local          # should say "synced with the desired state"; delete nothing if it creates a file, investigate instead
```

`migrations/atlas.sum` is a checksum of the migration files. If someone edits an applied file by hand, validation fails. Never edit a committed migration; add a new one. `atlas migrate hash --env local` rewrites the checksum file (only after a deliberate manual change).

**Current history:** `20261018134608_create_jobs.sql` creates `jobs` with the same columns, CHECK constraints (`jobs_id_not_empty`, `jobs_type_not_empty`, `jobs_status_valid`, `jobs_lifecycle_shape`) and index (`created_at DESC, id DESC`) as the old manual migration. PostgreSQL prints the constraint expressions in its canonical form, so the text differs slightly.

`20261018201314_execution_schema_foundation.sql` (M-A1) is an **additive** step toward reliable execution. It alters `jobs` (never recreates it) to add `attempt`, `max_attempts` (default 3), `claim_generation` (bigint, never negative), `next_run_at` (default `CURRENT_TIMESTAMP`), `lease_expires_at`, `worker_id` and `attempt_started_at`; adds the CHECK constraints `jobs_attempts_valid` and `jobs_generation_valid` and `jobs_error_pair`; relaxes `jobs_lifecycle_shape` so a `queued` job may keep `started_at` (the first start) and a previous error while it waits for a retry, and so a `running` job does not yet need a lease or worker; and adds two partial indexes: `jobs_due_idx (next_run_at, created_at, id) WHERE status = 'queued'` for picking due work in order, and `jobs_running_idx (created_at) WHERE status = 'running'` for finding running jobs to inspect for expired leases. `lease_expires_at` is deliberately not indexed, so lease renewals can stay cheap in-place updates. **Used since Task 22:** the claim loop uses `next_run_at`, `attempt`/`max_attempts`, `claim_generation`, `worker_id`, `attempt_started_at` and `lease_expires_at` (renewed by heartbeats since Task 23), `jobs_due_idx` and, in the reaper, `jobs_running_idx`. Still unused: retries of handler failures. The API response is unchanged. Existing rows received the defaults (`next_run_at` is the migration time, so legacy rows count as immediately due).

**Applying** (already done for the local development database; repeat on any new empty database). Keep the URL out of tracked files by using a shell variable built from your `.env` values:

```bash
export FORGE_DATABASE_URL='postgres://forge:YOUR_PASSWORD@127.0.0.1:5432/forge?sslmode=disable'
atlas migrate apply --env local --url "$FORGE_DATABASE_URL" --dry-run   # preview
atlas migrate apply --env local --url "$FORGE_DATABASE_URL"
atlas migrate status --env local --url "$FORGE_DATABASE_URL"
```

Atlas records which versions were applied in an `atlas_schema_revisions` table in the target database, which stops a migration running twice.

**Rolling back.** Prefer a new forward migration that undoes the change. `atlas migrate down` exists, but I have not tried it here, and it may require an Atlas account depending on the version. Reversing a schema change can permanently delete data (for example dropping the `jobs` table deletes every job). Do it only on a disposable database or after a backup.

## Local PostgreSQL with Docker Compose

[`compose.yaml`](../compose.yaml) runs a development PostgreSQL 16 server, which Forge uses at runtime (see "Running Forge with PostgreSQL"). Starting it does **not** apply migrations; run the Atlas apply step separately. This database is for local development only.

**Prerequisites:** Docker Engine with the Compose plugin (`docker compose version`), and permission to talk to the Docker daemon.

**Configure:** copy the example file, then **set your own database password** before the first start. `.env.example` ships only the placeholder `CHANGE_ME_before_first_start`; replace it with your own value in `.env` in **both** places it appears (`POSTGRES_PASSWORD` and inside `FORGE_DATABASE_URL`, URL-encoding special characters). The database user is created with that password the first time the container starts, so choose it before `docker compose up`. `.env` is listed in `.gitignore`; never commit it or put real credentials in `.env.example`.

```bash
cp .env.example .env
# now edit .env: replace CHANGE_ME_before_first_start in POSTGRES_PASSWORD and FORGE_DATABASE_URL
```

| Variable | Purpose |
|---|---|
| `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB` | Created on the **first** start of an empty volume. Changing them later does not change an existing database. |
| `FORGE_POSTGRES_PORT` | Host port (default `5432`). Change it if that port is taken. |

The port is published on `127.0.0.1` only, so other machines on your network cannot reach it.

**Validate, start and check readiness:**

```bash
docker compose config -q            # prints nothing if the file and .env are valid
docker compose up -d --wait         # starts PostgreSQL and waits until it is healthy
docker compose ps                   # STATUS should say "healthy"
docker compose logs -f postgres     # view logs (Ctrl-C stops following)
```

The health check runs `pg_isready` inside the container every 5 seconds.

**Connect with `psql` inside the container** (no local PostgreSQL install needed):

```bash
docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
```

**Local connection URL for Forge** (Forge reads `FORGE_DATABASE_URL` at startup):

```text
postgres://USER:PASSWORD@127.0.0.1:5432/DATABASE?sslmode=disable
```

Use the values from your `.env`, and URL-encode special characters in the password. `sslmode=disable` is acceptable only for this local, loopback-only database.

**Stop while keeping data:**

```bash
docker compose down                 # removes the container, keeps the named volume forge_pgdata
docker compose up -d --wait         # later: same data is still there
```

**Deliberate reset (DESTRUCTIVE).** This permanently deletes every table and row in the development database. Run it only when you intend to start from empty:

```bash
docker compose down -v
```

## Repository layout

```text
cmd/forge/                  Application entry point, environment parsing, listeners and wiring
internal/api/               Submission and status HTTP API (submit, get, list, health), validation, responses
internal/workerapi/         Remote worker HTTP API on its own listener (claim, renew, complete, fail, release)
internal/operatorapi/       Opt-in read-only operator API on its own listener (GET /v1/queues, GET /metrics)
internal/metrics/           Prometheus metrics: queue gauges read from the database, per-process counters
internal/jobs/              Job model, status, options and lifecycle fields
internal/store/             Store interface, sentinel errors, the GORM/PostgreSQL implementation, admission slots
internal/worker/            Submission service, lease reaper and retry policy
sdk/workerclient/           Public typed HTTP client for the worker API (no retries)
sdk/forgeclient/            Public client for the submission API: submit, get and list jobs, typed backpressure errors
sdk/workerruntime/          Public worker runtime: runs your handlers (concurrency, timeouts, graceful shutdown)
migrations/                 Atlas-generated versioned SQL migrations (including the hand-extended queue_counter migration) and atlas.sum
forgecart/         ForgeCart reference application, its migrations and workers
forgecart/test_files/   Scenario-based load-test / regression suite, the E-1 capacity harness, canonical report.md and audit documents
docs/decisions/             Architecture decision records 0001 to 0004
docs/                       documentation: architecture, reference, deployment, benchmarks, design records
compose.yaml, atlas.hcl     Local PostgreSQL and Atlas configuration
```

The layout may change as the implementation is refactored. Inspect the actual repository before editing rather than assuming this listing is exhaustive.

## Running Forge with PostgreSQL

Requirements: Go 1.24 or later, `curl`, Docker with Compose, and the Atlas CLI (see "Schema migrations with Atlas"). The race detector (`-race`) additionally requires a compatible C toolchain.

```bash
cp .env.example .env                    # once; then set your own password in .env (replace CHANGE_ME_before_first_start in both places)
docker compose up -d --wait             # start PostgreSQL
set -a; . ./.env; set +a                # load .env into this shell (provides FORGE_DATABASE_URL)
atlas migrate apply --env local --url "$FORGE_DATABASE_URL"   # once per new database
# Forge needs its worker settings (two different random keys of 32+ characters; see "Current configuration"):
export FORGE_REMOTE_JOB_TYPES=hello.greet FORGE_WORKER_ADDR=127.0.0.1:8081
export FORGE_API_KEY=$(openssl rand -hex 24) FORGE_WORKER_API_KEY=$(openssl rand -hex 24)
go run ./cmd/forge                      # or: go run -race ./cmd/forge
```

At startup Forge opens a GORM connection, checks it with a 5-second ping, and exits with a clear message if `FORGE_DATABASE_URL` is missing or PostgreSQL is unreachable (the URL and password are never printed). The pool is capped at 10 connections. On Ctrl-C or SIGTERM, Forge stops the HTTP server, then the workers, then closes the database connection. Stopping Forge or running `docker compose down` keeps the data; only `docker compose down -v` deletes it.

Before committing changes, also run `gofmt -l cmd internal`, `go build ./...` and `go vet ./...`.

By default, Forge listens on `127.0.0.1:8080`. **Persistence limits:** job records survive restarts, and a job that was `queued` when Forge stopped is claimed after the next start without being resubmitted. A job that was `running` when the process died is requeued by a reaper once its lease expires (default 15 s, plus up to one reaper interval), so a handler may run more than once; Forge does not provide exactly-once execution. Limitations: a `running` row that has no lease (written before Task 22) is never reaped; a handler's non-permanent error is retried, so handlers must tolerate repeated execution; a handler that ignores cancellation delays shutdown by up to 30 s and is then left to lease recovery; the database clock decides lease expiry, and a clock jump on the database host would shift it.

### Current configuration

| Variable | Default | Meaning |
|---|---|---|
| `FORGE_ADDR` | `127.0.0.1:8080` | HTTP listen address of the submission API. Local-only by default: the key it checks is a shared secret over plain HTTP. |
| `FORGE_API_KEY` | none (required) | The submit key: every submission-API request except `GET /health` must carry `Authorization: Bearer <key>` (`401 unauthorized` otherwise, before anything else runs). At least 32 characters and **different from `FORGE_WORKER_API_KEY` and `FORGE_OPERATOR_API_KEY`**. Never logged. Clients use it as `forgeclient.Config.APIKey`. |
| `FORGE_DATABASE_URL` | none (required) | PostgreSQL URL, for example `postgres://forge:PASSWORD@127.0.0.1:5432/forge?sslmode=disable`. Forge refuses to start without it. Keep it in the ignored `.env`. |
| `FORGE_EXECUTION_MODE` | `remote` | Optional and kept only for old configurations: unset or `remote` is accepted; any other value (including `embedded`, which was removed) is a startup error. |
| `FORGE_REMOTE_JOB_TYPES` | none | Required unless `FORGE_CONFIG_FILE` is used (and rejected together with it): comma-separated job types that may be submitted and claimed by remote workers. Each entry is trimmed and must match `^[a-z][a-z0-9_.-]{0,63}$`; empty entries are rejected, duplicates collapse, at most 32. A development stand-in for service registration: it does not express ownership or authorization. |
| `FORGE_WORKER_ADDR` | none | Required: listen address of the separate worker API server, for example `127.0.0.1:8081`. Must differ from `FORGE_ADDR`. |
| `FORGE_WORKER_API_KEY` | none | Required: the shared **development** key workers present as `Authorization: Bearer <key>`. At least 32 characters; generate it randomly (for example `openssl rand -base64 32`). Never logged or echoed. |
| `FORGE_WORKER_MAX_POLLS` | `64` | How many claim requests may wait at the same time; more get `429`. |
| `FORGE_KEYS_FILE` | none | Optional JSON file of **several named keys per role**, for rotating or revoking a key without downtime: `{"submit":[{"name":"shop","sha256":"<hex>"}], "worker":[...], "operator":[...]}`. It holds only the SHA-256 digest of each key (`printf %s "$KEY" \| sha256sum`), so the file is not a secret that works on its own. A role the file lists must not also be set by its `FORGE_*_API_KEY` variable (startup error); a role it leaves out still uses the variable. Names are `a-z 0-9 . _ -`, unique per role; the same key may not appear twice, in the file or across roles. The digest cannot show how strong the original key was: generate keys with `openssl rand -hex 24`. To rotate: add the new key, move the clients over, remove the old key, restart Forge. Key names (never keys) are logged at startup, and the worker API logs which key completed, failed or released a job. |
| `FORGE_OPERATOR_ADDR` | none | Optional. Listen address of the separate **operator API** (read-only queue status). Must be set together with `FORGE_OPERATOR_API_KEY` (setting only one is a startup error); neither set means the operator API is off. There is no default address, so it is never exposed by accident. Must be `host:port` and differ from `FORGE_ADDR` and `FORGE_WORKER_ADDR`. Use a loopback or management-network address. Works in both execution modes. |
| `FORGE_OPERATOR_API_KEY` | none | Required with `FORGE_OPERATOR_ADDR`: the bearer key operators present. At least 32 characters and **different from `FORGE_WORKER_API_KEY`** (operators and workers must not share a credential). Never logged. |
| `FORGE_MAX_QUEUED` | `1000` | Global limit on jobs waiting to run. A submission that would exceed it gets `503` with the stable error code `queue_full` and a `Retry-After: 5` header (whole seconds; a fixed conservative hint, because Forge cannot predict when workers will free capacity), and stores nothing. Retried, released and lease-recovered jobs return to the queue without this check, so the queued count can briefly exceed the limit. The limit is exact under concurrent submissions (a short database lock serialises the check-and-insert), at the cost of serialising submissions for a few milliseconds each. The check reads a trigger-maintained `queue_counter` row instead of counting queued rows (the count's cost had grown with queue depth); see the E-1 follow-up results for the measured effect. |
| `FORGE_MAX_JOB_TIMEOUT` | `30s` | Largest `options.timeout_seconds` a submitter may ask for. Whole seconds, from 30s (the default timeout; it cannot be set lower) to 1h. A submission above it gets `400 invalid_option` and nothing is stored; clients cannot raise it. Startup fails with a clear message for any other value. |
| `FORGE_POLL_INTERVAL` | `1s` | How often a waiting claim request re-checks the database for work (a Go duration such as `500ms`). |
| `FORGE_LEASE_DURATION` | `15s` | How long a claim stays valid after its last renewal; after this a reaper may recover the job. |
| `FORGE_HEARTBEAT_INTERVAL` | `5s` | How often a running job's lease is renewed. |
| `FORGE_LEASE_SAFETY_MARGIN` | `10s` | A worker cancels its handler if no renewal succeeded for this long. Startup requires heartbeat < safety margin < lease duration. |
| `FORGE_REAPER_INTERVAL` | `5s` | How often expired leases are looked for. |
| `FORGE_REAPER_BATCH_SIZE` | `100` | Most jobs recovered in one reaper pass. |
| `FORGE_RETENTION_SUCCEEDED` | unset | How long a succeeded job is kept after it finished, for example `168h`. Unset keeps succeeded jobs forever. At least `1m`. |
| `FORGE_RETENTION_FAILED` | unset | The same for failed jobs (often kept longer). Retention runs only if at least one of the two is set. |
| `FORGE_RETENTION_INTERVAL` | `10s` | How often finished jobs past their retention are deleted (batches of 1,000, up to 100,000 per pass). |
| `FORGE_RECOVERY_BACKOFF` | `2s` | Delay before a recovered job can be claimed again. |
| `FORGE_RETRY_BASE_DELAY` | `2s` | Nominal wait before the first retry of a failed attempt; doubles per attempt. |
| `FORGE_RETRY_MAX_DELAY` | `5m` | Upper bound for any retry wait (at most 24h). Must be at least the base delay. |

Current fixed values (constants in the code, not configurable): the default job execution timeout of 30 seconds (each job's own `timeout_seconds` replaces it; the SDK enforces it, the server does not), HTTP shutdown grace period 10 seconds, request body limit 1 MiB, result limit 256 KiB, database pool of 10 connections, a 5-second timeout on each store call, and bounded admission (3 concurrent submissions per process, 500 ms maximum wait).

## Current API

All request and response bodies are JSON. Errors use this shape:

```json
{"error":{"code":"invalid_input","message":"A concise, safe explanation."}}
```

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/health` | `200 {"status":"ok"}`. Confirms only that the HTTP server responds. |
| `POST` | `/v1/jobs` | Validates and queues a job; responds `202 Accepted` when accepted. |
| `POST` | `/v1/jobs/batch` | Validates and queues up to 100 jobs in one transaction (all or nothing); `202` with the jobs in request order. |
| `GET` | `/v1/jobs` | Lists recent jobs, newest first. `limit` is optional; valid range is 1–100, default 20. |
| `GET` | `/v1/jobs/{id}` | Returns a job's current state, plus result or safe error information when available. |

The original job input is not included in public job responses.

### Job options: `max_attempts` and `timeout_seconds`

`POST /v1/jobs` accepts an optional `options` object with two settings, both optional:

```json
{"type": "invoice.generate", "input": {"order": "o-1"}, "options": {"max_attempts": 5, "timeout_seconds": 120}}
```

- **Meaning.** `max_attempts` is the **total number of execution attempts, including the first**. `1` means the job runs once and is never retried; `5` allows the first run plus up to four retries. It is the retry *budget*, not the delay between attempts.
- **Default and range.** Omit `options` (or `max_attempts`, or send `null`) for the default of **3**, the same as before this option existed. Valid values are whole numbers from **1 to 20** (the limits of the `jobs.max_attempts` column). Anything else (`0`, negative, above 20, `2.5`, `1e1`, a string such as `"3"`, a boolean, an array or object) is rejected with `400 invalid_option` before anything is stored. An unknown key inside `options` is rejected as `400 invalid_request`.
- **Visible in responses.** `POST /v1/jobs`, `GET /v1/jobs` and `GET /v1/jobs/{id}` now include `"max_attempts"` (additive). Workers see it as `Job.MaxAttempts` next to `Job.Attempt` in `sdk/workerruntime`.
- **Behaviour at the limit.** A retryable failure (an error returned by the handler, a panic, an execution timeout) requeues the job with the existing backoff while `attempt < max_attempts`; on the last attempt the job ends `failed` with error code `attempts_exhausted`. A `workerruntime.Permanent(err)` failure is never retried, whatever the limit. A job whose worker died is recovered by the reaper under the same limit (with `max_attempts: 1` it ends `failed`/`attempts_exhausted` instead of running again). A clean shutdown release refunds the attempt.
- **`timeout_seconds`.** The execution timeout of **each attempt**, in whole seconds (see "Job execution timeouts"). Omit it (or send `null`) for the default of **30**. It must be a plain whole number of at least 1 and at most **this server's maximum** (`FORGE_MAX_JOB_TIMEOUT`, default 30, hard ceiling 3600): `0`, negatives, decimals, exponent forms, strings, booleans, arrays, objects and values above the maximum are rejected with `400 invalid_option` before anything is stored, and the message names the server's limit. Because the default maximum equals the default timeout, a stock server accepts nothing longer than before; the operator must raise `FORGE_MAX_JOB_TIMEOUT` explicitly. The response and job reads include `"timeout_seconds"` (additive); retries, release and lease recovery keep the stored value.
- **Not configurable per job.** The delay between attempts stays server policy (`FORGE_RETRY_BASE_DELAY`, `FORGE_RETRY_MAX_DELAY`, jitter). The Go client for these calls is `sdk/forgeclient` (next section). Execution is at-least-once, so handlers must stay idempotent on `Job.ID` however many attempts a job is given.

### Safe retries: the `Idempotency-Key` header

If a `POST /v1/jobs` times out or the connection drops, you cannot tell whether Forge stored the job, and sending it again could create a second one. An idempotency key solves that. Send a stable identifier of the business event (an order ID, for example) in the `Idempotency-Key` header:

```bash
curl -X POST http://127.0.0.1:8080/v1/jobs -H "Authorization: Bearer $FORGE_API_KEY" \
  -H 'Idempotency-Key: order-1234-invoice' -H 'Content-Type: application/json' \
  -d '{"type": "invoice.generate", "input": {"order": "o-1234"}}'
```

- **First request with a key:** stored as usual, `202 Accepted`.
- **The same key with the same content again:** nothing new is stored. Forge returns the job from the first request with `200 OK` and the header `Idempotent-Replayed: true`. This is answered even when the queue is full, because no new job is created. The job is returned in its current state, so a repeat after completion shows `succeeded` and the result.
- **The same key with different content:** `409 idempotency_key_conflict`, nothing stored. "Content" is the type, the input (compared as JSON, so key order and whitespace do not matter), `max_attempts` and `timeout_seconds` as they resolve after the type's defaults are applied.
- **Format:** 8 to 128 characters from `A-Z a-z 0-9 _ . : -`, sent once. A missing-format, repeated or empty header is a `400 invalid_idempotency_key`, never ignored.
- **Not supported on `POST /v1/jobs/batch` yet.** A key on a batch request is refused with `400`, so a client is never silently unprotected.
- **Scope and lifetime.** Keys are global to this Forge (there are no tenants), and are kept as long as the job row exists; there is no retention job yet, so they do not expire. The key is not echoed in responses or logs. Concurrent submissions of one key are safe: submissions already run one at a time under the admission lock, and a unique index is the backstop.
- **Go client.** `forgeclient.SubmitRequest.IdempotencyKey`; `Job.Replayed` is true for a repeat; `ErrIdempotencyConflict` for the 409. With a key set, repeating a `Submit` after an uncertain error is safe.
- **Validation record.** Checked by running a real Forge and PostgreSQL on AWS: new key, repeat, four kinds of conflict, key-order/whitespace equivalence, defaults equivalence, invalid and duplicated headers, a key on a batch, 60 parallel requests with one key (59 replays, one job row), 40 parallel distinct keys, a repeat while the queue was full and after the job had finished, and the Go client including a simulated lost response. Manual validation, not an automated suite.

### Submitting many jobs at once: `POST /v1/jobs/batch`

```bash
curl -i -X POST http://127.0.0.1:8080/v1/jobs/batch \
  -H "Authorization: Bearer $FORGE_API_KEY" -H 'Content-Type: application/json' \
  -d '{"jobs": [
        {"type": "hello.greet", "input": {"Name": "A"}},
        {"type": "hello.greet", "input": {"Name": "B"}, "options": {"max_attempts": 5}}
      ]}'
```

- **Why it exists.** Every submission holds Forge's global admission lock until its database commit reaches the disk, so the number of single submissions per second is bounded by the disk's commit latency (measured: about 335 submissions/s on t3.small hosts with network-disk databases; `docs/benchmarks-aws.md`). A batch stores all its jobs in **one transaction with one commit**, so that cost is paid once per batch.
- **All or nothing.** Either every job is stored (`202` with `{"jobs": [...]}` in request order, each in the usual job shape) or none is. An invalid item (missing field, unsupported type, bad option, an option above the type's maximum) rejects the whole batch with a `400` whose message names the item, for example `jobs[3]: ...`. A batch that would push the queue past `FORGE_MAX_QUEUED` stores nothing and gets `503 queue_full` (the limit stays exact: it is checked against queued jobs plus the batch size). Admission busy gives `503 admission_busy` as for single jobs.
- **Limits.** 1 to 100 jobs per request; the whole body is limited to 1 MiB, as for a single job; each job follows exactly the single-job rules and per-type policy. There is no idempotency key for batches yet: an uncertain outcome may have stored the whole batch.
- **Go client:** `forgeclient.SubmitBatch`.
- **Validated** against a real Forge and PostgreSQL (34 checks): order and per-item options, atomicity for each kind of invalid item, shape and size limits, authentication, the exact limit (60 concurrent batches against a limit of 100 stored exactly 100), per-item metrics, and the Go client. In a local micro-benchmark with 4 ms of simulated database round trip, batches of 10 and of 100 raised stored jobs per second from about 470 to about 4,500 and about 27,000.

### Submitting from Go: `sdk/forgeclient`

`github.com/Venkatraman-web/forge/sdk/forgeclient` is the Go client for the three job calls above, so an application does not hand-write HTTP:

```go
c, err := forgeclient.New(forgeclient.Config{BaseURL: "http://127.0.0.1:8080", APIKey: os.Getenv("FORGE_API_KEY")})
job, err := c.Submit(ctx, forgeclient.SubmitRequest{
    Type:           "invoice.generate",
    Input:          map[string]string{"order": "o-1"}, // any JSON-encodable value
    MaxAttempts:    5,  // optional; total attempts including the first (default 3, 1 to 20)
    TimeoutSeconds: 60, // optional; limit per attempt (default 30, 1 to the server's maximum)
})
switch {
case forgeclient.IsBackpressure(err): // ErrQueueFull or ErrAdmissionBusy: nothing was stored
    wait, _ := forgeclient.RetryAfter(err) // wait at least this long, plus jitter, then resubmit
case errors.Is(err, forgeclient.ErrInvalidOption): // for example a timeout above the server maximum
case forgeclient.IsUncertain(err): // timeout, 5xx or a broken response: the job MAY have been stored
}
j, err := c.Get(ctx, job.ID)  // status, result or safe error, timestamps (never the input)
list, err := c.List(ctx, 20)  // newest first, limit 1 to 100
```

- **Calls and errors.** `Submit`, `Get` and `List` make exactly one HTTP request each and never retry, because a blind retry of a submission can create a second job (unless the request carries an `IdempotencyKey`) and because backpressure carries a wait time the caller should honour. Errors work with `errors.Is`: `ErrQueueFull`, `ErrAdmissionBusy` (both mean nothing was stored), `ErrInvalidOption`, `ErrIdempotencyConflict`, `ErrNotFound`, `ErrRateLimited`, `ErrInvalidRequest` (other 4xx), `ErrUnavailable` (other 5xx) and `ErrMalformedResponse` (redirect, unexpected status or body, oversized body). `errors.As` gives `*forgeclient.Error` (HTTP status, Forge error code, short message, `RetryAfter`) or `*forgeclient.TransportError` (connection failure, timeout, cancelled context; `errors.Is(err, context.DeadlineExceeded)` works).
- **Uncertain results.** `IsUncertain(err)` is true for a transport failure, an unexpected 5xx or a response that broke the contract: the job may exist. Repeating a `Submit` that carries an `IdempotencyKey` is safe. For anything else (a batch, or a `Submit` without a key), make the work idempotent on a stable identifier in the input (ForgeCart's dispatcher puts an event ID in it) before repeating a submission.
- **Safety.** Redirects are never followed, response bodies are size-limited, server text is stripped of control characters and shortened, and impossible requests (empty type, `MaxAttempts` above 20, `TimeoutSeconds` above 3600) fail before anything is sent. The server may still refuse a value that is valid for the client, for example a timeout above its `FORGE_MAX_JOB_TIMEOUT`.
- **Authentication.** `Config.APIKey` (Forge's `FORGE_API_KEY`) is required and is sent as a bearer token with every call; a missing or wrong key gives `401`, which matches `errors.Is(err, forgeclient.ErrUnauthorized)`. It is one shared static key sent in clear text (Forge has no TLS): keep Forge on a trusted network, or put a TLS proxy in front.
- **Used by ForgeCart.** ForgeCart's dispatcher publishes its outbox events through this package; it no longer carries its own HTTP client.
- **Validation record.** Scratch programs that use only this package confirmed (against a scripted fake server, 34 checks, and a real Forge, 14 checks): request shape; every error class and its `IsUncertain` meaning; `Retry-After` parsing (seconds, HTTP date, invalid and oversized values); no redirect following; oversized and malformed bodies; context deadline; a full queue; a frozen database producing `ErrAdmissionBusy` and uncertain `ErrUnavailable`; server-side option limits; and a real submit, get and list round trip. This is manual validation, not an automated regression suite.

### Example: submit a job with curl

With Forge running (the Docker quickstart above, or `go run ./cmd/forge` with the settings shown there) and `hello.greet` among the job types:

```bash
curl -i -X POST http://127.0.0.1:8080/v1/jobs \
  -H "Authorization: Bearer $FORGE_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"type": "hello.greet", "input": {"Name": "Forge"}, "options": {"max_attempts": 3, "timeout_seconds": 30}}'
```

An accepted request returns `202 Accepted` and a generated `job_id`. Acceptance means the job was queued, not that execution has finished. A job runs only when a worker for its type is connected (the ForgeCart workers are an example). Poll a job using the returned ID:

```bash
curl -s -H "Authorization: Bearer $FORGE_API_KEY" http://127.0.0.1:8080/v1/jobs/<JOB_ID>
```

The response shows the status (`queued`, `running`, `succeeded`, `failed`), the result or a safe error, and the attempt settings. It never echoes the submitted input.

### Server-side job type policy: `FORGE_CONFIG_FILE`

In remote mode, a JSON file can declare the job types and give each one its own retry budget and timeout, so developers set them once on the server instead of in every submission.

```json
{
  "version": 1,
  "job_types": {
    "invoice.generate": { "default_max_attempts": 5, "max_max_attempts": 8, "default_timeout": "15s", "max_timeout": "30s" },
    "email.send":       { "default_max_attempts": 3 },
    "report.render":    {}
  }
}
```

- **Fields (all optional).** `default_max_attempts` / `max_max_attempts` (1 to 20) and `default_timeout` / `max_timeout` (whole seconds such as `"15s"`, at most `FORGE_MAX_JOB_TIMEOUT`). A type that sets none gets the server-wide defaults (3 attempts, 30 s) and limits (20 attempts, `FORGE_MAX_JOB_TIMEOUT`).
- **Precedence.** A submitter's own `options.max_attempts` / `options.timeout_seconds` win; otherwise the type's default; otherwise the server default. A type's maximum can only lower the server-wide limit; asking for more gets `400 invalid_option`.
- **Which types exist.** With the file, the file's `job_types` are the allowed types, so `FORGE_REMOTE_JOB_TYPES` must not also be set (startup fails). Without `FORGE_CONFIG_FILE` nothing changes: types come from `FORGE_REMOTE_JOB_TYPES` and the defaults are the server-wide ones.
- **Strict and read once.** Unknown keys, a wrong `version`, a default above its maximum, a timeout above `FORGE_MAX_JOB_TIMEOUT`, a bad type name, trailing data or a missing file stop Forge at startup with a message naming the problem. Changes take effect on restart; there is no reload.
- **See what is enforced.** `GET /v1/config` on the operator listener (same key as `/v1/queues`) returns the effective values per type with every default and limit resolved.
- **Not in the file yet:** per-type pending limits, input size limits, retention and per-type retry backoff (ADR 0004, section 4.3). The retry delay is still server-wide.
- **Example.** `forgecart/forge.json` is ForgeCart's policy; its test harness starts Forge with it, and its dispatcher sends no options. In Docker, mount a file and set `FORGE_CONFIG_FILE` to its path inside the container.

### Operator API: read-only queue status (`GET /v1/queues`)

A separate listener for operators, off unless `FORGE_OPERATOR_ADDR` **and** `FORGE_OPERATOR_API_KEY` are both set. It serves exactly one endpoint, `GET /v1/queues`, and never changes a job.

**Security model (what it is and is not).**
- It has its own listener and its own bearer key, deliberately separate from the submission API's key and from the worker API (whose key belongs to customer-run workers). Every path requires `Authorization: Bearer <FORGE_OPERATOR_API_KEY>`; without it the answer is `401` for any path, so an unauthenticated caller cannot even tell which routes exist, and nothing touches the database. The key is compared in constant time and never logged; rejected requests are summarised in the log at most every 30 seconds.
- The operator key does not work on the other two listeners and the worker key does not work here; the main API and the worker API do not serve `/v1/queues`.
- It is a **static key** (or several named keys from `FORGE_KEYS_FILE`, so it can be rotated): no user identities, expiry, audit trail, roles or per-tenant scoping. It separates the operator role from the worker role; it is not the authentication Forge still lacks (see Security and scope notes).
- It speaks **plain HTTP** (Forge has no TLS). Bind it to loopback or a trusted management network, and for any remote access use a TLS-terminating reverse proxy, a service mesh, or an SSH tunnel. Over an unprotected network the key travels in clear text. Forge logs a `WARNING` at startup if the address is not loopback.
- At most 4 status queries run at once; extra requests get `429` with `Retry-After: 1`, so a runaway poller cannot load the database that submissions and workers share.

**Response** (`200`, `Content-Type: application/json`, `Cache-Control: no-store`; field names are the stable contract):

```json
{
  "collected_at": "2026-10-22T10:00:00.123456Z",
  "limits": { "max_queued": 1000, "scope": "global" },
  "rejection_counters": { "scope": "process", "since": "2026-10-22T09:00:00Z",
                          "reset_on_restart": true, "aggregated_across_processes": false },
  "queues": [
    { "type": "email.send", "queued": 12, "running": 3,
      "oldest_due_job_age_seconds": 1.842, "max_pending": null,
      "queue_full_rejections_since_process_start": 4 }
  ],
  "totals": { "queued": 12, "running": 3, "oldest_due_job_age_seconds": 1.842,
              "queue_full_rejections_since_process_start": 4 }
}
```

| Field | Exact meaning |
|---|---|
| `collected_at` | The **database** clock when the counts were read (all counts come from one consistent snapshot). |
| `limits.max_queued` | The effective `FORGE_MAX_QUEUED`. It is a **global** limit shared by all job types. |
| `queues[].type` | One entry per configured job type (all of them, with zeros when idle) plus any type that has queued or running jobs in the database but is no longer configured. Sorted by `type`. |
| `queued` | Jobs with `status='queued'`: exactly what `max_queued` counts, so `totals.queued == limits.max_queued` means the queue is full. It **includes** jobs waiting out a retry backoff or scheduled for later, and jobs released or recovered after an earlier attempt. |
| `running` | Jobs with `status='running'` (claimed and not yet finished or reaped). |
| `oldest_due_job_age_seconds` | How long the oldest queued job that is **already due** has been due: database `now()` minus its `next_run_at`, in seconds (3 decimals). `null` when no queued job of that type is due, including when nothing is queued. Jobs still waiting out a retry backoff are not due and are not measured. It is not the time since creation. `totals` holds the largest per-type value. |
| `max_pending` | Per-type admission limit. Always `null`: no per-type limit exists yet. |
| `queue_full_rejections_since_process_start` | Submissions of this type refused with `503 queue_full` **by this Forge process** since it started. In memory only: **reset to 0 when Forge restarts, and not aggregated across Forge processes**; with several Forge processes, each reports only its own. It is a diagnostic for this process, not a deployment-wide total. Only submissions that passed validation are counted (not `400`s). |

A storage failure returns `503` `{"error":{"code":"unavailable",...}}` and never an empty or partial queue. Counts are never invented: a type with no jobs shows 0 because the snapshot is authoritative, and anything unknown is `null`.

**How it reads the database (and what that costs).** One SQL statement, so counts come from one snapshot: PostgreSQL's default `READ COMMITTED` takes a new snapshot per statement, and two separate `SELECT`s can count a job that moved between them twice. The statement uses the existing partial indexes (`jobs_due_idx` for queued, `jobs_running_idx` for running) and never reads finished jobs. Cost grows with the number of queued plus running jobs: in one scratch measurement on a single development host (PostgreSQL 16 in Docker, warm cache, 1,000,000 finished jobs) the median was about 0.5 ms with an empty queue, 3 ms at 10,000 queued jobs and 9.5 ms at 50,000, with the plan using only the two partial indexes. That is a development measurement, not a capacity guarantee. It takes no row locks and does not affect submission, claiming or retries.

### Metrics (`GET /metrics`)

The operator listener (see above; same `FORGE_OPERATOR_ADDR` and key, nothing is exposed without them) also serves `GET /metrics` in the Prometheus text format, so a developer can see whether jobs are healthy, keeping up, and where the bottleneck is. It is read-only and shares the 4-request in-flight cap of `/v1/queues`.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: forge
    authorization: { type: Bearer, credentials: "<FORGE_OPERATOR_API_KEY>" }
    static_configs: [{ targets: ["127.0.0.1:8082"] }]   # FORGE_OPERATOR_ADDR
```

| Metric | Labels | Source | Meaning |
|---|---|---|---|
| `forge_queue_jobs` | `type`, `state` (`queued`, `running`) | database, every scrape | Jobs waiting and running per job type (a job type is one service's work). |
| `forge_queue_oldest_due_age_seconds` | `type` | database | How long the oldest due queued job has been due; `0` when none is due. The best "are workers keeping up" number. |
| `forge_queue_limit` | | config | `FORGE_MAX_QUEUED`. |
| `forge_queue_stats_up` | | scrape | `1` if the gauges above were read from the database, `0` if that failed (the per-type series are then absent, never shown as zero). |
| `forge_submissions_total` | `type`, `result` (`accepted`, `replayed`, `queue_full`, `admission_busy`, `invalid`, `error`) | this process | Submissions by outcome. `replayed` is a repeat of an idempotency key (nothing new stored); `invalid` is an unsupported type, invalid input, an invalid option or an idempotency-key conflict; a request that is not even valid JSON never reaches the service and is not counted. |
| `forge_job_events_total` | `type`, `event` (`claimed`, `succeeded`, `retried`, `failed`, `released`, `ownership_lost`, `attempt_timeout`, `attempt_panic`) | this process | `retried` = attempt failed and the job is queued again; `failed` = the job is now failed. An attempt that ends by timeout counts once as `retried` or `failed` and once as `attempt_timeout` (same for `attempt_panic`). `ownership_lost` seen by the remote worker API is counted under type `other`, because that request names only a job ID. |
| `forge_reaper_recovered_total` | `outcome` (`requeued`, `exhausted`) | this process | Jobs recovered after their worker stopped renewing the lease. |
| `forge_job_queue_wait_seconds` (histogram) | `type` | this process | Time from a job becoming due (submission, or the moment a retry or release made it claimable) to its claim, measured by the database clock. Retries therefore count the wait after their backoff, not the backoff itself. |
| `forge_job_attempt_duration_seconds` (histogram) | `type`, `outcome` (`succeeded`, `retried`, `failed`) | this process | Time from an attempt's claim to Forge recording its outcome, by the database clock. For a remote worker this includes the time to report, so it is slightly longer than the handler's own run time. Compare it with the job timeout. |
| `forge_admission_slots_in_use` / `_total` | | this process | The bounded admission step (3 slots). Persistently at 3 means submissions are being refused with `admission_busy`. |
| `forge_db_pool_connections` (`state` = `in_use`, `idle`), `forge_db_pool_max_connections`, `forge_db_pool_wait_total`, `forge_db_pool_wait_seconds_total` | | this process | Forge's 10-connection database pool. Rising wait means requests and workers are queueing for a connection. |
| `go_*`, `process_*` | | this process | Standard Go runtime and process metrics. |

- **What is authoritative.** The gauges that come from the database are shared by every Forge process and are exact at the scrape. The counters belong to **one Forge process**: they start at zero when it restarts and say nothing about other instances. That is normal for Prometheus: `rate()` and `increase()` tolerate resets, and several instances are summed with `sum by (type)`.
- **Bounded labels.** The `type` label only takes the configured job types plus the single value `other` (anything else a client submits). A caller cannot create new series.
- **Cost.** One read-only query per scrape (the same one `/v1/queues` runs); measured at about 13 ms (p50) with 20,000 queued jobs; the histograms add under 2 ms. A scrape interval of 5 to 15 seconds is plenty.
- **Not available yet:** worker counts, a per-type throughput total that survives Forge restarts, and end-to-end latency (submission to completion) as a single series; add the queue-wait and attempt-duration quantiles for an approximation.
- **Dashboard (optional).** `docker compose --profile observability up -d prometheus grafana` starts a Prometheus that scrapes Forge and a Grafana with a provisioned "Forge overview" dashboard (queue age, depth, queue-wait and attempt-duration p50/p95, event rates, timeouts, submissions by result, admission, database pool) at http://127.0.0.1:3000 (login `admin` with `FORGE_GRAFANA_ADMIN_PASSWORD` from `.env`, at least 12 characters: Grafana refuses to start without it; anonymous access and sign-up are off). It needs `FORGE_OPERATOR_API_KEY` in `.env` (the key Forge uses) and Forge's operator listener reachable from a container: on Docker Desktop and similar, set `FORGE_OPERATOR_ADDR=0.0.0.0:8082` (plain HTTP and one static key: trusted machines only; a different port needs `FORGE_OPERATOR_TARGET=host.docker.internal:<port>`). Prometheus runs `v2.55.1`, Grafana `11.3.0`. Prometheus history (`forge_prometheus_data`, retention `FORGE_PROMETHEUS_RETENTION`, default 15 days) and Grafana's own data (`forge_grafana_data`) are in named volumes, so they survive `docker compose down` and restarts; only `down -v` deletes them. `docker compose up` without the profile starts only PostgreSQL, as before.

**Running it in a deployed environment (not only on a laptop).** The same profile works on a dedicated monitoring machine. **Status: validated on 2026-10-23 on AWS (a separate `t3.small` monitoring machine; Forge, the database, the workers and the load generator each running node_exporter 1.7.0 from Ubuntu's `prometheus-node-exporter` package).** Checked: all six scrape targets reported up; Grafana answered 401 to anonymous, wrong-password and default `admin/admin` requests and listed the provisioned dashboard with the real password; it exited with a clear message when the password was missing or shorter than 12 characters; after `docker compose down` and `up` (no `-v`) a dashboard created through the API and Prometheus samples from before the restart were both still there; ports 3000 and 9090 were refused from another machine in the same security group and reachable through an SSH tunnel; and during a 2-minute load test every one of the dashboard's 14 chart panels (including the four Machines panels) returned data through Grafana. Not checked: Session Manager access, a PostgreSQL exporter, behaviour at the end of the retention period, and rendering in a browser (the panel queries were run through Grafana's query API, not viewed). The pattern:

- **Where:** a separate machine from the ones doing the work, so a busy Forge does not hide its own symptoms. Set `FORGE_OPERATOR_TARGET=<forge host>:8082` and make Forge's operator listener reachable from the monitoring machine only. The compose file also describes the PostgreSQL service, whose variables are required even when it is not started, so a monitoring-only `.env` needs `POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_DB` set to any placeholder value.
- **Machine metrics (CPU, memory, disk).** Install `node_exporter` (the standard Prometheus tool; port 9100) on each machine, and list `host:9100` in `observability/targets/node.yml` (Prometheus re-reads the file about every minute). The dashboard's "Machines" row then shows CPU busy, iowait and steal, memory available and disk busy per machine. `node_exporter` is not part of Forge and is not installed by it.
- **Network rules:** Prometheus (9090) and Grafana (3000) are published on 127.0.0.1 of the monitoring machine and must stay that way; Prometheus has no login. Allow `node_exporter` (9100) and Forge's operator port (8082) only from the monitoring machine's security group.
- **Reaching Grafana:** from your own computer through an SSH tunnel (`ssh -L 3000:127.0.0.1:3000 <monitoring machine>`, then http://127.0.0.1:3000), or through AWS Systems Manager Session Manager port forwarding, which needs no open inbound port at all. Do not put Grafana on a public address without HTTPS and a real login.
- **ForgeCart's shop API.** If you also run ForgeCart, its optional metrics listener (see `forgecart/README.md`) adds a "Shop (foreground)" row: request rate, checkout and payment latency, payment outcomes and the outbox backlog. List its address in `observability/targets/shop.yml` and set `FORGECART_METRICS_KEY` in the monitoring `.env`.
- **Not covered:** database internals (wait events, slow statements) need a PostgreSQL exporter, which is not included; the key and password travel over the tunnel, but Forge's own operator traffic is plain HTTP inside the network.
- **Reading it (a first tuning guide).**

| You see | It usually means | Try |
|---|---|---|
| `forge_queue_oldest_due_age_seconds` rising, `running` below your total worker concurrency | workers are not claiming fast enough or are idle between polls | check workers are up; shorten the poll wait |
| age and `queued` rising, `running` equal to your total concurrency | workers are saturated | raise `Concurrency` or run more worker instances for that type |
| `attempt_timeout` rising | handlers exceed their timeout | raise `timeout_seconds` for that job, or speed up or fix the handler |
| `retried` and `failed` rising | handlers erroring | look at the handler; consider `Permanent` errors for non-retryable cases |
| `queue_full` rising while workers are idle | the global limit is too low for your bursts | raise `FORGE_MAX_QUEUED` |
| `admission_busy` rising, admission slots at 3 | you have reached Forge's submission rate ceiling | submit less often or in a smoother pattern; this ceiling is per Forge process |
| `forge_db_pool_wait_seconds_total` rising | the 10-connection pool is the bottleneck | reduce concurrent submissions and worker polling; the pool size is fixed |

- **Validation record.** A scratch driver against a real Forge confirmed (27 checks for v1a, 26 of which still apply after the removal of the in-process mode; a second 19-check driver covered the histograms, which agree with the database timestamps, and the compose profile, in which a real Prometheus scraped Forge and Grafana loaded the dashboard and ran all 17 panel queries). The first driver confirmed: access control (no key, wrong key and the worker key are refused); the exposition parses with the official Prometheus text parser; gauges equal the database and `/v1/queues`; counters match the jobs that actually ran (accepted, `queue_full`, invalid, claimed, succeeded, retried, failed, timeout, panic, released, reaper requeued and exhausted, ownership lost); bounded cardinality under 30 bogus types; a frozen database gives `forge_queue_stats_up 0` and a prompt 200; counters reset on restart while gauges do not; a scrape with 20,000 queued jobs; (the check of the removed in-process mode no longer applies). This is manual validation, not an automated regression suite.

### Current error behavior

- `400`: malformed/invalid request, unsupported job type, invalid handler input, invalid list limit, or an invalid job option (`invalid_option`), or an invalid `Idempotency-Key` (`invalid_idempotency_key`).
- `409`: `idempotency_key_conflict`, the key was already used for a different job.
- `404`: unknown job ID or route.
- `405`: unsupported HTTP method.
- `413`: request body larger than 1 MiB.
- `503` with error code `queue_full` and a `Retry-After` header (seconds): the queued-job limit (`FORGE_MAX_QUEUED`) is reached; the submission is rejected before anything is stored. This is backpressure, not a failure: a submitter should wait at least `Retry-After` (adding jitter) and try again, and should not count it against its own retry budget. 
- `503` with error code `admission_busy` and `Retry-After: 1`: Forge allows at most 3 submissions per process to be processed at once (they are serialised by a database lock anyway, and waiting submissions used to hold database connections the workers need). A submission that cannot get a slot within 500 ms is refused before any database work: nothing is stored and the queue may well have room. It is momentary backpressure, distinct from `queue_full`: retry after at least `Retry-After` (with jitter) without counting it against a retry budget. The limit is a fixed internal constant, per Forge process, and it does not itself raise the admission ceiling (roughly 1 divided by the time the lock is held; the check-and-insert is one database call, and on one host with a RAM-backed database 800 submissions/s held in both repetitions measured, see `docs/benchmarks.md`); above it, excess submissions are refused quickly instead of starving workers.
- Other `503` responses (for example a database failure) carry no `Retry-After` and neither of those codes.
- `500`: unexpected server-side failure; internal details are not sent to the caller.

## Phase 1 validation record

Checked manually on 2026-10-18 against the local Compose PostgreSQL 16 container, using Atlas v1.3.0 and a `go build -race` binary: migration applied to an empty database and schema inspected; health, submit, poll to `succeeded`, list, unknown ID (404), malformed JSON (400), unsupported type (400) and empty-records input (400); a 20-request burst (18 accepted, 2 rejected with 503 and no leftover rows); clean shutdown; restart with the earlier job still fetchable and listed; and `docker compose down`/`up` keeping all rows. Not exercised: the `failed` job path, execution timeout, database outage while running, and graceful behavior of in-flight jobs at shutdown. Manual checks give no regression protection.

## Validation policy

This repository contains no `*_test.go` files; behavior is validated by running the real application and the load-test tool. This is not a claim that automated tests are unnecessary for production software. Each task must report only commands that were actually run and outcomes that were actually observed.

The project was built in phases. Phase 1 was the PostgreSQL persistence foundation. The current phase, Phase 2 (Reliable Job Execution and Distributed Workers), is substantially implemented: the design and build-versus-adopt work is done (`docs/decisions/0001-execution-architecture.md` compared keeping the custom PostgreSQL engine against Cloud Tasks and Hatchet and chose to keep it; earlier design reports for a RabbitMQ outbox were not implemented and no broker is used), and the execution schema, claim loop, leases and reaper, retries, remote worker API, worker client and runtime, operator endpoint, exact queue counter, bounded admission, per-job attempt limits and a ForgeCart reference application are implemented and validated by hand and by the load runs. The status of the further developer-control work is tracked in `docs/decisions/0004-developer-controlled-configuration-and-scaling.md` (section 0A).

## Security and scope notes

- Do not expose this prototype to a public network: the three keys are shared static secrets over plain HTTP, with no user accounts, rotation or rate limiting.
- Do not execute arbitrary uploaded code or shell commands.
- Do not claim exactly-once job execution.
- Do not claim support for a specific number of users or jobs per second without reproducible benchmarks.
- Keep real credentials out of Git; use local environment variables or an ignored `.env` file for development.
