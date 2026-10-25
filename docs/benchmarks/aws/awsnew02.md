# Forge remote load run `awsnew02`

Started 2026-10-22 22:18:26 UTC, ended 22:20:10 UTC. Raw data: `remote.json`.

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
| Deployment (operator's description) | AWS ap-southeast-2a, 4x t3.small (unlimited); Forge on forge, PostgreSQL 16 on forge-db (durable commits, synchronous_commit=on), synthetic workers 3 procs x 32 slots (25 ms handler) on cart-workers, generator on cart-web; variant new; session 5 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-2000-r1 | 2000 | 1980.7 | 0 | 870 | 4.8 / 249.3 / 515.2 | 1972.0 | 285 / 794 | 1193 | 0.3 | no |
| sweep-2800-r1 | 2800 | 2572.6 | 0 | 8930 | 506.4 / 514.2 / 517.2 | 2023.3 | 7250 / 26442 | 24724 | 13.8 | no |

### sweep-2000-r1

- NOT: accepted 89130 of 90000 offered; queue_full 0; other errors 870
- ok: the generator kept to its schedule
- ok: backlog slope 15.7 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 794 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=89130, events:succeeded=89130, submissions:accepted=89130, submissions:admission_busy=870
- Estimated attempt duration (claim to recorded outcome): p50 31 ms, p95 49 ms, p99 84 ms over 89130 attempts

### sweep-2800-r1

- NOT: accepted 117070 of 126000 offered; queue_full 0; other errors 8930
- ok: the generator kept to its schedule
- NOT: backlog still growing at 451.0 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 13.8 s after the load
- NOT: estimated p95 queue wait 26442 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=117070, events:succeeded=117070, submissions:accepted=117070, submissions:admission_busy=8930
- Estimated attempt duration (claim to recorded outcome): p50 31 ms, p95 49 ms, p99 84 ms over 117070 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
