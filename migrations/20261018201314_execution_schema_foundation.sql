-- Modify "jobs" table
ALTER TABLE "jobs" DROP CONSTRAINT "jobs_lifecycle_shape", ADD CONSTRAINT "jobs_lifecycle_shape" CHECK (
CASE status
    WHEN 'queued'::text THEN ((finished_at IS NULL) AND (result IS NULL) AND (lease_expires_at IS NULL) AND (worker_id IS NULL))
    WHEN 'running'::text THEN ((started_at IS NOT NULL) AND (finished_at IS NULL) AND (result IS NULL))
    WHEN 'succeeded'::text THEN ((started_at IS NOT NULL) AND (finished_at IS NOT NULL) AND (error_code IS NULL) AND (error_message IS NULL) AND (lease_expires_at IS NULL) AND (worker_id IS NULL))
    WHEN 'failed'::text THEN ((started_at IS NOT NULL) AND (finished_at IS NOT NULL) AND (result IS NULL) AND (error_code IS NOT NULL) AND (error_message IS NOT NULL) AND (lease_expires_at IS NULL) AND (worker_id IS NULL))
    ELSE NULL::boolean
END), ADD CONSTRAINT "jobs_attempts_valid" CHECK (((max_attempts >= 1) AND (max_attempts <= 20)) AND ((attempt >= 0) AND (attempt <= max_attempts))), ADD CONSTRAINT "jobs_error_pair" CHECK ((error_code IS NULL) = (error_message IS NULL)), ADD CONSTRAINT "jobs_generation_valid" CHECK (claim_generation >= 0), ADD COLUMN "attempt" integer NOT NULL DEFAULT 0, ADD COLUMN "max_attempts" integer NOT NULL DEFAULT 3, ADD COLUMN "claim_generation" bigint NOT NULL DEFAULT 0, ADD COLUMN "next_run_at" timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP, ADD COLUMN "lease_expires_at" timestamptz NULL, ADD COLUMN "worker_id" text NULL, ADD COLUMN "attempt_started_at" timestamptz NULL;
-- Create index "jobs_due_idx" to table: "jobs"
CREATE INDEX "jobs_due_idx" ON "jobs" ("next_run_at", "created_at", "id") WHERE (status = 'queued'::text);
-- Create index "jobs_running_idx" to table: "jobs"
CREATE INDEX "jobs_running_idx" ON "jobs" ("created_at") WHERE (status = 'running'::text);
