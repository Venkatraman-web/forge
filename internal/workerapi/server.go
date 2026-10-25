// Package workerapi is the HTTP interface that customer-run workers use to get
// jobs from Forge. It is separate from internal/api (the submission and status
// API) because it has a different audience, a different listener, different
// timeouts (a claim may wait up to 25 s) and mandatory authentication.
//
// The five worker operations are claim (Task 25.2.2), lease renewal (25.2.3),
// completion (25.2.4), failure reporting (25.2.5) and cooperative release
// (25.2.6). A job whose worker simply stops renewing is recovered by the reaper
// when its lease expires.
//
// Authentication here is a single shared development key. It is a transitional
// mechanism, not tenant or service authentication.
package workerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Venkatraman-web/forge/internal/auth"
	"github.com/Venkatraman-web/forge/internal/jobs"
	"github.com/Venkatraman-web/forge/internal/metrics"
	"github.com/Venkatraman-web/forge/internal/store"
	"github.com/Venkatraman-web/forge/internal/worker"
)

const (
	// MaxWait is the longest a claim request may wait for work.
	MaxWait = 25 * time.Second

	// HTTP server timeouts for the worker listener. WriteTimeout covers the
	// whole handler, so it must exceed MaxWait with room for the response.
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 15 * time.Second
	WriteTimeout      = MaxWait + 15*time.Second
	IdleTimeout       = 60 * time.Second

	maxBodyBytes  = 64 << 10 // claim and renew requests are tiny
	maxClaimTypes = 8

	// A completion carries the job's result, so its body may be larger. The
	// result itself is limited separately and more tightly, so the identifiers
	// around it can never push a valid result over the body limit.
	maxCompleteBodyBytes = 1 << 20
	// maxCompleteBatchBodyBytes bounds a whole batch of completions; one result
	// may still be up to maxResultBytes, so a batch holds at least 16 of them.
	maxCompleteBatchBodyBytes = 4 << 20
	maxCompleteBatch          = store.MaxClaimBatch
	maxResultBytes            = 256 << 10

	// unauthorizedLogEvery limits how often rejected requests are logged.
	unauthorizedLogEvery = 30 * time.Second
)

var (
	workerIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)
	// jobIDPattern is the shape jobs.NewID produces.
	jobIDPattern = regexp.MustCompile(`^job_[0-9a-f]{16}$`)
)

// Config is everything the worker API needs.
type Config struct {
	Store store.Store
	// AllowedTypes is the development allowlist (FORGE_REMOTE_JOB_TYPES). A
	// claim may only ask for types on it.
	AllowedTypes []string
	// Keys are the worker keys (see auth.KeySet); at least one is required.
	Keys auth.KeySet

	// Lease timing, as validated at startup. The claim sets the lease, and the
	// response tells the worker how often to renew and when to give up.
	Lease     time.Duration
	Heartbeat time.Duration
	Safety    time.Duration
	// Deadline is the FALLBACK execution deadline reported to workers when a
	// claimed job carries no timeout of its own (normally every job does, see
	// jobs.Job.TimeoutSeconds). It is advisory:
	// the worker enforces it.
	Deadline time.Duration

	// Metrics is optional; nil records nothing. An ownership_lost answer cannot
	// be attributed to a job type (the request names only a job ID), so it is
	// counted under the type "other".
	Metrics *metrics.Metrics

	// RetryBase and RetryMax are the retry backoff bounds (server-wide
	// settings). The worker API turns a reported failure into a
	// delay with worker.RetryDelay; the remote worker never picks one.
	RetryBase time.Duration
	RetryMax  time.Duration

	// PollInterval is how often a waiting claim asks the database again.
	PollInterval time.Duration
	// MaxPolls bounds how many claim requests may wait at the same time.
	MaxPolls int
}

// Server serves the worker API.
type Server struct {
	cfg     Config
	allowed map[string]bool
	keys    auth.KeySet
	polls   chan struct{} // semaphore: one token per waiting claim request

	// stop is closed when shutdown begins: waiting claims then answer "no work"
	// at once and no new database claim starts.
	stop     chan struct{}
	stopOnce sync.Once

	logMu          sync.Mutex
	lastRejectLog  time.Time
	rejectedUnseen int
}

// NewServer validates the configuration. The key is hashed immediately so the
// plain text is not kept.
func NewServer(cfg Config) (*Server, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("worker api needs a store")
	case len(cfg.AllowedTypes) == 0:
		return nil, errors.New("worker api needs at least one allowed job type")
	case cfg.Lease <= 0 || cfg.Heartbeat <= 0 || cfg.Safety <= 0 || cfg.Deadline <= 0:
		return nil, errors.New("worker api needs positive lease, heartbeat, safety margin and deadline")
	case cfg.PollInterval <= 0:
		return nil, errors.New("worker api needs a positive poll interval")
	case worker.ValidateRetryDelays(cfg.RetryBase, cfg.RetryMax) != nil:
		return nil, fmt.Errorf("worker api retry settings: %w", worker.ValidateRetryDelays(cfg.RetryBase, cfg.RetryMax))
	case cfg.MaxPolls < 1:
		return nil, errors.New("worker api needs a poll limit of at least 1")
	case cfg.Keys.Empty():
		return nil, errors.New("worker api needs at least one key")
	}
	allowed := make(map[string]bool, len(cfg.AllowedTypes))
	for _, t := range cfg.AllowedTypes {
		allowed[t] = true
	}
	return &Server{
		cfg:     cfg,
		allowed: allowed,
		keys:    cfg.Keys,
		polls:   make(chan struct{}, cfg.MaxPolls),
		stop:    make(chan struct{}),
	}, nil
}

// BeginShutdown makes waiting claim requests return "no work" immediately and
// stops new claims from starting. Call it before http.Server.Shutdown, which
// would otherwise wait for the long polls to end by themselves.
func (s *Server) BeginShutdown() {
	s.stopOnce.Do(func() { close(s.stop) })
}

// Handler returns the router. Authentication wraps every path, so an
// unauthenticated caller learns nothing, not even which routes exist.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/worker/claim", s.handleClaim)
	mux.HandleFunc("/v1/worker/jobs/{id}/renew", s.handleRenew)
	mux.HandleFunc("/v1/worker/jobs/{id}/complete", s.handleComplete)
	mux.HandleFunc("/v1/worker/complete", s.handleCompleteBatch)
	mux.HandleFunc("/v1/worker/jobs/{id}/fail", s.handleFail)
	mux.HandleFunc("/v1/worker/jobs/{id}/release", s.handleRelease)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "The requested path does not exist.")
	})
	return s.authenticate(mux)
}

// authenticate rejects the request unless it carries the shared key. It runs
// before any handler, so nothing touches the database for an unauthenticated
// caller. Missing header, wrong scheme, empty token and wrong key all produce
// the same response. The two keys are compared as SHA-256 digests with a
// constant-time comparison, so neither length nor content leaks through timing.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := s.keys.Match(r.Header.Get("Authorization"))
		if !ok {
			s.noteRejected()
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithName(r.Context(), name)))
	})
}

// noteRejected counts a rejected request and logs a summary at most every
// unauthorizedLogEvery. Nothing from the request is logged.
func (s *Server) noteRejected() {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.rejectedUnseen++
	if time.Since(s.lastRejectLog) >= unauthorizedLogEvery {
		log.Printf("workerapi: rejected %d unauthenticated request(s) (further rejections are summarised at most every %s)", s.rejectedUnseen, unauthorizedLogEvery)
		s.lastRejectLog = time.Now()
		s.rejectedUnseen = 0
	}
}

type claimRequest struct {
	WorkerID    string   `json:"worker_id"`
	Types       []string `json:"types"`
	WaitSeconds int      `json:"wait_seconds"`
	// MaxJobs asks for up to this many jobs in one claim (1 to store.MaxClaimBatch)
	// and switches the answer to {"jobs": [...]}. Absent means one job, in the
	// original single-object answer, so older workers keep working.
	MaxJobs *int `json:"max_jobs"`
}

// claimBatchResponse is the answer to a claim that set max_jobs.
type claimBatchResponse struct {
	Jobs []claimResponse `json:"jobs"`
}

type claimResponse struct {
	JobID                    string          `json:"job_id"`
	Type                     string          `json:"type"`
	Input                    json.RawMessage `json:"input"`
	Attempt                  int             `json:"attempt"`
	MaxAttempts              int             `json:"max_attempts"`
	ClaimGeneration          int64           `json:"claim_generation"`
	WorkerID                 string          `json:"worker_id"`
	LeaseSeconds             float64         `json:"lease_seconds"`
	HeartbeatIntervalSeconds float64         `json:"heartbeat_interval_seconds"`
	SafetyMarginSeconds      float64         `json:"safety_margin_seconds"`
	DeadlineSeconds          float64         `json:"deadline_seconds"`
}

// handleClaim implements POST /v1/worker/claim: a bounded long poll that ends
// with a job the database has just assigned to the caller, or 204.
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
		return
	}
	req, ok := s.decodeClaim(w, r)
	if !ok {
		return
	}

	// Bound the number of claim requests that can wait at once.
	select {
	case s.polls <- struct{}{}:
		defer func() { <-s.polls }()
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "too_many_polls", "Too many claim requests are waiting. Try again shortly.")
		return
	}

	deadline := time.Now().Add(time.Duration(req.WaitSeconds) * time.Second)
	for {
		// Never start a claim once the client is gone or shutdown has begun:
		// a claim whose answer nobody receives costs the job an attempt.
		select {
		case <-r.Context().Done():
			return // the client left; there is nobody to answer
		case <-s.stop:
			w.WriteHeader(http.StatusNoContent)
			return
		default:
		}

		// The claim is the real atomic PostgreSQL claim (store.Claim): the
		// database picks the job, assigns it to this worker, bumps attempt and
		// claim_generation, and sets the lease. The statement runs on a context
		// detached from the request, because
		// cancelling it midway would leave the outcome unknowable. Claim adds
		// its own time limit.
		var batch []store.ClaimedJob
		var err error
		if req.MaxJobs == nil {
			var c store.ClaimedJob
			var found bool
			c, found, err = s.cfg.Store.Claim(context.WithoutCancel(r.Context()), req.WorkerID, s.cfg.Lease, req.Types)
			if found {
				batch = []store.ClaimedJob{c}
			}
		} else {
			batch, err = s.cfg.Store.ClaimBatch(context.WithoutCancel(r.Context()), req.WorkerID, s.cfg.Lease, req.Types, *req.MaxJobs)
		}
		if err != nil {
			// Never reported as an empty queue.
			log.Printf("workerapi: claim failed: %s", worker.DescribeDBError(err))
			writeError(w, http.StatusServiceUnavailable, "unavailable", "The service is temporarily unable to hand out work.")
			return
		}
		if len(batch) > 0 {
			for _, c := range batch {
				s.cfg.Metrics.JobEvent(c.Job.Type, metrics.EventClaimed)
				s.cfg.Metrics.QueueWait(c.Job.Type, c.QueueWait)
			}
			if req.MaxJobs == nil {
				s.respondClaimed(w, batch[0], req.WorkerID)
			} else {
				s.respondClaimedBatch(w, batch, req.WorkerID)
			}
			return
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		wait := s.cfg.PollInterval
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-s.stop:
			timer.Stop()
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
}

// deadlineFor is the execution deadline of this claim: the job's own timeout,
// chosen at submission and bounded by the server's maximum. The configured
// Deadline is only the fallback for a row that somehow carries none (the column
// is NOT NULL with a default, so this should not happen).
func (s *Server) deadlineFor(c store.ClaimedJob) time.Duration {
	if t := c.Job.TimeoutSeconds; t > 0 {
		return time.Duration(t) * time.Second
	}
	return s.cfg.Deadline
}

// respondClaimed sends the claimed job. If the write fails, the job is already
// running under a worker that never learned of it; nothing renews its lease, so
// the reaper recovers it and the attempt stays consumed (documented in the
// design). The log line says so without claiming delivery.
// claimWire builds the answer for one claimed job.
func (s *Server) claimWire(c store.ClaimedJob, workerID string) claimResponse {
	return claimResponse{
		JobID:                    c.Job.ID,
		Type:                     c.Job.Type,
		Input:                    c.Job.Input,
		Attempt:                  c.Attempt,
		MaxAttempts:              c.MaxAttempts,
		ClaimGeneration:          c.Generation,
		WorkerID:                 workerID,
		LeaseSeconds:             s.cfg.Lease.Seconds(),
		HeartbeatIntervalSeconds: s.cfg.Heartbeat.Seconds(),
		SafetyMarginSeconds:      s.cfg.Safety.Seconds(),
		DeadlineSeconds:          s.deadlineFor(c).Seconds(),
	}
}

func (s *Server) respondClaimed(w http.ResponseWriter, c store.ClaimedJob, workerID string) {
	s.writeClaimed(w, s.claimWire(c, workerID), []store.ClaimedJob{c}, workerID)
}

// respondClaimedBatch answers a claim that set max_jobs.
func (s *Server) respondClaimedBatch(w http.ResponseWriter, cs []store.ClaimedJob, workerID string) {
	resp := claimBatchResponse{Jobs: make([]claimResponse, len(cs))}
	for i, c := range cs {
		resp.Jobs[i] = s.claimWire(c, workerID)
	}
	s.writeClaimed(w, resp, cs, workerID)
}

// writeClaimed sends a claim answer. The jobs are already claimed in the
// database: if the answer cannot be encoded or delivered, nobody renews them and
// the lease expires, after which the reaper recovers them.
func (s *Server) writeClaimed(w http.ResponseWriter, resp any, cs []store.ClaimedJob, workerID string) {
	body, err := json.Marshal(resp)
	if err != nil {
		log.Printf("workerapi: %d job(s) claimed by worker %s but the response could not be encoded; the leases will expire and the reaper will recover them", len(cs), workerID)
		writeError(w, http.StatusInternalServerError, "internal_error", "An internal error occurred.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(append(body, '\n')); err != nil {
		log.Printf("workerapi: %d job(s) claimed by worker %s but the response was not delivered; the leases will expire and the reaper will recover them", len(cs), workerID)
		return
	}
	if len(cs) == 1 {
		c := cs[0]
		log.Printf("workerapi: job %s (%s) claimed by worker %s: generation %d, attempt %d of %d", c.Job.ID, c.Job.Type, workerID, c.Generation, c.Attempt, c.MaxAttempts)
		return
	}
	log.Printf("workerapi: %d jobs claimed by worker %s in one request (first %s)", len(cs), workerID, cs[0].Job.ID)
}

// decodeClaim reads and validates the request. On failure it has written the
// response and returns ok=false; nothing has been claimed.
func (s *Server) decodeClaim(w http.ResponseWriter, r *http.Request) (claimRequest, bool) {
	var req claimRequest
	if !decodeBody(w, r, &req, maxBodyBytes, `"worker_id", "types", "wait_seconds" and optional "max_jobs"`) {
		return req, false
	}

	switch {
	case !workerIDPattern.MatchString(req.WorkerID):
		writeError(w, http.StatusBadRequest, "invalid_request", "\"worker_id\" must be 8-64 characters from A-Z a-z 0-9 . _ -.")
	case len(req.Types) < 1 || len(req.Types) > maxClaimTypes:
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("\"types\" must list 1 to %d job types.", maxClaimTypes))
	case req.MaxJobs != nil && (*req.MaxJobs < 1 || *req.MaxJobs > store.MaxClaimBatch):
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("\"max_jobs\" must be a whole number from 1 to %d.", store.MaxClaimBatch))
	case req.WaitSeconds < 0 || req.WaitSeconds > int(MaxWait/time.Second):
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("\"wait_seconds\" must be a whole number from 0 to %d.", int(MaxWait/time.Second)))
	default:
		seen := make(map[string]bool, len(req.Types))
		unique := req.Types[:0:0]
		for _, t := range req.Types {
			if !s.allowed[t] {
				writeError(w, http.StatusBadRequest, "invalid_request", "\"types\" contains a job type that is not enabled for remote execution.")
				return req, false
			}
			if !seen[t] {
				seen[t] = true
				unique = append(unique, t)
			}
		}
		req.Types = unique
		return req, true
	}
	return req, false
}

type renewRequest struct {
	WorkerID        string `json:"worker_id"`
	ClaimGeneration int64  `json:"claim_generation"`
}

type renewResponse struct {
	LeaseSeconds float64 `json:"lease_seconds"`
}

// handleRenew implements POST /v1/worker/jobs/{id}/renew. It extends the lease
// of the claim the caller proves it holds, by the server's configured lease
// duration (the client cannot choose one: unknown fields are rejected).
//
// The ownership check is the store's RenewLease: one UPDATE conditioned on the
// job ID, status 'running', the claim generation and the worker ID. There is
// deliberately no lease-deadline condition: an
// overdue lease only makes the job eligible for the reaper, and ownership ends
// when the reaper COMMITS a new generation. PostgreSQL's row lock orders a
// renewal and a reaper pass on the same row, so exactly one of them wins:
// either the renewal extends the lease first (and the reaper then skips the
// job), or the reaper commits first (and the renewal matches nothing).
//
// Outcomes: 200 renewed; 409 the database says this claim is not current
// (stale generation, other worker, job finished, requeued or unknown, all
// answered identically); 503 the database could not confirm either way.
func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
		return
	}
	jobID := r.PathValue("id")
	if !jobIDPattern.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The job ID in the path is not valid.")
		return
	}
	var req renewRequest
	if !decodeBody(w, r, &req, maxBodyBytes, `"worker_id" and "claim_generation"`) {
		return
	}
	claim, ok := ownedClaim(w, jobID, req.WorkerID, req.ClaimGeneration)
	if !ok {
		return
	}
	err := s.cfg.Store.RenewLease(r.Context(), claim, s.cfg.Lease)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, renewResponse{LeaseSeconds: s.cfg.Lease.Seconds()})
	case errors.Is(err, store.ErrLostOwnership):
		s.cfg.Metrics.JobEvent("", metrics.EventOwnershipLost)
		writeError(w, http.StatusConflict, "ownership_lost", "This claim is no longer current.")
	default:
		// A database problem is not proof that ownership is gone, so it is
		// never a 409; the worker keeps trying until its safety margin ends.
		log.Printf("workerapi: renew failed: %s", worker.DescribeDBError(err))
		writeError(w, http.StatusServiceUnavailable, "unavailable", "The service could not confirm the renewal. Try again.")
	}
}

// ownedClaim validates the identifiers that prove a claim and builds the
// store's claim value from them. Only the job ID, generation and worker ID
// matter to the fenced writes. On failure it has written the response.
func ownedClaim(w http.ResponseWriter, jobID, workerID string, generation int64) (store.ClaimedJob, bool) {
	switch {
	case !workerIDPattern.MatchString(workerID):
		writeError(w, http.StatusBadRequest, "invalid_request", "\"worker_id\" must be 8-64 characters from A-Z a-z 0-9 . _ -.")
		return store.ClaimedJob{}, false
	case generation < 1:
		writeError(w, http.StatusBadRequest, "invalid_request", "\"claim_generation\" must be a whole number of at least 1.")
		return store.ClaimedJob{}, false
	}
	return store.ClaimedJob{Job: jobs.Job{ID: jobID}, Generation: generation, WorkerID: workerID}, true
}

type completeRequest struct {
	WorkerID        string          `json:"worker_id"`
	ClaimGeneration int64           `json:"claim_generation"`
	Result          json.RawMessage `json:"result"`
}

type completeResponse struct {
	Outcome string `json:"outcome"`
}

// handleComplete implements POST /v1/worker/jobs/{id}/complete: the worker
// reports success and an optional result.
//
// The durable step is store.Complete, a fenced UPDATE: it matches only a job that is still running under exactly this
// generation and worker, and then sets status succeeded, stores the result,
// stamps finished_at, and clears the error, lease and worker. There is no
// lease-deadline condition and no replay handling: after a successful
// completion the job is no longer running, so repeating the request gets 409,
// the same as a genuinely stale claim (the store cannot tell them apart).
//
// 200 is sent only after the store confirmed the write. A 503 means the outcome
// is not confirmed: the job may or may not have been completed.
//
// The result becomes publicly readable through GET /v1/jobs/{id}, so workers
// must not put secrets in it.
func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
		return
	}
	jobID := r.PathValue("id")
	if !jobIDPattern.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The job ID in the path is not valid.")
		return
	}
	var req completeRequest
	if !decodeBody(w, r, &req, maxCompleteBodyBytes, `"worker_id", "claim_generation" and "result"`) {
		return
	}
	claim, ok := ownedClaim(w, jobID, req.WorkerID, req.ClaimGeneration)
	if !ok {
		return
	}
	result, ok := checkResult(w, req.Result)
	if !ok {
		return
	}

	done, err := s.cfg.Store.Complete(r.Context(), claim, result)
	switch {
	case err == nil:
		s.cfg.Metrics.JobEvent(done.Type, metrics.EventSucceeded)
		s.cfg.Metrics.AttemptDuration(done.Type, metrics.EventSucceeded, done.AttemptDuration)
		log.Printf("workerapi: job %s generation %d: completed by worker %s (key %s)", jobID, req.ClaimGeneration, req.WorkerID, auth.NameFrom(r.Context()))
		writeJSON(w, http.StatusOK, completeResponse{Outcome: "succeeded"})
	case errors.Is(err, store.ErrLostOwnership):
		s.cfg.Metrics.JobEvent("", metrics.EventOwnershipLost)
		writeError(w, http.StatusConflict, "ownership_lost", "This claim is no longer current.")
	default:
		log.Printf("workerapi: complete failed: %s", worker.DescribeDBError(err))
		writeError(w, http.StatusServiceUnavailable, "unavailable", "The service could not confirm the completion. Try again.")
	}
}

type completeBatchRequest struct {
	WorkerID string `json:"worker_id"`
	Jobs     []struct {
		JobID           string          `json:"job_id"`
		ClaimGeneration int64           `json:"claim_generation"`
		Result          json.RawMessage `json:"result"`
	} `json:"jobs"`
}

type completeBatchResponse struct {
	Results []completeBatchResult `json:"results"`
}

type completeBatchResult struct {
	JobID   string `json:"job_id"`
	Outcome string `json:"outcome"` // "succeeded" or "ownership_lost"
}

// handleCompleteBatch implements POST /v1/worker/complete: handleComplete for
// 1 to 20 jobs of one worker, written in one statement and one database commit
// (store.CompleteBatch), which is what makes completing many jobs cheap.
//
// Every job is fenced on its own. A job whose claim is no longer current is
// reported "ownership_lost" in its own entry and does not affect the others, so
// a 200 means "each entry says what happened to that job", not "all succeeded".
// 503 means nothing in the batch is confirmed. A job ID may appear only once.
func (s *Server) handleCompleteBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
		return
	}
	var req completeBatchRequest
	if !decodeBody(w, r, &req, maxCompleteBatchBodyBytes, `"worker_id" and "jobs"`) {
		return
	}
	if !workerIDPattern.MatchString(req.WorkerID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "\"worker_id\" must be 8-64 characters from A-Z a-z 0-9 . _ -.")
		return
	}
	if n := len(req.Jobs); n < 1 || n > maxCompleteBatch {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("\"jobs\" must list 1 to %d jobs.", maxCompleteBatch))
		return
	}
	items := make([]store.CompleteItem, len(req.Jobs))
	seen := make(map[string]bool, len(req.Jobs))
	for i, j := range req.Jobs {
		claim, ok := ownedClaim(w, j.JobID, req.WorkerID, j.ClaimGeneration)
		if !ok {
			return
		}
		if !jobIDPattern.MatchString(j.JobID) || seen[j.JobID] {
			writeError(w, http.StatusBadRequest, "invalid_request", "Each job ID must be valid and appear only once.")
			return
		}
		seen[j.JobID] = true
		result, ok := checkResult(w, j.Result)
		if !ok {
			return
		}
		items[i] = store.CompleteItem{Claim: claim, Result: result}
	}

	outcomes, err := s.cfg.Store.CompleteBatch(r.Context(), items)
	if err != nil {
		log.Printf("workerapi: complete batch failed: %s", worker.DescribeDBError(err))
		writeError(w, http.StatusServiceUnavailable, "unavailable", "The service could not confirm the completions. Try again.")
		return
	}
	resp := completeBatchResponse{Results: make([]completeBatchResult, len(items))}
	for i, o := range outcomes {
		resp.Results[i].JobID = items[i].Claim.Job.ID
		if o.Done {
			resp.Results[i].Outcome = "succeeded"
			s.cfg.Metrics.JobEvent(o.Finished.Type, metrics.EventSucceeded)
			s.cfg.Metrics.AttemptDuration(o.Finished.Type, metrics.EventSucceeded, o.Finished.AttemptDuration)
		} else {
			resp.Results[i].Outcome = "ownership_lost"
			s.cfg.Metrics.JobEvent("", metrics.EventOwnershipLost)
		}
	}
	log.Printf("workerapi: batch of %d completions by worker %s", len(items), req.WorkerID)
	writeJSON(w, http.StatusOK, resp)
}

type releaseRequest struct {
	WorkerID        string `json:"worker_id"`
	ClaimGeneration int64  `json:"claim_generation"`
}

type releaseResponse struct {
	Outcome string `json:"outcome"`
}

// handleRelease implements POST /v1/worker/jobs/{id}/release: a worker that is
// shutting down hands a job back. The durable step is store.Release, a fenced
// UPDATE: for a job still
// running under exactly this generation and worker it sets status queued,
// claimable at once, refunds the interrupted attempt (attempt - 1, never below
// 0), clears worker and lease, and leaves claim_generation alone, so the next
// claim gets a higher one and this claim can never become valid again.
//
// CONTRACT FOR WORKERS: call release only after the handler has returned and
// renewals have stopped. The server cannot check that. Releasing while the
// handler still runs lets a second worker execute the job at the same time.
// Because the attempt is refunded, a worker that releases every job it claims
// can make no progress; v0 does not limit that.
//
// Only 200 {"outcome":"released"} confirms a release. 409 is a confirmed
// ownership miss, and it is also what a repeat gets after a release whose
// response was lost (the job is no longer running; the store cannot tell the
// cases apart). 503 means unconfirmed: the job may or may not be released.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
		return
	}
	jobID := r.PathValue("id")
	if !jobIDPattern.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The job ID in the path is not valid.")
		return
	}
	var req releaseRequest
	if !decodeBody(w, r, &req, maxBodyBytes, `"worker_id" and "claim_generation"`) {
		return
	}
	claim, ok := ownedClaim(w, jobID, req.WorkerID, req.ClaimGeneration)
	if !ok {
		return
	}

	jobType, err := s.cfg.Store.Release(r.Context(), claim)
	switch {
	case err == nil:
		s.cfg.Metrics.JobEvent(jobType, metrics.EventReleased)
		log.Printf("workerapi: job %s generation %d: released by worker %s (key %s); it can be claimed again", jobID, req.ClaimGeneration, req.WorkerID, auth.NameFrom(r.Context()))
		writeJSON(w, http.StatusOK, releaseResponse{Outcome: "released"})
	case errors.Is(err, store.ErrLostOwnership):
		s.cfg.Metrics.JobEvent("", metrics.EventOwnershipLost)
		writeError(w, http.StatusConflict, "ownership_lost", "This claim is no longer current.")
	default:
		log.Printf("workerapi: release failed: %s", worker.DescribeDBError(err))
		writeError(w, http.StatusServiceUnavailable, "unavailable", "The service could not confirm the release. Try again.")
	}
}

// maxAttempts is the schema's upper bound for max_attempts (jobs_attempts_valid).
const maxAttempts = 20

// failRequest is deliberately all enumerations and numbers: no free text from
// the worker is accepted, so nothing untrusted can be stored or logged.
// Retryable is a pointer so that a missing field is rejected instead of
// silently meaning "permanent failure".
type failRequest struct {
	WorkerID        string `json:"worker_id"`
	ClaimGeneration int64  `json:"claim_generation"`
	Attempt         int    `json:"attempt"`
	Retryable       *bool  `json:"retryable"`
	Kind            string `json:"kind"`
}

type failResponse struct {
	Outcome        string   `json:"outcome"`
	RetryInSeconds *float64 `json:"retry_in_seconds,omitempty"`
}

// handleFail implements POST /v1/worker/jobs/{id}/fail: the worker reports
// that this attempt failed. The worker classifies (retryable or not, and a
// kind from a fixed list); Forge owns everything else.
//
// kind selects a fixed public error (worker.FailureInfo); the backoff delay is
// computed by the server from the configured bounds (worker.RetryDelay); and
// store.FailAttempt, one fenced UPDATE, decides from the PERSISTED attempt and
// max_attempts whether the job is requeued or fails for good (with
// attempts_exhausted when a retryable failure has no attempts left). The
// response reports what the database did, not what the worker expected.
//
// attempt comes from the claim response and is used only to size the backoff:
// a worker that misreports it changes the delay of its own job and nothing else,
// because the retry decision never uses it.
//
// As elsewhere, 409 is a confirmed ownership miss and 503 means the outcome is
// not confirmed: the transition may have committed even though no 200 arrived,
// and a repeat then gets 409 (the store cannot tell it from a stale claim).
func (s *Server) handleFail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
		return
	}
	jobID := r.PathValue("id")
	if !jobIDPattern.MatchString(jobID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The job ID in the path is not valid.")
		return
	}
	var req failRequest
	if !decodeBody(w, r, &req, maxBodyBytes, `"worker_id", "claim_generation", "attempt", "retryable" and "kind"`) {
		return
	}
	claim, ok := ownedClaim(w, jobID, req.WorkerID, req.ClaimGeneration)
	if !ok {
		return
	}
	info, kindOK := worker.FailureInfo(req.Kind)
	switch {
	case req.Attempt < 1 || req.Attempt > maxAttempts:
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("\"attempt\" must be a whole number from 1 to %d.", maxAttempts))
		return
	case req.Retryable == nil:
		writeError(w, http.StatusBadRequest, "invalid_request", "\"retryable\" is required and must be true or false.")
		return
	case !kindOK:
		writeError(w, http.StatusBadRequest, "invalid_request", "\"kind\" must be \"error\", \"timeout\" or \"panic\".")
		return
	}

	delay := worker.RetryDelay(req.Attempt, s.cfg.RetryBase, s.cfg.RetryMax)
	willRetry, done, err := s.cfg.Store.FailAttempt(r.Context(), claim, info, *req.Retryable, delay)
	if err == nil {
		switch req.Kind {
		case "timeout":
			s.cfg.Metrics.JobEvent(done.Type, metrics.EventAttemptTimeout)
		case "panic":
			s.cfg.Metrics.JobEvent(done.Type, metrics.EventAttemptPanic)
		}
		outcome := metrics.EventFailed
		if willRetry {
			outcome = metrics.EventRetried
		}
		s.cfg.Metrics.JobEvent(done.Type, outcome)
		s.cfg.Metrics.AttemptDuration(done.Type, outcome, done.AttemptDuration)
	}
	switch {
	case err == nil && willRetry:
		log.Printf("workerapi: job %s generation %d: failure reported by worker %s (key %s, kind=%s, retryable=true); queued again, next try in about %s",
			jobID, req.ClaimGeneration, req.WorkerID, auth.NameFrom(r.Context()), req.Kind, delay.Round(time.Millisecond))
		secs := delay.Round(time.Millisecond).Seconds() // millisecond precision is plenty for a client
		writeJSON(w, http.StatusOK, failResponse{Outcome: "requeued", RetryInSeconds: &secs})
	case err == nil:
		log.Printf("workerapi: job %s generation %d: failure reported by worker %s (key %s, kind=%s, retryable=%t); job is now failed",
			jobID, req.ClaimGeneration, req.WorkerID, auth.NameFrom(r.Context()), req.Kind, *req.Retryable)
		writeJSON(w, http.StatusOK, failResponse{Outcome: "failed"})
	case errors.Is(err, store.ErrLostOwnership):
		s.cfg.Metrics.JobEvent("", metrics.EventOwnershipLost)
		writeError(w, http.StatusConflict, "ownership_lost", "This claim is no longer current.")
	default:
		log.Printf("workerapi: fail report failed: %s", worker.DescribeDBError(err))
		writeError(w, http.StatusServiceUnavailable, "unavailable", "The service could not confirm the failure report. Try again.")
	}
}

// checkResult validates the optional result and returns what to store. A
// missing field or JSON null means "no result" (stored as SQL NULL). Anything else must be valid JSON of
// at most maxResultBytes (decoding the body already proved it is valid JSON),
// and no string or key may contain U+0000, which PostgreSQL's jsonb cannot
// store: that would surface as a database error and be mistaken for an outage,
// so it is rejected here as a client error instead.
func checkResult(w http.ResponseWriter, raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, true
	}
	if len(trimmed) > maxResultBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", fmt.Sprintf("\"result\" must be at most %d KiB.", maxResultBytes>>10))
		return nil, false
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil || hasNUL(v) {
		writeError(w, http.StatusBadRequest, "invalid_request", "\"result\" must be valid JSON without U+0000 characters.")
		return nil, false
	}
	return trimmed, true
}

// hasNUL reports whether any string or object key in v contains U+0000.
func hasNUL(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.ContainsRune(x, 0)
	case []any:
		for _, e := range x {
			if hasNUL(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if strings.ContainsRune(k, 0) || hasNUL(e) {
				return true
			}
		}
	}
	return false
}

// decodeBody reads one JSON object into dst: size-limited, unknown fields
// rejected, nothing may follow the object. On failure it has written the
// response and returns false.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any, limit int64, fields string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeBodyError(w, err, fields)
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("trailing data")
		}
		writeBodyError(w, err, fields)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "An internal error occurred.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(body, '\n')); err != nil {
		log.Printf("workerapi: write response: %v", err)
	}
}

func writeBodyError(w http.ResponseWriter, err error, fields string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "The request body is too large.")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request",
		"The body must be a single JSON object with only the "+fields+" fields.")
}

// errorBody is the project's one error shape, the same as internal/api.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	out, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(out, '\n')); err != nil {
		log.Printf("workerapi: write response: %v", err)
	}
}
