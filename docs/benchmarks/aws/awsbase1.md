# Forge remote load run `awsbase1`

Started 2026-10-22 22:09:35 UTC, ended 22:12:25 UTC. Raw data: `remote.json`.

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
| sweep-1200-r1 | 1200 | 1184.3 | 0 | 710 | 4.3 / 203.9 / 526.4 | 1181.5 | 394 / 957 | 1099 | 0.3 | no |
| sweep-2000-r1 | 2000 | 1962.0 | 0 | 1490 | 9.4 / 259.4 / 519.6 | 1688.0 | 3227 / 8798 | 12270 | 7.1 | no |
| sweep-2800-r1 | 2800 | 2525.9 | 0 | 11130 | 504.7 / 518.4 / 529.7 | 1599.4 | 13803 / 28380 | 41653 | 27.3 | no |

### sweep-1200-r1

- NOT: accepted 53290 of 54000 offered; queue_full 0; other errors 710
- ok: the generator kept to its schedule
- ok: backlog slope -22.8 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 957 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=53290, events:succeeded=53290, submissions:accepted=53290, submissions:admission_busy=710
- Estimated attempt duration (claim to recorded outcome): p50 33 ms, p95 84 ms, p99 100 ms over 53290 attempts

### sweep-2000-r1

- NOT: accepted 88510 of 90000 offered; queue_full 0; other errors 1490
- ok: the generator kept to its schedule
- NOT: backlog still growing at 215.1 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 7.1 s after the load
- NOT: estimated p95 queue wait 8798 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=88510, events:succeeded=88510, submissions:accepted=88510, submissions:admission_busy=1490
- Estimated attempt duration (claim to recorded outcome): p50 33 ms, p95 82 ms, p99 98 ms over 88510 attempts

### sweep-2800-r1

- NOT: accepted 114870 of 126000 offered; queue_full 0; other errors 11130
- ok: the generator kept to its schedule
- NOT: backlog still growing at 900.8 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 27.3 s after the load
- NOT: estimated p95 queue wait 28380 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=114870, events:succeeded=114870, submissions:accepted=114870, submissions:admission_busy=11130
- Estimated attempt duration (claim to recorded outcome): p50 35 ms, p95 88 ms, p99 99 ms over 114870 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
