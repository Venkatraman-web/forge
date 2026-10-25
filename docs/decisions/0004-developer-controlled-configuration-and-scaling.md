# Design 0004 — Developer-controlled configuration and scaling

Status: **partially implemented design; owner decisions still open.** Written 2026-10-21 as a design-only record; the implementation status was reconciled with the code on 2026-10-22 (see section 0A). Sections 1 to 11 below are the original design and **still describe proposals**; where section 0A or an inline *Update* note says a part now exists, the source code is authoritative. Nothing here was changed in the design itself, and no decision D-13 to D-24 has been recorded as approved in the repository. Date: 2026-10-21.

Companion to [../forge-v1-requirements.md](../forge-v1-requirements.md) (requirements, gates and decisions D-1 to D-12) and to [0001](0001-execution-architecture.md) (architecture), [0002](0002-remote-worker-api-v0.md) (worker API) and [0003](0003-worker-runtime-v0.md) (worker runtime). This record answers: **how does a developer control how their background work executes and scales, and how do we prove the controls work?** It does not select a queue technology.

**Where this lives.** Designs in this repository are numbered records under `docs/decisions/`. `forge-v1-requirements.md` already holds the requirements work and has its own structure, so this design is a new record (0004) and the requirements file gets a short pointer (its section 11) instead of being rewritten.

## 0. Conventions

| Tag | Meaning |
|---|---|
| **[CODE]** | Verified by reading the source on 2026-10-21 (file named). |
| **[MEASURED]** | A number from a recorded run (`fullpost2` unless stated), with conditions. Not a guarantee. |
| **[INFERENCE]** | A conclusion from code or measurements; not directly tested. |
| **[PROPOSED]** | A design in this document. **No proposed API, field, endpoint or file below exists today.** Code blocks marked *proposed* are sketches, not working code. |
| **[ASSUMPTION]** | Taken as true to proceed; the owner should confirm. |
| **[DECISION D-n]** | Needs owner approval (section 10). |

Capability classes used in section 2:

| Class | Meaning |
|---|---|
| **C1** | Implemented and exposed to developers through a documented interface |
| **C2** | Implemented internally, not exposed through a stable developer-facing interface |
| **C3** | Partially implemented |
| **C4** | Not implemented |
| **C5** | Unknown or not sufficiently validated |

A benchmark that can change an internal setting does **not** make that setting a product feature: the benchmark harness sets Forge environment variables directly, which a developer may or may not be able to do.

---

## 0A. Implementation status (reconciled with the code on 2026-10-22, HEAD `a075cc9`)

This section was added after the design above was written. It records what exists, using the tags of section 0, and it changes no design or decision. **Implemented does not mean the related roadmap item is complete, and an experiment is not a production capacity guarantee.**

| Item (roadmap number in section 11) | Status | What exists, and what does not |
|---|---|---|
| Read-only queue status (item 2) | **Implemented, manually validated** | `GET /v1/queues` in `internal/operatorapi`. It is on its **own opt-in listener** with its **own bearer key** (`FORGE_OPERATOR_ADDR`, `FORGE_OPERATOR_API_KEY`); the design did not specify that separation. It returns per-type `queued`, `running`, `oldest_due_job_age_seconds`, the global `max_queued` and process-local `queue_full` rejection counters. `max_pending` is always `null` (no per-type limit exists). Counters are in memory, per process and reset on restart; `admission_busy` rejections are **not** counted. Plain HTTP and a single static key: not production authentication. |
| Backpressure contract, D-19 (item 3) | **Implemented in the recommended form (a), manually validated** | `503` with code `queue_full` and `Retry-After: 5`; ForgeCart's dispatcher (`forgecart/internal/dispatcher`) treats it as **uncharged** backpressure (rescheduled after the hint, claims paused). The repository holds no separate owner-approval record for D-19. The historical overload result (queue cap 40, 600 events drained, none failed) is project-notes evidence only; no retained result directory contains it. |
| Exact queue counter | **Implemented, validated; not a roadmap item in this record** | Migration `20261021173152_queue_counter.sql` adds a one-row `queue_counter` table kept equal to `count(*) WHERE status='queued'` by triggers on `jobs`; admission reads it instead of counting. It answers the admission-cost concern raised in 5.1 and E-3/E-4 (the count's cost grew with queue depth, measured with admission-only probes). Atlas's free build cannot plan triggers, so the migration was hand-extended before it was applied. Every enqueue and claim updates one row: a possible hot spot, measured only up to 600 offered jobs/s. |
| Bounded admission (not in this design) | **Implemented, validated** | At most 3 concurrent admissions per Forge process, 500 ms maximum wait for a slot, taken before any database work; otherwise `503 admission_busy` with `Retry-After: 1`, nothing stored. ForgeCart handles it as uncharged per-event backpressure without pausing claims. Added after E-1 follow-ups showed submissions waiting for the admission advisory lock occupying pooled connections that workers need. The constants are fixed, not tuned, per process, and not configurable. It does not itself raise the admission ceiling (about 1 / the lock hold time); the later `forge_submit_job` function (one database call instead of about a dozen round trips under the lock) shortened that hold time, see `docs/benchmarks.md`. Behaviour with several Forge processes sharing a database is untested. |
| Forge-isolated benchmark E-1 (item 4) | **Executed; historical evidence only** | Harness command `e1` in `forgecart/test_files/`, with follow-up experiments (knee sweep, admission-only probes, interleaved A/B, diagnostics). Single host, PostgreSQL on tmpfs, noisy desktop, 1 to 3 repetitions, large session-to-session variance. The results found a throughput cliff and the worker-starvation mechanism that led to bounded admission. They are **not** a capacity guarantee. The raw runs are git-ignored and exist only on the owner's machine. |
| Experiments E-3 to E-7 (item 5) | **Not run as specified** | The admission-only probes address part of E-3 informally; E-4 to E-7 were not run. |
| Worker concurrency per process | **Implemented (C1), validated** | `workerruntime.Config.Concurrency` already existed (1 to 64, default 1). No per-type `MaxConcurrent`, `Stats()`/`Observer` or `ConfigFromEnv` (item 8, D-22) exist. |
| Execution timeout, worker side | **Implemented, validated** | `workerruntime.WithTimeout(d)` as a `Worker.Handle` option: a per-job-**type**, worker-side setting that can only **shorten** the claimed job's own timeout; minimum 1 s. Not part of the original design; it complements the per-job timeout below. |
| Per-job `timeout` (half of item 7, D-15(b)) | **Implemented, validated; named and bounded differently from the design** | `POST /v1/jobs` accepts `options.timeout_seconds` (whole seconds, default 30, per attempt), stored as `jobs.timeout_seconds` (migration `20261022015208_job_timeout.sql`, CHECK 1 to 3600) and delivered to workers as the claim's `deadline_seconds`; retries, release and recovery keep it; the embedded pool uses it too. The upper bound is a **server-controlled maximum**, `FORGE_MAX_JOB_TIMEOUT` (default 30 s, i.e. a stock server accepts nothing longer than before; allowed 30 s to 3600 s), not a per-type policy (none exists). Invalid values and values above the maximum get `400 invalid_option` and nothing is stored. The 3600 s schema ceiling is a placeholder pending D-3. **Server-side enforcement of the deadline on lease renewal (D-15's optional part) is still not implemented.** |
| Per-job `max_attempts` (half of item 7, D-15(b)) | **Implemented, validated; bounded differently from the design** | `POST /v1/jobs` accepts `options.max_attempts` (total attempts including the first, default 3, 1 to 20); invalid values get `400 invalid_option`. The bound is the existing `jobs.max_attempts` column limit (1 to 20), **not** a per-type policy (none exists). Job responses now include `max_attempts`. No schema change. Backoff is still server-wide, as the design requires. |
| Static type policy, server config file, `GET /v1/config` (item 7, first slice) | **Implemented (a subset), validated manually** | `FORGE_CONFIG_FILE` (`internal/policy`): version 1, strict JSON, remote mode only, declares the job types and per type `default_max_attempts`, `max_max_attempts`, `default_timeout`, `max_timeout`; `GET /v1/config` on the operator listener shows the resolved values. Not implemented from section 4.3: `global` block, per-type `max_pending`, `max_input_bytes`, `retention`, `retry_backoff`, reload. The server-enforced deadline below is still not implemented. |
| Server-enforced deadline (rest of item 7) | **Not implemented, proposed** | The deadline is still advisory (enforced by the SDK and the embedded pool only), and there is no per-type default or maximum for timeouts or attempts. Jobs longer than the operator-raised maximum remain unsupported. Depends on owner decisions D-3 and D-14. |
| Submit client SDK (item 6, first half) | **Implemented, validated; narrower than the design** | `sdk/forgeclient`: `New`, `Submit`, `Get`, `List`; typed errors for `queue_full`, `admission_busy`, `invalid_option`, not found, unavailable and malformed responses; `IsBackpressure`, `RetryAfter`, `IsUncertain`. No automatic retries and no authentication (the API has none). ForgeCart's dispatcher now uses it instead of a private client. The design's `APIKey` field and `Options` struct are not present (flat `MaxAttempts`/`TimeoutSeconds` fields instead). |
| Submission idempotency key (item 6, second half, D-11) | **Implemented 2026-10-23 for single submission (header `Idempotency-Key`); not for batches; validated on AWS** | A repeat with the same key and content returns the first job; different content is `409 idempotency_key_conflict`. Content is compared in the database (type, input as jsonb, resolved `max_attempts` and `timeout_seconds`), so no fingerprint column exists. Keys live as long as the job row (no retention yet). |
| Per-type pending limits (item 9, D-16), retention (item 10), scaling guide (item 12), architecture gate (item 13) | **Not implemented, proposed** | `GET /v1/queues` is the only operational signal today; no metrics code exists. |
| Metrics exposition (item 11, D-20, first part) | **Implemented (v1a), validated manually; D-20 resolved as option (c)** | `GET /metrics` on the operator listener, Prometheus text via the adopted `prometheus/client_golang` v1.23.2 (newest release compatible with Go 1.24). Queue gauges per type are read from the database at scrape time; counters (submissions by result, job events, reaper recoveries, admission, DB pool) are per Forge process and reset on restart. v1b adds queue-wait and attempt-duration histograms (database clock) and an optional `observability` compose profile (Prometheus plus a provisioned Grafana dashboard). JSON `/v1/queues` is retained. |
| Submission API key (`FORGE_API_KEY`) | **Implemented 2026-10-22, validated manually** | The submission API (all paths except `GET /health`) requires its own bearer key, distinct from the worker and operator keys (`internal/auth`, shared by the three APIs). `sdk/forgeclient` requires `Config.APIKey`. Still one shared static key: no per-user identity, rotation or tenant scoping. |
| Batch submission (`POST /v1/jobs/batch`, `forgeclient.SubmitBatch`) | **Implemented 2026-10-23, validated locally; measured on AWS (`docs/benchmarks-aws.md`): at least 3,000 jobs/s accepted with no workers, 1,200 sustained with workers claiming** | Up to 100 jobs in one transaction with one commit (migration `forge_submit_jobs`), all or nothing, exact queue limit preserved. Added because the AWS measurement showed the admission lock held through each durable commit capped single submissions at about 335/s. ForgeCart's dispatcher still submits one job per request. |
| Sharded queue counter | **Implemented 2026-10-23, validated locally; measured on AWS (`docs/benchmarks-aws.md`): with workers claiming, sustained submissions rose from about 610 to 1,200 jobs/s** | `queue_counter` became 16 shard rows (migration `20261023120000_queue_counter_shards.sql`); triggers update the shard of the current transaction and admission reads the sum. Reason: on AWS the single row serialised every claim and submission behind each other's commit (about 580 transactions/s together, against 3,000 jobs/s for submissions alone). The queue limit stays exact (never exceeded). |
| Batch completion (`POST /v1/worker/complete`, `Store.CompleteBatch`, `workerclient.CompleteBatch`, flusher in `workerruntime`) | **Implemented 2026-10-23; checked against real PostgreSQL and measured on AWS (docs/benchmarks-aws.md section 8): completions during load about 1,950-2,030 jobs/s against about 1,550-1,720 without it** | Added because completing was the next-largest database cost (about 38% of database time per job) and each completion was its own commit.  |
| Batch claim (`max_jobs` on `POST /v1/worker/claim`, `workerclient.ClaimBatch`, one claimer per worker in `workerruntime`) | **Implemented 2026-10-23, validated locally; real-disk AWS effect not yet measured** | Added because a database profile showed claiming was 65% of database time per job (2.23 ms) and 48 slot loops claiming one job each skipped each other's locked rows. Locally: 0.35 ms per job and 1,452 jobs/s completed against 889. |
| Embedded execution mode and `dataset_validate` | **Removed 2026-10-22** | Forge now always runs with customer-run remote workers; `FORGE_EXECUTION_MODE` other than `remote` and `FORGE_WORKERS` are startup errors. Mentions of the in-process mode elsewhere in this document are historical. |
| Cancellation, delayed-job API, priorities, named queues, autoscaling, multi-tenancy | **Not implemented; deferred** | As in section 7. |
| Owner decisions D-13 to D-24 | **Unresolved** | Not recorded as approved. D-3 (maximum job duration) and D-15 (per-job options) are the ones that block the next API work. |

Item 1 (refresh `CLAUDE.md` and `task.md`) was carried out by the documentation-refresh milestone of 2026-10-22, together with the status lines of ADRs 0001 to 0004.

---

## 1. Executive summary and product principles

**Vision.** Developers control how their own applications execute and scale background work, using Forge's SDK/runtime, server-side configuration and their own deployment tooling. Forge provides the engine and the control surface; developers supply business logic and choose settings. ForgeCart is a reference workload for evaluating the controls and must not be baked into the product.

**What exists today.** The execution engine is solid (durable PostgreSQL job state, fenced claims, leases, retries, recovery, graceful shutdown; a Go worker SDK with per-process concurrency). The **control surface is thin**: a developer can set the number of slots per worker process and a few timing values, an operator can set a few global server environment variables, and almost everything else is a fixed constant or absent (no per-job options, no per-type limits, no queue status, no metrics, no submit SDK).

**Four findings from inspection that shape the design** (details in the sections cited):

1. **Backpressure is incomplete.** `POST /v1/jobs` answers `503 queue_full` with no `Retry-After` [CODE `internal/api/server.go`]. ForgeCart's dispatcher treats any 5xx as a *charged* retry, with a default budget of 8 attempts [CODE `forgecart/internal/dispatcher`], so about two minutes of sustained queue-full (backoff 1 s doubling to a 5 min cap) dead-letters outbox events. The benchmarks never exercised this because the harness raises the cap to 50,000 (section 5.2).
2. **Concurrency and the poll cap disagree by default.** The SDK allows 64 slots per process, each idle slot long-polls, and the server allows only 64 simultaneous waiting claims by default (`FORGE_WORKER_MAX_POLLS`). Two default-sized large workers cannot all wait [CODE-derived]. Each waiting claim also runs the full claim `UPDATE` statement every `FORGE_POLL_INTERVAL` (default 1 s) (section 5.3).
3. **Capacity is one global number.** `FORGE_MAX_QUEUED` bounds all job types together. One flooded type returns `503` to every other type, even though per-type worker pools already isolate *execution* (section 5.1).
4. **The job type is the only routing key,** and it comes from a static environment allowlist (≤32, restart to change) [CODE].

**Principles for the design.**

1. **Server-enforced ceilings, client-chosen values inside them.** Anything that protects shared state or other users is decided by the server; developers choose within bounds, and an out-of-range value is rejected, never silently clamped (matching `workerruntime.New` today).
2. **Ownership follows blast radius.** A setting that affects one job is the submitter's; one that affects a job type is the type owner's; one that affects the whole system is the operator's. A worker may declare what it can *handle*, never what the server must *accept*.
3. **Small, coherent control set.** Every knob needs a stated problem it solves, a default, a validation rule and a behavior at its limit. No knob ships "because it is possible".
4. **Additive and backward compatible.** Omitting every new field reproduces today's behavior exactly.
5. **Do not leak internals.** Claim generations, lease tokens and table names are never developer interfaces (the SDK already hides generation and worker ID from handlers).
6. **Manual scaling first.** Forge supplies the signals to scale; the deployment tool performs the scaling.
7. **Measure before restructuring.** No queue-technology change, and no performance claim, without the experiments in section 8.

---

## 2. Current architecture and capability inventory

### 2.1 Architecture in brief [CODE]

- One `forge` process serves the submission/status API (`internal/api`), the worker API on a second listener (`internal/workerapi`, five endpoints), and the reaper. `FORGE_EXECUTION_MODE=embedded` also runs an in-process worker pool with compiled-in handlers; `remote` runs none.
- PostgreSQL is the only store. The `jobs` row is the work item (no separate enqueue step). A claim is one statement (`claimSQL`, `internal/store/postgres.go`): oldest due `queued` job of the requested types, `FOR UPDATE SKIP LOCKED`, `ORDER BY next_run_at, created_at, id LIMIT 1`. All owner-sensitive writes are fenced on `status='running' AND claim_generation AND worker_id`.
- Developer-side: `sdk/workerclient` (typed HTTP client, no retries) and `sdk/workerruntime` (`Worker.Handle`, `Worker.Run`, slots, heartbeat, watchdog, claim backoff, graceful release). ForgeCart's dispatcher and five worker commands use them (`forgecart/internal/workerapp`).
- There is **no submit client SDK**: submitters call HTTP directly (ForgeCart's `dispatcher/forge.go` is a private client). *Update 2026-10-22: `sdk/forgeclient` now exists and the dispatcher uses it; see section 0A.*

### 2.2 Where each setting is controlled today [CODE]

| Setting | Controlled by | Default | Range / validation |
|---|---|---|---|
| Global pending-job cap | Env `FORGE_MAX_QUEUED` (operator) | 1000 | whole number ≥1 |
| Worker long-poll cap | Env `FORGE_WORKER_MAX_POLLS` | 64 | ≥1 |
| Claim poll interval (server side) | Env `FORGE_POLL_INTERVAL` | 1 s | duration >0 |
| Lease / heartbeat / safety margin | Env `FORGE_LEASE_DURATION`, `FORGE_HEARTBEAT_INTERVAL`, `FORGE_LEASE_SAFETY_MARGIN` | 15 s / 5 s / 10 s | heartbeat < safety < lease, checked at startup |
| Reaper interval, batch, recovery backoff | Env `FORGE_REAPER_*`, `FORGE_RECOVERY_BACKOFF` | 5 s, 100, 2 s | validated |
| Retry backoff base / max | Env `FORGE_RETRY_BASE_DELAY`, `FORGE_RETRY_MAX_DELAY` | 2 s / 5 min, ±20% jitter | `worker.ValidateRetryDelays` |
| Embedded worker count | Env `FORGE_WORKERS` (embedded only; rejected in remote mode) | 2 | ≥1 |
| Allowed job types | Env `FORGE_REMOTE_JOB_TYPES` | none (required in remote mode) | ≤32 types, `^[a-z][a-z0-9_.-]{0,63}$`, restart to change |
| Execution deadline | **Constant** `executionTimeout` in `cmd/forge/main.go` | 30 s | *Update 2026-10-22: this constant is now only the default and the fallback; each job carries its own `timeout_seconds` (`options.timeout_seconds`, bounded by `FORGE_MAX_JOB_TIMEOUT`).* Sent to workers as `deadline_seconds` (advisory), enforced by the SDK |
| Attempt budget (`max_attempts`) | DB column default; per job `options.max_attempts` (*added 2026-10-22*) | 3 | CHECK 1–20; **not settable at submission as of this snapshot (2026-10-21); settable since 2026-10-22** |
| Submission body / result size | **Constants** | 1 MiB / 256 KiB | not configurable |
| Database pool | **Constant** `dbMaxConns` | 10 | not configurable |
| Worker slots per process | SDK `workerruntime.Config.Concurrency` | 1 | 1–64 |
| Worker poll wait | SDK `Config.PollWait` | 20 s | 1–25 s, whole seconds |
| Worker shutdown grace | SDK `Config.ShutdownGrace` | 30 s | up to 1 h |
| Outcome-report resends | SDK `Config.ReportAttempts` | 2 | 1–3 (the package documents it as used by a later task) |
| Claim error backoff | SDK constants `claimBackoffBase/Max` | 1 s / 30 s | not configurable |
| ForgeCart worker settings | Env `FORGECART_WORKER_CONCURRENCY`, `_NAME`, URL, key | concurrency 4 | ForgeCart validates 1–8; **poll wait, grace and report attempts are not exposed by ForgeCart** (`workerapp.go` passes only `Name` and `Concurrency`) |
| ForgeCart dispatcher | Env `FORGECART_DISPATCHER_BATCH_SIZE` (also its concurrency bound), `_POLL_INTERVAL`, `_MAX_ATTEMPTS` | 10, 1 s, 8 | validated |

Benchmark harness note: the harness sets `FORGE_MAX_QUEUED=50000`, `FORGE_WORKER_MAX_POLLS=512`, `FORGE_RETRY_BASE_DELAY=1s`, `FORGE_RETRY_MAX_DELAY=10s`, short leases for the recovery scenario, and varies worker replicas, concurrency and dispatcher settings (`test_files/sandbox.go`). These are environment settings of a test sandbox, not evidence that the product exposes them to developers.

### 2.3 Capability inventory by layer

**Job level**

| Capability | Class | Evidence / note |
|---|---|---|
| Submit a job (type + JSON input), `202` with ID | **C1** | `POST /v1/jobs`, `internal/api/server.go` |
| Inspect a job | **C1** | `GET /v1/jobs/{id}`; list returns newest 100, no filters or pagination |
| Per-job timeout | **C4** (global fixed 30 s is **C2**) | `executionTimeout`; no request field. *Update 2026-10-22: now **C1**: `options.timeout_seconds` at submission (server maximum `FORGE_MAX_JOB_TIMEOUT`), plus per-job-type shortening through `workerruntime.WithTimeout`. See section 0A.* |
| Per-job retry budget | **C2** | `max_attempts` column exists (default 3, 1–20); `Service.Submit` never sets it. *Update 2026-10-22: now settable at submission through `options.max_attempts` (C1); see section 0A.* |
| Backoff policy | **C2** operator-global only | env `FORGE_RETRY_*`; per type or job: C4 |
| Failure classification | **C1** | `workerruntime.Permanent(err)`; timeouts, panics classified by the runtime |
| Submission idempotency key | **C4** | no field; ForgeCart implements it on *its own* checkout API (200 replay / 409 key reuse) and uses an outbox `event_id` for jobs |
| Cancellation by API | **C4** | handler context is cancelled only on lost ownership, self-fence, deadline, shutdown |
| Delayed / scheduled start | **C2** | `next_run_at` exists and drives claims; not settable at submission |
| Payload / result limits | **C2** | constants (1 MiB, 256 KiB); fixed |

**Queue level**

| Capability | Class | Evidence / note |
|---|---|---|
| Global pending cap, exact under concurrency | **C1** operator env | `CreateIfQueueBelow`: advisory lock, `COUNT(*) WHERE status='queued'`, insert in one transaction. *Update 2026-10-22: the count is now read from the trigger-maintained `queue_counter` row, and admission is bounded (3 slots per process); the lock and the exactness are unchanged. See section 0A.* |
| Queue-full response | **C3** | `503 queue_full`; no `Retry-After`; code is not distinguishable from other 503s by status alone. *Update 2026-10-22: `Retry-After: 5` is now sent and ForgeCart handles it as uncharged backpressure; a second distinct response, `admission_busy`, exists. See section 0A.* |
| Named queues / queue entity | **C4** | none; job `type` is the routing key |
| Per-type pending limit | **C4** | |
| Type-based routing to workers | **C3** | claim filters by type list (≤8 per claim) inside the claim statement; the set of valid types is a static env allowlist |
| Priorities | **C4** | claim order is global FIFO by due time |
| Fairness across submitters/types | **C4** | |
| Retention / expiration of finished jobs | **C4** | no cleanup; the table grows without bound |
| Queue status / backlog visibility | **C4** (*update 2026-10-22: C1 through the opt-in operator endpoint `GET /v1/queues`, see section 0A*) | `Store.CountQueued` exists but is used only for one startup log line (`cmd/forge/main.go:299`), not exposed by any endpoint; `/health` is HTTP liveness only |

**Worker-instance level**

| Capability | Class | Evidence / note |
|---|---|---|
| Max simultaneous executions per process | **C1** | `Config.Concurrency` 1–64 |
| Per-type (or per-queue) concurrency inside a process | **C4** | all registered types share the process's slots |
| Batch claim | **C4** | one job per claim request, one claim per slot |
| Poll wait | **C1** SDK; server polling interval **C2** operator env | |
| Graceful shutdown | **C3** | works; the runtime's package comment says final shutdown hardening is "not yet built" |
| Execution timeout / cancellation inside the process | **C3** | enforced against the fixed 30 s deadline; not configurable per type. *Update 2026-10-22: shortenable per type with `workerruntime.WithTimeout`.* |
| Resource guidance / safe ranges | **C5** | no documentation or measurements for CPU-bound or long handlers |
| Process-level stats for a developer | **C3** | only `Worker.Wedged()` and logs |

**Deployment level**

| Capability | Class | Evidence / note |
|---|---|---|
| Run several worker processes | **C1** (by nature) | any number of processes may claim; claims are safe across processes |
| Forge awareness of worker instances | **C4** | no registry, no liveness view; worker IDs are free-form strings |
| Orchestrator integration / autoscaling | **C4** | |

**Observability**

| Capability | Class | Evidence / note |
|---|---|---|
| Structured logs without payloads | **C1** | `slog` in the runtime; `log` in servers |
| Metrics (any) | **C4** | no metrics code anywhere in `cmd/`, `internal/`, `sdk/` |
| Readiness / queue-aware health | **C4** | |
| Attempt history | **C4** | current `attempt` counter only; the API shows no attempt count |
| Benchmark observability | **C3** | the harness polls `GET /v1/jobs?limit=100` and parses worker logs; it documents the API gaps |

### 2.4 Integration and scaling facts found during inspection

| # | Fact | Basis |
|---|---|---|
| I-1 | `503 queue_full` has no `Retry-After`. ForgeCart's dispatcher classifies by status only, treats any 5xx as retry, **charges** the attempt, and fails the event with `attempts_exhausted` after `FORGECART_DISPATCHER_MAX_ATTEMPTS` (default 8; backoff 1 s doubling, cap 5 min). Honoring a `Retry-After` is supported (`parseRetryAfter`) but never triggered. | [CODE] `internal/api/server.go:186`, `dispatcher/forge.go classify`, `dispatcher.go retry` |
| I-2 | Retried, released and lease-reaped jobs return to `queued` without an admission check, so `status='queued'` can exceed the cap; the cap counts retry-waiting and delayed jobs too. | [CODE] `FailAttempt`, `Release`, `ReapExpired`, `countQueued` |
| I-3 | Admission takes a **global** advisory lock and runs `COUNT(*)` over queued rows for every submission. Its cost grows with queue depth and serializes all submissions. | [CODE]; the cost is [INFERENCE], never measured |
| I-4 | The claim filters by `type` while ordering by `(next_run_at, created_at, id)` over a partial index that does not include `type`. A large backlog of a type nobody serves may be scanned past on every claim by workers of other types. | [CODE]; the cost is [INFERENCE] (no `EXPLAIN` run) |
| I-5 | Every idle slot runs the claim `UPDATE` once per poll interval. Idle load ≈ idle slots per second. The default server poll cap (64) is below the SDK per-process maximum (64) times process count. | [CODE]-derived |
| I-6 | Queue wait p50 of 0.55–0.77 s and p95 ≈ 1 s with a near-empty Forge queue is consistent with the 1 s polling interval. | [MEASURED] pipeline; interpretation [INFERENCE] |
| I-7 | Benchmarks to date are dispatcher-limited: in the soak 99.7% of the backlog was unpublished in ForgeCart's outbox, Forge held ~76 jobs. Forge's own admission, claim and completion capacity are unmeasured. | [MEASURED] soak (`outbox_unpublished` 24,740 of 24,816) |
| I-8 | ForgeCart measured the cost of an outbox relay hop: event created → accepted by Forge p50 2.6 s, p95 3.9 s (dispatcher batch 50, poll 200 ms). Config-specific; not a property of every relay. | [MEASURED] pipeline |
| I-9 | Peak ForgeCart-side database connections were 93 at the benchmark's highest concurrency: downstream connection budgets, not Forge, were the large consumer. | [MEASURED] max over `fullpost2` |

---

## 3. Configuration ownership model

### 3.1 Roles

- **Submitter**: the code calling Forge to create jobs (the developer's web application or service).
- **Handler author**: the code registered in a worker (`Handle`).
- **Type owner**: whoever owns a job type's policy (limits, timeouts, retries). In v1 under D-2 option A (one organization), this is the developer team; it is the same human group as the operator but a different *interface*: the policy is declared in server configuration, not chosen by each job.
- **Operator**: whoever runs the Forge server and its database, and the worker processes.

**[ASSUMPTION A-1]** For v1 (D-2 option A, single organization), the developer team and the operator are the same people, so developer-chosen settings can live in server configuration files they control. If v1 becomes multi-tenant (D-2 option B), type-level policy needs a management API with authentication and authorization, and the scope of this design widens (section 7).

### 3.2 Layering and precedence

```text
server hard ceilings (operator, global)           e.g. max body, max timeout, max attempts, DB-protecting limits
   └─ job-type policy (type owner, server config)  defaults and maximums per type
        └─ per-job options (submitter)             optional, must lie within the type's maximums
```

Effective value of a per-job setting = the job's value if given, else the type default, else the global default. A job value above its ceiling is **rejected with `400 invalid_option`** (never clamped). Worker-side values (concurrency, poll wait, grace) are chosen in the SDK because they configure one process, but can never raise a server ceiling.

### 3.3 A. Job-level settings

| Setting | Chosen by | Server-enforced bound | Default | Validation / behavior at the limit | Delegable to the client? |
|---|---|---|---|---|---|
| **Timeout** (`timeout`) | Submitter, optional | `≤` type `max_timeout` `≤` global ceiling | type `default_timeout` (today: fixed 30 s) | Out of range → `400 invalid_option`. Delivered to the worker as `deadline`. Enforcement: SDK cancels the handler; **server-side enforcement [PROPOSED]**: a renewal after `attempt_started_at + deadline` is refused so the worker self-fences and the reaper records a timeout. Without server enforcement a misbehaving worker can outlive its deadline (documented today: "the server never enforces it") | Partly: the *value* yes, the *ceiling* no |
| **Retry budget** (`max_attempts`) | Submitter, optional | `≤` type `max_max_attempts` `≤` 20 (existing CHECK) | type default (today 3) | Out of range → 400. Exhaustion → terminal `failed` / `attempts_exhausted` (existing) | Partly |
| **Backoff** (base, max, jitter) | **Type owner**, not per job | global bounds from `ValidateRetryDelays` | today global 2 s / 5 min / ±20% | Not per job: a per-job backoff would let one client create retry storms. Stays server-computed | **No** |
| **Idempotency key** | Submitter, optional | key charset/length; scope = job type; remembered for the job retention window | none | Same key + same payload fingerprint → returns the existing job (`200`, same shape as `202`) with `Idempotent-Replayed: true`. Same key + different payload → `409 idempotency_key_reused`. Mirrors the contract ForgeCart already implements and documents on its checkout API (200 replay / 409 reuse) | Yes (a key is the submitter's); the *window* is server-owned |
| **Cancellation** | Submitter requests; server arbitrates | only the submitting credential may cancel (once auth exists) | n/a | Queued job → atomic transition to a terminal `cancelled` state. Running job → cooperative: the next lease renewal tells the worker (a defined "cancel requested" outcome) and the handler context is cancelled; the job ends `cancelled` or, if the handler finishes first, its real outcome. Requires a schema change (new state and CHECK shape) | Request yes; state transitions no |
| **Delay / schedule** (`delay`) | Submitter, optional | `≤` type `max_delay` | 0 | Sets `next_run_at`. Counts toward the pending limit. No recurring schedules in v1 | Partly |
| **Payload size** | Server only | body ≤ type `max_input_bytes` ≤ global 1 MiB; result ≤ 256 KiB | global constants | `413` at the limit (existing for the global bound) | **No** (protects the database) |
| **Lease / heartbeat / safety margin** | Operator only | startup rule heartbeat < safety < lease | 15 s / 5 s / 10 s | Wrong values break fencing safety, so they are never per job or per worker | **No** |

### 3.4 B. Queue-level settings

**What "queue capacity" means in Forge.** Forge is durable: the queue is the set of `status='queued'` rows in PostgreSQL, not a bounded in-memory channel (Phase 1's channel is gone). "Capacity" is therefore an **admission policy**, not a storage limit: the maximum number of *queued* (waiting, including retry-waiting and delayed) jobs Forge will hold before refusing new submissions. Running jobs do not count. Jobs re-entering the queue after a retry, release or lease recovery are never refused (I-2), so the observed queued count may exceed the limit by the number of in-flight retries. A larger cap does not cost memory; it costs database rows, admission-time `COUNT`, and delay-to-start.

**Queue identity.** A **queue is named by its job type** in v1 [PROPOSED, D-13]. Routing already works that way (claims filter on type). A separate queue entity (queue name ≠ type, several types per queue, a type on several queues) is deferred; see 7.1.

| Setting | Scope | Meaning | Default | Validation | Behavior at the limit |
|---|---|---|---|---|---|
| `max_pending` | per type, plus the global cap as a ceiling | Maximum jobs with `status='queued'` of that type | unset = only the global cap applies (today's behavior) | integer ≥1, ≤ global cap | Submission refused with the stable error code **`queue_full`**, a `Retry-After` header, an error body naming the type and the limit, and a counter increment [PROPOSED]; the HTTP status stays `503` unless D-19 selects `429`. Never blocks the request, never drops an accepted job, never evicts. Enforced in the same transaction as the insert, exactly as the global cap is today |
| Global `max_pending` | whole server | existing `FORGE_MAX_QUEUED` | 1000 | existing | same response as above |
| Queue-full behavior | per type, one fixed policy: **reject fast with a retry hint** | Alternatives rejected: blocking the request (ties up connections, hides overload), dropping the oldest or newest accepted job (violates durability), spilling to unbounded storage (defeats the bound) | — | — | The submitter owns the response to rejection: retry later using `Retry-After`, shed, or slow its producers. ForgeCart's outbox + dispatcher is one such pattern |
| Priority | — | **None in v1** [PROPOSED, D-18]. Order within the eligible set stays FIFO by due time. Priority is expressed by *capacity*: give urgent types their own worker pool | — | — | — |
| Fairness | per worker claim | A worker serving several types takes the globally oldest eligible job among them. Fairness between types is achieved by dedicated pools and per-type pending limits, not by weighting | — | — | — |
| Retention | per type | How long finished jobs (and idempotency keys) are kept before deletion | owner decision D-6 | duration, minimum covers the idempotency window | A background job deletes in batches; deletion never touches queued or running rows |
| Status / backlog visibility | per type + total | Read-only: pending, running, oldest queued age, configured limits, rejections since start | — | — | See 3.7 |

**Why global admission needs revisiting regardless of per-type limits.** I-3 and I-4 say the current admission path may not scale with depth and the claim path may not isolate types at depth. Both are hypotheses to test (section 8) *before* per-type limits are built on top of them.

### 3.5 C. Worker-instance settings

Definitions that are easy to confuse:

- **Slot / concurrency**: one goroutine claiming and running one job at a time. `Concurrency = N` means at most N handlers run simultaneously **inside one process**.
- **Process / instance / replica**: one OS process running `Worker.Run`. Total concurrent executions in a deployment = Σ over processes of `Concurrency`.
- **Capacity in slots** for a job type = Σ over processes that registered that type of the slots *available to that type*.

| Setting | Chosen by | Meaning | Default | Range / validation | Notes |
|---|---|---|---|---|---|
| `Concurrency` (exists) | Handler author / operator via the app's config | Slots per process | 1 | 1–64 (exists) | Must be sized from handler duration and downstream limits (3.5.1) |
| **Per-type concurrency** (`MaxConcurrent`) [PROPOSED, SDK-only] | Handler author, at `Handle` | At most K of this process's slots may run this type at once | unset = shares all slots | 1 ≤ K ≤ `Concurrency` | Implemented by having each slot request only types that still have free capacity in its claim, which the claim API already supports (type list per request). Gives per-process isolation without a server change. A deployment-wide running cap per type needs server support and is deferred |
| `PollWait` (exists) | Operator | Long-poll length per claim request | 20 s | 1–25 s, whole seconds | Longer waits reduce request count; they do not reduce server-side DB polling (the server polls the database every `FORGE_POLL_INTERVAL` while a claim waits) |
| **Batch / fetch size** | — | Number of jobs per claim | 1 | — | **Not proposed for v1.** The claim API returns one job. A batch or *per-process multiplexed* claim (one long poll per process handing jobs to free slots) would cut polling load (I-5) but is a protocol and store change; justify with experiment E-6 first (section 8) |
| `ShutdownGrace` (exists) | Operator | How long `Run` waits for handlers after cancellation | 30 s | positive, ≤1 h | Must be ≥ the longest handler's shutdown time and ≤ the orchestrator's termination grace period |
| `ReportAttempts` (exists) | Operator | Resends of an outcome report when the result is uncertain | 2 | 1–3 | Documented as not yet wired |
| Name | Operator | Label in worker IDs and logs | host name | ≤40 chars | |
| **Environment loader** [PROPOSED, D-22] | — | `workerruntime.ConfigFromEnv()` reading a documented set of variables, so every application does not hand-write env parsing (ForgeCart exposes only concurrency today) | — | strict: unknown or invalid values rejected, never ignored | Must not collide with the server's `FORGE_WORKER_API_KEY` and `FORGE_WORKER_ADDR` names; naming is an open point |

#### 3.5.1 Safe ranges and sizing (guidance, not measurement)

- **Required slots ≈ job rate × mean job duration** (Little's law). 100 jobs/s of 25 ms jobs ≈ 2.5 slots; of 1 s jobs ≈ 100 slots.
- **Pick the limiting resource first.** Slots × per-handler downstream connections must stay under the downstream budget (ForgeCart's own database reached 93 connections at peak [MEASURED]). CPU-bound handlers: slots ≈ cores of that process's host. I/O-bound handlers: slots limited by downstream concurrency, not cores.
- **Idle cost.** Each idle slot costs Forge one claim statement per `FORGE_POLL_INTERVAL` (I-5). Prefer fewer, busier slots over many idle ones, until multiplexed claiming exists.
- **Server poll cap.** `FORGE_WORKER_MAX_POLLS` must be ≥ the total slots in the deployment, or claims receive `429` and back off (up to 30 s). v1 either raises the default with a documented relationship or removes the mismatch by multiplexing (E-6). **[DECISION D-21]**
- **[C5]** No safe range is documented or measured for CPU-bound or long handlers; the only measured handlers are ~25 ms, database-light.

### 3.6 D. Deployment and scaling settings

Responsibility boundaries:

| Concern | SDK / worker runtime | Forge server / API | Deployment tool, orchestrator or operator |
|---|---|---|---|
| Slots in one process | **Configures** (`Concurrency`, `MaxConcurrent`) | Learns nothing about it; sees claims | — |
| Number of worker processes | Cannot create or remove any | Has no registry today; may *observe* distinct worker IDs [PROPOSED, optional] | **Creates and removes** instances (Compose, systemd, Kubernetes, Nomad, a PaaS) |
| CPU / memory per process | Declares assumptions in documentation only | — | **Assigns** resource requests/limits |
| Job-system limits (pending caps, timeouts, retries) | Cannot raise them | **Owns and enforces** | — |
| Scaling decision | Exposes `Stats()` | Exposes queue depth/age per type | **Decides** (a human, a script or an autoscaler) |
| Graceful scale-in | Handles SIGTERM, releases cooperative jobs | Reaper recovers anything not released | Sends SIGTERM and waits at least `ShutdownGrace` + margin before SIGKILL |

A Go SDK option cannot provision servers. Developer-controlled scaling in v1 therefore means: *the developer sets slots per process in code or environment, runs N identical worker processes with their own tooling, and uses Forge's queue/worker metrics to decide N and the slot count.*

**Replica configuration rules (documentation requirements):**

1. Processes are interchangeable: any number may run the same handlers; claims are safe across processes. A process's identity is generated per start, so restarts and replicas never collide.
2. Total slots per type = Σ(slots × replicas). Choose it from section 3.5.1, then confirm with queue-wait and busy-ratio metrics.
3. Termination grace in the orchestrator ≥ `ShutdownGrace` + a few seconds; otherwise SIGKILL turns a cooperative release into lease-expiry recovery (attempt consumed).
4. Rolling deploys: start new before stopping old; stop old with SIGTERM.
5. Downstream limits: connections per replica × replicas ≤ downstream budget.

**What a future autoscaler would need** (identified, not built): per-type queue depth and oldest queued age; busy-slot ratio; a stable scale-in contract (SIGTERM + grace); min/max replica bounds and cool-down owned by the operator; a guard on downstream and database connection ceilings; and a way to attribute metrics to deployed instances. Forge would expose signals; an adapter (KEDA-style, a cloud autoscaler, a script) would act. No code in Forge talks to an orchestrator. **[DECISION D-23: confirm out of v1.]**

### 3.7 E. Observability and tuning

Placement rule: **the server owns anything derivable from job rows or API traffic; the runtime owns process-local facts; logs carry events; an external system aggregates and alerts.** One definition per metric, one owner.

| Metric | Owner | Definition | Why a developer needs it |
|---|---|---|---|
| Queue depth (pending), per type and total | Forge server | `COUNT` of `queued` jobs | capacity and cap tuning |
| Oldest queued age (per type) | Forge server | `now() − min(created_at)` over due queued jobs | the real backlog symptom; alert source |
| Running count | Forge server | `COUNT` of `running` | utilization vs capacity |
| Submission rate, rejection rate (`queue_full`, other 4xx) | Forge server | counters at the API | backpressure tuning |
| Completion throughput | Forge server | succeeded + failed per second | is the system keeping up |
| Queue wait p95/p99 | Forge server | `started_at − created_at` over a window (the same definition the benchmarks use) | the latency developers feel |
| Execution latency p95/p99 | Forge server (`finished_at − started_at`) and runtime (handler duration, finer) | | handler tuning, timeout choice |
| Retry rate, terminal-failure rate, `attempts_exhausted` count | Forge server | counters at `FailAttempt`/`Fail` | poison jobs, flaky dependencies |
| Lease recoveries (reaped, requeued vs exhausted) | Forge server | counters from `ReapExpired` results | worker health |
| Worker concurrency and utilization | Worker runtime | `Stats()`: slots, busy slots, per-type busy | sizing slots |
| Claim errors/backoffs, wedged handlers, release counts | Worker runtime | `Stats()` / logs | operability |
| Worker instance count | Orchestrator, or Forge via distinct recent worker IDs [optional] | | scaling decisions |
| Backlog drain time | derived externally (depth ÷ net completion rate) | | burst planning |
| Database health (connections, bloat, CPU) | External monitoring of PostgreSQL | | capacity |

**Interfaces [PROPOSED]:**

- **Server:** a read-only JSON `GET /v1/queues` (and `/v1/queues/{type}`) for status and counters, plus an operator-listener metrics endpoint. Whether to expose Prometheus text format and whether to hand-write it or adopt `prometheus/client_golang` is a build-versus-adopt question under `CLAUDE.md` §3A and **not decided here** **[DECISION D-20]**. Windowed percentiles (p95/p99) computed from job rows must be bounded in cost (window and sampling), or computed externally from exported events; this is an experiment (E-9).
- **Runtime:** `Worker.Stats()` returning a plain struct snapshot, plus an optional `Observer` interface (job started/finished with durations, claim error) so developers can feed their own metrics system without Forge adding a dependency.
- **Logs:** unchanged policy: identifiers and fixed words only.
- **Not proposed:** a bundled dashboard, tracing, or a second metrics path duplicating the server's job-row metrics inside the runtime.

---

## 4. Proposed SDK, runtime, server and deployment interfaces

*Everything in this section is **proposed**. It adds to, and does not change, the existing `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/jobs/{id}`, worker API and `workerruntime.Config` behavior when the new elements are omitted.*

### 4.1 Submit client (new package, **proposed**)

Today submitters hand-write HTTP calls. A small typed client removes that and carries the backpressure contract.

```go
// PROPOSED: github.com/Venkatraman-web/forge/sdk/client  (does not exist)
c, err := forgeclient.New(forgeclient.Config{BaseURL: forgeURL, APIKey: key})

job, err := c.Submit(ctx, forgeclient.Submit{
    Type:           "email.send",
    Input:          payload,                 // any JSON, within the type's size limit
    IdempotencyKey: "order-1234-confirmation", // optional
    Options: &forgeclient.Options{             // optional; each bounded by server policy
        Timeout:     20 * time.Second,
        MaxAttempts: 5,
    },
})
switch {
case errors.Is(err, forgeclient.ErrQueueFull):
    wait := forgeclient.RetryAfter(err) // server hint; flow control, not a failure
case errors.Is(err, forgeclient.ErrInvalidOption): // 400: the request itself is wrong
}
```

`ErrQueueFull` is distinguished from "Forge is broken" so callers can apply **uncharged** retries (I-1). A submit client also fixes the integration gap where ForgeCart's dispatcher had to infer meaning from status codes.

### 4.2 Handler registration and runtime configuration (extends `workerruntime`, **proposed**)

```go
// PROPOSED additions; Config and Handle keep their current behavior when unused
cfg, err := workerruntime.ConfigFromEnv()        // optional helper (D-22)
cfg.Client = wc
w, _ := workerruntime.New(cfg)

w.Handle("email.send",     sendEmail,   workerruntime.MaxConcurrent(6))
w.Handle("report.render",  renderReport, workerruntime.MaxConcurrent(2))

go func() { // developer-owned export
    for range time.Tick(10 * time.Second) {
        s := w.Stats() // slots, busy, per-type busy, claimed/completed/failed/retried, claim errors, wedged
        myMetrics.Gauge("forge_busy_slots", s.Busy)
    }
}()
err = w.Run(ctx)
```

Handler code is unchanged by any tuning: it still receives `ctx` and `workerruntime.Job` (ID, type, input, attempt, max attempts). Changing `MaxConcurrent`, `Concurrency`, replica count or timeouts never requires editing a handler.

### 4.3 Server-side policy (**proposed**)

Static, versioned, strictly decoded configuration, applied at startup (JSON, decoded with the standard library like the repository's scenario files; no new dependency):

```json
{
  "version": 1,
  "global": { "max_pending": 5000 },
  "job_types": {
    "email.send": {
      "max_pending": 2000,
      "default_timeout": "20s",  "max_timeout": "60s",
      "default_max_attempts": 5, "max_max_attempts": 8,
      "retry_backoff": { "base": "2s", "max": "5m" },
      "max_input_bytes": 65536,
      "retention": "7d"
    },
    "report.render": {
      "max_pending": 200,
      "default_timeout": "10m", "max_timeout": "30m",
      "default_max_attempts": 2, "max_max_attempts": 3
    }
  }
}
```

- Declares which job types exist, replacing `FORGE_REMOTE_JOB_TYPES` when present; if the file is absent, the environment-variable behavior is exactly today's (backward compatible).
- Validated at startup with cross-field rules: per-type `max_pending` ≤ global; default ≤ max; timeouts ≥ one heartbeat interval; limits within hard ceilings; unknown keys rejected.
- `version` allows a future, incompatible layout to be detected and refused rather than misread. The server states which versions it accepts.
- Changes take effect by restart in v1 (a documented, deterministic behavior). Reload and a management API are deferred **[DECISION D-14]**.
- Effective policy is readable through `GET /v1/config` (read-only) so a developer can confirm what the server enforces.

### 4.4 Server API additions (**proposed**, additive)

| Endpoint / change | Purpose |
|---|---|
| `POST /v1/jobs` accepts optional `idempotency_key` (header or field) and `options` `{timeout, max_attempts, delay}` | per-job settings within type policy (3.3) |
| `POST /v1/jobs` queue-full response: `429` (or `503`) + `Retry-After` + stable error `queue_full` with `type` and `limit` | explicit backpressure contract (I-1). **[DECISION D-19]**: changing 503→429 is observable to existing clients; the safe alternative is to keep `503` and add the header and fields |
| `GET /v1/queues`, `GET /v1/queues/{type}` | read-only depth, running, oldest age, limits, counters (3.7) |
| `GET /v1/config` | effective policy, read-only |
| `POST /v1/jobs/{id}/cancel` (optional, **D-17**) | cancellation (3.3) |
| `GET /v1/jobs` filters `status`, `type`, `limit`, a cursor | inspection beyond the newest 100; removes the harness's polling workaround |

Not proposed: any endpoint that exposes claim generations, lease times, worker IDs of other tenants, or table names.

### 4.5 Deployment interface (**no Forge code**)

A documented contract: the worker is a normal executable configured by environment variables, started N times by the operator's tool. Illustrative only (not an implemented artifact):

```yaml
# ILLUSTRATIVE ONLY: any orchestrator with a replica count works the same way
workers:
  replicas: 3                       # operator's lever for instance count
  env:
    FORGE_RUNTIME_CONCURRENCY: "8"  # developer's lever for slots per process (variable name illustrative, see D-22)
  resources: { cpu: "1", memory: "512Mi" }   # operator's lever for per-process resources
  terminationGracePeriodSeconds: 45          # >= ShutdownGrace + margin
```

### 4.6 Validation, application and versioning

| Aspect | Rule |
|---|---|
| **Validation** | Server config: at startup, fail with a specific message (existing pattern, `cmd/forge/main.go`). Per-job options: at submission, `400 invalid_option` naming the field and the allowed range. Runtime config: at `workerruntime.New`, explicit invalid values rejected, never corrected (existing behavior) |
| **Application** | Server config at restart; per-job options at submission; runtime config at process start. Nothing changes while jobs run, so no in-flight job is subject to a policy change |
| **Versioning** | HTTP API stays `/v1/` and changes only additively: new fields optional, defaults preserve today's behavior. Server config carries `version`. SDK uses semantic versioning with a compatibility table (SDK ↔ server). A protocol version header on worker calls is proposed so a server can refuse an SDK it cannot serve. **[ASSUMPTION]** enforcement of compatibility rules is manual (project policy: no automated tests), so the harness must include a compatibility scenario |
| **Backward compatibility** | Omit all new fields: submission, claim and worker behavior identical to today; `503 queue_full` keeps working for clients that ignore the new header |
| **Safety** | A client option can lower risk (shorter timeout) or ask for more only up to the ceiling; no option relaxes fencing, lease or payload protection |

---

## 5. Queue and worker configuration semantics

### 5.1 Queue capacity and isolation under PostgreSQL

- **What is bounded:** the number of `queued` rows (global and, proposed, per type). **What is not:** memory (nothing is queued in memory), running jobs, finished-job storage (needs retention).
- **Isolation of execution** exists today: a type with no workers simply waits; types with dedicated worker pools do not compete for slots. **Isolation of admission** does not: the one global cap is shared (finding 3), which per-type limits would fix.
- **Isolation of the claim path at depth** is unproven (I-4): the claim orders by due time over an index without `type`. A proposed remedy (index including `type`, or a per-type index) must be justified by experiment E-4 rather than assumed.
- **Admission cost at depth** is unproven (I-3): E-3 measures submission latency and throughput versus queued depth under the global lock. Alternatives to evaluate only if needed: an approximate counter maintained transactionally, per-type lock granularity (reduces serialization across types), or sharded counters. None is adopted by this design.

### 5.2 Backpressure end to end

```text
developer app --Submit--> Forge --queue_full(429/503 + Retry-After)--> app
        │                                                           │
        └── outbox / buffer / shed / slow producers ◄───────────────┘   (submitter's choice)
```

- Forge's job: refuse deterministically and tell the caller when to retry; keep every accepted job; never silently drop.
- The caller's job: treat queue-full as flow control. ForgeCart's dispatcher today treats it as a failure that spends its attempt budget (I-1), so under sustained pressure it loses events to `attempts_exhausted`. The fix lives in two places: Forge must send an unambiguous, header-carrying signal; the submit client (and ForgeCart's dispatcher, as the reference) must retry such responses **uncharged**.
- The cap must be re-derived rather than inherited: burst size × rate tolerated, minus drain rate. The default of 1000 is a conservative development value, not a recommendation [see requirements O7].

### 5.3 Worker concurrency, polling and the database

- Each slot is one independent claim loop. While idle, the *server* re-runs the claim `UPDATE` every `FORGE_POLL_INTERVAL` on behalf of every waiting request (I-5). N idle slots ⇒ ~N claim statements/s against PostgreSQL, from a connection pool fixed at 10 connections [CODE].
- Consequences to test: queue-wait floor ≈ poll interval (I-6); idle database load proportional to slots; poll-cap mismatch.
- Candidate remedies, **each requiring evidence before adoption**: (a) a notification-based wake-up so a new job wakes a waiting claim (E-5); (b) one multiplexed long-poll per process serving all free slots, optionally returning several jobs (E-6); (c) simply documenting that fewer, busier slots are preferable and raising the poll cap relationship. None is assumed beneficial.

### 5.4 Timeouts, retries and cancellation semantics

- **Timeout** = maximum wall time for one attempt, from `attempt_started_at`. At expiry the handler context is cancelled; the attempt is a retryable timeout failure. The lease and the timeout are independent: a lease is renewed for as long as the worker is alive, so long jobs need a long timeout and rely on renewal, not a long lease.
- **Retry**: attempts are counted at claim; the database decides requeue versus terminal from persisted counters; backoff is server-computed from the type's policy; `Permanent` ends retries immediately.
- **Cancellation** is cooperative for running work: Forge cannot kill a handler, only tell the runtime; a handler that ignores `ctx` keeps its slot (existing, documented behavior). Cancelling a queued job is a state change only.
- **Duplicate delivery** remains possible (lost responses, lease expiry): handlers must be idempotent on `Job.ID`. No setting in this document weakens or strengthens that contract; submission idempotency keys address *duplicate submissions*, a different problem.

---

## 6. Examples of developer-controlled scaling

Illustrative numbers below are **sizing arithmetic, not measurements or promises**. Proposed APIs are marked.

### 6.1 Start small and conservative

Situation: a team begins background email and report jobs; load is unknown.

1. **Submit** (proposed client; today: plain HTTP): jobs of type `email.send` with default options. Nothing else to configure.
2. **Server policy** (proposed file, or today's env): global `max_pending` 1000; `email.send` default timeout 20 s.
3. **Handler**: registered once with `Handle`; written to be idempotent on `job.ID`.
4. **Runtime**: `Concurrency: 2`, one worker process, default poll wait and grace.
5. **Scaling**: one instance. Resource assumption documented: I/O-bound, small memory.
6. **Observe** (proposed): queue depth, oldest queued age, busy slots. Healthy if oldest age stays within the team's chosen target (D-7) and busy slots are below ~80%.

### 6.2 Increase concurrency within each instance

Trigger: oldest queued age rising, busy slots at 100%, **but** the worker host and downstream systems have headroom.

1. Raise `Concurrency` from 2 to 8 in the worker's configuration (environment or code): no handler change, no server change. Restart the worker with SIGTERM + grace.
2. Guardrails: handler is concurrency-safe; downstream connections ≤ budget (8 slots × connections per handler); the poll relationship holds (total slots ≤ server poll cap or documented multiplexing).
3. Isolate a slow type: `w.Handle("report.render", renderReport, MaxConcurrent(2))` (proposed) so slow reports cannot occupy all 8 slots.
4. Confirm: queue wait p95 falls; busy ratio stays <~80%; database CPU and connection counts stay inside limits. If queue wait stops improving, the limit is elsewhere (downstream, the poll floor, or Forge).

### 6.3 Add more worker instances

Trigger: one host is CPU- or network-saturated, or availability across hosts is wanted.

1. The operator raises the replica count from 1 to 3 with their deployment tool (the only lever that creates instances). Each replica has its own `Concurrency` setting; total slots = 3 × 8.
2. Termination grace ≥ `ShutdownGrace` + margin; start new before stopping old on deploys.
3. Check the server poll cap against total idle slots (24 here): fine at the default 64; at 10 replicas × 8 slots it is not.
4. Downstream budget: 24 slots × connections per handler (ForgeCart reached 93 connections at its highest benchmark concurrency).
5. Evidence of benefit: a worker-scaling run comparable to ForgeCart's `worker-scaling` scenario, where drain rate rose 389 → 619 → 662 → 601 jobs/s for 1, 4, 8 and 16 slots per type [MEASURED] and efficiency fell to ~10% at 16 slots. More instances do **not** scale linearly; the limiting component (database, Forge, downstream) must be identified (E-1, E-2).

### 6.4 Separation of concerns, side by side

| Concern | Who | Where | Changing it requires |
|---|---|---|---|
| Submitting a job | Application code | `client.Submit(...)` | nothing else |
| Registering and running a handler | Handler author | `w.Handle(type, fn)` | a handler change only for business logic |
| Configuring one worker process | Operator/developer | `workerruntime.Config` / env | a restart of that process |
| Server-side queue limits | Type owner/operator | server policy file | a Forge restart (v1) |
| Number and size of worker instances | Operator | deployment tool | the tool's own change process |

---

## 7. Recommended v1 scope and deferred capabilities

Each row: recommendation, alternatives, implementation cost (S small, M medium, L large; relative to current code), operational consequence, and the evidence needed.

### 7.1 Decisions

| Question | Recommended v1 default | Alternatives | Cost | Operational consequence | Evidence needed |
|---|---|---|---|---|---|
| **One queue or many?** (D-13) | Many, but a **queue is simply a job type**: no new entity | (a) One shared queue only. (b) Named queues separate from types | recommended: S (type already routes); (b): M–L (column, registry, claim changes, config) | Isolation by dedicated pools per type; queue names equal type names | Owner use cases that need two queues for one type, or several types per queue |
| **Pending limit: global or per queue?** (D-16) | Global (exists) **plus** optional per-type limits | Global only | M (type-aware counting; index per E-3/E-4) | Prevents one type exhausting admission; one more setting to choose per type | E-3, E-4 results; a demonstrated noisy-neighbor case (section 9) |
| **Priorities** (D-18) | **None**; FIFO; use dedicated pools | (a) 2–3 fixed levels. (b) Arbitrary integer priority | (a): M–L (index change, starvation rules); (b): L | Simpler claim path and reasoning; urgent work needs its own capacity | A named use case dedicated pools cannot satisfy |
| **Worker concurrency limits: global or per queue?** | Per process (exists) + **per type per process** (SDK-only) | Add a deployment-wide per-type running cap on the server | S–M; server cap: L | Per-process isolation without coordination | Noisy-neighbor scenario results |
| **Config model: runtime/static vs dynamic APIs?** (D-14) | **Static, versioned server file + SDK options**, restart to apply; read-only `GET /v1/config` | Database-stored policy with a management API | static: S–M; dynamic: L (authz, audit, validation, in-flight semantics) | Simple, reviewable in version control; changes need a restart | Evidence developers must change limits without restarts; depends on D-2 |
| **Scaling: manual vs autoscaling integration?** (D-23) | **Manual**; publish the signals | Autoscaler adapter | manual: S (docs + metrics); autoscale: L | Developers use their orchestrator's controls | Requested use case; metrics proven stable first |
| **Per-job options** (D-15) | `timeout` and `max_attempts` bounded by type policy; `delay` optional | None (type-level only) | M (request model, migration for per-job timeout, validation) | One more thing for submitters to get wrong; mitigated by rejection of invalid values | Owner confirmation that per-job variation is needed |
| **Submission idempotency** (D-11, requirements) | **Include** | None | M (schema: key + fingerprint, unique constraint, retention) | Lets submitters retry safely under backpressure | Contract validated by the idempotency scenario |
| **Cancellation** (D-17) | Optional / v1.1 | Include in v1 | M–L (new state, CHECK, cooperative running-job protocol) | A new terminal state touches every consumer | Named use case |
| **Queue status and metrics** | **Include** (minimum set in 3.7) | Logs only | M | Enables every tuning decision | none: it is the prerequisite |
| **Batch / multiplexed claim** (D-21) | **Defer** pending E-6 | Build now | M–L (protocol + store) | Possible polling-load reduction | E-5/E-6 |

### 7.2 Required for v1 versus optional

**Required (small, coherent set):** accurate backpressure contract (`Retry-After`, stable error shape, uncharged retry guidance); queue status and the minimum metrics; per-job `timeout` and `max_attempts` within type policy, plus configurable type timeouts (unblocks the 30 s limit; ties to D-3); submission idempotency key; type-level (per-type) pending limits **if** experiments confirm the noisy-neighbor problem; `MaxConcurrent` per type per process; `Worker.Stats()`; documented deployment/scaling guidance; strict, versioned server configuration; retention (needed by requirements D-6).

**Optional or future:** cancellation; delayed jobs via API; named queues distinct from types; priorities; dynamic management APIs and server-side per-type running caps; batch or multiplexed claim (until measured); notification-based wake-up (until measured); autoscaling adapter; worker registry; per-tenant quotas and fairness (D-2 option B); recurring schedules; dashboard.

**Explicitly not proposed:** developer-supplied backoff per job, developer control of lease timing, any claim token in a public interface, any ForgeCart-specific field or default in Forge.

---

## 8. PostgreSQL versus broker: decision criteria

This design **does not select a broker** and adds no infrastructure. It defines what must be shown, and in what order, to justify one. It builds on the criteria in `forge-v1-requirements.md` section 6.

### 8.1 How the developer controls map to each design

| Control | PostgreSQL-backed (current) | PostgreSQL + broker (e.g., RabbitMQ) *(claims about brokers are [UNVERIFIED]; verify in the broker's official documentation at evaluation time)* |
|---|---|---|
| Queue capacity / backpressure | Admission count in the submit transaction: exact, but cost depends on depth (I-3) | The authoritative state stays in PostgreSQL, so capacity must **still** be enforced at submission in the same transaction; a broker's own length limits would sit *downstream* of the outbox and could disagree with PostgreSQL's count |
| Job state consistency | One source of truth; delivery intent and state change are one transaction | Two stores: needs a transactional outbox and a relay; states can diverge during relay failure; the relay is an extra failure domain. **Measured analogue in this repo:** ForgeCart's own outbox relay added p50 2.6 s / p95 3.9 s before Forge saw a job, and held 99.7% of the soak's backlog (I-7, I-8); a tuned relay may do far better, which is itself an experiment |
| Duplicate delivery / retries | At-least-once; fencing by generation; retries scheduled in the table | Broker redelivery adds a second duplicate source; acknowledgements must be coordinated with the PostgreSQL fence; retry/backoff either duplicated in the broker or kept in PostgreSQL |
| Queue isolation, priorities | Per type via worker pools; priorities need index and policy work | Named queues and priorities are natural broker features; the cost is mapping them to Forge's type/policy model |
| Worker scaling | Claim polling per slot (I-5) unless multiplexed/notified | Consumers with prefetch; push-style delivery lowers idle cost; worker-API/lease/fencing semantics still required for remote developer-run workers |
| Durability / recovery | One database to back up and fail over | Two systems: broker persistence, broker restart behavior, and reconciliation after partial failures |
| Operational complexity / cost | One component | An extra cluster to run, secure, monitor and pay for; interacts with the $0 database rule only for the database, not the broker (`CLAUDE.md` §3A) |
| Forge-specific work still required | Admission, claim, fencing, leases, history | All of it **plus** the outbox, relay, reconciliation and broker credentials |

### 8.2 Required experiments (in dependency order)

All run on a documented deployment with the corrected harness metadata, **interleaved** for any comparison, and reported with their spread. None assumes a result.

| ID | Experiment | Question | Primary metrics | Informs |
|---|---|---|---|---|
| **E-1** | Forge-isolated baseline: direct `POST /v1/jobs` (open-loop), SDK workers with synthetic handlers of configurable duration | What can Forge sustain with no application in the loop? | sustained accepted jobs/s, submit p95/p99, queue wait p95/p99, completion rate, DB CPU/connections | every target; first gate |
| **E-2** | Resource attribution: vary Forge DB CPU allowance, pool size, replica count | Which component saturates first? | throughput vs allowance; CPU per component | scale-out guidance |
| **E-3** | Admission cost vs depth: submissions at queued depths of e.g. 0, 1k, 10k, 100k (tooling-defined) | Does the global-lock `COUNT` limit throughput or latency at depth? | submit latency, throughput, lock wait | per-type limit design; broker necessity |
| **E-4** | Type isolation at depth: a large backlog of an unserved type while another type's workers claim | Does the claim scan past unserved types? (`EXPLAIN` plus timing) | claim latency vs backlog; plan | index design; isolation claim |
| **E-5** | Claim wake-up: polling vs notification-based wake-up (prototype outside the product path) | What are the queue-wait floor and DB load for each? | queue-wait p50/p95, idle DB statements/s | O4 latency classes |
| **E-6** | Claim multiplexing/batching: per-slot vs per-process claims, batch of 1/N | Does it reduce idle load and the poll-cap problem without hurting fairness? | statements/s, queue wait, correctness under fencing | D-21 |
| **E-7** | Retention and bloat: long soak with heartbeats and completed jobs, with and without cleanup | Do table/index bloat or storage growth limit steady state? | table/index size, autovacuum behavior, claim latency over time | retention design, hosting tier |
| **E-8** | Fault matrix under E-1 load: worker kill, Forge kill, database outage/restart (Forge's own database) | Recovery time and loss under real load | lost jobs (must be 0), time to drain, stuck `running` | O8/O9, G-F |
| **E-9** | Metrics cost: windowed percentile queries from job rows vs exported events | Can server-side p95/p99 be computed without hurting claims? | query cost, impact on claim/submit latency | D-20 |
| **E-10 (conditional)** | Broker prototype: same E-1 workload and fault matrix, outbox + relay + broker consumers, same hardware | Does it meet a **specific failed target** that E-3 to E-6 could not fix? | the same metrics, plus operational cost rubric | architecture gate (G-A) |

### 8.3 Decision rule

1. Complete E-1; adopt no change if approved targets are met with headroom and the fault matrix passes.
2. If a target fails, identify the limiting component (E-2) and try the PostgreSQL-side remedies (E-3 to E-7) first, one at a time.
3. Only if a **named** target still fails, and the owner authorizes (D-12), run E-10 against the same criteria. A broker is chosen only if it meets that target **and** its operational cost and coordination complexity are accepted.
4. Record the result as a new decision record with the measurements attached. Until then, the broker question stays open and no broker is added.

---

## 9. ForgeCart validation and acceptance criteria

ForgeCart's role: a realistic but narrow workload (five short, database-light job types, one outbox relay). Controls that need long, CPU-bound, noisy or mixed workloads are validated primarily with the Forge-isolated benchmark (E-1) and synthetic handlers; ForgeCart supplies end-to-end realism for the rest. **No local single-host result is a production capacity statement.** Metadata and interleaving rules from the corrected harness apply to every run.

### 9.1 Mapping

| Control | Existing coverage [MEASURED] | Gap | Proposed validation |
|---|---|---|---|
| Worker concurrency per process | `worker-scaling` varies replicas × slots and reports background drain rate (389/619/662/601 jobs/s, `fullpost2`) | Measured with workers started after publication; no concurrent arrival; single handler class | **S-1** `concurrency-under-load`: vary `Concurrency` with arrivals ongoing; record queue-wait p95/p99, busy ratio, DB load. Acceptance: p95 queue wait is non-increasing until the limiting resource saturates, and the saturating resource is identified |
| Replica scaling | same scenario | no Forge-isolated or mixed-class view; efficiency falls to ~10% at 16 slots with limiter unattributed | **S-2** replica sweep within E-1/E-2, with component CPU attribution; acceptance: reported gain and efficiency per step, limiter named |
| Queue capacity / full-queue behavior | **None**: the harness raises the cap to 50,000 | Default cap never hit; ForgeCart's dispatcher behavior at `queue_full` unobserved | **S-3** `queue-full`: set `max_pending` low (e.g., a few hundred), drive submissions above drain rate for a bounded period. Acceptance: exact bound holds (queued count ≤ cap + in-flight retries); every rejected submission gets the documented response; **zero accepted jobs lost**; recovery to normal acceptance after drain; no mis-count under concurrency |
| Backpressure / rejected submissions | none | the uncharged-retry contract (I-1) is untested | **S-4** `backpressure-end-to-end`: with S-3 conditions and ForgeCart's dispatcher, assert **no event is dead-lettered by queue-full alone**, outbox backlog grows and then drains, rejection and retry counts reconcile |
| Config correctness | none | no way to read effective config | **S-5** `config-validation`: invalid server config and invalid per-job options are rejected with specific errors; effective config (`/v1/config`) equals what was configured; omitted fields reproduce current behavior (golden comparison with a baseline run) |
| Throughput and p95/p99 changes from a setting | `ramp`, `saturation`, `baseline` for front-end load; `pipeline` for stage timings | settings are not varied systematically | **S-6** one-factor sweeps (slots, poll interval, batch/multiplex if built) with interleaved controls; acceptance: reported effect with spread vs the documented ±noise (≥9 repetitions per arm for A/B) |
| Sustained backlog growth and drain | `soak`, `burst` (but backlog mostly in ForgeCart's outbox) | Forge-side depth/age not observable | repeat with queue-status metrics (proposed `/v1/queues`) to separate outbox backlog from Forge backlog; acceptance: queue age and depth reported from Forge, reconciling with ForgeCart's counts |
| Retries, duplicate delivery, worker recovery | `recovery` (6 fault tests), `idempotency` (60/60 orders, 40 duplicate deliveries, zero duplicate effects) | Forge's own DB never faulted; no retry-storm or poison-job test; defaults not used for lease timings | **S-7** `forge-db-fault` (database restart under load), **S-8** `poison-and-storm` (permanent failures, retry-heavy type): acceptance: zero lost jobs, bounded retries per type, no starvation of healthy types |
| Timeouts / cancellation | none (handlers are fast) | deadline and cancellation untested | **S-9** `slow-handler`: handlers with configurable duration beyond and within the deadline; acceptance: timeouts classified retryable, server-enforced deadline refuses late renewals (if built), long jobs survive via renewal |
| Isolation between workloads | none | no noisy-neighbor test | **S-10** `noisy-neighbor`: flood one type (slow or numerous) while measuring the others' queue wait and submission acceptance, with and without per-type limits and per-type concurrency; acceptance: the quiet types' queue-wait p95 stays within a stated multiple of its isolated baseline, and per-type limits confine rejections to the flooded type |
| Graceful shutdown / rolling restart | `recovery` graceful-worker-shutdown (3.0 s drain) | SDK path under load, rolling replacement | **S-11** rolling restart under load; acceptance: 0 lost jobs; attempts refunded only for cooperative releases |

### 9.2 Acceptance criteria for this design (what "validated" will mean)

1. **Correctness:** zero lost jobs and zero stale overwrites in all fault scenarios, three repetitions each; all integrity checks pass; configuration is rejected or accepted exactly as specified.
2. **Backpressure:** at the configured limit, the response is the documented one, no accepted job is lost, and the reference dispatcher does not dead-letter events solely because of queue-full.
3. **Controls have measurable effect:** each exposed control changes its target metric in the documented direction in at least one scenario, or the control is removed from v1 (no knob without evidence).
4. **Isolation:** with per-type limits and per-type concurrency enabled, a flooded type does not push other types' queue-wait p95 beyond the approved bound (bound requires owner approval).
5. **Scaling guidance is evidence-based:** the scaling documentation cites the measured scaling curve and its limiting component, with the measurement conditions.
6. **Reporting:** results use the corrected harness's definitions, include metadata, and state that they are single-host measurements.

Numeric bounds (for example "p95 queue wait of other types stays within X× baseline") are **not set here** **[DECISION D-24]**.

---

## 10. Open decisions requiring owner approval

New decisions here continue the numbering of `forge-v1-requirements.md` (D-1 to D-12). Recommendations are provisional.

| ID | Decision | Options | Recommended |
|---|---|---|---|
| **D-13** | Queue model for v1 | (a) one shared queue; (b) queue = job type; (c) named queues independent of types | **(b)** |
| **D-14** | Where limits and policies live | (a) static server file + restart; (b) database + management API | **(a)** for D-2 option A; (b) only with multi-tenancy |
| **D-15** | Per-job options in v1 | (a) none; (b) `timeout` and `max_attempts` bounded by type policy; (c) b + `delay` | **(b)** |
| **D-16** | Per-type pending limits in v1 | (a) global only; (b) global + per-type | **(b) if E-3/E-4 or S-10 show a noisy-neighbor problem; otherwise (a)** |
| **D-17** | Cancellation in v1 | (a) none; (b) queued jobs only; (c) queued + cooperative running | **(a) or (b)**; (c) later |
| **D-18** | Priorities in v1 | (a) none; (b) fixed levels | **(a)** |
| **D-19** | Queue-full response | (a) keep `503`, add `Retry-After` and structured fields; (b) change to `429` | **(a)** (no behavior break), with uncharged-retry guidance for submitters |
| **D-20** | Metrics exposition | (a) JSON endpoints only; (b) plus Prometheus text hand-written; (c) plus an adopted client library | decide after the §3A build-versus-adopt check; **provisionally (a) then (b)** |
| **D-21** | Claim efficiency work (notify/multiplex/batch) | (a) not in v1; (b) investigate after E-5/E-6 | **(b)**, build only if evidence supports it |
| **D-22** | Environment loader for worker settings and its variable names | (a) none, document patterns; (b) `ConfigFromEnv` with a distinct prefix | **(b)**; naming must avoid the server's `FORGE_WORKER_*` variables |
| **D-23** | Autoscaling in v1 | (a) out; (b) signals only; (c) adapter | **(b)** |
| **D-24** | Numeric isolation and scaling-efficiency bounds for acceptance | owner supplies, or options after E-1 | after E-1 |

Prerequisite decisions from the requirements document still apply: D-1 (reference workload), D-2 (tenancy), D-3 (maximum job duration), D-4 (budget/tier), D-5 (recovery objectives), D-6 (retention), D-7 (latency classes), D-11 (submission idempotency), D-12 (broker evaluation budget).

---

## 11. Dependency-ordered implementation roadmap

Each item is one small, separately reviewed task, written when reached (project working rule). Dependencies are explicit; nothing here is started.

| # | Task | Depends on | Size | Why now | Validation (project convention: run the real system) |
|---|---|---|---|---|---|
| **0** | Owner decisions: D-1, D-2, D-3, D-7, D-13 to D-19 | — | — | Unblocks scope | review |
| **1** | Refresh `CLAUDE.md` and `task.md` to the implemented state; mark 0001 to 0003 statuses | — | S (docs) | Documentation drift hides what exists | read-through against the code |
| **2** | **Read-only queue status**: store method and `GET /v1/queues` (depth, running, oldest queued age per type, configured global limit, rejection counters since start); no behavior change | 0 (type vs queue naming only) | S–M | Every later measurement and tuning decision needs it; closes the harness's polling gap | curl against the running server; compare with direct SQL counts under load |
| **3** | Backpressure contract: `Retry-After` and structured `queue_full` body (D-19), and the dispatcher/submit-client classification as the reference | 0, 2 | S | Fixes I-1, which can lose events | **S-3/S-4** against a small cap |
| **4** | Forge-isolated benchmark (E-1) with open-loop submission and synthetic handlers; metadata as in the corrected harness | 2 | M (harness) | Turns the unmeasured rows of the requirements into evidence | harness self-checks; interleaved runs |
| **5** | Experiments E-3, E-4, E-5, E-6, E-7 (analysis only; prototypes outside the product path) | 4 | M | Decides per-type limits, indexes, claim efficiency | per experiment criteria |
| **6** | Submit client SDK and submission idempotency key (migration) | 0 (D-11) | M | Safe retries under backpressure | idempotency scenario extended to Forge's API |
| **7** | *(Update 2026-10-22: both halves are implemented as `options.max_attempts` (bounded by the existing 1 to 20 column limit) and `options.timeout_seconds` (bounded by the server maximum `FORGE_MAX_JOB_TIMEOUT`), not by a type policy; the type policy, the config file, `GET /v1/config` and server-side deadline enforcement are not.)* Per-job `timeout`/`max_attempts` within static type policy; server config file with strict validation; `GET /v1/config`; server-enforced deadline on renewal | 0 (D-3, D-14, D-15) | M–L | Unblocks W2/W3 and configuration control | S-5, S-9 |
| **8** | Runtime: `MaxConcurrent` per type, `Stats()`/`Observer`, `ConfigFromEnv` | 0 (D-22) | M | Developer-controlled concurrency and tuning signals | S-1, S-10 |
| **9** | Per-type pending limits and the index/count changes the experiments justify | 5, 0 (D-16) | M | Admission isolation | S-3, S-10 |
| **10** | Retention job and idempotency-key expiry | 0 (D-6), 6 | M | Bounds storage | E-7 soak |
| **11** | Metrics exposition (D-20) with documented definitions | 2, 4 | M | Operability gate | alert drills (requirements G-D) |
| **12** | Scaling guide and deployment examples, validated by S-1/S-2/S-11 | 8, 11 | S–M | Delivers the "developer controls scaling" promise | the scenarios themselves |
| **13** | Architecture gate: decision record using E-1 to E-9 results (broker prototype E-10 only if authorized and a target fails) | 5, 12 | M | Closes the PostgreSQL-versus-broker question | decision record, owner sign-off |
| **Later** | Cancellation, delayed-job API, named queues, priorities, management API, autoscaling adapter, worker registry, multiplexed/batch claim (if justified), multi-tenancy | owner decisions and evidence | — | Out of v1 unless promoted | — |

**Recommended next small task: item 2 (read-only queue status).** It adds no behavior, has no schema change, is backward compatible, and unlocks items 3, 4, 11 and every tuning decision in this design.
