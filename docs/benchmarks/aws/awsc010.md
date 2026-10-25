# Forge remote load run `awsc010`

Started 2026-10-22 20:12:17 UTC, ended 20:15:29 UTC. Raw data: `remote.json`.

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
| Deployment (operator's description) | AWS ap-southeast-2a, 6x t3.small; Forge with sharded queue counter + batch endpoint; PostgreSQL durable commits; jobs table emptied before this run; synthetic workers 2 procs x 24 slots (25 ms) on cart-workers; session 3 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-400-r1 | 400 | 399.5 | 0 | 0 | 3.3 / 6.3 / 44.1 | 397.9 | 27 / 49 | 38 | 0.3 | yes |
| sweep-800-r1 | 800 | 800.1 | 0 | 0 | 3.8 / 6.2 / 54.7 | 799.4 | 24 / 49 | 321 | 0.3 | yes |
| sweep-1200-r1 | 1200 | 1182.3 | 0 | 800 | 4.6 / 21.5 / 510.8 | 1181.0 | 29 / 721 | 991 | 0.3 | no |
| sweep-1600-r1 | 1600 | 1544.9 | 0 | 2480 | 5.7 / 70.0 / 541.3 | 1209.1 | 5213 / 23110 | 15059 | 11.3 | no |

### sweep-400-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.0 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 49 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=18000, events:succeeded=18000, submissions:accepted=18000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 49 ms, p99 81 ms over 18000 attempts

### sweep-800-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.1 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 49 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=36000, events:succeeded=36000, submissions:accepted=36000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 48 ms, p99 50 ms over 36000 attempts

### sweep-1200-r1

- NOT: accepted 53200 of 54000 offered; queue_full 0; other errors 800
- ok: the generator kept to its schedule
- ok: backlog slope -0.6 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 721 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=53200, events:succeeded=53200, submissions:accepted=53200, submissions:admission_busy=800
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 49 ms, p99 72 ms over 53200 attempts

### sweep-1600-r1

- NOT: accepted 69520 of 72000 offered; queue_full 0; other errors 2480
- ok: the generator kept to its schedule
- NOT: backlog still growing at 373.6 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 11.3 s after the load
- NOT: estimated p95 queue wait 23110 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=69520, events:succeeded=69520, submissions:accepted=69520, submissions:admission_busy=2480
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 49 ms, p99 75 ms over 69520 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
