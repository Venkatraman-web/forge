-- Modify "jobs" table
ALTER TABLE "jobs" ADD CONSTRAINT "jobs_idempotency_key_valid" CHECK ((idempotency_key IS NULL) OR ((length(idempotency_key) >= 8) AND (length(idempotency_key) <= 128))), ADD COLUMN "idempotency_key" text NULL;
-- Create index "jobs_idempotency_key_idx" to table: "jobs"
CREATE UNIQUE INDEX "jobs_idempotency_key_idx" ON "jobs" ("idempotency_key") WHERE (idempotency_key IS NOT NULL);

-- Hand-written, like the earlier function migrations: Atlas's free build cannot
-- plan functions.
--
-- forge_submit_job gains the submitter's optional idempotency key. Submissions
-- are already serialised by the advisory lock, so the lookup below cannot race
-- with another submission of the same key; the unique index is only the backstop.
-- The lookup comes BEFORE the queue-limit check, so a repeat of a job that
-- already exists is answered even when the queue is full (nothing new is
-- stored, so the limit is not at risk).
--
-- Result: 0 stored, 1 queue full, 2 id already exists, 3 the key exists with
-- the same content (a replay), 4 the key exists with different content.
-- "Same content" means type, input, max_attempts and timeout_seconds; input is
-- compared as jsonb, so key order and whitespace do not matter.
DROP FUNCTION forge_submit_job(bigint, bigint, text, text, text, timestamptz, integer, integer);

CREATE FUNCTION forge_submit_job(
  p_lock_key bigint,
  p_max_queued bigint,
  p_id text,
  p_type text,
  p_input text,
  p_created_at timestamptz,
  p_max_attempts integer,
  p_timeout_seconds integer,
  p_idempotency_key text
) RETURNS smallint LANGUAGE plpgsql AS $$
DECLARE
  v_queued bigint;
  v_inserted integer;
  v_same boolean;
BEGIN
  PERFORM pg_advisory_xact_lock(p_lock_key);
  IF p_idempotency_key IS NOT NULL THEN
    SELECT (type = p_type AND input = p_input::jsonb
            AND max_attempts = p_max_attempts AND timeout_seconds = p_timeout_seconds)
      INTO v_same FROM jobs WHERE idempotency_key = p_idempotency_key;
    IF FOUND THEN
      RETURN CASE WHEN v_same THEN 3 ELSE 4 END;
    END IF;
  END IF;
  SELECT sum(queued) INTO v_queued FROM queue_counter;
  IF v_queued IS NULL THEN
    RAISE EXCEPTION 'queue_counter rows missing';
  END IF;
  IF v_queued >= p_max_queued THEN
    RETURN 1;
  END IF;
  INSERT INTO jobs (id, type, status, input, created_at, max_attempts, timeout_seconds, idempotency_key)
  VALUES (p_id, p_type, 'queued', p_input::jsonb, p_created_at, p_max_attempts, p_timeout_seconds, p_idempotency_key)
  ON CONFLICT DO NOTHING;
  GET DIAGNOSTICS v_inserted = ROW_COUNT;
  IF v_inserted = 0 THEN
    RETURN 2;
  END IF;
  RETURN 0;
END;
$$;
