// Command test_files is the ForgeCart/Forge scenario-based load-testing suite.
// Run it through run.sh. It builds a disposable sandbox (two PostgreSQL
// containers in a uniquely named Compose project plus host processes for Forge,
// ForgeCart, the dispatcher and the workers), runs the selected scenarios and
// writes results/<run-id>/ and report.md. It never touches any other Docker
// project, volume or database.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const usage = `usage: run.sh <command> [flags]

commands:
  list                       list scenarios and profiles
  preflight                  check tools and Docker, change nothing
  dry-run                    print the resolved plan, start nothing
  run                        run the selected scenarios (default command)
  e1                         run experiment E-1, the Forge-isolated capacity baseline (jobs go
                             straight to Forge; no ForgeCart). Writes results/<run-id>/e1.json and
                             e1-report.md and never touches the canonical report.md.
                             flags: --run-id --rates a,b,c --stage-seconds N --sustained-seconds N
                             --handler-ms N --payload-bytes N --worker-procs N --slots N --max-queued N
                             --diagnostics (extra read-only PostgreSQL telemetry for investigating a stage)
  remote                     run the E-1 open-loop load against a Forge that is ALREADY RUNNING (for example
                             on other machines), with workers started separately. Reads only Forge's HTTP
                             APIs and /metrics; no sandbox, no database access. Needs FORGE_API_KEY and
                             FORGE_OPERATOR_API_KEY in the environment (never recorded).
                             flags: --run-id --forge-url --operator-url --types a,b --rates a,b,c
                             --stage-seconds N --repetitions N --payload-bytes N --drain-timeout N --note TEXT
                             --batch N (jobs per request, 1-100; N>1 uses POST /v1/jobs/batch; latency is then per request)
  remote-cart                run ForgeCart shopper workflows against a ForgeCart + Forge that are ALREADY
                             RUNNING (attach mode): foreground and background figures, ForgeCart integrity
                             checks. It EMPTIES ForgeCart's business tables between trials, so it demands
                             --erase-forgecart-tables. Needs FORGE_API_KEY, FORGE_OPERATOR_API_KEY and
                             FORGECART_ATTACH_APP_DB_URL in the environment (never recorded).
                             flags: --run-id --cart-url --forge-url --operator-url --levels 5,10,20
                             --workflows N --repetitions N --drain-timeout N --note TEXT --erase-forgecart-tables
                             --duration-seconds N (60-7200: run each level for N seconds instead of N workflows: a soak)
                             --hot-products N (every shopper orders only the first N products by SKU: N=1 is a
                             stampede on one stock row)  --inventory N (stock per product at the start of each trial)
  report                     regenerate a report from a preserved run, touching nothing else:
                             report --from <run-id> --out <new-file> (the file must not exist and
                             must not be inside results/; the run directory is only read)

flags (run, dry-run):
  --profile NAME             smoke (default) | standard | full | all
  --scenarios a,b,c          explicit scenario list (overrides --profile)
  --scale F                  multiply workflow counts and durations (0.1-4, default 1)
  --soak-seconds N           soak duration (max 1800)
  --seed N                   override the scenario seeds
  --race                     build services with the race detector (slower)
  --allow-fault-injection    permit the recovery scenario (sandbox only)
  --run-id ID                6-16 lower-case letters/digits (default: generated)
`

func main() {
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	profile := fs.String("profile", "smoke", "")
	list := fs.String("scenarios", "", "")
	scale := fs.Float64("scale", 1, "")
	soak := fs.Int("soak-seconds", 0, "")
	seed := fs.Int64("seed", 0, "")
	race := fs.Bool("race", false, "")
	fault := fs.Bool("allow-fault-injection", false, "")
	runID := fs.String("run-id", "", "")
	from := fs.String("from", "", "")
	e1Rates := fs.String("rates", "", "")
	e1Stage := fs.Int("stage-seconds", 0, "")
	e1Sustain := fs.Int("sustained-seconds", -1, "")
	e1Handler := fs.Int("handler-ms", -1, "")
	e1Payload := fs.Int("payload-bytes", 0, "")
	e1Procs := fs.Int("worker-procs", 0, "")
	e1Slots := fs.Int("slots", 0, "")
	e1MaxQ := fs.Int("max-queued", 0, "")
	e1Diag := fs.Bool("diagnostics", false, "")
	e1Reps := fs.Int("repetitions", 0, "")
	rmForge := fs.String("forge-url", "", "")
	rmOperator := fs.String("operator-url", "", "")
	rmTypes := fs.String("types", "", "")
	rmDrain := fs.Int("drain-timeout", 300, "")
	rmNote := fs.String("note", "", "")
	rmBatch := fs.Int("batch", 1, "")
	rcCart := fs.String("cart-url", "", "")
	rcLevels := fs.String("levels", "", "")
	rcWorkflows := fs.Int("workflows", 300, "")
	rcDuration := fs.Int("duration-seconds", 0, "")
	rcHot := fs.Int("hot-products", 0, "")
	rcInventory := fs.Int("inventory", 0, "")
	rcErase := fs.Bool("erase-forgecart-tables", false, "")
	e1NoWorkers := fs.Bool("no-workers", false, "")
	e1Watchdog := fs.Bool("watchdog", false, "")
	e1Variant := fs.String("variant", "", "")
	e1Bin := fs.String("forge-binary", "", "")
	e1Mig := fs.String("migrations-dir", "", "")
	out := fs.String("out", "", "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	repo, err := findRepo()
	if err != nil {
		fatal(err)
	}
	defs, err := LoadScenarios()
	if err != nil {
		fatal(err)
	}
	profiles, err := LoadProfiles()
	if err != nil {
		fatal(err)
	}
	switch cmd {
	case "list":
		doList(defs, profiles)
		return
	case "preflight":
		os.Exit(doPreflight(repo))
	case "report":
		os.Exit(doReport(repo, *from, *out))
	case "remote-cart":
		if !*rcErase {
			fatal(fmt.Errorf("remote-cart EMPTIES ForgeCart's business tables (orders, payments, outbox, invoices ...) between trials in the database named by FORGECART_ATTACH_APP_DB_URL; pass --erase-forgecart-tables to confirm that database holds nothing you need"))
		}
		rc := RemoteCartConfig{CartURL: *rcCart, ForgeURL: *rmForge, OperatorURL: *rmOperator, Workflows: *rcWorkflows, DurationS: *rcDuration, HotProducts: *rcHot, Inventory: *rcInventory, Repetitions: 1, DrainTimeoutS: *rmDrain, Note: *rmNote, Seed: 7000}
		if *seed > 0 {
			rc.Seed = *seed
		}
		for _, p := range strings.Split(*rcLevels, ",") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			v, err := strconv.Atoi(p)
			if err != nil {
				fatal(fmt.Errorf("--levels must be comma-separated whole numbers: %w", err))
			}
			rc.Levels = append(rc.Levels, v)
		}
		if *e1Reps > 0 {
			rc.Repetitions = *e1Reps
		}
		att := AttachConfig{APIKey: os.Getenv("FORGE_API_KEY"), OperatorKey: os.Getenv("FORGE_OPERATOR_API_KEY"), AppDBURL: os.Getenv("FORGECART_ATTACH_APP_DB_URL"), ResultsDBURL: os.Getenv("FORGECART_ATTACH_RESULTS_DB_URL"), PaymentsDBURL: os.Getenv("FORGECART_ATTACH_PAYMENTS_DB_URL")}
		id := *runID
		if id == "" {
			id = "rc" + time.Now().UTC().Format("060102150405") + randHex(1)
		}
		if !runIDRe.MatchString(id) {
			fatal(fmt.Errorf("--run-id must be 6-16 lower-case letters or digits"))
		}
		os.Exit(RunRemoteCart(repo, id, rc, att))
	case "remote":
		rc := RemoteConfig{ForgeURL: *rmForge, OperatorURL: *rmOperator, StageSeconds: 60, Repetitions: 1, PayloadBytes: 256, DrainTimeoutS: *rmDrain, Note: *rmNote, Batch: *rmBatch}
		for _, t := range strings.Split(*rmTypes, ",") {
			if t = strings.TrimSpace(t); t != "" {
				rc.Types = append(rc.Types, t)
			}
		}
		for _, p := range strings.Split(*e1Rates, ",") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			v, err := strconv.ParseFloat(p, 64)
			if err != nil {
				fatal(fmt.Errorf("--rates must be comma-separated numbers: %w", err))
			}
			rc.Rates = append(rc.Rates, v)
		}
		if *e1Stage > 0 {
			rc.StageSeconds = *e1Stage
		}
		if *e1Reps > 0 {
			rc.Repetitions = *e1Reps
		}
		if *e1Payload > 0 {
			rc.PayloadBytes = *e1Payload
		}
		id := *runID
		if id == "" {
			id = "rm" + time.Now().UTC().Format("060102150405") + randHex(1)
		}
		if !runIDRe.MatchString(id) {
			fatal(fmt.Errorf("--run-id must be 6-16 lower-case letters or digits"))
		}
		os.Exit(RunRemote(repo, id, rc))
	case "e1":
		cfg := DefaultE1()
		if *e1Rates != "" {
			cfg.Rates = nil
			for _, p := range strings.Split(*e1Rates, ",") {
				v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
				if err != nil {
					fatal(fmt.Errorf("--rates must be comma-separated numbers: %w", err))
				}
				cfg.Rates = append(cfg.Rates, v)
			}
		}
		if *e1Stage > 0 {
			cfg.StageSeconds = *e1Stage
		}
		if *e1Sustain >= 0 {
			cfg.SustainedSeconds = *e1Sustain
		}
		if *e1Handler >= 0 {
			cfg.HandlerMS = *e1Handler
		}
		if *e1Payload > 0 {
			cfg.PayloadBytes = *e1Payload
		}
		if *e1Procs > 0 {
			cfg.WorkerProcs = *e1Procs
		}
		if *e1Slots > 0 {
			cfg.SlotsPerProc = *e1Slots
		}
		if *e1MaxQ > 0 {
			cfg.MaxQueued = *e1MaxQ
		}
		cfg.Diagnostics = *e1Diag
		if *e1Reps > 0 {
			cfg.Repetitions = *e1Reps
		}
		cfg.NoWorkers, cfg.Watchdog = *e1NoWorkers, *e1Watchdog
		cfg.Variant, cfg.ForgeBinary, cfg.MigrationsDir = *e1Variant, *e1Bin, *e1Mig
		id := *runID
		if id == "" {
			id = "e1" + time.Now().UTC().Format("060102150405") + randHex(1)
		}
		if !runIDRe.MatchString(id) {
			fatal(fmt.Errorf("--run-id must be 6-16 lower-case letters or digits"))
		}
		os.Exit(RunE1(repo, id, cfg))
	case "run", "dry-run":
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", cmd, usage)
		os.Exit(2)
	}

	opts := Options{Scale: *scale, SoakSeconds: *soak, Seed: *seed, AllowFault: *fault, Race: *race, ObserveEvery: 100}
	if opts.Scale < 0.1 || opts.Scale > 4 {
		fatal(fmt.Errorf("--scale must be between 0.1 and 4"))
	}
	if opts.SoakSeconds < 0 || opts.SoakSeconds > HardMaxSeconds {
		fatal(fmt.Errorf("--soak-seconds must be between 0 and %d", HardMaxSeconds))
	}
	names, profName, err := selectScenarios(*profile, *list, defs, profiles)
	if err != nil {
		fatal(err)
	}
	var plan []ScenarioDef
	var caps []string
	for _, n := range names {
		d, notes := Resolve(defs[n], opts)
		if err := d.Validate(); err != nil {
			fatal(err)
		}
		for _, it := range notes.Items {
			caps = append(caps, n+": "+it)
		}
		plan = append(plan, d)
	}
	if cmd == "dry-run" {
		doDryRun(plan, caps, opts, profName)
		return
	}

	id := *runID
	if id == "" {
		id = time.Now().UTC().Format("060102150405") + randHex(2)
	}
	os.Exit(execute(repo, id, profName, plan, caps, opts))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(2)
}

// findRepo walks up from the working directory to the Forge go.mod.
func findRepo() (string, error) {
	dir, _ := os.Getwd()
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(b), "module forge") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("run from inside the Forge repository (go.mod with module forge not found)")
		}
		dir = parent
	}
}

func selectScenarios(profile, list string, defs map[string]ScenarioDef, profiles map[string][]string) ([]string, string, error) {
	if list != "" {
		var names []string
		for _, n := range strings.Split(list, ",") {
			n = strings.TrimSpace(n)
			if _, ok := defs[n]; !ok {
				return nil, "", fmt.Errorf("unknown scenario %q (see: run.sh list)", n)
			}
			names = append(names, n)
		}
		return names, "custom", nil
	}
	p, ok := profiles[profile]
	if !ok {
		return nil, "", fmt.Errorf("unknown profile %q (see: run.sh list)", profile)
	}
	return p, profile, nil
}

func doList(defs map[string]ScenarioDef, profiles map[string][]string) {
	fmt.Println("Scenarios:")
	for _, n := range Order {
		d := defs[n]
		flag := ""
		if d.Destructive {
			flag = " [fault injection: needs --allow-fault-injection]"
		}
		fmt.Printf("  %-17s %s%s\n", n, d.Title, flag)
	}
	fmt.Println("\nProfiles:")
	for _, k := range sortedKeys(profiles) {
		fmt.Printf("  %-9s %s\n", k, strings.Join(profiles[k], ", "))
	}
	fmt.Println("\nThe default profile is smoke.")
}

func doPreflight(repo string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, problems := Preflight(ctx, repo)
	for _, k := range sortedKeys(info) {
		fmt.Printf("ok       %-16s %s\n", k, info[k])
	}
	for _, p := range problems {
		fmt.Printf("PROBLEM  %s\n", p)
	}
	fmt.Printf("host     %d logical CPUs, %d MB memory\n", runtime.NumCPU(), memTotalMB())
	if len(problems) > 0 {
		return 1
	}
	fmt.Println("preflight passed; nothing was started or changed")
	return 0
}

func doDryRun(plan []ScenarioDef, caps []string, o Options, prof string) {
	fmt.Printf("Dry run: profile %s, scale %.2f, fault injection %v, race %v. Nothing will be started.\n\n", prof, o.Scale, o.AllowFault, o.Race)
	for _, d := range plan {
		b, _ := json.Marshal(d.Params)
		note := ""
		if d.Destructive && !o.AllowFault {
			note = "   -> would be SKIPPED (needs --allow-fault-injection)"
		}
		fmt.Printf("%-17s %s%s\n  params: %s\n", d.Name, d.Title, note, b)
	}
	if len(caps) > 0 {
		fmt.Println("\nSafety caps that would apply:")
		for _, c := range caps {
			fmt.Println("  -", c)
		}
	}
}

func memTotalMB() int {
	b, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(b), "\n") {
		var kb int
		if strings.HasPrefix(l, "MemTotal:") {
			fmt.Sscanf(strings.TrimPrefix(l, "MemTotal:"), "%d", &kb)
			return kb / 1024
		}
	}
	return 0
}

func cpuModel() string {
	b, _ := os.ReadFile("/proc/cpuinfo")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "model name") {
			if i := strings.Index(l, ":"); i >= 0 {
				return strings.TrimSpace(l[i+1:])
			}
		}
	}
	return ""
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// execute runs the plan and returns the process exit code.
func execute(repo, id, profName string, plan []ScenarioDef, caps []string, opts Options) int {
	suiteDir := filepath.Join(repo, "forgecart", "test_files")
	runDir := filepath.Join(suiteDir, "results", id)
	if _, err := os.Stat(runDir); err == nil {
		fatal(fmt.Errorf("results/%s already exists; run directories are immutable, choose another --run-id", id))
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fatal(err)
	}
	logf := func(format string, a ...any) { fmt.Printf(time.Now().Format("15:04:05 ")+format+"\n", a...) }

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	suite := &SuiteRecord{RunID: id, Profile: profName, StartedAt: time.Now(), Options: opts, Caps: caps, Reference: DefaultSandboxConfig()}
	for _, d := range plan {
		suite.Selected = append(suite.Selected, d.Name)
	}

	pfCtx, pfCancel := context.WithTimeout(ctx, 60*time.Second)
	info, problems := Preflight(pfCtx, repo)
	pfCancel()
	if len(problems) > 0 {
		for _, p := range problems {
			logf("preflight problem: %s", p)
		}
		suite.StopReason = "Preflight failed: " + strings.Join(problems, "; ") + "."
		return finalize(suiteDir, runDir, suite, nil)
	}

	sb, err := NewSandbox(repo, runDir, id)
	if err != nil {
		fatal(err)
	}
	// Cleanup always uses a fresh context so it works after Ctrl-C.
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := sb.Down(cctx); err != nil {
			logf("CLEANUP PROBLEM: %v (containers are labelled %s=true, run %s; remove only those)", err, labelKey, id)
		} else {
			logf("sandbox %s removed", sb.Project)
		}
		os.RemoveAll(sb.BinDir)
	}()

	logf("run %s: building binaries%s", id, map[bool]string{true: " (race detector)", false: ""}[opts.Race])
	if err := sb.Build(ctx, opts.Race); err != nil {
		suite.StopReason = "The services could not be built: " + err.Error()
		return finalize(suiteDir, runDir, suite, sb)
	}
	logf("starting sandbox databases (project %s)", sb.Project)
	if err := sb.Up(ctx, DefaultSandboxConfig()); err != nil {
		suite.StopReason = "The sandbox could not be started: " + err.Error()
		return finalize(suiteDir, runDir, suite, sb)
	}
	suite.Env = collectEnv(ctx, sb, info)

	e := NewEnv(ctx, sb, opts, repo, runDir, logf)
	defer func() {
		if e.Probe != nil {
			e.Probe.Close()
		}
	}()
	for _, d := range plan {
		if ctx.Err() != nil {
			suite.Interrupted = true
			suite.StopReason = "Interrupted by signal before all scenarios ran."
			break
		}
		logf("scenario %s", d.Name)
		res := &ScenarioResult{Name: d.Name, Title: d.Title, Kind: d.Kind, Description: d.Description, Params: d.Params, Objectives: d.Objectives, StartedAt: time.Now()}
		fn, ok := runners[d.Kind]
		if !ok {
			res.Status, res.Reasons = "error", []string{"no runner for kind " + d.Kind}
		} else {
			fn(e, d, res)
		}
		res.EndedAt = time.Now()
		res.DurationS = res.EndedAt.Sub(res.StartedAt).Seconds()
		if ctx.Err() != nil && res.Status != "skipped" {
			res.Status = "interrupted"
			res.Reasons = append(res.Reasons, "interrupted by signal; measurements of this scenario are partial")
			suite.Interrupted = true
			suite.StopReason = "Interrupted by signal during scenario " + d.Name + "."
		}
		suite.Scenarios = append(suite.Scenarios, res)
		_ = writeJSON(filepath.Join(runDir, d.Name+".json"), res)
		logf("scenario %s: %s", d.Name, res.Status)
		sb.StopAll()
	}
	suite.Complete = !suite.Interrupted && len(suite.Scenarios) == len(plan)
	return finalize(suiteDir, runDir, suite, sb)
}

func collectEnv(ctx context.Context, sb *Sandbox, tools map[string]string) EnvInfo {
	k, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	e := EnvInfo{Kernel: strings.TrimSpace(string(k)), CPUModel: cpuModel(), CPUs: runtime.NumCPU(), MemoryMB: memTotalMB(), Versions: tools,
		PgCPUs: DefaultSandboxConfig().PgCPUs, PgMem: DefaultSandboxConfig().PgMem,
		Services: "Forge (remote execution mode) + ForgeCart API + dispatcher + 5 worker processes, built from this repository"}
	for _, role := range sb.dbRoles() { // ask whichever database container this run owns
		if v, err := sb.psql(ctx, role, "postgres", "SHOW server_version"); err == nil {
			e.PostgresVer = strings.TrimSpace(v)
			break
		}
	}
	e.Host = collectHostMeta(ctx, sb.Repo)
	e.Binaries = sb.Binaries
	return e
}

// finalize writes suite.json, environment.json and the reports. A complete
// report replaces report.md (the previous one is archived first); an incomplete
// run writes report-partial.md and leaves the previous full report in place.
func finalize(suiteDir, runDir string, s *SuiteRecord, sb *Sandbox) int {
	s.EndedAt = time.Now()
	if s.Scenarios == nil {
		s.Scenarios = []*ScenarioResult{}
	}
	sort.SliceStable(s.Scenarios, func(i, j int) bool { return orderIdx(s.Scenarios[i].Name) < orderIdx(s.Scenarios[j].Name) })
	_ = writeJSON(filepath.Join(runDir, "suite.json"), s)
	_ = writeJSON(filepath.Join(runDir, "environment.json"), s.Env)
	rep := RenderReport(s)
	if err := os.WriteFile(filepath.Join(runDir, "report.md"), []byte(rep), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "could not write report:", err)
		return 1
	}
	top := filepath.Join(suiteDir, "report.md")
	if s.Complete {
		if old, err := os.ReadFile(top); err == nil {
			arch := filepath.Join(suiteDir, "results", "archive")
			_ = os.MkdirAll(arch, 0o755)
			name := fmt.Sprintf("report-%s.md", time.Now().UTC().Format("20060102T150405Z"))
			if err := os.WriteFile(filepath.Join(arch, name), old, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, "could not archive the previous report; leaving it in place:", err)
				return 1
			}
		}
		_ = os.WriteFile(top, []byte(rep), 0o644)
		fmt.Printf("report: %s\n", top)
	} else {
		p := filepath.Join(suiteDir, "report-partial.md")
		_ = os.WriteFile(p, []byte(rep), 0o644)
		fmt.Printf("INCOMPLETE run; partial report: %s (previous report.md, if any, is unchanged)\n", p)
	}
	for _, r := range s.Scenarios {
		if r.Status == "failed" || r.Status == "error" {
			return 1
		}
	}
	if !s.Complete {
		return 1
	}
	return 0
}

func orderIdx(n string) int {
	for i, o := range Order {
		if o == n {
			return i
		}
	}
	return len(Order)
}

// doReport re-renders report.md from results/<run-id>/suite.json with the current
// reporting code. It only reads the run directory and never overwrites a file.
func doReport(repo, from, out string) int {
	if !runIDRe.MatchString(from) || out == "" {
		fmt.Fprintln(os.Stderr, "usage: run.sh report --from <run-id> --out <new report file>")
		return 2
	}
	suiteDir := filepath.Join(repo, "forgecart", "test_files")
	b, err := os.ReadFile(filepath.Join(suiteDir, "results", from, "suite.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read the preserved run:", err)
		return 2
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.HasPrefix(absOut, filepath.Join(suiteDir, "results")+string(filepath.Separator)) {
		fmt.Fprintln(os.Stderr, "refusing to write inside results/: run directories are immutable")
		return 2
	}
	var s SuiteRecord
	if err := json.Unmarshal(b, &s); err != nil {
		fmt.Fprintln(os.Stderr, "cannot parse suite.json:", err)
		return 2
	}
	rep := fmt.Sprintf("> **Regenerated** from `results/%s/suite.json` with the current reporting code on %s. The raw measurements are unchanged; calculations, labels and tables below follow the current definitions, and metadata fields recorded after this run was made are marked unavailable.\n\n", from, time.Now().Format("2006-01-02")) + RenderReport(&s)
	f, err := os.OpenFile(absOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create the output file (it must not exist):", err)
		return 2
	}
	defer f.Close()
	if _, err := f.WriteString(rep); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("wrote", absOut)
	return 0
}
