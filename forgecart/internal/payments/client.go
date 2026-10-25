package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Errors the order service reports for the payment service's answers.
var (
	// ErrProvider: the payment provider (Razorpay) could not complete the request.
	// Nothing was recorded, so repeating the same request is safe.
	ErrProvider = errors.New("payments: the payment provider could not complete the request")
	// ErrInterrupted: the payment service was stopped or cancelled before it recorded
	// the attempt. Retrying the same key is safe.
	ErrInterrupted = errors.New("payments: interrupted before the attempt was recorded")
	// ErrUnavailable: the payment service could not be reached or failed. The attempt may
	// or may not have been recorded; retrying with the same key is safe either way.
	ErrUnavailable = errors.New("payments: the payment service is unavailable")

	errFinal    = errors.New("payments: the order's payment is already final")
	errMismatch = errors.New("payments: the payment service holds another amount for the order")
)

// OutcomePending is the answer while a Razorpay payment link has not been paid.
const OutcomePending = "pending"

// Outcomes of a recorded attempt.
const (
	OutcomeSucceeded        = "succeeded"
	OutcomeDeclined         = "declined"
	OutcomeTemporaryFailure = "temporary_failure"
)

// Client calls the payment service. It is the wire contract of that service's
// POST /v1/payments, written out here so the order service does not import the
// payment service's code.
type Client struct {
	base string
	key  string
	hc   *http.Client
}

// NewClient returns a client for the payment service at baseURL. Every shopper
// request makes one call, so the idle-connection limit is raised well above Go's
// default of 2 per host; otherwise most calls would open a new connection.
func NewClient(baseURL, key string) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 512
	tr.MaxIdleConnsPerHost = 512
	return &Client{base: baseURL, key: key, hc: &http.Client{Transport: tr}}
}

type payRequest struct {
	OrderID     string `json:"order_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type payResult struct {
	AttemptID         string    `json:"payment_attempt_id"`
	AttemptNumber     int       `json:"attempt_number"`
	Outcome           string    `json:"outcome"`
	ProviderReference *string   `json:"provider_reference"`
	PaymentURL        string    `json:"payment_url"`
	CompletedAt       time.Time `json:"completed_at"`
	Replayed          bool      `json:"replayed"`
	Expired           bool      `json:"expired"`
}

const maxAnswer = 1 << 20

// finalPayment is one final attempt returned by the payment service.
type finalPayment struct {
	OrderID     string    `json:"order_id"`
	AttemptID   string    `json:"payment_attempt_id"`
	Outcome     string    `json:"outcome"`
	CompletedAt time.Time `json:"completed_at"`
	Expired     bool      `json:"expired"` // a decline that closed an unpaid order
}

// finals asks for up to limit final attempts completed after the cursor and at least lag ago.
func (c *Client) finals(ctx context.Context, afterAt time.Time, afterID string, lag time.Duration, limit int) ([]finalPayment, error) {
	body, _ := json.Marshal(map[string]any{"after_completed_at": afterAt, "after_id": afterID, "lag_seconds": int(lag.Seconds()), "limit": limit})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/payments/finals", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: finals answered HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	var out struct {
		Payments []finalPayment `json:"payments"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: the answer was not valid JSON", ErrUnavailable)
	}
	return out.Payments, nil
}

func (c *Client) pay(ctx context.Context, key string, pr payRequest) (payResult, error) {
	return c.post(ctx, "/v1/payments", key, pr)
}

// cancel closes an order's payment because the order expired. The answer is the order's final
// attempt; Expired is true when it is the decline that closed the order.
func (c *Client) cancel(ctx context.Context, pr payRequest) (payResult, error) {
	return c.post(ctx, "/v1/payments/cancel", "", pr)
}

func (c *Client) post(ctx context.Context, path, key string, pr payRequest) (payResult, error) {
	body, _ := json.Marshal(pr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return payResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return payResult{}, fmt.Errorf("%w: request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return payResult{}, fmt.Errorf("%w: reading the answer failed", ErrUnavailable)
	}
	if resp.StatusCode == http.StatusOK {
		var out payResult
		if err := json.Unmarshal(raw, &out); err != nil {
			return payResult{}, fmt.Errorf("%w: the answer was not valid JSON", ErrUnavailable)
		}
		return out, nil
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	switch e.Error.Code {
	case "order_payment_final":
		return payResult{}, errFinal
	case "payment_amount_mismatch":
		return payResult{}, errMismatch
	case "payment_provider_error":
		return payResult{}, ErrProvider
	case "interrupted":
		return payResult{}, ErrInterrupted
	}
	if resp.StatusCode >= 500 {
		return payResult{}, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	// 4xx for anything else means the two services disagree about the contract
	// (bad key, malformed request): a bug or a misconfiguration, not a retryable state.
	return payResult{}, fmt.Errorf("payments: the payment service refused the request (HTTP %d, %q)", resp.StatusCode, e.Error.Code)
}
