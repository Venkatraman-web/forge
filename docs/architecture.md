# Architecture: how Forge and ForgeCart work, and why

This document explains the system as a whole: what each part does, why it exists, what happens when
something breaks, and what has and has not been measured. It is written so that the person who built
the project can explain any part of it without notes. Every number comes from a measurement in
[`benchmarks-aws.md`](benchmarks-aws.md) (section numbers are given); every guarantee names the code or
test that enforces it. Where something is planned and not built, it says so.

## 1. The two projects

**Forge** is a background-job platform. An application submits a job (a type name and a JSON input).
Forge stores it. A worker, a program the *customer* writes and runs, asks Forge for jobs of the types
it handles, runs its own code, and reports success or failure. The application can read the status.
Forge never runs customer code itself.

**ForgeCart** is an example customer: an online shop. It exists to prove Forge works under a realistic
workload. It has a web service for orders, a separate payment service, a dispatcher, and four workers
(inventory, invoice, fulfilment, notification). Forge is the reusable product; ForgeCart is the test bed.

Why a shop? A shop has all the hard parts of real systems: money (must not be charged twice), stock
(must not be oversold), and work that happens *after* the customer is told "paid" (invoice, shipping,
email) and must still happen exactly once in effect even if machines crash.

## 2. The picture

```text
 shopper ──▶ nginx ──▶ ForgeCart shop service ──▶ shop database (PostgreSQL)
                          │   │                      products, orders, order_items,
                          │   │                      outbox_events, payment records
                          │   └──HTTP──▶ payment service ──▶ payments database
                          │
              dispatcher(s) read outbox_events ──submit job──▶ Forge ──▶ Forge database
                                                                 ▲        (jobs table = the queue)
        4 ForgeCart workers ◀──claim / renew / complete / fail───┘
        (inventory, invoice, fulfilment, notification)  └──write results──▶ results database

 operator / Prometheus ──▶ Forge operator API (queue depth, config, /metrics)
```

Seven machines were used for the measurements (all the same size, 2 vCPUs): Forge, Forge's database,
the shop service with the payment service and the dispatchers, the shop database, the workers, the
results database (which also holds the payments database), and the load generator.

## 3. Forge in detail

### 3.1 The queue is a table
There is no message broker. The `jobs` table *is* the queue: a job is a row with a status
(`queued`, `running`, `succeeded`, `failed`). This was a decision, not an accident
([`decisions/0001`](decisions/0001-execution-architecture.md)): PostgreSQL already gives durability and
transactions, so one fewer system to run, and the whole job lifecycle can be reasoned about in SQL.
A broker such as RabbitMQ was evaluated on paper and not adopted; it would only be revisited with
measurements showing PostgreSQL is the limit.

### 3.2 Claiming: how two workers never get the same job
A worker's claim is **one SQL statement** that picks the next due job and marks it running for that
worker, using `FOR UPDATE SKIP LOCKED`. In plain words: each worker locks the rows it takes, and other
workers *skip* locked rows instead of waiting, so workers never fight over a job and never block each
other. Workers may take up to 20 jobs per claim (one statement, one round trip).

### 3.3 Leases, heartbeats and the reaper: how a dead worker's job is recovered
A claimed job carries a **lease**: "this job is yours until time T". While it works, the worker's SDK
**renews** the lease on a heartbeat. If the worker crashes or loses the network, renewals stop, the
lease expires, and a background **reaper** in Forge puts the job back in the queue (or fails it with
`attempts_exhausted` if it has used all its attempts).

### 3.4 Fencing: how a "zombie" worker cannot corrupt anything
What if a worker is merely *slow*, its lease expires, a second worker takes the job, and then the first
wakes up and reports a result? Every claim has a number, the **claim generation**, and every write
(renew, complete, fail, release) says "I am generation N". The database only applies the write if the
job is still running under exactly that generation and worker. A stale worker's write matches nothing
and is answered `409 ownership_lost`. The SDK also protects itself: if it cannot renew for longer than
a safety margin, it cancels its own handler and reports nothing (it *self-fences*).
Tested in `internal/workerapi`, `sdk/workerruntime` and against PostgreSQL in `internal/store`.

### 3.5 What "at-least-once" means (and why there is no "exactly once")
Forge guarantees a job runs **at least once**. A crash after the work but before the report means the
job runs again. Nobody can promise "exactly once" across a network, so Forge does not claim it. Instead
handlers must be **idempotent** (running twice has the same effect as once); ForgeCart's workers are
(section 4.5).

### 3.6 Retries, timeouts and backpressure
- A failed attempt is retried with exponential backoff and jitter, up to a retry budget the submitter
  chooses (`options.max_attempts`, default 3). A handler can mark an error permanent.
- Each attempt has a timeout (`options.timeout_seconds`, default 30); the SDK enforces it by cancelling
  the handler. A handler that ignores cancellation cannot be stopped; Forge never releases its job
  while it may still be running (that would run the job twice at once).
- The queue is bounded. When too many jobs are waiting, a submission gets `503 queue_full`; when too
  many submissions arrive at once, `503 admission_busy`. Both store nothing and tell the caller to
  retry later.

### 3.7 Idempotent submission
A submitter can send an `Idempotency-Key`. A repeat with the same key and content returns the first job
instead of creating a second; the same key with different content is rejected (`409`). This is what
makes "my request timed out, did it work?" safe to retry.

### 3.8 Three roles and rotatable keys
Submitting jobs, working jobs and operating Forge each use a different bearer key and a different
network port. Each role can hold **several named keys** (`FORGE_KEYS_FILE`, which stores only SHA-256
digests): to rotate a key you add the new one, move the clients over, then remove the old one, so there
is no moment when clients are locked out; a leaked key is revoked by removing its entry. Key names (never
keys) appear in the startup log and in the worker API's log lines. A key may not serve two roles.
What this is not: a key names a role and a label, not a customer. There are no user accounts. Forge is
**single-tenant by design**: the intended model is one deployment per team or application (for example one
container per developer, each with its own database and keys), so customers are isolated from each other
by running separate deployments. Inside one deployment any submit key can read every job and any worker
can claim any allowed type, so one deployment must not be shared by parties that do not trust each other.
Forge speaks plain HTTP (no TLS): it is meant for a private network, so the keys are not sent over the open internet (see section 8).

## 4. ForgeCart in detail

### 4.1 Checkout: no overselling, no double orders
`Checkout` runs in one database transaction:
1. Insert the order using the customer's idempotency key (a repeat finds the first order and returns it).
2. Lock the product rows with `SELECT ... FOR UPDATE`, **always in SKU order** (so two orders with the
   same two products cannot deadlock each other).
3. Check there is enough stock, price the order, add the lines, subtract stock with a guarded
   `UPDATE ... WHERE inventory_quantity >= n`.

Two customers ordering the last keyboard at once: the second waits on the row lock, then sees the new
stock and is told "insufficient inventory". The database also refuses negative stock by itself
(`CHECK inventory_quantity >= 0`), a third independent layer. Tested by `TestCheckoutNeverOversells`
(40 concurrent shoppers) and by integrity checks after every load run.
A product that is *very* popular makes all its buyers queue on one row lock; measured (200 shoppers, one product): about 300 orders per second instead of about 495, and exactly the available stock sold when it ran out (benchmarks section 30).

### 4.2 Payment is a separate service
The payment service has its own process and database. The shop calls it over HTTP, and the shop applies
the answer to the order in **one local transaction**: mark the order paid, record which attempt paid it,
and write one `order.paid` event to the outbox. A decline returns the stock once. Money safety rests on:
- a payment attempt has an idempotency key, so repeating a request never charges twice;
- an order can end in at most one final payment attempt (enforced by the database);
- if the payment was recorded but the shop never applied it (crash between the two), a repeat with the
  same key repairs it, and a **sweeper** in the shop repairs it even if the customer never returns;
- an unpaid order is cancelled and its stock returned after 30 minutes by the **expirer**, which first
  closes the order's payment in the payment service so a late payment cannot arrive after the stock went back.

Razorpay is supported as a provider (payment links, a signature-verified webhook that is only a *hint*
that is re-checked against Razorpay's API, and a poller for missed webhooks). The load tests use a
**simulator** (90% success, 5% declined, 5% temporary failure, 100 to 300 ms); real Razorpay was checked
by hand ([`benchmarks-aws.md`](benchmarks-aws.md) sections 19, 23, 24).

### 4.3 The outbox: how "paid" reliably becomes "invoice sent"
The shop must tell Forge to start the background jobs, but "mark the order paid" (a database write) and
"call Forge" (a network call) cannot be one transaction. If the shop called Forge first and crashed,
a job would exist for an unpaid order; if it wrote first and crashed, the jobs would never be created.
The **transactional outbox** solves this: in the same transaction that marks the order paid, the shop
writes a row to `outbox_events`. A separate **dispatcher** reads those rows and submits the jobs to Forge,
marking each row published afterwards. So a paid order always produces its event (same transaction), and
the dispatcher can crash and retry freely.

The dispatcher claims events the same way workers claim jobs (lease + fenced token), so two dispatchers
can run at once and never share an event (`internal/outbox`, tested against PostgreSQL). A crash
between "submitted to Forge" and "marked published" means the event is submitted again, and Forge ends
up with **two jobs** for it (the dispatcher's batch requests carry no idempotency key, although Forge's single-job
requests support one). That is harmless because the event's stable ID travels inside the job and every worker's
result row is unique per event: the second job finds the result already there and does nothing.
The dispatcher submits whole batches (one request and one database commit for the jobs of many events),
and Forge's batch endpoint has no idempotency key. Sending a key per job would mean one request per job
and lose the batching the dispatcher needs; adding keys to batches would be a change to Forge's core
(the endpoint plus a new database function). Since the duplicate is harmless, that change was judged not
worth its risk.

### 4.4 The four workers
For each paid order four jobs run: an inventory check (`inventory.reconcile`, a read-only check of the
order's stock figures, a demonstration rather than real warehouse work), create an invoice, prepare
fulfilment, and send a notification (recorded in a table; no email is actually sent). They are built on the SDK;
their results go to a separate database (raised end-to-end throughput about 12 to 18%, section 18).
They read the order from the shop database by order ID and trust the database, not the job payload.

### 4.5 Idempotent effects
Each worker's result row is unique per event (and per order), enforced by a database constraint, for
example one invoice and one fulfilment per order. A job that runs twice finds the row already there and
does nothing the second time. After every
load run, 25 integrity checks verify this and more: stock adds up, every paid order has exactly one
event, one effect per event and job type, no negative stock, and so on.

## 5. What happens when things break

| What fails | What happens | Why it is safe |
|---|---|---|
| A worker crashes mid-job | Lease expires, the reaper requeues the job, another worker runs it | At-least-once; effects are idempotent |
| A worker is slow or partitioned | Its renewals fail; it cancels its own handler. If another worker took over, the old one's writes get `409` | Claim generation fencing; self-fence |
| Forge's database is unreachable | Workers and submitters get `503` and back off; nothing is reported as done or as "no work" | A 503 is never mistaken for "ownership lost" or an empty queue (tested) |
| The dispatcher crashes after submitting | Its lease on the event expires; another dispatcher resubmits, so Forge holds a duplicate job | The stable event ID is in the job and results are unique per event, so the duplicate does nothing |
| The shop crashes after the payment was recorded | A repeat with the same key, or the sweeper, applies it | Payment attempts are idempotent and recorded first |
| The payment service is down | The shop reports an error; the order stays unpaid and expires after 30 minutes with stock returned | The expirer never cancels an order whose payment succeeded (tested) |
| A Razorpay webhook is lost | The poller finds the paid link | The webhook is only a hint |
| Two customers buy the last item | One succeeds, one gets "insufficient inventory" | Row lock + guarded update + `CHECK >= 0` |
| A customer double-clicks "pay" | One order, one charge | Idempotency keys |
| A disk fills | This happened once during benchmarking (7.3 million job rows). It led to optional retention (`FORGE_RETENTION_*`) and to a log-level setting after a worker log reached 4 GB | Retention deletes only finished jobs; queued and running jobs are never purged |

A crash of a whole process was exercised by hand on AWS (section 6).

## 6. What was measured

All figures are from AWS machines of 2 vCPUs, in one availability zone, with a **simulated** payment
step and a synthetic shopper load that waits for each reply (closed loop).

| Measure | Result | Where |
|---|---|---|
| Orders finished per second, including all four background jobs, 200 concurrent shoppers | about 546 (range 538 to 551, five runs) | section 28 |
| Same, 500 concurrent shoppers | about 581 (range 572 to 598, five runs) | section 28 |
| Paid orders per second | about 90% of the above | section 28 |
| 60 minutes at 100 shoppers | 1,302,376 orders, 0 failed, 4,688,816 background jobs, all 25 integrity checks passed; throughput drifted from about 396 to about 350 per second in the first half hour and then held | section 28 |
| Where the time goes | The shop database is at 93 to 100% CPU at these rates; about 3.6 ms of database time per order. The results database is also near its limit | sections 28, 29 |
| Forge alone (no ForgeCart) | about 1,200 jobs/s sustained with batch submission and workers claiming; about 2,000 completions/s measured on smaller machines | sections 3, 8 |

The largest improvements came from finding the real limit with measurement: batching submissions,
sharding the queue counter (a single counter row capped throughput near 580 transactions per second),
batching claims and completions, and moving the workers' results to their own database.
Several ideas gave nothing and were reverted (a database function for checkout, extra indexes,
connection-pool changes); they are listed in the benchmark document.

## 7. How it was checked

There are no automated tests in this repository. Correctness was checked three ways: 25 integrity checks
that the load-test tool runs against the databases after every load run (stock adds up, every paid order
has exactly one event and one effect per background job, no negative stock, and so on); load runs on AWS,
including an hour-long soak and a many-buyers-for-one-product test; and failure cases exercised by hand
(see the table in section 5). A set of 157 automated tests existed during development and was removed
from the repository on 2026-10-25 before publishing and is kept in the author's private archive.

## 8. Limits, stated plainly

- **Not production-ready.** It has not been run with real users, real money or real Razorpay traffic at scale.
- **Security:** keys are role-level (rotatable and revocable, but not per customer); isolation is by deployment (one Forge per team), not inside one Forge; **no TLS** (plain HTTP everywhere, including ForgeCart's services); no rate limiting. Keep it on a private network, or put a TLS proxy in front.
- **Capacity:** about 550 to 580 orders per second on this hardware, limited by the databases. More needs bigger machines (not free) or splitting data across databases (a large redesign, not built).
- **One Forge process** has been validated; several against one database have not.
- **One-hour soak,** not days. Throughput fell about 13% as the tables grew; the cause is probably table size but was not proven.
- **Load shape:** closed-loop shoppers show steady operation, not behaviour when more arrives than the system can take.
- **Same-product contention** was measured (section 30 of the benchmarks): with every shopper buying one product, throughput falls from about 495 to about 300 orders per second (-39%); stock was never oversold. A real flash sale with far more buyers than stock was not tested.
- Finished jobs are kept forever unless retention is configured.

## 9. Questions you may be asked

**Why PostgreSQL as the queue instead of Kafka or RabbitMQ?**
One system to run, real transactions, and the measured limit (about 2,000 jobs/s on small machines) is
far above the need. The decision is written down and would be revisited with evidence that the database
is the bottleneck. A broker would also not remove the need for idempotent handlers.

**Do you guarantee exactly-once?**
No. Forge is at-least-once; ForgeCart makes the *effects* idempotent so repeats do nothing.

**What stops two workers running the same job?**
`SKIP LOCKED` in the claim, and fencing by claim generation on every later write. If a lease expires
and the job runs elsewhere, the old worker's writes are rejected.

**What if the server crashes between charging and marking the order paid?**
The payment is recorded first in the payment service with an idempotency key. Repeating the request or
the sweeper applies it to the order; it cannot be charged twice.

**Why an outbox?**
Writing to the database and calling another service cannot be one transaction. The outbox row is written
in the same transaction as the order update, and a dispatcher delivers it afterwards, retrying; a retry
can create a duplicate job, which the workers' unique results make harmless.

**How do you prevent overselling?**
Row locks in a fixed order, a guarded decrement, and a database CHECK. Tested with 40 concurrent buyers
and verified after every load run.

**What is the bottleneck, and how would you scale it?**
The shop database CPU (about 3.6 ms per order on a 2-vCPU machine). Options: a bigger machine; fewer
statements per order (the candidates are 3 to 8% each and touch correctness code); a read replica for
the workers' reads (about 9% of database time); sharding by order ID, whose hard part is keeping checkout
inside one shard. The first measurement would be on a larger machine.

**How do you know it works?**
25 integrity checks after every load run; an hour-long soak with no failed
orders; failure cases exercised by hand on AWS. And what is not covered is listed in section 8.

**What would you do next?**
Pass the per-type config file through the Docker setup, add automated tests back if the project is to be maintained by others, and run an open-loop load test.

## 10. Glossary

- **Lease:** a time-limited claim on a job. If it is not renewed, the job is given to someone else.
- **Heartbeat:** the periodic renewal of a lease while the worker is alive.
- **Fencing / claim generation:** a number that increases with each claim; writes carrying an old number are rejected.
- **Idempotent:** doing it twice has the same effect as doing it once.
- **At-least-once:** it will happen one or more times, never zero.
- **Outbox:** a table written in the same transaction as the business change, drained by a separate process.
- **Backpressure:** telling callers to slow down instead of accepting work that cannot be done.
- **Row lock:** a lock on one database row; others that need it wait.
- **`SKIP LOCKED`:** a PostgreSQL option so a query skips rows another transaction holds.
