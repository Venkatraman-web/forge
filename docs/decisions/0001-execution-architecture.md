# Decision 0001 — Execution architecture for a multi-developer Forge

Status: **recommendation followed in practice (note added 2026-10-22).** Option A was carried out: the custom PostgreSQL engine was kept and the remote worker API (0002), the typed client `sdk/workerclient` and the runtime `sdk/workerruntime` (0003) were built on it. No Cloud Tasks, Hatchet, RabbitMQ or other broker was adopted. The repository holds no separate owner sign-off record, and the revisit triggers and the optional Hatchet spike in section 9 remain open; ADR 0004 (section 8) keeps a broker prototype as a possible later step only if measured targets fail and the owner authorises it. The text below is the original analysis and is otherwise unchanged. Date: 2026-10-19. Scope: design only; no code, schema or configuration was changed to produce this document. **Later note:** the in-process ("embedded") execution mode and the built-in `dataset_validate` handler discussed in this record were removed on 2026-10-22; Forge now always runs with customer-run remote workers. References to them below are historical.

## 1. Executive summary

Forge's intended product is a shared platform: developers register applications and services, submit jobs through an authenticated API, and run their own workers, which execute their own handlers and report back. Forge must not contain customers' business logic.

Three options were compared against that product, not just against queue features:

- **A — keep the custom PostgreSQL engine and add a remote worker protocol plus a Go SDK.**
- **B — Google Cloud Tasks pushing to customer HTTP endpoints.**
- **C — Hatchet (hosted or self-hosted) as the execution engine.**

**Recommendation: A**, as the starting point for the next stage, with the explicit revisit triggers in section 9. The deciding reasons:

1. The hard, product-defining parts are tenant isolation, per-service worker authorization, tenant-level fairness/caps, and a single authoritative job history. None of the three options provides these out of the box for *Forge's* customers. Cloud Tasks has no worker concept at all. Hatchet's isolation boundary is its *tenant*, and the hosted plan's tenant limit (10 on pay-as-you-go) is far below "many developers" (sections 4 and 5).
2. Adopting B or C would still require Forge to keep its own PostgreSQL job table (status, history, analytics, tenant quotas), plus a reconciliation layer between that table and the engine. That adds a second source of truth while leaving the correctness-critical piece, fencing stale results into Forge's own rows, to be rebuilt on top of an engine that does not expose a fencing token.
3. The code already written (claims, `claim_generation` fencing, leases, heartbeats, reaper, retries, release) *is* the server side of a remote-worker protocol. Its store methods map one-to-one onto worker API calls. Making it remote is additive work, not a rewrite.

The biggest trade-off: Forge owns a distributed protocol and a PostgreSQL-as-queue whose throughput has never been measured. Section 9 lists the evidence to collect, and a small optional Hatchet proof of concept that could overturn this recommendation.

Confidence: moderate. It rests on the isolation argument, and two Hatchet facts that I could not verify (per-tenant credentials and programmatic tenant creation) would change it. The proof of concept in section 9 targets exactly those.

## 2. Verified current architecture

Everything below was checked against the source in this repository (Go, `internal/store`, `internal/worker`, `internal/handlers`, `cmd/forge/main.go`, `migrations/`). The directory is not a Git repository, so there is no commit history to consult.

**Submission and API.** Four unauthenticated endpoints: `GET /health`, `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/jobs/{id}`. `POST` validates the job `type` against a compile-time handler registry, calls the handler's `Validate`, and inserts a `queued` row through `CreateIfQueueBelow`: count and insert run in one transaction under a PostgreSQL advisory lock, so a global queued-job cap (default 1000) is exact. A full queue gives 503.

**Registry and handlers.** The registry holds Go handlers compiled into the Forge binary. Only `dataset_validate` exists. A job's `type` is a lookup key into that registry; there is no service or tenant concept anywhere in the code (`tenant` does not appear in `internal/store/postgres.go`).

**Schema.** One `jobs` table, defined by the `JobRecord` struct tags and managed by two Atlas migrations. Columns for execution: `attempt`, `max_attempts` (default 3, range 1–20), `claim_generation` (bigint), `next_run_at`, `lease_expires_at`, `worker_id`, `attempt_started_at`. CHECK constraints enforce the lifecycle shape (a queued job has no worker/lease/finish time; a finished job has none either; failed jobs have an error pair). Partial indexes: due-queue `(next_run_at, created_at, id) WHERE status='queued'` and `WHERE status='running'`. `lease_expires_at` is deliberately unindexed so heartbeats stay cheap in-place updates.

**Claiming.** One statement picks the next due queued job (`ORDER BY next_run_at, created_at, id`, `FOR UPDATE SKIP LOCKED`), sets it `running`, increments `attempt` and `claim_generation`, sets `worker_id`, stamps `started_at` (first start only) and the lease, and returns the row. There is **no fairness** across submitters: ordering is global FIFO by due time.

**Fencing.** `Complete`, `Fail`, `FailAttempt`, `RenewLease` and `Release` all require `status='running' AND claim_generation=? AND worker_id=?` in the `WHERE` clause. Zero rows means `ErrLostOwnership`, a normal protocol outcome that is distinct from a database error. The lease deadline is not part of those conditions: an overdue lease only makes a job *eligible* for the reaper; ownership ends when the reaper *commits* a generation change.

**Leases, heartbeat, watchdog.** Defaults: lease 15 s, heartbeat 5 s, safety margin 10 s, validated at startup (heartbeat < safety < lease). Per claim, a heartbeat goroutine renews every interval and an independent watchdog on the local monotonic clock cancels the handler ("self-fence") if no renewal has succeeded within the safety margin, measured from when the renewal *request was sent*. On lost ownership or self-fence the worker writes nothing.

**Reaper.** One atomic `FOR UPDATE SKIP LOCKED` statement, safe with several reapers. Expired `running` jobs get `claim_generation + 1`; with attempts left they return to `queued` after a recovery backoff (`lease_expired`), otherwise they become `failed` with `attempts_exhausted`. Attempts consumed by a crash stay consumed.

**Retries and failure handling.** `FailAttempt` decides requeue versus terminal failure in the database from persisted counters. Handler errors are retryable unless wrapped with `handlers.Permanent`; timeouts are retryable; panics are recovered at the handler call and treated as a retryable `execution_failed` with a fixed safe message. Backoff is `min(base·2^(attempt-1), max)` with ±20 % jitter (defaults 2 s and 5 m). Handler error text is never logged or persisted; logs carry IDs, attempt numbers and allowlisted kinds only, and database errors are logged as fixed descriptions.

**Graceful shutdown (Task 25.1).** On SIGINT/SIGTERM the HTTP server stops, then the workers' context is cancelled. A cooperative handler that returns `context.Canceled` because of this is released by a fenced `Release`: back to `queued`, attempt refunded, generation untouched. A handler that ignores cancellation is *not* released; shutdown waits at most 30 s, then exits with status 1 and leaves recovery to the reaper. A SIGKILL leaves the attempt consumed.

**What is validated.** All of the above was exercised manually against real PostgreSQL 16 in earlier tasks (crash recovery, `SIGSTOP`/`SIGCONT` stale workers, forced renewal-versus-reaper orderings, database outages, retries, panics, shutdown). There are no automated tests by project policy, and no load or throughput measurement exists.

**Everything that runs in one process.** API, workers and reaper currently run in the same `forge` process. Workers call the store directly, not over a network.

**Historical material that is *not* implemented.** Earlier tasks produced design reports for a RabbitMQ outbox and a multi-tenant schema with row-level security, plus evidence spikes. None of that is in the code. `task.md` still describes the Phase 1 baseline ("channel queue, no claim protocol") and is out of date relative to the code; it should be refreshed at some point (not done here).

## 3. Intended product model and assumptions

Product model, as given: developers register accounts, applications and services; services declare job types; a developer's application submits jobs through an authenticated API; a developer-operated worker or SDK receives work and runs the developer's handler in the developer's environment; the worker reports heartbeats, success or failure; Forge manages durable state, scheduling, retries, status and analytics. Registration is metadata only.

Assumptions made (none blocked the analysis, all need confirmation):

- **A1.** Workers run in the customer's infrastructure, often behind NAT/firewalls and without a public inbound endpoint. *If customers always expose public HTTPS endpoints, B becomes more attractive.*
- **A2.** Jobs run seconds to minutes; occasionally longer. Payloads are small (≤ ~1 MiB).
- **A3.** At-least-once is acceptable and customers can make handlers idempotent. Forge never promises exactly-once.
- **A4.** Forge must show per-customer status, history and analytics from *its own* database regardless of the engine.
- **A5.** One Forge deployment serves many customers; a noisy customer must not starve others.
- **A6.** The next stage targets tens to low hundreds of customers and modest volume, not planet scale. Nothing here claims otherwise.

Open product decisions: SDK languages beyond Go; payload validation model (Forge cannot run the handler's `Validate`, see section 7); data retention and residency; whether customers need to cancel running jobs from the API; pricing model (affects which engine costs matter).

## 4. Research notes by option

Sources are official documentation fetched on 2026-10-19; URLs are in section 10. Items I could not confirm are marked **unverified**.

### Option B — Google Cloud Tasks

Verified:

- Delivery is **push**: Cloud Tasks "forwards the task request to the worker" at an HTTP endpoint; tasks with payloads must use POST or PUT. The queue "persists the task until it is successfully executed or the number of retries is exhausted". (overview)
- The handler must answer **2xx** within the deadline; anything else, or no answer, retries. Default timeout 10 minutes, maximum 30 minutes for HTTP targets. (overview)
- Delivery is **at least once**; the docs say "multiple task execution is possible" and handlers should be idempotent. (overview)
- Endpoints are "any HTTP endpoint with an external IP address". Whether private/VPC-internal endpoints are supported is **unverified**: neither the overview nor a targeted search confirmed it. (HTTP targets)
- Auth: an OIDC ID token or OAuth access token, set per task or per queue, signed on behalf of a **service account in the same project as the queue**, which also needs the Enqueuer role. (HTTP targets)
- Retry policy is **queue-level**: max attempts (example output 100), min/max backoff, max doublings, max retry duration; a task may override. Rate limits are queue-level too: max dispatches per second and max concurrent dispatches. (configuring queues)
- Headers added to each delivery: queue name, task name, retry count, execution count, ETA, and sometimes the previous response/retry reason. They are informational, not identity. (HTTP targets)
- Limits: 1 MiB per task, tasks can be scheduled up to 30 days ahead, retained at most 31 days, 1,000 queues per region per project. Task names deduplicate creation, and a deleted name is remembered for up to 24 hours. (quotas, overview)
- A completed task is deleted from the queue. Cloud Tasks is a delivery mechanism, not a job-history store.
- Pricing: the pricing page's text could not be fetched in full by my tool. A search summary of the page stated 1,000,000 billable operations per month free then $0.40 per additional million, chunked per 32 KB, with push delivery attempts (including retries) billed like other operations. Treat as **secondary; confirm on the live page**.

Not provided: any notion of a customer worker, worker identity, heartbeats or leases; a fencing token; stale-result protection; per-tenant fairness beyond using one queue per tenant (limited to 1,000 queues per region); graceful release of in-flight work; job history.

### Option C — Hatchet

Verified:

- Platform for background tasks and durable workflows; SDKs for Python, TypeScript, Go and Ruby; tasks are defined in code; workers are long-running processes in your infrastructure; MIT licence; hosted (Hatchet Cloud) or self-hosted. (docs home)
- Workers register their tasks when they start and Hatchet routes matching tasks to them; authentication is a client token (`HATCHET_CLIENT_TOKEN`). Slots cap concurrency per worker (default 100). Graceful shutdown: a worker stops taking new tasks and drains running ones until its grace period ends. (workers, FAQ)
- Workers send a heartbeat every 4 s; after 30 s without one the engine treats the worker as inactive and re-queues its in-flight tasks; the docs warn a reassign "could result in work being executed more than once". (FAQ)
- Retries: per-task `retries`, exponential backoff with a cap, a `NonRetryable` error type, and a `RetryAfter` error. Timeouts: schedule timeout (default 5 m) and execution timeout (default 60 s); a timeout counts as a failure and "does not guarantee that the task will be stopped immediately". (retry policies, timeouts)
- Concurrency: CEL-expression keys with `GROUP_ROUND_ROBIN` and several cancel strategies; per-key `max_runs`, which can be dynamic; the docs recommend per-user/organisation keys for fairness. (concurrency)
- Self-hosting: API server, engine (gRPC), PostgreSQL, dashboard, optional RabbitMQ. Hatchet Lite is a single image "for development, testing, or low-throughput production"; Compose and Helm are the documented production paths; no high-availability or scaling guidance is given. gRPC listens on 7070, API on 8080 by default; tenant creation is a server option. (self-hosting)
- Submitters without the worker's code can trigger a task through a "stub": a declaration with the same name and input/output types, with the docs warning this "can break type safety" and relies on names and schemas matching exactly. (inter-service triggering)
- Hosted pricing (pricing page, with internal inconsistencies noted by my fetch): pay-as-you-go includes 1M runs and events free per month, then $30 per million runs and $5 per million events; **10 tenants**, 100 workers, 50 users on a card-on-file plan; 7-day retention; retries do not count as runs. SSO/RBAC/audit logs and self-hosted/hybrid support are listed under the custom plan. The page contradicts itself on some limits (no-card daily caps versus the monthly allowance), so quote nothing from it without re-checking.

Unverified, and decisive for this product:

- Whether a **worker credential can be restricted** to a subset of task names, so one customer's worker cannot register another customer's task name and receive its payloads.
- Whether tenants can be created **programmatically** per customer, how many a self-hosted deployment tolerates, and whether tenant-scoped tokens give real isolation.
- Whether the Go SDK can register or trigger tasks with **names only known at runtime** from Forge-side metadata, which Forge's service registry would require.
- The transport direction and network requirements of workers (the worker docs do not state them; the engine's gRPC port suggests workers dial out, but this was not confirmed).
- What happens to a Hatchet task on a stale worker's late result (no fencing token is documented).

## 5. Comparison matrix

Effort ratings are qualitative and assume the current repository as the starting point. "Forge-built" means Forge's own code and database would still have to provide it.

| Requirement | A. Custom PostgreSQL + SDK | B. Cloud Tasks | C. Hatchet |
|---|---|---|---|
| Developer-operated workers / SDK | Forge-built SDK; workers dial out (HTTPS long-poll). Effort: medium–high | No workers. Customer must run an inbound HTTP endpoint and verify Google OIDC tokens. Forge-built receiver glue/SDK anyway | Official Go/Python/TS/Ruby SDKs, workers run in customer infra. Best developer experience, if isolation holds |
| Submission in / delivery out | API in; workers pull. No inbound port at customer | API in; Cloud Tasks pushes to customer endpoints | API (Forge) triggers by name via stub; workers connect to engine |
| Network / firewalls | Outbound-only for customers (favourable) | Inbound from Google to a reachable endpoint; private-network support unverified | Workers connect to engine (direction unconfirmed in docs); engine must be reachable by them |
| Authentication, service credentials | Forge-built (per-service worker keys) | Google service-account OIDC; account must be in the queue's project (cross-project/customer verification is customer work) | Hatchet client tokens; per-customer scoping unverified |
| Multi-tenant isolation & authz | Forge-built, but fully controllable: tenant column, per-service eligibility, RLS if wanted | Queue-per-tenant at most (1,000/region); no worker authz | Tenant is the boundary; hosted limit 10 tenants; per-tenant tokens unverified |
| Service / job-type registration | Forge-built registry (the natural fit: registration *is* metadata) | Forge-built; queues/URLs per service | Task names are defined by worker code at start; Forge metadata would have to be reconciled with that |
| Durable execution, crash recovery | Already built and validated (lease + reaper) | Retry until 2xx/exhaustion; no notion of a crashed worker beyond timeout | Built in (4 s heartbeat, 30 s timeout, reassign) |
| Duplicate delivery / idempotency | At-least-once; Forge supplies job ID + attempt + generation as idempotency material | At-least-once; no token; Forge must embed its own | Reassign can run twice; docs advise idempotency; no token documented |
| Lease / heartbeat / stale-worker protection | Built: generation fence on every write, SDK self-fence | None (the HTTP deadline of ≤ 30 min is the only liveness signal) | Heartbeat/reassign built in; fencing of late results into Forge's table is Forge-built and has nothing to fence on |
| Retries, backoff, permanent failure | Built (`FailAttempt`, classification, jitter) | Queue-level policy; task-level override; failure classification is only "non-2xx" | Built in (retries, backoff, NonRetryable, RetryAfter) |
| Rate limit, concurrency, fairness | Forge-built; claim currently global FIFO | Per-queue rate and concurrency; fairness requires a queue per tenant | Strongest: CEL keys with round-robin and dynamic `max_runs` |
| Status, history, analytics | In Forge's own tables (required in all options) | Not retained after success; Forge must record from callbacks | Dashboard and run history, but 7-day retention hosted; Forge still needs its own for customer-facing analytics |
| Graceful shutdown / interruption | Built server-side (`Release`); SDK would reuse it | Not applicable (no worker); in-flight request runs to its deadline | Built-in drain; behaviour on grace expiry is reassign |
| Operational complexity | One more public API surface, PostgreSQL; no new service | Managed by Google; Forge still operates API/DB/receiver | Hosted: low; self-hosted: engine + PostgreSQL (+ optional RabbitMQ), HA guidance absent |
| Expected implementation effort | Medium–high, incremental | Medium to build, but does not remove the main work; high customer friction | Medium integration, high uncertainty (isolation/registration unknowns) |
| Lock-in / portability | None | GCP-specific push model and IAM | Hosted: vendor API; self-hosted MIT reduces it; workflow code is SDK-shaped |
| Cost drivers | PostgreSQL size/IOPS; engineering time | Operations (including each delivery attempt and each retry) | Hosted: runs and events ($30 / $5 per million, tenant and worker limits); self-hosted: infrastructure and ops time |
| Fit with existing Go/PostgreSQL code | Direct | Removes claim/lease/reaper; keeps the rest | Replaces the engine layer; keeps API and DB, adds mapping layer |

## 6. Reuse versus replace

Classification against the intended product. "Remote" means workers talk to the Forge API instead of the database.

| Existing work | A (custom + SDK) | B (Cloud Tasks) | C (Hatchet) |
|---|---|---|---|
| `jobs` table, M-A1 columns, CHECK constraints | Keep; add tenant/service columns (later migration) | Keep (Forge's source of truth); `lease_expires_at`, `worker_id`, `claim_generation` columns become unused or advisory | Keep as Forge's mirror/history; engine columns mostly redundant |
| Claim with `FOR UPDATE SKIP LOCKED` | Keep the design; adapt to filter by service/job type and, later, tenant fairness | Replace (Cloud Tasks dispatches) | Replace (engine assigns) |
| Attempts and `claim_generation` | Keep unchanged; generation becomes the token returned to remote workers | Adapt: Forge must mint its own token and pass it in the payload, then check it on callback | Adapt similarly; the engine does not expose one |
| Fenced `Complete`/`Fail`/`FailAttempt`/`RenewLease`/`Release` | Keep unchanged; become the server side of the worker API | Complete/Fail stay (callbacks); `RenewLease` and `Release` become redundant | Complete/Fail stay for reconciliation; others redundant |
| Lease, heartbeat, watchdog | Keep the design; watchdog/heartbeat move into the SDK, server keeps lease authority | Replace with the HTTP deadline (≤ 30 min, no heartbeat) | Replace with Hatchet's heartbeat/reassign |
| Reaper | Keep unchanged | Replace by Cloud Tasks retries; may still be needed to fail stuck rows | Replace; still needed for rows whose engine run vanished |
| Retry classification (`Permanent`), backoff, jitter | Keep the policy; classification moves to the SDK (customer code classifies), backoff stays on the server | Backoff replaced by queue config; classification reduced to status codes (4xx vs 5xx) | Replaced by Hatchet retry config and `NonRetryable` |
| Panic recovery, safe worker logging | Keep the policy; recovery moves into the SDK; server-side logging rule (never log customer error text) is mandatory and transfers | Largely moot on Forge side; receiver code still needs it | SDK handles panics per its own behaviour; policy still applies to Forge logs |
| Global queued-job cap (`CreateIfQueueBelow`, advisory lock) | Keep; later per-tenant caps in the same transaction | Adapt (count in Forge's DB before creating the Cloud Tasks task) | Adapt (same) |
| Graceful shutdown and `Store.Release` | Keep; the SDK calls the same fenced release remotely | Not applicable | Replaced by the SDK's drain; `Release` unused |
| Handler registry (compiled-in handlers) | **Replace** for customers (registry becomes service metadata); keep for built-in test handlers | Replace | Replace |
| Input validation at submit | Adapt: Forge cannot run customer `Validate`; needs size cap + optional JSON Schema in service metadata | Same | Same |
| HTTP API | Keep; add auth, services, worker endpoints | Keep; add auth | Keep; add auth |
| Manual validation harnesses (outside the repo) | Directly reusable for remote-worker scenarios | Partly | Partly |

The honest reading of this table: A keeps nearly everything; B removes what we built and validated while adding a customer burden; C removes the same engine pieces, and its replacement behaviour (heartbeat 4 s / 30 s, reassign) is similar in kind but not observable by Forge's row-level fencing.

## 7. Recommended architecture

**Choose A: continue the custom PostgreSQL engine, expose it through an authenticated remote-worker API, and ship a Go worker SDK.** Own the following in Forge; delegate nothing to a managed engine at this stage:

- *Forge owns:* the job state machine, claim/lease/fencing/reaper/retry (already built), service and job-type registry, tenant identity and quotas, worker authentication, the worker API, the SDK, history and analytics, and the rule that customer-provided text is untrusted.
- *Customer owns:* the handler, its idempotency, and its side effects. Forge cannot fence external side effects.
- *Delegated to commodity infrastructure:* PostgreSQL hosting and backup, TLS termination, and (later) a managed identity provider for developer logins if needed.

**Why this beats the alternatives, and where they are stronger.**

- *Versus C:* Hatchet is stronger on engine-level features: concurrency keys with round-robin, a polished dashboard, multi-language SDKs, proven reassign behaviour, and no need for Forge to maintain the engine. It loses on the unverified isolation boundary (tenant limit of 10 hosted; per-customer credentials unknown), on dual sources of truth, and on stub-based submission coupling. If the proof of concept in section 9 shows tenant-scoped tokens and programmatic tenants work, C becomes a serious contender for the *engine layer only*.
- *Versus B:* Cloud Tasks is strongest on managed durability, scale and low ops for a push model with public endpoints. It loses on the worker model, long-running jobs (30-minute cap, no heartbeat), customer network friction (inbound endpoint, OIDC verification), the same-project service-account rule, and the absence of any history.

**How multiple developers register services.** A developer account owns applications; an application owns services; a service declares job types (name, optional input schema, default `max_attempts`, timeout, retry hints) and receives worker credentials scoped to that service. Registration writes metadata only. Jobs reference a service and a job type; the claim query filters on the services and types the authenticated worker is entitled to.

**How customer handlers receive jobs and report results.** The SDK authenticates, long-polls `claim` for the service's types, runs the registered handler under the returned deadline, heartbeats while it runs, then reports success, failure (with a classification) or release. Workers never touch the database; only the Forge API does.

**Crash, connectivity loss, late return.** Crash: the lease expires and the reaper requeues (or fails with `attempts_exhausted`); the attempt stays consumed. Connectivity loss: the SDK's watchdog cancels the handler when no renewal succeeded within the safety margin, and the server's lease deadline is the authority. Late return: the worker's call carries the claim generation and worker ID; a committed reaper or newer claim has changed the generation, so the server answers "ownership lost" (HTTP 409), nothing is written, and the SDK cancels any remaining work.

**Stale workers.** Every owner-sensitive write is a single statement conditioned on job ID, `status='running'`, generation and worker ID, so a stale worker cannot overwrite current state. This protects Forge's rows only, not the customer's external side effects.

**Evidence to collect before committing** (full criteria in section 9): PostgreSQL claim/heartbeat throughput under realistic remote-worker load (never measured); the cost of per-heartbeat writes; behaviour under concurrent tenants; and the Hatchet unknowns, via a short spike.

## 8. Conceptual remote worker protocol

This describes behaviour, not wire format. HTTPS with JSON and long polling is the likely simplest transport (stdlib, outbound-only); gRPC streaming is an alternative that can be decided later.

**Lifecycle:** `register/authenticate → claim → execute → heartbeat → report (success | failure | release) → retry or acknowledge`.

| Concern | Forge guarantees (server) | SDK / worker responsibility |
|---|---|---|
| Identity and authentication | Each worker credential belongs to one service (and tenant); a worker ID is assigned or validated per process start; credentials are revocable | Keep the secret safe; send it on every call; run TLS |
| Eligibility | Claim returns only jobs of the credential's service and entitled job types; nothing else is visible | Declare which job types it handles; refuse unknown types explicitly |
| Claim ownership and fencing | A claim returns `job_id`, `claim_generation`, `attempt`, `max_attempts`, `worker_id`, lease duration and a recommended heartbeat interval. Every later call must present generation and worker ID; a mismatch means "ownership lost" | Store the token; present it unchanged; never reuse it across jobs |
| Payload, result, error | Opaque JSON with size limits; results and errors are stored but treated as untrusted and never logged or echoed raw; failures carry a classification (permanent / retryable) and a short code | Provide a safe, bounded error code; avoid secrets in results |
| Deadline and cancellation | The claim response carries the execution deadline; the server's lease and attempt budget are authoritative | Run the handler under a context with that deadline; cancel it on lost ownership, self-fence, shutdown |
| Heartbeats and loss of ownership | A renewal extends the lease only if the claim is current; a rejected renewal tells the worker immediately | Renew at the recommended interval; local watchdog on a monotonic clock cancels the handler if renewals do not succeed within the safety margin |
| Retry scheduling and attempts | The server counts attempts, applies backoff with jitter, enforces `max_attempts`, and decides requeue versus terminal failure atomically | Classify errors (permanent vs retryable); report timeouts and panics as failures; do not retry locally |
| Idempotency and duplicates | At-least-once is the contract. The server supplies a stable job ID, the attempt number and the generation | Make the handler idempotent, using the job ID as the idempotency key; treat repeats as normal |
| Worker restart and graceful shutdown | A release call is fenced like the others, returns the job to the queue and refunds the attempt only for a cooperative shutdown | On SIGTERM stop claiming, cancel handlers, wait for them within a grace period, then release; if a handler ignores cancellation, do not release it (the lease will expire) |
| Crash | Lease expiry plus reaper recover the job | Nothing; a restarted worker simply claims again |

Behaviours the server must guarantee regardless of engine: fencing on every write, atomic retry-versus-terminal decisions, reaper recovery, safe handling of untrusted text, and tenant/service authorization on every call. Behaviours that the SDK provides but the server cannot enforce: cooperative cancellation, the local watchdog, and handler idempotency.

## 9. Risks, unknowns and staged plan

**Key risks of choosing A.**

- PostgreSQL as a queue has an unknown ceiling; the reaper, per-heartbeat writes and long-poll claims all hit the same table. Not measured.
- A public worker API raises the security bar: credential issuance, revocation, rate limits, payload size limits and abuse handling all become Forge's job.
- Tenant fairness and per-tenant caps do not exist yet; claim order is global FIFO.
- Maintaining an SDK (versioning, compatibility, languages) is a long-term commitment.
- Forge becomes responsible for subtle distributed behaviour; the manual-only validation policy gives limited regression protection.

**Unresolved questions.** Section 3 lists the product decisions. Technical ones: payload validation without customer code; whether jobs need cancellation from the API; whether webhooks are needed for completion notification; retention and deletion policy.

**Staged plan** (each item is a separate future task, to be written when reached; none is started):

1. *Decision review* of this document and the assumptions A1–A6.
2. *Task 25.2 (recommended next):* remote worker API v0 on the existing store: authenticated claim, renew, complete, fail, release endpoints over HTTPS (claim as a long poll), protected by a single static worker key for now (explicitly not the final auth), with the in-process pool left unchanged. Validate with a scratch remote-client harness against the same crash, stale-worker, outage and shutdown scenarios used before.
3. *Next:* Go SDK v0 (claim loop, heartbeat, watchdog, release on shutdown), built from the harness.
4. *Then:* tenant, application and service tables; service-scoped credentials; eligibility filter in claim; per-tenant caps and fair claim ordering; submit-time validation from service metadata.
5. *Then:* measurement (see criteria below) before any claim about capacity.

**Optional discriminating proof of concept (Hatchet isolation spike), timeboxed to a day or two, before step 4.** Self-host Hatchet Lite locally (no production claims) and test:

| # | Pass criterion | If it fails |
|---|---|---|
| 1 | Two tenants created without human sign-up (CLI/API/config) with separate tokens | C is rejected for per-customer isolation |
| 2 | A worker using tenant A's token cannot receive tasks or read runs of tenant B | C rejected |
| 3 | A worker can register a task name that Forge's metadata supplies at runtime (not compiled in), and Forge can trigger it by name over the API from a process that has no worker code | C rejected for the registry model |
| 4 | `kill -9` of a worker is detected and the task re-runs within about 35 s; the second execution is observable; late result from the killed worker (via `SIGSTOP`/`SIGCONT`) does not overwrite the new result | Record gap; Forge would need its own fencing on top |
| 5 | One noisy tenant with a round-robin concurrency key does not delay another tenant's task beyond a stated bound | Fairness claim withdrawn |
| 6 | Run history can be mirrored into a Forge table idempotently | Reconciliation cost goes up |

If 1–3 pass and the self-hosted operational burden is acceptable, reconsider C as the engine layer *behind* Forge's own API and registry. If any of 1–3 fails, this decision stands.

**Measurement criteria for A (collect before capacity claims).** Using a disposable PostgreSQL and N simulated remote workers (scratch harness, outside the repo): claim latency percentiles at stated queue depths; sustained heartbeat write rate versus table bloat/HOT-update ratio; reaper pass duration with many expired leases; behaviour with 1 noisy and 20 quiet tenants; recovery time after SIGKILL of the API process. State results with the hardware used; do not extrapolate.

## 10. Decision

**Proceed with the custom PostgreSQL engine plus a remote worker protocol and Go SDK (Option A).** Do not adopt Cloud Tasks (wrong delivery model for developer-operated workers and long jobs). Do not adopt Hatchet now; keep it as a candidate for the engine layer pending the optional spike above, whose failure of criteria 1–3 would confirm this decision and whose success would justify revisiting it.

**Recommended next task:** *Task 25.2 — Remote worker API v0* (authenticated claim/renew/complete/fail/release endpoints over the existing store, with the in-process pool unchanged and a scratch remote-client harness for validation).

### Sources

- Cloud Tasks overview — https://docs.cloud.google.com/tasks/docs/dual-overview
- Cloud Tasks queue configuration (retry, rate limits) — https://docs.cloud.google.com/tasks/docs/configuring-queues
- Cloud Tasks HTTP target tasks (auth, headers) — https://docs.cloud.google.com/tasks/docs/creating-http-target-tasks
- Cloud Tasks quotas and limits — https://docs.cloud.google.com/tasks/docs/quotas
- Cloud Tasks pricing (figures from a search summary; page text not fully retrieved) — https://cloud.google.com/tasks/pricing
- Hatchet documentation home — https://docs.hatchet.run/
- Hatchet workers — https://docs.hatchet.run/home/workers
- Hatchet FAQ (heartbeats, reassign, graceful shutdown) — https://docs.hatchet.run/v1/faq
- Hatchet retry policies — https://docs.hatchet.run/home/retry-policies
- Hatchet timeouts — https://docs.hatchet.run/home/timeouts
- Hatchet concurrency — https://docs.hatchet.run/home/concurrency
- Hatchet self-hosting — https://docs.hatchet.run/self-hosting
- Hatchet self-hosting configuration options — https://docs.hatchet.run/self-hosting/configuration-options
- Hatchet inter-service triggering — https://docs.hatchet.run/v1/inter-service-triggering
- Hatchet pricing — https://hatchet.run/pricing
