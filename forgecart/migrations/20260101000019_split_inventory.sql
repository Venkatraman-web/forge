-- The product catalogue and the stock moved to their own database (inventory_migrations/ and
-- docs/decisions/0005-inventory-service-and-checkout-saga.md). An order item keeps the product id and
-- the SKU, name and price it snapshots, but no longer references a table in this database.
--
-- WARNING: this drops the products table of THIS database. Copy it into the inventory database before
-- applying this migration to a database that holds a catalogue you want to keep.
ALTER TABLE order_items DROP CONSTRAINT order_items_product_id_fkey;
DROP TABLE products;
