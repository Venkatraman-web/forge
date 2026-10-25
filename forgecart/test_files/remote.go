package main

// The "remote" command: the E-1 open-loop load against a Forge that is ALREADY
// RUNNING (for example on separate machines), instead of a local sandbox.
//
// It needs only network access to Forge's submission API and operator
// listener, plus workers somebody else started (synthworker, same settings as
// E-1). It never touches Forge's database or any process. Everything it
// reports comes from three sources: the load generator's own timings, the
// operator API's queue state, and the DIFFERENCE between two scrapes of Forge's
// own /metrics (counters and histograms). That is why Forge must be a single
// process: its counters are per process, and a restart during a stage is
// detected and fails the stage.
//
// Stages are separated by waiting for the queue to drain, not by resetting the
// database, so a stage starts from an empty queue but the same Forge process.
//
// Secrets come from the environment (FORGE_API_KEY, FORGE_OPERATOR_API_KEY) and
// are never written to the results.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RemoteConfig is everything the remote run needs besides the two keys.
type RemoteConfig struct {
	ForgeURL      string    `json:"forge_url"`
	OperatorURL   string    `json:"operator_url"`
	Types         []string  `json:"job_types"`
	Rates         []float64 `json:"arrival_rates_jobs_per_s"`
	StageSeconds  int       `json:"stage_seconds"`
	Repetitions   int       `json:"repetitions_per_rate"`
	PayloadBytes  int       `json:"payload_bytes_approx"`
	Batch         int       `json:"jobs_per_request"` // 1 = one job per request; above 1 uses POST /v1/jobs/batch
	DrainTimeoutS int       `json:"drain_timeout_s"`
	Note          string    `json:"environment_note"` // what the operator says about the deployment (instance types, region ...)
}

// RemoteStage is one measured stage.
type RemoteStage struct {
	Label       string   `json:"label"`
	Rate        float64  `json:"offered_jobs_per_s"`
	Seconds     int      `json:"seconds"`
	Error       string   `json:"error,omitempty"`
	Submit      E1Submit `json:"submit"`
	BatchSize   int      `json:"jobs_per_request"`
	Sustainable bool     `json:"sustainable"`
	SustainWhy  []string `json:"sustainable_reasoning"`

	CompletedDuringLoad  float64            `json:"completed_during_load_per_s"`
	DrainS               float64            `json:"drain_s"`
	DrainTimedOut        bool               `json:"drain_timed_out"`
	PeakQueued           int                `json:"peak_queued"`
	PeakOldestDueS       float64            `json:"peak_oldest_due_age_s"`
	BacklogSlopePerS     float64            `json:"backlog_slope_second_half_jobs_per_s"`
	QueueWaitMS          RemoteQ            `json:"queue_wait_ms_bucket_estimate"`
	AttemptMS            RemoteQ            `json:"attempt_duration_ms_bucket_estimate"`
	ForgeCounters        map[string]float64 `json:"forge_counter_deltas"`
	GeneratorVsForgeNote string             `json:"generator_vs_forge_note,omitempty"`
}

// RemoteQ holds quantiles estimated from histogram bucket deltas. They are
// upper-bound-resolution estimates (linear inside a bucket), not exact.
type RemoteQ struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	N   float64 `json:"n"`
}

// RemoteResult is the whole run.
type RemoteResult struct {
	RunID      string          `json:"run_id"`
	StartedAt  time.Time       `json:"started_at"`
	EndedAt    time.Time       `json:"ended_at"`
	Config     RemoteConfig    `json:"config"`
	Runner     map[string]any  `json:"load_generator_host"`
	Policy     json.RawMessage `json:"forge_effective_config,omitempty"`
	Stages     []*RemoteStage  `json:"stages"`
	StopReason string          `json:"stop_reason,omitempty"`
}

// ---------- Prometheus text parsing (just what is needed) ----------

type promSample struct {
	name   string
	labels map[string]string
	value  float64
}

var promLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})? (\S+)$`)
var promLabel = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

func parseProm(r io.Reader) []promSample {
	var out []promSample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		l := sc.Text()
		if l == "" || l[0] == '#' {
			continue
		}
		m := promLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[4], 64)
		if err != nil {
			continue
		}
		s := promSample{name: m[1], labels: map[string]string{}, value: v}
		for _, lm := range promLabel.FindAllStringSubmatch(m[3], -1) {
			s.labels[lm[1]] = lm[2]
		}
		out = append(out, s)
	}
	return out
}

// snap is one scrape reduced to what the stage report needs.
type snap struct {
	at       time.Time
	counters map[string]float64  // "forge_job_events_total{event=succeeded}" summed over types
	bucketsQ map[float64]float64 // queue wait, le -> cumulative count summed over types
	bucketsA map[float64]float64 // attempt duration
	startedS float64             // process_start_time_seconds, to detect a restart
}

func summarizeProm(ss []promSample) snap {
	s := snap{counters: map[string]float64{}, bucketsQ: map[float64]float64{}, bucketsA: map[float64]float64{}}
	for _, x := range ss {
		switch x.name {
		case "forge_job_events_total":
			s.counters["events:"+x.labels["event"]] += x.value
		case "forge_submissions_total":
			s.counters["submissions:"+x.labels["result"]] += x.value
		case "forge_reaper_recovered_total":
			s.counters["reaper:"+x.labels["outcome"]] += x.value
		case "forge_job_queue_wait_seconds_bucket", "forge_job_attempt_duration_seconds_bucket":
			le := math.Inf(1)
			if x.labels["le"] != "+Inf" {
				le, _ = strconv.ParseFloat(x.labels["le"], 64)
			}
			if x.name == "forge_job_queue_wait_seconds_bucket" {
				s.bucketsQ[le] += x.value
			} else {
				s.bucketsA[le] += x.value
			}
		case "process_start_time_seconds":
			s.startedS = x.value
		}
	}
	return s
}

// quantilesFromDelta estimates quantiles (milliseconds) from the difference of
// two cumulative bucket sets.
func quantilesFromDelta(a, b map[float64]float64) RemoteQ {
	type bk struct{ le, c float64 }
	var bs []bk
	for le, c := range b {
		bs = append(bs, bk{le, c - a[le]})
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i].le < bs[j].le })
	if len(bs) == 0 || bs[len(bs)-1].c <= 0 {
		return RemoteQ{}
	}
	total := bs[len(bs)-1].c
	q := func(p float64) float64 {
		target := p / 100 * total
		prevLE, prevC := 0.0, 0.0
		for _, x := range bs {
			if x.c >= target {
				if math.IsInf(x.le, 1) {
					return prevLE * 1000 // beyond the last finite bucket: report that bound
				}
				if x.c == prevC {
					return x.le * 1000
				}
				return (prevLE + (x.le-prevLE)*(target-prevC)/(x.c-prevC)) * 1000
			}
			prevLE, prevC = x.le, x.c
		}
		return prevLE * 1000
	}
	return RemoteQ{P50: round3(q(50)), P95: round3(q(95)), P99: round3(q(99)), N: total}
}

// ---------- talking to Forge ----------

type remoteForge struct {
	cfg    RemoteConfig
	apiKey string
	opKey  string
	hc     *http.Client
	op     *opClient
}

func (f *remoteForge) get(ctx context.Context, path string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.cfg.OperatorURL, "/")+path, nil)
	req.Header.Set("Authorization", "Bearer "+f.opKey)
	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s answered %d", path, resp.StatusCode)
	}
	return b, nil
}

func (f *remoteForge) scrape(ctx context.Context) (snap, error) {
	b, err := f.get(ctx, "/metrics")
	if err != nil {
		return snap{}, err
	}
	s := summarizeProm(parseProm(strings.NewReader(string(b))))
	s.at = time.Now()
	return s, nil
}

func (f *remoteForge) queueDepth(ctx context.Context) (queued, running int, oldest float64, err error) {
	r, _, err := f.op.get(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	if r.Totals.Oldest != nil {
		oldest = *r.Totals.Oldest
	}
	return r.Totals.Queued, r.Totals.Running, oldest, nil
}

// waitIdle waits until nothing is queued or running.
func (f *remoteForge) waitIdle(ctx context.Context, limit time.Duration) (time.Duration, bool) {
	t0 := time.Now()
	for time.Since(t0) < limit && ctx.Err() == nil {
		q, r, _, err := f.queueDepth(ctx)
		if err == nil && q+r == 0 {
			return time.Since(t0), true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return time.Since(t0), false
}

// ---------- one stage ----------

func (f *remoteForge) runStage(ctx context.Context, label string, rate float64, runID string) *RemoteStage {
	cfg := f.cfg
	st := &RemoteStage{Label: label, Rate: rate, Seconds: cfg.StageSeconds}
	fail := func(format string, a ...any) *RemoteStage { st.Error = fmt.Sprintf(format, a...); return st }

	if _, ok := f.waitIdle(ctx, 120*time.Second); !ok {
		return fail("the queue was not empty before the stage (jobs still queued or running); stages are not mixed")
	}
	s0, err := f.scrape(ctx)
	if err != nil {
		return fail("metrics scrape before the stage: %v", err)
	}

	e1 := DefaultE1()
	e1.Types, e1.PayloadBytes = cfg.Types, cfg.PayloadBytes
	b := max(cfg.Batch, 1)
	gen := newE1Generator(e1, strings.TrimRight(cfg.ForgeURL, "/"), f.apiKey, rate/float64(b), cfg.StageSeconds, runID)
	if b > 1 {
		gen.batch, gen.url = b, strings.TrimRight(cfg.ForgeURL, "/")+"/v1/jobs/batch"
	}
	st.BatchSize = b

	// queue-state poller, 1 Hz
	monCtx, monStop := context.WithCancel(ctx)
	var mon sync.WaitGroup
	var mu sync.Mutex
	type pt struct {
		t float64
		q int
	}
	var pts []pt
	base := time.Now()
	mon.Add(1)
	go func() {
		defer mon.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-monCtx.Done():
				return
			case <-tick.C:
			}
			q, _, oldest, err := f.queueDepth(monCtx)
			if err != nil {
				continue
			}
			mu.Lock()
			pts = append(pts, pt{time.Since(base).Seconds(), q})
			st.PeakQueued = max(st.PeakQueued, q)
			st.PeakOldestDueS = math.Max(st.PeakOldestDueS, oldest)
			mu.Unlock()
		}
	}()

	loadStart := time.Now()
	gen.run(ctx)
	loadEnd := time.Now()
	sLoad, errLoad := f.scrape(ctx)

	drain, ok := f.waitIdle(ctx, time.Duration(cfg.DrainTimeoutS)*time.Second)
	monStop()
	mon.Wait()
	st.DrainS, st.DrainTimedOut = round3(drain.Seconds()), !ok
	s2, err := f.scrape(ctx)
	if err != nil {
		return fail("metrics scrape after the stage: %v", err)
	}
	if s2.startedS != s0.startedS {
		return fail("Forge restarted during the stage (process start time changed); its counters are not comparable")
	}
	st.Submit = gen.summary(loadStart)
	if b > 1 { // the generator counted requests; report jobs (latency stays per request)
		sm := &st.Submit
		sm.Offered, sm.Sent, sm.Accepted, sm.QueueFull, sm.QueueFullHint = sm.Offered*b, sm.Sent*b, sm.Accepted*b, sm.QueueFull*b, sm.QueueFullHint*b
		for k, v := range sm.Other {
			sm.Other[k] = v * b
		}
		sm.AcceptRatePerS, sm.OfferedRatePerS = round3(sm.AcceptRatePerS*float64(b)), rate
	}

	d := map[string]float64{}
	for k, v := range s2.counters {
		if delta := v - s0.counters[k]; delta != 0 {
			d[k] = delta
		}
	}
	st.ForgeCounters = d
	if errLoad == nil && loadEnd.Sub(loadStart) > 0 {
		st.CompletedDuringLoad = round3((sLoad.counters["events:succeeded"] - s0.counters["events:succeeded"]) / loadEnd.Sub(loadStart).Seconds())
	}
	st.QueueWaitMS = quantilesFromDelta(s0.bucketsQ, s2.bucketsQ)
	st.AttemptMS = quantilesFromDelta(s0.bucketsA, s2.bucketsA)
	if int(d["submissions:accepted"]) != st.Submit.Accepted {
		st.GeneratorVsForgeNote = fmt.Sprintf("the generator counted %d accepted submissions but Forge's counter moved by %d (another client may have used Forge, or responses were lost)", st.Submit.Accepted, int(d["submissions:accepted"]))
	}

	// backlog slope over the second half of the send window (least squares)
	mu.Lock()
	var xs, ys []float64
	for _, p := range pts {
		if p.t >= float64(cfg.StageSeconds)/2 && p.t <= float64(cfg.StageSeconds) {
			xs, ys = append(xs, p.t), append(ys, float64(p.q))
		}
	}
	mu.Unlock()
	st.BacklogSlopePerS = round3(slope(xs, ys))

	classifyRemote(st)
	return st
}

func slope(xs, ys []float64) float64 {
	n := float64(len(xs))
	if n < 2 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

// classifyRemote applies the E-1 rule (written before any run), using the
// bucket-estimated queue wait because there is no database access here.
func classifyRemote(st *RemoteStage) {
	ok := true
	why := func(c bool, good, bad string) {
		if c {
			st.SustainWhy = append(st.SustainWhy, "ok: "+good)
		} else {
			ok = false
			st.SustainWhy = append(st.SustainWhy, "NOT: "+bad)
		}
	}
	sm := st.Submit
	other := 0
	for _, v := range sm.Other {
		other += v
	}
	why(sm.Accepted == sm.Offered && sm.QueueFull == 0 && other == 0, "all offered requests accepted, no errors", fmt.Sprintf("accepted %d of %d offered; queue_full %d; other errors %d", sm.Accepted, sm.Offered, sm.QueueFull, other))
	why(float64(sm.LateOverCount) <= 0.01*float64(max(sm.Sent, 1)) && sm.InFlightCap == 0, "the generator kept to its schedule", fmt.Sprintf("%d sends later than the threshold, in-flight cap hit %d times (the load generator may be limiting)", sm.LateOverCount, sm.InFlightCap))
	why(st.BacklogSlopePerS <= 0.02*st.Rate, fmt.Sprintf("backlog slope %.1f jobs/s over the second half (<= 2%% of the rate)", st.BacklogSlopePerS), fmt.Sprintf("backlog still growing at %.1f jobs/s in the second half (> 2%% of the rate)", st.BacklogSlopePerS))
	why(!st.DrainTimedOut, fmt.Sprintf("the queue drained %.1f s after the load", st.DrainS), "the queue did not drain within the timeout")
	why(st.QueueWaitMS.N > 0 && st.QueueWaitMS.P95 <= 5000, fmt.Sprintf("estimated p95 queue wait %.0f ms (<= 5000 ms)", st.QueueWaitMS.P95), fmt.Sprintf("estimated p95 queue wait %.0f ms (> 5000 ms)", st.QueueWaitMS.P95))
	st.Sustainable = ok
}

// ---------- the run ----------

// RunRemote executes the sweep and writes results/<runID>/remote.json and
// remote-report.md. It returns the process exit code.
func RunRemote(repo, runID string, cfg RemoteConfig) int {
	if err := cfg.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	apiKey, opKey := os.Getenv("FORGE_API_KEY"), os.Getenv("FORGE_OPERATOR_API_KEY")
	if apiKey == "" || opKey == "" {
		fmt.Fprintln(os.Stderr, "error: set FORGE_API_KEY (the submit key) and FORGE_OPERATOR_API_KEY in the environment")
		return 2
	}
	runDir := filepath.Join(repo, "forgecart", "test_files", "results", runID)
	if _, err := os.Stat(runDir); err == nil {
		fmt.Fprintf(os.Stderr, "error: results/%s already exists; run directories are immutable, choose another --run-id\n", runID)
		return 2
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	ctx, stop := signalContext()
	defer stop()
	logf := func(format string, a ...any) { fmt.Printf(time.Now().Format("15:04:05 ")+format+"\n", a...) }

	res := &RemoteResult{RunID: runID, StartedAt: time.Now(), Config: cfg,
		Runner: map[string]any{"cpu": cpuModel(), "logical_cpus": runtime.NumCPU(), "memory_mb": memTotalMB(), "go": runtime.Version()}}
	finish := func(code int) int {
		res.EndedAt = time.Now()
		_ = writeJSON(filepath.Join(runDir, "remote.json"), res)
		_ = os.WriteFile(filepath.Join(runDir, "remote-report.md"), []byte(RenderRemote(res)), 0o644)
		logf("results: %s", runDir)
		return code
	}
	f := &remoteForge{cfg: cfg, apiKey: apiKey, opKey: opKey, hc: &http.Client{Timeout: 15 * time.Second}}
	f.op = &opClient{url: strings.TrimRight(cfg.OperatorURL, "/") + "/v1/queues", auth: "Bearer " + opKey, hc: f.hc}
	if _, _, _, err := f.queueDepth(ctx); err != nil {
		res.StopReason = "operator API not usable: " + err.Error()
		return finish(1)
	}
	if b, err := f.get(ctx, "/v1/config"); err == nil {
		res.Policy = json.RawMessage(b)
	}
	if _, err := f.scrape(ctx); err != nil {
		res.StopReason = "metrics not usable: " + err.Error()
		return finish(1)
	}

	// Each repetition rotates the rate order by one, so slow drift does not favour one rate.
	n := len(cfg.Rates)
	for rep := 0; rep < cfg.Repetitions; rep++ {
		for i := 0; i < n; i++ {
			rate := cfg.Rates[(i+rep)%n]
			label := fmt.Sprintf("sweep-%d-r%d", int(rate), rep+1)
			if ctx.Err() != nil {
				res.StopReason = "interrupted"
				return finish(1)
			}
			logf("stage %s: %.0f jobs/s for %d s", label, rate, cfg.StageSeconds)
			st := f.runStage(ctx, label, rate, runID)
			res.Stages = append(res.Stages, st)
			if st.Error != "" {
				logf("stage %s failed: %s", label, st.Error)
				res.StopReason = "stage " + label + " failed: " + st.Error
				return finish(1)
			}
			logf("stage %s done: accepted %.1f/s, completed-during-load %.1f/s, drain %.1f s, sustainable=%v", label, st.Submit.AcceptRatePerS, st.CompletedDuringLoad, st.DrainS, st.Sustainable)
		}
	}
	return finish(0)
}

func (c RemoteConfig) validate() error {
	for _, u := range []struct{ name, v string }{{"--forge-url", c.ForgeURL}, {"--operator-url", c.OperatorURL}} {
		p, err := url.Parse(u.v)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil {
			return fmt.Errorf("%s must be http(s)://host:port without credentials", u.name)
		}
	}
	switch {
	case len(c.Types) == 0:
		return fmt.Errorf("--types is required: the job types your workers handle (comma separated)")
	case len(c.Rates) == 0:
		return fmt.Errorf("--rates is required")
	case c.StageSeconds < 5 || c.StageSeconds > e1MaxStageS:
		return fmt.Errorf("--stage-seconds must be 5 to %d", e1MaxStageS)
	case c.Repetitions < 1 || c.Repetitions > 5:
		return fmt.Errorf("--repetitions must be 1 to 5")
	case c.PayloadBytes < 64 || c.PayloadBytes > 64<<10:
		return fmt.Errorf("--payload-bytes must be 64 to 65536")
	case c.Batch < 0 || c.Batch > 100:
		return fmt.Errorf("--batch must be 1 to 100 jobs per request")
	case c.DrainTimeoutS < 5 || c.DrainTimeoutS > 1800:
		return fmt.Errorf("--drain-timeout must be 5 to 1800 seconds")
	}
	for _, r := range c.Rates {
		if r < 1 || r > e1MaxRate {
			return fmt.Errorf("arrival rates must be 1 to %d jobs/s", e1MaxRate)
		}
	}
	return nil
}

// ---------- report ----------

func RenderRemote(r *RemoteResult) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# Forge remote load run `%s`", r.RunID)
	w("")
	w("Started %s, ended %s. Raw data: `remote.json`.", r.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"), r.EndedAt.UTC().Format("15:04:05 UTC"))
	w("")
	w("## What this is")
	w("")
	w("Open-loop load (a fixed arrival rate, whatever Forge does) sent to an **already running** Forge at the address below, executed by workers started separately. Nothing here touches Forge's database or processes. Server-side numbers are the **difference between two scrapes of Forge's own `/metrics`**; latency quantiles are **estimated from histogram buckets** (linear inside a bucket), so they are coarser than the database-timestamp figures of the local E-1 reports. Stages are separated by waiting for the queue to drain, not by resetting anything. The counters are per Forge process, so this only works against a single Forge process; a restart mid-stage fails the stage.")
	w("")
	w("| Setting | Value |")
	w("|---|---|")
	w("| Forge (submission API) | %s |", r.Config.ForgeURL)
	w("| Forge (operator API) | %s |", r.Config.OperatorURL)
	w("| Job types | %s |", strings.Join(r.Config.Types, ", "))
	w("| Stage length | %d s, %d repetition(s) per rate, rate order rotated each repetition |", r.Config.StageSeconds, r.Config.Repetitions)
	w("| Payload | about %d bytes of input |", r.Config.PayloadBytes)
	if r.Config.Batch > 1 {
		w("| Jobs per request | %d (`POST /v1/jobs/batch`); rates, counts and throughput are in jobs, but **submit latency is per request** (one request carries %d jobs) |", r.Config.Batch, r.Config.Batch)
	}
	w("| Load generator host | %v, %v logical CPUs, %v MB |", r.Runner["cpu"], r.Runner["logical_cpus"], r.Runner["memory_mb"])
	if r.Config.Note != "" {
		w("| Deployment (operator's description) | %s |", r.Config.Note)
	}
	w("")
	w("The deployment description is whatever the operator typed; this tool cannot verify it. If the load generator shares a machine with Forge or the workers, say so there: they would compete for CPU.")
	w("")
	if r.StopReason != "" {
		w("**Run stopped:** %s", r.StopReason)
		w("")
	}
	w("## Results by stage")
	w("")
	w("| Stage | Offered /s | Accepted /s | Rejected (queue_full) | Other errors | Submit p50 / p95 / p99 (ms) | Completed /s during load | Queue wait p50 / p95 (ms, bucket estimate) | Peak queued | Drain (s) | Sustainable? |")
	w("|---|---|---|---|---|---|---|---|---|---|---|")
	for _, s := range r.Stages {
		if s.Error != "" {
			w("| %s | %.0f | - | - | - | - | - | - | - | - | error: %s |", s.Label, s.Rate, s.Error)
			continue
		}
		other := 0
		for _, v := range s.Submit.Other {
			other += v
		}
		yes := "no"
		if s.Sustainable {
			yes = "yes"
		}
		w("| %s | %.0f | %.1f | %d | %d | %.1f / %.1f / %.1f | %.1f | %.0f / %.0f | %d | %.1f | %s |", s.Label, s.Rate, s.Submit.AcceptRatePerS, s.Submit.QueueFull, other,
			s.Submit.Latency.P50, s.Submit.Latency.P95, s.Submit.Latency.P99, s.CompletedDuringLoad, s.QueueWaitMS.P50, s.QueueWaitMS.P95, s.PeakQueued, s.DrainS, yes)
	}
	w("")
	for _, s := range r.Stages {
		if s.Error != "" {
			continue
		}
		w("### %s", s.Label)
		w("")
		for _, y := range s.SustainWhy {
			w("- %s", y)
		}
		if s.GeneratorVsForgeNote != "" {
			w("- NOTE: %s", s.GeneratorVsForgeNote)
		}
		keys := make([]string, 0, len(s.ForgeCounters))
		for k := range s.ForgeCounters {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%.0f", k, s.ForgeCounters[k]))
		}
		w("- Forge counter changes during the stage: %s", strings.Join(parts, ", "))
		w("- Estimated attempt duration (claim to recorded outcome): p50 %.0f ms, p95 %.0f ms, p99 %.0f ms over %.0f attempts", s.AttemptMS.P50, s.AttemptMS.P95, s.AttemptMS.P99, s.AttemptMS.N)
		w("")
	}
	w("## Limits of this measurement")
	w("")
	w("- The classification rule is the E-1 rule, but queue wait is a bucket estimate here, not a database timestamp difference.")
	w("- Only Forge's own view and the generator's view are used; a lost response or a second client would show up as a mismatch note above, not be corrected.")
	w("- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.")
	return b.String()
}
