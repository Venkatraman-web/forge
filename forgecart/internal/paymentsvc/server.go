package paymentsvc

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	maxBody = 1 << 10
	// requestTimeout bounds one payment: the simulated delay (at most MaxDelayLimit)
	// or the Razorpay calls (15 s client timeout each), plus the database work.
	requestTimeout = MaxDelayLimit + 10*time.Second
)

// API is the payment service's HTTP interface. The order service is its only
// caller and authenticates with one shared bearer key.
type API struct {
	svc *Service
	key string
	log *slog.Logger
	m   *metrics
}

func NewAPI(db *sql.DB, svc *Service, key string, log *slog.Logger) *API {
	return &API{svc: svc, key: key, log: log, m: newMetrics(db)}
}

// NewServer returns an http.Server with explicit timeouts.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      requestTimeout + 5*time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.m.instrument("health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("/v1/payments", a.m.instrument("pay", a.pay))
	mux.HandleFunc("/v1/payments/finals", a.m.instrument("finals", a.finals))
	mux.HandleFunc("/v1/payments/cancel", a.m.instrument("cancel", a.cancel))
	return mux
}

func (a *API) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(a.key)) == 1
}

// pay takes one payment. Body: {"order_id", "amount_minor", "currency"}; header
// Idempotency-Key. The outcome is decided by the provider (the simulator's
// settings are server configuration), never by the caller.
func (a *API) pay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST.")
		return
	}
	if !a.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "A valid bearer key is required.")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || !keyPattern.MatchString(strings.TrimSpace(keys[0])) {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Send exactly one valid Idempotency-Key header.")
		return
	}
	key := strings.TrimSpace(keys[0])
	var req Request
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "The body must be one JSON object with order_id, amount_minor and currency.")
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	res, err := a.svc.Pay(ctx, req, key)
	switch {
	case errors.Is(err, ErrFinal):
		writeError(w, http.StatusConflict, "order_payment_final", "The order's payment is already final.")
	case errors.Is(err, ErrAmountMismatch):
		writeError(w, http.StatusConflict, "payment_amount_mismatch", "The amount differs from the order's earlier payment attempts.")
	case errors.Is(err, ErrProvider):
		a.m.providerErrors.Inc()
		a.log.Warn("payment provider failed", "cause", "provider")
		writeError(w, http.StatusBadGateway, "payment_provider_error", "The payment provider could not complete the request. Try again.")
	case errors.Is(err, ErrInterrupted):
		writeError(w, http.StatusServiceUnavailable, "interrupted", "The attempt was not recorded; retry with the same Idempotency-Key.")
	case err != nil:
		// Log only a class: driver errors can carry row values.
		a.log.Error("payment failed", "cause", errClass(err))
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
	default:
		a.log.Info("payment", "order_id", req.OrderID, "attempt", res.AttemptNumber, "outcome", res.Outcome, "replayed", res.Replayed)
		if !res.Replayed {
			a.m.attempts.WithLabelValues(res.Outcome).Inc()
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// cancel closes an order's payment because the order expired (see Service.Cancel). Body:
// {"order_id", "amount_minor", "currency"}. The answer is the order's final attempt, with
// "expired": true when it is the decline that closed the order.
func (a *API) cancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST.")
		return
	}
	if !a.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "A valid bearer key is required.")
		return
	}
	var req Request
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "The body must be one JSON object with order_id, amount_minor and currency.")
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	res, expired, err := a.svc.Cancel(ctx, req)
	switch {
	case errors.Is(err, ErrAmountMismatch):
		writeError(w, http.StatusConflict, "payment_amount_mismatch", "The amount differs from the order's earlier payment attempts.")
	case errors.Is(err, ErrProvider):
		a.m.providerErrors.Inc()
		a.log.Warn("payment provider failed", "cause", "provider")
		writeError(w, http.StatusBadGateway, "payment_provider_error", "The payment provider could not complete the request. Try again.")
	case err != nil:
		a.log.Error("payment cancel failed", "cause", errClass(err))
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
	default:
		a.log.Info("payment cancel", "order_id", req.OrderID, "outcome", res.Outcome, "expired", expired, "replayed", res.Replayed)
		writeJSON(w, http.StatusOK, struct {
			Result
			Expired bool `json:"expired"`
		}{res, expired})
	}
}

// finals returns final attempts after a cursor, oldest first. Body:
// {"after_completed_at", "after_id", "lag_seconds", "limit"}. It only reads.
func (a *API) finals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST.")
		return
	}
	if !a.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "A valid bearer key is required.")
		return
	}
	var req struct {
		AfterCompletedAt time.Time `json:"after_completed_at"`
		AfterID          string    `json:"after_id"`
		LagSeconds       int       `json:"lag_seconds"`
		Limit            int       `json:"limit"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Limit < 1 || req.Limit > MaxFinals || req.LagSeconds < 0 || req.LagSeconds > 3600 || len(req.AfterID) > 64 {
		writeError(w, http.StatusBadRequest, "invalid_json", "The body must have after_completed_at, after_id, lag_seconds (0 to 3600) and limit (1 to 500).")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	finals, err := a.svc.Finals(ctx, req.AfterCompletedAt, req.AfterID, time.Duration(req.LagSeconds)*time.Second, req.Limit)
	if err != nil {
		a.log.Error("payment finals failed", "cause", errClass(err))
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payments": finals})
}

func errClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "other"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
