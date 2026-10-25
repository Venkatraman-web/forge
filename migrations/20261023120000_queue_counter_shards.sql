-- atlas:txmode none

-- Hand-written, like the queue_counter and forge_submit_job migrations: Atlas's
-- free build cannot plan functions or triggers.
--
-- WHY. queue_counter held ONE row that every claim (queued -> running) and every
-- submission updated through the triggers. A row update keeps its row lock until
-- the transaction COMMITS, and a commit waits for the disk, so all of those
-- transactions were serialised behind one another's commits: measured on AWS,
-- claims plus submissions together topped out near 580 transactions per second
-- even though submissions alone could sustain 3,000 jobs per second.
--
-- WHAT. The counter becomes 16 shard rows. Each transaction updates the shard
-- chosen by its transaction id, so unrelated transactions touch different rows
-- and their commits can proceed (and be flushed) together. The queued count is
-- the SUM over the shards. Row 1 keeps the existing count and the new shards
-- start at 0, so the sum is unchanged by this migration.
--
-- THE LIMIT STAYS EXACT. Submissions are still serialised by the advisory lock,
-- and a submission reads the sum after taking it, so it sees every earlier
-- submission's commit. A claim that has not committed yet is not reflected, which
-- can only make the sum HIGHER than the true count, never lower, so the limit can
-- be refused slightly early but never exceeded.
--
-- Block writers to jobs until this migration commits, so no job changes status
-- while the counter is being reshaped.
BEGIN;
LOCK TABLE "jobs" IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE "queue_counter" DROP CONSTRAINT "queue_counter_singleton";
ALTER TABLE "queue_counter" ADD CONSTRAINT "queue_counter_shard" CHECK (id BETWEEN 1 AND 16);
INSERT INTO "queue_counter" ("id", "queued") SELECT g, 0 FROM generate_series(2, 16) AS g;

CREATE OR REPLACE FUNCTION jobs_queued_counter() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  v_shard smallint := (txid_current() % 16) + 1;
BEGIN
  IF TG_OP = 'INSERT' THEN
    UPDATE queue_counter SET queued = queued + 1 WHERE id = v_shard;
  ELSIF TG_OP = 'DELETE' THEN
    UPDATE queue_counter SET queued = queued - 1 WHERE id = v_shard;
  ELSIF TG_OP = 'UPDATE' THEN
    -- the trigger's WHEN clause guarantees exactly one side is 'queued'
    UPDATE queue_counter SET queued = queued + CASE WHEN NEW.status = 'queued' THEN 1 ELSE -1 END WHERE id = v_shard;
  ELSE -- TRUNCATE
    UPDATE queue_counter SET queued = 0;
  END IF;
  RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION forge_submit_job(
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
  SELECT sum(queued) INTO v_queued FROM queue_counter;
  IF v_queued IS NULL THEN
    RAISE EXCEPTION 'queue_counter rows missing';
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

CREATE OR REPLACE FUNCTION forge_submit_jobs(
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
  SELECT sum(queued) INTO v_queued FROM queue_counter;
  IF v_queued IS NULL THEN
    RAISE EXCEPTION 'queue_counter rows missing';
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

COMMIT;
