-- Synthetic starter catalog. Prices are USD cents. ON CONFLICT DO NOTHING means
-- re-running never resets stock (or anything else) changed since the first run.
INSERT INTO products (sku, name, unit_price_minor, currency, inventory_quantity) VALUES
    ('KB-WL-001',  'Wireless Keyboard',        4999, 'USD', 50),
    ('HUB-USBC-7', 'USB-C 7-in-1 Hub',         3999, 'USD', 40),
    ('STAND-LP-2', 'Aluminium Laptop Stand',   2999, 'USD', 30),
    ('MOUSE-ERG-1','Ergonomic Wireless Mouse', 2499, 'USD', 60),
    ('MON-ARM-1',  'Monitor Arm (single)',     5999, 'USD', 20)
ON CONFLICT (sku) DO NOTHING;
