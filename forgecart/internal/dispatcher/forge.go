// Package dispatcher publishes ForgeCart outbox events to Forge as jobs over
// Forge's HTTP API. It never touches Forge's database and runs no business logic.
package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/Venkatraman-web/forge/forgecart/internal/outbox"
	"github.com/Venkatraman-web/forge/sdk/forgeclient"
)

// Outcome classes of one submission. Only Accepted means Forge confirmed the job.
type outcomeKind int

const (
	outcomeAccepted  outcomeKind = iota // 202 with a job_id: Forge stored the job
	outcomeRetry                        // transient or uncertain: try again later
	outcomePermanent                    // Forge rejected THIS event's request
	outcomeFatal                        // configuration-wide problem: stop the dispatcher
	// outcomeBackpressure: Forge is healthy but its queue is full (503 or 429 with
	// the stable error code queue_full). Nothing was stored. This is flow control,
	// not a failure of the event, so it is retried without spending the budget.
	outcomeBackpressure
	// outcomeAdmissionBusy: Forge answered quickly that it is limiting how many
	// submissions it processes at once (503 or 429 with the stable code
	// admission_busy). The queue may have room and nothing was stored. Like
	// backpressure it is not the event's fault and is never charged, but it is
	// momentary and per request, so only this event waits: unrelated events
	// continue and the dispatcher does not pause.
	outcomeAdmissionBusy
)

type outcome struct {
	kind       outcomeKind
	code       string        // sanitized machine code, safe to store and log
	retryAfter time.Duration // from a valid Retry-After header (429/503), else 0
}

const maxRetryAfter = 5 * time.Minute

var (
	errCodePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	orderIDPattern  = regexp.MustCompile(`^ord_[0-9a-f]{24}$`)
	attemptIDPatten = regexp.MustCompile(`^pay_[0-9a-f]{24}$`)
)

// forgeClient submits jobs through Forge's public Go client (sdk/forgeclient),
// the same package any other application would use. Forge's single-job API
// accepts an idempotency key, but the dispatcher submits BATCHES (one request,
// one database commit, for the jobs of many events), and a batch carries no
// key. A crash between submitting and marking the event published therefore
// leaves Forge with a duplicate job. That is harmless: the stable event_id
// travels inside the job input and every worker's result row is unique per
// event, so the duplicate does nothing. Sending one request per job with a key
// would remove the duplicate but give up the batching the dispatcher needs.
type forgeClient struct {
	c *forgeclient.Client
}

func newForgeClient(baseURL, apiKey string, timeout time.Duration) (*forgeClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("Forge URL must be http(s)://host[:port] with no credentials, path or query")
	}
	c, err := forgeclient.New(forgeclient.Config{
		BaseURL:    u.Scheme + "://" + u.Host,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: timeout}, // the client never follows redirects
	})
	if err != nil {
		return nil, err
	}
	return &forgeClient{c: c}, nil
}

type jobInput struct {
	EventID          string `json:"event_id"`
	OrderID          string `json:"order_id"`
	PaymentAttemptID string `json:"payment_attempt_id"`
}

// buildRequests maps an outbox event to its Forge submissions: an "order.paid"
// event becomes one job per outbox.JobTypes entry, and an older event (written
// when each job had its own event) becomes the one job of its own type. Every
// job's input holds the same IDs and no customer data. No options are sent: the
// retry budget and timeout of each job type are Forge's server-side policy
// (forgecart/forge.json, loaded through FORGE_CONFIG_FILE).
func buildRequests(eventID, eventType string, payload []byte) ([]forgeclient.SubmitRequest, error) {
	var p struct {
		OrderID          string `json:"order_id"`
		PaymentAttemptID string `json:"payment_attempt_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || !orderIDPattern.MatchString(p.OrderID) || !attemptIDPatten.MatchString(p.PaymentAttemptID) {
		return nil, errors.New("invalid outbox payload")
	}
	in := jobInput{EventID: eventID, OrderID: p.OrderID, PaymentAttemptID: p.PaymentAttemptID}
	if eventType != outbox.OrderPaid {
		return []forgeclient.SubmitRequest{{Type: eventType, Input: in}}, nil
	}
	reqs := make([]forgeclient.SubmitRequest, len(outbox.JobTypes))
	for i, t := range outbox.JobTypes {
		reqs[i] = forgeclient.SubmitRequest{Type: t, Input: in}
	}
	return reqs, nil
}

// submit sends one request and classifies the result. A transport error or
// timeout is outcomeRetry but UNCERTAIN: Forge may have stored the job.
func (c *forgeClient) submit(ctx context.Context, req forgeclient.SubmitRequest) outcome {
	_, err := c.c.Submit(ctx, req)
	return classify(err)
}

// submitJobs sends the jobs of one or more events: one job alone, several as a
// batch. See submitBatch for what a batch answer means.
func (c *forgeClient) submitJobs(ctx context.Context, reqs []forgeclient.SubmitRequest) outcome {
	if len(reqs) == 1 {
		return c.submit(ctx, reqs[0])
	}
	return c.submitBatch(ctx, reqs)
}

// submitBatch sends several events as ONE request (all or nothing in Forge, one
// database commit) and classifies the single answer, which applies to every
// event in the batch. An uncertain result (timeout, lost connection) may mean
// Forge stored the whole batch.
func (c *forgeClient) submitBatch(ctx context.Context, reqs []forgeclient.SubmitRequest) outcome {
	_, err := c.c.SubmitBatch(ctx, reqs)
	return classify(err)
}

// classify applies the retry policy to the result of a submission.
func classify(err error) outcome {
	if err == nil {
		return outcome{kind: outcomeAccepted}
	}
	var he *forgeclient.Error
	if errors.As(err, &he) {
		return classifyStatus(he.Status, safeErrorCode(he.Code), min(he.RetryAfter, maxRetryAfter))
	}
	var te *forgeclient.TransportError
	if errors.As(err, &te) {
		var ne interface{ Timeout() bool }
		if errors.Is(te, context.DeadlineExceeded) || (errors.As(te, &ne) && ne.Timeout()) {
			return outcome{kind: outcomeRetry, code: "timeout"}
		}
		return outcome{kind: outcomeRetry, code: "network_error"}
	}
	// The client refused to build the request (not a Forge answer): a bug here.
	return outcome{kind: outcomeFatal, code: "bad_request_build"}
}

// classifyStatus is the retry policy, keyed by Forge's HTTP status and error
// code (Forge's contract: 202 + job JSON on success; JSON {"error":{"code"}}
// otherwise). The client reports every response that is not the expected
// success as an error that still carries the status that arrived.
func classifyStatus(status int, code string, retryAfter time.Duration) outcome {
	switch {
	case status == http.StatusAccepted:
		// The client only reports a 202 as an error when its body is not a Forge
		// job: this is not the Forge we expect.
		return outcome{kind: outcomeFatal, code: "unexpected_response"}
	case (status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests) && code == "admission_busy":
		// Its own class, checked before queue_full and the generic 429/5xx rows;
		// the queue_full policy below is deliberately left as it was.
		return outcome{kind: outcomeAdmissionBusy, code: "forge_admission_busy", retryAfter: retryAfter}
	case (status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests) && code == "queue_full":
		// Checked before the generic 429/5xx rows: only this specific, documented
		// response is treated as backpressure. Every other 429 or 5xx keeps its
		// established, charged retry behaviour.
		return outcome{kind: outcomeBackpressure, code: "forge_queue_full", retryAfter: retryAfter}
	case status == http.StatusTooManyRequests:
		return outcome{kind: outcomeRetry, code: "forge_http_429", retryAfter: retryAfter}
	case status >= 500 && status <= 599, status == http.StatusRequestTimeout:
		return outcome{kind: outcomeRetry, code: "forge_http_" + strconv.Itoa(status), retryAfter: retryAfter}
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge:
		if code == "unsupported_job_type" {
			// ForgeCart only sends its known types, so this means Forge is
			// not configured for ForgeCart's jobs: every event would fail the
			// same way. It is a configuration problem, not a bad event.
			return outcome{kind: outcomeFatal, code: "forge_unsupported_job_type"}
		}
		return outcome{kind: outcomePermanent, code: "rejected_" + code}
	default:
		// 401/403 (auth in front of Forge), 404/405 (wrong URL), redirects and
		// any other unexpected status: fail fast, do not dead-letter events.
		return outcome{kind: outcomeFatal, code: "forge_http_" + strconv.Itoa(status)}
	}
}

// safeErrorCode keeps Forge's error code only if it looks like one; otherwise
// "unknown". Response text is never stored.
func safeErrorCode(code string) string {
	if errCodePattern.MatchString(code) {
		return code
	}
	return "unknown"
}

func (o outcome) String() string { return fmt.Sprintf("%d/%s", o.kind, o.code) }
