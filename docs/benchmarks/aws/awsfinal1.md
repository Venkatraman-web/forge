# ForgeCart on Forge, attached run `awsfinal1`

Started 2026-10-22 20:49:46 UTC, ended 20:50:48 UTC. Raw data: `remote-cart.json`.

## What this is

Simulated shoppers (create an order, pay it) use an **already running** ForgeCart; each paid order produces five background jobs that flow through the Forge it is connected to. The **foreground** figures are the shoppers' requests; the **background** figures are how fast Forge and ForgeCart's workers finish the jobs after the load ends. Nothing was started, stopped or configured by this tool: payment outcomes, worker counts and Forge settings are whatever the deployment has. Between trials the tool waited for Forge to go idle and **emptied ForgeCart's business tables** in the database `172.31.0.85:5432/forgecart`, then reset stock.

| Setting | Value |
|---|---|
| ForgeCart | http://172.31.2.34:8090 |
| Forge (submission / operator) | http://172.31.4.71:8080 / http://172.31.4.71:8082 |
| Shopper concurrency levels | [10 40 160], 600 workflows per trial, 2 repetition(s), order rotated each repetition |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description, unverified) | AWS ap-southeast-2a, 6x t3.small: forge, forge-db, cart-web (API+dispatcher, single-job submits), cart-workers (5 workers x 4 slots), cart-db, loadgen; FORGE_POLL_INTERVAL=200ms, sharded queue counter; payment simulator 100% success; session 4 (final ForgeCart run) |

## Results

| Trial | Foreground: workflows/s | Workflow p50 / p95 / p99 (ms) | Failed workflows | Background drain after load | Forge queue wait p50 / p95 (ms) | Forge execution p50 / p95 (ms) | Integrity checks |
|---|---|---|---|---|---|---|---|
| shoppers-10-r1 | 126.7 | 64.6 / 177.2 / 256.2 | 0 | 2.6 s (1153 jobs/s) | 93 / 224 | 14 / 30 | 28 of 28 pass |
| shoppers-40-r1 | 94.4 | 331.5 / 994.4 / 1523.8 | 0 | 2.9 s (1025 jobs/s) | 89 / 375 | 20 / 90 | 28 of 28 pass |
| shoppers-160-r1 | 140.7 | 956.8 / 1967.3 / 2638.2 | 0 | 3.5 s (868 jobs/s) | 80 / 187 | 15 / 29 | 28 of 28 pass |
| shoppers-40-r2 | 130.0 | 284.5 / 528.6 / 686.4 | 0 | 2.9 s (1029 jobs/s) | 84 / 189 | 15 / 27 | 28 of 28 pass |
| shoppers-160-r2 | 137.6 | 982.0 / 2164.9 / 2601.6 | 0 | 3.4 s (880 jobs/s) | 81 / 189 | 14 / 29 | 28 of 28 pass |
| shoppers-10-r2 | 135.6 | 64.0 / 147.7 / 177.5 | 0 | 2.9 s (1027 jobs/s) | 84 / 188 | 15 / 26 | 28 of 28 pass |

Queue wait and execution come from Forge's own job timestamps for the jobs the observer saw (median of the per-type medians; the larger of the per-type p95s). Forge's `/metrics` view of the same trials:

| Trial | Forge counter changes | Queue wait p50 / p95 (ms, bucket estimate) | Attempt duration p50 / p95 (ms, bucket estimate) |
|---|---|---|---|
| shoppers-10-r1 | events:claimed=3000, events:succeeded=3000, submissions:accepted=3000 | 79 / 448 | 27 / 48 |
| shoppers-40-r1 | events:claimed=3000, events:succeeded=3000, submissions:accepted=3000 | 78 / 451 | 31 / 86 |
| shoppers-160-r1 | events:claimed=3000, events:succeeded=3000, submissions:accepted=3000 | 74 / 441 | 27 / 48 |
| shoppers-40-r2 | events:claimed=3000, events:succeeded=3000, submissions:accepted=3000 | 74 / 441 | 28 / 48 |
| shoppers-160-r2 | events:claimed=3000, events:succeeded=3000, submissions:accepted=3000 | 70 / 440 | 26 / 48 |
| shoppers-10-r2 | events:claimed=3000, events:succeeded=3000, submissions:accepted=3000 | 73 / 441 | 28 / 48 |

## Limits of this measurement

- Foreground throughput is limited by ForgeCart itself (its web server, its database, its payment step) as well as by the load generator; it is not a Forge measurement. Forge's part is the background columns.
- Worker logs, process CPU and container figures are not available for a deployment this tool does not manage, so the retry counts and resource tables of the local report are absent.
- Integrity checks read the ForgeCart database only; they prove ForgeCart's own invariants (stock, payments, one event per job type, one effect per event), not Forge's internals.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
