-- Terminal failure state for outbox events (Task 25.4.1.6). The dispatcher gives
-- up on an event that Forge permanently rejects or that exhausts its attempts,
-- instead of retrying forever. Earlier migrations are untouched.
ALTER TABLE outbox_events
    ADD COLUMN failed_at timestamptz,
    -- Sanitized machine code of the most recent failure or retry reason, e.g.
    -- 'forge_http_503'. Never a response body, URL or credential.
    ADD COLUMN last_error_code text;

ALTER TABLE outbox_events DROP CONSTRAINT outbox_events_status_valid;
ALTER TABLE outbox_events
    ADD CONSTRAINT outbox_events_status_valid CHECK (status IN ('pending', 'published', 'failed')),
    ADD CONSTRAINT outbox_events_failed_matches_status CHECK ((status = 'failed') = (failed_at IS NOT NULL)),
    ADD CONSTRAINT outbox_events_failed_has_code CHECK (status <> 'failed' OR last_error_code IS NOT NULL),
    ADD CONSTRAINT outbox_events_error_code_format CHECK (last_error_code IS NULL OR last_error_code ~ '^[a-z0-9_]{1,64}$');
-- Existing constraints still apply: published_at is set exactly when published,
-- and a lease may only exist on a pending row, so a failed event is never leased.
