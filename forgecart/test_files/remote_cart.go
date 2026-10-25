package main

// The "remote-cart" command: ForgeCart's shopper workflows (create an order, pay,
// then the background jobs flow through Forge) against a ForgeCart + Forge
// deployment that is ALREADY RUNNING, using attach mode (attach.go) with the
// normal Trial machinery. It measures the foreground (shoppers' requests) and
// the background (how fast Forge and ForgeCart's workers finish the jobs after
// the load) separately, then runs ForgeCart's integrity checks on its database.
//
// It does not start, stop or configure anything: payment outcomes, worker
// counts and Forge settings are whatever the deployment has. It does EMPTY the
// ForgeCart business tables between trials, which is why it demands an explicit
// confirmation flag.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// RemoteCartConfig holds the non-secret settings of the run.
type RemoteCartConfig struct {
	CartURL       string `json:"cartforge_cart_url"`
	ForgeURL      string `json:"forge_url"`
	OperatorURL   string `json:"operator_url"`
	Levels        []int  `json:"shopper_concurrency_levels"`
	Workflows     int    `json:"workflows_per_trial"`
	DurationS     int    `json:"duration_s_per_trial,omitempty"`  // when set, each trial is a timed run and Workflows is ignored
	HotProducts   int    `json:"hot_products,omitempty"`          // when set, every shopper orders only the first N products (1 = one product for everyone)
	Inventory     int    `json:"inventory_per_product,omitempty"` // when set, stock per product at the start of each trial
	Repetitions   int    `json:"repetitions_per_level"`
	DrainTimeoutS int    `json:"drain_timeout_s"`
	Seed          int64  `json:"seed"`
	Note          string `json:"environment_note"`
	DBHost        string `json:"forgecart_database_host_erased_between_trials"`
}

type RemoteCartTrial struct {
	Level       int                `json:"shopper_concurrency"`
	Rep         int                `json:"repetition"`
	Record      *RunRecord         `json:"record"`
	ForgeDeltas map[string]float64 `json:"forge_counter_deltas,omitempty"`
	QueueWaitMS RemoteQ            `json:"forge_queue_wait_ms_bucket_estimate"`
	AttemptMS   RemoteQ            `json:"forge_attempt_duration_ms_bucket_estimate"`
}

type RemoteCartResult struct {
	RunID      string             `json:"run_id"`
	StartedAt  time.Time          `json:"started_at"`
	EndedAt    time.Time          `json:"ended_at"`
	Config     RemoteCartConfig   `json:"config"`
	Runner     map[string]any     `json:"load_generator_host"`
	Trials     []*RemoteCartTrial `json:"trials"`
	StopReason string             `json:"stop_reason,omitempty"`
}

// trialPhase is the load of one trial: a fixed number of workflows, or a timed run (a soak).
func trialPhase(rc RemoteCartConfig, label string, level int) Phase {
	if rc.DurationS > 0 {
		return Phase{Name: label, Concurrency: level, Duration: time.Duration(rc.DurationS) * time.Second}
	}
	return Phase{Name: label, Concurrency: level, Workflows: rc.Workflows}
}

func (c RemoteCartConfig) validate() error {
	if err := (RemoteConfig{ForgeURL: c.ForgeURL, OperatorURL: c.OperatorURL, Types: []string{"x"}, Rates: []float64{1}, StageSeconds: 5, Repetitions: 1, PayloadBytes: 64, DrainTimeoutS: 5}).validate(); err != nil {
		return err
	}
	if !strings.HasPrefix(c.CartURL, "http://") && !strings.HasPrefix(c.CartURL, "https://") {
		return fmt.Errorf("--cart-url must be http(s)://host:port")
	}
	switch {
	case len(c.Levels) == 0:
		return fmt.Errorf("--levels is required (shopper concurrency, for example 5,10,20)")
	case c.DurationS != 0 && (c.DurationS < 60 || c.DurationS > 7200):
		return fmt.Errorf("--duration-seconds must be 60 to 7200")
	case c.DurationS == 0 && (c.Workflows < 10 || c.Workflows > 100000):
		return fmt.Errorf("--workflows must be 10 to 100000")
	case c.HotProducts < 0 || c.HotProducts > 1000:
		return fmt.Errorf("--hot-products must be 0 to 1000")
	case c.Inventory < 0 || c.Inventory > 1_000_000_000:
		return fmt.Errorf("--inventory must be 0 to 1000000000")
	case c.Repetitions < 1 || c.Repetitions > 5:
		return fmt.Errorf("--repetitions must be 1 to 5")
	case c.DrainTimeoutS < 10 || c.DrainTimeoutS > 1800:
		return fmt.Errorf("--drain-timeout must be 10 to 1800 seconds")
	}
	for _, l := range c.Levels {
		if l < 1 || l > HardMaxConcurrency {
			return fmt.Errorf("shopper concurrency levels must be 1 to %d", HardMaxConcurrency)
		}
	}
	return nil
}

func RunRemoteCart(repo, runID string, rc RemoteCartConfig, att AttachConfig) int {
	if err := rc.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if att.APIKey == "" || att.OperatorKey == "" || att.AppDBURL == "" {
		fmt.Fprintln(os.Stderr, "error: set FORGE_API_KEY, FORGE_OPERATOR_API_KEY and FORGECART_ATTACH_APP_DB_URL (the ForgeCart database to erase between trials) in the environment")
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
	att.CartURL, att.ForgeURL, att.OperatorURL = rc.CartURL, rc.ForgeURL, rc.OperatorURL
	rc.DBHost = att.DBHost()
	logf("ATTACHED run %s: the ForgeCart tables in database %s will be EMPTIED before every trial", runID, rc.DBHost)

	res := &RemoteCartResult{RunID: runID, StartedAt: time.Now(), Config: rc,
		Runner: map[string]any{"cpu": cpuModel(), "logical_cpus": runtime.NumCPU(), "memory_mb": memTotalMB(), "go": runtime.Version()}}
	finish := func(code int) int {
		res.EndedAt = time.Now()
		_ = writeJSON(filepath.Join(runDir, "remote-cart.json"), res)
		_ = os.WriteFile(filepath.Join(runDir, "remote-cart-report.md"), []byte(RenderRemoteCart(res)), 0o644)
		logf("results: %s", runDir)
		return code
	}

	sb, err := NewAttachedSandbox(repo, runDir, runID, att)
	if err != nil {
		res.StopReason = err.Error()
		return finish(1)
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	f := &remoteForge{cfg: RemoteConfig{OperatorURL: rc.OperatorURL}, opKey: att.OperatorKey, hc: hc}
	f.op = &opClient{url: strings.TrimRight(rc.OperatorURL, "/") + "/v1/queues", auth: "Bearer " + att.OperatorKey, hc: hc}
	if _, _, _, err := f.queueDepth(ctx); err != nil {
		res.StopReason = "Forge operator API not usable: " + err.Error()
		return finish(1)
	}

	env := NewEnv(ctx, sb, Options{ObserveEvery: 100}, repo, runDir, logf)
	defer func() {
		if env.Probe != nil {
			env.Probe.Close()
		}
	}()
	cfg := DefaultSandboxConfig()
	if rc.Inventory > 0 {
		cfg.Inventory = rc.Inventory
	}
	n := len(rc.Levels)
	trialNo := 0
	for rep := 0; rep < rc.Repetitions; rep++ {
		for i := 0; i < n; i++ {
			level := rc.Levels[(i+rep)%n]
			if ctx.Err() != nil {
				res.StopReason = "interrupted"
				return finish(1)
			}
			trialNo++
			label := fmt.Sprintf("shoppers-%d-r%d", level, rep+1)
			s0, serr := f.scrape(ctx)
			rec := env.Trial(TrialSpec{Label: label, Cfg: cfg, Seed: rc.Seed + int64(trialNo), Tag: fmt.Sprintf("rc%d", trialNo), Drain: true, HotProducts: rc.HotProducts,
				DrainTimeout: time.Duration(rc.DrainTimeoutS) * time.Second,
				Phases:       []Phase{trialPhase(rc, label, level)}})
			t := &RemoteCartTrial{Level: level, Rep: rep + 1, Record: rec}
			if s2, err := f.scrape(ctx); serr == nil && err == nil && s2.startedS == s0.startedS {
				t.ForgeDeltas = map[string]float64{}
				for k, v := range s2.counters {
					if d := v - s0.counters[k]; d != 0 {
						t.ForgeDeltas[k] = d
					}
				}
				t.QueueWaitMS = quantilesFromDelta(s0.bucketsQ, s2.bucketsQ)
				t.AttemptMS = quantilesFromDelta(s0.bucketsA, s2.bucketsA)
			}
			res.Trials = append(res.Trials, t)
			if rec.Error != "" {
				res.StopReason = "trial " + label + " failed: " + rec.Error
				logf("%s", res.StopReason)
				return finish(1)
			}
			logf("trial %s: %.1f workflows/s, drain %s", label, deref(rec.Load.WfPerSec), drainBrief(rec.Drain))
		}
	}
	code := 0
	for _, t := range res.Trials {
		if len(failedChecks(t.Record)) > 0 || (t.Record.Drain != nil && !t.Record.Drain.Drained) {
			code = 1
		}
	}
	return finish(code)
}

func drainBrief(d *DrainResult) string {
	if d == nil {
		return "n/a"
	}
	if !d.Drained {
		return fmt.Sprintf("NOT drained in %.0f s", d.TimeoutS)
	}
	return fmt.Sprintf("%.1f s (%.0f jobs/s)", d.WaitedS, d.DrainRate)
}

func failedChecks(r *RunRecord) []string {
	var out []string
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c.Name+" ("+c.Detail+")")
		}
	}
	return out
}

func ms(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *p)
}

func RenderRemoteCart(r *RemoteCartResult) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# ForgeCart on Forge, attached run `%s`", r.RunID)
	w("")
	w("Started %s, ended %s. Raw data: `remote-cart.json`.", r.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"), r.EndedAt.UTC().Format("15:04:05 UTC"))
	w("")
	w("## What this is")
	w("")
	w("Simulated shoppers (create an order, pay it) use an **already running** ForgeCart; each paid order produces background jobs that flow through the Forge it is connected to. The **foreground** figures are the shoppers' requests; the **background** figures are how fast Forge and ForgeCart's workers finish the jobs after the load ends. Nothing was started, stopped or configured by this tool: payment outcomes, worker counts and Forge settings are whatever the deployment has. Between trials the tool waited for Forge to go idle and **emptied ForgeCart's business tables** in the database `%s`, then reset stock.", r.Config.DBHost)
	w("")
	w("| Setting | Value |")
	w("|---|---|")
	w("| ForgeCart | %s |", r.Config.CartURL)
	w("| Forge (submission / operator) | %s / %s |", r.Config.ForgeURL, r.Config.OperatorURL)
	if r.Config.DurationS > 0 {
		w("| Shopper concurrency levels | %v, each run for %d s (a timed run), %d repetition(s), order rotated each repetition |", r.Config.Levels, r.Config.DurationS, r.Config.Repetitions)
	} else {
		w("| Shopper concurrency levels | %v, %d workflows per trial, %d repetition(s), order rotated each repetition |", r.Config.Levels, r.Config.Workflows, r.Config.Repetitions)
	}
	if r.Config.HotProducts > 0 {
		w("| Product choice | every shopper orders only the first %d product(s) by SKU (a stampede on a few rows), not the whole catalogue |", r.Config.HotProducts)
	}
	if r.Config.Inventory > 0 {
		w("| Stock per product at the start of each trial | %d |", r.Config.Inventory)
	}
	w("| Load generator host | %v, %v logical CPUs, %v MB |", r.Runner["cpu"], r.Runner["logical_cpus"], r.Runner["memory_mb"])
	if r.Config.Note != "" {
		w("| Deployment (operator's description, unverified) | %s |", r.Config.Note)
	}
	w("")
	if r.StopReason != "" {
		w("**Run stopped:** %s", r.StopReason)
		w("")
	}
	w("## Results")
	w("")
	w("| Trial | Foreground: workflows/s | Workflow p50 / p95 / p99 (ms) | Failed workflows | Background drain after load | Forge queue wait p50 / p95 (ms) | Forge execution p50 / p95 (ms) | Integrity checks |")
	w("|---|---|---|---|---|---|---|---|")
	for _, t := range r.Trials {
		rec := t.Record
		if rec.Error != "" || rec.Load == nil {
			w("| shoppers-%d-r%d | error: %s | | | | | | |", t.Level, t.Rep, rec.Error)
			continue
		}
		var qw50, qw95, ex50, ex95 []float64
		for _, ty := range rec.Forge.Types {
			if ty.QueueWait.P50MS != nil && ty.QueueWait.P95MS != nil {
				qw50, qw95 = append(qw50, *ty.QueueWait.P50MS), append(qw95, *ty.QueueWait.P95MS)
			}
			if ty.Execution.P50MS != nil && ty.Execution.P95MS != nil {
				ex50, ex95 = append(ex50, *ty.Execution.P50MS), append(ex95, *ty.Execution.P95MS)
			}
		}
		fc := failedChecks(rec)
		chk := fmt.Sprintf("%d of %d pass", len(rec.Checks)-len(fc), len(rec.Checks))
		w("| shoppers-%d-r%d | %s | %s / %s / %s | %d | %s | %.0f / %.0f | %.0f / %.0f | %s |", t.Level, t.Rep, ms(rec.Load.WfPerSec),
			ms(rec.Load.WorkflowLat.P50MS), ms(rec.Load.WorkflowLat.P95MS), ms(rec.Load.WorkflowLat.P99MS), rec.Load.Failed, drainBrief(rec.Drain),
			med(qw50), maxOf(qw95), med(ex50), maxOf(ex95), chk)
	}
	w("")
	w("Queue wait and execution come from Forge's own job timestamps for the jobs the observer saw (median of the per-type medians; the larger of the per-type p95s). Forge's `/metrics` view of the same trials:")
	w("")
	w("| Trial | Forge counter changes | Queue wait p50 / p95 (ms, bucket estimate) | Attempt duration p50 / p95 (ms, bucket estimate) |")
	w("|---|---|---|---|")
	for _, t := range r.Trials {
		keys := make([]string, 0, len(t.ForgeDeltas))
		for k := range t.ForgeDeltas {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%.0f", k, t.ForgeDeltas[k]))
		}
		w("| shoppers-%d-r%d | %s | %.0f / %.0f | %.0f / %.0f |", t.Level, t.Rep, strings.Join(parts, ", "), t.QueueWaitMS.P50, t.QueueWaitMS.P95, t.AttemptMS.P50, t.AttemptMS.P95)
	}
	w("")
	for _, t := range r.Trials {
		if fc := failedChecks(t.Record); len(fc) > 0 {
			w("**Integrity check failures in shoppers-%d-r%d:**", t.Level, t.Rep)
			for _, c := range fc {
				w("- %s", c)
			}
			w("")
		}
	}
	w("## Limits of this measurement")
	w("")
	w("- Foreground throughput is limited by ForgeCart itself (its web server, its database, its payment step) as well as by the load generator; it is not a Forge measurement. Forge's part is the background columns.")
	w("- Worker logs, process CPU and container figures are not available for a deployment this tool does not manage, so the retry counts and resource tables of the local report are absent.")
	w("- Integrity checks read the ForgeCart database (and the results database, if the run was given one); they prove ForgeCart's own invariants (stock, payments, one event per job type, one effect per event), not Forge's internals.")
	w("- The numbers describe this deployment, this day and this load generator. They are observations, not capacity guarantees.")
	return b.String()
}

func med(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

var _ = context.Background
