package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics are the shop's own numbers: what shoppers experience (request rate,
// status and latency per route, payment outcomes) and the link to Forge (how many
// outbox events are still waiting to be published). They sit in a private
// registry owned by the API, not in a global one.
//
// The route label only takes the fixed names given at registration below, so a
// caller cannot create new series. Counters and histograms belong to this
// process and restart from zero with it; the outbox gauges are read from
// ForgeCart's database at every scrape.
type metrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	payments *prometheus.CounterVec
	// The call to the payment service and the repair of payments it recorded but the shop never applied.
	paymentCalls   *prometheus.CounterVec
	paymentCallDur prometheus.Histogram
	sweepPasses    *prometheus.CounterVec
	sweepRepaired  prometheus.Counter
	// Cancelling orders that stayed unpaid.
	ordersExpired prometheus.Counter
	expiryPasses  *prometheus.CounterVec
	// Requests being handled right now, per route (registered by instrument).
	mu     sync.Mutex
	routes map[string]*concurrency
}

func newMetrics(db *sql.DB) *metrics {
	m := &metrics{
		routes: map[string]*concurrency{},
		reg:    prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forgecart_http_requests_total",
			Help: "Requests by route and HTTP status code.",
		}, []string{"route", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "forgecart_http_request_duration_seconds",
			Help:    "Time to answer a request, by route.",
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15},
		}, []string{"route"}),
		payments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forgecart_payment_attempts_total",
			Help: "New payment attempts by outcome (succeeded, declined, temporary_failure). A replay of an earlier attempt is not counted again.",
		}, []string{"outcome"}),
	}
	m.paymentCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgecart_payment_service_calls_total",
		Help: "The shop's calls to the payment service by result: ok, final (the order's payment was already final), mismatch, provider_error, interrupted, unavailable (could not be reached or failed) or rejected (a contract or key problem).",
	}, []string{"result"})
	m.paymentCallDur = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "forgecart_payment_service_call_duration_seconds",
		Help:    "Time the shop waited for the payment service to answer a payment, including the provider's time.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20},
	})
	m.sweepPasses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgecart_payment_sweep_passes_total",
		Help: "Passes of the payment sweeper by result (ok or error).",
	}, []string{"result"})
	m.sweepRepaired = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "forgecart_payment_sweep_repaired_total",
		Help: "Payments the payment service had recorded but the shop never applied to their order, repaired by the sweeper. Anything above zero means an earlier request died between the two steps.",
	})
	m.ordersExpired = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "forgecart_orders_expired_total",
		Help: "Orders cancelled because they stayed unpaid past the expiry time (their stock was returned).",
	})
	m.expiryPasses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgecart_order_expiry_passes_total",
		Help: "Passes of the order expirer by result (ok or error).",
	}, []string{"result"})
	m.expiryPasses.WithLabelValues("ok")
	m.expiryPasses.WithLabelValues("error")
	for _, r := range []string{"ok", "final", "mismatch", "provider_error", "interrupted", "unavailable", "rejected"} {
		m.paymentCalls.WithLabelValues(r)
	}
	m.sweepPasses.WithLabelValues("ok")
	m.sweepPasses.WithLabelValues("error")
	m.reg.MustRegister(m.requests, m.latency, m.payments, m.paymentCalls, m.paymentCallDur, m.sweepPasses, m.sweepRepaired, m.ordersExpired, m.expiryPasses,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewDBStatsCollector(db, "forgecart"), &outboxCollector{db: db}, &awaitingPaymentCollector{db: db})
	return m
}

// ObservePaymentCall records one call to the payment service (see payments.Service).
func (a *API) ObservePaymentCall(result string, took time.Duration) {
	a.m.paymentCalls.WithLabelValues(result).Inc()
	a.m.paymentCallDur.Observe(took.Seconds())
}

// ObserveSweep records one pass of the payment sweeper.
func (a *API) ObserveSweep(repaired int, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	a.m.sweepPasses.WithLabelValues(result).Inc()
	a.m.sweepRepaired.Add(float64(repaired))
}

// ObserveExpire records one pass of the order expirer. repaired counts orders it completed from a
// payment the payment service already held, which also feeds the sweeper's repair counter.
func (a *API) ObserveExpire(expired, repaired int, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	a.m.expiryPasses.WithLabelValues(result).Inc()
	a.m.ordersExpired.Add(float64(expired))
	a.m.sweepRepaired.Add(float64(repaired))
}

// statusRecorder remembers the status code a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// concurrency counts the requests of one route that are being handled right now and the highest
// that number has been since the process started. It is the "how many users are doing this at the
// same moment" number: by Little's law it equals requests per second times seconds per request.
type concurrency struct{ cur, peak atomic.Int64 }

func (c *concurrency) enter() {
	n := c.cur.Add(1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

func (c *concurrency) leave() { c.cur.Add(-1) }

// concurrencyFor returns the counter of a route and, the first time, publishes it as two gauges.
func (m *metrics) concurrencyFor(route string) *concurrency {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.routes[route]; ok {
		return c
	}
	c := &concurrency{}
	m.routes[route] = c
	labels := prometheus.Labels{"route": route}
	m.reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "forgecart_http_in_flight", ConstLabels: labels,
			Help: "Requests of this route being handled right now: how many users are checking out or paying at this moment."},
			func() float64 { return float64(c.cur.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "forgecart_http_in_flight_peak", ConstLabels: labels,
			Help: "The highest number of requests of this route handled at the same time since this process started."},
			func() float64 { return float64(c.peak.Load()) }),
	)
	return c
}

// instrument counts and times a handler under a fixed route name, and tracks how many are in flight.
func (m *metrics) instrument(route string, h http.HandlerFunc) http.HandlerFunc {
	c := m.concurrencyFor(route)
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		start := time.Now()
		c.enter()
		defer c.leave()
		h(rec, r)
		m.latency.WithLabelValues(route).Observe(time.Since(start).Seconds())
		m.requests.WithLabelValues(route, strconv.Itoa(rec.code)).Inc()
	}
}

// outboxCollector reads the outbox backlog at scrape time. Both numbers use the
// partial index on pending events, so the query stays cheap however many events
// have already been published.
type outboxCollector struct{ db *sql.DB }

var (
	outboxPendingDesc = prometheus.NewDesc("forgecart_outbox_pending_events",
		"Outbox events written by paid orders and not yet published to Forge.", nil, nil)
	outboxOldestDesc = prometheus.NewDesc("forgecart_outbox_oldest_pending_age_seconds",
		"Age of the oldest pending outbox event; 0 when there is none. Rising means the dispatcher or Forge is not keeping up.", nil, nil)
	outboxUpDesc = prometheus.NewDesc("forgecart_outbox_stats_up",
		"1 if the outbox gauges were read from the database, 0 if that failed (the gauges are then absent, never zero).", nil, nil)
)

func (c *outboxCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- outboxPendingDesc
	ch <- outboxOldestDesc
	ch <- outboxUpDesc
}

func (c *outboxCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var pending int64
	var oldest float64
	err := c.db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(EXTRACT(EPOCH FROM (now() - min(created_at))), 0)::float8
		FROM outbox_events WHERE status = 'pending'`).Scan(&pending, &oldest)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(outboxUpDesc, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(outboxUpDesc, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(outboxPendingDesc, prometheus.GaugeValue, float64(pending))
	ch <- prometheus.MustNewConstMetric(outboxOldestDesc, prometheus.GaugeValue, oldest)
}

// awaitingPaymentCollector reports the orders that exist but are not paid yet: customers who reached
// the payment step (or are still deciding). Unlike the in-flight payment requests (milliseconds to
// seconds) these wait for minutes, for example on Razorpay's page. The partial index on pending
// orders keeps the query cheap however many orders have been paid.
type awaitingPaymentCollector struct{ db *sql.DB }

var (
	awaitingDesc = prometheus.NewDesc("forgecart_orders_awaiting_payment",
		"Orders created and not yet paid, declined or cancelled (stock reserved).", nil, nil)
	awaitingOldestDesc = prometheus.NewDesc("forgecart_orders_awaiting_payment_oldest_age_seconds",
		"Age of the oldest order still awaiting payment; 0 when there is none.", nil, nil)
)

func (c *awaitingPaymentCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- awaitingDesc
	ch <- awaitingOldestDesc
}

func (c *awaitingPaymentCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var n int64
	var oldest float64
	err := c.db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(EXTRACT(EPOCH FROM (now() - min(created_at))), 0)::float8
		FROM orders WHERE status = 'pending_payment'`).Scan(&n, &oldest)
	if err != nil {
		return // the gauges are absent, never zero (forgecart_outbox_stats_up already reports the database)
	}
	ch <- prometheus.MustNewConstMetric(awaitingDesc, prometheus.GaugeValue, float64(n))
	ch <- prometheus.MustNewConstMetric(awaitingOldestDesc, prometheus.GaugeValue, oldest)
}

// MetricsHandler serves GET /metrics for Prometheus. It must be served on its
// own listener, never on the shop's public address, and it requires the bearer
// key: the numbers describe the business (orders, payment outcomes).
func (a *API) MetricsHandler(key string) http.Handler {
	want := sha256.Sum256([]byte(key))
	metricsPage := promhttp.HandlerFor(a.m.reg, promhttp.HandlerOpts{})
	mux := http.NewServeMux()
	mux.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(token))
		// Compare digests in constant time, so neither the key's length nor its
		// content leaks through timing. Every failure gets the same answer.
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
			return
		}
		metricsPage.ServeHTTP(w, r)
	}))
	return mux
}
