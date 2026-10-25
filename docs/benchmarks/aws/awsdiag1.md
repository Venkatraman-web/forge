# Forge remote load run `awsdiag1`

Started 2026-10-22 18:58:47 UTC, ended 18:59:48 UTC. Raw data: `remote.json`.

## What this is

Open-loop load (a fixed arrival rate, whatever Forge does) sent to an **already running** Forge at the address below, executed by workers started separately. Nothing here touches Forge's database or processes. Server-side numbers are the **difference between two scrapes of Forge's own `/metrics`**; latency quantiles are **estimated from histogram buckets** (linear inside a bucket), so they are coarser than the database-timestamp figures of the local E-1 reports. Stages are separated by waiting for the queue to drain, not by resetting anything. The counters are per Forge process, so this only works against a single Forge process; a restart mid-stage fails the stage.

| Setting | Value |
|---|---|
| Forge (submission API) | http://172.31.4.71:8080 |
| Forge (operator API) | http://172.31.4.71:8082 |
| Job types | bench.a, bench.b |
| Stage length | 30 s, 1 repetition(s) per rate, rate order rotated each repetition |
| Payload | about 256 bytes of input |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description) | DIAGNOSTIC ONLY: synchronous_commit=off on forge-db (commits not waiting for the disk); not durable |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-600-r1 | 600 | 600.0 | 0 | 0 | 1.0 / 5.3 / 57.0 | 598.8 | 30 / 162 | 67 | 0.3 | yes |
| sweep-1000-r1 | 1000 | 1000.0 | 0 | 0 | 1.2 / 6.9 / 67.0 | 999.0 | 20 / 99 | 445 | 0.3 | yes |

### sweep-600-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 1.1 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 162 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=18000, events:succeeded=18000, submissions:accepted=18000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 48 ms, p99 50 ms over 18000 attempts

### sweep-1000-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.6 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 99 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=30000, events:succeeded=30000, submissions:accepted=30000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 48 ms, p99 50 ms over 30000 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
