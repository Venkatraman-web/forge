package main

// Derived results. Everything here is a pure function of the raw run records
// (RunRecord / LoadResult), so the same code produces a scenario's tables and
// findings while the suite runs AND when a report is regenerated later from a
// preserved suite.json (`run.sh report`). Keeping one implementation is what
// keeps the live report and a regenerated report consistent with each other.
//
// Metric names used in every table (units and intervals are part of the name):
//   - Workflow throughput (workflows/s) = completed workflows / HTTP load duration.
//     Foreground only; says nothing about background jobs.
//   - Drain after load (s) = time from the end of the HTTP load (or, where a
//     scenario starts workers late, from the moment they were started) until
//     every paid order has its effects. Polled every 250 ms.
//   - Background drain rate (jobs/s) = 5 x paid orders / drain seconds.
//   - Pipeline span rate (jobs/s) = effects / (first outbox event -> last effect).
//     It includes the HTTP load, so it is not a worker-capacity measure.

import (
	"fmt"
	"math"
	"strings"
)

// ---------- table heads and rows shared by the scenarios ----------

var trialHead = []string{"Trial", "Completed", "Workflow throughput (workflows/s)", "Order p95 (ms)", "Payment p95 (ms)", "Workflow p50 (ms)", "Workflow p95 (ms)", "Workflow p99 (ms)",
	"HTTP failure rate", "Drain after load (s)", "Max backlog (jobs)", "Realised payments succeeded/declined/temporary"}

func trialRow(r *RunRecord) []string {
	l := r.Load
	drain := "n/a"
	if r.Drain != nil {
		drain = fmt.Sprintf("%.1f%s", r.Drain.WaitedS, map[bool]string{true: "", false: " (not drained)"}[r.Drain.Drained])
	}
	return []string{r.Label, itoa(l.Completed), fmtF(l.WfPerSec), fmtMS(stageOf(l, "order_create").P95MS), fmtMS(stageOf(l, "payment").P95MS),
		fmtMS(l.WorkflowLat.P50MS), fmtMS(l.WorkflowLat.P95MS), fmtMS(l.WorkflowLat.P99MS), pctStr(l.ErrorRatePct), drain, itoa(r.Resources.MaxBacklog),
		fmt.Sprintf("%d/%d/%d", l.Payment["succeeded"], l.Payment["declined"], l.Payment["temporary_failure"])}
}

// okRuns are the trials that produced measurements.
func okRuns(res *ScenarioResult) []*RunRecord {
	var out []*RunRecord
	for _, r := range res.Runs {
		if r != nil && r.Error == "" && r.Load != nil {
			out = append(out, r)
		}
	}
	return out
}

// ---------- host / container CPU (D7) ----------

// hostCPUStats returns the host CPU average and maximum in percent. The first
// sample of a trial has no previous /proc/stat reading to difference against and
// is always 0, so it is excluded. When the stored series is complete the figures
// are recomputed from it, which also corrects results recorded before this rule.
func hostCPUStats(rs ResourceSummary) (avg, max float64, n int) {
	if len(rs.Series) >= 2 && len(rs.Series) == rs.Samples {
		sum := 0.0
		for _, s := range rs.Series[1:] {
			sum += s.HostCPUPct
			max = math.Max(max, s.HostCPUPct)
		}
		return sum / float64(len(rs.Series)-1), max, len(rs.Series) - 1
	}
	return rs.HostCPUAvgPct, rs.HostCPUMaxPct, rs.Samples
}

// containerReadings counts how many `docker stats` readings a container has
// (they are taken every fifth sample, i.e. about every 5 s).
func containerReadings(rs ResourceSummary, name string) int {
	n := 0
	for _, s := range rs.Series {
		if _, ok := s.Containers[name]; ok {
			n++
		}
	}
	return n
}

const minContainerReadings = 3

// containerCPUCell renders "avg / max" and marks a figure built from fewer than
// minContainerReadings readings with an asterisk.
func containerCPUCell(rs ResourceSummary, name string) string {
	n := containerReadings(rs, name)
	if len(rs.Series) != rs.Samples { // downsampled series: the long run has plenty of readings
		n = minContainerReadings
	}
	if n == 0 {
		return "—"
	}
	cell := fmt.Sprintf("%.0f / %.0f", rs.ContainerCPUAvg[name], rs.ContainerCPUMax[name])
	if n < minContainerReadings {
		cell += "*"
	}
	return cell
}

const containerCPUFootnote = "\\* fewer than 3 `docker stats` readings (taken about every 5 s, the first before the load starts): indicative only."

// ---------- windows (D1) ----------

func windowDuration(w PhaseResult) float64 {
	if w.DurationS > 0 {
		return w.DurationS
	}
	return w.EndS - w.StartS
}

// nominalWindow is the configured window length: the longest window of the run.
func nominalWindow(ws []PhaseResult) float64 {
	n := 0.0
	for _, w := range ws {
		n = math.Max(n, windowDuration(w))
	}
	return n
}

func windowIsPartial(w PhaseResult, nominal float64) bool { return windowDuration(w) < nominal*0.999 }

// fullWindows are the windows of the nominal length: the only ones comparable with each other.
func fullWindows(ws []PhaseResult) []PhaseResult {
	nom := nominalWindow(ws)
	var out []PhaseResult
	for _, w := range ws {
		if !windowIsPartial(w, nom) {
			out = append(out, w)
		}
	}
	return out
}

func windowLabel(w PhaseResult, nominal float64) string {
	if windowIsPartial(w, nominal) {
		return fmt.Sprintf("%.2f-%.2f s (partial: %.2f s)", w.StartS, w.EndS, windowDuration(w))
	}
	return fmt.Sprintf("%.1f-%.1f s", w.StartS, w.EndS)
}

// deriveSoak builds the window table and the sustained-run findings from the
// soak trial. Partial windows are shown with their real duration and are left
// out of every comparison.
func deriveSoak(res *ScenarioResult) (Table, []string) {
	tb := Table{Title: fmt.Sprintf("Windows of the sustained run (%d s configured)", res.Params.DurationSeconds),
		Head: []string{"Window", "Duration (s)", "Completed", "Workflow throughput (workflows/s)", "Workflow p50 (ms)", "Workflow p95 (ms)", "Workflow p99 (ms)", "Errors"}}
	runs := okRuns(res)
	if len(runs) == 0 {
		return tb, nil
	}
	ws := runs[0].Load.Windows
	nom := nominalWindow(ws)
	var findings []string
	var excluded []string
	for _, w := range ws {
		rate := fmtF(w.CompletedPerSec)
		if windowIsPartial(w, nom) {
			if windowDuration(w) < 1 { // a rate over under a second is not meaningful
				rate = "n/a (under 1 s)"
			}
			excluded = append(excluded, fmt.Sprintf("%s (%d workflows)", windowLabel(w, nom), w.Completed))
		}
		tb.Rows = append(tb.Rows, []string{windowLabel(w, nom), fmt.Sprintf("%.2f", windowDuration(w)), itoa(w.Completed), rate, fmtMS(w.WorkflowLatency.P50MS), fmtMS(w.WorkflowLatency.P95MS), fmtMS(w.WorkflowLatency.P99MS), itoa(w.Errors)})
	}
	full := fullWindows(ws)
	if n := len(full); n >= 3 {
		k := max(1, n/3)
		first, last := full[:k], full[n-k:]
		rate := func(s []PhaseResult) float64 {
			c, d := 0, 0.0
			for _, w := range s {
				c += w.Completed
				d += windowDuration(w)
			}
			return float64(c) / d
		}
		p95 := func(s []PhaseResult) float64 {
			t := 0.0
			for _, w := range s {
				t += deref(w.WorkflowLatency.P95MS)
			}
			return t / float64(len(s))
		}
		rf, rl, pf, pl := rate(first), rate(last), p95(first), p95(last)
		findings = append(findings, fmt.Sprintf("Workflow throughput, first %d full window(s) (%.0f-%.0f s) vs last %d (%.0f-%.0f s): %.1f vs %.1f workflows/s (%+.1f%%); mean of the per-window workflow p95: %.0f vs %.0f ms (%+.1f%%). Only full %.0f s windows are compared.",
			k, first[0].StartS, first[k-1].EndS, k, last[0].StartS, last[k-1].EndS, rf, rl, pctChange(rf, rl), pf, pl, pctChange(pf, pl), nom))
		lo, hi := full[0], full[0]
		for _, w := range full {
			if deref(w.CompletedPerSec) < deref(lo.CompletedPerSec) {
				lo = w
			}
			if deref(w.CompletedPerSec) > deref(hi.CompletedPerSec) {
				hi = w
			}
		}
		findings = append(findings, fmt.Sprintf("Lowest full window %s at %.1f workflows/s, highest %s at %.1f workflows/s.", windowLabel(lo, nom), deref(lo.CompletedPerSec), windowLabel(hi, nom), deref(hi.CompletedPerSec)))
	} else {
		findings = append(findings, fmt.Sprintf("Only %d full window(s): too few for a first-versus-last comparison.", len(full)))
	}
	if len(excluded) > 0 {
		findings = append(findings, "Excluded from comparisons as partial: "+strings.Join(excluded, "; ")+". A short tail window holds the workflows still in flight when the load stopped and its rate is not comparable.")
	}
	return tb, findings
}

// ---------- backlog (D2) ----------

// BacklogTrend describes how the background backlog moved during the HTTP load.
type BacklogTrend struct {
	Mid, End     int     // backlog (jobs) at the middle and at the end of the load
	SlopePerS    float64 // least-squares slope over the second half of the load, jobs/s
	ProducedPerS float64 // 5 x paid orders / load seconds: how fast jobs were created
	OK           bool
}

// backlogTrendOf uses the 1 s samples: backlog = 5 x paid orders - effects.
func backlogTrendOf(r *RunRecord) BacklogTrend {
	var bt BacklogTrend
	el := r.Load.ElapsedS
	var xs, ys []float64
	for _, s := range r.Resources.Series {
		if s.T <= el/2 {
			bt.Mid = s.Backlog
		}
		if s.T <= el {
			bt.End = s.Backlog
		}
		if s.T >= el/2 && s.T <= el {
			xs, ys = append(xs, s.T), append(ys, float64(s.Backlog))
		}
	}
	if len(xs) < 3 || el <= 0 {
		return bt
	}
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx, my = mx/float64(len(xs)), my/float64(len(xs))
	var num, den float64
	for i := range xs {
		num += (xs[i] - mx) * (ys[i] - my)
		den += (xs[i] - mx) * (xs[i] - mx)
	}
	if den > 0 {
		bt.SlopePerS = num / den
	}
	bt.ProducedPerS = float64(jobsPerOrder*r.Pipeline.Paid) / el
	bt.OK = true
	return bt
}

func (b BacklogTrend) cell() string {
	if !b.OK {
		return "n/a"
	}
	return fmt.Sprintf("%d -> %d (%+.0f jobs/s of %.0f produced/s)", b.Mid, b.End, b.SlopePerS, b.ProducedPerS)
}

// ---------- saturation (D2, D3) ----------

type satState struct {
	best float64
	flat int
}

// satReasons applies the stop rule to one stage. The rule itself is unchanged: error
// rate, p99, host CPU, or two consecutive stages without a 5% gain over the best.
func satReasons(p Params, r *RunRecord, st *satState) (stop []string, plateau string) {
	l := r.Load
	if l.ErrorRatePct != nil && *l.ErrorRatePct > p.MaxErrorRatePct {
		stop = append(stop, fmt.Sprintf("failure rate %.2f%% > %.1f%%", *l.ErrorRatePct, p.MaxErrorRatePct))
	}
	if p.StopP99MS > 0 && deref(l.WorkflowLat.P99MS) > p.StopP99MS {
		stop = append(stop, fmt.Sprintf("p99 %.0f ms > %.0f ms", deref(l.WorkflowLat.P99MS), p.StopP99MS))
	}
	if cpu, _, _ := hostCPUStats(r.Resources); p.StopCPUPct > 0 && cpu > p.StopCPUPct {
		stop = append(stop, fmt.Sprintf("host CPU %.0f%% > %.0f%%", cpu, p.StopCPUPct))
	}
	if tp := deref(l.WfPerSec); tp > st.best*1.05 {
		st.best, st.flat = tp, 0
	} else {
		st.flat++
		if st.flat >= 2 {
			plateau = fmt.Sprintf("throughput plateau: no 5%% gain over %.1f workflows/s (the last level that gained 5%%) for 2 consecutive stages", st.best)
		}
	}
	return stop, plateau
}

type satSummary struct {
	Table     Table
	Findings  []string
	HasStable bool // at least one stage without an error/latency/CPU stop reason
	StopFound bool // some stage triggered a stop reason (including the plateau rule)
}

func concOf(r *RunRecord) int {
	for _, ph := range r.Load.Phases {
		return ph.Concurrency
	}
	return 0
}

// deriveSaturation: the verdict per stage follows the stop rule; the operating point is
// the lowest concurrency within 5% of the peak throughput of the stable stages.
func deriveSaturation(res *ScenarioResult) satSummary {
	p := res.Params
	sum := satSummary{Table: Table{Title: "Saturation stages (fixed duration each, databases rebuilt per stage)",
		Head: []string{"Concurrency", "Workflow throughput (workflows/s)", "vs highest so far", "Workflow p95 (ms)", "Workflow p99 (ms)", "HTTP failure rate", "Host CPU avg / max (%)",
			"Backlog mid -> end of load (slope, 2nd half)", "Drain after load (s)", "Verdict"}}}
	runs := okRuns(res)
	var st satState
	runningMax := 0.0
	type stage struct {
		conc            int
		tp, p99, p95    float64
		stableNoPlateau bool
	}
	var stages []stage
	var stopText string
	for _, r := range runs {
		conc := concOf(r)
		stop, plateau := satReasons(p, r, &st)
		reasons := stop
		if plateau != "" {
			reasons = append(reasons, plateau)
		}
		verdict := "no stop condition met"
		if len(reasons) > 0 {
			verdict = "STOP: " + strings.Join(reasons, "; ")
			sum.StopFound = true
			if stopText == "" {
				stopText = fmt.Sprintf("concurrency %d: %s", conc, strings.Join(reasons, "; "))
			}
		}
		if r.Load.GeneratorSuspect {
			verdict += " (generator may be the limit)"
		}
		tp := deref(r.Load.WfPerSec)
		vs := "first stage"
		if runningMax > 0 {
			vs = fmt.Sprintf("%+.1f%%", pctChange(runningMax, tp))
		}
		runningMax = math.Max(runningMax, tp)
		cpu, cpuMax, _ := hostCPUStats(r.Resources)
		sum.Table.Rows = append(sum.Table.Rows, []string{itoa(conc), fmtF(r.Load.WfPerSec), vs, fmtMS(r.Load.WorkflowLat.P95MS), fmtMS(r.Load.WorkflowLat.P99MS), pctStr(r.Load.ErrorRatePct),
			fmt.Sprintf("%.0f / %.0f", cpu, cpuMax), backlogTrendOf(r).cell(), drainShort(r.Drain), verdict})
		stages = append(stages, stage{conc, tp, deref(r.Load.WorkflowLat.P99MS), deref(r.Load.WorkflowLat.P95MS), len(stop) == 0})
	}
	// operating point among the stages that met no error/latency/CPU stop condition
	peak, peakConc := 0.0, 0
	for _, s := range stages {
		if s.stableNoPlateau {
			sum.HasStable = true
			if s.tp > peak {
				peak, peakConc = s.tp, s.conc
			}
		}
	}
	if !sum.HasStable {
		sum.Findings = append(sum.Findings, "No stage was free of error-rate, latency and CPU stop conditions, so no operating point was demonstrated.")
	} else {
		var op stage
		for _, s := range stages {
			if s.stableNoPlateau && s.tp >= 0.95*peak {
				op = s
				break
			}
		}
		sum.Findings = append(sum.Findings, fmt.Sprintf("Peak workflow throughput %.1f workflows/s at concurrency %d. Lowest tested concurrency within 5%% of that peak: %d (%.1f workflows/s, workflow p99 %.0f ms). This is an observation from %d s stages, not a sustainable-capacity claim.",
			peak, peakConc, op.conc, op.tp, op.p99, p.StageSeconds))
		var higher []string
		for _, s := range stages {
			if s.conc > op.conc && s.stableNoPlateau {
				higher = append(higher, fmt.Sprintf("concurrency %d: %.1f workflows/s (%+.1f%% vs peak), p99 %.0f ms", s.conc, s.tp, pctChange(peak, s.tp), s.p99))
			}
		}
		if len(higher) > 0 {
			sum.Findings = append(sum.Findings, "Higher stages: "+strings.Join(higher, "; ")+".")
		}
		if len(stages) > 0 && op.conc == stages[0].conc {
			sum.Findings = append(sum.Findings, fmt.Sprintf("The throughput plateau is already reached at the first tested concurrency (%d): the knee lies at or below it and this ladder cannot locate it.", op.conc))
		}
	}
	if stopText != "" {
		sum.Findings = append(sum.Findings, "Stop condition first met at "+stopText+".")
	} else if len(stages) > 0 {
		sum.Findings = append(sum.Findings, "No stop condition was met within the configured range: the saturation point was not reached; raise the limits to look further.")
	}
	if len(stages) > 0 {
		lo, hi := math.MaxInt, 0
		for _, r := range runs {
			b := backlogTrendOf(r)
			if b.OK {
				lo, hi = min(lo, b.End), max(hi, b.End)
			}
		}
		if hi > 0 {
			sum.Findings = append(sum.Findings, fmt.Sprintf("Background backlog at the end of the load was %d-%d jobs across stages (backlog = 5 x paid orders - effects, a proxy: Forge's queue depth is not exposed). The per-stage trend is shown in the table as mid -> end values and the least-squares slope over the second half of the load, next to the production rate; no yes/no threshold is applied.", lo, hi))
		}
	}
	return sum
}

// ---------- ramp (D8) ----------

func deriveRamp(res *ScenarioResult) (Table, []string) {
	tb := Table{Title: "Concurrency ramp (same workload per stage, databases rebuilt per stage)", Head: append([]string{"Concurrency"}, trialHead[1:]...)}
	runs := okRuns(res)
	var findings []string
	var prevT, prevP95, bestT float64
	bestC, knee, kneeC := 0, "", 0
	var kneeT float64
	for i, r := range runs {
		conc := concOf(r)
		row := trialRow(r)
		row[0] = itoa(conc)
		tb.Rows = append(tb.Rows, row)
		t, p95 := deref(r.Load.WfPerSec), deref(r.Load.WorkflowLat.P95MS)
		if i > 0 && knee == "" {
			if t < prevT*1.10 {
				knee = fmt.Sprintf("throughput gained less than 10%% going from the previous stage to concurrency %d (%.1f -> %.1f workflows/s)", conc, prevT, t)
				kneeC, kneeT = conc, t
			} else if prevP95 > 0 && p95 > 2*prevP95 {
				knee = fmt.Sprintf("workflow p95 more than doubled at concurrency %d (%.0f -> %.0f ms)", conc, prevP95, p95)
				kneeC, kneeT = conc, t
			}
		}
		if t > bestT {
			bestT, bestC = t, conc
		}
		prevT, prevP95 = t, p95
	}
	switch {
	case knee != "":
		findings = append(findings, "First step with a scaling limit: "+knee+".")
		if bestC > kneeC && bestT > kneeT*1.05 {
			findings = append(findings, fmt.Sprintf("Later stages still gained: the best stage was concurrency %d at %.1f workflows/s (%+.1f%% vs the stage where the first limit appeared), so that first step was not the end of scaling.", bestC, bestT, pctChange(kneeT, bestT)))
		} else if bestC > 0 {
			findings = append(findings, fmt.Sprintf("Best stage overall: concurrency %d at %.1f workflows/s.", bestC, bestT))
		}
	case len(runs) > 1:
		findings = append(findings, fmt.Sprintf("No scaling limit was observed within the tested concurrency range (each step gained at least 10%% throughput and p95 never doubled); best stage: concurrency %d at %.1f workflows/s.", bestC, bestT))
	}
	if len(runs) > 0 {
		last := runs[len(runs)-1]
		if tail := firstTail(last); tail != "" {
			findings = append(findings, tail)
		}
	}
	return tb, findings
}

// firstTail warns that very short stages leave the background work unstarted, which
// makes their HTTP throughput not comparable with longer stages.
func firstTail(r *RunRecord) string {
	if r.Load.ElapsedS >= 5 {
		return ""
	}
	return fmt.Sprintf("The last stage lasted only %.1f s and its peak backlog was %d of %d produced jobs, i.e. almost no background work had completed while it was measured. Workflow throughput from stages this short may therefore not be comparable with stages long enough to include background work (hypothesis; compare the saturation scenario).", r.Load.ElapsedS, r.Resources.MaxBacklog, 5*r.Pipeline.Paid)
}

// ---------- baseline aggregation (naming, D5) ----------

func deriveBaseline(res *ScenarioResult) ([]Table, []string) {
	runs := okRuns(res)
	var rows [][]string
	var tput, p95s, drains, drainRates, spanRates []float64
	for _, r := range runs {
		rows = append(rows, trialRow(r))
		tput = append(tput, deref(r.Load.WfPerSec))
		p95s = append(p95s, deref(r.Load.WorkflowLat.P95MS))
		if r.Drain != nil {
			drains = append(drains, r.Drain.WaitedS)
			if v := drainRate(r.Drain); v > 0 {
				drainRates = append(drainRates, v)
			}
		}
		if r.Span != nil && r.Span.JobsPerSec != nil {
			spanRates = append(spanRates, *r.Span.JobsPerSec)
		}
	}
	tables := []Table{{Title: "Repetitions (each on freshly rebuilt databases)", Head: trialHead, Rows: rows}}
	var findings []string
	if len(tput) > 1 {
		lo, hi := minmax(tput)
		agg := Table{Title: "Aggregation across repetitions (median, with min and max; spread = (max-min)/median)", Head: []string{"Metric", "Median", "Min", "Max", "Spread"}}
		for _, m := range []struct {
			n string
			v []float64
		}{{"Workflow throughput (workflows/s = completed / HTTP load duration)", tput}, {"Workflow p95 (ms)", p95s}, {"Drain after load (s)", drains},
			{"Background drain rate (jobs/s = 5 x paid orders / drain seconds)", drainRates},
			{"Pipeline span rate (jobs/s, first outbox event -> last effect; includes the HTTP load)", spanRates}} {
			if len(m.v) == 0 {
				continue
			}
			a, b := minmax(m.v)
			med := median(m.v)
			sp := "n/a"
			if med > 0 {
				sp = fmt.Sprintf("%.1f%%", (b-a)/med*100)
			}
			agg.Rows = append(agg.Rows, []string{m.n, fmt.Sprintf("%.2f", med), fmt.Sprintf("%.2f", a), fmt.Sprintf("%.2f", b), sp})
		}
		tables = append(tables, agg)
		if med := median(tput); med > 0 && (hi-lo)/med > 0.2 {
			findings = append(findings, fmt.Sprintf("Run-to-run throughput spread is %.0f%% of the median: single runs of this workload are not precise enough to detect differences smaller than that.", (hi-lo)/med*100))
		}
	}
	return tables, findings
}

// drainRate is the background drain rate in jobs/s.
func drainRate(d *DrainResult) float64 {
	if d == nil || !d.Drained || d.WaitedS <= 0 {
		return 0
	}
	return float64(d.ExpectedJobs) / d.WaitedS
}

// ---------- worker scaling (D5) ----------

func deriveScaling(res *ScenarioResult) Table {
	tb := Table{Title: "Worker scaling (workers start after all events are published, so the drain measures worker + Forge + database capacity)",
		Head: []string{"Config", "Capacity (slots/type)", "Background drain rate (jobs/s = 5 x paid / drain s; drain polled every 250 ms)", "Gain vs smallest", "Efficiency", "Drain after workers started (s)",
			"Publication wait with workers stopped (s)", "Forge exec p50/p95 (ms, all types)", "Worker CPU (avg cores, all workers)", "Forge DB CPU avg / max (%)", "App DB CPU avg / max (%)"}}
	var baseRate, baseCap float64
	for i, r := range okRuns(res) {
		var sc ScalingConfig
		if i < len(res.Params.Scaling) {
			sc = res.Params.Scaling[i]
		}
		for _, c := range res.Params.Scaling {
			if c.Name == r.Label {
				sc = c
			}
		}
		rate := drainRate(r.Drain)
		capacity := float64(sc.Replicas * sc.Concurrency)
		if baseRate == 0 && rate > 0 {
			baseRate, baseCap = rate, capacity
		}
		gain, eff := "—", "—"
		if baseRate > 0 && rate > 0 && baseCap > 0 {
			gain = fmt.Sprintf("%.2fx", rate/baseRate)
			eff = fmt.Sprintf("%.0f%%", (rate/baseRate)/(capacity/baseCap)*100)
		}
		var wcpu float64
		for _, ps := range r.Resources.Procs {
			if strings.HasPrefix(ps.Name, "worker-") {
				wcpu += ps.CPUAvgCores
			}
		}
		var q50, q95 []float64
		for _, t := range r.Forge.Types {
			if t.Execution.P50MS != nil {
				q50, q95 = append(q50, *t.Execution.P50MS), append(q95, deref(t.Execution.P95MS))
			}
		}
		wait := "—"
		if v, ok := r.Extra["publish_wait_s"].(float64); ok {
			wait = fmt.Sprintf("%.1f", v)
		}
		tb.Rows = append(tb.Rows, []string{fmt.Sprintf("%s (%d replicas x %d)", r.Label, sc.Replicas, sc.Concurrency), fmt.Sprintf("%.0f", capacity), fmt.Sprintf("%.1f", rate), gain, eff,
			drainShort(r.Drain), wait, fmt.Sprintf("%.1f / %.1f", median(q50), median(q95)), fmt.Sprintf("%.2f", wcpu), containerCPUCell(r.Resources, "forge-db"), containerCPUCell(r.Resources, "app-db")})
	}
	return tb
}
