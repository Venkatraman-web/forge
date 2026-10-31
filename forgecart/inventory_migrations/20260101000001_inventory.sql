-- ForgeCart inventory database: the product catalogue, the stock, and the stock reservations of
-- the checkout saga (docs/decisions/0005-inventory-service-and-checkout-saga.md). Application-owned:
-- this directory is separate from Forge's own migrations/ and from the orders database's
-- forgecart/migrations/, and must be applied to the INVENTORY database only.
CREATE TABLE products (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sku                text        NOT NULL,
    name               text        NOT NULL,
    -- Integer minor units (cents for USD). Never floating point.
    unit_price_minor   bigint      NOT NULL,
    currency           text        NOT NULL DEFAULT 'USD',
    inventory_quantity integer     NOT NULL,
    active             boolean     NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT products_sku_key UNIQUE (sku),
    CONSTRAINT products_sku_format CHECK (sku ~ '^[A-Z0-9][A-Z0-9-]{2,63}$'),
    CONSTRAINT products_name_not_blank CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    CONSTRAINT products_price_non_negative CHECK (unit_price_minor >= 0),
    CONSTRAINT products_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT products_inventory_non_negative CHECK (inventory_quantity >= 0)
);

-- A reservation holds the stock of one order between the decrement and the order's fate: 'reserved' until
-- the order is known to be paid ('committed', set in batches by the reaper) or given up ('released').
-- The order id is the primary key, so reserving twice for one order takes the stock once; releasing
-- only acts on a reservation that is still 'reserved', so it returns the stock once and never for a
-- paid order. `lines` is the reserved lines, [{"product_id": 1, "sku": "KB-1", "quantity": 2, ...}], in SKU order.
CREATE TABLE stock_reservations (
    order_id    text        PRIMARY KEY,
    status      text        NOT NULL DEFAULT 'reserved',
    lines       jsonb       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    released_at timestamptz,
    CONSTRAINT stock_reservations_order_id_format CHECK (order_id ~ '^ord_[0-9a-f]{24}$'),
    CONSTRAINT stock_reservations_status_valid CHECK (status IN ('reserved', 'committed', 'released')),
    CONSTRAINT stock_reservations_released_matches CHECK ((status = 'released') = (released_at IS NOT NULL))
);

-- The reaper walks the reservations that stayed 'reserved' too long, in this order, with a cursor.
CREATE INDEX stock_reservations_reserved_idx ON stock_reservations (created_at, order_id) WHERE status = 'reserved';
