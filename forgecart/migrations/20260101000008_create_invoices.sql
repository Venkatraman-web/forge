-- Demo invoice records (Task 25.4.2.2). NOT a PDF, and not a jurisdiction-specific
-- tax or e-invoicing implementation: there is no tax, discount or legal numbering.
--
-- An invoice is an immutable snapshot of a paid order, issued once per order by
-- the invoice.generate background job. event_id and order_id are both UNIQUE:
-- a redelivered job (same event) and a second event for the same order cannot
-- produce a second invoice.
CREATE TABLE invoices (
    id                 text        PRIMARY KEY,
    -- Derived from the order ID ('INV-' + its 24 hex digits, upper-case), so it
    -- is deterministic, unique per order and never changes on redelivery.
    invoice_number     text        NOT NULL,
    event_id           text        NOT NULL REFERENCES outbox_events (id),
    order_id           text        NOT NULL REFERENCES orders (id),
    payment_attempt_id text        NOT NULL REFERENCES payment_attempts (id),
    -- Snapshot of the order's customer email at issue time.
    customer_email     text        NOT NULL,
    currency           text        NOT NULL,
    subtotal_minor     bigint      NOT NULL,
    total_minor        bigint      NOT NULL,
    issued_at          timestamptz NOT NULL DEFAULT now(),
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT invoices_number_key UNIQUE (invoice_number),
    CONSTRAINT invoices_event_key UNIQUE (event_id),
    CONSTRAINT invoices_order_key UNIQUE (order_id),
    CONSTRAINT invoices_id_format CHECK (id ~ '^inv_[0-9a-f]{24}$'),
    CONSTRAINT invoices_number_format CHECK (invoice_number ~ '^INV-[0-9A-F]{24}$'),
    CONSTRAINT invoices_email_length CHECK (length(customer_email) BETWEEN 3 AND 254),
    CONSTRAINT invoices_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT invoices_amounts_valid CHECK (subtotal_minor >= 0 AND total_minor >= subtotal_minor)
);

-- Line snapshots copied from order_items. Deliberately no foreign key to
-- products: an invoice must not depend on the live catalog.
CREATE TABLE invoice_items (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invoice_id       text    NOT NULL REFERENCES invoices (id),
    line_number      integer NOT NULL,
    sku              text    NOT NULL,
    product_name     text    NOT NULL,
    quantity         integer NOT NULL,
    unit_price_minor bigint  NOT NULL,
    currency         text    NOT NULL,
    line_total_minor bigint  NOT NULL,
    CONSTRAINT invoice_items_line_key UNIQUE (invoice_id, line_number),
    CONSTRAINT invoice_items_sku_key UNIQUE (invoice_id, sku),
    CONSTRAINT invoice_items_line_positive CHECK (line_number >= 1),
    CONSTRAINT invoice_items_quantity_positive CHECK (quantity > 0),
    CONSTRAINT invoice_items_price_non_negative CHECK (unit_price_minor >= 0),
    CONSTRAINT invoice_items_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT invoice_items_line_total_valid CHECK (line_total_minor = quantity::bigint * unit_price_minor)
);

-- Immutability enforced by the database, not by convention.
CREATE FUNCTION invoices_are_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'invoices and invoice lines are immutable' USING ERRCODE = 'integrity_constraint_violation';
END $$;
CREATE TRIGGER invoices_immutable BEFORE UPDATE OR DELETE ON invoices
    FOR EACH ROW EXECUTE FUNCTION invoices_are_immutable();
CREATE TRIGGER invoice_items_immutable BEFORE UPDATE OR DELETE ON invoice_items
    FOR EACH ROW EXECUTE FUNCTION invoices_are_immutable();
