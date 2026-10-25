# Forge remote load run `awsbase2`

Started 2026-10-22 22:16:04 UTC, ended 22:18:08 UTC. Raw data: `remote.json`.

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
| Deployment (operator's description) | AWS ap-southeast-2a, 4x t3.small (unlimited); Forge on forge, PostgreSQL 16 on forge-db (durable commits, synchronous_commit=on), synthetic workers 3 procs x 32 slots (25 ms handler) on cart-workers, generator on cart-web; variant base; session 5 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-2000-r1 | 2000 | 1994.6 | 0 | 0 | 9.5 / 30.2 / 81.1 | 1715.5 | 3765 / 9179 | 12506 | 7.2 | no |
| sweep-2800-r1 | 2800 | 2410.0 | 0 | 16330 | 506.9 / 521.2 / 535.1 | 1548.4 | 13190 / 28319 | 38779 | 26.0 | no |

### sweep-2000-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- NOT: backlog still growing at 276.8 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 7.2 s after the load
- NOT: estimated p95 queue wait 9179 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=90000, events:succeeded=90000, submissions:accepted=90000
- Estimated attempt duration (claim to recorded outcome): p50 33 ms, p95 83 ms, p99 97 ms over 90000 attempts

### sweep-2800-r1

- NOT: accepted 109670 of 126000 offered; queue_full 0; other errors 16330
- ok: the generator kept to its schedule
- NOT: backlog still growing at 896.1 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 26.0 s after the load
- NOT: estimated p95 queue wait 28319 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=109670, events:succeeded=109670, submissions:accepted=109670, submissions:admission_busy=16330
- Estimated attempt duration (claim to recorded outcome): p50 35 ms, p95 90 ms, p99 119 ms over 109670 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
