-- Local analytics ledger for the analytics.order_placed job (Task 25.4.2.5): one
-- immutable-by-convention row per paid order. Recorded in ForgeCart's own
-- database; nothing is sent to an analytics platform.
CREATE TABLE analytics_order_placed (
    id                 text        PRIMARY KEY,
    -- One record per outbox event, and one per order for this event type.
    event_id           text        NOT NULL REFERENCES outbox_events (id),
    order_id           text        NOT NULL REFERENCES orders (id),
    payment_attempt_id text        NOT NULL REFERENCES payment_attempts (id),
    currency           text        NOT NULL,
    -- orders.total_minor at the time, in integer minor units.
    order_value_minor  bigint      NOT NULL,
    -- Distinct order lines, and the sum of their quantities.
    line_count         integer     NOT NULL,
    unit_count         integer     NOT NULL,
    -- When the order was placed (paid): the successful attempt's completion time.
    occurred_at        timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT analytics_order_placed_event_key UNIQUE (event_id),
    CONSTRAINT analytics_order_placed_order_key UNIQUE (order_id),
    CONSTRAINT analytics_order_placed_id_format CHECK (id ~ '^ane_[0-9a-f]{24}$'),
    CONSTRAINT analytics_order_placed_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT analytics_order_placed_value_valid CHECK (order_value_minor >= 0),
    CONSTRAINT analytics_order_placed_counts_valid CHECK (line_count >= 1 AND unit_count >= line_count)
);
