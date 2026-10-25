package workerapp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

// workerMetrics are what one worker process says about its own load: how many of its slots are
// busy right now, the most that have been busy, how jobs end and how long they take. The ratio
// of busy to total slots is the signal for scaling this one service: near 1 means more slots or
// more copies would help, near 0 means they would not.
type workerMetrics struct {
	reg      *prometheus.Registry
	jobType  string
	cur      atomic.Int64
	peak     atomic.Int64
	outcomes *prometheus.CounterVec
	duration prometheus.Histogram
}

func newWorkerMetrics(jobType string, slots int, dbs ...*sql.DB) *workerMetrics {
	m := &workerMetrics{reg: prometheus.NewRegistry(), jobType: jobType}
	labels := prometheus.Labels{"job_type": jobType}
	m.outcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forgecart_worker_jobs_total", ConstLabels: labels,
		Help: "Jobs this worker finished, by outcome: succeeded, rejected (the data contradicts the event: never retried) or error (an infrastructure failure: Forge retries it).",
	}, []string{"outcome"})
	for _, o := range []string{"succeeded", "rejected", "error"} {
		m.outcomes.WithLabelValues(o)
	}
	m.duration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "forgecart_worker_job_duration_seconds", ConstLabels: labels,
		Help:    "Time the handler took for one job, from start to return.",
		Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	})
	m.reg.MustRegister(m.outcomes, m.duration,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "forgecart_worker_slots", ConstLabels: labels,
			Help: "How many jobs this worker process can run at the same time (FORGECART_WORKER_CONCURRENCY)."},
			func() float64 { return float64(slots) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "forgecart_worker_jobs_in_flight", ConstLabels: labels,
			Help: "Jobs this worker is running right now."},
			func() float64 { return float64(m.cur.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "forgecart_worker_jobs_in_flight_peak", ConstLabels: labels,
			Help: "The most jobs this worker has run at the same time since it started."},
			func() float64 { return float64(m.peak.Load()) }),
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	for i, db := range dbs {
		name := []string{"shop", "results"}[i]
		m.reg.MustRegister(collectors.NewDBStatsCollector(db, "worker_"+name))
	}
	return m
}

// wrap makes a handler count itself. It changes nothing about how the handler behaves or fails.
func (m *workerMetrics) wrap(h workerruntime.HandlerFunc) workerruntime.HandlerFunc {
	return func(ctx context.Context, job workerruntime.Job) (any, error) {
		n := m.cur.Add(1)
		for {
			p := m.peak.Load()
			if n <= p || m.peak.CompareAndSwap(p, n) {
				break
			}
		}
		start := time.Now()
		defer func() {
			m.cur.Add(-1)
			m.duration.Observe(time.Since(start).Seconds())
		}()
		res, err := h(ctx, job)
		var rejected *bizjob.RejectError
		switch {
		case err == nil:
			m.outcomes.WithLabelValues("succeeded").Inc()
		case errors.As(err, &rejected):
			m.outcomes.WithLabelValues("rejected").Inc()
		default:
			m.outcomes.WithLabelValues("error").Inc()
		}
		return res, err
	}
}

// handler serves GET /metrics behind the bearer key, like the shop's and the payment service's.
func (m *workerMetrics) handler(key string) http.Handler {
	want := sha256.Sum256([]byte(key))
	page := promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
	mux := http.NewServeMux()
	mux.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(token))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		page.ServeHTTP(w, r)
	}))
	return mux
}
