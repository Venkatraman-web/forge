-- One outbox event per paid order (event type 'order.paid') instead of one per
-- background job. The dispatcher turns it into the four Forge jobs when it
-- publishes. Each event was written four times (inserted, leased, attempt
-- counted, marked published), so an order caused 16 row writes on this table,
-- about 40% of the shop database's statement time at saturation; now it causes 4.
--
-- The four job types stay allowed so rows written before this change remain
-- valid and are still published (one job each). The unique (order_id, event_type)
-- rule still means one 'order.paid' event per order.
ALTER TABLE outbox_events DROP CONSTRAINT outbox_events_type_valid;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_type_valid CHECK (event_type IN (
    'order.paid', 'inventory.reconcile', 'fulfillment.prepare', 'invoice.generate', 'notification.send'));
