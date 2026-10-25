package paymentsvc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// ErrInterrupted means the simulated provider delay was cut short by shutdown or
// request cancellation. Nothing was recorded; retrying the same key is safe.
var ErrInterrupted = errors.New("paymentsvc: interrupted before the attempt was recorded")

// ErrFinal means the order already has a final attempt (succeeded or declined)
// recorded under a different key, so it cannot be paid again.
var ErrFinal = errors.New("paymentsvc: the order's payment is already final")

// ErrAmountMismatch means the request names another amount or currency than the
// order's earlier attempts did.
var ErrAmountMismatch = errors.New("paymentsvc: the amount differs from the order's earlier attempts")

var (
	orderIDPattern = regexp.MustCompile(`^ord_[0-9a-f]{24}$`)
	keyPattern     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
	currencyRe     = regexp.MustCompile(`^[A-Z]{3}$`)
)

// OutcomePending is the answer while a Razorpay payment link has not been paid.
// It is never stored.
const OutcomePending = "pending"

// Request is one payment to take for an order.
type Request struct {
	OrderID     string `json:"order_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

func (r Request) validate() error {
	if !orderIDPattern.MatchString(r.OrderID) {
		return errors.New("order_id is not a valid order ID")
	}
	if r.AmountMinor < 0 {
		return errors.New("amount_minor must not be negative")
	}
	if !currencyRe.MatchString(r.Currency) {
		return errors.New("currency must be a three-letter upper-case code")
	}
	return nil
}

// Result is the service's answer. A payment still waiting for the customer
// (Razorpay) has Outcome "pending" and a PaymentURL, and no attempt fields.
type Result struct {
	AttemptID         string    `json:"payment_attempt_id,omitempty"`
	AttemptNumber     int       `json:"attempt_number,omitempty"`
	Outcome           string    `json:"outcome"`
	ProviderReference *string   `json:"provider_reference,omitempty"`
	PaymentURL        string    `json:"payment_url,omitempty"`
	CompletedAt       time.Time `json:"completed_at,omitzero"`
	Replayed          bool      `json:"replayed"`
}

type Service struct {
	db  *sql.DB
	cfg Config
	rz  *razorpay // nil unless Config.Provider is "razorpay"
	// shutdown is cancelled when the process is stopping; it only interrupts
	// the simulated delay, never a database transaction.
	shutdown context.Context
	// OnPoll observes each pass of the Razorpay poller (metrics); set it before RunPoller.
	OnPoll func(settled int, err error)
}

func NewService(shutdown context.Context, db *sql.DB, cfg Config) *Service {
	s := &Service{db: db, cfg: cfg, shutdown: shutdown}
	if cfg.Provider == ProviderRazorpay {
		s.rz = newRazorpay(cfg.Razorpay)
	}
	return s
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// findAttempt returns the attempt recorded for (order, key), if any.
func findAttempt(ctx context.Context, q queryer, req Request, key string) (Result, bool, error) {
	r := Result{Replayed: true}
	var amount int64
	var currency string
	err := q.QueryRowContext(ctx, `
		SELECT id, attempt_number, outcome, provider_reference, completed_at, amount_minor, currency
		FROM payment_attempts WHERE order_id = $1 AND idempotency_key = $2`, req.OrderID, key).
		Scan(&r.AttemptID, &r.AttemptNumber, &r.Outcome, &r.ProviderReference, &r.CompletedAt, &amount, &currency)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if amount != req.AmountMinor || currency != req.Currency {
		return Result{}, false, ErrAmountMismatch
	}
	r.CompletedAt = r.CompletedAt.UTC()
	return r, true, nil
}

// latest is the order's most recent attempt. Nothing is recorded after a final
// attempt, so if the order has one it is the latest.
type latest struct {
	number   int
	outcome  string
	amount   int64
	currency string
}

// checkLatest returns the order's latest attempt (zero number when there is
// none) and refuses a request that cannot be taken: the order's payment is
// already final, or the amount differs from its earlier attempts.
func checkLatest(ctx context.Context, q queryer, req Request) (latest, error) {
	var l latest
	err := q.QueryRowContext(ctx, `
		SELECT attempt_number, outcome, amount_minor, currency FROM payment_attempts
		WHERE order_id = $1 ORDER BY attempt_number DESC LIMIT 1`, req.OrderID).Scan(&l.number, &l.outcome, &l.amount, &l.currency)
	if errors.Is(err, sql.ErrNoRows) {
		return latest{}, nil
	}
	if err != nil {
		return latest{}, err
	}
	if l.outcome != OutcomeTemporaryFailure {
		return l, ErrFinal
	}
	if l.amount != req.AmountMinor || l.currency != req.Currency {
		return l, ErrAmountMismatch
	}
	return l, nil
}

// MaxFinals is how many attempts one Finals call may return.
const MaxFinals = 500

// Final is a final (succeeded or declined) attempt.
type Final struct {
	OrderID           string    `json:"order_id"`
	AttemptID         string    `json:"payment_attempt_id"`
	AttemptNumber     int       `json:"attempt_number"`
	Outcome           string    `json:"outcome"`
	ProviderReference *string   `json:"provider_reference,omitempty"`
	CompletedAt       time.Time `json:"completed_at"`
	// Expired marks a decline that closed an unpaid order (see Cancel), as opposed to a
	// payment the provider declined.
	Expired bool `json:"expired"`
}

// expireKeyPrefix starts the idempotency key of the decline that closes an expired order.
const expireKeyPrefix = "expire:"

// Finals returns up to limit final attempts completed after the cursor (completed_at, id), in
// completion order, leaving out those completed less than lag ago: by then the request that
// recorded an attempt has either applied it to its order or died. The order service's sweeper
// uses it to find payments it recorded here but never applied.
func (s *Service) Finals(ctx context.Context, afterAt time.Time, afterID string, lag time.Duration, limit int) ([]Final, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT order_id, id, attempt_number, outcome, provider_reference, completed_at,
		       (outcome = 'declined' AND idempotency_key LIKE 'expire:%')
		FROM payment_attempts
		WHERE outcome IN ('succeeded', 'declined') AND (completed_at, id) > ($1::timestamptz, $2::text)
		  AND completed_at < now() - $3::interval
		ORDER BY completed_at, id LIMIT $4`, afterAt, afterID, lag.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Final{}
	for rows.Next() {
		var f Final
		if err := rows.Scan(&f.OrderID, &f.AttemptID, &f.AttemptNumber, &f.Outcome, &f.ProviderReference, &f.CompletedAt, &f.Expired); err != nil {
			return nil, err
		}
		f.CompletedAt = f.CompletedAt.UTC()
		out = append(out, f)
	}
	return out, rows.Err()
}

// finalAttempt returns the order's final (succeeded or declined) attempt, if it has one. There
// is at most one: the partial unique indexes allow one of each outcome and nothing is recorded
// after a final attempt.
func finalAttempt(ctx context.Context, q queryer, orderID string) (Result, bool, bool, error) {
	r := Result{Replayed: true}
	var key string
	err := q.QueryRowContext(ctx, `
		SELECT id, attempt_number, outcome, provider_reference, completed_at, idempotency_key
		FROM payment_attempts WHERE order_id = $1 AND outcome IN ('succeeded', 'declined') LIMIT 1`, orderID).
		Scan(&r.AttemptID, &r.AttemptNumber, &r.Outcome, &r.ProviderReference, &r.CompletedAt, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, false, nil
	}
	if err != nil {
		return Result{}, false, false, err
	}
	r.CompletedAt = r.CompletedAt.UTC()
	return r, true, r.Outcome == OutcomeDeclined && strings.HasPrefix(key, expireKeyPrefix), nil
}

// Cancel closes an order's payment for good, because the order is being expired. It
// returns the order's final attempt if it already has one (a payment that succeeded or
// was declined: the order service then applies that instead). Otherwise it records a
// decline under the key "expire:<order>" and the order can no longer be paid: any later
// payment request gets ErrFinal. For Razorpay it first looks at the payment link: a paid
// link is recorded as a succeeded payment (the customer did pay), and an unpaid link that
// can still be paid is cancelled at Razorpay before the decline is recorded, so money cannot
// arrive for a closed order. expired reports whether the result is the closing decline.
func (s *Service) Cancel(ctx context.Context, req Request) (res Result, expired bool, err error) {
	if r, found, exp, err := finalAttempt(ctx, s.db, req.OrderID); err != nil || found {
		return r, exp, err
	}
	key := expireKeyPrefix + req.OrderID
	dec := Decision{Outcome: OutcomeDeclined}
	if s.rz != nil {
		var linkID string
		err := s.db.QueryRowContext(ctx, `SELECT link_id FROM razorpay_links WHERE order_id = $1`, req.OrderID).Scan(&linkID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// No link was ever created: nothing can arrive.
		case err != nil:
			return Result{}, false, err
		default:
			link, err := s.rz.fetchLink(ctx, linkID)
			if err != nil {
				return Result{}, false, err
			}
			switch link.Status {
			case "paid":
				if len(link.Payments) == 0 || link.Payments[0].PaymentID == "" {
					return Result{}, false, fmt.Errorf("%w: the link is paid but lists no payment", ErrProvider)
				}
				dec = Decision{Outcome: OutcomeSucceeded, ProviderReference: link.Payments[0].PaymentID}
			case "expired", "cancelled":
			default:
				if err := s.rz.cancelLink(ctx, linkID); err != nil {
					return Result{}, false, err
				}
			}
		}
	}
	res, err = s.record(ctx, req, key, dec, time.Now())
	if errors.Is(err, ErrFinal) {
		// A payment request finished first: report that attempt.
		r, found, exp, ferr := finalAttempt(ctx, s.db, req.OrderID)
		if ferr != nil || !found {
			return Result{}, false, ferr
		}
		return r, exp, nil
	}
	return res, err == nil && dec.Outcome == OutcomeDeclined, err
}

// Pay performs one logical payment attempt for an order.
//
//  1. Look up (order, key). If it exists, replay the stored attempt without
//     asking the provider again.
//  2. Otherwise decide the outcome (deterministically, for the simulator) and
//     wait out the provider's time with NO transaction open and no lock held.
//  3. In one transaction: take the order's advisory lock (every state-changing
//     payment for one order is serialized by it), re-check the key and the
//     order's latest attempt, allocate the attempt number and insert the attempt.
//
// The provider has no external side effects in the simulator, so computing a
// candidate before the transaction is harmless: if persistence fails nothing was
// recorded, and a retry with the same key reaches the same decision.
func (s *Service) Pay(ctx context.Context, req Request, key string) (Result, error) {
	startedAt := time.Now()
	if r, found, err := findAttempt(ctx, s.db, req, key); err != nil || found {
		return r, err
	}
	if _, err := checkLatest(ctx, s.db, req); err != nil {
		// A concurrent request with this same key may have committed between the
		// lookup above and this read; if so, replay it instead of refusing.
		if r, found, ferr := findAttempt(ctx, s.db, req, key); ferr != nil || found {
			return r, ferr
		}
		if errors.Is(err, ErrFinal) && s.rz != nil {
			// The webhook may have settled the payment before the customer's request came back.
			if r, found, werr := s.webhookFinal(ctx, req.OrderID); werr != nil || found {
				return r, werr
			}
		}
		return Result{}, err
	}

	if s.rz != nil {
		return s.payRazorpay(ctx, req, key, startedAt)
	}
	dec := s.cfg.Decide(req.OrderID, key)
	if dec.Delay > 0 {
		t := time.NewTimer(dec.Delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return Result{}, ErrInterrupted
		case <-s.shutdown.Done():
			return Result{}, ErrInterrupted
		}
	}
	return s.record(ctx, req, key, dec, startedAt)
}

func (s *Service) record(ctx context.Context, req Request, key string, dec Decision, startedAt time.Time) (Result, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, req.OrderID); err != nil {
		return Result{}, err
	}
	// Re-check under the lock: the same key may have been recorded during the delay.
	if r, found, err := findAttempt(ctx, tx, req, key); err != nil || found {
		return r, err
	}
	l, err := checkLatest(ctx, tx, req)
	if err != nil {
		return Result{}, err
	}
	number := l.number + 1

	var rb [12]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return Result{}, err
	}
	attemptID := "pay_" + hex.EncodeToString(rb[:])
	var ref *string
	if dec.ProviderReference != "" {
		ref = &dec.ProviderReference
	}
	var completedAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO payment_attempts (id, order_id, idempotency_key, attempt_number, amount_minor, currency, outcome, provider_reference, simulated_delay_ms, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, LEAST($10::timestamptz, now()))
		RETURNING completed_at`,
		attemptID, req.OrderID, key, number, req.AmountMinor, req.Currency, dec.Outcome, ref, dec.Delay.Milliseconds(), startedAt).Scan(&completedAt)
	if err != nil {
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{AttemptID: attemptID, AttemptNumber: number, Outcome: dec.Outcome, ProviderReference: ref, CompletedAt: completedAt.UTC()}, nil
}

// payRazorpay is Pay for the Razorpay provider. The customer pays on Razorpay's
// hosted page, so one logical payment takes several calls:
//
//   - first call: create a payment link (stored, one per order) and answer
//     "pending" with its URL; nothing is recorded as an attempt yet;
//   - later calls: read the link from Razorpay. Still waiting: "pending" again.
//     Paid: record a succeeded attempt. Expired or cancelled: record a declined
//     attempt.
//
// The attempt is recorded under the caller's idempotency key only once the
// outcome is known, so after that, repeating the key replays it.
func (s *Service) payRazorpay(ctx context.Context, req Request, key string, startedAt time.Time) (Result, error) {
	var linkID, url string
	err := s.db.QueryRowContext(ctx, `SELECT link_id, short_url FROM razorpay_links WHERE order_id = $1`, req.OrderID).Scan(&linkID, &url)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		link, err := s.rz.createLink(ctx, req.OrderID, req.AmountMinor, req.Currency)
		if err != nil {
			return Result{}, err
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO razorpay_links (order_id, link_id, short_url) VALUES ($1, $2, $3) ON CONFLICT (order_id) DO NOTHING`,
			req.OrderID, link.ID, link.ShortURL); err != nil {
			return Result{}, err
		}
		return Result{Outcome: OutcomePending, PaymentURL: link.ShortURL}, nil
	case err != nil:
		return Result{}, err
	}

	link, err := s.rz.fetchLink(ctx, linkID)
	if err != nil {
		return Result{}, err
	}
	dec, final, err := linkDecision(link)
	if err != nil {
		return Result{}, err
	}
	if final {
		return s.recordRazorpay(ctx, req, key, dec, startedAt)
	}
	return Result{Outcome: OutcomePending, PaymentURL: url}, nil // waiting for the customer
}

// linkDecision turns a payment link's state into the decision it implies. final is false while
// the customer can still pay.
func linkDecision(link paymentLink) (dec Decision, final bool, err error) {
	switch link.Status {
	case "paid":
		if len(link.Payments) == 0 || link.Payments[0].PaymentID == "" {
			return Decision{}, false, fmt.Errorf("%w: the link is paid but lists no payment", ErrProvider)
		}
		return Decision{Outcome: OutcomeSucceeded, ProviderReference: link.Payments[0].PaymentID}, true, nil
	case "expired", "cancelled":
		return Decision{Outcome: OutcomeDeclined}, true, nil
	}
	// Still "created": Razorpay does not update the status when expire_by passes,
	// so an unpaid link well past its expiry is treated as expired here.
	if link.ExpireBy > 0 && time.Now().After(time.Unix(link.ExpireBy, 0).Add(expiryGrace)) {
		return Decision{Outcome: OutcomeDeclined}, true, nil
	}
	return Decision{}, false, nil
}

// settleKeyPrefix starts the idempotency key of an attempt that the Razorpay webhook or the
// Razorpay poller recorded (both settle an order from the payment link, with no customer request).
const settleKeyPrefix = "settle:"

// webhookFinal returns the order's final attempt if the webhook or the poller recorded it. The customer's own
// polling request uses a different key, so without this it would be refused as "already final"
// although the payment it is waiting for has just been settled.
func (s *Service) webhookFinal(ctx context.Context, orderID string) (Result, bool, error) {
	r := Result{Replayed: true}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, attempt_number, outcome, provider_reference, completed_at
		FROM payment_attempts WHERE order_id = $1 AND outcome IN ('succeeded', 'declined') AND idempotency_key LIKE 'settle:%'
		LIMIT 1`, orderID).Scan(&r.AttemptID, &r.AttemptNumber, &r.Outcome, &r.ProviderReference, &r.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	r.CompletedAt = r.CompletedAt.UTC()
	return r, true, nil
}

// recordRazorpay is record for a Razorpay outcome: if a webhook settled the order in the meantime,
// that attempt is the answer.
func (s *Service) recordRazorpay(ctx context.Context, req Request, key string, dec Decision, startedAt time.Time) (Result, error) {
	res, err := s.record(ctx, req, key, dec, startedAt)
	if errors.Is(err, ErrFinal) {
		if r, found, werr := s.webhookFinal(ctx, req.OrderID); werr != nil || found {
			return r, werr
		}
	}
	return res, err
}

// Settle is what a verified Razorpay webhook (or the poller) triggers for an order. The webhook is
// only a hint that something changed: the link is read from Razorpay's API and decided exactly as
// a payment request would decide it, so a stale, repeated or forged-but-correctly-signed event
// cannot change anything by itself. It records the final attempt under "settle:<order>" (so
// repeating it is harmless) and does nothing while the customer can still pay. An error means
// "try again" and makes the webhook answer 5xx, so Razorpay redelivers.
func (s *Service) Settle(ctx context.Context, orderID string) (settled bool, err error) {
	var linkID string
	err = s.db.QueryRowContext(ctx, `SELECT link_id FROM razorpay_links WHERE order_id = $1`, orderID).Scan(&linkID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // not one of our links
	}
	if err != nil {
		return false, err
	}
	if _, found, _, err := finalAttempt(ctx, s.db, orderID); err != nil || found {
		return false, err // already settled
	}
	link, err := s.rz.fetchLink(ctx, linkID)
	if err != nil {
		return false, err
	}
	dec, final, err := linkDecision(link)
	if err != nil || !final {
		return false, err
	}
	req := Request{OrderID: orderID, AmountMinor: link.Amount, Currency: link.Currency}
	if _, err := s.record(ctx, req, settleKeyPrefix+orderID, dec, time.Now()); err != nil {
		if errors.Is(err, ErrFinal) {
			return false, nil // a payment request or the expirer finished first
		}
		return false, err
	}
	return true, nil
}

// PollPending settles Razorpay payments that nobody has asked about: it takes up to batch payment
// links created within window that have no final attempt yet, least recently checked first, and
// settles each exactly as the webhook would. It is the safety net for a webhook that is not set up,
// that never arrived, or whose retries ran out. After maxPollErrors failures in one pass (Razorpay
// is probably down) it stops instead of hammering the API; the next pass tries again.
func (s *Service) PollPending(ctx context.Context, batch int, window time.Duration) (checked, settled int, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.order_id FROM razorpay_links l
		WHERE l.created_at > now() - $1::interval
		  AND NOT EXISTS (SELECT 1 FROM payment_attempts a WHERE a.order_id = l.order_id AND a.outcome IN ('succeeded', 'declined'))
		ORDER BY l.checked_at NULLS FIRST, l.created_at LIMIT $2`, window.String(), batch)
	if err != nil {
		return 0, 0, err
	}
	var orders []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		orders = append(orders, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	failures := 0
	var lastErr error
	for _, id := range orders {
		checked++
		ok, err := s.Settle(ctx, id)
		// Mark it looked at even when it failed, so the next pass moves on to other links.
		if _, merr := s.db.ExecContext(ctx, `UPDATE razorpay_links SET checked_at = now() WHERE order_id = $1`, id); merr != nil && err == nil {
			err = merr
		}
		switch {
		case ctx.Err() != nil:
			return checked, settled, ctx.Err()
		case err != nil:
			lastErr = err
			if failures++; failures >= maxPollErrors {
				return checked, settled, err
			}
		case ok:
			settled++
		}
	}
	// A pass in which some link could not be settled is reported as failed even if others were
	// fine, so an outage is visible (log and metric) however few links are waiting.
	return checked, settled, lastErr
}

// maxPollErrors is how many failed links end a poll pass.
const maxPollErrors = 3

// RunPoller polls every interval until ctx is cancelled. One goroutine, one pass at a time.
func (s *Service) RunPoller(ctx context.Context, interval time.Duration, batch int, window time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		checked, settled, err := s.PollPending(ctx, batch, window)
		if s.OnPoll != nil && ctx.Err() == nil {
			s.OnPoll(settled, err)
		}
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("razorpay poll failed", "cause", errClass(err), "checked", checked, "settled", settled)
		case settled > 0:
			// The webhook (or the customer) normally gets there first: a settlement here means neither did.
			log.Warn("razorpay poll settled payments nobody had asked about", "settled", settled, "checked", checked)
		}
	}
}
