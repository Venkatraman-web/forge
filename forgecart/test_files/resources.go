package main

// Resource and backlog sampling during a run. Process figures come from /proc,
// container figures from `docker stats`, connection counts and the pipeline
// backlog from cheap read-only queries. Sampling is deliberately light (one
// small query per interval) and its cost is reported with the results.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sample is one observation.
type Sample struct {
	T           float64                  `json:"t_s"`
	Paid        int                      `json:"paid_orders"`
	Published   int                      `json:"outbox_published"`
	Events      int                      `json:"outbox_events"`
	Effects     int                      `json:"effects_total"`
	Backlog     int                      `json:"backlog_jobs"`       // jobs per order * paid - effects: jobs not yet completed
	OutboxQueue int                      `json:"outbox_unpublished"` // events not yet accepted by Forge
	ConnsApp    int                      `json:"connections_app_db"`
	ConnsForge  int                      `json:"connections_forge_db"`
	Procs       map[string]ProcStat      `json:"procs,omitempty"`
	Containers  map[string]ContainerStat `json:"containers,omitempty"`
	HostCPUPct  float64                  `json:"host_cpu_pct"`
}

type Sampler struct {
	sb       *Sandbox
	probe    *Probe
	forgeSrv *sql.DB
	interval time.Duration
	start    time.Time

	mu                  sync.Mutex
	samples             []Sample
	queryUS             []int64
	maxConn             int
	cancel              context.CancelFunc
	done                chan struct{}
	dockerWG            sync.WaitGroup
	prevBusy, prevTotal float64
}

func StartSampler(ctx context.Context, sb *Sandbox, probe *Probe, interval time.Duration) *Sampler {
	s := &Sampler{sb: sb, probe: probe, interval: interval, start: time.Now()}
	if u := sb.ForgeDBServerURL(); u != "" {
		if db, err := sql.Open("pgx", u); err == nil {
			db.SetMaxOpenConns(1)
			s.forgeSrv = db
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	s.cancel, s.done = cancel, make(chan struct{})
	go s.loop(cctx)
	return s
}

func (s *Sampler) loop(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	n := 0
	for {
		s.take(ctx, n%5 == 0)
		n++
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Sampler) take(ctx context.Context, withContainers bool) {
	smp := Sample{T: round3(time.Since(s.start).Seconds())}
	smp.HostCPUPct = s.hostCPU()
	q0 := time.Now()
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if pl, err := s.probe.Counts(qctx); err == nil {
		smp.Paid, smp.Published, smp.Events, smp.Effects = pl.Paid, pl.Published, pl.Events, pl.EffectsTotal
		smp.Backlog = jobsPerOrder*pl.Paid - pl.EffectsTotal
		smp.OutboxQueue = pl.Events - pl.Published
	}
	if a, m, err := s.probe.Connections(qctx); err == nil {
		smp.ConnsApp = a
		s.mu.Lock()
		s.maxConn = m
		s.mu.Unlock()
	}
	if s.forgeSrv != nil {
		var c int
		if s.forgeSrv.QueryRowContext(qctx, `SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend'`).Scan(&c) == nil {
			smp.ConnsForge = c
		}
	}
	cancel()
	smp.Procs = s.sb.SampleProcs()
	if withContainers {
		// docker stats takes about a second; run it beside the loop so the interval holds.
		s.dockerWG.Add(1)
		go func(idx float64) {
			defer s.dockerWG.Done()
			cs := s.sb.SampleContainers(ctx)
			s.mu.Lock()
			for i := len(s.samples) - 1; i >= 0; i-- {
				if s.samples[i].T == idx {
					s.samples[i].Containers = cs
					break
				}
			}
			s.mu.Unlock()
		}(smp.T)
	}
	s.mu.Lock()
	s.samples = append(s.samples, smp)
	s.queryUS = append(s.queryUS, time.Since(q0).Microseconds())
	s.mu.Unlock()
}

// ProcSummary aggregates one process over the run.
type ProcSummary struct {
	Name        string  `json:"process"`
	CPUAvgCores float64 `json:"cpu_avg_cores"`
	CPUMaxCores float64 `json:"cpu_max_cores_1s"`
	RSSStartMB  float64 `json:"rss_start_mb"`
	RSSMaxMB    float64 `json:"rss_max_mb"`
	RSSEndMB    float64 `json:"rss_end_mb"`
}

// ResourceSummary is what the report shows.
type ResourceSummary struct {
	Samples           int                `json:"samples"`
	IntervalS         float64            `json:"interval_s"`
	Procs             []ProcSummary      `json:"processes"`
	ContainerCPUAvg   map[string]float64 `json:"container_cpu_avg_pct"`
	ContainerCPUMax   map[string]float64 `json:"container_cpu_max_pct"`
	ContainerMemMaxMB map[string]float64 `json:"container_mem_max_mb"`
	MaxConnsApp       int                `json:"max_connections_app_db"`
	MaxConnsForge     int                `json:"max_connections_forge_db"`
	ServerMaxConns    int                `json:"postgres_max_connections"`
	MaxBacklog        int                `json:"max_backlog_jobs"`
	HostCPUAvgPct     float64            `json:"host_cpu_avg_pct"`
	HostCPUMaxPct     float64            `json:"host_cpu_max_pct"`
	MaxOutboxQueue    int                `json:"max_outbox_unpublished"`
	SamplerQueryMS    *float64           `json:"sampler_mean_cycle_ms"`
	Series            []Sample           `json:"series"` // downsampled, without per-process detail
	HostCPUs          int                `json:"host_cpus"`
}

// Stop ends sampling and summarizes. window limits the backlog/connection maxima
// to samples at or after fromS (use 0 for all).
func (s *Sampler) Stop(hostCPUs int) (ResourceSummary, []Sample) {
	s.cancel()
	<-s.done
	s.dockerWG.Wait()
	if s.forgeSrv != nil {
		s.forgeSrv.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := ResourceSummary{Samples: len(s.samples), IntervalS: s.interval.Seconds(), HostCPUs: hostCPUs, ServerMaxConns: s.maxConn,
		ContainerCPUAvg: map[string]float64{}, ContainerCPUMax: map[string]float64{}, ContainerMemMaxMB: map[string]float64{}}
	type acc struct {
		first, last   ProcStat
		firstT, lastT float64
		maxCPU        float64
		prev          ProcStat
		prevT         float64
		rssMax        float64
		seen          bool
	}
	by := map[string]*acc{}
	hostSum := 0.0
	cnt := map[string]int{}
	sums := map[string]float64{}
	for si, smp := range s.samples {
		for n, st := range smp.Procs {
			a := by[n]
			if a == nil {
				a = &acc{first: st, firstT: smp.T}
				by[n] = a
			} else if smp.T > a.prevT {
				if c := (st.CPUSeconds - a.prev.CPUSeconds) / (smp.T - a.prevT); c > a.maxCPU {
					a.maxCPU = c
				}
			}
			a.prev, a.prevT, a.last, a.lastT = st, smp.T, st, smp.T
			if st.RSSMB > a.rssMax {
				a.rssMax = st.RSSMB
			}
		}
		for n, c := range smp.Containers {
			sums[n] += c.CPUPct
			cnt[n]++
			if c.CPUPct > rs.ContainerCPUMax[n] {
				rs.ContainerCPUMax[n] = c.CPUPct
			}
			if c.MemMB > rs.ContainerMemMaxMB[n] {
				rs.ContainerMemMaxMB[n] = c.MemMB
			}
		}
		// The first sample has no earlier /proc/stat reading to difference against
		// (its value is always 0), so it is left out of the host CPU average and maximum.
		if si > 0 {
			hostSum += smp.HostCPUPct
			if smp.HostCPUPct > rs.HostCPUMaxPct {
				rs.HostCPUMaxPct = smp.HostCPUPct
			}
		}
		if smp.ConnsApp > rs.MaxConnsApp {
			rs.MaxConnsApp = smp.ConnsApp
		}
		if smp.ConnsForge > rs.MaxConnsForge {
			rs.MaxConnsForge = smp.ConnsForge
		}
		if smp.Backlog > rs.MaxBacklog {
			rs.MaxBacklog = smp.Backlog
		}
		if smp.OutboxQueue > rs.MaxOutboxQueue {
			rs.MaxOutboxQueue = smp.OutboxQueue
		}
	}
	if len(s.samples) > 1 {
		rs.HostCPUAvgPct = round3(hostSum / float64(len(s.samples)-1))
	}
	for n, c := range cnt {
		rs.ContainerCPUAvg[n] = round3(sums[n] / float64(c))
	}
	names := make([]string, 0, len(by))
	for n := range by {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		a := by[n]
		ps := ProcSummary{Name: n, RSSStartMB: round3(a.first.RSSMB), RSSMaxMB: round3(a.rssMax), RSSEndMB: round3(a.last.RSSMB), CPUMaxCores: round3(a.maxCPU)}
		if d := a.lastT - a.firstT; d > 0 {
			ps.CPUAvgCores = round3((a.last.CPUSeconds - a.first.CPUSeconds) / d)
		}
		rs.Procs = append(rs.Procs, ps)
	}
	if len(s.queryUS) > 0 {
		var t int64
		for _, v := range s.queryUS {
			t += v
		}
		rs.SamplerQueryMS = r3(float64(t) / float64(len(s.queryUS)) / 1000)
	}
	full := append([]Sample(nil), s.samples...)
	// downsample the stored series to at most 600 points, dropping per-process detail
	step := 1
	if len(full) > 600 {
		step = (len(full) + 599) / 600
	}
	for i := 0; i < len(full); i += step {
		p := full[i]
		p.Procs = nil
		rs.Series = append(rs.Series, p)
	}
	return rs, full
}

// hostCPU returns whole-host CPU use in percent since the previous call.
func (s *Sampler) hostCPU() float64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	f := strings.Fields(line)
	if len(f) < 8 || f[0] != "cpu" {
		return 0
	}
	var vals [10]float64
	var total float64
	for i := 1; i < len(f) && i <= 10; i++ {
		fmt.Sscan(f[i], &vals[i-1])
		total += vals[i-1]
	}
	idle := vals[3] + vals[4]
	busy := total - idle
	pct := 0.0
	if s.prevTotal > 0 && total > s.prevTotal {
		pct = (busy - s.prevBusy) / (total - s.prevTotal) * 100
	}
	s.prevBusy, s.prevTotal = busy, total
	return round3(pct)
}
