# Forge remote load run `awsb10`

Started 2026-10-22 19:28:31 UTC, ended 19:31:03 UTC. Raw data: `remote.json`.

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
| Deployment (operator's description) | AWS ap-southeast-2a, 6x t3.small; Forge on forge, PostgreSQL on forge-db (durable commits, synchronous_commit=on), synthetic workers 2 procs x 24 slots (25 ms) on cart-workers, generator on loadgen; session 2 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-400-r1 | 400 | 400.2 | 0 | 0 | 4.6 / 21.4 / 97.8 | 399.6 | 27 / 50 | 88 | 0.3 | yes |
| sweep-800-r1 | 800 | 609.1 | 0 | 8250 | 530.1 / 592.0 / 633.8 | 517.4 | 3977 / 9286 | 4145 | 8.4 | no |
| sweep-1200-r1 | 1200 | 603.1 | 0 | 26550 | 538.3 / 600.8 / 636.0 | 522.3 | 3980 / 9305 | 3596 | 7.3 | no |

### sweep-400-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.3 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 50 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=18000, events:succeeded=18000, submissions:accepted=18000
- Estimated attempt duration (claim to recorded outcome): p50 31 ms, p95 50 ms, p99 98 ms over 18000 attempts

### sweep-800-r1

- NOT: accepted 27750 of 36000 offered; queue_full 0; other errors 8250
- ok: the generator kept to its schedule
- NOT: backlog still growing at 81.5 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 8.4 s after the load
- NOT: estimated p95 queue wait 9286 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=27750, events:succeeded=27750, submissions:accepted=27750, submissions:admission_busy=8250
- Estimated attempt duration (claim to recorded outcome): p50 64 ms, p95 179 ms, p99 238 ms over 27750 attempts

### sweep-1200-r1

- NOT: accepted 27450 of 54000 offered; queue_full 0; other errors 26550
- ok: the generator kept to its schedule
- NOT: backlog still growing at 70.3 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 7.3 s after the load
- NOT: estimated p95 queue wait 9305 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=27450, events:succeeded=27450, submissions:accepted=27450, submissions:admission_busy=26550
- Estimated attempt duration (claim to recorded outcome): p50 64 ms, p95 176 ms, p99 236 ms over 27450 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
