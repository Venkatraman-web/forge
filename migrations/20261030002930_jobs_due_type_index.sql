-- Create index "jobs_due_type_idx" to table: "jobs"
CREATE INDEX "jobs_due_type_idx" ON "jobs" ("type", "next_run_at", "created_at", "id") WHERE (status = 'queued'::text);
