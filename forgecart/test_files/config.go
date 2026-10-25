package main

// Scenario definitions live in scenarios/*.json (version controlled, embedded in
// the binary). Options apply bounded overrides; every clamp to a hard safety
// limit is recorded so the report can say a stage was reduced or skipped.

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed scenarios/*.json
var scenarioFS embed.FS

// Mix is a simulated-payment outcome mix in percent (must sum to 100).
type Mix struct {
	Success int `json:"success"`
	Decline int `json:"decline"`
	Temp    int `json:"temp_failure"`
}

type Profile struct {
	Name string `json:"name"`
	Mix  Mix    `json:"mix"`
}

type ScalingConfig struct {
	Name        string `json:"name"`
	Replicas    int    `json:"replicas_per_worker_type"`
	Concurrency int    `json:"concurrency_per_worker"`
}

// Objectives are performance targets. A null value means "unspecified by the
// owner": the report then shows the measurement and marks the target unspecified.
type Objectives struct {
	P95MS           *float64 `json:"p95_workflow_ms"`
	P99MS           *float64 `json:"p99_workflow_ms"`
	MinWfPerSec     *float64 `json:"min_completed_workflows_per_s"`
	MaxDrainSeconds *float64 `json:"max_drain_s"`
}

// Params is the union of all scenario parameters (unused ones stay zero).
type Params struct {
	Seed             int64           `json:"seed"`
	Workflows        int             `json:"workflows"`
	Concurrency      int             `json:"concurrency"`
	Repetitions      int             `json:"repetitions"`
	Mix              *Mix            `json:"payment_mix"`
	PaymentMaxDelay  string          `json:"payment_max_delay"`
	Inventory        int             `json:"inventory_per_product"`
	DrainTimeoutS    int             `json:"drain_timeout_s"`
	Concurrencies    []int           `json:"concurrencies"`
	StageWorkflows   int             `json:"stage_workflows"`
	StageSeconds     int             `json:"stage_seconds"`
	MaxErrorRatePct  float64         `json:"stop_error_rate_pct"`
	StopP95MS        float64         `json:"stop_p95_ms"`
	StopP99MS        float64         `json:"stop_p99_ms"`
	BaseConcurrency  int             `json:"base_concurrency"`
	BurstConcurrency int             `json:"burst_concurrency"`
	BaseSeconds      int             `json:"base_seconds"`
	BurstSeconds     int             `json:"burst_seconds"`
	RecoverySeconds  int             `json:"recovery_seconds"`
	DurationSeconds  int             `json:"duration_s"`
	WindowSeconds    int             `json:"window_s"`
	StartConcurrency int             `json:"start_concurrency"`
	Factor           float64         `json:"factor"`
	MaxConcurrency   int             `json:"max_concurrency"`
	StopCPUPct       float64         `json:"stop_host_cpu_pct"`
	Profiles         []Profile       `json:"profiles"`
	LogicalOrders    int             `json:"logical_orders"`
	Duplicates       int             `json:"duplicate_submissions"`
	JobDupEvents     int             `json:"duplicate_job_events"`
	Scaling          []ScalingConfig `json:"scaling_configs"`
	PauseSeconds     int             `json:"pause_seconds"`
}

type ScenarioDef struct {
	Name        string     `json:"name"`
	Title       string     `json:"title"`
	Kind        string     `json:"kind"`
	Description string     `json:"description"`
	Destructive bool       `json:"fault_injection"` // needs explicit permission
	Params      Params     `json:"params"`
	Objectives  Objectives `json:"objectives"`
}

// Order is the canonical scenario order of the full suite.
var Order = []string{"smoke", "baseline", "ramp", "burst", "soak", "saturation", "stock-contention", "payment-mix", "idempotency", "pipeline", "worker-scaling", "recovery"}

func LoadScenarios() (map[string]ScenarioDef, error) {
	out := map[string]ScenarioDef{}
	entries, err := fs.ReadDir(scenarioFS, "scenarios")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() == "profiles.json" {
			continue
		}
		b, _ := scenarioFS.ReadFile("scenarios/" + e.Name())
		var d ScenarioDef
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d); err != nil {
			return nil, fmt.Errorf("scenarios/%s: %w", e.Name(), err)
		}
		if d.Name == "" || d.Kind == "" {
			return nil, fmt.Errorf("scenarios/%s: name and kind are required", e.Name())
		}
		out[d.Name] = d
	}
	return out, nil
}

// Profiles maps profile names to scenario lists.
func LoadProfiles() (map[string][]string, error) {
	b, err := scenarioFS.ReadFile("scenarios/profiles.json")
	if err != nil {
		return nil, err
	}
	var p map[string][]string
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return p, nil
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// Options are the bounded user overrides.
type Options struct {
	Scale        float64 // multiplies workflow counts and durations, 0.1-4
	SoakSeconds  int     // 0 = scenario default
	Seed         int64   // 0 = scenario default
	AllowFault   bool
	Race         bool
	ObserveEvery int // ms between Forge job-list polls
}

// Notes collects every safety clamp applied while resolving parameters.
type Notes struct{ Items []string }

func (n *Notes) add(format string, a ...any) { n.Items = append(n.Items, fmt.Sprintf(format, a...)) }

func clampInt(v, lo, hi int, what string, n *Notes) int {
	if v > hi {
		n.add("%s %d exceeds the safety limit %d and was reduced", what, v, hi)
		return hi
	}
	if v < lo {
		return lo
	}
	return v
}

// Resolve applies Options to a definition and clamps everything to the hard limits.
func Resolve(d ScenarioDef, o Options) (ScenarioDef, *Notes) {
	n := &Notes{}
	p := d.Params
	sc := o.Scale
	if sc <= 0 {
		sc = 1
	}
	scaleN := func(v int) int {
		if v == 0 {
			return 0
		}
		return max(1, int(float64(v)*sc+0.5))
	}
	if o.Seed != 0 {
		p.Seed = o.Seed
	}
	p.Workflows = clampInt(scaleN(p.Workflows), 0, HardMaxWorkflows, "workflows", n)
	p.StageWorkflows = clampInt(scaleN(p.StageWorkflows), 0, HardMaxWorkflows, "stage workflows", n)
	p.LogicalOrders = clampInt(scaleN(p.LogicalOrders), 0, 2000, "logical orders", n)
	for _, f := range []*int{&p.StageSeconds, &p.BaseSeconds, &p.BurstSeconds, &p.RecoverySeconds, &p.WindowSeconds, &p.PauseSeconds} {
		*f = clampInt(scaleN(*f), 0, HardMaxSeconds, "seconds", n)
	}
	p.DurationSeconds = scaleN(p.DurationSeconds)
	if o.SoakSeconds > 0 && d.Kind == "soak" {
		p.DurationSeconds = o.SoakSeconds
	}
	p.DurationSeconds = clampInt(p.DurationSeconds, 0, HardMaxSeconds, "soak duration (seconds)", n)
	p.Concurrency = clampInt(p.Concurrency, 0, HardMaxConcurrency, "concurrency", n)
	p.BaseConcurrency = clampInt(p.BaseConcurrency, 0, HardMaxConcurrency, "base concurrency", n)
	p.BurstConcurrency = clampInt(p.BurstConcurrency, 0, HardMaxConcurrency, "burst concurrency", n)
	p.StartConcurrency = clampInt(p.StartConcurrency, 0, HardMaxConcurrency, "start concurrency", n)
	p.MaxConcurrency = clampInt(p.MaxConcurrency, 0, HardMaxConcurrency, "maximum concurrency", n)
	var cs []int
	for _, c := range p.Concurrencies {
		if c > HardMaxConcurrency {
			n.add("ramp step %d skipped: above the safety limit %d", c, HardMaxConcurrency)
			continue
		}
		cs = append(cs, c)
	}
	p.Concurrencies = cs
	d.Params = p
	return d, n
}

// Validate checks a resolved definition.
func (d ScenarioDef) Validate() error {
	p := d.Params
	check := func(m *Mix, what string) error {
		if m == nil {
			return nil
		}
		if m.Success < 0 || m.Decline < 0 || m.Temp < 0 || m.Success+m.Decline+m.Temp != 100 {
			return fmt.Errorf("%s: payment mix must be non-negative and sum to 100", what)
		}
		return nil
	}
	if err := check(p.Mix, d.Name); err != nil {
		return err
	}
	for _, pr := range p.Profiles {
		if err := check(&pr.Mix, d.Name+"/"+pr.Name); err != nil {
			return err
		}
	}
	for _, s := range p.Scaling {
		if s.Replicas < 1 || s.Replicas > 8 || s.Concurrency < 1 || s.Concurrency > 8 {
			return fmt.Errorf("%s: scaling configs need 1-8 replicas and 1-8 concurrency", d.Name)
		}
	}
	return nil
}
