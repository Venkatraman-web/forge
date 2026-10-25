# Forge remote load run `awsd10`

Started 2026-10-22 19:47:43 UTC, ended 19:50:18 UTC. Raw data: `remote.json`.

## What this is

Open-loop load (a fixed arrival rate, whatever Forge does) sent to an **already running** Forge at the address below, executed by workers started separately. Nothing here touches Forge's database or processes. Server-side numbers are the **difference between two scrapes of Forge's own `/metrics`**; latency quantiles are **estimated from histogram buckets** (linear inside a bucket), so they are coarser than the database-timestamp figures of the local E-1 reports. Stages are separated by waiting for the queue to drain, not by resetting anything. The counters are per Forge process, so this only works against a single Forge process; a restart mid-stage fails the stage.

| Setting | Value |
|---|---|
| Forge (submission API) | http://172.31.4.71:8080 |
| Forge (operator API) | http://172.31.4.71:8082 |
| Job types | bench.a, bench.b |
| Stage length | 30 s, 1 repetition(s) per rate, rate order rotated each repetition |
| Payload | about 256 bytes of input |
| Jobs per request | 10 (`POST /v1/jobs/batch`); rates, counts and throughput are in jobs, but **submit latency is per request** (one request carries 10 jobs) |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description) | DIAGNOSTIC: no workers running, so only submissions touch the queue counter; clean table; AWS session 2 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

**Run stopped:** stage sweep-1200-r1 failed: the queue was not empty before the stage (jobs still queued or running); stages are not mixed

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-800-r1 | 800 | 800.2 | 0 | 0 | 3.1 / 7.1 / 21.8 | 0.0 | 0 / 0 | 24000 | 5.1 | no |
| sweep-1200-r1 | 1200 | - | - | - | - | - | - | - | - | error: the queue was not empty before the stage (jobs still queued or running); stages are not mixed |

### sweep-800-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- NOT: backlog still growing at 800.4 jobs/s in the second half (> 2% of the rate)
- NOT: the queue did not drain within the timeout
- NOT: estimated p95 queue wait 0 ms (> 5000 ms)
- Forge counter changes during the stage: submissions:accepted=24000
- Estimated attempt duration (claim to recorded outcome): p50 0 ms, p95 0 ms, p99 0 ms over 0 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
