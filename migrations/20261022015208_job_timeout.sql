-- Modify "jobs" table
ALTER TABLE "jobs" ADD CONSTRAINT "jobs_timeout_valid" CHECK ((timeout_seconds >= 1) AND (timeout_seconds <= 3600)), ADD COLUMN "timeout_seconds" integer NOT NULL DEFAULT 30;
