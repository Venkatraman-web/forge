# Forge and ForgeCart on AWS (separate machines)

**Status: five measurement sessions on 2026-10-22 and 2026-10-23 (session 5 is section 8), about 2 hours of machine time in total in the first four, on small burstable machines.** These are observations from one deployment, not capacity guarantees. The earlier local numbers (`docs/benchmarks.md`) came from one laptop with a RAM-backed database; the differences are explained below and are part of the result. Everything was torn down at the end (see "Cost and teardown").

## Summary

| | Before | After | What changed it |
|---|---|---|---|
| Forge alone: submissions per second, durable commits, workers claiming at the same time | ceiling about **335 jobs/s**, about 200 sustained | **1,200 jobs/s sustained**, 2,000 jobs/s accepted with zero rejections | batch endpoint, then the sharded queue counter |
| Forge alone: submissions with no workers | about 335 jobs/s | at least **3,000 jobs/s** accepted, zero rejections (the generator's maximum in the test) | batch endpoint |
| Forge alone, bigger database (`m7i-flex.large`), 2,000 jobs/s fully clean; completions peak about 2,350 jobs/s | about 2,025 | about **2,350** | database machine with a faster CPU and more memory |
| Forge alone: jobs completed per second during load at 2,000-2,800 offered (session 5) | about 1,550-1,720 | about **1,950-2,030** | batch completion (`POST /v1/worker/complete`) |
| ForgeCart: job queue wait (median) | about 700 ms | about **75 ms** | `FORGE_POLL_INTERVAL` 1 s to 200 ms (and the counter fix) |
| ForgeCart: 3,000 background jobs drain | 7-9 s | about 3 s | the same |
| Adding workers (one job type overloaded at 480 jobs/s) | 1 machine: 123 completions/s | 2: 252, 3: **383** | horizontal scaling, no change to Forge |
| Hard-killing a worker mid-load (6,000 jobs) | n/a | **all 6,000 succeeded**, 20 recovered and run a second time | leases and the reaper |

Raw reports: [`benchmarks/aws/`](benchmarks/aws/).

## Deployment

Six EC2 instances, all `t3.small` (2 vCPU, 2 GiB, burstable, "unlimited" credit mode), Ubuntu 24.04, 12 GiB gp3 disks, one availability zone (ap-southeast-2a), default VPC, SSH open only to the operator's addresses and all ports open only between the machines. Round trip between Forge and its database: 0.22 ms. PostgreSQL 16.15, default settings except `max_connections = 200`; `synchronous_commit = on` throughout (one clearly labelled diagnostic excepted). Disk write sync latency on the Forge database host (`pg_test_fsync`, fdatasync): about 0.94 ms.

| Machine | Runs |
|---|---|
| `forge` | Forge (the commit named in each section), `FORGE_MAX_QUEUED=200000`, three separate API keys, config file with ForgeCart's job types plus two benchmark types |
| `forge-db` | PostgreSQL for Forge's jobs |
| `cart-web` | ForgeCart API and outbox dispatcher (one job per submission request) |
| `cart-workers` | ForgeCart's five workers (4 slots each), or the synthetic workers (24 slots, 25 ms handler) for Forge-only runs |
| `cart-db` | PostgreSQL for ForgeCart |
| `loadgen` | the test harness (`remote` and `remote-cart` commands) |

ForgeCart's payment simulator was set to 100% success with no delay. Every ForgeCart integrity check passed in every trial of every session, and no workflow failed.

## 1. Starting point: the durable commit was the ceiling (session 1, code at commit `590204d`)

Open-loop jobs sent straight to Forge, 60 s per stage ([`awsfo1`](benchmarks/aws/awsfo1.md)): 100 and 200 jobs/s held in every repetition; from 400 offered upward, accepted throughput was **flat at about 335-340 jobs/s**, and every rejection was `admission_busy` (the queue never filled). Cause, tested: a submission holds Forge's global admission lock until its commit reaches the network disk, about 3 ms per submission, so about 1 / 3 ms = 335/s. With `synchronous_commit = off` on the test database for one 30-second run ([`awsdiag1`](benchmarks/aws/awsdiag1.md)), 600 and 1,000 jobs/s were sustained; that setting acknowledges commits before they reach the disk (a crash can lose accepted jobs), so it was used only to locate the limit and was reset immediately. The laptop figure of 800 sustained did not carry over because its database was in RAM.

## 2. Fix one: batch submission (session 2, commit `835a3b7`)

`POST /v1/jobs/batch` stores up to 100 jobs in one transaction with one commit (all or nothing, exact queue limit preserved), so the lock and the disk write are paid once per batch.

- **No workers running** ([`awsd10`, `awsd120010`, `awsd200010`, `awsd300010`, `awsd300050`](benchmarks/aws/)): the table was emptied between stages; 800, 1,200, 2,000 and 3,000 jobs/s (batches of 10), and 3,000 jobs/s (batches of 50), were each accepted at the offered rate with **zero rejections** (the generator's maximum, not Forge's). Forge averaged 19% CPU and the Forge database 39% (peak 101%).
- **With workers claiming at the same time** ([`awsb10`](benchmarks/aws/awsb10.md), then a repeat on an emptied table, [`awsc10`](benchmarks/aws/awsc10.md)): in the first run 400 jobs/s held; in the repeat the 400 stage accepted only about 250 jobs/s (the first stage after a restart; not explained). In both runs, at 800 and 1,200 offered the accepted rate stuck at **about 610 jobs/s** with `admission_busy` rejections, and completions at about 515/s. Forge sat at about 30% CPU and the Forge database at 34% average.

## 3. A finding and fix two: the shared counter row (sessions 2-3, commit `cf88557`)

The ceiling with workers claiming, 61 batches/s plus about 515 claims/s, is about 575 database transactions per second in total. Submissions alone could do 3,000. The common element is the one-row `queue_counter` table: every claim (queued to running) and every submission updated that row through triggers, and a row update keeps its lock until the transaction commits, so every claim and submission waited for the previous one's commit. Submissions with no claims in flight did not hit it.

The counter became **16 shard rows** (a transaction updates the shard chosen by its transaction id; admission reads the SUM). The limit stays exact: a claim that has not committed yet can only make the sum higher, never lower, so a submission can be refused slightly early but the limit cannot be exceeded. Validated locally (upgrade of a database holding data, triggers, 60 concurrent batches against a limit, a 20-second mixed run of 12,509 accepted jobs that never exceeded the limit and reconciled exactly) and on AWS (the migration was applied to a database holding 75,000 queued jobs and preserved the sum).

Result with workers claiming ([`awsc001`](benchmarks/aws/awsc001.md), [`awsc010`](benchmarks/aws/awsc010.md), [`awsc050`](benchmarks/aws/awsc050.md); 45 s stages, one repetition each):

| Jobs per request | Offered (jobs/s) | Accepted (jobs/s) | Completed during load (jobs/s) | Rejections |
|---|---|---|---|---|
| 1 | 400 | 338 | 338 | `admission_busy` (unchanged single-job ceiling) |
| 10 | 400 | 399.5 | 398 | none (sustained) |
| 10 | 800 | 800.1 | 799 | none (sustained) |
| 10 | 1,200 | 1,182 (98.5%) | 1,181 | some `admission_busy` |
| 10 | 1,600 | 1,545 (96.6%) | 1,209 | some `admission_busy`; queue grows |
| 50 | 1,200 | 1,200.9 | 1,198.9 | none (sustained) |
| 50 | 1,600 | 1,600.8 | 1,236.7 | none; queue grows |
| 50 | 2,000 | 2,000.7 | 1,144.3 | none; queue grows |

- Sustained with workers claiming: **1,200 jobs/s** with batches of 50 (the single-job path stays at about 335). At higher offered rates, submissions are still accepted with no rejections, but the **workers' completion rate plateaus at about 1,150-1,240 jobs/s**, so a backlog builds.
- What limits that now: the Forge database CPU, which averaged 64% with peaks of 96% during these stages (Forge averaged 49%, the workers' machine 41%). A larger database instance would be the first thing to try; I did not measure it.
- The 2,500 jobs/s stage was cut off when a session ended and is not reported.

## 4. Investigations that did not explain the ceiling (session 2)

Recorded because they were tested and ruled out, not assumed:

- Inside the database, a call to the batch function cost about 2.0 ms for 1 row, 9.1 ms for 10 and 47.6 ms for 50 on the large leftover `jobs` table, but 1.9 ms for 10 and 3.6 ms for 50 on an **empty** table, so table state matters for raw insert cost. Yet the end-to-end ceiling with workers was the same (about 610 jobs/s) on an emptied table, which is how the shared counter row was found.
- Scratch copies of the table with and without the counter trigger cost about 0.02 ms per row either way; time-ordered job IDs instead of random ones made no difference; write-ahead log volume per call was small (42 KB for 50 rows, one sync). None of these was the limit.
- Practical note: `jobs` rows are never deleted (no retention yet), and a large churned table makes inserts slower; retention is not implemented.

## 5. Independent scaling (session 4)

One job type (`bench.a`) offered 480 jobs/s (batches of 10, 40 s), each worker machine running one 4-slot synthetic worker with a 25 ms handler (about 160 jobs/s of capacity), worker machines added one at a time ([`awsscale1`](benchmarks/aws/awsscale1.md), [`awsscale2`](benchmarks/aws/awsscale2.md), [`awsscale3`](benchmarks/aws/awsscale3.md)):

| Worker machines | Completed during load (jobs/s) | Worst oldest-queued age | Time to drain after load | Jobs succeeded |
|---|---|---|---|---|
| 1 | 123 | 112 s | 113 s | 19,200 of 19,200 |
| 2 | 252 (2.0x) | 36 s | 36 s | 19,200 of 19,200 |
| 3 | 383 (3.1x) | 10 s | 10 s | 19,200 of 19,200 |

Throughput scaled close to linearly with workers and the backlog shrank accordingly, with no change to Forge. Limitation: only one job type was loaded, so this shows scaling, not isolation from a second type's load.

## 6. Crash recovery (session 4)

Two worker machines (24 slots each, 400 ms handler), 6,000 jobs at 100 jobs/s over 60 s, one worker hard-killed with SIGKILL about 25 s in and restarted 23 s later ([`awscrash1`](benchmarks/aws/awscrash1.md)). Lease 15 s, reaper every 5 s.

- Forge's counters: 6,000 submissions accepted, **6,020 claims, 20 jobs requeued by the reaper, 6,000 succeeded**.
- The database afterwards: **6,000 jobs succeeded, 20 of them with a second attempt, none queued or stuck**, queue counter 0.
- The oldest queued job waited at most 8.4 s; the queue drained 6.1 s after the load ended.

This is at-least-once execution: the 20 jobs that were running on the dead worker ran again. Handlers must be idempotent.

## 7. ForgeCart (shoppers)

**First session** (poll interval 1 s, single counter row; [`aws001`](benchmarks/aws/aws001.md) / `aws002`): foreground flat at about **195-215 workflows/s** from 10 to 160 shoppers, background drain 6.9-8.9 s for 3,000 jobs, Forge queue wait about 600-780 ms (median). CPU during a larger run: `cart-db` averaged about 77% (peak 98%), Forge about 31%, the Forge database about 24%, `cart-web` about 40%, `cart-workers` about 27%, the load generator about 6%. ForgeCart's own database was the foreground limit; Forge had room.

**Poll interval experiment** (20 shoppers, 3 repetitions each, [`awspoll1`](benchmarks/aws/awspoll1.md), [`awspoll2`](benchmarks/aws/awspoll2.md)): waiting claim requests re-check the database every `FORGE_POLL_INTERVAL`. Setting it from 1 s (default) to 200 ms cut the median queue wait from about 660-840 ms (job timestamps 756-979 ms) to about 170-240 ms (155-173 ms), p95 from about 2.0 s to about 0.47 s, with no change in throughput. The cost is more frequent idle polling of Forge's database.

**Final run** (poll 200 ms, sharded counter, [`awsfinal1`](benchmarks/aws/awsfinal1.md), 6 trials, 600 workflows each): all trials drained, 0 failed workflows, 0 failed integrity checks.

| Shopper concurrency | Workflows/s | Workflow p50 / p95 (ms) | 3,000-job drain | Forge queue wait p50 / p95 (ms) |
|---|---|---|---|---|
| 10 | 127, 136 | 64 / 148-177 | 2.6-2.9 s (1,030-1,150 jobs/s) | 73-79 / 441-448 |
| 40 | 94, 130 | 285-332 / 529-994 | 2.9 s (1,025-1,029 jobs/s) | 74-78 / 441-451 |
| 160 | 141, 138 | 957-982 / 1,967-2,165 | 3.4-3.5 s (868-880 jobs/s) | 70-74 / 440 |

Background work finished about 2.5x faster and Forge's queue wait fell about 9x. **The foreground rate was lower than in the first session (94-141 against 195-215 workflows/s)**, and `cart-db` was again the busiest machine (average 75%, peak 99%). I did not isolate why. A plausible explanation, not tested, is that with Forge delivering jobs several times faster, ForgeCart's workers now load `cart-db` while shoppers are still checking out, so the shared database serves background work earlier and the foreground gets less. Burstable CPU throttling was checked and is not the cause: the instances run in "unlimited" credit mode (their credit balance was low, 3-33 credits, so surplus credits were being used), and CPU steal was only 0-5%. Whatever the cause, the foreground limit is ForgeCart's own database.

## 8. Batch completion (session 5, 2026-10-23)

**What was compared.** The same four `t3.small` machines (Forge, PostgreSQL 16 with durable commits, 3 synthetic worker processes x 32 slots with a 25 ms handler, load generator), the same sweep (batches of 10 jobs per request, 45 s stages), two builds of the code:

- **base** = commit `7d23b31`: batch submit, sharded counter, batch claim, but each finished job written by its own request and commit.
- **new** = base plus `POST /v1/worker/complete` (up to 20 jobs per request and per commit) and the runtime's completion flusher.

Raw reports: `benchmarks/aws/awsbase1.md`, `awsbase2.md`, `awsnew01.md`, `awsnew02.md`.

| Offered jobs/s | base: completed/s during load | new: completed/s during load | base drain after load | new drain after load |
|---|---|---|---|---|
| 1,200 (1 run each) | 1,182 | 1,193 | 0.3 s | 0.3 s |
| 2,000 (2 runs each) | 1,688 and 1,716 | 1,947 and 1,972 | 7.1 s and 7.2 s | 0.3 s and 0.3 s |
| 2,800 (2 runs each) | 1,599 and 1,548 | 2,028 and 2,023 | 27.3 s and 26.0 s | 10.7 s and 13.8 s |

- **What it shows:** with the same hardware, completions per second rose about 15% at 2,000 offered and about 30% at 2,800 offered, and the backlog after the load drained 2 to 20 times faster. The two repetitions of each build agree within a few percent, and the order was base, new, base, new. At 1,200 the base run had 710 jobs rejected as `admission_busy` (the new run had none); that gap is not repeated elsewhere, so I do not attribute it to the change.
- **Where the limit is now:** at 2,800 offered, completions flatten at about 2,025 jobs/s while the generator is still turned away with `admission_busy` for 9,600 to 11,000 jobs per stage. During the new runs the database machine averaged 43-56% user CPU, 11-14% I/O wait and 16-28% idle; Forge and the workers were 60-66% idle. So the database is still the busiest machine, but it is not at 100%; the remaining ceiling is the short admission section for submissions plus the database's commit waits. I did not isolate which.
- **Correctness on real PostgreSQL:** before the load, a direct API check showed a mixed batch (an object result, a null result, an array result, and a stale generation) gives `succeeded` for the valid items and `ownership_lost` for the stale one; a repeated completion gives `ownership_lost`; duplicate IDs, an empty list, 21 jobs, a bad worker ID, a bad job ID, an unknown field and a result with U+0000 each gave 400; a wrong key gave 401. After all the runs: 921,060 jobs succeeded, none failed, none stuck `running`, none queued, and the queue counter's sum was 0, equal to the real queued count.
- **Not shown:** this run does not separate batch claim from the earlier code (the base build already has it), and one availability zone, one repetition at 1,200, and short stages are the usual limits. "Sustainable" was `no` at 2,000 and 2,800 in all four runs because some submissions were rejected as `admission_busy`, not because workers fell behind at 2,000.

## 9. A bigger database machine (session 5, 2026-10-23)

AWS refused to resize the existing database machine (`FreeTierRestrictionError`: changing an instance type is not available on a free plan account), so a new database host was launched instead: an `m7i-flex.large` (2 vCPU, 7.8 GB, Intel Xeon Platinum 8488C), with a fresh PostgreSQL 16 on the same default settings (durable commits, `shared_buffers` 128 MB, so the extra memory was only used by the page cache). Everything else, and the new code (batch claim and batch completion), is as in section 8. Raw reports: `benchmarks/aws/awsbig01.md`, `awsbig02.md`.

| Offered jobs/s | t3.small database: completed/s during load (section 8) | m7i-flex.large database: completed/s during load | Submissions turned away (`admission_busy`), small / large |
|---|---|---|---|
| 2,000 | 1,947 and 1,972 | **1,997** (1 run) | about 1,900 jobs / **0** |
| 2,800 | 2,028 and 2,023 | 2,369 and 2,378 | 9,600-11,000 jobs / **0** |
| 3,600 | not run | 2,350 and 2,238 | not run / **0** |

- **What it shows:** with the bigger database, the submission side kept up at every rate (accepted equals offered within 2%, none rejected), and 2,000 jobs/s was the first fully clean stage (`sustainable=yes`: no rejection, queue drained 0.3 s after the load). The ceiling for completions moved from about 2,025 to about **2,350 jobs/s** (+16%), not to the offered rate.
- **Where the limit is now:** the database machine again: it averaged 49-51% user CPU, 6-8% system and 15% I/O wait, with 21-23% idle, while Forge and the workers were 54-57% idle. A queue built up at 2,800 and 3,600 offered and took 7.5 to 27 s to drain. I did not isolate which part of the database work limits it. Two vCPUs and commit waits are the likely causes; more cores would be the next thing to try, and the free plan only offers 2-vCPU types.
- **Limits of this test:** one run at 2,000, two at 2,800 and 3,600; the database host also has a newer CPU than the `t3.small`, so "bigger" here means more memory and a faster core, not more cores. Stages were 45 s.
- **Correctness:** after the runs, 664,250 jobs succeeded and none failed, queued or stuck running, and the queue counter's sum equalled the real queued count (0).

## 10. Profiling the database, and four changes that did not help (session 5)

Same machines as section 9 (database on an `m7i-flex.large`), new code. Raw numbers come from `pg_stat_statements` and `EXPLAIN ANALYZE` on the database host.

- **Where the database time goes.** The first profile (2,400 jobs/s offered, 60 s, on a freshly filled table) put the claim statement at 69% of statement time (2.14 ms per call, 3.9 jobs per call), completion at 14% and batch submit at 13%. Two later profiles (2,800 offered, 45 s) put the claim at 42-51% (0.60-0.76 ms per call), batch submit at 25-27% and completion at 22-23%. The first profile therefore overstated the claim; I did not isolate why (the table then had about 230,000 dead row versions and autovacuum had not run, which I suspect but did not verify).
- **Statement-level counter triggers: rejected.** I wrote a migration that replaces the per-row counter triggers with statement triggers using transition tables. Measured warm (the same session run three times, because a cold session adds about 1 ms of one-off function compilation that is not a real cost), a 20-job claim ran in 0.88-0.95 ms with it against 0.63-0.69 ms with the row triggers. It was slower, so the migration was deleted and not committed.
- **Waiting a few ms to claim more jobs per request: no gain.** A 3 ms gather window raised the average claim from 3.9 to 6.9 jobs but completions were 2,286 jobs/s against 2,374 without it (within run-to-run noise of about 5%). Not kept.
- **Forge's database pool from 10 to 24 connections: no gain.** 2,389 against 2,374 jobs/s. Not kept.
- **The ceiling held.** Completions during load at 2,800 jobs/s offered were 2,213-2,389 across every variant. The database host was not saturated while the load ran: 36-49% user CPU, 4-8% system, 20-26% I/O wait, about 30% idle.
- **The burstable Forge host was tested and ruled out** (next section).

## 11. What limits the ceiling: waiting for the disk (session 5)

Three more measurements on the setup of section 9, in the order they were done:

- **A non-burstable Forge host does not help.** Forge moved to an `m7i-flex.large` (the earlier `t3.small` had shown 17-47% CPU steal): completions during load were 2,345 jobs/s at 2,800 offered and 2,277 at 3,600 (`benchmarks/aws/awsfm701.md`), the same as before. The Forge machine was 71% idle with no steal. So the Forge host was not the limit.
- **Where the database's sessions wait.** I sampled `pg_stat_activity` about every 0.1 s during a 2,800 jobs/s stage (932 samples of an active session): `LWLock:WALWrite` 35%, `Lock:advisory` 26% (submissions queued behind the admission lock, which is held through the commit), running on CPU 22%, `IO:WALSync` 14%. So about half of the active time is a session waiting for the write-ahead log to reach the disk, and the advisory lock time is the same wait seen from the submission side.
- **Diagnostic, not a setting I recommend: `synchronous_commit = off`** on the benchmark database (a commit then returns before the disk flush, so a crash can lose the last fraction of a second of acknowledged jobs). With it, 2,800 jobs/s offered was fully clean (2,788 jobs/s completed, drain 0.3 s) and at 4,000 offered 2,881 jobs/s completed with every submission accepted. The setting was reset afterwards (`ALTER DATABASE forge RESET synchronous_commit`).

**Conclusion.** The ceiling of about 2,350 jobs/s with durable commits is set mostly by how fast the database can make each commit durable on this disk (network-attached `gp3`), not by Forge, the workers or the database CPU. Turning the durability wait off raises the completed rate by roughly 20% at 2,800 and shows about 2,900 jobs/s at 4,000 offered, where the next limit is probably the 2-vCPU database. Fewer commits per job (larger claim and completion batches) or a faster disk would be the levers; I tested one version of the first (a 3 ms claim gather window, section 10) with no gain, and have not tested a faster disk (provisioned IOPS cost extra). These are single runs (two stages each).

## 12. A dashboard in the deployed environment (session 5)

Until now the AWS sessions were watched with `vmstat` and database statistics. This session added a monitoring machine (a `t3.small` running Prometheus and Grafana from the repository's `observability` profile, with persistent volumes and a required Grafana password) and `node_exporter` on the other five machines (setup, checks and limits: README, "Running it in a deployed environment"). While the same load test as before ran (1,200, 2,000 and 2,800 jobs/s offered, 45 s each; Forge and the database on `m7i-flex.large`, workers 3 x 32 slots), the dashboard's own panel queries, run through Grafana, showed:

| Panel (peak over the 2-minute window) | Value |
|---|---|
| Database CPU busy | 82.7% |
| Database CPU waiting for disk (iowait) | 25.1% |
| Database disk busy | 71.9% |
| Workers CPU busy | 41.0% |
| Forge CPU busy | 27.7% |
| Oldest due job age | 7.2 s |
| Queued jobs (peak) | about 7,400 per job type |
| Job events per second | about 1,146 per type |
| Admission slots in use | 3 of 3 |
| Forge database pool in use | 10 of 10 |

This agrees with the earlier finding from `vmstat` and wait-event sampling: the database machine is the busiest and waits on its disk, while Forge and the workers have room. It is one 2-minute run, read from a time series, not an additional measurement of capacity. The throughput of this run (1,169, 1,935 and 2,320 jobs/s completed) was in line with earlier runs.

## 13. Tuning the whole ForgeCart pipeline (session 5)

All runs: the real shop, dispatcher and four real workers, simulated shoppers creating and paying orders (`remote-cart`, 40 shoppers, 9,000 orders, so 36,000 background jobs), Forge and the databases on `m7i-flex.large`, one run per row, same day. "End to end" is orders whose four background jobs all finished, per second, over the load plus the drain: 36,000 jobs / (load time + drain time) / 4. Every run completed all 36,000 jobs with no failed workflows and all integrity checks passing (24 of 24; the run with no workers had only the 19 checks that apply before the drain).

| Run | What changed | Orders/s | Drain after load | End to end (orders/s) |
|---|---|---|---|---|
| 1 | start: 5 products, one database server for Forge and the shop, 8-slot workers, one-job-per-request dispatcher | 143 | 73 s (495 jobs/s) | 66 |
| 2 | realistic catalogue (3,005 products) | 248 | 105 s (344 jobs/s) | 64 |
| 3 | dispatcher sends its events in one Forge batch request | 326 | 104 s (347 jobs/s) | 68 |
| 5 | workers at 32 slots each (ForgeCart's cap of 8 raised to the SDK's 64) | 333 | 94 s (383 jobs/s) | 74 |
| 6 | dispatcher records its outcomes concurrently instead of one after another | 230 | 48 s (753 jobs/s) | 103 |
| 7 | the shop's database on its own machine | 278 | 34 s (1,051 jobs/s) | **135** |

| 8 | the dispatcher records attempts and "published" marks for a whole group in one statement each (three repetitions) | 351, 357, 346 | 1.1 s, 3.1 s, 1.2 s (the pipeline keeps up with the load) | **about 337** |

(Run 4 is omitted: the workers refused to start with 32 slots because of ForgeCart's own cap, so no jobs ran; the dispatcher still published all 36,000 events and the foreground reached 414 orders/s, which shows how fast the shop side alone can go.)

What each step found, from the wait-event sampling of the database sessions:
- **Run 1:** 56% of the shop database's session time was waiting on row locks. The 5-product test catalogue made every order queue behind the same few stock rows; a real shop has thousands of products. Not a product limit, a test artifact.
- **Run 2:** the lock waits vanished (0%) and commit waits (the log reaching disk) took over at 65%. Background drain got slower only because the foreground produced jobs faster.
- **Run 3:** Forge's admission lock stopped being a bottleneck. The drain did not improve, which showed the limit was elsewhere: outbox events piled up in the shop (peak about 28,000) and drained at only about 250-300 events/s while Forge's own queue stayed tiny.
- **Run 5:** the 8-slot cap on the workers is arbitrary. With 32 slots, a ready backlog drained at about 1,100 jobs/s, so the workers were not the end-to-end limit.
- **Run 6:** the cause was the dispatcher itself: after Forge accepted a batch, it recorded "published" for each event one after another, each a separate durable commit of about 3 ms. Recording them at the same time doubled the drain rate. The foreground fell (230 orders/s) because the dispatcher now competes for the same database's disk.
- **Run 7:** with one database server for everything (Forge's database, the shop's, and all workers' writes), gains on one side came out of another. Splitting the shop's database onto its own machine raised both sides: 278 orders/s and 1,051 jobs/s.

**Run 8, the three repetitions.** The dispatcher still committed two database updates per event on its own, 8 of the roughly 16 commits an order causes on the shop database. Doing them as one statement per group of events (`StartAttempts` and `MarkPublishedBatch`, each event still fenced by its own token) lifted the foreground to 346-357 orders/s (3 runs within about 3% of each other) and made the background drain disappear: 1 to 3 seconds after the load ended, against 34 s before. End to end, orders whose four jobs all finished went from 66 at the start of the day to about 337 (36,000 jobs in about 27 s), a factor of about 5. All integrity checks passed in all three runs (24 of 24). During them the shop database machine peaked at 95% CPU (48% disk busy), the workers machine at 67%, the machine running the shop API, dispatcher and load generator at 53%, Forge's database at 61% and Forge at 19%.

**Where it stood after run 7.** The shop's database machine peaked at 94% CPU with 45% disk busy and spent 60% of its session time waiting for the log (commit waits): it is the next limit. End to end went from 66 to 135 orders per second (2x) with no change to Forge itself. These are single runs of 9,000 orders; the run-to-run variation I saw elsewhere was about 5%, so differences of that size between runs are not meaningful, but the trend across the six rows is. The measurement tool also costs the database something: its drain polling query took about 17-52 ms per call and was 5-30% of the shop database's statement time in some runs.

## 14. The most the changed architecture handled (session 5)

The setup of run 8 (section 13) plus a **dedicated load generator** on its own `m7i-flex.large`, so the test no longer shares a machine with the shop. The real shop, dispatcher and four real workers (32 slots each); simulated shoppers; 12,000 orders (48,000 jobs) per level; one run per level.

| Shoppers at once | Orders/s | Jobs/s (4 per order) | Drain after load | Peak CPU: shop database / workers / Forge database / Forge |
|---|---|---|---|---|
| 20 | 359 | about 1,430 | 1.2 s | 99% / 79% / 73% / 21% |
| 40 | 367 | about 1,470 | 5.9 s | 99% / 78% / 73% / 20% |
| 80 | 355 | about 1,420 | 1.6 s | 99% / 83% / 78% / 21% |
| 120 | 367 | about 1,470 | 3.2 s | 99% / 78% / 75% / 20% |
| 200 | **384** | **about 1,540** | 13.1 s | 98% / 77% / 75% / 20% |

- **The plateau.** Throughput is flat at about 355-384 orders/s (about 1,420-1,540 real jobs/s) from 20 to 200 shoppers: more shoppers add only waiting. The load generator (6% CPU) was not the limit; the **shop database machine was at 98-99% CPU at every level**, with the workers (77-83%) and Forge's database (73-78%) close behind. Forge itself stayed at about 20%.
- **Against the earlier 2,350 jobs/s.** That figure is Forge alone with stand-in workers that do no work; today's re-measurement of it was 2,221-2,235 completed/s at 2,400-2,800 offered. The real pipeline tops out lower, at about 1,500 jobs/s, because every order also costs the shop database real work: Forge is not the limit, the shop's database is.
- **What the shop database spends its time on (profile at saturation, 12,000 orders).** 61 statements per order (733,705 in total), spread flat: no single statement above 16% of statement time. The largest are the four outbox inserts per order (16%), the dispatcher's claim and its two grouped updates (about 8%, 7% and 7%), the order and payment updates (about 5% each) and the workers' effect inserts (about 3-4% each). The test harness itself used about 8% of the statement time (its drain-polling query and its integrity query), so the numbers above are slightly lower than the system could do without being watched.
- **Limits.** One run per level; a single availability zone; the shop API, dispatcher and four workers are plain processes on `t3.small` machines (the workers machine and the shop machine are burstable); the harness's hard limit is 200 shoppers, so the true plateau beyond that was not explored.

## 15. Two changes that did not raise the ceiling (session 5)

The setup of section 14 (dedicated load generator, 80 shoppers, 12,000 orders per run). Baseline: 355, 356 and 350 orders/s in three runs, with the shop database machine at 98-99% CPU.

| Change | Orders/s | Shop database CPU peak | Kept? |
|---|---|---|---|
| The four workers' verification (four queries per job: the event, the order, the payment attempt and the count of succeeded attempts) merged into one query on the normal path, with the original step-by-step checks as the fallback so every rejection keeps its exact reason; 12 of the 61 statements per order removed | 356, 350 | 98% | **No**, reverted: no measurable gain |
| PostgreSQL tuning of the shop database, durability untouched (`shared_buffers` 2 GB instead of the default 128 MB, `effective_cache_size` 6 GB, `wal_buffers` 64 MB, `max_wal_size` 8 GB, `checkpoint_timeout` 15 min, `checkpoint_completion_target` 0.9, `random_page_cost` 1.1) | 361, 365 | 99% | Left on the benchmark machine; about +2%, within run-to-run noise (about 3%) |

(One 200-shopper run of the first change did not start because I gave it a run name that was too long, so there is no result at that level.)

**What this says.** The shop database is limited by the CPU it spends on the roughly 61 statements one order causes (about 22,000 statements per second at the plateau on two vCPUs), mostly writes: inserts and updates of about 0.2-0.35 ms each, which include time spent waiting for a CPU, because more than 200 sessions share two cores. Cheap indexed reads and memory and checkpoint settings are not what keeps it busy. The outbox table alone is written four times per event (insert, lease, attempt count, published mark) and accounts for about 40% of the statement time. A further rise would need less write work per order or more CPU for it; a larger machine is not available on the free plan, which allows only two-vCPU instance types.

## 16. The whole workflow, stepped up until it stops scaling (session 5)

**The workflow.** A shopper creates an order, then pays; on success the order is marked paid and the four background jobs run through Forge, otherwise nothing more happens. Simulated shoppers (the harness's `remote-cart`, one run per level) on a dedicated load-generator machine. The payment simulator was set to a realistic mix: **90% success, 5% declined, 5% temporary failure**, with **100-300 ms of gateway delay** per payment. All 24 integrity checks passed at every level, including that only paid orders ever got jobs (for example 93,484 jobs at 200 shoppers, 89.9% of the 26,000 orders times 4).

**Deployment used** (after an audit of the previous one; `m7i-flex.large` is a 2-vCPU machine, and the account allows 16 vCPUs running at once): Forge, Forge's database, the shop's database, the shop API with the dispatcher, the four workers (32 slots each) and the load generator each on their own `m7i-flex.large`, plus a monitoring machine. Every service is a boot-time systemd service (verified by rebooting the three application machines: everything came back by itself), Forge polls at 200 ms, both databases carry the same durable-safe tuning.

| Shoppers at once | Orders/s | Paid jobs/s (about) | Order latency p50 / p95 / p99 (ms) | Peak CPU: shop DB / Forge DB / workers / Forge / shop API |
|---|---|---|---|---|
| 10 | 46 | 167 | 213 / 304 / 319 | 38 / 18 / 14 / 6 / 6 |
| 20 | 92 | 330 | 218 / 309 / 329 | 59 / 30 / 18 / 9 / 9 |
| 40 | 160 | 577 | 247 / 363 / 417 | 85 / 47 / 28 / 14 / 14 |
| 80 | 224 | 808 | 346 / 548 / 636 | 97 / 55 / 39 / 19 / 18 |
| 120 | 252 | 908 | 457 / 765 / 914 | 99 / 60 / 43 / 22 / 19 |
| 160 | 267 (277 in a repeat) | 962 | 570 / 986 / 1,196 | 100 / 66 / 45 / 21 / 21 |
| 200 | 262 | 944 | 721 / 1,297 / 1,619 | 100 / 66 / 45 / 22 / 20 |

- **Where it stops scaling.** Throughput grows almost linearly up to about 40 shoppers, bends at 80, and is **flat from about 120-160 shoppers at roughly 260-270 orders/s (about 950 paid jobs/s)**. Past 120 shoppers latency keeps climbing without any gain: from 120 to 200 shoppers the median goes from 457 to 721 ms and p95 from 765 to 1,297 ms while throughput stays level (and dips slightly). The floor of about 213 ms at 10 shoppers is the simulated gateway delay, so the knee of the curve is around **120 shoppers at about 94% of the maximum**.
- **What limits it.** The shop database machine: 97-100% CPU from 80 shoppers up. Forge's database reached 66%, the workers 45%, and Forge, the shop API and the load generator 20% or less. No machine showed CPU steal (under 0.2%), so it is the 2-vCPU database itself.
- **The background keeps up.** The wait after the load stopped for the last job to finish was 0.3 to 2.6 seconds at every level: jobs are processed as orders arrive.
- **Why this is lower than the earlier 355-384 orders/s.** Those earlier runs had every payment succeed with no gateway delay, and a slower workers tier that fell behind the load (it finished the last jobs 6 to 13 seconds after the shoppers stopped), which left the shop database more capacity for the foreground. Counting an order only when its jobs have finished too, the earlier runs come to about 270 orders/s end to end, close to today's 257-265, so the two agree once the same thing is counted. A profile at saturation showed 57 statements per order (61 earlier; fewer because 10% of orders fail payment), each taking 40-60% longer than in the earlier profile because more sessions now compete for the two cores, which I did not investigate further. The earlier run 8 (337 orders/s end to end) is higher than this and I do not have an explanation for the difference.
- **Limits.** One run per level (a repeat of 160 gave 277 against 267); 45 to 100 seconds of load per level; one availability zone; the harness stops at 200 shoppers; the gateway is simulated, not Razorpay.

## 17. One outbox event per paid order (session 5)

**The change.** Before, a paid order wrote four outbox events (one per background job), and each was written four times on the shop database (inserted, leased, attempt counted, marked published): 16 row writes per order, which the profile suggested was about 40% of the database's statement time. Now a paid order writes **one** `order.paid` event, and the dispatcher turns it into the four Forge jobs when it publishes (always the four in one Forge request; older single-job events still work). Same deployment and same sweep as section 16 (90/5/5 payment mix, 100-300 ms gateway delay, 10 to 200 shoppers, one run per level).

| Shoppers | Orders/s before -> after | Latency p50 / p95 before -> after (ms) | Shop database CPU before -> after |
|---|---|---|---|
| 40 | 160 -> 161 | 247 / 363 -> 247 / 349 | 85% -> 96% |
| 80 | 224 -> 220 | 346 / 548 -> 349 / 576 | 97% -> 99% |
| 120 | 252 -> 245 | 457 / 765 -> 469 / 799 | 99% -> 100% |
| 160 | 267 (277 in a repeat) -> 274 and 274 | 570 / 986 -> 559 / 964 | 100% -> 100% |
| 200 | 262 -> 280 | 721 / 1,297 -> 677 / 1,206 | 100% -> 100% |

- **The result is small: about +3 to 7% at the plateau** (262-277 before, 273-280 after), which is about the size of the run-to-run variation (two runs of the same level differed by up to 10 orders/s). Below the plateau nothing changed, as expected. All 24 integrity checks passed at every level and the background drained in under a second at every level.
- **My estimate was wrong.** I predicted +30 to 40%. The profile at saturation shows why: statements per order fell only from 57 to 54, because the per-event outbox updates were already grouped, so a large share of their "statement time" was waiting rather than CPU. Statement time per order fell by about 16% (7.5 ms against 8.9 ms), but the machine's two cores are still spent across about 50 small inserts and updates of 0.1 to 0.5 ms each, with no single dominant one left.
- **Correctness checks of the new design on AWS** (separate from the sweep): 39 paid orders gave exactly 39 `order.paid` events, all published, each with all four effects; the declined order had no event and no jobs; an order whose events were written the old way (four events) was still published and processed (4 effects); and a deliberately corrupt `order.paid` event failed alone (`invalid_payload`, no attempts, no effects) without blocking anything else.
- **A mistake in my test tool, found and fixed:** its "has everything drained" check still expected one event per job, so the first sweep with this change waited out its 5-minute timeout at every level and was discarded; the numbers above are from the rerun after the fix.

> **Correction (found while writing section 25).** The load tool accepted up to 500 shoppers on the command line but silently capped the number of running shoppers at 200 (`HardMaxConcurrency`). Every run in sections 18 to 24 labelled 240 to 500 shoppers therefore ran with **200** shoppers; the labels below are marked "200 shoppers (labelled N)". The comparisons in those sections (before against after, in the same session at the same level) are still valid, because both sides ran at the same real load. Statements about *the ceiling at 400 or 500 shoppers* were not measured at that load; section 25 measures it for real (and the ceiling of about 510 orders/s turned out to hold). The cap is now 500 and the command line and the cap use one constant.

## 18. The workers' result rows in their own database (session 6)

**The change.** The four workers used to read the paid order and write their result row (invoice and lines, fulfillment plan and lines, notification delivery, inventory reconciliation) in one transaction on the shop database. Now a worker reads with plain statements on the shop database (no transaction, no row lock, the line snapshot is carried in memory) and writes the result in one transaction on a database named by `FORGECART_RESULTS_DATABASE_URL`, a new `m7i-flex.large` machine (`results-db`, PostgreSQL 16, same tuning and the same migrations). Migration 15 drops the result tables' foreign keys to orders, payment attempts and events, which cannot span two databases. Before the change, a profile of the shop database at 160 shoppers (12,000 orders) showed the result-table statements at 39% of its statement time and the workers' verification reads at about 5%; that is what the expected gain was based on.

**Same-day comparison** (same machines, 90/5/5 payment mix, 100-300 ms gateway delay, 12,000 orders per level, one run per level; all 24 integrity checks passed in every run, with the checks reading the results from whichever database held them):

| Shoppers | old workers, one database | new workers, one database | new workers, results database separate |
|---|---|---|---|
| 160 | 316 (profile run, earlier in the session) | | 357 |
| 200 | 341 | 341 | 393 |
| 280 | | | 385 |
| 360 | 328 | 350 | 389 (378 with 14,000 orders) |
| 440 | | | 389 |

- **The separate database gave about +12 to 18%** (389-393 against 328-350 orders/s), and the rate no longer rises from 200 shoppers on, with the workflow p50 falling from about 555-580 ms to about 485 ms. This is far below my estimate of about 1.6 times. The statement time that moved was 39% of the shop database's total, but part of it was time waiting for the log and for a CPU, which does not translate into freed capacity one for one.
- **The new read path alone made no clear difference** (0 to +7% against the old workers on one database, about the run-to-run variation).
- **Where the limit is now** (vmstat during 200 shoppers (labelled 360), 14,000 orders): the shop database 99% CPU at its peak, the results database 82%, Forge's database 68%, the workers machine 61%, Forge 20%, the shop API and dispatcher machine 17%, the load generator 7%. The shop database is still the limit; the results database is the next machine to fill.
- **The earlier days' plateau (273-280 orders/s, sections 16 and 17) is not directly comparable**: today's single-database runs reached 328-350 with essentially the same code, and I did not find out why (a different day on the same instance types; I did not repeat the old sweep).
- **The cost of this change** is one more 2-vCPU machine, which uses the last 2 of the account's 16 running vCPUs: the monitoring machine cannot run at the same time as all the others. Cross-database timestamps (the per-job latency in the load test's report) compare clocks of two machines.
- **What is not covered:** the result rows and the order are now written in two steps; a crash between them is covered by redelivery (the job is retried and finds no result row), and a worker never writes a result unless it has just read a paid order with a succeeded payment, but there is no foreign key to prove it afterwards (the load test checks that no result exists for an unpaid order). Database failover for either machine was not tested.

**Checkout as one database function: no gain, reverted.** The profile after the split (12,000 orders, 200 shoppers (labelled 360)) showed the shop database spending about 30% of its statement time on checkout (7 + 2n round trips per order), 15% on payment, 8% on the outbox insert, 20% on the dispatcher's grouped outbox updates and 14% on the workers' reads, with no statement above 10%. I moved checkout into one function (`forgecart_checkout`: same steps, same lock order, one call) and compared the two shop binaries alternately in the same session, 200 shoppers (labelled 360), 12,000 orders: old 380, 374 and 393 orders/s, new 385 and 383, so no measurable difference (the spread of the old binary alone is 5%). The function, its migration and the Go change were removed. Table statistics also showed nothing cheap in the indexes (every index on the order tables is used) or in in-place updates (95% on orders and products; the outbox's status changes cannot be in-place because status is in a partial index). The shop database is limited by the work of the row writes themselves, not by the number of round trips.

## 19. Payment as its own service (session 6)

**The change.** Payment moved out of the shop process into a separate service (`forgecart-payments`) with its own database (`payments`, on the results machine; its tables are `payment_attempts` and `razorpay_links`). The shop calls it over HTTP (`POST /v1/payments`, one call per payment attempt, a pooled connection with a large idle limit) and applies the answer to the order in one local transaction (paid state, `paid_attempt_id` and the outbox event, or the stock release). Per paid order the shop database now runs about 5 payment-related statements instead of about 11, and the attempt row is written to another machine's database. The service runs on the shop's machine in this deployment (loopback); the workers verify against `orders.paid_attempt_id`, so they no longer read payment tables.

**Same-session comparison** (same machines, 90/5/5 payment mix, 100-300 ms provider delay, 200 shoppers (labelled 360), 12,000 orders per run; the "old" runs are the previous architecture: payment inside the shop, one transaction; the results database was separate in both):

| Architecture | Orders/s in each run | Drain after load |
|---|---|---|
| before (payment inside the shop) | 417, 401, 406, 398 | 0.3-0.6 s |
| after (payment service) | 506, 513, 512, 511 | 0.4-0.7 s |

- **About +25% (about 510 against about 405 orders/s)**, the four-against-four runs do not overlap, and all 25 integrity checks passed in every run of the new architecture (24 earlier checks plus a new one that compares the payment service's records with the orders: every paid order names its succeeded attempt, and no payment is stranded). My estimate was +10 to 15%; the saving is larger than the payment statements alone because the round trips and the order lock that serialized payment for an order are gone from the shop database.
- **Two runs are left out**: the first run after the machines were started in each architecture (544 and 627 orders/s) had a foreground faster than the rest but the background took 16 s and 9 s to catch up, so the figures are not comparable with runs where the pipeline keeps up. The same happened to an earlier profile run today.
- **The new ceiling is about 505 orders/s** (two runs labelled 400 and 500 shoppers, both really 200 shoppers: 504 and 489; section 25 measures real 300 to 500 shoppers). At the plateau the shop database and the results machine (results and payment databases together) both peaked at 99% CPU; Forge's database 81%, the workers machine 62%, the shop machine 46%, Forge 19%, the load generator 6%. That is about 505 x 0.9 x 4 = **1,820 paid jobs/s** end to end.
- **Moving the payment database to the Forge machine did not help** (485 and 506 orders/s): the results machine was still at 97% CPU without it, so the limit is the shop database plus the workers' result writes, not the payment database. It was moved back.
- **Failure cases checked through the real services** (not part of the load runs): a payment recorded by the payment service that the shop never applied was repaired by repeating the request with the same key (order paid, event written); with the payment service stopped, the shop answered `503 payment_unavailable` and the same key succeeded after the restart; a declined payment released the stock (stock plus held units = the starting stock); a second key after a final payment got `409 order_not_payable`; an unknown order `404`.
- **Not done:** Razorpay through the new service was checked afterwards (below); the local Docker sandbox mode of the test tool was updated (it starts the payment service and its database) but not run, because the Docker socket is not available to this account.

**Metrics for the payment service, the shop's view of it and the sweeper (checked on AWS).** The payment service has its own optional keyed metrics listener (requests and latency per route, new answers by outcome, provider errors), the shop counts its calls to the payment service by result with a latency histogram, and the sweeper's passes and repairs are counted; the dashboard has a "Payment service" row with six panels (33 panels in total). After a 12,000-order run plus six payments recorded directly at the payment service, the numbers reconciled: the shop made 12,000 calls with result `ok` and the payment service saw 12,006 payment requests; succeeded attempts were 10,809 against 10,804 on the shop side, and the 5 extra were exactly the stranded successes the sweeper then repaired (`forgecart_payment_sweep_repaired_total` = 5). Without the key the metrics listener answers 401. With the monitoring machine running (it replaced the idle load generator because of the 16-vCPU limit) all scrape targets were up and every query of the six new panels returned data under a trickle of about 8 orders per second: payments per second, p50 latency 0.2 s (the simulator's 100 to 300 ms delay) with the shop's view of the call within a millisecond of the service's own, outcomes per second, calls by result and sweeper passes. Grafana loaded the dashboard with the new row. The Grafana panels themselves were not looked at in a browser.

**Razorpay through the payment service, checked with a real test-mode payment.** The payment service was switched to the Razorpay provider (test keys) and two orders were created. Asking for payment returned `pending` with a payment link for each, and the payment database held both links with no attempt recorded. After a person paid the first link on Razorpay's hosted page (Indian test card), asking again with the same key recorded a succeeded attempt with Razorpay's own payment reference, marked the order paid with that attempt, wrote exactly one event (published), and all four job results appeared; repeating the request replayed the stored attempt. The second order was left unpaid: about 18 minutes after its link was created (the 16-minute link life plus the 2-minute grace), asking again recorded a declined attempt, moved the order to `payment_failed`, wrote no event and returned its stock (stock plus held units equal to the starting stock). The service was then switched back to the simulator.

**The payment sweeper (added after the comparison above).** It repairs a payment the payment service recorded but the shop never applied, when the client does not come back (see `forgecart/README.md`). Checked on AWS with the sweeper set to run every 5 s with a 10 s lag: 12 orders were paid directly at the payment service without telling the shop (11 succeeded, 1 declined), and one pass repaired all 12 (orders paid with their attempt and one event each, the declined order's stock released, stock plus held units equal to the starting stock, and all four job results present for every repaired order). Two things went wrong on the way and are fixed: a first design that scanned pending orders took a minute to reach a fresh repair because the test database held several hundred legitimately abandoned orders (replaced by walking the payment service's final attempts from a cursor), and the first deployment failed every pass because the new cursor table had no privilege for the application's database role (a deployment grant, now noted in the README).

Cost under load, sweeper on at its defaults (every 30 s, 30 s lag) against off, alternating, 200 shoppers (labelled 360), 12,000 orders, runs where the pipeline kept up: on 470; off 475, 458, 458 orders/s, and all 25 integrity checks passed in every run. That is no measurable cost (the spread between off runs is as large as the difference). Other runs in that series were not comparable: the first runs after the machines started had a foreground faster than usual and a background that needed 7 to 13 s to catch up (as noted above), and the last runs hit the disk problem below.

**An incident that invalidated the last runs of the session: Forge's database disk filled up.** The `forge-db2` machine has an 11 GB disk. It held the `jobs` table with 7.3 million benchmark rows (4 GB; Forge never deletes jobs, see the limits below) and 3.9 GB of write-ahead log that I had allowed by setting `max_wal_size` to 8 GB. At 100% full, PostgreSQL could not extend the file, Forge refused jobs (`No space left on device`), the dispatcher used up its eight attempts per event and marked 10,312 of 10,731 events failed, and two runs reported "not drained in 120 s". Nothing was wrong with the code; the runs after the disk filled are not used. Done: `max_wal_size` lowered to 2 GB (PostgreSQL keeps already allocated log files for reuse, so this alone freed nothing now), the apt cache and old logs removed (369 MB free). **Resolved afterwards (section 20):** the benchmark `jobs` rows were cleared with your approval, and Forge now has opt-in retention for finished jobs.

## 20. Retention of finished jobs (session 6)

**The problem.** A job's row is both the queue entry and the record a client reads, and Forge never deleted it, so the table grew without bound: 7.27 million finished benchmark jobs (4 GB with indexes) filled the 11 GB disk of the Forge database machine and stopped Forge (section 19). The benchmark rows were then cleared with `TRUNCATE jobs` (approved; nothing queued or running).

**The change.** Opt-in retention: `FORGE_RETENTION_SUCCEEDED` and `FORGE_RETENTION_FAILED` (a duration of at least 1 minute each; unset keeps that status forever), checked every `FORGE_RETENTION_INTERVAL` (10 s). One goroutine deletes finished jobs older than their retention in batches of 1,000 (up to 100,000 per pass) with one `FOR UPDATE SKIP LOCKED` delete statement, served by a new partial index `jobs_finished_idx` on `finished_at` for finished rows only. Queued and running jobs are never deleted, and the queued-job counter is unaffected (its delete trigger only fires for queued rows). A deleted job is gone: `GET /v1/jobs/{id}` answers 404 and its `Idempotency-Key` is forgotten. A counter `forge_retention_purged_total{status}` counts what was deleted.

**Checked on AWS.** With succeeded kept 1 minute and failed kept 2 minutes: scratch rows with controlled ages were deleted exactly when expected (an old and an aged succeeded job, an old failed job; a fresh failed job stayed until its 2 minutes passed; an old queued job was never touched). Real jobs from a 400-order run were readable (HTTP 200), then 404 after their retention passed, all 1,440 were deleted, and the metric counted them. A first version of the delete statement failed with "undefined column" because the query layer mis-parsed `@name::type`; it is written with `CAST(@name AS timestamptz)` now.

**Cost under load** (60,000 orders per run, about 2 minutes, 200 shoppers (labelled 360), retention alternated off/on, succeeded kept 1 minute so the purge was deleting during the run): off 498 and 506 orders/s, on 511 and 507, all drained within 0.6 s. No measurable cost. The table stayed bounded: about 98,000 rows after the retention runs against 216,000 and 314,000 (and growing) after the runs without it.

**Not done.** Retention is global, not per job type; the migration for the index was written by hand (Atlas's dev database needs Docker, which this account cannot use), so `atlas migrate diff` was not run against it; deleting leaves dead rows that autovacuum must clean (not measured over a long run); and the write-ahead log of the Forge database (3.8 GB kept from the earlier setting) was not shrunk. The deployed Forge now runs with succeeded kept 10 minutes and failed kept 24 hours.

## 21. Does the split let the order service scale on its own? (session 6)

**The question.** With payment in its own service, can the order-service tier be scaled independently by running more copies? Setup: a second machine (`shop2`, `m7i-flex.large`) running a second copy of the order service with its own payment service, nginx on the load generator in front (round robin), and the first copy as before (the dispatcher stays on the first machine). 400 and 200 shoppers (labelled 500), 12,000 orders per run, runs alternated. "End to end" below is orders divided by (load time plus the time the background needed to drain), because a faster foreground that leaves a backlog is not a faster system.

| Configuration | Foreground orders/s | Drain after load | End to end orders/s |
|---|---|---|---|
| 1 copy (pool 10), 200 shoppers (labelled 400) | 476, 469, 472 | 0.3-0.6 s | 471, 458, 465 |
| 2 copies (pool 10 each), 200 shoppers (labelled 400) | 530, 518, 547 | 0.4-1.4 s | 522, 504, 515 |
| 1 copy, 200 shoppers (labelled 500) | 475 | 0.5 s | 465 |
| 2 copies, 200 shoppers (labelled 500) | 538 | 0.6 s | 524 |

- **A second copy gave about +10 to 13%**, although the first copy's machine peaked at only about 50% CPU and the second one's at about 22%. The shop database stayed at 99-100% and the results machine at 90-99%. So the order tier was not out of CPU; something else about running two copies helped.
- **The explanation is database connections.** Each process holds a pool of at most 10 connections to the shop database, so two copies doubled the connections that the order service could use. Making the pool size configurable (`FORGECART_DB_MAX_CONNS`, default 10) and testing a single copy:

| Pool size (1 copy, 200 shoppers (labelled 400)) | Foreground orders/s | Drain | End to end orders/s |
|---|---|---|---|
| 10 | 467, 472 | 0.4 s | 460, 465 |
| 20 | 531, 543 | 0.4-0.7 s | 515, 533 |
| 40 | 641, 637 | 4.6 and 6.2 s | 515, 480 |

  A pool of 20 gives the same end-to-end rate as two copies with a pool of 10 (about +12 to 15% over 10). A pool of 40 speeds up the foreground a lot (about 640 orders/s) but the dispatcher and the workers fall 5 to 6 seconds behind: the order service takes a larger share of a database that was already full, and the background gets less. For a burst that hides the problem; under a sustained load the backlog would keep growing. The order service's pool of 20 is the best setting measured, and the number to watch with it is the outbox backlog.
- **The payment service's pool size makes no difference** (pool 20 against 10: 529, 521, 517 against 524, 509, 508 orders/s end to end, within noise), so it stays at the default.
- **So the honest answer:** the split makes the order tier replaceable and separately deployable, and a second copy works (including a second sweeper) with no change to the code, but it does not raise the ceiling: the shop database and the results machine are the limit, and the gain from a second copy was the same as from a larger connection pool. Real independent scaling of the payment tier would matter when the provider is slow (Razorpay) or a payment needs more CPU; it was not the limit here.
- **Limits:** the shop database allows 100 connections and other services already hold about 53 of them, so pool sizes above about 40 do not fit; two copies at pool 20 were not tried for that reason. The load generator also runs nginx (about 9% CPU), so these runs are not directly comparable to earlier ones that went to the shop directly.
- **Deployed:** the order service runs with a pool of 20; the second machine is stopped.

## 22. Expiry of unpaid orders (session 6)

**The gap.** An order that was created and never paid kept its stock reserved for ever (only the Razorpay path declined an expired link, and only when someone asked again). In the load tests about 5% of orders end that way (temporary failures that nobody retries).

**The change.** An expirer in the shop process (`FORGECART_ORDER_EXPIRY`, default 30 minutes, 0 turns it off; checked every 30 s, at most 2,000 orders per pass, oldest first) cancels orders that stayed `pending_payment` and returns their stock. It first asks the payment service to close the order's payment (`POST /v1/payments/cancel`), because a payment may be in flight or a Razorpay customer may still be able to pay: the service returns the order's final payment if there is one (which the shop applies, so a payment that succeeded makes the order paid, not cancelled), or records a decline under the key `expire:<order>` after which no payment for the order is accepted; for Razorpay it first cancels a link that could still be paid. The sweeper applies such a decline as a cancellation too. A partial index `orders_pending_created_idx` (pending orders only) serves the query.

**Checked on AWS** (expiry 1 minute, every 10 s, simulator unless noted):
- The first pass cancelled the 592 unpaid orders left in the shop database by earlier load tests, with 592 closing declines recorded in the payment service and no stock-ledger violation.
- An order never paid: cancelled, stock released. An order with a temporary failure first: cancelled after the second (closing) attempt. An order whose success was recorded in the payment service without the shop being told: ended `paid` with its one event, not cancelled. Paying an expired order, or retrying with a new key: `409 order_not_payable (status: cancelled)`.
- Razorpay (test mode): an unpaid order's payment link was `created` at Razorpay before the expiry and `cancelled` after it, the order was cancelled with its stock released, and the closing decline was recorded.
- Cost of the index under load (200 shoppers (labelled 400), 12,000 orders, alternated; the first run after a restart had a 9 s drain and is left out): index on 506, 508, 503 orders/s end to end, off 519, 513; about 2%, kept.

**Not done / limits.** The test tool's integrity checks now accept `cancelled` orders (one declined attempt, no success), but they were checked by the manual cases above, not by a long run with expiry firing during the load; the expiry time is one value for all orders; the Razorpay link lives 16 minutes, so an expiry below that cancels the link early; the closing decline appears in the payment service as an ordinary declined attempt marked only by its key.

## 23. Razorpay webhook and a Razorpay outage (session 6)

**The gap.** A Razorpay payment was only noticed when something asked again, so a customer who paid and closed the tab waited for the expirer (30 minutes by default), and nothing had been tested with Razorpay unreachable.

**The change.** An optional webhook listener on the payment service (`FORGECART_PAYMENTS_WEBHOOK_ADDR` and `FORGECART_RAZORPAY_WEBHOOK_SECRET`), written from Razorpay's documentation (signature `X-Razorpay-Signature`, HMAC-SHA256 over the raw body; event names `payment_link.paid`, `.expired`, `.cancelled`; answer 2xx within 5 seconds; retries with backoff for 24 hours; delivery is at-least-once and unordered; ports 80 and 443 only). A verified event is only a hint: the handler takes the order ID, reads the link from Razorpay's API and decides as a payment request would.

**Checked on AWS** (Razorpay test mode and a real payment you made on the hosted page; the events were signed by me with the test secret, not sent by Razorpay):
- Rejections: no signature, a wrong signature and a non-hex signature were `401`; a GET was `405`; a 70 KB body with a valid signature was `413`; garbage JSON with a valid signature was `400`. A `partially_paid` event and a foreign `reference_id` were `200` and ignored.
- A correctly signed `paid` event for an order whose link was **not** paid was answered `200` and changed nothing (no attempt recorded, order still pending), because the link is re-read from Razorpay.
- **Razorpay unreachable** (the service pointed at a closed port): the webhook for the paid order answered `503` (so Razorpay would deliver it again) and the customer's own request answered `502 payment_provider_error`; the order and the payment database were unchanged. After the address was restored, redelivering the event answered `200` and recorded one succeeded attempt (Razorpay's payment reference, key `webhook:...`). The order was still pending at that moment, as designed.
- The customer's request with a new key then got that attempt back as a replay, the order became paid with one published event and all four job results, and two more deliveries of the same event changed nothing (still one attempt; the metric counted three `ok`). A further new key also got the replay.

**Not done.** Real deliveries from Razorpay were not tested: they need a public HTTPS endpoint on port 80 or 443 and the webhook registered in Razorpay's dashboard (and a firewall change on AWS), which only you can do. The payload shape comes from Razorpay's documentation, not from a captured real delivery. The webhook listener has no rate limit (it is bounded by a 64 KiB body and a cheap signature check). Secret rotation is not supported. The expirer's head-of-line blocking during a Razorpay outage, the lost link-creation answer, and the lack of retries inside the service (the other gaps listed after the outage question) are still open.

## 24. Razorpay poller (session 6)

**The gap.** The webhook needs a public HTTPS endpoint on port 80 or 443 and a dashboard setup, so a Razorpay payment could still sit unnoticed until the expirer (30 minutes). A poller in the payment service now settles Razorpay links that nobody has asked about (every 30 s by default, up to 50 links created within 2 hours, least recently checked first, `razorpay_links.checked_at`), using the same code as the webhook. Attempts it records have the key `settle:<order>`; the webhook uses the same key (renamed from `webhook:`).

**Checked on AWS** (Razorpay test mode, a real payment you made, no webhook configured, poll interval 5 s, the shop's sweeper set to 5 s for the test):
- The payment on the hosted page was found and recorded by the poller alone within seconds (key `settle:`), the shop's sweeper then marked the order paid, one event was published and all four job results appeared; no customer request and no webhook were involved.
- An unpaid link was declined by the poller on its own, about 40 seconds after its `expire_by`: the order became `payment_failed`, its stock was released and the stock ledger had no violation. (This is earlier than the 2-minute grace I had measured Razorpay's status to need in an earlier session, so Razorpay reported the link as expired by then this time; either way the poller handles it.)
- **A bug found by the test and fixed:** with Razorpay unreachable, a poll pass with fewer than 3 unsettled links reported `ok`, so an outage was neither logged nor counted. A pass that could not settle some link is now reported as failed. After the fix, an outage of 22 seconds showed 4 failed passes, each logged, and after Razorpay was reachable again the passes were `ok` within seconds.

**Not done / limits.** The payment that the poller settled completed before the outage window of my test began, so "a payment made during an outage is picked up after recovery" was shown by the webhook test (section 23) and by the poller's recovery to `ok` passes, not by one payment going through both. The poll costs about one Razorpay API call per unsettled link per interval; Razorpay's API rate limits were not checked. The poller only runs with the Razorpay provider.

## 25. How many at once, and which service is the weak one (session 6)

**Why this was measured.** The question: how many customers can check out at once, how many pay at once, and how loaded is each of the four background services, so that each can be scaled on its own? New measurements were added for that: concurrent requests and their peak per route in the order service and the payment service, the orders awaiting payment, and per worker the jobs running against the slots (docs of the metrics in `forgecart/README.md`; the dashboard has a "Capacity" row). Services were restarted before each level so every peak belongs to that level alone.

**A flaw in the load tool, found by this sweep.** The tool capped the running shoppers at 200 although it accepted up to 500 (see the correction at the top of section 18). The first attempt at this sweep showed it: the payment service's peak was exactly 200 for the levels 200, 300, 400 and 500. The cap is fixed (500, one constant) and the sweep below is the first one with real 300 to 500 shoppers.

**The sweep** (12,000 orders per level, one run each, 90/5/5 payment mix with 100 to 300 ms at the provider, payment service and order service on one machine, order-service pool 20, results database separate; all 25 integrity checks passed and no workflow failed at any level):

| Shoppers | Orders/s | Drain | Workflow p50 / p95 (ms) | Peak checkouts at once | Peak payments at once (order service / payment service) | Shop DB / results DB / workers machine CPU (peak) |
|---|---|---|---|---|---|---|
| 50 | 229 | 0.6 s | 218 / 308 | 50 | 50 / 50 | 68% / 65% / 38% |
| 100 | 367 | 0.5 s | 269 / 392 | 100 | 100 / 100 | 97% / 87% / 55% |
| 200 | 513 | 0.4 s | 373 / 605 | 200 | 200 / 200 | 100% / 97% / 70% |
| 300 | 517 | 0.6 s | 534 / 972 | 296 | 300 / 300 | 100% / 97% / 72% |
| 400 | 574 | 4.5 s | 636 / 1,190 | 400 | 397 / 391 | 100% / 99% / **91%** |
| 500 | 548 | 0.8 s | 830 / 1,632 | 490 | 482 / 447 | 100% / 98% / 72% |

**What it says.**
- **The order service took every concurrent customer asked of it** (up to 500 inside checkout and 480 paying at the same moment, no failure), but that is not the useful limit. **The useful limit is about 200 concurrent shoppers: about 513 orders/s with p95 0.6 s.** Beyond that throughput stops rising (517 at 300) and the only thing that grows is waiting: p95 972 ms at 300, 1.2 s at 400, 1.6 s at 500. The foreground rate at 400 (574) is not sustained: the background fell 4.5 s behind (an unexplained stall, see section 26), so end to end it was about 472 orders/s; at 500 it was about 529. Sustained end-to-end throughput is therefore about 515 to 530 orders/s, which is the ceiling quoted elsewhere (about 1,850 to 1,900 paid jobs/s).
- **Order of saturation, which is the order of "weak service":** the shop database (100% from 200 shoppers), the results database (97 to 99%), then the machine running the four workers (66 to 75% in the later repeats; one run at 400 shoppers showed 91%), then Forge's database (about 70%), the order-service machine (at most 51%), the load generator (at most 11%). Scaling the order or payment application tier would not help; the databases come first. The workers' machine was the first application-side resource to fill up, but section 26 tested whether that mattered and found it did not: the 4.5 s lag at 400 shoppers was not caused by it.
- **Payments at once.** There are two different numbers: payments in progress (peak = all shoppers at the same moment, 400 at 400 shoppers; each lasts about the provider's 0.1 to 0.3 s) and orders awaiting payment (about 580 to 630 after each run: the 5% of orders whose payment had a temporary failure and was never retried). Real customers waiting on Razorpay's page are the second kind and last minutes.
- **The four background services.** Each worker has 32 slots. The average job takes 15 ms at 50 shoppers and 31 to 36 ms at 500 (the same database work gets slower as the database saturates). At 500 shoppers each type runs about 490 jobs/s, so on average 490 x 0.035 s = about 17 of its 32 slots (about half) are busy. The **peak** of jobs in flight reached 32 of 32 at every level, even at 50 shoppers, because a worker claims up to 20 jobs in one batch: the peak alone overstates the load, and the right scaling signal is the busy fraction over time (the dashboard's "busy slots" panel) together with the queue's oldest-job age.
- **Independent scaling, honestly.** The measurements would let each service be scaled on its own signal, but in this deployment only the databases are the limit, and a database cannot be scaled by adding copies of the application. The next real step up needs more database CPU (a bigger machine) or splitting orders across databases.

**Limits.** One run per level; the 400-shopper foreground figure is higher than its end-to-end figure and I did not repeat it; peaks are since the process started (restarted per level) and are sampled by reading the final value, not as a time series; the monitoring dashboard's new panels were built and their queries are the same as these metrics, but were not run against Prometheus in this session.

## 26. Spreading the background services over two machines (session 6)

**The question.** The previous sweep showed the machine that runs the four background workers as the first application-side resource to fill (66 to 91% CPU). Can a service be moved to another machine on its own, and does it change anything? Setup: the second worker machine (`shop2`, the spare 2-vCPU slot) runs two of the four services (fulfillment and notification), the original machine the other two (inventory and invoice); each worker process is unchanged and just points at the same Forge and databases, so nothing in Forge or ForgeCart was modified for this. The two layouts were alternated at 300, 400 and 500 real shoppers, 12,000 orders per run, three rounds (18 runs, one warm-up run before them). "End to end" is orders divided by load time plus drain time.

| Workers | Shoppers | End-to-end orders/s (3 runs) | Mean | Workers CPU peak (first / second machine) |
|---|---|---|---|---|
| all four on one machine | 300 | 515, 515, 509 | 513 | 66 to 69% / - |
| split over two machines | 300 | 538, 517, 530 | 528 | 34 to 36% / 32 to 33% |
| all four on one machine | 400 | 526, 517, 511 | 518 | 69 to 74% / - |
| split over two machines | 400 | 469*, 515, 525 | 503 | 33 to 38% / 33 to 42% |
| all four on one machine | 500 | 526, 516, 459* | 500 | 70 to 75% / - |
| split over two machines | 500 | 534, 515, 527 | 525 | 32 to 36% / 32 to 35% |

(* = a run with a background stall, below.)

**What it shows.**
- **The move works and halves the load per machine** (66 to 75% CPU on one machine against 33 to 42% on each of two), with no change to either service.
- **It does not raise throughput today.** Over all nine runs per layout the mean end-to-end rate is 510 (one machine) against 519 (two), median 515 against 525; without the stalled runs 517 against 525. That is about +2%, inside the run-to-run spread, as expected: the shop database and the results database stay at 100% and 96 to 100% CPU in every run, so they remain the limit. Spreading services only removes a future limit.
- **A background stall that is not the workers' CPU.** In 3 of the 27 runs at 400 to 500 shoppers (one in the section 25 sweep, one in each layout here) the background finished 4.5 to 4.8 s after the load while the foreground was at its fastest (560 to 574 orders/s); the same size of stall appeared with the workers at 38% CPU on two machines, so the explanation I gave in section 25 (workers' machine at 91%) was wrong, and the section is corrected. The dispatcher logged no warnings or errors (the system journal suppresses repeated lines, so that is weak evidence). The cause was then located in the dispatcher (events waiting to be published), see section 27.

**Independent scaling, as far as it was shown.** Each service can be placed on its own machine without touching the others, and each reports the signals to decide when to (concurrent requests, busy slots, queue age). In this deployment those signals say that only the databases need more capacity.

**Limits.** Three runs per cell; the layouts were alternated in one session but the stall makes means unstable (the medians are the safer comparison); only two of the four services were moved, never a single service on its own with more copies; the spare machine slot was used, so no more machines can run at once under the 16-vCPU limit.

## 27. The background stall: where it is, and a second dispatcher (session 6)

**The symptom.** In some runs the last background jobs finished 4.5 to 6 s (or more) after the load ended, instead of the usual 0.4 to 0.7 s ("drain"). Section 26 showed it is not the workers' CPU. Looking back through all the stored reports of the project, long drains (3 to 16 s, and a few runs that never drained) appear in 25 earlier runs, not only in this session; several of them I had put down to "the first run after a machine boot", which I did not verify and now doubt.

**Where the time goes** (stage latencies stored by the load tool for every run). A run that stalled (500 shoppers, drain 4.8 s) against a normal one at the same level (drain 0.7 s):

| Stage | Normal run | Stalled run |
|---|---|---|
| Event written to event published to Forge (the dispatcher) | mean 280 ms, max 482 ms | **mean 2,980 ms, max 5,260 ms** |
| Forge queue wait / job execution (per type, sampled) | max about 480 ms / 290 ms | max about 480 ms / 200 ms |
| Payment to each of the four results | mean about 450 ms, max about 870 ms | mean about 3,150 ms, max about 5,500 ms |

So the whole delay is in one stage: events wait about 3 s between being written and being published to Forge. Forge and the four workers behaved normally. That stage is the dispatcher, which is a single serial loop (claim up to 100 events, send them to Forge in chunks, wait, repeat; it sleeps only when a round comes back short). At about 485 events/s it has little headroom, and it shares the shop database with the foreground traffic.

**Test: one dispatcher against two** (the second is another process of the same program; each event is fenced by its own claim token). Alternated at 400 and 500 shoppers, 12,000 orders, four rounds, 8 runs per configuration, outbox backlog sampled twice a second:

| | One dispatcher | Two dispatchers |
|---|---|---|
| Event publication latency, mean (p95) | 281 ms (414) | 181 ms (303) |
| Peak backlog of unpublished events (mean, max) | 256, 383 | 184, 256 |
| Seconds per run with more than 100 events waiting | 10.8 | 5.4 |
| End-to-end orders/s (mean, range) | 534 (515 to 544) | 531 (519 to 543) |

A second dispatcher cuts the wait before Forge by about a third and halves the time spent with a backlog, and does not change throughput (the databases still set it). **No run stalled in either configuration this time (0 of 16)**, so this does not prove the stall is gone: earlier about 1 run in 10 at these levels stalled, and 16 runs is too few to tell zero from one in ten. The honest reading is: the stall is a backlog in the dispatcher, a second dispatcher doubles the headroom and measurably shrinks the backlog, and the deployed system now runs two.

**What triggers a large backlog is not known.** The shop database's checkpoints during a stalled run were small (286 and 2,669 buffers written over minutes); autovacuum is not logged, so it is neither confirmed nor excluded as the cause of a pause.

**Not done.** The dispatcher was not changed. A better design would not wait for the previous batch before claiming the next (a pipeline), or would send its chunks to Forge concurrently (Forge accepts 3 submissions at once per process); both would need their own tests and a measurement. The backlog is visible in `forgecart_outbox_pending_events` and `forgecart_outbox_oldest_pending_age_seconds` (the dashboard has them).

## 28. Repeated headline runs, and a 60-minute soak (session 6)

Two questions: how much do the headline numbers vary from run to run, and does the system keep working for an hour at a steady load. Same deployment as section 27 (two dispatchers, all four workers on one machine, results database separate, payment service on the simulator with 90% success, 5% declined, 5% temporary failure, 100 to 300 ms). The shop, dispatchers, payment service and workers ran with `FORGECART_LOG_LEVEL=warn` (new this session): at the default level a worker wrote about 0.8 MB/s of log lines and would have filled its 11 GB disk during the soak.

**A. Five runs each, same session.** End-to-end orders per second (workflows/s while the load runs, combined with the drain time):

| Shoppers | Runs | Mean | Range | Workflow p95 (ms) | Failed | Integrity checks |
|---|---|---|---|---|---|---|
| 200 | 548, 544, 549, 538, 551 | 546 | 538 to 551 | 520 to 544 | 0 | all pass |
| 500 | 572, 587, 573, 577, 598 | 581 | 572 to 598 | 1,418 to 1,491 | 0 | all pass |

Run to run the spread is about 2% at 200 shoppers and about 4% at 500. Drain after load was under 1 s in every run.

**B. Soak: 100 shoppers for 3,600 s** (about 70% of the rate the 200-shopper runs reach), through nginx, on a freshly emptied database.

| Measure | Result |
|---|---|
| Orders completed | 1,302,376 (0 failed, 0 interrupted, 0 HTTP failures) |
| Paid orders | 1,172,204; 65,010 declined; 65,162 temporary failures (the simulator's 5% / 5%) |
| Background jobs | 4,688,816 submitted, claimed and succeeded (exactly 4 per paid order); 0 outbox events unpublished, 0 failed |
| Drain after the load stopped | 3.5 s |
| Integrity checks | 25 of 25 pass |
| Forge queue wait p50 / p95 | 189 / 469 ms (bucket estimates) |
| Largest backlog seen | 572 jobs queued in Forge, 124 unpublished outbox events |

Orders still `pending_payment` at the end were 31,464; 33,698 had been cancelled by the order expirer (unpaid after 30 minutes, stock returned). That is the expiry path running under load, not a leak.

**Throughput drifted down, then levelled off.** Completed orders per second, by five-minute block: 396, 383, 379, 371, 361, 354, 351, 352, 353, 347, 348, 346. Workflow p95 rose from about 349 ms to about 395 ms. So over the hour throughput fell about 13% and the fall stopped after about 30 minutes; this is the rate to quote for an hour of sustained load (about 350/s), not the first minutes (about 396/s). The cause is not proven. What changed over the hour: the shop database grew from 28 MB to 1.86 GB and its CPU sat at 93 to 100% from the first minutes (so it was near its limit throughout), results database 39 MB to 3.2 GB. A larger table and its indexes means more work per order; that fits the drift but I did not test it.

**What stayed flat.** Process memory: shop 22 to 29 MB, payment service 20 to 24 MB, each dispatcher 19 to 20 MB, each worker 20 to 21 MB, with no upward trend (so no leak visible at this size). Dead rows in the shop database peaked around 214,000 and fell again, so autovacuum kept up (inferred from dead-row counts; its activity is not logged). Highest disk use: the workers machine 73% (71% before), the results database 62%, the shop database 44%; Forge's retention (10 minutes for succeeded jobs) held its table steady. No restarts or errors were reported by the load tool apart from one failed poll of Forge's job list (`poll_errors: 1`).

**Limits.** One run of one hour, one configuration; the load was closed-loop (a shopper waits for each reply), so it shows steady operation, not how the system behaves when more arrives than it can take. One hour does not show what happens over days. The shop database at 100% CPU means there is no headroom for a real traffic burst on this machine size. Job-latency figures come from a sample of 300,000 jobs; counts and integrity checks cover all of them.

Raw data: `~/.forge-aws/results/soak60min/remote-cart.json` and `soak-health.txt` (outside the repository).

## 29. Where the shop database's time goes (session 7)

The shop database was at 93 to 100% CPU in the soak (section 28), so I profiled it with `pg_stat_statements` over one 30,000-order run at 200 shoppers (529 orders/s, 26,997 of them paid, after a warm-up run). The statements ran for about 125 s in total over a run of about 60 s on a 2-vCPU machine, so statement execution accounts for essentially all the CPU: the database is busy doing the work, not waiting on something else. That is about 3.6 ms of database time per order, and it matches the observed ceiling (2 cores / 3.6 ms is about 550 orders/s).

| Share of statement time | What |
|---|---|
| 11% | insert the order's lines (`order_items`, 0.23 ms each) |
| 11% | the load tool's own progress query (counts whole tables every second; a measurement cost, not shop work) |
| 11% | insert the outbox event |
| 8% each | insert the order; update it to paid; update it with the priced totals; read and lock the products |
| 7% | subtract stock (0.14 ms each) |
| 12% | the dispatcher's three writes per event (claim, count the attempt, mark published) |
| 9% | the four workers' reads of the event, the order and its lines (11 small reads per paid order, 0.03 to 0.05 ms each) |

No single statement is slow: all the per-order statements take 0.03 to 0.5 ms. Index scans are all used except `order_items_pkey` (an identity key nothing looks up by). 97% of product updates and 67% of order updates are HOT updates (they skip index maintenance), and dead rows were 0 at the end, so there is no vacuum or index problem to fix.

**What I did not change, and why.** Every item above is work the order needs, and the largest is about a tenth of the total. The candidates I could see were: drop the second order update by pricing before inserting (about 8%, but it changes the order of the idempotency check on the checkout path); count the dispatcher's attempt at claim time instead of just before the Forge request (about 4%, but it changes what the attempt counter means after a crash); merge the workers' three reads into one (about 3 to 4%); drop the unused `order_items_pkey` (a few percent of one insert). Each would be inside the 2 to 4% run-to-run spread I measured in section 28, so I could not tell a gain from noise without many repeats, and the first two touch correctness-critical code that a database function already failed to speed up in an earlier session. The remaining ways to go faster are a larger database machine (costs money) or splitting the data across several databases (sharding; a large redesign of checkout), not tuning.

**One finding about the benchmark itself.** The load tool's progress query takes about 220 ms and uses about 11% of the shop database's statement time during a run, so the throughput figures in this document are slightly lower than the shop would reach without an observer attached. It is a measurement cost and applies equally to every run, so comparisons between runs stand.

## 30. Many buyers for the same product (session 7)

Checkout locks the product rows it buys (`SELECT ... FOR UPDATE`, section 4.1 of `architecture.md`), so
buyers of the *same* product wait for each other. Until now every load run spread orders over the whole
catalogue (3,005 products, 1 to 3 per order). Two new options of the load tool, `--hot-products N` (every
shopper orders only the first N products by SKU) and `--inventory N` (stock per product at the start), made
two tests. Same deployment as section 28 (simulator payments, two dispatchers, results database separate);
200 shoppers, 12,000 orders per run, run through nginx.

**A. How much does contention cost?** Four spreads, three rounds, the order rotated each round. The first run
after the machines were started (all products, 741 orders/s, with a 21 s background drain) is the known
cold-start artefact (sections 14 and 28) and is **excluded**; the all-products figure below therefore rests on
two runs.

| Products the shoppers choose from | Orders/s (each run) | Mean | vs all products | Workflow p95 (ms) | Integrity checks |
|---|---|---|---|---|---|
| all 3,005 | 499, 492 | 495 | (baseline) | 615, 622 | 25/25 |
| 100 | 525, 522, 508 | 518 | +5% (inside the spread) | 571, 565, 587 | 25/25 |
| 10 | 378, 370, 373 | 373 | **-25%** | 875, 893, 884 | 25/25 |
| **1** | 301, 302, 298 | 300 | **-39%** | 1,111, 1,116, 1,127 | 25/25 |

Every run finished 12,000 orders with 0 failed workflows, and the background drain after the load was under
a second. The foreground rate is shown; with such short drains the end-to-end rate is within about 3% of it.
This session's all-products figure (495) is lower than the 546 of section 28 (a different boot and session);
only the comparison *within* this table is meaningful. With one product every order has exactly one line
(an order picks 1 to 3 *different* products), so those runs do less work per order than the others, which if
anything understates the cost of contention.

What this says: contention is invisible with 100 hot products, costs about a quarter at 10, and about 40%
with a single product, where throughput settles near **300 orders per second** and latency roughly doubles.
That is about 3.3 ms per order, which fits a lock held from the row lock until the commit reaches the
(network) disk, so that all orders for the one product go through one at a time. That explanation is a
hypothesis consistent with the numbers; it was not separately proven. Not tested: more than 200 shoppers on
one product, and a real flash sale where far more buyers than stock arrive within seconds.

**B. Selling out.** One product, **3,000 units**, 200 shoppers, 12,000 purchase attempts (1 to 3 units each).

| Result | Value |
|---|---|
| Orders created | 1,590 (1,422 paid, 78 payment declined, 90 temporary failure left unpaid) |
| Purchases refused with "insufficient inventory" | 10,410 |
| Units held by orders at the end | **3,000** exactly |
| Stock left | **0** (never negative) |
| Units returned by the 78 declined payments, and sold again to later buyers | 158 |
| Integrity checks | 25 of 25 (including the stock ledger: starting stock = stock now + units held, and no negative stock) |
| Time for all 12,000 attempts | about 8 seconds (refusals are fast) |

So under 200 concurrent buyers exactly the available stock was sold, no unit twice, and the units given back
by declined payments were re-sold. This is one run.

**What would help if one product must sell faster.** None of this was built. The usual approaches are to split
a product's stock into several rows ("buckets") that checkouts pick between, or to accept reservations in a
queue and confirm them in batches; both change the checkout design and need their own measurement.

## 31. Forge's database on Neon (managed PostgreSQL) against PostgreSQL on EC2 (version 2, first result)

Question: can Forge's own database be a managed service instead of our self-managed PostgreSQL on EC2, and what does it cost? Two measurements, both from machines in the same region as the Neon project (`aws-ap-southeast-2`, Sydney). Neon was on its **Free plan**, direct connection (not the pooler). It reported **PostgreSQL 18.6**; the EC2 database is 16.15, and all 9 of Forge's migrations applied to Neon without changes.

**A. A probe of the connection** (`neonrtt`, one run each, Forge machine to the database).

| Measure | EC2 PostgreSQL | Neon Free |
|---|---|---|
| Trivial query round trip, median | 0.09 ms | 0.45 ms |
| Write transaction with commit, median (95th percentile) | 1.39 ms (1.81) | 2.33 ms (3.04) |
| Write transactions per second with 8 connections | 4,053 | 1,814 |
| Write transactions per second with 32 connections | 14,081 | 3,252 |
| First connection | 5.8 ms | about 1,056 ms (probably the compute waking from scale-to-zero) |

**B. Forge alone, same code and settings, only the database changed** (open-loop load, batches of 10 jobs per request, 40 s per stage, 2 synthetic workers of 24 slots with a 25 ms handler on one machine, a fresh `forge_bench` database on each side with its retention at 1 minute). The order was warm-up on Neon (discarded), then EC2, Neon, EC2, Neon.

| Offered jobs/s | Completed during load, EC2 (two runs) | Completed during load, Neon (two runs) | Drain after load, EC2 / Neon |
|---|---|---|---|
| 800 | 798.9, 795.9 | 798.0, 794.3 | 0.3 s / 0.3 s |
| 1,600 | 1,448, 1,457 | 1,270, 1,249 (**about -13%**) | 4.0-4.3 s / 10.4-11.4 s |
| 2,400 | 1,419, 1,423 | 1,242, 1,267 (**about -12%**) | 27.0-27.3 s / 36.9-39.1 s |

Every stage on both sides accepted what was offered (no `queue_full` or `admission_busy` rejection). At 800 jobs/s the two are the same. Above the point where completions stop keeping up, Neon completed about 12-13% fewer jobs per second and took 10 to 12 seconds longer to drain.

**What it says.** For Forge's queue, Neon Free was far closer to EC2 than the probe (A) suggested: the same at 800 jobs/s, about 12-13% lower at the saturated end. The 3,252 write transactions per second of the probe is a ceiling on many small concurrent commits, and Forge's batching (10 jobs per submission, up to 20 per claim and completion) keeps it well below that.

**Limits of this result.** Two runs per side; one availability zone; the plateau of about 1,450 jobs/s on EC2 is lower than the 2,350 measured in section 9 because this test used one workers machine of 48 slots (the workers machine may be part of the limit, so read the comparison, not the absolute rate); single-job submission and Forge's long-running behaviour were not tested; the Neon Free caps (1 GB storage, 5 GB data out per month, 100 CU-hours) were not observed, only worked within; PostgreSQL 18 against 16 is a second difference besides the service; ForgeCart's databases (shop, results, payments) were not yet moved.

## 32. ForgeCart's three databases on Neon (managed PostgreSQL) against EC2 PostgreSQL (version 2, second result)

After section 31 (Forge's own queue database), the shop, results and payments databases were moved to three Neon Free projects in Sydney (direct connections, PostgreSQL 18.6; their schemas applied from the existing migrations and the 3,005-product catalogue copied from the EC2 shop database). Forge's own database stayed on EC2 in both configurations, so this isolates the effect of ForgeCart's databases. The same ForgeCart processes, machines and load were used; only the three database addresses in the services' settings changed (the original settings were kept and restored afterwards).

**Method.** 200 shoppers, 12,000 orders per run, closed loop, 90/5/5 payment mix with 100-300 ms provider delay (as in section 28). Order of runs: Neon, EC2, EC2, Neon, each preceded by a 3,000-order warm-up that was discarded (the services restart at every switch).

| Run | Database | Foreground orders/s | Drain | End to end (orders/s) | Workflow p50 / p95 / p99 (ms) | Failed | Integrity checks |
|---|---|---|---|---|---|---|---|
| 1 | Neon | 484.7 | 0.8 s | 470 | 399 / 601 / 712 | 0 | 25 of 25 |
| 2 | EC2 | 465.4 | 0.6 s | 456 | 413 / 651 / 792 | 0 | 25 of 25 |
| 3 | EC2 | 463.1 | 0.4 s | 457 | 414 / 657 / 784 | 0 | 25 of 25 |
| 4 | Neon | 477.7 | 0.6 s | 466 | 404 / 611 / 726 | 0 | 25 of 25 |

**What it says.** In this session, with the databases on Neon Free the shop finished about **2-3% more orders per second end to end** (466-470 against 456-457) and was slightly faster at the 95th percentile (601-611 ms against 651-657 ms). The difference is at or just above the usual run-to-run spread (2-4%), so the safe reading is: **no worse, possibly marginally better**, and all 25 integrity checks passed on every run on both sides. This is better than I expected from the connection probe in section 31 and from the extra network hop; I did not find out why. Possible reasons, none tested: Neon's compute is not the saturated 2-vCPU machine the EC2 shop database was, its storage path does not wait on a local network disk the same way, or the limit in this configuration is elsewhere (Forge's own database, the workers or the Forge machine) so that neither database was the binding constraint at 200 shoppers.

**Limits of this result.** Two runs per side; 200 shoppers only (not the 500-shopper level, the one-hour soak or the same-product test); this session's EC2 figure (457) is lower than the 546 of section 28, which was not explained (different day and machines restarted), so the comparison is meaningful only within this table; no Neon-side metrics were read (compute size reached, data transferred out, compute hours used), and the Free plan caps (1 GB storage and 5 GB data out per project per month, 100 CU-hours) were not observed, only worked within; Forge's database remained on EC2; PostgreSQL 18 on Neon against 16 on EC2 is a second difference besides the service itself; the results database and the payments database were separate projects here, while on EC2 they share one machine.

## 33. ForgeCart on Neon against EC2 at 500 shoppers (version 2, third result)

Same setup as section 32 (shop, results and payments databases on three Neon Free projects against the EC2 databases; Forge's own database on EC2 in both), but 500 shoppers and 15,000 orders per run, each preceded by a discarded 3,000-order warm-up. Order: Neon, EC2, EC2, Neon. The machines had been stopped and started again just before, so run 1 was the first load after boot.

| Run | Database | Foreground orders/s | End to end (orders/s) | Workflow p50 / p95 / p99 (ms) | Failed | Integrity checks |
|---|---|---|---|---|---|---|
| 1 | Neon | 433.7 | 425 | 1007 / 2246 / 3157 | 0 | 25 of 25 |
| 2 | EC2 | 503.4 | 492 | 902 / 1804 / 2319 | 0 | 25 of 25 |
| 3 | EC2 | 507.6 | 499 | 893 / 1775 / 2338 | 0 | 25 of 25 |
| 4 | Neon | 488.0 | 474 | 926 / 1871 / 2382 | 0 | 25 of 25 |

**What it says.** At 500 shoppers the result is not the "Neon is slightly better" of section 32. EC2 finished 492-499 orders/s end to end; Neon finished 425 and 474. The two Neon runs differ from each other by 11%, far more than the 0.7% spread of the two EC2 runs, so Neon at this load is **between 4% and 14% slower than EC2 and much less repeatable**; the p95 was 1,871-2,246 ms against 1,775-1,804 ms. No order failed and all 25 integrity checks passed on every run.

**Not explained.** I do not know why run 1 was so much lower than run 4. It came first after a restart of every machine, so a cold effect (Neon compute starting from idle and scaling up, cold caches on the Neon side) is the likeliest candidate, but I did not read Neon's compute size or any Neon-side metric, so this is a guess. With only two runs per side and a gap this uneven, the ranking of the two backends at 500 shoppers is not settled; at 200 shoppers (section 32) they were level.

**Limits.** Two runs per side; one session; Neon-side metrics, the Free caps (storage, data transferred out, compute hours) and a longer run were not observed; Forge's own database stayed on EC2; PostgreSQL 18 against 16.

## 34. Repeat of the 500-shopper comparison after a long warm-up (version 2, fourth result)

Section 33 left one question: was its low first Neon run (425 orders/s) a cold start after the machines were restarted? This repeat started the machines again, ran a **12,000-order warm-up** before the first run (3,000 before the others, all discarded), and used the order Neon, Neon, EC2, Neon. Everything else is as in section 33 (500 shoppers, 15,000 orders per run, Forge's own database on EC2 throughout).

| Run | Database | Foreground orders/s | Drain | End to end (orders/s) | Workflow p50 / p95 / p99 (ms) | Failed | Integrity checks |
|---|---|---|---|---|---|---|---|
| 1 | Neon | 504.1 | **44.6 s** | **202** | 843 / 2028 / 3267 | 0 | 25 of 25 |
| 2 | Neon | 479.0 | 0.7 s | 468 | 942 / 1903 / 2451 | 0 | 25 of 25 |
| 3 | EC2 | 437.6 | 0.7 s | 429 | 1036 / 2100 / 2699 | 0 | 25 of 25 |
| 4 | Neon | 455.4 | 0.8 s | 445 | 983 / 2021 / 2591 | 0 | 25 of 25 |

**What it says.**

1. **The long warm-up did not remove the variation, so a cold start is not the explanation.** After it, Neon gave 468 and 445 and the single EC2 run gave 429. Across sections 33 and 34 the Neon end-to-end figures at 500 shoppers are 425, 474, 468 and 445, and EC2's are 492, 499 and 429. The ranges overlap (Neon 425-474, EC2 429-499). **I cannot say either backend is faster at 500 shoppers.** The EC2 figure itself moved by 14% between two sessions on identical software, which is larger than the Neon-against-EC2 difference, so the session-to-session spread (machines restarted, different host placement and database state) is the bigger effect in these measurements.
2. **Run 1 had a 44.6 s drain.** The shoppers finished 15,000 orders at the highest rate of the four (504 per second, which is why its foreground number is high), but the background work (jobs for the workers) kept running for 44.6 s afterwards at about 1,200 jobs per second against 60,000-110,000 per second in the other drains, so end to end it counts as 202. All 25 integrity checks still passed and nothing was lost, so the work was only late. I did not find the cause: the run came right after the long warm-up, and I did not read the Neon-side or worker-side metrics while it happened. It is recorded as an anomaly, not as a typical result and not removed. It is also the only run in the table that was slow in the background rather than in the foreground, so it may point at something that the other 14 runs on Neon and EC2 so far did not show; it needs a repeat before anything is concluded.

**Limits.** Neon: three measured runs here plus the earlier ones; EC2: one run here; Neon-side metrics (compute size, data out, storage, CU-hours) and the Free caps were not read; PostgreSQL 18 against 16; Forge's own database on EC2.

## 35. Ten-minute soak, ForgeCart on Neon against EC2, same session (version 2, fifth result)

A one-hour soak on Neon Free is not possible: the same test on EC2 (section 28) grew the shop database to 1.86 GB and Neon Free allows 1 GB per project. So this is a **600-second soak at 100 shoppers**, Neon first and EC2 second in the same session (3,000-order warm-up before each, discarded), with the same machines and software; only the three ForgeCart databases changed (Forge's own database on EC2 in both).

| Database | Orders completed | Orders/s | Workflow p50 / p95 / p99 (ms) | Failed / interrupted | Drain | Integrity checks |
|---|---|---|---|---|---|---|
| Neon | 209,459 | 348.9 | 285 / 394 / 453 | 0 / 0 | 0.7 s | 25 of 25 |
| EC2 | 221,085 | 368.2 | 270 / 385 / 447 | 0 / 0 | 0.5 s | 25 of 25 |

**What it says.** At 100 shoppers for ten minutes the two backends behave alike: Neon completed about **5% fewer orders per second** (348.9 against 368.2) with p95 394 against 385 ms. No order failed or was interrupted, the background work drained within a second, and all 25 integrity checks passed on both. One run per side, so a 5% gap is within what sections 32-34 showed between sessions (14% on EC2 alone at 500 shoppers) and I do not claim it as a difference. Database sizes after the Neon run: shop 301 MB, results 511 MB, payments 114 MB, so a 10-minute soak used about half of each project's 1 GB at this rate (about 2.4 KB per order in the results database).

**An aborted first attempt.** The first Neon soak was set to 900 s but stopped by a size guard I had set at 700 MB when the results database reached 712 MB (at 823 s, 287,348 orders, 348.9 orders/s, p95 394 ms, 0 failed). That left only 20 of the 25 integrity checks (the rest failed with "context canceled") and no drain measurement, so it is not a valid soak and is not in the table; the guard threshold was my own conservative setting and not a Neon failure. The 700 MB figure is a reminder that on Free storage, not speed, limits how long a soak can run: the 1 GB cap is reached at about 400,000 orders (at 2.4 KB per order in the results database), roughly 20 minutes at 350 orders per second.

**Limits.** One run per side; ten minutes, not an hour; the size guard did not record any in-run samples in the second attempt (the log file was not created), so only the sizes after the Neon run are known; Neon's data transferred out, compute hours and compute size were not read; PostgreSQL 18 against 16; Forge's own database on EC2.

## 36. What limits ForgeCart on Neon: a shopper sweep, the shop's pool size, and the background part (version 2, sixth result)

Setup as in sections 32-35 (shop, results and payments databases on Neon Free, Forge's own database on EC2). `pg_stat_statements` was installed on the three Neon databases for this section (only a statistics view, no data change). All runs 20,000 orders after a discarded warm-up, all 25 integrity checks passed on every run, none failed.

**A. Shopper sweep (shop database pool 20).**

| Shoppers | Foreground orders/s | End to end (orders/s) | Workflow p50 / p95 (ms) |
|---|---|---|---|
| 200 | 438.5 | 435 | 425 / 726 |
| 300 | 458.3 | 451 | 611 / 1,092 |
| 400 | 469.8 | 459 | 779 / 1,518 |

Throughput is flat from about 300 shoppers while latency doubles: this is the plateau, about 460 orders/s. Levels 600 and 800 could not be run: the load tool refuses more than 500 shoppers at once (`shopper concurrency levels must be 1 to 500`), which is a limit of the tool, not of Neon or ForgeCart. (My first two sweep attempts failed instantly for a run id shorter than 6 characters and for that limit; they produced no measurements and are not in the table.)

On the shop database during these runs, the active sessions were mostly waiting on `IPC:SyncRep` (a commit waiting for Neon to confirm the write is replicated): 59 of 108 samples at 200, 68 of 98 at 300, 51 of 98 at 400, with 19, 13 and 21 samples on CPU. Statement time was about 2.4 ms per order (48 s over 20,000 orders, excluding the integrity check's one 5 s query), about 1.1 cores at 470 orders/s, so CPU was not saturated.

**B. Shop database pool size, 400 shoppers (order 20, 60, 60, 20).**

| Run | Pool | Foreground orders/s | Drain | End to end (orders/s) | p95 (ms) |
|---|---|---|---|---|---|
| 1 | 20 | 471.8 | 0.6 s | 465 | 1,498 |
| 2 | 60 | 660.7 | 15.4 s | 438 | 942 |
| 3 | 60 | 698.5 | 15.8 s | 450 | 870 |
| 4 | 20 | 464.1 | 0.7 s | 456 | 1,533 |

With 60 connections the part customers see (create and pay an order) ran **43-50% faster (661-699 against 464-472 orders/s) and about 40% lower p95**, so with a pool of 20 the **shop's database pool was the limit of the front part on Neon**, consistent with every commit holding a connection for the replication round trip. But **end to end nothing improved (438-450 against 456-465)**: a backlog of about 15 s built up behind the front part, so the background part (dispatchers, Forge, workers, results database) now sets the ceiling, at about 455 orders/s whatever the front does.

**C. Who is busy in the background (pool 60, 400 shoppers, one run, 685.2 orders/s foreground, drain 17.1 s).** Machine CPU from `vmstat` every 5 s over 13 samples: **the workers machine peaked at 95% (average 63%)**, the shop machine at 51% (average 23%), Forge at 37% (25%), the load generator at 12%. The results database was not saturated: 34 s of statement time for 20,000 orders (1.7 ms per order, mostly small inserts of 0.18-0.45 ms) and its active sessions mostly in `IPC:SyncRep` (71 of 128 samples). The payments database used 4.7 s in total. So the likeliest background limit is **CPU on the workers machine (four worker processes on 2 vCPU)**; I did not test this directly (for example by moving workers to a second machine), so it remains the leading candidate, not a proven cause.

**Was the Neon shop database out of CPU? No (read from Neon's console, forge-shop project).** The console graphs for the hour that held these runs (shown in local time, about 8:53-9:07 PM) put CPU used at peaks of roughly 1.0-1.3 vCPU against 2 vCPU allocated, RAM used about 1.3 GB of 8 GB, open connections at a few tens against a maximum of 905, and database size at most about 340 MB. These are values read by eye from smoothed graphs (a very short spike could be hidden), and the graph does not say which run each peak belongs to. Taken together with the statement data, the tripling of statement time at pool 60 (155 s against 52 s for the same 20,000 orders; the product lookup 2.12 ms against 0.32 ms, the order-item insert 0.75 against 0.16 ms) was **waiting, not CPU**: most active sessions were in `IPC:SyncRep` (commits waiting for Neon's replication confirmation) and the rest in client reads and short locks. The results and payments projects' graphs were not read.

**Limits.** One run per pool setting pair (two per setting) and one diagnostic run; vmstat samples every 5 s over about a minute; Neon-side data out and CU-hours were not read (CPU, RAM and connections were read for the shop project only, by eye); Forge's own database on EC2; the Neon free compute (up to 2 CU) is fixed, so a bigger compute is not an option inside the Free plan.

## 37. A second workers machine on Neon (version 2, seventh result)

Section 36 named CPU on the workers machine (peak 95%) as the likeliest limit of the background part. This test splits the four workers over two machines: the original workers machine runs the inventory and invoice workers and the spare `m7i-flex.large` runs the fulfillment and notification workers (the eighth machine the 16-vCPU quota allows; the binaries were copied byte for byte and their checksums compared). Neon setup, shop database pool 60, 400 shoppers, 20,000 orders per run, warm-up discarded, peak CPU from `vmstat` every 5 s.

| Run | Workers | Foreground orders/s | Drain | End to end (orders/s) | p95 (ms) | Peak CPU (workers machine / second machine) | Integrity checks |
|---|---|---|---|---|---|---|---|
| 2 | split (2 + 2) | 616.2 | 8.8 s | 485 | 1,165 | 49% / 58% | 25 of 25 |
| 3 | split (2 + 2) | 691.1 | 8.7 s | 531 | 884 | 49% / 54% | 25 of 25 |
| 4 | one machine (4) | 703.9 | 12.1 s | 494 | 865 | 88% / (idle) | 25 of 25 |

The first run of the planned A, B, B, A order did not run (the load tool reported no trial; most likely because I had used the same run id by hand a minute earlier, but I did not check), so the one-machine setting has a single run in this session. Earlier attempts of this test measured nothing: run ids must be 6-16 lower-case letters or digits and mine were 5 characters, then upper-case.

**What it says.** Splitting the workers cut the peak CPU from 88% to about 50-58% per machine and shortened the backlog after the load from 12.1 s to about 8.7 s, but **end to end rose only from 494 to 485-531, which is within the spread of the runs**, and even with each worker machine around half busy the background part still fell 8.7 s behind. So **CPU on the workers machine was not the only limit, and probably not the main one**: with that CPU freed, something else still holds the background part to about 500 orders/s. For comparison, the same one-machine setting measured 432-450 orders/s end to end in section 36, in a different session; this session's single run gave 494, which shows how much a session changes the figure and is the reason I do not claim a gain from this table.

**Where to look next (not tested).** In the pool-60 run of section 36 the shop database's third largest consumer of statement time was the dispatcher's outbox query (`WITH due AS (SELECT ... FROM outbox_events WHERE status = 'pending' ...)`): 567 calls at 16.5 ms each, 9.4 s in total, much slower than the 0.04-0.2 ms of the other statements. The two dispatchers, each with a pool of 10, take jobs out of the outbox and submit them to Forge, so their rate is a candidate for the remaining limit; Forge's own database (commit ceiling about 2,350 jobs/s, about 650 orders/s at four jobs per paid order) is the other.

**Limits.** One run for the one-machine setting and two for the split; one session; the second machine's CPU on its own was not isolated from the first; Neon-side metrics for the results project were not read; Forge's own database on EC2.

## 38. Where the background backlog sits, and more worker slots (version 2, eighth result)

Section 37 left the background part at about 500 orders/s with neither CPU nor the dispatcher batch size (already 100 events per round with a 200 ms poll) as the obvious limit. Neon setup, shop pool 60, workers split over two machines, 400 shoppers, 20,000 orders.

**A. Where does the backlog build?** One run with a sample every 2 s of the unpublished events in the shop's outbox and of Forge's queued and running jobs (706.1 orders/s foreground, drain 10.3 s):

- **Shop outbox:** about 180-330 unpublished events throughout the load, then 0. The dispatchers publish about as fast as orders are paid, so they are not the limit.
- **Forge's queue:** queued jobs rose steadily from 0 to about 20,200 at the end of the load (about 28 s) and fell to 0 by about 41 s, a drain of about 2,000 jobs/s. **The backlog is in Forge's queue, behind the workers.**
- **Running jobs:** 95-125 at all times, against 4 workers x 32 slots = 128: nearly every worker slot was busy. At about 1,900-2,000 jobs/s that is about 60 ms from claim to completion per job (Little's law, an estimate).
- The same run showed Forge's admission slots at 0 in use at every sample, and my rejection counter reading 0, but that counter is a sum over metric names I guessed (it may match none), so I do not claim from it that no submission was ever rejected. The first attempt of this test recorded the trial (687.3 orders/s, drain 10.9 s) but no samples, because the sampler did not start.

**B. Slots per worker, 32 against 64 (order 32, 64, 64, 32).**

| Run | Slots per worker | Foreground orders/s | Drain | End to end (orders/s) | p95 (ms) | Peak CPU (workers / second machine) | Checks |
|---|---|---|---|---|---|---|---|
| 1 | 32 | 657.7 | 9.8 s | 498 | 929 | 39% / 36% | 25 of 25 |
| 2 | 64 | 593.3 | 0.7 s | 581 | 1,028 | 33% / 30% | 25 of 25 |
| 3 | 64 | 643.7 | 2.7 s | 592 | 982 | 34% / 31% | 25 of 25 |
| 4 | 32 | 654.9 | 10.0 s | 494 | 951 | 37% / 35% | 25 of 25 |

**What it says.** With 64 slots per worker the background part kept up almost completely (drain 0.7-2.7 s against 9.8-10.0 s), and **end to end rose from 494-498 to 581-592 orders/s, about 17-20%**, with the two runs on each side within 2%. The foreground rate dipped slightly (593-644 against 655-658) because the extra worker activity competes for the same databases. This is a larger and more consistent effect than any change before it in this session series, and with it the Neon setup (581-592) is at or above the 546-581 of the earlier EC2 sessions, though those were different sessions and the EC2 setup was not retuned in this one, so this is not yet a same-session comparison. Worker machines were at only 30-39% peak CPU, so there is room for more slots.

**Check on the setting.** My script printed `Permission denied` where it should have confirmed the new value in `/etc/cartw.env` (a `grep` without `sudo`); the sed that changes it ran with `sudo`, and the behaviour changed in the expected way (shorter drains with 64, the same drains as before with 32), so the change was applied, but the file itself was not read back.

**Limits.** Two runs per setting; one session; the front part now sets the ceiling (593-644 foreground, with the shop pool at 60) and Forge's own database (about 650 orders/s at four jobs per order, measured earlier) is close behind; slot counts above 64 were not tried; Neon-side metrics for the results project were not read.

## 39. An incomplete session: 128 worker slots and the matched Neon against EC2 comparison were not finished (version 2)

This session was meant to test 64 against 128 worker slots on Neon (order 64, 128, 128, 64) and then compare the Neon and EC2 databases with the same tuning. It was stopped early after the first two runs, for two reasons recorded here so that nothing is read into the missing results.

**What was measured.** One run at 64 slots per worker (Neon, shop pool 60, workers split over two machines, 400 shoppers, 20,000 orders, warm-up discarded): 638.9 orders/s foreground, drain 1.6 s, **608 orders/s end to end**, p95 970 ms, 0 failed, 25 of 25 integrity checks, peak CPU 37% on the workers machine and 46% on the second machine. It agrees with the 581-592 of section 38 (one run, so it is not a second repetition of anything).

**What failed.** The next run (128 slots) did not start: the load tool refused to reset the ForgeCart tables because Forge still had queued or running jobs after 180 s, and the worker machines sat at 5% CPU. **The cause was found afterwards, while adding the worker pool setting: the SDK's limit on worker slots is 64, and ForgeCart's workers refuse to start with `FORGECART_WORKER_CONCURRENCY=128` ("must be between 1 and 64"), so the workers were not running and the queue could not drain; it had nothing to do with Neon.** Neon's console warned at about the same time that one project had used 99.4% of its monthly network-transfer allowance (the Free plan caps data out of each project at 5 GB per month); that warning was unrelated to the failure.

**Why the session was stopped.** With one project at 99.4% of its monthly transfer cap, further Neon runs risked making that project unreachable until the monthly reset, and the remaining runs would have been unreliable. The services, pool size, worker slots and databases were put back to the EC2 setup (pool 20, 32 slots, four workers on the workers machine), and the machines were stopped.

**What this means for the earlier Neon sections.** All the Neon measurements in sections 31-38 transferred data out of the Neon projects, and the Free allowance is 5 GB per project per month, so the allowance is a real limit of this approach for load testing: the same amount of load testing on EC2 databases has no such cap. I do not know which project reported the warning or how much each project had used.

**Still open:** more concurrency than 64 slots per process (it needs more worker processes or a higher SDK limit, not a bigger setting); the matched Neon against EC2 comparison with identical tuning on both.

## 40. How much data leaves the shop database per order (Neon Free's transfer cap)

The Neon project holding the shop database reported 99.4% of its monthly network-transfer allowance (Free plan: 5 GB of data out per project per month). To find where the transfer comes from, one small run (300 orders, 50 shoppers, no warm-up, services on the Neon shop project, pool 20, four workers on one machine) was measured with a count-only firewall rule on each machine that counted the bytes arriving from the shop project's three IPv4 addresses (the rules were removed afterwards, and no traffic was changed), plus `pg_stat_statements` on the shop database.

| Machine | Bytes from the shop project | Share |
|---|---|---|
| Workers machine | 3,816,597 | 62% |
| Shop machine (shop service and two dispatchers) | 2,011,322 | 33% |
| Load generator (setup and integrity checks) | 315,425 | 5% |
| **Total** | **6,143,344** | about 20.5 KB per order |

**What it says.** About **20 KB leave the shop database per order**, so 5 GB is about **250,000 orders**. That is a small number for these tests: 12 to 15 runs of 20,000 orders would use it, which matches what happened over these sessions (dozens of runs of 15,000 to 20,000 orders each). The 20 KB is an upper estimate for the marginal cost per order: the counted window also includes the load tool's setup (a 3,005-row catalogue reset and the checks) and connection set-up, and I measured only one small run, so I do not know how much of it is fixed overhead.

**Where it comes from.** For each paid order (274 of the 300 were paid) the four background jobs each read the shop database again: the order (1,096 reads), its outbox event (1,096 reads) and, for two of the jobs, the order lines (822 reads), about 3.5 KB per job on the workers machine, which is more than the rows themselves (small rows; protocol and encryption overhead per query account for the rest). So the workers' re-reading is the largest part (62%), and the shop service's own work (product lookups, order inserts, outbox claims) is the second.

**Correction to what I said earlier.** I told you that at a realistic shop rate of a few orders per second 5 GB per month "would last far longer than it did in these tests". That was wrong for ForgeCart as written: at 20 KB per order, 5 GB is about 250,000 orders per month, about 0.1 order per second on average, so a shop doing 3 orders per second would pass the Free cap in under a day. The transfer cap, not speed, is what limits ForgeCart on Neon Free.

**What would reduce it (not implemented).** The workers could receive the order data they need in the job input instead of reading it back from the shop database, at the cost of weakening the check that each worker re-verifies the order's state in the database (the current design does that on purpose); batching reads would cut the per-query overhead. Either needs a design decision and a new measurement. Not done.

**Limits.** One run of 300 orders; the fixed part of the transfer is not separated from the per-order part; only the shop project was measured (the results project, where the workers write, was not); the counts are bytes received at the machines' network interface (including the encryption overhead), which is close to but not exactly what Neon bills.

## 41. A new forge-shop project: worker database pool, a lower level than before, and Neon's changing latency (version 2, ninth result)

The old forge-shop project had used up its monthly transfer allowance (section 40), so a new project was created (same region, same plan). Its schema was built from the 18 migration files (13 tables; the only difference from the EC2 shop database was the two `pg_stat_statements` views) and the 3,005-product catalogue was copied from the EC2 database (same row count, same checksum over SKU, price and inventory). A new setting was added at the same time: `FORGECART_WORKER_DB_MAX_CONNS` (default 10, 1 to 200), the size of each of a worker's two database pools; before this the workers' pools were fixed at 10 connections each. The failed 128-slot run of section 39 was explained while doing this: the SDK allows at most 64 slots per process, and a ForgeCart worker refuses to start above that.

**A. Worker pool 10 against 30** (Neon, shop pool 60, 64 slots per worker, workers split over two machines, 400 shoppers, 12,000 orders after a 2,000-order warm-up; order 10, 30, 30, 10):

| Run | Worker pool | Foreground orders/s | Drain | End to end (orders/s) | p95 (ms) |
|---|---|---|---|---|---|
| 1 | 10 | 422.2 | 4.1 s | 368 | 1,587 |
| 2 | 30 | 428.6 | 1.1 s | 413 | 1,567 |
| 3 | 30 | 427.5 | 1.0 s | 413 | 1,557 |
| 4 | 10 | 438.2 | 2.9 s | 397 | 1,513 |

A pool of 30 shortened the drain (about 1 s against 3-4 s) and raised end to end by about 4-12%; foreground did not change. Two runs per setting, so a small effect of uncertain size. All checks passed, no failures. (Peak CPU stayed at 29-40% on both worker machines.)

**B. The whole level was lower than in section 38** (foreground 422-438 against 593-704 with the same shop pool, slots and worker split). Two repeats on the new project with section 38's run settings (20,000 orders, 4,000 warm-up, pool 30) gave 438.3 and 422.8 foreground and 430 and 416 end to end, so run length and warm-up were not the reason. Two things changed together, the project and the day, so the project alone cannot be blamed either.

**C. Round-trip time to each Neon project changes between wake-ups.** `SELECT 1` median (300 statements over one connection, from the EC2 machines, read-only):

| Time (UTC) | shop | results | payments | Forge's project |
|---|---|---|---|---|
| 17:14 | 0.40 ms | 1.56 ms | 0.99 ms | 1.62 ms |
| 17:20 (after 6 idle minutes) | 0.62 | 0.55 | 1.96 | 1.76 |
| 17:26 (after 6 idle minutes) | 1.65 | 0.53 | 0.94 | 0.63 |
| 17:31 (start of the tuning session) | 0.44 | 1.79 | 1.00 | not probed |

The same project ranged from 0.4 to 1.8 ms at different times; Forge's project had measured 0.45 ms earlier the same day. A Neon compute that has scaled to zero can restart on another machine, and these figures fit that, though I cannot see the placement. This is a large hidden variable in every Neon figure of sections 31-41. I did not record these latencies during the fast runs of section 38, so I cannot say they explain the difference between that session and this one.

**D. Tuning session** (one awake session, 500 shoppers, 12,000 orders after a 2,000 warm-up, 64 slots, worker pool 30, workers split; round trips at the start: shop 0.44, results 1.79, payments 1.00 ms):

| Run | Setting | Foreground orders/s | Drain | End to end (orders/s) | p95 (ms) | Peak CPU forge-db2 |
|---|---|---|---|---|---|---|
| 1 | C0: shop pool 60 | 429.7 | 1.7 s | 405 | 2,009 | 60% |
| 2 | C1: shop pool 100 | 488.2 | 6.2 s | 390 | 1,709 | 59% |
| 3 | C2: pool 100 + payment pool 30 | 478.1 | 7.1 s | 372 | 1,752 | 81% |
| 4 | C2 | 487.7 | 6.3 s | 388 | 1,718 | 60% |
| 5 | C1 | 470.3 | 4.3 s | 403 | 1,787 | 64% |
| 6 | C0 | 381.9 | 16.2 s | 252 | 2,673 | 64% |

A shop pool of 100 raised the foreground rate by about 14-25% (470-488 against 382-430), as with 60 against 20 earlier. The payment service pool of 30 changed nothing. The background part kept falling behind (drains of 4-7 s, 16 s in the last run) with the results project at 1.79 ms; run 6, the last, was the worst of the session and I do not know what changed during it. Forge's database machine peaked at 59-81% CPU, so it was not the limit in this session. **No run reached 700; the best was 488 orders/s foreground and 405 end to end.**

**Transfer used.** This session and sections A-B used roughly 4 GB of the new project's 5 GB monthly allowance by my estimate (about 20 KB per order, section 40); the console figure was not read.

**Limits.** Two runs per setting; the substrate changed under the tests (latency to the results project 1.79 ms against 0.55 ms in another round); run 6 is unexplained; the effect of latency on throughput was inferred, not isolated; only the shop project's transfer is estimated.

## 42. The attempt to reach 700 orders/s on Neon: more parallelism did not help, and the substrate keeps changing (version 2, tenth result)

Two more sessions on the new forge-shop project (Neon, workers split over two machines, 500 shoppers, 8,000 orders after a 1,500-order warm-up, round trips to each project read before and after). R1 = shop pool 100, worker pool 30, 64 slots per worker, one process per job type. R2 = shop pool 150, payment service pool 30, worker pool 60 (the same 64 slots per worker). **R2 was meant to double the worker processes as well, but the second processes never started**: the second copy of each worker unit had no metrics address of its own and the worker refuses to start when the metrics key is set without one (`FORGECART_METRICS_ADDR and FORGECART_METRICS_KEY must be set together`), so R2 tested only the larger pools.

| Session | Run | Setting | Foreground orders/s | Drain | End to end (orders/s) | p95 (ms) |
|---|---|---|---|---|---|---|
| 1 (start: shop 1.68, results 1.51, payments 1.00 ms; end: 0.45, 0.43, 1.06 ms) | 1 | R1 | 399.9 | 0.8 s | 384 | 2,095 |
| | 2 | R2 | 435.2 | 3.2 s | 371 | 1,963 |
| | 3 | R2 | 463.0 | 5.1 s | 358 | 1,825 |
| | 4 | R1 | 447.1 | 2.7 s | 388 | 1,876 |
| 2 (start: 1.55, 1.49, 1.11 ms; end: 1.60, 0.42, 1.95 ms) | 1 | R2 | 476.4 | 7.3 s | 332 | 1,742 |
| | 2 | R1 | 378.9 | 1.9 s | 348 | 3,452 |
| | 3 | R2 | 490.0 | 6.0 s | 359 | 1,683 |

All 25 integrity checks passed and nothing failed in any run. Peak CPU stayed at 24-45% on every ForgeCart machine and 57-76% on Forge's database machine.

**What it says.** Bigger pools (R2) raised the foreground rate again (435-490 against 379-447) but the background part fell further behind (drains of 3-7 s against 1-3 s), so **end to end did not improve (332-371 against 348-388)**: past a point more concurrent connections just make the database commits slower for each other. Nothing reached 700; the best end-to-end figure on the new project was 405 orders/s (section 41), against 608 on the old project in section 38.

**The latency is the main uncontrolled variable.** `SELECT 1` medians for the three projects fell into three groups, about 0.4-0.6 ms, about 1.0 ms and about 1.5-1.9 ms, and each project moved between groups from one wake-up to the next, sometimes within one session (section 41). Three groups match the three availability zones of the region, with only a compute in the same zone as the EC2 machines being fast, so each project would have about a one-in-three chance of a fast placement and all three together about one in 27. That reading fits the numbers but I cannot see the placement, and I cannot hold it fixed: restarting a compute from the console did not give a fast one. A comparison that spans a placement change cannot be read, which is why none of the R1 against R2 differences above is a finding.

**What follows.** Every added connection helps until the database's commits (each waiting on a round trip) become the limit, so on Neon Free the throughput of this architecture depends on the placement of three projects and on how many round trips each order needs. More parallelism does not fix the second part; fewer round trips per order would (for example the four jobs of a paid order each re-reading the order from the shop database, section 40, which would also cut data transferred and database work). That change is a design decision and was not made.

**Limits.** One to three runs per setting; the placement changed within sessions; the transfer allowance of the new forge-shop project is by my estimate nearly used (the console figure was not read); R2 did not include the second worker processes; Forge's database stayed on EC2.

## 43. Workers read the order in one query (EC2 databases, same-session A/B)

Each paid order starts four background jobs, and each used to read the shop database again: the event, the order and (for three of the four) its lines, 11 reads per paid order. `bizjob.ReadPaidOrder` and `VerifyPaidOrder` now run **one** statement that joins the event, the order and an aggregated list of its lines (`internal/bizjob/verify.go`); every check and every rejection is kept, in the same order, so nothing about the verification is weakened and the job input is still not trusted (this replaces the idea of carrying the order data in the job input, which would have weakened it).

**A/B** on the EC2 databases (default pools: shop 20, workers 10; four workers on one machine at 32 slots; 200 shoppers; 12,000 orders after a 3,000-order warm-up; order A, B, B, A; the binary of each run was identified by the hash of the running executable):

| Run | Workers | Foreground orders/s | End to end (orders/s) | p95 (ms) | Shop calls per order | Shop DB time per order | Integrity checks |
|---|---|---|---|---|---|---|---|
| 1 | A (three reads) | 524.4 | 517 | 568 | 24.8 | 3.89 ms | 25 of 25 |
| 2 | B (one read) | 516.8 | 505 | 581 | 18.5 | 3.95 ms | 25 of 25 |
| 3 | B | 514.8 | 502 | 585 | 18.5 | 4.01 ms | 25 of 25 |
| 4 | A | 501.6 | 493 | 590 | 24.7 | 3.98 ms | 25 of 25 |

**What it says.** Shop database calls per order fell by 25% (24.8 to 18.5), as designed, and all checks passed. **Throughput did not change** (502-505 against 493-517) because the EC2 shop database is CPU-bound (100% CPU in every run) and the one combined query costs about as much database time as the three it replaces: 3.95-4.01 ms per order against 3.89-3.98 ms. Fewer round trips only help where round trips are the limit (Neon's changing latency, section 42) or where transferred data matters (section 40); neither was measured for this change.

**Rejection paths.** Four jobs were submitted through Forge's API against each binary set: three with bad input (a made-up event; a real event with a made-up order; a real event with a wrong payment attempt) and one valid. The outcome was the same for both builds: the three bad jobs failed at once, the valid job succeeded. The API gives only "Job execution failed" for a failure, so the specific reason codes (`unknown_event`, `event_mismatch`, ...) were not told apart; `order_not_found` and `wrong_event_type` were not exercised.

**Where the database time goes (EC2 shop database, run 4, 12,000 orders, about 4.0 ms per order).** Seven statements of the order-creation path take about 70% of it: insert order items 15.0% (2 calls per order), insert outbox event 10.8%, update orders to set currency and totals 9.4% (1 call per order), insert order 9.0%, product lookup 8.9%, update orders to paid 8.7%, update product inventory 8.2% (2 calls per order); the dispatcher's claim and attempt statements take a further 7%. At 2 vCPU, 700 orders per second needs at most about 2.8 ms of database time per order, 28% less than now, so the next step is to cut these writes, not to add parallelism.

**Limits.** One session, two runs per build; EC2 databases only; the Neon effect and the data saved were not measured; the reason codes were not exercised.

## 44. Checkout in one statement instead of five (EC2 databases, same-session A/B)

Section 43 showed that seven statements of the order-creation path take about 70% of the shop database's time. `orders.Store.Checkout` (`forgecart/internal/orders/checkout.go`) used to run, per order: insert the order, lock and read the products, update the order's totals, then for each item an insert and a stock decrement (seven statements for a two-item order). It now runs three: the order insert (kept first, because the idempotency key must block a concurrent duplicate before any product row is touched), the product lock and read (unchanged, with all the validation), and **one statement** with three data-modifying CTEs (decrement stock with the same `inventory_quantity >= quantity` guard, insert the item snapshots, fill in the order's totals). Duplicate SKUs are rejected earlier by `NewRequest`, so each product row is matched by one item. If the number of decremented rows differs from the number of items, which the lock and the earlier checks should make impossible, the transaction rolls back with `insufficient_inventory` (without the SKU in the message, which the old code added in that unreachable case).

**Checks by hand against both builds** (EC2 shop database, `POST /v1/orders`, seven cases): a new two-item order (201), the same key and body (200, same order), the same key with another body (409 `idempotency_key_reused`), quantity 2 with stock 1 (409 `insufficient_inventory`, nothing stored), quantity 1 with stock 1 (201), quantity 1 with stock 0 (409), an unknown SKU (404 `product_not_found`). Status codes, fields, stock changes and order totals against item totals were identical for both builds, and no rolled-back order was left behind. (Some of the check orders were not deleted afterwards; the next load run empties those tables.)

**A/B** (EC2 databases, default pools, one-query workers on one machine at 32 slots, 200 shoppers, 12,000 orders after a 3,000-order warm-up, order A, B, B, A; the running binary of each run was identified by hash):

| Run | Checkout | Foreground orders/s | End to end (orders/s) | p95 (ms) | Shop calls per order | Shop DB time per order | Integrity checks |
|---|---|---|---|---|---|---|---|
| 1 | A (seven statements) | 537.6 | 523 | 548 | 18.5 | 3.94 ms | 25 of 25 |
| 2 | B (three statements) | 568.6 | 561 | 515 | 14.5 | 3.57 ms | 25 of 25 |
| 3 | B | 564.5 | 554 | 516 | 14.5 | 3.77 ms | 25 of 25 |
| 4 | A | 515.6 | 507 | 582 | 18.5 | 4.06 ms | 25 of 25 |

**What it says.** Calls per order fell by 22% (18.5 to 14.5), database time per order by about 8% (3.67 ms against 4.00 ms on average), and end to end rose by about 8% (554-561 against 507-523), with every check passing and no failure. The gain is real but far smaller than the 30% of statement time the three replaced statements represented: the work inside the one statement (writing the item rows and their indexes, updating the product and order rows, and writing the log for them) is mostly unchanged; what is saved is the per-statement overhead. The shop database was at 100% CPU in all four runs.

**What it means for 700.** At 2 vCPU, 700 orders per second needs about 2.8 ms of database time per order; it is 3.67 ms now, so about 24% more would have to go, and Forge's own database (about 2,350 jobs/s measured, 2,500 needed at 700) is also at its limit. The remaining statements in the path are the outbox insert, the order update to paid (two statements in the payment step that could become one), the dispatcher's claim and attempt updates (about 7%), and the order insert; together they would give another 5-10% at best by my estimate, which has not been tested.

**Limits.** One session, two runs per build, EC2 databases only; the effect on Neon (where round trips are the limit) was not measured; the per-statement profile of build B was not taken.

## 45. Batched worker reads, and the payments database on its own machine (EC2 databases)

Two changes, each measured against the previous build in the same session (EC2 databases, shop pool 20, four workers at 32 slots with pool 10 on one machine, 500 shoppers, 15,000 orders after a 3,000-order warm-up, all 25 integrity checks passing and no failure in every run unless noted).

**A. The payments database on a separate machine.** The payments database shared the results database's machine (`results-db`), where it took about 0.6 ms of statement time per order (one insert, `payment_attempts`, was 87% of it) against about 1.6 ms for the results database itself, and that machine peaked at 99% CPU. A PostgreSQL 16 on the spare machine (`shop2`, 2 vCPU; same role, database name, migrations and non-default settings, scaled to its memory) now holds it; the payment service was pointed there.

| Run | Payments database | End to end (orders/s) | Results machine peak CPU | `shop2` peak CPU |
|---|---|---|---|---|
| 1 | results machine | 571 | 99% | 7% |
| 2 | `shop2` | 624 | 93% | 61% |
| 3 | `shop2` | 606 | 92% | 54% |
| 4 | results machine | 597 | 99% | 11% |

A gain of about 3-7%. The shop database machine stayed at 100% CPU in all runs.

**B. Batched worker reads** (`bizjob.Reader`, `forgecart/internal/bizjob/reader.go`). The jobs of one worker process that need an order at the same moment have their shop reads gathered, for up to about a millisecond or 64 reads, into one statement that takes arrays of event and order IDs; every job still receives its own row and goes through the same checks (`paidRow.verified`, shared with the single read), and the job input is unchanged (three IDs, no customer data). This replaces the idea of carrying the order in the job input, which would have put customer emails into Forge, something the code deliberately avoids. Workers that do not use the reader behave as before (`Databases.ReadPaidOrder` falls back to the single query).

| Run | Workers | Foreground orders/s | Drain | End to end (orders/s) | p95 (ms) | Shop calls per order | Shop DB time per order | Peak CPU: shop DB / results / Forge DB |
|---|---|---|---|---|---|---|---|---|
| 1 | one query per job | 614.7 | 0.4 s | 604 | 1,438 | 14.5 | 3.54 ms | 100% / 91% / 67% |
| 2 | batched | 745.8 | 2.8 s | 655 | 1,155 | 11.6 | 3.20 ms | 100% / 100% / 90% |
| 3 | batched | 746.4 | 2.9 s | 651 | 1,144 | 11.6 | 3.34 ms | 100% / 100% / 90% |
| 4 | one query per job | 614.1 | 0.6 s | 599 | 1,447 | 14.5 | 3.66 ms | 100% / 91% / 73% |

**What it says.** Batching cut shop database calls by 20% and its time per order by about 9%; the part of the workflow customers see ran at **746 orders per second, 21% faster**, with a lower p95. The background work did not keep up with that rate: it finished about 3 s after the load stopped, so the sustained end-to-end rate is **651-655 orders/s** (8-9% above the 599-604 without batching), the best measured on EC2 so far. The limit is now the background part: the results machine at 100% CPU and Forge's database machine at 90%.

**Checks by hand against both builds.** 37 valid and 3 bad jobs (made-up event, wrong order, wrong payment attempt) submitted in quick succession through Forge's API: all 37 valid jobs succeeded and the 3 bad ones failed, for both builds. The jobs went in one after another through `curl`, which only partly exercises batching, so the load runs (25 of 25 checks under real concurrency) are the stronger evidence.

**A failed attempt in the same session, and its cause.** A run with the shop pool at 60, 64 slots and a worker pool of 30 gave 171 and 186 failed orders and one failed integrity check, and is not counted: those settings open about 180 connections to a database whose `max_connections` is 100, and the log shows 438 "remaining connection slots are reserved" errors. It was a configuration mistake of mine, not a defect of the code; larger pools need a higher `max_connections` on the database machines.

**Limits.** Two runs per build; EC2 databases only (the effect on Neon, where round trips were the limit, was not measured); "end to end" includes the time to drain, so it measures the sustained rate while the foreground figure is a burst the background cannot yet absorb.

## 46. Single-statement result writes, group commit, the payment apply, and the current code on the Neon setup

**A. Result writes in one statement (EC2 databases).** Each worker used to write its result with five round trips to the results database (look-up, BEGIN, header insert, items insert, COMMIT). `Effect.Write` (`forgecart/internal/bizjob/effect.go`) now runs one autocommit statement, and invoice and fulfillment write the header and the line snapshot in one statement with a data-modifying CTE (a statement is atomic by itself; its affected-row count is the number of lines when the header was written and 0 on a duplicate); the outcome handling (duplicate, another event owning the order, real database error) is the same as before. A same-session A/B (C = before, D = after; payments database on `shop2`, 500 shoppers, 15,000 orders, all 25 checks passing, no failures):

| Run | Workers | Foreground orders/s | End to end (orders/s) | Results DB calls per order | Results machine CPU |
|---|---|---|---|---|---|
| 1 | C | 764.5 | 661 | 16.2 | 100% |
| 2 | D | 766.6 | 673 | 7.2 | 100% |
| 3 | D | 749.6 | 665 | 7.2 | 100% |
| 4 | C | 733.3 | 649 | 16.1 | 100% |

Round trips to the results database fell by 55% but end to end rose by only about 2-3% (within the noise). **Duplicate delivery test:** the invoice and fulfillment rows of one event were removed from the results database (the tables refuse deletes through immutability triggers, so the triggers were bypassed for that session as the database superuser, on benchmark data), then 10 invoice jobs and 10 fulfillment jobs for that event were submitted at once: all 20 succeeded and exactly one invoice (3 items) and one fulfillment (3 items) existed, matching the order's 3 lines. (A first attempt of this test did not remove the rows and exercised only the "already exists" path; it is not counted.)

**B. Where the busy time goes on the database machines** (`vmstat`, busiest 5 samples of the load, 500 shoppers, workers D): the shop database machine 100% busy = 72% user + 28% system, the results machine 100% = 53% user + 47% system, Forge's machine 87% = 68% user + 5% system + 14% I/O wait, `shop2` (payments database) 74%. I/O wait was 0 on the shop and results machines, so the "100% CPU" is real computing and not waiting for the disk. (Earlier sections computed "busy" as 100 minus idle, which includes I/O wait; for those two machines it was zero.)

**C. Group commit** (`commit_delay` 100 microseconds, `commit_siblings` 3, on the three database machines; durability unchanged because every commit still waits for its flush): end to end 664 with it off, 659 and 657 on, 664 off; the CPU split of the results machine did not change (47-48% system). No effect; reset to the defaults.

**D. The payment step in one statement.** `applyPaid` (`forgecart/internal/payments/payments.go`) marks the order paid and writes its one outbox event in one statement (`outbox.NewPaidOrderEvent` builds the row; the event is inserted only for the row the update matched), instead of BEGIN, update, insert, COMMIT. Checked by hand against both builds with 20 orders each (paid, declined and temporarily failed outcomes from the simulator): every call returned 200, replays with the same key gave the same answer, every paid order has exactly one outbox event naming its paying attempt, no unpaid order has an event. **Not yet measured under load.**

**E. The current code on the Neon setup** (shop, results and payments on Neon; Forge's database on EC2; shop pool 100, payment pool 30, 64 slots per worker with pool 30, 500 shoppers, 12,000 orders after a 2,000-order warm-up):

| Run | Foreground orders/s | Drain | End to end (orders/s) | p95 (ms) | Failed | Checks |
|---|---|---|---|---|---|---|
| 1 | 598.6 | 6.5 s | 452 | 1,363 | 0 | 25 of 25 |
| 2 | 642.6 | 8.5 s | 442 | 1,242 | 0 | 25 of 25 |

Round trip (`SELECT 1` median) to shop / results / payments: 1.64 / 1.32 / 0.91 ms at the start (0.57 / 0.53 / 1.00 ms about two minutes earlier, before the computes were reassigned), 1.61 / 1.70 / 0.98 ms after run 1 and 1.80 / 1.32 / 0.89 ms after run 2. Data received from the three Neon projects together: 119 MB and 122 MB per 12,000 orders, about 10 KB per order (the three projects resolve to the same front-door addresses, so the bytes cannot be split per project; the earlier figure of 20 KB per order, section 40, was for the shop project alone and the old code).

**What it says.** On Neon, with the current code and a placement with 1.3-1.8 ms round trips to the shop and results projects, the shopper-facing rate was 599-643 orders/s but the background fell 6-9 s behind, so the sustained rate was 442-452 orders/s, against 649-673 on the EC2 databases with the same code. I do not know whether the drain comes from the round-trip latency or from the Neon compute of the results (or shop) project reaching its limit; the console graphs for that window were not read. The EC2 figures of sections 43-46 are not Neon figures.

**Limits.** Two runs per Neon configuration; the placement changed between my probe and the run; no Neon console metrics; Forge's database on EC2.

## 47. Neon with the current code: the front-door addresses, slot-bound workers, and a front part that saturates near 620 orders/s

Setup of every run in this section: shop, results and payments databases on Neon Free (current projects), Forge's database on EC2, the current code (shop binary with the one-statement checkout and payment apply, workers with batched reads and single-statement writes), the services on EC2 machines in the same region.

**A. Each Neon hostname resolves to three addresses with different latencies, and each connection lands on a random one.** Measuring the round trip to each address separately (`PGHOSTADDR`, 300 `SELECT 1` over one connection): shop 0.58 / 1.44 / 1.85 ms, results 0.52 / 1.47 / 1.71 ms, payments 0.91 / 0.95 / 1.81 ms for the addresses 3.106.69.51 / 54.153.234.207 / 54.206.85.193. The three groups match the "placements" seen in sections 31-42, so most of that variation was the front door a connection happened to get (a probe is one connection; an application pool is a mix), not the compute moving. Pointing the three hostnames at 3.106.69.51 in `/etc/hosts` on the shop, workers, load generator and probe machines (`pin_neon.sh`; reversible) made every probe 0.47-0.60 ms for shop and results and about 0.9 ms for payments, and made repeated runs agree. The pin is a test device: Neon does not promise these addresses, and a production setup should choose the nearest address at connect time instead. The pin is still on the machines.

**B. The workers were slot-bound.** With 64 slots per process on Neon (round trip about 1.3-1.8 ms, mixed doors) every worker had all its slots busy at peak and a mean handler time of 81-138 ms (reconcile 138, invoice 106, notification 99, fulfillment 81), which caps the slowest job type near 460 jobs/s; Forge's mean queue wait was 0.2-1.2 s. The handlers contain only a few database calls of 0.5-20 ms each, so the time is mostly the shop project answering slowly under load (statements averaging 3-21 ms: the checkout 18-21, the batched worker read 17, the product lookup 8.9, the order insert 3.0, the payment apply 3.0-3.2; the results project's statements were 0.2-1.3 ms and it had only about 2 active backends at a time). Wait events on the shop project: 38% commits waiting for replication, 16% page locks, 11% running, 9% row-lock waits, 9% log writes; a second sample of ungranted locks showed about 1-2 backends waiting at a time (page locks in the checkout and payment statements, row locks on the product lookup), so contention exists but is small.

**C. A second worker process per job type (128 slots per type), address pinned**, 500 shoppers, 8,000 orders, all checks passing and no failures:

| Run | Processes | Foreground | Drain | End to end (orders/s) | Handler mean | Forge queue wait |
|---|---|---|---|---|---|---|
| 1 | 1 | 611.9 | 4.9 s | 444 | 126 ms | 769 ms |
| 2 | 2 | 645.6 | 4.4 s | 475 | 136 ms | 213 ms |
| 3 | 2 | 645.9 | 5.3 s | 451 | 127 ms | 476 ms |
| 4 | 1 | 617.0 | 4.2 s | 466 | 94 ms | 475 ms |

The second process cut Forge's queue wait but not the end-to-end rate: with 128 slots the average number of jobs in flight is well below the slots, so the slots were no longer the limit.

**D. More shoppers** (pinned, two processes per type, shop pool 150, payment pool 60, 12,000 orders after a 2,000-order warm-up; the load tool's limit was raised from 500 to 1,000 for this, `HardMaxConcurrency` in `forgecart/test_files/engine.go`):

| Run | Shoppers | Foreground orders/s | Drain | End to end (orders/s) | p50 / p95 (ms) | Failed | Checks | Peak CPU: workers / shop machine / Forge DB |
|---|---|---|---|---|---|---|---|---|
| 1 | 500 | 610.7 | 4.5 s | 496 | 743 / 1,305 | 0 | 25 of 25 | 53% / 46% / 79% |
| 2 | 700 | 621.1 | 5.7 s | 479 | 1,019 / 1,912 | 0 | 25 of 25 | 82% / 47% / 78% |
| 3 | 900 | 576.6 | 6.8 s | 434 | 1,398 / 2,738 | 0 | 25 of 25 | 65% / 43% / 96% |
| 4 | 500 | 619.0 | 6.8 s | 458 | 739 / 1,326 | 0 | 25 of 25 | 79% / 45% / 73% |

The foreground rate stays at about 610-620 while the median latency grows roughly in proportion to the shoppers: a saturated resource. The load generator used 0.17 cores and was not a limit. At 900 shoppers Forge's database machine reached 96% CPU.

**E. A correction to section 36 and to what I said during this work.** Sections 36 and 41-42 and a note in this session read the Neon console CPU graphs (about 1.1 vCPU for the shop project, about 0.9 for results, of 2 allocated) as showing the computes were not CPU-bound. Those graphs average over about a minute, while each run is 20-35 seconds of load, so a burst near 2 vCPU would show as about 1 vCPU. They do not rule out CPU saturation of the shop compute. The flat throughput in D (about 3.2 ms of CPU per order at about 620 orders/s, close to the 3.2-3.5 ms measured per order on EC2) is consistent with the shop compute being the limit at its 2 CU maximum, but I did not measure the CPU directly.

**F. Where it stands.** Sustained end to end on this Neon setup is about 435-500 orders/s and the shopper-facing rate about 580-650, against 649-673 sustained and 733-767 foreground on the EC2 databases with the same code. Reaching 700 here would need both a lower database cost per order on the shop side (about 12-15% for the foreground, by the 3.2 ms arithmetic) and a background that keeps up, and Forge's database machine already runs at 73-96% in these runs.

**Limits.** One to two runs per configuration; Neon CPU not measured directly; transfer used was about 120 MB per 12,000 orders in the first Neon run of the session and not counted again; the address pin is a test device.

## 48. One background job for the three result-writing effects (EC2 databases)

A paid order used to start four Forge jobs: `inventory.reconcile`, `invoice.generate`, `fulfillment.prepare` and `notification.send`. Each job costs a claim and a completion in Forge, a read of the order in the shop database, and a look-up, a write and a commit in the results database. The three effects that only write result rows are now one job, `order.process`, written by the new `forgecart/internal/postpay` package and run by `cmd/forgecart-order-worker`; the dispatcher (`outbox.JobTypes`) publishes two jobs per paid order, and `forge.json` declares the new type (5 attempts, 20 s). The separate workers and packages still exist and still work.

**How it records.** One read of the results database checks that none of the three rows exists for the event; the order is read once (the shared batched read with all its checks); then **one statement** writes the invoice with its items, the fulfillment plan with its items and the notification (data-modifying CTEs, atomic by itself, one commit). Each row keeps its own table and its own `UNIQUE(event_id)`. Anything unusual (a duplicate delivery, a row left by one of the old jobs, a conflicting payment attempt, an order another event already owns, an order without a recipient, a total below the subtotal) is handed to the three separate steps, which are each idempotent and apply their own rules; if one fails with an infrastructure error the job is retried, otherwise the first rejection is the job's rejection.

**Trade-off.** The three effects now succeed, are retried and are rejected together, and run in one job: this is right for cheap effects that only write rows (as they are here, since delivery, shipping and the invoice are simulated), and wrong for effects that call outside services or take long (a real email provider, a courier, a PDF renderer), which should stay separate jobs with their own workers, timeouts and scaling.

**Correctness checks by hand** (EC2 databases, jobs submitted through Forge's API against the new workers): ten identical jobs at once for an event with no result rows: all 10 succeeded and exactly one invoice (3 items), one fulfillment (3 items) and one notification existed; only the notification missing: restored, the invoice row unchanged; only the invoice (with its items) missing: recreated, the fulfillment row unchanged; a duplicate delivery of a complete event: nothing changed; an unknown event, a wrong order and a wrong payment attempt: all failed. (The result rows were removed for these tests by bypassing the immutability triggers as the database superuser, on benchmark data.) A load smoke test (200 shoppers, 5,000 orders): 25 of 25 checks, 17,920 of 17,920 expected result rows, Forge received only `inventory.reconcile` and `order.process` jobs.

**A/B** (same session; ec2p setup, payments database on `shop2`; shop pool 20, 32 slots per worker, worker pool 10; 500 shoppers, 15,000 orders after a 3,000-order warm-up; order old, new, new, old; the dispatcher and workers of each run identified by hash):

| Run | Pipeline | Foreground orders/s | Drain | End to end (orders/s) | p95 (ms) | Failed | Checks | Calls per order: shop / results / Forge DB | Peak CPU: Forge DB / workers |
|---|---|---|---|---|---|---|---|---|---|
| 1 | old (4 jobs) | 787.3 | 3.5 s | 666 | 1,079 | 0 | 25 of 25 | 8.9 / 7.2 / 2.6 | 93% / 58% |
| 2 | new (2 jobs) | 820.3 | 1.5 s | 759 | 1,026 | 0 | 25 of 25 | 8.4 / 3.6 / 1.7 | 65% / 40% |
| 3 | new (2 jobs) | 821.1 | 1.8 s | 747 | 1,018 | 0 | 25 of 25 | 8.4 / 3.6 / 1.7 | 67% / 39% |
| 4 | old (4 jobs) | 780.5 | 3.0 s | 674 | 1,088 | 0 | 25 of 25 | 8.9 / 7.2 / 2.6 | 92% / 59% |

**What it says.** Sustained end to end rose from 666-674 to 747-759 orders/s (about 12%) with the results database's calls per order halved (7.2 to 3.6), Forge's database calls cut by a third (2.6 to 1.7) and its machine from 92-93% to 65-67% CPU, and the workers machine from about 59% to about 40%. The shop database machine stayed at 100% CPU in all four runs, so the foreground rate rose only about 4% (787 and 781 to 820 and 821). All 25 integrity checks passed in every run and no order failed.

**Limits.** Two runs per pipeline; EC2 databases only (Neon not yet measured for this change); the effects are simulated and cheap, which is why merging them pays here.

## 49. The two-job pipeline on Neon: the backlog is in the outbox, and the Neon figures vary by about 25% between identical runs

Setup of every run in this section: shop, results and payments databases on Neon Free (the three hostnames pinned to the nearest front-door address, section 47), Forge's database on EC2, the two-job pipeline (section 48: new dispatcher, inventory and order workers, two processes each = 128 slots per job type), shop pool 150, payment pool 60, worker pool 30, 12,000 orders after a 2,000-order warm-up. All 25 integrity checks passed and no order failed in every run.

**A. Shopper sweep** (round trip to shop / results / payments 0.44-0.63 ms throughout):

| Run | Shoppers | Foreground orders/s | Drain | End to end (orders/s) | p50 / p95 (ms) |
|---|---|---|---|---|---|
| 1 | 500 | 582.5 | 4.6 s | 476 | 764 / 1,466 |
| 2 | 700 | 606.6 | 4.7 s | 490 | 1,027 / 2,007 |
| 3 | 500 | 621.8 | 5.4 s | 486 | 732 / 1,271 |
| 4 | 700 | 610.9 | 5.5 s | 477 | 1,032 / 1,977 |

Compared with the four-job pipeline on the same setup (section 47, end to end 434-496) the merge changed nothing measurable on Neon, while on EC2 it raised the sustained rate by about 12% (section 48).

**B. Where the background backlog sits** (one run at 700 shoppers, a sample every second): the **shop's outbox** grew from 0 to about 5,000 unpublished events during the 19-second load and emptied about 8 seconds after it; Forge's queue stayed near zero during the load (peak 2,112 in the catch-up) and the worker slots were not all busy (17-240 jobs running of 256). So on Neon the sustained rate is limited by how fast the dispatchers publish events (about 380 per second against about 600 arriving), not by Forge or the workers. Forge's own counters (since its restart at 19:55, covering all runs of this section) show **0 `admission_busy` and 0 `queue_full` rejections** for every job type, so Forge's bounded admission did not throttle the dispatchers; my hypothesis that it did was wrong.

**C. What the dispatcher costs on the Neon shop project** (`pg_stat_statements`, accumulated over several runs): the attempt-count update averaged 71.6 ms per call (89.5 rows per call), the mark-published update 40.7 ms (47.8 rows), the claim 15.6 ms (22.2 rows). Per event that is about 2.3 ms of shop-database time, similar to the whole checkout, and each outbox row is rewritten three times after its insert (claim, attempt, published). The payment apply averaged 7.5 ms per call. A parallel set of numbers on EC2 (section 29) had the dispatcher at about 12% of the shop database's time.

**D. Four dispatchers instead of two** (700 shoppers, order 2, 4, 4, 2):

| Run | Dispatchers | Foreground | Drain | End to end (orders/s) | Peak unpublished events |
|---|---|---|---|---|---|
| 1 | 2 | 588.1 | 4.6 s | 481 | 5,064 |
| 2 | 4 | 562.3 | 0.3 s | 553 | 479 |
| 3 | 4 | 585.8 | 2.9 s | 513 | 4,348 |
| 4 | 2 | 616.8 | 5.1 s | 488 | 5,247 |

Four dispatchers gave 513-553 against 481-488, but the two runs with four differ a lot (backlog 479 and 4,348), so the effect is real in direction and not yet reliable in size.

**E. Table fill factor 50 against 100** on the orders, outbox and products tables (700 shoppers, order 100, 50, 50, 100): end to end 477, 511, 424, 566; mean statement times (attempts 54 / 40 / 45 / 41 ms, published 43 / 35 / 41 / 36 ms, payment 7.8 / 6.8 / 5.0 / 6.3 ms). No consistent effect; the setting was put back to 100 and no migration was made.

**F. The Neon results vary by about 25% between identical runs.** Run 4 of E, with the default fill factor like run 1, gave 779.4 orders/s at the shoppers (p95 1,450 ms; checkout statement 11.7 ms on average) against 614.4 (p95 1,935 ms; 35.1 ms) in run 1, and 566 against 477 end to end. So the "ceiling of about 610 orders/s" in sections 47 and 49A is not a hard limit of the shop compute. I did not find the cause. Two untested candidates: Neon autoscaling needs time to scale a compute up, and each measured run is only about 20 seconds of load; and the compute sharing a host with other tenants.

**G. Where it stands on Neon** (two-job pipeline): foreground 560-780 orders/s (typically 600-620), sustained end to end 424-566 (typically about 480). On the EC2 databases with the same code: 820 foreground, 747-759 sustained.

**Limits.** One to two runs per configuration; Neon CPU and compute size not measured; transfer not recounted; the address pin is still active on the test machines.

## 50. Front part only on Neon, and a lock change that made it slower

**A. What the front part does on Neon with no background work** (dispatchers and workers stopped; events inserted into the outbox but never published; shop pool 200, payment pool 100; the three hostnames pinned; 20,000 orders after a 4,000-order warm-up):

| Run | Shoppers | Orders/s | p50 / p95 (ms) | Failed |
|---|---|---|---|---|
| 1 | 500 | 753.0 | 592 / 1,158 | 0 |
| 2 | 700 | 761.2 | 835 / 1,539 | 0 |
| 3 | 900 | 728.6 | 1,102 / 2,097 | 0 |
| 4 | 1,000 | 709.3 | 1,264 / 2,473 | 0 |
| 5 | 700 | 721.0 | 868 / 1,702 | 0 |

Removing the background work lifts the front part on Neon from about 610 (with the pipeline running) to about 710-760, not to 1,000, and more shoppers do not raise it (latency doubles). The load generator used 0.2 cores.

**B. A 150-second steady run** (700 shoppers, 109,049 orders, 721.8 orders/s, p50 894 ms, p95 1,652 ms, 0 failed) with the wait events of the shop project sampled every 2 seconds: commits waiting for replication 31% + 10%, row-lock waits (`transactionid`, `tuple`) 20% + 3%, page locks 17%, log writes 5%, waiting for the client 5%, **running on CPU 4%**. So the shop compute is not CPU-bound; its sessions wait on commits and locks. The statements with the most total time: the product `SELECT ... FOR UPDATE` (41.3 ms on average), the checkout statement (34.6 ms), the order insert (6.2 ms), the payment apply (4.3 ms) and the stock release after a declined payment (18.9 ms).

**C. The change that did not work.** Idea: hold the product-row locks for less time by reading the products without locks and taking the locks inside the one statement that decrements stock (a `locked` CTE with `FOR UPDATE`, then the guarded decrement, with a retry from a fresh read if a guard fails).
1. **First version: deadlocks.** In a stampede test on the EC2 databases (300 shoppers all ordering the first 3 products, 2,000 units of stock each, 6,000 orders) the old checkout gave 308 and 305 orders/s with 0 failed, 25 of 25 checks and 0 deadlock log lines; the new one gave 21 and 19 orders/s, 866 and 814 failed orders, 24 of 25 checks ("no payment is stranded": 352 and 361 violations) and 83 and 95 deadlock lines. Cause: it locked in product-id order while `releaseStock` (the declined-payment path) updates in SKU order, so the two could wait for each other.
2. **Second version: lock in SKU order.** Stampede test again: new 329 and 340 orders/s against old 317 and 319, 0 failed, 25 of 25 checks, 0 deadlock lines, the same number of stock rejections (about 4,370-4,400). Correct, and 4-7% faster on EC2.
3. **On Neon it was slower** (front part only, same session, old new new old, 15,000 orders after a 3,000-order warm-up, all with 0 failed orders):

| Shoppers | Old checkout (orders/s) | New checkout (orders/s) | Mean checkout + product read, old | Mean checkout statement, new |
|---|---|---|---|---|
| 700 | 690.2, 687.2 | 504.9, 484.2 | 94-116 ms (41-50 + 53-66) | 197-213 ms |
| 1,000 | 647.1, 666.6 | 447.7, 494.7 | 112 ms (51 + 61) | 203-222 ms |

The new code was 27-30% slower. **A likely reason (not verified):** in READ COMMITTED a statement that waits for a row lock and then updates that row has to re-check the row against its newer version, with a snapshot taken before the wait, so putting the lock wait and the update in one statement costs more than a lock statement followed by a fresh update statement. **The change was reverted** (the revert is in the history); the old checkout stays.

**D. Setup mistakes during this work, recorded so they are not read as results.** Three test attempts produced no numbers: the load tool needs a drain wait of at least 10 seconds (I gave 5); it refuses to reset the tables while Forge holds queued jobs, and stale unpublished events left in the Neon shop outbox by the no-background runs (56,274 of them) were published into Forge whenever the dispatchers restarted, which filled its queue (about 45,000 jobs) while the workers were stopped; the scripts now mark such events published and check that Forge's queue is empty first.

**Limits.** Two runs per configuration; the explanation of the slowdown is a hypothesis; the Neon computes' CPU was not measured; the address pin is still active.

## 51. The inventory database and the checkout saga: verification on the EC2 databases

Why: on Neon the shop project's sessions waited on commits (41%) and row locks (23%) and ran on CPU 4% of the time, and the front part alone was flat at about 710-760 orders/s (sections 49-50); narrowing the lock window inside the one database was 27-30% slower on Neon and was reverted. The stock moved to its own database with a saga (docs/decisions/0005-inventory-service-and-checkout-saga.md). This section is the correctness record on the EC2 databases (the orders and inventory databases are two databases on one server). **Nothing here is a measurement on Neon.**

**By hand** (seven checkout cases against the saga shop): the status codes and fields equal those of the old checkout in all seven (201 new, 200 replay, 409 key reused, 409 out of stock, 201 for the exact stock, 409 at zero stock, 404 unknown product); the stock after is exactly as expected; one reservation per accepted order and none for rejected requests; a replay and a key reuse each reserved and then released, leaving the stock unchanged. **Payments:** 80 orders paid with the simulator: 72 paid, 5 declined, 3 pending; all 5 declined orders have their reservation released and the stock equals 100,000 minus the 75 orders that hold it. **Reaper:** 72 reservations of paid orders became committed; a planted orphan (a reservation and a stock decrement with no order) was released and the stock returned; a planted failed release (a declined order whose reservation was reserved again) was released.

**Stampede** (300 shoppers on the first 3 products, 2,000 units each, 6,000 orders; three runs): 341-378 orders/s, 1,607-1,626 completed, 4,374-4,393 rejected for stock, 0 failed, 26 of 26 integrity checks, 0 deadlock lines in the database log. The old checkout gave 305-340 orders/s, about 1,600 completed and about 4,400 rejected: the same orders win and lose, and the saga was about 10% faster on this (fast) database.

**Crash test** (a 60-second load at 300 shoppers while the shop is killed with SIGKILL 12 times, every 3 seconds; reaper after 5 s every 2 s; a 25-second settling pause; the books checked across both databases by an independent script):

| Check | First run (old reaper) | After the fix |
|---|---|---|
| Ledger violations (stock + units held by reserved or committed orders differs from the starting stock) | 445 products | **0** |
| Orders that hold stock without a reservation | 0 | 0 |
| Reservations held for a missing, declined or cancelled order | 255 | **0** |
| Orphans released by the reaper | 121 | 430 |

After the fix: 27,555 orders, 27,985 reservations (430 more, the orphans); reservations committed 22,280 (the paid orders), reserved 4,041 (the pending orders), released 1,664 (1,234 declined plus the 430 orphans). 88.6% of the load's requests failed while the shop was down.

**Defects the tests found, all fixed:** (1) the first version of the lock-narrowing checkout deadlocked with the stock release (section 50); (2) the load tool's effect checks looped over the workers, so after the three jobs became one worker three checks failed with SQL errors (17 of 20 checks), fixed to loop over the four effects; (3) the reaper read the oldest 2,000 `reserved` reservations on each pass, so more than 2,000 orders awaiting payment hid the orphans behind them (255 stayed held, 445 products off), fixed with a cursor over the whole list; (4) two earlier attempts of the crash test produced no result (a duration of 45 s where the tool needs at least 60, and a previous result read in its place) and are not counted.

**Limits.** EC2 databases only; a single process kill per 3 seconds is one fault type (no database outage, no network fault); the reaper settled in the 25-second pause, so the books are exact after settling, not at every instant; the effect on Neon throughput is not measured.

## 52. The checkout saga on Neon: the front part alone, monolith against saga

Same session, same Neon compute for the orders: the **monolith** variant ran the old checkout (shop binary c) against a `monolith` database in the shop project that holds the old schema (orders, items and the products table with its foreign key) and the catalogue; the **saga** variant ran the saga checkout (shop binary s) against the shop project's `neondb` database (orders only) and the new `forge-inventory` project (catalogue, stock, reservations). The dispatchers and all workers were stopped, so this is the front part alone: create an order and pay it, with no background work. The three hostnames of the shop, results and payments projects were pinned to one front door and the inventory project to its own (0.84-0.92 ms against about 0.5 for the others, a different availability zone). Shop pool 200, payment pool 100, inventory pool 200 (saga). 15,000 orders after a 3,000-order warm-up. Order c, s, s, c at 700 shoppers and then at 1,000. Stale unpublished events were marked published before every switch and Forge's queue was empty.

| Run | Shoppers | Variant | Orders/s | p50 / p95 (ms) | Failed | Stock checks | Mean statement times |
|---|---|---|---|---|---|---|---|
| 1 | 700 | monolith | 671.7 | 914 / 1,854 | 0 | ok | checkout 58.1, product lock 45.4, payment 8.9 |
| 2 | 700 | saga | 1,145.3 | 494 / 1,149 | 0 | ok, ok | order insert 12.3, payment 4.1; inventory: lock 1.8, update 1.0, insert 0.4 |
| 3 | 700 | saga | 1,154.4 | 495 / 1,071 | 0 | ok, ok | order insert 9.4, payment 3.8; inventory: lock 6.8, update 3.9, insert 0.9 |
| 4 | 700 | monolith | 627.2 | 982 / 1,912 | 0 | ok | checkout 52.6, product lock 35.8, payment 12.6 |
| 5 | 1,000 | monolith | 657.3 | 1,359 / 2,714 | 0 | ok | checkout 66.7, product lock 50.2, payment 5.2 |
| 6 | 1,000 | saga | 1,225.2 | 693 / 1,350 | 0 | ok, ok | order insert 8.1, payment 4.7; inventory: lock 10.5, update 3.9, insert 2.0 |
| 7 | 1,000 | saga | 1,362.5 | 629 / 1,151 | 0 | ok, ok | order insert 10.4, payment 6.8; inventory: lock 15.5, update 4.7, insert 0.9 |
| 8 | 1,000 | monolith | 665.9 | 1,354 / 2,629 | 0 | ok | checkout 55.3, product lock 44.7, payment 8.7 |

**What it says.** On this setup the saga's front part ran at **1,145-1,154 orders/s at 700 shoppers and 1,225-1,363 at 1,000**, against **627-672 and 657-666** for the monolith: about 75% faster at 700 shoppers and about 85-110% faster at 1,000, with the median latency roughly halved, and with no failed order and the stock checks passing in every run. The saga's two runs at 700 shoppers agree within 1%; its throughput rises with more shoppers where the monolith's does not. The reason is in the statement times: the monolith's product-lock select (36-50 ms) and checkout statement (53-67 ms) wait on row locks held across slow commits, while the saga's reservation is three short statements on the inventory project (lock 1.8-15.5 ms, update 1.0-4.7 ms, insert 0.4-2.0 ms). The reservation's lock time rises with more shoppers (1.8 ms at 700, up to 15.5 ms at 1,000 in the last run), so the inventory project has its own ceiling above this.

**Limits.** The front part alone (no dispatcher, workers, Forge or results writes) and short runs (11-24 seconds); two runs per variant per shopper count; the figures say nothing yet about the full pipeline, whose sustained rate is what matters and is limited by the background work; the inventory project's compute sits in another availability zone than the others, so its round trip (0.8-1.0 ms) is higher; the monolith database shares the shop project's compute with the saga's orders database (one is used at a time).

## 53. The checkout saga on Neon: the full pipeline

**Question.** Section 52 measured only the front part. What does the whole pipeline sustain with the saga: shop (orders in the shop Neon project), inventory (own Neon project), payment and results projects on Neon, Forge and its database on EC2?

**Setup.** Shop binary with the saga, 4 dispatcher replicas, workers `inventory` and `order` with two processes each (128 slots, worker pool 30 connections), shop pool 200, inventory pool 200, payment pool 100, region pins on, load tool with 26 integrity checks. Stale unpublished outbox events were marked published first and Forge's queue was confirmed empty. Neon round trip from the database host, median: shop 0.6 ms, results 0.5, payments 0.9-1.0, inventory 0.9-1.0.

| Run | Shoppers | Orders | Foreground (orders/s) | Drain | End-to-end (orders/s) | p50 / p95 (ms) | Failed | Checks | Peak unpublished outbox events |
|---|---|---|---|---|---|---|---|---|---|
| 1 | 700 | 20,000 | 979.7 | 7.1 s | 726 | 512 / 1,702 | 0 | 26/26 | 6,237 |
| 2 | 1,000 | 20,000 | 1,123.5 | 8.3 s | 766 | 746 / 1,607 | 0 | 26/26 | 7,846 |
| 3 | 700 | 20,000 | 1,055.2 | 8.8 s | 721 | 529 / 1,385 | 0 | 26/26 | 8,851 |
| 4 | 1,000 | 20,000 | 1,139.9 | 24.6 s | 475 | 732 / 1,636 | 0 | 26/26 | 10,184 |
| 5 | 1,000 | 169,478 in 152 s | 1,118.2 | 91.6 s | 697 | 764 / 1,795 | 0 | 26/26 | 43,681 |

**What it says.**
- The checkout stays above 1,000 orders/s with the whole pipeline running: 1,055-1,140 at 1,000 shoppers in the short runs and **1,118 sustained for 152 seconds** (169,478 orders, none failed, all 26 checks passed, fully drained). The previous full-pipeline foreground on Neon was about 600-620.
- The **end-to-end rate (foreground plus drain) is 697-766**, except run 4 (475, a 24.6 s drain that I did not investigate; Neon run-to-run variance is about 25%). Orders arrive faster than the dispatchers publish them: the unpublished outbox backlog grew to 43,681 in the long run and took 91.6 s to clear after the load stopped. Nothing was lost; the work waits in the outbox.
- So the 1,000 orders/s target is met for the checkout (the front part of the pipeline) and **not yet for the full pipeline end to end**. The limiter is the dispatcher: the cost per outbox event on the orders project.

**Limits.** Five runs, one session, one repetition of the long run; no A/B against the previous pipeline in the same session (the 600-620 figure is from earlier sessions). The "peak cpu" column the script printed for the 150 s run (100% on every machine, including the load generator) is not credible and is not used; the CPU peaks of the 20,000-order runs (shop 71-87%, Forge database machine 73-79%, workers 51-55%, load generator 22-23%) come from `vmstat` samples. The old `fulfillment`, `invoice` and `notification` worker services were still running next to the new ones; the dispatcher only emits `inventory.reconcile` and `order.process` (`outbox.JobTypes`), so they should have had nothing to claim, but I did not check Forge's queue per type during the run.

## 54. The dispatcher's wait after a partial batch: no effect (change reverted)

**Question.** Section 53 left the end-to-end rate at 697-766 orders/s against a checkout of about 1,100. The dispatcher statements on the orders project took only about 2.4 ms of database time per event (44 s for 18,046 events in a 20,000-order run, from `pg_stat_statements`; the order insert and the payment statement took about 10 and 8-10 ms per order). The dispatcher loop sleeps `PollInterval` (200 ms) after any claim that returns fewer than `BatchSize` (100) events, and the average claim returned about 40 events, so the hypothesis was that the sleep, not the database, held the publish rate down.

**Method.** Full pipeline on Neon with the saga as in section 53, 4 dispatcher replicas, 1,000 shoppers, 20,000 orders after a 4,000-order warm-up, six runs in the order A, B, C, C, B, A, the outbox backlog cleared before each. A = the existing dispatcher, poll 200 ms. B = the same binary, poll 100 ms (the minimum). C = a binary that sleeps only when a claim returns zero events.

| Run | Variant | Foreground (orders/s) | Drain | End-to-end (orders/s) | Failed | Checks | Peak unpublished outbox events |
|---|---|---|---|---|---|---|---|
| 1 | A | 1,152.1 | 11.0 s | 705 | 0 | 26/26 | 8,977 |
| 2 | B | 1,078.7 | 11.1 s | 674 | 0 | 26/26 | 9,613 |
| 3 | C | 1,120.7 | 10.3 s | 710 | 0 | 26/26 | 8,652 |
| 4 | C | 1,103.5 | 11.0 s | 687 | 0 | 26/26 | 9,208 |
| 5 | B | 1,162.2 | 12.4 s | 676 | 0 | 26/26 | 10,505 |
| 6 | A | 1,111.9 | 11.5 s | 679 | 0 | 26/26 | 10,067 |

**What it says.** The three variants are indistinguishable (end-to-end A 679-705, B 674-676, C 687-710, inside the usual Neon variation of about 25%). The hypothesis is refuted: the sleep is not what limits the publish rate, and the code change was **reverted** (not committed). The drain times (10-12 s for a backlog of 9-10 thousand events once the checkout stops) suggest the dispatchers publish about 900 events/s when the checkout is not running and about 550/s while it is (20,000 minus the roughly 9,500 left behind, over the 18 s of load); that is an inference from two measured numbers, not a measurement of the dispatchers themselves. It points at contention with the checkout for the same Neon compute and commits, not at the dispatcher loop.

**Limits.** Two runs per variant, short runs, no per-round timing inside the dispatcher (where a round's wall time goes, including the Forge submit, is still not measured).

## 55. The attempt count folded into the claim: no effect (change reverted)

**Question.** Does removing one of the dispatcher's three database statements per batch (the separate `StartAttempts` rewrite of every row) raise the end-to-end rate? The variant D claimed events with `attempts = attempts + 1` in the same statement and dropped `StartAttempts`; the retry budget check used the count before the claim, and a claimed event that is lost to a crash before it is sent now uses an attempt (before, only a crash after the separate statement did).

**Method.** As section 54: full pipeline on Neon with the saga, 4 dispatcher replicas, poll 200 ms, 1,000 shoppers, 20,000 orders after a 4,000-order warm-up, order A, D, D, A, D, A (A = the existing dispatcher), the outbox backlog cleared before each run.

| Run | Variant | Foreground (orders/s) | Drain | End-to-end (orders/s) | Failed | Checks | Peak unpublished outbox events |
|---|---|---|---|---|---|---|---|
| 1 | A | 1,038.1 | 9.1 s | 705 | 0 | 26/26 | 6,335 |
| 2 | D | 1,094.3 | 10.5 s | 694 | 0 | 26/26 | 7,680 |
| 3 | D | 1,126.0 | 11.4 s | 687 | 0 | 26/26 | 6,926 |
| 4 | A | 1,191.6 | 12.8 s | 675 | 0 | 26/26 | 10,296 |
| 5 | D | 1,083.1 | 10.2 s | 697 | 0 | 26/26 | 6,318 |
| 6 | A | 1,198.5 | 14.5 s | 642 | 2 | 24/26 | 12,181 |

**What it says.** End-to-end is 642-705 for A and 687-697 for D: no difference beyond the usual Neon variation. D shows a lower peak backlog in its three runs (6,318-7,680 against 6,335-12,181) but that is not a rate difference and the A runs vary as much. The change did not help, and since it also alters the retry accounting it was **reverted** (not committed).

**A failed run, on the unchanged dispatcher (run 6, variant A).** Two shoppers got a client timeout (one on creating the order, one on payment), and 2 of the 26 checks failed: the stock ledger check (2 violations) and "every order that holds stock has its reservation, and none is held for a missing order" (1 violation). This run had the highest backlog of the session (12,181). The likely explanation is the saga's designed uncertain outcome: a request that timed out for the client may leave a stock reservation (or a payment applied without the client knowing) that the reservation reaper only settles after `FORGECART_RESERVATION_REAP_AFTER` (60 s), while the load tool checks right after the drain. I could not confirm that, because the next run erased the tables; I did not wait for the reaper or re-run the checks afterwards. It is recorded as an unexplained failure until that is shown, not as a pass.

**Limits.** Three runs per variant, short runs; the failed run's cause is not established.

## 56. Where the full pipeline queues up on Neon, and two changes that did not help (claimer linger, per-type index)

**Where the backlog sits.** One 150 s run at 1,000 shoppers (saga, Neon, 4 dispatcher replicas, 4 worker processes of 64 slots, Forge and its database on EC2), sampled every 2 s (176,206 orders, foreground 1,162 orders/s, 0 failed, 26/26 checks, drain 113.6 s). Both stages accumulate work. During the load the unpublished outbox events on the orders project grew to about 62,000 and the Forge queue to about 27,500 queued jobs (running jobs stayed at 50-160). After the load stopped, the outbox emptied in about 50 s (about 1,400 events/s without the checkout competing for the orders project) while the Forge queue kept rising to a peak of about 95,000 and then drained to zero at about 270 s.

Derived rates (estimates from those samples, assuming one outbox event per paid order, two Forge jobs per order and about 1,160 orders/s arriving): the dispatchers published about 765 orders/s while the checkout ran; the workers and Forge completed about 1,320 jobs/s, about 660 orders/s. Two stages in series, and the second is the tighter one; this matches the end-to-end rate of 600-700 seen in every run. Forge's database machine averaged 81% busy (peak 100%) while the workers averaged 39%, the shop 46% and the Forge server 27% (`vmstat`, 2 s samples). In Forge's database (`pg_stat_statements`, whole run), the claim statement took 415 s of about 540 s of statement time (109,559 calls, 3.79 ms average, 2.9 jobs per call), the completion statement 59 s, retention 31 s, submissions 30 s: about 2 cores' worth of statement time on a 2-vCPU machine.

**Claimer linger (worker set g against f), 20,000-order runs.** The claimer waited up to 10 ms for min(10, slots/8) idle slots before claiming. It halved the claim calls (9,873-10,203 against 18,551-20,106 per run, 3.5-3.7 jobs per call against 1.8-1.9) and the total claim time (6-7 s against 13-14 s), but not the end-to-end rate: g 701, 697, 514 (the last with an 18.2 s drain that was not investigated), f 697, 659, 695; 0 failed, 26/26 checks. In these short runs the claim averaged only about 0.7 ms, against 3.8 ms in the long run above, whose queue was far deeper. The change was **reverted** (not committed); it was not tested on a long run.

**Per-type index, 150 s runs.** Hypothesis: `jobs_due_idx` has no type column, so a claimer for one type walks past queued rows of the other type. Extra index `(type, next_run_at, created_at, id) WHERE status='queued'` created in Forge's database, order N, X, X, N:

| Run | Index | Foreground (orders/s) | Drain | End-to-end (orders/s) | Claim calls | Claim avg | Claim total | Failed | Checks |
|---|---|---|---|---|---|---|---|---|---|
| 1 | no | 1,119.4 | 128.4 s | 606 | 126,386 | 2.76 ms | 349 s | 0 | 26/26 |
| 2 | yes | 1,163.8 | 118.0 s | 654 | 123,191 | 3.08 ms | 380 s | 0 | 26/26 |
| 3 | yes | 1,141.8 | 124.5 s | 627 | 113,509 | 3.31 ms | 376 s | 0 | 26/26 |
| 4 | no | 1,104.2 | 111.6 s | 636 | 119,698 | 3.01 ms | 360 s | 0 | 26/26 |

The index did not make claims cheaper (3.08 and 3.31 ms with it, 2.76 and 3.01 without) and the end-to-end rate did not change beyond the usual variation. **This test was invalid and its conclusion is withdrawn (see section 57):** the planner never used the index, because the claim filters with `type = ANY(string_to_array(...))`; I did not check the plan or the index's use when I wrote this paragraph. The index was created by hand for the test and dropped afterwards, and no migration was written.

**Limits.** One long run for the profile, two to four runs per variant, no repetition of the profile. The throughput figures in jobs per second assume two jobs per order and were not read from Forge directly.

## 57. The claim query and Forge's database: a 3.4 to 5x cheaper claim, no change in the end-to-end rate

**What the plan showed.** On a scratch database on the Forge database machine (300,000 queued jobs of two types, only one type claimed, `EXPLAIN (ANALYZE, BUFFERS)`), one claim for the claimed type read the index `jobs_due_idx` from its head and removed 32,517 rows of the other type with the filter (5 ms against 0.6 ms on a fresh queue). The index has no type column, and with the filter `type = ANY(string_to_array(...))` the planner does not use an index that starts with `type` (checked: it kept `jobs_due_idx` after the index was created). Eight concurrent claimers (`pgbench`, 30 s, same machine as the database, so the absolute figures are low): the current claim fell from 597 to 224 transactions per second with only one type claimed, and from 2,058 to 1,374 when both types were claimed evenly; a claim that looks up each requested type separately (`unnest(types)` with a `LATERAL ... LIMIT ... FOR UPDATE SKIP LOCKED` subquery) over an index `(type, next_run_at, created_at, id) WHERE status='queued'` ran at 1,924 to 1,351 with one type claimed and 2,033 to 1,759 with both. Vacuum did not help (a `VACUUM`, or autovacuum tuned to 13 runs in 30 s, left the degradation unchanged), so dead tuples at the head of the index were not the cause.

**The pipeline test.** Full pipeline on Neon with the saga, 4 dispatcher replicas, worker set f, 1,000 shoppers, 150 s steady load, on **freshly created shop and payments Neon projects** (the old ones hit their free quota; the inventory and results projects were kept). O = Forge built from the same commit with the old claim and no extra index; N = the new claim and the index. A first attempt with the new build failed completely (every claim returned SQLSTATE 42P18: a comment in the SQL contained the text of a named parameter, which the database layer turned into an unused parameter); fixed, and rerun. Run 1 of O is from the first attempt, the other three from the second.

| Run | Variant | Foreground (orders/s) | Drain | End-to-end (orders/s) | Failed | Checks | Claim calls | Claim avg | Claim total |
|---|---|---|---|---|---|---|---|---|---|
| 1 | O | 1,103.8 | 60.6 s | 789 | 0 | 26/26 | 99,512 | 1.85 ms | 184 s |
| 2 | N | 1,194.7 | 81.6 s | 777 | 0 | 26/26 | 128,986 | 0.54 ms | 70 s |
| 3 | N | 1,130.8 | 60.9 s | 807 | 0 | 26/26 | 121,847 | 0.58 ms | 71 s |
| 4 | O | 1,149.4 | 71.8 s | 780 | 0 | 26/26 | 93,055 | 2.89 ms | 269 s |

**What it says.** The new claim is 3.2 to 5 times cheaper on Forge's database (claim time per run 70 s against 184 and 269 s; no claim errors in the Forge log) but the end-to-end rate is the same (777 and 807 against 789 and 780, inside the usual variation). So the claim query was a real cost but not what limits the end-to-end rate; the next candidates are the dispatchers (about 765 orders/s while the checkout runs, section 56) and the contention for the orders project. The end-to-end figures on the fresh projects (777-807) are higher than on the old ones (600-700), but the difference is confounded: the Forge build also differs from the one used then, and I did not repeat the old setup, so I cannot say what caused it.

**Limits.** Two runs per variant, one session; the Forge database's busy percentage was not recorded in these runs, so how much of its capacity the cheaper claim freed was not measured directly.

## 58. Dispatchers on a separate machine: no gain

**Question.** Does moving the four dispatcher replicas off the shop machine (which peaked at 87-100% busy and also runs the shop and payment services) raise the end-to-end rate? Setup as section 57: full pipeline on Neon with the saga on the fresh shop and payments projects, Forge built from the same commit with the old claim and no extra index, worker set f, 1,000 shoppers, 150 s steady load, a 4,000-order warm-up before each run, order S, R, R, S. S = 4 dispatcher replicas on the shop machine; R = none there and 4 on the idle `shop2` machine (same binary and settings, same Neon front-door pin for the shop project).

| Run | Dispatchers on | Foreground (orders/s) | Drain | End-to-end (orders/s) | Failed | Checks | Shop busy avg (`vmstat`, 5 s samples over the load) | shop2 busy avg |
|---|---|---|---|---|---|---|---|---|
| 1 | shop (S) | 1,193.2 | 73.4 s | 805 | 0 | 26/26 | 76% | 6% |
| 2 | shop2 (R) | 1,152.2 | 74.9 s | 771 | 0 | 26/26 | 71% | 9% |
| 3 | shop2 (R) | 1,154.2 | 72.8 s | 780 | 0 | 26/26 | 69% | 9% |
| 4 | shop (S) | 1,182.7 | 68.6 s | 814 | 0 | 26/26 | 72% | 6% |

**What it says.** Moving the dispatchers did not raise the end-to-end rate: 771 and 780 on `shop2` against 805 and 814 on the shop machine (the separate machine is, if anything, 4-5% lower, which is inside the usual variation but consistently in the same direction). The shop machine's average busy percentage fell by only about 5 points without the dispatchers (76 and 72% with them, 71 and 69% without), and the four dispatchers used about 6-9% of `shop2`: the dispatchers are light on CPU, and the shop machine's CPU was not what limited them. The remaining candidates are the shared databases (the orders project, where the dispatchers compete with the checkout for commits and locks, and Forge's database). The highest end-to-end rate measured in these sessions is 814 orders/s (run 4), typical 780-815 on the fresh projects.

**Limits.** Two runs per variant, one session, 150 s each. The `vmstat` busy maximum of 100% on both machines in every run is not used (the first samples after the start of the sampler are not representative).

## 59. Eight dispatcher replicas: no gain, slightly lower

**Where the time goes (one 150 s run, 1,000 shoppers, 756 end-to-end, 0 failed, 26/26 checks; `pg_stat_activity` sampled every second).** On the orders project (Neon) 83% of the active-backend samples were commits waiting for replication (59% `LWLock:SyncRep`, 24% `IPC:SyncRep`) and 3% CPU. In Forge's database (EC2) 46% were CPU and 50% were the write-ahead log (33% `LWLock:WALWrite`, 17% `IO:WALSync`); its disk wrote 588 times per second at 1.02 ms with 54% utilisation, and the machine was 17% in I/O wait and 30% idle. Both databases are bound by how many commits they can finish, not by CPU or locks.

**The test.** Forge on the claim rewrite build with `jobs_due_type_idx` (section 57), worker set f, 1,000 shoppers, 150 s steady load, a 4,000-order warm-up before each run, order S, E, E, S. S = 4 dispatcher replicas on the shop machine; E = 4 there and 4 more on `shop2` (8 in all).

| Run | Replicas | Foreground (orders/s) | Drain | End-to-end (orders/s) | Failed | Checks | Shop busy avg |
|---|---|---|---|---|---|---|---|
| 1 | 4 | 1,224.9 | 84.8 s | 786 | 0 | 26/26 | 74% |
| 2 | 8 | 1,145.2 | 84.7 s | 735 | 0 | 26/26 | 70% |
| 3 | 8 | 1,010.0 | 53.6 s | 747 | 0 | 26/26 | 66% |
| 4 | 4 | 1,172.1 | 66.4 s | 815 | 0 | 26/26 | 71% |

**What it says.** Eight replicas gave 735 and 747 against 786 and 815 with four: no gain, and slightly lower (about 6-9%), with a lower checkout rate in run 3 (1,010 against 1,172-1,225): the extra dispatchers compete with the checkout for the orders project's commits. This supports the reading that the orders project's commit rate is the shared limit. More dispatcher concurrency does not help.

**A flaw in this run.** The script's check of which Forge binary was running printed the hash of an empty string (`e3b0c442...`), so it did not confirm that the claim rewrite build was the one serving the runs (health was 200, the index was present and the Forge log held no claim errors). I changed the service's `ExecStart` to that build, but the identity of the running binary is not verified for these runs.

**Limits.** Two runs per variant, one session.

## 60. Both stages improved at once: dispatcher with the attempt count in the claim, and a claimer that waits for a few idle slots

**Why this test.** Sections 54-59 changed one stage at a time and none moved the end-to-end rate. The two stages sit in series (dispatchers, then Forge and the workers), so a gain on one stage can be hidden by the other. Here both are changed together, on top of the cheaper claim of section 57 for both variants. Forge: the claim rewrite build (hash checked on the running process: 43bc3e8164dd) with `jobs_due_type_idx`. B = the existing dispatcher (three database statements per round) and worker set f. C = a dispatcher whose claim adds the attempt (`attempts = attempts + 1` in the claiming statement; no separate attempt statement; the retry budget check uses the count before the claim; an event claimed and then lost to a crash before it is sent now uses an attempt) and worker set g (the claimer waits up to 10 ms for min(10, slots/8) idle slots before claiming; workers with fewer than 16 slots never wait). Fresh shop and payments Neon projects, 4 dispatcher replicas on the shop machine, 128 worker slots, 1,000 shoppers, 150 s steady load, a 4,000-order warm-up before each run, order B, C, C, B, C, B.

| Run | Variant | Foreground (orders/s) | Drain | End-to-end (orders/s) | Failed | Checks | Claim calls | Rows per claim | Claim avg | Claim total |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | B | 1,200.5 | 66.2 s | 836 | 0 | 26/26 | 117,817 | 2.8 | 0.58 ms | 68 s |
| 2 | C | 1,157.2 | 39.4 s | 919 | 0 | 26/26 | 51,597 | 6.1 | 0.76 ms | 39 s |
| 3 | C | 1,227.1 | 56.0 s | 896 | 0 | 26/26 | 53,726 | 6.2 | 0.88 ms | 47 s |
| 4 | B | 1,154.8 | 56.8 s | 840 | 0 | 26/26 | 106,261 | 3.0 | 0.66 ms | 70 s |
| 5 | C | 1,100.8 | 71.5 s | 749 | 0 | 26/26 | 48,434 | 6.2 | 0.75 ms | 36 s |
| 6 | B | 1,163.0 | 70.7 s | 794 | 0 | 26/26 | 106,514 | 3.0 | 0.62 ms | 66 s |

**What it says.** B averaged 823 end-to-end (794 to 840), C averaged 855 (749 to 919): C is about 4% higher on average, with a median of 896 against 836, but the two ranges overlap (run 5 of C is the lowest of all six) and the spread is the usual Neon spread of about 25%, so this is a suggestion, not a demonstrated gain. What is solid: C halves the number of claims (about 50,000 against 106,000-118,000 per run, 6.1-6.2 jobs per claim against 2.8-3.0) and the claim time (36-47 s against 66-70 s), with no claim errors, 0 failed orders and all checks passing in all six runs. By the rule agreed for this work (at least three runs at or above 850 on average, 0 failed, 26/26 checks) C meets 850 on its average of three (855), with a weak margin. The highest single end-to-end figures measured are 919 and 896 (runs 2 and 3).

**Limits.** Three runs per variant, one session. The baseline B (823) is higher than the 786-815 measured for the same dispatcher and Forge build earlier the same day, which is the variation between runs in practice. The code for C is not committed (the claim rewrite, the attempt-in-claim dispatcher, the claimer wait).

## 61. Second set of section 60 on newly created shop and results projects

The shop project hit the free transfer quota again after about 18 long runs in three hours (the results project had run out too); both were deleted and created again, with the schema applied from the migrations (the results project got the same migrations as the shop project, whose result tables it uses). Setup, Forge build (hash checked: 43bc3e8164dd), variants and run length as in section 60; order C, B, B, C, B, C; one discarded 10,000-order warm-up first. A first attempt of this set failed at setup (the results project was empty) and a second one earlier failed because the quota had run out; neither produced a result.

| Run | Variant | Foreground (orders/s) | Drain | End-to-end (orders/s) | Failed | Checks | Claim calls | Rows per claim | Claim total |
|---|---|---|---|---|---|---|---|---|---|
| 1 | C | 1,332.4 | 35.7 s | 1,078 | 0 | 26/26 | 56,883 | 6.4 | 44 s |
| 2 | B | 1,498.4 | 80.0 s | 979 | 0 | 26/26 | 143,251 | 2.8 | 100 s |
| 3 | B | 1,429.3 | 53.3 s | 1,057 | 5 | 24/26 | 130,700 | 3.0 | 98 s |
| 4 | C | 1,472.1 | 55.4 s | 1,077 | 5 | 26/26 | 61,537 | 6.5 | 66 s |
| 5 | B | 1,439.8 | 53.7 s | 1,063 | 5 | 25/26 | 131,878 | 3.0 | 102 s |
| 6 | C | 1,467.3 | 65.6 s | 1,023 | 0 | 26/26 | 61,281 | 6.5 | 66 s |

**What it says.** Everything is much faster than in the earlier sets (checkout 1,330-1,500 orders/s, end-to-end 979-1,078), for both variants, so the cause is the environment (the new projects, or where Neon placed them), not the code; I did not isolate it. Within this set, C averages 1,059 and B 1,033 (+2.5%), inside the variation. Together with section 60 (C 855 against B 823, +4%) the direction is the same in both sets and the size is small; the number of claims stays about halved in all six C runs (about 57,000-62,000 against 131,000-143,000). The target of 850 end-to-end at the 150 s definition is exceeded in this environment by both variants, so it does not show a gain from the changes.

**Failures.** Runs 3, 4 and 5 each had 5 failed orders (client timeouts; runs 4 and 5 are known: run 4 order_create 4 and payment 1, run 5 order_create 1 and payment 4). In runs 3 and 5 integrity checks failed afterwards: the stock ledger and the reservation match (run 3), and "no payment is stranded: every succeeded or declined attempt was applied to its order" with 3 violations (run 5). Both variants are affected, so this is not specific to C. These are the saga's uncertain outcomes after a client gave up (a reservation or an applied payment the client never saw), which the reservation reaper (60 s) and the payment sweeper are meant to repair, but the load tool checks right after the drain and I did not wait and re-check, so the repair is not shown. Still open.

**Limits.** Three runs per variant, one session, 150 s each; the same kind of failure as the unexplained one in section 55.

## 62. Chaos test 1: the shop service frozen for 12 s under load (a saga defect found)

**Test.** Full pipeline on Neon (the setup of section 61: dispatcher-new, worker set f, Forge base2), 1,000 shoppers for 60 s; when the orders table passed 8,000 rows the shop service was frozen with `SIGSTOP` for 12 s and then resumed. Afterwards a read-only checker (the same three comparisons as the load tool: reservations against orders, the stock ledger, stranded payments) ran right after the drain (T0) and 90 s (T1) and 180 s (T2) later, to see whether the saga repairs itself. A first attempt froze the shop before the load tool had erased its tables (my script's mistake) and produced nothing.

**Load tool result.** Foreground 1,048.3 orders/s, drain 10.0 s, 63,568 completed, **1,000 failed** (every shopper in flight during the freeze: 424 `order_create` timeouts and 576 `payment` timeouts), 24/26 checks passed; the two failing checks were the stock ledger and the reservation match.

| Snapshot | Reservation mismatches | Products off in the stock ledger | Stranded payments | Payment state violations |
|---|---|---|---|---|
| T0 (right after the drain) | 47 | 94 | 0 | 0 |
| T1 (+90 s) | 25 | 47 | 0 | 0 |
| T2 (+180 s) | 25 | 47 | 0 | 0 |

**What it says.** The payment side converged (no stranded payments and no payment violations at any snapshot). The reservation side repaired only half: the reaper released the reservations held for orders that were never created (47 became 25 mismatches), but **25 mismatches never went away**. All 25 are the same state: an order in `pending_payment` with `inventory_status = 'reserved'` whose stock reservation is `released` (created about 03:33:08, the end of the freeze), so the stock was returned to inventory while an order still believes it holds it. None of the 25 was paid in the test. If such an order were later paid, its units would be counted as available again (an oversell); if it expires unpaid, the release is a no-op and the ledger would settle. I did not test either follow-up.

**Cause (from reading the code, consistent with the data, not proven by a targeted test).** In `forgecart/internal/orders/checkout.go`, when the statement that inserts the order returns any error, the code releases the reservation at once (`s.release(ctx, id)`). A statement that returned a context-cancelled or connection error may still have committed on the server (the client gave up during the freeze, the order insert finished after the shop resumed). The order then exists and its reservation has been released. An error is not proof that nothing was written; the reservation reaper already handles the "order really missing" case after 60 s, so the immediate release is both unnecessary and unsafe in the uncertain case.

**Status.** Found, not fixed. A fix would look up the order by id (with a context that is not cancelled) when the insert fails: keep the reservation if the order exists, release it if it is confirmed absent, and leave it to the reaper if the lookup itself fails.

**Limits.** One run, one disturbance (a freeze), one session; the 25 stuck orders were counted, not followed to payment or expiry.

## 63. Chaos test 1 repeated after the checkout fix: the saga converges

**Change.** `forgecart/internal/orders/checkout.go` no longer releases the reservation when the order insert returns an error; the reaper (60 s grace period, every 10 s) releases it only if no order exists. Shop build verified by hash on the running process (`aa35169de62b`); everything else as in section 62 (the same freeze of 12 s at 1,000 shoppers for 60 s, the same checker at T0, T1 and T2).

**Load tool.** Foreground 1,054.1 orders/s, drain 9.7 s, 63,763 completed, **1,000 failed** (342 `order_create` and 658 `payment` timeouts: every shopper in flight during the freeze), 23/26 checks passed right after the run (the stock ledger, the reservation match and the stranded-payment checks failed at that moment).

| Snapshot | Reservation mismatches | Products off in the stock ledger | Stranded payments | Payment state violations |
|---|---|---|---|---|
| T0 (right after the drain) | 206 | 381 | 0 | 0 |
| T1 (+90 s) | 0 | 0 | 0 | 0 |
| T2 (+180 s) | 0 | 0 | 0 | 0 |

**What it says.** Before the fix 25 mismatches and 47 products off stayed forever (section 62); after it, everything is repaired by the first snapshot after the reaper's grace period and stays repaired (T1 and T2 are identical, 0 everywhere). The T0 figures are higher than before (206 against 47) because the checkout now leaves the reservation of an order that failed to the reaper instead of releasing it at once; they are the transient state that the reaper clears. The load tool's own "no payment is stranded" check failed at the end of the run, yet my checker found 0 stranded payments at T0 (about a minute later): the payment sweeper repaired them in between (not measured separately).

**Limits.** One run per version, one disturbance (a 12 s freeze of the shop service); the fix was not tested against a kill of the shop process or against database outages.

## 64. Chaos group 1: killing the dispatchers, the workers, Forge, the shop and the payment service under load

**Method.** Full pipeline on Neon (shop build with the checkout fix of section 63, dispatcher-new, worker set f, Forge base2), 1,000 shoppers for 60 s on the projects of section 61. When the orders table passed 8,000 rows (counted after the load tool had erased it), one component was killed with `SIGKILL` (systemd's `Restart=on-failure` brought every one of them back within 6 s; I recorded that and started them by hand if not). After the drain a read-only checker (reservations against orders, the stock ledger, stranded payments, payment state) ran at once and again 100 s later. The expectations were written before the runs (see `~/.forge-aws/neon_kills.sh`; the dispatcher trial was repeated because its run id was longer than the tool allows, and the shop trial was repeated with snapshots up to 300 s).

| Killed | Load tool | Checker, right after the drain and later |
|---|---|---|
| All four dispatchers | 1,425 orders/s foreground, drain 22.8 s, 0 failed, 26/26 checks, drained | 0 mismatches, 0 products off, 0 stranded at both snapshots |
| All four workers | 1,320 orders/s, drain 29.4 s, 0 failed, 26/26, drained | all 0 at both snapshots |
| Forge | 1,464 orders/s, drain 26.3 s, 0 failed, 26/26, drained | all 0 at both snapshots |
| The shop service, first run | 1,378 orders/s, drain 23.0 s, 30,539 failed shopper requests (29,843 `order_create` and 691 `payment` HTTP 502, 5 timeouts), 23/26 checks | right after: 3 reservation mismatches, 8 products off, **1 stranded payment**; 100 s later: 0 and 0 but **still 1 stranded payment** |
| The shop service, second run | 1,361 orders/s, drain 18.1 s, 32,773 failed requests (all HTTP 502 while the shop was down), 26/26 checks | all 0 at 0, 100, 200 and 300 s |
| The payment service | 1,392 orders/s, drain 34.0 s, 6,073 failed requests (all clean `503 payment_unavailable`), 26/26, drained | all 0; 10,331 orders left in `pending_payment` (about 4,600 in a normal run) with their reservations held |

**What it says.** Nothing was lost or corrupted by killing the dispatchers, the workers or Forge: the background pipeline recovers by itself, the integrity checks pass, and every order still gets its jobs. The payment service going away produces clean refusals and no inconsistency (the orders whose payment was refused stay pending until the shopper pays again or the order expires). The failed-request counts of the shop kill are attempts, not lost orders: a shopper whose request fails at once repeats it, so a short outage produces tens of thousands of counted failures. After a kill of the shop the saga repairs itself (mismatches and ledger differences clear within 100 s), as after the freeze of section 63.

**Open: one stranded payment.** In the first shop kill, one payment that had succeeded in the payment service had not been applied to its order 100 s after the drain, although the payment sweeper (every 30 s, on payments older than 30 s) should repair it. The second shop kill, watched for 300 s, left none. I could not keep the first state (the next run erases the tables), so I do not know which order it was or whether the sweeper repaired it later. From reading `forgecart/internal/payments/sweeper.go` the sweeper walks the finished attempts in order of completion from a stored cursor; I did not find a case that explains it (a payment becoming visible after the cursor had passed its completion time is the candidate I could not rule out). Treat it as an unexplained, non-reproduced observation, not as a verified defect.

**Limits.** One run per kill (two for the shop and the dispatchers), one session; the kills happen at one moment of the load; no database outages, network faults or provider faults yet (groups 2 to 5 of the plan).

## 65. Chaos group 6: a deep backlog, then everything killed and restarted

**Method.** Full pipeline on Neon (setup of section 64), but the workers were stopped for a 60 s load at 1,000 shoppers, so the paid orders piled up. At the end of the load: 76,447 paid orders, 26,488 events still unpublished in the outbox and 99,918 jobs queued in Forge (its limit is 100,000 queued jobs), none running. Then the shop service, the payment service, the four dispatchers and Forge were all killed with `SIGKILL` at once; systemd had brought them back within 8 s; the workers were started 17 s after the kill. The load tool's own checks at the end of the load are not meaningful here (nothing had drained).

**Result.**
- The pipeline emptied (outbox 0, Forge 0 queued and 0 running) **77 s after the kill** (57 s after the kill the outbox was empty and 39,688 jobs were still queued).
- Every paid order got exactly one row in each result table: invoices 76,447 rows / 76,447 orders, fulfillments 76,447 / 76,447, notification deliveries 76,447 / 76,447, inventory reconciliations 76,447 / 76,447. Nothing was lost and, in this run, nothing was duplicated.
- Forge: 152,894 jobs created since the test started, all succeeded (2 per paid order), 0 failed, 0 retried (`attempt > 1`), none left queued or running.
- The saga checker afterwards: 84,848 orders (76,447 paid, 4,141 pending, 4,260 payment failed), 0 reservation mismatches, 0 products off in the stock ledger, 0 stranded payments, 0 payment state violations.

**Not covered.** The workers were stopped before the kill, so no job was running and Forge's lease recovery of a job held by a dead worker was not exercised here (it was in the worker kill of section 64, which had 0 failed orders and 26/26 checks). The Forge database itself was not restarted. The Forge `jobs` table also holds 132,772 failed jobs from earlier today (from the sessions in which the Neon quota ran out and the tables were erased under queued jobs); none of them is from this test (0 failed since it started), and no retention is set for failed jobs.

**Limits.** One run, one backlog size (about 100,000 jobs).

## 66. Chaos group 2, part 1: Forge's own database restarted and crashed under load

**Method.** Full pipeline on Neon (setup of section 64, on newly created shop, payments and inventory projects after the earlier ones ran out of quota), a 60 s load at 400 shoppers (smaller than before to save the Neon transfer quota); when the orders table passed 5,000 rows the PostgreSQL service of Forge's database on EC2 was (1) restarted with `systemctl restart`, then in a second run (2) killed with `SIGKILL` so that crash recovery runs when it starts again (the service has no automatic restart, so it was started by hand a few seconds after the kill). A first attempt failed: the trigger of the clean restart never fired, and in the crash run the Neon inventory project ran out of quota, so the checks that read it failed for that reason; neither is counted. Expected, written before the runs: Forge answers with database errors while the database is down, the dispatchers and workers retry, Forge stays up and reconnects, nothing acknowledged is lost, the pipeline drains.

| Trial | Forge process | Forge log lines with a database error | Load tool | Forge jobs | Saga checker, once, right after the drain |
|---|---|---|---|---|---|
| Clean restart | stayed up (same process id) | 186 | 362.7 orders/s, drain 0.6 s, drained, 7 failed (5 `order_create` timeouts, 1 `order_create` HTTP 500, 1 `payment` timeout), 24/26 checks (stock ledger, reservation match) | 66 retried, none failed | 1 reservation mismatch, 2 products off in the ledger, 0 stranded payments |
| Crash (SIGKILL) | stayed up | 445 | 378.2 orders/s, drain 0.4 s, drained, 5 failed (all `order_create` timeouts), 26/26 checks | 138 retried, none failed | all 0 |

**What it says.** Forge survives the loss of its own database both ways: it stayed up, logged database errors while the database was down, reconnected by itself, and the retried jobs (66 and 138) all succeeded; no Forge job failed, the pipeline drained, and no paid order lacked its results (26/26 checks in the crash run). Recovery after the `SIGKILL` replayed the log without losing anything the checks could see. The checkout does not use Forge's database, yet 5 to 7 shopper requests timed out in each run; I did not establish why (the first run after creating the projects, so a cold Neon start is a candidate, and the 400-shopper level had not been run before). In the clean restart run the 1 reservation mismatch and 2 products off are the state the checkout leaves for a request that timed out until the reaper clears it (section 63); the checker ran once, so that repair is not shown for this run.

**Limits.** One run per trial, one disturbance moment, a smaller load than the other tests; the Forge process restarted nothing, so its own recovery of a half-written lease or completion was not distinguished from plain retrying.

## 67. Chaos group 3: network delay, packet loss and a cut

**Method.** Full pipeline on Neon (setup of section 66), a 60 s load at 400 shoppers; when the orders table passed 5,000 rows one disturbance was applied for about 20 s and removed (Linux traffic shaping on one interface, or a firewall rule; both also removed by a cleanup step at the end). Expected, written before the runs: delay: throughput falls while it lasts, some requests may time out, everything recovers and the saga repairs itself; loss: claims and completions are slow or retried, nothing is lost; cut: events stay in the outbox, the dispatchers retry, nothing is lost or duplicated. The first run of loss and cut could not show that the disturbances had taken effect (no errors in the logs, unchanged throughput), so they were repeated with a probe while the disturbance was on and a higher loss.

| Disturbance | Proof that it took effect | Load tool | Checks | Forge jobs | Saga checker after the drain |
|---|---|---|---|---|---|
| +150 ms on traffic from the shop machine to the Neon front door of the shop, payments and results projects | throughput 273.5 orders/s against about 380 in the other 400-shopper runs | 10 failed (all `payment` timeouts), drain 0.6 s | 24/26 at the end of the load (stock ledger, reservation match) | 146 retried, 0 failed (the 6-minute window includes the previous run) | all 0: no mismatches, no products off, no stranded payments |
| 30% packet loss from the worker machine to Forge | 40 pings: 25% loss measured | 5 failed (2 HTTP 500 and 3 `order_create` timeouts), drain 3.8 s, drained | 26/26 | 15 retried, 0 failed | all 0 |
| Shop machine (the dispatchers) cut off from Forge's port 8080 | a request to `/health` from the shop machine got no answer within 4 s | 8 failed (2 HTTP 500, 6 `order_create` timeouts), drain 0.4 s, drained | 26/26 | 7 retried, 0 failed | all 0 |

**What it says.** None of the three lost or duplicated anything: no Forge job failed, every drain completed, and the saga checker found nothing inconsistent after any of them. A slow database path slows the checkout (about 28% here) and produces payment timeouts that the saga repairs; a lossy link to Forge costs retries but no failures; a 20 s cut between the dispatchers and Forge is absorbed by the outbox (the events wait and are published after the cut). The first run of loss (15%) and cut had shown the same pipeline results but could not prove the disturbance had been on.

**Not explained.** Each 400-shopper run in this chaos work had a handful (1 to 10) of failed checkout requests (`order_create` timeouts or HTTP 500) even when the disturbance (a lossy link to Forge, a cut to Forge) cannot touch the checkout; I did not find the cause (a cold Neon start after the projects were created, or the load level, are candidates). **Limits.** One run per disturbance, 20 s each, one moment in the load.

## 68. Chaos tests, second day: control runs, a repeated shop kill, front-door outages, worker outage, payment faults, overload, a flood

Setup as in section 66 (Neon projects created on 2026-10-30, shop build with the checkout fix of section 63), a 60 s load per test unless noted, the same checker as before. Each test had its expectation written before the run (the result files are in `~/.forge-aws/results/neon-c2-*.txt`, `neon-kill3.txt` and `neon-flood.txt`).

| Test | What was done | Result |
|---|---|---|
| Control, 400 shoppers, no disturbance | nothing | 10 of 21,713 checkouts failed on their own (7 timeouts, 3 HTTP 500; the checkout deadline is 10 s and p99 of the workflow was 5.7 s, maximum 14.6 s); 26/26 checks; saga checker all 0. The 1 to 10 failed requests seen in every 400-shopper run of section 66 and 67 were this baseline, not an effect of the disturbance. |
| Shop service killed (SIGKILL) three more times, 1,000 shoppers | stranded-payment details captured at once, +100 s and +200 s | 0 stranded payments in all three (30, 35 and 23 reservation mismatches right after the drain, all 0 at +100 s). With the repeat of section 64 the one stranded payment of section 64 was not reproduced in 4 attempts; it stays unexplained. A control at 1,000 shoppers showed a stranded payment right after the drain that was gone 90 s later (sweeper), which suggests the earlier one was a transient state seen at a slightly different moment (not proven). |
| Slowdown found on the way | the checkout fell from about 1,400 to about 380 orders/s | Cause: after the inventory project was recreated I seeded the 5-product starter catalogue instead of the 3,005 products of every earlier test, so all shoppers waited for the same 5 stock rows (92% of the inventory project's time was `Lock:tuple`). Restoring the 3,005 products brought the control back to 1,464.7 orders/s. Throughput figures of the chaos runs made between the recreation and the restore (sections 66 and 67 and the shop kills above) are therefore low; their correctness results stand. |
| Shop machine loses the Neon front door (shop, payments, results projects) for 20 s | connections refused (verified) | 38,286 failed attempts, 37,945 `order_create` HTTP 500; **37,162 orphan reservations and 2,000 products with stock held** right after the drain, cleared by the reaper in about 5.5 minutes (about 150 per second); nothing stranded. A weakness (see section 69). |
| Inventory project unreachable for 20 s | connections refused (verified) | 212,666 failed attempts (all `order_create` HTTP 500, shoppers retry at once), 13 mismatches and 28 products off right after the drain, all 0 at +90 s. |
| Workers lose the shop and results Neon projects for 20 s | connections refused (verified) | 0 failed orders, 26/26 checks; 24,203 jobs retried, 0 failed (the retry budget was not used up), nothing lost. |
| Payment service frozen for 25 s (longer than the shop's 20 s payment timeout) | `SIGSTOP`/`SIGCONT` | 800 payment timeouts (the 400 shoppers in flight, twice), 26/26 checks, 0 stranded payments, 0 mismatches. |
| Payment provider with 20% declines and 30% temporary failures for 25 s (the payment service restarted at both ends) | simulator settings | 718 failed requests (490 `503 interrupted` from the restarts, 228 `503 payment_unavailable`), 26/26 checks, 0 stranded, 0 mismatches. |
| Overload: 1,000 shoppers all ordering 3 products | stampede on 3 stock rows | 246.4 orders/s, p50 3.0 s, p99 9.6 s; 781 failed (619 `order_create` timeouts, 145 HTTP 500, 17 payment timeouts); 24/26 at the end of the load; the checker found all 0 about 13 minutes later; no negative stock. |
| Flood into Forge's submit API | batches of 100 jobs of a type nobody consumes, 64 threads, 150 s | 100,000 accepted (667 jobs/s), then 349,134 `503 queue_full` (the first after 4.6 s); Forge stayed healthy (health answered in 0.4 ms); nothing accepted was lost; **a job of another type was refused as well**: the queue limit is global, so one runaway job type blocks all others. After deleting the junk the queue was empty and its counter matched the real count. |

**Harness problems (mine, not ForgeCart's).** A trigger that never fired when the tables held 1,000 to 5,000 rows from an earlier run, a stuck reused SSH connection that hung one script for 8 minutes, and a deployment whose copy dropped its connection while the test was started anyway on the old build (stopped, redone, build verified by hash).

**Not covered.** A real pause and resume of a Neon project (its cold start); the front-door tests refuse connections, which imitates an outage but not that. One run per test.

## 69. The cleanup after a shop-project outage, fixed

**What changed (shop build `848f3c1ac7a5`).** (1) The reaper releases reservations in bulk: `inventory.ReleaseMany` gives back up to 200 reservations in one transaction and one commit, locking every product once in SKU order, instead of one commit per reservation. (2) The checkout releases its reservation at once when the order insert fails because the connection could not be made at all (`pgconn.ConnectError`: the statement was never sent); for any other error it still leaves the decision to the reaper (section 63). (3) The checkout and payment handlers answer `503 service_unavailable` with `Retry-After: 2` when the database is unreachable, instead of a 500.

**Repeat of the 20 s loss of the front door (400 shoppers, 60 s).** The probe confirmed connections were refused and worked again afterwards. Load tool: 679.2 orders/s foreground, 41,078 completed, 20,484 failed attempts: 20,051 `order_create` `503 service_unavailable`, 355 payment `503 payment_unavailable`, 10 payment `503 service_unavailable`, and only 68 HTTP 500 (45 `order_create`, 23 payment; failures in flight when the connections were reset, which the handlers do not yet classify). Right after the drain the checker found **0 reservation mismatches, 0 products off, 0 stranded payments, 0 payment violations** (before the fix: 37,162 mismatches and 2,000 products off, clearing in 5.5 minutes). Forge: 119,250 jobs succeeded, 0 retried, 0 failed.

**Limits.** One run. The 68 remaining HTTP 500s are errors from connections that broke while a statement was in flight; they are still uncertain outcomes and are still left to the reaper.

## 70. A per-type limit on queued jobs (`max_pending`)

**Why.** The flood of section 68 filled Forge's global limit of 100,000 queued jobs with one job type, and a job of another type was refused too. The decision record (ADR 0004) specifies a per-type `max_pending`; this implements it (config file field, enforced in the admission functions, reported by `GET /v1/queues`).

**Functional test** (Forge on the new build, `invoice.generate` limited to 1,000, the global limit 100,000; no worker consuming the flooded types). A 20 s flood of batches of 100 `invoice.generate` jobs from 16 threads: **exactly 1,000 jobs accepted** (10 batches), 41,696 batch requests refused with `503 queue_full`, the first after 0.3 s; the database held exactly 1,000 queued rows of the type. While it was full, one `notification.send` was accepted (202) and so was a batch of 5; one `invoice.generate` was refused (503); a batch of 2 `invoice.generate` and 1 `notification.send` was refused entirely (all or nothing); `GET /v1/queues` showed `max_pending` 1,000 for `invoice.generate`, `null` for `notification.send`, and counted the rejections. The junk (1,006 rows) was deleted afterwards.

**What the count costs.** The check runs inside the global admission lock and reads up to `max_pending` queued rows of the type. Same flood (16 threads, batches of 100, 25 s, `invoice.generate`) with no per-type limit, then with a limit of 50,000, run twice each: with no per-type limit the flood was refused by the global limit after 100,000 jobs and Forge handled about 46,700 requests in 25 s (1,000 accepted + 45,689 refused, then 1,000 + 46,594); with a limit of 50,000 it handled only about 3,800 requests (500 accepted + 3,296 refused, then 500 + 3,259), about **12 times fewer**, because every refusal counted 50,000 rows under the lock. With a limit of 1,000 (the functional test) it handled about 41,700 requests in 20 s, about as many per second as the global refusals, so no visible cost at that size. A large limit on a type that is hammered therefore slows the admission of every type: a per-type counter table (like the global queue counter) is the way to remove the cost and has not been built.

**Error body (added afterwards).** The refusal by a type's limit now names the type and the limit (`job_type`, `limit`). Checked on the Forge machine with `invoice.generate` limited to 5: a single job and a batch (2 `invoice.generate` and 1 `notification.send`) were both refused with `503`, `Retry-After: 5`, and `"job_type":"invoice.generate","limit":5`, while a single `notification.send` was accepted. The SDK's new `JobType` and `Limit` fields were built and vetted but not exercised against the server.

**Limits.** One setup, one flood size, no ForgeCart load run with limits set (ForgeCart configures none). The migration and the type-aware claim of section 57 are committed together because the count needs `jobs_due_type_idx`.

## 71. The current end-to-end rate, and the dispatchers committing asynchronously

**Setup (2026-11-01).** Full pipeline on Neon with the saga: shop build with the cleanup fixes (`848f3c1ac7a5`), Forge with the type-aware claim and the per-type limit code (`fa343a99d410`, no per-type limit configured), the original dispatcher with four replicas, worker set f, the 3,005-product catalogue, 1,000 shoppers, 150 s steady load, a 4,000-order warm-up before each run, one discarded 10,000-order run first, order N, Y, Y, N. N = as it is. Y = the four dispatchers connect with `options=-c synchronous_commit=off` (their own connection only; their updates are safe to lose in a crash, because an event not marked published is published again and the jobs tolerate duplicates). The check that the option reached the dispatchers printed 0 in the script because of a quoting mistake in the script; it was checked by hand during run 3 (the option is in the environment of the dispatcher processes, and `/etc/cart-dispatcher.env` existed).

| Run | Dispatchers | Foreground (orders/s) | Drain | End-to-end (orders/s) | Failed | Checks | Peak unpublished outbox events |
|---|---|---|---|---|---|---|---|
| 1 | N | 1,466.9 | 64.4 s | 1,029 | 1 | 26/26 | 55,732 |
| 2 | Y | 1,465.8 | 62.1 s | 1,039 | 2 | 25/26 (stranded-payment check, transient) | 24,941 |
| 3 | Y | 1,467.5 | 72.0 s | 994 | 0 | 26/26 | 13,216 |
| 4 | N | 1,471.7 | 81.6 s | 957 | 0 | 26/26 | 27,276 |

**What it says.** The current end-to-end rate is about **1,000 orders/s** (N: 1,029 and 957; the best single run 1,039), with a checkout of about 1,470 orders/s sustained for 150 s (about 222,000 orders per run). Asynchronous commits in the dispatchers did what they should at their own stage: the peak backlog in the outbox fell to about half (13,216 and 24,941 events against 27,276 and 55,732), but the end-to-end rate did not move (Y 994 and 1,039, average 1,017; N 957 and 1,029, average 993; +2%, inside the usual variation). So the dispatcher stage is not what limits end-to-end now; the stage after it (Forge's database and the workers) is. The drain (62 to 82 s for about 220,000 orders) is the time that stage needs to finish the backlog.

**Limits.** Two runs per variant, one session; the stranded-payment failure of run 2 was not re-checked afterwards (the sweeper clears it in earlier runs). The setting was removed again afterwards.

## Limits of these sessions

- A handful of sessions on one availability zone and small burstable machines; one to three repetitions per level, and the batch sweeps one repetition each. Quantiles from `/metrics` are bucket estimates.
- Forge ran as a single process; several Forge processes against one database were not validated.
- The Forge-only runs used a 25 ms synthetic handler; ForgeCart's payment step is a simulator.
- ForgeCart's dispatcher still submits one job per request, so ForgeCart did not use the batch endpoint.
- The load generator was in the same zone: latency from real users is not represented.
- `jobs` rows are never deleted unless the operator sets a retention time (added in session 6, section 20).

## Cost and teardown

About 12.6 machine-hours in total (six machines for about 2.1 hours), which at the third-party list price of about $0.0264 per hour for a t3.small is roughly $0.33, plus disks, public IP addresses and a small amount of "unlimited" CPU-credit surplus; the real figure is in the AWS Billing console. At the end, all six instances were terminated and the firewall and key pair deleted; read-only checks showed no instances, volumes, Elastic IPs, network interfaces, snapshots, security group or key pair remaining. The IAM user `forge-deployer` and its access key still exist (this account's permissions cannot delete them): delete the access key or the user in the IAM console.
