package main

// The twelve scenarios. Each is a function from a resolved definition to a
// ScenarioResult. They share Env.Trial, the criteria helpers and the report tables.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type scenarioFn func(e *Env, d ScenarioDef, res *ScenarioResult)

var runners = map[string]scenarioFn{
	"smoke": runSmoke, "baseline": runBaseline, "ramp": runRamp, "burst": runBurst, "soak": runSoak, "saturation": runSaturation,
	"stock": runStock, "paymix": runPaymix, "idempotency": runIdempotency, "pipeline": runPipeline, "scaling": runScaling, "recovery": runRecovery,
}

func (e *Env) cfgFor(p Params) SandboxConfig {
	c := DefaultSandboxConfig()
	if p.Inventory > 0 {
		c.Inventory = p.Inventory
	}
	return e.mixCfg(c, p.Mix, p.PaymentMaxDelay)
}

func drainTimeout(p Params) time.Duration {
	if p.DrainTimeoutS <= 0 {
		return 120 * time.Second
	}
	return time.Duration(p.DrainTimeoutS) * time.Second
}

func trialErr(res *ScenarioResult, c *CritSet, r *RunRecord) bool {
	res.Runs = append(res.Runs, r)
	if r.Error != "" {
		c.Correct("setup-"+r.Label, "the trial could be set up and executed", false, r.Error)
		return true
	}
	return false
}

// pipelineWfSecs returns the order/payment/workflow p95 of a trial for tables.
func stageOf(l *LoadResult, name string) LatencyStats {
	for _, s := range l.Stages {
		if s.Name == name {
			return s.Latency
		}
	}
	return LatencyStats{}
}

func finishWith(res *ScenarioResult, c *CritSet, e *Env, extra ...string) {
	var inc []string
	for _, r := range res.Runs {
		if r.Load != nil && r.Load.GeneratorSuspect {
			inc = append(inc, fmt.Sprintf("trial %s: the load generator may have limited the run (%s)", r.Label, strings.TrimSpace(r.Load.GeneratorNote)))
		}
	}
	res.finish(c, append(inc, extra...))
}

// ---------- A. smoke ----------

func runSmoke(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	r := e.Trial(TrialSpec{Label: "smoke", Cfg: e.cfgFor(p), Seed: p.Seed, Tag: "smoke", Drain: true, DrainTimeout: drainTimeout(p),
		Phases: []Phase{{Name: "smoke", Concurrency: p.Concurrency, Workflows: p.Workflows}}})
	if trialErr(res, c, r) {
		res.finish(c, nil)
		return
	}
	pipelineCriteria(c, "", r, true)
	c.Correct("all-orders", "every smoke workflow completed (success-only payment profile)", r.Load.Completed == p.Workflows && r.Pipeline.Paid == p.Workflows, fmt.Sprintf("completed %d of %d, paid %d", r.Load.Completed, p.Workflows, r.Pipeline.Paid))
	for _, w := range WorkerTypes {
		var ws WorkerLogStats
		for _, x := range r.Workers {
			if x.Short == w.Short {
				ws = x
			}
		}
		var ts TypeJobStats
		for _, x := range r.Forge.Types {
			if x.Type == w.Type {
				ts = x
			}
		}
		c.Correct("type-"+w.Short, fmt.Sprintf("%s: effect row, worker execution and Forge job exist for every order", w.Type),
			r.Pipeline.Effects[w.Short] == p.Workflows && ws.Completed == p.Workflows && ts.Succeeded == p.Workflows,
			fmt.Sprintf("effects %d, worker completions %d, Forge jobs succeeded %d (expected %d)", r.Pipeline.Effects[w.Short], ws.Completed, ts.Succeeded, p.Workflows))
	}
	res.Tables = append(res.Tables, Table{Title: "Smoke result", Head: trialHead, Rows: [][]string{trialRow(r)}})
	finishWith(res, c, e)
}

// ---------- B. baseline ----------

func runBaseline(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	var tput, p95s []float64
	for i := 1; i <= max(1, p.Repetitions); i++ {
		if e.Ctx.Err() != nil {
			break
		}
		r := e.Trial(TrialSpec{Label: fmt.Sprintf("rep%d", i), Cfg: e.cfgFor(p), Seed: p.Seed + int64(i), Tag: fmt.Sprintf("b%d", i), Drain: true, DrainTimeout: drainTimeout(p),
			Phases: []Phase{{Name: "baseline", Concurrency: p.Concurrency, Workflows: p.Workflows}}})
		if trialErr(res, c, r) {
			continue
		}
		pipelineCriteria(c, fmt.Sprintf("rep%d-", i), r, true)
		tput = append(tput, deref(r.Load.WfPerSec))
		p95s = append(p95s, deref(r.Load.WorkflowLat.P95MS))
	}
	tables, findings := deriveBaseline(res)
	res.Tables = append(res.Tables, tables...)
	res.Findings = append(res.Findings, findings...)
	o := d.Objectives
	if len(p95s) > 0 {
		c.Perf("p95", "workflow p95 latency (median of repetitions)", o.P95MS, fp(median(p95s)), true, "ms")
		c.Perf("throughput", "completed workflows per second (median of repetitions)", o.MinWfPerSec, fp(median(tput)), false, "wf/s")
	}
	finishWith(res, c, e)
}

func minmax(v []float64) (float64, float64) {
	lo, hi := v[0], v[0]
	for _, x := range v {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	return lo, hi
}

// ---------- C. ramp ----------

func runRamp(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	stopped := ""
	stages := 0
	for _, conc := range p.Concurrencies {
		if e.Ctx.Err() != nil {
			break
		}
		r := e.Trial(TrialSpec{Label: fmt.Sprintf("c%d", conc), Cfg: e.cfgFor(p), Seed: p.Seed, Tag: fmt.Sprintf("r%d", conc), Drain: true, DrainTimeout: drainTimeout(p),
			Phases: []Phase{{Name: fmt.Sprintf("c=%d", conc), Concurrency: conc, Workflows: p.StageWorkflows}}})
		if trialErr(res, c, r) {
			stopped = fmt.Sprintf("stopped at concurrency %d: the stage could not run (%s)", conc, r.Error)
			break
		}
		stages++
		pipelineCriteria(c, fmt.Sprintf("c%d-", conc), r, true)
		p95 := deref(r.Load.WorkflowLat.P95MS)
		if r.Load.ErrorRatePct != nil && *r.Load.ErrorRatePct > p.MaxErrorRatePct {
			stopped = fmt.Sprintf("stopped after concurrency %d: HTTP failure rate %.2f%% exceeded the %.1f%% stop threshold", conc, *r.Load.ErrorRatePct, p.MaxErrorRatePct)
		} else if p.StopP95MS > 0 && p95 > p.StopP95MS {
			stopped = fmt.Sprintf("stopped after concurrency %d: workflow p95 %.0f ms exceeded the %.0f ms stop threshold", conc, p95, p.StopP95MS)
		} else if r.Load.GeneratorSuspect {
			stopped = fmt.Sprintf("stopped after concurrency %d: the load generator may be the bottleneck", conc)
		}
		if stopped != "" {
			break
		}
	}
	tb, findings := deriveRamp(res)
	res.Tables = append(res.Tables, tb)
	res.Findings = append(res.Findings, findings...)
	if stopped != "" {
		res.Notes = append(res.Notes, stopped)
	}
	if len(p.Concurrencies) > stages && stopped == "" && e.Ctx.Err() == nil {
		res.Notes = append(res.Notes, "Some configured stages were not executed.")
	}
	finishWith(res, c, e)
}

// ---------- D. burst ----------

func runBurst(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	r := e.Trial(TrialSpec{Label: "burst", Cfg: e.cfgFor(p), Seed: p.Seed, Tag: "burst", Drain: true, DrainTimeout: drainTimeout(p),
		Phases: []Phase{
			{Name: "normal-before", Concurrency: p.BaseConcurrency, Duration: time.Duration(p.BaseSeconds) * time.Second},
			{Name: "burst", Concurrency: p.BurstConcurrency, Duration: time.Duration(p.BurstSeconds) * time.Second},
			{Name: "normal-after", Concurrency: p.BaseConcurrency, Duration: time.Duration(p.RecoverySeconds) * time.Second}}})
	if trialErr(res, c, r) {
		res.finish(c, nil)
		return
	}
	pipelineCriteria(c, "", r, true)
	tb := Table{Title: "Burst phases", Head: []string{"Phase", "Concurrency", "Window", "Completed", "Workflows/s", "Workflow p50", "Workflow p95", "Workflow p99", "Requests", "Errors"}}
	for _, ph := range r.Load.Phases {
		tb.Rows = append(tb.Rows, []string{ph.Name, itoa(ph.Concurrency), fmt.Sprintf("%.0f-%.0f s", ph.StartS, ph.EndS), itoa(ph.Completed), fmtF(ph.CompletedPerSec),
			fmtMS(ph.WorkflowLatency.P50MS), fmtMS(ph.WorkflowLatency.P95MS), fmtMS(ph.WorkflowLatency.P99MS), itoa(ph.Requests), itoa(ph.Errors)})
	}
	res.Tables = append(res.Tables, tb)
	// Backlog analysis from the 1 s samples (sampler time is within about 1 s of load time).
	series := r.Resources.Series
	if len(r.Load.PhaseMarks) == 3 && len(series) > 0 {
		pre := r.Load.PhaseMarks[0]
		burst := r.Load.PhaseMarks[1]
		var preVals []float64
		maxBurst := 0
		for _, s := range series {
			if s.T >= pre.EndS-5 && s.T <= pre.EndS {
				preVals = append(preVals, float64(s.Backlog))
			}
			if s.T >= burst.StartS && s.T <= burst.EndS+2 && s.Backlog > maxBurst {
				maxBurst = s.Backlog
			}
		}
		preLevel := median(preVals)
		recovery := "the backlog did not return to its pre-burst level before the load ended; see the drain time"
		target := preLevel*1.1 + 10
		for _, s := range series {
			if s.T > burst.EndS && float64(s.Backlog) <= target {
				recovery = fmt.Sprintf("%.0f s after the burst ended", s.T-burst.EndS)
				break
			}
		}
		res.Tables = append(res.Tables, Table{Title: "Backlog (jobs created but not yet completed)", Head: []string{"Pre-burst level (median of last 5 s)", "Peak during burst", "Peak overall", "Back to pre-burst level (+10%+10)"},
			Rows: [][]string{{fmt.Sprintf("%.0f", preLevel), itoa(maxBurst), itoa(r.Resources.MaxBacklog), recovery}}})
		res.Findings = append(res.Findings, fmt.Sprintf("The burst raised the background backlog from about %.0f to a peak of %d jobs; return to the pre-burst level: %s.", preLevel, r.Resources.MaxBacklog, recovery))
	}
	finishWith(res, c, e)
}

// ---------- E. soak ----------

func runSoak(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	r := e.Trial(TrialSpec{Label: "soak", Cfg: e.cfgFor(p), Seed: p.Seed, Tag: "soak", Drain: true, DrainTimeout: drainTimeout(p), SampleEvery: time.Second,
		Phases: []Phase{{Name: "soak", Concurrency: p.Concurrency, Duration: time.Duration(p.DurationSeconds) * time.Second}}})
	if trialErr(res, c, r) {
		res.finish(c, nil)
		return
	}
	pipelineCriteria(c, "", r, true)
	tb, findings := deriveSoak(res)
	res.Tables = append(res.Tables, tb)
	res.Findings = append(res.Findings, findings...)
	for _, ps := range r.Resources.Procs {
		if ps.RSSEndMB > ps.RSSStartMB*1.5 && ps.RSSEndMB-ps.RSSStartMB > 50 {
			res.Findings = append(res.Findings, fmt.Sprintf("Process %s grew from %.0f to %.0f MB RSS during the run; whether this is a leak or warm-up needs a longer soak.", ps.Name, ps.RSSStartMB, ps.RSSEndMB))
		}
	}
	res.Findings = append(res.Findings, fmt.Sprintf("Database connections peaked at %d (ForgeCart DB) and %d (Forge DB) of %d allowed; maximum backlog %d jobs; worker retries %d.",
		r.Resources.MaxConnsApp, r.Resources.MaxConnsForge, r.Resources.ServerMaxConns, r.Resources.MaxBacklog, totalRetries(r)))
	finishWith(res, c, e)
}

func pctChange(a, b float64) float64 {
	if a == 0 {
		return 0
	}
	return (b - a) / a * 100
}

func totalRetries(r *RunRecord) int {
	n := 0
	for _, w := range r.Workers {
		n += w.Retries
	}
	return n
}

// ---------- F. saturation ----------

func runSaturation(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	var concs []int
	for cc := float64(p.StartConcurrency); int(cc+0.5) <= p.MaxConcurrency; cc *= p.Factor {
		v := int(cc + 0.5)
		if len(concs) == 0 || v != concs[len(concs)-1] {
			concs = append(concs, v)
		}
		if p.Factor <= 1 {
			break
		}
	}
	var st satState
	inconclusive := []string{}
	for _, conc := range concs {
		if e.Ctx.Err() != nil {
			break
		}
		r := e.Trial(TrialSpec{Label: fmt.Sprintf("c%d", conc), Cfg: e.cfgFor(p), Seed: p.Seed, Tag: fmt.Sprintf("s%d", conc), Drain: true, DrainTimeout: drainTimeout(p),
			Phases: []Phase{{Name: fmt.Sprintf("c=%d", conc), Concurrency: conc, Duration: time.Duration(p.StageSeconds) * time.Second}}})
		if trialErr(res, c, r) {
			break
		}
		pipelineCriteria(c, fmt.Sprintf("c%d-", conc), r, true)
		stop, plateau := satReasons(p, r, &st)
		if r.Load.GeneratorSuspect {
			inconclusive = append(inconclusive, fmt.Sprintf("stage c=%d: the load generator may have limited the run", conc))
		}
		if len(stop) > 0 || plateau != "" || r.Load.GeneratorSuspect {
			break
		}
	}
	sum := deriveSaturation(res)
	res.Tables = append(res.Tables, sum.Table)
	res.Findings = append(res.Findings, sum.Findings...)
	if !sum.HasStable {
		inconclusive = append(inconclusive, "no stage met the front-end stability criteria, so no stable operating point was demonstrated")
	}
	if !sum.StopFound {
		inconclusive = append(inconclusive, "the saturation point was not reached within the configured maximum concurrency")
	}
	finishWith(res, c, e, inconclusive...)
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func drainShort(d *DrainResult) string {
	if d == nil {
		return "n/a"
	}
	if !d.Drained {
		return fmt.Sprintf("NOT drained in %.0f s", d.WaitedS)
	}
	return fmt.Sprintf("%.1f s", d.WaitedS)
}

// ---------- G. stock contention ----------

func runStock(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	r := e.Trial(TrialSpec{Label: "stock", Cfg: e.cfgFor(p), Seed: p.Seed, Tag: "stk", Drain: true, DrainTimeout: drainTimeout(p),
		Phases: []Phase{{Name: "contention", Concurrency: p.Concurrency, Workflows: p.Workflows}}})
	if trialErr(res, c, r) {
		res.finish(c, nil)
		return
	}
	l := r.Load
	pipelineCriteria(c, "", r, true)
	c.Correct("conflicts", "stock conflicts occurred (the scenario really contended) and were classified as business rejections, not failures",
		l.RejectedStock > 0 && l.Failed == 0, fmt.Sprintf("%d stock conflicts, %d failed workflows", l.RejectedStock, l.Failed))
	c.Correct("accounting-stock", "created orders + stock conflicts = workflows", l.OrderOutcomes["created"]+l.RejectedStock == p.Workflows,
		fmt.Sprintf("created %d + conflicts %d vs %d", l.OrderOutcomes["created"], l.RejectedStock, p.Workflows))
	stock := e.Probe.StockByProduct(e.Ctx)
	left := 0
	var skus []string
	for s, n := range stock {
		left += n
		skus = append(skus, s)
	}
	sort.Strings(skus)
	total := p.Inventory * len(stock)
	c.Correct("no-oversell", "units reserved never exceed the initial stock (no negative stock, ledger balanced)", left >= 0 && total-left <= total, fmt.Sprintf("initial %d units, remaining %d, reserved/committed %d", total, left, total-left))
	dl, ok := e.Probe.Deadlocks(e.Ctx)
	dlS := "unavailable"
	if ok {
		dlS = itoa(dl)
	}
	rows := [][]string{}
	for _, s := range skus {
		rows = append(rows, []string{s, itoa(p.Inventory), itoa(stock[s]), itoa(p.Inventory - stock[s])})
	}
	res.Tables = append(res.Tables, Table{Title: "Inventory after the run", Head: []string{"SKU", "Initial", "Remaining", "Reserved"}, Rows: rows})
	res.Tables = append(res.Tables, Table{Title: "Contention result", Head: trialHead, Rows: [][]string{trialRow(r)}})
	res.Findings = append(res.Findings, fmt.Sprintf("Order creation p50/p95/p99 under contention: %s / %s / %s ms; PostgreSQL deadlocks in the ForgeCart database: %s. Lock wait time per statement is not measured separately (it is included in the order-creation latency).",
		fmtMS(stageOf(l, "order_create").P50MS), fmtMS(stageOf(l, "order_create").P95MS), fmtMS(stageOf(l, "order_create").P99MS), dlS))
	finishWith(res, c, e)
}

// ---------- H. payment mix ----------

func runPaymix(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	tb := Table{Title: "Configured vs observed payment outcomes", Head: []string{"Profile", "Configured (s/d/t %)", "Completed workflows", "Succeeded", "Declined", "Temporary failure", "Observed (s/d/t %)", "paid orders", "payment_failed orders", "pending orders"}}
	for i, pr := range p.Profiles {
		mix := pr.Mix
		cfg := e.mixCfg(e.cfgFor(p), &mix, "")
		r := e.Trial(TrialSpec{Label: pr.Name, Cfg: cfg, Seed: p.Seed + int64(i), Tag: fmt.Sprintf("pm%d", i), Drain: true, DrainTimeout: drainTimeout(p),
			Phases: []Phase{{Name: pr.Name, Concurrency: p.Concurrency, Workflows: p.Workflows}}})
		if trialErr(res, c, r) {
			continue
		}
		pipelineCriteria(c, pr.Name+"-", r, true)
		l := r.Load
		s, dc, t := l.Payment["succeeded"], l.Payment["declined"], l.Payment["temporary_failure"]
		n := s + dc + t
		c.Correct(pr.Name+"-sum", "payment outcomes account for every completed workflow", n == l.Completed, fmt.Sprintf("%d outcomes vs %d completed", n, l.Completed))
		c.Correct(pr.Name+"-states", "declined payments left payment_failed orders, temporary failures left pending orders, successes left paid orders",
			r.Orders["payment_failed"] == dc && r.Orders["pending_payment"] == t && r.Orders["paid"] == s,
			fmt.Sprintf("payment_failed %d/%d, pending %d/%d, paid %d/%d", r.Orders["payment_failed"], dc, r.Orders["pending_payment"], t, r.Orders["paid"], s))
		c.Correct(pr.Name+"-events", "only successful payments created an outbox event (exactly one per paid order)", r.Pipeline.Events == s, fmt.Sprintf("%d events for %d successes", r.Pipeline.Events, s))
		pc := func(x int) string {
			if n == 0 {
				return "n/a"
			}
			return fmt.Sprintf("%.1f", float64(x)/float64(n)*100)
		}
		tb.Rows = append(tb.Rows, []string{pr.Name, fmt.Sprintf("%d/%d/%d", mix.Success, mix.Decline, mix.Temp), itoa(l.Completed), itoa(s), itoa(dc), itoa(t),
			pc(s) + "/" + pc(dc) + "/" + pc(t), itoa(r.Orders["paid"]), itoa(r.Orders["payment_failed"]), itoa(r.Orders["pending_payment"])})
	}
	res.Tables = append(res.Tables, tb)
	res.Notes = append(res.Notes, "The ForgeCart API process is restarted for every profile because the simulator reads its configuration at startup; the databases are rebuilt as well.")
	finishWith(res, c, e)
}

// ---------- J. pipeline ----------

func runPipeline(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	r := e.Trial(TrialSpec{Label: "pipeline", Cfg: e.cfgFor(p), Seed: p.Seed, Tag: "pl", Drain: true, DrainTimeout: drainTimeout(p),
		Phases: []Phase{{Name: "pipeline", Concurrency: p.Concurrency, Workflows: p.Workflows}}})
	if trialErr(res, c, r) {
		res.finish(c, nil)
		return
	}
	pipelineCriteria(c, "", r, true)
	paid := r.Pipeline.Paid
	for _, t := range r.Forge.Types {
		c.Correct("seen-"+t.Type, fmt.Sprintf("%s: every job was observed (jobs seen through Forge's public API)", t.Type), t.Observed >= paid*9/10, fmt.Sprintf("observed %d of %d (a 90%% floor is required for the timing statistics to be meaningful)", t.Observed, paid))
	}
	if r.Drain != nil && r.Drain.Drained {
		c.Perf("drain", "drain time after the load", d.Objectives.MaxDrainSeconds, fp(r.Drain.WaitedS), true, "s")
	}
	res.Tables = append(res.Tables, Table{Title: "Pipeline run", Head: trialHead, Rows: [][]string{trialRow(r)}})
	res.Findings = append(res.Findings, pipelineFindings(r)...)
	finishWith(res, c, e)
}

func pipelineFindings(r *RunRecord) []string {
	var f []string
	f = append(f, fmt.Sprintf("Outbox publication (event created in the payment transaction -> accepted by Forge): p50 %s ms, p95 %s ms, p99 %s ms over %d events.", fmtMS(r.Outbox.P50MS), fmtMS(r.Outbox.P95MS), fmtMS(r.Outbox.P99MS), r.Outbox.Count))
	if r.Span != nil {
		f = append(f, fmt.Sprintf("Pipeline span rate (first outbox event -> last effect, includes the HTTP load): %.1f s for %d jobs = %s jobs/s.", r.Span.Seconds, r.Pipeline.EffectsTotal, fmtF(r.Span.JobsPerSec)))
		if r.Drain != nil && drainRate(r.Drain) > 0 {
			f = append(f, fmt.Sprintf("Background drain rate (5 x paid orders / drain seconds, after the load ended): %.1f jobs/s over %.1f s.", drainRate(r.Drain), r.Drain.WaitedS))
		}
	}
	worst, wv := "", 0.0
	for _, t := range r.Forge.Types {
		if v := deref(t.QueueWait.P95MS); v > wv {
			worst, wv = t.Type, v
		}
	}
	if worst != "" {
		f = append(f, fmt.Sprintf("Longest Forge queue wait (p95): %s at %.0f ms. Forge timestamps are used, so this is the time between a job being created in Forge and a worker starting it.", worst, wv))
	}
	return f
}

// ---------- K. worker scaling ----------

func runScaling(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	for _, sc := range p.Scaling {
		if e.Ctx.Err() != nil {
			break
		}
		cfg := e.cfgFor(p)
		cfg.StartWorkers = false
		cfg.WorkerReplicas, cfg.WorkerConcurrency = map[string]int{}, map[string]int{}
		for _, w := range WorkerTypes {
			cfg.WorkerReplicas[w.Short], cfg.WorkerConcurrency[w.Short] = sc.Replicas, sc.Concurrency
		}
		var extra = map[string]any{}
		r := e.Trial(TrialSpec{Label: sc.Name, Cfg: cfg, Seed: p.Seed, Tag: "sc" + sc.Name, Drain: true, DrainTimeout: drainTimeout(p),
			Phases: []Phase{{Name: sc.Name, Concurrency: p.Concurrency, Workflows: p.Workflows}},
			BeforeDrain: func(ctx context.Context, e *Env) {
				// wait until the dispatcher has handed every event to Forge, then start the workers
				t0 := time.Now()
				for time.Since(t0) < 120*time.Second && ctx.Err() == nil {
					pl, err := e.Probe.Counts(ctx)
					if err == nil && pl.Events > 0 && pl.Published == pl.Events {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
				extra["publish_wait_s"] = round3(time.Since(t0).Seconds())
				pl, _ := e.Probe.Counts(ctx)
				extra["jobs_queued_when_workers_started"] = jobsPerOrder*pl.Paid - pl.EffectsTotal
				extra["effects_done_before_workers_started"] = pl.EffectsTotal
				if err := e.SB.StartWorkers(ctx, cfg); err != nil {
					extra["worker_start_error"] = err.Error()
				}
				// The drain clock starts when this hook returns, i.e. after the workers were started.
				pl2, _ := e.Probe.Counts(ctx)
				extra["effects_done_when_drain_clock_started"] = pl2.EffectsTotal
			}})
		if trialErr(res, c, r) {
			continue
		}
		r.Extra = extra
		r.Extra["pipeline_span_note"] = "the first-event-to-last-effect span includes the HTTP load and the publication wait with workers stopped; it is not a worker-capacity measure"
		pipelineCriteria(c, sc.Name+"-", r, true)
	}
	res.Tables = append(res.Tables, deriveScaling(res))
	res.Notes = append(res.Notes, "Efficiency = (background drain rate gain) / (capacity gain); 100% would be linear scaling. Capacity is worker replicas x slots per replica, per job type. The drain clock starts after the workers were started, so jobs finished while the worker processes were starting are not excluded (see the effects-done field in the scenario JSON). Each configuration is one trial and the drain is polled every 250 ms (about +/-5-9% on a 3-5 s drain).")
	finishWith(res, c, e)
}

// ---------- I. idempotency ----------

type httpTally struct {
	mu     sync.Mutex
	counts map[string]int
}

func (t *httpTally) add(k string) {
	t.mu.Lock()
	if t.counts == nil {
		t.counts = map[string]int{}
	}
	t.counts[k]++
	t.mu.Unlock()
}

func runIdempotency(e *Env, d ScenarioDef, res *ScenarioResult) {
	p := d.Params
	c := &CritSet{}
	rec := &RunRecord{Label: "idempotency", Config: e.cfgFor(p), Initial: e.cfgFor(p).Inventory}
	res.Runs = append(res.Runs, rec)
	products, err := e.Setup("idempotency", rec.Config)
	if err != nil {
		rec.Error = err.Error()
		c.Correct("setup", "the scenario could be set up", false, err.Error())
		res.finish(c, nil)
		return
	}
	cl := NewClient(e.SB.CartURL(), e.Timeout, 64)
	defer cl.Close()
	tally := &httpTally{}
	K, L := p.Duplicates, p.LogicalOrders
	type orderRes struct {
		created, replayed, other              int
		ids                                   map[string]bool
		attempts                              map[string]bool
		payFresh, payReplay, payOther, pay409 int
	}
	results := make([]orderRes, L)
	sem := make(chan struct{}, 12)
	var wg sync.WaitGroup
	t0 := time.Now()
	for i := 0; i < L; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			or := orderRes{ids: map[string]bool{}, attempts: map[string]bool{}}
			key := fmt.Sprintf("idem-%s-%d-order", e.SB.RunID, i)
			body, _ := json.Marshal(Plan(products, p.Seed, "idem", i))
			var mu sync.Mutex
			var iw sync.WaitGroup
			for k := 0; k < K; k++ {
				iw.Add(1)
				go func() {
					defer iw.Done()
					rp := cl.do(e.Ctx, http.MethodPost, "/v1/orders", map[string]string{"Content-Type": "application/json", "Idempotency-Key": key}, body)
					mu.Lock()
					defer mu.Unlock()
					tally.add(fmt.Sprintf("POST /v1/orders -> %d", rp.status))
					var o struct {
						OrderID  string `json:"order_id"`
						Replayed bool   `json:"replayed"`
					}
					if rp.fail == kExpected && json.Unmarshal(rp.body, &o) == nil && orderIDRe.MatchString(o.OrderID) && (rp.status == 201 || rp.status == 200) {
						or.ids[o.OrderID] = true
						if rp.status == 201 && !o.Replayed {
							or.created++
						} else {
							or.replayed++
						}
					} else {
						or.other++
					}
				}()
			}
			iw.Wait()
			if len(or.ids) == 1 {
				var id string
				for k := range or.ids {
					id = k
				}
				pkey := fmt.Sprintf("idem-%s-%d-pay", e.SB.RunID, i)
				for k := 0; k < K; k++ {
					iw.Add(1)
					go func() {
						defer iw.Done()
						rp := cl.do(e.Ctx, http.MethodPost, "/v1/orders/"+id+"/payment-attempts", map[string]string{"Idempotency-Key": pkey}, nil)
						mu.Lock()
						defer mu.Unlock()
						tally.add(fmt.Sprintf("POST payment-attempts -> %d", rp.status))
						var pr struct {
							AttemptID string `json:"payment_attempt_id"`
							Replayed  bool   `json:"replayed"`
						}
						if rp.fail == kExpected && rp.status == 200 && json.Unmarshal(rp.body, &pr) == nil && pr.AttemptID != "" {
							or.attempts[pr.AttemptID] = true
							if pr.Replayed {
								or.payReplay++
							} else {
								or.payFresh++
							}
						} else {
							or.payOther++
							if rp.status == 409 {
								or.pay409++
							}
						}
					}()
				}
				iw.Wait()
			}
			results[i] = or
		}(i)
	}
	wg.Wait()
	httpSecs := time.Since(t0).Seconds()
	oneOrder, oneCreate, onePay, oneFresh, clean := 0, 0, 0, 0, 0
	for _, r := range results {
		if len(r.ids) == 1 {
			oneOrder++
		}
		if r.created == 1 && r.replayed == K-1 {
			oneCreate++
		}
		if len(r.attempts) == 1 {
			onePay++
		}
		if r.payFresh == 1 && r.payReplay == K-1 {
			oneFresh++
		}
		if r.other == 0 && r.payOther == 0 {
			clean++
		}
	}
	c.Correct("orders-one", fmt.Sprintf("each logical order, submitted %d times concurrently, resolved to exactly one order id", K), oneOrder == L, fmt.Sprintf("%d of %d", oneOrder, L))
	c.Correct("orders-201", "exactly one 201 Created and the rest replays per logical order", oneCreate == L, fmt.Sprintf("%d of %d", oneCreate, L))
	c.Correct("payments-one", "each order's concurrent payment submissions resolved to exactly one payment attempt", onePay == L, fmt.Sprintf("%d of %d", onePay, L))
	conflicts := 0
	for _, r := range results {
		conflicts += r.pay409
	}
	if conflicts > 0 {
		res.Findings = append(res.Findings, fmt.Sprintf("%d of %d concurrent duplicate payment submissions received 409 order_not_payable instead of a replay of the recorded attempt. Measured: every order still has exactly one payment attempt (criterion payments-one) and the database integrity checks are reported below. Hypothesis from reading internal/payments/payments.go: a duplicate that passes the first 'attempt already recorded?' lookup, and reads the order only after the first request committed, sees status paid and is rejected without a second lookup. Concurrent duplicates are safe but not always answered as replays; this suite did not change ForgeCart.", conflicts, L*K))
	}
	c.Correct("payments-fresh", "exactly one non-replayed response and the rest replays per payment", oneFresh == L, fmt.Sprintf("%d of %d", oneFresh, L))
	c.Correct("no-unexpected-http", "no unexpected HTTP response or transport failure", clean == L, fmt.Sprintf("%d of %d clean", clean, L))

	dr, pl := e.Probe.WaitDrain(e.Ctx, drainTimeout(p))
	rec.Drain, rec.Pipeline = &dr, pl
	events0 := e.Probe.DistinctEventIDs(e.Ctx)
	c.Correct("orders-db", "the database holds exactly one order per logical order", pl.Orders == L, fmt.Sprintf("%d orders for %d logical orders", pl.Orders, L))
	c.Correct("drain", "the events of every paid order were published and executed", dr.Drained, drainStr(&dr))

	// duplicate Forge job deliveries through the public API
	refs := e.Probe.EventsForPaidOrders(e.Ctx, max(1, p.JobDupEvents/5))
	type sub struct{ id, event string }
	var subs []sub
	hc := &http.Client{Timeout: 10 * time.Second}
	submitted, submitFailed := 0, 0
	for _, ref := range refs {
		in, _ := json.Marshal(map[string]any{"type": ref.Type, "input": map[string]string{"event_id": ref.EventID, "order_id": ref.OrderID, "payment_attempt_id": ref.AttemptID}})
		for k := 0; k < 2; k++ {
			req, _ := e.SB.ForgeRequest(e.Ctx, http.MethodPost, "/v1/jobs", in)
			resp, err := hc.Do(req)
			if err != nil {
				submitFailed++
				continue
			}
			var j apiJob
			ok := resp.StatusCode == 202 && json.NewDecoder(resp.Body).Decode(&j) == nil
			resp.Body.Close()
			tally.add(fmt.Sprintf("POST Forge /v1/jobs -> %d", resp.StatusCode))
			if ok {
				submitted++
				subs = append(subs, sub{j.ID, ref.EventID})
			} else {
				submitFailed++
			}
		}
	}
	dupOK, notDone := 0, 0
	deadline := time.Now().Add(60 * time.Second)
	for _, s := range subs {
		for {
			req, _ := e.SB.ForgeRequest(e.Ctx, http.MethodGet, "/v1/jobs/"+s.id, nil)
			resp, err := hc.Do(req)
			if err == nil {
				var j struct {
					Status string `json:"status"`
					Result struct {
						Duplicate bool `json:"duplicate"`
					} `json:"result"`
				}
				ok := json.NewDecoder(resp.Body).Decode(&j) == nil
				resp.Body.Close()
				if ok && j.Status == "succeeded" {
					if j.Result.Duplicate {
						dupOK++
					}
					break
				}
				if ok && j.Status == "failed" {
					break
				}
			}
			if time.Now().After(deadline) {
				notDone++
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	after, _ := e.Probe.Counts(e.Ctx)
	c.Correct("dup-submitted", "all duplicate job submissions were accepted by Forge's public API", submitFailed == 0 && submitted == len(refs)*2, fmt.Sprintf("%d submitted, %d failed", submitted, submitFailed))
	c.Correct("dup-results", "every duplicate job ended succeeded and reported duplicate=true", dupOK == submitted && notDone == 0, fmt.Sprintf("%d of %d, %d unfinished", dupOK, submitted, notDone))
	c.Correct("dup-effects", "duplicate deliveries created no extra business effects (effect counts unchanged)", after.EffectsTotal == pl.EffectsTotal, fmt.Sprintf("%d before, %d after", pl.EffectsTotal, after.EffectsTotal))
	c.Correct("stable-ids", "outbox event identities were preserved (same distinct ids before and after the duplicates)", e.Probe.DistinctEventIDs(e.Ctx) == events0 && after.Events == pl.Events, fmt.Sprintf("%d distinct ids, %d events", events0, after.Events))
	rec.Pipeline = after
	rec.Orders = e.Probe.OrderStatusCounts(e.Ctx)
	rec.Checks = e.Probe.Verify(e.Ctx, rec.Initial, true)
	bad := 0
	for _, ck := range rec.Checks {
		if !ck.OK {
			bad++
		}
	}
	c.Correct("integrity", fmt.Sprintf("all %d database integrity checks pass", len(rec.Checks)), bad == 0, fmt.Sprintf("%d failed", bad))
	rec.Workers = ParseWorkerLogs(e.SB.LogDir)
	rec.Load = &LoadResult{Label: "idempotency", ElapsedS: round3(httpSecs)}
	keys := make([]string, 0, len(tally.counts))
	for k := range tally.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tb := Table{Title: "HTTP responses by request type", Head: []string{"Request -> status", "Count"}}
	for _, k := range keys {
		tb.Rows = append(tb.Rows, []string{k, itoa(tally.counts[k])})
	}
	res.Tables = append(res.Tables, tb)
	res.Tables = append(res.Tables, Table{Title: "Duplicate delivery summary", Head: []string{"Logical orders", "Submissions per order", "Orders in DB", "Paid orders", "Events", "Effects (5 per paid order)", "Duplicate job submissions", "Duplicate results"},
		Rows: [][]string{{itoa(L), itoa(K), itoa(after.Orders), itoa(after.Paid), itoa(after.Events), itoa(after.EffectsTotal), itoa(submitted), itoa(dupOK)}}})
	finishWith(res, c, e)
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
