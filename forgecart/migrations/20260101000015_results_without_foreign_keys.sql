-- The workers' result tables may live in a second database (FORGECART_RESULTS_DATABASE_URL),
-- where there are no orders, payment attempts or outbox events to point at: PostgreSQL
-- cannot enforce a foreign key across databases. The same schema is applied to both
-- databases, so these references are dropped everywhere.
--
-- What replaces them: a worker only writes a result after reading the event, the paid
-- order and its successful payment attempt from the shop database (bizjob.VerifyPaidOrder),
-- and UNIQUE(event_id) / UNIQUE(order_id) still allow at most one result per event and order.
-- The load-test harness checks the other direction (no result for an unpaid order).
ALTER TABLE inventory_reconciliations
    DROP CONSTRAINT inventory_reconciliations_event_id_fkey,
    DROP CONSTRAINT inventory_reconciliations_order_id_fkey,
    DROP CONSTRAINT inventory_reconciliations_payment_attempt_id_fkey;
ALTER TABLE invoices
    DROP CONSTRAINT invoices_event_id_fkey,
    DROP CONSTRAINT invoices_order_id_fkey,
    DROP CONSTRAINT invoices_payment_attempt_id_fkey;
ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_event_id_fkey,
    DROP CONSTRAINT notification_deliveries_order_id_fkey,
    DROP CONSTRAINT notification_deliveries_payment_attempt_id_fkey;
ALTER TABLE fulfillments
    DROP CONSTRAINT fulfillments_event_id_fkey,
    DROP CONSTRAINT fulfillments_order_id_fkey,
    DROP CONSTRAINT fulfillments_payment_attempt_id_fkey;
