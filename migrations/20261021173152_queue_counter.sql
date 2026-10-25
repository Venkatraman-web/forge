-- atlas:txmode none

-- Hand-extended after "atlas migrate diff": Atlas's free build cannot plan
-- functions or triggers, so they are written here. This file was reviewed and
-- hashed before it was applied anywhere.
--
-- Block writers to jobs (reads still work) until this migration commits, so no
-- job can change status between the backfill count and the triggers existing.
-- The transaction is explicit because the lock only lasts inside one.
BEGIN;
LOCK TABLE "jobs" IN SHARE ROW EXCLUSIVE MODE;

-- Create "queue_counter" table
CREATE TABLE "queue_counter" (
  "id" smallint NOT NULL,
  "queued" bigint NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "queue_counter_singleton" CHECK (id = 1)
);

-- Backfill: the counter starts at the real number of queued rows.
INSERT INTO "queue_counter" ("id", "queued") SELECT 1, count(*) FROM "jobs" WHERE status = 'queued';

-- Triggers: keep the counter exact in the same statement as every change of a row's queued-ness.
CREATE FUNCTION jobs_queued_counter() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    UPDATE queue_counter SET queued = queued + 1 WHERE id = 1;
  ELSIF TG_OP = 'DELETE' THEN
    UPDATE queue_counter SET queued = queued - 1 WHERE id = 1;
  ELSIF TG_OP = 'UPDATE' THEN
    -- the trigger's WHEN clause guarantees exactly one side is 'queued'
    UPDATE queue_counter SET queued = queued + CASE WHEN NEW.status = 'queued' THEN 1 ELSE -1 END WHERE id = 1;
  ELSE -- TRUNCATE
    UPDATE queue_counter SET queued = 0 WHERE id = 1;
  END IF;
  RETURN NULL;
END;
$$;

CREATE TRIGGER jobs_queued_counter_insert AFTER INSERT ON jobs
  FOR EACH ROW WHEN (NEW.status = 'queued') EXECUTE FUNCTION jobs_queued_counter();

CREATE TRIGGER jobs_queued_counter_delete AFTER DELETE ON jobs
  FOR EACH ROW WHEN (OLD.status = 'queued') EXECUTE FUNCTION jobs_queued_counter();

CREATE TRIGGER jobs_queued_counter_update AFTER UPDATE OF status ON jobs
  FOR EACH ROW WHEN ((OLD.status = 'queued') <> (NEW.status = 'queued')) EXECUTE FUNCTION jobs_queued_counter();

CREATE TRIGGER jobs_queued_counter_truncate AFTER TRUNCATE ON jobs
  FOR EACH STATEMENT EXECUTE FUNCTION jobs_queued_counter();

COMMIT;
