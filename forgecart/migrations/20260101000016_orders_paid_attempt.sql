-- The payment service (its own database) now owns payment attempts. The order records which
-- attempt made it paid, set in the same transaction as the paid state and the outbox event,
-- so the background jobs can check the event against the order without reading the payment
-- service's database.
--
-- The old payment_attempts and razorpay_links tables stay in this database, unused: the payment
-- service has its own copies (payments_migrations/). Nothing here is dropped.
ALTER TABLE orders ADD COLUMN paid_attempt_id text;

-- Orders paid before the split: take the attempt from the old table.
UPDATE orders o SET paid_attempt_id = pa.id
FROM payment_attempts pa
WHERE pa.order_id = o.id AND pa.outcome = 'succeeded' AND o.status = 'paid';

ALTER TABLE orders ADD CONSTRAINT orders_paid_attempt_format CHECK (paid_attempt_id IS NULL OR paid_attempt_id ~ '^pay_[0-9a-f]{24}$');
