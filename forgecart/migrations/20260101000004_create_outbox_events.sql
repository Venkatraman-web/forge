-- ForgeCart transactional outbox (Task 25.4.1.4). Application-owned.
-- A row is durable "publication intent": a successful payment inserts five rows
-- in the same transaction that marks the order paid. A later dispatcher task
-- turns rows into Forge jobs. Nothing in this migration backfills old orders.
CREATE TABLE outbox_events (
    -- Stable identity: never changes across publication retries, so it can be
    -- used as the correlation / de-duplication key on the Forge job and in the
    -- business effect.
    id              text        PRIMARY KEY,
    order_id        text        NOT NULL REFERENCES orders (id),
    event_type      text        NOT NULL,
    -- Small reference payload ({"order_id", "payment_attempt_id"}); workers load
    -- business data from ForgeCart's database by order ID.
    payload         jsonb       NOT NULL,
    status          text        NOT NULL DEFAULT 'pending',
    -- Publication attempts so far (the dispatcher increments it).
    attempts        integer     NOT NULL DEFAULT 0,
    -- When the event is next eligible; a dispatcher backs this off after a
    -- transient failure.
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    -- Minimal claim metadata for several dispatchers: who holds the event and
    -- until when. An expired lease means the holder is presumed crashed.
    -- Nothing sets these in this task.
    leased_by       text,
    lease_expires_at timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,
    -- One event of each type per order: the final guard against duplicate sets.
    CONSTRAINT outbox_events_order_type_key UNIQUE (order_id, event_type),
    CONSTRAINT outbox_events_id_format CHECK (id ~ '^evt_[0-9a-f]{24}$'),
    CONSTRAINT outbox_events_type_valid CHECK (event_type IN (
        'inventory.reconcile', 'fulfillment.prepare', 'invoice.generate',
        'notification.send', 'analytics.order_placed')),
    CONSTRAINT outbox_events_payload_shape CHECK (
        jsonb_typeof(payload) = 'object'
        AND octet_length(payload::text) <= 1024
        AND payload ->> 'order_id' = order_id),
    -- 'pending' covers new, leased and retry-waiting events; the dispatcher
    -- task may add a terminal failure status if it needs one.
    CONSTRAINT outbox_events_status_valid CHECK (status IN ('pending', 'published')),
    CONSTRAINT outbox_events_attempts_non_negative CHECK (attempts >= 0),
    CONSTRAINT outbox_events_published_matches_status CHECK ((status = 'published') = (published_at IS NOT NULL)),
    CONSTRAINT outbox_events_lease_pair CHECK ((leased_by IS NULL) = (lease_expires_at IS NULL)),
    CONSTRAINT outbox_events_lease_only_pending CHECK (status = 'pending' OR leased_by IS NULL)
);

-- The dispatcher's query is "pending events that are due, oldest first".
CREATE INDEX outbox_events_pending_due_idx ON outbox_events (next_attempt_at, id) WHERE status = 'pending';
