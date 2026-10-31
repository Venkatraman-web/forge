-- Hand-written, like the other function migrations (Atlas's free build cannot plan functions).
-- It does not change any table.
--
-- A per-type limit on queued jobs (docs/decisions/0004, item "max_pending"). The global limit
-- stays; a type that has its own limit is also refused when that many jobs OF THAT TYPE are
-- already queued. Without it one job type that floods the queue fills the global limit and
-- every other type is refused too (found by a flood test, docs/benchmarks-aws.md section 68).
--
-- The count runs inside the admission lock, so it must be cheap: it stops after p_type_max rows
-- (LIMIT) and reads them from jobs_due_type_idx (type first, only queued rows), so its cost
-- is bounded by the limit and not by how many other jobs are queued.
--
-- forge_submit_job: new parameter p_type_max (0 = this type has no limit), new result
-- 5 = the type is at its limit (nothing inserted). Results 0 to 4 are unchanged.
-- forge_submit_jobs (the batch): new parameter p_type_limits, a JSON object {type: limit};
-- result 5 = some type in the batch would go over its limit (nothing inserted). All or nothing.
DROP FUNCTION forge_submit_job(bigint, bigint, text, text, text, timestamptz, integer, integer, text);

CREATE FUNCTION forge_submit_job(
  p_lock_key bigint,
  p_max_queued bigint,
  p_id text,
  p_type text,
  p_input text,
  p_created_at timestamptz,
  p_max_attempts integer,
  p_timeout_seconds integer,
  p_idempotency_key text,
  p_type_max bigint
) RETURNS smallint LANGUAGE plpgsql AS $$
DECLARE
  v_queued bigint;
  v_type_queued bigint;
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
  IF p_type_max > 0 THEN
    SELECT count(*) INTO v_type_queued
      FROM (SELECT 1 FROM jobs WHERE status = 'queued' AND type = p_type LIMIT p_type_max) t;
    IF v_type_queued >= p_type_max THEN
      RETURN 5;
    END IF;
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

DROP FUNCTION forge_submit_jobs(bigint, bigint, jsonb);

CREATE FUNCTION forge_submit_jobs(
  p_lock_key bigint,
  p_max_queued bigint,
  p_rows jsonb,
  p_type_limits jsonb
) RETURNS smallint LANGUAGE plpgsql AS $$
DECLARE
  v_queued bigint;
  v_n integer;
  v_limit bigint;
  v_type_queued bigint;
  r record;
BEGIN
  v_n := jsonb_array_length(p_rows);
  PERFORM pg_advisory_xact_lock(p_lock_key);
  SELECT sum(queued) INTO v_queued FROM queue_counter;
  IF v_queued IS NULL THEN
    RAISE EXCEPTION 'queue_counter rows missing';
  END IF;
  IF v_queued + v_n > p_max_queued THEN
    RETURN 1;
  END IF;
  FOR r IN SELECT x.type AS type, count(*) AS n
           FROM jsonb_to_recordset(p_rows) AS x(type text) GROUP BY x.type LOOP
    v_limit := (p_type_limits ->> r.type)::bigint;
    IF v_limit IS NOT NULL AND v_limit > 0 THEN
      SELECT count(*) INTO v_type_queued
        FROM (SELECT 1 FROM jobs WHERE status = 'queued' AND type = r.type LIMIT v_limit) t;
      IF v_type_queued + r.n > v_limit THEN
        RETURN 5;
      END IF;
    END IF;
  END LOOP;
  INSERT INTO jobs (id, type, status, input, created_at, max_attempts, timeout_seconds)
  SELECT r2.id, r2.type, 'queued', r2.input, r2.created_at, r2.max_attempts, r2.timeout_seconds
  FROM jsonb_to_recordset(p_rows) AS r2(id text, type text, input jsonb, created_at timestamptz, max_attempts integer, timeout_seconds integer);
  RETURN 0;
END;
$$;
