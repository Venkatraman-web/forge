package main

// Measurement primitives shared by every scenario: nearest-rank percentiles,
// per-stage request accounting and the per-second timeline. These are ported from
// the earlier HTTP-only generator so the numbers stay comparable, and they are the
// only calculation path: every report figure comes from these records.

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Percentile is the nearest-rank percentile of ascending samples: the sample at
// 1-based rank ceil(p/100*n). It reports false for an empty set.
func Percentile(sortedAsc []int64, p float64) (int64, bool) {
	n := len(sortedAsc)
	if n == 0 || p <= 0 || p > 100 {
		return 0, false
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sortedAsc[rank-1], true
}

func r3(v float64) *float64 { v = math.Round(v*1000) / 1000; return &v }

// LatencyStats is milliseconds computed from microsecond samples; nil means "no samples".
type LatencyStats struct {
	Count  int      `json:"count"`
	MinMS  *float64 `json:"min_ms"`
	MeanMS *float64 `json:"mean_ms"`
	P50MS  *float64 `json:"p50_ms"`
	P95MS  *float64 `json:"p95_ms"`
	P99MS  *float64 `json:"p99_ms"`
	MaxMS  *float64 `json:"max_ms"`
}

// LatencyOf computes the statistics of microsecond samples.
func LatencyOf(samplesUS []int64) LatencyStats {
	l := LatencyStats{Count: len(samplesUS)}
	if len(samplesUS) == 0 {
		return l
	}
	s := append([]int64(nil), samplesUS...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum int64
	for _, v := range s {
		sum += v
	}
	p50, _ := Percentile(s, 50)
	p95, _ := Percentile(s, 95)
	p99, _ := Percentile(s, 99)
	l.MinMS, l.MaxMS = r3(float64(s[0])/1000), r3(float64(s[len(s)-1])/1000)
	l.MeanMS = r3(float64(sum) / float64(len(s)) / 1000)
	l.P50MS, l.P95MS, l.P99MS = r3(float64(p50)/1000), r3(float64(p95)/1000), r3(float64(p99)/1000)
	return l
}

// reqKind classifies one request; exactly one applies.
type reqKind int

const (
	kExpected   reqKind = iota // the documented 2xx
	kBusiness                  // a documented business rejection (stock conflict, ...)
	kUnexpected                // any other HTTP response or malformed body
	kTransport                 // no response: connection or protocol failure
	kTimeout                   // no response before the client timeout
	kCancelled                 // aborted by the run ending
)

// Counts is the request accounting of one stage.
type Counts struct {
	Requests      int `json:"requests"`
	Responses     int `json:"http_responses"`
	S2xx          int `json:"status_2xx"`
	S4xx          int `json:"status_4xx"`
	S5xx          int `json:"status_5xx"`
	Expected      int `json:"expected_outcomes"`
	Business      int `json:"business_rejections"`
	Unexpected    int `json:"unexpected_responses"`
	Transport     int `json:"transport_failures"`
	Timeouts      int `json:"timeouts"`
	Cancellations int `json:"cancellations"`
}

type stageAcc struct {
	c       Counts
	samples []int64
}

func (a *stageAcc) add(status int, lat time.Duration, k reqKind) {
	a.c.Requests++
	switch k {
	case kTransport:
		a.c.Transport++
		return
	case kTimeout:
		a.c.Timeouts++
		return
	case kCancelled:
		a.c.Cancellations++
		return
	}
	a.c.Responses++
	switch {
	case status >= 200 && status < 300:
		a.c.S2xx++
	case status >= 400 && status < 500:
		a.c.S4xx++
	case status >= 500:
		a.c.S5xx++
	}
	switch k {
	case kExpected:
		a.c.Expected++
	case kBusiness:
		a.c.Business++
	case kUnexpected:
		a.c.Unexpected++
	}
	a.samples = append(a.samples, lat.Microseconds())
}

// StageResult is the report form of a stage.
type StageResult struct {
	Name    string       `json:"name"`
	Counts  Counts       `json:"counts"`
	Latency LatencyStats `json:"latency"`
}

func (a *stageAcc) result(name string) StageResult {
	return StageResult{Name: name, Counts: a.c, Latency: LatencyOf(a.samples)}
}

// failureRatePct is (unexpected + transport + timeouts) / (requests - cancellations), in percent.
func failureRatePct(c Counts) *float64 {
	den := c.Requests - c.Cancellations
	if den <= 0 {
		return nil
	}
	return r3(float64(c.Unexpected+c.Transport+c.Timeouts) / float64(den) * 100)
}

// SecPoint is one whole second of workload activity.
type SecPoint struct {
	T         int `json:"t_s"`
	Completed int `json:"completed_workflows"`
	Requests  int `json:"requests"`
	Errors    int `json:"errors"`
	Active    int `json:"active_max"`
}

type wfPoint struct {
	atMS  int32 // offset of completion from the start, ms
	latUS int32
}

const (
	maxWFPoints        = 400_000
	maxGapSamples      = 200_000
	maxErrorCategories = 60
)

// Recorder collects the measurements of one load run. Memory is bounded: one
// int64 per request sample (the workload size is capped), a per-second slice and
// capped error categories.
type Recorder struct {
	mu                                                          sync.Mutex
	origin                                                      time.Time
	stages                                                      map[string]*stageAcc
	sec                                                         []SecPoint
	wf                                                          []wfPoint
	gaps                                                        []int64 // inter-request overhead inside a workflow, microseconds
	errs                                                        map[string]int
	payment                                                     map[string]int
	orderOut                                                    map[string]int
	started, completed, rejStock, rejOther, failed, interrupted int
	active, maxActive                                           int
}

func NewRecorder() *Recorder {
	return &Recorder{
		stages: map[string]*stageAcc{"order_create": {}, "payment": {}, "discovery": {}},
		errs:   map[string]int{}, payment: map[string]int{}, orderOut: map[string]int{},
	}
}

func (r *Recorder) secAt(t time.Time) *SecPoint {
	i := int(t.Sub(r.origin) / time.Second)
	if i < 0 {
		i = 0
	}
	for len(r.sec) <= i {
		r.sec = append(r.sec, SecPoint{T: len(r.sec)})
	}
	return &r.sec[i]
}

func (r *Recorder) Request(stage string, status int, lat time.Duration, k reqKind, cat string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages[stage].add(status, lat, k)
	if stage != "discovery" && !r.origin.IsZero() {
		sp := r.secAt(time.Now())
		sp.Requests++
		if k == kUnexpected || k == kTransport || k == kTimeout {
			sp.Errors++
		}
	}
	if cat != "" {
		if _, ok := r.errs[cat]; !ok && len(r.errs) >= maxErrorCategories {
			cat = "other (category limit reached)"
		}
		r.errs[cat]++
	}
}

func (r *Recorder) Gap(d time.Duration) {
	r.mu.Lock()
	if len(r.gaps) < maxGapSamples {
		r.gaps = append(r.gaps, d.Microseconds())
	}
	r.mu.Unlock()
}

func (r *Recorder) Payment(o string)  { r.mu.Lock(); r.payment[o]++; r.mu.Unlock() }
func (r *Recorder) OrderOut(o string) { r.mu.Lock(); r.orderOut[o]++; r.mu.Unlock() }
func (r *Recorder) WorkflowStart() {
	r.mu.Lock()
	r.started++
	r.active++
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	if !r.origin.IsZero() {
		if sp := r.secAt(time.Now()); r.active > sp.Active {
			sp.Active = r.active
		}
	}
	r.mu.Unlock()
}

type wfEnd int

const (
	endCompleted wfEnd = iota
	endRejectedStock
	endRejectedOther
	endFailed
	endInterrupted
)

func (r *Recorder) WorkflowEnd(e wfEnd, lat time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
	switch e {
	case endCompleted:
		r.completed++
		if !r.origin.IsZero() {
			now := time.Now()
			r.secAt(now).Completed++
			if len(r.wf) < maxWFPoints {
				r.wf = append(r.wf, wfPoint{int32(now.Sub(r.origin) / time.Millisecond), int32(lat.Microseconds())})
			}
		}
	case endRejectedStock:
		r.rejStock++
	case endRejectedOther:
		r.rejOther++
	case endFailed:
		r.failed++
	case endInterrupted:
		r.interrupted++
	}
}
