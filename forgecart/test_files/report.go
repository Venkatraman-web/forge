package main

// report.md is generated from the collected SuiteRecord only: every number comes
// from a measurement record, and statements that interpret the numbers are
// labelled as observations or hypotheses.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type EnvInfo struct {
	Kernel      string            `json:"kernel"`
	CPUModel    string            `json:"cpu_model"`
	CPUs        int               `json:"cpus"`
	MemoryMB    int               `json:"memory_mb"`
	Versions    map[string]string `json:"tool_versions"`
	PostgresVer string            `json:"postgres_version"`
	PgCPUs      string            `json:"postgres_container_cpus"`
	PgMem       string            `json:"postgres_container_memory"`
	Services    string            `json:"services"`
	Host        HostMeta          `json:"host_and_engine"`
	Binaries    []BinaryInfo      `json:"binaries"`
}

type SuiteRecord struct {
	RunID       string            `json:"run_id"`
	Profile     string            `json:"profile"`
	Selected    []string          `json:"selected_scenarios"`
	StartedAt   time.Time         `json:"started_at"`
	EndedAt     time.Time         `json:"ended_at"`
	Interrupted bool              `json:"interrupted"`
	StopReason  string            `json:"stop_reason,omitempty"`
	Complete    bool              `json:"complete"`
	Env         EnvInfo           `json:"environment"`
	Reference   SandboxConfig     `json:"reference_sandbox_config"`
	Caps        []string          `json:"safety_caps_applied"`
	Scenarios   []*ScenarioResult `json:"scenarios"`
	Options     Options           `json:"options"`
}

func (s *SuiteRecord) find(name string) *ScenarioResult {
	for _, r := range s.Scenarios {
		if r.Name == name {
			return r
		}
	}
	return nil
}

func statusIcon(st string) string {
	switch st {
	case "passed":
		return "PASSED"
	case "failed":
		return "FAILED"
	case "inconclusive":
		return "INCONCLUSIVE"
	case "error":
		return "ERROR (could not run)"
	case "skipped":
		return "SKIPPED"
	case "interrupted":
		return "INTERRUPTED"
	}
	return strings.ToUpper(st)
}

type md struct{ b strings.Builder }

func (m *md) f(format string, a ...any) { fmt.Fprintf(&m.b, format, a...) }
func (m *md) ln(s string)               { m.b.WriteString(s + "\n") }
func (m *md) table(t Table) {
	if len(t.Rows) == 0 {
		m.f("*%s: no rows were produced.*\n\n", t.Title)
		return
	}
	if t.Title != "" {
		m.f("**%s**\n\n", t.Title)
	}
	m.ln("| " + strings.Join(escAll(t.Head), " | ") + " |")
	seps := make([]string, len(t.Head))
	for i := range seps {
		seps[i] = "---"
	}
	m.ln("| " + strings.Join(seps, " | ") + " |")
	for _, r := range t.Rows {
		m.ln("| " + strings.Join(escAll(r), " | ") + " |")
	}
	m.ln("")
}

func escAll(s []string) []string {
	o := make([]string, len(s))
	for i, v := range s {
		o[i] = mdEscape(v)
	}
	return o
}

// RenderReport builds the Markdown report.
func RenderReport(s *SuiteRecord) string {
	m := &md{}
	title := "ForgeCart / Forge load-test report"
	if !s.Complete {
		title += " (PARTIAL)"
	}
	m.f("# %s\n\n", title)
	m.f("Run `%s` · profile `%s` · started %s · duration %s\n\n", s.RunID, s.Profile, s.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"), s.EndedAt.Sub(s.StartedAt).Round(time.Second))
	if !s.Complete {
		m.f("> **This report is incomplete.** %s Scenarios that did not run are listed as such; nothing below was estimated for them.\n\n", orStr(s.StopReason, "The run did not finish."))
	}
	m.f("Raw measurements: `results/%s/` (suite.json and one JSON file per scenario). This report is generated from those records.\n\n", s.RunID)

	sectionSummary(m, s)
	sectionEnv(m, s)
	sectionScenarios(m, s)
	sectionConfigs(m, s)
	scenarioSection(m, s, 5, "Baseline measurements", "baseline", baselineExtra)
	scenarioSection(m, s, 6, "Concurrency-ramp results", "ramp", rampExtra)
	m.ln("## 7. Burst and sustained-load results\n")
	scenarioBody(m, s.find("burst"), "burst")
	scenarioBody(m, s.find("soak"), "soak")
	scenarioSection(m, s, 8, "Saturation and highest demonstrated stable operating point", "saturation", nil)
	m.ln("## 9. Stock-contention and payment-mix results\n")
	scenarioBody(m, s.find("stock-contention"), "stock-contention")
	scenarioBody(m, s.find("payment-mix"), "payment-mix")
	scenarioSection(m, s, 10, "Idempotency and duplicate-delivery results", "idempotency", nil)
	scenarioSection(m, s, 11, "Full background-pipeline and drain measurements", "pipeline", pipelineExtra)
	sectionWorkers(m, s)
	scenarioSection(m, s, 13, "Worker-scaling comparison and efficiency", "worker-scaling", nil)
	scenarioSection(m, s, 14, "Recovery and resilience results", "recovery", nil)
	sectionIntegrity(m, s)
	sectionResources(m, s)
	sectionErrors(m, s)
	sectionBottlenecks(m, s)
	sectionRecommend(m, s)
	sectionLimits(m, s)
	return m.b.String()
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---------- 1. summary ----------

func sectionSummary(m *md, s *SuiteRecord) {
	m.ln("## 1. Executive summary\n")
	counts := map[string]int{}
	for _, r := range s.Scenarios {
		counts[r.Status]++
	}
	m.f("%d of %d selected scenarios were executed: **%d passed, %d failed, %d inconclusive, %d errored, %d skipped, %d interrupted**.\n\n",
		len(s.Scenarios)-counts["skipped"]-counts["error"], len(s.Selected), counts["passed"], counts["failed"], counts["inconclusive"], counts["error"], counts["skipped"], counts["interrupted"])
	t := Table{Head: []string{"Scenario", "Status", "Headline result"}}
	for _, r := range s.Scenarios {
		t.Rows = append(t.Rows, []string{r.Title, statusIcon(r.Status), headline(r)})
	}
	m.table(t)
	m.ln("**Metric names.** *Workflow throughput (workflows/s)* = completed workflows / HTTP load duration: foreground only. *Drain after load (s)* = time until every paid order has its effects, measured from the end of the HTTP load (from the moment workers were started in the worker-scaling scenario) and polled every 250 ms. *Background drain rate (jobs/s)* = 5 x paid orders / drain seconds. *Pipeline span rate (jobs/s)* = effects / (first outbox event -> last effect): it includes the HTTP load and is not a capacity measure. *Backlog* = 5 x paid orders - effects, sampled every second (a proxy: Forge's queue depth is not exposed). Soak/trend comparisons use only full windows.\n")
	m.ln("**Reading this report.** *Passed* means every correctness criterion held. Performance objectives were not specified by the owner, so measured values are reported without a pass/fail judgement (\"unspecified\"). *Inconclusive* means the data cannot support a conclusion (for example the load generator may have limited the run).\n")
	for _, r := range s.Scenarios {
		if r.Name == "saturation" || r.Name == "ramp" {
			_, fs := displayParts(r)
			for _, f := range fs {
				m.f("- **%s**: %s\n", r.Title, f)
			}
		}
	}
	m.ln("")
}

func headline(r *ScenarioResult) string {
	if r.Status == "skipped" || r.Status == "error" {
		return strings.Join(r.Reasons, "; ")
	}
	var parts []string
	switch r.Kind {
	case "smoke", "pipeline", "stock":
		if len(r.Runs) > 0 && r.Runs[0].Load != nil {
			l := r.Runs[0].Load
			parts = append(parts, fmt.Sprintf("%d completed, %s workflows/s, workflow p95 %s ms", l.Completed, fmtF(l.WfPerSec), fmtMS(l.WorkflowLat.P95MS)))
			if r.Runs[0].Drain != nil {
				parts = append(parts, "drain "+drainShort(r.Runs[0].Drain))
			}
		}
	case "baseline":
		var t []float64
		for _, x := range r.Runs {
			if x.Load != nil && x.Load.WfPerSec != nil {
				t = append(t, *x.Load.WfPerSec)
			}
		}
		if len(t) > 0 {
			parts = append(parts, fmt.Sprintf("median %.1f workflows/s over %d repetitions", median(t), len(t)))
		}
	case "ramp", "saturation":
		parts = append(parts, fmt.Sprintf("%d stage(s) executed", len(r.Runs)))
	case "scaling":
		parts = append(parts, fmt.Sprintf("%d worker configuration(s) compared", len(r.Runs)))
	case "recovery":
		ok := 0
		for _, x := range r.Runs {
			if x.Error == "" && x.Drain != nil && x.Drain.Drained {
				ok++
			}
		}
		parts = append(parts, fmt.Sprintf("%d of %d fault tests drained completely", ok, len(r.Runs)))
	default:
		parts = append(parts, fmt.Sprintf("%d run(s)", len(r.Runs)))
	}
	if len(r.Reasons) > 0 && r.Status != "passed" {
		parts = append(parts, "see section 17")
	}
	return strings.Join(parts, "; ")
}

// ---------- 2-4 ----------

func sectionEnv(m *md, s *SuiteRecord) {
	m.ln("## 2. Test environment and resource limits\n")
	e := s.Env
	t := Table{Head: []string{"Item", "Value"}, Rows: [][]string{
		{"Host CPU", fmt.Sprintf("%s, %d logical CPUs", orStr(e.CPUModel, "unknown"), e.CPUs)},
		{"Host memory", fmt.Sprintf("%d MB", e.MemoryMB)},
		{"Kernel", e.Kernel},
		{"PostgreSQL", e.PostgresVer},
		{"PostgreSQL containers", fmt.Sprintf("two (Forge job database, ForgeCart application database); each limited to %s CPUs and %s memory; tmpfs storage; max_connections 400", e.PgCPUs, e.PgMem)},
		{"Services", e.Services},
	}}
	for _, k := range sortedKeys(e.Versions) {
		t.Rows = append(t.Rows, []string{k, e.Versions[k]})
	}
	h := e.Host
	na := func(v string) string {
		return orStr(v, unavail("not recorded: this result was produced before the field existed"))
	}
	if h == (HostMeta{}) {
		t.Rows = append(t.Rows, []string{"Host boot time, uptime, load average, CPU governor, container engine and VM size, source fingerprint", unavail("not recorded: this result was produced before these fields existed")})
	} else {
		t.Rows = append(t.Rows,
			[]string{"Host boot time / uptime at start", na(h.BootTime) + " / " + na(h.UptimeAtStart)},
			[]string{"Host load average at start (1/5/15 min)", na(h.LoadAvgAtStart)},
			[]string{"CPU scaling governor", na(h.CPUGovernor)},
			[]string{"Container engine", na(h.DockerEngine)},
			[]string{"Container engine VM: CPUs / memory bytes / kernel", na(h.DockerVMCPUs) + " / " + na(h.DockerVMMemory) + " / " + na(h.DockerVMKernel)},
			[]string{"Container engine VM process", na(h.DockerVMProcess)},
			[]string{"Source tree fingerprint", fmt.Sprintf("%s (%d files; %s)", na(h.SourceTreeHash), h.SourceFiles, orStr(h.SourceTreeRule, "rule not recorded"))})
	}
	m.table(t)
	if len(e.Binaries) > 0 {
		bt := Table{Title: "Measured executables", Head: []string{"Binary", "sha256", "Go build info"}}
		for _, b := range e.Binaries {
			bt.Rows = append(bt.Rows, []string{b.Name, b.SHA256, b.BuildInfo})
		}
		m.table(bt)
		m.ln("Binary hashes include embedded build paths, so equal source built in a different directory hashes differently; compare the source tree fingerprint for source identity.\n")
	} else {
		m.f("*Executable hashes and build info: %s*\n\n", unavail("not recorded: this result was produced before the field existed"))
	}
	m.ln("The load generator, Forge, ForgeCart, the dispatcher and every worker run as processes on the same host, so they compete with each other and with Docker for CPU. Results characterize this host and sandbox, not production hardware.\n")
	rc := s.Reference
	m.f("Reference service configuration: Forge max queued jobs %d (the Forge default is 1000), worker-API max concurrent claim requests %d, lease %s / heartbeat %s / safety margin %s, dispatcher batch %d poll %s x%d, workers 1 replica x 4 slots per type, payment simulator max delay %s.\n\n",
		rc.ForgeMaxQueued, rc.WorkerMaxPolls, rc.Lease, rc.Heartbeat, rc.SafetyMargin, rc.DispatcherBatch, rc.DispatcherPoll, rc.DispatcherReplicas, rc.PaymentMaxDelay)
	if len(s.Caps) > 0 {
		m.ln("**Safety caps applied:**\n")
		for _, c := range s.Caps {
			m.f("- %s\n", c)
		}
		m.ln("")
	}
}

func sectionScenarios(m *md, s *SuiteRecord) {
	m.ln("## 3. Scenarios executed, skipped and failed\n")
	t := Table{Head: []string{"Scenario", "Status", "Duration", "Trials", "Reasons / notes"}}
	for _, r := range s.Scenarios {
		note := strings.Join(append(append([]string{}, r.Reasons...), r.Notes...), " | ")
		t.Rows = append(t.Rows, []string{r.Name, statusIcon(r.Status), fmt.Sprintf("%.0f s", r.DurationS), itoa(len(r.Runs)), note})
	}
	have := map[string]bool{}
	for _, r := range s.Scenarios {
		have[r.Name] = true
	}
	for _, n := range s.Selected {
		if !have[n] {
			t.Rows = append(t.Rows, []string{n, "NOT RUN", "-", "0", orStr(s.StopReason, "the run ended before this scenario started")})
		}
	}
	m.table(t)
	selfChecks(m, s)
	m.ln("*Skipped* and *not run* scenarios produced no measurements; *failed* scenarios ran and missed a criterion; *error* means the sandbox could not execute the scenario.\n")
}

func sectionConfigs(m *md, s *SuiteRecord) {
	m.ln("## 4. Workload configurations and initial conditions\n")
	m.ln("**Seeds fix the offered requests only.** Customer emails, product choices, quantities and idempotency keys are reproducible from the seed. The payment outcome is a pure function of seed, order ID and key, and order IDs are random, so the *realised* payment mix (and with it the number of paid orders and background jobs) differs slightly between trials; the realised counts are reported next to the configured mix below and in each trial table.\n")
	m.ln("Every trial starts from freshly rebuilt databases (both dropped, recreated from the versioned migrations, catalog seeded with the deterministic five-product seed, inventory set as listed) and freshly started services. Seeds make the offered orders reproducible.\n")
	t := Table{Head: []string{"Scenario", "Seed", "Workload", "Concurrency", "Payment mix s/d/t % (configured)", "Realised payments s/d/t (counts, all trials)", "Initial stock per product", "Drain timeout"}}
	for _, r := range s.Scenarios {
		p := r.Params
		work := "—"
		switch {
		case p.Workflows > 0:
			work = fmt.Sprintf("%d workflows", p.Workflows)
			if p.Repetitions > 1 {
				work += fmt.Sprintf(" x %d repetitions", p.Repetitions)
			}
		case p.StageWorkflows > 0:
			work = fmt.Sprintf("%d workflows per stage", p.StageWorkflows)
		case p.DurationSeconds > 0:
			work = fmt.Sprintf("%d s", p.DurationSeconds)
		case p.StageSeconds > 0:
			work = fmt.Sprintf("%d s per stage", p.StageSeconds)
		case p.BaseSeconds > 0:
			work = fmt.Sprintf("%d s normal + %d s burst + %d s normal", p.BaseSeconds, p.BurstSeconds, p.RecoverySeconds)
		case p.LogicalOrders > 0:
			work = fmt.Sprintf("%d logical orders x %d concurrent submissions, %d duplicate-job events", p.LogicalOrders, p.Duplicates, p.JobDupEvents)
		}
		conc := "—"
		switch {
		case len(p.Concurrencies) > 0:
			conc = fmt.Sprint(p.Concurrencies)
		case p.StartConcurrency > 0:
			conc = fmt.Sprintf("%d x%.1f up to %d", p.StartConcurrency, p.Factor, p.MaxConcurrency)
		case p.BaseConcurrency > 0:
			conc = fmt.Sprintf("%d / burst %d", p.BaseConcurrency, p.BurstConcurrency)
		case p.Concurrency > 0:
			conc = itoa(p.Concurrency)
		}
		mix := "—"
		if p.Mix != nil {
			mix = fmt.Sprintf("%d/%d/%d", p.Mix.Success, p.Mix.Decline, p.Mix.Temp)
		} else if len(p.Profiles) > 0 {
			var ps []string
			for _, pr := range p.Profiles {
				ps = append(ps, fmt.Sprintf("%s %d/%d/%d", pr.Name, pr.Mix.Success, pr.Mix.Decline, pr.Mix.Temp))
			}
			mix = strings.Join(ps, "; ")
		}
		var rs, rd, rt int
		for _, x := range okRuns(r) {
			rs, rd, rt = rs+x.Load.Payment["succeeded"], rd+x.Load.Payment["declined"], rt+x.Load.Payment["temporary_failure"]
		}
		realised := "—"
		if rs+rd+rt > 0 {
			realised = fmt.Sprintf("%d/%d/%d", rs, rd, rt)
		}
		t.Rows = append(t.Rows, []string{r.Name, fmt.Sprint(p.Seed), work, conc, mix, realised, itoa(p.Inventory), fmt.Sprintf("%d s", p.DrainTimeoutS)})
	}
	m.table(t)
	m.ln("Workflow = one checkout request followed by one payment request. Closed-loop: each of the *concurrency* workers starts its next workflow when the previous one finishes, so the offered rate follows the system's speed; this measures what the system sustains at a given concurrency, not a fixed arrival rate.\n")
}

// ---------- per-scenario ----------

func scenarioSection(m *md, s *SuiteRecord, n int, title, name string, extra func(m *md, r *ScenarioResult)) {
	m.f("## %d. %s\n\n", n, title)
	r := s.find(name)
	scenarioBodyX(m, r, name, extra)
}

func scenarioBody(m *md, r *ScenarioResult, name string) { scenarioBodyX(m, r, name, extraFor(name)) }

func extraFor(name string) func(m *md, r *ScenarioResult) {
	switch name {
	case "burst":
		return burstExtra
	case "soak":
		return soakExtra
	}
	return nil
}

func scenarioBodyX(m *md, r *ScenarioResult, name string, extra func(m *md, r *ScenarioResult)) {
	if r == nil {
		m.f("### %s\n\n*Not run in this suite execution.*\n\n", name)
		return
	}
	m.f("### %s — %s\n\n", r.Title, statusIcon(r.Status))
	m.f("%s\n\n", r.Description)
	m.f("*Executed %s – %s (%.0f s), %d trial(s).*\n\n", r.StartedAt.UTC().Format("15:04:05"), r.EndedAt.UTC().Format("15:04:05"), r.DurationS, len(r.Runs))
	if r.Status == "skipped" || r.Status == "error" {
		for _, x := range r.Reasons {
			m.f("- %s\n", x)
		}
		m.ln("")
		return
	}
	tables, findings := displayParts(r)
	for _, t := range tables {
		m.table(t)
	}
	if extra != nil {
		extra(m, r)
	}
	if len(findings) > 0 {
		m.ln("**Observations**\n")
		for _, f := range findings {
			m.f("- %s\n", f)
		}
		m.ln("")
	}
	if len(r.Notes) > 0 {
		for _, f := range r.Notes {
			m.f("- Note: %s\n", f)
		}
		m.ln("")
	}
	crit := Table{Title: "Acceptance criteria", Head: []string{"Criterion", "Kind", "Result", "Detail"}}
	for _, c := range r.Criteria {
		crit.Rows = append(crit.Rows, []string{c.Description, c.Kind, strings.ToUpper(c.Status), c.Detail})
	}
	if len(crit.Rows) > 24 {
		pass, fail, uns := 0, 0, 0
		for _, c := range r.Criteria {
			switch c.Status {
			case "pass":
				pass++
			case "fail":
				fail++
			default:
				uns++
			}
		}
		m.f("**Acceptance criteria:** %d evaluated — %d passed, %d failed, %d unspecified. Failed and unspecified ones:\n\n", len(r.Criteria), pass, fail, uns)
		var rows [][]string
		for _, c := range r.Criteria {
			if c.Status != "pass" {
				rows = append(rows, []string{c.Description, c.Kind, strings.ToUpper(c.Status), c.Detail})
			}
		}
		if len(rows) == 0 {
			m.ln("*(none: every criterion passed; the full list is in the scenario JSON)*\n")
		} else {
			crit.Rows = rows
			crit.Title = ""
			m.table(crit)
		}
	} else {
		m.table(crit)
	}
	m.f("**Outcome: %s.**", statusIcon(r.Status))
	if len(r.Reasons) > 0 {
		m.f(" %s", strings.Join(r.Reasons, "; "))
	}
	m.ln("\n")
}

func baselineExtra(m *md, r *ScenarioResult) {
	m.ln("Aggregation: each repetition is an independent trial on rebuilt databases with a different seed (seed + repetition number). The median across repetitions is used as the reference; the spread column shows run-to-run variability. Only three repetitions were run, so the spread is itself imprecise.\n")
}

func rampExtra(m *md, r *ScenarioResult) {
	var rows []string
	max := 0.0
	for _, x := range r.Runs {
		if x.Load != nil && x.Load.WfPerSec != nil && *x.Load.WfPerSec > max {
			max = *x.Load.WfPerSec
		}
	}
	for _, x := range r.Runs {
		if x.Load != nil {
			rows = append(rows, fmt.Sprintf("%-6s %-40s %6.1f wf/s  p95 %s ms", x.Label, bar(deref(x.Load.WfPerSec), max, 40), deref(x.Load.WfPerSec), fmtMS(x.Load.WorkflowLat.P95MS)))
		}
	}
	if len(rows) > 0 {
		m.ln("```text\nThroughput by concurrency\n" + strings.Join(rows, "\n") + "\n```\n")
	}
}

func burstExtra(m *md, r *ScenarioResult) {
	if len(r.Runs) == 0 || r.Runs[0].Load == nil {
		return
	}
	rec := r.Runs[0]
	tl := rec.Load.Timeline
	maxC := 0
	for _, p := range tl {
		if p.Completed > maxC {
			maxC = p.Completed
		}
	}
	back := map[int]int{}
	maxB := 0
	for _, sm := range rec.Resources.Series {
		back[int(sm.T)] = sm.Backlog
		if sm.Backlog > maxB {
			maxB = sm.Backlog
		}
	}
	var rows []string
	for _, p := range tl {
		rows = append(rows, fmt.Sprintf("%3ds %-30s %3d wf/s | backlog %-24s %d", p.T, bar(float64(p.Completed), float64(maxC), 30), p.Completed, bar(float64(back[p.T]), float64(maxB), 24), back[p.T]))
	}
	m.ln("```text\nCompleted workflows per second and background backlog (jobs not yet completed)\n" + strings.Join(rows, "\n") + "\n```\n")
}

func soakExtra(m *md, r *ScenarioResult) {
	if len(r.Runs) == 0 || r.Runs[0].Load == nil {
		return
	}
	var rows []string
	maxT := 0.0
	full := fullWindows(r.Runs[0].Load.Windows)
	for _, w := range full {
		maxT = maxFloat(maxT, deref(w.CompletedPerSec))
	}
	for _, w := range full {
		rows = append(rows, fmt.Sprintf("%-13s %-40s %6.1f workflows/s  p95 %s ms", fmt.Sprintf("%.0f-%.0f s", w.StartS, w.EndS), bar(deref(w.CompletedPerSec), maxT, 40), deref(w.CompletedPerSec), fmtMS(w.WorkflowLatency.P95MS)))
	}
	m.ln("```text\nWorkflow throughput per full window (partial tail windows omitted)\n" + strings.Join(rows, "\n") + "\n```\n")
	rs := r.Runs[0].Resources
	t := Table{Title: "Process memory and CPU during the sustained run", Head: []string{"Process", "CPU avg (cores)", "CPU max 1 s (cores)", "RSS start MB", "RSS max MB", "RSS end MB"}}
	for _, ps := range rs.Procs {
		t.Rows = append(t.Rows, []string{ps.Name, fmt.Sprintf("%.2f", ps.CPUAvgCores), fmt.Sprintf("%.2f", ps.CPUMaxCores), fmt.Sprintf("%.0f", ps.RSSStartMB), fmt.Sprintf("%.0f", ps.RSSMaxMB), fmt.Sprintf("%.0f", ps.RSSEndMB)})
	}
	m.table(t)
}

func maxFloat(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

func pipelineExtra(m *md, r *ScenarioResult) {
	if len(r.Runs) == 0 || r.Runs[0].Load == nil {
		return
	}
	pipelineDetail(m, r.Runs[0])
}

func pipelineDetail(m *md, rec *RunRecord) {
	m.f("**Stage timings (milliseconds).** *Outbox publication* is the time from the outbox row being created (inside the payment transaction) to Forge accepting the job: p50 %s, p95 %s, p99 %s (%d events). *Queue wait* and *execution* use Forge's own job timestamps (created -> started, started -> finished) for the jobs the poller observed; *effect latency* is outbox event to business-effect row, from ForgeCart's database.\n\n",
		fmtMS(rec.Outbox.P50MS), fmtMS(rec.Outbox.P95MS), fmtMS(rec.Outbox.P99MS), rec.Outbox.Count)
	t := Table{Head: []string{"Job type", "Jobs observed in Forge", "Queue wait p50/p95/p99", "Execution p50/p95/p99", "Effect latency p50/p95/p99 (event -> effect)"}}
	for _, ts := range rec.Forge.Types {
		var short string
		for _, w := range WorkerTypes {
			if w.Type == ts.Type {
				short = w.Short
			}
		}
		ef := rec.Effects[short]
		t.Rows = append(t.Rows, []string{ts.Type, itoa(ts.Observed), tri(ts.QueueWait), tri(ts.Execution), tri(ef)})
	}
	m.table(t)
	m.f("Forge job observation: %d polls (%d failed, %d where all 100 listed jobs were new and some may have been missed); poll latency p50 %s ms. %s\n\n", rec.Forge.Polls, rec.Forge.PollErrors, rec.Forge.FullWindowPolls, fmtMS(rec.Forge.PollLatency.P50MS), rec.Forge.CoverageNote)
	if rec.Drain != nil {
		m.f("**Drain.** %s. The drain check ran %d count queries (mean %s ms each) on ForgeCart's database; that is verification overhead, separate from the HTTP load.\n\n", drainStr(rec.Drain), rec.Drain.PollQueries, fmtMS(rec.Drain.PollMeanMS))
	}
}

func tri(l LatencyStats) string {
	if l.Count == 0 {
		return "—"
	}
	return fmt.Sprintf("%s / %s / %s", fmtMS(l.P50MS), fmtMS(l.P95MS), fmtMS(l.P99MS))
}

// ---------- 12 workers ----------

func bestPipelineRun(s *SuiteRecord) (*RunRecord, string) {
	for _, n := range []string{"pipeline", "baseline", "smoke"} {
		if r := s.find(n); r != nil {
			for _, x := range r.Runs {
				if x.Error == "" && x.Load != nil && len(x.Forge.Types) > 0 {
					return x, n + " / " + x.Label
				}
			}
		}
	}
	return nil, ""
}

func sectionWorkers(m *md, s *SuiteRecord) {
	m.ln("## 12. Per-worker throughput and latency comparison\n")
	rec, from := bestPipelineRun(s)
	if rec == nil {
		m.ln("*No pipeline run was available (the pipeline, baseline and smoke scenarios did not run).* \n")
		return
	}
	m.f("Source: %s. Rates are completed jobs / the pipeline span from the first outbox event to the last effect (%s s), which includes the HTTP load (pipeline span rate, jobs/s), not a worker-capacity measure.\n\n", from, fmtF(spanSecs(rec)))
	t := Table{Head: []string{"Worker", "Effects recorded", "Effects/s over the pipeline span", "Job starts (logs)", "Retries (logs)", "Execution p50/p95 from worker logs ms", "Execution p50/p95 from Forge timestamps ms", "Queue wait p95 ms (Forge)"}}
	for _, w := range WorkerTypes {
		var wl WorkerLogStats
		for _, x := range rec.Workers {
			if x.Short == w.Short {
				wl = x
			}
		}
		var ts TypeJobStats
		for _, x := range rec.Forge.Types {
			if x.Type == w.Type {
				ts = x
			}
		}
		t.Rows = append(t.Rows, []string{w.Type, itoa(rec.Pipeline.Effects[w.Short]), fmt.Sprintf("%.1f", rec.EffectRate[w.Short]), itoa(wl.Started), itoa(wl.Retries),
			fmtMS(wl.Exec.P50MS) + " / " + fmtMS(wl.Exec.P95MS), fmtMS(ts.Execution.P50MS) + " / " + fmtMS(ts.Execution.P95MS), fmtMS(ts.QueueWait.P95MS)})
	}
	m.table(t)
	m.ln("Retry counts come from the workers' own logs because Forge's public API does not expose attempt numbers.\n")
}

func spanSecs(r *RunRecord) *float64 {
	if r.Span == nil {
		return nil
	}
	return &r.Span.Seconds
}

// ---------- 15 integrity ----------

func sectionIntegrity(m *md, s *SuiteRecord) {
	m.ln("## 15. Business-effect and database-integrity verification\n")
	m.ln("After every trial the suite ran read-only integrity queries against ForgeCart's database (after the measurement window; their duration is recorded per trial): stock ledger balance, no negative stock, one set of events exactly for paid orders and none otherwise, payment-state consistency, one effect per order and per event for each of the workers, invoice/fulfillment snapshot consistency, no failed outbox events and (when drained) every event published and every effect present. The suite never reads or writes Forge's job tables.\n")
	t := Table{Head: []string{"Scenario", "Trials verified", "Checks run", "Checks failed", "Verification time (s)"}}
	for _, r := range s.Scenarios {
		tr, ch, bad := 0, 0, 0
		vt := 0.0
		for _, x := range r.Runs {
			if len(x.Checks) == 0 {
				continue
			}
			tr++
			ch += len(x.Checks)
			vt += x.VerifyS
			for _, c := range x.Checks {
				if !c.OK {
					bad++
				}
			}
		}
		if tr > 0 {
			t.Rows = append(t.Rows, []string{r.Name, itoa(tr), itoa(ch), itoa(bad), fmt.Sprintf("%.2f", vt)})
		}
	}
	m.table(t)
	var failed []string
	for _, r := range s.Scenarios {
		for _, x := range r.Runs {
			for _, c := range x.Checks {
				if !c.OK {
					failed = append(failed, fmt.Sprintf("%s/%s: %s — %s", r.Name, x.Label, c.Name, c.Detail))
				}
			}
		}
	}
	if len(failed) == 0 {
		m.ln("**No integrity check failed in any executed trial.**\n")
	} else {
		m.ln("**Failed integrity checks:**\n")
		for _, f := range failed {
			m.f("- %s\n", f)
		}
		m.ln("")
	}
}

// ---------- 16 resources ----------

func sectionResources(m *md, s *SuiteRecord) {
	m.ln("## 16. CPU, memory, database and connection utilization\n")
	m.ln("Sources: /proc for the host and every service process, `docker stats` for the two PostgreSQL containers (sampled every 5 s), `pg_stat_activity` for connections, all sampled about once per second during the run and drain. Container CPU is a percentage of one core (200% = two cores). Values marked — were not available.\n")
	t := Table{Head: []string{"Scenario / trial", "Host CPU avg/max % (first sample excluded)", "Forge DB CPU avg/max %", "App DB CPU avg/max %", "App DB mem max MB", "Max connections app/forge (limit)", "Max backlog", "Top service processes by avg CPU (cores)", "Sampler cycle ms"}}
	for _, r := range s.Scenarios {
		for _, x := range r.Runs {
			if x.Load == nil || x.Resources.Samples == 0 {
				continue
			}
			rs := x.Resources
			procs := append([]ProcSummary(nil), rs.Procs...)
			sort.Slice(procs, func(i, j int) bool { return procs[i].CPUAvgCores > procs[j].CPUAvgCores })
			var top []string
			for i, p := range procs {
				if i >= 3 {
					break
				}
				top = append(top, fmt.Sprintf("%s %.2f", p.Name, p.CPUAvgCores))
			}
			hostAvg, hostMax, _ := hostCPUStats(rs)
			t.Rows = append(t.Rows, []string{r.Name + "/" + x.Label, fmt.Sprintf("%.0f / %.0f", hostAvg, hostMax),
				containerCPUCell(rs, "forge-db"), containerCPUCell(rs, "app-db"),
				fmt.Sprintf("%.0f", rs.ContainerMemMaxMB["app-db"]), fmt.Sprintf("%d / %d (%d)", rs.MaxConnsApp, rs.MaxConnsForge, rs.ServerMaxConns), itoa(rs.MaxBacklog), strings.Join(top, ", "), fmtMS(rs.SamplerQueryMS)})
		}
	}
	m.table(t)
	m.ln(containerCPUFootnote + "\n")
	m.ln("The load generator's own CPU use and the client-side gap between the order and payment requests are recorded per trial; a warning is raised when either suggests the generator limited the run:\n")
	g := Table{Head: []string{"Scenario / trial", "Generator CPU (avg cores)", "Client gap p99 (µs)", "Generator a possible bottleneck?"}}
	for _, r := range s.Scenarios {
		for _, x := range r.Runs {
			if x.Load != nil && x.Load.ElapsedS > 0 && x.Load.Started > 0 {
				g.Rows = append(g.Rows, []string{r.Name + "/" + x.Label, fmt.Sprintf("%.2f", x.Load.GeneratorCores), fmtF(x.Load.GapP99US), yn(x.Load.GeneratorSuspect)})
			}
		}
	}
	m.table(g)
}

// ---------- 17 errors ----------

func sectionErrors(m *md, s *SuiteRecord) {
	m.ln("## 17. Errors, failed acceptance criteria and inconclusive runs\n")
	any := false
	for _, r := range s.Scenarios {
		if r.Status == "passed" {
			continue
		}
		any = true
		m.f("### %s — %s\n\n", r.Title, statusIcon(r.Status))
		for _, x := range r.Reasons {
			m.f("- %s\n", x)
		}
		for _, run := range r.Runs {
			if run.Error != "" {
				m.f("- trial %s could not run: %s\n", run.Label, run.Error)
			}
		}
		m.ln("")
	}
	var errs []string
	for _, r := range s.Scenarios {
		for _, x := range r.Runs {
			if x.Load == nil {
				continue
			}
			for k, v := range x.Load.Errors {
				errs = append(errs, fmt.Sprintf("%s/%s: %s ×%d", r.Name, x.Label, k, v))
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		m.ln("**HTTP errors recorded by the generator** (sanitized categories; business rejections such as stock conflicts and payment declines are not errors):\n")
		for _, e := range errs {
			m.f("- %s\n", e)
		}
		m.ln("")
		any = true
	}
	if !any {
		m.ln("No scenario failed, errored or was inconclusive, and the generator recorded no HTTP errors.\n")
	}
}

// ---------- 18 bottlenecks ----------

func sectionBottlenecks(m *md, s *SuiteRecord) {
	m.ln("## 18. Bottleneck analysis (evidence and hypotheses)\n")
	m.ln("This section separates **measured facts** from **hypotheses**. A resource that is busy when throughput stops growing is a candidate, not proof: all components share one host here, so CPU contention between them cannot be separated from a limit inside any one of them without a follow-up experiment.\n")
	any := false
	for _, name := range []string{"ramp", "saturation"} {
		r := s.find(name)
		if r == nil || len(r.Runs) < 2 {
			continue
		}
		last := r.Runs[len(r.Runs)-1]
		if last.Load == nil || last.Resources.Samples == 0 {
			continue
		}
		any = true
		m.f("### %s: the most loaded stage (%s)\n\n", name, last.Label)
		rs := last.Resources
		type comp struct {
			name string
			cpu  float64
		}
		var cs []comp
		for _, p := range rs.Procs {
			cs = append(cs, comp{p.Name, p.CPUAvgCores * 100})
		}
		cs = append(cs, comp{"postgres container forge-db", rs.ContainerCPUAvg["forge-db"]}, comp{"postgres container app-db", rs.ContainerCPUAvg["app-db"]}, comp{"load generator", last.Load.GeneratorCores * 100})
		sort.Slice(cs, func(i, j int) bool { return cs[i].cpu > cs[j].cpu })
		m.ln("*Measured:* average CPU by component during that stage (percent of one core):\n")
		for i, c := range cs {
			if i >= 6 {
				break
			}
			m.f("- %s: %.0f%%\n", c.name, c.cpu)
		}
		hostAvg, hostMax, _ := hostCPUStats(rs)
		m.f("- host CPU average %.0f%% (max %.0f%%) of %d logical CPUs; ForgeCart DB connections peaked at %d, Forge DB at %d of %d; max backlog %d jobs; workflow p95 %s ms.\n\n",
			hostAvg, hostMax, rs.HostCPUs, rs.MaxConnsApp, rs.MaxConnsForge, rs.ServerMaxConns, rs.MaxBacklog, fmtMS(last.Load.WorkflowLat.P95MS))
		hyp := []string{}
		if hostAvg >= 85 {
			hyp = append(hyp, "the host CPU is close to saturation, so the limit may be the machine rather than any single service; run the same stage with the load generator and services pinned to separate cores, or on a larger host, to check")
		}
		if cs[0].cpu >= 150 {
			hyp = append(hyp, fmt.Sprintf("%s used more than 1.5 cores on average and is the heaviest component; if throughput stopped scaling here, it is the first candidate (follow-up: profile it at this stage)", cs[0].name))
		}
		if rs.ContainerCPUAvg["app-db"] >= 0.8*float64(parseCPUs(s.Env.PgCPUs))*100 || rs.ContainerCPUAvg["forge-db"] >= 0.8*float64(parseCPUs(s.Env.PgCPUs))*100 {
			hyp = append(hyp, "a PostgreSQL container used at least 80% of its CPU allowance, which could limit checkout, the outbox or Forge's job table (follow-up: raise the container CPU limit and repeat)")
		}
		if rs.MaxConnsApp >= rs.ServerMaxConns*8/10 || rs.MaxConnsForge >= rs.ServerMaxConns*8/10 {
			hyp = append(hyp, "database connections approached max_connections")
		}
		if last.Load.GeneratorSuspect {
			hyp = append(hyp, "the load generator may itself have limited this stage")
		}
		if len(hyp) == 0 {
			m.ln("*Hypotheses:* none supported: no component was close to a measured limit at this stage.\n")
		}
		for _, h := range hyp {
			m.f("- *Hypothesis:* %s.\n", h)
		}
		m.ln("")
	}
	if sc := s.find("worker-scaling"); sc != nil && len(sc.Runs) > 1 {
		any = true
		m.ln("### Worker scaling\n")
		m.ln("*Measured:* see section 13 for jobs/s per configuration and efficiency. If efficiency falls well below 100% while worker CPU stays low and a database container or Forge is busy, the limit is more likely shared (Forge claim API or PostgreSQL) than the workers; if worker CPU is high, the workers' host share is the limit. These are hypotheses to test by repeating the largest configuration with a larger PostgreSQL CPU allowance.\n")
	}
	if p := s.find("pipeline"); p != nil && len(p.Runs) > 0 && p.Runs[0].Load != nil {
		any = true
		r := p.Runs[0]
		m.f("### Background pipeline\n\n*Measured:* outbox publication p95 %s ms; longest Forge queue wait p95 among types %s ms; drain %s. A long queue wait with short execution means jobs wait for a free worker slot (capacity) rather than run slowly.\n\n", fmtMS(r.Outbox.P95MS), maxQueueWait(r), drainShort(r.Drain))
	}
	if !any {
		m.ln("There was not enough multi-stage data (the ramp, saturation, scaling or pipeline scenarios did not run) to attribute any slowdown to a component.\n")
	}
}

func parseCPUs(s string) int {
	var n int
	fmt.Sscan(s, &n)
	if n < 1 {
		return 1
	}
	return n
}

func maxQueueWait(r *RunRecord) string {
	best := 0.0
	for _, t := range r.Forge.Types {
		if v := deref(t.QueueWait.P95MS); v > best {
			best = v
		}
	}
	return fmt.Sprintf("%.0f", best)
}

// ---------- 19 recommendations ----------

func sectionRecommend(m *md, s *SuiteRecord) {
	m.ln("## 19. Recommended corrections and next experiments\n")
	m.ln("Recommendations follow only from this run's evidence.\n")
	n := 0
	rec := func(format string, a ...any) { n++; m.f("%d. %s\n", n, fmt.Sprintf(format, a...)) }
	for _, r := range s.Scenarios {
		if r.Status == "failed" {
			rec("**Investigate the failed %s scenario first** (section 17): %s.", r.Name, strings.Join(firstN(r.Reasons, 2), "; "))
		}
	}
	if sat := s.find("saturation"); sat != nil && len(okRuns(sat)) > 0 {
		_, fs := displayParts(sat)
		for _, f := range fs {
			if strings.HasPrefix(f, "The throughput plateau is already reached at the first tested") {
				rec("The saturation ladder starts at or above its knee, so it cannot locate it: add lower concurrency stages (a scenario-definition change that was not made as part of the reporting corrections).")
			}
		}
		rec("Re-run the saturation scenario with longer stages (`--scale 2`) and a larger PostgreSQL allowance, and compare which resource saturates first (section 16).")
	}
	if rp := s.find("ramp"); rp != nil {
		_, fs := displayParts(rp)
		for _, f := range fs {
			if strings.HasPrefix(f, "First step with a scaling limit: ") {
				rec("The ramp flagged a scaling limit (%s). Repeat that stage and the one before it three times each to confirm it is not run-to-run noise (baseline spread is in section 5).", strings.TrimPrefix(f, "First step with a scaling limit: "))
			}
		}
	}
	if sc := s.find("worker-scaling"); sc != nil && len(sc.Runs) > 1 {
		rec("Use the worker-scaling efficiency (section 13) to decide where adding workers stops paying off; repeat the largest configuration with more PostgreSQL CPU to test whether the database is the shared limit.")
	}
	if b := s.find("baseline"); b != nil {
		_, bf := displayParts(b)
		for _, f := range bf {
			rec("%s Increase the repetitions before drawing conclusions from small differences.", f)
		}
	}
	for _, r := range s.Scenarios {
		if r.Status == "skipped" || r.Status == "inconclusive" {
			rec("Complete the %s scenario (%s): %s.", r.Name, r.Status, strings.Join(firstN(r.Reasons, 1), "; "))
		}
	}
	rec("Forge's public API lists only the newest 100 jobs and exposes no attempt count; at high job rates some jobs are not observed (section 11 gives the coverage). A paginated or filtered job-list endpoint, or attempt and error fields on job responses, would make queue-wait and retry measurements complete. This is a proposal for a separate Forge task, not something this suite changed.")
	rec("Next experiment: an open-loop (fixed arrival rate) scenario around the highest stable closed-loop point, to see whether latency stays flat at that rate and how much queueing the closed loop was hiding.")
	m.ln("")
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------- 20 limits ----------

func sectionLimits(m *md, s *SuiteRecord) {
	m.ln("## 20. Limitations and unmeasured metrics\n")
	for _, l := range []string{
		"All components (load generator, Forge, ForgeCart, dispatcher, the workers and Docker) share one host, so they compete for CPU and memory; the PostgreSQL containers use tmpfs storage, so disk I/O cost is not represented.",
		"Closed-loop load only: the offered rate follows the system's speed, which can hide queueing delay (coordinated omission). An open-loop scenario is not part of this suite yet.",
		"Single runs unless a scenario states repetitions; only the baseline is repeated. Differences smaller than the baseline spread are not evidence.",
		"The stability thresholds of the saturation scenario are configurable heuristics, and a stage passes them in a single 12-second window; this is not a sustainable-capacity measurement.",
		"Forge job timing is available only for jobs the poller saw in the newest-100 listing (see coverage notes); retries come from worker logs; Forge's public API has no attempt counts, queue depth or filtering.",
		"Backlog (jobs created but not yet completed) is derived from ForgeCart's database (5 x paid orders - effects) sampled about once a second; it is a proxy for Forge's queue depth, which is not exposed.",
		"Process CPU/memory come from /proc and container figures from `docker stats` (every 5 s); PostgreSQL internals such as lock waits or per-query timings are not collected (only connection counts and the deadlock counter).",
		"Fault injection is limited to controlled process stops/kills and a 3-second database container pause inside the sandbox; network partitions, disk-full and clock faults were not exercised.",
		"The sandbox runs services as host processes built from the repository; the repository has no container images for them.",
		"No performance objectives were specified by the owner, so no throughput or latency target was judged.",
	} {
		m.f("- %s\n", l)
	}
	if s.Options.Race {
		m.ln("- Services were built with the race detector, which slows them substantially; these numbers are not representative performance.")
	}
	m.ln("")
}

// displayParts returns the tables and findings to print for a scenario. The
// data-driven ones are recomputed from the raw trial records with the same
// functions the runners use, so a report regenerated from an older suite.json
// shows the corrected calculations rather than the text stored at run time.
func displayParts(r *ScenarioResult) ([]Table, []string) {
	tables := append([]Table(nil), r.Tables...)
	findings := append([]string(nil), r.Findings...)
	if len(okRuns(r)) == 0 {
		return tables, findings
	}
	replaceTable := func(prefix string, nt Table) {
		for i := range tables {
			if strings.HasPrefix(tables[i].Title, prefix) {
				tables[i] = nt
				return
			}
		}
		tables = append(tables, nt)
	}
	// withDerived puts the freshly derived findings first and removes stored ones that
	// are stale (old wording) or identical to a derived one (stored by a newer run).
	var dropFindings func(prefixes ...string)
	withDerived := func(derived []string, stalePrefixes ...string) {
		dropFindings(stalePrefixes...)
		var kept []string
		for _, f := range findings {
			dup := false
			for _, d := range derived {
				dup = dup || f == d
			}
			if !dup {
				kept = append(kept, f)
			}
		}
		findings = append(append([]string(nil), derived...), kept...)
	}
	dropFindings = func(prefixes ...string) {
		var kept []string
		for _, f := range findings {
			drop := false
			for _, p := range prefixes {
				drop = drop || strings.HasPrefix(f, p)
			}
			if !drop {
				kept = append(kept, f)
			}
		}
		findings = kept
	}
	switch r.Kind {
	case "smoke", "stock", "pipeline":
		for i := range tables {
			if tables[i].Head[0] == "Trial" && len(tables[i].Rows) == 1 {
				tables[i] = Table{Title: tables[i].Title, Head: trialHead, Rows: [][]string{trialRow(okRuns(r)[0])}}
			}
		}
		if r.Kind == "pipeline" {
			findings = pipelineFindings(okRuns(r)[0])
		}
	case "baseline":
		ts, f := deriveBaseline(r)
		replaceTable("Repetitions", ts[0])
		if len(ts) > 1 {
			replaceTable("Aggregation", ts[1])
		}
		withDerived(f, "Run-to-run throughput spread")
	case "ramp":
		tb, f := deriveRamp(r)
		replaceTable("Concurrency ramp", tb)
		withDerived(f, "First sign of scaling limits", "No scaling limit was observed")
	case "soak":
		tb, f := deriveSoak(r)
		replaceTable("Windows of the sustained run", tb)
		withDerived(f, "Throughput first third")
	case "saturation":
		sum := deriveSaturation(r)
		replaceTable("Saturation stages", sum.Table)
		findings = sum.Findings
	case "scaling":
		replaceTable("Worker scaling", deriveScaling(r))
	}
	return tables, findings
}

// selfChecks reconciles the report against the raw records it was built from.
func selfChecks(m *md, s *SuiteRecord) {
	counts := map[string]int{}
	for _, r := range s.Scenarios {
		counts[r.Status]++
	}
	ran := len(s.Scenarios) - counts["skipped"] - counts["error"]
	notRun := len(s.Selected) - len(s.Scenarios)
	m.f("**Report self-checks.** Scenario accounting: %d selected = %d executed (%d passed, %d failed, %d inconclusive, %d interrupted) + %d skipped + %d errored + %d not started.",
		len(s.Selected), ran, counts["passed"], counts["failed"], counts["inconclusive"], counts["interrupted"], counts["skipped"], counts["error"], notRun)
	if ran+counts["skipped"]+counts["error"]+notRun != len(s.Selected) {
		m.f(" **MISMATCH: the parts do not add up to the selection.**")
	}
	trials, worst := 0, 0.0
	var bad []string
	for _, r := range s.Scenarios {
		for _, x := range okRuns(r) {
			l := x.Load
			if l.ElapsedS <= 0 || l.WfPerSec == nil {
				continue
			}
			trials++
			d := math.Abs(float64(l.Completed)/l.ElapsedS - *l.WfPerSec)
			worst = math.Max(worst, d)
			// the stored elapsed time is truncated to 1 ms, which moves completed/elapsed by up to completed/elapsed^2 * 0.001
			bound := float64(l.Completed)/(l.ElapsedS*l.ElapsedS)*0.001 + 0.001
			if d > bound {
				bad = append(bad, fmt.Sprintf("%s/%s (%.3f > %.3f)", r.Name, x.Label, d, bound))
			}
		}
	}
	m.f(" Throughput recomputed as completed / elapsed for %d trials: largest difference from the stored value %.3f workflows/s; every difference is within the bound set by the stored elapsed time being truncated to 1 ms unless flagged below.", trials, worst)
	if len(bad) > 0 {
		m.f(" **MISMATCH in: %s.**", strings.Join(bad, ", "))
	}
	m.ln("\n")
}
