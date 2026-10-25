package paymentsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// RazorpayConfig selects Razorpay as the payment provider. KeyID and KeySecret
// are the account's test-mode (or live) API keys; the secret is never logged.
// BaseURL is https://api.razorpay.com unless a stand-in server is used.
type RazorpayConfig struct {
	KeyID     string
	KeySecret string
	BaseURL   string
}

// ErrProvider means Razorpay could not be reached or refused the request. The
// order is unchanged, so repeating the request is safe.
var ErrProvider = errors.New("paymentsvc: the payment provider could not complete the request")

const maxProviderBody = 1 << 20

// linkLifetime is how long a payment link stays payable. After that Razorpay
// marks it expired, the next payment request declines the order and the stock
// reserved at checkout is released (without an expiry, Razorpay's default would
// hold it for six months). Razorpay requires expire_by to be at least 15 minutes
// ahead and refuses exactly 15 (checked: 900 s refused, 905 s accepted), so one
// minute of margin covers the request time and any clock difference.
const linkLifetime = 16 * time.Minute

// expiryGrace is how long past expire_by ForgeCart waits before declaring an
// unpaid link expired itself, because Razorpay's status does not flip on time.
// The wait keeps a customer who pays in the last seconds from being declined by
// mistake: a payment that went through shows the link as "paid" almost at once.
const expiryGrace = 2 * time.Minute

// razorpay is a minimal client for the two Payment Links calls ForgeCart needs:
// create a link, and read its status. It uses only the standard library.
type razorpay struct {
	cfg RazorpayConfig
	hc  *http.Client
}

func newRazorpay(cfg RazorpayConfig) *razorpay {
	return &razorpay{cfg: cfg, hc: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// paymentLink is the part of Razorpay's payment link object ForgeCart reads.
type paymentLink struct {
	ID       string `json:"id"`
	ShortURL string `json:"short_url"`
	Status   string `json:"status"` // created, partially_paid, expired, cancelled or paid
	// Amount (minor units) and Currency are what the link asks for.
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	// ExpireBy is when the link stops being payable (Unix seconds). Razorpay does
	// not move an unpaid link to "expired" at that moment: measured on AWS, a link
	// was still "created" more than six minutes past its expire_by.
	ExpireBy int64 `json:"expire_by"`
	// Payments lists the captured payments; Razorpay sends null until one exists.
	Payments []struct {
		PaymentID string `json:"payment_id"`
	} `json:"payments"`
}

// createLink asks Razorpay for a hosted payment page for the order. The order ID
// is the link's reference_id (at most 40 characters; ours has 28), which
// Razorpay requires to be unique, so one order can never get two links.
// Razorpay's own notifications are turned off: ForgeCart sends nothing to the
// customer through Razorpay and shares no customer details with it.
func (r *razorpay) createLink(ctx context.Context, orderID string, amountMinor int64, currency string) (paymentLink, error) {
	var out paymentLink
	err := r.do(ctx, http.MethodPost, "/v1/payment_links", map[string]any{
		"amount":       amountMinor,
		"currency":     currency,
		"reference_id": orderID,
		"expire_by":    time.Now().Add(linkLifetime).Unix(),
		"description":  "ForgeCart order " + orderID,
		"notify":       map[string]bool{"sms": false, "email": false},
	}, &out)
	if err == nil && (out.ID == "" || out.ShortURL == "") {
		err = fmt.Errorf("%w: the answer had no link", ErrProvider)
	}
	return out, err
}

// fetchLink reads the current state of a payment link.
func (r *razorpay) fetchLink(ctx context.Context, id string) (paymentLink, error) {
	var out paymentLink
	err := r.do(ctx, http.MethodGet, "/v1/payment_links/"+url.PathEscape(id), nil, &out)
	return out, err
}

// cancelLink stops a payment link from being payable. Razorpay refuses to cancel a link
// that is already paid, which the caller has checked just before.
func (r *razorpay) cancelLink(ctx context.Context, id string) error {
	var out paymentLink
	return r.do(ctx, http.MethodPost, "/v1/payment_links/"+url.PathEscape(id)+"/cancel", nil, &out)
}

// do sends one request with HTTP Basic authentication. Errors name only the
// HTTP status or a coarse cause, never the response body or the keys.
func (r *razorpay) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.cfg.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.SetBasicAuth(r.cfg.KeyID, r.cfg.KeySecret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: request failed", ErrProvider)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
	if err != nil {
		return fmt.Errorf("%w: reading the answer failed", ErrProvider)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: HTTP %d", ErrProvider, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: the answer was not valid JSON", ErrProvider)
	}
	return nil
}
