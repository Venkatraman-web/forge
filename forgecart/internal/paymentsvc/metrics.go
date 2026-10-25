package paymentsvc

import (
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

// metrics are the payment service's own numbers: request rate, status and latency
// per route, new attempts by outcome and provider failures. They sit in a private
// registry, and the route label only takes the fixed names given at registration,
// so a caller cannot create new series. Counters and histograms belong to this
// process and restart from zero with it.
type metrics struct {
	reg            *prometheus.Registry
	requests       *prometheus.CounterVec
	latency        *prometheus.HistogramVec
	attempts       *prometheus.CounterVec
	providerErrors prometheus.Counter
	webhooks       *prometheus.CounterVec
	pollPasses     *prometheus.CounterVec
	pollSettled    prometheus.Counter
	mu             sync.Mutex
	routes         map[string]*concurrency
}

func newMetrics(db *sql.DB) *metrics {
	m := &metrics{
		routes: map[string]*concurrency{},
		reg:    prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forgecart_paymentsvc_http_requests_total",
			Help: "Payment service requests by route and HTTP status code.",
		}, []string{"route", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "forgecart_paymentsvc_http_request_duration_seconds",
			Help:    "Time the payment service took to answer, by route. For payments it includes the provider's time (the simulator's delay, or Razorpay).",
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20},
		}, []string{"route"}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forgecart_paymentsvc_attempts_total",
			Help: "New payment answers by outcome: succeeded, declined and temporary_failure are recorded attempts; pending is a Razorpay payment still waiting for the customer (nothing recorded). A replay of an earlier attempt is not counted again.",
		}, []string{"outcome"}),
		providerErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "forgecart_paymentsvc_provider_errors_total",
			Help: "Payments that failed because the payment provider (Razorpay) could not complete the request. Nothing was recorded.",
		}),
	}
	m.webhooks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgecart_paymentsvc_webhooks_total",
		Help: "Razorpay webhook requests by result: ok (handled), ignored (an event or reference that does not concern payments here), bad_signature, bad_request, mismatch (the link's amount differs; not retried) or error (5xx, Razorpay delivers again).",
	}, []string{"result"})
	m.pollPasses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgecart_paymentsvc_razorpay_poll_passes_total",
		Help: "Passes of the Razorpay poller by result (ok or error). Errors usually mean Razorpay's API could not be reached.",
	}, []string{"result"})
	m.pollPasses.WithLabelValues("ok")
	m.pollPasses.WithLabelValues("error")
	m.pollSettled = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "forgecart_paymentsvc_razorpay_poll_settled_total",
		Help: "Payments the poller settled because neither the webhook nor the customer had asked about them. Anything above zero means the webhook is missing or late.",
	})
	for _, r := range []string{"ok", "ignored", "bad_signature", "bad_request", "mismatch", "error"} {
		m.webhooks.WithLabelValues(r)
	}
	for _, o := range []string{OutcomeSucceeded, OutcomeDeclined, OutcomeTemporaryFailure, OutcomePending} {
		m.attempts.WithLabelValues(o)
	}
	m.reg.MustRegister(m.requests, m.latency, m.attempts, m.providerErrors, m.webhooks, m.pollPasses, m.pollSettled,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewDBStatsCollector(db, "payments"))
	return m
}

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
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "forgecart_paymentsvc_http_in_flight", ConstLabels: labels,
			Help: "Requests of this route being handled right now. For pay this is how many payments are in progress at this moment, including the time spent at the provider."},
			func() float64 { return float64(c.cur.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "forgecart_paymentsvc_http_in_flight_peak", ConstLabels: labels,
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

// ObservePoll records one pass of the Razorpay poller.
func (a *API) ObservePoll(settled int, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	a.m.pollPasses.WithLabelValues(result).Inc()
	a.m.pollSettled.Add(float64(settled))
}

// MetricsHandler serves GET /metrics for Prometheus. Serve it on its own listener,
// never on the address the order service calls, and keep the bearer key: the
// numbers describe the business.
func (a *API) MetricsHandler(key string) http.Handler {
	want := sha256.Sum256([]byte(key))
	page := promhttp.HandlerFor(a.m.reg, promhttp.HandlerOpts{})
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
		page.ServeHTTP(w, r)
	}))
	return mux
}
