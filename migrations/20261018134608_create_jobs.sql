-- Create "jobs" table
CREATE TABLE "jobs" (
  "id" text NOT NULL,
  "type" text NOT NULL,
  "status" text NOT NULL,
  "input" jsonb NOT NULL,
  "result" jsonb NULL,
  "error_code" text NULL,
  "error_message" text NULL,
  "created_at" timestamptz NOT NULL,
  "started_at" timestamptz NULL,
  "finished_at" timestamptz NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "jobs_id_not_empty" CHECK (id <> ''::text),
  CONSTRAINT "jobs_lifecycle_shape" CHECK (
CASE status
    WHEN 'queued'::text THEN ((started_at IS NULL) AND (finished_at IS NULL) AND (result IS NULL) AND (error_code IS NULL) AND (error_message IS NULL))
    WHEN 'running'::text THEN ((started_at IS NOT NULL) AND (finished_at IS NULL) AND (result IS NULL) AND (error_code IS NULL) AND (error_message IS NULL))
    WHEN 'succeeded'::text THEN ((started_at IS NOT NULL) AND (finished_at IS NOT NULL) AND (error_code IS NULL) AND (error_message IS NULL))
    WHEN 'failed'::text THEN ((started_at IS NOT NULL) AND (finished_at IS NOT NULL) AND (result IS NULL) AND (error_code IS NOT NULL) AND (error_message IS NOT NULL))
    ELSE NULL::boolean
END),
  CONSTRAINT "jobs_status_valid" CHECK (status = ANY (ARRAY['queued'::text, 'running'::text, 'succeeded'::text, 'failed'::text])),
  CONSTRAINT "jobs_type_not_empty" CHECK (type <> ''::text)
);
-- Create index "jobs_created_at_id_idx" to table: "jobs"
CREATE INDEX "jobs_created_at_id_idx" ON "jobs" ("created_at" DESC, "id" DESC);
