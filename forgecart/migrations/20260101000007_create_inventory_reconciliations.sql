-- Result of the inventory.reconcile background job (Task 25.4.2.1).
-- One row per outbox event: the primary key on event_id is what makes duplicate
-- deliveries of the same job produce a single business effect. The job only
-- VERIFIES that the paid order's reservation is consistent; it never changes
-- stock, so this table is the whole effect.
CREATE TABLE inventory_reconciliations (
    event_id           text        PRIMARY KEY REFERENCES outbox_events (id),
    order_id           text        NOT NULL REFERENCES orders (id),
    payment_attempt_id text        NOT NULL REFERENCES payment_attempts (id),
    outcome            text        NOT NULL,
    -- What was inspected: distinct order lines and total units.
    items_checked      integer     NOT NULL,
    units_checked      integer     NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    -- An order has exactly one inventory.reconcile event, so also one result.
    CONSTRAINT inventory_reconciliations_order_key UNIQUE (order_id),
    CONSTRAINT inventory_reconciliations_outcome_valid CHECK (outcome IN ('verified')),
    CONSTRAINT inventory_reconciliations_counts_valid CHECK (items_checked > 0 AND units_checked >= items_checked)
);
