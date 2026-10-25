-- The order service's sweeper walks the final (succeeded or declined) attempts in the order they
-- were completed, from a cursor it keeps: this index serves that range scan.
CREATE INDEX payment_attempts_final_completed_idx ON payment_attempts (completed_at, id) WHERE outcome IN ('succeeded', 'declined');
