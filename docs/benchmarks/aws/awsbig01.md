# Forge remote load run `awsbig01`

Started 2026-10-22 22:30:31 UTC, ended 22:33:21 UTC. Raw data: `remote.json`.

## What this is

Open-loop load (a fixed arrival rate, whatever Forge does) sent to an **already running** Forge at the address below, executed by workers started separately. Nothing here touches Forge's database or processes. Server-side numbers are the **difference between two scrapes of Forge's own `/metrics`**; latency quantiles are **estimated from histogram buckets** (linear inside a bucket), so they are coarser than the database-timestamp figures of the local E-1 reports. Stages are separated by waiting for the queue to drain, not by resetting anything. The counters are per Forge process, so this only works against a single Forge process; a restart mid-stage fails the stage.

| Setting | Value |
|---|---|
| Forge (submission API) | http://172.31.0.242:8080 |
| Forge (operator API) | http://172.31.0.242:8082 |
| Job types | bench.a, bench.b |
| Stage length | 45 s, 1 repetition(s) per rate, rate order rotated each repetition |
| Payload | about 256 bytes of input |
| Jobs per request | 10 (`POST /v1/jobs/batch`); rates, counts and throughput are in jobs, but **submit latency is per request** (one request carries 10 jobs) |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description) | AWS ap-southeast-2a, 3x t3.small + 1x m7i-flex.large for the database; Forge on forge, PostgreSQL 16 on forge-db2 (m7i-flex.large, durable commits, synchronous_commit=on), synthetic workers 3 procs x 32 slots (25 ms handler) on cart-workers, generator on cart-web; variant new; session 5 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-2000-r1 | 2000 | 2000.0 | 0 | 0 | 3.8 / 8.2 / 20.6 | 1997.1 | 358 / 1114 | 2409 | 0.3 | yes |
| sweep-2800-r1 | 2800 | 2798.9 | 0 | 0 | 4.1 / 7.7 / 29.2 | 2369.1 | 3705 / 9184 | 19265 | 8.5 | no |
| sweep-3600-r1 | 3600 | 3585.1 | 0 | 0 | 22.2 / 116.9 / 155.1 | 2350.0 | 12575 / 28258 | 55522 | 25.4 | no |

### sweep-2000-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -22.9 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 1114 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=90000, events:succeeded=90000, submissions:accepted=90000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 49 ms, p99 64 ms over 90000 attempts

### sweep-2800-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- NOT: backlog still growing at 398.5 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 8.5 s after the load
- NOT: estimated p95 queue wait 9184 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=126000, events:succeeded=126000, submissions:accepted=126000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 49 ms, p99 67 ms over 126000 attempts

### sweep-3600-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- NOT: backlog still growing at 1253.3 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 25.4 s after the load
- NOT: estimated p95 queue wait 28258 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=162000, events:succeeded=162000, submissions:accepted=162000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 49 ms, p99 64 ms over 162000 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
