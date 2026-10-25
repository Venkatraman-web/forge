# ForgeCart / Forge load-testing suite

A scenario-based suite that exercises ForgeCart (checkout, payments, transactional outbox), the dispatcher, Forge's job API, PostgreSQL and all four worker types, then writes `report.md`.

## Quick start (from anywhere inside the repository)

```bash
./forgecart/test_files/run.sh list
./forgecart/test_files/run.sh preflight          # needs go, docker, docker compose, atlas
./forgecart/test_files/run.sh dry-run --profile standard
./forgecart/test_files/run.sh run                # default: smoke profile (about 15 s)
./forgecart/test_files/run.sh run --profile standard   # about 2 min
./forgecart/test_files/run.sh run --profile full --allow-fault-injection   # about 20 min
# regenerate a report from a preserved run with the current reporting code (reads only; refuses to
# overwrite a file or to write inside results/):
./forgecart/test_files/run.sh report --from final003 --out /tmp/final003-regenerated.md
```

## Profiles and scenarios

| Profile | Scenarios |
|---|---|
| `smoke` (default) | smoke |
| `standard` | smoke, baseline, ramp, stock-contention, pipeline |
| `full` / `all` | all twelve |

Scenarios (definitions in `scenarios/*.json`, selectable with `--scenarios a,b`): smoke, baseline (3 repetitions, median and range), ramp, burst, soak, saturation, stock-contention, payment-mix, idempotency, pipeline, worker-scaling, recovery.

Flags: `--profile`, `--scenarios`, `--scale` (0.1-4, multiplies workflow counts and durations), `--soak-seconds` (max 1800), `--seed`, `--race` (build services with the race detector), `--allow-fault-injection` (required for `recovery`; otherwise it is reported as skipped), `--run-id`.

Hard limits (reported in the report when they reduce a request): concurrency 500, 50,000 workflows, 1,800 s per timed phase.

## What happens in a run

1. Preflight, then the services are built into `results/<run-id>/bin` (deleted afterwards).
2. A Compose project `forgecart-lt-<run-id>` starts two labelled `postgres:16-alpine` containers (tmpfs, CPU and memory limits, loopback ports, dedicated network): one Forge database, one ForgeCart database. Every destructive step first verifies the container names, labels and run id (`assertOwned`).
3. For each trial both databases are dropped and recreated, the repository's existing Atlas migrations are applied, the catalog is seeded with a fixed five-product seed, inventory is set as the scenario requires, and Forge, ForgeCart, the dispatcher and the workers are started fresh.
4. The load runs closed-loop over the public API; Forge jobs are observed through `GET /v1/jobs`; the ForgeCart database is read (read-only, outside the request path, with its query cost reported) to measure drain and verify integrity; processes and containers are sampled for CPU, memory and connections.
5. The sandbox is torn down on completion, error or Ctrl-C (SIGINT/SIGTERM). Only the containers and network of this run's project are removed.

## Outputs

- `report.md`: the 20-section report of the latest complete run. A complete run archives the previous `report.md` to `results/archive/` first.
- `results/<run-id>/`: `suite.json`, `environment.json`, one JSON file per scenario, `report.md` (copy) and `logs/` (service logs; ForgeCart and Forge do not log payloads or secrets). Run directories are never overwritten.
- An interrupted or incomplete run writes `report-partial.md` (clearly marked) and leaves the previous full `report.md` untouched.
- `results/archive/`: earlier reports, including the output of the retired load generator.

## Metric names (units and measurement intervals are part of the name)

| Name | Definition |
|---|---|
| Workflow throughput (workflows/s) | completed workflows / HTTP load duration. Foreground only. |
| Drain after load (s) | time until every paid order has its effects, from the end of the HTTP load (in the worker-scaling scenario: from the moment the workers were started). Polled every 250 ms, so a 3-5 s drain is only good to about +/-5-9%. |
| Background drain rate (jobs/s) | 5 x paid orders / drain seconds. |
| Pipeline span rate (jobs/s) | effects / (first outbox event -> last effect). Includes the HTTP load; not a capacity measure. |
| Backlog (jobs) | 5 x paid orders - effects, sampled every second. A proxy: Forge's queue depth is not exposed. |

Windows: a soak run is cut into fixed windows. A final window shorter than the nominal length (for example the workflows still in flight when the load stops) is reported with its real duration, labelled "partial", and is never used in first-versus-last comparisons. Host CPU averages exclude the first sample of a trial (it has no earlier reading to difference against). Container CPU from `docker stats` (read about every 5 s) is marked `*` when it rests on fewer than 3 readings.

## Seeds

Seeds fix the *offered requests* (customer emails, product choices, quantities, idempotency keys). The payment outcome is a pure function of the payment seed, the order ID and the key, and order IDs are random, so the *realised* payment mix, the number of paid orders and the number of background jobs differ slightly between trials with the same seed. Reports print the configured mix next to the realised counts.

## Run metadata

`environment.json` / the report's section 2 record the host boot time and uptime, load average at the start, CPU scaling governor, container engine and VM size (Docker Desktop runs in a VM whose CPU count and memory bound the containers), a fingerprint of the source tree and the hash and build flags of every built binary. A field that cannot be read is recorded as `unavailable (reason)`; results recorded before a field existed show it as unavailable.

## Acceptance criteria

Correctness criteria (no unexpected HTTP failures, exact payment/stock/outbox accounting, one effect per worker per paid order, ~28 integrity checks) are fixed. Performance objectives (`objectives` in each scenario JSON) are `null` = **unspecified**: the report shows the measurement and marks it unspecified instead of passing it. Set a number in the JSON to turn it into a pass/fail criterion.

## Deviations and limits worth knowing

- Services run as **host processes**, not containers: the repository has no Dockerfiles. Everything shares one host, so results characterize this machine only.
- Forge's public API lists only the newest 100 jobs, has no filter or pagination and no attempt counts. The suite reports observed-vs-expected job coverage and takes retry counts from the workers' own logs. These are missing capabilities, not worked around by reading Forge's tables.
- Closed-loop load only; no open-loop arrival-rate scenario yet.
- Execution is at-least-once; the suite never claims exactly-once.
- The fault injection (process SIGTERM/SIGKILL, `docker pause` of the sandbox ForgeCart database) runs only in the sandbox and only with `--allow-fault-injection`.
- No `_test.go` files: the suite validates the product by running it.

## Running against an existing deployment (`remote`)

`remote` runs the E-1 open-loop load against a Forge that is **already running**, for example on separate machines, instead of building a local sandbox. It reads only Forge's HTTP APIs and its `/metrics`; it never touches Forge's database or any process, and it never records the keys.

```bash
export FORGE_API_KEY=...            # Forge's submit key
export FORGE_OPERATOR_API_KEY=...   # the operator key (for /v1/queues, /v1/config and /metrics)
./run.sh remote --run-id aws001 --forge-url http://FORGE:8080 --operator-url http://FORGE:8082 \
  --types bench.a,bench.b --rates 100,300,600 --stage-seconds 60 --repetitions 2 \
  --note "Forge: t3.small, DB: t3.small, workers and load generator on separate instances, eu-west-1a"
```

- **Workers are started separately.** Build `synthworker` (`go build ./forgecart/test_files/synthworker`) and run it where you want the workers, with the `SW_*` settings described at the top of `synthworker/main.go` (it handles the `--types` you pass). The run does not start, stop or count them.
- **How numbers are obtained.** Submission latency and accepted rate come from the generator. Everything server-side is the difference between two `/metrics` scrapes (before the stage, at the end of the load, after the drain): completed jobs, retries, the admission and queue-full counters, and queue-wait and attempt-duration quantiles **estimated from histogram buckets** (coarser than the database-timestamp figures of the local E-1 reports). The queue state is polled once a second from `/v1/queues`.
- **Stages are separated by draining**, not by resetting a database: a stage refuses to start while jobs are queued or running. The counters are per Forge process, so use it against **one** Forge process; a restart during a stage fails that stage.
- **Same rule for "sustainable"** as E-1 (all offered requests accepted, generator on schedule, backlog not growing, queue drained, estimated p95 queue wait at most 5 s).
- **Output:** `results/<run-id>/remote.json` and `remote-report.md`, which includes the `--note` text (unverified, as typed) and the load generator host.
- **Validated** against a local Forge with a real PostgreSQL and `synthworker`: counters matched the generator exactly (accepted and succeeded), missing keys and flags, a wrong operator key, a non-empty queue before a stage and a Forge restart mid-stage were each rejected clearly, and no key appeared in any output. Not yet run against a remote deployment.

## ForgeCart against an existing deployment (`remote-cart`)

`remote-cart` runs ForgeCart's shopper workflows (create an order, pay it, then the background jobs flow through Forge) against a ForgeCart + Forge that are **already running**, using the normal trial machinery in an *attach mode* (`attach.go`). It reports the foreground (shoppers' requests) and the background (how fast Forge and ForgeCart's workers finish the jobs after the load) separately, runs ForgeCart's integrity checks on its database, and adds Forge's own `/metrics` deltas.

```bash
export FORGE_API_KEY=... FORGE_OPERATOR_API_KEY=...
export FORGECART_ATTACH_APP_DB_URL='postgres://USER:PASSWORD@CART-DB-HOST:5432/forgecart?sslmode=require'
# The payment service's own database (its attempts are emptied between trials too, and the payment checks read it):
export FORGECART_ATTACH_PAYMENTS_DB_URL='postgres://USER:PASSWORD@PAYMENTS-DB-HOST:5432/payments?sslmode=require'
# Only if the workers write their results to a separate database (FORGECART_RESULTS_DATABASE_URL):
# export FORGECART_ATTACH_RESULTS_DB_URL='postgres://USER:PASSWORD@RESULTS-DB-HOST:5432/forgecart?sslmode=require'
./run.sh remote-cart --erase-forgecart-tables --run-id aws002 \
  --cart-url http://CART:8090 --forge-url http://FORGE:8080 --operator-url http://FORGE:8082 \
  --levels 5,10,20,40 --workflows 400 --repetitions 2 --note "what the deployment is"
```

- **It erases data.** Between trials the tool waits for Forge to go idle (it refuses to continue if jobs are still queued after 180 s), then **empties ForgeCart's business tables** (and the results database's result tables, if `FORGECART_ATTACH_RESULTS_DB_URL` is set) (orders, payments, outbox, invoices, fulfilments, notifications, reconciliations) in the database named by `FORGECART_ATTACH_APP_DB_URL`, and resets stock. The catalogue (`products`) is kept. It will not run without `--erase-forgecart-tables`, and it logs which database host it will erase (never the password). Point it only at a database that holds nothing you need. Forge's own database is never touched.
- **Nothing is started, stopped or configured.** Payment outcomes, worker counts and Forge settings are whatever the deployment has, so scenarios that need those (payment mix, worker scaling, fault injection) are not available in this mode.
- **Not available without a managed deployment:** worker log statistics (retry counts), process CPU and container figures. The report says so.
- **Forge job timing** in the table comes from Forge's own job timestamps for the jobs created during the trial (jobs already listed when the trial starts are ignored, because Forge's job table is not reset); the second table is Forge's `/metrics` view of the same trial.
- **Validated** locally against a real Forge, PostgreSQL, ForgeCart API, dispatcher and four workers started separately: 60 workflows per trial all completed and drained, ForgeCart's integrity checks passed, Forge's counters moved by exactly the expected 300 accepted and 300 succeeded jobs, the tables were emptied between trials, the safety flag, the missing database URL and a busy Forge were each refused clearly, and no key or password appeared in any output. Not yet run against a remote deployment.

## Experiment E-1 — Forge-isolated capacity (`e1`)

`go run . e1` measures Forge alone (docs/decisions/0004 §8.2). ForgeCart, its dispatcher and its outbox are not used. An open-loop generator posts directly to `POST /v1/jobs` at a fixed arrival rate; Forge runs in remote mode with SDK workers (`synthworker/`, a fixed-duration no-I/O handler); queue state is read from the authenticated operator endpoint `GET /v1/queues` once per second. Only a PostgreSQL container is started (project `forgecart-lt-<run-id>`, disposable, RAM-backed); `forge-postgres-1` is never touched.

Flags: `--rates 25,50,...` `--stage-seconds` `--sustained-seconds` (0 = skip) `--handler-ms` `--payload-bytes` `--worker-procs` `--slots` `--max-queued` `--run-id` and the opt-in `--diagnostics` (PostgreSQL wait events, locks, dead tuples, index scan counters). Hard limits: rate ≤ 5000/s, stage ≤ 900 s, sustained ≤ 1800 s, ≤ 64 worker slots.

Outputs: `results/<run-id>/e1.json` (raw), `e1-report.md` (generated), `logs/`. The canonical `report.md` is never modified. Each stage runs on a freshly migrated database. Submission, completion-during-load and drain throughput are reported separately; the sustainable/not classification and the sustained-stage rate rule (0.8 × highest sustainable sweep rate) are this experiment's own assumptions, not SLOs.

Limits: single host, shared CPU, tmpfs database (no disk I/O cost), one handler duration and payload size, one repetition per rate. The numbers are not a production capacity figure.

### E-1 follow-up options (opt-in; the default `e1` behaviour is unchanged)

- `--repetitions N` runs every rate N times, interleaved: repetition *k* rotates the rate order by *k* places (a Latin square when N equals the number of rates). Stage labels get an `-rK` suffix.
- `--no-workers` is the admission-only probe: Forge runs in remote mode but no worker process starts, so nothing is claimed and the queue only grows. Stages are labelled `admission-<rate>`, are not classified sustainable/unsustainable, and the integrity checks expect every accepted job to remain `queued`. Set `--max-queued` above rate × seconds so the probe measures admission cost rather than `queue_full`.
- `--watchdog` stops *sending* (the trial is still drained, analysed and marked "stopped early") if (1) the generator's in-flight bound is reached 1000 times, (2) after 60 s at least half of the last 30 s of responses were not accepted, or (3) Forge's resident memory exceeds 1.5 GB. An aborted trial fails the "every planned request was sent" check by design.
- Reports add a per-30 s submission table beside the operator queue depth, and normalised Forge/worker log-line counts. Run on a host that cannot suspend (a suspend mid-trial invalidates it: wall-clock job timestamps keep advancing while the harness's monotonic clock does not).
