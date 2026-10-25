// Package httpapi serves ForgeCart's HTTP endpoints.
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Venkatraman-web/forge/forgecart/internal/catalog"
	"github.com/Venkatraman-web/forge/forgecart/internal/orders"
	"github.com/Venkatraman-web/forge/forgecart/internal/payments"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

const (
	maxOrderBody    = 16 << 10 // bytes; far above the largest valid order (20 items)
	checkoutTimeout = 10 * time.Second
	paymentTimeout  = 20 * time.Second // the payment service's own limit is 15 s, plus applying the outcome
	maxPaymentBody  = 1 << 10
)

type API struct {
	db      *sql.DB
	catalog *catalog.Store
	orders  *orders.Store
	pay     *payments.Service
	log     *slog.Logger
	m       *metrics
}

func New(db *sql.DB, log *slog.Logger, pay *payments.Service) *API {
	return &API{db: db, catalog: catalog.NewStore(db), orders: orders.NewStore(db), pay: pay, log: log, m: newMetrics(db)}
}

// NewServer returns an http.Server with explicit timeouts.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	// Patterns carry no method on purpose: the mux's built-in 405 is plain
	// text, so allowRead answers wrong methods in the API's JSON shape.
	// Each route has a fixed name for the metrics, so labels stay bounded.
	mux.HandleFunc("/healthz", a.m.instrument("health", allowRead(a.healthz)))
	mux.HandleFunc("/readyz", a.m.instrument("ready", allowRead(a.readyz)))
	mux.HandleFunc("/v1/products", a.m.instrument("products_list", allowRead(a.listProducts)))
	mux.HandleFunc("/v1/products/{sku}", a.m.instrument("product_get", allowRead(a.getProduct)))
	mux.HandleFunc("/v1/orders", a.m.instrument("order_create", allowOnly(a.createOrder, http.MethodPost)))
	mux.HandleFunc("/v1/orders/{order_id}/payment-attempts", a.m.instrument("payment_attempt", allowOnly(a.createPaymentAttempt, http.MethodPost)))
	return mux
}

// allowRead permits GET and HEAD (net/http answers HEAD without a body).
func allowRead(h http.HandlerFunc) http.HandlerFunc {
	return allowOnly(h, http.MethodGet, http.MethodHead)
}

// allowOnly rejects every method not listed with a JSON 405 and an Allow header.
func allowOnly(h http.HandlerFunc, methods ...string) http.HandlerFunc {
	allow := strings.Join(methods, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		for _, m := range methods {
			if r.Method == m {
				h(w, r)
				return
			}
		}
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed; this endpoint supports: "+allow+".")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (a *API) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.db.PingContext(ctx); err != nil {
		a.log.Warn("readiness check failed")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (a *API) listProducts(w http.ResponseWriter, r *http.Request) {
	// r.URL.Query() silently drops pairs it cannot parse (a raw ";" or bad
	// percent-encoding), which would turn a broken cursor into "first page".
	// ParseQuery reports those errors, so reject the request instead.
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "The query string is malformed (check percent-encoding and use & between parameters).")
		return
	}
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be an integer from 1 to 100.")
			return
		}
		limit = n
	}
	after := q.Get("after")
	if after != "" && !catalog.SKUPattern.MatchString(after) {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "after must be a SKU returned as next_cursor.")
		return
	}
	// Fetch one extra row to learn whether another page exists.
	items, err := a.catalog.List(r.Context(), after, limit+1)
	if err != nil {
		a.log.Error("list products failed")
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		c := items[limit-1].SKU
		next = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": items, "next_cursor": next})
}

func (a *API) getProduct(w http.ResponseWriter, r *http.Request) {
	sku := r.PathValue("sku")
	if !catalog.SKUPattern.MatchString(sku) {
		writeError(w, http.StatusNotFound, "product_not_found", "No product with that SKU.")
		return
	}
	p, err := a.catalog.Get(r.Context(), sku)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, "product_not_found", "No product with that SKU.")
	case err != nil:
		a.log.Error("get product failed")
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

type orderRequest struct {
	CustomerEmail string `json:"customer_email"`
	Items         []struct {
		SKU      string `json:"sku"`
		Quantity int    `json:"quantity"`
	} `json:"items"`
}

func (a *API) createOrder(w http.ResponseWriter, r *http.Request) {
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Send exactly one Idempotency-Key header.")
		return
	}
	key := strings.TrimSpace(keys[0])
	if e := orders.ValidateKey(key); e != nil {
		writeOrderError(w, e)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.")
		return
	}

	var body orderRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOrderBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(&body)
	if err == nil {
		// A second value (or garbage) after the object is rejected, not ignored.
		if _, err2 := dec.Token(); err2 != io.EOF {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "The request body is too large.")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "The body must be one JSON object with customer_email and items, with no unknown fields.")
		return
	}

	items := make([]orders.Item, len(body.Items))
	for i, it := range body.Items {
		items[i] = orders.Item{SKU: it.SKU, Quantity: it.Quantity}
	}
	req, e := orders.NewRequest(body.CustomerEmail, items)
	if e != nil {
		writeOrderError(w, e)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), checkoutTimeout)
	defer cancel()
	order, err := a.orders.Checkout(ctx, key, req)
	var oe *orders.Error
	switch {
	case errors.As(err, &oe):
		writeOrderError(w, oe)
	case err != nil:
		// Log only a class: driver errors can carry row values such as emails.
		a.log.Error("checkout failed", "cause", errClass(err))
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
	case order.Replayed:
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, order)
	default:
		writeJSON(w, http.StatusCreated, order)
	}
}

func writeOrderError(w http.ResponseWriter, e *orders.Error) {
	status := http.StatusBadRequest
	switch e.Kind {
	case orders.KindNotFound:
		status = http.StatusNotFound
	case orders.KindConflict:
		status = http.StatusConflict
	}
	writeError(w, status, e.Code, e.Message)
}

// errClass names a failure without its text: the PostgreSQL SQLSTATE when
// there is one, otherwise a coarse category.
func errClass(err error) string {
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe):
		return "sqlstate_" + pe.Code
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "other"
}

// createPaymentAttempt runs one simulated payment for an order. The outcome is
// decided by server configuration; the body must be empty (or "{}"), so no
// client can supply card data or a payment status.
func (a *API) createPaymentAttempt(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("order_id")
	if !payments.ValidOrderID(orderID) {
		writeError(w, http.StatusBadRequest, "invalid_order_id", "order_id is not a valid order ID.")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Send exactly one Idempotency-Key header.")
		return
	}
	key := strings.TrimSpace(keys[0])
	if e := orders.ValidateKey(key); e != nil {
		writeOrderError(w, e)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPaymentBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "The request body is too large.")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body could not be read.")
		return
	}
	if b := strings.TrimSpace(string(body)); b != "" && b != "{}" {
		writeError(w, http.StatusBadRequest, "unexpected_body", "This endpoint takes no request body; the payment outcome is set by the server.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), paymentTimeout)
	defer cancel()
	res, err := a.pay.Pay(ctx, orderID, key)
	var oe *orders.Error
	switch {
	case errors.As(err, &oe):
		writeOrderError(w, oe)
	case errors.Is(err, payments.ErrProvider):
		// Razorpay could not be reached or refused the request. The order is
		// unchanged, so the same request can simply be repeated.
		a.log.Warn("payment provider failed", "cause", errClass(err))
		writeError(w, http.StatusBadGateway, "payment_provider_error", "The payment provider could not complete the request. Try again.")
	case errors.Is(err, payments.ErrInterrupted):
		writeError(w, http.StatusServiceUnavailable, "interrupted", "The attempt was not recorded; retry with the same Idempotency-Key.")
	case errors.Is(err, payments.ErrUnavailable):
		// The attempt may or may not have been recorded; the same key settles it.
		a.log.Warn("payment service unavailable", "cause", errClass(err))
		writeError(w, http.StatusServiceUnavailable, "payment_unavailable", "The payment service could not be reached; retry with the same Idempotency-Key.")
	case err != nil:
		a.log.Error("payment attempt failed", "cause", errClass(err))
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
	default:
		a.log.Info("payment attempt", "order_id", res.OrderID, "attempt", res.AttemptNumber, "outcome", res.Outcome, "replayed", res.Replayed)
		if !res.Replayed && res.Outcome != payments.OutcomePending {
			a.m.payments.WithLabelValues(res.Outcome).Inc()
		}
		if res.Replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		writeJSON(w, http.StatusOK, res)
	}
}
