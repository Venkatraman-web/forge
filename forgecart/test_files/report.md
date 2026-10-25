# ForgeCart / Forge load-test report

Run `fullpost19` · profile `full` · started 2026-10-22 21:17:22 UTC · duration 7m7s

Raw measurements: `results/fullpost19/` (suite.json and one JSON file per scenario). This report is generated from those records.

## 1. Executive summary

12 of 12 selected scenarios were executed: **12 passed, 0 failed, 0 inconclusive, 0 errored, 0 skipped, 0 interrupted**.

| Scenario | Status | Headline result |
| --- | --- | --- |
| A. Smoke test | PASSED | 10 completed, 46.0 workflows/s, workflow p95 47.4 ms; drain 1.0 s |
| B. Baseline benchmark | PASSED | median 86.5 workflows/s over 3 repetitions |
| C. Concurrency ramp | PASSED | 7 stage(s) executed |
| D. Burst / spike test | PASSED | 1 run(s) |
| E. Sustained / soak test | PASSED | 1 run(s) |
| F. Saturation test | PASSED | 3 stage(s) executed |
| G. Stock contention | PASSED | 53 completed, 64.8 workflows/s, workflow p95 344.2 ms; drain 0.8 s |
| H. Payment outcome mix | PASSED | 2 run(s) |
| I. Idempotency and duplicate delivery | PASSED | 1 run(s) |
| J. Full background pipeline and drain | PASSED | 400 completed, 85.9 workflows/s, workflow p95 269.0 ms; drain 1.0 s |
| K. Worker scaling comparison | PASSED | 4 worker configuration(s) compared |
| L. Recovery and resilience | PASSED | 6 of 6 fault tests drained completely |

**Metric names.** *Workflow throughput (workflows/s)* = completed workflows / HTTP load duration: foreground only. *Drain after load (s)* = time until every paid order has its five effects, measured from the end of the HTTP load (from the moment workers were started in the worker-scaling scenario) and polled every 250 ms. *Background drain rate (jobs/s)* = 5 x paid orders / drain seconds. *Pipeline span rate (jobs/s)* = effects / (first outbox event -> last effect): it includes the HTTP load and is not a capacity measure. *Backlog* = 5 x paid orders - effects, sampled every second (a proxy: Forge's queue depth is not exposed). Soak/trend comparisons use only full windows.

**Reading this report.** *Passed* means every correctness criterion held. Performance objectives were not specified by the owner, so measured values are reported without a pass/fail judgement ("unspecified"). *Inconclusive* means the data cannot support a conclusion (for example the load generator may have limited the run).

- **C. Concurrency ramp**: First step with a scaling limit: throughput gained less than 10% going from the previous stage to concurrency 10 (75.9 -> 75.4 workflows/s).
- **C. Concurrency ramp**: Later stages still gained: the best stage was concurrency 80 at 89.7 workflows/s (+18.9% vs the stage where the first limit appeared), so that first step was not the end of scaling.
- **C. Concurrency ramp**: The last stage lasted only 2.2 s and its peak backlog was 345 of 780 produced jobs, i.e. almost no background work had completed while it was measured. Workflow throughput from stages this short may therefore not be comparable with stages long enough to include background work (hypothesis; compare the saturation scenario).
- **F. Saturation test**: Peak workflow throughput 91.0 workflows/s at concurrency 10. Lowest tested concurrency within 5% of that peak: 10 (91.0 workflows/s, workflow p99 383 ms). This is an observation from 12 s stages, not a sustainable-capacity claim.
- **F. Saturation test**: Higher stages: concurrency 16: 80.4 workflows/s (-11.7% vs peak), p99 560 ms; concurrency 26: 88.4 workflows/s (-2.9% vs peak), p99 765 ms.
- **F. Saturation test**: The throughput plateau is already reached at the first tested concurrency (10): the knee lies at or below it and this ladder cannot locate it.
- **F. Saturation test**: Stop condition first met at concurrency 26: throughput plateau: no 5% gain over 91.0 workflows/s (the last level that gained 5%) for 2 consecutive stages.
- **F. Saturation test**: Background backlog at the end of the load was 206-408 jobs across stages (backlog = 5 x paid orders - effects, a proxy: Forge's queue depth is not exposed). The per-stage trend is shown in the table as mid -> end values and the least-squares slope over the second half of the load, next to the production rate; no yes/no threshold is applied.

## 2. Test environment and resource limits

| Item | Value |
| --- | --- |
| Host CPU | AMD Ryzen 5 220 w/ Radeon 740M Graphics, 12 logical CPUs |
| Host memory | 14488 MB |
| Kernel | 7.0.0-38-generic |
| PostgreSQL | 16.15 |
| PostgreSQL containers | two (Forge job database, ForgeCart application database); each limited to 4 CPUs and 2g memory; tmpfs storage; max_connections 400 |
| Services | Forge (remote execution mode) + ForgeCart API + dispatcher + 5 worker processes, built from this repository |
| atlas | atlas version v1.3.0 |
| docker | Docker version 29.9.1, build dfc520d |
| docker_compose | Docker Compose version v5.6.0 |
| docker_server | 29.8.2 |
| go | go version go1.26.8 linux/amd64 |
| postgres_image | postgres:16-alpine (present) |
| Host boot time / uptime at start | 2026-10-22T16:05:35+05:30 / 10h41m59s |
| Host load average at start (1/5/15 min) | 2.13 1.56 1.19 |
| CPU scaling governor | powersave |
| Container engine | Docker Desktop (server 29.8.2) |
| Container engine VM: CPUs / memory bytes / kernel | 12 / 3609370624 / 7.0.14-linuxkit |
| Container engine VM process | qemu-system-x86 running for 57m44s |
| Source tree fingerprint | a1b5f228d53d322ac7c757f9c7b6e66348cc3bddfb65d48ca16a933574bc1c51 (120 files; sha256 over (relative path, file sha256) of every .go/.sql/.hcl/.mod/.sum/.json/.yaml/.yml/.sh file under the repository root, sorted by path, excluding .git and examples/forgecart/test_files/{results,audits}) |

**Measured executables**

| Binary | sha256 | Go build info |
| --- | --- | --- |
| forge | 6699b62e882f3fc5da07f02a8e939c834f2a3ed64e835c3af967b4c52157b59d | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=1598cc6ab97f82afaeefe69a783694c641d105d2; vcs.time=2026-10-22T20:58:42Z; vcs.modified=true |
| forgecart | 440c75de0182d38541da111c4dc24f99d6e84aa8256a469e059bd586bc682fd6 | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=1598cc6ab97f82afaeefe69a783694c641d105d2; vcs.time=2026-10-22T20:58:42Z; vcs.modified=true |
| forgecart-analytics-worker | 72aa1a16512f039393dc5bd5aedfe7545c03a06371cbe05b739b6b40392744e0 | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=1598cc6ab97f82afaeefe69a783694c641d105d2; vcs.time=2026-10-22T20:58:42Z; vcs.modified=true |
| forgecart-dispatcher | d8318126d46d22acabf943f18f16792b11898fa3cc0ab35ab1939634251e2708 | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=1598cc6ab97f82afaeefe69a783694c641d105d2; vcs.time=2026-10-22T20:58:42Z; vcs.modified=true |
| forgecart-fulfillment-worker | 87daaed02863ee18dc4d617abe8e3f9ba81ad52fb3ae869781f3cfce03872d00 | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=1598cc6ab97f82afaeefe69a783694c641d105d2; vcs.time=2026-10-22T20:58:42Z; vcs.modified=true |
| forgecart-inventory-worker | ca582820e25f6da35108ff1e715150e982157162f5bf92507a8a116b98d08f60 | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=1598cc6ab97f82afaeefe69a783694c641d105d2; vcs.time=2026-10-22T20:58:42Z; vcs.modified=true |
| forgecart-invoice-worker | 05047a6926d2b819c719326ca9edbcddea7d068dfffb2735a768636b7d07cb7e | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=1598cc6ab97f82afaeefe69a783694c641d105d2; vcs.time=2026-10-22T20:58:42Z; vcs.modified=true |
| forgecart-notification-worker | b86d6ef33203b12517a41be7d125137cb0e131478dea0d5ce4d5fe4b5c8c803e | go1.26.8; -buildmode=exe; -compiler=gc; DefaultGODEBUG=containermaxprocs=0,cryptocustomrand=1,decoratemappings=0,tlssecpmlkem=0,tlssha1=1,updatemaxprocs=0,urlstrictcolons=0,x509sha256skid=0; CGO_ENABLED=1; CGO_CFLAGS=; CGO_CPPFLAGS=; CGO_CXXFLAGS=; CGO_LDFLAGS=; GOARCH=amd64; GOOS=linux; GOAMD64=v1; vcs=git; vcs.revision=1598cc6ab97f82afaeefe69a783694c641d105d2; vcs.time=2026-10-22T20:58:42Z; vcs.modified=true |

Binary hashes include embedded build paths, so equal source built in a different directory hashes differently; compare the source tree fingerprint for source identity.

The load generator, Forge, ForgeCart, the dispatcher and every worker run as processes on the same host, so they compete with each other and with Docker for CPU. Results characterize this host and sandbox, not production hardware.

Reference service configuration: Forge max queued jobs 50000 (the Forge default is 1000), worker-API max concurrent claim requests 512, lease 15s / heartbeat 5s / safety margin 10s, dispatcher batch 50 poll 200ms x1, workers 1 replica x 4 slots per type, payment simulator max delay 0s.

## 3. Scenarios executed, skipped and failed

| Scenario | Status | Duration | Trials | Reasons / notes |
| --- | --- | --- | --- | --- |
| smoke | PASSED | 4 s | 1 |  |
| baseline | PASSED | 29 s | 3 |  |
| ramp | PASSED | 61 s | 7 |  |
| burst | PASSED | 37 s | 1 |  |
| soak | PASSED | 67 s | 1 |  |
| saturation | PASSED | 55 s | 3 |  |
| stock-contention | PASSED | 5 s | 1 |  |
| payment-mix | PASSED | 17 s | 2 | The ForgeCart API process is restarted for every profile because the simulator reads its configuration at startup; the databases are rebuilt as well. |
| idempotency | PASSED | 5 s | 1 |  |
| pipeline | PASSED | 10 s | 1 |  |
| worker-scaling | PASSED | 51 s | 4 | Efficiency = (background drain rate gain) / (capacity gain); 100% would be linear scaling. Capacity is worker replicas x slots per replica, per job type. The drain clock starts after the workers were started, so jobs finished while the worker processes were starting are not excluded (see the effects-done field in the scenario JSON). Each configuration is one trial and the drain is polled every 250 ms (about +/-5-9% on a 3-5 s drain). |
| recovery | PASSED | 73 s | 6 | Forge lease settings for this scenario: lease 6 s, heartbeat 2 s, safety margin 4 s, reaper every 1 s (defaults are 15/5/10/5) so that recovery after a hard kill is observable within the drain timeout. |

**Report self-checks.** Scenario accounting: 12 selected = 12 executed (12 passed, 0 failed, 0 inconclusive, 0 interrupted) + 0 skipped + 0 errored + 0 not started. Throughput recomputed as completed / elapsed for 30 trials: largest difference from the stored value 0.080 workflows/s; every difference is within the bound set by the stored elapsed time being truncated to 1 ms unless flagged below.

*Skipped* and *not run* scenarios produced no measurements; *failed* scenarios ran and missed a criterion; *error* means the sandbox could not execute the scenario.

## 4. Workload configurations and initial conditions

**Seeds fix the offered requests only.** Customer emails, product choices, quantities and idempotency keys are reproducible from the seed. The payment outcome is a pure function of seed, order ID and key, and order IDs are random, so the *realised* payment mix (and with it the number of paid orders and background jobs) differs slightly between trials; the realised counts are reported next to the configured mix below and in each trial table.

Every trial starts from freshly rebuilt databases (both dropped, recreated from the versioned migrations, catalog seeded with the deterministic five-product seed, inventory set as listed) and freshly started services. Seeds make the offered orders reproducible.

| Scenario | Seed | Workload | Concurrency | Payment mix s/d/t % (configured) | Realised payments s/d/t (counts, all trials) | Initial stock per product | Drain timeout |
| --- | --- | --- | --- | --- | --- | --- | --- |
| smoke | 1001 | 10 workflows | 1 | 100/0/0 | 10/0/0 | 1000000 | 90 s |
| baseline | 2001 | 300 workflows x 3 repetitions | 10 | 80/12/8 | 695/125/80 | 1000000 | 120 s |
| ramp | 3001 | 200 workflows per stage | [1 2 5 10 20 40 80] | 80/12/8 | 1141/155/104 | 1000000 | 120 s |
| burst | 4001 | 10 s normal + 6 s burst + 14 s normal | 5 / burst 40 | 80/12/8 | 2247/362/212 | 1000000 | 120 s |
| soak | 5001 | 60 s | 10 | 80/12/8 | 4562/669/474 | 10000000 | 180 s |
| saturation | 6001 | 12 s per stage | 10 x1.6 up to 200 | 80/12/8 | 2529/378/247 | 10000000 | 180 s |
| stock-contention | 7001 | 400 workflows | 20 | 100/0/0 | 53/0/0 | 40 | 120 s |
| payment-mix | 8001 | 300 workflows | 10 | normal-mixed 70/20/10; success-dominant 95/3/2 | 498/62/40 | 1000000 | 120 s |
| idempotency | 9001 | 60 logical orders x 5 concurrent submissions, 20 duplicate-job events | — | 100/0/0 | — | 1000000 | 120 s |
| pipeline | 10001 | 400 workflows | 10 | 80/12/8 | 316/50/34 | 1000000 | 180 s |
| worker-scaling | 11001 | 400 workflows | 20 | 95/3/2 | 1527/40/33 | 1000000 | 240 s |
| recovery | 12001 | 200 workflows | 10 | 95/3/2 | 1132/46/22 | 1000000 | 180 s |

Workflow = one checkout request followed by one payment request. Closed-loop: each of the *concurrency* workers starts its next workflow when the previous one finishes, so the offered rate follows the system's speed; this measures what the system sustains at a given concurrency, not a fixed arrival rate.

## 5. Baseline measurements

### B. Baseline benchmark — PASSED

A modest reproducible workload (300 workflows, concurrency 10, payment mix 80% success / 12% decline / 8% temporary failure) with ample inventory and all five workers (one replica each, 4 slots) active. Repeated three times, each repetition on freshly rebuilt databases, to expose run-to-run variance.

*Executed 21:17:39 – 21:18:08 (29 s), 3 trial(s).*

**Repetitions (each on freshly rebuilt databases)**

| Trial | Completed | Workflow throughput (workflows/s) | Order p95 (ms) | Payment p95 (ms) | Workflow p50 (ms) | Workflow p95 (ms) | Workflow p99 (ms) | HTTP failure rate | Drain after load (s) | Max backlog (jobs) | Realised payments succeeded/declined/temporary |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| rep1 | 300 | 83.6 | 202.7 | 91.5 | 104.4 | 279.7 | 375.5 | 0.00% | 1.0 | 480 | 237/38/25 |
| rep2 | 300 | 86.5 | 234.8 | 103.1 | 89.8 | 286.3 | 349.9 | 0.00% | 1.0 | 470 | 223/47/30 |
| rep3 | 300 | 93.4 | 213.0 | 93.1 | 81.4 | 270.5 | 441.2 | 0.00% | 1.0 | 465 | 235/40/25 |

**Aggregation across repetitions (median, with min and max; spread = (max-min)/median)**

| Metric | Median | Min | Max | Spread |
| --- | --- | --- | --- | --- |
| Workflow throughput (workflows/s = completed / HTTP load duration) | 86.53 | 83.57 | 93.37 | 11.3% |
| Workflow p95 (ms) | 279.68 | 270.47 | 286.30 | 5.7% |
| Drain after load (s) | 1.02 | 1.01 | 1.02 | 0.3% |
| Background drain rate (jobs/s = 5 x paid orders / drain seconds) | 1157.64 | 1095.28 | 1165.19 | 6.0% |
| Pipeline span rate (jobs/s, first outbox event -> last effect; includes the HTTP load) | 262.75 | 259.91 | 282.79 | 8.7% |

Aggregation: each repetition is an independent trial on rebuilt databases with a different seed (seed + repetition number). The median across repetitions is used as the reference; the spread column shows run-to-run variability. Only three repetitions were run, so the spread is itself imprecise.

**Acceptance criteria**

| Criterion | Kind | Result | Detail |
| --- | --- | --- | --- |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 300 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 237, responses 237 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 1.0 s; effects 1185 of 1185, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 300 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 223, responses 223 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 1.0 s; effects 1115 of 1115, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 300 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 235, responses 235 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 1.0 s; effects 1175 of 1175, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| workflow p95 latency (median of repetitions) | performance | UNSPECIFIED | objective not specified by the owner; measured 279.68 ms |
| completed workflows per second (median of repetitions) | performance | UNSPECIFIED | objective not specified by the owner; measured 86.53 wf/s |

**Outcome: PASSED.**

## 6. Concurrency-ramp results

### C. Concurrency ramp — PASSED

The same workload (200 workflows per stage, 80/12/8 payment mix) at increasing concurrency, each stage on freshly rebuilt databases so stages are comparable. The ramp stops early on an error-rate or latency threshold, a timeout or a generator-saturation warning.

*Executed 21:18:08 – 21:19:08 (61 s), 7 trial(s).*

**Concurrency ramp (same workload per stage, databases rebuilt per stage)**

| Concurrency | Completed | Workflow throughput (workflows/s) | Order p95 (ms) | Payment p95 (ms) | Workflow p50 (ms) | Workflow p95 (ms) | Workflow p99 (ms) | HTTP failure rate | Drain after load (s) | Max backlog (jobs) | Realised payments succeeded/declined/temporary |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 200 | 42.0 | 24.6 | 36.1 | 16.5 | 60.1 | 77.6 | 0.00% | 0.8 | 215 | 166/19/15 |
| 2 | 200 | 59.7 | 31.1 | 43.8 | 22.8 | 78.5 | 133.0 | 0.00% | 0.5 | 377 | 177/14/9 |
| 5 | 200 | 75.9 | 78.3 | 59.9 | 57.9 | 124.2 | 161.7 | 0.00% | 1.3 | 445 | 159/23/18 |
| 10 | 200 | 75.4 | 258.8 | 88.8 | 105.8 | 328.5 | 457.0 | 0.00% | 1.3 | 490 | 166/20/14 |
| 20 | 200 | 77.0 | 266.7 | 306.8 | 224.1 | 495.9 | 557.3 | 0.00% | 1.3 | 440 | 160/22/18 |
| 40 | 200 | 85.2 | 498.1 | 612.9 | 378.8 | 934.9 | 1130 | 0.00% | 0.8 | 415 | 157/24/19 |
| 80 | 200 | 89.7 | 641.3 | 1184 | 736.2 | 1406 | 1628 | 0.00% | 1.3 | 345 | 156/33/11 |

```text
Throughput by concurrency
c1     ███████████████████                        42.0 wf/s  p95 60.1 ms
c2     ███████████████████████████                59.7 wf/s  p95 78.5 ms
c5     ██████████████████████████████████         75.9 wf/s  p95 124.2 ms
c10    ██████████████████████████████████         75.4 wf/s  p95 328.5 ms
c20    ██████████████████████████████████         77.0 wf/s  p95 495.9 ms
c40    ██████████████████████████████████████     85.2 wf/s  p95 934.9 ms
c80    ████████████████████████████████████████   89.7 wf/s  p95 1406 ms
```

**Observations**

- First step with a scaling limit: throughput gained less than 10% going from the previous stage to concurrency 10 (75.9 -> 75.4 workflows/s).
- Later stages still gained: the best stage was concurrency 80 at 89.7 workflows/s (+18.9% vs the stage where the first limit appeared), so that first step was not the end of scaling.
- The last stage lasted only 2.2 s and its peak backlog was 345 of 780 produced jobs, i.e. almost no background work had completed while it was measured. Workflow throughput from stages this short may therefore not be comparable with stages long enough to include background work (hypothesis; compare the saturation scenario).

**Acceptance criteria:** 42 evaluated — 42 passed, 0 failed, 0 unspecified. Failed and unspecified ones:

*(none: every criterion passed; the full list is in the scenario JSON)*

**Outcome: PASSED.**

## 7. Burst and sustained-load results

### D. Burst / spike test — PASSED

Normal load (concurrency 5), a short burst (concurrency 40), then normal load again. Measures the throughput spike, latency impact, errors, background backlog growth and how long the backlog takes to return to its pre-burst level.

*Executed 21:19:08 – 21:19:45 (37 s), 1 trial(s).*

**Burst phases**

| Phase | Concurrency | Window | Completed | Workflows/s | Workflow p50 | Workflow p95 | Workflow p99 | Requests | Errors |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| normal-before | 5 | 0-10 s | 917 | 90.7 | 47.1 | 117.6 | 150.8 | 1987 | 0 |
| burst | 40 | 10-16 s | 597 | 95.0 | 383.0 | 847.4 | 982.8 | 1266 | 0 |
| normal-after | 5 | 16-30 s | 1307 | 93.2 | 50.8 | 98.9 | 122.8 | 2679 | 0 |

**Backlog (jobs created but not yet completed)**

| Pre-burst level (median of last 5 s) | Peak during burst | Peak overall | Back to pre-burst level (+10%+10) |
| --- | --- | --- | --- |
| 398 | 543 | 543 | 1 s after the burst ended |

```text
Completed workflows per second and background backlog (jobs not yet completed)
  0s ████████████████████████████   127 wf/s | backlog                          0
  1s ██████████                      45 wf/s | backlog ███████████████████████  510
  2s ████████████████                74 wf/s | backlog ████████████             271
  3s ████████████████████████       112 wf/s | backlog ████████                 170
  4s █████████████                   58 wf/s | backlog ███████████████████████  518
  5s ████████████████                75 wf/s | backlog ████████████             280
  6s ██████████████████████████████ 138 wf/s | backlog ████████                 181
  7s ██████████████                  63 wf/s | backlog ███████████████████████  526
  8s ████████████████████████████   131 wf/s | backlog ████████                 174
  9s ███████████████████             89 wf/s | backlog ██████████████████████   489
 10s ███████████████                 67 wf/s | backlog ██████████████████       398
 11s ████████████████████████████   127 wf/s | backlog ████████                 181
 12s ██████████████                  65 wf/s | backlog ████████████████████     460
 13s ████████████████████            93 wf/s | backlog ████████████             263
 14s ██████████████████████████████ 136 wf/s | backlog █████████                212
 15s ███████████████                 69 wf/s | backlog ████████████████████████ 543
 16s █████████████████               76 wf/s | backlog ███████████████████      421
 17s █████████████████████████      115 wf/s | backlog ████████                 191
 18s ████████████████████            90 wf/s | backlog ███████████████████      421
 19s █████████████████               78 wf/s | backlog ██████████████████       398
 20s ████████████████████████████   128 wf/s | backlog ███████                  158
 21s ██████████████                  65 wf/s | backlog ██████████████████████   504
 22s █████████████████████           97 wf/s | backlog ███████████              258
 23s ████████████████████████       110 wf/s | backlog ████████████             266
 24s █████████████████               77 wf/s | backlog ██████████████████       406
 25s ███████████████████████        104 wf/s | backlog █████████                198
 26s ██████████████████              84 wf/s | backlog ████████████████         365
 27s █████████████████████           98 wf/s | backlog ███████████              257
 28s ███████████████████████████    123 wf/s | backlog █████████                209
 29s ███████████████                 71 wf/s | backlog ████████████████████     453
 30s ████████                        36 wf/s | backlog ██████████               229
```

**Observations**

- The burst raised the background backlog from about 398 to a peak of 543 jobs; return to the pre-burst level: 1 s after the burst ended.

**Acceptance criteria**

| Criterion | Kind | Result | Detail |
| --- | --- | --- | --- |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 2821 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 2247, responses 2247 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 1.1 s; effects 11235 of 11235, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |

**Outcome: PASSED.**

### E. Sustained / soak test — PASSED

A bounded sustained workload (concurrency 10) to look at stability over time: throughput and latency per window, backlog, process memory and database connections. The default duration is a short validation setting; use --soak-seconds for a longer benchmark (hard maximum 1800 s).

*Executed 21:19:45 – 21:20:53 (67 s), 1 trial(s).*

**Windows of the sustained run (60 s configured)**

| Window | Duration (s) | Completed | Workflow throughput (workflows/s) | Workflow p50 (ms) | Workflow p95 (ms) | Workflow p99 (ms) | Errors |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 0.0-10.0 s | 10.00 | 935 | 93.5 | 80.1 | 276.5 | 402.6 | 0 |
| 10.0-20.0 s | 10.00 | 947 | 94.7 | 85.4 | 243.5 | 377.3 | 0 |
| 20.0-30.0 s | 10.00 | 935 | 93.5 | 86.2 | 241.2 | 313.5 | 0 |
| 30.0-40.0 s | 10.00 | 983 | 98.3 | 81.9 | 246.2 | 364.9 | 0 |
| 40.0-50.0 s | 10.00 | 957 | 95.7 | 85.9 | 239.6 | 346.7 | 0 |
| 50.0-60.0 s | 10.00 | 938 | 93.8 | 84.8 | 243.7 | 342.9 | 0 |
| 60.00-60.07 s (partial: 0.07 s) | 0.07 | 10 | n/a (under 1 s) | 82.6 | 311.7 | 311.7 | 0 |

```text
Workflow throughput per full window (partial tail windows omitted)
0-10 s        ██████████████████████████████████████     93.5 workflows/s  p95 276.5 ms
10-20 s       ███████████████████████████████████████    94.7 workflows/s  p95 243.5 ms
20-30 s       ██████████████████████████████████████     93.5 workflows/s  p95 241.2 ms
30-40 s       ████████████████████████████████████████   98.3 workflows/s  p95 246.2 ms
40-50 s       ███████████████████████████████████████    95.7 workflows/s  p95 239.6 ms
50-60 s       ██████████████████████████████████████     93.8 workflows/s  p95 243.7 ms
```

**Process memory and CPU during the sustained run**

| Process | CPU avg (cores) | CPU max 1 s (cores) | RSS start MB | RSS max MB | RSS end MB |
| --- | --- | --- | --- | --- | --- |
| dispatcher-1 | 0.39 | 0.54 | 12 | 21 | 20 |
| forge | 0.69 | 0.86 | 21 | 28 | 27 |
| forgecart | 0.29 | 0.42 | 16 | 20 | 20 |
| worker-analytics-1 | 0.16 | 0.32 | 13 | 19 | 19 |
| worker-fulfillment-1 | 0.17 | 0.28 | 13 | 19 | 19 |
| worker-inventory-1 | 0.15 | 0.31 | 13 | 19 | 19 |
| worker-invoice-1 | 0.16 | 0.27 | 13 | 19 | 19 |
| worker-notification-1 | 0.15 | 0.27 | 13 | 20 | 20 |

**Observations**

- Workflow throughput, first 2 full window(s) (0-20 s) vs last 2 (40-60 s): 94.1 vs 94.8 workflows/s (+0.7%); mean of the per-window workflow p95: 260 vs 242 ms (-7.0%). Only full 10 s windows are compared.
- Lowest full window 0.0-10.0 s at 93.5 workflows/s, highest 30.0-40.0 s at 98.3 workflows/s.
- Excluded from comparisons as partial: 60.00-60.07 s (partial: 0.07 s) (10 workflows). A short tail window holds the workflows still in flight when the load stopped and its rate is not comparable.
- Database connections peaked at 42 (ForgeCart DB) and 11 (Forge DB) of 400 allowed; maximum backlog 566 jobs; worker retries 0.

**Acceptance criteria**

| Criterion | Kind | Result | Detail |
| --- | --- | --- | --- |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 5705 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 4562, responses 4562 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 0.9 s; effects 22810 of 22810, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |

**Outcome: PASSED.**

## 8. Saturation and highest demonstrated stable operating point

### F. Saturation test — PASSED

Concurrency is increased geometrically (stage length fixed) until a stop condition: error rate, p99 latency, throughput plateau, host CPU or a generator-saturation warning. Reports the highest tested configuration that met the stability criteria and the first that did not. The background backlog is reported per stage but is not a stop condition (the workers have separate capacity). A single stable stage is not a capacity claim.

*Executed 21:20:53 – 21:21:48 (55 s), 3 trial(s).*

**Saturation stages (fixed duration each, databases rebuilt per stage)**

| Concurrency | Workflow throughput (workflows/s) | vs highest so far | Workflow p95 (ms) | Workflow p99 (ms) | HTTP failure rate | Host CPU avg / max (%) | Backlog mid -> end of load (slope, 2nd half) | Drain after load (s) | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 10 | 91.0 | first stage | 269.0 | 383.4 | 0.00% | 90 / 95 | 295 -> 206 (-34 jobs/s of 362 produced/s) | 1.3 s | no stop condition met |
| 16 | 80.4 | -11.7% | 432.4 | 560.3 | 0.00% | 92 / 95 | 131 -> 408 (+32 jobs/s of 322 produced/s) | 1.0 s | no stop condition met |
| 26 | 88.4 | -2.9% | 575.6 | 765.4 | 0.00% | 90 / 95 | 298 -> 279 (-42 jobs/s of 358 produced/s) | 1.3 s | STOP: throughput plateau: no 5% gain over 91.0 workflows/s (the last level that gained 5%) for 2 consecutive stages |

**Observations**

- Peak workflow throughput 91.0 workflows/s at concurrency 10. Lowest tested concurrency within 5% of that peak: 10 (91.0 workflows/s, workflow p99 383 ms). This is an observation from 12 s stages, not a sustainable-capacity claim.
- Higher stages: concurrency 16: 80.4 workflows/s (-11.7% vs peak), p99 560 ms; concurrency 26: 88.4 workflows/s (-2.9% vs peak), p99 765 ms.
- The throughput plateau is already reached at the first tested concurrency (10): the knee lies at or below it and this ladder cannot locate it.
- Stop condition first met at concurrency 26: throughput plateau: no 5% gain over 91.0 workflows/s (the last level that gained 5%) for 2 consecutive stages.
- Background backlog at the end of the load was 206-408 jobs across stages (backlog = 5 x paid orders - effects, a proxy: Forge's queue depth is not exposed). The per-stage trend is shown in the table as mid -> end values and the least-squares slope over the second half of the load, next to the production rate; no yes/no threshold is applied.

**Acceptance criteria**

| Criterion | Kind | Result | Detail |
| --- | --- | --- | --- |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 1096 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 871, responses 871 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 1.3 s; effects 4355 of 4355, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 975 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 781, responses 781 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 1.0 s; effects 3905 of 3905, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 1083 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 877, responses 877 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 1.3 s; effects 4385 of 4385, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |

**Outcome: PASSED.**

## 9. Stock-contention and payment-mix results

### G. Stock contention — PASSED

Deliberately constrained inventory (40 units per product, 200 in total) against 400 workflows at concurrency 20. Stock conflicts are expected business outcomes. Checks that exactly the available stock is reserved, nothing is oversold and the accepted orders complete their background work.

*Executed 21:21:48 – 21:21:53 (5 s), 1 trial(s).*

**Inventory after the run**

| SKU | Initial | Remaining | Reserved |
| --- | --- | --- | --- |
| HUB-USBC-7 | 40 | 0 | 40 |
| KB-WL-001 | 40 | 0 | 40 |
| MON-ARM-1 | 40 | 0 | 40 |
| MOUSE-ERG-1 | 40 | 0 | 40 |
| STAND-LP-2 | 40 | 0 | 40 |

**Contention result**

| Trial | Completed | Workflow throughput (workflows/s) | Order p95 (ms) | Payment p95 (ms) | Workflow p50 (ms) | Workflow p95 (ms) | Workflow p99 (ms) | HTTP failure rate | Drain after load (s) | Max backlog (jobs) | Realised payments succeeded/declined/temporary |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| stock | 53 | 64.8 | 90.2 | 182.9 | 156.7 | 344.2 | 367.3 | 0.00% | 0.8 | 265 | 53/0/0 |

**Observations**

- Order creation p50/p95/p99 under contention: 17.2 / 90.2 / 147.9 ms; PostgreSQL deadlocks in the ForgeCart database: 0. Lock wait time per statement is not measured separately (it is included in the order-creation latency).

**Acceptance criteria**

| Criterion | Kind | Result | Detail |
| --- | --- | --- | --- |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 400 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 53, responses 53 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 0.8 s; effects 265 of 265, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| stock conflicts occurred (the scenario really contended) and were classified as business rejections, not failures | correctness | PASS | 347 stock conflicts, 0 failed workflows |
| created orders + stock conflicts = workflows | correctness | PASS | created 53 + conflicts 347 vs 400 |
| units reserved never exceed the initial stock (no negative stock, ledger balanced) | correctness | PASS | initial 200 units, remaining 0, reserved/committed 200 |

**Outcome: PASSED.**

### H. Payment outcome mix — PASSED

Two simulator profiles run in turn (the ForgeCart API is restarted with each): a normal mixed profile and a success-dominant profile. Records the actual outcome distribution and checks that declines and temporary failures create none of the five events while declines release their stock.

*Executed 21:21:53 – 21:22:10 (17 s), 2 trial(s).*

**Configured vs observed payment outcomes**

| Profile | Configured (s/d/t %) | Completed workflows | Succeeded | Declined | Temporary failure | Observed (s/d/t %) | paid orders | payment_failed orders | pending orders |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| normal-mixed | 70/20/10 | 300 | 206 | 58 | 36 | 68.7/19.3/12.0 | 206 | 58 | 36 |
| success-dominant | 95/3/2 | 300 | 292 | 4 | 4 | 97.3/1.3/1.3 | 292 | 4 | 4 |

- Note: The ForgeCart API process is restarted for every profile because the simulator reads its configuration at startup; the databases are rebuilt as well.

**Acceptance criteria**

| Criterion | Kind | Result | Detail |
| --- | --- | --- | --- |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 300 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 206, responses 206 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 1.3 s; effects 1030 of 1030, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| payment outcomes account for every completed workflow | correctness | PASS | 300 outcomes vs 300 completed |
| declined payments left payment_failed orders, temporary failures left pending orders, successes left paid orders | correctness | PASS | payment_failed 58/58, pending 36/36, paid 206/206 |
| only successful payments created the five events (5 x paid events exist in total) | correctness | PASS | 1030 events for 206 successes |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 300 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 292, responses 292 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 1.3 s; effects 1460 of 1460, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| payment outcomes account for every completed workflow | correctness | PASS | 300 outcomes vs 300 completed |
| declined payments left payment_failed orders, temporary failures left pending orders, successes left paid orders | correctness | PASS | payment_failed 4/4, pending 4/4, paid 292/292 |
| only successful payments created the five events (5 x paid events exist in total) | correctness | PASS | 1460 events for 292 successes |

**Outcome: PASSED.**

## 10. Idempotency and duplicate-delivery results

### I. Idempotency and duplicate delivery — PASSED

Each logical order is submitted several times concurrently with the same Idempotency-Key (checkout and payment), then duplicate Forge jobs carrying the same event ids are submitted through Forge's public API. Verifies one order, one payment attempt and one effect per event, and that duplicates succeed as duplicates.

*Executed 21:22:10 – 21:22:15 (5 s), 1 trial(s).*

**HTTP responses by request type**

| Request -> status | Count |
| --- | --- |
| POST /v1/orders -> 200 | 240 |
| POST /v1/orders -> 201 | 60 |
| POST Forge /v1/jobs -> 202 | 40 |
| POST payment-attempts -> 200 | 300 |

**Duplicate delivery summary**

| Logical orders | Submissions per order | Orders in DB | Paid orders | Events | Effects (5 per paid order) | Duplicate job submissions | Duplicate results |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 60 | 5 | 60 | 60 | 300 | 300 | 40 | 40 |

**Acceptance criteria**

| Criterion | Kind | Result | Detail |
| --- | --- | --- | --- |
| each logical order, submitted 5 times concurrently, resolved to exactly one order id | correctness | PASS | 60 of 60 |
| exactly one 201 Created and the rest replays per logical order | correctness | PASS | 60 of 60 |
| each order's concurrent payment submissions resolved to exactly one payment attempt | correctness | PASS | 60 of 60 |
| exactly one non-replayed response and the rest replays per payment | correctness | PASS | 60 of 60 |
| no unexpected HTTP response or transport failure | correctness | PASS | 60 of 60 clean |
| the database holds exactly one order per logical order | correctness | PASS | 60 orders for 60 logical orders |
| the five events of every paid order were published and executed | correctness | PASS | drained=true after 0.8 s; effects 300 of 300, unpublished events 0 |
| all duplicate job submissions were accepted by Forge's public API | correctness | PASS | 40 submitted, 0 failed |
| every duplicate job ended succeeded and reported duplicate=true | correctness | PASS | 40 of 40, 0 unfinished |
| duplicate deliveries created no extra business effects (effect counts unchanged) | correctness | PASS | 300 before, 300 after |
| outbox event identities were preserved (same distinct ids before and after the duplicates) | correctness | PASS | 300 distinct ids, 300 events |
| all 28 database integrity checks pass | correctness | PASS | 0 failed |

**Outcome: PASSED.**

## 11. Full background-pipeline and drain measurements

### J. Full background pipeline and drain — PASSED

The whole workflow from order creation to completion of every background job, with detailed pipeline measurements: outbox publication latency, Forge job queue wait and execution (from Forge's own timestamps), retries (worker logs), business-effect completion and total drain time. Forge jobs are observed through its public HTTP API only.

*Executed 21:22:15 – 21:22:25 (10 s), 1 trial(s).*

**Pipeline run**

| Trial | Completed | Workflow throughput (workflows/s) | Order p95 (ms) | Payment p95 (ms) | Workflow p50 (ms) | Workflow p95 (ms) | Workflow p99 (ms) | HTTP failure rate | Drain after load (s) | Max backlog (jobs) | Realised payments succeeded/declined/temporary |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| pipeline | 400 | 85.9 | 205.2 | 96.8 | 87.9 | 269.0 | 384.2 | 0.00% | 1.0 | 505 | 316/50/34 |

**Stage timings (milliseconds).** *Outbox publication* is the time from the outbox row being created (inside the payment transaction) to Forge accepting the job: p50 251.5, p95 366.1, p99 403.6 (1580 events). *Queue wait* and *execution* use Forge's own job timestamps (created -> started, started -> finished) for the jobs the poller observed; *effect latency* is outbox event to business-effect row, from ForgeCart's database.

| Job type | Jobs observed in Forge | Queue wait p50/p95/p99 | Execution p50/p95/p99 | Effect latency p50/p95/p99 (event -> effect) |
| --- | --- | --- | --- | --- |
| inventory.reconcile | 316 | 845.6 / 1044 / 1071 | 32.8 / 50.6 / 137.8 | 1027 / 1302 / 1339 |
| invoice.generate | 316 | 935.3 / 1077 / 1098 | 35.4 / 59.2 / 101.2 | 1130 / 1330 / 1387 |
| notification.send | 316 | 828.6 / 1012 / 1045 | 33.0 / 52.2 / 98.3 | 1014 / 1269 / 1312 |
| fulfillment.prepare | 316 | 919.5 / 1148 / 1167 | 35.6 / 59.5 / 167.7 | 1162 / 1423 / 1496 |
| analytics.order_placed | 316 | 969.0 / 1106 / 1131 | 33.8 / 56.2 / 161.7 | 1152 / 1383 / 1433 |

Forge job observation: 69 polls (0 failed, 0 where all 100 listed jobs were new and some may have been missed); poll latency p50 6.9 ms. Forge's public API lists only the newest 100 jobs and has no pagination or filters, so jobs are observed by polling; 316 of 316 expected jobs per type were seen on average. Jobs created faster than the poll interval can be missed; those timings are not measured.

**Drain.** drained=true after 1.0 s; effects 1580 of 1580, unpublished events 0. The drain check ran 5 count queries (mean 4.3 ms each) on ForgeCart's database; that is verification overhead, separate from the HTTP load.

**Observations**

- Outbox publication (event created in the payment transaction -> accepted by Forge): p50 251.5 ms, p95 366.1 ms, p99 403.6 ms over 1580 events.
- Pipeline span rate (first outbox event -> last effect, includes the HTTP load): 5.4 s for 1580 jobs = 293.2 jobs/s.
- Background drain rate (5 x paid orders / drain seconds, after the load ended): 1544.5 jobs/s over 1.0 s.
- Longest Forge queue wait (p95): fulfillment.prepare at 1148 ms. Forge timestamps are used, so this is the time between a job being created in Forge and a worker starting it.

**Acceptance criteria**

| Criterion | Kind | Result | Detail |
| --- | --- | --- | --- |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 400 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 316, responses 316 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 1.0 s; effects 1580 of 1580, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| inventory.reconcile: every job was observed (jobs seen through Forge's public API) | correctness | PASS | observed 316 of 316 (a 90% floor is required for the timing statistics to be meaningful) |
| invoice.generate: every job was observed (jobs seen through Forge's public API) | correctness | PASS | observed 316 of 316 (a 90% floor is required for the timing statistics to be meaningful) |
| notification.send: every job was observed (jobs seen through Forge's public API) | correctness | PASS | observed 316 of 316 (a 90% floor is required for the timing statistics to be meaningful) |
| fulfillment.prepare: every job was observed (jobs seen through Forge's public API) | correctness | PASS | observed 316 of 316 (a 90% floor is required for the timing statistics to be meaningful) |
| analytics.order_placed: every job was observed (jobs seen through Forge's public API) | correctness | PASS | observed 316 of 316 (a 90% floor is required for the timing statistics to be meaningful) |
| drain time after the load | performance | UNSPECIFIED | objective not specified by the owner; measured 1.02 s |

**Outcome: PASSED.**

## 12. Per-worker throughput and latency comparison

Source: pipeline / pipeline. Rates are completed jobs / the pipeline span from the first outbox event to the last effect (5.4 s), which includes the HTTP load (pipeline span rate, jobs/s), not a worker-capacity measure.

| Worker | Effects recorded | Effects/s over the pipeline span | Job starts (logs) | Retries (logs) | Execution p50/p95 from worker logs ms | Execution p50/p95 from Forge timestamps ms | Queue wait p95 ms (Forge) |
| --- | --- | --- | --- | --- | --- | --- | --- |
| inventory.reconcile | 316 | 58.6 | 316 | 0 | 31.9 / 50.3 | 32.8 / 50.6 | 1044 |
| invoice.generate | 316 | 58.6 | 316 | 0 | 34.2 / 58.6 | 35.4 / 59.2 | 1077 |
| notification.send | 316 | 58.6 | 316 | 0 | 31.6 / 50.3 | 33.0 / 52.2 | 1012 |
| fulfillment.prepare | 316 | 58.6 | 316 | 0 | 34.9 / 59.0 | 35.6 / 59.5 | 1148 |
| analytics.order_placed | 316 | 58.6 | 316 | 0 | 34.2 / 53.7 | 33.8 / 56.2 | 1106 |

Retry counts come from the workers' own logs because Forge's public API does not expose attempt numbers.

## 13. Worker-scaling comparison and efficiency

### K. Worker scaling comparison — PASSED

The same workload and initial data against different worker capacities (replicas per worker type x concurrency per replica). Throughput of the background pipeline is compared with the smallest configuration; efficiency is the observed gain divided by the capacity gain. Linear scaling is not assumed.

*Executed 21:22:25 – 21:23:16 (51 s), 4 trial(s).*

**Worker scaling (workers start after all events are published, so the drain measures worker + Forge + database capacity)**

| Config | Capacity (slots/type) | Background drain rate (jobs/s = 5 x paid / drain s; drain polled every 250 ms) | Gain vs smallest | Efficiency | Drain after workers started (s) | Publication wait with workers stopped (s) | Forge exec p50/p95 (ms, all types) | Worker CPU (avg cores, all workers) | Forge DB CPU avg / max (%) | App DB CPU avg / max (%) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1x1 (1 replicas x 1) | 1 | 359.8 | 1.00x | 100% | 5.3 s | 0.2 | 11.6 / 15.6 | 0.90 | 76 / 98* | 193 / 304* |
| 1x4 (1 replicas x 4) | 4 | 575.2 | 1.60x | 40% | 3.3 s | 0.4 | 27.0 / 36.4 | 1.45 | 54 / 54* | 311 / 311* |
| 2x4 (2 replicas x 4) | 8 | 571.8 | 1.59x | 20% | 3.4 s | 0.2 | 51.8 / 72.7 | 1.69 | 166 / 264* | 290 / 308* |
| 4x4 (4 replicas x 4) | 16 | 550.0 | 1.53x | 10% | 3.4 s | 0.4 | 77.8 / 193.4 | 1.76 | 126 / 200* | 308 / 310* |

- Note: Efficiency = (background drain rate gain) / (capacity gain); 100% would be linear scaling. Capacity is worker replicas x slots per replica, per job type. The drain clock starts after the workers were started, so jobs finished while the worker processes were starting are not excluded (see the effects-done field in the scenario JSON). Each configuration is one trial and the drain is polled every 250 ms (about +/-5-9% on a 3-5 s drain).

**Acceptance criteria**

| Criterion | Kind | Result | Detail |
| --- | --- | --- | --- |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 400 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 384, responses 384 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 5.3 s; effects 1920 of 1920, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 400 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 383, responses 383 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 3.3 s; effects 1915 of 1915, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 400 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 387, responses 387 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 3.4 s; effects 1935 of 1935, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |
| no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows | correctness | PASS | failed workflows 0, failure rate 0.00% |
| every started workflow ended as completed, rejected, failed or interrupted | correctness | PASS | started 400 |
| paid orders in the database equal successful payment responses | correctness | PASS | database 373, responses 373 |
| every paid order's five events were published and five effects recorded within the drain timeout | correctness | PASS | drained=true after 3.4 s; effects 1865 of 1865, unpublished events 0 |
| all 28 database integrity checks pass | correctness | PASS | 0 failed  |
| no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt | correctness | PASS | failed jobs seen 0, outbox failed 0, worker failures 0 |

**Outcome: PASSED.**

## 14. Recovery and resilience results

### L. Recovery and resilience — PASSED

Controlled failures inside the sandbox only: one worker type stopped while jobs queue, all workers paused to build a backlog, the dispatcher restarted while durable outbox events exist, graceful worker shutdown mid-load, a hard-killed worker (lease recovery) and a short pause of the ForgeCart database container. After each, the backlog must drain and every effect must be exactly once.

*Executed 21:23:16 – 21:24:29 (73 s), 6 trial(s).*

**Recovery tests**

| Test | Injected fault | Observation at the fault | Drain after recovery | Worker retry starts | Released on shutdown | Verdict |
| --- | --- | --- | --- | --- | --- | --- |
| stop-one-worker-type | fulfillment worker stopped (SIGTERM) at load start, restarted after the load | map[fulfillment_effects_before_restart:0 paid_orders:187] | 1.3 s | 0 | 0 | recovered |
| pause-all-workers | all workers stopped (SIGTERM) at load start, held for the pause, then restarted | map[backlog_at_resume:950 effects_at_resume:0 paid_orders:190] | 1.8 s | 0 | 0 | recovered |
| restart-dispatcher | dispatcher stopped (SIGTERM) 1 s into the load, restarted after the load | map[outbox_events:960 outbox_unpublished_at_restart:525] | 1.8 s | 0 | 0 | recovered |
| graceful-worker-shutdown | all workers sent SIGTERM 1.5 s into the load (in-flight jobs released), restarted 1 s later | - | 7.4 s | 3 | 16 | recovered |
| hard-kill-workers | all workers killed (SIGKILL) 1.5 s into the load, restarted 2 s later; Forge's reaper must recover their leases | - | 6.6 s | 18 | 0 | recovered |
| app-db-pause | ForgeCart database container paused (docker pause) for 3 s, 1.5 s into the load | - | 0.8 s | 0 | 0 | recovered |

- Note: Forge lease settings for this scenario: lease 6 s, heartbeat 2 s, safety margin 4 s, reaper every 1 s (defaults are 15/5/10/5) so that recovery after a hard kill is observable within the drain timeout.

**Acceptance criteria:** 39 evaluated — 39 passed, 0 failed, 0 unspecified. Failed and unspecified ones:

*(none: every criterion passed; the full list is in the scenario JSON)*

**Outcome: PASSED.**

## 15. Business-effect and database-integrity verification

After every trial the suite ran read-only integrity queries against ForgeCart's database (after the measurement window; their duration is recorded per trial): stock ledger balance, no negative stock, five events exactly for paid orders and none otherwise, payment-state consistency, one effect per order and per event for each of the five workers, invoice/fulfillment/analytics snapshot consistency, no failed outbox events and (when drained) every event published and every effect present. The suite never reads or writes Forge's job tables.

| Scenario | Trials verified | Checks run | Checks failed | Verification time (s) |
| --- | --- | --- | --- | --- |
| smoke | 1 | 28 | 0 | 0.06 |
| baseline | 3 | 84 | 0 | 0.32 |
| ramp | 7 | 196 | 0 | 0.61 |
| burst | 1 | 28 | 0 | 0.35 |
| soak | 1 | 28 | 0 | 1.02 |
| saturation | 3 | 84 | 0 | 0.43 |
| stock-contention | 1 | 28 | 0 | 0.07 |
| payment-mix | 2 | 56 | 0 | 0.21 |
| idempotency | 1 | 28 | 0 | 0.00 |
| pipeline | 1 | 28 | 0 | 0.10 |
| worker-scaling | 4 | 112 | 0 | 0.43 |
| recovery | 6 | 168 | 0 | 0.53 |

**No integrity check failed in any executed trial.**

## 16. CPU, memory, database and connection utilization

Sources: /proc for the host and every service process, `docker stats` for the two PostgreSQL containers (sampled every 5 s), `pg_stat_activity` for connections, all sampled about once per second during the run and drain. Container CPU is a percentage of one core (200% = two cores). Values marked — were not available.

| Scenario / trial | Host CPU avg/max % (first sample excluded) | Forge DB CPU avg/max % | App DB CPU avg/max % | App DB mem max MB | Max connections app/forge (limit) | Max backlog | Top service processes by avg CPU (cores) | Sampler cycle ms |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| smoke/smoke | 27 / 27 | — | — | 0 | 28 / 6 (400) | 50 | dispatcher-1 0.09, forge 0.07, forgecart 0.03 | 37.4 |
| baseline/rep1 | 89 / 97 | 61 / 61* | 340 / 340* | 202 | 37 / 11 (400) | 480 | forge 0.61, dispatcher-1 0.36, forgecart 0.28 | 26.9 |
| baseline/rep2 | 88 / 95 | 142 / 142* | 320 / 320* | 201 | 37 / 11 (400) | 470 | forge 0.58, dispatcher-1 0.34, forgecart 0.27 | 27.6 |
| baseline/rep3 | 83 / 95 | 124 / 124* | 322 / 322* | 195 | 37 / 11 (400) | 465 | forge 0.53, dispatcher-1 0.34, forgecart 0.24 | 29.7 |
| ramp/c1 | 66 / 77 | 25 / 25* | 135 / 135* | 176 | 28 / 11 (400) | 215 | forge 0.35, dispatcher-1 0.22, forgecart 0.16 | 18.7 |
| ramp/c2 | 81 / 92 | 47 / 47* | 211 / 211* | 178 | 29 / 11 (400) | 377 | forge 0.45, dispatcher-1 0.34, forgecart 0.25 | 26.5 |
| ramp/c5 | 87 / 95 | 59 / 59* | 300 / 300* | 182 | 32 / 11 (400) | 445 | forge 0.58, dispatcher-1 0.33, forgecart 0.24 | 24.8 |
| ramp/c10 | 90 / 95 | 59 / 59* | 319 / 319* | 198 | 37 / 11 (400) | 490 | forge 0.64, dispatcher-1 0.35, forgecart 0.25 | 29.3 |
| ramp/c20 | 88 / 94 | 151 / 151* | 326 / 326* | 193 | 37 / 11 (400) | 440 | forge 0.61, dispatcher-1 0.34, forgecart 0.26 | 30.8 |
| ramp/c40 | 88 / 95 | 42 / 42* | 382 / 382* | 203 | 42 / 11 (400) | 415 | forge 0.62, dispatcher-1 0.33, forgecart 0.25 | 32.3 |
| ramp/c80 | 85 / 96 | 42 / 42* | 276 / 276* | 205 | 37 / 11 (400) | 345 | forge 0.58, dispatcher-1 0.33, forgecart 0.25 | 32.3 |
| burst/burst | 93 / 96 | 121 / 157 | 313 / 359 | 257 | 42 / 11 (400) | 543 | forge 0.67, dispatcher-1 0.38, forgecart 0.28 | 24.1 |
| soak/soak | 94 / 96 | 130 / 163 | 325 / 360 | 382 | 42 / 11 (400) | 566 | forge 0.69, dispatcher-1 0.39, forgecart 0.29 | 34.4 |
| saturation/c10 | 90 / 95 | 116 / 143 | 326 / 330 | 492 | 42 / 11 (400) | 515 | forge 0.65, dispatcher-1 0.36, forgecart 0.27 | 22.6 |
| saturation/c16 | 92 / 95 | 85 / 108 | 319 / 333 | 490 | 42 / 11 (400) | 500 | forge 0.62, dispatcher-1 0.35, forgecart 0.26 | 20.5 |
| saturation/c26 | 90 / 95 | 79 / 91 | 323 / 339 | 492 | 42 / 11 (400) | 562 | forge 0.66, dispatcher-1 0.38, forgecart 0.27 | 22.9 |
| stock-contention/stock | 78 / 78 | 39 / 39* | 268 / 268* | 453 | 27 / 6 (400) | 265 | forgecart 0.44, dispatcher-1 0.35, forge 0.23 | 41.3 |
| payment-mix/normal-mixed | 89 / 94 | 162 / 162* | 289 / 289* | 465 | 42 / 11 (400) | 415 | forge 0.56, dispatcher-1 0.33, forgecart 0.27 | 30.8 |
| payment-mix/success-dominant | 87 / 95 | 59 / 59* | 344 / 344* | 472 | 42 / 11 (400) | 600 | forge 0.67, dispatcher-1 0.37, forgecart 0.23 | 29.6 |
| pipeline/pipeline | 92 / 94 | 137 / 137* | 328 / 328* | 468 | 42 / 11 (400) | 505 | forge 0.65, dispatcher-1 0.40, forgecart 0.29 | 26.0 |
| worker-scaling/1x1 | 85 / 91 | 76 / 98* | 193 / 304* | 429 | 22 / 6 (400) | 1795 | forge 0.43, dispatcher-1 0.29, worker-notification-1 0.18 | 15.7 |
| worker-scaling/1x4 | 89 / 93 | 54 / 54* | 311 / 311* | 430 | 32 / 11 (400) | 1782 | forge 0.55, dispatcher-1 0.38, worker-inventory-1 0.30 | 20.1 |
| worker-scaling/2x4 | 91 / 95 | 166 / 264* | 290 / 308* | 512 | 52 / 11 (400) | 1780 | forge 0.62, dispatcher-1 0.39, forgecart 0.23 | 28.8 |
| worker-scaling/4x4 | 88 / 95 | 126 / 200* | 308 / 310* | 612 | 93 / 11 (400) | 1863 | forge 0.56, dispatcher-1 0.32, forgecart 0.20 | 32.2 |
| recovery/stop-one-worker-type | 92 / 94 | 62 / 62* | 379 / 379* | 454 | 38 / 11 (400) | 570 | forge 0.66, dispatcher-1 0.39, forgecart 0.25 | 27.9 |
| recovery/pause-all-workers | 38 / 93 | 29 / 52* | 158 / 310* | 414 | 32 / 11 (400) | 950 | worker-analytics-1 0.31, worker-fulfillment-1 0.31, worker-inventory-1 0.31 | 16.2 |
| recovery/restart-dispatcher | 47 / 93 | 99 / 99* | 318 / 318* | 431 | 32 / 11 (400) | 605 | forge 0.28, forgecart 0.12, dispatcher-1 0.10 | 18.2 |
| recovery/graceful-worker-shutdown | 45 / 90 | 33 / 59* | 165 / 322* | 454 | 32 / 11 (400) | 807 | forge 0.26, dispatcher-1 0.13, forgecart 0.08 | 15.7 |
| recovery/hard-kill-workers | 39 / 91 | 31 / 59* | 165 / 327* | 426 | 32 / 11 (400) | 818 | forge 0.25, dispatcher-1 0.12, forgecart 0.07 | 16.3 |
| recovery/app-db-pause | 74 / 97 | 93 / 93* | 326 / 326* | 438 | 42 / 11 (400) | 565 | forge 0.30, dispatcher-1 0.19, forgecart 0.13 | 513.5 |

\* fewer than 3 `docker stats` readings (taken about every 5 s, the first before the load starts): indicative only.

The load generator's own CPU use and the client-side gap between the order and payment requests are recorded per trial; a warning is raised when either suggests the generator limited the run:

| Scenario / trial | Generator CPU (avg cores) | Client gap p99 (µs) | Generator a possible bottleneck? |
| --- | --- | --- | --- |
| smoke/smoke | 0.09 | 54.0 | no |
| baseline/rep1 | 0.07 | 162.0 | no |
| baseline/rep2 | 0.08 | 202.0 | no |
| baseline/rep3 | 0.08 | 222.0 | no |
| ramp/c1 | 0.04 | 67.0 | no |
| ramp/c2 | 0.05 | 82.0 | no |
| ramp/c5 | 0.07 | 158.0 | no |
| ramp/c10 | 0.07 | 110.0 | no |
| ramp/c20 | 0.07 | 92.0 | no |
| ramp/c40 | 0.08 | 108.0 | no |
| ramp/c80 | 0.09 | 99.0 | no |
| burst/burst | 0.07 | 165.0 | no |
| soak/soak | 0.07 | 190.0 | no |
| saturation/c10 | 0.07 | 171.0 | no |
| saturation/c16 | 0.07 | 325.0 | no |
| saturation/c26 | 0.07 | 243.0 | no |
| stock-contention/stock | 0.17 | 792.0 | no |
| payment-mix/normal-mixed | 0.08 | 80.0 | no |
| payment-mix/success-dominant | 0.07 | 97.0 | no |
| pipeline/pipeline | 0.08 | 134.0 | no |
| worker-scaling/1x1 | 0.10 | 113.0 | no |
| worker-scaling/1x4 | 0.10 | 143.0 | no |
| worker-scaling/2x4 | 0.10 | 265.0 | no |
| worker-scaling/4x4 | 0.10 | 141.0 | no |
| recovery/stop-one-worker-type | 0.08 | 241.0 | no |
| recovery/pause-all-workers | 0.10 | 86.0 | no |
| recovery/restart-dispatcher | 0.09 | 165.0 | no |
| recovery/graceful-worker-shutdown | 0.09 | 76.0 | no |
| recovery/hard-kill-workers | 0.09 | 150.0 | no |
| recovery/app-db-pause | 0.04 | 85.0 | no |

## 17. Errors, failed acceptance criteria and inconclusive runs

No scenario failed, errored or was inconclusive, and the generator recorded no HTTP errors.

## 18. Bottleneck analysis (evidence and hypotheses)

This section separates **measured facts** from **hypotheses**. A resource that is busy when throughput stops growing is a candidate, not proof: all components share one host here, so CPU contention between them cannot be separated from a limit inside any one of them without a follow-up experiment.

### ramp: the most loaded stage (c80)

*Measured:* average CPU by component during that stage (percent of one core):

- postgres container app-db: 276%
- forge: 58%
- postgres container forge-db: 42%
- dispatcher-1: 33%
- forgecart: 25%
- worker-fulfillment-1: 15%
- host CPU average 85% (max 96%) of 12 logical CPUs; ForgeCart DB connections peaked at 37, Forge DB at 11 of 400; max backlog 345 jobs; workflow p95 1406 ms.

- *Hypothesis:* the host CPU is close to saturation, so the limit may be the machine rather than any single service; run the same stage with the load generator and services pinned to separate cores, or on a larger host, to check.
- *Hypothesis:* postgres container app-db used more than 1.5 cores on average and is the heaviest component; if throughput stopped scaling here, it is the first candidate (follow-up: profile it at this stage).

### saturation: the most loaded stage (c26)

*Measured:* average CPU by component during that stage (percent of one core):

- postgres container app-db: 323%
- postgres container forge-db: 79%
- forge: 66%
- dispatcher-1: 38%
- forgecart: 27%
- worker-fulfillment-1: 16%
- host CPU average 90% (max 95%) of 12 logical CPUs; ForgeCart DB connections peaked at 42, Forge DB at 11 of 400; max backlog 562 jobs; workflow p95 575.6 ms.

- *Hypothesis:* the host CPU is close to saturation, so the limit may be the machine rather than any single service; run the same stage with the load generator and services pinned to separate cores, or on a larger host, to check.
- *Hypothesis:* postgres container app-db used more than 1.5 cores on average and is the heaviest component; if throughput stopped scaling here, it is the first candidate (follow-up: profile it at this stage).
- *Hypothesis:* a PostgreSQL container used at least 80% of its CPU allowance, which could limit checkout, the outbox or Forge's job table (follow-up: raise the container CPU limit and repeat).

### Worker scaling

*Measured:* see section 13 for jobs/s per configuration and efficiency. If efficiency falls well below 100% while worker CPU stays low and a database container or Forge is busy, the limit is more likely shared (Forge claim API or PostgreSQL) than the workers; if worker CPU is high, the workers' host share is the limit. These are hypotheses to test by repeating the largest configuration with a larger PostgreSQL CPU allowance.

### Background pipeline

*Measured:* outbox publication p95 366.1 ms; longest Forge queue wait p95 among types 1148 ms; drain 1.0 s. A long queue wait with short execution means jobs wait for a free worker slot (capacity) rather than run slowly.

## 19. Recommended corrections and next experiments

Recommendations follow only from this run's evidence.

1. The saturation ladder starts at or above its knee, so it cannot locate it: add lower concurrency stages (a scenario-definition change that was not made as part of the reporting corrections).
2. Re-run the saturation scenario with longer stages (`--scale 2`) and a larger PostgreSQL allowance, and compare which resource saturates first (section 16).
3. The ramp flagged a scaling limit (throughput gained less than 10% going from the previous stage to concurrency 10 (75.9 -> 75.4 workflows/s).). Repeat that stage and the one before it three times each to confirm it is not run-to-run noise (baseline spread is in section 5).
4. Use the worker-scaling efficiency (section 13) to decide where adding workers stops paying off; repeat the largest configuration with more PostgreSQL CPU to test whether the database is the shared limit.
5. Forge's public API lists only the newest 100 jobs and exposes no attempt count; at high job rates some jobs are not observed (section 11 gives the coverage). A paginated or filtered job-list endpoint, or attempt and error fields on job responses, would make queue-wait and retry measurements complete. This is a proposal for a separate Forge task, not something this suite changed.
6. Next experiment: an open-loop (fixed arrival rate) scenario around the highest stable closed-loop point, to see whether latency stays flat at that rate and how much queueing the closed loop was hiding.

## 20. Limitations and unmeasured metrics

- All components (load generator, Forge, ForgeCart, dispatcher, five workers and Docker) share one host, so they compete for CPU and memory; the PostgreSQL containers use tmpfs storage, so disk I/O cost is not represented.
- Closed-loop load only: the offered rate follows the system's speed, which can hide queueing delay (coordinated omission). An open-loop scenario is not part of this suite yet.
- Single runs unless a scenario states repetitions; only the baseline is repeated. Differences smaller than the baseline spread are not evidence.
- The stability thresholds of the saturation scenario are configurable heuristics, and a stage passes them in a single 12-second window; this is not a sustainable-capacity measurement.
- Forge job timing is available only for jobs the poller saw in the newest-100 listing (see coverage notes); retries come from worker logs; Forge's public API has no attempt counts, queue depth or filtering.
- Backlog (jobs created but not yet completed) is derived from ForgeCart's database (5 x paid orders - effects) sampled about once a second; it is a proxy for Forge's queue depth, which is not exposed.
- Process CPU/memory come from /proc and container figures from `docker stats` (every 5 s); PostgreSQL internals such as lock waits or per-query timings are not collected (only connection counts and the deadlock counter).
- Fault injection is limited to controlled process stops/kills and a 3-second database container pause inside the sandbox; network partitions, disk-full and clock faults were not exercised.
- The sandbox runs services as host processes built from the repository; the repository has no container images for them.
- No performance objectives were specified by the owner, so no throughput or latency target was judged.

