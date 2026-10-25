-- Hand-written, like forge_submit_job: Atlas's free build cannot plan functions.
-- It does not change any table.
--
-- forge_submit_jobs stores SEVERAL jobs in one transaction with one commit.
-- Taking the global admission lock, reading the exact queued count and inserting
-- is the same step as forge_submit_job, but the cost that limits submissions per
-- second (the lock is held until the commit reaches the disk) is now paid once
-- per batch instead of once per job. All or nothing: either every job is stored
-- or none is.
--
-- The limit stays exact: the batch is stored only if queued + batch size does not
-- exceed p_max_queued. For a batch of one this is the same rule as before.
--
-- p_rows is a JSON array of objects with id, type, input, created_at,
-- max_attempts and timeout_seconds. A duplicate id raises a unique violation
-- (SQLSTATE 23505), which aborts the whole transaction.
--
-- Returns 0 = stored, 1 = queue full (nothing stored).
CREATE FUNCTION forge_submit_jobs(
  p_lock_key bigint,
  p_max_queued bigint,
  p_rows jsonb
) RETURNS smallint LANGUAGE plpgsql AS $$
DECLARE
  v_queued bigint;
  v_n integer;
BEGIN
  v_n := jsonb_array_length(p_rows);
  PERFORM pg_advisory_xact_lock(p_lock_key);
  SELECT queued INTO v_queued FROM queue_counter WHERE id = 1;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'queue_counter row missing';
  END IF;
  IF v_queued + v_n > p_max_queued THEN
    RETURN 1;
  END IF;
  INSERT INTO jobs (id, type, status, input, created_at, max_attempts, timeout_seconds)
  SELECT r.id, r.type, 'queued', r.input, r.created_at, r.max_attempts, r.timeout_seconds
  FROM jsonb_to_recordset(p_rows) AS r(id text, type text, input jsonb, created_at timestamptz, max_attempts integer, timeout_seconds integer);
  RETURN 0;
END;
$$;
