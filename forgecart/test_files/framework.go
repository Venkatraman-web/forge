package main

// The trial framework shared by all scenarios: rebuild the sandbox state, run a
// measured load, wait for the background pipeline to drain, then verify. Results
// are plain records; the report is generated from them and nothing else.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// RunRecord is the complete measurement of one trial.
type RunRecord struct {
	Label      string                  `json:"label"`
	Config     SandboxConfig           `json:"sandbox_config"`
	Initial    int                     `json:"initial_inventory_per_product"`
	Load       *LoadResult             `json:"load"`
	Drain      *DrainResult            `json:"drain,omitempty"`
	Pipeline   Pipeline                `json:"pipeline_counts"`
	Orders     map[string]int          `json:"orders_by_status"`
	Forge      ForgeJobSummary         `json:"forge_jobs"`
	Workers    []WorkerLogStats        `json:"worker_logs"`
	Outbox     LatencyStats            `json:"outbox_publication_latency"`
	Effects    map[string]LatencyStats `json:"effect_latency_after_payment"`
	Span       *PipelineSpan           `json:"pipeline_span,omitempty"`
	EffectRate map[string]float64      `json:"effects_per_s_over_span,omitempty"`
	Resources  ResourceSummary         `json:"resources"`
	Checks     []Check                 `json:"integrity_checks"`
	VerifyS    float64                 `json:"verification_duration_s"`
	Extra      map[string]any          `json:"extra,omitempty"`
	Error      string                  `json:"error,omitempty"`
}

// PipelineSpan covers the first outbox event to the last business effect.
type PipelineSpan struct {
	Definition   string    `json:"definition"`
	FirstEventAt time.Time `json:"first_outbox_event_at"`
	LastEffectAt time.Time `json:"last_effect_at"`
	Seconds      float64   `json:"seconds"`
	JobsPerSec   *float64  `json:"jobs_completed_per_s"`
}

type CriterionResult struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Kind        string `json:"kind"`   // correctness | performance
	Status      string `json:"status"` // pass | fail | unspecified
	Detail      string `json:"detail"`
}

// ScenarioResult is one scenario's record.
type ScenarioResult struct {
	Name        string            `json:"name"`
	Title       string            `json:"title"`
	Kind        string            `json:"kind"`
	Description string            `json:"description"`
	Status      string            `json:"status"` // passed | failed | inconclusive | error | skipped | interrupted
	Reasons     []string          `json:"reasons,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	EndedAt     time.Time         `json:"ended_at"`
	DurationS   float64           `json:"duration_s"`
	Params      Params            `json:"parameters"`
	Objectives  Objectives        `json:"objectives"`
	Notes       []string          `json:"notes,omitempty"`
	Criteria    []CriterionResult `json:"acceptance_criteria"`
	Runs        []*RunRecord      `json:"runs"`
	Tables      []Table           `json:"tables,omitempty"`
	Findings    []string          `json:"findings,omitempty"`
}

// Table is a small result table a scenario adds for the report.
type Table struct {
	Title string     `json:"title"`
	Head  []string   `json:"head"`
	Rows  [][]string `json:"rows"`
}

// Env is the execution environment handed to scenarios.
type Env struct {
	Ctx     context.Context
	SB      *Sandbox
	Probe   *Probe
	Opts    Options
	Repo    string
	RunDir  string
	CPUs    int
	Logf    func(format string, a ...any)
	Timeout time.Duration // per-request HTTP timeout
}

func NewEnv(ctx context.Context, sb *Sandbox, opts Options, repo, runDir string, logf func(string, ...any)) *Env {
	return &Env{Ctx: ctx, SB: sb, Opts: opts, Repo: repo, RunDir: runDir, CPUs: runtime.NumCPU(), Logf: logf, Timeout: 10 * time.Second}
}

// TrialSpec describes one measured trial.
type TrialSpec struct {
	Label        string
	Cfg          SandboxConfig
	Phases       []Phase
	Seed         int64
	Tag          string
	Drain        bool
	DrainTimeout time.Duration
	// HotProducts, when above zero, limits the shoppers to the first N products by SKU, so many
	// shoppers contend for the same few stock rows.
	HotProducts int
	// Hook runs concurrently with the load (fault injection, pauses). It must
	// return when ctx is cancelled.
	Hook func(ctx context.Context, e *Env, loadStart time.Time)
	// BeforeDrain runs after the load ends and before waiting for the drain.
	BeforeDrain func(ctx context.Context, e *Env)
	// SkipObserve disables Forge job observation.
	SkipObserve bool
	SampleEvery time.Duration
}

func (e *Env) mixCfg(base SandboxConfig, m *Mix, maxDelay string) SandboxConfig {
	c := base
	if m != nil {
		c.PaymentSuccess, c.PaymentDecline, c.PaymentTemp = m.Success, m.Decline, m.Temp
	}
	if maxDelay != "" {
		c.PaymentMaxDelay = maxDelay
	}
	return c
}

func (e *Env) newProbe() error {
	if e.Probe != nil {
		e.Probe.Close()
		e.Probe = nil
	}
	p, err := NewProbe(e.SB.AppDBURL(), e.SB.AppDBServerURL(), e.SB.ResultsDBURL(), e.SB.PaymentsDBURL())
	if err != nil {
		return err
	}
	e.Probe = p
	return nil
}

// Setup rebuilds both databases, starts the services and discovers the catalog.
func (e *Env) Setup(label string, cfg SandboxConfig) ([]product, error) {
	e.Logf("  [%s] rebuilding databases and starting services", label)
	if err := e.SB.ResetState(e.Ctx, cfg); err != nil {
		return nil, fmt.Errorf("sandbox setup failed: %v", err)
	}
	if err := e.newProbe(); err != nil {
		return nil, fmt.Errorf("database probe failed: %v", err)
	}
	cl := NewClient(e.SB.CartURL(), e.Timeout, 2)
	defer cl.Close()
	products, err := cl.DiscoverProducts(e.Ctx)
	if err != nil {
		return nil, fmt.Errorf("product discovery failed: %v", err)
	}
	return products, nil
}

// Trial rebuilds the sandbox state with ts.Cfg, runs the load and collects everything.
func (e *Env) Trial(ts TrialSpec) *RunRecord {
	rec := &RunRecord{Label: ts.Label, Config: ts.Cfg, Initial: ts.Cfg.Inventory, Extra: map[string]any{}}
	rec.Extra["host_loadavg_at_trial_start"] = readLoadAvg()
	fail := func(format string, a ...any) *RunRecord { rec.Error = fmt.Sprintf(format, a...); return rec }
	products, err := e.Setup(ts.Label, ts.Cfg)
	if err != nil {
		return fail("%v", err)
	}
	if ts.HotProducts > 0 && ts.HotProducts < len(products) {
		products = products[:ts.HotProducts]
		e.Logf("  [%s] every shopper orders only %d product(s): %s", ts.Label, len(products), products[0].SKU)
	}
	var obs *ForgeObserver
	if !ts.SkipObserve {
		obs = NewForgeObserver(e.SB.ForgeURL(), e.SB.apiKey, time.Duration(max(e.Opts.ObserveEvery, 50))*time.Millisecond)
		obs.SkipPreexisting = e.SB.attach != nil
		obs.Start(e.Ctx)
	}
	every := ts.SampleEvery
	if every == 0 {
		every = time.Second
	}
	smp := StartSampler(e.Ctx, e.SB, e.Probe, every)

	hookCtx, hookCancel := context.WithCancel(e.Ctx)
	hookDone := make(chan struct{})
	loadStart := time.Now()
	if ts.Hook != nil {
		go func() { defer close(hookDone); ts.Hook(hookCtx, e, loadStart) }()
	} else {
		close(hookDone)
	}
	e.Logf("  [%s] load running", ts.Label)
	rec.Load = RunLoad(e.Ctx, e.SB.CartURL(), ts.Label, ts.Tag, ts.Seed, e.Timeout, products, ts.Phases, 0)
	hookCancel()
	<-hookDone
	if ts.BeforeDrain != nil {
		ts.BeforeDrain(e.Ctx, e)
	}
	var last Pipeline
	if ts.Drain {
		e.Logf("  [%s] load finished (%d completed); waiting for the background pipeline to drain", ts.Label, rec.Load.Completed)
		dr, pl := e.Probe.WaitDrain(e.Ctx, ts.DrainTimeout)
		if ts.BeforeDrain != nil {
			dr.Note = "drain time is measured from the moment the scenario's BeforeDrain hook returned (for example after workers were started), not from the end of the HTTP load, to the poll that sees every paid order with all its events published and all its business effects recorded; it is polled every 250 ms"
		}
		rec.Drain, last = &dr, pl
	}
	rec.Resources, _ = smp.Stop(e.CPUs)
	if obs != nil {
		obs.Refresh(e.Ctx, 3000)
		obs.Stop()
	}
	// Verification (read-only, after the measurement window; timed separately).
	v0 := time.Now()
	if pl, err := e.Probe.Counts(e.Ctx); err == nil {
		rec.Pipeline = pl
	} else {
		rec.Pipeline = last
	}
	rec.Orders = e.Probe.OrderStatusCounts(e.Ctx)
	drained := rec.Drain != nil && rec.Drain.Drained
	rec.Checks = e.Probe.Verify(e.Ctx, ts.Cfg.Inventory, drained)
	rec.Outbox = e.Probe.OutboxLatency(e.Ctx)
	rec.Effects = map[string]LatencyStats{}
	for _, w := range WorkerTypes {
		rec.Effects[w.Short] = e.Probe.EffectLatency(e.Ctx, w.Short)
	}
	rec.Span = e.Probe.Span(e.Ctx, rec.Pipeline.EffectsTotal)
	if rec.Span != nil {
		rec.Span.Definition = "effects / (first outbox event -> last effect); includes the HTTP load duration and any wait before workers run, so it is not a worker-capacity or foreground-throughput measure"
	}
	if rec.Span != nil && rec.Span.Seconds > 0 {
		rec.EffectRate = map[string]float64{}
		for short, n := range rec.Pipeline.Effects {
			rec.EffectRate[short] = round3(float64(n) / rec.Span.Seconds)
		}
	}
	rec.VerifyS = round3(time.Since(v0).Seconds())
	if obs != nil {
		per := rec.Pipeline.Paid
		rec.Forge = obs.Summary(per)
	}
	rec.Workers = ParseWorkerLogs(e.SB.LogDir)
	return rec
}

// ---------- criteria ----------

type CritSet struct{ list []CriterionResult }

func (c *CritSet) Correct(id, desc string, pass bool, detail string) {
	st := "pass"
	if !pass {
		st = "fail"
	}
	c.list = append(c.list, CriterionResult{ID: id, Description: desc, Kind: "correctness", Status: st, Detail: detail})
}

// Perf records a performance objective: unspecified when the owner set none.
func (c *CritSet) Perf(id, desc string, objective *float64, measured *float64, lowerIsBetter bool, unit string) {
	cr := CriterionResult{ID: id, Description: desc, Kind: "performance"}
	switch {
	case measured == nil:
		cr.Status, cr.Detail = "unspecified", "not measured"
	case objective == nil:
		cr.Status, cr.Detail = "unspecified", fmt.Sprintf("objective not specified by the owner; measured %.2f %s", *measured, unit)
	case lowerIsBetter == (*measured <= *objective):
		cr.Status, cr.Detail = "pass", fmt.Sprintf("measured %.2f %s against objective %.2f", *measured, unit, *objective)
	default:
		cr.Status, cr.Detail = "fail", fmt.Sprintf("measured %.2f %s against objective %.2f", *measured, unit, *objective)
	}
	c.list = append(c.list, cr)
}

func (c *CritSet) Failed() []string {
	var f []string
	for _, x := range c.list {
		if x.Status == "fail" {
			f = append(f, x.ID+": "+x.Description+" ("+x.Detail+")")
		}
	}
	return f
}

// pipelineCriteria are the correctness criteria shared by every trial that drains.
// allowStockConflicts means stock conflicts are business outcomes, not failures.
func pipelineCriteria(c *CritSet, prefix string, r *RunRecord, expectDrain bool) {
	l := r.Load
	c.Correct(prefix+"http", "no unexpected HTTP failures (unexpected responses, transport failures, timeouts) and no failed workflows",
		l.Failed == 0 && l.Interrupted == 0 && (l.ErrorRatePct == nil || *l.ErrorRatePct == 0),
		fmt.Sprintf("failed workflows %d, failure rate %s", l.Failed, pctStr(l.ErrorRatePct)))
	c.Correct(prefix+"accounting", "every started workflow ended as completed, rejected, failed or interrupted",
		l.Started == l.Completed+l.RejectedStock+l.RejectedOther+l.Failed+l.Interrupted, fmt.Sprintf("started %d", l.Started))
	c.Correct(prefix+"paid", "paid orders in the database equal successful payment responses",
		r.Pipeline.Paid == l.Payment["succeeded"], fmt.Sprintf("database %d, responses %d", r.Pipeline.Paid, l.Payment["succeeded"]))
	if expectDrain {
		d := r.Drain
		c.Correct(prefix+"drain", "every paid order's events were published and its effects recorded within the drain timeout",
			d != nil && d.Drained, drainStr(d))
	}
	bad := 0
	var first string
	for _, ck := range r.Checks {
		if !ck.OK {
			bad++
			if first == "" {
				first = ck.Name + " — " + ck.Detail
			}
		}
	}
	c.Correct(prefix+"integrity", fmt.Sprintf("all %d database integrity checks pass", len(r.Checks)), bad == 0, fmt.Sprintf("%d failed %s", bad, first))
	failedJobs := 0
	for _, t := range r.Forge.Types {
		failedJobs += t.Failed
	}
	wf := 0
	for _, w := range r.Workers {
		wf += w.FailuresRec
	}
	c.Correct(prefix+"jobs", "no observed Forge job ended failed, no outbox event failed, no worker recorded a failed attempt",
		failedJobs == 0 && r.Pipeline.OutboxFailed == 0 && wf == 0, fmt.Sprintf("failed jobs seen %d, outbox failed %d, worker failures %d", failedJobs, r.Pipeline.OutboxFailed, wf))
}

func pctStr(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%%", *p)
}

func drainStr(d *DrainResult) string {
	if d == nil {
		return "not waited"
	}
	return fmt.Sprintf("drained=%v after %.1f s; effects %d of %d, unpublished events %d", d.Drained, d.WaitedS, d.FinalEffects, d.ExpectedJobs, d.FinalPending)
}

// finish derives the scenario status from its criteria.
func (r *ScenarioResult) finish(c *CritSet, inconclusive []string) {
	r.Criteria = c.list
	fails := c.Failed()
	switch {
	case len(fails) > 0:
		r.Status, r.Reasons = "failed", append(r.Reasons, fails...)
	case len(inconclusive) > 0:
		r.Status, r.Reasons = "inconclusive", append(r.Reasons, inconclusive...)
	default:
		r.Status = "passed"
	}
}

// helpers used by several scenarios
func fp(v float64) *float64 { return &v }

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// readLoadAvg returns the 1/5/15-minute load averages, or an explicit "unavailable".
func readLoadAvg() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unavailable (cannot read /proc/loadavg)"
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return "unavailable (unexpected /proc/loadavg format)"
	}
	return strings.Join(f[:3], " ")
}
