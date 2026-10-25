# Forge remote load run `awsc10`

Started 2026-10-22 19:44:20 UTC, ended 19:47:08 UTC. Raw data: `remote.json`.

## What this is

Open-loop load (a fixed arrival rate, whatever Forge does) sent to an **already running** Forge at the address below, executed by workers started separately. Nothing here touches Forge's database or processes. Server-side numbers are the **difference between two scrapes of Forge's own `/metrics`**; latency quantiles are **estimated from histogram buckets** (linear inside a bucket), so they are coarser than the database-timestamp figures of the local E-1 reports. Stages are separated by waiting for the queue to drain, not by resetting anything. The counters are per Forge process, so this only works against a single Forge process; a restart mid-stage fails the stage.

| Setting | Value |
|---|---|
| Forge (submission API) | http://172.31.4.71:8080 |
| Forge (operator API) | http://172.31.4.71:8082 |
| Job types | bench.a, bench.b |
| Stage length | 45 s, 1 repetition(s) per rate, rate order rotated each repetition |
| Payload | about 256 bytes of input |
| Jobs per request | 10 (`POST /v1/jobs/batch`); rates, counts and throughput are in jobs, but **submit latency is per request** (one request carries 10 jobs) |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description) | AWS ap-southeast-2a, 6x t3.small; Forge on forge, PostgreSQL on forge-db (durable commits), jobs table emptied (TRUNCATE) before this run; synthetic workers 2 procs x 24 slots (25 ms) on cart-workers; session 2 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-400-r1 | 400 | 249.5 | 0 | 6570 | 382.2 / 862.5 / 1124.3 | 185.5 | 5615 / 27470 | 2906 | 14.7 | no |
| sweep-800-r1 | 800 | 608.0 | 0 | 8320 | 532.2 / 589.9 / 637.2 | 513.7 | 3711 / 9279 | 4227 | 7.3 | no |
| sweep-1200-r1 | 1200 | 615.5 | 0 | 25970 | 538.9 / 596.0 / 632.4 | 519.4 | 4437 / 9424 | 4246 | 8.9 | no |

### sweep-400-r1

- NOT: accepted 11430 of 18000 offered; queue_full 0; other errors 6570
- ok: the generator kept to its schedule
- NOT: backlog still growing at 55.5 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 14.7 s after the load
- NOT: estimated p95 queue wait 27470 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=11430, events:succeeded=11430, submissions:accepted=11430, submissions:admission_busy=6570
- Estimated attempt duration (claim to recorded outcome): p50 103 ms, p95 472 ms, p99 880 ms over 11430 attempts

### sweep-800-r1

- NOT: accepted 27680 of 36000 offered; queue_full 0; other errors 8320
- ok: the generator kept to its schedule
- NOT: backlog still growing at 109.8 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 7.3 s after the load
- NOT: estimated p95 queue wait 9279 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=27680, events:succeeded=27680, submissions:accepted=27680, submissions:admission_busy=8320
- Estimated attempt duration (claim to recorded outcome): p50 64 ms, p95 175 ms, p99 236 ms over 27680 attempts

### sweep-1200-r1

- NOT: accepted 28030 of 54000 offered; queue_full 0; other errors 25970
- ok: the generator kept to its schedule
- NOT: backlog still growing at 84.5 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 8.9 s after the load
- NOT: estimated p95 queue wait 9424 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=28030, events:succeeded=28030, submissions:accepted=28030, submissions:admission_busy=25970
- Estimated attempt duration (claim to recorded outcome): p50 64 ms, p95 178 ms, p99 237 ms over 28030 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
