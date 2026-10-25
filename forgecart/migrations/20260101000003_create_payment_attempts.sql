-- ForgeCart simulated payments (Task 25.4.1.3). Application-owned.
-- Order state and inventory state stay separate columns (orders.status is the
-- payment lifecycle, orders.inventory_status is the stock reservation). The
-- existing value lists from the orders migration are already correct; this adds
-- the rule that only meaningful PAIRS can exist, so no code path can, for
-- example, mark an order paid while its stock is released.
ALTER TABLE orders ADD CONSTRAINT orders_state_pairs CHECK (
    (status, inventory_status) IN (
        ('pending_payment', 'reserved'),
        ('paid', 'committed'),
        ('payment_failed', 'released'),
        ('cancelled', 'released')
    )
);

CREATE TABLE payment_attempts (
    id                 text        PRIMARY KEY,
    order_id           text        NOT NULL REFERENCES orders (id),
    -- A logical attempt is (order, key): the same key on the same order is the
    -- same attempt forever; a new key is a new attempt.
    idempotency_key    text        NOT NULL,
    -- 1, 2, 3... per order, assigned while the order row is locked.
    attempt_number     integer     NOT NULL,
    outcome            text        NOT NULL,
    -- Synthetic reference; present exactly when the payment succeeded.
    provider_reference text,
    -- How long the simulator pretended the provider took (metadata only).
    simulated_delay_ms integer     NOT NULL DEFAULT 0,
    created_at         timestamptz NOT NULL,
    completed_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payment_attempts_order_key_key UNIQUE (order_id, idempotency_key),
    CONSTRAINT payment_attempts_order_number_key UNIQUE (order_id, attempt_number),
    CONSTRAINT payment_attempts_id_format CHECK (id ~ '^pay_[0-9a-f]{24}$'),
    CONSTRAINT payment_attempts_key_format CHECK (idempotency_key ~ '^[A-Za-z0-9_.:-]{8,128}$'),
    CONSTRAINT payment_attempts_number_positive CHECK (attempt_number >= 1),
    CONSTRAINT payment_attempts_outcome_valid CHECK (outcome IN ('succeeded', 'declined', 'temporary_failure')),
    CONSTRAINT payment_attempts_reference_matches_outcome CHECK (
        (outcome = 'succeeded') = (provider_reference IS NOT NULL)
        AND (provider_reference IS NULL OR provider_reference ~ '^sim_[0-9a-f]{24}$')
    ),
    CONSTRAINT payment_attempts_delay_valid CHECK (simulated_delay_ms BETWEEN 0 AND 60000),
    CONSTRAINT payment_attempts_times_ordered CHECK (completed_at >= created_at)
);
