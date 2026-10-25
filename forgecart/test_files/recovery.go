package main

// Scenario L: controlled failures, only inside the suite's own sandbox. The faults
// act on processes the suite started and on the sandbox's own database container
// (ownership is verified before every container operation).

import (
	"context"
	"fmt"
	"time"
)

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func runRecovery(e *Env, d ScenarioDef, res *ScenarioResult) {
	if !e.Opts.AllowFault {
		res.Status = "skipped"
		res.Reasons = []string{"fault injection is not enabled: pass --allow-fault-injection (the 'full' and 'all' profiles and an explicit 'recovery' run on run.sh enable it) so processes and containers of the sandbox may be stopped on purpose"}
		return
	}
	p := d.Params
	c := &CritSet{}
	base := e.cfgFor(p)
	// short leases so a hard-killed worker's jobs are recovered within seconds
	base.Lease, base.Heartbeat, base.SafetyMargin, base.ReaperInterval = "6s", "2s", "4s", "1s"
	res.Notes = append(res.Notes, "Forge lease settings for this scenario: lease 6 s, heartbeat 2 s, safety margin 4 s, reaper every 1 s (defaults are 15/5/10/5) so that recovery after a hard kill is observable within the drain timeout.")
	pause := time.Duration(p.PauseSeconds) * time.Second
	tb := Table{Title: "Recovery tests", Head: []string{"Test", "Injected fault", "Observation at the fault", "Drain after recovery", "Worker retry starts", "Released on shutdown", "Verdict"}}
	phases := []Phase{{Name: "recovery", Concurrency: p.Concurrency, Workflows: p.Workflows}}

	type test struct {
		id, fault string
		hook      func(ctx context.Context, e *Env, t0 time.Time)
		before    func(ctx context.Context, e *Env, x map[string]any)
		relaxHTTP bool
		observe   func(c *CritSet, id string, x map[string]any, r *RunRecord)
	}
	tests := []test{
		{id: "stop-one-worker-type", fault: "fulfillment worker stopped (SIGTERM) at load start, restarted after the load",
			hook: func(ctx context.Context, e *Env, t0 time.Time) { e.SB.StopKind("worker", "fulfillment", true) },
			before: func(ctx context.Context, e *Env, x map[string]any) {
				pl, _ := e.Probe.Counts(ctx)
				x["paid_orders"], x["fulfillment_effects_before_restart"] = pl.Paid, pl.Effects["fulfillment"]
				e.SB.StartKind(ctx, "worker", "fulfillment")
			},
			observe: func(c *CritSet, id string, x map[string]any, r *RunRecord) {
				paid, ful := x["paid_orders"].(int), x["fulfillment_effects_before_restart"].(int)
				c.Correct(id+"-effect", "the fault took effect: fulfillment jobs queued up while their worker was down", ful < paid, fmt.Sprintf("%d fulfillment effects for %d paid orders when the worker was restarted", ful, paid))
			}},
		{id: "pause-all-workers", fault: "all workers stopped (SIGTERM) at load start, held for the pause, then restarted",
			hook: func(ctx context.Context, e *Env, t0 time.Time) { e.SB.StopKind("worker", "", true) },
			before: func(ctx context.Context, e *Env, x map[string]any) {
				sleepCtx(ctx, pause)
				pl, _ := e.Probe.Counts(ctx)
				x["paid_orders"], x["effects_at_resume"], x["backlog_at_resume"] = pl.Paid, pl.EffectsTotal, jobsPerOrder*pl.Paid-pl.EffectsTotal
				e.SB.StartKind(ctx, "worker", "")
			},
			observe: func(c *CritSet, id string, x map[string]any, r *RunRecord) {
				paid, eff := x["paid_orders"].(int), x["effects_at_resume"].(int)
				c.Correct(id+"-effect", "a backlog built up while processing was paused (at most 10% of the work done at resume)", eff*10 <= jobsPerOrder*paid, fmt.Sprintf("%d of %d jobs done when workers resumed", eff, 5*paid))
			}},
		{id: "restart-dispatcher", fault: "dispatcher stopped (SIGTERM) 1 s into the load, restarted after the load",
			hook: func(ctx context.Context, e *Env, t0 time.Time) {
				if sleepCtx(ctx, time.Second) {
					e.SB.StopKind("dispatcher", "", true)
				}
			},
			before: func(ctx context.Context, e *Env, x map[string]any) {
				pl, _ := e.Probe.Counts(ctx)
				x["outbox_events"], x["outbox_unpublished_at_restart"] = pl.Events, pl.Events-pl.Published
				sleepCtx(ctx, pause/2)
				e.SB.StartKind(ctx, "dispatcher", "")
			},
			observe: func(c *CritSet, id string, x map[string]any, r *RunRecord) {
				un := x["outbox_unpublished_at_restart"].(int)
				c.Correct(id+"-effect", "durable outbox events existed unpublished while the dispatcher was down, and survived the restart", un > 0, fmt.Sprintf("%d unpublished events at restart", un))
			}},
		{id: "graceful-worker-shutdown", fault: "all workers sent SIGTERM 1.5 s into the load (in-flight jobs released), restarted 1 s later",
			hook: func(ctx context.Context, e *Env, t0 time.Time) {
				if sleepCtx(ctx, 1500*time.Millisecond) {
					e.SB.StopKind("worker", "", true)
					// The restart must happen even if the load has already ended
					// (ctx cancelled), otherwise the drain would wait for dead workers.
					sleepCtx(context.Background(), time.Second)
					e.SB.StartKind(context.Background(), "worker", "")
				}
			}},
		{id: "hard-kill-workers", fault: "all workers killed (SIGKILL) 1.5 s into the load, restarted 2 s later; Forge's reaper must recover their leases",
			hook: func(ctx context.Context, e *Env, t0 time.Time) {
				if sleepCtx(ctx, 1500*time.Millisecond) {
					e.SB.StopKind("worker", "", false)
					sleepCtx(context.Background(), 2*time.Second)
					e.SB.StartKind(context.Background(), "worker", "")
				}
			}},
		{id: "app-db-pause", fault: "ForgeCart database container paused (docker pause) for 3 s, 1.5 s into the load",
			relaxHTTP: true,
			hook: func(ctx context.Context, e *Env, t0 time.Time) {
				if sleepCtx(ctx, 1500*time.Millisecond) {
					if err := e.SB.PauseAppDB(ctx); err == nil {
						sleepCtx(context.Background(), 3*time.Second)
						e.SB.UnpauseAppDB(context.Background())
					}
				}
			}},
	}
	for i, t := range tests {
		if e.Ctx.Err() != nil {
			break
		}
		x := map[string]any{}
		ts := TrialSpec{Label: t.id, Cfg: base, Seed: p.Seed + int64(i), Tag: fmt.Sprintf("rc%d", i), Drain: true, DrainTimeout: drainTimeout(p), Phases: phases, Hook: t.hook}
		if t.before != nil {
			tt := t
			ts.BeforeDrain = func(ctx context.Context, e *Env) { tt.before(ctx, e, x) }
		}
		r := e.Trial(ts)
		// make sure nothing is left paused or stopped for the next trial
		e.SB.UnpauseAppDB(context.Background())
		if trialErr(res, c, r) {
			tb.Rows = append(tb.Rows, []string{t.id, t.fault, "-", "-", "-", "-", "ERROR: " + r.Error})
			continue
		}
		r.Extra = x
		before := len(c.list)
		pipelineCriteria(c, t.id+"-", r, true)
		if t.relaxHTTP {
			kept := c.list[:before]
			for _, cr := range c.list[before:] {
				if cr.ID == t.id+"-http" || cr.ID == t.id+"-paid" {
					continue
				}
				kept = append(kept, cr)
			}
			c.list = kept
			c.Correct(t.id+"-http-note", "workflows hit by the outage either completed or failed visibly; the outcome of those requests is reported, not asserted (a payment may commit while its response is lost)", true,
				fmt.Sprintf("completed %d, failed %d, interrupted %d, HTTP failure rate %s", r.Load.Completed, r.Load.Failed, r.Load.Interrupted, pctStr(r.Load.ErrorRatePct)))
			c.Correct(t.id+"-paid", "paid orders in the database are at least the successful payment responses (a committed payment may lose its response)", r.Pipeline.Paid >= r.Load.Payment["succeeded"], fmt.Sprintf("database %d, responses %d", r.Pipeline.Paid, r.Load.Payment["succeeded"]))
		}
		if t.observe != nil {
			t.observe(c, t.id, x, r)
		}
		obs := "-"
		if len(x) > 0 {
			obs = fmt.Sprint(x)
		}
		released := 0
		for _, w := range r.Workers {
			released += w.Released
		}
		verdict := "recovered"
		for _, cr := range c.list[before:] {
			if cr.Status == "fail" {
				verdict = "FAILED: " + cr.Description
				break
			}
		}
		tb.Rows = append(tb.Rows, []string{t.id, t.fault, obs, drainShort(r.Drain), itoa(totalRetries(r)), itoa(released), verdict})
	}
	res.Tables = append(res.Tables, tb)
	finishWith(res, c, e)
}
