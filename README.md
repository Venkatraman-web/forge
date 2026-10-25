# Forge

**A background-job platform for Go developers, built to be run by you.** Your application submits a job
(a type name and some JSON). Forge stores it safely in PostgreSQL. Workers that *you* write and run claim
the job, do the work, and report back. Forge handles the hard parts: never giving one job to two workers,
recovering jobs when a worker dies, retrying failures, limiting how much can queue up, and showing you
what is happening.

Forge is single-tenant by design: you run your own copy (one container plus one PostgreSQL), so nobody
else's jobs are in it.

> **Status:** a well-tested, measured project, not a production service. It has not carried real users or
> real money. The limits are stated in [docs/architecture.md](docs/architecture.md#8-limits-stated-plainly).

## Try it in five minutes

You need Docker (with Compose) and Go 1.24 or newer.

```bash
cp .env.example .env
# Edit .env: put one password in BOTH POSTGRES_PASSWORD and FORGE_DATABASE_URL, and set
# FORGE_API_KEY, FORGE_WORKER_API_KEY and FORGE_OPERATOR_API_KEY to three different random
# values (for example the output of: openssl rand -hex 24).

docker compose --profile forge up -d --build     # PostgreSQL, the schema, then Forge
curl http://127.0.0.1:8080/health                 # {"status":"ok"}

export FORGE_WORKER_API_KEY=...                   # the same values you put in .env
go run ./examples/hello worker &                  # a worker that handles "hello.greet"
export FORGE_API_KEY=...
go run ./examples/hello submit Ada                # succeeded: result={"message":"Hello, Ada!"}
```

[`examples/hello/main.go`](examples/hello/main.go) is the whole application, worker and submitter, in
about 130 lines. To build your own: submit with [`sdk/forgeclient`](sdk/forgeclient) and write your workers
with [`sdk/workerruntime`](sdk/workerruntime).

## Use the SDK in your own project

You need Go 1.24 or newer and a running Forge (the quickstart above). In your own Go project:

```bash
go get github.com/Venkatraman-web/forge@v0.1.0
```

**1. Submit jobs from your application** with `forgeclient`:

```go
import "github.com/Venkatraman-web/forge/sdk/forgeclient"

client, err := forgeclient.New(forgeclient.Config{
    BaseURL: "http://FORGE_HOST:8080",
    APIKey:  os.Getenv("FORGE_API_KEY"), // the submit key
})
job, err := client.Submit(ctx, forgeclient.SubmitRequest{
    Type:  "email.send",
    Input: map[string]string{"to": "ada@example.com"},
})
// later: client.Get(ctx, job.ID) returns the status and result
// many at once: client.SubmitBatch(ctx, []forgeclient.SubmitRequest{...})
```

**2. Run workers that do the work** with `workerruntime`:

```go
import (
    "github.com/Venkatraman-web/forge/sdk/workerclient"
    "github.com/Venkatraman-web/forge/sdk/workerruntime"
)

wc, err := workerclient.New(workerclient.Config{
    BaseURL: "http://FORGE_HOST:8081",
    APIKey:  os.Getenv("FORGE_WORKER_API_KEY"), // the worker key
})
w, err := workerruntime.New(workerruntime.Config{Client: wc, Name: "mailer", Concurrency: 8})
err = w.Handle("email.send", func(ctx context.Context, job workerruntime.Job) (any, error) {
    var in struct{ To string }
    if err := job.Decode(&in); err != nil {
        return nil, workerruntime.Permanent(err) // a bad input will never get better: do not retry
    }
    return map[string]string{"status": "sent to " + in.To}, nil
})
err = w.Run(ctx) // claims jobs, keeps leases alive, reports results, shuts down cleanly
```

**3. Tell Forge your job types.** Forge rejects types it does not know: list them in
`FORGE_REMOTE_JOB_TYPES` (for example `email.send`) in your `.env`.

Three rules to know:

- A job can run more than once (a worker may crash after doing the work), so a handler must be safe to repeat.
- Return `workerruntime.Permanent(err)` for an error that retrying cannot fix; any other error is retried.
- A job's result can be read by anyone with the submit key, so never put secrets in it.

[`examples/hello`](examples/hello/main.go) is a complete working program, and [docs/reference.md](docs/reference.md)
describes every setting and function.

## What you get

- **At-least-once execution** with leases and heartbeats: a worker that dies loses its job to another
  worker; a slow "zombie" worker's late reports are rejected (fencing).
- **Retries and timeouts** per job, bounded queue with clear backpressure (`503 queue_full`), idempotent
  submission (`Idempotency-Key`), batch submit, optional retention of finished jobs.
- **Three separate keys** (submit / worker / operator), with optional key rotation. Forge speaks plain HTTP: run it on a private network.
- **Metrics** for Prometheus and a ready Grafana dashboard.
- **A Go SDK** for the application side and the worker side.

## A worked example at scale

[`forgecart/`](forgecart/README.md) is an online shop built on Forge (orders, a separate payment service,
an outbox, four background workers) with a load-test tool. On small AWS machines it finished about 550 to
580 complete orders per second (including four background jobs each), and ran 1.3 million orders in an
hour with no failures. Every number, and how it was measured, is in
[docs/benchmarks-aws.md](docs/benchmarks-aws.md).

## Where to read next

| If you want to... | Read |
|---|---|
| understand how it all works and why | [docs/architecture.md](docs/architecture.md) |
| know what every folder and file is for | [docs/project-map.md](docs/project-map.md) |
| run it yourself on AWS with Docker | [docs/deploy-on-aws.md](docs/deploy-on-aws.md) |
| publish the project and the Docker image | [docs/releasing.md](docs/releasing.md) |
| look up a setting, an endpoint or the SDK | [docs/reference.md](docs/reference.md) |
| see the measurements | [docs/benchmarks-aws.md](docs/benchmarks-aws.md) |
| see why it is built this way | [docs/decisions/](docs/decisions/) |
