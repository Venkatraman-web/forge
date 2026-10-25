# Forge remote load run `awsc001`

Started 2026-10-22 20:11:31 UTC, ended 20:12:16 UTC. Raw data: `remote.json`.

## What this is

Open-loop load (a fixed arrival rate, whatever Forge does) sent to an **already running** Forge at the address below, executed by workers started separately. Nothing here touches Forge's database or processes. Server-side numbers are the **difference between two scrapes of Forge's own `/metrics`**; latency quantiles are **estimated from histogram buckets** (linear inside a bucket), so they are coarser than the database-timestamp figures of the local E-1 reports. Stages are separated by waiting for the queue to drain, not by resetting anything. The counters are per Forge process, so this only works against a single Forge process; a restart mid-stage fails the stage.

| Setting | Value |
|---|---|
| Forge (submission API) | http://172.31.4.71:8080 |
| Forge (operator API) | http://172.31.4.71:8082 |
| Job types | bench.a, bench.b |
| Stage length | 45 s, 1 repetition(s) per rate, rate order rotated each repetition |
| Payload | about 256 bytes of input |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description) | AWS ap-southeast-2a, 6x t3.small; Forge with sharded queue counter + batch endpoint; PostgreSQL durable commits; jobs table emptied before this run; synthetic workers 2 procs x 24 slots (25 ms) on cart-workers; session 3 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-400-r1 | 400 | 337.8 | 0 | 2797 | 3.4 / 512.4 / 545.3 | 337.5 | 10 / 93 | 41 | 0.3 | no |

### sweep-400-r1

- NOT: accepted 15203 of 18000 offered; queue_full 0; other errors 2797
- ok: the generator kept to its schedule
- ok: backlog slope -0.1 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 93 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=15203, events:succeeded=15203, submissions:accepted=15203, submissions:admission_busy=2797
- Estimated attempt duration (claim to recorded outcome): p50 34 ms, p95 92 ms, p99 199 ms over 15203 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
