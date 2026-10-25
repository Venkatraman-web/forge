# Forge remote load run `awsfm701`

Started 2026-10-22 23:07:33 UTC, ended 23:09:26 UTC. Raw data: `remote.json`.

## What this is

Open-loop load (a fixed arrival rate, whatever Forge does) sent to an **already running** Forge at the address below, executed by workers started separately. Nothing here touches Forge's database or processes. Server-side numbers are the **difference between two scrapes of Forge's own `/metrics`**; latency quantiles are **estimated from histogram buckets** (linear inside a bucket), so they are coarser than the database-timestamp figures of the local E-1 reports. Stages are separated by waiting for the queue to drain, not by resetting anything. The counters are per Forge process, so this only works against a single Forge process; a restart mid-stage fails the stage.

| Setting | Value |
|---|---|
| Forge (submission API) | http://172.31.3.197:8080 |
| Forge (operator API) | http://172.31.3.197:8082 |
| Job types | bench.a, bench.b |
| Stage length | 45 s, 1 repetition(s) per rate, rate order rotated each repetition |
| Payload | about 256 bytes of input |
| Jobs per request | 10 (`POST /v1/jobs/batch`); rates, counts and throughput are in jobs, but **submit latency is per request** (one request carries 10 jobs) |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description) | AWS ap-southeast-2a, 2x t3.small (workers, generator) + 2x m7i-flex.large (Forge, database); Forge on forge, PostgreSQL 16 on forge-db2 (m7i-flex.large, durable commits, synchronous_commit=on), synthetic workers 3 procs x 32 slots (25 ms handler) on cart-workers, generator on cart-web; variant new; session 5 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-2800-r1 | 2800 | 2707.8 | 0 | 4150 | 3.7 / 418.1 / 524.3 | 2345.4 | 3654 / 9086 | 16227 | 6.4 | no |
| sweep-3600-r1 | 3600 | 3154.4 | 0 | 18420 | 506.4 / 511.2 / 514.1 | 2277.3 | 9528 / 27885 | 39671 | 15.7 | no |

### sweep-2800-r1

- NOT: accepted 121850 of 126000 offered; queue_full 0; other errors 4150
- ok: the generator kept to its schedule
- NOT: backlog still growing at 297.5 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 6.4 s after the load
- NOT: estimated p95 queue wait 9086 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=121850, events:succeeded=121850, submissions:accepted=121850, submissions:admission_busy=4150
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 48 ms, p99 50 ms over 121850 attempts

### sweep-3600-r1

- NOT: accepted 143580 of 162000 offered; queue_full 0; other errors 18420
- ok: the generator kept to its schedule
- NOT: backlog still growing at 716.1 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 15.7 s after the load
- NOT: estimated p95 queue wait 27885 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=143580, events:succeeded=143580, submissions:accepted=143580, submissions:admission_busy=18420
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 48 ms, p99 50 ms over 143580 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
