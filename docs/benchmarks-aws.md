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

## Limits of these sessions

- A handful of sessions on one availability zone and small burstable machines; one to three repetitions per level, and the batch sweeps one repetition each. Quantiles from `/metrics` are bucket estimates.
- Forge ran as a single process; several Forge processes against one database were not validated.
- The Forge-only runs used a 25 ms synthetic handler; ForgeCart's payment step is a simulator.
- ForgeCart's dispatcher still submits one job per request, so ForgeCart did not use the batch endpoint.
- The load generator was in the same zone: latency from real users is not represented.
- `jobs` rows are never deleted unless the operator sets a retention time (added in session 6, section 20).

## Cost and teardown

About 12.6 machine-hours in total (six machines for about 2.1 hours), which at the third-party list price of about $0.0264 per hour for a t3.small is roughly $0.33, plus disks, public IP addresses and a small amount of "unlimited" CPU-credit surplus; the real figure is in the AWS Billing console. At the end, all six instances were terminated and the firewall and key pair deleted; read-only checks showed no instances, volumes, Elastic IPs, network interfaces, snapshots, security group or key pair remaining. The IAM user `forge-deployer` and its access key still exist (this account's permissions cannot delete them): delete the access key or the user in the IAM console.
