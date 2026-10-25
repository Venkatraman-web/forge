-- Hand-written, like the queue_counter triggers: Atlas's free build cannot plan
-- functions, so this one is added here. It does not change any table.
--
-- forge_submit_job runs the whole admission step in ONE database call:
-- take the global advisory lock, read the exact queued count from
-- queue_counter, and insert the job if there is room. Before this function the
-- Go code did the same as five separate round trips (begin, lock, select,
-- insert, commit) while holding the lock, so every millisecond of network
-- latency to the database was paid several times INSIDE the lock, which
-- capped submissions per second. Here the lock is held only for the
-- function's own execution, whatever the latency.
--
-- Same semantics as before: each statement of a plpgsql function in READ
-- COMMITTED takes a fresh snapshot, so the counter read after the lock sees
-- every earlier submission that committed.
--
-- Returns 0 = inserted, 1 = queue full (nothing inserted), 2 = id already exists.
CREATE FUNCTION forge_submit_job(
  p_lock_key bigint,
  p_max_queued bigint,
  p_id text,
  p_type text,
  p_input text,
  p_created_at timestamptz,
  p_max_attempts integer,
  p_timeout_seconds integer
) RETURNS smallint LANGUAGE plpgsql AS $$
DECLARE
  v_queued bigint;
  v_inserted integer;
BEGIN
  PERFORM pg_advisory_xact_lock(p_lock_key);
  SELECT queued INTO v_queued FROM queue_counter WHERE id = 1;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'queue_counter row missing';
  END IF;
  IF v_queued >= p_max_queued THEN
    RETURN 1;
  END IF;
  INSERT INTO jobs (id, type, status, input, created_at, max_attempts, timeout_seconds)
  VALUES (p_id, p_type, 'queued', p_input::jsonb, p_created_at, p_max_attempts, p_timeout_seconds)
  ON CONFLICT DO NOTHING;
  GET DIAGNOSTICS v_inserted = ROW_COUNT;
  IF v_inserted = 0 THEN
    RETURN 2;
  END IF;
  RETURN 0;
END;
$$;
