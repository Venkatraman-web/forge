-- ForgeCart product catalog. Application-owned: this directory is separate from
-- Forge's own migrations/ and must be applied to the ForgeCart database only.
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
