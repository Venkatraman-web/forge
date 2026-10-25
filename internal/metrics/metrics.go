// Package metrics exposes Forge's operational numbers in the Prometheus text
// format, so a developer can see whether their jobs are healthy, keeping up,
// and where the bottleneck is (see docs/reference.md, "Metrics").
//
// Two kinds of numbers, deliberately kept apart:
//
//   - Gauges read from the database at scrape time (queued, running, oldest due
//     age per job type): authoritative, shared by every Forge process.
//   - Counters kept by THIS Forge process (submissions by result, job events,
//     reaper recoveries): they restart from zero when the process restarts and
//     cover only this process. Prometheus handles that: rate() tolerates resets
//     and several instances are simply summed.
//
// The job type is the only label with many values, and it is bounded: a type
// that is not configured (a client can submit any string and have it rejected)
// is reported as "other", so a caller cannot create unbounded series.
//
// Every method of *Metrics is safe to call on a nil receiver and does nothing,
// so code that records metrics never needs a nil check.
package metrics

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Venkatraman-web/forge/internal/store"
)

// Submission results (label "result" of forge_submissions_total).
const (
	ResultAccepted      = "accepted"
	ResultQueueFull     = "queue_full"
	ResultAdmissionBusy = "admission_busy"
	ResultReplayed      = "replayed" // an idempotency key already held this job; nothing new stored
	ResultInvalid       = "invalid"  // unsupported type, invalid input, invalid option or idempotency conflict
	ResultError         = "error"    // storage failure or other internal error
)

// Job events (label "event" of forge_job_events_total).
const (
	EventClaimed        = "claimed"
	EventSucceeded      = "succeeded"
	EventRetried        = "retried" // attempt failed, job queued again
	EventFailed         = "failed"  // attempt failed, job is now failed
	EventReleased       = "released"
	EventOwnershipLost  = "ownership_lost"
	EventAttemptTimeout = "attempt_timeout" // an attempt ended because it hit its execution timeout
	EventAttemptPanic   = "attempt_panic"
)

var (
	results = []string{ResultAccepted, ResultReplayed, ResultQueueFull, ResultAdmissionBusy, ResultInvalid, ResultError}
	events  = []string{EventClaimed, EventSucceeded, EventRetried, EventFailed, EventReleased, EventOwnershipLost, EventAttemptTimeout, EventAttemptPanic}
)

const otherType = "other"

// Metrics holds Forge's registry and the counters this process maintains.
type Metrics struct {
	reg         *prometheus.Registry
	known       map[string]bool
	submissions *prometheus.CounterVec
	jobEvents   *prometheus.CounterVec
	reaped      *prometheus.CounterVec
	purged      *prometheus.CounterVec
	queueWait   *prometheus.HistogramVec
	attemptTime *prometheus.HistogramVec
}

// New creates the registry. types are the configured job types; their counters
// are created at zero so a rate() never starts from a missing series.
func New(types []string) *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry(), known: map[string]bool{}}
	m.submissions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forge_submissions_total",
		Help: "Job submissions handled by this Forge process, by job type and result. Resets when the process restarts.",
	}, []string{"type", "result"})
	m.jobEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forge_job_events_total",
		Help: "Job lifecycle events seen by this Forge process, by job type and event. An attempt that ends by timeout counts once as retried or failed and once as attempt_timeout. Resets when the process restarts.",
	}, []string{"type", "event"})
	m.reaped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forge_reaper_recovered_total",
		Help: "Jobs recovered by this process's reaper after their worker stopped renewing the lease (requeued, or failed with no attempts left). Resets when the process restarts.",
	}, []string{"outcome"})
	m.purged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "forge_retention_purged_total",
		Help: "Finished jobs deleted by this process's retention purge, by the status they had (succeeded, failed). Resets when the process restarts.",
	}, []string{"status"})
	m.queueWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "forge_job_queue_wait_seconds",
		Help:    "Time a job waited between becoming due (submission, or the moment a retry or release made it claimable) and being claimed, by the database clock, for claims handed out by this Forge process. Resets when the process restarts.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 300},
	}, []string{"type"})
	m.attemptTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "forge_job_attempt_duration_seconds",
		Help:    "Time from an attempt's claim to Forge recording its outcome (succeeded, retried or failed), by the database clock; for a remote worker it includes the time to report. Resets when the process restarts.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 600},
	}, []string{"type", "outcome"})
	m.reg.MustRegister(m.submissions, m.jobEvents, m.reaped, m.purged, m.queueWait, m.attemptTime,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	for _, t := range append(append([]string{}, types...), otherType) {
		if t != otherType {
			m.known[t] = true
		}
		for _, r := range results {
			m.submissions.WithLabelValues(t, r)
		}
		for _, e := range events {
			m.jobEvents.WithLabelValues(t, e)
		}
		m.queueWait.WithLabelValues(t)
		for _, o := range []string{EventSucceeded, EventRetried, EventFailed} {
			m.attemptTime.WithLabelValues(t, o)
		}
	}
	m.reaped.WithLabelValues("requeued")
	m.reaped.WithLabelValues("exhausted")
	m.purged.WithLabelValues("succeeded")
	m.purged.WithLabelValues("failed")
	return m
}

// label bounds the type label to the configured types.
func (m *Metrics) label(jobType string) string {
	if m.known[jobType] {
		return jobType
	}
	return otherType
}

// Submission counts one submission outcome.
func (m *Metrics) Submission(jobType, result string) {
	if m == nil {
		return
	}
	m.submissions.WithLabelValues(m.label(jobType), result).Inc()
}

// JobEvent counts one job lifecycle event.
func (m *Metrics) JobEvent(jobType, event string) {
	if m == nil {
		return
	}
	m.jobEvents.WithLabelValues(m.label(jobType), event).Inc()
}

// QueueWait records how long a claimed job waited since it became due.
func (m *Metrics) QueueWait(jobType string, wait time.Duration) {
	if m == nil {
		return
	}
	m.queueWait.WithLabelValues(m.label(jobType)).Observe(wait.Seconds())
}

// AttemptDuration records how long an attempt took; outcome is EventSucceeded,
// EventRetried or EventFailed. A zero duration means unknown and is skipped.
func (m *Metrics) AttemptDuration(jobType, outcome string, d time.Duration) {
	if m == nil || d <= 0 {
		return
	}
	m.attemptTime.WithLabelValues(m.label(jobType), outcome).Observe(d.Seconds())
}

// Reaped counts jobs recovered by one reaper pass.
func (m *Metrics) Reaped(requeued, exhausted int) {
	if m == nil {
		return
	}
	m.reaped.WithLabelValues("requeued").Add(float64(requeued))
	m.reaped.WithLabelValues("exhausted").Add(float64(exhausted))
}

// Purged counts finished jobs deleted by one retention pass.
func (m *Metrics) Purged(succeeded, failed int) {
	if m == nil {
		return
	}
	m.purged.WithLabelValues("succeeded").Add(float64(succeeded))
	m.purged.WithLabelValues("failed").Add(float64(failed))
}

// QueueSource is what the queue gauges need from storage (store.Store has it).
type QueueSource interface {
	QueueStats(ctx context.Context) (store.QueueStats, error)
}

// RegisterQueue adds the database-read gauges. They run one read-only query per
// scrape; if it fails, forge_queue_stats_up is 0 and the per-type gauges are
// left out, never reported as zero. limit is the configured queue limit.
func (m *Metrics) RegisterQueue(src QueueSource, limit int, types []string) {
	if m == nil {
		return
	}
	m.reg.MustRegister(&queueCollector{src: src, limit: limit, types: types})
}

// RegisterDB adds the database connection pool gauges and counters.
func (m *Metrics) RegisterDB(db *sql.DB) {
	if m == nil {
		return
	}
	m.reg.MustRegister(&dbCollector{db: db})
}

// RegisterAdmission adds the admission-slot gauges. slots returns the number
// of slots in use and the total.
func (m *Metrics) RegisterAdmission(slots func() (inUse, total int)) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "forge_admission_slots_in_use",
		Help: "Submissions currently inside Forge's bounded admission step in this process.",
	}, func() float64 { n, _ := slots(); return float64(n) }))
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "forge_admission_slots_total",
		Help: "Size of the bounded admission step in this process (a fixed constant).",
	}, func() float64 { _, n := slots(); return float64(n) }))
}

// Handler serves the Prometheus text exposition. A collector that fails
// reports the failure through its own up-style gauge; the scrape itself still
// succeeds.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// ---------- database-read gauges ----------

const queueStatsTimeout = 3 * time.Second

var (
	descQueueJobs = prometheus.NewDesc("forge_queue_jobs",
		"Jobs per type and state, read from the database at scrape time (states: queued, running).", []string{"type", "state"}, nil)
	descOldest = prometheus.NewDesc("forge_queue_oldest_due_age_seconds",
		"Seconds the oldest queued job that is already due has been due, per type; 0 when none is due.", []string{"type"}, nil)
	descLimit = prometheus.NewDesc("forge_queue_limit",
		"Configured global limit on queued jobs (FORGE_MAX_QUEUED).", nil, nil)
	descUp = prometheus.NewDesc("forge_queue_stats_up",
		"1 if the queue gauges were read from the database at this scrape, 0 if the read failed (the per-type gauges are then absent).", nil, nil)
)

type queueCollector struct {
	src   QueueSource
	limit int
	types []string
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descQueueJobs
	ch <- descOldest
	ch <- descLimit
	ch <- descUp
}

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(descLimit, prometheus.GaugeValue, float64(c.limit))
	ctx, cancel := context.WithTimeout(context.Background(), queueStatsTimeout)
	defer cancel()
	stats, err := c.src.QueueStats(ctx)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 1)
	seen := map[string]bool{}
	emit := func(t string, queued, running int, oldest *time.Duration) {
		seen[t] = true
		ch <- prometheus.MustNewConstMetric(descQueueJobs, prometheus.GaugeValue, float64(queued), t, "queued")
		ch <- prometheus.MustNewConstMetric(descQueueJobs, prometheus.GaugeValue, float64(running), t, "running")
		age := 0.0
		if oldest != nil {
			age = oldest.Seconds()
		}
		ch <- prometheus.MustNewConstMetric(descOldest, prometheus.GaugeValue, age, t)
	}
	for _, t := range stats.Types {
		emit(t.Type, t.Queued, t.Running, t.OldestDueAge)
	}
	for _, t := range c.types { // a configured type with no jobs is a known zero
		if !seen[t] {
			emit(t, 0, 0, nil)
		}
	}
}

// ---------- database pool ----------

var (
	descPoolConns = prometheus.NewDesc("forge_db_pool_connections",
		"Connections in Forge's database pool by state (in_use, idle).", []string{"state"}, nil)
	descPoolMax = prometheus.NewDesc("forge_db_pool_max_connections",
		"Configured maximum open connections in the pool (fixed at 10).", nil, nil)
	descPoolWaitCount = prometheus.NewDesc("forge_db_pool_wait_total",
		"Times a caller had to wait for a free pooled connection, since process start.", nil, nil)
	descPoolWaitSeconds = prometheus.NewDesc("forge_db_pool_wait_seconds_total",
		"Total seconds callers spent waiting for a pooled connection, since process start.", nil, nil)
)

type dbCollector struct{ db *sql.DB }

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descPoolConns
	ch <- descPoolMax
	ch <- descPoolWaitCount
	ch <- descPoolWaitSeconds
}

func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	st := c.db.Stats()
	ch <- prometheus.MustNewConstMetric(descPoolConns, prometheus.GaugeValue, float64(st.InUse), "in_use")
	ch <- prometheus.MustNewConstMetric(descPoolConns, prometheus.GaugeValue, float64(st.Idle), "idle")
	ch <- prometheus.MustNewConstMetric(descPoolMax, prometheus.GaugeValue, float64(st.MaxOpenConnections))
	ch <- prometheus.MustNewConstMetric(descPoolWaitCount, prometheus.CounterValue, float64(st.WaitCount))
	ch <- prometheus.MustNewConstMetric(descPoolWaitSeconds, prometheus.CounterValue, st.WaitDuration.Seconds())
}
