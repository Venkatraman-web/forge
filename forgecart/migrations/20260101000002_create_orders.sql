-- ForgeCart orders (Task 25.4.1.2). Application-owned; never touches Forge's tables.
-- Checkout creates an order in 'pending_payment' with its stock 'reserved'.
-- 'paid', 'payment_failed', 'cancelled', 'committed' and 'released' are the
-- states the payment task will move orders into; nothing sets them yet.
CREATE TABLE orders (
    id                  text        PRIMARY KEY,
    -- Client-supplied Idempotency-Key. UNIQUE is what makes a retry (or two
    -- concurrent identical requests) produce one order.
    idempotency_key     text        NOT NULL,
    -- SHA-256 of the normalized request; detects the key being reused for a
    -- different purchase.
    request_fingerprint text        NOT NULL,
    customer_email      text        NOT NULL,
    status              text        NOT NULL DEFAULT 'pending_payment',
    inventory_status    text        NOT NULL DEFAULT 'reserved',
    currency            text        NOT NULL,
    subtotal_minor      bigint      NOT NULL,
    total_minor         bigint      NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT orders_idempotency_key_key UNIQUE (idempotency_key),
    CONSTRAINT orders_id_format CHECK (id ~ '^ord_[0-9a-f]{24}$'),
    CONSTRAINT orders_idempotency_key_format CHECK (idempotency_key ~ '^[A-Za-z0-9_.:-]{8,128}$'),
    CONSTRAINT orders_fingerprint_format CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    CONSTRAINT orders_email_length CHECK (length(customer_email) BETWEEN 3 AND 254),
    CONSTRAINT orders_status_valid CHECK (status IN ('pending_payment', 'paid', 'payment_failed', 'cancelled')),
    CONSTRAINT orders_inventory_status_valid CHECK (inventory_status IN ('reserved', 'committed', 'released')),
    CONSTRAINT orders_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT orders_amounts_valid CHECK (subtotal_minor >= 0 AND total_minor >= subtotal_minor)
);

CREATE TABLE order_items (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id         text    NOT NULL REFERENCES orders (id),
    product_id       bigint  NOT NULL REFERENCES products (id),
    -- Snapshots taken at checkout: later catalog edits must not rewrite history.
    sku              text    NOT NULL,
    product_name     text    NOT NULL,
    quantity         integer NOT NULL,
    unit_price_minor bigint  NOT NULL,
    currency         text    NOT NULL,
    line_total_minor bigint  NOT NULL,
    -- One line per product per order; also indexes order_id for lookups.
    CONSTRAINT order_items_order_product_key UNIQUE (order_id, product_id),
    CONSTRAINT order_items_quantity_positive CHECK (quantity > 0),
    CONSTRAINT order_items_price_non_negative CHECK (unit_price_minor >= 0),
    CONSTRAINT order_items_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT order_items_line_total_valid CHECK (line_total_minor = quantity::bigint * unit_price_minor)
);
