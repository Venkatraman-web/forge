# Forge v1 — Product and Performance Requirements

Status: **proposed, awaiting owner review.** Date: 2026-10-21. Scope: requirements only. No application code, SDK, harness, scenario, dependency or benchmark result was changed to produce this document, and no benchmark was run for it.

This document sits beside the decision records in [docs/decisions/](decisions/) (0001 execution architecture, 0002 remote worker API, 0003 worker runtime). Those are design records; this one states what Forge v1 must do and how we will know. It does not choose between PostgreSQL-only queueing and a broker (section 6 defines how that choice will be made).

## 0. How to read this document

### Evidence tags

Every statement that matters carries one of these tags.

| Tag | Meaning |
|---|---|
| **[CODE]** | Verified by reading the repository source on 2026-10-21 (file named). |
| **[MEASURED]** | A number from a recorded run, with run ID and conditions. Never a guarantee. |
| **[INFERENCE]** | A conclusion drawn from measured or code facts; stated reasoning, not directly tested. |
| **[ASSUMPTION]** | Something we are taking as true to proceed; the owner should confirm. |
| **[UNVERIFIED]** | Not checked, or not checkable with current evidence. |
| **[PROVISIONAL]** | A recommended target or choice, subject to the gates in section 7. |
| **[DECISION D-n]** | Requires owner approval. Options and trade-offs are in section 8. |

### Target labels (section 4)

**S** = supported by existing measurements; **P** = provisional recommendation; **D** = decision requiring owner approval. A target may carry more than one label (for example "baseline S, target D").

### Evidence base and its limits

- **Code:** `cmd/forge`, `internal/{api,workerapi,worker,store,jobs,handlers}`, `sdk/{workerclient,workerruntime}`, `migrations/`, `compose.yaml`, plus the ForgeCart example under `forgecart/`.
- **Design records:** `docs/decisions/0001`–`0003` (all marked "proposed, awaiting review"; their content is largely implemented, see 3.1).
- **Benchmarks:** the ForgeCart load-test suite, full-profile run `fullpost2` (12 scenarios, 896 integrity checks, all passed) in `forgecart/test_files/results/fullpost2/`, plus `fullpost1`, `final003`, the audit `audits/audit-fullpost1-vs-final003.md` and the A/B result `audits/ab-payment-fix-benchmark.md`.
- **Project rules:** `CLAUDE.md` (build-versus-adopt policy; database hosting must cost $0 unless the owner approves; no claim of exactly-once, high availability or production readiness without evidence).

**Limits of the benchmark evidence (these bound every "S" label below):**

1. One host (12 logical CPUs, 14.5 GB), with the load generator, Forge, ForgeCart, dispatcher, workers and two PostgreSQL containers (Docker Desktop VM, tmpfs storage) competing for the same CPUs. [MEASURED context, `environment.json`]
2. The suite measures the **ForgeCart pipeline**, not Forge in isolation. See 4.1: in the soak, almost all of the reported backlog was upstream of Forge. **No existing benchmark isolates Forge's submission (`POST /v1/jobs`), claim, or completion capacity.**
3. The benchmark sandbox uses non-default Forge settings: `FORGE_MAX_QUEUED=50000` (default 1000) and `FORGE_WORKER_MAX_POLLS=512` (default 64). [CODE `cmd/forge/main.go`; MEASURED `sandbox_config`] Results do not describe a default deployment.
4. Throughput varied by up to about 55% between sessions for identical code (baseline 121.8, 164.3, 189.8 workflows/s in `final003`, `fullpost1`, `fullpost2`). A/B testing showed the payment fix was not the cause; the cause is unresolved (see the audits). Single-run numbers cannot support a target on their own.
5. Handlers are trivial (about 25 ms median execution, database-light). CPU-heavy, I/O-heavy and long handlers have never been benchmarked.

---

## 1. Product scope

### 1.1 What Forge is responsible for

Source: `CLAUDE.md` §1 and §4, `docs/decisions/0001` §3 and §7. The intended product is a shared platform where a client submits a job, Forge queues it durably, a worker invokes a registered handler, and the client retrieves status and result. **Whether v1 serves one organization or many independent developers is an open decision (D-2)**; the responsibilities below hold either way.

Forge owns:

- **Accepting and validating submissions** (type allowlist, JSON validity, size bounds) and refusing work it cannot hold (bounded backlog).
- **Durable job state and the job state machine**: `queued → running → succeeded | failed`, with attempts, leases and a fencing token (`claim_generation`).
- **Delivery to workers** with at-least-once semantics, safe concurrent claiming, retries with backoff and jitter, lease renewal, recovery of abandoned work, and stale-worker protection (fenced writes).
- **A worker API and Go SDK** so a developer can run handlers in their own infrastructure without database access.
- **Job inspection** (status, timing, result, safe error code).
- **Operating the platform**: configuration, observability, backups, upgrades (section 5).
- *(if D-2 selects multi-tenant)* authentication, authorization, tenant isolation and quotas.

### 1.2 What the developer's application is responsible for

- **The handler's business logic** and its correctness. Forge "does not invent a task's business logic" (`CLAUDE.md` §12).
- **Idempotency of side effects.** Forge delivers at least once and cannot fence external side effects. The stable key is the job ID. [CODE `sdk/workerruntime/runtime.go` package comment] ForgeCart shows one pattern: an outbox event ID inside the job input, and a unique constraint in the handler's own table.
- **Deciding when to submit**, including avoiding duplicate submissions if exactly one job per business event is required (single submissions can carry an `Idempotency-Key` since 2026-10-23; batches cannot).
- **Running and scaling workers** (process count, concurrency, host sizing) in v1's model, and keeping credentials safe.
- **Payload hygiene**: no secrets in job inputs or results; results and errors are treated as untrusted by Forge.
- **Cooperative cancellation**: a handler that ignores its context cannot be stopped and keeps its slot (documented runtime behavior).

### 1.3 What the Go SDK and worker runtime must make easy

Required of `sdk/workerclient` and `sdk/workerruntime` for v1:

1. Register handlers by job type and run N slots with one `Run` call. [CODE exists: `Handle`, `Config.Concurrency` 1–64]
2. Correct lease, heartbeat, watchdog and fencing behavior without the developer writing any of it. [CODE exists]
3. Classify failures simply (`Permanent(err)` versus any other error as retryable; panics and timeouts handled). [CODE exists]
4. Graceful shutdown: stop claiming, cancel handlers, wait for a grace period, release cooperative jobs. [CODE exists; the package comment says the final shutdown hardening is "not yet built"]
5. Make idempotency the easy path: the job ID and attempt number available on `Job`, documented patterns, and one example handler that is idempotent under duplicate delivery. [PARTIAL: `Job.ID` exists; no documented pattern outside ForgeCart]
6. A developer can go from nothing to a running handler quickly (gate G-O, section 7).
7. Safe-by-default logging: identifiers and fixed words only, never payloads or handler error text. [CODE exists]

### 1.4 Job execution versus scaling the developer's web application

Forge executes **background work**. It does not scale, host, load-balance or autoscale the developer's web application, and it does not make a slow web request fast; it lets the application take work off the request path. Two separate capacities exist and must never be conflated in reports or promises:

- **Foreground capacity**: how fast the developer's application (for ForgeCart, the checkout and payment API) can accept user requests. In the benchmarks this is "workflow throughput" (about 120–190 workflows/s on one host). It is a property of the **example application**.
- **Background capacity**: how fast Forge accepts jobs, queues them, and workers complete them. This is what Forge's requirements concern.

The ForgeCart workflow numbers are **not** Forge capacity. [INFERENCE from 4.1] Forge's job-acceptance endpoint was loaded only indirectly through ForgeCart's dispatcher.

### 1.5 Explicitly out of scope for v1

Documented as future phases in `CLAUDE.md` §12 or excluded by `task.md`, unless marked *proposed*:

- Execution of untrusted or developer-uploaded code, container-based handler deployment, and multi-tenant sandboxing of handler code (needs its own security model).
- Autoscaling, Kubernetes, multi-region and cloud-provider-specific deployment automation.
- Workflows/DAGs, fan-out/fan-in orchestration, cron-style recurring schedules. *(proposed; the schema has `next_run_at` but there is no scheduling API)*
- SDKs other than Go. *(proposed; D-9)*
- A developer dashboard and UI. *(CLAUDE.md lists it as a later phase; D-8 asks whether a minimal status view is required for v1)*
- Exactly-once execution, and any claim of high availability or production readiness without gate evidence. (`CLAUDE.md` §3)
- Billing, usage-based pricing, SSO/RBAC for human users. *(proposed)*
- Replacing the developer's own database transactions or message flows; Forge is not a general message broker.

---

## 2. Job workload classes

The system is designed around these classes. ForgeCart is **one** workload (class W1 mixed with W5/W6); it is not the design target.

| ID | Class | Typical shape | What it stresses in Forge | Existing evidence |
|---|---|---|---|---|
| **W1** | Short I/O-bound jobs | 10 ms – 2 s; email, webhook, record update; high count | Submission rate, claim latency and rate, queue-wait, per-job overhead (a claim, heartbeats, a completion) | ForgeCart's five handlers: median ~25 ms, p95 ~135 ms. [MEASURED `fullpost2` pipeline] Only this class has been measured. |
| **W2** | CPU-intensive jobs | 1 s – several minutes; image/report/ML preprocessing | Worker host sizing, concurrency far below slot cap, deadline vs lease interplay, no head-of-line blocking of short jobs | **None.** [UNVERIFIED] |
| **W3** | Long-running jobs | Minutes to hours; exports, batch imports | Lease renewal over long periods, shutdown/deploy during execution, progress visibility, resumability | **None; also a code constraint.** The execution deadline is a fixed 30 s (`executionTimeout`, `cmd/forge/main.go`; enforced by the runtime as `causeDeadline`). A 30-s-plus handler is cancelled and counted as a timeout today. [CODE] |
| **W4** | Bursty submissions | Idle, then a surge (campaign, import, retry storm) | Backlog bound (default `FORGE_MAX_QUEUED` 1000 returns 503), burst absorption, drain time, fairness | Pipeline-level only, with the queue limit raised to 50000: concurrency 5→40 (8×) for 6 s inside a 30 s run; peak backlog 12,523, of which 12,416 were still unpublished in ForgeCart's outbox (about 107 jobs were inside Forge); back to the pre-burst level 49 s after the burst ended. [MEASURED `fullpost2` burst] |
| **W5** | Jobs that fail and retry | Transient dependency errors, rate limits, poison input | Retry budget, backoff with jitter, permanent-failure classification, visibility of attempts, retry-storm control, dead-letter handling | Mechanics validated manually in earlier tasks (0001 §2); ForgeCart recovery tests show retried work completing (`hard-kill-workers`, 20 retry starts). Attempt history is **not exposed** (API shows no attempt count; no attempts table). [CODE, MEASURED] |
| **W6** | Duplicate-delivery-safe jobs | Payments, invoices, notifications, any external effect | Fencing, duplicate submission, at-least-once handlers, no corruption on redelivery | ForgeCart idempotency scenario passed (60/60 orders, 40 duplicate job deliveries, zero duplicate effects, `fullpost2`). [MEASURED] Demonstrates one pattern for one application. |

Additional workload dimensions v1 must state limits for (all **[UNVERIFIED]** today beyond the code limits shown): payload size (API body limit 1 MiB [CODE `internal/api/server.go`]; result limit 256 KiB [CODE `internal/workerapi`]); number of distinct job types per worker (8 per claim [CODE]) and per deployment (32 in the development allowlist [CODE]); job duration (30 s fixed deadline); queue depth; and retention of finished jobs (no policy exists).

**Little's law for sizing** (planning aid, not a measurement): required concurrent worker slots = job rate × mean job duration.

| Job rate | 25 ms jobs | 1 s jobs | 30 s jobs |
|---|---|---|---|
| 10 jobs/s | 0.25 slots | 10 slots | 300 slots |
| 100 jobs/s | 2.5 slots | 100 slots | 3000 slots |

Code limits that bind this: `Concurrency` ≤ 64 per worker process; the server allows 64 simultaneously waiting claim requests by default (`FORGE_WORKER_MAX_POLLS`, beyond which claims get 429), so by default at most about 64 idle slots can wait for work across all workers. [CODE] Neither is a measured capacity limit, but both are configuration limits that v1 must either lift deliberately or document.

---

## 3. Functional requirements

Status values: **Exists** (implemented and, where stated, manually or harness validated), **Partial**, **Missing**, **Unverified**. "Exists" never means "complete for production": a passed scenario demonstrates one behavior under one configuration.

### 3.1 Current state in one paragraph, and documentation drift

[CODE] Forge runs as one process with an HTTP API (`/health`, `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/jobs/{id}`), an optional embedded worker pool, a lease reaper, and (in `FORGE_EXECUTION_MODE=remote`) a separate worker listener with five endpoints (claim, renew, complete, fail, release), authenticated by one shared key. `sdk/workerclient` is the typed client; `sdk/workerruntime` runs multi-slot handlers with heartbeat, watchdog, backoff and shutdown. ForgeCart's dispatcher and five workers run against this in the benchmarks.

**Drift to fix (not changed here):** `CLAUDE.md` §4 still says "there is no worker runtime yet", `task.md` still describes the Phase 1 baseline, and `docs/decisions/0001`–`0003` are headed "proposed, awaiting review" although most of their content is implemented. Requirements below use the code as the source of truth.

### 3.2 Requirement table

| # | Requirement | Existing | Missing / unverified |
|---|---|---|---|
| **F1** | **Job submission** | `POST /v1/jobs` with `type` + JSON `input`; type checked against an allowlist (remote mode: `FORGE_REMOTE_JOB_TYPES`, ≤32, input only checked as valid JSON); `202` with job ID; bounded body (1 MiB); global bounded backlog (default 1000 → `503`), exact under concurrency via an advisory lock. [CODE] | **Missing:** submission idempotency key; per-job `max_attempts`, delay/schedule, priority; batch submit; per-type schema validation (Forge cannot run customer `Validate`); per-submitter quotas. **Concern [INFERENCE]:** the count-and-insert serializes submissions under one advisory lock, which may cap submission rate; never measured. |
| **F2** | **Job inspection** | `GET /v1/jobs/{id}` (status, timestamps, result, safe error code; input never echoed). `GET /v1/jobs` returns the newest 100 only. [CODE] | **Missing:** pagination, filters (status/type/time), attempt count and attempt history, queue depth/age endpoint, completion notification (webhook/callback), result/error retention policy. The harness had to poll the newest-100 list to observe jobs; coverage is only complete if polled faster than the creation rate. [MEASURED limitation, `fullpost2` coverage note] |
| **F3** | **Durable job state** | PostgreSQL is the only store; the row is the work item (no separate enqueue step); lifecycle enforced by CHECK constraints; partial indexes for due and running jobs; jobs survive Forge and PostgreSQL restarts (manual validation, earlier tasks). [CODE, 0001 §2] | **Missing:** retention/archival of finished jobs (table grows without bound), partitioning or cleanup, attempt-history table, tested backup/restore, replication/HA. **Unverified:** behavior and query performance at millions of rows. |
| **F4** | **Worker identity and concurrency** | Remote claim by job type (≤8 types/claim), worker ID pattern, long-poll up to 25 s, server-side poll cap (default 64) with `429` + `Retry-After`; SDK `Concurrency` 1–64, claim-error backoff, stop on unrecoverable claim errors. [CODE] | **Missing:** worker registry and liveness view, per-service credentials, per-type concurrency limits, fair claim across submitters (claim order is global FIFO by due time), worker/SDK version compatibility policy. **Unverified:** behavior with hundreds of workers; each waiting claim polls PostgreSQL at `FORGE_POLL_INTERVAL` (default 1 s). |
| **F5** | **Retries** | Per-job attempt budget (default 3, DB range 1–20), exponential backoff base 2 s cap 5 min with ±20% jitter, `Permanent` errors not retried, timeouts and panics retryable, exhausted attempts become terminal `attempts_exhausted`; the database decides requeue versus terminal. [CODE] | **Missing:** per-type or per-job retry policy, manual retry/requeue of failed jobs, dead-letter handling, retry-storm control (global rate limit on retries). |
| **F6** | **Timeouts** | Lease 15 s, heartbeat 5 s, safety margin 10 s (validated at startup); handler deadline **fixed at 30 s**, advisory on the server and enforced by the SDK. [CODE] | **Missing:** configurable deadline per job type or per job (blocks W2/W3 classes). **Unverified:** behavior of deadline versus lease for jobs well beyond 30 s. |
| **F7** | **Cancellation** | Handler context is cancelled on lost ownership, self-fence, deadline and shutdown. [CODE] | **Missing:** cancelling a queued or running job via API; a cancelled terminal state. |
| **F8** | **Graceful shutdown** | Embedded pool and SDK release cooperative jobs (attempt refunded); non-cooperative handlers are not released and are left to lease recovery; Forge server drains in order (HTTP, workers, DB). [CODE, manual validation] | **Partial:** `workerruntime` states "final shutdown hardening" is not built. **Missing:** rolling-deploy procedure and zero-lost-job drain demonstration for the SDK path under load. |
| **F9** | **Recovery after worker failure** | Lease expiry plus reaper (atomic, safe with several reapers) requeues or fails with `attempts_exhausted`; stale workers cannot write (fence on job ID + status + generation + worker ID). Harness: after SIGKILL of all workers, all work drained (6.6 s with the harness's short lease settings). [CODE, MEASURED `fullpost2` recovery] | **Unverified:** recovery time at default lease (derived worst case about lease 15 s + reaper interval 5 s + backoff 2 s ≈ 22 s, **[INFERENCE]**, not measured); recovery with thousands of simultaneously expired leases. |
| **F10** | **Recovery after database failure** | Workers back off and resume when the database returns; Forge restarts reclaim queued work; earlier tasks exercised database outages manually. [CODE, 0001 §2] The harness paused **ForgeCart's** database container for 3 s and everything drained (3.0 s). [MEASURED] | **Unverified:** the harness has **never** faulted **Forge's own** database; no RTO/RPO is defined; no HA, failover, point-in-time recovery or tested restore. The API's behavior during a database outage (error codes, latency) is not measured. |
| **F11** | **Delivery guarantee** | At-least-once, stated explicitly; no exactly-once claim anywhere; fencing prevents stale-worker overwrites in Forge's own rows. [CODE] | **Limit (permanent):** Forge cannot fence external side effects. **Known protocol gap:** outcome reports are not replayable; a repeat of a committed complete/fail/release returns `409`, indistinguishable from lost ownership, and a `503` can hide a committed write (documented in 0003). A duplicate-submission defense must be built by the submitter (ForgeCart outbox + `event_id`). |
| **F12** | **Authentication** | Worker API: one shared key (≥32 chars, constant-time comparison, rejected requests logged at a limited rate). Submission/status API: **none**; bound to `127.0.0.1` by default. [CODE] | **Missing:** everything required to expose Forge beyond localhost: per-service worker credentials, submit/read credentials, key rotation and revocation, TLS (assumed terminated outside Forge; **[UNVERIFIED]** whether any deployment path exists), and abuse controls on the submission endpoint. `CLAUDE.md` forbids public exposure until then. |
| **F13** | **Authorization and tenant isolation** | None. There is no tenant, application, service or user concept in code or schema (`jobs` has no such columns). The worker allowlist is global. [CODE] | **Missing (D-2):** tenant/service identity, eligibility filtering on claim and on read, per-tenant quotas and fairness, isolation tests. Cost is highest if retrofitted after v1 data exists. |
| **F14** | **Observability** | Structured logs with a no-payload policy; `/health` (HTTP liveness only, says nothing about queue or workers); the job list; the external harness. [CODE] | **Missing:** metrics (queue depth, oldest queued age, claim latency, attempt counts, lease expirations/reaper actions, API latency/error rate), readiness endpoint, tracing, alerts, dashboards, per-job event/attempt log. Nothing in the code emits metrics today. |
| **F15** | **Operational controls** | Environment-variable configuration with startup validation; Atlas migrations (manual apply); reaper batch/interval tuning. [CODE] | **Missing:** pause/drain a job type, administrative requeue/cancel, runtime configuration changes, documented upgrade/rollback procedure (README notes `atlas migrate down` was not tried), queue-depth limits per type, retention job. |

---

## 4. Performance and reliability objectives

### 4.0 Principles for this section

1. **A target is a promise about a defined workload on a defined deployment.** Every target below states both, or says that the owner must supply them.
2. **Existing benchmarks are a baseline, not an SLO.** They come from one host, one job class, a dispatcher-limited topology (4.1), non-default Forge settings, and sessions that differed by up to 55% for identical code.
3. **No target is final until gate G-P** (section 7) is met on the reference deployment (section 5) by a Forge-isolated benchmark.
4. Numbers marked **P** are offered so the owner can accept, change or reject them; they are not hidden decisions.

### 4.1 What the existing measurements do and do not say

[MEASURED, `fullpost2` unless stated; context: one host, ForgeCart pipeline, 1 dispatcher (batch 50, 200 ms poll), 1 replica × 4 slots per job type, handlers ~25 ms]

| Observation | Value |
|---|---|
| Soak (60 s, concurrency 10): jobs created by ForgeCart | ~580 jobs/s |
| Soak: jobs accepted by Forge during the load (outbox published) | 167 jobs/s (10,050 in 60 s) |
| Soak: effects completed during the load | 166 jobs/s |
| Soak: reported backlog at end of load (5 × paid − effects) | 24,816 jobs |
| Soak: of that backlog, still **unpublished in ForgeCart's outbox** | 24,740 jobs (99.7%) |
| Soak: jobs published to Forge but not yet completed | ~76 jobs |
| Soak: Forge queue wait (created → started), p95 per type | 0.79 – 0.90 s |
| Soak: drain after the load ended | 100.8 s (345 jobs/s) |
| Pipeline (400 workflows, 327 paid): Forge queue wait p50 / p95 / p99 | 0.55–0.77 s / 0.98–1.01 s / 1.03 s (two types shown) |
| Pipeline: Forge execution p50 / p95 | ~25 ms / ~137 ms |
| Pipeline: job created → finished (Forge timestamps) p95 | ~1.03 s |
| Pipeline: outbox event created → accepted by Forge p50 / p95 / p99 | 2.6 s / 3.9 s / 4.0 s |
| Pipeline: outbox event → business effect row p50 / p95 | ~3.1–3.5 s / ~4.2–4.5 s |
| Worker scaling (workers started after ~1.9k jobs are queued), background drain rate, 1×1 / 1×4 / 2×4 / 4×4 | 389 / 619 / 662 / 601 jobs/s (efficiency falls to 10% at 16 slots per type) |
| Forge `GET /v1/jobs` poll latency during the pipeline, p50 / p95 / p99 | 10.5 / 85.9 / 248.2 ms |
| Forge process (soak) | 0.54 cores average, 26 MB RSS |
| Forge PostgreSQL container (soak) | 72% of one core average, 223 MB peak memory |
| Connections to Forge's database, peak | 11 (Forge pool is fixed at 10) |
| Recovery tests, drain after load: stop one worker type / pause all workers / restart dispatcher / graceful worker shutdown / SIGKILL all workers / pause **ForgeCart** DB 3 s | 4.1 / 1.5 / 3.8 / 3.0 / 6.6 / 3.0 s (all fully drained) |
| Integrity | 896/896 checks; zero lost or duplicated effects in every scenario, including idempotency |

**[INFERENCE]** The soak was **dispatcher-limited, not Forge-limited**: the backlog sat in ForgeCart's outbox while Forge's own queue held tens of jobs and its queue wait stayed near 1 s. Whether the limiter between the two systems was the dispatcher's rounds, Forge's `POST /v1/jobs` (including the advisory-lock count-and-insert), or ForgeCart's outbox claim, **is not established**. Consequently:

- **Not established by any existing measurement:** Forge's maximum sustained submission rate; its API p95/p99 under load; its claim throughput and queue-wait with a deep queue; its behavior at the default 1000-job cap; its capacity with heavier handlers; or its database-failure recovery.
- Queue-wait p50 of 0.55–0.77 s is consistent with a polling floor (claims poll PostgreSQL at `FORGE_POLL_INTERVAL`, default 1 s). **[INFERENCE, untested]** A push-style wake-up could remove that floor; whether it is needed depends on the latency target (D-7).
- The earlier audit's observation that deep-queue drain (~250–345 jobs/s) was slower than the worker-scaling drain (~370–660 jobs/s) is explained by this finding (the deep queue was in the outbox, publishing at ~167–345 jobs/s). **[INFERENCE]**

### 4.2 Objectives

Each row: the objective, the existing evidence, the proposed target with its label, and what is needed to confirm it. "Reference load" means the workload class and rate the owner approves in D-1.

| # | Objective | Existing evidence | Proposed target | Label | Needed to confirm |
|---|---|---|---|---|---|
| **O1** | **Sustained job-submission rate** to Forge (accepted jobs/s, W1) | Indirect only: 167 jobs/s accepted over 60 s via the dispatcher (soak); ~320 jobs/s over ~6 s (worker-scaling publication). Forge-isolated rate unknown. | Owner chooses the target rate (D-1). Options: **A** 25 jobs/s; **B** 100 jobs/s; **C** ≥250 jobs/s. A and B are within the indirect baseline; C exceeds it and likely needs design work. **P:** require sustained 2× headroom over the chosen rate in the isolated benchmark. | baseline **S** (indirect); target **D** | G-P: Forge-isolated submission benchmark, ≥10 min sustained, with the advisory-lock path included |
| **O2** | **Completed jobs/s** (workers, W1) | 166 jobs/s during the soak (limited by publication); worker-only drain 372–662 jobs/s with a 1.9k queue. | At least the submission target O1 at steady state, so the queue does not grow. Per-class capacity follows Little's law (section 2). | baseline **S** (W1 only); target **P**; W2/W3 **D** | G-P with W1, W2 and W3 handlers; worker sizing per class |
| **O3** | **API latency** (Forge's own): `POST /v1/jobs`, `GET /v1/jobs/{id}` | None isolated. Only `GET /v1/jobs?limit=100` observed at p50 10.5 / p95 85.9 / p99 248 ms under load, from the harness. (ForgeCart's own API latencies are a different system.) | **P:** submit p95 ≤ 100 ms and p99 ≤ 250 ms; status read p95 ≤ 50 ms and p99 ≤ 150 ms, at the O1 rate, on the reference deployment. Stricter or looser values are D-7. | **P**, strictness **D** | G-P with open-loop load at the O1 rate |
| **O4** | **Queue-wait latency** (created → started) | p95 0.8–1.0 s, p99 ~1.03 s at ~100–170 jobs/s with a near-empty Forge queue (consistent with a ~1 s polling floor). | Define per class (D-7). Options: **interactive** p95 ≤ 2 s; **standard** p95 ≤ 10 s; **batch** p95 ≤ 5 min. **P:** v1 offers "standard" and states interactive only if D-7 requires it (a 1 s polling floor already consumes half of a 2 s budget). | baseline **S**; target **D** | G-P under steady and burst load |
| **O5** | **End-to-end completion latency** (submission → finished) | Forge-side created → finished p95 ~1.03 s (trivial handlers, near-empty queue); ForgeCart adds outbox publication p95 3.9 s (application-specific, not a Forge requirement). | End-to-end p95 = queue-wait target + handler duration; stated per class. No separate number until O4 and the handler classes are decided. | **D** | G-P |
| **O6** | **Maximum acceptable queue age** and backlog growth | Not defined. Soak backlog grew ~350 jobs/s at the ForgeCart level while Forge's queue stayed shallow. | **P:** at the O1 rate, oldest-queued age p99 ≤ the O4 class target, and backlog slope ≈ 0 over a ≥10 min soak (growth ≤ 1% of the submission rate). Alert threshold: oldest queued age > 2× target. Requires queue age to be observable (F14). | **P** | metrics from F14; G-P soak |
| **O7** | **Burst handling and drain** | Pipeline-level burst: 8× concurrency for 6 s, backlog peak 12.5k (99% in ForgeCart's outbox, about 107 jobs inside Forge), back to the pre-burst level 49 s after the burst, with the Forge queue cap raised to 50000. At the default cap (1000 queued) a burst above the drain rate returns `503`. | Owner chooses burst size and window (D-1). Options: **A** 5× for 60 s; **B** 10× for 5 min. **P:** the rest of the system must either absorb the burst within the queue cap (cap sized from burst × rate) or shed it with `503` and a clear `Retry-After`; backlog drained within 5× the burst duration. The default cap of 1000 must be re-derived, not kept by habit. | **D** (size), **P** (shape) | G-P burst test at the final cap |
| **O8** | **Recovery time: worker failure** | SIGKILL of all workers: drained in 6.6 s with short harness leases; default-lease worst case ~22 s is derived, not measured. | **P:** lost-job count 0; abandoned work resumes within lease + reaper interval + recovery backoff (default ≈ 22 s) after the last renewal; attempt consumed. Faster requires shorter leases at higher write cost (to be measured). | baseline **S** (short lease); target **P** | G-F fault matrix with default settings |
| **O9** | **Recovery time: Forge database failure** | Not measured. | Owner chooses RTO/RPO (D-5). Options: **A** best effort, restore from last backup (RPO hours, RTO hours; free-tier compatible); **B** managed database with automated backups/PITR (RPO minutes, RTO tens of minutes; paid); **C** replicated/failover (RPO ≈ 0, RTO minutes; paid, highest ops cost). In all cases: after the database returns, queued work is not lost and workers resume without manual action. | **D** | G-F database-outage test; G-D restore drill |
| **O10** | **Correctness under faults** | 896/896 checks in `fullpost2`; fencing validated manually. | **P:** zero lost jobs, zero jobs stuck `running` beyond lease + margin + reaper interval, zero stale-worker overwrites, across the full fault matrix, repeated ≥3 times. (A correctness target, not a performance one.) | **P** | G-F |
| **O11** | **Resource consumption** | Forge ≈0.5 core / 26 MB at ~170 jobs/s; its PostgreSQL ≈0.7 core / 223 MB at the same load (single host, tmpfs, no disk I/O cost). | **P:** at the O1 rate, Forge ≤ 1 core / 512 MB and the database within the chosen tier's CPU/memory/connection limits with ≥30% headroom; database size growth bounded by a retention policy (D-6). | baseline **S**; target **P** | G-P with disk-backed PostgreSQL |
| **O12** | **Deployment cost** | Not defined. Project rule: database hosting must cost $0 unless the owner approves otherwise (`CLAUDE.md` §3A). | Owner chooses the tier (D-4, section 5). Cost is a constraint on all of O1–O11: a free tier may not meet O9 or O1 at option B/C. | **D** | provider terms verified at selection time |

### 4.3 Targets that must not be stated yet

- Any "jobs per second" capacity claim for Forge: no isolated measurement exists.
- Any availability percentage (for example "99.9%"): there is no HA design, redundancy, or outage history.
- Any claim for CPU-bound or long-running jobs: unmeasured and, for W3, blocked by the 30 s deadline.
- Any tail-latency promise below the polling floor (about 1 s) before the claim wake-up question is settled.

---

## 5. Reference deployment

This section specifies what the v1 performance gates will run on. It is a requirement on the environment, not a hosting recommendation, and **it does not assume paid infrastructure is acceptable**.

### 5.1 Topology (all tiers)

```text
developer app --HTTPS--> [TLS-terminating proxy] --> Forge API listener  (submit/read)
developer worker(s) --HTTPS (outbound only)--> [proxy] --> Forge worker listener (claim/renew/complete/fail/release)
                                                          Forge process (API + worker API + reaper) --> PostgreSQL
```

- **Forge process:** `FORGE_EXECUTION_MODE=remote` (no embedded handlers). The API and worker listeners are separate addresses and must be separate network exposures (0002 §1). Forge's TLS is **[ASSUMPTION]** handled by a reverse proxy or the hosting platform; Forge itself serves plain HTTP today.
- **Replicas:** v1 needs **two Forge processes** to test reaper safety and restart behavior, but **one is sufficient for the baseline**. Several reapers are safe by design. **[UNVERIFIED]** two API/worker-API processes sharing a database under load (claims are safe across processes; the advisory-lock submission path and the poll cap are per process).
- **PostgreSQL:** one primary, version 16, durable storage, TLS to the application if not on a private network. The fixed Forge pool (10 connections) must fit the tier's connection limit with room for migration and operations.
- **Workers:** developer-operated, outbound only. Reference sizing for benchmarks: 2–4 worker processes × 4–16 slots, with the class mix from D-1. The SDK allows 64 slots per process.

### 5.2 Persistent database requirements

| Requirement | Statement | Status |
|---|---|---|
| Durability | `fsync` on, durable volume (the compose file's named volume is for development only; the benchmark sandbox uses tmpfs and so excludes disk cost) | benchmark gap: disk I/O cost unmeasured |
| Backups | Automated, at least daily; restore tested (gate G-D); retention stated | **Missing** |
| Point-in-time recovery | Required only if D-5 selects RPO of minutes | owner decision |
| Capacity | Storage for jobs + results at the retention policy (D-6); connection limit ≥ Forge pool + operations | retention policy **Missing** |
| Migrations | Versioned Atlas SQL, reviewed, applied before deploy; rollback = forward migration | exists; rollback untested |
| Monitoring | Connections, replication lag (if any), disk, CPU, slow queries, table/index bloat on `jobs` | **Missing** (high update rate on the hot table from heartbeats is a known concern, unmeasured) |

### 5.3 Required monitoring and backup capabilities (minimum)

Queue depth and oldest queued age per type; claim rate and latency; job outcomes by type (succeeded, failed, retried, abandoned/reaped); API latency and error rate; worker liveness; lease expirations; database connections, CPU, storage and bloat; alerts for stuck queue, reaper failures, database unreachable, backup failure. None of this exists in code today (F14); until it does, deployment gates G-D cannot pass.

### 5.4 Cost and free-tier versus production-grade

| Tier | Intent | Database | What it can honestly claim | Cost implication |
|---|---|---|---|---|
| **T0 Local development** | Build and benchmark | Docker PostgreSQL | Nothing about production | $0 (existing) |
| **T1 Free-tier pilot** | Demo and early users, if the owner accepts the limits | A hosted PostgreSQL free tier **only after verifying its current terms** | "Functional pilot, no availability or recovery promise" unless the tier provides backups; likely cannot satisfy O9 beyond option A | $0 required by `CLAUDE.md`; **never auto-provision** |
| **T2 Production-grade small** | Real workloads with RPO/RTO | Paid managed PostgreSQL with backups (option B in O9) | Targets from section 4 after G-P | **Recurring cost; requires explicit owner approval (D-4)** |
| **T3 Highly available** | Strict RTO | Replicated database, multiple Forge replicas | Only after an HA design and failover tests exist | Highest; out of scope unless D-5 selects option C |

**What must be verified about any free tier before reliance** (not assumed here): storage cap and what happens at the cap; compute and connection limits against the 10-connection pool plus migrations; idle suspension and cold-start behavior (a suspended database stalls claims and the reaper); backup and restore availability; region and latency to workers; terms for production use; whether exceeding limits causes charges or throttling. Provider terms change, so these must be fetched from current official documentation at selection time and recorded with the date.

---

## 6. Architecture decision criteria

### 6.1 The question

Decide whether v1's job queueing stays **PostgreSQL-backed (the current design: the `jobs` row is the queue, claimed with `FOR UPDATE SKIP LOCKED`)** or adds **PostgreSQL plus a broker such as RabbitMQ** (or another mature option). **This document does not select one.** The record so far:

- `docs/decisions/0001` (proposed) recommends keeping the PostgreSQL engine and adding a remote worker protocol; it compares Google Cloud Tasks and Hatchet, and lists revisit triggers. It also states that earlier design reports for a RabbitMQ outbox existed but are not in the repository and not implemented; this document therefore does **not** rely on them.
- `task.md` Task 17 required a comparison including River, Asynq and RabbitMQ + transactional outbox. Only the 0001 comparison is in the repository. **[UNVERIFIED]** whether the River/Asynq/RabbitMQ comparison was ever recorded; if it was not, it is a gap in the decision record.
- `CLAUDE.md` §3A applies: adopt mature components for commodity infrastructure when that improves reliability, security, maintainability or speed, and do not rebuild a queue engine without comparing credible options.

### 6.2 Preconditions (the choice cannot be made earlier than these)

1. Owner decisions D-1, D-3, D-5 and D-7 (workload, durations, recovery, latency) are made: without a target there is nothing to compare against.
2. A **Forge-isolated benchmark** exists (gate G-B) so each candidate is measured on Forge's own path, not through ForgeCart.
3. F14 minimum metrics exist so queue depth, queue age and claim latency are observable.
4. Known, cheap PostgreSQL-side improvements have been measured first (6.4), so a broker is not adopted to fix something a query or setting would fix.

### 6.3 Candidates and how to compare them

Candidates: **(P0)** PostgreSQL-only as built; **(P1)** PostgreSQL-only with measured tuning (6.4); **(B)** PostgreSQL plus a broker with a transactional outbox (RabbitMQ is the named example); **(F)** a mature PostgreSQL-backed Go job framework (for example River, evaluated against the Forge schema and licence); and the Hatchet proof of concept from 0001 §9 if its trigger conditions are met. Each candidate is assessed with identical inputs.

| Criterion | What to measure or establish | Pass condition (proposed) |
|---|---|---|
| **Throughput** | Sustained submit, claim and complete rate for W1, and for the D-1 class mix, on the reference deployment | Meets the approved O1/O2 targets with ≥2× headroom |
| **Tail latency** | Queue-wait p95/p99 and submit p95/p99 at 50%, 100% and burst load | Within approved O3/O4 targets |
| **Durability** | Any acknowledged job survives process kill, database restart, broker restart (if any), power-loss simulation where possible | Zero acknowledged jobs lost in ≥3 repetitions per fault |
| **Consistency** | Atomicity between accepting a job and recording delivery intent; duplicate delivery rate; fencing of stale workers; behavior of the outbox publisher on crash between commit and publish | No state where a job is accepted but undeliverable, or completed twice without fencing evidence; duplicates handled as documented |
| **Failure recovery** | Time to resume and number of manual steps after: worker kill, Forge kill, database outage, broker outage, network partition between components | Within approved O8/O9; no manual steps for worker and Forge restarts |
| **Operational complexity** | Count of components to run, upgrade, monitor, back up and secure; runbook length; number of distinct failure modes; skills required | Qualitative rubric, scored by the owner with the evidence attached |
| **Cost** | Recurring hosting cost at the reference load for each tier in 5.4; engineering time; free-tier feasibility | Within the approved budget (D-4) |
| **Security** | Added attack surface (credentials, network paths, management UI), tenant isolation implications | No new unauthenticated surface; credentials per service |
| **Fit and lock-in** | Compatibility with the GORM/Atlas schema; migration cost; licence; maintenance health; ability to leave | Documented per `CLAUDE.md` §3A questions 1–5 |
| **Forge-owned work** | What Forge must still build under each candidate (the remote protocol, tenancy, fencing, history are Forge's under all of them, per 0001) | Listed explicitly; no duplicate mechanisms |

### 6.4 Tuning ladder to exhaust before adopting a broker (P0 → P1)

Each step is measured individually; none is assumed to help. These are hypotheses, not findings.

1. Claim wake-up: replace or supplement fixed-interval polling with a notification mechanism, and measure queue-wait versus load on the database. (The ~1 s queue-wait floor is the observed motivator.)
2. Per-claim polling load: the cost of 64 waiting claims each polling every second.
3. Heartbeat write cost and `jobs` table bloat/HOT-update behavior under many long jobs.
4. Submission path: the advisory-lock count-and-insert versus alternatives that keep the bound exact.
5. Retention/partitioning of finished jobs to keep the hot indexes small.
6. Batch claiming and connection-pool size (fixed at 10 today).

### 6.5 Decision rule (proposed)

- Keep PostgreSQL-only (P0 or P1) if it meets all approved targets with headroom and the fault matrix passes. The simplicity and single source of truth are the default.
- Consider a broker (B) only if, after 6.4, PostgreSQL-only **fails a specific approved target** (named, with measurements) **and** a broker prototype meets it at an operational cost and price the owner accepts.
- The outcome is recorded as a new decision record (0004+) with the measurements attached, owner sign-off, and the revisit triggers. No broker is introduced before that record exists.

---

## 7. Acceptance gates

Gates are pass/fail. "Evidence" is the artifact that must exist in the repository or a linked immutable result. A gate may not be marked passed on a single run.

### G-F Functional correctness

| ID | Criterion | Measure |
|---|---|---|
| F-1 | Fault matrix: API crash, dispatcher/publisher crash, worker crash mid-job (SIGKILL), worker graceful stop, stale worker (SIGSTOP/CONT), lease expiry, Forge-database outage and restart, duplicate submission, duplicate delivery | Each scenario ≥3 repetitions at default settings: **0 lost jobs, 0 jobs stuck `running` beyond lease + margin + reaper interval, 0 stale overwrites** |
| F-2 | Retry semantics | Permanent failures not retried; exhausted attempts terminal with a visible code; backoff within documented bounds with jitter |
| F-3 | Idempotency demonstration | At least two example handlers (different external-effect patterns) pass a duplicate-delivery test; the runtime documents the pattern |
| F-4 | Behavior at limits | Queue cap, payload cap, poll cap and deadline produce documented, tested responses (`503`/`429`/`413`/timeout classification) |
| F-5 | Shutdown | SDK path: rolling restart of workers under load loses 0 jobs and refunds attempts only for cooperative releases |
| F-6 | Workload classes | W1, W2 and W3 each have a passing functional test (W3 requires the deadline decision D-3) |
| Evidence | Immutable result directories plus a short report per scenario; criteria not weakened after seeing results |

### G-B Reproducible benchmarking

| ID | Criterion |
|---|---|
| B-1 | A **Forge-isolated** benchmark exists (direct submission to `POST /v1/jobs`, workers via the SDK, no application in the loop) with configurable job duration, payload size and type mix |
| B-2 | Every run records the metadata the corrected harness records (boot time, load average, governor, container engine/VM size, source-tree hash, binary hashes) and marks unavailable fields explicitly |
| B-3 | Comparisons between code versions use **interleaved A/B** with ≥9 repetitions per arm; cross-session comparisons are labelled non-comparable (audits showed up to 55% drift for identical code) |
| B-4 | Each reported metric has a documented definition, unit and interval; run-to-run spread is reported; self-checks reconcile reported figures with raw data |
| B-5 | Open-loop (fixed arrival rate) load is available, because the existing closed-loop generator hides queueing |
| B-6 | The sandbox uses disk-backed PostgreSQL at least for the G-P runs, and benchmark settings equal the deployment defaults or are listed as deviations |
| B-7 | Raw results are immutable and archived; reports are regenerated from raw data, never edited |

### G-P Performance validation

| ID | Criterion |
|---|---|
| P-1 | At the approved O1 rate, a ≥10-minute sustained run meets O2, O3, O4 and O6 targets; headroom run at 2× O1 shows degradation shape (no cliff) |
| P-2 | Burst test at the approved size meets O7 at the final queue cap |
| P-3 | Recovery times meet O8 (default settings) and O9 (database outage) |
| P-4 | Resource use meets O11 on the reference deployment |
| P-5 | A ≥1-hour soak at the O1 rate with a retention job running: backlog slope ≈ 0, no memory growth in Forge or the SDK, `jobs` table and index size stable |
| P-6 | Each of W1, W2, W3 is measured; targets are stated per class |
| P-7 | Results are reported only as "measured on [deployment]"; no capacity claim outside the measured envelope |

### G-A Architecture selection

| ID | Criterion |
|---|---|
| A-1 | Preconditions in 6.2 are satisfied |
| A-2 | A decision record (0004 or later) compares the candidates on every criterion in 6.3 with attached measurements, and lists what Forge still builds |
| A-3 | The tuning ladder in 6.4 was measured before any broker prototype |
| A-4 | Owner sign-off on the record; revisit triggers stated |
| A-5 | `CLAUDE.md` and `task.md` are refreshed to match the decision and the implemented state |

### G-S Security

| ID | Criterion |
|---|---|
| S-1 | No unauthenticated endpoint is reachable from outside localhost, except any explicitly public health endpoint |
| S-2 | Per-service worker credentials and submit/read credentials (D-2 scope), with rotation and revocation demonstrated |
| S-3 | If multi-tenant (D-2): a tenant cannot read, claim, or affect another tenant's jobs: negative tests for every endpoint and for claim eligibility |
| S-4 | TLS between clients/workers and Forge; secrets never logged; payloads and handler error text never logged (existing policy) verified by log inspection under fault tests |
| S-5 | Abuse controls: request rate limits and payload limits on the submit endpoint; the queue cap cannot be exhausted by one submitter (per-submitter quota) |
| S-6 | Dependency and vulnerability review (Go modules, base images); threat model written and reviewed |
| S-7 | Worker-side code execution of untrusted code is not offered (out of scope) and the documentation says so |

### G-D Deployment and operations

| ID | Criterion |
|---|---|
| D-1 | Deployment is reproducible from documented steps on the reference tier; configuration validated at startup |
| D-2 | Backups run automatically; a **restore drill** recovers the database and Forge resumes work, with measured RPO/RTO matching D-5 |
| D-3 | Metrics, alerts and dashboards for section 5.3 exist and each alert has been triggered once deliberately |
| D-4 | Runbooks: stuck queue, database outage, worker flood, credential leak, bad deploy/rollback; upgrade and migration procedure including rollback tested |
| D-5 | Cost at the reference load is documented for the chosen tier; free-tier terms (if used) recorded with the date they were read |
| D-6 | A retention/cleanup job keeps storage bounded |

### G-O Developer onboarding and v1 release

| ID | Criterion |
|---|---|
| O-1 | A developer who has not seen Forge submits a job and runs a handler from the documentation alone in ≤ the approved time (D-10; proposed 30 minutes) |
| O-2 | SDK documentation states delivery semantics, idempotency expectations, limits (payload, deadline, concurrency), error classes and shutdown behavior; at least one idempotent example |
| O-3 | Versioning and compatibility policy for the API and SDK is written; breaking changes are rare and announced |
| O-4 | Known limitations are listed in the README with no unmeasured claims (no exactly-once, HA or throughput claim beyond gate evidence) |
| O-5 | **v1 release** requires G-F, G-B, G-P, G-A, G-S, G-D all passed (or each failed gate explicitly waived by the owner with the risk written down) and `CLAUDE.md`/`task.md` refreshed |

---

## 8. Decisions requiring owner approval

Each decision lists defensible options and the trade-off. "Recommended" is my provisional recommendation, not a choice made.

| ID | Decision | Options and trade-offs | Recommended (provisional) |
|---|---|---|---|
| **D-1** | **Reference workload**: submission rate, burst size and window, job-duration mix | (A) 25 jobs/s, burst 5× for 60 s: small team, comfortable with current evidence. (B) 100 jobs/s, burst 10× for 5 min: needs the isolated benchmark to show headroom. (C) ≥250 jobs/s: beyond the indirect baseline, likely needs engineering. Also name the class mix (W1–W6). | **B as the planning target**, to be confirmed or reduced by the isolated benchmark; owner may substitute the real expected load |
| **D-2** | **Tenancy and audience for v1** | (A) Single organization, trusted network, authenticated per service: smallest scope. (B) Multi-tenant shared platform for external developers: tenant isolation, quotas and fair claim become v1 requirements and dominate effort. | **A for v1** unless the owner states external developers will share an instance; design schema so B can be added without rewriting job rows. This also decides whether `CLAUDE.md`'s Phase 3 items are in v1 |
| **D-3** | **Maximum job duration in v1** | (A) Keep the fixed 30 s deadline, document it (W3 unsupported). (B) Per-type configurable deadline up to minutes. (C) Up to hours with progress reporting. | **B** if W2/W3 are in D-1; otherwise A, documented |
| **D-4** | **Budget and tier** | T1 free-tier pilot; T2 paid small production; T3 HA. Free tier compatible with `CLAUDE.md`'s $0 database rule but constrains O9/O1. | **T1 for pilot, T2 only on explicit approval**; no provisioning without approval |
| **D-5** | **Recovery objectives for the Forge database** | RPO/RTO options A/B/C in O9 | **A for T1, B for T2**; C only with an HA design |
| **D-6** | **Retention of finished jobs and results** | (A) Keep for N days then delete. (B) Keep metadata longer, drop payloads. (C) Keep forever (not viable unbounded). | **A**, with N chosen by the owner (for example 7 or 30 days) |
| **D-7** | **Latency classes** | Interactive (p95 queue wait ≤ 2 s), standard (≤ 10 s), batch (≤ 5 min). Interactive likely requires removing the ~1 s polling floor. | **Standard + batch for v1**; interactive only if a named use case needs it |
| **D-8** | **Observability depth and a status UI** | (A) Metrics + logs + CLI/curl only. (B) Add a minimal read-only status page. | **A for v1**; dashboard stays a later phase per `CLAUDE.md` |
| **D-9** | **SDK scope** | Go only (current) versus adding another language (more effort, wider audience). | **Go only** |
| **D-10** | **Onboarding target time** | e.g. 15, 30 or 60 minutes from clone to first running handler | **30 minutes** |
| **D-11** | **Submission idempotency and scheduling in v1** | (A) None; submitters use an outbox/own keys (current ForgeCart pattern). (B) Client-supplied idempotency key on submit. (C) B plus delayed/scheduled jobs and cancellation. | **B** (cheap, high value for W6); C deferred unless named |
| **D-12** | **Broker evaluation budget** | Whether to authorize a time-boxed prototype of a broker candidate after the tuning ladder if a target fails, and who signs off | Authorize only conditionally per 6.5 |

---

## 9. Remaining unknowns and risks (not decisions)

- **[UNVERIFIED]** Forge's isolated submission, claim and completion capacity, and its database-failure behavior. The first two benchmarks to build (section 10) address this.
- **[UNVERIFIED]** The cause of the 55% cross-session throughput drift; until understood, every benchmark needs interleaved controls.
- **[RISK]** The `jobs` table receives heartbeat updates and holds all history: bloat and retention behavior are unmeasured.
- **[RISK]** The 10-connection fixed pool, the global advisory lock on submission, and per-claim polling are all design constants with unmeasured effects at higher rates.
- **[RISK]** Documentation drift (`CLAUDE.md`, `task.md`, decision-record status headers) can mislead future work; refresh is part of gate A-5/O-5.
- **[RISK]** Manual-only validation policy limits regression protection; the harness (not unit tests) is the safety net, so G-B is not optional.

## 10. Implications for sequencing (informational, not a plan)

1. Owner decisions D-1 to D-5 and D-7 unblock everything in section 4 and 6.
2. **Highest-value next milestone: a Forge-isolated benchmark plus the minimum observability (queue depth, oldest queued age, claim latency) it needs.** It turns the unmeasured rows of section 4 into evidence, and it is a precondition for the architecture decision.
3. The architecture decision (G-A) follows measurement; security (G-S) scope follows D-2; deployment (G-D) follows D-4/D-5.

No implementation is proposed or started by this document.

## 11. Companion design: developer-controlled configuration and scaling

Added 2026-10-21. The design that turns requirements F1 to F15 and objectives O1 to O12 into a developer-facing configuration model is [decisions/0004-developer-controlled-configuration-and-scaling.md](decisions/0004-developer-controlled-configuration-and-scaling.md) (proposed, design only). It contributes to this document:

- **A capability inventory** classifying every relevant setting (implemented and exposed, implemented internally, partial, absent, unvalidated), with file references (0004 section 2).
- **Integration findings that refine section 3 and 4 above:** `503 queue_full` carries no `Retry-After` and ForgeCart's dispatcher charges it against an 8-attempt budget; the default server poll cap (64) equals the SDK's per-process slot maximum; capacity is a single global number; admission and claim-path costs at depth are unmeasured (0004 section 2.4). The benchmarks never exercised the default queue cap because the harness raises it to 50,000.
- **Open decisions D-13 to D-24** (queue model, where policies live, per-job options, per-type limits, cancellation, priorities, queue-full status, metrics exposition, claim efficiency, environment loader, autoscaling, isolation bounds), continuing the numbering of section 8 (0004 section 10).
- **Experiments E-1 to E-10 and validation scenarios S-1 to S-11** that extend gates G-B and G-P and feed the architecture gate G-A (0004 sections 8 and 9).
- **A dependency-ordered roadmap** whose first small task is a read-only queue-status endpoint (0004 section 11).

Nothing in 0004 changes the targets, labels or gates in this document.
