# Forge remote load run `awsfo1`

Started 2026-10-22 18:44:56 UTC, ended 18:57:04 UTC. Raw data: `remote.json`.

## What this is

Open-loop load (a fixed arrival rate, whatever Forge does) sent to an **already running** Forge at the address below, executed by workers started separately. Nothing here touches Forge's database or processes. Server-side numbers are the **difference between two scrapes of Forge's own `/metrics`**; latency quantiles are **estimated from histogram buckets** (linear inside a bucket), so they are coarser than the database-timestamp figures of the local E-1 reports. Stages are separated by waiting for the queue to drain, not by resetting anything. The counters are per Forge process, so this only works against a single Forge process; a restart mid-stage fails the stage.

| Setting | Value |
|---|---|
| Forge (submission API) | http://172.31.4.71:8080 |
| Forge (operator API) | http://172.31.4.71:8082 |
| Job types | bench.a, bench.b |
| Stage length | 60 s, 2 repetition(s) per rate, rate order rotated each repetition |
| Payload | about 256 bytes of input |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description) | AWS ap-southeast-2a, 6x t3.small: forge (Forge), forge-db (PostgreSQL 16), loadgen (this generator), synthetic workers 2 procs x 24 slots, 25 ms handler, on cart-workers; ForgeCart stopped during this run |

The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.

## Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |
|---|---|---|---|---|---|---|---|---|---|---|
| sweep-100-r1 | 100 | 100.0 | 0 | 0 | 2.3 / 4.3 / 14.9 | 99.7 | 44 / 433 | 28 | 0.3 | yes |
| sweep-200-r1 | 200 | 200.0 | 0 | 0 | 2.2 / 13.2 / 37.7 | 199.9 | 41 / 403 | 41 | 0.3 | yes |
| sweep-400-r1 | 400 | 339.9 | 0 | 3435 | 498.5 / 510.1 / 543.9 | 339.3 | 174 / 472 | 251 | 0.3 | no |
| sweep-600-r1 | 600 | 339.7 | 0 | 15447 | 506.6 / 513.4 / 554.6 | 338.8 | 206 / 485 | 174 | 0.3 | no |
| sweep-800-r1 | 800 | 343.9 | 0 | 27186 | 507.1 / 513.6 / 550.3 | 343.2 | 198 / 489 | 540 | 0.3 | no |
| sweep-1000-r1 | 1000 | 337.3 | 0 | 39587 | 507.0 / 515.2 / 555.6 | 335.8 | 244 / 491 | 382 | 0.3 | no |
| sweep-200-r2 | 200 | 200.0 | 0 | 0 | 2.3 / 6.9 / 35.8 | 199.6 | 27 / 310 | 32 | 0.3 | yes |
| sweep-400-r2 | 400 | 332.0 | 0 | 3927 | 494.7 / 510.6 / 551.8 | 330.8 | 243 / 487 | 213 | 0.5 | no |
| sweep-600-r2 | 600 | 334.0 | 0 | 15788 | 506.5 / 514.6 / 553.7 | 333.6 | 219 / 472 | 207 | 0.3 | no |
| sweep-800-r2 | 800 | 335.2 | 0 | 27709 | 507.0 / 515.1 / 555.2 | 331.4 | 224 / 498 | 243 | 0.5 | no |
| sweep-1000-r2 | 1000 | 339.7 | 0 | 39446 | 507.0 / 515.9 / 555.6 | 337.9 | 231 / 498 | 422 | 0.3 | no |
| sweep-100-r2 | 100 | 100.0 | 0 | 0 | 2.4 / 3.8 / 17.0 | 100.0 | 14 / 418 | 31 | 0.3 | yes |

### sweep-100-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.2 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 433 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=6000, events:succeeded=6000, submissions:accepted=6000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 48 ms, p99 50 ms over 6000 attempts

### sweep-200-r1

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.5 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 403 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=12000, events:succeeded=12000, submissions:accepted=12000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 49 ms, p99 68 ms over 12000 attempts

### sweep-400-r1

- NOT: accepted 20565 of 24000 offered; queue_full 0; other errors 3435
- ok: the generator kept to its schedule
- ok: backlog slope -0.7 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 472 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=20565, events:succeeded=20565, submissions:accepted=20565, submissions:admission_busy=3435
- Estimated attempt duration (claim to recorded outcome): p50 34 ms, p95 87 ms, p99 130 ms over 20565 attempts

### sweep-600-r1

- NOT: accepted 20553 of 36000 offered; queue_full 0; other errors 15447
- ok: the generator kept to its schedule
- ok: backlog slope 0.6 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 485 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=20553, events:succeeded=20553, submissions:accepted=20553, submissions:admission_busy=15447
- Estimated attempt duration (claim to recorded outcome): p50 34 ms, p95 89 ms, p99 150 ms over 20553 attempts

### sweep-800-r1

- NOT: accepted 20814 of 48000 offered; queue_full 0; other errors 27186
- ok: the generator kept to its schedule
- ok: backlog slope 0.3 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 489 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=20814, events:succeeded=20814, submissions:accepted=20814, submissions:admission_busy=27186
- Estimated attempt duration (claim to recorded outcome): p50 34 ms, p95 88 ms, p99 138 ms over 20814 attempts

### sweep-1000-r1

- NOT: accepted 20413 of 60000 offered; queue_full 0; other errors 39587
- ok: the generator kept to its schedule
- ok: backlog slope 1.3 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 491 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=20413, events:succeeded=20413, submissions:accepted=20413, submissions:admission_busy=39587
- Estimated attempt duration (claim to recorded outcome): p50 35 ms, p95 92 ms, p99 172 ms over 20413 attempts

### sweep-200-r2

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.1 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 310 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=12000, events:succeeded=12000, submissions:accepted=12000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 49 ms, p99 67 ms over 12000 attempts

### sweep-400-r2

- NOT: accepted 20073 of 24000 offered; queue_full 0; other errors 3927
- ok: the generator kept to its schedule
- ok: backlog slope -0.4 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.5 s after the load
- ok: estimated p95 queue wait 487 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=20073, events:succeeded=20073, submissions:accepted=20073, submissions:admission_busy=3927
- Estimated attempt duration (claim to recorded outcome): p50 36 ms, p95 93 ms, p99 187 ms over 20073 attempts

### sweep-600-r2

- NOT: accepted 20212 of 36000 offered; queue_full 0; other errors 15788
- ok: the generator kept to its schedule
- ok: backlog slope -0.1 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 472 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=20212, events:succeeded=20212, submissions:accepted=20212, submissions:admission_busy=15788
- Estimated attempt duration (claim to recorded outcome): p50 34 ms, p95 89 ms, p99 154 ms over 20212 attempts

### sweep-800-r2

- NOT: accepted 20291 of 48000 offered; queue_full 0; other errors 27709
- ok: the generator kept to its schedule
- ok: backlog slope -0.2 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.5 s after the load
- ok: estimated p95 queue wait 498 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=20291, events:succeeded=20291, submissions:accepted=20291, submissions:admission_busy=27709
- Estimated attempt duration (claim to recorded outcome): p50 35 ms, p95 91 ms, p99 173 ms over 20291 attempts

### sweep-1000-r2

- NOT: accepted 20554 of 60000 offered; queue_full 0; other errors 39446
- ok: the generator kept to its schedule
- ok: backlog slope 1.4 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 498 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=20554, events:succeeded=20554, submissions:accepted=20554, submissions:admission_busy=39446
- Estimated attempt duration (claim to recorded outcome): p50 35 ms, p95 90 ms, p99 163 ms over 20554 attempts

### sweep-100-r2

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.3 jobs/s over the second half (<= 2% of the rate)
- ok: the queue drained 0.3 s after the load
- ok: estimated p95 queue wait 418 ms (<= 5000 ms)
- Forge counter changes during the stage: events:claimed=6000, events:succeeded=6000, submissions:accepted=6000
- Estimated attempt duration (claim to recorded outcome): p50 30 ms, p95 49 ms, p99 68 ms over 6000 attempts

## Limits of this measurement

- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.
- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
