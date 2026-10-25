# Forge benchmarks (local, single host)

**Status: local measurements only.** Everything here ran on one laptop in Docker Desktop, where the load generator, Forge, the workers and PostgreSQL share the same 12 logical CPUs and the database is RAM-backed (tmpfs). The numbers show how Forge behaves relative to itself on this machine. They are **not** a capacity claim for Forge on dedicated hardware, and they say nothing about a deployment with a network between Forge and its database. A deployed measurement is planned (see "Next"). The first two sections describe the code before the admission fix; the third describes the code after it.

## What was measured

Experiment **E-1** ("Forge-isolated capacity"): jobs are submitted straight to `POST /v1/jobs` by an **open-loop** generator (a fixed arrival rate, whatever Forge does), and executed by Forge SDK workers running a synthetic 25 ms handler (no I/O). No ForgeCart code is involved, so the result is Forge's own submit-to-complete path.

| Setting | Value |
|---|---|
| Source | the repository at the commit that contains this file (binaries and source fingerprint are in the linked reports) |
| Host | AMD Ryzen 5 220, 12 logical CPUs, 14.5 GB RAM, Docker Desktop VM (12 CPUs), CPU governor `powersave` |
| PostgreSQL | 16.15 in one container, 4 CPUs / 2 GB, **tmpfs storage (no disk I/O cost)**, Forge's pool fixed at 10 connections |
| Workers | 2 processes x 24 slots = 48 slots, handler 25 ms fixed wait, two job types |
| Queue cap | `FORGE_MAX_QUEUED=200000` (the default is 1000), so overload shows as errors, not rejection |
| Stage | 60 s at a fixed offered rate; the database is dropped and recreated and all processes restart before every stage |
| Repetitions | 3 per rate for 50 to 300 jobs/s, 2 per rate for 400 to 650 jobs/s |
| Stage rule (written before the run) | *sustainable* = every request accepted without error, generator on schedule, backlog not growing |

Full tables, definitions, environment and binary hashes: [`benchmarks/e1-benchpost1.md`](benchmarks/e1-benchpost1.md) (50 to 300 jobs/s, run `benchpost1`) and [`benchmarks/e1-benchpost2.md`](benchmarks/e1-benchpost2.md) (400 to 650 jobs/s, run `benchpost2`). The raw JSON is not committed (it is large); both runs are reproducible with `forgecart/test_files/run.sh e1 --rates ... --stage-seconds 60 --sustained-seconds 0 --repetitions N --max-queued 200000`.

## Results

| Offered (jobs/s) | Stages sustainable | Accepted (jobs/s), per stage | Submit latency p50 / p99 (ms) | Queue wait p50 / p95 (ms), last half | Errors |
|---|---|---|---|---|---|
| 50 | 3 of 3 | 50.0 | 3.3-3.4 / 5.1-5.7 | 140 / 274 | none |
| 100 | 3 of 3 | 100.0 | 3.0 / 4.8-5.5 | 130-139 / 265-270 | none |
| 200 | 3 of 3 | 200.0 | 2.6 / 5.2-7.3 | 51-56 / 113-150 | none |
| 300 | 3 of 3 | 300.0 | 2.5 / 18-65 | 26-42 / 158-200 | none |
| 400 | **1 of 2** | 399.8 and 347.8 | 2.9 and 397 / 116 and 527 | 201 and 568 / 392 and 953 | 0 and 3,130 `503` |
| 500 | 0 of 2 | 467.2 and 439.1 | about 480-500 / about 515 | about 740 / 930-980 | 1,728 and 3,450 `503` |
| 650 | 0 of 2 | 445.4 and 424.0 | about 505 / about 515 | about 770 / 980-1050 | 12,054 and 13,345 `503` |

- **Up to 300 jobs/s, in every repetition**, every job was accepted, completed during the load (99.6% or more by the end of the load window) and the backlog drained in under a second. Execution stayed near the 25 ms handler time.
- **Around 400 jobs/s is the edge on this machine**: one repetition held, one did not. That spread between repetitions is itself a result; treat 300 jobs/s as the level that held in all runs and 400 as unreliable.
- **Above that, accepted throughput stopped rising** (about 420 to 470 jobs/s whatever was offered) and the extra submissions were refused with `503` after waiting about 500 ms. These `503`s are Forge's bounded admission (`admission_busy`) doing its job, not crashes: nothing was stored for them.
- The best single accepted rate seen was 467 jobs/s. That is a peak of one stage, not a sustained capacity.

## After the admission fix (run `benchpost3`)

The first sweep pointed at the submission step. Measuring it in isolation (a scratch micro-benchmark, three concurrent submitters, the same PostgreSQL) showed why: the lock was held across about a dozen database round trips (begin, lock, count, insert, commit, plus statement preparation), so any latency to the database was paid many times inside the lock.

| Scratch micro-benchmark, accepted submissions/s | Before | After (one database call) |
|---|---|---|
| Local database, submissions only | 660 | 2,653 |
| Local database, 8 claim loops running at the same time | 176 | 786 |
| 0.5 ms one-way delay to the database (1 ms round trip) | 111 | 1,162 |
| 2 ms one-way delay (4 ms round trip) | 64 | 452 |

The fix moves the lock, the count and the insert into one database function, `forge_submit_job`, called as a single statement. The queue limit stayed exact: 5 repeated races of 300 concurrent submitters at limit 50 each ended with exactly 50 queued rows and a counter of 50, and limits of 1 and 500 were also exact. The delay was added by a small TCP proxy on the same machine, which only approximates a real network.

Rerunning the sweep ([`benchmarks/e1-benchpost3.md`](benchmarks/e1-benchpost3.md), 60 s stages, 2 repetitions per rate, same settings as above):

| Offered (jobs/s) | Stages sustainable | Accepted (jobs/s) | Submit p50 / p99 (ms) | Queue wait p50 / p95 (ms), last half | Errors |
|---|---|---|---|---|---|
| 400 | 2 of 2 | 400.0 | 1.0-1.3 / 3.1-4.0 | 15-20 / 51-59 | none |
| 600 | 2 of 2 | 600.0 | 1.1 / 11-18 | 16-26 / 83-153 | none |
| 800 | 2 of 2 | 800.0 | 1.3-1.5 / 76-98 | 17-32 / 198-224 | none |
| 1000 | **1 of 2** | 996.3 and 1000.0 | 9.2 and 1.9 / 427 and 412 | 310 and 127 / 699 and 733 | 224 `503` in one stage (of 60,000 requests), none in the other |

Before the fix, 400 jobs/s held in 1 of 2 stages and 500 or more failed in every stage. After it, **800 jobs/s held in both repetitions and 1000 jobs/s held in one**. Submit latency at 400 jobs/s fell from about 3 ms to about 1 ms (p50), and queue wait at 400 jobs/s was 15-20 ms (p50), where it had been 130-200 ms at much lower rates; the earlier, larger queue wait was most likely time spent waiting for the admission lock (a job's creation time is stamped before it waits for the lock), not the workers' poll interval, which I first suspected; I have not isolated the two. 1000 jobs/s is the edge on this machine. Workers (48 slots, 25 ms handler) have a ceiling of roughly 1,900 jobs/s, so the remaining limit at 1000 jobs/s is not the number of worker slots; I have not yet identified what it is (candidates include the database CPU shared with the generator, the single counter row that claims also update, and the pool of 10 database connections), and have not measured them.

## What this does and does not show

- It showed the **submission path** (admission, the global queue limit and the insert) was the first thing to limit throughput on this setup before the fix; workers kept up (backlog drained in under a second at every level). The lock hold time was then measured (see above) and shortened.
- **Not shown:** durability costs (tmpfs hides disk I/O), network latency to the database, behaviour over hours, several Forge processes against one database (unvalidated), larger payloads, handlers that do real I/O, or any claim about "users". 60 s stages with 2 to 3 repetitions are enough to compare configurations here, not to publish confidence intervals.

## ForgeCart (the reference application)

The ForgeCart regression in `forgecart/test_files/report.md` measures the whole application, with the same single-host caveats. Its foreground throughput (about 130 workflows/s, limited by the ForgeCart HTTP path and its simulated payment step) and its background drain (hundreds of jobs/s) are different quantities from the open-loop submission figures above and must not be compared with them. In the latest run, execution of each ForgeCart job took about 25 to 50 ms and no job retried.

## Next

1. **Done, see `benchmarks-aws.md`:** the same sweep on separate machines with a disk-backed database. Starting ceiling about 335 jobs/s (about 200 sustained), set by the durable commit held inside the admission lock; the laptop figures above do not carry over to a durable disk. After the batch endpoint and a sharded queue counter, 1,200 jobs/s were sustained with workers claiming and 2,000 jobs/s were accepted with no rejections.
2. Not done: a larger database instance (the Forge database CPU is now the limit), batching in ForgeCart's dispatcher, job retention, and several Forge processes against one database.
