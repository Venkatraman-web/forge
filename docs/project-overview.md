# Forge: project overview

## What it is

Forge is a background-job platform written in Go. An application submits a job (a type and a JSON input); Forge stores it durably in PostgreSQL; **workers that the developer writes and runs themselves** claim jobs of the types they handle, run their own code, and report the result; the application reads the status. Forge never runs customer code. It provides the infrastructure that every application otherwise rebuilds by hand: a durable queue, leases and crash recovery, retries with backoff, timeouts, backpressure, per-job-type policy, authentication between the three kinds of caller, and metrics.

**ForgeCart**, a reference online shop (orders, a simulated payment step, an outbox, and four background workers for inventory, invoicing, fulfillment and notification), runs on top of Forge and is the test bed for everything below.

## Shape of the system

```text
 application ──submit (API key)──▶ Forge ──▶ PostgreSQL (jobs table = the queue)
                                      ▲
 customer-run workers ──claim / renew / complete / fail (worker key)
 operator / Prometheus ──queue state, effective config, /metrics (operator key)
```

- **One queue, no broker:** the `jobs` table. A worker claims with a single `FOR UPDATE SKIP LOCKED` statement; every later write is fenced by a claim generation, so a worker that lost its lease can never overwrite newer state. Execution is **at-least-once**; handlers must be idempotent.
- **Recovery:** a heartbeat renews each lease; a reaper requeues, or fails with `attempts_exhausted`, jobs whose worker stopped renewing.
- **Backpressure:** an exact queued-job limit (`queue_full`) and a bounded admission step (`admission_busy`), both retryable and both storing nothing.
- **Developer controls:** per-job and per-type retry budget and timeout (a JSON config file, strictly validated); worker concurrency and poll settings in the SDK; three separate bearer keys (submit, worker, operator).
- **Public Go SDK:** `sdk/forgeclient` (submit, batch submit, read), `sdk/workerruntime` (runs handlers with slots, heartbeat, graceful shutdown), `sdk/workerclient` (the raw worker API).
- **Operations:** Prometheus metrics (queue depth and age per type, submissions by result, job events, queue-wait and attempt-duration histograms), a Grafana dashboard, a Docker image and a one-command quickstart.

## What was measured, and how it was improved

Measurements were taken on one laptop and on six separate small AWS machines (details and limits in [`benchmarks.md`](benchmarks.md) and [`benchmarks-aws.md`](benchmarks-aws.md)). The useful story is the loop: measure, find the real limit, fix it, measure again.

| Step | Finding (measured) | Change | Result |
|---|---|---|---|
| Local sweep | submission path was the limit; lock held across about a dozen round trips | one database function does lock, count and insert in one call | about 4x locally, 7-10x with simulated database latency |
| First AWS run | flat ceiling of about 335 submissions/s; cause tested: the lock is held through each durable commit to a network disk (a RAM-backed laptop database hid this) | batch endpoint: many jobs, one transaction, one commit | at least 3,000 jobs/s accepted with no workers |
| Second AWS run | with workers claiming, only about 610 jobs/s: every claim and submission updated one counter row and waited for the previous commit | counter split into 16 shards; the queue limit stays exact | **1,200 jobs/s sustained** with workers, 2,000 accepted with zero rejections |
| ForgeCart | median queue wait about 700 ms | `FORGE_POLL_INTERVAL` 1 s to 200 ms (a developer-side setting) | about 75 ms |

Also shown on AWS: **independent scaling** (123, 252 and 383 completions/s with 1, 2 and 3 worker machines for one overloaded job type) and **crash recovery** (a worker hard-killed mid-load: all 6,000 jobs succeeded, the 20 that were running on it were requeued and run again). ForgeCart's own database, not Forge, limited the shop's foreground throughput (about 100-215 workflows/s on the small machines). Several explanations I tried for the ceilings were tested and ruled out; they are recorded in the benchmark document.

## Honest limits

- Measurements come from a few sessions on small burstable machines in one availability zone, with one to three repetitions; they are observations, not capacity guarantees, and nothing here claims production readiness or high availability.
- Forge runs as one process; several Forge processes on one database have not been validated.
- The keys separate roles and can be rotated (several named, hashed keys per role) but a key does not identify a customer: any submit key can read every job. There is no rate limiting and no tenant isolation; There is no TLS (plain HTTP).
- Job retention is opt-in (`FORGE_RETENTION_SUCCEEDED` / `FORGE_RETENTION_FAILED`; off by default, so finished jobs stay), no cancellation, no scheduled jobs, no idempotency key on batch submission (single submissions have one), no autoscaler.
- ForgeCart's dispatcher submits one job per request and does not use the batch endpoint; its payment step is a simulator.
- Validation is manual and scripted outside the repository (the project deliberately has no test files); a scenario-based regression and load harness lives in `forgecart/test_files/`.

## Where to look

[`reference.md`](reference.md) (architecture, configuration, API) and [`deploy-on-aws.md`](deploy-on-aws.md), [`decisions/`](decisions/) (design records, including why there is no broker), [`benchmarks.md`](benchmarks.md) and [`benchmarks-aws.md`](benchmarks-aws.md) (measurements), `forgecart` (the reference application and test harness).

## What I would do next

A larger database instance (the Forge database is now the limit at about 1,200 jobs/s), retention of finished jobs, batching in ForgeCart's dispatcher, validating several Forge processes on one database, then real per-user authentication and per-type queue limits.
