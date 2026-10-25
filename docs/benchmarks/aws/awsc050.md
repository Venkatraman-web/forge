# Forge remote load run `awsc050`

Started 2026-10-22 20:15:38 UTC, ended 20:20:28 UTC. Raw data: `remote.json`.

## What this is

Open-loop load (a fixed arrival rate, whatever Forge does) sent to an **already running** Forge at the address below, executed by workers started separately. Nothing here touches Forge's database or processes. Server-side numbers are the **difference between two scrapes of Forge's own `/metrics`**; latency quantiles are **estimated from histogram buckets** (linear inside a bucket), so they are coarser than the database-timestamp figures of the local E-1 reports. Stages are separated by waiting for the queue to drain, not by resetting anything. The counters are per Forge process, so this only works against a single Forge process; a restart mid-stage fails the stage.

| Setting | Value |
|---|---|
| Forge (submission API) | http://172.31.4.71:8080 |
| Forge (operator API) | http://172.31.4.71:8082 |
| Job types | bench.a, bench.b |
| Stage length | 45 s, 1 repetition(s) per rate, rate order rotated each repetition |
| Payload | about 256 bytes of input |
| Jobs per request | 50 (`POST /v1/jobs/batch`); rates, counts and throughput are in jobs, but **submit latency is per request** (one request carries 50 jobs) |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description) | AWS ap-southeast-2a, 6x t3.small; Forge with sharded queue counter + batch endpoint; PostgreSQL durable commits; synthetic workers 2 procs x 24 slots (25 ms) on cart-workers; session 3 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

**Run stopped:** stage sweep-2500-r1 failed: metrics scrape after the stage: Get "http://172.31.4.71:8082/metrics": terminated signal received

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-1200-r1 | 1200 | 1200.9 | 0 | 0 | 9.4 / 17.3 / 74.6 | 1198.9 | 47 / 1381 | 1449 | 0.3 | yes |
| sweep-1600-r1 | 1600 | 1600.8 | 0 | 0 | 10.1 / 28.2 / 84.7 | 1236.7 | 5886 / 26026 | 16308 | 12.3 | no |
| sweep-2000-r1 | 2000 | 2000.7 | 0 | 0 | 11.6 / 101.8 / 408.9 | 1144.3 | 16111 / 50010 | 38426 | 36.7 | no |
| sweep-2500-r1 | 2500 | - | - | - | - | - | - | - | - | error: metrics scrape after the stage: Get "http://172.31.4.71:8082/metrics": terminated signal received |

### sweep-1200-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -68.1 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 1381 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=54000, events:succeeded=54000, submissions:accepted=54000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 48 ms, p99 60 ms over 54000 attempts

### sweep-1600-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- NOT: backlog still growing at 434.3 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 12.3 s after the load
- NOT: estimated p95 queue wait 26026 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=72000, events:succeeded=72000, submissions:accepted=72000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 49 ms, p99 68 ms over 72000 attempts

### sweep-2000-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- NOT: backlog still growing at 897.7 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 36.7 s after the load
- NOT: estimated p95 queue wait 50010 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=90000, events:succeeded=90000, submissions:accepted=90000
- Estimated attempt duration (claim to recorded outcome): p50 31 ms, p95 50 ms, p99 93 ms over 90000 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
