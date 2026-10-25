-- The order expirer looks for orders that stayed unpaid, oldest first. This index holds only
-- pending orders, so it stays small however many orders have been paid.
CREATE INDEX orders_pending_created_idx ON orders (created_at, id) WHERE status = 'pending_payment';
