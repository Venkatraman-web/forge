package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Venkatraman-web/forge/internal/jobs"
)

// queryTimeout is the longest any single database call may take. Each call
// derives a timeout from the caller's context, so whichever limit is reached
// first (caller cancellation or this bound) stops the query.
const queryTimeout = 5 * time.Second

var _ Store = (*PostgresStore)(nil)

// PostgresStore keeps jobs in the "jobs" table created by
// migrations/000001_create_jobs.up.sql, using GORM to build and run the SQL.
//
// Connection ownership: the caller opens the *gorm.DB (and so the underlying
// connection pool) and closes it at shutdown. The store only borrows it.
//
// Requirements on the caller's *gorm.DB:
//   - It must use the PostgreSQL dialect (the SQL here and the JSONB columns
//     are PostgreSQL features).
//   - No special gorm.Config is needed for duplicate IDs: Create detects them
//     with ON CONFLICT DO NOTHING, not by translating driver errors, so
//     TranslateError does not need to be enabled.
//   - Never call AutoMigrate. The schema is owned by the versioned files in
//     migrations/ and the store assumes the table already exists.
//
// Things worth knowing: PostgreSQL stores timestamps to
// the microsecond, so Go's nanoseconds are rounded; and JSONB normalises
// input (whitespace and key order may change, duplicate keys keep the last).
// The JSON meaning is the same.
type PostgresStore struct {
	db *gorm.DB
}

// NewPostgresStore wraps an already-initialised GORM handle.
func NewPostgresStore(db *gorm.DB) (*PostgresStore, error) {
	if db == nil {
		return nil, errors.New("postgres store needs a gorm database handle")
	}
	return &PostgresStore{db: db}, nil
}

// jsonb carries raw JSON to and from a JSONB column. It is sent as text and
// PostgreSQL parses it as JSON; empty means SQL NULL. Keeping it a named type
// avoids an extra dependency just for JSON columns.
type jsonb []byte

// Value implements driver.Valuer: how the value is sent to the database.
func (j jsonb) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return string(j), nil
}

// Scan implements sql.Scanner: how the value is read back. The driver may
// hand JSON over as []byte or string, and NULL as nil.
func (j *jsonb) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append(jsonb(nil), v...) // copy: the driver may reuse its buffer
	case string:
		*j = jsonb(v)
	default:
		return fmt.Errorf("scan jsonb: unsupported type %T", src)
	}
	return nil
}

// JobRecord is the database shape of a job: one field per column of the jobs
// table. It exists so the domain type jobs.Job stays free of storage details
// (GORM tags, database types). Nullable columns are pointers or jsonb.
//
// It is exported, and its tags are complete, because Atlas reads this struct
// to learn the desired schema (see atlas.hcl): the tags are the single
// definition of the table, including its CHECK constraints and index. Change
// the schema here, then generate a migration with Atlas; never AutoMigrate.
type JobRecord struct {
	ID           string    `gorm:"column:id;type:text;primaryKey;check:jobs_id_not_empty,id <> '';index:jobs_created_at_id_idx,priority:2,sort:desc;index:jobs_due_idx,priority:3,where:status = 'queued'"`
	Type         string    `gorm:"column:type;type:text;not null;check:jobs_type_not_empty,type <> ''"`
	Status       string    `gorm:"column:status;type:text;not null;check:jobs_status_valid,status IN ('queued', 'running', 'succeeded', 'failed')"`
	Input        jsonb     `gorm:"column:input;type:jsonb;not null"`
	Result       jsonb     `gorm:"column:result;type:jsonb"`
	ErrorCode    *string   `gorm:"column:error_code;type:text;check:jobs_error_pair,(error_code IS NULL) = (error_message IS NULL)"`
	ErrorMessage *string   `gorm:"column:error_message;type:text"`
	CreatedAt    time.Time `gorm:"column:created_at;type:timestamptz;not null;autoCreateTime:false;index:jobs_created_at_id_idx,priority:1,sort:desc;index:jobs_due_idx,priority:2,where:status = 'queued';index:jobs_running_idx,where:status = 'running'"` // always set by the app; no GORM magic
	// StartedAt is when the job FIRST began executing (never reset by a retry).
	StartedAt *time.Time `gorm:"column:started_at;type:timestamptz"`
	// GORM allows one check: tag per field, so this table-wide constraint is
	// attached here, on the last lifecycle column. It limits which columns
	// may be set in each status (see internal/jobs/job.go). A queued job may
	// carry StartedAt and a previous error (it is waiting for a retry); a
	// running job's lease and worker are not required by this check (rows from
	// before leases existed stay valid). Only a queued or finished job must not
	// hold a lease.
	FinishedAt *time.Time `gorm:"column:finished_at;type:timestamptz;index:jobs_finished_idx,where:status = 'succeeded' OR status = 'failed';check:jobs_lifecycle_shape,CASE status WHEN 'queued' THEN finished_at IS NULL AND result IS NULL AND lease_expires_at IS NULL AND worker_id IS NULL WHEN 'running' THEN started_at IS NOT NULL AND finished_at IS NULL AND result IS NULL WHEN 'succeeded' THEN started_at IS NOT NULL AND finished_at IS NOT NULL AND error_code IS NULL AND error_message IS NULL AND lease_expires_at IS NULL AND worker_id IS NULL WHEN 'failed' THEN started_at IS NOT NULL AND finished_at IS NOT NULL AND result IS NULL AND error_code IS NOT NULL AND error_message IS NOT NULL AND lease_expires_at IS NULL AND worker_id IS NULL END"`

	// --- Execution-engine columns (migration M-A1). Workers own them
	// through Claim, Complete and Fail below. New rows leave
	// them zero, and GORM omits zero values that have a default tag on
	// INSERT, so the database defaults apply.

	// Attempt counts executions started (incremented at claim; a graceful
	// release will refund it). MaxAttempts is the budget.
	Attempt     int `gorm:"column:attempt;type:integer;not null;default:0"`
	MaxAttempts int `gorm:"column:max_attempts;type:integer;not null;default:3;check:jobs_attempts_valid,max_attempts BETWEEN 1 AND 20 AND attempt BETWEEN 0 AND max_attempts"`

	// TimeoutSeconds is the execution timeout of each attempt, chosen at
	// submission (default 30). It is read at every claim and never changed by
	// retries, release or recovery, so a later attempt gets the same limit.
	TimeoutSeconds int `gorm:"column:timeout_seconds;type:integer;not null;default:30;check:jobs_timeout_valid,timeout_seconds BETWEEN 1 AND 3600"`
	// ClaimGeneration is the fencing token: it grows on every claim and every
	// recovery, and is distinct from Attempt. bigint cannot realistically wrap.
	ClaimGeneration int64 `gorm:"column:claim_generation;type:bigint;not null;default:0;check:jobs_generation_valid,claim_generation >= 0"`
	// NextRunAt is the earliest time a queued job may be claimed.
	NextRunAt time.Time `gorm:"column:next_run_at;type:timestamptz;not null;default:CURRENT_TIMESTAMP;index:jobs_due_idx,priority:1,where:status = 'queued'"`
	// LeaseExpiresAt, WorkerID and AttemptStartedAt describe the current
	// claim. They are deliberately NOT indexed: heartbeats rewrite the lease,
	// and an indexed column would stop those updates being cheap in-place
	// (HOT) updates.
	LeaseExpiresAt   *time.Time `gorm:"column:lease_expires_at;type:timestamptz"`
	WorkerID         *string    `gorm:"column:worker_id;type:text"`
	AttemptStartedAt *time.Time `gorm:"column:attempt_started_at;type:timestamptz"`

	// IdempotencyKey is the submitter's optional key. The unique index makes it
	// the final guard against two jobs for one key (submissions are already
	// serialised by the admission lock). It is never updated after insert, so
	// the index does not stop heartbeat and status updates being HOT updates.
	IdempotencyKey *string `gorm:"column:idempotency_key;type:text;uniqueIndex:jobs_idempotency_key_idx,where:idempotency_key IS NOT NULL;check:jobs_idempotency_key_valid,idempotency_key IS NULL OR length(idempotency_key) BETWEEN 8 AND 128"`
}

func (JobRecord) TableName() string { return "jobs" }

// QueueCounterRecord is the table holding the number of jobs whose status is
// 'queued', split over 16 shard rows whose SUM is the count. It exists only so
// admission can read that number in O(1) instead of counting rows while it holds
// the global admission lock (the cost of that COUNT grew with queue depth; see
// the E-1 follow-up results). It is sharded because one row would make every
// claim and submission wait for the previous one's commit (see the
// queue_counter_shards migration); an individual shard may be negative.
//
// Nothing in Go writes it. Triggers on the jobs table (created by the
// queue_counter migration; GORM tags cannot describe triggers, and Atlas's free
// build cannot plan functions) apply every change of a row's queued-ness in the same
// statement, and therefore the same transaction, as the row change itself, so
// the counter commits or rolls back with the job and cannot miss a code path.
// Its authoritative meaning is still "SELECT count(*) FROM jobs WHERE status =
// 'queued'", and that query is the reconciliation check.
type QueueCounterRecord struct {
	ID     int16 `gorm:"column:id;type:smallint;primaryKey;autoIncrement:false;check:queue_counter_shard,id BETWEEN 1 AND 16"`
	Queued int64 `gorm:"column:queued;type:bigint;not null"`
}

func (QueueCounterRecord) TableName() string { return "queue_counter" }

func toRecord(j jobs.Job) JobRecord {
	r := JobRecord{
		ID:             j.ID,
		Type:           j.Type,
		Status:         string(j.Status),
		MaxAttempts:    j.MaxAttempts,    // 0 (a caller that never set it) lets the column default apply
		TimeoutSeconds: j.TimeoutSeconds, // likewise
		Input:          jsonb(j.Input),
		Result:         jsonb(j.Result),
		CreatedAt:      j.CreatedAt.UTC(),
		StartedAt:      utcPtr(j.StartedAt),
		FinishedAt:     utcPtr(j.FinishedAt),
	}
	if j.IdempotencyKey != "" {
		key := j.IdempotencyKey
		r.IdempotencyKey = &key
	}
	if j.Error != nil {
		code, message := j.Error.Code, j.Error.Message
		r.ErrorCode, r.ErrorMessage = &code, &message
	}
	return r
}

func (r JobRecord) toJob() jobs.Job {
	j := jobs.Job{
		ID:             r.ID,
		Type:           r.Type,
		Status:         jobs.Status(r.Status),
		MaxAttempts:    r.MaxAttempts,
		TimeoutSeconds: r.TimeoutSeconds,
		Input:          []byte(r.Input),
		Result:         []byte(r.Result),
		CreatedAt:      r.CreatedAt.UTC(),
		StartedAt:      utcPtr(r.StartedAt),
		FinishedAt:     utcPtr(r.FinishedAt),
	}
	if r.IdempotencyKey != nil {
		j.IdempotencyKey = *r.IdempotencyKey
	}
	if r.ErrorCode != nil && r.ErrorMessage != nil {
		j.Error = &jobs.ErrorInfo{Code: *r.ErrorCode, Message: *r.ErrorMessage}
	}
	return j
}

// utcPtr returns a UTC copy of t, or nil. Copying keeps the record and the
// job from sharing a time value.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// Create inserts a new job. It fails if the ID is empty or already used.
func (s *PostgresStore) Create(ctx context.Context, j jobs.Job) error {
	if j.ID == "" {
		return ErrEmptyID
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	// DO NOTHING turns a duplicate ID into "0 rows inserted" rather than an
	// error, which is how we tell it apart from a real failure.
	rec := toRecord(j)
	res := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&rec)
	if res.Error != nil {
		return fmt.Errorf("insert job %s: %w", j.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrAlreadyExists
	}
	return nil
}

// Get returns the job, ErrNotFound if no row has that ID, or a wrapped
// database error. Only "record not found" means not found.
func (s *PostgresStore) Get(ctx context.Context, id string) (jobs.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var rec JobRecord
	err := s.db.WithContext(ctx).Where("id = ?", id).Take(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return jobs.Job{}, ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, fmt.Errorf("get job %s: %w", id, err)
	}
	return rec.toJob(), nil
}

// Delete removes an existing job.
func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	res := s.db.WithContext(ctx).Where("id = ?", id).Delete(&JobRecord{})
	if res.Error != nil {
		return fmt.Errorf("delete job %s: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns up to limit jobs, newest first. A limit of zero or less returns
// no jobs. No matching rows is an empty list, not an error.
func (s *PostgresStore) List(ctx context.Context, limit int) ([]jobs.Job, error) {
	if limit <= 0 {
		return []jobs.Job{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	// Newest first; the ID breaks ties between equal timestamps.
	// It matches the jobs_created_at_id_idx index.
	var recs []JobRecord
	err := s.db.WithContext(ctx).
		Order("created_at DESC").Order("id DESC").
		Limit(limit).
		Find(&recs).Error
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}

	out := make([]jobs.Job, len(recs))
	for i, rec := range recs {
		out[i] = rec.toJob()
	}
	return out, nil
}

// admissionLockKey is the PostgreSQL advisory-lock key that serialises the
// "count queued, then insert" step of CreateIfQueueBelow. The lock is held
// only for that short transaction (it is released at commit or rollback).
const admissionLockKey int64 = 7_411_022

const (
	// admissionSlotCount is how many submissions per Forge process may be inside
	// the admission transaction (holding or waiting for the advisory lock) at
	// once. The lock already lets only one of them work at a time, so more than
	// a few add nothing except waiting database connections. Those connections
	// come from the same fixed pool the workers use, so a crowd of waiting
	// submissions starved claims, completions and lease renewals (measured in the
	// E-1 follow-up results). Waiting happens here in Go instead, where it holds
	// no connection.
	admissionSlotCount = 3

	// admissionWait is the longest a submission waits for a slot before it is
	// refused with ErrAdmissionBusy. It is deliberately short and separate from
	// queryTimeout: a long wait would only turn into a late timeout. The value is
	// a validated starting point, not a tuned optimum.
	admissionWait = 500 * time.Millisecond
)

// admissionSlots is the semaphore behind admissionSlotCount. The limit is per
// process: several Forge processes each have their own slots.
var admissionSlots = make(chan struct{}, admissionSlotCount)

// AdmissionSlots reports how many admission slots this process is using right
// now and how many exist, for the metrics endpoint.
func AdmissionSlots() (inUse, total int) { return len(admissionSlots), cap(admissionSlots) }

// acquireAdmissionSlot takes a slot, waiting at most admissionWait. It runs
// before any database work, so a refused or cancelled caller has not touched
// the pool. On success the caller must call the returned release exactly once.
func acquireAdmissionSlot(ctx context.Context) (release func(), err error) {
	release = func() { <-admissionSlots }
	select {
	case admissionSlots <- struct{}{}:
		return release, nil
	default:
	}
	timer := time.NewTimer(admissionWait)
	defer timer.Stop()
	select {
	case admissionSlots <- struct{}{}:
		return release, nil
	case <-timer.C:
		return nil, ErrAdmissionBusy
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for admission slot: %w", ctx.Err())
	}
}

// submitJobSQL runs the whole admission step in one database call (see the
// forge_submit_job migration). The result column is 0 inserted, 1 queue full,
// 2 id already exists, 3 idempotency key replay, 4 idempotency key conflict.
const submitJobSQL = `SELECT forge_submit_job(@lock, @max, @id, @type, @input, @created, @attempts, @timeout, @key) AS outcome`

// CreateIfQueueBelow inserts a queued job only if fewer than maxQueued jobs are
// queued. A plain COUNT followed by INSERT would let concurrent submissions
// all see "room" and all insert. Instead, every submission takes a
// transaction-scoped advisory lock, so the count that follows (a new statement,
// hence a fresh snapshot) already includes every earlier submission that
// committed. The cost is that submissions are serialised, so the time the lock
// is held limits submissions per second.
//
// The lock, the count and the insert run inside one database function
// (forge_submit_job), called with one statement: the lock is held for the
// function's own run time only. Done from Go as separate statements they cost
// about a dozen round trips inside the lock, which at 1 ms of network latency
// to the database cut throughput to a sixth.
//
// The count is read from the trigger-maintained queue_counter shards rather than
// computed with COUNT(*): that count scanned every queued row while the lock
// was held, so the lock's hold time grew with queue depth.
func (s *PostgresStore) CreateIfQueueBelow(ctx context.Context, j jobs.Job, maxQueued int) (jobs.Job, bool, error) {
	if j.ID == "" {
		return jobs.Job{}, false, ErrEmptyID
	}
	if j.Status != jobs.StatusQueued {
		return jobs.Job{}, false, fmt.Errorf("insert job %s: status must be queued, got %q", j.ID, j.Status)
	}
	// Admission capacity first: until a slot is held there is no pooled
	// connection and no database timeout running. The deferred release runs on
	// every exit, including errors, timeouts and cancellation.
	release, err := acquireAdmissionSlot(ctx)
	if err != nil {
		return jobs.Job{}, false, err
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	rec := toRecord(j)
	var key any // nil becomes SQL NULL: no idempotency key
	if rec.IdempotencyKey != nil {
		key = *rec.IdempotencyKey
	}
	maxAttempts, timeoutSeconds := rec.MaxAttempts, rec.TimeoutSeconds
	if maxAttempts == 0 {
		maxAttempts = jobs.DefaultMaxAttempts // same values as the column defaults
	}
	if timeoutSeconds == 0 {
		timeoutSeconds = jobs.DefaultTimeoutSeconds
	}
	var rows []struct{ Outcome int }
	err = s.db.WithContext(ctx).Raw(submitJobSQL,
		sql.Named("lock", admissionLockKey), sql.Named("max", int64(maxQueued)),
		sql.Named("id", j.ID), sql.Named("type", j.Type), sql.Named("input", string(rec.Input)),
		sql.Named("created", rec.CreatedAt), sql.Named("attempts", maxAttempts), sql.Named("timeout", timeoutSeconds), sql.Named("key", key)).
		Scan(&rows).Error
	if err != nil {
		return jobs.Job{}, false, fmt.Errorf("insert job %s: %w", j.ID, err)
	}
	if len(rows) != 1 {
		return jobs.Job{}, false, fmt.Errorf("insert job %s: unexpected result", j.ID)
	}
	switch rows[0].Outcome {
	case 0:
		return j, false, nil
	case 1:
		return jobs.Job{}, false, ErrQueueFull
	case 2:
		return jobs.Job{}, false, ErrAlreadyExists
	case 3:
		// A replay: return the job the key already points to, in its current
		// state. The key never changes once stored, so reading it outside the
		// admission lock is safe.
		var existing JobRecord
		err := s.db.WithContext(ctx).Where("idempotency_key = ?", *rec.IdempotencyKey).Take(&existing).Error
		if err != nil {
			return jobs.Job{}, false, fmt.Errorf("read job for idempotency key: %w", err)
		}
		return existing.toJob(), true, nil
	case 4:
		return jobs.Job{}, false, ErrIdempotencyConflict
	}
	return jobs.Job{}, false, fmt.Errorf("insert job %s: unexpected outcome %d", j.ID, rows[0].Outcome)
}

// submitJobsSQL stores a whole batch in one database call (see the
// forge_submit_jobs migration). The result is 0 stored, 1 queue full.
const submitJobsSQL = `SELECT forge_submit_jobs(@lock, @max, CAST(@rows AS jsonb)) AS outcome`

type batchRow struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	Input          json.RawMessage `json:"input"`
	CreatedAt      time.Time       `json:"created_at"`
	MaxAttempts    int             `json:"max_attempts"`
	TimeoutSeconds int             `json:"timeout_seconds"`
}

// CreateBatchIfQueueBelow implements Store. It takes ONE admission slot for the
// whole batch and stores every job in one transaction, so the lock hold time and
// the disk write are shared by all of them.
func (s *PostgresStore) CreateBatchIfQueueBelow(ctx context.Context, js []jobs.Job, maxQueued int) error {
	if len(js) == 0 {
		return errors.New("insert batch: no jobs")
	}
	rows := make([]batchRow, len(js))
	for i, j := range js {
		if j.ID == "" {
			return ErrEmptyID
		}
		if j.Status != jobs.StatusQueued {
			return fmt.Errorf("insert job %s: status must be queued, got %q", j.ID, j.Status)
		}
		rec := toRecord(j)
		rows[i] = batchRow{ID: j.ID, Type: j.Type, Input: json.RawMessage(rec.Input), CreatedAt: rec.CreatedAt,
			MaxAttempts: rec.MaxAttempts, TimeoutSeconds: rec.TimeoutSeconds}
		if rows[i].MaxAttempts == 0 {
			rows[i].MaxAttempts = jobs.DefaultMaxAttempts // same values as the column defaults
		}
		if rows[i].TimeoutSeconds == 0 {
			rows[i].TimeoutSeconds = jobs.DefaultTimeoutSeconds
		}
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("insert batch: %w", err)
	}

	release, err := acquireAdmissionSlot(ctx)
	if err != nil {
		return err
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var out []struct{ Outcome int }
	err = s.db.WithContext(ctx).Raw(submitJobsSQL,
		sql.Named("lock", admissionLockKey), sql.Named("max", int64(maxQueued)), sql.Named("rows", string(payload))).
		Scan(&out).Error
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return ErrAlreadyExists
	case err != nil:
		return fmt.Errorf("insert batch of %d jobs: %w", len(js), err)
	case len(out) != 1:
		return errors.New("insert batch: unexpected result")
	case out[0].Outcome == 1:
		return ErrQueueFull
	case out[0].Outcome != 0:
		return fmt.Errorf("insert batch: unexpected outcome %d", out[0].Outcome)
	}
	return nil
}

// CountQueued reports how many jobs have status "queued".
func (s *PostgresStore) CountQueued(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	return countQueued(s.db.WithContext(ctx))
}

func countQueued(db *gorm.DB) (int, error) {
	var n int64
	if err := db.Model(&JobRecord{}).Where("status = ?", string(jobs.StatusQueued)).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count queued jobs: %w", err)
	}
	return int(n), nil
}

// queueStatsSQL reads the queue state in ONE statement. PostgreSQL's default
// isolation level (READ COMMITTED) gives every STATEMENT its own snapshot, not
// every transaction, so two separate SELECTs could see a job as queued in one
// and running in the other (counted twice) or in neither (missed). A single
// statement, including all its CTEs, sees one snapshot, so the counts are
// consistent without a stricter transaction. now() is the transaction start,
// identical everywhere in the statement, so "due" and "age" use the same instant.
//
// The two filters match the partial indexes (jobs_due_idx where status='queued',
// jobs_running_idx where status='running'), so the work is proportional to the
// jobs that are queued or running, not to the table (finished jobs are never
// read). type is not in those indexes, so each such row is also fetched from the
// table. The LEFT JOIN ... ON true keeps one row, with the clock, even when
// nothing is queued or running.
const queueStatsSQL = `
WITH q AS (
  SELECT type, count(*) AS queued,
         min(next_run_at) FILTER (WHERE next_run_at <= now()) AS oldest_due
  FROM jobs WHERE status = 'queued' GROUP BY type
), r AS (
  SELECT type, count(*) AS running FROM jobs WHERE status = 'running' GROUP BY type
), per AS (
  SELECT COALESCE(q.type, r.type) AS type,
         COALESCE(q.queued, 0) AS queued,
         COALESCE(r.running, 0) AS running,
         EXTRACT(EPOCH FROM (now() - q.oldest_due))::float8 AS age_seconds
  FROM q FULL OUTER JOIN r ON q.type = r.type
)
SELECT n.collected_at, per.type, per.queued, per.running, per.age_seconds
FROM (SELECT now() AS collected_at) n LEFT JOIN per ON true
ORDER BY per.type`

type queueStatsRow struct {
	CollectedAt time.Time `gorm:"column:collected_at"`
	Type        *string   `gorm:"column:type"`
	Queued      *int64    `gorm:"column:queued"`
	Running     *int64    `gorm:"column:running"`
	AgeSeconds  *float64  `gorm:"column:age_seconds"`
}

// QueueStats implements Store.
func (s *PostgresStore) QueueStats(ctx context.Context) (QueueStats, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var rows []queueStatsRow
	if err := s.db.WithContext(ctx).Raw(queueStatsSQL).Scan(&rows).Error; err != nil {
		return QueueStats{}, fmt.Errorf("queue stats: %w", err)
	}
	if len(rows) == 0 {
		return QueueStats{}, errors.New("queue stats: the query returned no row")
	}
	out := QueueStats{CollectedAt: rows[0].CollectedAt.UTC()}
	for _, r := range rows {
		if r.Type == nil {
			continue // the clock row of an empty queue
		}
		t := QueueTypeStats{Type: *r.Type}
		if r.Queued != nil {
			t.Queued = int(*r.Queued)
		}
		if r.Running != nil {
			t.Running = int(*r.Running)
		}
		if r.AgeSeconds != nil {
			d := time.Duration(*r.AgeSeconds * float64(time.Second))
			t.OldestDueAge = &d
		}
		out.Types = append(out.Types, t)
	}
	return out, nil
}

// claimSQL claims the next due job in ONE statement, so there is no window
// between choosing a row and owning it. FOR UPDATE SKIP LOCKED lets several
// workers claim at once: a row another worker is claiming is skipped, never
// waited for. The row lock lasts only for this statement; nothing is held while
// the handler runs.
//
// claim_generation and attempt both increase on every claim but mean different
// things: the generation is the fencing token, the attempt counts executions.
// All timestamps come from the database clock (now()), never from a worker.
// started_at is kept if the job already has one (the FIRST start).
//
// GORM named parameters must not be followed directly by "::type", so casts
// are written CAST(@x AS type).
const claimSQL = `
WITH locked AS (
  SELECT id, octet_length(input::text) AS size, next_run_at, created_at FROM jobs
  WHERE status = 'queued' AND next_run_at <= now() AND attempt < max_attempts
    AND type = ANY(string_to_array(CAST(@types AS text), ','))
  ORDER BY next_run_at, created_at, id
  LIMIT @max
  FOR UPDATE SKIP LOCKED
), candidate AS (
  -- Cap the total input size of one batch so the answer stays small: a row is
  -- taken while the rows before it add up to less than the cap (so the first row
  -- is always taken, however large). Locked rows that are not taken stay queued.
  SELECT id FROM (
    SELECT id, size, sum(size) OVER (ORDER BY next_run_at, created_at, id) AS running FROM locked
  ) t WHERE running - size < CAST(@bytes AS bigint)
)
UPDATE jobs j SET
  status = 'running',
  claim_generation = j.claim_generation + 1,
  attempt = j.attempt + 1,
  worker_id = @worker,
  started_at = COALESCE(j.started_at, now()),
  attempt_started_at = now(),
  lease_expires_at = now() + make_interval(secs => CAST(@lease AS float8))
FROM candidate
WHERE j.id = candidate.id
RETURNING j.id, j.type, j.input::text AS input, j.created_at, j.started_at, j.attempt, j.max_attempts, j.timeout_seconds, j.claim_generation,
  GREATEST(EXTRACT(EPOCH FROM (now() - j.next_run_at)), 0)::float8 AS wait_seconds`

// MaxClaimBatch is the most jobs one ClaimBatch call may take, and
// claimBatchInputBytes the input size after which a batch stops growing.
const (
	MaxClaimBatch        = 20
	claimBatchInputBytes = 512 << 10
)

type claimRow struct {
	ID              string     `gorm:"column:id"`
	Type            string     `gorm:"column:type"`
	Input           string     `gorm:"column:input"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	StartedAt       *time.Time `gorm:"column:started_at"`
	Attempt         int        `gorm:"column:attempt"`
	MaxAttempts     int        `gorm:"column:max_attempts"`
	TimeoutSeconds  int        `gorm:"column:timeout_seconds"`
	ClaimGeneration int64      `gorm:"column:claim_generation"`
	WaitSeconds     float64    `gorm:"column:wait_seconds"`
}

// joinClaimTypes turns the type filter into one comma-separated string. GORM
// expands a Go slice given as a named parameter into a SQL value list, which
// does not fit "= ANY(array)", so the list travels as a single text parameter
// and PostgreSQL splits it. A type that is empty or contains a comma would
// change the meaning of that string, so both are rejected here instead of
// trusting the caller. An empty list is an error, never "all types".
func joinClaimTypes(types []string) (string, error) {
	if len(types) == 0 {
		return "", ErrNoClaimTypes
	}
	for _, t := range types {
		if t == "" || strings.Contains(t, ",") {
			return "", fmt.Errorf("claim: invalid job type %q in filter", t)
		}
	}
	return strings.Join(types, ","), nil
}

// Claim implements Store.
func (s *PostgresStore) Claim(ctx context.Context, workerID string, lease time.Duration, types []string) (ClaimedJob, bool, error) {
	cs, err := s.ClaimBatch(ctx, workerID, lease, types, 1)
	if err != nil || len(cs) == 0 {
		return ClaimedJob{}, false, err
	}
	return cs[0], true, nil
}

// ClaimBatch implements Store. It takes up to max jobs in ONE statement, so the
// scan of the head of the queue (the dominant database cost of a claim, because
// many claimers skip each other's locked rows there) and the counter update are
// shared by every job of the batch.
func (s *PostgresStore) ClaimBatch(ctx context.Context, workerID string, lease time.Duration, types []string, max int) ([]ClaimedJob, error) {
	if max < 1 || max > MaxClaimBatch {
		return nil, fmt.Errorf("claim: max must be from 1 to %d, got %d", MaxClaimBatch, max)
	}
	typeList, err := joinClaimTypes(types)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var rows []claimRow
	err = s.db.WithContext(ctx).
		Raw(claimSQL, sql.Named("worker", workerID), sql.Named("lease", lease.Seconds()), sql.Named("types", typeList),
			sql.Named("max", max), sql.Named("bytes", int64(claimBatchInputBytes))).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	out := make([]ClaimedJob, len(rows))
	for i, r := range rows {
		out[i] = ClaimedJob{
			Job: jobs.Job{
				ID:             r.ID,
				Type:           r.Type,
				Status:         jobs.StatusRunning,
				Input:          json.RawMessage(r.Input),
				CreatedAt:      r.CreatedAt.UTC(),
				StartedAt:      utcPtr(r.StartedAt),
				TimeoutSeconds: r.TimeoutSeconds, // the execution deadline of this attempt
			},
			Attempt:     r.Attempt,
			MaxAttempts: r.MaxAttempts,
			Generation:  r.ClaimGeneration,
			WorkerID:    workerID,
			QueueWait:   time.Duration(r.WaitSeconds * float64(time.Second)),
		}
	}
	return out, nil
}

// The two terminal writes below are FENCED: they match the row only while it is
// still "running" under this claim's generation and worker. A reaper (a later
// task) or any newer claim changes the generation, after which these match
// zero rows. The lease deadline is deliberately NOT checked here: time alone
// does not revoke ownership; only a committed change of the generation does.
// Both clear the lease and worker, as the table's CHECK constraints require of
// a finished job.
const completeSQL = `
UPDATE jobs SET
  status = 'succeeded', result = CAST(@result AS jsonb), finished_at = now(),
  error_code = NULL, error_message = NULL, lease_expires_at = NULL, worker_id = NULL
WHERE id = @id AND status = 'running' AND claim_generation = @gen AND worker_id = @worker
RETURNING type, COALESCE(EXTRACT(EPOCH FROM (now() - attempt_started_at)), 0)::float8 AS attempt_seconds`

const failSQL = `
UPDATE jobs SET
  status = 'failed', result = NULL, finished_at = now(),
  error_code = @code, error_message = @message, lease_expires_at = NULL, worker_id = NULL
WHERE id = @id AND status = 'running' AND claim_generation = @gen AND worker_id = @worker
RETURNING type, COALESCE(EXTRACT(EPOCH FROM (now() - attempt_started_at)), 0)::float8 AS attempt_seconds`

// releaseSQL returns a claimed job to the queue for a clean shutdown. attempt
// is refunded because the interruption was Forge's, not the handler's; the
// generation is left alone (only a claim or the reaper changes it). Any error
// recorded by an EARLIER attempt is kept: nothing failed here. The lifecycle
// CHECK for queued needs no finish time, result, lease or worker, all cleared.
const releaseSQL = `
UPDATE jobs SET
  status = 'queued', attempt = GREATEST(attempt - 1, 0), next_run_at = now(),
  lease_expires_at = NULL, worker_id = NULL
WHERE id = @id AND status = 'running' AND claim_generation = @gen AND worker_id = @worker
RETURNING type`

// fenced runs one of the fenced writes above. Each ends in RETURNING type (and
// the attempt duration), so zero rows means the claim is no longer current
// (ErrLostOwnership), distinct from a database error, and the job's type comes
// back from the same statement.
func (s *PostgresStore) fenced(ctx context.Context, op string, c ClaimedJob, query string, args ...any) (Finished, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var rows []finishedRow
	if err := s.db.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return Finished{}, fmt.Errorf("%s job %s: %w", op, c.Job.ID, err)
	}
	if len(rows) == 0 {
		return Finished{}, ErrLostOwnership
	}
	return rows[0].finished(), nil
}

// finishedRow is the RETURNING list of the fenced writes (releaseSQL returns
// only the type, leaving the duration zero).
type finishedRow struct {
	Type           string  `gorm:"column:type"`
	Status         string  `gorm:"column:status"`
	AttemptSeconds float64 `gorm:"column:attempt_seconds"`
}

func (r finishedRow) finished() Finished {
	return Finished{Type: r.Type, AttemptDuration: time.Duration(r.AttemptSeconds * float64(time.Second))}
}

// Release implements Store.
func (s *PostgresStore) Release(ctx context.Context, c ClaimedJob) (string, error) {
	f, err := s.fenced(ctx, "release", c, releaseSQL,
		sql.Named("id", c.Job.ID), sql.Named("gen", c.Generation), sql.Named("worker", c.WorkerID))
	return f.Type, err
}

// Complete implements Store.
func (s *PostgresStore) Complete(ctx context.Context, c ClaimedJob, result json.RawMessage) (Finished, error) {
	var res any // nil becomes SQL NULL
	if len(result) > 0 {
		res = string(result)
	}
	return s.fenced(ctx, "complete", c, completeSQL,
		sql.Named("result", res), sql.Named("id", c.Job.ID), sql.Named("gen", c.Generation), sql.Named("worker", c.WorkerID))
}

// completeBatchSQL is completeSQL for many jobs in one statement and one commit.
// Every job is still fenced individually: a row whose generation or worker no
// longer matches is simply not updated, and so is absent from RETURNING; the
// others in the batch are unaffected. The JSON payload is the list of
// {id, gen, worker, result} items.
const completeBatchSQL = `
UPDATE jobs j SET
  status = 'succeeded', result = NULLIF(v.result, 'null'::jsonb), finished_at = now(),
  error_code = NULL, error_message = NULL, lease_expires_at = NULL, worker_id = NULL
FROM jsonb_to_recordset(CAST(@items AS jsonb)) AS v(id text, gen bigint, worker text, result jsonb)
WHERE j.id = v.id AND j.status = 'running' AND j.claim_generation = v.gen AND j.worker_id = v.worker
RETURNING j.id, j.type, COALESCE(EXTRACT(EPOCH FROM (now() - j.attempt_started_at)), 0)::float8 AS attempt_seconds`

type completeBatchItem struct {
	ID     string          `json:"id"`
	Gen    int64           `json:"gen"`
	Worker string          `json:"worker"`
	Result json.RawMessage `json:"result"` // nil marshals as null => SQL-level JSON null
}

// CompleteBatch implements Store.
func (s *PostgresStore) CompleteBatch(ctx context.Context, items []CompleteItem) ([]CompleteOutcome, error) {
	if len(items) == 0 {
		return nil, errors.New("complete batch: no items")
	}
	rows := make([]completeBatchItem, len(items))
	for i, it := range items {
		rows[i] = completeBatchItem{ID: it.Claim.Job.ID, Gen: it.Claim.Generation, Worker: it.Claim.WorkerID, Result: it.Result}
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return nil, fmt.Errorf("complete batch: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var done []struct {
		ID             string  `gorm:"column:id"`
		Type           string  `gorm:"column:type"`
		AttemptSeconds float64 `gorm:"column:attempt_seconds"`
	}
	if err := s.db.WithContext(ctx).Raw(completeBatchSQL, sql.Named("items", string(payload))).Scan(&done).Error; err != nil {
		return nil, fmt.Errorf("complete batch of %d jobs: %w", len(items), err)
	}
	byID := make(map[string]int, len(done))
	for i, d := range done {
		byID[d.ID] = i
	}
	out := make([]CompleteOutcome, len(items))
	for i, it := range items {
		if k, ok := byID[it.Claim.Job.ID]; ok {
			d := done[k]
			out[i] = CompleteOutcome{Done: true, Finished: Finished{Type: d.Type, AttemptDuration: time.Duration(d.AttemptSeconds * float64(time.Second))}}
		}
	}
	return out, nil
}

// Fail implements Store.
func (s *PostgresStore) Fail(ctx context.Context, c ClaimedJob, info jobs.ErrorInfo) (Finished, error) {
	return s.fenced(ctx, "fail", c, failSQL,
		sql.Named("code", info.Code), sql.Named("message", info.Message),
		sql.Named("id", c.Job.ID), sql.Named("gen", c.Generation), sql.Named("worker", c.WorkerID))
}

// failAttemptSQL decides requeue-versus-terminal inside the database, using the
// attempt counters as they are persisted now (not a copy the worker loaded
// earlier). In an UPDATE's SET list, column names mean the OLD row values, so
// "attempt < max_attempts" is evaluated before anything changes.
//
// Requeue keeps started_at and attempt_started_at (history of the attempts so
// far), keeps claim_generation (only claims and reaper transitions bump it) and
// stores the latest failure so it stays visible while the job waits. Terminal
// failure on an exhausted retry stores attempts_exhausted, with the last safe
// message appended so the real reason is not lost. A permanent failure keeps
// its own code and message.
const failAttemptSQL = `
UPDATE jobs SET
  status = CASE WHEN CAST(@retryable AS boolean) AND attempt < max_attempts THEN 'queued' ELSE 'failed' END,
  next_run_at = CASE WHEN CAST(@retryable AS boolean) AND attempt < max_attempts
                     THEN now() + make_interval(secs => CAST(@delay AS float8)) ELSE next_run_at END,
  finished_at = CASE WHEN CAST(@retryable AS boolean) AND attempt < max_attempts THEN NULL ELSE now() END,
  error_code = CASE WHEN CAST(@retryable AS boolean) AND attempt >= max_attempts
                    THEN 'attempts_exhausted' ELSE @code END,
  error_message = CASE WHEN CAST(@retryable AS boolean) AND attempt >= max_attempts
                       THEN 'No attempts left. Last error: ' || @message ELSE @message END,
  result = NULL, lease_expires_at = NULL, worker_id = NULL
WHERE id = @id AND status = 'running' AND claim_generation = @gen AND worker_id = @worker
RETURNING status, type, COALESCE(EXTRACT(EPOCH FROM (now() - attempt_started_at)), 0)::float8 AS attempt_seconds`

// FailAttempt implements Store.
func (s *PostgresStore) FailAttempt(ctx context.Context, c ClaimedJob, info jobs.ErrorInfo, retryable bool, retryDelay time.Duration) (bool, Finished, error) {
	if retryDelay < 0 {
		// A programming error, not lost ownership: refuse before touching the database.
		return false, Finished{}, fmt.Errorf("fail attempt of job %s: negative retry delay %s", c.Job.ID, retryDelay)
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var rows []finishedRow
	tx := s.db.WithContext(ctx).Raw(failAttemptSQL,
		sql.Named("retryable", retryable), sql.Named("delay", retryDelay.Seconds()),
		sql.Named("code", info.Code), sql.Named("message", info.Message),
		sql.Named("id", c.Job.ID), sql.Named("gen", c.Generation), sql.Named("worker", c.WorkerID)).Scan(&rows)
	if tx.Error != nil {
		return false, Finished{}, fmt.Errorf("fail attempt of job %s: %w", c.Job.ID, tx.Error)
	}
	if len(rows) == 0 {
		return false, Finished{}, ErrLostOwnership
	}
	return rows[0].Status == string(jobs.StatusQueued), rows[0].finished(), nil
}

// fencedResult separates the three outcomes of a fenced write that returns no
// row (the lease renewal): a database error, zero rows (ownership lost) and
// success.
func fencedResult(tx *gorm.DB, op string, c ClaimedJob) error {
	if tx.Error != nil {
		return fmt.Errorf("%s job %s: %w", op, c.Job.ID, tx.Error)
	}
	if tx.RowsAffected == 0 {
		return ErrLostOwnership
	}
	return nil
}

// renewSQL is fenced exactly like the terminal writes. The lease deadline is
// deliberately not part of the WHERE clause: an overdue claim that nobody has
// reaped yet still owns its generation, so a late renewal is accepted; only a
// committed reaper transition (which changes claim_generation) ends ownership.
const renewSQL = `
UPDATE jobs SET lease_expires_at = now() + make_interval(secs => CAST(@lease AS float8))
WHERE id = @id AND status = 'running' AND claim_generation = @gen AND worker_id = @worker`

// RenewLease implements Store.
func (s *PostgresStore) RenewLease(ctx context.Context, c ClaimedJob, lease time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	tx := s.db.WithContext(ctx).Exec(renewSQL,
		sql.Named("lease", lease.Seconds()), sql.Named("id", c.Job.ID),
		sql.Named("gen", c.Generation), sql.Named("worker", c.WorkerID))
	return fencedResult(tx, "renew lease of", c)
}

// reapSQL finds expired running jobs and transitions them in ONE statement.
//
//   - FOR UPDATE SKIP LOCKED: a row that a worker is renewing or finishing at
//     this moment is skipped rather than waited for, and two reapers never
//     take the same row. If a renewal commits first, the lease is no longer
//     expired and the row is not touched (the UPDATE re-checks it too).
//   - claim_generation + 1 on every row it changes: the former owner's
//     generation is dead from the moment this commits, whatever that worker
//     still believes.
//   - attempt is NOT changed (it counts executions started, and the crashed
//     one already counted); started_at and attempt_started_at are kept.
//   - With attempts left: back to queued, not claimable until the recovery
//     backoff has passed, with a safe error code recording why. With none
//     left: terminal failed, which no later pass touches (it is not running).
//
// Every resulting row satisfies the table's CHECK constraints.
const reapSQL = `
WITH expired AS (
  SELECT id FROM jobs
  WHERE status = 'running' AND lease_expires_at <= now()
  ORDER BY lease_expires_at, id
  LIMIT @batch
  FOR UPDATE SKIP LOCKED
), reaped AS (
  UPDATE jobs j SET
    claim_generation = j.claim_generation + 1,
    status = CASE WHEN j.attempt >= j.max_attempts THEN 'failed' ELSE 'queued' END,
    next_run_at = CASE WHEN j.attempt >= j.max_attempts THEN j.next_run_at
                       ELSE now() + make_interval(secs => CAST(@backoff AS float8)) END,
    finished_at = CASE WHEN j.attempt >= j.max_attempts THEN now() ELSE NULL END,
    result = NULL,
    error_code = CASE WHEN j.attempt >= j.max_attempts THEN 'attempts_exhausted' ELSE 'lease_expired' END,
    error_message = CASE WHEN j.attempt >= j.max_attempts
                         THEN 'The job did not finish within its allowed attempts: its worker stopped renewing its lease.'
                         ELSE 'The worker running this job stopped renewing its lease; the job was queued to run again.' END,
    lease_expires_at = NULL,
    worker_id = NULL
  FROM expired
  WHERE j.id = expired.id AND j.status = 'running' AND j.lease_expires_at <= now()
  RETURNING j.status
)
SELECT status, count(*) AS n FROM reaped GROUP BY status`

const purgeSQL = `
WITH doomed AS (
  SELECT id FROM jobs
  WHERE (status = 'succeeded' AND CAST(@succeeded_before AS timestamptz) IS NOT NULL AND finished_at < CAST(@succeeded_before AS timestamptz))
     OR (status = 'failed'    AND CAST(@failed_before AS timestamptz)    IS NOT NULL AND finished_at < CAST(@failed_before AS timestamptz))
  ORDER BY finished_at
  LIMIT @batch
  FOR UPDATE SKIP LOCKED
), purged AS (
  DELETE FROM jobs j USING doomed WHERE j.id = doomed.id
  RETURNING j.status
)
SELECT status, count(*) AS n FROM purged GROUP BY status`

// PurgeFinished implements Store.
func (s *PostgresStore) PurgeFinished(ctx context.Context, succeededBefore, failedBefore *time.Time, limit int) (PurgeResult, error) {
	if limit < 1 {
		return PurgeResult{}, fmt.Errorf("purge limit must be at least 1, got %d", limit)
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var rows []struct {
		Status string `gorm:"column:status"`
		N      int    `gorm:"column:n"`
	}
	err := s.db.WithContext(ctx).
		Raw(purgeSQL, sql.Named("succeeded_before", succeededBefore), sql.Named("failed_before", failedBefore), sql.Named("batch", limit)).
		Scan(&rows).Error
	if err != nil {
		return PurgeResult{}, fmt.Errorf("purge finished jobs: %w", err)
	}
	var res PurgeResult
	for _, r := range rows {
		switch r.Status {
		case string(jobs.StatusSucceeded):
			res.Succeeded = r.N
		case string(jobs.StatusFailed):
			res.Failed = r.N
		}
	}
	return res, nil
}

// ReapExpired implements Store.
func (s *PostgresStore) ReapExpired(ctx context.Context, limit int, recoveryBackoff time.Duration) (ReapResult, error) {
	if limit < 1 {
		return ReapResult{}, fmt.Errorf("reap limit must be at least 1, got %d", limit)
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var rows []struct {
		Status string `gorm:"column:status"`
		N      int    `gorm:"column:n"`
	}
	err := s.db.WithContext(ctx).
		Raw(reapSQL, sql.Named("batch", limit), sql.Named("backoff", recoveryBackoff.Seconds())).
		Scan(&rows).Error
	if err != nil {
		return ReapResult{}, fmt.Errorf("reap expired jobs: %w", err)
	}
	var res ReapResult
	for _, r := range rows {
		switch r.Status {
		case string(jobs.StatusQueued):
			res.Requeued = r.N
		case string(jobs.StatusFailed):
			res.Exhausted = r.N
		}
	}
	return res, nil
}
