# E-1 Forge-isolated capacity baseline — run `benchpost3`

Started 2026-10-22 15:01:40 UTC · duration 8m58s · raw data: `results/benchpost3/e1.json`

Generated from the raw result. This report states what was measured and how; the interpretation (likely bottleneck, hypotheses) is kept separate from it.

## 1. What this experiment is

Jobs are submitted **directly to Forge's `POST /v1/jobs`** by an **open-loop** generator (a fixed arrival rate: request *i* is scheduled at *i / rate* seconds after the stage start, whatever the service does) and executed by Forge SDK workers running a **synthetic handler**. There is no ForgeCart process, database, outbox or dispatcher in the run. Queue state is read through the authenticated operator endpoint (`GET /v1/queues`); latencies come from the job rows' own timestamps; final state is reconciled against the database. Everything runs on **one host**, so this isolates Forge from ForgeCart's business pipeline. It is **not** a multi-host or production capacity test.

**Metric definitions.**

- *Offered rate*: the scheduled arrival rate. *Submission throughput (accepted/s)*: `202` responses ÷ the send window (stage start to the last response). Not the same as completion or drain throughput.
- *Submit latency*: time from sending the request to receiving the response (service time), for `202` responses. *Send lateness*: how far behind its schedule the generator actually sent; large lateness means the generator, not Forge, limited the offered load.
- *Completion throughput during load*: jobs whose `finished_at` is at or before the end of the load ÷ load duration (authoritative job timestamps).
- *Drain throughput*: jobs still unfinished at the end of the load ÷ the time from the end of the load to the last `finished_at`. It describes the system working off a backlog and is reported separately.
- *Queue wait* = `started_at − created_at` (first start); *execution* = `finished_at − started_at`; *end-to-end* = `finished_at − created_at`. "Last half" means jobs created in the second half of the load, to leave out start-up effects.
- *Queue depth / oldest due-job age*: the operator endpoint's `totals.queued` and `totals.oldest_due_job_age_seconds`, polled once per second.

**Assumptions made where the design (docs/decisions/0004 section 8.2) leaves details open.** The design fixes only: direct open-loop submission, SDK workers, synthetic handlers of configurable duration. Everything below is therefore this experiment's own choice and is part of the result:

| Setting | Value | Why / note |
| --- | --- | --- |
| Arrival rates (sweep) | [400 600 800 1000] jobs/s | geometric steps spanning the requirements' option A (25), B (100) and C (≥250) toward saturation |
| Stage length | 60 s | long enough for a steady half; short of the 10-minute G-P soak |
| Sustained stage | 80% of the highest rate classified sustainable, 0 s | rule fixed before the run; chosen rate: 0 |
| Handler | 25 ms fixed wait, no I/O | matches ForgeCart's measured ~25 ms median; D-1 (the real workload) is undecided |
| Payload | ~256 bytes of input | small JSON; sizes and type mixes are configurable but one size was run |
| Job types | bench.a, bench.b (alternating) | two types exercise the type filter |
| Workers | 2 processes × 24 slots = 48 slots | below Forge's default poll cap of 64; slot ceiling ≈ 1920 jobs/s at this handler time |
| Queue cap | FORGE_MAX_QUEUED=200000 (default 1000) | raised so saturation shows as queue growth rather than rejection; not the default |
| Fresh state per stage | yes | database dropped, migrated and all processes restarted for every stage |
| Operator polling | every 1000 ms | its latency is reported per stage |
| Generator bounds | ≤ 4000 requests in flight; lateness threshold 50 ms | a send that cannot start on time is sent late and counted, never dropped |

**Stage classification (written before the run).** A stage is *sustainable* if (1) every offered request was accepted with no error, (2) the generator stayed on schedule (≤1% of sends later than the threshold, in-flight bound never reached), (3) the operator-observed backlog grew by at most 2% of the arrival rate per second over the second half, and (4) p95 queue wait of second-half jobs was at most 5 s. These thresholds are this experiment's own assumptions, used only to choose the sustained rate; they are not service-level objectives.

## 2. Environment

| Item | Value |
| --- | --- |
| Host | AMD Ryzen 5 220 w/ Radeon 740M Graphics, 12 logical CPUs, 14488 MB, kernel 7.0.0-38-generic |
| Host boot / uptime at start / load average | 2026-10-22T16:05:35+05:30 / 4h26m7s / 1.31 2.89 3.97 |
| CPU governor | powersave |
| Container engine | Docker Desktop (server 29.8.2); VM CPUs 12, memory 3609378816 bytes; qemu-system-x86 running for 2h15m14s |
| PostgreSQL | 16.15 in one container, limited to 4 CPUs and 2g memory, **tmpfs (RAM-backed): no disk I/O cost is represented**, max_connections 400 |
| Forge | remote execution; FORGE_MAX_QUEUED=200000 (default 1000); worker poll cap, lease, heartbeat, safety margin, poll interval, reaper and retry settings at their defaults; database pool fixed at 10 connections |
| Source tree fingerprint | 1763bec0a0335f149729a54fdeb6196705a92aa536ed8427a2d3925df143f2f1 (118 files) |
| ForgeCart components present | false |
| Operator API | loopback listener, bearer key generated for this run (not recorded) |
| atlas | atlas version v1.3.0 |
| docker | Docker version 29.9.1, build dfc520d |
| docker_compose | Docker Compose version v5.6.0 |
| docker_server | 29.8.2 |
| go | go version go1.26.8 linux/amd64 |
| postgres_image | postgres:16-alpine (present) |

**Measured executables**

| Binary | sha256 | Go build info |
| --- | --- | --- |
| forge | 50cd15fae92719e27f267edcbc85bcc070bf232fab5102bd1ddeee16e17b1040 | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=8675fd70f4c34dec8d7300f8574b0b2c67d6fa79; vcs.time=2026-10-22T14:54:21Z; vcs.modified=true |
| synthworker | 881d5f81e718f248c1ce6717ee887b298292ce44ed65ba4c06048a3fe471fe88 | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=8675fd70f4c34dec8d7300f8574b0b2c67d6fa79; vcs.time=2026-10-22T14:54:21Z; vcs.modified=true |

Generator, Forge, workers and PostgreSQL share one host and one container VM, so they compete for CPU; the PostgreSQL volume is RAM-backed, so disk I/O cost is **not** represented.

## 3. Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 / p99, last half (ms) | End-to-end p95, last half (ms) | Queue depth at load end (peak) | Oldest due age peak (s) | Backlog slope, 2nd half (jobs/s) | Drain (s) | Drain throughput (jobs/s) | Sustainable? |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| sweep-400-r1 | 400 | 400.0 | 0 | 0 | 1.0 / 1.8 / 3.1 | 399.6 | 15.1 / 51.2 / 96.4 | 78.8 | 7 (348) | 0.9 | 0.3 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-600-r1 | 600 | 600.0 | 0 | 0 | 1.1 / 2.8 / 11.1 | 598.0 | 25.6 / 82.5 / 140.7 | 110.3 | 111 (549) | 0.9 | 0.3 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-800-r1 | 800 | 800.0 | 0 | 0 | 1.5 / 16.8 / 98.2 | 798.9 | 31.5 / 198.0 / 350.4 | 227.6 | 45 (746) | 0.9 | -1.5 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-1000-r1 | 1000 | 996.3 | 0 | 224 | 9.2 / 280.6 / 426.7 | 993.9 | 310.2 / 698.8 / 785.1 | 733.9 | 113 (932) | 0.9 | -10.3 | 0.1 | n/a (backlog worked off in under 1 s) | no |
| sweep-600-r2 | 600 | 600.0 | 0 | 0 | 1.1 / 2.9 / 17.6 | 599.0 | 16.2 / 153.1 / 387.5 | 181.7 | 19 (553) | 0.9 | 0.0 | 0.0 | n/a (backlog worked off in under 1 s) | yes |
| sweep-800-r2 | 800 | 800.0 | 0 | 0 | 1.3 / 29.1 / 76.4 | 799.3 | 16.5 / 224.1 / 359.8 | 252.7 | 24 (746) | 0.9 | -1.8 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-1000-r2 | 1000 | 1000.0 | 0 | 0 | 1.9 / 189.7 / 411.8 | 998.5 | 127.3 / 732.6 / 1004.1 | 763.7 | 56 (952) | 1.0 | 4.5 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-400-r2 | 400 | 400.0 | 0 | 0 | 1.3 / 2.1 / 4.0 | 399.7 | 20.0 / 58.7 / 101.1 | 86.4 | 18 (350) | 0.9 | 0.5 | 0.0 | n/a (backlog worked off in under 1 s) | yes |

### sweep-400-r1 — 400 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 24000 / 24000 / 24000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.999 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 1.0 / 1.8 / 3.1 / 33.3 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 1.0 / 1.9 / 3.5 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.09 / 5.12; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 24000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 23974 / 26 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 15.4 / 82.0 / 522.2 / 967.1 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 27.1 / 28.5 / 30.1 / 48.2 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 42.7 / 109.1 / 548.9 / 996.2 |
| `jobs` table size at the end | 13.4 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 34.8 % | 56.5 % | — |
| Forge process | 0.44 cores | 0.77 cores | 29 MB |
| Workers (all processes) | 0.25 cores | 0.44 cores | 33 MB |
| Load generator (this process) | 0.12 cores | 0.14 cores | 58 MB |
| PostgreSQL container | 64 % of one core (13 readings) | 90 % | 193 MB |
| PostgreSQL client connections (total / active), max | 10 / 7 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.6 / 3.2 / 5.5 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 12000 | 12000 | 1.0 / 2.8 | 4 |
| 30 | 12000 | 12000 | 1.0 / 3.5 | 7 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 24000 | `workerapi: job N generation N: completed by worker N` |
| 12000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 12000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 24000 | `job_started` |
| 24000 | `Nompleted` |
| 2 | `worker_started` |
| 2 | `synthworker configured` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.3 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 51 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 24000, sent 24000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 24000 + 0 + 0 vs 24000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 24000, distinct 24000 |
| database rows = accepted submissions | PASS | rows 24000, accepted 24000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:24000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 24000 / 59.999 s = 400.005/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 23974 / 59.999 s |
| per-second completion timeline sums to the finished jobs | PASS | 24000 vs 24000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:12000 bench.b:12000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-600-r1 — 600 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 36000 / 36000 / 36000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 60.000 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 1.1 / 2.8 / 11.1 / 78.1 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 1.1 / 3.1 / 14.3 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.10 / 6.61; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 36000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 35878 / 122 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 25.0 / 111.2 / 595.8 / 965.0 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 27.2 / 29.4 / 32.0 / 78.1 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 52.5 / 139.3 / 622.5 / 993.3 |
| `jobs` table size at the end | 20.1 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 52.2 % | 75.8 % | — |
| Forge process | 0.69 cores | 1.30 cores | 30 MB |
| Workers (all processes) | 0.40 cores | 0.71 cores | 33 MB |
| Load generator (this process) | 0.18 cores | 0.22 cores | 55 MB |
| PostgreSQL container | 101 % of one core (13 readings) | 172 % | 255 MB |
| PostgreSQL client connections (total / active), max | 10 / 6 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.9 / 3.5 / 5.7 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 18000 | 18000 | 1.1 / 6.5 | 3 |
| 30 | 18000 | 18000 | 1.1 / 14.3 | 25 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 36000 | `workerapi: job N generation N: completed by worker N` |
| 18000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 18000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 36000 | `job_started` |
| 36000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.3 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 83 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 36000, sent 36000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 36000 + 0 + 0 vs 36000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 36000, distinct 36000 |
| database rows = accepted submissions | PASS | rows 36000, accepted 36000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:36000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 36000 / 60.000 s = 599.995/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 35878 / 60.000 s |
| per-second completion timeline sums to the finished jobs | PASS | 36000 vs 36000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:18000 bench.b:18000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-800-r1 — 800 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 48000 / 48000 / 48000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 60.000 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 1.5 / 16.8 / 98.2 / 161.2 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 1.7 / 54.2 / 122.5 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.36 / 4.58; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 48000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 47934 / 66 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 24.8 / 209.2 / 720.1 / 975.9 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 28.1 / 31.5 / 35.2 / 58.1 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 53.1 / 238.7 / 750.2 / 1005.3 |
| `jobs` table size at the end | 26.7 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 72.4 % | 91.3 % | — |
| Forge process | 1.06 cores | 1.92 cores | 35 MB |
| Workers (all processes) | 0.62 cores | 1.09 cores | 34 MB |
| Load generator (this process) | 0.25 cores | 0.31 cores | 87 MB |
| PostgreSQL container | 193 % of one core (13 readings) | 317 % | 270 MB |
| PostgreSQL client connections (total / active), max | 10 / 8 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 2.4 / 4.6 / 6.9 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 24000 | 24000 | 1.4 / 21.2 | 9 |
| 30 | 24000 | 24000 | 1.7 / 122.5 | 19 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 48000 | `workerapi: job N generation N: completed by worker N` |
| 24000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 24000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 48000 | `job_started` |
| 48000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -1.5 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 198 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 48000, sent 48000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 48000 + 0 + 0 vs 48000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 48000, distinct 48000 |
| database rows = accepted submissions | PASS | rows 48000, accepted 48000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:48000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 48000 / 60.000 s = 799.993/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 47934 / 60.001 s |
| per-second completion timeline sums to the finished jobs | PASS | 48000 vs 48000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:24000 bench.b:24000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-1000-r1 — 1000 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 60000 / 60000 / 59776 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / map[http_503:224] |
| Send window | 60.001 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 9.2 / 280.6 / 426.7 / 543.3 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 44.3 / 357.8 / 456.2 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.79 / 7.46; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 59776 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 59635 / 141 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 193.2 / 676.9 / 819.2 / 964.9 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 28.9 / 34.6 / 41.2 / 99.9 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 222.3 / 707.2 / 851.1 / 998.3 |
| `jobs` table size at the end | 34.1 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 83.0 % | 91.0 % | — |
| Forge process | 1.20 cores | 1.61 cores | 56 MB |
| Workers (all processes) | 0.77 cores | 1.05 cores | 33 MB |
| Load generator (this process) | 0.29 cores | 0.34 cores | 107 MB |
| PostgreSQL container | 261 % of one core (13 readings) | 351 % | 333 MB |
| PostgreSQL client connections (total / active), max | 10 / 9 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 3.2 / 7.7 / 14.7 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 30000 | 30000 | 3.1 / 263.0 | 270 |
| 30 | 30000 | 29776 | 44.3 / 456.2 | 15 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 59776 | `workerapi: job N generation N: completed by worker N` |
| 29891 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 29885 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 59776 | `job_started` |
| 59776 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** no

- NOT: accepted 59776 of 60000 offered; queue_full 0; other errors 224
- ok: the generator kept to its schedule
- ok: backlog slope -10.3 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 699 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 60000, sent 60000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 59776 + 0 + 224 vs 60000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 59776, distinct 59776 |
| database rows = accepted submissions | PASS | rows 59776, accepted 59776, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:59776] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 59776 / 60.001 s = 996.258/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 59635 / 60.001 s |
| per-second completion timeline sums to the finished jobs | PASS | 59776 vs 59776 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:29891 bench.b:29885] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-600-r2 — 600 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 36000 / 36000 / 36000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 60.005 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 1.1 / 2.9 / 17.6 / 50.1 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 1.1 / 2.8 / 19.0 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.10 / 7.00; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 36000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 35946 / 54 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 19.2 / 135.2 / 603.6 / 974.6 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 27.1 / 29.4 / 33.2 / 53.5 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 46.5 / 164.4 / 630.5 / 1005.8 |
| `jobs` table size at the end | 21.2 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 51.4 % | 71.6 % | — |
| Forge process | 0.70 cores | 1.30 cores | 29 MB |
| Workers (all processes) | 0.40 cores | 0.74 cores | 33 MB |
| Load generator (this process) | 0.18 cores | 0.22 cores | 144 MB |
| PostgreSQL container | 101 % of one core (13 readings) | 171 % | 320 MB |
| PostgreSQL client connections (total / active), max | 10 / 4 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.8 / 4.3 / 6.0 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 18000 | 18000 | 1.2 / 17.0 | 6 |
| 30 | 18000 | 18000 | 1.1 / 19.0 | 37 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 36000 | `workerapi: job N generation N: completed by worker N` |
| 18000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 18000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 36000 | `job_started` |
| 36000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.0 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 153 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 36000, sent 36000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 36000 + 0 + 0 vs 36000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 36000, distinct 36000 |
| database rows = accepted submissions | PASS | rows 36000, accepted 36000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:36000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 36000 / 60.005 s = 599.950/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 35946 / 60.005 s |
| per-second completion timeline sums to the finished jobs | PASS | 36000 vs 36000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:18000 bench.b:18000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-800-r2 — 800 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 48000 / 48000 / 48000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 60.001 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 1.3 / 29.1 / 76.4 / 148.0 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 1.2 / 5.1 / 48.4 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.26 / 6.69; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 48000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 47957 / 43 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 26.5 / 349.2 / 698.2 / 962.3 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 27.7 / 31.5 / 35.8 / 77.5 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 54.5 / 380.1 / 727.5 / 990.4 |
| `jobs` table size at the end | 27.0 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 68.7 % | 91.8 % | — |
| Forge process | 0.97 cores | 1.90 cores | 33 MB |
| Workers (all processes) | 0.57 cores | 1.05 cores | 33 MB |
| Load generator (this process) | 0.23 cores | 0.29 cores | 90 MB |
| PostgreSQL container | 161 % of one core (13 readings) | 315 % | 326 MB |
| PostgreSQL client connections (total / active), max | 10 / 6 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 2.1 / 6.0 / 15.6 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 24000 | 24000 | 1.5 / 96.6 | 22 |
| 30 | 24000 | 24000 | 1.2 / 48.4 | 9 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 48000 | `workerapi: job N generation N: completed by worker N` |
| 24000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 24000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 48000 | `job_started` |
| 48000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -1.8 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 224 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 48000, sent 48000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 48000 + 0 + 0 vs 48000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 48000, distinct 48000 |
| database rows = accepted submissions | PASS | rows 48000, accepted 48000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:48000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 48000 / 60.001 s = 799.988/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 47957 / 60.001 s |
| per-second completion timeline sums to the finished jobs | PASS | 48000 vs 48000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:24000 bench.b:24000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-1000-r2 — 1000 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 60000 / 60000 / 60000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 60.001 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 1.9 / 189.7 / 411.8 / 473.1 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 2.9 / 284.8 / 446.3 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.47 / 12.66; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 60000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 59914 / 86 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 37.9 / 650.2 / 949.6 / 1052.4 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 28.2 / 32.0 / 36.2 / 323.7 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 66.3 / 680.9 / 982.6 / 1094.7 |
| `jobs` table size at the end | 33.8 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 79.4 % | 91.0 % | — |
| Forge process | 1.21 cores | 1.72 cores | 52 MB |
| Workers (all processes) | 0.73 cores | 1.10 cores | 33 MB |
| Load generator (this process) | 0.28 cores | 0.59 cores | 100 MB |
| PostgreSQL container | 238 % of one core (13 readings) | 362 % | 333 MB |
| PostgreSQL client connections (total / active), max | 10 / 6 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 2.5 / 5.5 / 18.4 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 30000 | 30000 | 1.6 / 66.4 | 6 |
| 30 | 30000 | 30000 | 2.9 / 446.3 | 102 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 60000 | `workerapi: job N generation N: completed by worker N` |
| 30000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 30000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 60000 | `job_started` |
| 60000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 4.5 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 733 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 60000, sent 60000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 60000 + 0 + 0 vs 60000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 60000, distinct 60000 |
| database rows = accepted submissions | PASS | rows 60000, accepted 60000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:60000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 60000 / 60.001 s = 999.977/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 59914 / 60.001 s |
| per-second completion timeline sums to the finished jobs | PASS | 60000 vs 60000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:30000 bench.b:30000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-400-r2 — 400 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 24000 / 24000 / 24000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.999 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 1.3 / 2.1 / 4.0 / 18.9 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 1.3 / 2.2 / 3.8 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.09 / 3.17; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 24000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 23983 / 17 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 20.0 / 76.8 / 548.4 / 971.0 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 27.2 / 29.0 / 31.6 / 55.9 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 47.3 / 104.8 / 576.3 / 1002.5 |
| `jobs` table size at the end | 13.8 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 39.7 % | 62.6 % | — |
| Forge process | 0.54 cores | 1.11 cores | 29 MB |
| Workers (all processes) | 0.29 cores | 0.61 cores | 33 MB |
| Load generator (this process) | 0.14 cores | 0.17 cores | 134 MB |
| PostgreSQL container | 73 % of one core (13 readings) | 145 % | 312 MB |
| PostgreSQL client connections (total / active), max | 10 / 5 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.8 / 4.1 / 7.8 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 12000 | 12000 | 1.3 / 4.2 | 1 |
| 30 | 12000 | 12000 | 1.3 / 3.8 | 18 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 24000 | `workerapi: job N generation N: completed by worker N` |
| 12000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 12000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 24000 | `job_started` |
| 24000 | `Nompleted` |
| 2 | `worker_started` |
| 2 | `synthworker configured` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.5 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 59 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 24000, sent 24000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 24000 + 0 + 0 vs 24000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 24000, distinct 24000 |
| database rows = accepted submissions | PASS | rows 24000, accepted 24000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:24000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 24000 / 59.999 s = 400.007/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 23983 / 59.999 s |
| per-second completion timeline sums to the finished jobs | PASS | 24000 vs 24000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:12000 bench.b:12000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

## 5. Limits of this experiment

- One host: the generator, Forge, two worker processes and PostgreSQL (in a container VM) share 12 logical CPUs. Component CPU figures show who used what, not what each would use alone.
- PostgreSQL storage is RAM-backed (tmpfs); disk, WAL and checkpoint costs are not represented. Requirement B-6 asks for disk-backed storage for the performance gate.
- One handler duration, one payload size, one type pair and one worker shape were run. Results do not transfer to CPU-bound, long, or large-payload jobs.
- Each stage ran once. Sessions on this host have varied by tens of percent for identical code (see the audits), so no difference between stages smaller than that variation should be read as real without interleaved repetition.
- `FORGE_MAX_QUEUED` was raised to 200000. The default of 1000 would have turned saturation into rejections (the contract validated separately); admission cost at depth is entangled with these results (experiment E-3 isolates it).
- Docker stats gives few readings per stage; PostgreSQL CPU is indicative.
- Results are single-host measurements, not a production capacity statement.

