// Package workerclient is a typed Go client for the Forge remote-worker API.
//
// It wraps the five worker operations (Claim, Renew, Complete, Fail, Release)
// and nothing else. It does not run handlers, send heartbeats, retry, or
// manage shutdown: those decisions belong to the worker runtime that uses it.
// Each method makes exactly one HTTP request.
//
// # Ambiguous outcomes
//
// A request can succeed on the server and still fail for the caller (the
// response is lost, or a 503 is returned after the database committed). The
// client never retries, and it cannot tell whether the operation was applied;
// see IsUncertain and ErrOwnershipLost. Handlers must therefore be idempotent,
// because Forge delivers jobs at least once.
//
// # Security
//
// The server authenticates with one shared development key sent as
// "Authorization: Bearer <key>". That is not production authentication: the
// key has no scope, rotation or revocation, and the current server speaks plain
// HTTP, so use https:// through a TLS-terminating proxy anywhere but a local
// machine. Using this SDK does not make the server production-safe.
package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	// MaxWait is the longest wait a claim may request (the server's limit).
	MaxWait = 25 * time.Second
	// MaxClaimTypes is the most job types one claim may list.
	MaxClaimTypes = 8
	// MaxResultBytes is the largest result Complete will send (the server's limit).
	MaxResultBytes = 256 << 10
	// maxAttempts is the largest attempt number the server accepts.
	maxAttempts = 20

	defaultTimeout   = MaxWait + 20*time.Second // a full long poll plus margin
	maxResponseBytes = 2 << 20                  // a claim carries the job input (up to 1 MiB)
	maxErrorBytes    = 64 << 10
	maxMessageRunes  = 200
)

var workerIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// Config configures a Client.
type Config struct {
	// BaseURL is the address of the worker API listener
	// (FORGE_WORKER_ADDR), for example "http://127.0.0.1:8081". A path prefix
	// is kept, so a reverse proxy mounted below a path works. No credentials,
	// query or fragment.
	BaseURL string
	// APIKey is the shared development key (FORGE_WORKER_API_KEY).
	APIKey string
	// HTTPClient is optional. Use it for a custom Transport (TLS settings,
	// proxies) or Timeout. If nil, a client with a 45 s timeout is used. The
	// value is copied, not modified, and its redirect policy is always
	// replaced: redirects are never followed, so the Authorization header
	// cannot be forwarded to another host.
	HTTPClient *http.Client
}

// Client calls the Forge worker API. It is safe for concurrent use. Create one
// with New.
type Client struct {
	base *url.URL
	key  string
	hc   *http.Client
}

// New validates the configuration and creates a Client. It makes no request.
func New(cfg Config) (*Client, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, errors.New("workerclient: APIKey is required")
	}
	for _, r := range key {
		if r > unicode.MaxASCII || unicode.IsControl(r) || unicode.IsSpace(r) {
			return nil, errors.New("workerclient: APIKey must be printable ASCII without spaces")
		}
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("workerclient: BaseURL is required")
	}
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	switch {
	case err != nil:
		// The parse error can quote the input, which must not hold a secret anyway.
		return nil, errors.New("workerclient: BaseURL is not a valid URL")
	case base.Scheme != "http" && base.Scheme != "https":
		return nil, errors.New("workerclient: BaseURL must start with http:// or https://")
	case base.Hostname() == "":
		return nil, errors.New("workerclient: BaseURL has no host")
	case base.User != nil:
		return nil, errors.New("workerclient: BaseURL must not contain credentials; use APIKey")
	case base.RawQuery != "" || base.Fragment != "":
		return nil, errors.New("workerclient: BaseURL must not contain a query or fragment")
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

// ClaimedJob is a job the database has just assigned to this worker, with
// everything needed to work on it and to report back. Pass it unchanged to
// Renew, Complete, Fail and Release: it carries the fencing token
// (ClaimGeneration) and WorkerID that prove ownership, and the Attempt used to
// size the retry delay.
type ClaimedJob struct {
	JobID           string
	Type            string
	Input           json.RawMessage // the job's input, as submitted
	Attempt         int             // 1 for the first execution
	MaxAttempts     int
	ClaimGeneration int64
	WorkerID        string

	// Timing, from the server's configuration. Fractions of a second are kept.
	// Renew at least every HeartbeatInterval. If no renewal has succeeded for
	// SafetyMargin (measured from when the last successful request was SENT),
	// stop the handler: the lease is about to expire. Lease is how far ahead a
	// claim or renewal moves the lease deadline. Deadline is advisory: the
	// worker enforces the execution time limit, the server does not.
	Lease             time.Duration
	HeartbeatInterval time.Duration
	SafetyMargin      time.Duration
	Deadline          time.Duration
}

// ClaimRequest asks for work.
type ClaimRequest struct {
	// WorkerID identifies this worker process or slot: 8-64 characters from
	// A-Z a-z 0-9 . _ - . Uniqueness is not enforced, because the generation
	// is what fences claims.
	WorkerID string
	// Types lists 1 to MaxClaimTypes job types this worker can run. Each must
	// be enabled on the server (FORGE_REMOTE_JOB_TYPES).
	Types []string
	// Wait is how long the server may hold the request open waiting for a job,
	// from 0 to MaxWait. Sub-second parts are dropped (the server counts whole
	// seconds). Zero asks for an immediate answer.
	Wait time.Duration
}

// MaxClaimBatch is the most jobs ClaimBatch may ask for (the server's limit).
const MaxClaimBatch = 20

// ClaimBatchRequest is ClaimRequest for several jobs.
type ClaimBatchRequest struct {
	WorkerID string
	Types    []string
	Wait     time.Duration
	// Max is the most jobs to take, 1 to MaxClaimBatch. The server may return
	// fewer (whatever is eligible, and a cap on their total input size).
	Max int
}

// ClaimBatch takes up to Max jobs in one request. Every returned job is claimed
// under req.WorkerID with its own claim generation and must be renewed and
// resolved individually, exactly like a job from Claim. It returns an empty
// slice when no job became available within Wait. As with Claim, a failure
// after the server may have committed the claims is recovered by the lease.
func (c *Client) ClaimBatch(ctx context.Context, req ClaimBatchRequest) ([]ClaimedJob, error) {
	const op = "claim_batch"
	if !workerIDPattern.MatchString(req.WorkerID) {
		return nil, errBadArg(op, "WorkerID must be 8-64 characters from A-Z a-z 0-9 . _ -")
	}
	if n := len(req.Types); n < 1 || n > MaxClaimTypes {
		return nil, errBadArg(op, fmt.Sprintf("Types must list 1 to %d job types", MaxClaimTypes))
	}
	for _, t := range req.Types {
		if t == "" {
			return nil, errBadArg(op, "Types must not contain an empty name")
		}
	}
	if req.Wait < 0 || req.Wait > MaxWait {
		return nil, errBadArg(op, fmt.Sprintf("Wait must be between 0 and %s", MaxWait))
	}
	if req.Max < 1 || req.Max > MaxClaimBatch {
		return nil, errBadArg(op, fmt.Sprintf("Max must be from 1 to %d", MaxClaimBatch))
	}
	body := struct {
		WorkerID    string   `json:"worker_id"`
		Types       []string `json:"types"`
		WaitSeconds int      `json:"wait_seconds"`
		MaxJobs     int      `json:"max_jobs"`
	}{req.WorkerID, req.Types, int(req.Wait / time.Second), req.Max}

	status, hdr, raw, err := c.do(ctx, op, "v1/worker/claim", body, maxResponseBytes)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var wire struct {
			Jobs []json.RawMessage `json:"jobs"`
		}
		if json.Unmarshal(raw, &wire) != nil || len(wire.Jobs) == 0 || len(wire.Jobs) > req.Max {
			return nil, &responseError{op, "body is not the documented batch of claims"}
		}
		out := make([]ClaimedJob, 0, len(wire.Jobs))
		for _, j := range wire.Jobs {
			cj, err := decodeClaim(op, j, req.WorkerID)
			if err != nil {
				return nil, err
			}
			out = append(out, cj)
		}
		return out, nil
	}
	return nil, c.failure(op, status, hdr, raw)
}

// Claim asks the server for a job. found is false, with a nil error, when no
// eligible job appeared within the wait (HTTP 204): an idle queue is normal.
//
// When found is true the job is already running under this worker in the
// database, so it must be worked on, completed, failed or released. If the
// response is lost (a transport error, or ErrMalformedResponse after a 200), a
// job may have been claimed without the caller learning of it; nothing renews
// its lease, so the server's reaper will recover it after the lease expires and
// the attempt stays used.
func (c *Client) Claim(ctx context.Context, req ClaimRequest) (job ClaimedJob, found bool, err error) {
	const op = "claim"
	if !workerIDPattern.MatchString(req.WorkerID) {
		return job, false, errBadArg(op, "WorkerID must be 8-64 characters from A-Z a-z 0-9 . _ -")
	}
	if n := len(req.Types); n < 1 || n > MaxClaimTypes {
		return job, false, errBadArg(op, fmt.Sprintf("Types must list 1 to %d job types", MaxClaimTypes))
	}
	for _, t := range req.Types {
		if t == "" {
			return job, false, errBadArg(op, "Types must not contain an empty name")
		}
	}
	if req.Wait < 0 || req.Wait > MaxWait {
		return job, false, errBadArg(op, fmt.Sprintf("Wait must be between 0 and %s", MaxWait))
	}

	body := struct {
		WorkerID    string   `json:"worker_id"`
		Types       []string `json:"types"`
		WaitSeconds int      `json:"wait_seconds"`
	}{req.WorkerID, req.Types, int(req.Wait / time.Second)}

	status, hdr, raw, err := c.do(ctx, op, "v1/worker/claim", body, maxResponseBytes)
	if err != nil {
		return job, false, err
	}
	switch status {
	case http.StatusNoContent:
		return job, false, nil
	case http.StatusOK:
		job, err = decodeClaim(op, raw, req.WorkerID)
		return job, err == nil, err
	}
	return job, false, c.failure(op, status, hdr, raw)
}

type claimWire struct {
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

func decodeClaim(op string, raw []byte, wantWorker string) (ClaimedJob, error) {
	var w claimWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return ClaimedJob{}, &responseError{op, "body is not the documented claim object"}
	}
	switch {
	case w.JobID == "" || w.Type == "" || len(w.Input) == 0:
		return ClaimedJob{}, &responseError{op, "claim is missing job_id, type or input"}
	case w.WorkerID != wantWorker:
		return ClaimedJob{}, &responseError{op, "claim names a different worker_id"}
	case w.ClaimGeneration < 1 || w.Attempt < 1 || w.MaxAttempts < w.Attempt:
		return ClaimedJob{}, &responseError{op, "claim has an invalid generation or attempt"}
	}
	lease, ok1 := seconds(w.LeaseSeconds)
	hb, ok2 := seconds(w.HeartbeatIntervalSeconds)
	safety, ok3 := seconds(w.SafetyMarginSeconds)
	deadline, ok4 := seconds(w.DeadlineSeconds)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return ClaimedJob{}, &responseError{op, "claim has an invalid timing value"}
	}
	return ClaimedJob{
		JobID: w.JobID, Type: w.Type, Input: w.Input, Attempt: w.Attempt, MaxAttempts: w.MaxAttempts,
		ClaimGeneration: w.ClaimGeneration, WorkerID: w.WorkerID,
		Lease: lease, HeartbeatInterval: hb, SafetyMargin: safety, Deadline: deadline,
	}, nil
}

// seconds converts a positive, finite number of seconds to a Duration without
// dropping fractions.
func seconds(s float64) (time.Duration, bool) {
	if math.IsNaN(s) || math.IsInf(s, 0) || s <= 0 || s > 24*3600 {
		return 0, false
	}
	return time.Duration(math.Round(s * float64(time.Second))), true
}

// Renew extends the lease of the claim by the server's configured lease
// duration (the client cannot choose one) and returns the lease length the
// server applied. ErrOwnershipLost means stop the handler and make no further
// calls for this claim. IsUncertain errors do not prove ownership is lost: the
// caller keeps trying until its safety margin runs out.
func (c *Client) Renew(ctx context.Context, job ClaimedJob) (lease time.Duration, err error) {
	const op = "renew"
	if err := checkClaim(op, job); err != nil {
		return 0, err
	}
	status, hdr, raw, err := c.do(ctx, op, jobPath(job, "renew"), ownership(job), maxErrorBytes)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, c.failure(op, status, hdr, raw)
	}
	var out struct {
		LeaseSeconds float64 `json:"lease_seconds"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return 0, &responseError{op, "body is not the documented renew object"}
	}
	d, ok := seconds(out.LeaseSeconds)
	if !ok {
		return 0, &responseError{op, "renew has an invalid lease_seconds"}
	}
	return d, nil
}

// Complete reports success. result is the job's result as ALREADY ENCODED JSON
// (any JSON value: object, array, string, number, boolean). nil or empty sends
// no result; a JSON null is treated by the server as no result too. It must be
// valid JSON of at most MaxResultBytes, and it must not contain U+0000 (the
// server rejects that). The result becomes readable through the public job
// status API, so it must not hold secrets.
//
// A nil error means the server confirmed the completion. ErrOwnershipLost
// means it was not applied, or that an earlier request already applied it; see
// ErrOwnershipLost. On an IsUncertain error the job may or may not be
// completed.
func (c *Client) Complete(ctx context.Context, job ClaimedJob, result json.RawMessage) error {
	const op = "complete"
	if err := checkClaim(op, job); err != nil {
		return err
	}
	if len(result) > 0 {
		if len(result) > MaxResultBytes {
			return errBadArg(op, fmt.Sprintf("result is larger than %d bytes", MaxResultBytes))
		}
		if !json.Valid(result) {
			return errBadArg(op, "result is not valid JSON")
		}
	}
	body := struct {
		WorkerID        string          `json:"worker_id"`
		ClaimGeneration int64           `json:"claim_generation"`
		Result          json.RawMessage `json:"result,omitempty"`
	}{job.WorkerID, job.ClaimGeneration, result}
	status, hdr, raw, err := c.do(ctx, op, jobPath(job, "complete"), body, maxErrorBytes)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return c.failure(op, status, hdr, raw)
	}
	return expectOutcome(op, raw, "succeeded")
}

// CompleteItem is one job of a CompleteBatch: a claimed job and its result,
// with the rules of Complete's result argument.
type CompleteItem struct {
	Job    ClaimedJob
	Result json.RawMessage
}

// CompleteBatch reports success for 1 to MaxClaimBatch jobs of ONE worker in a
// single request, which the server writes in a single database commit. It is
// Complete for many jobs: the cost of a request and a commit is shared.
//
// The returned slice has one entry per item, in order: nil when the server
// confirmed that completion, or an error matching ErrOwnershipLost when that
// job's claim was no longer current. Items are independent: one stale claim
// does not stop the others. The second return value is non-nil when the whole
// call failed (a transport error, or a 5xx); then the slice is nil and
// IsUncertain tells whether the batch may have been applied, exactly as for
// Complete. A job may appear only once, and every item must carry the same
// WorkerID. The total request is limited to about 4 MiB.
func (c *Client) CompleteBatch(ctx context.Context, items []CompleteItem) ([]error, error) {
	const op = "complete"
	if len(items) < 1 || len(items) > MaxClaimBatch {
		return nil, errBadArg(op, fmt.Sprintf("items must list 1 to %d jobs", MaxClaimBatch))
	}
	type wireItem struct {
		JobID           string          `json:"job_id"`
		ClaimGeneration int64           `json:"claim_generation"`
		Result          json.RawMessage `json:"result,omitempty"`
	}
	wire := make([]wireItem, len(items))
	seen := make(map[string]bool, len(items))
	for i, it := range items {
		if err := checkClaim(op, it.Job); err != nil {
			return nil, err
		}
		if it.Job.WorkerID != items[0].Job.WorkerID || seen[it.Job.JobID] {
			return nil, errBadArg(op, "all items must share one WorkerID and list each job once")
		}
		seen[it.Job.JobID] = true
		if len(it.Result) > MaxResultBytes {
			return nil, errBadArg(op, fmt.Sprintf("result is larger than %d bytes", MaxResultBytes))
		}
		if len(it.Result) > 0 && !json.Valid(it.Result) {
			return nil, errBadArg(op, "result is not valid JSON")
		}
		wire[i] = wireItem{it.Job.JobID, it.Job.ClaimGeneration, it.Result}
	}
	body := struct {
		WorkerID string     `json:"worker_id"`
		Jobs     []wireItem `json:"jobs"`
	}{items[0].Job.WorkerID, wire}
	status, hdr, raw, err := c.do(ctx, op, "v1/worker/complete", body, 64<<10)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.failure(op, status, hdr, raw)
	}
	var out struct {
		Results []struct {
			JobID   string `json:"job_id"`
			Outcome string `json:"outcome"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Results) != len(items) {
		return nil, &responseError{op, "body is not the documented complete batch object"}
	}
	errs := make([]error, len(items))
	for i, r := range out.Results {
		switch {
		case r.JobID != items[i].Job.JobID:
			return nil, &responseError{op, "body is not the documented complete batch object"}
		case r.Outcome == "succeeded":
		case r.Outcome == "ownership_lost":
			errs[i] = &Error{Status: http.StatusConflict, Code: "ownership_lost", Op: op}
		default:
			return nil, &responseError{op, "body is not the documented complete batch object"}
		}
	}
	return errs, nil
}

// FailureKind classifies why an attempt failed. The server maps each kind to a
// fixed public error; no free text is accepted.
type FailureKind string

const (
	// KindError: the handler returned an error.
	KindError FailureKind = "error"
	// KindTimeout: the handler exceeded the execution time limit.
	KindTimeout FailureKind = "timeout"
	// KindPanic: the handler panicked (and the worker recovered).
	KindPanic FailureKind = "panic"
)

// Failure describes a failed attempt.
type Failure struct {
	Kind FailureKind
	// Permanent marks a failure that retrying cannot fix. The zero value
	// (false) is a retryable failure, the safe default: Forge retries it
	// while attempts remain.
	Permanent bool
}

// FailResult is what the server did with a failure report.
type FailResult struct {
	// Requeued is true when the job was put back in the queue for another
	// attempt; false when it is now failed for good (a permanent failure, or
	// no attempts left).
	Requeued bool
	// RetryIn is how long until the job can be claimed again (Requeued only).
	// The server computes it from its own backoff settings; the SDK never
	// calculates or sends a delay.
	RetryIn time.Duration
}

// Fail reports that the attempt failed. The attempt number sent comes from job,
// so callers do not supply one. (The server uses it only to size the backoff
// and does not check it against the stored attempt; a caller that edits
// job.Attempt can shorten its own job's retry delay. Hardening that is
// server-side work.) Whether the job is retried is decided by the server from
// its stored state, not by the SDK.
//
// Uncertain outcomes are as for Complete: after an IsUncertain error the
// failure may or may not have been recorded, and a repeat that gets
// ErrOwnershipLost may be a repeat of a report that already succeeded.
func (c *Client) Fail(ctx context.Context, job ClaimedJob, f Failure) (FailResult, error) {
	const op = "fail"
	if err := checkClaim(op, job); err != nil {
		return FailResult{}, err
	}
	switch f.Kind {
	case KindError, KindTimeout, KindPanic:
	default:
		return FailResult{}, errBadArg(op, `Kind must be KindError, KindTimeout or KindPanic`)
	}
	if job.Attempt < 1 || job.Attempt > maxAttempts {
		return FailResult{}, errBadArg(op, fmt.Sprintf("job.Attempt must be 1 to %d (use the job returned by Claim)", maxAttempts))
	}
	body := struct {
		WorkerID        string `json:"worker_id"`
		ClaimGeneration int64  `json:"claim_generation"`
		Attempt         int    `json:"attempt"`
		Retryable       bool   `json:"retryable"`
		Kind            string `json:"kind"`
	}{job.WorkerID, job.ClaimGeneration, job.Attempt, !f.Permanent, string(f.Kind)}
	status, hdr, raw, err := c.do(ctx, op, jobPath(job, "fail"), body, maxErrorBytes)
	if err != nil {
		return FailResult{}, err
	}
	if status != http.StatusOK {
		return FailResult{}, c.failure(op, status, hdr, raw)
	}
	var out struct {
		Outcome        string   `json:"outcome"`
		RetryInSeconds *float64 `json:"retry_in_seconds"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return FailResult{}, &responseError{op, "body is not the documented fail object"}
	}
	switch out.Outcome {
	case "failed":
		return FailResult{}, nil
	case "requeued":
		if out.RetryInSeconds == nil {
			return FailResult{}, &responseError{op, "requeued without retry_in_seconds"}
		}
		if math.IsNaN(*out.RetryInSeconds) || math.IsInf(*out.RetryInSeconds, 0) || *out.RetryInSeconds < 0 || *out.RetryInSeconds > 24*3600 {
			return FailResult{}, &responseError{op, "invalid retry_in_seconds"}
		}
		return FailResult{Requeued: true, RetryIn: time.Duration(math.Round(*out.RetryInSeconds * float64(time.Second)))}, nil
	}
	return FailResult{}, &responseError{op, "unknown outcome"}
}

// Release hands the claim back, for example during a clean shutdown: the job
// becomes claimable at once and the interrupted attempt is refunded. Call it
// only after the handler has returned and renewals have stopped. The server
// cannot check that, and releasing while the handler still runs would let
// another worker execute the job at the same time.
//
// A nil error is the only confirmation of a release. ErrOwnershipLost means it
// was not applied or was already applied; an IsUncertain error means the job
// may or may not be released. The SDK never releases on its own, not even after
// a failed request.
func (c *Client) Release(ctx context.Context, job ClaimedJob) error {
	const op = "release"
	if err := checkClaim(op, job); err != nil {
		return err
	}
	status, hdr, raw, err := c.do(ctx, op, jobPath(job, "release"), ownership(job), maxErrorBytes)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return c.failure(op, status, hdr, raw)
	}
	return expectOutcome(op, raw, "released")
}

// --- shared plumbing -------------------------------------------------------

func errBadArg(op, msg string) error { return fmt.Errorf("workerclient: %s: %s", op, msg) }

func checkClaim(op string, job ClaimedJob) error {
	switch {
	case job.JobID == "":
		return errBadArg(op, "job.JobID is empty (use the job returned by Claim)")
	case !workerIDPattern.MatchString(job.WorkerID):
		return errBadArg(op, "job.WorkerID is invalid (use the job returned by Claim)")
	case job.ClaimGeneration < 1:
		return errBadArg(op, "job.ClaimGeneration must be at least 1 (use the job returned by Claim)")
	}
	return nil
}

func ownership(job ClaimedJob) any {
	return struct {
		WorkerID        string `json:"worker_id"`
		ClaimGeneration int64  `json:"claim_generation"`
	}{job.WorkerID, job.ClaimGeneration}
}

func jobPath(job ClaimedJob, action string) string {
	return "v1/worker/jobs/" + url.PathEscape(job.JobID) + "/" + action
}

func expectOutcome(op string, raw []byte, want string) error {
	var out struct {
		Outcome string `json:"outcome"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Outcome != want {
		return &responseError{op, "body is not the documented " + op + " object"}
	}
	return nil
}

// do sends one POST with a JSON body and returns the status and the bounded
// response body. It never retries. The Authorization header is the only place
// the key is used.
func (c *Client) do(ctx context.Context, op, path string, body any, limit int64) (int, http.Header, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, nil, errBadArg(op, "request could not be encoded")
	}
	u := c.base.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, errBadArg(op, "request could not be built")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

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
		return 0, nil, nil, &responseError{op, "response body is larger than the SDK accepts"}
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// sanitize removes the request URL from a *url.Error (it adds nothing useful
// and the caller already knows the base URL) while keeping the cause for
// errors.Is.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// failure turns a non-success response into an error.
func (c *Client) failure(op string, status int, hdr http.Header, raw []byte) error {
	if status >= 300 && status < 400 {
		return &responseError{op, "the server redirected the request; redirects are not followed"}
	}
	if status < 400 || status >= 600 {
		return &responseError{op, "unexpected HTTP status " + strconv.Itoa(status)}
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body) // a body that does not parse leaves Code and Message empty
	e := &Error{Status: status, Code: clean(body.Error.Code, 64), Message: clean(body.Error.Message, maxMessageRunes), Op: op}
	// Retry-After in whole seconds (the form this server sends); anything else is ignored.
	if n, err := strconv.Atoi(strings.TrimSpace(hdr.Get("Retry-After"))); err == nil && n >= 0 && n <= 3600 {
		e.RetryAfter = time.Duration(n) * time.Second
	}
	return e
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
