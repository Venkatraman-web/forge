-- The payment service's own database (ForgeCart payment service). It holds no orders: an
-- attempt names its order by ID and carries the amount it was asked to charge, so there is
-- no foreign key to the order service's database.
--
-- A logical attempt is (order, key): the same key on the same order is the same attempt
-- forever; a new key is a new attempt.
CREATE TABLE payment_attempts (
    id                 text        PRIMARY KEY,
    order_id           text        NOT NULL,
    idempotency_key    text        NOT NULL,
    -- 1, 2, 3... per order, assigned while the order's advisory lock is held.
    attempt_number     integer     NOT NULL,
    amount_minor       bigint      NOT NULL,
    currency           text        NOT NULL,
    outcome            text        NOT NULL,
    -- The provider's reference; present exactly when the payment succeeded. The simulator's
    -- is "sim_..." and Razorpay's payment IDs look like "pay_<letters and digits>".
    provider_reference text,
    -- How long the simulator pretended the provider took (metadata only).
    simulated_delay_ms integer     NOT NULL DEFAULT 0,
    created_at         timestamptz NOT NULL,
    completed_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payment_attempts_order_key_key UNIQUE (order_id, idempotency_key),
    CONSTRAINT payment_attempts_order_number_key UNIQUE (order_id, attempt_number),
    CONSTRAINT payment_attempts_id_format CHECK (id ~ '^pay_[0-9a-f]{24}$'),
    CONSTRAINT payment_attempts_order_format CHECK (order_id ~ '^ord_[0-9a-f]{24}$'),
    CONSTRAINT payment_attempts_key_format CHECK (idempotency_key ~ '^[A-Za-z0-9_.:-]{8,128}$'),
    CONSTRAINT payment_attempts_number_positive CHECK (attempt_number >= 1),
    CONSTRAINT payment_attempts_amount_valid CHECK (amount_minor >= 0),
    CONSTRAINT payment_attempts_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT payment_attempts_outcome_valid CHECK (outcome IN ('succeeded', 'declined', 'temporary_failure')),
    CONSTRAINT payment_attempts_reference_matches_outcome CHECK (
        (outcome = 'succeeded') = (provider_reference IS NOT NULL)
        AND (provider_reference IS NULL OR provider_reference ~ '^(sim_[0-9a-f]{24}|pay_[A-Za-z0-9]{1,40})$')
    ),
    CONSTRAINT payment_attempts_delay_valid CHECK (simulated_delay_ms BETWEEN 0 AND 60000),
    CONSTRAINT payment_attempts_times_ordered CHECK (completed_at >= created_at)
);

-- An order is paid at most once and declined at most once, and the database says so: a second
-- final attempt for the same order cannot be inserted whatever the application does.
CREATE UNIQUE INDEX payment_attempts_one_success_key ON payment_attempts (order_id) WHERE outcome = 'succeeded';
CREATE UNIQUE INDEX payment_attempts_one_decline_key ON payment_attempts (order_id) WHERE outcome = 'declined';

-- Optional Razorpay provider (FORGECART_PAYMENT_PROVIDER=razorpay): one payment link per order.
-- The first request for an order creates the link and stores it here, so every later request
-- reuses it instead of creating another.
CREATE TABLE razorpay_links (
    order_id   text        PRIMARY KEY,
    link_id    text        NOT NULL,
    short_url  text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT razorpay_links_link_key UNIQUE (link_id),
    CONSTRAINT razorpay_links_order_format CHECK (order_id ~ '^ord_[0-9a-f]{24}$'),
    CONSTRAINT razorpay_links_link_format CHECK (link_id ~ '^plink_[A-Za-z0-9]{1,40}$'),
    CONSTRAINT razorpay_links_url_format CHECK (short_url ~ '^https://')
);
