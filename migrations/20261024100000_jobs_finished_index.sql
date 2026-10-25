-- Retention (the purge of finished jobs) walks finished jobs oldest first; this index serves it.
-- Only finished rows are indexed, so queued and running jobs, which change constantly, pay nothing.
CREATE INDEX "jobs_finished_idx" ON "jobs" ("finished_at") WHERE ((status = 'succeeded'::text) OR (status = 'failed'::text));
