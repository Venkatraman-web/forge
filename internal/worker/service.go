package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Venkatraman-web/forge/internal/jobs"
	"github.com/Venkatraman-web/forge/internal/metrics"
	"github.com/Venkatraman-web/forge/internal/store"
)

// Errors the HTTP layer can tell apart with errors.Is.
var (
	ErrUnsupportedJobType = errors.New("unsupported job type")
	ErrInvalidInput       = errors.New("invalid job input")
	ErrQueueFull          = errors.New("job queue is full")
	ErrAdmissionBusy      = errors.New("admission capacity busy")
	ErrInvalidOption      = errors.New("invalid job option")
	// ErrIdempotencyConflict: the idempotency key was already used for a job
	// with different content.
	ErrIdempotencyConflict = errors.New("idempotency key already used for a different job")
)

// Service accepts new jobs: it validates them and records them in PostgreSQL.
// That is all it takes to queue a job: the row IS the work item, and workers
// (the customer's own processes, through the worker API) find it by claiming
// from the database. There is no second "enqueue" step that could fail and
// need undoing.
//
// The job type must be on the allowlist (the config file's job types, or
// FORGE_REMOTE_JOB_TYPES) and the input only has to be valid JSON: the code
// that understands it runs in the customer's own worker, not in Forge.
type Service struct {
	store     store.Store
	allowed   map[string]bool
	maxQueued int
	// maxTimeoutSeconds is the server's cap on a job's execution timeout
	// (FORGE_MAX_JOB_TIMEOUT): between jobs.DefaultTimeoutSeconds and
	// jobs.MaxTimeoutSecondsLimit. A submission above it is refused before
	// anything is stored.
	maxTimeoutSeconds int
	// policies are the optional per-type defaults and maxima (UsePolicy).
	policies map[string]jobs.TypePolicy
	metrics  *metrics.Metrics // optional; nil records nothing

	// Queue-full rejections, per job type. They live in this process's memory
	// only: they reset when Forge restarts and are not shared between Forge
	// processes, so they are a diagnostic for THIS process, never a deployment
	// total. Only submissions that passed validation are counted, so the keys
	// are limited to the configured job types.
	startedAt time.Time
	rejMu     sync.Mutex
	rejected  map[string]uint64
}

// maxRemoteJobTypes bounds the development allowlist.
const maxRemoteJobTypes = 32

var remoteJobTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// ParseRemoteJobTypes parses the FORGE_REMOTE_JOB_TYPES value: a comma-separated
// list of job types. Each entry is trimmed and must match
// ^[a-z][a-z0-9_.-]{0,63}$ (which also rules out commas and control
// characters). Empty entries (",,", a trailing comma) are rejected rather than
// skipped, so a typo cannot silently shrink the list. Duplicates are accepted
// and collapsed. The result is sorted, so the same input always gives the same
// output. An empty list is an error: it must never mean "anything".
func ParseRemoteJobTypes(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("no job types given")
	}
	seen := make(map[string]bool)
	for _, entry := range strings.Split(value, ",") {
		t := strings.TrimSpace(entry)
		if t == "" {
			return nil, errors.New("empty entry in the job type list")
		}
		if !remoteJobTypePattern.MatchString(t) {
			return nil, fmt.Errorf("invalid job type %q: use lowercase letters, digits, '_', '.', '-', starting with a letter, at most 64 characters", t)
		}
		seen[t] = true
	}
	if len(seen) > maxRemoteJobTypes {
		return nil, fmt.Errorf("too many job types: %d (at most %d)", len(seen), maxRemoteJobTypes)
	}
	types := make([]string, 0, len(seen))
	for t := range seen {
		types = append(types, t)
	}
	sort.Strings(types)
	return types, nil
}

// ValidateMaxTimeout validates the server's maximum job timeout: whole seconds,
// at least the default (the maximum may be raised, never lowered below what
// jobs submitted without the option get) and at most the schema's ceiling.
func ValidateMaxTimeout(d time.Duration) (int, error) {
	switch {
	case d%time.Second != 0:
		return 0, fmt.Errorf("maximum job timeout must be a whole number of seconds, got %s", d)
	case d < jobs.DefaultTimeoutSeconds*time.Second || d > jobs.MaxTimeoutSecondsLimit*time.Second:
		return 0, fmt.Errorf("maximum job timeout must be from %ds to %ds, got %s", jobs.DefaultTimeoutSeconds, jobs.MaxTimeoutSecondsLimit, d)
	}
	return int(d / time.Second), nil
}

// UseMetrics makes the service count submissions by result. Call it before
// the service handles requests.
func (s *Service) UseMetrics(m *metrics.Metrics) { s.metrics = m }

// NewService creates the submission service.
// allowedTypes is the development allowlist (see ParseRemoteJobTypes); an empty
// list is refused so the service can never accept every type.
func NewService(jobStore store.Store, allowedTypes []string, maxQueued int, maxTimeout time.Duration) (*Service, error) {
	switch {
	case jobStore == nil:
		return nil, errors.New("service needs a store")
	case len(allowedTypes) == 0:
		return nil, errors.New("remote service needs at least one allowed job type")
	case maxQueued < 1:
		return nil, fmt.Errorf("max queued jobs must be at least 1, got %d", maxQueued)
	}
	maxTimeoutSeconds, err := ValidateMaxTimeout(maxTimeout)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(allowedTypes))
	for _, t := range allowedTypes {
		allowed[t] = true
	}
	return &Service{store: jobStore, allowed: allowed, maxQueued: maxQueued, maxTimeoutSeconds: maxTimeoutSeconds, startedAt: time.Now().UTC(), rejected: map[string]uint64{}}, nil
}

// check applies the mode's type and input rules.
func (s *Service) check(jobType string, input json.RawMessage) error {
	if !s.allowed[jobType] {
		return fmt.Errorf("%w: %q", ErrUnsupportedJobType, jobType)
	}
	if !json.Valid(input) {
		return fmt.Errorf("%w: input is not valid JSON", ErrInvalidInput)
	}
	return nil
}

// Submit validates the input and stores a queued job, unless the backlog is
// already at its limit (then it stores nothing and returns ErrQueueFull), or
// when too many submissions are being admitted at once (ErrAdmissionBusy,
// also nothing stored). A
// database failure while deciding is returned as an error: work is never
// accepted when the limit could not be checked.
//
// With opts.IdempotencyKey set, repeating a submission is safe: the same key
// with the same content returns the job stored the first time (replayed true,
// nothing new stored, even if the queue is full); the same key with different
// content returns ErrIdempotencyConflict.
func (s *Service) Submit(ctx context.Context, jobType string, input json.RawMessage, opts jobs.Options) (job jobs.Job, replayed bool, err error) {
	job, replayed, err = s.submit(ctx, jobType, input, opts)
	if replayed && err == nil {
		s.metrics.Submission(jobType, metrics.ResultReplayed)
	} else {
		s.metrics.Submission(jobType, submissionResult(err))
	}
	return job, replayed, err
}

// submissionResult names a Submit outcome for the metrics.
func submissionResult(err error) string {
	switch {
	case err == nil:
		return metrics.ResultAccepted
	case errors.Is(err, ErrQueueFull):
		return metrics.ResultQueueFull
	case errors.Is(err, ErrAdmissionBusy):
		return metrics.ResultAdmissionBusy
	case errors.Is(err, ErrUnsupportedJobType), errors.Is(err, ErrInvalidInput), errors.Is(err, ErrInvalidOption), errors.Is(err, ErrIdempotencyConflict):
		return metrics.ResultInvalid
	}
	return metrics.ResultError
}

func (s *Service) submit(ctx context.Context, jobType string, input json.RawMessage, opts jobs.Options) (jobs.Job, bool, error) {
	job, err := s.prepare(jobType, input, opts)
	if err != nil {
		return jobs.Job{}, false, err
	}
	stored, replayed, err := s.store.CreateIfQueueBelow(ctx, job, s.maxQueued)
	switch {
	case errors.Is(err, store.ErrQueueFull):
		s.countRejection(jobType)
		return jobs.Job{}, false, ErrQueueFull
	case errors.Is(err, store.ErrAdmissionBusy):
		// Not counted with queue_full: the queue may have room.
		return jobs.Job{}, false, ErrAdmissionBusy
	case errors.Is(err, store.ErrIdempotencyConflict):
		return jobs.Job{}, false, ErrIdempotencyConflict
	case err != nil:
		return jobs.Job{}, false, fmt.Errorf("store job: %w", err)
	}
	return stored, replayed, nil
}

// prepare validates one submission and builds the queued job, without storing it.
// A single submission and every item of a batch go through the same rules.
func (s *Service) prepare(jobType string, input json.RawMessage, opts jobs.Options) (jobs.Job, error) {
	if err := s.check(jobType, input); err != nil {
		return jobs.Job{}, err
	}
	// Precedence for each setting: the submitter's value, then the job type's
	// default from the config file, then the server-wide default. A type's
	// maximum can only lower the server-wide limit, never raise it.
	pol := s.policies[jobType]
	maxAttemptsLimit := jobs.MaxAttemptsLimit
	if pol.MaxMaxAttempts > 0 && pol.MaxMaxAttempts < maxAttemptsLimit {
		maxAttemptsLimit = pol.MaxMaxAttempts
	}
	timeoutLimit := s.maxTimeoutSeconds
	if pol.MaxTimeoutSecs > 0 && pol.MaxTimeoutSecs < timeoutLimit {
		timeoutLimit = pol.MaxTimeoutSecs
	}
	if opts.MaxAttempts < 0 || opts.MaxAttempts > maxAttemptsLimit {
		return jobs.Job{}, fmt.Errorf("%w: max_attempts must be from 1 to %d for this job type", ErrInvalidOption, maxAttemptsLimit)
	}
	if opts.TimeoutSeconds < 0 || opts.TimeoutSeconds > timeoutLimit {
		// Checked here, before any insert: a client cannot get past the maximum by
		// asking for more; the operator raises it with FORGE_MAX_JOB_TIMEOUT or the
		// job type's max_timeout.
		return jobs.Job{}, fmt.Errorf("%w: timeout_seconds must be a whole number from 1 to %d for this job type", ErrInvalidOption, timeoutLimit)
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = pol.DefaultMaxAttempts // may stay 0: jobs.New's default applies
	}
	if opts.TimeoutSeconds == 0 {
		opts.TimeoutSeconds = pol.DefaultTimeoutSecs
	}

	job, err := jobs.New(jobType, input, time.Now().UTC())
	if err != nil {
		return jobs.Job{}, fmt.Errorf("create job: %w", err)
	}
	if opts.MaxAttempts > 0 {
		job.MaxAttempts = opts.MaxAttempts // otherwise the default set by jobs.New stays
	}
	if opts.TimeoutSeconds > 0 {
		job.TimeoutSeconds = opts.TimeoutSeconds
	}
	job.IdempotencyKey = opts.IdempotencyKey
	return job, nil
}

// BatchItem is one job of a batch submission.
type BatchItem struct {
	Type  string
	Input json.RawMessage
	Opts  jobs.Options
}

// MaxBatchSize is the most jobs one batch may hold.
const MaxBatchSize = 100

// ItemError says which item of a batch was invalid. Unwrap gives the usual
// ErrUnsupportedJobType, ErrInvalidInput or ErrInvalidOption.
type ItemError struct {
	Index int
	Err   error
}

func (e *ItemError) Error() string { return fmt.Sprintf("jobs[%d]: %v", e.Index, e.Err) }
func (e *ItemError) Unwrap() error { return e.Err }

// SubmitBatch validates every item, then stores them all in one transaction with
// one commit, or none: an invalid item rejects the whole batch (an *ItemError),
// and a batch that would push the queue past its limit stores nothing and returns
// ErrQueueFull. The returned jobs are in request order. Metrics count every item
// with the batch's outcome (for an invalid batch, only the offending item).
func (s *Service) SubmitBatch(ctx context.Context, items []BatchItem) ([]jobs.Job, error) {
	if len(items) == 0 || len(items) > MaxBatchSize {
		return nil, fmt.Errorf("%w: a batch holds 1 to %d jobs", ErrInvalidOption, MaxBatchSize)
	}
	built := make([]jobs.Job, len(items))
	for i, it := range items {
		j, err := s.prepare(it.Type, it.Input, it.Opts)
		if err != nil {
			s.metrics.Submission(it.Type, submissionResult(err))
			return nil, &ItemError{Index: i, Err: err}
		}
		built[i] = j
	}
	err := s.store.CreateBatchIfQueueBelow(ctx, built, s.maxQueued)
	switch {
	case errors.Is(err, store.ErrQueueFull):
		for _, j := range built {
			s.countRejection(j.Type)
		}
		err = ErrQueueFull
	case errors.Is(err, store.ErrAdmissionBusy):
		err = ErrAdmissionBusy
	case err != nil:
		err = fmt.Errorf("store batch: %w", err)
	}
	for _, j := range built {
		s.metrics.Submission(j.Type, submissionResult(err))
	}
	if err != nil {
		return nil, err
	}
	return built, nil
}

func (s *Service) countRejection(jobType string) {
	s.rejMu.Lock()
	s.rejected[jobType]++
	s.rejMu.Unlock()
}

// MaxQueued is the effective global limit on queued jobs.
func (s *Service) MaxQueued() int { return s.maxQueued }

// UsePolicy sets the per-type defaults and maxima from the config file. Call it
// once at startup, before the service handles requests.
func (s *Service) UsePolicy(policies map[string]jobs.TypePolicy) { s.policies = policies }

// EffectiveType is what the server enforces for one job type, with every
// default and limit resolved (the config file's value, or the server-wide one).
type EffectiveType struct {
	DefaultMaxAttempts int `json:"default_max_attempts"`
	MaxMaxAttempts     int `json:"max_max_attempts"`
	DefaultTimeoutSecs int `json:"default_timeout_seconds"`
	MaxTimeoutSecs     int `json:"max_timeout_seconds"`
}

// EffectivePolicy is the whole effective configuration of the submission side.
type EffectivePolicy struct {
	MaxQueued int                      `json:"max_queued"`
	JobTypes  map[string]EffectiveType `json:"job_types"`
}

// Effective resolves the policy of every configured job type, so a developer
// can confirm what the server will do instead of inferring it from the file.
func (s *Service) Effective() EffectivePolicy {
	out := EffectivePolicy{MaxQueued: s.maxQueued, JobTypes: map[string]EffectiveType{}}
	for _, t := range s.ConfiguredTypes() {
		pol := s.policies[t]
		e := EffectiveType{
			DefaultMaxAttempts: jobs.DefaultMaxAttempts, MaxMaxAttempts: jobs.MaxAttemptsLimit,
			DefaultTimeoutSecs: jobs.DefaultTimeoutSeconds, MaxTimeoutSecs: s.maxTimeoutSeconds,
		}
		if pol.DefaultMaxAttempts > 0 {
			e.DefaultMaxAttempts = pol.DefaultMaxAttempts
		}
		if pol.MaxMaxAttempts > 0 && pol.MaxMaxAttempts < e.MaxMaxAttempts {
			e.MaxMaxAttempts = pol.MaxMaxAttempts
		}
		if pol.DefaultTimeoutSecs > 0 {
			e.DefaultTimeoutSecs = pol.DefaultTimeoutSecs
		}
		if pol.MaxTimeoutSecs > 0 && pol.MaxTimeoutSecs < e.MaxTimeoutSecs {
			e.MaxTimeoutSecs = pol.MaxTimeoutSecs
		}
		out.JobTypes[t] = e
	}
	return out
}

// ConfiguredTypes lists the job types this service accepts, sorted.
func (s *Service) ConfiguredTypes() []string {
	types := make([]string, 0, len(s.allowed))
	for t := range s.allowed {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// QueueFullRejections returns when counting started (process start) and a copy
// of the per-type counts of submissions refused because the queue was full.
// See the Service comment for the scope: this process only, in memory.
func (s *Service) QueueFullRejections() (since time.Time, byType map[string]uint64) {
	s.rejMu.Lock()
	defer s.rejMu.Unlock()
	byType = make(map[string]uint64, len(s.rejected))
	for t, n := range s.rejected {
		byType[t] = n
	}
	return s.startedAt, byType
}
