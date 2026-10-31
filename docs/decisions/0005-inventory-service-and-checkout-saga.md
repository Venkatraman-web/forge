# Decision 0005 — A separate inventory database and a checkout saga

Status: **accepted 2026-10-30; implemented and verified on the EC2 databases (section 8); not yet measured on Neon.**

## 1. Why

ForgeCart's shop database holds orders, order items, the outbox **and the product stock**. On the Neon Free setup (shop, results and payments on three Neon projects, Forge's database on EC2) the front part alone (create an order, pay it, no background work) topped out at 710-760 orders per second however many shoppers were added (`docs/benchmarks-aws.md`, sections 49-50). A 150-second steady run showed why: the shop project's sessions were running on CPU 4% of the time and waiting the rest, on commits waiting for replication (41%), on row locks (23%) and on page locks (17%); the product `SELECT ... FOR UPDATE` averaged 41 ms and the checkout statement 35 ms, against about 1 ms on EC2. Every checkout holds row locks on its products from the first read to the commit, and a commit on Neon takes tens of milliseconds, so checkouts queue behind each other on the stock rows and on one commit stream.

Narrowing the lock window inside the one database was tried and was 27-30% slower on Neon (section 50, reverted). The remaining lever is to take the stock rows, their locks and their commits out of the orders database.

## 2. The decision

- **Inventory** becomes its own database (a Neon project; on EC2, a second database on the same server). It owns the product catalogue (price, name, active) and the stock, and a table of **stock reservations**.
- **Orders** keeps orders, order items, the outbox and the payment hand-off. It has no foreign key to products: an order item snapshots the product (id, SKU, name, price) as it already does.
- **Checkout is a saga with two local transactions** instead of one distributed one:
  1. read the products (no locks) and validate (existence, currency, price, advisory stock);
  2. **reserve** the stock in the inventory database: one short transaction that inserts the reservation (keyed by the order id) and decrements the stock under a conditional guard;
  3. create the order and its items in the orders database in **one statement** (one commit), using the same order id;
  4. if step 3 fails or finds that the idempotency key was already used, **release** the reservation (the compensation) and, for a replay, return the existing order.
- **Declined payment, expiry and cancellation** change the order state in the orders database first and then **release** the reservation in the inventory database. The release is idempotent (it only acts on a reservation still in state `reserved`).
- **A reaper** (in the shop process, next to the order expirer) releases what a crash leaves behind: a reservation still `reserved` after a grace period whose order does not exist, or whose order is `payment_failed` or `cancelled`.
- The inventory worker no longer joins the products table (the stock can never be negative: a CHECK constraint enforces it); the load tool's integrity checks read both databases.

## 3. What can go wrong, and what recovers it

| Failure | Result | Recovery |
|---|---|---|
| Crash or error after the reservation, before the order exists | stock held for no order | the reaper releases it after the grace period |
| The order insert returns an error (a timeout, a cancelled request, a lost connection) | the insert may or may not have committed; releasing at once could return the stock of an order that exists | the checkout does **not** release, except when the connection could not be made at all (the statement was never sent); otherwise the reaper (which releases in bulk) releases the reservation after the grace period only if no order exists (found by chaos test 1, section 62 of `docs/benchmarks-aws.md`) |
| Order created, then the shop crashes before replying | the client retries with the same key and gets the order (a replay); the reservation is already right | none needed |
| Two requests with the same key at once | both reserve, one order insert wins | the loser releases its own reservation |
| Payment declined or order expired, release call fails | order says `released`, stock still held | the reaper (order state `payment_failed` or `cancelled`, reservation `reserved`) |
| Release called twice | stock returned once | the reservation state makes it idempotent |
| Reserve called twice for one order id | stock taken once | the reservation row (primary key on the order id) makes it idempotent |

**Invariants to prove:** stock never goes negative; no unit is sold twice; once everything has settled, `initial stock = current stock + units of orders that are reserved or paid` (the existing stock-ledger check, now across the two databases) and no reservation is `reserved` without a live order in state `pending_payment` or `paid`.

## 4. What is given up

- **Atomicity of "order and stock".** A saga can be observed half-done (stock reserved, order not yet there). The invariants above hold only after the compensations or the reaper have run, so the load tool's ledger check must run after the drain, and the reaper interval matters.
- **The foreign key from order items to products**, and any join between them (the load tool and the inventory worker used some).
- **One round trip more** per checkout (the reservation) and **one more database** to run. On the target (Neon) each checkout transaction becomes short and the two commit streams run in parallel.

## 5. Alternatives considered

- **Tune the single database** (lock window, fill factor, more dispatchers): tried, not enough (sections 47-50).
- **Sharded stock counters** (several rows per product): reduces row contention but not the commit stream, and complicates the stock check.
- **A read replica for the catalogue**: reads were not the problem.
- **Two-phase commit across the databases**: Neon does not offer it, and it would hold locks across both commits, which is the cost to avoid.

## 6. How it is verified

1. EC2 first (two databases on one server, no transfer cap): the single-order checks, the stampede test (300 shoppers on 3 products with 2,000 units each: stock rejections, zero deadlocks, 25 of 25 checks), and crash tests (kill the shop between the steps; the reaper must restore the ledger).
2. Then Neon with the inventory project: the front part alone, monolith against saga, in one session, before any claim.

## 7. Revisit triggers

If the measurement on Neon shows no gain, revert and record why; if the reaper is needed often in normal operation, the saga's ordering is wrong and should be reconsidered.

## 8. Implementation and verification record (2026-10-30)

Implemented: the `inventory` package (lookup, reserve, release, commit, a cursor over stale reservations), the saga checkout in `orders`, the release after a declined or expired order and the **reaper** in `payments`, the inventory migrations (`forgecart/inventory_migrations/`) and the orders migration that drops the products table (`20260101000019_split_inventory.sql`), the shop's `FORGECART_INVENTORY_DATABASE_URL`, the inventory worker without the products join, and the load tool's cross-database stock checks (now 26 checks).

A third reservation state, **`committed`**, was added during implementation: a paid order's reservation would otherwise stay `reserved` forever and the reaper would keep reading it. The reaper marks the reservations of paid orders committed in batches.

Verified on the EC2 databases (two databases on one server): the single-order checks; replay and key reuse (reserve then release, stock unchanged); the declined-payment release; the reaper's commit, orphan release and failed-release repair; a stampede (300 shoppers on 3 products, 2,000 units each): same winners and losers as before the change, 0 failed, 0 deadlocks, 26 of 26 checks; and a crash test (the shop killed with SIGKILL 12 times during a 60-second load): ledger exact, no order without a reservation, nothing held for a dead order, 430 orphans released.

A third defect was found by a chaos test on Neon (the shop service frozen for 12 s under load, 2026-10-30): the checkout released the reservation whenever the order insert returned an error, including when the insert had committed, so 25 orders were left without their stock; it no longer releases on an insert error and leaves the decision to the reaper (verified by repeating the test: 206 reservation mismatches right after the drain became 0 within 90 s and stayed 0, `docs/benchmarks-aws.md` sections 62 and 63).

Two defects were found by these tests and fixed: the load tool's effect checks iterated over the workers instead of the effects, and the reaper read the oldest 2,000 reservations on every pass, so more than 2,000 orders awaiting payment hid the orphans behind them (it now walks the list with a cursor).
