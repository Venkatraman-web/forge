// Package jobs defines the shared job model. It knows nothing about HTTP,
// storage, handlers or workers; every other package may import it.
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Status is the lifecycle state of a job.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// Retry budget. MaxAttempts counts TOTAL execution attempts, the first one
// included: 1 means the job runs once and is never retried. The limits mirror
// the jobs table (column default and the jobs_attempts_valid CHECK), which
// remains the final guard.
const (
	DefaultMaxAttempts = 3
	MaxAttemptsLimit   = 20
)

// Execution timeout. TimeoutSeconds is the longest ONE attempt of a job may
// execute before its handler's context is cancelled; it is not the total time
// across retries, and it is not a database, polling, lease or shutdown timeout.
// The default is the long-standing fixed deadline. MaxTimeoutSecondsLimit is
// the hard ceiling the schema allows (jobs_timeout_valid); the server's own
// maximum (FORGE_MAX_JOB_TIMEOUT) is at most this and defaults to the default.
// The ceiling is a conservative placeholder until the owner decides the
// maximum job duration (ADR 0004, D-3).
const (
	DefaultTimeoutSeconds  = 30
	MaxTimeoutSecondsLimit = 3600
)

// TypePolicy is the server-side policy of one job type (see internal/policy).
// A zero field means "not set": the server-wide default or limit applies.
type TypePolicy struct {
	DefaultMaxAttempts int `json:"default_max_attempts,omitempty"`
	MaxMaxAttempts     int `json:"max_max_attempts,omitempty"`
	DefaultTimeoutSecs int `json:"default_timeout_seconds,omitempty"`
	MaxTimeoutSecs     int `json:"max_timeout_seconds,omitempty"`
}

// Options are the per-job settings a submitter may choose. A zero field means
// "use the default". The delay between attempts is server policy and is
// deliberately not an option.
type Options struct {
	MaxAttempts    int
	TimeoutSeconds int
	// IdempotencyKey, when not empty, makes the submission safe to repeat: a
	// second submission with the same key and the same content returns the job
	// stored by the first instead of creating another. It is not echoed in
	// responses.
	IdempotencyKey string
}

// ErrorInfo is the safe, public description of why a job failed. It must never
// contain stack traces, secrets or raw input.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Job is one unit of background work. Its lifecycle is queued -> running ->
// succeeded | failed. Status changes are made only by conditional SQL in the
// store (Claim, Complete, Fail), never by editing a Job in memory and saving
// it, because an in-memory copy can be stale.
//
// Input is kept internally and is hidden from JSON (`json:"-"`) so API
// responses do not echo the raw payload by default.
type Job struct {
	ID     string          `json:"job_id"`
	Type   string          `json:"type"`
	Status Status          `json:"status"`
	Input  json.RawMessage `json:"-"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorInfo      `json:"error,omitempty"`
	// MaxAttempts is the job's total allowed execution attempts (see the
	// constants above): it is the retry budget, not the delay between attempts.
	MaxAttempts int `json:"max_attempts"`
	// TimeoutSeconds is the execution timeout of each attempt (see the
	// constants above).
	TimeoutSeconds int        `json:"timeout_seconds"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	// IdempotencyKey is the submitter's key for this job, if it sent one.
	IdempotencyKey string `json:"-"`
}

// NewID returns a job ID such as "job_9f86d081884c7d65": a "job_" prefix plus
// 8 random bytes written as 16 hexadecimal characters. If the system's random
// source fails, the error is returned; we never fall back to a guessable ID.
func NewID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}
	return "job_" + hex.EncodeToString(b), nil
}

// New creates a queued job with a fresh ID.
func New(jobType string, input json.RawMessage, now time.Time) (Job, error) {
	id, err := NewID()
	if err != nil {
		return Job{}, err
	}
	return Job{
		ID:        id,
		Type:      jobType,
		Status:    StatusQueued,
		Input:     input,
		CreatedAt: now,
		// The default is set here so API responses show the effective budget;
		// the table default is the same value.
		MaxAttempts:    DefaultMaxAttempts,
		TimeoutSeconds: DefaultTimeoutSeconds,
	}, nil
}
