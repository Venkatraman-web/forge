package main

// Experiment E-1: Forge-isolated capacity baseline (docs/decisions/0004, section 8.2).
//
// Jobs are submitted DIRECTLY to Forge's POST /v1/jobs by an open-loop (fixed
// arrival rate) generator and executed by SDK workers running a synthetic
// handler. There is no ForgeCart process, database, outbox or dispatcher in the
// run. Queue state is observed through the authenticated operator endpoint
// (GET /v1/queues); latencies come from the job rows' own timestamps; the final
// state is reconciled against the database.
//
// Everything runs on one host, so this isolates Forge from ForgeCart's business
// pipeline; it is NOT a multi-host or production capacity test.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// E1Config is the experiment definition. Every field is recorded in the result.
type E1Config struct {
	Rates            []float64 `json:"arrival_rates_jobs_per_s"`
	StageSeconds     int       `json:"stage_seconds"`
	SustainedFrac    float64   `json:"sustained_fraction_of_highest_sustainable_rate"`
	SustainedSeconds int       `json:"sustained_seconds"`
	HandlerMS        int       `json:"handler_ms"`
	PayloadBytes     int       `json:"payload_bytes_approx"`
	WorkerProcs      int       `json:"worker_processes"`
	SlotsPerProc     int       `json:"slots_per_process"`
	MaxQueued        int       `json:"forge_max_queued"`
	Types            []string  `json:"job_types"`
	OperatorPollMS   int       `json:"operator_poll_interval_ms"`
	DrainTimeoutS    int       `json:"drain_timeout_s"`
	MaxInFlight      int       `json:"generator_max_in_flight"`
	LateThresholdMS  int       `json:"generator_late_threshold_ms"`
	// Diagnostics adds read-only PostgreSQL telemetry (wait events, locks, the
	// oldest transaction, dead tuples and index scan counters) to the 1 Hz sampler.
	// Off by default: the baseline definition is unchanged without it.
	Diagnostics bool `json:"diagnostics_extra_postgres_telemetry"`
	// Repetitions runs every rate this many times, interleaved (each repetition
	// rotates the rate order by one) so a slow drift over the session does not
	// favour one rate. 1 = the original behaviour.
	Repetitions int `json:"repetitions_per_rate"`
	// NoWorkers is the admission-only probe: no worker process is started, so no
	// job is claimed or completed and the queue only grows. It isolates the
	// submission path from claiming and execution.
	NoWorkers bool `json:"admission_only_no_workers"`
	// Watchdog stops sending (the trial is then analysed and marked aborted) when
	// the trial can no longer be a valid measurement or threatens the host. Off
	// by default; the abort rules are listed in watchdogRules.
	Watchdog bool `json:"watchdog_abort_rules"`
	// Variant, ForgeBinary and MigrationsDir support a controlled A/B: the same
	// harness runs a prebuilt Forge binary (instead of building one from the tree)
	// against a chosen migrations directory. All three are recorded in the result.
	Variant       string `json:"variant,omitempty"`
	ForgeBinary   string `json:"prebuilt_forge_binary,omitempty"`
	MigrationsDir string `json:"migrations_dir,omitempty"`
}

// DefaultE1 holds the documented assumptions for everything the design leaves open.
func DefaultE1() E1Config {
	return E1Config{Rates: []float64{25, 50, 100, 200, 400, 800, 1600}, StageSeconds: 60, SustainedFrac: 0.8, SustainedSeconds: 600,
		HandlerMS: 25, PayloadBytes: 256, WorkerProcs: 2, SlotsPerProc: 24, MaxQueued: 200000, Types: []string{"bench.a", "bench.b"},
		OperatorPollMS: 1000, DrainTimeoutS: 300, MaxInFlight: 4000, LateThresholdMS: 50, Repetitions: 1}
}

const (
	e1MaxRate     = 5000
	e1MaxStageS   = 900
	e1MaxSustainS = 1800
)

func (c E1Config) validate() error {
	switch {
	case len(c.Rates) == 0 && c.SustainedSeconds == 0:
		return fmt.Errorf("nothing to run: give --rates or --sustained-seconds")
	case c.StageSeconds < 5 || c.StageSeconds > e1MaxStageS:
		return fmt.Errorf("--stage-seconds must be 5 to %d", e1MaxStageS)
	case c.SustainedSeconds < 0 || c.SustainedSeconds > e1MaxSustainS:
		return fmt.Errorf("--sustained-seconds must be 0 to %d", e1MaxSustainS)
	case c.HandlerMS < 0 || c.HandlerMS > 10000:
		return fmt.Errorf("--handler-ms must be 0 to 10000")
	case c.PayloadBytes < 64 || c.PayloadBytes > 64<<10:
		return fmt.Errorf("--payload-bytes must be 64 to 65536")
	case c.WorkerProcs < 1 || c.WorkerProcs > 8 || c.SlotsPerProc < 1 || c.SlotsPerProc > 64:
		return fmt.Errorf("--worker-procs must be 1-8 and --slots 1-64")
	case c.WorkerProcs*c.SlotsPerProc > 64:
		return fmt.Errorf("total slots (%d) exceed Forge's default worker poll cap of 64 (FORGE_WORKER_MAX_POLLS); this experiment keeps the default", c.WorkerProcs*c.SlotsPerProc)
	case c.MaxQueued < 1 || c.MaxQueued > 1_000_000:
		return fmt.Errorf("--max-queued must be 1 to 1000000")
	case c.Repetitions < 1 || c.Repetitions > 5:
		return fmt.Errorf("--repetitions must be 1 to 5")
	case c.NoWorkers && c.SustainedSeconds != 0:
		return fmt.Errorf("--no-workers has no sustainable-rate rule: use --sustained-seconds 0")
	case c.SustainedFrac <= 0 || c.SustainedFrac > 1:
		return fmt.Errorf("sustained fraction must be in (0,1]")
	}
	for _, r := range c.Rates {
		if r < 1 || r > e1MaxRate {
			return fmt.Errorf("arrival rates must be 1 to %d jobs/s", e1MaxRate)
		}
	}
	return nil
}

// ---------- results ----------

// E1Window summarises a latency distribution in milliseconds.
type E1Dist struct {
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
	N   int     `json:"n"`
}

type E1Submit struct {
	Offered         int            `json:"offered_planned"`
	Sent            int            `json:"sent"`
	Accepted        int            `json:"accepted_202"`
	QueueFull       int            `json:"queue_full_503"`
	QueueFullHint   int            `json:"queue_full_with_retry_after"`
	Other           map[string]int `json:"other_outcomes"`
	AcceptRatePerS  float64        `json:"accepted_per_s"`
	OfferedRatePerS float64        `json:"offered_per_s"`
	WindowS         float64        `json:"send_window_s"`
	Latency         E1Dist         `json:"submit_latency_accepted"`
	LatencyLastHalf E1Dist         `json:"submit_latency_accepted_last_half"`
	// Generator health: how far behind its own schedule the sends ran.
	LateP99MS     float64 `json:"send_lateness_p99_ms"`
	LateMaxMS     float64 `json:"send_lateness_max_ms"`
	LateOverCount int     `json:"sends_later_than_threshold"`
	InFlightCap   int     `json:"times_in_flight_cap_reached"`
	GeneratorCPU  float64 `json:"generator_cpu_cores_avg"`
	GeneratorRSS  float64 `json:"generator_rss_max_mb"`
	AcceptedPerS  []int   `json:"accepted_by_second"`
	// Windows are 30 s slices of the send schedule, so latency can be compared
	// against queue depth over time.
	Windows []E1Win `json:"by_30s_window"`
}

type E1Win struct {
	FromS    int     `json:"from_s"`
	Sent     int     `json:"sent"`
	Accepted int     `json:"accepted"`
	P50MS    float64 `json:"accepted_latency_p50_ms"`
	P99MS    float64 `json:"accepted_latency_p99_ms"`
}

type E1Poll struct {
	T          float64  `json:"t_s"`
	LatencyMS  float64  `json:"latency_ms"`
	Status     int      `json:"status"`
	Queued     int      `json:"queued"`
	Running    int      `json:"running"`
	OldestDueS *float64 `json:"oldest_due_job_age_s"`
	Rejections uint64   `json:"queue_full_rejections_since_process_start"`
}

type E1Sample struct {
	T           float64 `json:"t_s"`
	HostCPUPct  float64 `json:"host_cpu_pct"`
	ForgeCPU    float64 `json:"forge_cpu_cores"`
	WorkersCPU  float64 `json:"workers_cpu_cores"`
	GenCPU      float64 `json:"generator_cpu_cores"`
	ForgeRSSMB  float64 `json:"forge_rss_mb"`
	WorkersRSS  float64 `json:"workers_rss_mb"`
	GenRSSMB    float64 `json:"generator_rss_mb"`
	PGConnTotal int     `json:"pg_connections"`
	PGConnActiv int     `json:"pg_connections_active"`
	// Diagnostics (only when E1Config.Diagnostics): cumulative counters are as
	// PostgreSQL reports them; the report differences them per window.
	Diag *E1Diag `json:"diagnostics,omitempty"`
}

type E1Diag struct {
	Waits           map[string]int `json:"client_backends_by_wait_event_type_event_state"`
	LocksNotGranted int            `json:"locks_not_granted"`
	AdvisoryWaiting int            `json:"advisory_locks_not_granted"`
	OldestXactAgeS  float64        `json:"oldest_transaction_age_s"`
	MaxXminAge      int64          `json:"max_backend_xmin_age_xids"`
	LiveTup         int64          `json:"jobs_n_live_tup"`
	DeadTup         int64          `json:"jobs_n_dead_tup"`
	AutovacuumCount int64          `json:"jobs_autovacuum_count"`
	DueIdxScans     int64          `json:"jobs_due_idx_scans_cumulative"`
	DueIdxTupRead   int64          `json:"jobs_due_idx_entries_read_cumulative"`
	DueIdxTupFetch  int64          `json:"jobs_due_idx_heap_fetches_cumulative"`
}

type E1DB struct {
	Rows             int            `json:"rows"`
	ByStatus         map[string]int `json:"by_status"`
	ByType           map[string]int `json:"by_type"`
	RetriedAttempts  int            `json:"jobs_with_attempt_gt_1"`
	MaxAttempt       int            `json:"max_attempt"`
	WrongResult      int            `json:"succeeded_with_unexpected_result"`
	DistinctSeq      int            `json:"distinct_seq"`
	AcceptedMissing  int            `json:"accepted_but_absent_from_database"`
	DBOnly           int            `json:"in_database_but_not_acknowledged"`
	CompletedByEnd   int            `json:"finished_by_load_end"`
	FirstCreated     time.Time      `json:"first_created_at"`
	LastFinished     time.Time      `json:"last_finished_at"`
	QueueWait        E1Dist         `json:"queue_wait_all"`
	QueueWaitLast    E1Dist         `json:"queue_wait_last_half"`
	Exec             E1Dist         `json:"execution_all"`
	EndToEnd         E1Dist         `json:"end_to_end_all"`
	EndToEndLast     E1Dist         `json:"end_to_end_last_half"`
	CompletedBySec   []int          `json:"completed_by_second"`
	TableBytes       int64          `json:"jobs_table_total_bytes"`
	ForgeLogProblems int            `json:"forge_log_error_lines"`
}

type E1Resources struct {
	HostCPUAvg         float64 `json:"host_cpu_pct_avg"`
	HostCPUMax         float64 `json:"host_cpu_pct_max"`
	ForgeCPUAvg        float64 `json:"forge_cpu_cores_avg"`
	ForgeCPUMax        float64 `json:"forge_cpu_cores_max_1s"`
	WorkersCPUAvg      float64 `json:"workers_cpu_cores_avg_total"`
	WorkersCPUMax      float64 `json:"workers_cpu_cores_max_1s_total"`
	GenCPUAvg          float64 `json:"generator_cpu_cores_avg"`
	GenCPUMax          float64 `json:"generator_cpu_cores_max_1s"`
	ForgeRSSMax        float64 `json:"forge_rss_max_mb"`
	WorkersRSSMax      float64 `json:"workers_rss_max_mb_total"`
	GenRSSMax          float64 `json:"generator_rss_max_mb"`
	PGConnMax          int     `json:"postgres_connections_max"`
	PGActiveMax        int     `json:"postgres_active_connections_max"`
	PGCPUAvg           float64 `json:"postgres_container_cpu_pct_avg"` // percent of ONE core, from `docker stats`
	PGCPUMax           float64 `json:"postgres_container_cpu_pct_max"`
	PGCPUReadings      int     `json:"postgres_container_cpu_readings"` // taken about every 5 s: few readings = indicative only
	PGMemMaxMB         float64 `json:"postgres_container_memory_max_mb"`
	OperatorPollP50    float64 `json:"operator_poll_latency_p50_ms"`
	OperatorPollP95    float64 `json:"operator_poll_latency_p95_ms"`
	OperatorPollMax    float64 `json:"operator_poll_latency_max_ms"`
	OperatorPolls      int     `json:"operator_polls"`
	OperatorPollErrors int     `json:"operator_poll_errors"`
}

type E1Stage struct {
	Label            string         `json:"label"`
	Kind             string         `json:"kind"` // sweep | sustained | admission
	Aborted          string         `json:"aborted_by_watchdog,omitempty"`
	ForgeLog         map[string]int `json:"forge_log_line_counts,omitempty"`
	WorkerLog        map[string]int `json:"worker_log_line_counts,omitempty"`
	RateJobsPerS     float64        `json:"arrival_rate_jobs_per_s"`
	Seconds          int            `json:"seconds"`
	Submit           E1Submit       `json:"submission"`
	DB               E1DB           `json:"database_final_state"`
	LoadStartOffsetS float64        `json:"load_start_offset_in_monitor_clock_s"` // poll and sample times are relative to monitor start; the load began this much later
	CompletionRate   float64        `json:"completed_per_s_during_load"`
	DrainSeconds     float64        `json:"drain_seconds"`
	RemainingAtEnd   int            `json:"remaining_at_load_end"`
	DrainRatePerS    float64        `json:"drain_rate_jobs_per_s"`
	DrainTimedOut    bool           `json:"drain_timed_out"`
	QueuedPeak       int            `json:"operator_queued_peak"`
	QueuedAtLoadEnd  int            `json:"operator_queued_at_load_end"`
	OldestAgePeak    float64        `json:"operator_oldest_due_age_peak_s"`
	BacklogSlopePerS float64        `json:"operator_backlog_slope_last_half_jobs_per_s"`
	Polls            []E1Poll       `json:"operator_polls"`
	Samples          []E1Sample     `json:"resource_samples"`
	Res              E1Resources    `json:"resources"`
	Sustainable      bool           `json:"classified_sustainable"`
	SustainWhy       []string       `json:"classification_reasons"`
	Checks           []Check        `json:"integrity_and_isolation_checks"`
	Error            string         `json:"error,omitempty"`
}

type E1Result struct {
	RunID       string     `json:"run_id"`
	Experiment  string     `json:"experiment"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     time.Time  `json:"ended_at"`
	Complete    bool       `json:"complete"`
	StopReason  string     `json:"stop_reason,omitempty"`
	Config      E1Config   `json:"configuration"`
	Env         EnvInfo    `json:"environment"`
	Sandbox     E1Sandbox  `json:"sandbox"`
	Stages      []*E1Stage `json:"stages"`
	SustainRate float64    `json:"sustained_stage_rate_chosen"`
	Notes       []string   `json:"notes"`
}

type E1Sandbox struct {
	PostgresCPUs   string `json:"postgres_cpus"`
	PostgresMemory string `json:"postgres_memory"`
	Storage        string `json:"postgres_storage"`
	ForgeSettings  string `json:"forge_settings"`
	ForgeCartIn    bool   `json:"forgecart_components_present"`
}

// ---------- entry point ----------

// RunE1 runs the experiment and returns the process exit code. It writes
// results/<runID>/e1.json and e1-report.md and never touches the canonical report.
func RunE1(repo, runID string, cfg E1Config) int {
	if err := cfg.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	suiteDir := filepath.Join(repo, "forgecart", "test_files")
	runDir := filepath.Join(suiteDir, "results", runID)
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

	res := &E1Result{RunID: runID, Experiment: e1Title(cfg), StartedAt: time.Now(), Config: cfg,
		Sandbox: E1Sandbox{PostgresCPUs: DefaultSandboxConfig().PgCPUs, PostgresMemory: DefaultSandboxConfig().PgMem, Storage: "tmpfs (RAM-backed): no disk I/O cost is represented", ForgeCartIn: false}}
	finish := func(code int) int {
		res.EndedAt = time.Now()
		_ = writeJSON(filepath.Join(runDir, "e1.json"), res)
		_ = os.WriteFile(filepath.Join(runDir, "e1-report.md"), []byte(RenderE1(res)), 0o644)
		logf("results: %s", runDir)
		return code
	}

	pfCtx, pfCancel := context.WithTimeout(ctx, 60*time.Second)
	info, problems := Preflight(pfCtx, repo)
	pfCancel()
	if len(problems) > 0 {
		for _, p := range problems {
			logf("preflight problem: %s", p)
		}
		res.StopReason = "Preflight failed: " + strings.Join(problems, "; ")
		return finish(1)
	}
	sb, err := NewSandbox(repo, runDir, runID)
	if err != nil {
		res.StopReason = err.Error()
		return finish(1)
	}
	sb.Roles = []string{"forge-db"} // no ForgeCart database exists in this run
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := sb.Down(cctx); err != nil {
			logf("CLEANUP PROBLEM: %v (containers carry the label %s=true and run %s)", err, labelKey, runID)
		} else {
			logf("sandbox %s removed", sb.Project)
		}
		os.RemoveAll(sb.BinDir)
	}()

	logf("run %s: building forge and the synthetic worker", runID)
	if err := buildE1(ctx, sb, cfg); err != nil {
		res.StopReason = "build failed: " + err.Error()
		return finish(1)
	}
	logf("starting the sandbox database (project %s)", sb.Project)
	if err := sb.Up(ctx, DefaultSandboxConfig()); err != nil {
		res.StopReason = "sandbox start failed: " + err.Error()
		return finish(1)
	}
	res.Env = collectEnv(ctx, sb, info)
	if cfg.Variant != "" {
		res.Notes = append(res.Notes, fmt.Sprintf("A/B variant %q: forge binary %q, migrations %q. The source-tree fingerprint above describes the working tree the harness ran from, not necessarily this variant's binary; the binary hashes above identify the variant.", cfg.Variant, cfg.ForgeBinary, migrationsURL(cfg)))
	}
	res.Sandbox.ForgeSettings = fmt.Sprintf("remote execution; FORGE_MAX_QUEUED=%d (default 1000); worker poll cap, lease, heartbeat, safety margin, poll interval, reaper and retry settings at their defaults; database pool fixed at 10 connections", cfg.MaxQueued)
	if n := countSandboxContainers(ctx, sb); n != 1 {
		res.StopReason = fmt.Sprintf("isolation check failed: expected exactly one sandbox container (forge-db), found %d", n)
		return finish(1)
	}

	opKey := randHex(24)
	sweepOK := map[float64]bool{}
	var plan []struct {
		label string
		kind  string
		rate  float64
		secs  int
	}
	kindOf := "sweep"
	if cfg.NoWorkers {
		kindOf = "admission"
	}
	// Interleaved order: repetition k starts k places further along the rate list.
	for rep := 0; rep < cfg.Repetitions; rep++ {
		for j := range cfg.Rates {
			r := cfg.Rates[(j+rep)%len(cfg.Rates)]
			label := fmt.Sprintf("%s-%g", kindOf, r)
			if cfg.Repetitions > 1 {
				label += fmt.Sprintf("-r%d", rep+1)
			}
			plan = append(plan, struct {
				label string
				kind  string
				rate  float64
				secs  int
			}{label, kindOf, r, cfg.StageSeconds})
		}
	}
	for i := 0; i < len(plan) || (cfg.SustainedSeconds > 0 && i == len(plan)); i++ {
		if ctx.Err() != nil {
			res.StopReason = "interrupted by signal"
			return finish(1)
		}
		var step struct {
			label string
			kind  string
			rate  float64
			secs  int
		}
		if i < len(plan) {
			step = plan[i]
		} else {
			// The sustained stage's rate is fixed by a rule written before the run:
			// SustainedFrac x the highest sweep rate classified sustainable (rounded to 25).
			best := 0.0
			for r, ok := range sweepOK {
				if ok && r > best {
					best = r
				}
			}
			if best == 0 {
				res.Notes = append(res.Notes, "No sweep stage was classified sustainable, so the sustained stage was not run.")
				break
			}
			rate := math.Max(25, math.Round(best*cfg.SustainedFrac/25)*25)
			res.SustainRate = rate
			step = struct {
				label string
				kind  string
				rate  float64
				secs  int
			}{fmt.Sprintf("sustained-%g", rate), "sustained", rate, cfg.SustainedSeconds}
		}
		logf("stage %s: %.0f jobs/s for %d s", step.label, step.rate, step.secs)
		st := runE1Stage(ctx, sb, cfg, step.label, step.kind, step.rate, step.secs, opKey, logf)
		res.Stages = append(res.Stages, st)
		if st.Kind == "sweep" && cfg.Repetitions == 1 {
			sweepOK[st.RateJobsPerS] = st.Sustainable
		}
		logf("stage %s done: accepted %.1f/s, completed-during-load %.1f/s, drain %.1f s, sustainable=%v", step.label, st.Submit.AcceptRatePerS, st.CompletionRate, st.DrainSeconds, st.Sustainable)
		if st.Error != "" {
			res.StopReason = fmt.Sprintf("stage %s could not be completed: %s", step.label, st.Error)
			return finish(1)
		}
	}
	res.Complete = ctx.Err() == nil && res.StopReason == ""
	code := 0
	for _, st := range res.Stages {
		for _, c := range st.Checks {
			if !c.OK {
				code = 1
			}
		}
	}
	return finish(code)
}

func countSandboxContainers(ctx context.Context, sb *Sandbox) int {
	out, err := run(ctx, "", nil, "docker", "ps", "-a", "--filter", "label="+labelRun+"="+sb.RunID, "--format", "{{.Names}}")
	if err != nil {
		return -1
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			n++
		}
	}
	return n
}

func buildE1(ctx context.Context, sb *Sandbox, cfg E1Config) error {
	for _, t := range []struct{ out, pkg string }{{"forge", "./cmd/forge"}, {"synthworker", "./forgecart/test_files/synthworker"}} {
		path := filepath.Join(sb.BinDir, t.out)
		if t.out == "forge" && cfg.ForgeBinary != "" {
			b, err := os.ReadFile(cfg.ForgeBinary)
			if err != nil {
				return fmt.Errorf("reading the prebuilt forge binary: %w", err)
			}
			if err := os.WriteFile(path, b, 0o755); err != nil {
				return err
			}
			sb.Binaries = append(sb.Binaries, binaryInfo(ctx, path))
			continue
		}
		if out, err := run(ctx, sb.Repo, nil, "go", "build", "-o", path, t.pkg); err != nil {
			return fmt.Errorf("building %s: %s", t.out, strings.TrimSpace(out))
		}
		sb.Binaries = append(sb.Binaries, binaryInfo(ctx, path))
	}
	return nil
}

// ---------- one stage ----------

func runE1Stage(ctx context.Context, sb *Sandbox, cfg E1Config, label, kind string, rate float64, secs int, opKey string, logf func(string, ...any)) *E1Stage {
	st := &E1Stage{Label: label, Kind: kind, RateJobsPerS: rate, Seconds: secs}
	fail := func(format string, a ...any) *E1Stage { st.Error = fmt.Sprintf(format, a...); return st }

	// fresh Forge database and processes for every stage, so stages are comparable
	sb.StopAll()
	sb.archiveLogs()
	for _, c := range []string{"DROP DATABASE IF EXISTS " + sb.ForgeDBName + " WITH (FORCE)", "CREATE DATABASE " + sb.ForgeDBName} {
		if _, err := sb.psql(ctx, "forge-db", "postgres", c); err != nil {
			return fail("database reset: %v", err)
		}
	}
	if out, err := run(ctx, sb.Repo, nil, "atlas", "migrate", "apply", "--dir", migrationsURL(cfg), "--url", sb.forgeURL()); err != nil {
		return fail("migrations: %s", strings.TrimSpace(tail(out, 300)))
	}
	opPort, err := freePort()
	if err != nil {
		return fail("%v", err)
	}
	opAddr := fmt.Sprintf("127.0.0.1:%d", opPort) // trusted interface: loopback only
	forgeEnv := []string{"PATH=" + os.Getenv("PATH"), "FORGE_DATABASE_URL=" + sb.forgeURL(),
		fmt.Sprintf("FORGE_ADDR=127.0.0.1:%d", sb.Ports.ForgeAPI), "FORGE_EXECUTION_MODE=remote",
		"FORGE_REMOTE_JOB_TYPES=" + strings.Join(cfg.Types, ","), fmt.Sprintf("FORGE_WORKER_ADDR=127.0.0.1:%d", sb.Ports.ForgeWorker),
		"FORGE_WORKER_API_KEY=" + sb.key, "FORGE_API_KEY=" + sb.apiKey, fmt.Sprintf("FORGE_MAX_QUEUED=%d", cfg.MaxQueued),
		"FORGE_OPERATOR_ADDR=" + opAddr, "FORGE_OPERATOR_API_KEY=" + opKey}
	if err := sb.StartProc(ctx, "forge", "forge", "forge", "", forgeEnv); err != nil {
		return fail("starting forge: %v", err)
	}
	if err := waitHTTP(ctx, sb.ForgeURL()+"/health", 30*time.Second); err != nil {
		return fail("forge not ready: %v", err)
	}
	op := &opClient{url: "http://" + opAddr + "/v1/queues", auth: "Bearer " + opKey, hc: &http.Client{Timeout: 10 * time.Second}}
	if _, _, err := op.get(ctx); err != nil {
		return fail("operator endpoint not usable: %v", err)
	}
	for i := 0; i < cfg.WorkerProcs && !cfg.NoWorkers; i++ {
		name := fmt.Sprintf("worker-bench-%d", i+1)
		env := []string{"PATH=" + os.Getenv("PATH"), fmt.Sprintf("SW_URL=http://127.0.0.1:%d", sb.Ports.ForgeWorker), "SW_KEY=" + sb.key,
			fmt.Sprintf("SW_NAME=%s", name), fmt.Sprintf("SW_CONCURRENCY=%d", cfg.SlotsPerProc), fmt.Sprintf("SW_HANDLER_MS=%d", cfg.HandlerMS),
			"SW_TYPES=" + strings.Join(cfg.Types, ",")}
		if err := sb.StartProc(ctx, name, "synthworker", "worker", "bench", env); err != nil {
			return fail("starting %s: %v", name, err)
		}
	}
	if err := sb.waitStarted(ctx, 30*time.Second); err != nil {
		return fail("workers not ready: %v", err)
	}
	fdb, err := sql.Open("pgx", sb.forgeURL())
	if err != nil {
		return fail("%v", err)
	}
	defer fdb.Close()
	sdb, err := sql.Open("pgx", sb.ForgeDBServerURL()) // sampler connection, on the postgres database so it is not counted
	if err != nil {
		return fail("%v", err)
	}
	sdb.SetMaxOpenConns(1)
	defer sdb.Close()
	var ddb *sql.DB // diagnostics only: a connection to the Forge database for pg_stat_user_* views
	if cfg.Diagnostics {
		if ddb, err = sql.Open("pgx", sb.forgeURL()); err != nil {
			return fail("%v", err)
		}
		ddb.SetMaxOpenConns(1)
		defer ddb.Close()
	}
	time.Sleep(2 * time.Second) // let idle slots settle into their long polls

	// ----- monitoring goroutines
	base := time.Now()
	monCtx, monStop := context.WithCancel(ctx)
	var mon sync.WaitGroup
	var mu sync.Mutex
	mon.Add(1)
	go func() { // resource sampler, 1 Hz; docker stats every 5th tick
		defer mon.Done()
		hs := &Sampler{} // used only for hostCPU()
		hs.hostCPU()
		prevF, prevW := 0.0, 0.0
		prevG := procCPUSeconds()
		prevT := time.Now()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		n := 0
		for {
			select {
			case <-monCtx.Done():
				return
			case <-tick.C:
			}
			now := time.Now()
			dt := now.Sub(prevT).Seconds()
			prevT = now
			procs := sb.SampleProcs()
			var f, w, frss, wrss float64
			for name, p := range procs {
				if name == "forge" {
					f, frss = p.CPUSeconds, p.RSSMB
				} else if strings.HasPrefix(name, "worker-") {
					w += p.CPUSeconds
					wrss += p.RSSMB
				}
			}
			g := procCPUSeconds()
			s := E1Sample{T: now.Sub(base).Seconds(), HostCPUPct: hs.hostCPU(), ForgeRSSMB: frss, WorkersRSS: wrss, GenRSSMB: selfRSSMB()}
			if dt > 0 {
				s.ForgeCPU, s.WorkersCPU, s.GenCPU = (f-prevF)/dt, (w-prevW)/dt, (g-prevG)/dt
			}
			prevF, prevW, prevG = f, w, g
			qctx, qc := context.WithTimeout(monCtx, 2*time.Second)
			_ = sdb.QueryRowContext(qctx, `SELECT count(*), count(*) FILTER (WHERE state='active') FROM pg_stat_activity WHERE datname=$1 AND backend_type='client backend'`, sb.ForgeDBName).Scan(&s.PGConnTotal, &s.PGConnActiv)
			qc()
			if cfg.Diagnostics {
				s.Diag = sampleDiag(monCtx, sdb, ddb, sb.ForgeDBName)
			}
			mu.Lock()
			st.Samples = append(st.Samples, s)
			mu.Unlock()
			if n%5 == 0 {
				mon.Add(1)
				go func() {
					defer mon.Done()
					cs := sb.SampleContainers(monCtx)
					if c, ok := cs["forge-db"]; ok {
						mu.Lock()
						st.Res.PGCPUReadings++
						st.Res.PGCPUAvg += c.CPUPct
						st.Res.PGCPUMax = math.Max(st.Res.PGCPUMax, c.CPUPct)
						st.Res.PGMemMaxMB = math.Max(st.Res.PGMemMaxMB, c.MemMB)
						mu.Unlock()
					}
				}()
			}
			n++
		}
	}()
	mon.Add(1)
	go func() { // operator endpoint poller (controlled rate)
		defer mon.Done()
		tick := time.NewTicker(time.Duration(cfg.OperatorPollMS) * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-monCtx.Done():
				return
			case <-tick.C:
			}
			t := time.Now()
			r, lat, err := op.get(monCtx)
			p := E1Poll{T: t.Sub(base).Seconds(), LatencyMS: float64(lat.Microseconds()) / 1000}
			if err != nil {
				p.Status = -1
			} else {
				p.Status = 200
				p.Queued, p.Running, p.OldestDueS, p.Rejections = r.Totals.Queued, r.Totals.Running, r.Totals.Oldest, r.Totals.Rejections
			}
			mu.Lock()
			st.Polls = append(st.Polls, p)
			mu.Unlock()
		}
	}()

	// ----- load
	gen := newE1Generator(cfg, sb.ForgeURL(), sb.apiKey, rate, secs, sb.RunID)
	cpu0 := procCPUSeconds()
	loadStart := time.Now()
	wdDone := make(chan struct{})
	if cfg.Watchdog {
		go func() {
			st.Aborted = watchdog(monCtx, gen, loadStart, func() float64 {
				mu.Lock()
				defer mu.Unlock()
				if n := len(st.Samples); n > 0 {
					return st.Samples[n-1].ForgeRSSMB
				}
				return 0
			})
			close(wdDone)
		}()
	} else {
		close(wdDone)
	}
	gen.run(ctx)
	gen.stop.Store(true) // ends the watchdog
	<-wdDone
	loadEnd := time.Now()
	genCPU := (procCPUSeconds() - cpu0) / loadEnd.Sub(loadStart).Seconds()
	st.LoadStartOffsetS = round3(loadStart.Sub(base).Seconds())
	st.Submit = gen.summary(loadStart)
	st.Submit.GeneratorCPU = round3(genCPU)
	st.QueuedAtLoadEnd = -1
	mu.Lock()
	for _, p := range st.Polls {
		if p.Status == 200 && p.T <= loadEnd.Sub(base).Seconds() {
			st.QueuedAtLoadEnd = p.Queued
		}
	}
	mu.Unlock()

	// ----- drain, observed through the operator endpoint
	drainDeadline := time.Now().Add(time.Duration(cfg.DrainTimeoutS) * time.Second)
	zero := 0
	for time.Now().Before(drainDeadline) && ctx.Err() == nil && !cfg.NoWorkers {
		time.Sleep(time.Duration(cfg.OperatorPollMS) * time.Millisecond)
		mu.Lock()
		var last *E1Poll
		if len(st.Polls) > 0 {
			last = &st.Polls[len(st.Polls)-1]
		}
		done := last != nil && last.Status == 200 && last.Queued == 0 && last.Running == 0 && last.T > loadEnd.Sub(base).Seconds()
		mu.Unlock()
		if done {
			zero++
			if zero >= 2 {
				break
			}
		} else {
			zero = 0
		}
	}
	st.DrainTimedOut = zero < 2 && !cfg.NoWorkers
	time.Sleep(1500 * time.Millisecond) // let the last samples land
	monStop()
	mon.Wait()
	finalPoll, _, ferr := op.get(ctx)

	// ----- analysis from the database (read-only, after the measurement window)
	analyzeE1(ctx, fdb, sb, st, gen, loadStart, loadEnd, cfg)
	if ferr == nil {
		wantQ := 0
		if cfg.NoWorkers {
			wantQ = st.DB.Rows // nothing is ever claimed in the admission-only probe
		}
		st.Checks = append(st.Checks, Check{Name: "operator endpoint after the run agrees with the database (queued=0 and running=0 unless admission-only, then queued = all rows)", OK: finalPoll.Totals.Queued == wantQ && finalPoll.Totals.Running == 0 && st.DB.ByStatus["queued"] == wantQ && st.DB.ByStatus["running"] == 0,
			Detail: fmt.Sprintf("operator queued=%d running=%d; database queued=%d running=%d", finalPoll.Totals.Queued, finalPoll.Totals.Running, st.DB.ByStatus["queued"], st.DB.ByStatus["running"])})
	} else {
		st.Checks = append(st.Checks, Check{Name: "operator endpoint reachable after the run", OK: false, Detail: ferr.Error()})
	}
	summariseE1Resources(st, loadStart.Sub(base).Seconds(), loadEnd.Sub(base).Seconds(), cfg)
	if !cfg.NoWorkers {
		classifyE1(st, cfg)
	} else {
		st.SustainWhy = []string{"not classified: the admission-only probe has no workers, so there is no sustainable-rate rule"}
	}
	b, _ := os.ReadFile(sb.LogDir + "/forge.log")
	st.ForgeLog = logShapes(string(b))
	var wl strings.Builder
	for i := 0; i < cfg.WorkerProcs && !cfg.NoWorkers; i++ {
		wb, _ := os.ReadFile(fmt.Sprintf("%s/worker-bench-%d.log", sb.LogDir, i+1))
		wl.Write(wb)
	}
	st.WorkerLog = logShapes(wl.String())
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "failed") || strings.Contains(l, "panic") || strings.Contains(l, "error") {
			st.DB.ForgeLogProblems++
		}
	}
	sb.StopAll()
	return st
}

func selfRSSMB() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			var kb float64
			fmt.Sscan(strings.TrimPrefix(l, "VmRSS:"), &kb)
			return kb / 1024
		}
	}
	return 0
}

// sampleDiag reads PostgreSQL's own view of what the Forge database is waiting
// on. Every query is read-only and cheap; it exists to tell apart explanations
// (lock queueing, pool exhaustion, dead index entries) that CPU figures cannot.
func sampleDiag(ctx context.Context, sdb, ddb *sql.DB, dbName string) *E1Diag {
	d := &E1Diag{Waits: map[string]int{}}
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if rows, err := sdb.QueryContext(qctx, `SELECT coalesce(wait_event_type,'none')||'/'||coalesce(wait_event,'none')||'/'||coalesce(state,'?'), count(*)
		FROM pg_stat_activity WHERE datname=$1 AND backend_type='client backend' GROUP BY 1`, dbName); err == nil {
		for rows.Next() {
			var k string
			var n int
			rows.Scan(&k, &n)
			d.Waits[k] = n
		}
		rows.Close()
	}
	_ = sdb.QueryRowContext(qctx, `SELECT coalesce(extract(epoch FROM (now()-min(xact_start))),0)::float8, coalesce(max(age(backend_xmin)),0)::bigint
		FROM pg_stat_activity WHERE datname=$1 AND backend_type='client backend' AND pid<>pg_backend_pid()`, dbName).Scan(&d.OldestXactAgeS, &d.MaxXminAge)
	_ = sdb.QueryRowContext(qctx, `SELECT count(*) FILTER (WHERE NOT granted), count(*) FILTER (WHERE NOT granted AND locktype='advisory') FROM pg_locks`).Scan(&d.LocksNotGranted, &d.AdvisoryWaiting)
	if ddb != nil {
		_ = ddb.QueryRowContext(qctx, `SELECT n_live_tup, n_dead_tup, autovacuum_count FROM pg_stat_user_tables WHERE relname='jobs'`).Scan(&d.LiveTup, &d.DeadTup, &d.AutovacuumCount)
		_ = ddb.QueryRowContext(qctx, `SELECT idx_scan, idx_tup_read, idx_tup_fetch FROM pg_stat_user_indexes WHERE indexrelname='jobs_due_idx'`).Scan(&d.DueIdxScans, &d.DueIdxTupRead, &d.DueIdxTupFetch)
	}
	return d
}

// ---------- operator client ----------

type opClient struct {
	url, auth string
	hc        *http.Client
}

type opResp struct {
	Totals struct {
		Queued     int      `json:"queued"`
		Running    int      `json:"running"`
		Oldest     *float64 `json:"oldest_due_job_age_seconds"`
		Rejections uint64   `json:"queue_full_rejections_since_process_start"`
	} `json:"totals"`
}

func (o *opClient) get(ctx context.Context) (opResp, time.Duration, error) {
	t := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.url, nil)
	req.Header.Set("Authorization", o.auth)
	resp, err := o.hc.Do(req)
	if err != nil {
		return opResp{}, time.Since(t), err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r opResp
	if resp.StatusCode != 200 {
		return r, time.Since(t), fmt.Errorf("operator endpoint answered %d", resp.StatusCode)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, time.Since(t), err
	}
	return r, time.Since(t), nil
}

// ---------- open-loop generator ----------

type e1Generator struct {
	cfg     E1Config
	apiKey  string // Forge's submit key
	batch   int    // jobs per request; above 1 the generator posts to /v1/jobs/batch (set by the remote command)
	url     string
	rate    float64
	secs    int
	runID   string
	planned int

	hc *http.Client

	mu        sync.Mutex
	status    []int16 // outcome class per sequence number
	latUS     []int32 // service time of each request, microseconds
	lateUS    []int32 // actual send time minus scheduled time, microseconds
	schedMS   []int32 // scheduled offset, ms
	accepted  []bool  // by sequence number
	other     map[string]int
	hint      int
	sent      int
	capHits   int
	stop      atomic.Bool // set by the watchdog: no further requests are sent
	okCount   int         // accepted so far (watchdog input)
	lastDone  time.Duration
	startTime time.Time
}

const (
	outAccepted int16 = iota + 1
	outQueueFull
	outOther
)

func newE1Generator(cfg E1Config, forgeURL, apiKey string, rate float64, secs int, runID string) *e1Generator {
	n := int(math.Round(rate * float64(secs)))
	tr := &http.Transport{Proxy: nil, MaxIdleConns: cfg.MaxInFlight, MaxIdleConnsPerHost: cfg.MaxInFlight, IdleConnTimeout: 30 * time.Second,
		DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	return &e1Generator{cfg: cfg, apiKey: apiKey, url: forgeURL + "/v1/jobs", rate: rate, secs: secs, runID: runID, planned: n,
		hc:     &http.Client{Timeout: 20 * time.Second, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		status: make([]int16, n), latUS: make([]int32, n), lateUS: make([]int32, n), schedMS: make([]int32, n), accepted: make([]bool, n), other: map[string]int{}}
}

// run sends planned requests at their scheduled instants (i / rate after the
// start). A send that cannot start on time (the in-flight bound is reached, or
// the process is overloaded) is still sent, late, and the lateness is recorded:
// the offered load is never silently reduced.
func (g *e1Generator) run(ctx context.Context) {
	slots := make(chan struct{}, g.cfg.MaxInFlight)
	var wg sync.WaitGroup
	pad := strings.Repeat("x", max(0, g.cfg.PayloadBytes-120))
	start := time.Now()
	g.startTime = start
	for i := 0; i < g.planned && ctx.Err() == nil && !g.stop.Load(); i++ {
		target := start.Add(time.Duration(float64(i) / g.rate * float64(time.Second)))
		if d := time.Until(target); d > 0 {
			time.Sleep(d)
		}
		select {
		case slots <- struct{}{}:
		default:
			g.mu.Lock()
			g.capHits++
			g.mu.Unlock()
			slots <- struct{}{} // wait for a free slot: the send becomes late, never dropped
		}
		wg.Add(1)
		go func(i int, target time.Time) {
			defer wg.Done()
			defer func() { <-slots }()
			g.send(ctx, i, target, start, pad)
		}(i, target)
	}
	wg.Wait()
}

func (g *e1Generator) send(ctx context.Context, i int, target, start time.Time, pad string) {
	var body string
	if g.batch > 1 {
		items := make([]string, g.batch)
		for k := range items {
			seq := i*g.batch + k
			items[k] = fmt.Sprintf(`{"type":%q,"input":{"seq":%d,"run":%q,"pad":%q}}`, g.cfg.Types[seq%len(g.cfg.Types)], seq, g.runID, pad)
		}
		body = `{"jobs":[` + strings.Join(items, ",") + `]}`
	} else {
		typ := g.cfg.Types[i%len(g.cfg.Types)]
		body = fmt.Sprintf(`{"type":%q,"input":{"seq":%d,"run":%q,"pad":%q}}`, typ, i, g.runID, pad)
	}
	// A body reader without GetBody means the HTTP client can never replay the request.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.url, io.NopCloser(bytes.NewReader([]byte(body))))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	sentAt := time.Now()
	late := sentAt.Sub(target)
	resp, err := g.hc.Do(req)
	lat := time.Since(sentAt)
	var out int16 = outOther
	key := ""
	hint := false
	if err != nil {
		key = "transport_error"
		if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
			key = "timeout"
		}
	} else {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch {
		case resp.StatusCode == 202:
			out = outAccepted
		case resp.StatusCode == 503 && bytes.Contains(raw, []byte(`"queue_full"`)):
			out = outQueueFull
			hint = resp.Header.Get("Retry-After") != ""
		default:
			key = "http_" + strconv.Itoa(resp.StatusCode)
		}
	}
	g.mu.Lock()
	g.sent++
	g.status[i], g.latUS[i], g.lateUS[i], g.schedMS[i] = out, int32(lat.Microseconds()), int32(late.Microseconds()), int32(target.Sub(start).Milliseconds())
	if out == outAccepted {
		g.accepted[i] = true
		g.okCount++
	}
	if key != "" {
		g.other[key]++
	}
	if hint {
		g.hint++
	}
	if d := sentAt.Add(lat).Sub(start); d > g.lastDone {
		g.lastDone = d
	}
	g.mu.Unlock()
}

func (g *e1Generator) summary(loadStart time.Time) E1Submit {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := E1Submit{Offered: g.planned, Sent: g.sent, Other: map[string]int{}, OfferedRatePerS: g.rate, InFlightCap: g.capHits, QueueFullHint: g.hint}
	for k, v := range g.other {
		s.Other[k] = v
	}
	var accLat, accLast, late []int64
	halfMS := int32(g.secs * 1000 / 2)
	s.AcceptedPerS = make([]int, g.secs+1)
	for i := 0; i < g.planned; i++ {
		if g.status[i] == 0 {
			continue // never sent (interrupted)
		}
		late = append(late, int64(g.lateUS[i]))
		switch g.status[i] {
		case outAccepted:
			s.Accepted++
			accLat = append(accLat, int64(g.latUS[i]))
			if g.schedMS[i] >= halfMS {
				accLast = append(accLast, int64(g.latUS[i]))
			}
			if sec := int(g.schedMS[i] / 1000); sec < len(s.AcceptedPerS) {
				s.AcceptedPerS[sec]++
			}
		case outQueueFull:
			s.QueueFull++
		}
	}
	type wacc struct {
		sent, acc int
		lat       []int64
	}
	wins := map[int]*wacc{}
	for i := 0; i < g.planned; i++ {
		if g.status[i] == 0 {
			continue
		}
		w := int(g.schedMS[i]/1000) / 30
		if wins[w] == nil {
			wins[w] = &wacc{}
		}
		wins[w].sent++
		if g.status[i] == outAccepted {
			wins[w].acc++
			wins[w].lat = append(wins[w].lat, int64(g.latUS[i]))
		}
	}
	for w := 0; w <= g.secs/30; w++ {
		a := wins[w]
		if a == nil {
			continue
		}
		sort.Slice(a.lat, func(i, j int) bool { return a.lat[i] < a.lat[j] })
		ew := E1Win{FromS: w * 30, Sent: a.sent, Accepted: a.acc}
		if len(a.lat) > 0 {
			p50, _ := Percentile(a.lat, 50)
			p99, _ := Percentile(a.lat, 99)
			ew.P50MS, ew.P99MS = round3(float64(p50)/1000), round3(float64(p99)/1000)
		}
		s.Windows = append(s.Windows, ew)
	}
	s.WindowS = g.lastDone.Seconds()
	if s.WindowS > 0 {
		s.AcceptRatePerS = round3(float64(s.Accepted) / s.WindowS)
	}
	s.Latency, s.LatencyLastHalf = distOf(accLat), distOf(accLast)
	sort.Slice(late, func(i, j int) bool { return late[i] < late[j] })
	if len(late) > 0 {
		p, _ := Percentile(late, 99)
		s.LateP99MS = round3(float64(p) / 1000)
		s.LateMaxMS = round3(float64(late[len(late)-1]) / 1000)
		for _, v := range late {
			if v > int64(g.cfg.LateThresholdMS)*1000 {
				s.LateOverCount++
			}
		}
	}
	return s
}

func distOf(us []int64) E1Dist {
	if len(us) == 0 {
		return E1Dist{}
	}
	l := LatencyOf(us)
	return E1Dist{P50: deref(l.P50MS), P95: deref(l.P95MS), P99: deref(l.P99MS), Max: deref(l.MaxMS), N: l.Count}
}

// ---------- analysis ----------

func analyzeE1(ctx context.Context, db *sql.DB, sb *Sandbox, st *E1Stage, gen *e1Generator, loadStart, loadEnd time.Time, cfg E1Config) {
	add := func(name string, ok bool, detail string) {
		st.Checks = append(st.Checks, Check{Name: name, OK: ok, Detail: detail})
	}
	q := func(query string, args ...any) *sql.Row { return db.QueryRowContext(ctx, query, args...) }
	d := &st.DB
	d.ByStatus, d.ByType = map[string]int{}, map[string]int{}
	rows, err := db.QueryContext(ctx, `SELECT status, count(*) FROM jobs GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var s string
			var n int
			rows.Scan(&s, &n)
			d.ByStatus[s] = n
			d.Rows += n
		}
		rows.Close()
	}
	rows, err = db.QueryContext(ctx, `SELECT type, count(*) FROM jobs GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var s string
			var n int
			rows.Scan(&s, &n)
			d.ByType[s] = n
		}
		rows.Close()
	}
	var firstC, lastF sql.NullTime
	_ = q(`SELECT count(*) FILTER (WHERE attempt > 1), coalesce(max(attempt),0),
	       count(*) FILTER (WHERE status='succeeded' AND result IS DISTINCT FROM '{"ok": true}'::jsonb),
	       count(DISTINCT (input->>'seq')::int), min(created_at), max(finished_at),
	       count(*) FILTER (WHERE finished_at <= $1)
	       FROM jobs`, loadEnd).Scan(&d.RetriedAttempts, &d.MaxAttempt, &d.WrongResult, &d.DistinctSeq, &firstC, &lastF, &d.CompletedByEnd)
	d.FirstCreated, d.LastFinished = firstC.Time.UTC(), lastF.Time.UTC()
	half := loadStart.Add(time.Duration(cfg2secs(st)) * time.Second / 2)
	dist := func(expr string, from, to time.Time) E1Dist {
		var v [4]sql.NullFloat64
		var n int
		err := q(fmt.Sprintf(`SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY x), percentile_disc(0.95) WITHIN GROUP (ORDER BY x),
		        percentile_disc(0.99) WITHIN GROUP (ORDER BY x), max(x), count(*) FROM (SELECT %s AS x FROM jobs WHERE status='succeeded' AND created_at >= $1 AND created_at < $2) t`, expr), from, to).Scan(&v[0], &v[1], &v[2], &v[3], &n)
		if err != nil {
			return E1Dist{}
		}
		return E1Dist{P50: round3(v[0].Float64), P95: round3(v[1].Float64), P99: round3(v[2].Float64), Max: round3(v[3].Float64), N: n}
	}
	far := loadEnd.Add(24 * time.Hour)
	qw, ex, e2e := `EXTRACT(EPOCH FROM (started_at - created_at))*1000`, `EXTRACT(EPOCH FROM (finished_at - started_at))*1000`, `EXTRACT(EPOCH FROM (finished_at - created_at))*1000`
	d.QueueWait, d.QueueWaitLast = dist(qw, loadStart.Add(-time.Hour), far), dist(qw, half, far)
	d.Exec = dist(ex, loadStart.Add(-time.Hour), far)
	d.EndToEnd, d.EndToEndLast = dist(e2e, loadStart.Add(-time.Hour), far), dist(e2e, half, far)
	cs := map[int]int{}
	maxS := 0
	rows, err = db.QueryContext(ctx, `SELECT floor(EXTRACT(EPOCH FROM (finished_at - $1)))::int, count(*) FROM jobs WHERE finished_at IS NOT NULL GROUP BY 1`, loadStart)
	if err == nil {
		for rows.Next() {
			var s, n int
			rows.Scan(&s, &n)
			if s < 0 {
				s = 0
			}
			cs[s] = n
			maxS = max(maxS, s)
		}
		rows.Close()
	}
	d.CompletedBySec = make([]int, maxS+1)
	for s, n := range cs {
		d.CompletedBySec[s] = n
	}
	_ = q(`SELECT pg_total_relation_size('jobs')`).Scan(&d.TableBytes)

	// membership: every acknowledged submission is in the database, and the database holds nothing else
	inDB := make([]bool, gen.planned)
	rows, err = db.QueryContext(ctx, `SELECT (input->>'seq')::int FROM jobs`)
	dup := 0
	if err == nil {
		for rows.Next() {
			var s int
			rows.Scan(&s)
			if s >= 0 && s < len(inDB) {
				if inDB[s] {
					dup++
				}
				inDB[s] = true
			}
		}
		rows.Close()
	}
	for i := 0; i < gen.planned; i++ {
		if gen.accepted[i] && !inDB[i] {
			d.AcceptedMissing++
		}
		if !gen.accepted[i] && inDB[i] {
			d.DBOnly++
		}
	}

	// completion / drain, from the rows' own timestamps
	st.CompletionRate = round3(float64(d.CompletedByEnd) / loadEnd.Sub(loadStart).Seconds())
	st.RemainingAtEnd = st.Submit.Accepted - d.CompletedByEnd
	if !lastF.Valid {
		st.DrainSeconds = 0
	} else if lastF.Time.After(loadEnd) {
		st.DrainSeconds = round3(lastF.Time.Sub(loadEnd).Seconds())
	}
	// A drain rate is only meaningful when there was a real backlog to work off:
	// a sub-second tail of in-flight jobs says nothing about drain throughput.
	if st.DrainSeconds >= 1 && st.RemainingAtEnd > 0 {
		st.DrainRatePerS = round3(float64(st.RemainingAtEnd) / st.DrainSeconds)
	}

	// ----- checks
	sm := st.Submit
	other := 0
	for _, v := range sm.Other {
		other += v
	}
	add("every planned request was sent", sm.Sent == sm.Offered, fmt.Sprintf("planned %d, sent %d", sm.Offered, sm.Sent))
	add("every outcome is accounted for: accepted + queue_full + other = sent", sm.Accepted+sm.QueueFull+other == sm.Sent, fmt.Sprintf("%d + %d + %d vs %d", sm.Accepted, sm.QueueFull, other, sm.Sent))
	add("every acknowledged (202) submission exists in the database", d.AcceptedMissing == 0, fmt.Sprintf("missing %d", d.AcceptedMissing))
	add("the database holds no row that was not acknowledged (no lost-response duplicates)", d.DBOnly == 0, fmt.Sprintf("unacknowledged rows %d (uncertain outcomes would appear here)", d.DBOnly))
	add("no sequence number appears twice in the database", dup == 0 && d.DistinctSeq == d.Rows, fmt.Sprintf("rows %d, distinct %d", d.Rows, d.DistinctSeq))
	add("database rows = accepted submissions", d.Rows == sm.Accepted+d.DBOnly, fmt.Sprintf("rows %d, accepted %d, unacknowledged %d", d.Rows, sm.Accepted, d.DBOnly))
	add("status counts reconcile with the row total", d.ByStatus["queued"]+d.ByStatus["running"]+d.ByStatus["succeeded"]+d.ByStatus["failed"] == d.Rows, fmt.Sprint(d.ByStatus))
	if cfg.NoWorkers {
		add("no workers ran: every accepted job is still queued and none was claimed", d.ByStatus["queued"] == d.Rows && d.ByStatus["running"] == 0 && d.MaxAttempt <= 1, fmt.Sprintf("queued %d of %d rows, running %d, max attempt %d", d.ByStatus["queued"], d.Rows, d.ByStatus["running"], d.MaxAttempt))
	} else {
		add("every accepted job reached a terminal state (drained)", !st.DrainTimedOut && d.ByStatus["queued"] == 0 && d.ByStatus["running"] == 0, fmt.Sprintf("queued %d running %d, drain timed out=%v", d.ByStatus["queued"], d.ByStatus["running"], st.DrainTimedOut))
		add("all terminal jobs succeeded; none failed", d.ByStatus["failed"] == 0 && d.ByStatus["succeeded"] == d.Rows, fmt.Sprintf("failed %d", d.ByStatus["failed"]))
		add("every succeeded job carries the handler's result", d.WrongResult == 0, fmt.Sprintf("unexpected results %d", d.WrongResult))
		add("no job needed a retry (attempt > 1)", d.RetriedAttempts == 0, fmt.Sprintf("retried %d, max attempt %d", d.RetriedAttempts, d.MaxAttempt))
	}
	add("submission throughput reproduces from raw counts: accepted / send window", math.Abs(sm.AcceptRatePerS-float64(sm.Accepted)/sm.WindowS) < 0.01, fmt.Sprintf("%d / %.3f s = %.3f/s", sm.Accepted, sm.WindowS, float64(sm.Accepted)/sm.WindowS))
	add("completion throughput reproduces from timestamps: finished by load end / load duration", math.Abs(st.CompletionRate-float64(d.CompletedByEnd)/loadEnd.Sub(loadStart).Seconds()) < 0.01, fmt.Sprintf("%d / %.3f s", d.CompletedByEnd, loadEnd.Sub(loadStart).Seconds()))
	completedSum := 0
	for _, n := range d.CompletedBySec {
		completedSum += n
	}
	add("per-second completion timeline sums to the finished jobs", completedSum == d.ByStatus["succeeded"]+d.ByStatus["failed"], fmt.Sprintf("%d vs %d", completedSum, d.ByStatus["succeeded"]+d.ByStatus["failed"]))
	// The maintained queue counter (migration queue_counter) must equal the authoritative count.
	var ctr sql.NullInt64
	if err := q(`SELECT sum(queued) FROM queue_counter`).Scan(&ctr); err == nil {
		add("queue_counter equals count(*) of status='queued' rows (reconciliation)", ctr.Valid && int(ctr.Int64) == d.ByStatus["queued"], fmt.Sprintf("counter %d, rows %d", ctr.Int64, d.ByStatus["queued"]))
	}
	add("ForgeCart is absent: only the Forge database container exists and the job types are the synthetic ones", countSandboxContainers(ctx, sb) == 1 && len(d.ByType) <= len(cfg.Types), fmt.Sprintf("types %v", d.ByType))
}

func cfg2secs(st *E1Stage) int { return st.Seconds }

// ---------- resources and classification ----------

func summariseE1Resources(st *E1Stage, loadFrom, loadTo float64, cfg E1Config) {
	r := &st.Res
	var host, f, w, g []float64
	for _, s := range st.Samples {
		if s.T < loadFrom || s.T > loadTo {
			continue
		}
		host = append(host, s.HostCPUPct)
		f = append(f, s.ForgeCPU)
		w = append(w, s.WorkersCPU)
		g = append(g, s.GenCPU)
		r.ForgeRSSMax = math.Max(r.ForgeRSSMax, s.ForgeRSSMB)
		r.WorkersRSSMax = math.Max(r.WorkersRSSMax, s.WorkersRSS)
		r.GenRSSMax = math.Max(r.GenRSSMax, s.GenRSSMB)
		r.PGConnMax = max(r.PGConnMax, s.PGConnTotal)
		r.PGActiveMax = max(r.PGActiveMax, s.PGConnActiv)
	}
	avgMax := func(v []float64) (float64, float64) {
		if len(v) == 0 {
			return 0, 0
		}
		sum, mx := 0.0, 0.0
		for _, x := range v {
			sum += x
			mx = math.Max(mx, x)
		}
		return round3(sum / float64(len(v))), round3(mx)
	}
	r.HostCPUAvg, r.HostCPUMax = avgMax(host)
	r.ForgeCPUAvg, r.ForgeCPUMax = avgMax(f)
	r.WorkersCPUAvg, r.WorkersCPUMax = avgMax(w)
	r.GenCPUAvg, r.GenCPUMax = avgMax(g)
	if r.PGCPUReadings > 0 {
		r.PGCPUAvg = round3(r.PGCPUAvg / float64(r.PGCPUReadings))
	}
	var lat []float64
	for _, p := range st.Polls {
		if p.Status == 200 {
			lat = append(lat, p.LatencyMS)
		} else {
			r.OperatorPollErrors++
		}
		r.OperatorPolls++
	}
	sort.Float64s(lat)
	if len(lat) > 0 {
		r.OperatorPollP50, r.OperatorPollP95, r.OperatorPollMax = lat[len(lat)/2], lat[int(float64(len(lat))*0.95)-1+boolInt(len(lat) < 20)], lat[len(lat)-1]
	}
	// queue observations from the operator endpoint
	var xs, ys []float64
	for _, p := range st.Polls {
		if p.Status != 200 {
			continue
		}
		st.QueuedPeak = max(st.QueuedPeak, p.Queued)
		if p.OldestDueS != nil {
			st.OldestAgePeak = math.Max(st.OldestAgePeak, *p.OldestDueS)
		}
		if p.T >= loadFrom+float64(st.Seconds)/2 && p.T <= loadTo {
			xs, ys = append(xs, p.T), append(ys, float64(p.Queued))
		}
	}
	st.OldestAgePeak = round3(st.OldestAgePeak)
	if len(xs) >= 3 {
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
			st.BacklogSlopePerS = round3(num / den)
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// classifyE1 applies the stage classification written before the run. A stage
// is "sustainable" when (1) every offered request was accepted with no error,
// (2) the generator kept to its schedule (at most 1% of sends later than the
// threshold, and the in-flight bound never reached), (3) the queue did not grow
// during the second half of the load (operator-observed backlog slope at most
// 2% of the arrival rate per second) and (4) the p95 queue wait of jobs created
// in the second half was at most 5 s. The thresholds are this experiment's own
// assumptions, used only to choose the sustained-stage rate; they are not
// service-level objectives.
func classifyE1(st *E1Stage, cfg E1Config) {
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
	why(float64(sm.LateOverCount) <= 0.01*float64(max(sm.Sent, 1)) && sm.InFlightCap == 0, "the generator kept to its schedule", fmt.Sprintf("%d sends later than %d ms, in-flight cap hit %d times (generator may be limiting)", sm.LateOverCount, cfg.LateThresholdMS, sm.InFlightCap))
	why(st.BacklogSlopePerS <= 0.02*st.RateJobsPerS, fmt.Sprintf("backlog slope %.1f jobs/s over the second half (<= 2%% of the rate)", st.BacklogSlopePerS), fmt.Sprintf("backlog still growing at %.1f jobs/s in the second half (> 2%% of the rate)", st.BacklogSlopePerS))
	why(st.DB.QueueWaitLast.N > 0 && st.DB.QueueWaitLast.P95 <= 5000, fmt.Sprintf("p95 queue wait %.0f ms in the second half (<= 5000 ms)", st.DB.QueueWaitLast.P95), fmt.Sprintf("p95 queue wait %.0f ms in the second half (> 5000 ms)", st.DB.QueueWaitLast.P95))
	st.Sustainable = ok
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// logShapes counts log lines with timestamps, ids and numbers removed, keeping
// the ten most frequent shapes. Messages never contain payloads.
func logShapes(text string) map[string]int {
	re := regexp.MustCompile(`job_[0-9a-f]+|worker-[A-Za-z0-9-]+|[0-9]+(\.[0-9]+)?(ms|s|µs)?`)
	counts := map[string]int{}
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "{") { // slog JSON line: keep the message only
			if i := strings.Index(l, `"msg":"`); i >= 0 {
				l = l[i+7:]
				if j := strings.Index(l, `"`); j >= 0 {
					l = l[:j]
				}
			}
		} else if len(l) > 20 && l[4] == '/' { // standard log prefix "2006/01/02 15:04:05 "
			l = l[20:]
		}
		counts[re.ReplaceAllString(l, "N")]++
	}
	if len(counts) > 10 {
		type kv struct {
			k string
			v int
		}
		var all []kv
		for k, v := range counts {
			all = append(all, kv{k, v})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
		counts = map[string]int{}
		for _, e := range all[:10] {
			counts[e.k] = e.v
		}
	}
	return counts
}

// Watchdog abort rules (opt-in, written before the runs). Each makes the trial
// stop sending; the trial is still drained and analysed, and is marked aborted.
//  1. The generator is invalid: the in-flight bound was reached 1000 times, so
//     the offered rate is no longer being delivered.
//  2. Severe failure: after 60 s of load, at least half of the requests that
//     completed in the last 30 s were not accepted.
//  3. Host safety: Forge's resident memory exceeds 1.5 GB.
func watchdog(ctx context.Context, g *e1Generator, loadStart time.Time, forgeRSS func() float64) string {
	type snap struct{ sent, ok int }
	var hist []snap
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-tick.C:
		}
		if g.stop.Load() {
			return "" // the load finished normally
		}
		g.mu.Lock()
		sn := snap{g.sent, g.okCount}
		caps := g.capHits
		g.mu.Unlock()
		hist = append(hist, sn)
		reason := ""
		switch {
		case caps >= 1000:
			reason = fmt.Sprintf("generator invalid: in-flight bound reached %d times", caps)
		case forgeRSS() > 1536:
			reason = fmt.Sprintf("host safety: Forge resident memory %.0f MB", forgeRSS())
		case len(hist) > 30 && time.Since(loadStart) > 60*time.Second:
			old := hist[len(hist)-31]
			if d := sn.sent - old.sent; d >= 100 && float64(sn.ok-old.ok) <= 0.5*float64(d) {
				reason = fmt.Sprintf("severe failure: %d of %d responses in the last 30 s were not accepted", d-(sn.ok-old.ok), d)
			}
		}
		if reason != "" {
			g.stop.Store(true)
			return reason
		}
	}
}

func e1Title(cfg E1Config) string {
	switch {
	case cfg.NoWorkers:
		return "E-1 follow-up: admission-only probe (no workers)"
	case cfg.Repetitions > 1:
		return "E-1 follow-up: capacity-knee sweep (interleaved repetitions)"
	}
	return "E-1 Forge-isolated capacity baseline"
}

func migrationsURL(cfg E1Config) string {
	if cfg.MigrationsDir != "" {
		return "file://" + cfg.MigrationsDir
	}
	return "file://migrations"
}
