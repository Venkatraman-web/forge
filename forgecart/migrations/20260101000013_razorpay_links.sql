-- Optional Razorpay provider (FORGECART_PAYMENT_PROVIDER=razorpay). Application-owned.
--
-- One Razorpay payment link per order: the first payment request for an order
-- creates the link and stores it here, so every later request reuses the same
-- link instead of creating another.
CREATE TABLE razorpay_links (
    order_id   text        PRIMARY KEY REFERENCES orders (id),
    link_id    text        NOT NULL,
    short_url  text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT razorpay_links_link_key UNIQUE (link_id),
    CONSTRAINT razorpay_links_link_format CHECK (link_id ~ '^plink_[A-Za-z0-9]{1,40}$'),
    CONSTRAINT razorpay_links_url_format CHECK (short_url ~ '^https://')
);

-- A successful payment's provider reference was always the simulator's
-- "sim_..." value. Razorpay's payment IDs look like "pay_<letters and digits>",
-- so both shapes are allowed now; the rule that a reference exists exactly when
-- the payment succeeded is unchanged.
ALTER TABLE payment_attempts DROP CONSTRAINT payment_attempts_reference_matches_outcome;
ALTER TABLE payment_attempts ADD CONSTRAINT payment_attempts_reference_matches_outcome CHECK (
    (outcome = 'succeeded') = (provider_reference IS NOT NULL)
    AND (provider_reference IS NULL OR provider_reference ~ '^(sim_[0-9a-f]{24}|pay_[A-Za-z0-9]{1,40})$')
);
