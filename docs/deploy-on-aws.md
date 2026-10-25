# Run Forge yourself on AWS

This is the path a developer takes to run their own Forge, put their own application on top of it, and
load-test and scale it, the way ForgeCart was tested. Forge is **single-tenant by design**: you run your
own deployment (its own container and its own PostgreSQL), so nobody else's jobs or keys are involved.

Everything below was walked through on a fresh Ubuntu 24.04 machine on 2026-10-25 (Docker installed,
image built, started, healthy; a worker and a submitter ran against it; then with a key file,
including a key rotation). What has **not** been tried by following this document end to end from an empty
AWS account is marked. Prices change: check AWS's pricing page and your free-tier terms first, set a
billing alarm, and stop or terminate machines when you finish. Stopped machines still pay for their disks.

## 1. What you will run

```text
 your application ──submit──▶ ┌──────── Forge machine ────────┐
 (any machine)                │ Forge container  ──▶ PostgreSQL│
 your workers ◀──claim/report─│ ports 8080 / 8081 / 8082       │
 (any machines)               └────────────────────────────────┘
```

Forge runs **no handlers itself**. Your application submits jobs with `sdk/forgeclient`; your workers,
programs you write with `sdk/workerruntime`, claim and run them. `examples/hello` is the smallest
complete example of both halves.

## 2. Launch the machine

- One Ubuntu 24.04 machine is enough to start. A burstable or general-purpose type with 2 vCPUs and 4 GB
  or more is plenty for development. Disk: 20 GB or more. Forge keeps every finished job unless you set
  retention (section 5), and a disk that fills stops the database.
- For **load testing**, run the database on its own machine, and the load generator and your workers on
  other machines. Measurements in this project showed the database machine, not Forge, is the limit
  (`benchmarks-aws.md`, sections 3 and 11), and a load generator on the same machine measures itself.
- **Security group** (the firewall): allow SSH only from your own IP. Allow Forge's ports only from the
  security group of the machines that need them, never from `0.0.0.0/0`:
  8080 from your application, 8081 from your workers, 8082 from your monitoring. Traffic is plain HTTP
  (Forge has no TLS), so keep these ports inside your private network.

## 3. Install and start

```bash
# on the Forge machine
sudo apt-get update && sudo apt-get install -y docker.io docker-compose-v2
sudo usermod -aG docker $USER   # log out and in again, or prefix the commands below with sudo

git clone <your copy of this repository> forge && cd forge   # or copy the source with scp
cp .env.example .env
# In .env: put one password in BOTH POSTGRES_PASSWORD and FORGE_DATABASE_URL, and set three DIFFERENT
# keys. Generate each with: openssl rand -hex 24
#   FORGE_API_KEY (your application submits with it)   FORGE_WORKER_API_KEY (workers)
#   FORGE_OPERATOR_API_KEY (monitoring)
# Set FORGE_REMOTE_JOB_TYPES to the job types your workers handle (default hello.greet).

docker compose --profile forge up -d --build   # PostgreSQL, a one-shot migration, then Forge
docker compose ps                              # forge should become "healthy"
curl http://127.0.0.1:8080/health              # {"status":"ok"}
```

The image is built on the machine (about 34 MB, runs as an unprivileged user). No image registry is
needed; to build once and copy it to other machines use `docker save forge:local | gzip > forge.tgz` and
`docker load < forge.tgz`.

To let your application and workers on **other machines** connect, set `FORGE_BIND` in `.env` to this
machine's private IP (the default, `127.0.0.1`, is this machine only) and restart with the same command.

## 4. Try it, then rotate keys

On any machine with Go, from the repository:

```bash
export FORGE_URL=http://FORGE_PRIVATE_IP:8080 FORGE_WORKER_URL=http://FORGE_PRIVATE_IP:8081
export FORGE_WORKER_API_KEY=...   # the worker key
go run ./examples/hello worker &  # runs the hello.greet handler
export FORGE_API_KEY=...          # the submit key
go run ./examples/hello submit Ada   # prints: succeeded: result={"message":"Hello, Ada!"}
```

**Rotating or revoking a key:** Forge can hold several named keys per role in a key file (`FORGE_KEYS_FILE`, see
[reference.md](reference.md)). The Docker setup in this repository does not mount a key file, to keep it simple. To use
one, add your own volume to the `forge` service in `compose.yaml` and set `FORGE_KEYS_FILE` to the path inside the
container, or run Forge outside Docker.

## 5. Settings you will want

| Setting | Why |
|---|---|
| `FORGE_RETENTION_SUCCEEDED`, `FORGE_RETENTION_FAILED` (for example `24h`, `168h`) | Without them finished jobs are kept forever and the table grows without bound (7.3 million rows filled a disk during benchmarking). |
| `FORGE_MAX_QUEUED` | The most jobs that may wait; beyond it submissions get `503 queue_full`, which your application should treat as "slow down". |
| per-type retry budget and timeout | Declared in a JSON config file (`FORGE_CONFIG_FILE`, see [reference.md](reference.md)). The Docker setup does not yet pass this file through; run Forge from the binary, or extend `compose.yaml`. |
| Backups | Forge's data is the `forge_pgdata` volume. Take regular `pg_dump` backups; a dead disk is otherwise a lost queue. |

## 6. Load test and scale it

Treat this as an experiment, as `benchmarks-aws.md` does:

1. **Separate machines** for Forge+database, your application, your workers and the load generator, in one
   availability zone. A load generator that is busy distorts every number.
2. **Turn on monitoring first:** `docker compose --profile forge --profile observability up -d` starts
   Prometheus and a Grafana dashboard (queue depth and age, submissions by result, job events, queue-wait
   and attempt-duration histograms; ports on 127.0.0.1, reach them by SSH tunnel). Set
   `FORGE_GRAFANA_ADMIN_PASSWORD` in `.env`.
3. **Load Forge alone first.** `forgecart/test_files` holds a load tool; `go build -o tf ./forgecart/test_files`
   then `./tf remote --help`. Its `remote` command drives a running Forge with an open-loop load
   (needs `FORGE_API_KEY` and `FORGE_OPERATOR_API_KEY` in the environment). Find where the rate stops
   rising and which machine is at 100% CPU or disk (CPU graphs, `iostat`).
4. **Then load your application,** as `forgecart/test_files/remote_cart.go` does for ForgeCart: shoppers that
   wait for each reply, a fixed duration, integrity checks at the end (did every job's effect happen
   exactly once?). Repeat each measurement several times and report a range, not one number.
5. **Scale one thing at a time.** More worker processes or slots raise the rate while the workers are the
   limit. Batch submission (`SubmitBatch`) and the SDK's batched claims and completions raise Forge's own
   ceiling. Separate databases for separate data helped ForgeCart. Past that, the database machine's
   CPU and disk set the limit.

What to expect from Forge itself, measured on small 2-vCPU machines (`benchmarks-aws.md`): about 1,200 jobs
per second sustained with batch submission and workers claiming, and about 2,000 completions per second at
best. These are observations on one setup, not guarantees.

## 7. What this does not give you

- **Not multi-tenant.** One deployment per team or application. Any submit key can read every job.
- **Static, role-level keys** (rotatable, but not per user), no rate limiting, and no TLS (plain HTTP). Do not expose the ports to the
  internet; if you must cross an untrusted network, put a TLS-terminating proxy in front.
- **One Forge process** is what has been validated. Running several against one database is untested.
- **No high availability:** one database, one Forge. A machine failure means downtime until you restart it,
  and jobs wait safely in PostgreSQL meanwhile.
- **Not yet verified from an empty AWS account:** creating the machine and security group by following this
  document. The container, the example and the key-file steps were verified on a fresh machine.
