# E-1 Forge-isolated capacity baseline — run `benchpost1`

Started 2026-10-22 14:33:11 UTC · duration 13m28s · raw data: `results/benchpost1/e1.json`

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
| Arrival rates (sweep) | [50 100 200 300] jobs/s | geometric steps spanning the requirements' option A (25), B (100) and C (≥250) toward saturation |
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
| Host boot / uptime at start / load average | 2026-10-22T16:05:35+05:30 / 3h57m39s / 0.54 4.21 5.74 |
| CPU governor | powersave |
| Container engine | Docker Desktop (server 29.8.2); VM CPUs 12, memory 3609378816 bytes; qemu-system-x86 running for 1h46m45s |
| PostgreSQL | 16.15 in one container, limited to 4 CPUs and 2g memory, **tmpfs (RAM-backed): no disk I/O cost is represented**, max_connections 400 |
| Forge | remote execution; FORGE_MAX_QUEUED=200000 (default 1000); worker poll cap, lease, heartbeat, safety margin, poll interval, reaper and retry settings at their defaults; database pool fixed at 10 connections |
| Source tree fingerprint | a581747d8f8dadee8565708081a5072fe2ebe64dfeb8a43a93432ce3deb51b2c (117 files) |
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
| forge | 4830a1d1130f0c07316448d2267f5125b30ab70052bd6de61b72c75ccdf18154 | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=247bc51f0a71727cf0a2cf16381e9c5c52fe3a0f; vcs.time=2026-10-22T14:29:30Z; vcs.modified=true |
| synthworker | 92260a10df5064557c73716c840eeaed530f8d2a2a9f1962b6344b0f6cf22f15 | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=247bc51f0a71727cf0a2cf16381e9c5c52fe3a0f; vcs.time=2026-10-22T14:29:30Z; vcs.modified=true |

Generator, Forge, workers and PostgreSQL share one host and one container VM, so they compete for CPU; the PostgreSQL volume is RAM-backed, so disk I/O cost is **not** represented.

## 3. Results by stage

| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 / p99, last half (ms) | End-to-end p95, last half (ms) | Queue depth at load end (peak) | Oldest due age peak (s) | Backlog slope, 2nd half (jobs/s) | Drain (s) | Drain throughput (jobs/s) | Sustainable? |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| sweep-50-r1 | 50 | 50.0 | 0 | 0 | 3.3 / 4.0 / 5.1 | 49.9 | 140.0 / 273.3 / 285.2 | 300.3 | 5 (14) | 0.3 | -0.2 | 0.2 | n/a (backlog worked off in under 1 s) | yes |
| sweep-100-r1 | 100 | 100.0 | 0 | 0 | 3.0 / 3.8 / 5.3 | 99.6 | 130.1 / 264.8 / 281.7 | 291.7 | 23 (52) | 0.5 | 0.7 | 0.2 | n/a (backlog worked off in under 1 s) | yes |
| sweep-200-r1 | 200 | 200.0 | 0 | 0 | 2.6 / 3.4 / 5.3 | 199.6 | 51.0 / 150.3 / 255.7 | 177.1 | 0 (145) | 0.7 | -0.1 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-300-r1 | 300 | 300.0 | 0 | 0 | 2.5 / 4.4 / 18.3 | 299.7 | 26.5 / 164.3 / 303.5 | 191.4 | 12 (247) | 0.8 | -0.2 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-100-r2 | 100 | 100.0 | 0 | 0 | 3.0 / 3.7 / 4.8 | 99.6 | 139.2 / 269.7 / 283.2 | 296.8 | 26 (46) | 0.5 | 0.4 | 0.2 | n/a (backlog worked off in under 1 s) | yes |
| sweep-200-r2 | 200 | 200.0 | 0 | 0 | 2.6 / 3.4 / 5.2 | 199.6 | 56.4 / 145.3 / 254.6 | 173.6 | 52 (152) | 0.8 | 0.5 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-300-r2 | 300 | 300.0 | 0 | 0 | 2.5 / 10.2 / 64.9 | 299.6 | 42.3 / 199.5 / 280.7 | 227.4 | 13 (242) | 0.8 | -0.0 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-50-r2 | 50 | 50.0 | 0 | 0 | 3.4 / 4.1 / 5.5 | 49.9 | 141.2 / 274.7 / 286.7 | 301.8 | 5 (15) | 0.3 | -0.2 | 0.2 | n/a (backlog worked off in under 1 s) | yes |
| sweep-200-r3 | 200 | 200.0 | 0 | 0 | 2.6 / 4.0 / 7.3 | 199.6 | 53.1 / 113.0 / 198.5 | 140.3 | 21 (151) | 0.8 | 0.1 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-300-r3 | 300 | 300.0 | 0 | 0 | 2.5 / 6.6 / 38.6 | 299.8 | 30.4 / 158.0 / 248.9 | 184.8 | 4 (245) | 0.8 | -0.2 | 0.0 | n/a (backlog worked off in under 1 s) | yes |
| sweep-50-r3 | 50 | 50.0 | 0 | 0 | 3.4 / 4.1 / 5.7 | 49.9 | 141.6 / 273.7 / 284.6 | 300.7 | 5 (14) | 0.3 | -0.2 | 0.2 | n/a (backlog worked off in under 1 s) | yes |
| sweep-100-r3 | 100 | 100.0 | 0 | 0 | 3.0 / 3.9 / 5.5 | 99.6 | 135.3 / 267.8 / 284.8 | 294.5 | 23 (49) | 0.5 | 0.5 | 0.2 | n/a (backlog worked off in under 1 s) | yes |

### sweep-50-r1 — 50 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 3000 / 3000 / 3000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.984 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 3.3 / 4.0 / 5.1 / 10.2 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 3.2 / 3.9 / 4.5 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.14 / 1.39; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 3000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 2994 / 6 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 144.2 / 277.2 / 384.8 / 976.0 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.9 / 29.2 / 31.2 / 39.0 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 171.9 / 304.3 / 423.8 / 1007.3 |
| `jobs` table size at the end | 1.7 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 11.9 % | 24.0 % | — |
| Forge process | 0.12 cores | 0.15 cores | 29 MB |
| Workers (all processes) | 0.04 cores | 0.06 cores | 32 MB |
| Load generator (this process) | 0.03 cores | 0.04 cores | 59 MB |
| PostgreSQL container | 13 % of one core (13 readings) | 18 % | 136 MB |
| PostgreSQL client connections (total / active), max | 10 / 2 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.9 / 3.3 / 4.3 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 1500 | 1500 | 3.3 / 5.4 | 10 |
| 30 | 1500 | 1500 | 3.2 / 4.5 | 5 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 3000 | `workerapi: job N generation N: completed by worker N` |
| 1500 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1500 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 3000 | `job_started` |
| 3000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.2 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 273 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 3000, sent 3000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 3000 + 0 + 0 vs 3000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 3000, distinct 3000 |
| database rows = accepted submissions | PASS | rows 3000, accepted 3000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:3000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 3000 / 59.984 s = 50.014/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 2994 / 59.984 s |
| per-second completion timeline sums to the finished jobs | PASS | 3000 vs 3000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:1500 bench.b:1500] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-100-r1 — 100 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 6000 / 6000 / 6000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.993 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 3.0 / 3.8 / 5.3 / 15.3 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 3.0 / 3.7 / 4.7 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.10 / 2.42; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 6000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 5975 / 25 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 135.4 / 270.7 / 436.2 / 973.1 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.8 / 28.6 / 31.4 / 60.9 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 162.5 / 297.7 / 464.0 / 1024.0 |
| `jobs` table size at the end | 3.4 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 19.1 % | 38.0 % | — |
| Forge process | 0.19 cores | 0.24 cores | 28 MB |
| Workers (all processes) | 0.07 cores | 0.10 cores | 32 MB |
| Load generator (this process) | 0.04 cores | 0.05 cores | 20 MB |
| PostgreSQL container | 20 % of one core (13 readings) | 28 % | 140 MB |
| PostgreSQL client connections (total / active), max | 10 / 2 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.7 / 3.1 / 3.7 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 3000 | 3000 | 3.1 / 5.7 | 10 |
| 30 | 3000 | 3000 | 3.0 / 4.7 | 23 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 6000 | `workerapi: job N generation N: completed by worker N` |
| 3000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 3000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 6000 | `job_started` |
| 6000 | `Nompleted` |
| 2 | `worker_started` |
| 2 | `synthworker configured` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.7 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 265 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 6000, sent 6000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 6000 + 0 + 0 vs 6000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 6000, distinct 6000 |
| database rows = accepted submissions | PASS | rows 6000, accepted 6000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:6000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 6000 / 59.993 s = 100.011/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 5975 / 59.993 s |
| per-second completion timeline sums to the finished jobs | PASS | 6000 vs 6000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:3000 bench.b:3000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-200-r1 — 200 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 12000 / 12000 / 12000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.998 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 2.6 / 3.4 / 5.3 / 26.9 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 2.6 / 3.3 / 4.9 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.08 / 3.20; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 12000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 11975 / 25 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 51.8 / 118.1 / 452.2 / 961.4 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.7 / 28.0 / 30.2 / 51.1 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 78.6 / 145.4 / 479.6 / 991.9 |
| `jobs` table size at the end | 6.8 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 30.2 % | 52.2 % | — |
| Forge process | 0.31 cores | 0.47 cores | 28 MB |
| Workers (all processes) | 0.12 cores | 0.22 cores | 33 MB |
| Load generator (this process) | 0.06 cores | 0.07 cores | 21 MB |
| PostgreSQL container | 33 % of one core (13 readings) | 47 % | 164 MB |
| PostgreSQL client connections (total / active), max | 10 / 4 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.4 / 2.9 / 3.3 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 6000 | 6000 | 2.6 / 5.9 | 10 |
| 30 | 6000 | 6000 | 2.6 / 4.9 | 0 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 12000 | `workerapi: job N generation N: completed by worker N` |
| 6000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 6000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 12000 | `job_started` |
| 12000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.1 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 150 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 12000, sent 12000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 12000 + 0 + 0 vs 12000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 12000, distinct 12000 |
| database rows = accepted submissions | PASS | rows 12000, accepted 12000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:12000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 12000 / 59.998 s = 200.008/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 11975 / 59.998 s |
| per-second completion timeline sums to the finished jobs | PASS | 12000 vs 12000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:6000 bench.b:6000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-300-r1 — 300 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 18000 / 18000 / 18000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 60.000 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 2.5 / 4.4 / 18.3 / 113.7 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 2.5 / 4.1 / 9.2 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.08 / 2.79; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 18000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 17980 / 20 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 26.8 / 161.3 / 505.1 / 966.8 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.9 / 28.5 / 31.1 / 62.5 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 53.8 / 188.7 / 535.4 / 997.5 |
| `jobs` table size at the end | 10.6 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 43.7 % | 62.1 % | — |
| Forge process | 0.46 cores | 0.76 cores | 29 MB |
| Workers (all processes) | 0.18 cores | 0.36 cores | 33 MB |
| Load generator (this process) | 0.09 cores | 0.11 cores | 39 MB |
| PostgreSQL container | 52 % of one core (13 readings) | 87 % | 172 MB |
| PostgreSQL client connections (total / active), max | 10 / 4 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.3 / 3.0 / 7.8 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 9000 | 9000 | 2.6 / 49.5 | 7 |
| 30 | 9000 | 9000 | 2.5 / 9.2 | 12 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 18000 | `workerapi: job N generation N: completed by worker N` |
| 9000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 9000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 18000 | `job_started` |
| 18000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.2 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 164 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 18000, sent 18000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 18000 + 0 + 0 vs 18000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 18000, distinct 18000 |
| database rows = accepted submissions | PASS | rows 18000, accepted 18000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:18000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 18000 / 60.000 s = 300.002/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 17980 / 60.000 s |
| per-second completion timeline sums to the finished jobs | PASS | 18000 vs 18000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:9000 bench.b:9000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-100-r2 — 100 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 6000 / 6000 / 6000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.994 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 3.0 / 3.7 / 4.8 / 15.3 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 3.0 / 3.7 / 4.5 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.09 / 1.77; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 6000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 5973 / 27 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 132.5 / 270.8 / 410.2 / 963.5 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.8 / 28.5 / 30.7 / 46.9 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 159.6 / 297.9 / 437.9 / 995.1 |
| `jobs` table size at the end | 3.4 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 18.5 % | 38.3 % | — |
| Forge process | 0.19 cores | 0.24 cores | 28 MB |
| Workers (all processes) | 0.07 cores | 0.10 cores | 33 MB |
| Load generator (this process) | 0.04 cores | 0.05 cores | 52 MB |
| PostgreSQL container | 20 % of one core (13 readings) | 22 % | 169 MB |
| PostgreSQL client connections (total / active), max | 10 / 5 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.8 / 3.1 / 3.6 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 3000 | 3000 | 3.0 / 5.1 | 1 |
| 30 | 3000 | 3000 | 3.0 / 4.5 | 26 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 6000 | `workerapi: job N generation N: completed by worker N` |
| 3000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 3000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 6000 | `job_started` |
| 6000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.4 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 270 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 6000, sent 6000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 6000 + 0 + 0 vs 6000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 6000, distinct 6000 |
| database rows = accepted submissions | PASS | rows 6000, accepted 6000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:6000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 6000 / 59.994 s = 100.010/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 5973 / 59.994 s |
| per-second completion timeline sums to the finished jobs | PASS | 6000 vs 6000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:3000 bench.b:3000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-200-r2 — 200 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 12000 / 12000 / 12000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.998 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 2.6 / 3.4 / 5.2 / 16.8 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 2.6 / 3.4 / 4.9 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.08 / 2.59; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 12000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 11978 / 22 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 56.2 / 143.9 / 449.0 / 971.1 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.9 / 28.1 / 30.0 / 41.4 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 83.2 / 171.7 / 477.1 / 1000.9 |
| `jobs` table size at the end | 6.7 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 30.3 % | 52.1 % | — |
| Forge process | 0.31 cores | 0.46 cores | 28 MB |
| Workers (all processes) | 0.12 cores | 0.21 cores | 33 MB |
| Load generator (this process) | 0.06 cores | 0.07 cores | 27 MB |
| PostgreSQL container | 36 % of one core (13 readings) | 45 % | 174 MB |
| PostgreSQL client connections (total / active), max | 10 / 2 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.5 / 3.0 / 6.5 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 6000 | 6000 | 2.6 / 5.5 | 9 |
| 30 | 6000 | 6000 | 2.6 / 4.9 | 52 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 12000 | `workerapi: job N generation N: completed by worker N` |
| 6000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 6000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 12000 | `job_started` |
| 12000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.5 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 145 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 12000, sent 12000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 12000 + 0 + 0 vs 12000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 12000, distinct 12000 |
| database rows = accepted submissions | PASS | rows 12000, accepted 12000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:12000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 12000 / 59.998 s = 200.005/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 11978 / 59.998 s |
| per-second completion timeline sums to the finished jobs | PASS | 12000 vs 12000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:6000 bench.b:6000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-300-r2 — 300 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 18000 / 18000 / 18000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 60.003 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 2.5 / 10.2 / 64.9 / 128.9 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 2.6 / 9.4 / 65.3 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.08 / 5.37; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 18000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 17979 / 21 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 41.9 / 262.1 / 543.9 / 964.7 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.9 / 30.0 / 34.9 / 70.9 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 68.9 / 290.5 / 572.2 / 993.6 |
| `jobs` table size at the end | 10.3 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 44.9 % | 64.1 % | — |
| Forge process | 0.47 cores | 0.74 cores | 30 MB |
| Workers (all processes) | 0.19 cores | 0.35 cores | 33 MB |
| Load generator (this process) | 0.09 cores | 0.12 cores | 39 MB |
| PostgreSQL container | 55 % of one core (13 readings) | 87 % | 188 MB |
| PostgreSQL client connections (total / active), max | 10 / 8 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.3 / 2.8 / 4.5 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 9000 | 9000 | 2.5 / 64.6 | 8 |
| 30 | 9000 | 9000 | 2.6 / 65.3 | 60 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 18000 | `workerapi: job N generation N: completed by worker N` |
| 9000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 9000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 18000 | `job_started` |
| 18000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.0 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 199 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 18000, sent 18000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 18000 + 0 + 0 vs 18000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 18000, distinct 18000 |
| database rows = accepted submissions | PASS | rows 18000, accepted 18000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:18000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 18000 / 60.003 s = 299.986/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 17979 / 60.003 s |
| per-second completion timeline sums to the finished jobs | PASS | 18000 vs 18000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:9000 bench.b:9000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-50-r2 — 50 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 3000 / 3000 / 3000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.984 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 3.4 / 4.1 / 5.5 / 15.8 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 3.4 / 4.2 / 5.5 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.13 / 1.95; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 3000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 2993 / 7 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 144.0 / 277.3 / 382.1 / 972.3 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.9 / 29.1 / 30.7 / 34.2 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 171.2 / 304.5 / 411.6 / 1001.7 |
| `jobs` table size at the end | 1.7 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 12.4 % | 24.7 % | — |
| Forge process | 0.12 cores | 0.15 cores | 28 MB |
| Workers (all processes) | 0.04 cores | 0.06 cores | 32 MB |
| Load generator (this process) | 0.03 cores | 0.04 cores | 49 MB |
| PostgreSQL container | 13 % of one core (13 readings) | 14 % | 179 MB |
| PostgreSQL client connections (total / active), max | 10 / 2 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.8 / 3.2 / 3.7 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 1500 | 1500 | 3.4 / 5.3 | 9 |
| 30 | 1500 | 1500 | 3.4 / 5.5 | 5 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 3000 | `workerapi: job N generation N: completed by worker N` |
| 1500 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 1500 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 3000 | `Nompleted` |
| 3000 | `job_started` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.2 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 275 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 3000, sent 3000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 3000 + 0 + 0 vs 3000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 3000, distinct 3000 |
| database rows = accepted submissions | PASS | rows 3000, accepted 3000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:3000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 3000 / 59.984 s = 50.013/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 2993 / 59.984 s |
| per-second completion timeline sums to the finished jobs | PASS | 3000 vs 3000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:1500 bench.b:1500] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-200-r3 — 200 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 12000 / 12000 / 12000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.998 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 2.6 / 4.0 / 7.3 / 33.8 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 2.6 / 3.8 / 6.2 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.09 / 3.36; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 12000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 11976 / 24 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 51.3 / 119.1 / 457.6 / 970.1 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.9 / 28.3 / 30.4 / 54.4 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 78.2 / 146.6 / 485.0 / 1000.7 |
| `jobs` table size at the end | 6.7 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 32.5 % | 53.0 % | — |
| Forge process | 0.32 cores | 0.44 cores | 28 MB |
| Workers (all processes) | 0.12 cores | 0.24 cores | 33 MB |
| Load generator (this process) | 0.07 cores | 0.08 cores | 22 MB |
| PostgreSQL container | 36 % of one core (13 readings) | 51 % | 184 MB |
| PostgreSQL client connections (total / active), max | 10 / 3 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.5 / 3.1 / 3.8 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 6000 | 6000 | 2.6 / 9.4 | 2 |
| 30 | 6000 | 6000 | 2.6 / 6.2 | 21 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 12000 | `workerapi: job N generation N: completed by worker N` |
| 6000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 6000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 12000 | `job_started` |
| 12000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.1 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 113 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 12000, sent 12000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 12000 + 0 + 0 vs 12000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 12000, distinct 12000 |
| database rows = accepted submissions | PASS | rows 12000, accepted 12000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:12000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 12000 / 59.998 s = 200.007/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 11976 / 59.998 s |
| per-second completion timeline sums to the finished jobs | PASS | 12000 vs 12000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:6000 bench.b:6000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-300-r3 — 300 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 18000 / 18000 / 18000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 60.000 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 2.5 / 6.6 / 38.6 / 102.0 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 2.5 / 5.3 / 18.4 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.08 / 6.30; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 18000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 17987 / 13 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 32.5 / 189.8 / 529.1 / 966.1 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.9 / 29.1 / 31.7 / 48.2 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 59.5 / 217.9 / 556.9 / 995.9 |
| `jobs` table size at the end | 10.5 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 44.9 % | 67.5 % | — |
| Forge process | 0.46 cores | 0.74 cores | 29 MB |
| Workers (all processes) | 0.19 cores | 0.35 cores | 33 MB |
| Load generator (this process) | 0.09 cores | 0.12 cores | 41 MB |
| PostgreSQL container | 54 % of one core (13 readings) | 84 % | 188 MB |
| PostgreSQL client connections (total / active), max | 10 / 7 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.4 / 2.9 / 4.9 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 9000 | 9000 | 2.5 / 59.0 | 4 |
| 30 | 9000 | 9000 | 2.5 / 18.4 | 4 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 18000 | `workerapi: job N generation N: completed by worker N` |
| 9000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 9000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 18000 | `Nompleted` |
| 18000 | `job_started` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.2 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 158 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 18000, sent 18000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 18000 + 0 + 0 vs 18000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 18000, distinct 18000 |
| database rows = accepted submissions | PASS | rows 18000, accepted 18000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:18000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 18000 / 60.000 s = 300.001/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 17987 / 60.000 s |
| per-second completion timeline sums to the finished jobs | PASS | 18000 vs 18000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:9000 bench.b:9000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-50-r3 — 50 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 3000 / 3000 / 3000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.984 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 3.4 / 4.1 / 5.7 / 12.9 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 3.3 / 4.0 / 5.0 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.14 / 3.44; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 3000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 2993 / 7 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 145.1 / 277.8 / 378.2 / 960.4 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.9 / 28.9 / 30.4 / 35.3 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 172.0 / 304.8 / 410.3 / 990.6 |
| `jobs` table size at the end | 1.8 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 13.0 % | 26.1 % | — |
| Forge process | 0.12 cores | 0.16 cores | 28 MB |
| Workers (all processes) | 0.04 cores | 0.06 cores | 32 MB |
| Load generator (this process) | 0.03 cores | 0.04 cores | 50 MB |
| PostgreSQL container | 12 % of one core (13 readings) | 14 % | 185 MB |
| PostgreSQL client connections (total / active), max | 10 / 3 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.9 / 3.3 / 3.8 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 1500 | 1500 | 3.4 / 5.9 | 11 |
| 30 | 1500 | 1500 | 3.3 / 5.0 | 5 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 3000 | `workerapi: job N generation N: completed by worker N` |
| 1500 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1500 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 3000 | `job_started` |
| 3000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope -0.2 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 274 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 3000, sent 3000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 3000 + 0 + 0 vs 3000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 3000, distinct 3000 |
| database rows = accepted submissions | PASS | rows 3000, accepted 3000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:3000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 3000 / 59.984 s = 50.014/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 2993 / 59.984 s |
| per-second completion timeline sums to the finished jobs | PASS | 3000 vs 3000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:1500 bench.b:1500] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-100-r3 — 100 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 6000 / 6000 / 6000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 59.993 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 3.0 / 3.9 / 5.5 / 12.9 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 3.0 / 3.9 / 5.5 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.11 / 2.39; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 6000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 5976 / 24 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 138.6 / 272.2 / 412.2 / 966.4 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 26.8 / 28.5 / 30.0 / 42.2 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 165.8 / 299.4 / 440.2 / 996.4 |
| `jobs` table size at the end | 3.4 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 19.6 % | 34.4 % | — |
| Forge process | 0.19 cores | 0.24 cores | 28 MB |
| Workers (all processes) | 0.07 cores | 0.11 cores | 33 MB |
| Load generator (this process) | 0.04 cores | 0.05 cores | 24 MB |
| PostgreSQL container | 20 % of one core (13 readings) | 22 % | 186 MB |
| PostgreSQL client connections (total / active), max | 10 / 6 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.7 / 3.2 / 6.8 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 3000 | 3000 | 3.0 / 5.5 | 12 |
| 30 | 3000 | 3000 | 3.0 / 5.5 | 23 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 6000 | `workerapi: job N generation N: completed by worker N` |
| 3000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 3000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 6000 | `job_started` |
| 6000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.5 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 268 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 6000, sent 6000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 6000 + 0 + 0 vs 6000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 6000, distinct 6000 |
| database rows = accepted submissions | PASS | rows 6000, accepted 6000, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:6000] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 6000 / 59.993 s = 100.011/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 5976 / 59.993 s |
| per-second completion timeline sums to the finished jobs | PASS | 6000 vs 6000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:3000 bench.b:3000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

## 5. Limits of this experiment

- One host: the generator, Forge, two worker processes and PostgreSQL (in a container VM) share 12 logical CPUs. Component CPU figures show who used what, not what each would use alone.
- PostgreSQL storage is RAM-backed (tmpfs); disk, WAL and checkpoint costs are not represented. Requirement B-6 asks for disk-backed storage for the performance gate.
- One handler duration, one payload size, one type pair and one worker shape were run. Results do not transfer to CPU-bound, long, or large-payload jobs.
- Each stage ran once. Sessions on this host have varied by tens of percent for identical code (see the audits), so no difference between stages smaller than that variation should be read as real without interleaved repetition.
- `FORGE_MAX_QUEUED` was raised to 200000. The default of 1000 would have turned saturation into rejections (the contract validated separately); admission cost at depth is entangled with these results (experiment E-3 isolates it).
- Docker stats gives few readings per stage; PostgreSQL CPU is indicative.
- Results are single-host measurements, not a production capacity statement.

