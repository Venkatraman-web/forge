// Package operatorapi is Forge's read-only operator interface. Today it has one
// endpoint, GET /v1/queues, which reports queue saturation and job processing
// per job type. It is deliberately separate from internal/api (the submission
// and status API, which has no authentication) and from internal/workerapi (the
// worker protocol, whose key belongs to customer-run workers): it has its own
// listener and its own bearer key, and Forge refuses to start it without one.
//
// Authentication here is a single shared static key. It separates the operator
// role from the worker role, but it is not user, service or tenant
// authentication, has no rotation or audit trail, and does not replace them.
//
// The endpoint never changes a job: it runs one SELECT.
package operatorapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Venkatraman-web/forge/internal/auth"
	"github.com/Venkatraman-web/forge/internal/metrics"
	"github.com/Venkatraman-web/forge/internal/store"
	"github.com/Venkatraman-web/forge/internal/worker"
)

const (
	// MinKeyLength is the shortest accepted operator key.
	MinKeyLength = auth.MinKeyLength

	// HTTP server timeouts for the operator listener. A status request is one
	// small query, so these are short.
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 10 * time.Second
	WriteTimeout      = 15 * time.Second
	IdleTimeout       = 60 * time.Second

	// maxInFlight bounds concurrent status queries. The query reads every queued
	// and running row, so an unbounded poller could load the database that
	// submissions and workers share; excess requests get 429.
	maxInFlight = 4

	// unauthorizedLogEvery limits how often rejected requests are logged.
	unauthorizedLogEvery = 30 * time.Second
)

// StatsSource is the one thing the endpoint needs from storage.
type StatsSource interface {
	QueueStats(ctx context.Context) (store.QueueStats, error)
}

// Admission is what the endpoint needs to know about the submission side:
// the effective limit, the configured job types, and this process's
// queue-full rejection counts. *worker.Service implements it.
type Admission interface {
	MaxQueued() int
	ConfiguredTypes() []string
	QueueFullRejections() (since time.Time, byType map[string]uint64)
	Effective() worker.EffectivePolicy
}

// Config is everything the operator API needs.
type Config struct {
	Stats     StatsSource
	Admission Admission
	// Keys are the operator keys (see auth.KeySet); at least one is required.
	Keys auth.KeySet
	// Metrics, if set, is served at GET /metrics (Prometheus text format) with the
	// same key and the same concurrency limit as /v1/queues.
	Metrics *metrics.Metrics
}

// Server serves the operator API.
type Server struct {
	cfg      Config
	keys     auth.KeySet
	inFlight chan struct{}

	logMu          sync.Mutex
	lastRejectLog  time.Time
	rejectedUnseen int
}

// NewServer validates the configuration.
func NewServer(cfg Config) (*Server, error) {
	switch {
	case cfg.Stats == nil:
		return nil, errors.New("operator api needs a stats source")
	case cfg.Admission == nil:
		return nil, errors.New("operator api needs the submission service")
	}
	if cfg.Keys.Empty() {
		return nil, errors.New("operator api needs at least one key")
	}
	return &Server{cfg: cfg, keys: cfg.Keys, inFlight: make(chan struct{}, maxInFlight)}, nil
}

// Handler returns the router. Authentication wraps every path, so an
// unauthenticated caller learns nothing, not even which routes exist.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/queues", s.handleQueues)
	mux.HandleFunc("/v1/config", s.handleConfig)
	if s.cfg.Metrics != nil {
		mux.HandleFunc("/metrics", s.handleMetrics)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "The requested path does not exist.")
	})
	return s.authenticate(mux)
}

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
		log.Printf("operatorapi: rejected %d unauthenticated request(s) (further rejections are summarised at most every %s)", s.rejectedUnseen, unauthorizedLogEvery)
		s.lastRejectLog, s.rejectedUnseen = time.Now(), 0
	}
}

// ---------- response ----------

// The JSON field names below are the endpoint's stable contract.

type queuesResponse struct {
	// CollectedAt is the database clock when the counts were read.
	CollectedAt time.Time `json:"collected_at"`
	// Limits holds the effective admission limit.
	Limits limitsJSON `json:"limits"`
	// RejectionCounters says what queue_full_rejections_since_process_start means.
	RejectionCounters rejectionScope `json:"rejection_counters"`
	Queues            []queueJSON    `json:"queues"`
	Totals            totalsJSON     `json:"totals"`
}

type limitsJSON struct {
	// MaxQueued is FORGE_MAX_QUEUED: the most jobs with status 'queued' that
	// Forge will hold before refusing submissions (503 queue_full).
	MaxQueued int `json:"max_queued"`
	// Scope is "global": one limit shared by all job types. There is no
	// per-type limit yet, so a queue's max_pending is always null.
	Scope string `json:"scope"`
}

type rejectionScope struct {
	Scope          string    `json:"scope"` // "process"
	Since          time.Time `json:"since"` // when this Forge process started counting
	ResetOnRestart bool      `json:"reset_on_restart"`
	Aggregated     bool      `json:"aggregated_across_processes"`
}

type queueJSON struct {
	Type string `json:"type"`
	// Queued is the number of jobs with status 'queued': what the admission
	// limit counts, including jobs waiting out a retry backoff or scheduled.
	Queued int `json:"queued"`
	// Running is the number of jobs with status 'running'.
	Running int `json:"running"`
	// OldestDueJobAgeSeconds is how long the oldest queued job that is already
	// due has been due (now minus its next_run_at). null when no queued job of
	// this type is due. A job waiting out a retry backoff is not due and is not
	// measured until its time arrives.
	OldestDueJobAgeSeconds *float64 `json:"oldest_due_job_age_seconds"`
	// MaxPending is the per-type admission limit; null because none exists yet.
	MaxPending *int `json:"max_pending"`
	// QueueFullRejections counts submissions of this type refused with
	// queue_full by THIS Forge process since it started.
	QueueFullRejections uint64 `json:"queue_full_rejections_since_process_start"`
}

type totalsJSON struct {
	Queued  int `json:"queued"`
	Running int `json:"running"`
	// OldestDueJobAgeSeconds is the largest per-type value, null if none is due.
	OldestDueJobAgeSeconds *float64 `json:"oldest_due_job_age_seconds"`
	QueueFullRejections    uint64   `json:"queue_full_rejections_since_process_start"`
}

func (s *Server) handleQueues(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
		return
	}
	select {
	case s.inFlight <- struct{}{}:
		defer func() { <-s.inFlight }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "too_many_requests", "Too many queue status requests are in progress. Try again shortly.")
		return
	}

	stats, err := s.cfg.Stats.QueueStats(r.Context())
	if err != nil {
		// A storage failure is reported as unavailable, never as an empty queue.
		log.Printf("operatorapi: queue stats: %s", worker.DescribeDBError(err))
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Queue status is temporarily unavailable.")
		return
	}
	since, rejected := s.cfg.Admission.QueueFullRejections()
	writeJSON(w, http.StatusOK, buildResponse(stats, s.cfg.Admission.MaxQueued(), s.cfg.Admission.ConfiguredTypes(), since, rejected))
}

// handleConfig returns the effective policy per job type. It reads no
// database (the policy is fixed at startup), so it needs no in-flight cap.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.Admission.Effective())
}

// handleMetrics serves the Prometheus exposition. It reads the database once per
// scrape (the queue gauges), so it shares the in-flight cap with /v1/queues: a
// misconfigured scraper cannot load the database that submissions and workers
// share.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
		return
	}
	select {
	case s.inFlight <- struct{}{}:
		defer func() { <-s.inFlight }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "too_many_requests", "Too many metrics requests are in progress. Try again shortly.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.cfg.Metrics.Handler().ServeHTTP(w, r)
}

// buildResponse merges the database snapshot with the configuration. Every
// configured type is listed, with zeros when it has no jobs (known zeros: the
// snapshot is authoritative). A type that has jobs but is no longer configured
// is listed too, so nothing is hidden.
func buildResponse(stats store.QueueStats, maxQueued int, configured []string, since time.Time, rejected map[string]uint64) queuesResponse {
	byType := map[string]*queueJSON{}
	get := func(t string) *queueJSON {
		if q, ok := byType[t]; ok {
			return q
		}
		q := &queueJSON{Type: t}
		byType[t] = q
		return q
	}
	for _, t := range configured {
		get(t)
	}
	for _, t := range stats.Types {
		q := get(t.Type)
		q.Queued, q.Running = t.Queued, t.Running
		if t.OldestDueAge != nil {
			v := roundSeconds(t.OldestDueAge.Seconds())
			q.OldestDueJobAgeSeconds = &v
		}
	}
	for t, n := range rejected {
		get(t).QueueFullRejections = n
	}
	resp := queuesResponse{
		CollectedAt:       stats.CollectedAt,
		Limits:            limitsJSON{MaxQueued: maxQueued, Scope: "global"},
		RejectionCounters: rejectionScope{Scope: "process", Since: since.UTC(), ResetOnRestart: true, Aggregated: false},
		Queues:            []queueJSON{},
	}
	names := make([]string, 0, len(byType))
	for t := range byType {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		q := byType[t]
		resp.Queues = append(resp.Queues, *q)
		resp.Totals.Queued += q.Queued
		resp.Totals.Running += q.Running
		resp.Totals.QueueFullRejections += q.QueueFullRejections
		if q.OldestDueJobAgeSeconds != nil && (resp.Totals.OldestDueJobAgeSeconds == nil || *q.OldestDueJobAgeSeconds > *resp.Totals.OldestDueJobAgeSeconds) {
			v := *q.OldestDueJobAgeSeconds
			resp.Totals.OldestDueJobAgeSeconds = &v
		}
	}
	return resp
}

func roundSeconds(v float64) float64 { return math.Round(v*1000) / 1000 }

// ---------- JSON helpers (the same error shape as the other APIs) ----------

type errorBody struct {
	Error errorDetail `json:"error"`
}
type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		log.Printf("operatorapi: encode response: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Operational data: never cache it in a shared proxy.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(append(body, '\n')); err != nil {
		log.Printf("operatorapi: write response: %v", err)
	}
}
