-- Where the payment sweeper stopped. The sweeper walks the payment service's final attempts in the
-- order they were completed and applies the ones whose order is still pending (a crash between the
-- payment service recording an attempt and the shop applying it). One row; it starts at the moment
-- this migration runs, so older payments are not re-examined.
CREATE TABLE payment_sweep_cursor (
    only_row     boolean     PRIMARY KEY DEFAULT true CHECK (only_row),
    completed_at timestamptz NOT NULL,
    attempt_id   text        NOT NULL DEFAULT ''
);
INSERT INTO payment_sweep_cursor (completed_at) VALUES (now());
