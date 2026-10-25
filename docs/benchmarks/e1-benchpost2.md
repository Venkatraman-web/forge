# E-1 Forge-isolated capacity baseline — run `benchpost2`

Started 2026-10-22 14:46:50 UTC · duration 6m46s · raw data: `results/benchpost2/e1.json`

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
| Arrival rates (sweep) | [400 500 650] jobs/s | geometric steps spanning the requirements' option A (25), B (100) and C (≥250) toward saturation |
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
| Host boot / uptime at start / load average | 2026-10-22T16:05:35+05:30 / 4h11m18s / 2.54 2.93 4.01 |
| CPU governor | powersave |
| Container engine | Docker Desktop (server 29.8.2); VM CPUs 12, memory 3609378816 bytes; qemu-system-x86 running for 2h0m24s |
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
| sweep-400-r1 | 400 | 399.8 | 0 | 0 | 2.9 / 66.7 / 115.9 | 399.4 | 201.2 / 391.9 / 467.5 | 420.0 | 3 (346) | 0.9 | 0.0 | 0.1 | n/a (backlog worked off in under 1 s) | yes |
| sweep-500-r1 | 500 | 467.2 | 0 | 1728 | 479.8 / 507.3 / 514.9 | 465.5 | 739.0 / 931.6 / 983.8 | 960.5 | 178 (441) | 0.9 | -0.7 | 0.1 | n/a (backlog worked off in under 1 s) | no |
| sweep-650-r1 | 650 | 445.4 | 0 | 12054 | 505.6 / 512.7 / 517.9 | 442.0 | 762.4 / 977.7 / 1049.9 | 1006.1 | 52 (573) | 0.9 | -0.4 | 0.4 | n/a (backlog worked off in under 1 s) | no |
| sweep-500-r2 | 500 | 439.1 | 0 | 3450 | 498.2 / 511.6 / 518.2 | 436.4 | 740.7 / 982.6 / 1099.2 | 1011.6 | 63 (432) | 0.9 | -0.5 | 0.3 | n/a (backlog worked off in under 1 s) | no |
| sweep-650-r2 | 650 | 424.0 | 0 | 13345 | 505.8 / 513.8 / 519.1 | 422.1 | 776.0 / 1053.9 / 1177.2 | 1084.3 | 93 (558) | 0.9 | -1.3 | 0.6 | n/a (backlog worked off in under 1 s) | no |
| sweep-400-r2 | 400 | 347.8 | 0 | 3130 | 397.1 / 512.9 / 527.1 | 344.8 | 567.9 / 952.6 / 1122.1 | 983.0 | 173 (319) | 0.9 | 0.9 | 0.2 | n/a (backlog worked off in under 1 s) | no |

### sweep-400-r1 — 400 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 24000 / 24000 / 24000 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / none |
| Send window | 60.036 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 2.9 / 66.7 / 115.9 / 202.7 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 2.8 / 56.5 / 75.8 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.12 / 5.22; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 24000 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 23980 / 20 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 210.2 / 455.0 / 661.3 / 969.5 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 27.5 / 31.5 / 35.3 / 68.6 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 238.3 / 483.4 / 689.2 / 998.7 |
| `jobs` table size at the end | 13.6 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 57.4 % | 71.5 % | — |
| Forge process | 0.65 cores | 0.91 cores | 31 MB |
| Workers (all processes) | 0.28 cores | 0.45 cores | 33 MB |
| Load generator (this process) | 0.12 cores | 0.17 cores | 60 MB |
| PostgreSQL container | 84 % of one core (13 readings) | 113 % | 193 MB |
| PostgreSQL client connections (total / active), max | 10 / 6 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.5 / 4.6 / 6.3 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 12000 | 12000 | 3.1 / 151.5 | 50 |
| 30 | 12000 | 12000 | 2.8 / 75.8 | 30 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 24000 | `workerapi: job N generation N: completed by worker N` |
| 12000 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 12000 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 24000 | `job_started` |
| 24000 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** yes

- ok: all offered requests accepted, no errors
- ok: the generator kept to its schedule
- ok: backlog slope 0.0 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 392 ms in the second half (<= 5000 ms)

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
| submission throughput reproduces from raw counts: accepted / send window | PASS | 24000 / 60.036 s = 399.758/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 23980 / 60.036 s |
| per-second completion timeline sums to the finished jobs | PASS | 24000 vs 24000 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:12000 bench.b:12000] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-500-r1 — 500 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 30000 / 30000 / 28272 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / map[http_503:1728] |
| Send window | 60.510 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 479.8 / 507.3 / 514.9 / 532.3 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 478.3 / 507.8 / 515.1 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.13 / 4.25; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 28272 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 28168 / 104 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 723.1 / 953.0 / 1061.5 / 1189.5 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 28.1 / 32.4 / 36.2 / 53.1 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 751.9 / 982.3 / 1090.7 / 1217.1 |
| `jobs` table size at the end | 16.8 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 64.0 % | 77.5 % | — |
| Forge process | 0.74 cores | 0.97 cores | 44 MB |
| Workers (all processes) | 0.33 cores | 0.54 cores | 33 MB |
| Load generator (this process) | 0.16 cores | 0.19 cores | 55 MB |
| PostgreSQL container | 107 % of one core (13 readings) | 149 % | 232 MB |
| PostgreSQL client connections (total / active), max | 10 / 6 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 1.8 / 5.8 / 8.6 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 15000 | 14179 | 481.4 / 514.6 | 7 |
| 30 | 15000 | 14093 | 478.3 / 515.1 | 214 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 28272 | `workerapi: job N generation N: completed by worker N` |
| 14152 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 14120 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 28272 | `job_started` |
| 28272 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** no

- NOT: accepted 28272 of 30000 offered; queue_full 0; other errors 1728
- ok: the generator kept to its schedule
- ok: backlog slope -0.7 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 932 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 30000, sent 30000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 28272 + 0 + 1728 vs 30000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 28272, distinct 28272 |
| database rows = accepted submissions | PASS | rows 28272, accepted 28272, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:28272] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 28272 / 60.510 s = 467.228/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 28168 / 60.510 s |
| per-second completion timeline sums to the finished jobs | PASS | 28272 vs 28272 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:14120 bench.b:14152] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-650-r1 — 650 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 39000 / 39000 / 26946 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / map[http_503:12054] |
| Send window | 60.503 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 505.6 / 512.7 / 517.9 / 539.1 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 505.5 / 512.5 / 517.7 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.15 / 4.91; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 26946 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 26741 / 205 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 756.5 / 990.6 / 1074.9 / 1154.8 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 28.0 / 32.4 / 37.4 / 73.8 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 785.3 / 1019.1 / 1104.8 / 1182.5 |
| `jobs` table size at the end | 16.2 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 65.2 % | 76.7 % | — |
| Forge process | 0.76 cores | 1.12 cores | 48 MB |
| Workers (all processes) | 0.33 cores | 0.65 cores | 33 MB |
| Load generator (this process) | 0.21 cores | 0.25 cores | 74 MB |
| PostgreSQL container | 107 % of one core (13 readings) | 156 % | 239 MB |
| PostgreSQL client connections (total / active), max | 10 / 9 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 2.0 / 4.8 / 20.6 (n=64) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 19500 | 13361 | 505.6 / 518.0 | 45 |
| 30 | 19500 | 13585 | 505.5 / 517.7 | 121 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 26946 | `workerapi: job N generation N: completed by worker N` |
| 13519 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 13427 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 26946 | `Nompleted` |
| 26946 | `job_started` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** no

- NOT: accepted 26946 of 39000 offered; queue_full 0; other errors 12054
- ok: the generator kept to its schedule
- ok: backlog slope -0.4 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 978 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 39000, sent 39000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 26946 + 0 + 12054 vs 39000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 26946, distinct 26946 |
| database rows = accepted submissions | PASS | rows 26946, accepted 26946, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:26946] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 26946 / 60.503 s = 445.365/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 26741 / 60.503 s |
| per-second completion timeline sums to the finished jobs | PASS | 26946 vs 26946 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:13519 bench.b:13427] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-500-r2 — 500 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 30000 / 30000 / 26550 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / map[http_503:3450] |
| Send window | 60.464 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 498.2 / 511.6 / 518.2 / 543.9 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 495.5 / 511.6 / 518.3 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.22 / 4.99; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 26550 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 26385 / 165 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 732.9 / 977.1 / 1085.8 / 1186.2 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 28.0 / 33.2 / 38.4 / 68.3 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 761.8 / 1006.4 / 1116.5 / 1213.8 |
| `jobs` table size at the end | 16.3 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 65.3 % | 77.7 % | — |
| Forge process | 0.73 cores | 1.01 cores | 44 MB |
| Workers (all processes) | 0.32 cores | 0.53 cores | 33 MB |
| Load generator (this process) | 0.17 cores | 0.20 cores | 36 MB |
| PostgreSQL container | 97 % of one core (13 readings) | 134 % | 232 MB |
| PostgreSQL client connections (total / active), max | 10 / 9 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 2.0 / 6.0 / 9.0 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 15000 | 13183 | 500.6 / 518.1 | 105 |
| 30 | 15000 | 13367 | 495.5 / 518.3 | 95 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 26550 | `workerapi: job N generation N: completed by worker N` |
| 13279 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 13271 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 26550 | `job_started` |
| 26550 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** no

- NOT: accepted 26550 of 30000 offered; queue_full 0; other errors 3450
- ok: the generator kept to its schedule
- ok: backlog slope -0.5 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 983 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 30000, sent 30000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 26550 + 0 + 3450 vs 30000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 26550, distinct 26550 |
| database rows = accepted submissions | PASS | rows 26550, accepted 26550, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:26550] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 26550 / 60.464 s = 439.105/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 26385 / 60.464 s |
| per-second completion timeline sums to the finished jobs | PASS | 26550 vs 26550 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:13279 bench.b:13271] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-650-r2 — 650 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 39000 / 39000 / 25655 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / map[http_503:13345] |
| Send window | 60.504 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 505.8 / 513.8 / 519.1 / 582.7 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 506.0 / 514.2 / 519.3 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.20 / 5.17; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 25655 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 25540 / 115 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 762.9 / 1042.2 / 1144.5 / 1296.2 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 28.2 / 33.6 / 39.6 / 86.8 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 792.1 / 1072.4 / 1177.3 / 1323.3 |
| `jobs` table size at the end | 15.8 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 65.5 % | 79.5 % | — |
| Forge process | 0.74 cores | 1.08 cores | 49 MB |
| Workers (all processes) | 0.32 cores | 0.60 cores | 34 MB |
| Load generator (this process) | 0.21 cores | 0.26 cores | 68 MB |
| PostgreSQL container | 98 % of one core (13 readings) | 151 % | 232 MB |
| PostgreSQL client connections (total / active), max | 10 / 6 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 2.0 / 5.7 / 47.1 (n=65) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 19500 | 13152 | 505.7 / 518.6 | 16 |
| 30 | 19500 | 12503 | 506.0 / 519.3 | 36 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 25655 | `workerapi: job N generation N: completed by worker N` |
| 12937 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 12718 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 25655 | `Nompleted` |
| 25655 | `job_started` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** no

- NOT: accepted 25655 of 39000 offered; queue_full 0; other errors 13345
- ok: the generator kept to its schedule
- ok: backlog slope -1.3 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 1054 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 39000, sent 39000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 25655 + 0 + 13345 vs 39000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 25655, distinct 25655 |
| database rows = accepted submissions | PASS | rows 25655, accepted 25655, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:25655] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 25655 / 60.504 s = 424.024/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 25540 / 60.504 s |
| per-second completion timeline sums to the finished jobs | PASS | 25655 vs 25655 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:12718 bench.b:12937] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

### sweep-400-r2 — 400 jobs/s for 60 s

| Measure | Value |
| --- | --- |
| Requests planned / sent / accepted | 24000 / 24000 / 20870 |
| Rejected `queue_full` (with Retry-After) / other outcomes | 0 (0) / map[http_503:3130] |
| Send window | 60.001 s |
| Submit latency, accepted: p50 / p95 / p99 / max (ms) | 397.1 / 512.9 / 527.1 / 592.6 |
| Submit latency, second half: p50 / p95 / p99 (ms) | 310.7 / 507.5 / 517.4 |
| Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits | 1.59 / 5.97; 0; 0 |
| Jobs in the database: succeeded / failed / queued / running | 20870 / 0 / 0 / 0 |
| Retried (attempt > 1) / max attempt / drain timed out | 0 / 1 / false |
| Finished by load end / unfinished at load end | 20689 / 181 |
| Queue wait, all jobs: p50 / p95 / p99 / max (ms) | 631.3 / 1014.8 / 1124.2 / 1261.6 |
| Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms) | 28.8 / 44.1 / 60.9 / 122.7 |
| End-to-end, all jobs: p50 / p95 / p99 / max (ms) | 662.5 / 1049.1 / 1162.0 / 1289.7 |
| `jobs` table size at the end | 12.7 MB |

**Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)**

| Component | Average | Max (1 s) | Memory max |
| --- | --- | --- | --- |
| Host CPU | 71.0 % | 84.2 % | — |
| Forge process | 0.68 cores | 0.89 cores | 41 MB |
| Workers (all processes) | 0.29 cores | 0.50 cores | 33 MB |
| Load generator (this process) | 0.16 cores | 0.24 cores | 67 MB |
| PostgreSQL container | 97 % of one core (13 readings) | 140 % | 228 MB |
| PostgreSQL client connections (total / active), max | 10 / 9 | — | — |
| Operator endpoint polls: latency p50 / p95 / max (ms); errors | 4.4 / 11.7 / 26.2 (n=63) | 0 | — |

**Submission by 30 s window (with the operator-reported queue depth at the window's end)**

| From (s) | Sent | Accepted | Accepted latency p50 / p99 (ms) | Queue depth at window end |
| --- | --- | --- | --- | --- |
| 0 | 12000 | 9332 | 475.6 / 535.9 | 74 |
| 30 | 12000 | 11538 | 310.7 / 517.4 | 144 |

**Forge log lines (ids and numbers normalised; ten most frequent shapes)**

| Count | Line shape |
| --- | --- |
| 20870 | `workerapi: job N generation N: completed by worker N` |
| 10435 | `workerapi: job N (bench.b) claimed by worker N: generation N, attempt N of N` |
| 10435 | `workerapi: job N (bench.a) claimed by worker N: generation N, attempt N of N` |
| 1 | `forge: operator API listening on N.N:N (read-only GET /vN/queues; bearer key; plain HTTP: use a trusted network or a TLS-terminating proxy for anything but loopback)` |
| 1 | `forge: execution mode remote; listening on N.N:N with NO embedded workers (max queued N); allowed job types: bench.a,bench.b; N job(s) already queued in the database` |
| 1 | `forge: worker API listening on N.N:N (development key authentication; claim, renew, complete, fail and release endpoints)` |
| 1 | `forge: a remote worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt` |
| 1 | `forge: lease N, heartbeat N, safety margin N; reaper every N (batch N, recovery backoff N); retry backoff N to NmN` |

**Worker log lines (all worker processes)**

| Count | Line shape |
| --- | --- |
| 20870 | `job_started` |
| 20870 | `Nompleted` |
| 2 | `synthworker configured` |
| 2 | `worker_started` |

**Classification:** no

- NOT: accepted 20870 of 24000 offered; queue_full 0; other errors 3130
- ok: the generator kept to its schedule
- ok: backlog slope 0.9 jobs/s over the second half (<= 2% of the rate)
- ok: p95 queue wait 953 ms in the second half (<= 5000 ms)

**Integrity and isolation checks**

| Check | Result | Detail |
| --- | --- | --- |
| every planned request was sent | PASS | planned 24000, sent 24000 |
| every outcome is accounted for: accepted + queue_full + other = sent | PASS | 20870 + 0 + 3130 vs 24000 |
| every acknowledged (202) submission exists in the database | PASS | missing 0 |
| the database holds no row that was not acknowledged (no lost-response duplicates) | PASS | unacknowledged rows 0 (uncertain outcomes would appear here) |
| no sequence number appears twice in the database | PASS | rows 20870, distinct 20870 |
| database rows = accepted submissions | PASS | rows 20870, accepted 20870, unacknowledged 0 |
| status counts reconcile with the row total | PASS | map[succeeded:20870] |
| every accepted job reached a terminal state (drained) | PASS | queued 0 running 0, drain timed out=false |
| all terminal jobs succeeded; none failed | PASS | failed 0 |
| every succeeded job carries the handler's result | PASS | unexpected results 0 |
| no job needed a retry (attempt > 1) | PASS | retried 0, max attempt 1 |
| submission throughput reproduces from raw counts: accepted / send window | PASS | 20870 / 60.001 s = 347.825/s |
| completion throughput reproduces from timestamps: finished by load end / load duration | PASS | 20689 / 60.001 s |
| per-second completion timeline sums to the finished jobs | PASS | 20870 vs 20870 |
| queue_counter equals count(*) of status='queued' rows (reconciliation) | PASS | counter 0, rows 0 |
| ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones | PASS | types map[bench.a:10435 bench.b:10435] |
| operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows) | PASS | operator queued=0 running=0; database queued=0 running=0 |

## 5. Limits of this experiment

- One host: the generator, Forge, two worker processes and PostgreSQL (in a container VM) share 12 logical CPUs. Component CPU figures show who used what, not what each would use alone.
- PostgreSQL storage is RAM-backed (tmpfs); disk, WAL and checkpoint costs are not represented. Requirement B-6 asks for disk-backed storage for the performance gate.
- One handler duration, one payload size, one type pair and one worker shape were run. Results do not transfer to CPU-bound, long, or large-payload jobs.
- Each stage ran once. Sessions on this host have varied by tens of percent for identical code (see the audits), so no difference between stages smaller than that variation should be read as real without interleaved repetition.
- `FORGE_MAX_QUEUED` was raised to 200000. The default of 1000 would have turned saturation into rejections (the contract validated separately); admission cost at depth is entangled with these results (experiment E-3 isolates it).
- Docker stats gives few readings per stage; PostgreSQL CPU is indicative.
- Results are single-host measurements, not a production capacity statement.

