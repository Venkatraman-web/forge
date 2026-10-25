-- Simulated fulfillment plans (Task 25.4.2.4). 'prepared' means only that
-- ForgeCart recorded a plan listing what to prepare. Nothing is packed, shipped
-- or delivered, and no courier or inventory system is involved.
CREATE TABLE fulfillments (
    id                 text        PRIMARY KEY,
    -- One plan per outbox event, and one plan per order.
    event_id           text        NOT NULL REFERENCES outbox_events (id),
    order_id           text        NOT NULL REFERENCES orders (id),
    payment_attempt_id text        NOT NULL REFERENCES payment_attempts (id),
    status             text        NOT NULL,
    prepared_at        timestamptz NOT NULL DEFAULT now(),
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fulfillments_event_key UNIQUE (event_id),
    CONSTRAINT fulfillments_order_key UNIQUE (order_id),
    CONSTRAINT fulfillments_id_format CHECK (id ~ '^ful_[0-9a-f]{24}$'),
    CONSTRAINT fulfillments_status_valid CHECK (status IN ('prepared'))
);

-- Snapshot of the order lines to prepare, copied from order_items (not from the
-- live catalog). No prices: the plan does not need them.
CREATE TABLE fulfillment_items (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    fulfillment_id text    NOT NULL REFERENCES fulfillments (id),
    line_number    integer NOT NULL,
    sku            text    NOT NULL,
    product_name   text    NOT NULL,
    quantity       integer NOT NULL,
    CONSTRAINT fulfillment_items_line_key UNIQUE (fulfillment_id, line_number),
    CONSTRAINT fulfillment_items_sku_key UNIQUE (fulfillment_id, sku),
    CONSTRAINT fulfillment_items_line_positive CHECK (line_number >= 1),
    CONSTRAINT fulfillment_items_quantity_positive CHECK (quantity > 0)
);
