-- Simulated order-confirmation deliveries (Task 25.4.2.3). No email is ever
-- sent: a row with status 'simulated_sent' only means ForgeCart recorded that the
-- notification.send job ran for a paid order.
CREATE TABLE notification_deliveries (
    id                 text        PRIMARY KEY,
    -- One delivery per outbox event, and one confirmation per order.
    event_id           text        NOT NULL REFERENCES outbox_events (id),
    order_id           text        NOT NULL REFERENCES orders (id),
    payment_attempt_id text        NOT NULL REFERENCES payment_attempts (id),
    template           text        NOT NULL,
    -- Snapshot of the order's customer email. Never logged or returned by the job.
    recipient_email    text        NOT NULL,
    status             text        NOT NULL,
    status_at          timestamptz NOT NULL DEFAULT now(),
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notification_deliveries_event_key UNIQUE (event_id),
    CONSTRAINT notification_deliveries_order_template_key UNIQUE (order_id, template),
    CONSTRAINT notification_deliveries_id_format CHECK (id ~ '^ntf_[0-9a-f]{24}$'),
    CONSTRAINT notification_deliveries_template_valid CHECK (template IN ('order_confirmation')),
    CONSTRAINT notification_deliveries_recipient_valid CHECK (length(recipient_email) BETWEEN 3 AND 254 AND recipient_email LIKE '_%@_%'),
    CONSTRAINT notification_deliveries_status_valid CHECK (status IN ('simulated_sent'))
);
