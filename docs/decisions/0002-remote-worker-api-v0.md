# Specification 0002 — Remote Worker API v0 (Task 25.2)

Status: **implemented as v0 (note added 2026-10-22).** `FORGE_EXECUTION_MODE=remote`, the separate worker listener and the five endpoints (claim, renew, complete, fail, release) exist in `cmd/forge/main.go` and `internal/workerapi/server.go`, with the typed client `sdk/workerclient`. The sections below are the original specification and were **not** line-by-line reconciled with the code: where they differ, the source is authoritative. Still open from this document: per-service authorization and key scoping (section 4.3 and section 7); authentication remains a single shared development key over plain HTTP, with no rotation, revocation or brute-force throttling, and must not be treated as production-grade. Server-side enforcement of the execution deadline is not implemented (see 0003 section 9). **Later note:** the in-process ("embedded") execution mode and the built-in `dataset_validate` handler discussed in this record were removed on 2026-10-22; Forge now always runs with customer-run remote workers. References to them below are historical. **Later note (2026-10-23):** the claim request gained an optional `max_jobs` (1 to 20); with it the answer is `{"jobs": [...]}` and without it the single-job answer is unchanged.
Follows [0001-execution-architecture.md](0001-execution-architecture.md), which decided to keep the custom PostgreSQL engine and add a remote worker protocol and SDK. That decision is unchanged and 0001 is not edited.

## 1. Current verified behaviour

Checked against the source on 2026-10-19. The directory is not a Git repository, so there is no working-tree status to report; the files below were read directly.

**Store contract** (`internal/store/store.go`). `ClaimedJob` carries `Job` (ID, type, input), `Attempt`, `MaxAttempts`, `Generation`, `WorkerID`. Methods that matter here:

| Method | Behaviour |
|---|---|
| `Claim(ctx, workerID, lease) (ClaimedJob, ok, err)` | One statement: oldest due `queued` job (`ORDER BY next_run_at, created_at, id`, `FOR UPDATE SKIP LOCKED`, `attempt < max_attempts`) becomes `running`; `attempt` and `claim_generation` +1; sets `worker_id`, first-only `started_at`, `attempt_started_at`, lease. **No filter by job type.** `ok=false` with nil error means no work. |
| `RenewLease(ctx, c, lease) error` | Fenced on `status='running' AND claim_generation AND worker_id`; no lease-deadline check. Zero rows => `ErrLostOwnership`. |
| `Complete(ctx, c, result) error` | Same fence; `succeeded`, result stored, lease/worker cleared. |
| `Fail(ctx, c, info) error` | Same fence; terminal `failed` with a supplied code and message. Used in-process only for an unregistered job type. |
| `FailAttempt(ctx, c, info, retryable, retryDelay) (willRetry, err)` | Same fence. One statement decides from the *persisted* `attempt`/`max_attempts`: requeue at `now() + retryDelay` (database clock) or terminal `failed` (`attempts_exhausted` if the budget is spent, otherwise the supplied code). Uses only job ID, generation and worker ID from `c`. |
| `Release(ctx, c) error` | Same fence; back to `queued`, `next_run_at = now()`, attempt refunded, generation kept. |
| `ReapExpired(ctx, limit, backoff)` | Expired `running` jobs get `claim_generation + 1`; requeued (`lease_expired`) or failed (`attempts_exhausted`). Attempt stays consumed. |
| Errors | `ErrLostOwnership` (zero rows, a normal protocol outcome), `ErrQueueFull`, `ErrNotFound`. Anything else is an operational error. |

**Retry policy lives in the worker package.** `retryDelay(attempt, base, max)` and `classifyFailure` are unexported in `internal/worker/retry.go`. Safe error text is fixed server-side (`execution_failed` / "Job execution failed.", `timeout`, "Handler panicked during execution."); handler error text is never logged or stored.

**Submission** (`internal/worker/service.go`). `Submit` looks the type up in the compile-time `handlers.Registry` (unknown type => 400 `unsupported_job_type`), runs that handler's `Validate`, and calls `CreateIfQueueBelow` (global cap, default 1000, exact under concurrency). There is no way to accept a type without a Forge-compiled handler.

**In-process execution** (`internal/worker/pool.go`, `cmd/forge/main.go`). `Pool.Start` launches `FORGE_WORKERS` claim loops **and** the reaper together. Each loop calls `Claim` with no type filter. A claimed job whose type is not in the registry is failed immediately (`internal_error`). Handler timeout is the fixed 30 s `executionTimeout`. Lease/heartbeat/safety/reaper/retry settings come from `FORGE_*` variables, validated at startup (heartbeat < safety margin < lease).

**HTTP** (`internal/api/server.go`, `main.go`). One listener (default `127.0.0.1:8080`, `ReadTimeout`/`WriteTimeout` 15 s, body limit 1 MiB). Four routes, all unauthenticated. Error shape: `{"error":{"code","message"}}`. `GET /v1/jobs/{id}` returns the stored result and error, never the input.

**Database pool.** `dbMaxConns = 10`.

**Shutdown.** Task 25.1: HTTP stops, then workers; cooperative handlers are released with a refunded attempt; up to 30 s drain.

**Consequences for this task (verified, not assumed):**

1. Nothing prevents two kinds of claimers from racing: any process that calls `Claim` can take any job. A remote claimer needs a type filter, and the in-process pool needs an explicit way to not run (section 4).
2. A job left in the table with a type the in-process pool does not know is *destroyed* by it (failed as `internal_error`). Switching modes on a database that still holds remote jobs would therefore lose them unless the in-process claim is also filtered by type (section 4.3).
3. The single listener's 15 s `WriteTimeout` is shorter than a sensible long poll.
4. The unauthenticated submit/read API must not share a network exposure with the worker API.

## 2. Remote-worker architecture

```
 customer app ──POST /v1/jobs──────────►┐  loopback listener (unauthenticated, as today)
                                        │
                                  ┌─────▼──────────────────────────┐
                                  │ Forge process                  │
                                  │  PostgreSQL store (unchanged)  │
                                  │  reaper (runs in every mode)   │
                                  │  worker API listener ◄─────────┼── HTTPS (via TLS proxy) ──┐
                                  │  in-process pool: OFF in       │                           │
                                  │  remote mode                   │                           │
                                  └────────────────────────────────┘                           │
                                                                                               │
                              customer environment (outbound-only):  worker/SDK ──────────────┘
                              claim ─► run customer handler ─► renew … ─► complete | fail | release
```

- Workers only make **outbound HTTPS** calls; no inbound port is opened at the customer.
- Workers never touch PostgreSQL. Only the Forge process does, through the existing `Store`.
- Forge contains no handler for remote job types. In remote mode the handler registry is not built at all, so Forge cannot execute such a job even by mistake.
- The reaper keeps running inside Forge, so crashed remote workers are recovered exactly as crashed in-process workers are.

New code is limited to: an execution-mode switch, a type filter on `Claim`, a small submit-validation seam, a worker-API HTTP package, and a shared retry/failure-info helper (section 9).

## 3. Execution mode and competing-execution prevention

### 3.1 Decision

One new setting selects who may execute jobs:

| Setting | Values | Default |
|---|---|---|
| `FORGE_EXECUTION_MODE` | `embedded`, `remote` | `embedded` |

- **`embedded`** (default, today's behaviour): the in-process pool runs; the worker API listener is **not started** and its routes do not exist. Existing deployments are unchanged.
- **`remote`**: the in-process claim loops are **not started** and no handler registry is built; the reaper **is** started; the worker API listener **is** started.

There is deliberately no hybrid mode in v0. A mixed deployment would need per-type routing between the two claimers, which is the service-registration work this task excludes.

### 3.2 Validation at startup (fail fast, clear message)

- Unknown mode value => exit 1, naming the allowed values.
- `remote` requires `FORGE_WORKER_API_KEY` (section 6), `FORGE_WORKER_ADDR`, and a non-empty `FORGE_REMOTE_JOB_TYPES` (section 5).
- `remote` rejects `FORGE_WORKERS` if it is set: it would be silently ignored otherwise, which hides a misconfiguration.
- `embedded` rejects `FORGE_WORKER_API_KEY`, `FORGE_WORKER_ADDR` and `FORGE_REMOTE_JOB_TYPES` if set, for the same reason.
- The existing lease/heartbeat/safety/reaper/retry validation applies in both modes.

### 3.3 Why a type filter on `Claim` is still required

The mode switch prevents *live* competition. The filter prevents *data loss across a mode change* (section 1, consequence 2) and gives the remote worker a way to ask for only the types it can run:

`Claim(ctx, workerID, lease, types []string)`. An empty/nil `types` means "any type" (not used by either mode in v0; kept only so the signature is total). The SQL gains `AND type = ANY(...)`.

- **Remote** claims pass the worker's requested types, intersected with the allowlist (section 5); never empty.
- **Embedded** passes `registry` type names, so leftover remote-type rows stay `queued` instead of being failed as `internal_error`. The existing branch that fails an unregistered type becomes unreachable in practice; it stays as a safety net.

This is the only store signature change, and it is needed for the reasons above. No other store method changes; `FailAttempt`, `Complete`, `Release`, `RenewLease` already take the claim and ignore everything but ID, generation and worker ID.

Implementation note for subtask 25.2.1 (an open technical point, not a decision): passing a Go `[]string` into a GORM named parameter as `ANY(CAST(@types AS text[]))` must be verified against the pgx driver in use. Fallback if it does not work: pass a comma-joined string and use `string_to_array(CAST(@types AS text), ',')`; this is safe because job types are validated to a charset that excludes commas.

## 4. Job-type eligibility

### 4.1 Options compared

| Option | Behaviour | Pros | Cons |
|---|---|---|---|
| **(1) Accept any syntactically valid type** in remote mode | Submit checks only a regex | Zero configuration; closest to the final "any registered service" feel | Violates the existing rule "unknown types must be rejected"; a typo queues work no worker will ever take, consuming the global cap; nothing says which worker may run what |
| **(2) Explicit allowlist** `FORGE_REMOTE_JOB_TYPES` | Submit rejects types not in the list; claim only returns listed types | Keeps "unknown types are rejected"; typo => 400; a worker's claim can be checked against it; trivially replaced by a registry table later | One more environment variable; restart to change the list; not per service |

### 4.2 Recommendation: option 2

`FORGE_REMOTE_JOB_TYPES` is a comma-separated list. Each entry must match `^[a-z][a-z0-9_.-]{0,63}$` (this excludes commas, spaces and control characters). At most 32 entries. It is explicitly a **development stand-in for service registration**, with no meaning of ownership: any holder of the one worker key may claim any listed type.

Submission in remote mode:

- type not in the allowlist => `400 unsupported_job_type` (same code and message as today).
- `input` must be present and valid JSON; Forge cannot run the customer's `Validate`, so deeper validation is the worker's job. The 1 MiB body limit still applies. A worker that finds the input unusable reports a **permanent** failure.
- `CreateIfQueueBelow` and the global cap are unchanged.

Implementation shape: a minimal seam in `worker.Service` (e.g. a type/input checker that is registry-backed in `embedded` and allowlist-backed in `remote`). The exact form is left to subtask 25.2.1; the registry-based behaviour must stay byte-for-byte the same in `embedded`.

### 4.3 What this does not provide (must be fixed before production)

No per-service ownership: one key can run every type. No tenant or developer identity. No per-service input schema, retry or timeout settings. No way to add a type without restarting Forge. Therefore service registration with per-service credentials is **required** before any production use; this v0 only proves the coordination boundary.

## 5. API contract

### 5.1 Listener, headers and conventions

- A **separate** `http.Server` on `FORGE_WORKER_ADDR` (no default; required in `remote` mode). Reasons: only this port needs to be reachable by workers, while the unauthenticated submit/read API stays on loopback; and it needs its own timeouts (the main listener's 15 s `WriteTimeout` would cut off a long poll).
- Worker listener timeouts: `ReadHeaderTimeout` 5 s, `ReadTimeout` 15 s, `WriteTimeout` 40 s, `IdleTimeout` 60 s. Body limit 1 MiB (`http.MaxBytesReader`), `DisallowUnknownFields`, single JSON object.
- Forge does **not** terminate TLS in v0. Workers must reach it through a TLS-terminating proxy; the key is otherwise sent in clear (section 7).
- Auth header on every request: `Authorization: Bearer <key>`.
- Errors use the existing shape `{"error":{"code":"…","message":"…"}}`. Messages never echo request bodies, the key, or database text.
- Routes accept only the stated method; a wrong method gets the existing JSON 405.
- All identifiers are validated: `job_id` matches `^job_[0-9a-f]{16}$`; `worker_id` matches `^[A-Za-z0-9._-]{8,64}$` (this also prevents log injection, since `worker_id` is logged); `claim_generation` is an integer ≥ 1.
- Database operations use a bounded context (existing 5 s `queryTimeout`). A database failure is `503 unavailable`, never `409`.

### 5.2 Claim — `POST /v1/worker/claim`

Request:

```json
{ "worker_id": "billing-host1-4821-a3f9", "types": ["order.send_email"], "wait_seconds": 20 }
```

- `types`: 1–8 entries, each must be in the allowlist; otherwise `400 invalid_request`. One or several types are allowed; the server does not guarantee fairness across the listed types in v0.
- `wait_seconds`: integer 0–25, default 0 (immediate answer). Values above 25 => 400.
- Durable operation: `store.Claim(ctx, worker_id, FORGE_LEASE_DURATION, types)`.

Behaviour: the handler tries `Claim`, and if there is no work it sleeps `FORGE_POLL_INTERVAL` (existing setting, default 1 s) and tries again until `wait_seconds` elapses. Between polls it checks the request context and the shutdown signal. The statement itself runs on a context detached from the request (as the in-process pool does) so a client disconnect cannot cancel a statement whose outcome would then be unknowable.

Success `200`:

```json
{
  "job_id": "job_9f86d081884c7d65",
  "type": "order.send_email",
  "input": { "order_id": "A-1001" },
  "attempt": 1,
  "max_attempts": 3,
  "claim_generation": 4,
  "worker_id": "billing-host1-4821-a3f9",
  "lease_seconds": 15,
  "heartbeat_interval_seconds": 5,
  "safety_margin_seconds": 10,
  "deadline_seconds": 30
}
```

- The five timing fields tell the SDK what to do; they come from the server's validated configuration. The SDK must renew at least every `heartbeat_interval_seconds`, and cancel its handler if no renewal has succeeded within `safety_margin_seconds` of the last renewal request it *sent*.
- `deadline_seconds` is the existing fixed 30 s execution timeout. In v0 it is **advisory and enforced by the SDK**: the server's only liveness check is the lease, so a hung handler whose SDK keeps renewing would hold its job indefinitely. This is a known gap (section 10).
- `input` is the job's raw JSON. This is the only place input leaves Forge; the endpoint is key-protected.

No work: `204 No Content`, empty body, after at most `wait_seconds`. This is also what a waiting claim returns when Forge begins shutting down (it never starts a new `Claim` after shutdown has begun).

Errors: `400 invalid_request`, `401 unauthorized`, `503 unavailable` (database), `429 too_many_polls` with `Retry-After: 5` when more than `FORGE_WORKER_MAX_POLLS` (default 64, new, section 9) long polls are in flight. This cap is the only concurrency control in v0; per-worker concurrency is the SDK's business (one claim request per free slot).

State change: `queued -> running`, `attempt+1`, `claim_generation+1`.

**Lost claim response.** If Forge commits the claim but the response never reaches the worker (disconnect, crash), the job is `running` under a worker that does not know it. Nothing renews the lease, so after `lease_seconds` plus the reaper interval it is recovered; **that attempt stays consumed**. v0 accepts this cost; the alternative (best-effort `Release` on a detected write failure) is listed in section 10.

### 5.3 Renew — `POST /v1/worker/jobs/{job_id}/renew`

```json
{ "worker_id": "billing-host1-4821-a3f9", "claim_generation": 4 }
```

- Durable operation: `store.RenewLease(ctx, claim, FORGE_LEASE_DURATION)`.
- `200`: `{"lease_seconds": 15}`.
- `409 ownership_lost`: zero rows matched. The worker must **stop the handler immediately** and make no further call for this claim.
- `503 unavailable`: database error. This is **not** proof that ownership is lost.
- The request is idempotent; repeating it is harmless.

### 5.4 Complete — `POST /v1/worker/jobs/{job_id}/complete`

```json
{ "worker_id": "billing-host1-4821-a3f9", "claim_generation": 4, "result": { "sent": true } }
```

- `result` optional; if present it must be valid JSON of at most 256 KiB (inside the 1 MiB body). It becomes the job's public result in `GET /v1/jobs/{id}`, so workers must not put secrets in it.
- Durable operation: `store.Complete(ctx, claim, result)`.
- `200`: `{"outcome": "succeeded"}`. State: `running -> succeeded`, lease and worker cleared.
- `409 ownership_lost`: not applied. The worker must treat its work as superseded.
- `503 unavailable`: not confirmed (section 6.3).

### 5.5 Fail — `POST /v1/worker/jobs/{job_id}/fail`

```json
{
  "worker_id": "billing-host1-4821-a3f9",
  "claim_generation": 4,
  "attempt": 1,
  "retryable": true,
  "kind": "error"
}
```

- `kind` is one of `error`, `timeout`, `panic`. It selects a **fixed, server-owned** code and message, using the same text the in-process worker uses (`execution_failed`, `timeout`, and `execution_failed` with the "Handler panicked…" message). **No customer-written error text is accepted or stored in v0**, because such text is untrusted and is exposed through the unauthenticated read API. A future opt-in summary field would need its own limits and warnings.
- `retryable`: `false` means a permanent failure (the SDK's equivalent of `handlers.Permanent`).
- `attempt`: the value from the claim response; must be 1 ≤ attempt ≤ 20. The server uses it only to compute the backoff delay with the existing formula (`min(base·2^(attempt-1), max)`, ±20 % jitter). A client that lies only changes the delay of its own job; whether a retry is allowed is decided by the database from persisted counters.
- Durable operation: `store.FailAttempt(ctx, claim, info, retryable, delay)`.
- `200`, one of:
  - `{"outcome": "requeued", "retry_in_seconds": 3}`: persisted as `queued`, claimable after the delay (database clock).
  - `{"outcome": "failed"}`: terminal (`attempts_exhausted` if the budget was spent, otherwise the supplied code).
- `409 ownership_lost`, `503 unavailable` as above.
- The response is authoritative: the SDK must not guess from its own attempt count.

### 5.6 Release — `POST /v1/worker/jobs/{job_id}/release`

```json
{ "worker_id": "billing-host1-4821-a3f9", "claim_generation": 4 }
```

- Durable operation: `store.Release(ctx, claim)`. State: `running -> queued`, attempt refunded, generation unchanged, claimable at once.
- `200`: `{"outcome": "released"}`. This is the only response that confirms a release.
- `409 ownership_lost`: not applied. It also results if the release had already committed on an earlier request whose response was lost: the store cannot tell these apart (documented in Task 25.1). The worker takes no further action either way.
- `503 unavailable` or any transport failure: **unconfirmed**; the job may or may not have been released.
- Contract for the SDK: call release only after the handler has returned and renewals have stopped. The server cannot check this. A premature release would let a second worker run the job concurrently with a still-running handler.

### 5.7 Endpoint summary

| Operation | Route | Store call | Success |
|---|---|---|---|
| Claim | `POST /v1/worker/claim` | `Claim(…, types)` | 200 job / 204 no work |
| Renew | `POST /v1/worker/jobs/{id}/renew` | `RenewLease` | 200 |
| Complete | `POST /v1/worker/jobs/{id}/complete` | `Complete` | 200 |
| Fail | `POST /v1/worker/jobs/{id}/fail` | `FailAttempt` | 200 requeued/failed |
| Release | `POST /v1/worker/jobs/{id}/release` | `Release` | 200 |

Common errors: `401 unauthorized`, `400 invalid_request`, `405 method_not_allowed`, `409 ownership_lost`, `413 payload_too_large`, `429 too_many_polls` (claim only), `503 unavailable`. Unknown job IDs return `409 ownership_lost` (not 404): to a caller without a valid claim, "job does not exist" and "not yours" must look identical.

## 6. Ownership, retries, heartbeats, release and shutdown

### 6.1 Fencing carries over unchanged

Every mutating call presents `job_id`, `claim_generation` and `worker_id`, which the store requires in the `WHERE` clause together with `status='running'`. A reaper transition or newer claim changes the generation, so an older worker's calls match zero rows and return 409 without writing anything. `claim_generation` is the fencing token; `attempt` is only a budget counter and is not used for fencing. The lease deadline is still not part of the check: a late-but-first renewal succeeds, and only a *committed* reaper change revokes ownership. A 409 means "the database says this claim is no longer current"; it does not claim anything about what the job's state is now.

### 6.2 Who owns heartbeats and deadlines

- The **SDK** sends renewals and runs the local watchdog, as `internal/worker/lease.go` does in-process: a renewal that returns 409 cancels the handler at once; failures (network, 5xx) are retried, and if no renewal has succeeded within `safety_margin_seconds` of the last request sent, the SDK cancels the handler ("self-fence") and reports nothing for this claim. The watchdog must not depend on the renewal call returning.
- The **server** owns lease authority: it extends or refuses, and the reaper expires.
- `safety_margin < lease` guarantees a cooperative handler is told to stop before the reaper can give the job away.
- Go cannot kill a non-cooperative handler; the same holds for any SDK. A handler that ignores cancellation may keep running after ownership is lost. Forge fences its own rows, not the customer's external side effects.

### 6.3 Unconfirmed outcomes

For `complete`, `fail` and `release`, a timeout, transport error or 5xx means *the outcome is not confirmed*. The SDK may retry the **same** call a bounded number of times (the in-process default is 3 tries, 1 s apart), never on 4xx. If an earlier try committed but its response was lost, a retry returns 409; the SDK cannot tell that from a genuine loss and must stop either way, logging "outcome not confirmed" rather than success. This is the same ambiguity the store already has; v0 does not add an idempotent-replay path (section 10).

### 6.4 Duplicate execution and idempotency

Forge provides at-least-once execution. A worker can finish a business operation and lose its connection before `complete` arrives; the lease then expires, the job is reclaimed, and the handler runs again. The same happens after a crash, a self-fence, or a Forge restart that outlasts the lease. Customers must therefore make handlers idempotent, using the stable `job_id` (and, if useful, `attempt` and `claim_generation`) as the idempotency key. Forge does not and cannot promise exactly-once business execution.

### 6.5 Retry and failure classification

The SDK classifies (`retryable` true/false, `kind`); the server owns backoff, the attempt budget and the requeue-or-terminal decision. Handler timeouts and recovered panics are reported as retryable failures, exactly as in-process. A permanent failure is terminal at once. No retry logic runs in the SDK; its only retries are the bounded resends of an unconfirmed call.

### 6.6 Cooperative shutdown of a remote worker

On SIGTERM the SDK: (1) stops sending new `claim` requests and abandons any in-flight long poll (a late 200 for an abandoned claim is lost-claim-response, section 5.2, and is recovered by lease expiry); (2) cancels running handlers; (3) waits for them within a bounded grace period; (4) for each job whose handler returned `context.Canceled` because of the shutdown and whose claim is still held, stops renewing and calls `release`; (5) never releases a job whose handler is still running; (6) logs one of four safe outcomes per job.

| Outcome | Evidence | Meaning |
|---|---|---|
| `released` | 200 `released` | Job is queued again, attempt refunded |
| `ownership_lost` | 409 | Claim was already superseded; nothing more to do |
| `release_not_saved` | timeout, transport error, 5xx | Unknown; lease expiry will recover it (attempt consumed if it was not released) |
| `crashed` (no call) | none | Reaper recovers after lease expiry; attempt consumed |

Forge-side shutdown: the worker listener stops accepting, in-flight long polls answer `204` immediately, and no new `Claim` starts. Running remote jobs are not affected by a Forge stop: they continue, and their renewals fail until Forge returns. If Forge is down longer than the lease window the reaper recovers those jobs on restart and the old workers receive 409.

## 7. Authentication: prototype and production limits

**v0 mechanism.** One shared secret, read from the environment variable `FORGE_WORKER_API_KEY` at startup. Never stored in the database, a file, source code or documentation examples. Requirements:

- Minimum 32 bytes of randomness (e.g. generated at setup time with a cryptographic random source and base64-encoded); startup rejects a shorter key. Documentation uses `$FORGE_WORKER_API_KEY` only, never a literal value.
- Compared in constant time: hash both the presented and configured values with SHA-256 and compare with `crypto/subtle.ConstantTimeCompare`, so neither length nor content leaks through timing.
- Missing header, wrong scheme, wrong key and malformed value all return the **same** `401 unauthorized` with `WWW-Authenticate: Bearer`. No work is done before authentication succeeds, including no database access.
- The header value is never logged, echoed or placed in an error. Logs record only a count of rejected requests, rate-limited (for example one line per 30 s), without the address-derived or header-derived data beyond what the server already logs.
- Treat the key as exposed to anyone who can read process environments on the Forge host.

**What v0 cannot provide.** Any holder can claim and mutate every allowlisted type; there is no identity per customer, service, worker or developer; no rotation without a restart and redeploy of all workers; no revocation of a single worker; no brute-force throttling (the key's entropy is the defence); no audit trail of who did what; no TLS in Forge itself, so the key and job payloads travel in clear unless a TLS proxy is placed in front.

**Production path.** Service-scoped credentials issued per registered service (stored hashed, revocable, rotatable), checked on every call against the job's service and type; claim restricted to the credential's service; tenant-scoped quotas and fairness; TLS and rate limiting at the edge. These belong to the service-registration work described in 0001, not to this task.

**Do not expose this prototype publicly as a production service.** The submit/read API remains unauthenticated by design in v0; this is why the worker API is on its own listener.

## 8. Acceptance criteria

Method: no test files and no helper programs inside the repository. Use a disposable PostgreSQL 16 container (as in earlier tasks), the real Forge binary built with `-race`, `curl` for most protocol checks, and a throwaway Go or shell harness in the session scratch directory for timing and concurrency scenarios. Items marked **DB** require the real PostgreSQL; the rest can be answered by the harness alone. Report which checks ran; do not claim others.

| # | Check | Pass criterion | Needs |
|---|---|---|---|
| 1 | Remote happy path | In remote mode, submit a `custom.type` job; a harness worker claims it, then `complete` with a result; `GET /v1/jobs/{id}` shows `succeeded` and the result; Forge executed no handler | DB |
| 2 | Failure and retry | `fail` with `retryable:true` returns `requeued` with `retry_in_seconds` within the formula's bounds; the job is not claimable early; the next claim has generation +1 and attempt +1; budget exhaustion ends `failed` / `attempts_exhausted`; `retryable:false` ends `failed` at once | DB |
| 3 | Bounded no-work | An idle claim with `wait_seconds=3` returns `204` after about 3 s and not earlier than 2.5 s nor later than 4 s; `wait_seconds=26` is `400` | DB |
| 4 | Authentication | Missing, malformed and wrong keys all get an identical `401` on all five endpoints; no job changes (row hashes identical before/after); the key does not appear in Forge's log output or any response body | DB |
| 5 | Fencing | After a reaper-forced second claim, the old generation's renew, complete, fail and release all return `409` and leave the row unchanged; a wrong `worker_id` with the right generation also returns `409` | DB |
| 6 | Mode separation | In remote mode with queued remote-type jobs and no worker connected, no job leaves `queued` for 15 s; in embedded mode the worker API port is closed and behaviour matches the pre-change regression run; switching a database holding queued remote-type jobs to embedded mode leaves them `queued` (not failed) | DB |
| 7 | Startup validation | Each rule in section 3.2 exits 1 with a message that does not print the key | no DB |
| 8 | Submit rules | In remote mode a type outside the allowlist is `400 unsupported_job_type`; a listed type is `202`; the global queue cap still returns `503 queue_full` | DB |
| 9 | Graceful remote shutdown | Harness worker holding a job receives SIGTERM: it stops polling, cancels its handler, calls `release` after the handler returns, receives `released`; the row is `queued`, attempt refunded, generation unchanged; the next claim has generation +1. A handler that ignores cancellation is not released | DB |
| 10 | Disconnected worker | `SIGSTOP` a harness worker past the lease; after the reaper takes the job and a second worker finishes it, `SIGCONT`: the resumed worker's renew/complete/fail/release all return `409` and the second worker's result is untouched | DB |
| 11 | Database outage | With PostgreSQL stopped, renew/complete/fail/release return `503`, never `409`; the harness worker logs "not confirmed", not success; after restart, processing resumes | DB |
| 12 | Hard crash | `kill -9` of a harness worker: the reaper recovers the job; attempt not refunded; a later worker completes it | DB |
| 13 | Log and payload hygiene | With a distinctive dummy marker in the job input, in a result, and in a worker's local error text: the marker appears in no Forge log line; the input appears only in the claim response; no response ever contains the key | DB |
| 14 | Long-poll cap and shutdown | More than `FORGE_WORKER_MAX_POLLS` concurrent polls get `429`; Forge SIGTERM makes waiting polls return `204` promptly | DB |
| 15 | Existing behaviour | In embedded mode: the four existing routes behave as before (health 200, submit 202, get, list, unknown type 400, missing id 404); the Task 25.1 shutdown scenarios (cooperative release, ownership loss, SIGKILL, non-cooperative handler, database outage at release) still pass; the Task 23 reaper regressions still pass | DB |
| 16 | Static checks | `gofmt -l cmd internal` empty; `go build ./...`; `go vet ./...`; `go build -race` binary run during 1–15 shows no races; `atlas migrate validate --env local` passes and `migrations/` and `atlas.sum` are unchanged | no DB (Atlas uses its own dev container) |

Passing these shows the v0 protocol behaves as specified under controlled conditions on one machine. It does **not** show production capacity, multi-tenant security, or safety against a hostile client.

## 9. Staged implementation plan

Each subtask is a separate change, stops for review, and ends with its own report. None is started.

**25.2.1 — Execution mode and eligibility groundwork (no HTTP surface).**
- Add `FORGE_EXECUTION_MODE` with validation (3.2); in `remote`, skip the pool's claim loops and registry, keep the reaper (split `Pool` start so the reaper can run alone, or give the pool an explicit reaper-only start).
- Add the type filter to `Claim` and pass registry types from the embedded pool.
- Add the submit-validation seam and `FORGE_REMOTE_JOB_TYPES` parsing.
- Export the minimal shared helpers the worker API will need (retry delay computation and the fixed failure-info mapping) from the `worker` package.
- Completion criteria: checks 6, 7, 8, 15, 16 pass; the embedded-mode regression is unchanged; no new routes exist.
- Depends on: nothing.

**25.2.2 — Worker API server and static key.**
- New `internal/workerapi` package and listener (5.1), the five endpoints, auth middleware (7), the long-poll cap, shutdown behaviour.
- Reads `FORGE_WORKER_ADDR`, `FORGE_WORKER_API_KEY`, `FORGE_WORKER_MAX_POLLS`.
- Completion criteria: checks 1–5, 13, 14, 16 pass using `curl` and a small scratch harness.
- Depends on: 25.2.1.

**25.2.3 — Resilience validation and documentation.**
- Run checks 9–12 with a scratch remote-worker harness outside the repository, including the `SIGSTOP`, `kill -9`, outage and shutdown scenarios; update README (modes, new variables, limitations) and CLAUDE.md known limitations.
- Completion criteria: all checks 1–16 reported with actual results; docs state the v0 limits from sections 7 and 10.
- Depends on: 25.2.2.

**Separate, later task (not part of 25.2): Go worker SDK.** A reusable client with the claim loop, renewal, watchdog, classification, panic recovery and shutdown/release, plus its own versioning decisions. The 25.2.3 scratch harness is *not* the SDK and is not kept in the repository.

**Recommended first subtask: 25.2.1.** It is the only one that changes existing behaviour (the `Claim` signature and the mode switch), it has no network surface, and its regression criteria protect Task 25.1 before any remote code exists.

## 10. Unresolved questions and known gaps

1. **Server-enforced execution deadline.** v0 trusts the SDK to enforce `deadline_seconds`. A hung handler with a healthy heartbeat holds its job forever. Server-side enforcement (renewals refused after a maximum runtime measured from `attempt_started_at`) would change `RenewLease` semantics and its meaning of 409; decide whether it belongs in v0 or the first follow-up.
2. **Lost claim response.** Accept the consumed attempt (v0 choice) or add a best-effort `Release` when the server detects a failed response write. The latter cannot catch every loss, so it is an optimisation, not a guarantee.
3. **Idempotent replay of `complete`/`fail`/`release`.** Today an already-committed repeat is indistinguishable from lost ownership (409). Supporting an idempotent 200 on replay would need a read of the job's current claim data; no such method exists. Postponed.
4. **Customer-provided error summaries.** v0 stores only fixed server-chosen text. A customer-facing diagnostic field needs size limits, redaction guidance and a decision on who may read it.
5. **Result and input visibility.** `result` is readable through the unauthenticated `GET /v1/jobs` API. Acceptable only because that API is local-only in v0; revisit with authentication.
6. **Long-poll mechanism.** Polling the database every `FORGE_POLL_INTERVAL` per waiting request is simple and bounded by `FORGE_WORKER_MAX_POLLS`, but it scales with waiting workers. `LISTEN/NOTIFY` is a possible later optimisation; measure first.
7. **Fairness across several requested types** in one claim, and **per-worker concurrency limits**, are not specified beyond "one claim request per free slot". Needed when service registration exists.
8. **Allowlist changes require a restart** and the in-flight rows of a removed type stay queued indefinitely. Acceptable for a development stand-in.
9. **Mode change hygiene.** Moving a database between modes with running (not just queued) jobs should be done after draining; the spec guarantees queued rows are preserved but does not attempt to migrate running claims.
10. **Transport.** HTTPS JSON long polling is specified because it needs no new dependency and fits an outbound-only worker. gRPC streaming can replace it later without changing the semantics above.
11. **SDK language scope** and compatibility policy are open and belong to the SDK task.
12. **`task.md` is stale** (it still describes the Phase 1 baseline). It is deliberately left untouched by this task; it should be refreshed in a documentation-only change.

No product question blocks the first subtask.
