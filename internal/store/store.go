// Package store defines how Forge saves and loads jobs (the Store interface)
// and provides the PostgreSQL implementation.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Venkatraman-web/forge/internal/jobs"
)

// Errors every Store implementation returns for these situations.
var (
	ErrEmptyID       = errors.New("job id is empty")
	ErrAlreadyExists = errors.New("job already exists")
	ErrNotFound      = errors.New("job not found")

	// ErrQueueFull means CreateIfQueueBelow found the backlog at its limit and
	// stored nothing.
	ErrQueueFull = errors.New("job queue is full")

	// ErrIdempotencyConflict means CreateIfQueueBelow found a job stored under
	// the same idempotency key but with different content (type, input, retry
	// budget or timeout). Nothing was stored.
	ErrIdempotencyConflict = errors.New("idempotency key already used for a different job")

	// ErrAdmissionBusy means CreateIfQueueBelow could not get one of the limited
	// admission slots within admissionWait. Nothing was stored and no database
	// work was started; the queue may well have room. Retrying later is expected
	// to work.
	ErrAdmissionBusy = errors.New("admission capacity busy")

	// ErrNoClaimTypes means Claim was called without any job type. Claiming
	// "everything" must never be the result of a forgotten filter, so it is
	// rejected before any SQL runs.
	ErrNoClaimTypes = errors.New("claim needs at least one job type")

	// ErrLostOwnership means a fenced write (Complete or Fail) matched ZERO
	// rows: the job is no longer running under this claim's generation. It is
	// a normal protocol outcome, not a database failure, and the caller must
	// stop acting on that claim without retrying the write. Note: if an
	// earlier attempt of the same write actually committed but its response
	// was lost, a retry reports ErrLostOwnership too, because the job is no
	// longer "running"; either way the right reaction is to stop.
	ErrLostOwnership = errors.New("job is no longer owned by this claim")
)

// ClaimedJob is a job a worker has just claimed, together with the two values
// that identify the claim. Generation is the fencing token: every
// owner-sensitive write must present it. Attempt is a different number (how
// many executions have been started) and is NOT a fence.
type ClaimedJob struct {
	Job     jobs.Job
	Attempt int
	// MaxAttempts is the job's attempt budget when it was claimed. It is for
	// diagnostics only; the database decides whether attempts remain.
	MaxAttempts int
	Generation  int64
	WorkerID    string
	// QueueWait is how long the job waited, by the database clock, between
	// becoming due (next_run_at: its submission time, or the time a retry or
	// release made it claimable) and this claim. It is for metrics only.
	QueueWait time.Duration
}

// Finished is what a fenced write reports about the job it just changed, read
// by the same statement. A caller that only holds a job ID (the remote worker
// API) uses it to attribute metrics.
type Finished struct {
	Type string
	// AttemptDuration is the time from this attempt's claim to the write, by the
	// database clock, so it includes the worker's reporting delay. Zero means
	// unknown (a legacy row without attempt_started_at).
	AttemptDuration time.Duration
}

// CompleteItem is one job of a CompleteBatch.
type CompleteItem struct {
	Claim  ClaimedJob
	Result json.RawMessage
}

// CompleteOutcome is the result for one CompleteItem. Done false means the
// claim was no longer current (the batch form of ErrLostOwnership).
type CompleteOutcome struct {
	Done     bool
	Finished Finished
}

// PurgeResult says what one PurgeFinished call deleted.
type PurgeResult struct {
	Succeeded int
	Failed    int
}

// ReapResult says what one ReapExpired call did.
type ReapResult struct {
	Requeued  int // expired jobs put back to queued (attempts remained)
	Exhausted int // expired jobs failed because no attempts remained
}

// QueueTypeStats is the live state of one job type's queue.
type QueueTypeStats struct {
	Type string
	// Queued counts jobs whose status is 'queued'. That is exactly what the
	// admission limit counts, so it includes jobs waiting out a retry backoff or
	// scheduled for later, not only jobs a worker could claim right now.
	Queued int
	// Running counts jobs whose status is 'running' (claimed, lease not reaped).
	Running int
	// OldestDueAge is how long the oldest DUE queued job has been due: now minus
	// the smallest next_run_at among queued jobs whose next_run_at has passed.
	// It is nil when no queued job is due (including when nothing is queued).
	OldestDueAge *time.Duration
}

// QueueStats is a consistent snapshot of every job type that has queued or
// running jobs.
type QueueStats struct {
	// CollectedAt is the database clock when the snapshot was taken.
	CollectedAt time.Time
	// Types holds one entry per type with at least one queued or running job,
	// sorted by type. A type with none is absent, not zero: callers that want to
	// list configured types add them.
	Types []QueueTypeStats
}

// Store is what the API and the worker code need from job storage. It exists
// as a seam: they depend on these behaviours, not on a concrete store, so a
// different implementation can be passed in from main without changing them.
//
// Every method takes a context so callers can cancel or time-limit the call,
// and every method returns an error so that a storage failure can never look
// like "no such job" or "no jobs". Implementations must be safe for concurrent
// use. Errors to tell apart with errors.Is: ErrEmptyID, ErrAlreadyExists
// (Create, CreateIfQueueBelow), ErrQueueFull and ErrAdmissionBusy
// (CreateIfQueueBelow), ErrIdempotencyConflict (CreateIfQueueBelow), ErrNotFound
// (Get, Delete) and ErrLostOwnership (Complete, Fail). Any other error,
// including context.Canceled and context.DeadlineExceeded, is an operational
// failure.
//
// There is deliberately no method that overwrites a whole job: the worker
// changes a job's state only through Claim, Complete and Fail, each of which is
// conditional in SQL.
type Store interface {
	Create(ctx context.Context, j jobs.Job) error
	// CreateIfQueueBelow stores a new queued job only if fewer than maxQueued
	// jobs are currently queued. The check and the insert are one atomic step
	// with respect to other callers, so the limit holds under concurrency.
	//
	// If j carries an IdempotencyKey that an earlier job already holds with the
	// same content, nothing is stored and that earlier job is returned with
	// replayed true; this is checked before the limit, so a repeat is answered
	// even when the queue is full. The same key with different content returns
	// ErrIdempotencyConflict. Otherwise the stored job is j and replayed is false.
	CreateIfQueueBelow(ctx context.Context, j jobs.Job, maxQueued int) (stored jobs.Job, replayed bool, err error)
	// CreateBatchIfQueueBelow stores several new queued jobs in ONE transaction
	// with one commit, only if queued + len(js) stays within maxQueued (all or
	// nothing). The commit cost that limits submissions per second is paid once
	// for the whole batch. A duplicate id stores nothing and returns
	// ErrAlreadyExists.
	CreateBatchIfQueueBelow(ctx context.Context, js []jobs.Job, maxQueued int) error
	Get(ctx context.Context, id string) (jobs.Job, error)
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, limit int) ([]jobs.Job, error)

	// Claim atomically takes the next due queued job of one of the given types
	// for workerID, marking it running with a lease of the given length. The
	// type filter is applied inside the claim statement, so a job of any other
	// type is never touched: it stays queued for a claimer that can run it.
	// types must be non-empty: an empty filter is an error (ErrNoClaimTypes),
	// never "any type". It returns ok=false with a nil error when no job is
	// currently eligible (an empty queue is normal, not a failure). Any other
	// non-nil error is an operational failure.
	Claim(ctx context.Context, workerID string, lease time.Duration, types []string) (c ClaimedJob, ok bool, err error)
	// ClaimBatch is Claim for up to max jobs (1 to MaxClaimBatch) in one
	// statement, with a cap on their total input size. It returns an empty slice
	// when nothing is eligible. Every job of the batch is claimed under the same
	// workerID and each gets its own claim generation.
	ClaimBatch(ctx context.Context, workerID string, lease time.Duration, types []string, max int) ([]ClaimedJob, error)
	// Complete records success. Zero matching rows => ErrLostOwnership. On success
	// it returns the job's type and attempt duration, read by the same statement,
	// so a caller that only holds a job ID (the remote worker API) can attribute
	// the outcome (metrics).
	Complete(ctx context.Context, c ClaimedJob, result json.RawMessage) (Finished, error)
	// CompleteBatch is Complete for several jobs in one statement and one
	// commit. Each item is fenced on its own: the outcome at the same index says
	// whether it was applied (Done) or its claim was no longer current (the
	// ErrLostOwnership case); one stale item never blocks the others. An error
	// means NONE of the batch is known to be applied. Callers must not list one
	// job twice.
	CompleteBatch(ctx context.Context, items []CompleteItem) ([]CompleteOutcome, error)
	// Fail records terminal failure. Zero matching rows => ErrLostOwnership. It
	// returns what Complete returns.
	Fail(ctx context.Context, c ClaimedJob, info jobs.ErrorInfo) (Finished, error)

	// FailAttempt records that the attempt under claim c failed and lets the
	// database decide, in one fenced statement, what happens to the job. If
	// retryable is true and attempts remain (attempt < max_attempts) the job goes
	// back to queued, claimable retryDelay from now (measured by the database
	// clock, which also stamps leases and finish times), and willRetry is true. Otherwise it
	// becomes terminally failed. Zero matching rows => ErrLostOwnership. The
	// caller must not assume the outcome from its own copy of the job: only the
	// returned willRetry is authoritative. retryDelay is ignored for a failure that is not retried.
	// finished is read by the same statement, like Complete's.
	FailAttempt(ctx context.Context, c ClaimedJob, info jobs.ErrorInfo, retryable bool, retryDelay time.Duration) (willRetry bool, finished Finished, err error)

	// Release hands the claim back because the worker is shutting down
	// cleanly, not because the job failed. It is fenced like Complete and Fail.
	// The job returns to queued, claimable at once, with the interrupted attempt
	// refunded (attempt - 1, never below 0). claim_generation is NOT reduced: the
	// next claim increments it, so this claim can never become valid again.
	// Zero matching rows => ErrLostOwnership. Call it only after the handler has
	// returned: releasing a job whose handler is still running would let a second
	// worker execute it concurrently.
	Release(ctx context.Context, c ClaimedJob) (jobType string, err error)

	// RenewLease pushes the claim's lease deadline forward (database time),
	// changing nothing else. It is fenced like Complete and Fail: it matches
	// only while the job is running under this claim's generation and worker,
	// so it can never extend a lease that a reaper has already invalidated, nor
	// resurrect a finished job. Zero matching rows => ErrLostOwnership.
	RenewLease(ctx context.Context, c ClaimedJob, lease time.Duration) error

	// ReapExpired recovers jobs whose lease has expired, in one atomic
	// statement, at most limit per call. Each recovered job gets a new
	// claim_generation (which invalidates the former owner). With attempts
	// left it goes back to queued, claimable after recoveryBackoff; with none
	// left it becomes failed. Jobs that are renewed or finished concurrently
	// are left alone. Safe to run from several callers at once.
	ReapExpired(ctx context.Context, limit int, recoveryBackoff time.Duration) (ReapResult, error)

	// PurgeFinished deletes finished jobs whose retention has passed, at most limit per
	// call, oldest first: succeeded jobs finished before succeededBefore and failed jobs
	// finished before failedBefore (a nil bound keeps that status forever). Queued and
	// running jobs are never touched. A deleted job is gone for good: GET /v1/jobs/{id}
	// answers 404 and its idempotency key is forgotten. Safe to run from several callers
	// at once.
	PurgeFinished(ctx context.Context, succeededBefore, failedBefore *time.Time, limit int) (PurgeResult, error)

	// CountQueued reports how many jobs are waiting (status queued), including
	// ones scheduled for a future time.
	CountQueued(ctx context.Context) (int, error)

	// QueueStats reports queued and running counts and the age of the oldest due
	// queued job per type. It is read-only, changes no job, takes no row locks,
	// and answers from ONE consistent snapshot (a single SQL statement), so a job
	// that moves from queued to running while it runs is counted exactly once.
	QueueStats(ctx context.Context) (QueueStats, error)
}
