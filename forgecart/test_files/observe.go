package main

// Observation of Forge and the background pipeline.
//  - ForgeObserver polls the PUBLIC Forge job API (GET /v1/jobs, newest 100) and
//    records the timestamps Forge itself reports.
//  - ParseWorkerLogs reads the worker processes' own logs for attempts and retries
//    (the public API does not expose attempt counts).
//  - Probe reads ForgeCart's application database, read-only, for the pipeline
//    counters and the final integrity checks. It never touches Forge's tables.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// ---------- Forge job observer (public HTTP API only) ----------

type jobObs struct {
	Type      string
	Status    string
	Created   time.Time
	Started   *time.Time
	Finished  *time.Time
	FirstSeen time.Time
}

type ForgeObserver struct {
	base   string
	apiKey string // Forge's submit key
	// SkipPreexisting ignores the jobs already listed by the first successful poll.
	// Attach mode needs it: Forge's job table is not reset between trials there,
	// so the previous trial's jobs would otherwise be counted in this one.
	SkipPreexisting bool
	ignore          map[string]bool
	firstDone       bool
	interval        time.Duration
	hc              *http.Client

	mu          sync.Mutex
	jobs        map[string]*jobObs
	polls       int
	pollErrors  int
	fullWindows int // polls in which all 100 returned jobs were new: jobs may have been missed
	pollLatUS   []int64
	capped      bool

	cancel context.CancelFunc
	done   chan struct{}
}

const maxObservedJobs = 300_000

func NewForgeObserver(base, apiKey string, interval time.Duration) *ForgeObserver {
	return &ForgeObserver{base: base, apiKey: apiKey, interval: interval, hc: &http.Client{Timeout: 5 * time.Second}, jobs: map[string]*jobObs{}}
}

type apiJob struct {
	ID         string     `json:"job_id"`
	Type       string     `json:"type"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

func (o *ForgeObserver) poll(ctx context.Context) {
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/v1/jobs?limit=100", nil)
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	resp, err := o.hc.Do(req)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.polls++
	if err != nil {
		o.pollErrors++
		return
	}
	defer resp.Body.Close()
	var out struct {
		Jobs []apiJob `json:"jobs"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&out) != nil {
		o.pollErrors++
		return
	}
	if len(o.pollLatUS) < 100_000 {
		o.pollLatUS = append(o.pollLatUS, time.Since(start).Microseconds())
	}
	now := time.Now()
	fresh := 0
	if o.SkipPreexisting && !o.firstDone {
		o.ignore = map[string]bool{}
		for _, j := range out.Jobs {
			o.ignore[j.ID] = true
		}
	}
	o.firstDone = true
	for _, j := range out.Jobs {
		if o.ignore[j.ID] {
			continue
		}
		cur := o.jobs[j.ID]
		if cur == nil {
			if len(o.jobs) >= maxObservedJobs {
				o.capped = true
				continue
			}
			cur = &jobObs{FirstSeen: now, Created: j.CreatedAt}
			o.jobs[j.ID] = cur
			fresh++
		}
		cur.Type, cur.Status, cur.Started, cur.Finished = j.Type, j.Status, j.StartedAt, j.FinishedAt
	}
	if fresh == 100 {
		o.fullWindows++
	}
}

func (o *ForgeObserver) Start(ctx context.Context) {
	cctx, cancel := context.WithCancel(ctx)
	o.cancel, o.done = cancel, make(chan struct{})
	go func() {
		defer close(o.done)
		t := time.NewTicker(o.interval)
		defer t.Stop()
		for {
			o.poll(cctx)
			select {
			case <-cctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (o *ForgeObserver) Stop() {
	if o.cancel != nil {
		o.cancel()
		<-o.done
	}
}

// Refresh re-reads (by id, GET /v1/jobs/{id}) up to limit observed jobs that were
// not yet final when last listed, so their finish times are recorded.
func (o *ForgeObserver) Refresh(ctx context.Context, limit int) int {
	o.mu.Lock()
	var ids []string
	for id, j := range o.jobs {
		if j.Status != "succeeded" && j.Status != "failed" {
			ids = append(ids, id)
		}
	}
	o.mu.Unlock()
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	n := 0
	for _, id := range ids {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/v1/jobs/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
		resp, err := o.hc.Do(req)
		if err != nil {
			continue
		}
		var j apiJob
		ok := resp.StatusCode == 200 && json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&j) == nil
		resp.Body.Close()
		if ok {
			o.mu.Lock()
			if cur := o.jobs[id]; cur != nil {
				cur.Status, cur.Started, cur.Finished = j.Status, j.StartedAt, j.FinishedAt
				n++
			}
			o.mu.Unlock()
		}
	}
	return n
}

// TypeJobStats is what Forge itself reports for one job type.
type TypeJobStats struct {
	Type          string       `json:"type"`
	Observed      int          `json:"observed"`
	Succeeded     int          `json:"succeeded"`
	Failed        int          `json:"failed"`
	NotFinal      int          `json:"not_final"`
	QueueWait     LatencyStats `json:"queue_wait_forge_timestamps"` // created_at -> started_at
	Execution     LatencyStats `json:"execution_forge_timestamps"`  // started_at -> finished_at
	EndToEnd      LatencyStats `json:"created_to_finished"`
	FirstFinished *time.Time   `json:"first_finished_at,omitempty"`
	LastFinished  *time.Time   `json:"last_finished_at,omitempty"`
	JobsPerSecond *float64     `json:"jobs_per_s_first_to_last_finish"`
}

type ForgeJobSummary struct {
	Polls           int            `json:"polls"`
	PollErrors      int            `json:"poll_errors"`
	PollLatency     LatencyStats   `json:"poll_latency"`
	FullWindowPolls int            `json:"polls_where_all_100_jobs_were_new"`
	TrackedJobs     int            `json:"jobs_observed"`
	Capped          bool           `json:"tracking_cap_reached"`
	Types           []TypeJobStats `json:"types"`
	CoverageNote    string         `json:"coverage_note"`
}

func (o *ForgeObserver) Summary(expectedPerType int) ForgeJobSummary {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := ForgeJobSummary{Polls: o.polls, PollErrors: o.pollErrors, PollLatency: LatencyOf(o.pollLatUS), FullWindowPolls: o.fullWindows, TrackedJobs: len(o.jobs), Capped: o.capped}
	by := map[string][]*jobObs{}
	for _, j := range o.jobs {
		by[j.Type] = append(by[j.Type], j)
	}
	for _, w := range WorkerTypes {
		js := by[w.Type]
		ts := TypeJobStats{Type: w.Type, Observed: len(js)}
		var q, e, ee []int64
		for _, j := range js {
			switch j.Status {
			case "succeeded":
				ts.Succeeded++
			case "failed":
				ts.Failed++
			default:
				ts.NotFinal++
			}
			if j.Started != nil {
				q = append(q, j.Started.Sub(j.Created).Microseconds())
			}
			if j.Started != nil && j.Finished != nil {
				e = append(e, j.Finished.Sub(*j.Started).Microseconds())
			}
			if j.Finished != nil {
				ee = append(ee, j.Finished.Sub(j.Created).Microseconds())
				if ts.FirstFinished == nil || j.Finished.Before(*ts.FirstFinished) {
					t := *j.Finished
					ts.FirstFinished = &t
				}
				if ts.LastFinished == nil || j.Finished.After(*ts.LastFinished) {
					t := *j.Finished
					ts.LastFinished = &t
				}
			}
		}
		ts.QueueWait, ts.Execution, ts.EndToEnd = LatencyOf(q), LatencyOf(e), LatencyOf(ee)
		if ts.FirstFinished != nil && ts.LastFinished != nil {
			if d := ts.LastFinished.Sub(*ts.FirstFinished).Seconds(); d > 0 {
				ts.JobsPerSecond = r3(float64(len(ee)) / d)
			}
		}
		s.Types = append(s.Types, ts)
	}
	s.CoverageNote = fmt.Sprintf("Forge's public API lists only the newest 100 jobs and has no pagination or filters, so jobs are observed by polling; %d of %d expected jobs per type were seen on average. Jobs created faster than the poll interval can be missed; those timings are not measured.",
		avgObserved(s.Types), expectedPerType)
	return s
}

func avgObserved(ts []TypeJobStats) int {
	if len(ts) == 0 {
		return 0
	}
	t := 0
	for _, x := range ts {
		t += x.Observed
	}
	return t / len(ts)
}

// ---------- worker logs ----------

// WorkerLogStats is derived from the workers' own JSON logs.
type WorkerLogStats struct {
	Short       string       `json:"worker"`
	Started     int          `json:"job_starts"`
	Retries     int          `json:"retry_starts"` // job_started with attempt > 1
	Completed   int          `json:"job_completed"`
	FailuresRec int          `json:"failures_recorded"`
	Released    int          `json:"released_on_shutdown"`
	Exec        LatencyStats `json:"execution_from_logs"` // job_started -> job_completed, same attempt
}

func ParseWorkerLogs(logDir string) []WorkerLogStats {
	var res []WorkerLogStats
	for _, w := range WorkerTypes {
		st := WorkerLogStats{Short: w.Short}
		files, _ := filepath.Glob(filepath.Join(logDir, "worker-"+w.Short+"-*.log"))
		var exec []int64
		for _, f := range files {
			fh, err := os.Open(f)
			if err != nil {
				continue
			}
			startAt := map[string]time.Time{}
			sc := bufio.NewScanner(fh)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				line := sc.Bytes()
				if !strings.Contains(string(line), `"job_`) {
					continue
				}
				var l struct {
					Time    time.Time `json:"time"`
					Msg     string    `json:"msg"`
					JobID   string    `json:"job_id"`
					Attempt int       `json:"attempt"`
				}
				if json.Unmarshal(line, &l) != nil {
					continue
				}
				key := fmt.Sprintf("%s/%d", l.JobID, l.Attempt)
				switch l.Msg {
				case "job_started":
					st.Started++
					if l.Attempt > 1 {
						st.Retries++
					}
					startAt[key] = l.Time
				case "job_completed":
					st.Completed++
					if t0, ok := startAt[key]; ok && len(exec) < 200_000 {
						exec = append(exec, l.Time.Sub(t0).Microseconds())
					}
				case "job_failure_recorded":
					st.FailuresRec++
				case "job_released":
					st.Released++
				}
			}
			fh.Close()
		}
		st.Exec = LatencyOf(exec)
		res = append(res, st)
	}
	return res
}

// ---------- ForgeCart database probe (read-only) ----------

type Probe struct {
	db   *sql.DB
	res  *sql.DB // the workers' result tables: the same pool as db unless the deployment has a results database
	pay  *sql.DB // the payment service's database
	conn *sql.DB // server-level connection for pg_stat_activity
}

// NewProbe opens the read-only connections. resultsURL is empty when the workers'
// result tables are in the ForgeCart database.
func NewProbe(appURL, appServerURL, resultsURL, paymentsURL string) (*Probe, error) {
	db, err := sql.Open("pgx", appURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	res := db
	if resultsURL != "" {
		if res, err = sql.Open("pgx", resultsURL); err != nil {
			return nil, err
		}
		res.SetMaxOpenConns(2)
	}
	pay, err := sql.Open("pgx", paymentsURL)
	if err != nil {
		return nil, err
	}
	pay.SetMaxOpenConns(2)
	srv, err := sql.Open("pgx", appServerURL)
	if err != nil {
		return nil, err
	}
	srv.SetMaxOpenConns(1)
	return &Probe{db: db, res: res, pay: pay, conn: srv}, nil
}

func (p *Probe) Close() {
	p.db.Close()
	if p.res != p.db {
		p.res.Close()
	}
	p.pay.Close()
	p.conn.Close()
}

// ids returns the first column of a query as a set (used for checks that compare
// the shop database with the results database, which one SQL statement cannot).
func ids(ctx context.Context, db *sql.DB, query string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

// sums returns (key, value) rows of a query as a map.
func sums(ctx context.Context, db *sql.DB, query string) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Pipeline is a snapshot of the business pipeline counters.
type Pipeline struct {
	Orders       int            `json:"orders"`
	Paid         int            `json:"paid_orders"`
	Events       int            `json:"outbox_events"`
	Published    int            `json:"outbox_published"`
	OutboxFailed int            `json:"outbox_failed"`
	Effects      map[string]int `json:"effects"` // by worker short name
	EffectsTotal int            `json:"effects_total"`
}

func (p *Probe) Counts(ctx context.Context) (Pipeline, error) {
	var pl Pipeline
	pl.Effects = map[string]int{}
	var inv, invc, ntf, ful int
	err := p.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM orders), (SELECT count(*) FROM orders WHERE status='paid'),
		(SELECT count(*) FROM outbox_events), (SELECT count(*) FROM outbox_events WHERE status='published'),
		(SELECT count(*) FROM outbox_events WHERE status='failed')`).
		Scan(&pl.Orders, &pl.Paid, &pl.Events, &pl.Published, &pl.OutboxFailed)
	if err != nil {
		return pl, err
	}
	err = p.res.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM inventory_reconciliations), (SELECT count(*) FROM invoices),
		(SELECT count(*) FROM notification_deliveries), (SELECT count(*) FROM fulfillments)`).
		Scan(&inv, &invc, &ntf, &ful)
	pl.Effects["inventory"], pl.Effects["invoice"], pl.Effects["notification"], pl.Effects["fulfillment"] = inv, invc, ntf, ful
	pl.EffectsTotal = inv + invc + ntf + ful
	return pl, err
}

// Connections returns the number of server connections and the server limit.
func (p *Probe) Connections(ctx context.Context) (active, max int, err error) {
	err = p.conn.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend'), current_setting('max_connections')::int`).Scan(&active, &max)
	return
}

// samples runs a query returning one microsecond value per row.
func (p *Probe) samples(ctx context.Context, q string, args ...any) []int64 {
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v sql.NullInt64
		if rows.Scan(&v) == nil && v.Valid && len(out) < 300_000 {
			out = append(out, v.Int64)
		}
	}
	return out
}

var effectTables = map[string]struct{ table string }{
	"inventory":    {"inventory_reconciliations"},
	"invoice":      {"invoices"},
	"notification": {"notification_deliveries"},
	"fulfillment":  {"fulfillments"},
}

// OutboxLatency is publication latency: outbox row created (in the payment
// transaction) to published (Forge accepted the job).
func (p *Probe) OutboxLatency(ctx context.Context) LatencyStats {
	return LatencyOf(p.samples(ctx, `SELECT (extract(epoch from (published_at - created_at))*1e6)::bigint FROM outbox_events WHERE status='published'`))
}

// EffectLatency is the time from the outbox event to its business-effect row, per worker.
func (p *Probe) EffectLatency(ctx context.Context, short string) LatencyStats {
	t := effectTables[short]
	// The event and its effect may be in different databases, so join them here.
	created, err := sums(ctx, p.db, `SELECT id, (extract(epoch from created_at)*1e6)::bigint FROM outbox_events`)
	if err != nil {
		return LatencyOf(nil)
	}
	rows, err := p.res.QueryContext(ctx, fmt.Sprintf(`SELECT event_id, (extract(epoch from created_at)*1e6)::bigint FROM %s`, t.table))
	if err != nil {
		return LatencyOf(nil)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id string
		var at int64
		if rows.Scan(&id, &at) == nil {
			if from, ok := created[id]; ok && len(out) < 300_000 {
				out = append(out, at-from)
			}
		}
	}
	return LatencyOf(out)
}

// Check is one named integrity assertion.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Verify runs the read-only integrity checks. initialStock is the uniform
// per-product starting inventory of the scenario. drained says whether the
// pipeline was expected to be fully drained (all events published, all effects present).
func (p *Probe) Verify(ctx context.Context, initialStock int, drained bool) []Check {
	var cs []Check
	violations := func(name string, n int, err error) {
		if err != nil {
			cs = append(cs, Check{name, false, "query failed: " + err.Error()})
			return
		}
		cs = append(cs, Check{name, n == 0, fmt.Sprintf("%d violations", n)})
	}
	on := func(db *sql.DB, name, query string, args ...any) {
		var n int
		err := db.QueryRowContext(ctx, query, args...).Scan(&n)
		violations(name, n, err)
	}
	one := func(name, query string, args ...any) { on(p.db, name, query, args...) }
	one("stock ledger: initial stock = current stock + units held by reserved/committed orders, for every product",
		`SELECT count(*) FROM products p WHERE p.inventory_quantity + COALESCE((SELECT sum(oi.quantity) FROM order_items oi JOIN orders o ON o.id=oi.order_id WHERE oi.product_id=p.id AND o.inventory_status IN ('reserved','committed')),0) <> $1`, initialStock)
	one("no product has negative stock", `SELECT count(*) FROM products WHERE inventory_quantity < 0`)
	one("every paid order has exactly one outbox event (order.paid, which the dispatcher turns into its background jobs)",
		`SELECT count(*) FROM orders o WHERE o.status='paid' AND (SELECT count(*) FROM outbox_events e WHERE e.order_id=o.id)<>1`)
	one("no outbox event exists for an order that is not paid (declines and temporary failures create none)",
		`SELECT count(*) FROM outbox_events e JOIN orders o ON o.id=e.order_id WHERE o.status<>'paid'`)
	// Payments live in the payment service's database, orders in the shop's: compare in code.
	type orderPay struct{ status, paidBy string }
	orderRows, oerr := p.db.QueryContext(ctx, `SELECT id, status, COALESCE(paid_attempt_id, '') FROM orders`)
	orderPays := map[string]orderPay{}
	if oerr == nil {
		defer orderRows.Close()
		for orderRows.Next() {
			var id string
			var op orderPay
			if oerr = orderRows.Scan(&id, &op.status, &op.paidBy); oerr != nil {
				break
			}
			orderPays[id] = op
		}
		if oerr == nil {
			oerr = orderRows.Err()
		}
	}
	type attempts struct {
		succeeded, declined int
		successID           string
	}
	byOrder := map[string]*attempts{}
	attemptRows, perr := p.pay.QueryContext(ctx, `SELECT order_id, id, outcome FROM payment_attempts WHERE outcome IN ('succeeded', 'declined')`)
	if perr == nil {
		defer attemptRows.Close()
		for attemptRows.Next() {
			var order, id, outcome string
			if perr = attemptRows.Scan(&order, &id, &outcome); perr != nil {
				break
			}
			a := byOrder[order]
			if a == nil {
				a = &attempts{}
				byOrder[order] = a
			}
			if outcome == "succeeded" {
				a.succeeded++
				a.successID = id
			} else {
				a.declined++
			}
		}
		if perr == nil {
			perr = attemptRows.Err()
		}
	}
	badPay, stranded := 0, 0
	for id, o := range orderPays {
		a := byOrder[id]
		if a == nil {
			a = &attempts{}
		}
		switch o.status {
		case "paid":
			if a.succeeded != 1 || a.declined != 0 || o.paidBy != a.successID {
				badPay++
			}
		case "payment_failed", "cancelled":
			// A failed order has the provider's decline; a cancelled (expired) one the
			// decline that closed it. Either way exactly one decline and no success.
			if a.declined != 1 || a.succeeded != 0 {
				badPay++
			}
		default:
			// A pending order may only have failed (temporary) attempts: a final payment
			// that was not applied to the order is a stranded payment.
			if a.succeeded != 0 || a.declined != 0 {
				stranded++
			}
		}
	}
	for id := range byOrder {
		if _, ok := orderPays[id]; !ok {
			badPay++ // a final payment for an order the shop does not know
		}
	}
	err := oerr
	if err == nil {
		err = perr
	}
	violations("payment state: paid orders have exactly one succeeded attempt, and it is the attempt the order names; payment_failed and cancelled orders one declined and no succeeded", badPay, err)
	violations("no payment is stranded: every succeeded or declined attempt was applied to its order (pending orders have only temporary failures)", stranded, err)
	one("order/inventory state pairs: paid=committed, payment_failed or cancelled=released, pending=reserved",
		`SELECT count(*) FROM orders WHERE NOT ((status='paid' AND inventory_status='committed') OR (status IN ('payment_failed', 'cancelled') AND inventory_status='released') OR (status='pending_payment' AND inventory_status='reserved'))`)
	one("order totals equal the sum of their lines", `SELECT count(*) FROM orders o WHERE o.subtotal_minor <> COALESCE((SELECT sum(line_total_minor) FROM order_items i WHERE i.order_id=o.id),-1)`)
	// The workers' results may be in another database than the orders, so the checks
	// that relate them compare sets here instead of joining in SQL.
	paid, paidErr := ids(ctx, p.db, `SELECT id FROM orders WHERE status='paid'`)
	for _, w := range WorkerTypes {
		t := effectTables[w.Short]
		on(p.res, fmt.Sprintf("%s: never more than one effect per order or per event (no duplicate business effect)", w.Short),
			fmt.Sprintf(`SELECT (SELECT count(*) - count(DISTINCT order_id) FROM %s) + (SELECT count(*) - count(DISTINCT event_id) FROM %s)`, t.table, t.table))
		withEffect, err := ids(ctx, p.res, fmt.Sprintf(`SELECT order_id FROM %s`, t.table))
		if err == nil {
			err = paidErr
		}
		notPaid := 0
		for id := range withEffect {
			if !paid[id] {
				notPaid++
			}
		}
		violations(fmt.Sprintf("%s: no effect for an order that is not paid", w.Short), notPaid, err)
		if drained {
			missing := 0
			for id := range paid {
				if !withEffect[id] {
					missing++
				}
			}
			violations(fmt.Sprintf("%s: every paid order has its effect (pipeline drained)", w.Short), missing, err)
		}
	}
	on(p.res, "invoices: line snapshots exist and sum to the invoice subtotal",
		`SELECT count(*) FROM invoices i WHERE i.subtotal_minor <> COALESCE((SELECT sum(line_total_minor) FROM invoice_items l WHERE l.invoice_id=i.id),-1)`)
	planned, err1 := sums(ctx, p.res, `SELECT f.order_id, COALESCE(sum(x.quantity), -1) FROM fulfillments f LEFT JOIN fulfillment_items x ON x.fulfillment_id=f.id GROUP BY f.order_id`)
	ordered, err2 := sums(ctx, p.db, `SELECT order_id, sum(quantity) FROM order_items GROUP BY order_id`)
	if err1 == nil {
		err1 = err2
	}
	wrongPlans := 0
	for order, q := range planned {
		if want, ok := ordered[order]; !ok || want != q {
			wrongPlans++
		}
	}
	violations("fulfillment plans: item quantities equal the order's item quantities", wrongPlans, err1)
	one("inventory reconciliation never changed stock: covered by the stock ledger above (it only reads)", `SELECT 0`)
	one("no outbox event is in the terminal 'failed' state", `SELECT count(*) FROM outbox_events WHERE status='failed'`)
	if drained {
		one("every outbox event has been published (nothing stranded)", `SELECT count(*) FROM outbox_events WHERE status<>'published'`)
	}
	return cs
}

// OrderStatusCounts reports orders by status.
func (p *Probe) OrderStatusCounts(ctx context.Context) map[string]int {
	out := map[string]int{}
	rows, err := p.db.QueryContext(ctx, `SELECT status, count(*) FROM orders GROUP BY status`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if rows.Scan(&s, &n) == nil {
			out[s] = n
		}
	}
	return out
}

// ---------- drain ----------

// DrainResult describes waiting for the background pipeline to finish.
type DrainResult struct {
	Drained      bool     `json:"drained"`
	WaitedS      float64  `json:"drain_time_s"` // load end -> last expected effect present
	TimeoutS     float64  `json:"timeout_s"`
	ExpectedJobs int      `json:"expected_jobs"`
	FinalEffects int      `json:"final_effects"`
	FinalPending int      `json:"outbox_not_published"`
	PollQueries  int      `json:"verification_queries"`
	PollMeanMS   *float64 `json:"mean_query_ms"`
	// DrainRate is the background drain rate: 5 x paid orders / drain seconds (0 if not drained).
	DrainRate      float64 `json:"drain_rate_jobs_per_s"`
	PollIntervalMS int     `json:"poll_interval_ms"` // the drain is only observed every poll: its resolution
	Note           string  `json:"note"`
}

// WaitDrain polls the pipeline counters every 250 ms until every paid order has
// its event published (one per order) and all its job effects present (four per
// order), or the timeout.
func (p *Probe) WaitDrain(ctx context.Context, timeout time.Duration) (DrainResult, Pipeline) {
	start := time.Now()
	dr := DrainResult{TimeoutS: timeout.Seconds()}
	var last Pipeline
	var qd []int64
	for {
		q0 := time.Now()
		pl, err := p.Counts(ctx)
		qd = append(qd, time.Since(q0).Microseconds())
		dr.PollQueries++
		if err == nil {
			last = pl
			dr.ExpectedJobs = jobsPerOrder * pl.Paid
			if pl.EffectsTotal == dr.ExpectedJobs && pl.Published == pl.Paid && pl.Events == pl.Paid {
				dr.Drained = true
				break
			}
		}
		if time.Since(start) > timeout || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
	dr.WaitedS = round3(time.Since(start).Seconds())
	dr.FinalEffects, dr.FinalPending = last.EffectsTotal, last.Events-last.Published
	if len(qd) > 0 {
		var s int64
		for _, v := range qd {
			s += v
		}
		dr.PollMeanMS = r3(float64(s) / float64(len(qd)) / 1000)
	}
	dr.PollIntervalMS = 250
	if dr.Drained && dr.WaitedS > 0 {
		dr.DrainRate = round3(float64(dr.ExpectedJobs) / dr.WaitedS)
	}
	dr.Note = "drain time is measured from the start of the wait (normally the end of the HTTP load; the framework replaces this note if a scenario hook ran first) to the poll that sees every paid order with all its events published and all its business effects recorded; it is polled every 250 ms"
	return dr, last
}

// Span returns the time from the first outbox event to the last business effect.
func (p *Probe) Span(ctx context.Context, effects int) *PipelineSpan {
	var first, last sql.NullTime
	err := p.db.QueryRowContext(ctx, `SELECT min(created_at) FROM outbox_events`).Scan(&first)
	if err == nil {
		err = p.res.QueryRowContext(ctx, `SELECT GREATEST((SELECT max(created_at) FROM inventory_reconciliations), (SELECT max(created_at) FROM invoices), (SELECT max(created_at) FROM notification_deliveries),
		         (SELECT max(created_at) FROM fulfillments))`).Scan(&last)
	}
	if err != nil || !first.Valid || !last.Valid {
		return nil
	}
	sp := &PipelineSpan{FirstEventAt: first.Time.UTC(), LastEffectAt: last.Time.UTC()}
	sp.Seconds = round3(last.Time.Sub(first.Time).Seconds())
	if sp.Seconds > 0 {
		sp.JobsPerSec = r3(float64(effects) / sp.Seconds)
	}
	return sp
}

// StockByProduct returns the current stock per SKU.
func (p *Probe) StockByProduct(ctx context.Context) map[string]int {
	out := map[string]int{}
	rows, err := p.db.QueryContext(ctx, `SELECT sku, inventory_quantity FROM products`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if rows.Scan(&s, &n) == nil {
			out[s] = n
		}
	}
	return out
}

// Deadlocks is the cumulative deadlock count of the ForgeCart database since it was created.
func (p *Probe) Deadlocks(ctx context.Context) (int, bool) {
	var n int
	if err := p.conn.QueryRowContext(ctx, `SELECT COALESCE(sum(deadlocks),0)::int FROM pg_stat_database WHERE datname = 'forgecart_lt'`).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// EventRef identifies one outbox event for the duplicate-delivery scenario.
type EventRef struct {
	EventID, Type, OrderID, AttemptID string
}

// EventsForPaidOrders returns the jobs of up to n paid orders (read-only): each
// order's one outbox event is listed once per worker job type, which is what the
// dispatcher submits to Forge for it.
func (p *Probe) EventsForPaidOrders(ctx context.Context, n int) []EventRef {
	rows, err := p.db.QueryContext(ctx, `SELECT e.id, e.event_type, e.order_id, e.payload->>'payment_attempt_id'
		FROM outbox_events e WHERE e.order_id IN (SELECT id FROM orders WHERE status='paid' ORDER BY created_at LIMIT $1) ORDER BY e.order_id, e.event_type`, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []EventRef
	for rows.Next() {
		var r EventRef
		if rows.Scan(&r.EventID, &r.Type, &r.OrderID, &r.AttemptID) != nil {
			continue
		}
		if r.Type != "order.paid" {
			out = append(out, r) // an older event, which is one job of its own type
			continue
		}
		for _, w := range WorkerTypes {
			j := r
			j.Type = w.Type
			out = append(out, j)
		}
	}
	return out
}

// DistinctEventIDs returns how many distinct outbox event ids exist.
func (p *Probe) DistinctEventIDs(ctx context.Context) int {
	var n int
	p.db.QueryRowContext(ctx, `SELECT count(DISTINCT id) FROM outbox_events`).Scan(&n)
	return n
}
