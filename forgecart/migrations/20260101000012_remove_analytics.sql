-- ForgeCart no longer has an analytics job: in a real shop, analytics is an
-- event pipeline outside the transactional path, not a step of an order.
-- Removes its table and its events, and tightens the event-type constraint to
-- the four remaining job types. Deleting the events matters: a pending
-- analytics event would be submitted to Forge as a job type Forge no longer
-- declares and be rejected.
DELETE FROM outbox_events WHERE event_type = 'analytics.order_placed';
DROP TABLE analytics_order_placed;
ALTER TABLE outbox_events DROP CONSTRAINT outbox_events_type_valid;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_type_valid CHECK (event_type IN (
    'inventory.reconcile', 'fulfillment.prepare', 'invoice.generate', 'notification.send'));
