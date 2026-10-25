# Project map: what every folder and file is for

Two things live in this repository, and keeping them apart is the key to understanding it:

- **Forge** is the product: a background-job server plus the Go libraries (the SDK) that applications
  and workers use to talk to it. Its code is in `cmd/`, `internal/`, `sdk/` and `migrations/`.
- **ForgeCart** is a sample online shop built *on top of* Forge, used to prove Forge works under a
  realistic load. Its code is all inside `forgecart/`. Forge does not depend on it; delete `forgecart/`
  and Forge still builds and runs.

```text
forge/
├── cmd/forge/               the Forge server program (what the Docker image runs)
├── internal/                the server's inside parts (other projects cannot import these)
├── sdk/                     the public Go libraries customers import
├── migrations/              the database schema, as numbered SQL files
├── examples/hello/          the smallest complete app: one worker + one submitter
├── forgecart/               the sample shop, its services and its load-test tool
├── observability/           Prometheus + Grafana setup (optional dashboards)
├── docs/                    all documentation (see below)
├── .github/workflows/       automatic checks (CI) that run on GitHub
├── Dockerfile               builds the Forge server image
├── compose.yaml             starts PostgreSQL (+ Forge, + dashboards) with Docker
├── atlas.hcl                tells the Atlas tool how to generate schema migrations
├── .env.example             the template for your local settings (copy to .env)
├── go.mod / go.sum          the Go module and its pinned dependencies
└── README.md                the short introduction
```

## 1. The Forge server: `cmd/forge` and `internal/`

A job's life: **API** accepts it → **service** validates and stores it → a **worker** claims it through the
**worker API** → the worker reports back → the **reaper** rescues jobs whose worker died.

| Path | What it does |
|---|---|
| `cmd/forge/main.go` | Starts everything: reads settings from environment variables, connects to PostgreSQL, builds the parts below, runs the three web listeners, shuts down cleanly. |
| `cmd/forge/keys.go` | Works out which keys each of the three APIs accepts (single-key variables or a key file) and refuses unsafe combinations. |
| `internal/api/` | The **submission API** (port 8080): `POST /v1/jobs`, batch submit, read a job, list. Only HTTP concerns: parsing, status codes, JSON. |
| `internal/workerapi/` | The **worker API** (port 8081): claim, renew lease, complete, fail, release. This is how customers' workers get jobs. |
| `internal/operatorapi/` | The **operator API** (port 8082, optional): read-only queue sizes, effective config, Prometheus metrics. |
| `internal/worker/` | The server-side job logic: `service.go` validates a submission and stores it (with the queue limit); `reaper.go` requeues jobs whose worker stopped renewing; `retention.go` deletes old finished jobs; `retry.go` is the retry back-off; `safelog.go` turns database errors into short, safe log text. |
| `internal/store/` | **All database access**: `store.go` is the interface and error values, `postgres.go` is the PostgreSQL version (claiming with `FOR UPDATE SKIP LOCKED`, fenced writes). |
| `internal/jobs/` | The job data type and its states (`queued`, `running`, `succeeded`, `failed`). |
| `internal/auth/` | Bearer-key checking: `auth.go` one key, `keyset.go` several named keys and the key file. |
| `internal/policy/` | The optional JSON file that declares job types and their retry/timeout defaults. |
| `internal/metrics/` | The numbers exposed to Prometheus (queue depth, job counts, timings). |
| `migrations/` | The database tables, as SQL files applied in order by Atlas. `atlas.sum` is Atlas's checksum of them; never edit an applied file. |
| `atlas.hcl` | Atlas settings (used to *generate* migrations from the Go model). |

## 2. The SDK: `sdk/` (what customers import)

| Path | What it does |
|---|---|
| `sdk/forgeclient/` | For the **application**: submit jobs (one or many, with an idempotency key) and read their status. |
| `sdk/workerruntime/` | For **workers**: you register a function per job type; it claims jobs, runs your function with a timeout, keeps the lease alive, reports the result, retries, and shuts down cleanly. Most customers only use this. |
| `sdk/workerclient/` | The low-level worker-API client that `workerruntime` is built on. Use it only if you want to write your own runtime. |
| `examples/hello/` | Both halves in one file: `go run ./examples/hello worker` and `... submit Ada`. |

## 3. ForgeCart: `forgecart/` (the sample shop and its test tool)

| Path | What it does |
|---|---|
| `forgecart/cmd/forgecart/` | The **shop service**: catalog, checkout, payment calls, order expiry. |
| `forgecart/cmd/forgecart-payments/` | The separate **payment service** (own database): a simulator, and optionally Razorpay. |
| `forgecart/cmd/forgecart-dispatcher/` | Reads the shop's **outbox** table and submits the follow-up jobs to Forge. |
| `forgecart/cmd/forgecart-{inventory,invoice,fulfillment,notification}-worker/` | The four **workers**: one per kind of background job, each a few lines around the code below. |
| `forgecart/internal/orders/` | Checkout: validate the request, lock products, reserve stock, create the order. |
| `forgecart/internal/payments/` | The shop's side of payment: calling the payment service, the sweeper that repairs half-finished payments, expiry of unpaid orders. |
| `forgecart/internal/paymentsvc/` | The payment service itself: providers (simulator, Razorpay), webhook, HTTP server. |
| `forgecart/internal/outbox/` | The outbox table logic (write an event with the order, claim events safely). |
| `forgecart/internal/dispatcher/` | The loop that turns outbox events into Forge jobs. |
| `forgecart/internal/{reconcile,invoice,fulfillment,notification}/` | What each background job actually does. |
| `forgecart/internal/bizjob/`, `workerapp/` | Shared parts of the workers (reading the order, writing results once). |
| `forgecart/internal/{catalog,config,appdb,httpapi,logging,testdb}/` | Catalog reads, settings, database connection, the shop's web handlers, log level, test helper. |
| `forgecart/migrations/`, `payments_migrations/` | Tables of the shop database and of the payment service database. |
| `forgecart/forge.json` | The job types and retry/timeouts ForgeCart asks Forge to use. |
| `forgecart/test_files/` | The **load-test tool**, a Go program. `main.go` is the entry, `run.sh` a wrapper, `scenarios/*.json` the test scenarios, `engine.go` the load generator, `remote_cart.go` the shopper test used on AWS, `remote.go` the Forge-only test, `report.go` writes `report.md`, `sandbox.go` starts a throwaway local environment, `synthworker/` is a fake worker for Forge-only tests. |

## 4. Documentation: `docs/`

| File | Read it for |
|---|---|
| `architecture.md` | **Start here.** How it all works, failure cases, measured limits, interview questions. |
| `reference.md` | Every setting and endpoint, the SDKs in detail, metrics, security notes (the old long README). |
| `deploy-on-aws.md` | Running Forge yourself with Docker on AWS. |
| `releasing.md` | Checklist for putting the project on GitHub and publishing the Docker image. |
| `benchmarks-aws.md` | Every measurement on AWS, section by section (the evidence behind the numbers). |
| `benchmarks.md`, `benchmarks/` | Earlier measurements on one laptop; `benchmarks/aws/` holds the raw per-run reports behind `benchmarks-aws.md`. |
| `decisions/` | Design records: why PostgreSQL and no broker (0001), the worker protocol (0002), the worker runtime (0003), developer-controlled settings (0004). |
| `project-overview.md` | A one-page summary. |
| `forge-v1-requirements.md` | The requirements written before the work (a planning document). |

## 5. Other files

| Path | What it is |
|---|---|
| `Dockerfile` | Two stages: compile Forge, then copy only the program into a 34 MB image running as an unprivileged user. |
| `compose.yaml` | PostgreSQL always; with `--profile forge` also the migration step and Forge; with `--profile observability` also Prometheus and Grafana. |
| `.github/workflows/ci.yml` | On every push to GitHub: check formatting, vet and build. |
| `.github/workflows/release.yml` | When you push a version tag: builds the Docker image for amd64 and arm64 and publishes it to GitHub's container registry (never run yet; see `docs/releasing.md`). |
| `observability/` | Prometheus settings (`prometheus.yml`, `targets/`) and the Grafana dashboard and its provisioning. |
| `.dockerignore`, `.gitignore` | Keep build junk and secrets out of the image and out of git. |
| `.env` (not in git) | Your real local settings and passwords. Never committed. |
