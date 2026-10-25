# Forge remote load run `awsscale3`

Started 2026-10-22 20:46:07 UTC, ended 20:46:58 UTC. Raw data: `remote.json`.

## What this is

Open-loop load (a fixed arrival rate, whatever Forge does) sent to an **already running** Forge at the address below, executed by workers started separately. Nothing here touches Forge's database or processes. Server-side numbers are the **difference between two scrapes of Forge's own `/metrics`**; latency quantiles are **estimated from histogram buckets** (linear inside a bucket), so they are coarser than the database-timestamp figures of the local E-1 reports. Stages are separated by waiting for the queue to drain, not by resetting anything. The counters are per Forge process, so this only works against a single Forge process; a restart mid-stage fails the stage.

| Setting | Value |
|---|---|
| Forge (submission API) | http://172.31.4.71:8080 |
| Forge (operator API) | http://172.31.4.71:8082 |
| Job types | bench.a |
| Stage length | 40 s, 1 repetition(s) per rate, rate order rotated each repetition |
| Payload | about 256 bytes of input |
| Jobs per request | 10 (`POST /v1/jobs/batch`); rates, counts and throughput are in jobs, but **submit latency is per request** (one request carries 10 jobs) |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description) | AWS ap-southeast-2a, 6x t3.small; SCALING DEMO: job type bench.a only, offered 480 jobs/s (batch 10), each worker machine runs ONE synthetic worker process with 4 slots and a 25 ms handler (about 160 jobs/s capacity each); worker machines added one at a time; session 4 |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-480-r1 | 480 | 480.2 | 0 | 0 | 3.2 / 4.3 / 5.1 | 383.4 | 5084 / 9539 | 3850 | 10.3 | no |

### sweep-480-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- NOT: backlog still growing at 95.3 jobs/s in the second half (> 2% of the rate)
- ok: the queue drained 10.3 s after the load
- NOT: estimated p95 queue wait 9539 ms (> 5000 ms)
- Forge counter changes during the stage: events:claimed=19200, events:succeeded=19200, submissions:accepted=19200
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 48 ms, p99 50 ms over 19200 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
