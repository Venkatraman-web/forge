package main

// The disposable load-test sandbox: two PostgreSQL containers (Docker Compose,
// unique project, tmpfs) plus the Forge, ForgeCart, dispatcher and worker
// processes started from freshly built binaries. Everything destructive goes
// through assertOwned(), which proves the target is this suite's own sandbox.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
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

var runIDRe = regexp.MustCompile(`^[a-z0-9]{6,16}$`)

// jobsPerOrder is how many background jobs one paid order produces: one per
// business worker.
var jobsPerOrder = len(WorkerTypes)

// WorkerTypes are the business workers, in a fixed order.
var WorkerTypes = []struct{ Type, Bin, Short string }{
	{"inventory.reconcile", "forgecart-inventory-worker", "inventory"},
	{"invoice.generate", "forgecart-invoice-worker", "invoice"},
	{"notification.send", "forgecart-notification-worker", "notification"},
	{"fulfillment.prepare", "forgecart-fulfillment-worker", "fulfillment"},
}

// SandboxConfig describes how the services are configured for a scenario.
type SandboxConfig struct {
	ForgeMaxQueued     int            `json:"forge_max_queued"`
	WorkerMaxPolls     int            `json:"forge_worker_max_polls"`
	Lease              string         `json:"forge_lease_duration"`
	Heartbeat          string         `json:"forge_heartbeat_interval"`
	SafetyMargin       string         `json:"forge_lease_safety_margin"`
	ReaperInterval     string         `json:"forge_reaper_interval"`
	DispatcherBatch    int            `json:"dispatcher_batch_size"`
	DispatcherPoll     string         `json:"dispatcher_poll_interval"`
	DispatcherReplicas int            `json:"dispatcher_replicas"`
	WorkerReplicas     map[string]int `json:"worker_replicas"`    // by short name; default 1
	WorkerConcurrency  map[string]int `json:"worker_concurrency"` // per process, 1-8; default 4
	PaymentSuccess     int            `json:"payment_success_pct"`
	PaymentDecline     int            `json:"payment_decline_pct"`
	PaymentTemp        int            `json:"payment_temp_failure_pct"`
	PaymentSeed        string         `json:"payment_seed"`
	PaymentMaxDelay    string         `json:"payment_max_delay"`
	Inventory          int            `json:"initial_inventory_per_product"`
	PgCPUs             string         `json:"postgres_cpus"`
	PgMem              string         `json:"postgres_memory"`
	StartWorkers       bool           `json:"start_workers"`
}

// DefaultSandboxConfig is the reference configuration.
func DefaultSandboxConfig() SandboxConfig {
	return SandboxConfig{ForgeMaxQueued: 50000, WorkerMaxPolls: 512, Lease: "15s", Heartbeat: "5s", SafetyMargin: "10s", ReaperInterval: "5s",
		DispatcherBatch: 50, DispatcherPoll: "200ms", DispatcherReplicas: 1, WorkerReplicas: map[string]int{}, WorkerConcurrency: map[string]int{},
		PaymentSuccess: 100, PaymentDecline: 0, PaymentTemp: 0, PaymentSeed: "forgecart-lt", PaymentMaxDelay: "0s",
		Inventory: 1_000_000, PgCPUs: "4", PgMem: "2g", StartWorkers: true}
}

func (c SandboxConfig) replicas(short string) int {
	if n := c.WorkerReplicas[short]; n > 0 {
		return n
	}
	return 1
}
func (c SandboxConfig) concurrency(short string) int {
	if n := c.WorkerConcurrency[short]; n > 0 {
		return min(n, 8)
	}
	return 4
}

// Proc is one managed host process.
type Proc struct {
	Name   string
	Kind   string // forge, api, dispatcher, worker
	Short  string // worker short name
	cmd    *exec.Cmd
	log    *os.File
	done   chan struct{}
	exited atomic.Bool
}

type Sandbox struct {
	RunID, Project string
	Repo, RunDir   string
	BinDir, LogDir string
	Ports          struct{ ForgeAPI, ForgeWorker, CartAPI, PaymentAPI, ForgeDB, AppDB int }
	password, key  string // key is the worker key; apiKey is Forge's submit key
	payKey         string // shared by the shop and the payment service
	apiKey         string
	attach         *AttachConfig // non-nil: attached to a running deployment, nothing is managed
	ForgeDBName    string
	AppDBName      string
	PayDBName      string // the payment service's database: a second database in the app-db container

	mu    sync.Mutex
	procs map[string]*Proc
	cfg   SandboxConfig
	up    bool

	trialSeq int

	Binaries []BinaryInfo // hashes and build flags of what Build produced

	// Roles lists the database containers this sandbox owns ("forge-db",
	// "app-db"). Empty means both, which is what every ForgeCart scenario uses.
	// The Forge-isolated experiment (E-1) sets just "forge-db": no ForgeCart
	// database exists in that run.
	Roles []string
}

func (s *Sandbox) dbRoles() []string {
	if len(s.Roles) > 0 {
		return s.Roles
	}
	return []string{"forge-db", "app-db"}
}

const (
	labelKey = "com.forgecart.loadtest"
	labelRun = "com.forgecart.loadtest.run"
)

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func NewSandbox(repo, runDir, runID string) (*Sandbox, error) {
	if !runIDRe.MatchString(runID) {
		return nil, errors.New("run id must be 6-16 lower-case letters or digits")
	}
	s := &Sandbox{RunID: runID, Project: "forgecart-lt-" + runID, Repo: repo, RunDir: runDir,
		BinDir: filepath.Join(runDir, "bin"), LogDir: filepath.Join(runDir, "logs"),
		password: randHex(12), key: randHex(24), apiKey: randHex(24), payKey: randHex(24), ForgeDBName: "forge_lt", AppDBName: "forgecart_lt", PayDBName: "forgecart_pay_lt", procs: map[string]*Proc{}}
	for _, p := range []*int{&s.Ports.ForgeAPI, &s.Ports.ForgeWorker, &s.Ports.CartAPI, &s.Ports.PaymentAPI, &s.Ports.ForgeDB, &s.Ports.AppDB} {
		port, err := freePort()
		if err != nil {
			return nil, err
		}
		*p = port
	}
	for _, d := range []string{s.BinDir, s.LogDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Sandbox) forgeURL() string {
	return fmt.Sprintf("postgres://lt:%s@127.0.0.1:%d/%s?sslmode=disable", s.password, s.Ports.ForgeDB, s.ForgeDBName)
}
func (s *Sandbox) appURL() string {
	return fmt.Sprintf("postgres://lt:%s@127.0.0.1:%d/%s?sslmode=disable", s.password, s.Ports.AppDB, s.AppDBName)
}

func (s *Sandbox) payURL() string {
	return fmt.Sprintf("postgres://lt:%s@127.0.0.1:%d/%s?sslmode=disable", s.password, s.Ports.AppDB, s.PayDBName)
}

// PaymentsDBURL returns the payment service's database URL (never logged).
func (s *Sandbox) PaymentsDBURL() string {
	if s.attach != nil {
		return s.attach.PaymentsDBURL
	}
	return s.payURL()
}

// AppDBURL returns the sandbox's own ForgeCart database URL (never logged).
func (s *Sandbox) AppDBURL() string {
	if s.attach != nil {
		return s.attach.AppDBURL
	}
	return s.appURL()
}

// ResultsDBURL is the workers' separate results database in attach mode, or ""
// when their result tables are in the ForgeCart database.
func (s *Sandbox) ResultsDBURL() string {
	if s.attach != nil {
		return s.attach.ResultsDBURL
	}
	return ""
}

// ForgeDBServerURL is used only for server-level resource metrics (pg_stat_activity).
func (s *Sandbox) ForgeDBServerURL() string {
	if s.attach != nil {
		return "" // no access to Forge's database in attach mode
	}
	return fmt.Sprintf("postgres://lt:%s@127.0.0.1:%d/postgres?sslmode=disable", s.password, s.Ports.ForgeDB)
}
func (s *Sandbox) AppDBServerURL() string {
	if s.attach != nil {
		return s.attach.AppDBURL
	}
	return fmt.Sprintf("postgres://lt:%s@127.0.0.1:%d/postgres?sslmode=disable", s.password, s.Ports.AppDB)
}
func (s *Sandbox) CartURL() string {
	if s.attach != nil {
		return s.attach.CartURL
	}
	return fmt.Sprintf("http://127.0.0.1:%d", s.Ports.CartAPI)
}
func (s *Sandbox) ForgeURL() string {
	if s.attach != nil {
		return s.attach.ForgeURL
	}
	return fmt.Sprintf("http://127.0.0.1:%d", s.Ports.ForgeAPI)
}

func (s *Sandbox) container(role string) string { return s.Project + "-" + role }

func run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// assertOwned proves both database containers are this run's sandbox: the exact
// expected names, our labels with this run id, and the project's network. Every
// destructive operation calls it first.
func (s *Sandbox) assertOwned(ctx context.Context) error {
	for _, role := range s.dbRoles() {
		name := s.container(role)
		if !strings.HasPrefix(name, "forgecart-lt-") || strings.Contains(name, "forge-postgres") {
			return fmt.Errorf("refusing to touch container %q: not a load-test sandbox name", name)
		}
		out, err := run(ctx, "", nil, "docker", "inspect", "--format", `{{index .Config.Labels "`+labelKey+`"}}|{{index .Config.Labels "`+labelRun+`"}}|{{.Name}}`, name)
		if err != nil {
			return fmt.Errorf("sandbox container %s is not available: %v", name, strings.TrimSpace(out))
		}
		parts := strings.Split(strings.TrimSpace(out), "|")
		if len(parts) != 3 || parts[0] != "true" || parts[1] != s.RunID || parts[2] != "/"+name {
			return fmt.Errorf("refusing to touch container %s: its labels do not identify it as the sandbox of run %s", name, s.RunID)
		}
	}
	return nil
}

// Preflight checks the tools the suite needs.
func Preflight(ctx context.Context, repo string) (map[string]string, []string) {
	info := map[string]string{}
	var problems []string
	check := func(key string, name string, args ...string) {
		out, err := run(ctx, repo, nil, name, args...)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s is not usable (%s)", name, strings.TrimSpace(firstLine(out))))
			return
		}
		info[key] = strings.TrimSpace(firstLine(out))
	}
	check("go", "go", "version")
	check("docker", "docker", "--version")
	check("docker_compose", "docker", "compose", "version")
	check("atlas", "atlas", "version")
	if out, err := run(ctx, repo, nil, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		problems = append(problems, "the Docker daemon is not reachable: "+strings.TrimSpace(firstLine(out)))
	} else {
		info["docker_server"] = strings.TrimSpace(out)
	}
	if out, err := run(ctx, repo, nil, "docker", "image", "inspect", "postgres:16-alpine", "--format", "{{.Id}}"); err != nil {
		info["postgres_image"] = "postgres:16-alpine not present locally (it will be pulled)"
		_ = out
	} else {
		info["postgres_image"] = "postgres:16-alpine (present)"
	}
	return info, problems
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Build compiles every binary the sandbox runs.
func (s *Sandbox) Build(ctx context.Context, race bool) error {
	targets := map[string]string{
		"forge":                "./cmd/forge",
		"forgecart":            "./forgecart/cmd/forgecart",
		"forgecart-dispatcher": "./forgecart/cmd/forgecart-dispatcher",
		"forgecart-payments":   "./forgecart/cmd/forgecart-payments",
	}
	for _, w := range WorkerTypes {
		targets[w.Bin] = "./forgecart/cmd/" + w.Bin
	}
	names := make([]string, 0, len(targets))
	for n := range targets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		args := []string{"build", "-o", filepath.Join(s.BinDir, n)}
		if race {
			args = append(args, "-race")
		}
		args = append(args, targets[n])
		if out, err := run(ctx, s.Repo, nil, "go", args...); err != nil {
			return fmt.Errorf("building %s failed: %s", n, strings.TrimSpace(out))
		}
		s.Binaries = append(s.Binaries, binaryInfo(ctx, filepath.Join(s.BinDir, n)))
	}
	return nil
}

// Up starts the two database containers (unique compose project) and waits until healthy.
func (s *Sandbox) Up(ctx context.Context, cfg SandboxConfig) error {
	env := append(os.Environ(),
		"LT_PROJECT="+s.Project, "LT_RUN="+s.RunID, "LT_DB_PASSWORD="+s.password,
		"LT_FORGE_DB_PORT="+strconv.Itoa(s.Ports.ForgeDB), "LT_APP_DB_PORT="+strconv.Itoa(s.Ports.AppDB),
		"LT_PG_CPUS="+cfg.PgCPUs, "LT_PG_MEM="+cfg.PgMem, "LT_PG_MAX_CONN=400")
	compose := filepath.Join(s.Repo, "forgecart", "test_files", "sandbox", "compose.yaml")
	upArgs := append([]string{"compose", "-p", s.Project, "-f", compose, "up", "-d", "--wait", "--wait-timeout", "90"}, s.dbRoles()...)
	if out, err := run(ctx, s.Repo, env, "docker", upArgs...); err != nil {
		return fmt.Errorf("starting the sandbox databases failed: %s", strings.TrimSpace(tail(out, 600)))
	}
	s.up = true
	return s.assertOwned(ctx)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// Down stops every managed process and removes the sandbox containers and network
// of THIS project only (explicit project name, ownership verified first).
func (s *Sandbox) Down(ctx context.Context) error {
	s.StopAll()
	if !s.up {
		return nil
	}
	if err := s.assertOwned(ctx); err != nil {
		return fmt.Errorf("not removing anything: %w", err)
	}
	compose := filepath.Join(s.Repo, "forgecart", "test_files", "sandbox", "compose.yaml")
	env := append(os.Environ(), "LT_PROJECT="+s.Project, "LT_RUN="+s.RunID, "LT_DB_PASSWORD=x", "LT_FORGE_DB_PORT=1", "LT_APP_DB_PORT=1", "LT_PG_CPUS=1", "LT_PG_MEM=1g", "LT_PG_MAX_CONN=100")
	out, err := run(ctx, s.Repo, env, "docker", "compose", "-p", s.Project, "-f", compose, "down", "-v", "--remove-orphans", "--timeout", "10")
	s.up = false
	if err != nil {
		return fmt.Errorf("sandbox cleanup failed: %s", strings.TrimSpace(tail(out, 400)))
	}
	return nil
}

// psql runs one SQL statement as the sandbox superuser inside the OWNED container.
func (s *Sandbox) psql(ctx context.Context, role, db, sql string) (string, error) {
	if err := s.assertOwned(ctx); err != nil {
		return "", err
	}
	out, err := run(ctx, "", nil, "docker", "exec", s.container(role), "psql", "-U", "lt", "-d", db, "-v", "ON_ERROR_STOP=1", "-Atc", sql)
	if err != nil {
		return out, fmt.Errorf("psql failed: %s", strings.TrimSpace(tail(out, 300)))
	}
	return strings.TrimSpace(out), nil
}

// ResetState rebuilds both databases from the versioned migrations, seeds the
// deterministic catalog and starts the services with cfg. All destructive steps
// run only after assertOwned.
func (s *Sandbox) ResetState(ctx context.Context, cfg SandboxConfig) error {
	if s.attach != nil {
		return s.resetAttached(ctx, cfg)
	}
	s.StopAll()
	s.cfg = cfg
	s.archiveLogs()
	for _, d := range []struct{ role, db string }{{"forge-db", s.ForgeDBName}, {"app-db", s.AppDBName}} {
		if _, err := s.psql(ctx, d.role, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", d.db)); err != nil {
			return err
		}
		if _, err := s.psql(ctx, d.role, "postgres", "CREATE DATABASE "+d.db); err != nil {
			return err
		}
	}
	if out, err := run(ctx, s.Repo, nil, "atlas", "migrate", "apply", "--dir", "file://migrations", "--url", s.forgeURL()); err != nil {
		return fmt.Errorf("Forge migrations failed: %s", strings.TrimSpace(tail(out, 400)))
	}
	if out, err := run(ctx, s.Repo, nil, "atlas", "migrate", "apply", "--dir", "file://forgecart/migrations", "--url", s.appURL()); err != nil {
		return fmt.Errorf("ForgeCart migrations failed: %s", strings.TrimSpace(tail(out, 400)))
	}
	if _, err := s.psql(ctx, "app-db", "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", s.PayDBName)); err != nil {
		return err
	}
	if _, err := s.psql(ctx, "app-db", "postgres", "CREATE DATABASE "+s.PayDBName); err != nil {
		return err
	}
	if out, err := run(ctx, s.Repo, nil, "atlas", "migrate", "apply", "--dir", "file://forgecart/payments_migrations", "--url", s.payURL()); err != nil {
		return fmt.Errorf("payment service migrations failed: %s", strings.TrimSpace(tail(out, 400)))
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "FORGECART_DATABASE_URL=" + s.appURL()}
	if out, err := run(ctx, s.Repo, env, filepath.Join(s.BinDir, "forgecart"), "seed"); err != nil {
		return fmt.Errorf("seeding failed: %s", strings.TrimSpace(tail(out, 300)))
	}
	if _, err := s.psql(ctx, "app-db", s.AppDBName, fmt.Sprintf("UPDATE products SET inventory_quantity = %d", cfg.Inventory)); err != nil {
		return err
	}
	return s.startServices(ctx, cfg)
}

// archiveLogs moves the previous trial's service logs into logs/trial-NNN/ so
// the log parser only ever sees the current trial (process restarts inside a
// trial still append to the same file). Nothing is deleted.
func (s *Sandbox) archiveLogs() {
	files, _ := filepath.Glob(filepath.Join(s.LogDir, "*.log"))
	if len(files) == 0 {
		return
	}
	s.trialSeq++
	dir := filepath.Join(s.LogDir, fmt.Sprintf("trial-%03d", s.trialSeq))
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	for _, f := range files {
		os.Rename(f, filepath.Join(dir, filepath.Base(f)))
	}
}

// ForgeRequest builds a request to this sandbox's Forge submission API with the
// submit key attached.
func (s *Sandbox) ForgeRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.ForgeURL()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (s *Sandbox) forgeEnv(cfg SandboxConfig) []string {
	return []string{"PATH=" + os.Getenv("PATH"),
		"FORGE_DATABASE_URL=" + s.forgeURL(),
		fmt.Sprintf("FORGE_ADDR=127.0.0.1:%d", s.Ports.ForgeAPI),
		"FORGE_EXECUTION_MODE=remote",
		// Job types and their retry/timeout policy come from the ForgeCart config file.
		"FORGE_CONFIG_FILE=" + filepath.Join(s.Repo, "forgecart", "forge.json"),
		fmt.Sprintf("FORGE_WORKER_ADDR=127.0.0.1:%d", s.Ports.ForgeWorker),
		"FORGE_WORKER_API_KEY=" + s.key, "FORGE_API_KEY=" + s.apiKey,
		fmt.Sprintf("FORGE_MAX_QUEUED=%d", cfg.ForgeMaxQueued),
		fmt.Sprintf("FORGE_WORKER_MAX_POLLS=%d", cfg.WorkerMaxPolls),
		"FORGE_LEASE_DURATION=" + cfg.Lease, "FORGE_HEARTBEAT_INTERVAL=" + cfg.Heartbeat,
		"FORGE_LEASE_SAFETY_MARGIN=" + cfg.SafetyMargin, "FORGE_REAPER_INTERVAL=" + cfg.ReaperInterval,
		"FORGE_RETRY_BASE_DELAY=1s", "FORGE_RETRY_MAX_DELAY=10s"}
}

func (s *Sandbox) cartEnv(cfg SandboxConfig) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "FORGECART_DATABASE_URL=" + s.appURL(),
		fmt.Sprintf("FORGECART_ADDR=127.0.0.1:%d", s.Ports.CartAPI),
		fmt.Sprintf("FORGECART_PAYMENT_URL=http://127.0.0.1:%d", s.Ports.PaymentAPI), "FORGECART_PAYMENT_API_KEY=" + s.payKey}
}

// paymentEnv is the payment service's environment: the outcome mix is its setting, not the shop's.
func (s *Sandbox) paymentEnv(cfg SandboxConfig) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "FORGECART_PAYMENTS_DATABASE_URL=" + s.payURL(),
		fmt.Sprintf("FORGECART_PAYMENTS_ADDR=127.0.0.1:%d", s.Ports.PaymentAPI), "FORGECART_PAYMENT_API_KEY=" + s.payKey,
		fmt.Sprintf("FORGECART_PAYMENT_SUCCESS_PCT=%d", cfg.PaymentSuccess), fmt.Sprintf("FORGECART_PAYMENT_DECLINE_PCT=%d", cfg.PaymentDecline),
		fmt.Sprintf("FORGECART_PAYMENT_TEMP_FAILURE_PCT=%d", cfg.PaymentTemp), "FORGECART_PAYMENT_SEED=" + cfg.PaymentSeed,
		"FORGECART_PAYMENT_MAX_DELAY=" + cfg.PaymentMaxDelay}
}

func (s *Sandbox) dispatcherEnv(cfg SandboxConfig) []string {
	lease := "30s"
	return []string{"PATH=" + os.Getenv("PATH"), "FORGECART_DATABASE_URL=" + s.appURL(),
		"FORGECART_FORGE_URL=" + s.ForgeURL(), "FORGECART_FORGE_API_KEY=" + s.apiKey,
		"FORGECART_DISPATCHER_POLL_INTERVAL=" + cfg.DispatcherPoll,
		fmt.Sprintf("FORGECART_DISPATCHER_BATCH_SIZE=%d", cfg.DispatcherBatch), "FORGECART_DISPATCHER_LEASE_DURATION=" + lease}
}

func (s *Sandbox) workerEnv(cfg SandboxConfig, short, name string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "FORGECART_DATABASE_URL=" + s.appURL(),
		fmt.Sprintf("FORGECART_FORGE_WORKER_URL=http://127.0.0.1:%d", s.Ports.ForgeWorker),
		"FORGECART_FORGE_WORKER_API_KEY=" + s.key, "FORGECART_WORKER_NAME=" + name,
		fmt.Sprintf("FORGECART_WORKER_CONCURRENCY=%d", cfg.concurrency(short))}
}

func (s *Sandbox) startServices(ctx context.Context, cfg SandboxConfig) error {
	if err := s.StartProc(ctx, "forge", "forge", "forge", "", s.forgeEnv(cfg)); err != nil {
		return err
	}
	if err := waitHTTP(ctx, s.ForgeURL()+"/health", 30*time.Second); err != nil {
		return fmt.Errorf("Forge did not become ready: %w", err)
	}
	if err := s.StartProc(ctx, "payments", "forgecart-payments", "payments", "", s.paymentEnv(cfg)); err != nil {
		return err
	}
	if err := waitHTTP(ctx, fmt.Sprintf("http://127.0.0.1:%d/healthz", s.Ports.PaymentAPI), 30*time.Second); err != nil {
		return fmt.Errorf("the payment service did not become ready: %w", err)
	}
	if err := s.StartProc(ctx, "forgecart", "forgecart", "api", "", s.cartEnv(cfg)); err != nil {
		return err
	}
	if err := waitHTTP(ctx, s.CartURL()+"/readyz", 30*time.Second); err != nil {
		return fmt.Errorf("ForgeCart did not become ready: %w", err)
	}
	for i := 1; i <= cfg.DispatcherReplicas; i++ {
		if err := s.StartProc(ctx, fmt.Sprintf("dispatcher-%d", i), "forgecart-dispatcher", "dispatcher", "", s.dispatcherEnv(cfg)); err != nil {
			return err
		}
	}
	if cfg.StartWorkers {
		if err := s.StartWorkers(ctx, cfg); err != nil {
			return err
		}
	}
	return s.waitStarted(ctx, 30*time.Second)
}

// StartWorkers starts every worker replica of cfg and waits until they report ready.
func (s *Sandbox) StartWorkers(ctx context.Context, cfg SandboxConfig) error {
	for _, w := range WorkerTypes {
		for i := 1; i <= cfg.replicas(w.Short); i++ {
			name := fmt.Sprintf("worker-%s-%d", w.Short, i)
			if err := s.StartProc(ctx, name, w.Bin, "worker", w.Short, s.workerEnv(cfg, w.Short, name)); err != nil {
				return err
			}
		}
	}
	return s.waitStarted(ctx, 30*time.Second)
}

// PauseAppDB freezes the sandbox ForgeCart database container (docker pause) after
// proving ownership; UnpauseAppDB resumes it.
func (s *Sandbox) PauseAppDB(ctx context.Context) error {
	if err := s.assertOwned(ctx); err != nil {
		return err
	}
	out, err := run(ctx, "", nil, "docker", "pause", s.container("app-db"))
	if err != nil {
		return fmt.Errorf("docker pause failed: %s", strings.TrimSpace(out))
	}
	return nil
}

func (s *Sandbox) UnpauseAppDB(ctx context.Context) error {
	if err := s.assertOwned(ctx); err != nil {
		return err
	}
	out, err := run(ctx, "", nil, "docker", "unpause", s.container("app-db"))
	if err != nil {
		return fmt.Errorf("docker unpause failed: %s", strings.TrimSpace(out))
	}
	return nil
}

// StartProc starts (or restarts) a managed process; its log is appended to logs/<name>.log.
func (s *Sandbox) StartProc(ctx context.Context, name, bin, kind, short string, env []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.procs[name]; ok && !p.exited.Load() {
		return fmt.Errorf("process %s is already running", name)
	}
	lf, err := os.OpenFile(filepath.Join(s.LogDir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(s.BinDir, bin))
	cmd.Dir = s.Repo
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("starting %s failed: %w", name, err)
	}
	p := &Proc{Name: name, Kind: kind, Short: short, cmd: cmd, log: lf, done: make(chan struct{})}
	go func() { cmd.Wait(); p.exited.Store(true); close(p.done); lf.Close() }()
	s.procs[name] = p
	return nil
}

// ProcEnv rebuilds the environment of a named process (for restarts).
func (s *Sandbox) ProcEnv(name string) ([]string, string, string, string) {
	cfg := s.cfg
	switch {
	case name == "forge":
		return s.forgeEnv(cfg), "forge", "forge", ""
	case name == "forgecart":
		return s.cartEnv(cfg), "forgecart", "api", ""
	case name == "payments":
		return s.paymentEnv(cfg), "forgecart-payments", "payments", ""
	case strings.HasPrefix(name, "dispatcher-"):
		return s.dispatcherEnv(cfg), "forgecart-dispatcher", "dispatcher", ""
	}
	for _, w := range WorkerTypes {
		if strings.HasPrefix(name, "worker-"+w.Short+"-") {
			return s.workerEnv(cfg, w.Short, name), w.Bin, "worker", w.Short
		}
	}
	return nil, "", "", ""
}

func (s *Sandbox) RestartProc(ctx context.Context, name string) error {
	env, bin, kind, short := s.ProcEnv(name)
	if bin == "" {
		return fmt.Errorf("unknown process %s", name)
	}
	return s.StartProc(ctx, name, bin, kind, short, env)
}

// StopProc stops a process: SIGTERM (graceful) or SIGKILL, then waits.
func (s *Sandbox) StopProc(name string, graceful bool) error {
	s.mu.Lock()
	p := s.procs[name]
	s.mu.Unlock()
	if p == nil || p.exited.Load() {
		return nil
	}
	sig := syscall.SIGTERM
	if !graceful {
		sig = syscall.SIGKILL
	}
	syscall.Kill(-p.cmd.Process.Pid, sig)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
	return nil
}

// StopKind stops all processes of a kind (and optional worker short name) concurrently.
func (s *Sandbox) StopKind(kind, short string, graceful bool) {
	var wg sync.WaitGroup
	for _, n := range s.ProcNames() {
		p := s.proc(n)
		if p.Kind == kind && (short == "" || p.Short == short) {
			wg.Add(1)
			go func() { defer wg.Done(); s.StopProc(n, graceful) }()
		}
	}
	wg.Wait()
}

func (s *Sandbox) StartKind(ctx context.Context, kind, short string) error {
	for _, n := range s.ProcNames() {
		p := s.proc(n)
		if p.Kind == kind && (short == "" || p.Short == short) && p.exited.Load() {
			if err := s.RestartProc(ctx, n); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Sandbox) proc(name string) *Proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[name]
}

func (s *Sandbox) ProcNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.procs))
	for n := range s.procs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// StopAll stops every managed process, workers and dispatchers first.
func (s *Sandbox) StopAll() {
	for _, kind := range []string{"worker", "dispatcher", "api", "payments", "forge"} {
		s.StopKind(kind, "", true)
	}
	s.mu.Lock()
	s.procs = map[string]*Proc{}
	s.mu.Unlock()
}

func (s *Sandbox) waitStarted(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	for _, n := range s.ProcNames() {
		p := s.proc(n)
		if p.Kind != "worker" && p.Kind != "dispatcher" {
			continue
		}
		marker := "worker_started"
		if p.Kind == "dispatcher" {
			marker = "dispatcher started"
		}
		for {
			b, _ := os.ReadFile(filepath.Join(s.LogDir, n+".log"))
			if bytes.Contains(b, []byte(marker)) {
				break
			}
			if p.exited.Load() {
				return fmt.Errorf("%s exited during startup: %s", n, strings.TrimSpace(tail(string(b), 300)))
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return fmt.Errorf("%s did not report readiness", n)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return nil
}

func waitHTTP(ctx context.Context, url string, d time.Duration) error {
	deadline := time.Now().Add(d)
	c := &http.Client{Timeout: 2 * time.Second}
	var last error
	for time.Now().Before(deadline) && ctx.Err() == nil {
		resp, err := c.Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

// ---------- resources ----------

// ProcStat is the cumulative CPU time and resident memory of one process.
type ProcStat struct {
	CPUSeconds float64 `json:"cpu_seconds"`
	RSSMB      float64 `json:"rss_mb"`
}

func readProc(pid int) (ProcStat, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ProcStat{}, false
	}
	str := string(b)
	i := strings.LastIndex(str, ")")
	f := strings.Fields(str[i+2:])
	if len(f) < 13 {
		return ProcStat{}, false
	}
	var ut, st float64
	fmt.Sscan(f[11], &ut)
	fmt.Sscan(f[12], &st)
	ps := ProcStat{CPUSeconds: (ut + st) / 100}
	if m, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
		for _, line := range strings.Split(string(m), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				var kb float64
				fmt.Sscan(strings.TrimSpace(strings.TrimPrefix(line, "VmRSS:")), &kb)
				ps.RSSMB = kb / 1024
			}
		}
	}
	return ps, true
}

// SampleProcs reads every running managed process.
func (s *Sandbox) SampleProcs() map[string]ProcStat {
	out := map[string]ProcStat{}
	for _, n := range s.ProcNames() {
		p := s.proc(n)
		if p.exited.Load() {
			continue
		}
		if st, ok := readProc(p.cmd.Process.Pid); ok {
			out[n] = st
		}
	}
	return out
}

// ContainerStat is one docker stats reading.
type ContainerStat struct {
	CPUPct float64 `json:"cpu_pct"`
	MemMB  float64 `json:"mem_mb"`
}

// SampleContainers runs one `docker stats --no-stream` for the two sandbox databases.
func (s *Sandbox) SampleContainers(ctx context.Context) map[string]ContainerStat {
	if s.attach != nil {
		return map[string]ContainerStat{} // no containers are managed in attach mode
	}
	statsArgs := []string{"stats", "--no-stream", "--format", "{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}"}
	for _, role := range s.dbRoles() {
		statsArgs = append(statsArgs, s.container(role))
	}
	out, err := run(ctx, "", nil, "docker", statsArgs...)
	res := map[string]ContainerStat{}
	if err != nil {
		return res
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		p := strings.Split(line, "|")
		if len(p) != 3 {
			continue
		}
		var cs ContainerStat
		fmt.Sscan(strings.TrimSuffix(p[1], "%"), &cs.CPUPct)
		mem := strings.Fields(strings.Split(p[2], "/")[0])
		if len(mem) == 1 {
			v := mem[0]
			var x float64
			switch {
			case strings.HasSuffix(v, "GiB"):
				fmt.Sscan(strings.TrimSuffix(v, "GiB"), &x)
				x *= 1024
			case strings.HasSuffix(v, "MiB"):
				fmt.Sscan(strings.TrimSuffix(v, "MiB"), &x)
			case strings.HasSuffix(v, "KiB"):
				fmt.Sscan(strings.TrimSuffix(v, "KiB"), &x)
				x /= 1024
			}
			cs.MemMB = x
		}
		name := strings.TrimPrefix(p[0], s.Project+"-")
		res[name] = cs
	}
	return res
}
