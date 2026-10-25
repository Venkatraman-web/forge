# ForgeCart (reference application, through milestone 25.4.3.2)

ForgeCart is a local-development ecommerce example used to evaluate Forge's background-job platform. **So far it has a product catalog, checkout (order creation, stock reservation, idempotency) and a simulated payment provider and a transactional outbox.** No real payment is ever taken. A separate **dispatcher** process publishes the outbox events to Forge as jobs. **All four business workers exist** (`inventory.reconcile`, `invoice.generate`, `notification.send`, `fulfillment.prepare`), each a separate process. (An analytics worker existed earlier and was removed: in a real shop analytics is an event pipeline outside the order path.)

## Database: separate from Forge's

ForgeCart owns its data. Point `FORGECART_DATABASE_URL` at a **different database** from the one Forge uses (`FORGE_DATABASE_URL`). The variable name does not enforce this; nothing in the code checks it. If you pointed both at the same database, ForgeCart's `products` table would sit next to Forge's `jobs` table. ForgeCart never reads or writes `jobs`, and it never falls back to Forge's database: without its own URL it refuses to start.

Create an empty database for it (for example with the same PostgreSQL container Forge uses, `CREATE DATABASE forgecart;`).

## Run

From the repository root:

```bash
export FORGECART_DATABASE_URL='postgres://USER:PASSWORD@127.0.0.1:5432/forgecart?sslmode=disable'

# 1. Apply the schema explicitly (versioned SQL in forgecart/migrations: products, orders, payment attempts, outbox, outbox failed state, uncharged attempts, inventory reconciliations, invoices, notification deliveries, fulfillments, and the removal of the earlier analytics table; nothing migrates at startup)
atlas migrate apply -c file://forgecart/atlas.hcl --env forgecart --url "$FORGECART_DATABASE_URL"

# 2. Seed the starter catalog (safe to repeat)
go run ./forgecart/cmd/forgecart seed

# 3. Serve
go run ./forgecart/cmd/forgecart
```

Configuration: `FORGECART_DATABASE_URL` (required, `postgres://` or `postgresql://`), `FORGECART_ADDR` (default `127.0.0.1:8081`; loopback because there is no authentication). Invalid or missing values stop startup with a message that never includes the URL.

Validate the migration directory: `atlas migrate validate -c file://forgecart/atlas.hcl --env forgecart`.

## Seed data

Currency is USD; **prices are integer cents** (`unit_price_minor`), never floats.

| SKU | Name | Price (cents) | Stock |
|---|---|---|---|
| KB-WL-001 | Wireless Keyboard | 4999 | 50 |
| HUB-USBC-7 | USB-C 7-in-1 Hub | 3999 | 40 |
| STAND-LP-2 | Aluminium Laptop Stand | 2999 | 30 |
| MOUSE-ERG-1 | Ergonomic Wireless Mouse | 2499 | 60 |
| MON-ARM-1 | Monitor Arm (single) | 5999 | 20 |

The seed uses `INSERT ... ON CONFLICT (sku) DO NOTHING`: existing rows are left exactly as they are, so re-running never resets stock or duplicates a SKU. (A side effect: editing the seed file does not update existing rows.)

## Endpoints

Errors always look like `{"error":{"code":"...","message":"..."}}`. Any method other than GET (and HEAD) on these paths gets `405 method_not_allowed` in the same JSON shape with `Allow: GET, HEAD`. Unknown paths still get Go's plain-text 404.

- `GET /healthz` → `200 {"status":"ok"}` while the process runs.
- `GET /readyz` → `200 {"status":"ready"}`, or `503 {"status":"unavailable"}` when the database does not answer a ping within 2 s.
- `GET /v1/products?limit=N&after=SKU` → `200 {"products":[...],"next_cursor":"SKU"|null}`. Ordered by SKU. `limit` is 1-100 (default 20); `after` is the previous page's `next_cursor`. A malformed query string (raw `;`, bad percent-encoding) → `400 invalid_query`; invalid values → `400` (`invalid_limit`, `invalid_cursor`). Nothing malformed is ignored or treated as the first page.
- `GET /v1/products/{sku}` → `200` product, or `404 product_not_found`.

### Checkout: `POST /v1/orders`

Headers: `Idempotency-Key` (required, exactly one; 8-128 chars from `A-Z a-z 0-9 _ . : -`) and `Content-Type: application/json`. Body, at most 16 KiB:

```json
{"customer_email": "alice@example.com",
 "items": [{"sku": "KB-WL-001", "quantity": 2}, {"sku": "HUB-USBC-7", "quantity": 1}]}
```

Unknown fields (including any client price), trailing data and non-integer quantities are rejected. Limits: 1-20 items, each quantity 1-100, **each SKU at most once** (duplicates are rejected, not merged). The email is trimmed, lower-cased and must be a plain address (`name@example.com`, no display name).

Prices, names, line totals and currency come only from the catalog at checkout. Response (`201 Created`):

```json
{"order_id": "ord_<24 hex>", "status": "pending_payment", "inventory_status": "reserved",
 "currency": "USD", "subtotal_minor": 8997, "total_minor": 8997,
 "items": [{"sku": "...", "name": "...", "quantity": 2, "unit_price_minor": 4999, "line_total_minor": 9998}],
 "created_at": "2026-01-01T00:00:00.000000Z", "replayed": false}
```

Items are sorted by SKU and hold the price/name snapshots stored at checkout; later catalog edits never change them. A successful checkout means **stock is reserved and payment is still owed**.

| Status | Code | When |
|---|---|---|
| 201 | | New order created, stock decremented once |
| 200 | | Replay: same key and same request; the stored order, `replayed: true`, header `Idempotent-Replayed: true`, no stock change |
| 400 | `invalid_idempotency_key`, `invalid_json`, `invalid_email`, `invalid_items`, `invalid_sku`, `invalid_quantity`, `duplicate_item`, `order_too_large` | Malformed or invalid input; `order_too_large` = the total would overflow int64 |
| 404 | `product_not_found` | A SKU is unknown or inactive (nothing is reserved) |
| 409 | `insufficient_inventory`, `idempotency_key_reused`, `mixed_currency`, `unsupported_currency` | Stock too low; key already used for a different request; products with different or non-USD currency |
| 413 / 415 | `request_too_large`, `unsupported_media_type` | |
| 500 | `internal_error` | Unexpected failure; the message is generic |

### The payment service (a separate process with its own database)

Payment is its own service, `forgecart/cmd/forgecart-payments`, with its own database (`FORGECART_PAYMENTS_DATABASE_URL`, schema in `forgecart/payments_migrations/`: `payment_attempts` and `razorpay_links`, no foreign keys to the shop). It owns the payment providers (the simulator and Razorpay below), the attempt records, and one HTTP endpoint the shop calls: `POST /v1/payments` with `{"order_id", "amount_minor", "currency"}`, a header `Idempotency-Key` and the bearer key `FORGECART_PAYMENT_API_KEY` (at least 32 characters, the same on both sides; a development stand-in for real service authentication). It knows nothing about orders beyond their ID and the amount it was asked to charge. A second final attempt for an order (succeeded or declined) is refused (`409 order_payment_final`), also by two partial unique indexes; the same key replays the stored attempt; a different amount than the order's earlier attempts is `409 payment_amount_mismatch`.

Both processes take `FORGECART_DB_MAX_CONNS` (default `10`, 1 to 200) as their database pool size, each from its own environment; every process of every service counts against PostgreSQL's connection limit (100 by default). On the AWS setup the order service did best at 20 and the payment service gained nothing from more (`docs/benchmarks-aws.md`, section 21).

The shop (order service) is configured with `FORGECART_PAYMENT_URL` (for example `http://127.0.0.1:8095`) and `FORGECART_PAYMENT_API_KEY`. The service listens on `FORGECART_PAYMENTS_ADDR` (default `127.0.0.1:8095`). The simulator and Razorpay variables below belong to the **payment service's** environment now, not the shop's.

The public endpoint below is unchanged for clients. Behind it, the shop reads the order, calls the payment service with the shopper's key, and applies the answer in one local transaction: `succeeded` marks the order paid (recording `orders.paid_attempt_id`) and writes its one `order.paid` event; `declined` releases the stock; temporary failures and pending Razorpay payments change nothing. The order and the payment are in two databases, so a crash between the payment service recording an attempt and the shop applying it leaves a recorded payment and a pending order. **Repeating the request with the same key repairs it** (the service replays the stored attempt and the shop applies it; applying twice is harmless). If the client never retries, the **payment sweeper** repairs it: one goroutine in the shop process, every `FORGECART_PAYMENT_SWEEP_INTERVAL` (default `30s`, `0s` turns it off), walks the payment service's final attempts (`POST /v1/payments/finals`) in the order they were completed, from a cursor kept in the shop's database (`payment_sweep_cursor`, which starts when its migration runs), and applies those whose order is still pending, at most 20 pages of 500 per pass. It only looks at attempts completed more than `FORGECART_PAYMENT_SWEEP_AGE` ago (default `30s`, minimum `5s`), so it does not race the request that is applying them. Walking payments instead of pending orders keeps the repair time independent of how many orders were abandoned unpaid (a first version that scanned pending orders needed a full cycle through them: the test database's several hundred abandoned orders delayed a repair by a minute). A repair is logged as a warning, because it means an earlier request died between the two steps. The database role the shop uses needs privileges on `payment_sweep_cursor` like on every table. If the payment service cannot be reached the shop answers `503 payment_unavailable` and the same key can be retried. The background jobs check the event against `orders.paid_attempt_id`, so they no longer read payment tables. The old `payment_attempts` and `razorpay_links` tables remain unused in the shop's database.

### Razorpay webhook (optional, Razorpay provider only)

Without it, a Razorpay payment is only noticed when somebody asks again (the customer's browser, or the expirer, 30 minutes later by default). With it, Razorpay tells the payment service when a payment link is paid, expires or is cancelled. Set `FORGECART_PAYMENTS_WEBHOOK_ADDR` (host:port of a separate listener) and `FORGECART_RAZORPAY_WEBHOOK_SECRET` (at least 12 characters, the secret entered for the webhook in Razorpay's dashboard) together, with `FORGECART_PAYMENT_PROVIDER=razorpay`. The listener serves only `POST /webhooks/razorpay`.

- **A verified event is only a hint.** The request must carry `X-Razorpay-Signature`, the hex HMAC-SHA256 of the raw body under the secret (constant-time comparison; anything else is `401`). Only the order ID (the link's `reference_id`) is taken from the body. The payment service then reads the link from Razorpay's API and decides exactly as a payment request would (paid: a succeeded attempt with Razorpay's payment reference; expired or cancelled: a declined one), so a stale, repeated or forged-but-correctly-signed event cannot mark anything paid by itself.
- **Idempotent.** The attempt is recorded under the key `settle:<order>` (the same key the poller uses), so a repeated delivery changes nothing. Events other than `payment_link.paid`, `.expired` and `.cancelled`, and references that are not ours, are answered `200` and ignored. A body over 64 KiB is `413`.
- **Failure handling follows Razorpay's rules** (checked in its documentation): answer `2xx` within 5 seconds (the handler works inside a 4 second budget), and Razorpay retries any other answer with exponential backoff for 24 hours before disabling the webhook. If Razorpay's API cannot be reached the handler answers `503`, so the event is delivered again. A link whose amount differs from the order's earlier attempts is logged and answered `200` (retrying cannot fix it).
- **The customer's own request** (a different idempotency key) gets the attempt the webhook recorded, as a replay, instead of "payment already final". The shop then applies it as usual, and the sweeper does so if the customer never comes back.
- **What Razorpay requires of the URL:** ports 80 or 443 only, HTTPS with TLS 1.2 or later recommended, and its webhook IPs allowed through any firewall. Put a TLS-terminating proxy on 443 in front of the listener, register the URL and secret in Razorpay's dashboard (Settings, Webhooks) and subscribe to the three events. The secret cannot be rotated without a gap (no support for two secrets at once). `forgecart_paymentsvc_webhooks_total{result}` counts what arrives.

### Razorpay poller (Razorpay provider only)

The webhook needs a public HTTPS endpoint and a dashboard setup; the poller needs nothing. With the Razorpay provider the payment service runs one goroutine that every `FORGECART_RAZORPAY_POLL_INTERVAL` (default `30s`, `0s` turns it off) takes up to `FORGECART_RAZORPAY_POLL_BATCH` (default 50, at most 500) payment links created in the last 2 hours that have no final attempt, least recently checked first (`razorpay_links.checked_at`), and settles each one exactly as the webhook does: it reads the link from Razorpay's API and records a succeeded attempt (paid) or a declined one (expired or cancelled), or does nothing while the customer can still pay. The attempt gets the key `settle:<order>`; the shop's sweeper then applies it to the order. So a customer who pays and closes the tab no longer waits for the expirer.

If Razorpay cannot be reached, a pass stops after 3 failed links instead of hammering the API; every pass that failed to settle some link is logged as a warning and counted (`forgecart_paymentsvc_razorpay_poll_passes_total{result="error"}`), and the next pass tries again. `forgecart_paymentsvc_razorpay_poll_settled_total` counts payments the poller settled because neither the webhook nor the customer had asked: anything above zero means the webhook is missing or late. The cost is about one Razorpay API call per unsettled link per interval.

### Expiry of unpaid orders

An order that is created but never paid keeps its stock reserved (`pending_payment`, `reserved`). After `FORGECART_ORDER_EXPIRY` (default `30m`; `0s` turns it off; 1 minute to 30 days) the shop's **expirer** cancels it: `status` `cancelled`, `inventory_status` `released`, and the reserved quantities are added back, once. One goroutine in the shop process looks every `FORGECART_ORDER_EXPIRY_INTERVAL` (default `30s`) for stale orders, oldest first, at most 2,000 per pass, using the partial index `orders_pending_created_idx` (only pending orders).

**Log level.** Every ForgeCart process (shop, payment service, dispatcher, the four workers) logs JSON to standard output and reads `FORGECART_LOG_LEVEL` (`debug`, `info`, `warn` or `error`; default `info`; any other value stops the process at startup). At `info` a worker writes a line per job and the dispatcher one per event, about 0.8 MB/s per worker at full load; use `warn` for long runs. The load tool's `remote-cart` command also takes `--duration-seconds N` (60 to 7200) to run each level for N seconds instead of N workflows (a soak).

It cannot simply flip the order, because a payment may be in flight or a Razorpay customer may still be able to pay. So for each stale order it first asks the payment service to **close** the order's payment (`POST /v1/payments/cancel`):

- If the order already has a final payment there (succeeded, or declined by the provider), the service returns it and the shop applies it exactly as the sweeper would: a payment that succeeded makes the order **paid**, not cancelled.
- Otherwise the service records a decline under the key `expire:<order>`, and from then on refuses every payment for that order (`order_payment_final`, which the shop reports as `409 order_not_payable`). For Razorpay it first looks at the payment link: a paid link is recorded as a succeeded payment, and a link that could still be paid is cancelled at Razorpay before the decline is recorded, so money cannot arrive for a closed order.
- Only then does the shop cancel the order. Every step is idempotent, so a crash in between is repaired by the next pass. `forgecart_orders_expired_total` and `forgecart_order_expiry_passes_total{result}` count what it does. Pick an expiry longer than the longest time a customer can reasonably take to pay (a Razorpay link lives 16 minutes).

### Payment: `POST /v1/orders/{order_id}/payment-attempts`

One simulated payment attempt for an order. Header: `Idempotency-Key` (same rules as checkout). No body (empty or `{}`; anything else is `400 unexpected_body`, so no card data or client-chosen outcome can be sent). The outcome is chosen by the server (see below). Response `200` for all three outcomes, because a decline is a business result, not an HTTP failure:

```json
{"order_id": "ord_...", "payment_attempt_id": "pay_<24 hex>", "attempt_number": 1,
 "outcome": "succeeded", "provider_reference": "sim_<24 hex>",
 "order_status": "paid", "inventory_status": "committed",
 "completed_at": "2026-01-01T00:00:00.000000Z", "replayed": false}
```

| `outcome` | `order_status` / `inventory_status` after | Stock |
|---|---|---|
| `succeeded` (has `provider_reference`) | `paid` / `committed` | stays deducted (not deducted again) |
| `declined` | `payment_failed` / `released` | the order's quantities are added back, once |
| `temporary_failure` | `pending_payment` / `reserved` | stays reserved; try again with a **new** key |

Errors: `400` (`invalid_order_id`, `invalid_idempotency_key`, `unexpected_body`), `404 order_not_found`, `503 payment_unavailable` (the payment service could not be reached; retry with the same key), `502 payment_provider_error` (Razorpay), `409 order_not_payable` (the order is already paid or failed and this key was never recorded; nothing changes), `413`, `503 interrupted` (shutdown or cancellation during the simulated delay: nothing was recorded, retry the same key), `500 internal_error`.

**Replay.** The logical attempt is (order, key), stored in `payment_attempts` with unique constraints on `(order_id, idempotency_key)` and `(order_id, attempt_number)`. The same key on the same order always returns the stored attempt (`replayed: true`, header `Idempotent-Replayed: true`) without running the simulator, changing stock or creating a row, even after the order has moved on. This includes a recorded `temporary_failure`: the same key never retries; use a new key. `order_status`/`inventory_status` in a replay are the order's *current* state. The same key on a different order is a different attempt.

**Concurrency.** Without holding any lock, the request looks up the key, reads the order (if it is no longer `pending_payment` the key is looked up once more before answering `409`, because a concurrent request with the same key may have just committed; only a never-recorded key is a conflict), picks the outcome and waits out the delay. Then one transaction locks the order row (`SELECT ... FOR UPDATE`), re-checks the key, requires `pending_payment`/`reserved`, takes the next attempt number (`MAX + 1`, safe under the lock), inserts the attempt, and applies the order change with a conditional `UPDATE ... WHERE status='pending_payment' AND inventory_status='reserved'` (one row must match). A decline also adds the stock back in the same transaction, in SKU order (checkout's lock order), so stock is returned exactly once because only one transaction can make the reserved-to-released change. If anything fails, everything rolls back and the same key can be retried. This is idempotent recording, not exactly-once payment: a client whose response was lost simply replays and gets the stored result.

**Simulator configuration** (all optional; validated at startup):

| Variable | Default | Rule |
|---|---|---|
| `FORGECART_PAYMENT_SUCCESS_PCT` | `100` | integer 0-100 |
| `FORGECART_PAYMENT_DECLINE_PCT` | `0` | integer 0-100 |
| `FORGECART_PAYMENT_TEMP_FAILURE_PCT` | `0` | integer 0-100; the three must sum to exactly 100 |
| `FORGECART_PAYMENT_SEED` | `forgecart-dev` | 1-64 characters |
| `FORGECART_PAYMENT_MIN_DELAY` / `_MAX_DELAY` | `0s` / `0s` | `0 <= min <= max <= 5s` |

The outcome and delay are a pure function of SHA-256(seed, order ID, key): the same inputs give the same result in every process, and different keys can differ. To force a scenario set 100 for one outcome, e.g. decline-only: `SUCCESS_PCT=0 DECLINE_PCT=100`; temporary-failure-only: `SUCCESS_PCT=0 TEMP_FAILURE_PCT=100`. With a mix, a temporary failure followed by a new key can succeed. The delay waits on a timer that stops at once on request cancellation or SIGTERM. The config is read at startup and is not stored with the attempt; changing it between a failed persistence and a retry can change the decision.

### Optional: Razorpay as the payment provider

By default payments are simulated. Set `FORGECART_PAYMENT_PROVIDER=razorpay` to use [Razorpay Payment Links](https://razorpay.com/docs/api/payments/payment-links/) instead. Use your account's **test-mode** keys, so no real money moves.

| Variable | Meaning |
|---|---|
| `FORGECART_PAYMENT_PROVIDER` | `simulator` (default) or `razorpay` |
| `FORGECART_RAZORPAY_KEY_ID`, `FORGECART_RAZORPAY_KEY_SECRET` | the account's API keys; both required with `razorpay`; the secret is never logged or echoed |
| `FORGECART_RAZORPAY_BASE_URL` | default `https://api.razorpay.com`; only change it to point at a stand-in server |

How it works, using the same endpoint, `POST /v1/orders/{order_id}/payment-attempts`:

1. **First call:** ForgeCart creates a payment link for the order (the order ID is the link's `reference_id`, so an order can never get two links) that stays payable for **16 minutes** (Razorpay's minimum is 15 and it refuses exactly 15; without an expiry the default is six months, which would hold the order's stock that long), and stores it in `razorpay_links`. The answer is `200` with `"outcome": "pending"` and a `payment_url`. Nothing else changes.
2. **Open the `payment_url`** in a browser and pay with a Razorpay test card (see Razorpay's test-card documentation).
3. **Call the same endpoint again.** ForgeCart reads the link from Razorpay. Still unpaid: `pending` again with the same URL. **Paid:** a `succeeded` attempt is recorded, the order becomes paid and its `order.paid` event is written in one transaction, exactly as with the simulator, with the Razorpay payment ID as `provider_reference`. **Expired or cancelled:** a `declined` attempt is recorded and the stock is released. Razorpay does not flip an unpaid link to "expired" when its time is up (measured: still "created" more than six minutes later), so ForgeCart also treats a link still unpaid **2 minutes past its `expire_by`** (so about 18 minutes after creation) as expired; the grace keeps a customer who pays in the last seconds from being declined. This happens when the endpoint is called after that time; nothing calls it automatically.
4. After the outcome is recorded, repeating the same `Idempotency-Key` replays it.

If Razorpay cannot be reached or refuses a request, the answer is `502 payment_provider_error` and the order is unchanged, so the call can simply be repeated.

**Validated on 2026-10-24** with a real Razorpay test-mode account on AWS: a payment link was created for a real order, a test card payment was made on the hosted page, and the next call to the endpoint recorded it: the order became paid with Razorpay's payment ID as `provider_reference`, a repeat with the same key was a replay, and the four background jobs ran once each. Also checked against a stand-in Razorpay server (the paid, expired and provider-failure paths) and by the standard simulator regression (24 of 24 integrity checks). The expiry was also checked against the real Razorpay (a link left unpaid was still reported as `created` six minutes past its `expire_by`, and ForgeCart then declined the order and released its stock). Not checked: webhooks and refunds (not built) and live mode. Two things seen on the real hosted page: a USD link offers **cards only** (UPI and netbanking appear for INR), and an account without international payments enabled refuses international test cards ("International cards are not supported"), so use one of Razorpay's Indian test cards; if the page offers to save the card and asks for an OTP, choose "Skip OTP", because that OTP is a real SMS code, not the test OTP.

Kept deliberately small: no webhook (you call the endpoint again to check), no refund, no new background job, no Razorpay library (plain `net/http`), and ForgeCart's own notifications to customers are unaffected (Razorpay's SMS and email for the link are turned off, and no customer details are sent to Razorpay). The order's currency and amount are sent as they are, so **your Razorpay account must accept the order's currency** (ForgeCart orders are in USD; an account that only takes INR will be refused with the `502` above). The **load tests use the simulator**: Razorpay needs a person to pay, and its API is rate-limited.

### Transactional outbox

**Why.** After a payment succeeds, ForgeCart wants four background jobs run. Calling Forge from the payment request could leave a paid order with no jobs (Forge down, crash after commit) or jobs for a payment that rolled back. Instead the payment transaction writes the *intent* to the `outbox_events` table; a separate dispatcher (a later task, **not written yet**) will read the rows and submit jobs to Forge. The outbox gives durable publication intent. It does not give exactly-once publication or execution: the dispatcher will publish at least once, and handlers must tolerate duplicates.

**Transaction boundary.** In the same transaction that inserts the successful attempt and sets the order `paid`/`committed`, ForgeCart inserts **one** outbox row: the order's `order.paid` event (the insert must affect one row). If any step fails, the attempt, the order change and the event roll back together, and the same payment key can be retried. A committed successful payment therefore always has its event, and no event exists for a payment that did not commit. The payment request sends nothing to Forge and starts no goroutine.

**One event, four jobs (since 2026-10-24).** The dispatcher turns an `order.paid` event into **four Forge jobs** when it publishes it, always in the same Forge request, so the event is either published or not (an event's jobs are never split across two requests). Writing one row per order instead of one per job cut the outbox's writes from 16 row writes per order (each of four events was inserted, leased, attempt-counted and marked published) to 4; measured on AWS the gain in end-to-end throughput was only about 3 to 7% (details in `docs/benchmarks-aws.md`). Events written by the earlier design (one event per job, with the job's own type) are still valid and are published as one job each, and the workers accept them.

| Job type (one job per paid order) | The worker's job |
|---|---|
| `inventory.reconcile` | Check order/stock consistency. Stock was already deducted at checkout and payment only marks it `committed`, so this must never blindly add or subtract stock; any correction must follow explicit idempotent rules. |
| `fulfillment.prepare` | Prepare shipment for the order. |
| `invoice.generate` | Generate the invoice. |
| `notification.send` | Send the confirmation. |

**Event contract.** `id` is `evt_` + 24 hex characters, generated once and never changed, so it can identify the event across publication retries, on the Forge job and in the business effect (a handler can use it to avoid doing the work twice). `payload` is a JSON object of at most 1 KiB, and contains only `{"order_id": "ord_...", "payment_attempt_id": "pay_..."}`. It has no customer data, prices, items, secrets or error text; workers load what they need from ForgeCart's database by order ID. The dispatcher will build each Forge job input from `event_id` (the row `id`), the event type (as the job type) and that payload.

**Columns for the dispatcher.** `status` (`pending` or `published`), `attempts`, `next_attempt_at` (due time and retry backoff), `leased_by` and `lease_expires_at` (so several dispatchers can claim an event without owning it twice, and an expired lease marks a crashed holder), `published_at`. Rows start `pending`, 0 attempts, due now, unleased. A partial index on `(next_attempt_at, id) WHERE status='pending'` serves the "due events" query. `UNIQUE (order_id, event_type)` is the final guard against duplicate sets. I kept the lease to two columns; a terminal failure status, if the dispatcher needs one, is for its task to add.

**Claim, lease and state operations (`internal/outbox/claims.go`).** Persistence functions used by the dispatcher. Each is one conditional SQL statement using PostgreSQL's clock.

- **Eligibility.** `Claim(batch 1-100, lease)` considers events with `status='pending'`, `next_attempt_at <= now()` and either no lease or `lease_expires_at <= now()`, oldest first (`next_attempt_at`, then `id`). `SELECT ... FOR UPDATE SKIP LOCKED` inside one statement means two dispatchers never actively hold the same event; one that finds a row locked simply skips it. The lease is at most 1 hour and should be a little longer than the publisher's request deadline. There is no lease renewal; the future dispatcher must finish, or give up, within the lease.
- **Crash recovery.** A dispatcher that dies keeps nothing alive: its lease simply expires and the event becomes claimable again, by anyone.
- **Fencing tokens.** Every claimed event gets its own random 128-bit token (`crypto/rand`, 32 hex chars), stored in `leased_by` and returned to the caller as `Event.ClaimToken` (never serialized or logged). A reclaim writes a new token. `StartAttempts`, `MarkPublished`, `RescheduleAfter` and `MarkFailed` require the event to be pending, the token to match, and the lease to be unexpired; otherwise they change nothing and return `ErrLeaseLost`. So an old owner cannot touch an event that was reclaimed, and an owner whose lease expired cannot finish even if nobody has reclaimed it yet. Malformed arguments return `ErrInvalidInput`; database failures return the underlying error, never `ErrLeaseLost`.
- **`attempts`.** `StartAttempts` adds 1 and is meant to be called immediately before the Forge request, so it counts attempts *started*. It cannot be atomic with an HTTP call: if the process crashes after it commits but before the request is sent, the counter is one too high.
- **`published`.** `MarkPublished` sets `status='published'`, `published_at`, and clears the lease. It means only that Forge accepted the job submission. It does not mean the job ran or succeeded, and a published event is never changed by these operations.
- **At-least-once.** If the Forge request's outcome is uncertain (timeout, lost response) the dispatcher cannot know whether Forge created the job. It must either reschedule (`RescheduleAfter(id, token, delay, reason)` keeps the event pending, with the same stable event ID, due again `delay` later, 0 < delay <= 30 days, **counted by PostgreSQL's clock inside the update**, so a skewed application clock cannot change it) and accept that Forge may end up with two submissions, or risk losing the event. If a publisher is slower than its lease, another dispatcher can reclaim and publish the event too. Hence publication is at-least-once, and Forge jobs/handlers must deduplicate on the event ID.
- `MarkFailed(id, token, reason)` moves the event to the terminal `failed` state (migration 5 adds `failed` to `status`, plus `failed_at` and `last_error_code`). `last_error_code` is a sanitized lowercase code such as `forge_http_503`, never response text. A failed event is never claimed or changed again by these operations.

**No duplicates.** Declined and temporary-failure attempts never touch the outbox. A replayed key returns the stored attempt before any transaction and inserts nothing. A new key on a paid order gets `409 order_not_payable` before any insert. Concurrent requests serialize on the order row lock, so only one transaction can make the pending-to-paid change that inserts events, and the unique constraint would reject a second set anyway.

**Existing data.** Orders already paid before this migration are **not** backfilled: they have no events, and only new successful payments create them. For the local demonstration, create fresh orders after applying the migration (or design a deliberate backfill separately).

### Dispatcher (`cmd/forgecart-dispatcher`)

A separate process that turns outbox events into Forge jobs. It talks to Forge only over HTTP (`POST /v1/jobs`) and never reads Forge's database. It is independent of the API process; run any number of replicas against the same ForgeCart database (they cannot hold the same event at once, because of the claim leases). **Run at least two under load**: one dispatcher is a single serial loop (claim up to the batch size, send to Forge, wait, repeat), so near 500 orders/s it has little headroom, and any pause in the shop database builds a backlog that it clears slowly. Measured on AWS, a second replica cut the time events wait before reaching Forge by about a third and halved the backlog (`docs/benchmarks-aws.md`, section 27).

```bash
export FORGECART_DATABASE_URL='postgres://USER:PASSWORD@127.0.0.1:5432/forgecart?sslmode=disable'  # ForgeCart's own database
export FORGECART_FORGE_URL='http://127.0.0.1:8080'     # Forge's job API
export FORGECART_FORGE_API_KEY='<Forge FORGE_API_KEY>'  # Forge's submit key (a secret)
go run ./forgecart/cmd/forgecart-dispatcher
```

Forge must accept the four job types: start it in remote mode with `FORGE_EXECUTION_MODE=remote FORGE_REMOTE_JOB_TYPES=inventory.reconcile,fulfillment.prepare,invoice.generate,notification.send` (plus its worker API settings). Forge's job API has **no authentication and no idempotency key** (verified in its source), so the dispatcher sends neither and does not use an API key; put Forge behind your own network controls. Never expose it publicly.

| Variable | Default | Rule |
|---|---|---|
| `FORGECART_DATABASE_URL` | required | postgres URL |
| `FORGECART_FORGE_URL` | required | `http(s)://host[:port]`, no credentials, path or query |
| `FORGECART_FORGE_API_KEY` | required (dispatcher) | Forge's submit key (`FORGE_API_KEY`); sent as a bearer token; never logged |
| `FORGECART_DISPATCHER_POLL_INTERVAL` | `1s` | 100ms-1m, wait when idle |
| `FORGECART_DISPATCHER_BATCH_SIZE` | `10` | 1-100; events claimed per round, and the size of the batch sent to Forge |
| `FORGECART_DISPATCHER_LEASE_DURATION` | `30s` | at least HTTP timeout + 5s, at most 1h |
| `FORGECART_DISPATCHER_HTTP_TIMEOUT` | `10s` | 100ms-2m |
| `FORGECART_DISPATCHER_MAX_ATTEMPTS` | `8` | 1-100 |

**Client.** The dispatcher submits through Forge's public Go client, `sdk/forgeclient`, exactly as an outside application would; it keeps only the policy table below (which Forge answer is retried, charged, uncharged or fatal) and no HTTP code of its own.

**Mapping.** For an `order.paid` event, one job of each of the four types above; for an older single-job event, one job of its own type. Job `input` = `{"event_id": "<outbox event id>", "order_id": "...", "payment_attempt_id": "..."}` and nothing else: no customer data. Business handlers must use `event_id` to make their work idempotent.

**Per round (batched since 2026-10-23).** Claim up to `BATCH_SIZE` events and, concurrently, validate each and record its attempt (`StartAttempts`, so none waits while its lease runs down). The ready events are then sent to Forge in **one** `POST /v1/jobs/batch` request (all or nothing, one commit in Forge), because a single submission costs Forge a durable commit under its admission lock and that rate is what limits submissions; the single answer is then applied to every event. The database updates are grouped too: `StartAttempts` records the attempt of every ready event in one statement and `MarkPublishedBatch` marks every accepted event in one statement (each event is still fenced by its own claim token, and an event that lost its lease is reported and left unchanged), because one commit per event, each waiting for the disk, capped the dispatcher near 300 events per second on AWS. Rarer outcomes (retry, backpressure, failure) are still applied per event. If Forge rejects the whole batch as invalid (`400`, naming one item), nothing was stored and each event is sent alone with the same recorded attempt, so only the bad event fails. A batch has no idempotency key, so an uncertain outcome (timeout, lost response) may have stored the whole batch and the events are sent again: the jobs must tolerate duplicates through `event_id`, as before. The rules below apply to each event: `StartAttempts` immediately before the request (no database transaction is open during the HTTP call), POST, then `MarkPublished` on a `202` whose body has a Forge `job_id`. **Published means Forge accepted the job. It does not mean the job ran or succeeded.** `attempts` counts attempts the dispatcher *started*; a crash between `StartAttempts` and the send consumes an attempt without any request reaching Forge.

**Retry budget.** `FORGECART_DISPATCHER_MAX_ATTEMPTS` limits *charged* attempts: `attempts - uncharged_attempts`. An attempt is **uncharged** (via `RescheduleUncharged`, which also bumps `uncharged_attempts`) when it ended in a Forge-wide failure (the "fatal" row below, including `systematic_rejection`) or because the dispatcher itself was shutting down. Those say nothing about the event, so restarting a dispatcher against a misconfigured Forge any number of times cannot use up an event's budget or make it `failed`; `attempts` still counts every attempt started. Only retryable failures (timeouts, network errors, 408, 429, 5xx) are charged, and the budget is checked both when such a failure happens and when an event is claimed. **Forge's `queue_full` answer (503, or 429, with the error code `queue_full`) is not one of them:** it means Forge stored nothing and is temporarily at capacity, so it is flow control, not a failure of the event. Such an attempt is uncharged, the event stays `pending` with `last_error_code = forge_queue_full`, and no number of full-queue answers can make it `failed`. Its retry delay is Forge's `Retry-After` (5 seconds if the header is missing or invalid; never less than 1 second) plus up to 50% random jitter, capped at 5 minutes, and the dispatcher also pauses claiming for that long so it stops submitting other events into a full queue. When Forge has capacity again the events flow in their original order of readiness. A persistently full queue therefore shows up as a growing pile of `pending` events, not as lost ones; there is still no alert or status endpoint for it (see Limits). After a Forge-wide failure the event is due again in 10 seconds (fixed, not exponential) so it flows as soon as the problem is fixed. If the database call that records the uncharge itself fails, the attempt stays charged (the conservative direction) and lease expiry recovers the event.

| Result from Forge | Action |
|---|---|
| `202` + `job_id` | publish |
| network error, timeout, other 5xx, 408 | retry later, charged to the budget (capped exponential backoff on the charged count, 1s doubling to 5m, 50-100% jitter); **outcome is uncertain** |
| 503 or 429 with error code `admission_busy` | **admission backpressure**: not published, rescheduled as an *uncharged* attempt after `Retry-After` (min 1s, default 1s, +up to 50% jitter, max 5m); **claiming does not pause**, so unrelated events continue; never fails the event by itself |
| 503 or 429 with error code `queue_full` | **backpressure**: not published, rescheduled as an *uncharged* attempt after `Retry-After` (default 5s, min 1s, +up to 50% jitter, max 5m); claiming pauses for the same time; never fails the event by itself |
| 429 (any other) | retry, charged; a valid `Retry-After` (seconds or HTTP date, capped at 5m) replaces the backoff |
| 400 / 413 except `unsupported_job_type` | the event is rejected: mark `failed` (`rejected_<forge error code>`) |
| max attempts reached (also checked at claim time) or malformed payload | mark `failed` (`attempts_exhausted`, `invalid_payload`) |
| 401, 403, 404, 405, redirects (never followed), unexpected statuses, a `202` that is not a Forge job response, 400 `unsupported_job_type` | **fatal**: the event is rescheduled (not failed) as an *uncharged* attempt, the dispatcher stops claiming and exits with status 1 |

`unsupported_job_type` is treated as fatal rather than a bad event because the dispatcher only sends its four known types: it means Forge is not set up for ForgeCart, so every event would fail identically. Likewise, 5 consecutive rejections with no success between them stop the dispatcher (`systematic_rejection`) instead of failing the rest of the outbox; up to 4 events can already be marked failed by then.

**Uncertain outcomes and duplicates.** Because Forge cannot deduplicate submissions, a timeout or lost response may mean the job exists, and the retry creates a second job. If Forge accepts but ForgeCart cannot record it (database error, or the lease was lost or expired), the event is left to be reclaimed and submitted again. Publication is therefore **at-least-once**, and the same `event_id` appears in each duplicate job. Database errors are never treated as proof that Forge rejected anything; state changes that cannot be recorded are logged (`accepted_but_unrecorded`, `accepted_but_lease_lost`, `database_error`) and left to lease expiry.

**Shutdown.** On SIGINT/SIGTERM the dispatcher stops claiming; in-flight events get 5 seconds, after which their requests are cancelled. Cancelled requests are recorded as the uncertain `interrupted_by_shutdown` and rescheduled as uncharged attempts, never as failed, because the job may have been created. Each database update has a 5s deadline. Logs are JSON with event ID, type, operation, outcome and retry delay; they contain no payloads, URLs, credentials or response bodies.

**Limits.** No status endpoint, no metrics, no dead-letter replay (a `failed` event stays failed), no lease renewal, no worker for the jobs.

### Inventory reconciliation worker (`cmd/forgecart-inventory-worker`)

The first business worker. It runs `inventory.reconcile` jobs, using the Forge Go SDK (`sdk/workerclient`, `sdk/workerruntime`) against Forge's worker API. It is a separate process from the API and the dispatcher; run any number of replicas. It talks to Forge only over the worker API and never reads Forge's database.

**What it does.** For a paid order it *verifies and records*; it does **not** change stock. Checkout already deducted the stock and payment success only marked the reservation `committed`; a decline already releases stock in its own transaction. Reconciliation repeats neither. In one transaction it checks that: the outbox event exists, is an `inventory.reconcile` event and names exactly the input's order and payment attempt; the order is `paid`/`committed`; the payment attempt belongs to the order and `succeeded` (and is the order's only succeeded attempt); the order has lines with positive quantities whose line totals add up to the order subtotal, over products with non-negative stock. It then inserts one row in `inventory_reconciliations` (outcome `verified`, number of lines and units inspected).

**Idempotency.** The primary key is the outbox `event_id`, and `order_id` is also unique. Redelivery (a Forge retry, a duplicate submission from an uncertain dispatcher publish, concurrent runs) sees the stored row and succeeds as a duplicate (`"duplicate": true` in the job result) **only if** it describes the same order and payment attempt. The same `event_id` with different data is rejected (`conflicts_with_recorded_event`) and the stored row is never overwritten. Concurrent first deliveries race on the primary key; exactly one inserts.

**Failure classification** (through the runtime's `Permanent`): malformed input (bad JSON, unknown or missing fields, malformed IDs) and every confirmed contradiction (unknown event, wrong event type, event/input mismatch, order not paid and committed, attempt not succeeded or of another order, inconsistent lines) are **permanent**: Forge fails the job at once and nothing is recorded. Database errors, timeouts and cancellation are returned unmarked, so Forge retries them. On shutdown a running job is released by the runtime. Job results contain only counts, and error text is never sent or logged.

| Variable | Default | Meaning |
|---|---|---|
| `FORGECART_DATABASE_URL` | required | ForgeCart's own database |
| `FORGECART_RESULTS_DATABASE_URL` | none | optional second database for the workers' result rows (inventory reconciliations, invoices, notification deliveries, fulfillment plans). Unset = they are written to `FORGECART_DATABASE_URL`. Apply the same migrations to it; the workers still read the order from `FORGECART_DATABASE_URL`. See below |
| `FORGECART_FORGE_WORKER_URL` | required | Forge's worker API address (`FORGE_WORKER_ADDR`), e.g. `http://127.0.0.1:8081` |
| `FORGECART_FORGE_WORKER_API_KEY` | required | Forge's shared worker key (`FORGE_WORKER_API_KEY`). A secret: never logged or printed, but it is a development stand-in, not real authentication |
| `FORGECART_WORKER_NAME` | `forgecart-inventory` | label in worker IDs and logs |
| `FORGECART_WORKER_CONCURRENCY` | `4` | handlers at once, 1 to 64 (the worker SDK's limit) |
| `FORGECART_WORKER_POLL_WAIT` | SDK default (20 s) | how long one claim request waits for work, whole seconds 1s-25s |
| `FORGECART_WORKER_SHUTDOWN_GRACE` | SDK default (30 s) | how long to wait for running jobs when stopping, up to 1h |
| `FORGECART_WORKER_JOB_TIMEOUT` | none | worker-side cap on one attempt (`workerruntime.WithTimeout`); it can only shorten the job's own timeout, minimum 1s |

**Results database (optional).** A worker reads the event, the paid order, its lines and its payment attempt from the ForgeCart database with plain statements, then writes its result row (and the line snapshots) in one transaction on the database named by `FORGECART_RESULTS_DATABASE_URL`, or on the ForgeCart database when that is unset. The read and the write are two steps, not one transaction: nothing is locked across them, because a paid order never changes afterwards, and a redelivered job is made harmless by `UNIQUE(event_id)` and `UNIQUE(order_id)` on the result row. Because PostgreSQL cannot enforce a foreign key across two databases, migration 15 removes the result tables' foreign keys to `orders`, `payment_attempts` and `outbox_events` in both modes; the second database gets the same migrations (its `orders` and `outbox_events` stay empty). The result tables in the ForgeCart database are then unused. Measurement (`docs/benchmarks-aws.md`, section 18): moving the result rows to their own machine raised the end-to-end rate on the AWS setup by about 12 to 18%, not more, because the ForgeCart database was still at 99% CPU.

**Per-job-type policy.** The retry budget and per-attempt timeout of each job type live in Forge's server-side config file, `forgecart/forge.json` (`FORGE_CONFIG_FILE`; the test harness starts Forge with it and the dispatcher sends no options). Values: `inventory.reconcile` 5 attempts / 20 s, `invoice.generate` and `fulfillment.prepare` 5 / 15 s, `notification.send` 5 / 10 s. These are starting values chosen by reasoning about each handler (short idempotent database writes), not tuned optima; Forge's own defaults (3 attempts, 30 s) apply to any field a type leaves out. (Before the config file existed, the dispatcher sent these values with every job; the first tuning read below was taken in that form.)

**First tuning read (one full regression run, `fullpost11`; single trial, not a benchmark).** In the pipeline scenario (1,635 jobs, 327 per type, payment mix with 8% temporary failures) every type's attempt took p50 24-28 ms, p95 40-48 ms and p99 83-108 ms by Forge's own timestamps, with zero handler retries in the worker logs. The 10-20 s timeouts therefore have roughly 100x headroom over the slowest observed p99, and no attempt came near them; the retry budgets were not exercised by this run (the temporary failures happen in the payment step, before any job exists). Queue wait was about 0.7-1.0 s at p50-p95 while execution was about 40 ms, so jobs waited for a claim, not for a slow handler. The worker-scaling scenario in the same run shows 16 slots per type giving only 1.52x the throughput of 1 slot, so adding worker capacity is not what limits the background pipeline here; that points to Forge's claim path or the shared database, which is the candidate for the bottleneck-fix step (not yet investigated, so not a conclusion).

```bash
go run ./forgecart/cmd/forgecart-inventory-worker
```

Forge must run in remote mode with `inventory.reconcile` allowed (the dispatcher section lists all four). **Limits:** reconciliation never repairs anything, it only rejects or records; a rejected order stays unreconciled and its Forge job stays `failed` (no replay tooling); delivery is at-least-once, which is why the handler is idempotent.

### Invoice worker (`cmd/forgecart-invoice-worker`)

Runs `invoice.generate` jobs. Configuration, deployment and shutdown are the same as the inventory worker (it shares the `workerapp` main and the `bizjob` input/classification code); only the default name differs (`forgecart-invoice`):

```bash
go run ./forgecart/cmd/forgecart-invoice-worker
```

Forge must allow `invoice.generate` (see the dispatcher section). The worker is independent of the API, the dispatcher and the inventory worker.

**What it records.** One invoice per paid order, in `invoices` (header) and `invoice_items` (lines), written in one transaction:
- Header: invoice id (`inv_...`), invoice number, source `event_id`, `order_id`, `payment_attempt_id`, a snapshot of the customer email, currency, `subtotal_minor`, `total_minor` (integer minor units), `issued_at`, `created_at`. `event_id`, `order_id` and `invoice_number` are each UNIQUE.
- Lines: numbered 1..n in SKU order, with SKU, product name, quantity, unit price, currency and line total copied from the order lines. There is deliberately no foreign key to `products`: later catalog changes cannot affect an issued invoice.
- The invoice number is `INV-` plus the order's 24 hex digits in upper case: deterministic, one per order, never changed by a redelivery.
- Triggers refuse any `UPDATE` or `DELETE` on both tables, so issued invoices are immutable at the database level.

**Verification before issuing** (plain reads on the shop database, before the result transaction; nothing is locked): the outbox event exists, is exactly `invoice.generate`, and names the input's order and payment attempt; the order is `paid`/`committed`; the attempt belongs to the order, `succeeded` and is its only succeeded attempt; the order has lines, all in the order's currency with positive quantities, that sum to the stored subtotal, and `total >= subtotal`. Any contradiction rejects the job and writes nothing.

**Idempotency.** `event_id` is the delivery key. A redelivery for the same order and payment attempt returns the existing invoice (`"duplicate": true`) and changes nothing, including the issue time. The same event with another order or attempt is rejected (`conflicts_with_issued_invoice`). A different event for an already-invoiced order is rejected (`order_already_invoiced_by_other_event`; the outbox itself allows only one `invoice.generate` event per order, and `UNIQUE(order_id)` is the backstop). Concurrent first deliveries race on `UNIQUE(event_id)`; one wins.

**Errors.** Malformed input and confirmed contradictions are permanent (Forge fails the job at once). Database errors, timeouts and cancellation are returned as ordinary errors, so Forge retries them; a failure at any point rolls the whole invoice back, so there is never a header without its lines. The job result holds only the invoice id, number, order id and the duplicate flag: no customer details are returned or logged.

**Limitations.** These are demo records, **not PDF documents**, and there is no tax, discount, legal numbering or jurisdiction-specific e-invoicing. The order must already be paid; nothing sends or renders the invoice. There is no invoice-reading endpoint yet.

### Notification worker (`cmd/forgecart-notification-worker`)

Runs `notification.send` jobs. **Delivery is simulated: no email is sent, no SMTP, email API or any other external service is contacted.** The job only records a receipt in ForgeCart's database. Configuration, launch and shutdown are the same as the other workers (shared `workerapp`); the default name is `forgecart-notification`:

```bash
go run ./forgecart/cmd/forgecart-notification-worker
```

Forge must allow `notification.send` (see the dispatcher section). It is independent of the API, the dispatcher and the other workers.

**What it records.** One row per order in `notification_deliveries`: id (`ntf_...`), source `event_id`, `order_id`, `payment_attempt_id`, template `order_confirmation` (the only accepted value), a snapshot of the order's customer email as `recipient_email`, status `simulated_sent` and its timestamp (`status_at`), and `created_at`. `simulated_sent` means only that ForgeCart recorded the simulated delivery. `event_id` is UNIQUE and so is `(order_id, template)`, and the recipient, id format, template and status are CHECKed.

**Verification** (shared with the other workers in `bizjob.VerifyPaidOrder`: plain reads on the shop database, nothing locked): the event exists, is exactly `notification.send` and names the input's order and payment attempt; the order is `paid`/`committed`; the attempt belongs to the order and `succeeded` (and is its only succeeded attempt); the order has a stored email and lines, in one currency with positive quantities, that sum to the subtotal.

**Idempotency and errors.** `event_id` is the delivery key. A redelivery for the same order and attempt returns the existing delivery (`"duplicate": true`) and rewrites nothing, timestamps included. The same event with different data is rejected (`conflicts_with_recorded_delivery`); a different event for an already-confirmed order is rejected (`order_already_confirmed_by_other_event`). Concurrent first deliveries race on `UNIQUE(event_id)`; one wins. Malformed input and confirmed contradictions are permanent (Forge fails the job); database errors, timeouts and cancellation are ordinary errors that Forge retries, and a failed transaction leaves no row. The job result holds only the delivery id, order id, status and the duplicate flag. **The recipient email is never returned or logged.**

**Limits.** Simulation only: no templates, rendering, provider, bounce or retry-to-recipient logic; a rejected order stays unconfirmed and its Forge job stays `failed`.

### Fulfillment worker (`cmd/forgecart-fulfillment-worker`)

Runs `fulfillment.prepare` jobs. **Fulfillment is simulated: there is no courier or shipping integration, no shipment, and no inventory change.** The job only records a plan of what to prepare. Configuration, launch and shutdown are the same as the other workers (shared `workerapp`); the default name is `forgecart-fulfillment`:

```bash
go run ./forgecart/cmd/forgecart-fulfillment-worker
```

Forge must allow `fulfillment.prepare` (see the dispatcher section). It is independent of the API, the dispatcher and the other workers.

**What it records.** `fulfillments`: id (`ful_...`), source `event_id`, `order_id`, `payment_attempt_id`, status `prepared`, `prepared_at`, `created_at`; `event_id` and `order_id` are each UNIQUE (one plan per event and per order). `fulfillment_items`: numbered lines (SKU, product name, quantity > 0) copied from the order's persisted lines in SKU order; no prices. The copy comes from `order_items`, never the live catalog, and is checked against them in the transaction, so renaming or re-pricing a product later does not change a plan. `prepared` means only that ForgeCart recorded a plan; nothing was packed, shipped or delivered.

**Verification** is the shared `bizjob.VerifyPaidOrder` (event exists, is exactly `fulfillment.prepare`, names the input's order and payment attempt; order `paid`/`committed`; attempt belongs to the order, `succeeded`, only succeeded attempt) plus order lines that exist, share the order's currency, have positive quantities and sum to the subtotal.

**The worker writes only the new plan and its items.** It never changes stock, order or inventory status, payment attempts or order lines.

**Idempotency and errors.** `event_id` is the key. A matching redelivery returns the existing plan (`"duplicate": true`) without rewriting it; the same event with different data is rejected (`conflicts_with_prepared_plan`); a different event for an already-prepared order is rejected (`order_already_prepared_by_other_event`). Concurrent first deliveries race on `UNIQUE(event_id)`; one wins. Malformed input and confirmed contradictions are permanent (Forge fails the job); database errors, timeouts and cancellation are ordinary errors that Forge retries, and a failed transaction leaves neither a plan nor items. The result holds the fulfillment id, order id, status, item count and the duplicate flag.

**Limits.** Simulation only: no packing, carriers, tracking, partial shipments or stock allocation. A rejected order stays unprepared and its Forge job stays `failed`.

### Load testing (`test_files/`)

Load and behaviour testing lives in one scenario-based suite, [test_files/](test_files/README.md). It replaces the former `cmd/forgecart-loadgen` HTTP generator and its live dashboard. It drives ForgeCart's public HTTP API, observes background jobs through Forge's public API, and verifies business effects with read-only queries against a **disposable sandbox** it creates itself (two PostgreSQL containers in a uniquely named Compose project, plus Forge, ForgeCart, the dispatcher and the four workers as host processes). It never touches `forge-postgres-1` or any other existing database.

```bash
./forgecart/test_files/run.sh list          # scenarios and profiles
./forgecart/test_files/run.sh preflight     # check tools, change nothing
./forgecart/test_files/run.sh dry-run --profile standard
./forgecart/test_files/run.sh run           # default profile: smoke
./forgecart/test_files/run.sh run --profile standard
./forgecart/test_files/run.sh report --from <run-id> --out <new file>   # re-render a preserved run, read-only
```

Output: `test_files/report.md` (20 sections) and the immutable `test_files/results/<run-id>/`. See the suite README for scenarios, flags, safety rules and limitations. Previous generator reports were archived under `test_files/results/archive/`.

### How checkout works

One database transaction: (1) insert the order row with `ON CONFLICT (idempotency_key) DO NOTHING`; (2) lock the requested product rows with `SELECT ... FOR UPDATE` in SKU order; (3) check active/currency/stock on the locked rows, price the order with overflow-checked integer math; (4) insert the item snapshots and decrement stock with `UPDATE ... WHERE inventory_quantity >= qty` (rows affected must be 1); (5) commit. Any failure rolls back everything, including the idempotency row, so a failed checkout leaves no order and no reservation, and the same key can be retried. Concurrent checkouts serialize on the product row locks, so stock cannot go negative or be oversold; the `CHECK (inventory_quantity >= 0)` constraint is a last line of defence.

**Idempotency.** The fingerprint is SHA-256 over the normalized request: lower-cased email plus the SKU=quantity pairs sorted by SKU (so item order and email case do not matter). Prices and stock are excluded. Two concurrent requests with the same key block on the unique index at step 1; once the first commits, the second sees the order and replays it (same fingerprint) or gets `409 idempotency_key_reused` (different one). If the connection fails during commit, the client cannot know whether the order exists; retrying with the same key is the safe answer. Keys are never expired or cleaned up.

**States.** `orders.status` is the payment lifecycle (`pending_payment`, `paid`, `payment_failed`, `cancelled`); `orders.inventory_status` is the stock reservation (`reserved`, `committed`, `released`). A database CHECK allows only these pairs: pending_payment/reserved, paid/committed, payment_failed/released, cancelled/released. Checkout creates pending_payment/reserved; payments (below) move it on. Nothing sets `cancelled` yet.

Product: `{"sku","name","unit_price_minor","currency","available_inventory","active"}`. The catalog endpoints never change prices or stock; only checkout decrements stock.

### Metrics for the shop API (`GET /metrics`, optional)

Forge's own metrics describe the background jobs. The shop's foreground side (what a shopper waits for) is described by metrics from the shop API itself. They are off unless you set both `FORGECART_METRICS_ADDR` (host:port, a different port from the shop's) and `FORGECART_METRICS_KEY` (a secret of at least 32 characters). Setting only one, using the shop's own port, or a short key stops the shop at startup with a message that names the variable. They are served on that separate listener with the key as a bearer token, never on the shop's public address, because the numbers describe the business. Plain HTTP: keep it on a trusted network.

| Metric | Labels | Meaning |
|---|---|---|
| `forgecart_http_requests_total` | `route`, `code` | Requests by fixed route name (`order_create`, `payment_attempt`, `products_list`, `product_get`, `health`, `ready`) and HTTP status. For checkout, 201 is a new order, 200 a replay, 4xx a refusal (for example out of stock) and 5xx a failure of the shop. |
| `forgecart_http_request_duration_seconds` | `route` | Latency histogram; the p50 and p95 of `order_create` and `payment_attempt` are what a shopper waits for. |
| `forgecart_payment_attempts_total` | `outcome` | New payment attempts: `succeeded`, `declined`, `temporary_failure`. A replay of an earlier attempt is not counted again. |
| `forgecart_outbox_pending_events`, `forgecart_outbox_oldest_pending_age_seconds`, `forgecart_outbox_stats_up` | | Read from the shop's database at every scrape (using the partial index on pending events): paid orders whose jobs are not yet in Forge, and how old the oldest is. Rising age means the dispatcher or Forge is not keeping up. If the read fails, `stats_up` is 0 and the two gauges are absent, never shown as zero. |
| `forgecart_http_in_flight`, `forgecart_http_in_flight_peak` | `route` | Requests of the route being handled **right now**, and the most there have been at once since the process started. For `order_create` this is how many customers are inside checkout at this moment; for `payment_attempt`, how many are paying. By Little's law it equals requests per second times seconds per request. |
| `forgecart_orders_awaiting_payment`, `forgecart_orders_awaiting_payment_oldest_age_seconds` | | Read from the database at every scrape (partial index on pending orders): customers who reached the payment step or are still deciding, as opposed to the payments in progress (which last milliseconds to seconds), and how long the longest has waited. |
| `forgecart_payment_service_calls_total` | `result` | The shop's calls to the payment service: `ok`, `final`, `mismatch`, `provider_error`, `interrupted`, `unavailable` (could not be reached or failed), `rejected` (contract or key problem). |
| `forgecart_payment_service_call_duration_seconds` | | How long the shop waited for the payment service, including the provider's time. |
| `forgecart_payment_sweep_passes_total` | `result` | Passes of the payment sweeper (`ok`, `error`). |
| `forgecart_payment_sweep_repaired_total` | | Payments the payment service recorded but the shop never applied, repaired by the sweeper. Above zero means a request died between the two steps. |
| `go_sql_*` (`db_name="forgecart"`), `go_*`, `process_*` | | The shop's database connection pool and the Go runtime. |

Counters and histograms belong to this process and restart from zero with it. The `route` label only takes the fixed names above, so a caller cannot create new series. The dashboard's "Shop (foreground)" row charts these; list the shop's metrics address in `observability/targets/shop.yml` and set `FORGECART_METRICS_KEY` in the monitoring machine's `.env`. Not covered: the dispatcher and the four workers expose no metrics of their own (Forge's per-job-type metrics describe their work), and there is no per-error-code count of refused checkouts.

### Metrics for the four background workers (`GET /metrics`, optional)

Each worker process (inventory, invoice, fulfillment, notification) has the same optional keyed listener, with the same two variables (`FORGECART_METRICS_ADDR`, `FORGECART_METRICS_KEY`), so several workers on one machine need a different port each. They say how loaded that one service is, which is the signal for scaling it on its own:

| Metric | Meaning |
|---|---|
| `forgecart_worker_slots{job_type}` | How many jobs the process can run at once (`FORGECART_WORKER_CONCURRENCY`). |
| `forgecart_worker_jobs_in_flight{job_type}`, `..._peak` | Jobs running right now, and the most at once since the process started. Jobs in flight divided by slots is the busy fraction: near 1 means this service needs more slots or copies, near 0 means more capacity here would change nothing. |
| `forgecart_worker_jobs_total{job_type,outcome}` | `succeeded`, `rejected` (the data contradicts the event, never retried) or `error` (an infrastructure failure, Forge retries it). |
| `forgecart_worker_job_duration_seconds{job_type}` | How long the handler took. |
| `go_sql_*{db_name="worker_shop"/"worker_results"}` | The connection pools to the shop and results databases. |

What waits in Forge for each job type (jobs queued, running, age of the oldest due job) comes from Forge's own metrics. The dashboard's "Capacity" row puts these side by side: concurrent checkouts, concurrent payments, orders awaiting payment, busy slots per background service, queue and oldest-job age per job type, and connections in use per service.

### Metrics for the payment service (`GET /metrics`, optional)

The payment service has the same optional metrics listener, with the same variables (`FORGECART_METRICS_ADDR`, `FORGECART_METRICS_KEY`, at least 32 characters, set together, a different port from `FORGECART_PAYMENTS_ADDR`) in its own environment. A private registry, fixed route names, counters that restart with the process:

| Metric | Labels | Meaning |
|---|---|---|
| `forgecart_paymentsvc_http_requests_total` | `route`, `code` | Requests by route (`pay`, `finals`, `health`) and HTTP status. |
| `forgecart_paymentsvc_http_request_duration_seconds` | `route` | Latency histogram; for `pay` it includes the provider's time (the simulator's delay, or Razorpay). |
| `forgecart_paymentsvc_http_in_flight`, `forgecart_paymentsvc_http_in_flight_peak` | `route` | Requests being handled now and the peak since start. For `pay` this is how many payments are in progress at this moment, including the time at the provider. |
| `forgecart_paymentsvc_attempts_total` | `outcome` | New answers: `succeeded`, `declined`, `temporary_failure` (recorded attempts) and `pending` (a Razorpay payment waiting for the customer; nothing recorded). A replay is not counted again. |
| `forgecart_paymentsvc_provider_errors_total` | | Payments that failed because Razorpay could not complete the request (nothing recorded). |
| `go_sql_*` (`db_name="payments"`), `go_*`, `process_*` | | The payment service's database pool and the Go runtime. |

List the payment service's metrics address in `observability/targets/shop.yml` next to the shop's: both are scraped by the same Prometheus job with the same key. The dashboard's "Payment service" row charts these and the shop-side metrics above.

## Limitations

No authentication, no TLS, no tests. No endpoint to read an order later (only the checkout response and its replay), and expiry of unpaid orders is a fixed time for every order (see "Expiry of unpaid orders"). Payments have no endpoint to list attempts and no refunds. A request that hits a PostgreSQL deadlock or lock wait is not retried by the server (it returns 500 and the client may retry with the same key). Unknown paths return Go's plain-text 404, and a repeated `limit`/`after` parameter uses its first value.
