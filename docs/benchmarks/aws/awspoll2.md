# ForgeCart on Forge, attached run `awspoll2`

Started 2026-10-22 19:01:23 UTC, ended 19:02:00 UTC. Raw data: `remote-cart.json`.

## What this is

Simulated shoppers (create an order, pay it) use an **already running** ForgeCart; each paid order produces five background jobs that flow through the Forge it is connected to. The **foreground** figures are the shoppers' requests; the **background** figures are how fast Forge and ForgeCart's workers finish the jobs after the load ends. Nothing was started, stopped or configured by this tool: payment outcomes, worker counts and Forge settings are whatever the deployment has. Between trials the tool waited for Forge to go idle and **emptied ForgeCart's business tables** in the database `172.31.0.85:5432/forgecart`, then reset stock.

| Setting | Value |
|---|---|
| ForgeCart | http://172.31.2.34:8090 |
| Forge (submission / operator) | http://172.31.4.71:8080 / http://172.31.4.71:8082 |
| Shopper concurrency levels | [20], 600 workflows per trial, 3 repetition(s), order rotated each repetition |
| Load generator host | Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz, 2 logical CPUs, 1905 MB |
| Deployment (operator's description, unverified) | FORGE_POLL_INTERVAL=200ms |

## Results

| Trial | Foreground: workflows/s | Workflow p50 / p95 / p99 (ms) | Failed workflows | Background drain after load | Forge queue wait p50 / p95 (ms) | Forge execution p50 / p95 (ms) | Integrity checks |
|---|---|---|---|---|---|---|---|
| shoppers-20-r1 | 209.2 | 91.8 / 161.0 / 186.2 | 0 | 6.9 s (434 jobs/s) | 155 / 323 | 14 / 34 | 28 of 28 pass |
| shoppers-20-r2 | 211.4 | 87.9 / 157.4 / 197.6 | 0 | 6.9 s (434 jobs/s) | 173 / 338 | 17 / 41 | 28 of 28 pass |
| shoppers-20-r3 | 204.7 | 89.5 / 173.1 / 219.6 | 0 | 6.9 s (434 jobs/s) | 150 / 317 | 13 / 34 | 28 of 28 pass |

Queue wait and execution come from Forge's own job timestamps for the jobs the observer saw (median of the per-type medians; the larger of the per-type p95s). Forge's `/metrics` view of the same trials:

| Trial | Forge counter changes | Queue wait p50 / p95 (ms, bucket estimate) | Attempt duration p50 / p95 (ms, bucket estimate) |
|---|---|---|---|
| shoppers-20-r1 | events:claimed=3000, events:succeeded=3000, submissions:accepted=3000 | 173 / 467 | 24 / 48 |
| shoppers-20-r2 | events:claimed=3000, events:succeeded=3000, submissions:accepted=3000 | 241 / 474 | 27 / 48 |
| shoppers-20-r3 | events:claimed=3000, events:succeeded=3000, submissions:accepted=3000 | 182 / 468 | 21 / 47 |

## Limits of this measurement

- Foreground throughput is limited by ForgeCart itself (its web server, its database, its payment step) as well as by the load generator; it is not a Forge measurement. Forge's part is the background columns.
- Worker logs, process CPU and container figures are not available for a deployment this tool does not manage, so the retry counts and resource tables of the local report are absent.
- Integrity checks read the ForgeCart database only; they prove ForgeCart's own invariants (stock, payments, one event per job type, one effect per event), not Forge's internals.
- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.
