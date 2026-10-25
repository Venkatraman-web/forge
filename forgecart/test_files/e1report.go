package main

// The E-1 report is generated from the raw E1Result only. It states measured
// values and the definitions behind them; it draws no conclusion about the
// bottleneck (that interpretation is written separately, with hypotheses
// labelled as such).

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func f1(v float64) string { return fmt.Sprintf("%.1f", v) }
func f0(v float64) string { return fmt.Sprintf("%.0f", v) }

func distCell(d E1Dist) string {
	if d.N == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f / %.1f / %.1f", d.P50, d.P95, d.P99)
}

func RenderE1(r *E1Result) string {
	m := &md{}
	title := "E-1 Forge-isolated capacity baseline"
	if !r.Complete {
		title += " (INCOMPLETE)"
	}
	m.f("# %s — run `%s`\n\n", title, r.RunID)
	m.f("Started %s · duration %s · raw data: `results/%s/e1.json`\n\n", r.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"), r.EndedAt.Sub(r.StartedAt).Round(time.Second), r.RunID)
	if !r.Complete {
		m.f("> **This run is incomplete.** %s\n\n", orStr(r.StopReason, "It did not finish."))
	}
	m.ln("Generated from the raw result. This report states what was measured and how; the interpretation (likely bottleneck, hypotheses) is kept separate from it.\n")

	c := r.Config
	m.ln("## 1. What this experiment is\n")
	m.ln("Jobs are submitted **directly to Forge's `POST /v1/jobs`** by an **open-loop** generator (a fixed arrival rate: request *i* is scheduled at *i / rate* seconds after the stage start, whatever the service does) and executed by Forge SDK workers running a **synthetic handler**. There is no ForgeCart process, database, outbox or dispatcher in the run. Queue state is read through the authenticated operator endpoint (`GET /v1/queues`); latencies come from the job rows' own timestamps; final state is reconciled against the database. Everything runs on **one host**, so this isolates Forge from ForgeCart's business pipeline. It is **not** a multi-host or production capacity test.\n")
	m.ln("**Metric definitions.**\n")
	m.ln("- *Offered rate*: the scheduled arrival rate. *Submission throughput (accepted/s)*: `202` responses ÷ the send window (stage start to the last response). Not the same as completion or drain throughput.")
	m.ln("- *Submit latency*: time from sending the request to receiving the response (service time), for `202` responses. *Send lateness*: how far behind its schedule the generator actually sent; large lateness means the generator, not Forge, limited the offered load.")
	m.ln("- *Completion throughput during load*: jobs whose `finished_at` is at or before the end of the load ÷ load duration (authoritative job timestamps).")
	m.ln("- *Drain throughput*: jobs still unfinished at the end of the load ÷ the time from the end of the load to the last `finished_at`. It describes the system working off a backlog and is reported separately.")
	m.ln("- *Queue wait* = `started_at − created_at` (first start); *execution* = `finished_at − started_at`; *end-to-end* = `finished_at − created_at`. \"Last half\" means jobs created in the second half of the load, to leave out start-up effects.")
	m.ln("- *Queue depth / oldest due-job age*: the operator endpoint's `totals.queued` and `totals.oldest_due_job_age_seconds`, polled once per second.\n")
	m.ln("**Assumptions made where the design (docs/decisions/0004 section 8.2) leaves details open.** The design fixes only: direct open-loop submission, SDK workers, synthetic handlers of configurable duration. Everything below is therefore this experiment's own choice and is part of the result:\n")
	t := Table{Head: []string{"Setting", "Value", "Why / note"}, Rows: [][]string{
		{"Arrival rates (sweep)", fmt.Sprint(c.Rates) + " jobs/s", "geometric steps spanning the requirements' option A (25), B (100) and C (≥250) toward saturation"},
		{"Stage length", fmt.Sprintf("%d s", c.StageSeconds), "long enough for a steady half; short of the 10-minute G-P soak"},
		{"Sustained stage", fmt.Sprintf("%.0f%% of the highest rate classified sustainable, %d s", c.SustainedFrac*100, c.SustainedSeconds), "rule fixed before the run; chosen rate: " + fmt.Sprint(r.SustainRate)},
		{"Handler", fmt.Sprintf("%d ms fixed wait, no I/O", c.HandlerMS), "matches ForgeCart's measured ~25 ms median; D-1 (the real workload) is undecided"},
		{"Payload", fmt.Sprintf("~%d bytes of input", c.PayloadBytes), "small JSON; sizes and type mixes are configurable but one size was run"},
		{"Job types", strings.Join(c.Types, ", ") + " (alternating)", "two types exercise the type filter"},
		{"Workers", fmt.Sprintf("%d processes × %d slots = %d slots", c.WorkerProcs, c.SlotsPerProc, c.WorkerProcs*c.SlotsPerProc), "below Forge's default poll cap of 64; slot ceiling ≈ " + f0(float64(c.WorkerProcs*c.SlotsPerProc)*1000/float64(max(c.HandlerMS, 1))) + " jobs/s at this handler time"},
		{"Queue cap", fmt.Sprintf("FORGE_MAX_QUEUED=%d (default 1000)", c.MaxQueued), "raised so saturation shows as queue growth rather than rejection; not the default"},
		{"Fresh state per stage", "yes", "database dropped, migrated and all processes restarted for every stage"},
		{"Operator polling", fmt.Sprintf("every %d ms", c.OperatorPollMS), "its latency is reported per stage"},
		{"Generator bounds", fmt.Sprintf("≤ %d requests in flight; lateness threshold %d ms", c.MaxInFlight, c.LateThresholdMS), "a send that cannot start on time is sent late and counted, never dropped"},
	}}
	m.table(t)
	m.ln("**Stage classification (written before the run).** A stage is *sustainable* if (1) every offered request was accepted with no error, (2) the generator stayed on schedule (≤1% of sends later than the threshold, in-flight bound never reached), (3) the operator-observed backlog grew by at most 2% of the arrival rate per second over the second half, and (4) p95 queue wait of second-half jobs was at most 5 s. These thresholds are this experiment's own assumptions, used only to choose the sustained rate; they are not service-level objectives.\n")

	m.ln("## 2. Environment\n")
	e := r.Env
	h := e.Host
	et := Table{Head: []string{"Item", "Value"}, Rows: [][]string{
		{"Host", fmt.Sprintf("%s, %d logical CPUs, %d MB, kernel %s", orStr(e.CPUModel, "unknown"), e.CPUs, e.MemoryMB, e.Kernel)},
		{"Host boot / uptime at start / load average", h.BootTime + " / " + h.UptimeAtStart + " / " + h.LoadAvgAtStart},
		{"CPU governor", h.CPUGovernor},
		{"Container engine", fmt.Sprintf("%s; VM CPUs %s, memory %s bytes; %s", h.DockerEngine, h.DockerVMCPUs, h.DockerVMMemory, h.DockerVMProcess)},
		{"PostgreSQL", fmt.Sprintf("%s in one container, limited to %s CPUs and %s memory, **%s**, max_connections 400", e.PostgresVer, r.Sandbox.PostgresCPUs, r.Sandbox.PostgresMemory, r.Sandbox.Storage)},
		{"Forge", r.Sandbox.ForgeSettings},
		{"Source tree fingerprint", fmt.Sprintf("%s (%d files)", h.SourceTreeHash, h.SourceFiles)},
		{"ForgeCart components present", fmt.Sprint(r.Sandbox.ForgeCartIn)},
		{"Operator API", "loopback listener, bearer key generated for this run (not recorded)"},
	}}
	for _, k := range sortedKeys(e.Versions) {
		et.Rows = append(et.Rows, []string{k, e.Versions[k]})
	}
	m.table(et)
	if len(e.Binaries) > 0 {
		bt := Table{Title: "Measured executables", Head: []string{"Binary", "sha256", "Go build info"}}
		for _, b := range e.Binaries {
			bt.Rows = append(bt.Rows, []string{b.Name, b.SHA256, b.BuildInfo})
		}
		m.table(bt)
	}
	m.ln("Generator, Forge, workers and PostgreSQL share one host and one container VM, so they compete for CPU; the PostgreSQL volume is RAM-backed, so disk I/O cost is **not** represented.\n")

	m.ln("## 3. Results by stage\n")
	st := Table{Head: []string{"Stage", "Offered /s", "Accepted /s", "Rejected (queue_full)", "Other errors", "Submit p50 / p95 / p99 (ms)", "Completed /s during load", "Queue wait p50 / p95 / p99, last half (ms)", "End-to-end p95, last half (ms)", "Queue depth at load end (peak)", "Oldest due age peak (s)", "Backlog slope, 2nd half (jobs/s)", "Drain (s)", "Drain throughput (jobs/s)", "Sustainable?"}}
	for _, s := range r.Stages {
		other := 0
		for _, v := range s.Submit.Other {
			other += v
		}
		st.Rows = append(st.Rows, []string{s.Label, f0(s.RateJobsPerS), f1(s.Submit.AcceptRatePerS), fmt.Sprint(s.Submit.QueueFull), fmt.Sprint(other),
			distCell(s.Submit.Latency), f1(s.CompletionRate), distCell(s.DB.QueueWaitLast), f1(s.DB.EndToEndLast.P95),
			fmt.Sprintf("%d (%d)", s.QueuedAtLoadEnd, s.QueuedPeak), f1(s.OldestAgePeak), f1(s.BacklogSlopePerS), f1(s.DrainSeconds), drainCell(s), yn(s.Sustainable)})
	}
	m.table(st)

	for _, s := range r.Stages {
		m.f("### %s — %s jobs/s for %d s\n\n", s.Label, f0(s.RateJobsPerS), s.Seconds)
		sm := s.Submit
		dt := Table{Head: []string{"Measure", "Value"}, Rows: [][]string{
			{"Requests planned / sent / accepted", fmt.Sprintf("%d / %d / %d", sm.Offered, sm.Sent, sm.Accepted)},
			{"Rejected `queue_full` (with Retry-After) / other outcomes", fmt.Sprintf("%d (%d) / %s", sm.QueueFull, sm.QueueFullHint, otherCell(sm.Other))},
			{"Send window", fmt.Sprintf("%.3f s", sm.WindowS)},
			{"Submit latency, accepted: p50 / p95 / p99 / max (ms)", fmt.Sprintf("%.1f / %.1f / %.1f / %.1f", sm.Latency.P50, sm.Latency.P95, sm.Latency.P99, sm.Latency.Max)},
			{"Submit latency, second half: p50 / p95 / p99 (ms)", distCell(sm.LatencyLastHalf)},
			{"Generator send lateness: p99 / max (ms); sends over threshold; in-flight cap hits", fmt.Sprintf("%.2f / %.2f; %d; %d", sm.LateP99MS, sm.LateMaxMS, sm.LateOverCount, sm.InFlightCap)},
			{"Jobs in the database: succeeded / failed / queued / running", fmt.Sprintf("%d / %d / %d / %d", s.DB.ByStatus["succeeded"], s.DB.ByStatus["failed"], s.DB.ByStatus["queued"], s.DB.ByStatus["running"])},
			{"Retried (attempt > 1) / max attempt / drain timed out", fmt.Sprintf("%d / %d / %v", s.DB.RetriedAttempts, s.DB.MaxAttempt, s.DrainTimedOut)},
			{"Finished by load end / unfinished at load end", fmt.Sprintf("%d / %d", s.DB.CompletedByEnd, s.RemainingAtEnd)},
			{"Queue wait, all jobs: p50 / p95 / p99 / max (ms)", fmt.Sprintf("%.1f / %.1f / %.1f / %.1f", s.DB.QueueWait.P50, s.DB.QueueWait.P95, s.DB.QueueWait.P99, s.DB.QueueWait.Max)},
			{"Execution (finished − started), all jobs: p50 / p95 / p99 / max (ms)", fmt.Sprintf("%.1f / %.1f / %.1f / %.1f", s.DB.Exec.P50, s.DB.Exec.P95, s.DB.Exec.P99, s.DB.Exec.Max)},
			{"End-to-end, all jobs: p50 / p95 / p99 / max (ms)", fmt.Sprintf("%.1f / %.1f / %.1f / %.1f", s.DB.EndToEnd.P50, s.DB.EndToEnd.P95, s.DB.EndToEnd.P99, s.DB.EndToEnd.Max)},
			{"`jobs` table size at the end", fmt.Sprintf("%.1f MB", float64(s.DB.TableBytes)/(1<<20))},
		}}
		m.table(dt)
		rs := s.Res
		rt := Table{Title: "Resource use during the load (1 s samples; PostgreSQL from `docker stats`, about every 5 s)", Head: []string{"Component", "Average", "Max (1 s)", "Memory max"}, Rows: [][]string{
			{"Host CPU", f1(rs.HostCPUAvg) + " %", f1(rs.HostCPUMax) + " %", "—"},
			{"Forge process", "", fmt.Sprintf("%.2f cores", rs.ForgeCPUMax), fmt.Sprintf("%.0f MB", rs.ForgeRSSMax)},
			{"Workers (all processes)", fmt.Sprintf("%.2f cores", rs.WorkersCPUAvg), fmt.Sprintf("%.2f cores", rs.WorkersCPUMax), fmt.Sprintf("%.0f MB", rs.WorkersRSSMax)},
			{"Load generator (this process)", fmt.Sprintf("%.2f cores", rs.GenCPUAvg), fmt.Sprintf("%.2f cores", rs.GenCPUMax), fmt.Sprintf("%.0f MB", rs.GenRSSMax)},
			{"PostgreSQL container", fmt.Sprintf("%.0f %% of one core (%d readings)", rs.PGCPUAvg, rs.PGCPUReadings), fmt.Sprintf("%.0f %%", rs.PGCPUMax), fmt.Sprintf("%.0f MB", rs.PGMemMaxMB)},
			{"PostgreSQL client connections (total / active), max", fmt.Sprintf("%d / %d", rs.PGConnMax, rs.PGActiveMax), "—", "—"},
			{"Operator endpoint polls: latency p50 / p95 / max (ms); errors", fmt.Sprintf("%.1f / %.1f / %.1f (n=%d)", rs.OperatorPollP50, rs.OperatorPollP95, rs.OperatorPollMax, rs.OperatorPolls), fmt.Sprint(rs.OperatorPollErrors), "—"},
		}}
		rt.Rows[1][1] = fmt.Sprintf("%.2f cores", rs.ForgeCPUAvg)
		m.table(rt)
		if s.Aborted != "" {
			m.f("**Stopped early by the watchdog:** %s\n\n", s.Aborted)
		}
		m.table(winTable(s))
		m.table(logTable("Forge log lines (ids and numbers normalised; ten most frequent shapes)", s.ForgeLog))
		m.table(logTable("Worker log lines (all worker processes)", s.WorkerLog))
		m.f("**Classification:** %s\n\n", yn(s.Sustainable))
		for _, w := range s.SustainWhy {
			m.f("- %s\n", w)
		}
		m.ln("")
		ct := Table{Title: "Integrity and isolation checks", Head: []string{"Check", "Result", "Detail"}}
		for _, ck := range s.Checks {
			ct.Rows = append(ct.Rows, []string{ck.Name, map[bool]string{true: "PASS", false: "FAIL"}[ck.OK], ck.Detail})
		}
		m.table(ct)
		if s.Error != "" {
			m.f("**Stage error:** %s\n\n", s.Error)
		}
		if s.Kind == "sustained" {
			m.table(sustainedWindows(s))
		}
	}
	if r.Config.Diagnostics {
		m.ln("## 4. PostgreSQL diagnostics (opt-in telemetry)\n")
		m.ln("Read from PostgreSQL itself once per second. *Entries scanned per index scan* is the change in `idx_tup_read` divided by the change in `idx_scan` for `jobs_due_idx` over the window: it is how many index entries the queue's claim/count scans walk per scan. *Waits* lists client backends by `wait_event_type/wait_event/state`.\n")
		for _, s := range r.Stages {
			m.table(diagTable(s))
		}
	}
	m.ln("## 5. Limits of this experiment\n")
	for _, l := range []string{
		"One host: the generator, Forge, two worker processes and PostgreSQL (in a container VM) share 12 logical CPUs. Component CPU figures show who used what, not what each would use alone.",
		"PostgreSQL storage is RAM-backed (tmpfs); disk, WAL and checkpoint costs are not represented. Requirement B-6 asks for disk-backed storage for the performance gate.",
		"One handler duration, one payload size, one type pair and one worker shape were run. Results do not transfer to CPU-bound, long, or large-payload jobs.",
		"Each stage ran once. Sessions on this host have varied by tens of percent for identical code (see the audits), so no difference between stages smaller than that variation should be read as real without interleaved repetition.",
		"`FORGE_MAX_QUEUED` was raised to " + fmt.Sprint(c.MaxQueued) + ". The default of 1000 would have turned saturation into rejections (the contract validated separately); admission cost at depth is entangled with these results (experiment E-3 isolates it).",
		"Docker stats gives few readings per stage; PostgreSQL CPU is indicative.",
		"Results are single-host measurements, not a production capacity statement.",
	} {
		m.f("- %s\n", l)
	}
	for _, n := range r.Notes {
		m.f("- %s\n", n)
	}
	m.ln("")
	return m.b.String()
}

// sustainedWindows summarises the long stage in 60 s windows from the raw per-second data.
func sustainedWindows(s *E1Stage) Table {
	t := Table{Title: "Sustained stage by 60 s window", Head: []string{"Window (s)", "Accepted /s", "Completed /s", "Operator queue depth (mean)", "Oldest due age (max, s)"}}
	for w := 0; w*60 < s.Seconds; w++ {
		from, to := w*60, min((w+1)*60, s.Seconds)
		acc, comp := 0, 0
		for i := from; i < to && i < len(s.Submit.AcceptedPerS); i++ {
			acc += s.Submit.AcceptedPerS[i]
		}
		for i := from; i < to && i < len(s.DB.CompletedBySec); i++ {
			comp += s.DB.CompletedBySec[i]
		}
		qsum, qn, age := 0.0, 0, 0.0
		for _, p := range s.Polls {
			// poll times are relative to the stage's monitoring start, which precedes the load by the settle time
			if p.Status == 200 && p.T >= s.LoadStartOffsetS+float64(from) && p.T < s.LoadStartOffsetS+float64(to) {
				qsum += float64(p.Queued)
				qn++
				if p.OldestDueS != nil {
					age = max(age, *p.OldestDueS)
				}
			}
		}
		mean := "—"
		if qn > 0 {
			mean = f1(qsum / float64(qn))
		}
		d := float64(to - from)
		t.Rows = append(t.Rows, []string{fmt.Sprintf("%d–%d", from, to), f1(float64(acc) / d), f1(float64(comp) / d), mean, f1(age)})
	}
	return t
}

func drainCell(s *E1Stage) string {
	if s.DrainRatePerS == 0 {
		return "n/a (backlog worked off in under 1 s)"
	}
	return f1(s.DrainRatePerS)
}

func otherCell(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	return fmt.Sprint(m)
}

// diagTable summarises the diagnostics samples of a stage in 10 s windows.
func diagTable(s *E1Stage) Table {
	t := Table{Title: s.Label + ": PostgreSQL telemetry by 10 s window (seconds from the start of the load)", Head: []string{"Window", "Accepted /s", "Completed /s", "Top waits (client backends, mean count)", "Locks not granted (max) / advisory", "Oldest transaction (max s)", "Dead tuples (end) / autovacuum runs (end)", "Index entries scanned per index scan", "Heap fetches per index scan"}}
	total := len(s.DB.CompletedBySec)
	end := max(s.Seconds, total)
	for w := 0; w*10 < end; w++ {
		from, to := w*10, (w+1)*10
		acc, comp := 0, 0
		for i := from; i < to && i < len(s.Submit.AcceptedPerS); i++ {
			acc += s.Submit.AcceptedPerS[i]
		}
		for i := from; i < to && i < len(s.DB.CompletedBySec); i++ {
			comp += s.DB.CompletedBySec[i]
		}
		waits := map[string]float64{}
		n := 0
		lockMax, advMax, xactMax := 0, 0, 0.0
		var first, last *E1Diag
		var dead, av int64
		for i := range s.Samples {
			x := &s.Samples[i]
			rel := x.T - s.LoadStartOffsetS
			if x.Diag == nil || rel < float64(from) || rel >= float64(to) {
				continue
			}
			n++
			for k, v := range x.Diag.Waits {
				waits[k] += float64(v)
			}
			lockMax, advMax = max(lockMax, x.Diag.LocksNotGranted), max(advMax, x.Diag.AdvisoryWaiting)
			xactMax = max(xactMax, x.Diag.OldestXactAgeS)
			if first == nil {
				first = x.Diag
			}
			last = x.Diag
			dead, av = x.Diag.DeadTup, x.Diag.AutovacuumCount
		}
		if n == 0 {
			continue
		}
		type kv struct {
			k string
			v float64
		}
		var ws []kv
		for k, v := range waits {
			ws = append(ws, kv{k, v / float64(n)})
		}
		sort.Slice(ws, func(i, j int) bool { return ws[i].v > ws[j].v })
		var parts []string
		for i, x := range ws {
			if i >= 4 {
				break
			}
			parts = append(parts, fmt.Sprintf("%s=%.1f", x.k, x.v))
		}
		scan, fetch := "—", "—"
		if first != nil && last != nil && last.DueIdxScans > first.DueIdxScans {
			ds := float64(last.DueIdxScans - first.DueIdxScans)
			scan = f1(float64(last.DueIdxTupRead-first.DueIdxTupRead) / ds)
			fetch = f1(float64(last.DueIdxTupFetch-first.DueIdxTupFetch) / ds)
		}
		t.Rows = append(t.Rows, []string{fmt.Sprintf("%d–%d", from, to), f1(float64(acc) / 10), f1(float64(comp) / 10), strings.Join(parts, "; "),
			fmt.Sprintf("%d / %d", lockMax, advMax), f1(xactMax), fmt.Sprintf("%d / %d", dead, av), scan, fetch})
	}
	return t
}

// winTable shows submit latency in 30 s slices of the load, so it can be read against queue depth.
func winTable(s *E1Stage) Table {
	t := Table{Title: "Submission by 30 s window (with the operator-reported queue depth at the window's end)", Head: []string{"From (s)", "Sent", "Accepted", "Accepted latency p50 / p99 (ms)", "Queue depth at window end"}}
	for _, w := range s.Submit.Windows {
		depth := "n/a"
		end := s.LoadStartOffsetS + float64(w.FromS+30)
		for _, p := range s.Polls {
			if p.Status == 200 && p.T <= end {
				depth = fmt.Sprint(p.Queued)
			}
		}
		t.Rows = append(t.Rows, []string{fmt.Sprint(w.FromS), fmt.Sprint(w.Sent), fmt.Sprint(w.Accepted), fmt.Sprintf("%.1f / %.1f", w.P50MS, w.P99MS), depth})
	}
	return t
}

func logTable(title string, m map[string]int) Table {
	t := Table{Title: title, Head: []string{"Count", "Line shape"}}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	for _, k := range keys {
		t.Rows = append(t.Rows, []string{fmt.Sprint(m[k]), "`" + k + "`"})
	}
	if len(t.Rows) == 0 {
		t.Rows = [][]string{{"0", "(none)"}}
	}
	return t
}
