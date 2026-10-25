-- Retry budget fix (Task 25.4.1.6.1). `attempts` keeps its meaning: publication
-- attempts STARTED. `uncharged_attempts` counts those that ended for a reason
-- unrelated to the event itself (a Forge-wide configuration or protocol failure,
-- or the dispatcher shutting down), so they must not use up the event's retry
-- budget. Budget used = attempts - uncharged_attempts. last_error_code alone
-- could not express this: it records only the latest reason, not how many of the
-- earlier attempts were of each kind.
ALTER TABLE outbox_events
    ADD COLUMN uncharged_attempts integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT outbox_events_uncharged_valid CHECK (uncharged_attempts >= 0 AND uncharged_attempts <= attempts);
