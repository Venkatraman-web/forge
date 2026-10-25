// Package forgeclient is the Go client for Forge's job-submission API: submit
// a job, read one, list recent jobs. It is the submitting side of Forge; the
// workers that run jobs use sdk/workerruntime and sdk/workerclient.
//
// The client makes exactly one HTTP request per call and never retries by
// itself, because a blind retry of a submission can create a second job
// (unless the request carries an IdempotencyKey) and because Forge's
// backpressure answers carry a wait time the caller should honour. Use IsBackpressure, RetryAfter
// and IsUncertain to decide what to do with an error.
//
// Every call carries the submit key (Config.APIKey, Forge's FORGE_API_KEY) as a
// bearer token. It is a shared secret sent in clear text unless TLS is
// terminated in front of Forge, so keep Forge on a trusted network.
//
//	c, _ := forgeclient.New(forgeclient.Config{BaseURL: "http://127.0.0.1:8080", APIKey: key})
//	job, err := c.Submit(ctx, forgeclient.SubmitRequest{
//		Type:           "invoice.generate",
//		Input:          map[string]string{"order": "o-1"},
//		MaxAttempts:    5,  // optional: total attempts including the first (default 3)
//		TimeoutSeconds: 60, // optional: limit per attempt (default 30, capped by the server)
//	})
//	if forgeclient.IsBackpressure(err) {
//		wait, _ := forgeclient.RetryAfter(err) // wait at least this long, plus jitter
//	}
package forgeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	defaultTimeout = 30 * time.Second

	// Limits mirror what the server accepts, so an impossible request fails
	// before it is sent. The server's own maximum timeout (FORGE_MAX_JOB_TIMEOUT)
	// can be lower; it answers ErrInvalidOption then.
	MaxAttemptsLimit  = 20
	MaxTimeoutSeconds = 3600
	// MaxBatchSize is the most jobs SubmitBatch accepts (the server's limit).
	MaxBatchSize        = 100
	maxListLimit        = 100
	maxMessageRunes     = 200
	maxRetryAfterHeader = time.Hour

	submitBodyLimit = 64 << 10 // a submission answer is one small job object
	batchBodyLimit  = 1 << 20  // up to MaxBatchSize small job objects
	getBodyLimit    = 1 << 20  // one job: a result is at most 256 KiB
	listBodyLimit   = 32 << 20 // up to 100 jobs with results
)

var jobIDPattern = regexp.MustCompile(`^job_[0-9a-f]{1,64}$`)

// idempotencyKeyPattern is the shape the server accepts for an Idempotency-Key.
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

// Config configures a Client.
type Config struct {
	// BaseURL is the address of the submission API (FORGE_ADDR), for example
	// "http://127.0.0.1:8080". A path prefix is kept, so a reverse proxy mounted
	// below a path works. No credentials, query or fragment.
	BaseURL string
	// APIKey is the submit key (FORGE_API_KEY), sent as a bearer token with
	// every request. Required: Forge refuses unauthenticated submission API
	// calls. Printable ASCII without spaces.
	APIKey string
	// HTTPClient is optional. Use it for a custom Transport (TLS settings,
	// proxies) or Timeout. If nil, a client with a 30 s timeout is used. The
	// value is copied, not modified, and its redirect policy is always
	// replaced: redirects are never followed.
	HTTPClient *http.Client
}

// Client calls the Forge submission API. It is safe for concurrent use. Create
// one with New.
type Client struct {
	base *url.URL
	key  string
	hc   *http.Client
}

// New validates the configuration and creates a Client. It makes no request.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("forgeclient: BaseURL is required")
	}
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	switch {
	case err != nil:
		return nil, errors.New("forgeclient: BaseURL is not a valid URL")
	case base.Scheme != "http" && base.Scheme != "https":
		return nil, errors.New("forgeclient: BaseURL must start with http:// or https://")
	case base.Hostname() == "":
		return nil, errors.New("forgeclient: BaseURL has no host")
	case base.User != nil:
		return nil, errors.New("forgeclient: BaseURL must not contain credentials")
	case base.RawQuery != "" || base.Fragment != "":
		return nil, errors.New("forgeclient: BaseURL must not contain a query or fragment")
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, errors.New("forgeclient: APIKey is required")
	}
	for _, r := range key {
		if r <= ' ' || r > '~' {
			return nil, errors.New("forgeclient: APIKey must be printable ASCII without spaces")
		}
	}
	var hc http.Client
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
	} else {
		hc.Timeout = defaultTimeout
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: base, key: key, hc: &hc}, nil
}

// Job status values.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// JobError is the safe, public description of why a job failed.
type JobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Job is Forge's public view of a job. The submitted input is never returned.
type Job struct {
	ID             string          `json:"job_id"`
	Type           string          `json:"type"`
	Status         string          `json:"status"`
	Result         json.RawMessage `json:"result,omitempty"`
	Error          *JobError       `json:"error,omitempty"`
	MaxAttempts    int             `json:"max_attempts"`
	TimeoutSeconds int             `json:"timeout_seconds"`
	CreatedAt      time.Time       `json:"created_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
	// Replayed is true when Submit returned a job stored by an earlier
	// submission with the same IdempotencyKey, so nothing new was created.
	Replayed bool `json:"-"`
}

// SubmitRequest describes one job. Type and Input are required; the other
// fields are optional and zero means "use the server default".
type SubmitRequest struct {
	// Type selects the worker handler, for example "invoice.generate".
	Type string
	// Input is the job's payload. It is encoded with encoding/json (a
	// json.RawMessage is sent as it is) and must not be nil.
	Input any
	// MaxAttempts is the total number of execution attempts including the first
	// (1 means never retried). 0 means the server default (3). Valid: 1 to 20.
	MaxAttempts int
	// TimeoutSeconds limits how long ONE attempt may run before its handler's
	// context is cancelled. 0 means the server default (30 s). Valid: 1 to 3600,
	// and at most the server's FORGE_MAX_JOB_TIMEOUT.
	TimeoutSeconds int
	// IdempotencyKey, when set, makes Submit safe to repeat: if an earlier
	// submission with the same key and the same content was stored, Submit
	// returns that job (Job.Replayed is true) instead of creating another. Use a
	// stable identifier of the business event, for example an order ID. 8 to 128
	// characters from A-Z a-z 0-9 _ . : -. The same key with different content
	// fails with ErrIdempotencyConflict. Not supported by SubmitBatch.
	IdempotencyKey string
}

// Submit sends one job. On success the job is stored as queued. See IsUncertain
// for what an error means.
func (c *Client) Submit(ctx context.Context, req SubmitRequest) (Job, error) {
	const op = "submit"
	switch {
	case strings.TrimSpace(req.Type) == "":
		return Job{}, errBadArg(op, "Type is required")
	case req.Input == nil:
		return Job{}, errBadArg(op, "Input is required")
	case req.MaxAttempts < 0 || req.MaxAttempts > MaxAttemptsLimit:
		return Job{}, errBadArg(op, fmt.Sprintf("MaxAttempts must be 0 (default) or 1 to %d", MaxAttemptsLimit))
	case req.TimeoutSeconds < 0 || req.TimeoutSeconds > MaxTimeoutSeconds:
		return Job{}, errBadArg(op, fmt.Sprintf("TimeoutSeconds must be 0 (default) or 1 to %d", MaxTimeoutSeconds))
	case req.IdempotencyKey != "" && !idempotencyKeyPattern.MatchString(req.IdempotencyKey):
		return Job{}, errBadArg(op, "IdempotencyKey must be 8 to 128 characters from A-Z a-z 0-9 _ . : -")
	}
	input, err := json.Marshal(req.Input)
	if err != nil {
		return Job{}, errBadArg(op, "Input could not be encoded as JSON")
	}
	body := struct {
		Type    string          `json:"type"`
		Input   json.RawMessage `json:"input"`
		Options *submitOptions  `json:"options,omitempty"`
	}{Type: req.Type, Input: input}
	if req.MaxAttempts > 0 || req.TimeoutSeconds > 0 {
		body.Options = &submitOptions{MaxAttempts: req.MaxAttempts, TimeoutSeconds: req.TimeoutSeconds}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Job{}, errBadArg(op, "request could not be encoded")
	}
	status, hdr, raw, err := c.do(ctx, op, http.MethodPost, "v1/jobs", payload, submitBodyLimit, req.IdempotencyKey)
	if err != nil {
		return Job{}, err
	}
	// 202: stored. 200: only for a repeated idempotency key, which returns the
	// job stored the first time.
	replayed := status == http.StatusOK && req.IdempotencyKey != "" && hdr.Get("Idempotent-Replayed") == "true"
	if status != http.StatusAccepted && !replayed {
		return Job{}, failure(op, status, hdr, raw)
	}
	job, err := decodeJob(op, status, raw)
	job.Replayed = replayed
	return job, err
}

// SubmitBatch sends up to MaxBatchSize jobs in one request. The server stores
// them in one transaction with one commit, which is much cheaper per job than
// separate submissions when you have many to send. It is all or nothing: either
// every job is stored (the returned jobs are in request order) or none is.
//
// An invalid item rejects the whole batch (a 400 whose message names the item,
// for example "jobs[3]: ..."); a full queue or busy admission stores nothing
// (see IsBackpressure); an uncertain outcome (see IsUncertain) may have stored
// the whole batch. Each item is validated like Submit's request.
func (c *Client) SubmitBatch(ctx context.Context, reqs []SubmitRequest) ([]Job, error) {
	const op = "submit_batch"
	if len(reqs) == 0 || len(reqs) > MaxBatchSize {
		return nil, errBadArg(op, fmt.Sprintf("a batch holds 1 to %d jobs", MaxBatchSize))
	}
	type item struct {
		Type    string          `json:"type"`
		Input   json.RawMessage `json:"input"`
		Options *submitOptions  `json:"options,omitempty"`
	}
	items := make([]item, len(reqs))
	for i, req := range reqs {
		switch {
		case strings.TrimSpace(req.Type) == "":
			return nil, errBadArg(op, fmt.Sprintf("jobs[%d]: Type is required", i))
		case req.Input == nil:
			return nil, errBadArg(op, fmt.Sprintf("jobs[%d]: Input is required", i))
		case req.MaxAttempts < 0 || req.MaxAttempts > MaxAttemptsLimit:
			return nil, errBadArg(op, fmt.Sprintf("jobs[%d]: MaxAttempts must be 0 (default) or 1 to %d", i, MaxAttemptsLimit))
		case req.TimeoutSeconds < 0 || req.TimeoutSeconds > MaxTimeoutSeconds:
			return nil, errBadArg(op, fmt.Sprintf("jobs[%d]: TimeoutSeconds must be 0 (default) or 1 to %d", i, MaxTimeoutSeconds))
		}
		input, err := json.Marshal(req.Input)
		if err != nil {
			return nil, errBadArg(op, fmt.Sprintf("jobs[%d]: Input could not be encoded as JSON", i))
		}
		items[i] = item{Type: req.Type, Input: input}
		if req.MaxAttempts > 0 || req.TimeoutSeconds > 0 {
			items[i].Options = &submitOptions{MaxAttempts: req.MaxAttempts, TimeoutSeconds: req.TimeoutSeconds}
		}
	}
	payload, err := json.Marshal(struct {
		Jobs []item `json:"jobs"`
	}{items})
	if err != nil {
		return nil, errBadArg(op, "request could not be encoded")
	}
	status, hdr, raw, err := c.do(ctx, op, http.MethodPost, "v1/jobs/batch", payload, batchBodyLimit, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusAccepted {
		return nil, failure(op, status, hdr, raw)
	}
	var out struct {
		Jobs []Job `json:"jobs"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Jobs) != len(reqs) {
		return nil, &Error{Status: status, Op: op, Message: "body is not the documented batch of jobs"}
	}
	for _, j := range out.Jobs {
		if !jobIDPattern.MatchString(j.ID) {
			return nil, &Error{Status: status, Op: op, Message: "body is not the documented batch of jobs"}
		}
	}
	return out.Jobs, nil
}

type submitOptions struct {
	MaxAttempts    int `json:"max_attempts,omitempty"`
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Get returns one job. ErrNotFound means no job has that ID.
func (c *Client) Get(ctx context.Context, id string) (Job, error) {
	const op = "get"
	if !jobIDPattern.MatchString(id) {
		return Job{}, errBadArg(op, "id is not a job ID (job_ followed by hexadecimal)")
	}
	status, hdr, raw, err := c.do(ctx, op, http.MethodGet, "v1/jobs/"+url.PathEscape(id), nil, getBodyLimit, "")
	if err != nil {
		return Job{}, err
	}
	if status != http.StatusOK {
		return Job{}, failure(op, status, hdr, raw)
	}
	return decodeJob(op, status, raw)
}

// List returns up to limit recent jobs, newest first. limit is 1 to 100; 0
// means the server default (20).
func (c *Client) List(ctx context.Context, limit int) ([]Job, error) {
	const op = "list"
	if limit < 0 || limit > maxListLimit {
		return nil, errBadArg(op, fmt.Sprintf("limit must be 0 (default) or 1 to %d", maxListLimit))
	}
	path := "v1/jobs"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	status, hdr, raw, err := c.do(ctx, op, http.MethodGet, path, nil, listBodyLimit, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, failure(op, status, hdr, raw)
	}
	var out struct {
		Jobs []Job `json:"jobs"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Jobs == nil {
		return nil, &Error{Status: status, Op: op, Message: "body is not the documented job list"}
	}
	for _, j := range out.Jobs {
		if !jobIDPattern.MatchString(j.ID) {
			return nil, &Error{Status: status, Op: op, Message: "body is not the documented job list"}
		}
	}
	return out.Jobs, nil
}

// --- shared plumbing -------------------------------------------------------

func errBadArg(op, msg string) error { return fmt.Errorf("forgeclient: %s: %s", op, msg) }

// decodeJob parses a success body and checks it is a Forge job.
func decodeJob(op string, status int, raw []byte) (Job, error) {
	var j Job
	if json.Unmarshal(raw, &j) != nil || !jobIDPattern.MatchString(j.ID) {
		return Job{}, &Error{Status: status, Op: op, Message: "body is not the documented job object"}
	}
	return j, nil
}

// do sends one request and returns the status and the bounded response body.
// It never retries.
func (c *Client) do(ctx context.Context, op, method, path string, body []byte, limit int64, idempotencyKey string) (int, http.Header, []byte, error) {
	rel, err := url.Parse(path)
	if err != nil {
		return 0, nil, nil, errBadArg(op, "request could not be built")
	}
	u := c.base.JoinPath(rel.Path)
	u.RawQuery = rel.RawQuery
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return 0, nil, nil, errBadArg(op, "request could not be built")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, nil, &TransportError{Op: op, Err: sanitize(err)}
	}
	defer resp.Body.Close()
	// Read one byte past the limit to tell "exactly at" from "too large".
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, nil, &TransportError{Op: op, Err: sanitize(err)}
	}
	if int64(len(raw)) > limit {
		return 0, nil, nil, &Error{Status: resp.StatusCode, Op: op, Message: "response body is larger than the SDK accepts"}
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// sanitize removes the request URL from a *url.Error while keeping the cause
// for errors.Is.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// failure turns any response that is not the expected success into an *Error.
func failure(op string, status int, hdr http.Header, raw []byte) error {
	e := &Error{Status: status, Op: op}
	switch {
	case status >= 300 && status < 400:
		e.Message = "the server redirected the request; redirects are not followed"
	case status < 400 || status >= 600:
		e.Message = "unexpected HTTP status"
	default:
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &body) // a body that does not parse leaves Code and Message empty
		e.Code, e.Message = clean(body.Error.Code, 64), clean(body.Error.Message, maxMessageRunes)
		e.RetryAfter = parseRetryAfter(hdr.Get("Retry-After"))
	}
	return e
}

// parseRetryAfter reads delta-seconds or an HTTP date; an invalid or
// non-positive value gives 0. The result is capped at one hour.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		if n > int64(maxRetryAfterHeader/time.Second) {
			return maxRetryAfterHeader
		}
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
		if d <= 0 {
			return 0
		}
	} else {
		return 0
	}
	return min(d, maxRetryAfterHeader)
}

// clean drops control characters and shortens untrusted server text.
func clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		if n++; n > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
