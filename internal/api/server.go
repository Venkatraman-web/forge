// Package api is the HTTP layer. It parses requests, calls the job service or
// store, and writes JSON responses. It contains no job-specific logic.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/Venkatraman-web/forge/internal/auth"
	"github.com/Venkatraman-web/forge/internal/jobs"
	"github.com/Venkatraman-web/forge/internal/store"
	"github.com/Venkatraman-web/forge/internal/worker"
)

const (
	maxBodyBytes = 1 << 20 // 1 MB limit for POST /v1/jobs
	defaultLimit = 20
	maxLimit     = 100

	// queueFullRetryAfterSeconds is the Retry-After hint sent with queue_full.
	// Forge cannot know when capacity will return (that depends on how fast the
	// workers finish jobs), so this is a fixed, conservative pause rather than
	// an estimate. Its job is to stop a submitter from retrying in a tight loop;
	// the submitter is expected to add jitter. Whole seconds, as HTTP requires
	// for the delta-seconds form.
	queueFullRetryAfterSeconds = 5

	// admissionBusyRetryAfterSeconds is the Retry-After hint sent with
	// admission_busy. It is short because the condition is momentary: Forge is
	// limiting how many submissions it processes at once, not out of room.
	admissionBusyRetryAfterSeconds = 1
)

// Server holds what the handlers need: the service to submit jobs and the
// store to read them. Reads go straight to the store; only submission needs
// the service's validate-store-enqueue workflow.
type Server struct {
	store   store.Store
	service *worker.Service
	keys    auth.KeySet
}

// NewServer creates the submission API. keys are the submit keys: every request except
// GET /health must present one of them as a bearer token.
func NewServer(jobStore store.Store, service *worker.Service, keys auth.KeySet) (*Server, error) {
	if keys.Empty() {
		return nil, errors.New("submission api needs at least one key")
	}
	return &Server{store: jobStore, service: service, keys: keys}, nil
}

// Handler returns the router. Routes are registered without methods so that a
// wrong method gets our JSON 405 (and unknown paths our JSON 404) instead of
// ServeMux's plain-text replies.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/jobs", s.handleJobs)
	mux.HandleFunc("/v1/jobs/batch", s.handleBatch)
	mux.HandleFunc("/v1/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "The requested path does not exist.")
	})
	return s.authenticate(mux)
}

// authenticate requires the submit key on everything except /health (which
// carries no job data and is what container health checks call). It runs before
// any handler, so an unauthenticated caller reaches neither the database nor
// the job service, and learns nothing, not even which paths exist.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := s.keys.Match(r.Header.Get("Authorization"))
		if r.URL.Path != "/health" && !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithName(r.Context(), name)))
	})
}

// errorBody is the one error shape used by every failure response.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type submitRequest struct {
	Type    string          `json:"type"`
	Input   json.RawMessage `json:"input"` // kept as raw JSON; the handler validates it
	Options *submitOptions  `json:"options"`
}

// submitOptions are the optional per-job settings. max_attempts is kept as raw
// JSON so that only a plain whole number is accepted: a string such as "3", a
// decimal or an exponent form must not be silently converted.
type submitOptions struct {
	MaxAttempts    json.RawMessage `json:"max_attempts"`
	TimeoutSeconds json.RawMessage `json:"timeout_seconds"`
}

// parseWholeNumber reads an option kept as raw JSON. present is false when the
// option is absent or null (meaning "use the default"). Otherwise the value must
// be a plain run of digits of at most maxDigits: a string such as "3", a
// decimal, an exponent form, a sign, a boolean, an array or an object must not
// be silently converted. Zero is returned as an error by the callers, never
// treated as "unset".
func parseWholeNumber(raw json.RawMessage, maxDigits int) (n int, present bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, nil
	}
	s := string(raw)
	if len(s) > maxDigits || strings.Trim(s, "0123456789") != "" {
		return 0, true, errors.New("not a whole number")
	}
	n, _ = strconv.Atoi(s)
	return n, true, nil
}

// parseMaxAttempts returns 0 when the option is absent, and an error unless the
// value is a whole number from 1 to the limit.
func parseMaxAttempts(raw json.RawMessage) (int, error) {
	n, present, err := parseWholeNumber(raw, 3)
	if err != nil || !present {
		return 0, err
	}
	if n < 1 || n > jobs.MaxAttemptsLimit {
		return 0, errors.New("out of range")
	}
	return n, nil
}

// parseTimeoutSeconds returns 0 when the option is absent, and an error unless
// the value is a whole number of at least 1. The upper bound is the server's
// own maximum, which only the service knows, so it is checked there.
func parseTimeoutSeconds(raw json.RawMessage) (int, error) {
	n, present, err := parseWholeNumber(raw, 4)
	if err != nil || !present {
		// A run of digits too long to be a sane number of seconds is "too big", not
		// "malformed": pass an out-of-range value on so the service answers with the
		// server's real limit instead of a misleading message.
		if s := string(raw); err != nil && len(s) > 4 && strings.Trim(s, "0123456789") == "" {
			return jobs.MaxTimeoutSecondsLimit + 1, nil
		}
		return 0, err
	}
	if n < 1 {
		return 0, errors.New("out of range")
	}
	return n, nil
}

type listResponse struct {
	Jobs []jobs.Job `json:"jobs"`
}

// handleHealth only shows that the HTTP server is answering. It says nothing
// about the workers or the queue.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleJobs serves /v1/jobs: POST submits a job, GET lists jobs.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.submitJob(w, r)
	case http.MethodGet:
		s.listJobs(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
	}
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s.submitBatch(w, r)
}

func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer r.Body.Close()

	var req submitRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeBodyError(w, err)
		return
	}
	// After the object, the body must end. Anything else is trailing data.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("trailing data")
		}
		writeBodyError(w, err)
		return
	}

	opts, bad := checkSubmitFields(req)
	if bad != nil {
		writeError(w, http.StatusBadRequest, bad.code, bad.message)
		return
	}

	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	opts.IdempotencyKey = key

	job, replayed, err := s.service.Submit(r.Context(), req.Type, req.Input, opts)
	if err != nil {
		writeSubmitError(w, err)
		return
	}
	if replayed {
		// The key already held this job: nothing new was stored, so 200 rather
		// than 202, and the header tells a client library it was a repeat.
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, job)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// idempotencyKeyPattern is the accepted shape of an Idempotency-Key: 8 to 128
// characters from letters, digits and _ . : - (the same rule ForgeCart's checkout uses).
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

// idempotencyKey reads the optional Idempotency-Key header. It returns "" when
// the header is absent. A repeated, empty or malformed header is a 400, never
// silently ignored: a client that thinks it is protected must be.
func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) == 0 {
		return "", true
	}
	if len(values) != 1 || !idempotencyKeyPattern.MatchString(values[0]) {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key",
			"Idempotency-Key must appear once and be 8 to 128 characters from A-Z a-z 0-9 _ . : -.")
		return "", false
	}
	return values[0], true
}

// fieldProblem is a request-shape problem found before the service is called.
type fieldProblem struct{ code, message string }

// checkSubmitFields validates the fields of one submission (a single job, or one
// item of a batch) and parses its options. It never looks at the input's content.
func checkSubmitFields(req submitRequest) (jobs.Options, *fieldProblem) {
	if strings.TrimSpace(req.Type) == "" {
		return jobs.Options{}, &fieldProblem{"invalid_request", "The \"type\" field is required."}
	}
	if len(req.Input) == 0 {
		return jobs.Options{}, &fieldProblem{"invalid_request", "The \"input\" field is required."}
	}
	var opts jobs.Options
	if req.Options != nil {
		n, err := parseMaxAttempts(req.Options.MaxAttempts)
		if err != nil {
			return opts, &fieldProblem{"invalid_option",
				fmt.Sprintf("\"options.max_attempts\" must be a whole number from 1 to %d (total execution attempts, including the first).", jobs.MaxAttemptsLimit)}
		}
		opts.MaxAttempts = n
		t, err := parseTimeoutSeconds(req.Options.TimeoutSeconds)
		if err != nil {
			return opts, &fieldProblem{"invalid_option",
				"\"options.timeout_seconds\" must be a whole number of seconds, at least 1 (the execution time limit of one attempt)."}
		}
		opts.TimeoutSeconds = t
	}
	return opts, nil
}

type batchRequest struct {
	Jobs []submitRequest `json:"jobs"`
}

// submitBatch implements POST /v1/jobs/batch: up to worker.MaxBatchSize jobs in
// one request, stored in one transaction (all or nothing). The body limit is the
// same 1 MiB as a single submission.
func (s *Server) submitBatch(w http.ResponseWriter, r *http.Request) {
	if len(r.Header.Values("Idempotency-Key")) > 0 {
		// Not supported yet: refuse rather than ignore, so a client never
		// believes a batch is protected against duplicates when it is not.
		writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key is not supported on batch submission yet.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer r.Body.Close()

	var req batchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeBodyError(w, err)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "The body must be a single JSON object with a \"jobs\" array.")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "The body must be a single JSON object with a \"jobs\" array.")
		return
	}
	if len(req.Jobs) == 0 || len(req.Jobs) > worker.MaxBatchSize {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("\"jobs\" must hold 1 to %d jobs.", worker.MaxBatchSize))
		return
	}
	items := make([]worker.BatchItem, len(req.Jobs))
	for i, jr := range req.Jobs {
		opts, bad := checkSubmitFields(jr)
		if bad != nil {
			writeError(w, http.StatusBadRequest, bad.code, fmt.Sprintf("jobs[%d]: %s", i, bad.message))
			return
		}
		items[i] = worker.BatchItem{Type: jr.Type, Input: jr.Input, Opts: opts}
	}
	created, err := s.service.SubmitBatch(r.Context(), items)
	if err != nil {
		writeSubmitError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		Jobs []jobs.Job `json:"jobs"`
	}{created})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if r.URL.Query().Has("limit") {
		n, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", "The \"limit\" parameter must be a whole number from 1 to 100.")
			return
		}
		limit = n
	}
	list, err := s.store.List(r.Context(), limit)
	if err != nil {
		log.Printf("api: list jobs: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "An internal error occurred.")
		return
	}
	writeJSON(w, http.StatusOK, listResponse{Jobs: list})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	job, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job_not_found", "No job exists with that ID.")
		return
	}
	if err != nil {
		// A real storage failure is not "not found": log the detail, send a safe 500.
		log.Printf("api: get job: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "An internal error occurred.")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// writeBodyError turns a body-reading or decoding failure into a 413 or 400.
func writeBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "The request body exceeds the 1 MB limit.")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request",
		"The body must be a single JSON object with only the \"type\", \"input\" and optional \"options\" fields.")
}

// writeSubmitError maps the service's errors to HTTP responses.
func writeSubmitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, worker.ErrUnsupportedJobType):
		msg := "The requested job type is not registered."
		var ie *worker.ItemError
		if errors.As(err, &ie) {
			msg = fmt.Sprintf("The job type of jobs[%d] is not registered.", ie.Index)
		}
		writeError(w, http.StatusBadRequest, "unsupported_job_type", msg)
	case errors.Is(err, worker.ErrInvalidOption):
		writeError(w, http.StatusBadRequest, "invalid_option", err.Error())
	case errors.Is(err, worker.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_key_conflict", "This Idempotency-Key was already used for a different request.")
	case errors.Is(err, worker.ErrInvalidInput):
		// The service's message is the handler's explanation, which is
		// written not to include record values.
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, worker.ErrQueueFull):
		// Backpressure, not a fault: nothing was stored, and trying again later
		// is expected to work. The stable code lets a submitter tell this apart
		// from other 503s, and Retry-After tells it how long to wait.
		w.Header().Set("Retry-After", strconv.Itoa(queueFullRetryAfterSeconds))
		writeError(w, http.StatusServiceUnavailable, "queue_full", "Forge is at capacity. Please try again later.")
	case errors.Is(err, worker.ErrAdmissionBusy):
		// Backpressure, distinct from queue_full: the queue may have room. Nothing
		// was stored and no database work was started.
		w.Header().Set("Retry-After", strconv.Itoa(admissionBusyRetryAfterSeconds))
		writeError(w, http.StatusServiceUnavailable, "admission_busy", "Forge is busy accepting jobs. Please try again shortly.")
	default:
		log.Printf("api: submit job: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "An internal error occurred.")
	}
}

// requireMethod writes a 405 and returns false if the request method differs.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for this path.")
	return false
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// writeJSON encodes first, so an encoding failure can still become a clean 500
// (headers are not sent until we know the body is good).
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		log.Printf("api: encode response: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(body, '\n')); err != nil {
		log.Printf("api: write response: %v", err)
	}
}
