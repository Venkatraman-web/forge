package main

// The scenario engine: a bounded closed-loop workload generator that drives
// ForgeCart's public HTTP API (checkout, payment) exactly as a client would. Every
// scenario that needs HTTP load uses RunLoad; nothing here reads a database.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxResponseBytes = 256 << 10
	// Hard safety limits for any scenario (configurable values are clamped to these).
	// 500 is also the most the remote-cart command accepts; a lower value here silently turns a
	// request for more shoppers into fewer (it was 200 until 2026-10-24, so earlier runs labelled
	// 300 to 500 shoppers really used 200).
	HardMaxConcurrency = 500
	HardMaxWorkflows   = 50_000
	HardMaxSeconds     = 1800
)

var (
	orderIDRe = regexp.MustCompile(`^ord_[0-9a-f]{24}$`)
	codeRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	skuRe     = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{2,63}$`)
)

type product struct {
	SKU       string `json:"sku"`
	Currency  string `json:"currency"`
	Available int    `json:"available_inventory"`
	Active    bool   `json:"active"`
}

// Client is a bounded HTTP client for the ForgeCart API.
type Client struct {
	base string
	hc   *http.Client
}

func NewClient(base string, timeout time.Duration, conns int) *Client {
	tr := &http.Transport{MaxIdleConnsPerHost: conns, MaxConnsPerHost: conns + 4, IdleConnTimeout: 30 * time.Second, Proxy: nil}
	return &Client{base: base, hc: &http.Client{Timeout: timeout, Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *Client) Close() { c.hc.CloseIdleConnections() }

type reply struct {
	status int
	body   []byte
	lat    time.Duration
	fail   reqKind // kExpected means "got a response"
	cat    string
	sentAt time.Time
	doneAt time.Time
}

// do sends one request. POSTs are made non-replayable: net/http would otherwise
// silently resend a POST carrying an Idempotency-Key header on a broken reused
// connection, and the generator must count what it sends.
func (c *Client) do(ctx context.Context, method, path string, hdr map[string]string, body []byte) reply {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else if method == http.MethodPost {
		rd = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return reply{fail: kTransport, cat: "request build failed"}
	}
	if method == http.MethodPost {
		req.GetBody = nil
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		lat := time.Since(start)
		switch {
		case ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled):
			return reply{lat: lat, fail: kCancelled, sentAt: start, doneAt: time.Now()}
		case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
			return reply{lat: lat, fail: kTimeout, cat: "timeout", sentAt: start, doneAt: time.Now()}
		}
		return reply{lat: lat, fail: kTransport, cat: "transport failure (connection or protocol error)", sentAt: start, doneAt: time.Now()}
	}
	defer resp.Body.Close()
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	done := time.Now()
	if rerr != nil {
		switch {
		case ctx.Err() != nil:
			return reply{lat: done.Sub(start), fail: kCancelled, sentAt: start, doneAt: done}
		case errors.Is(rerr, context.DeadlineExceeded) || isTimeout(rerr):
			return reply{lat: done.Sub(start), fail: kTimeout, cat: "timeout", sentAt: start, doneAt: done}
		}
		return reply{lat: done.Sub(start), fail: kTransport, cat: "transport failure (reading response)", sentAt: start, doneAt: done}
	}
	return reply{status: resp.StatusCode, body: b, lat: done.Sub(start), sentAt: start, doneAt: done}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func errCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && codeRe.MatchString(e.Error.Code) {
		return e.Error.Code
	}
	return "unrecognized_error_body"
}

// DiscoverProducts lists the catalog through the public API and returns usable products.
func (c *Client) DiscoverProducts(ctx context.Context) ([]product, error) {
	var out []product
	after := ""
	for page := 0; page < 20; page++ {
		path := "/v1/products?limit=100"
		if after != "" {
			path += "&after=" + after
		}
		rp := c.do(ctx, http.MethodGet, path, nil, nil)
		if rp.fail != kExpected || rp.status != 200 {
			return nil, fmt.Errorf("product discovery failed (status %d)", rp.status)
		}
		var pg struct {
			Products   []product `json:"products"`
			NextCursor *string   `json:"next_cursor"`
		}
		if json.Unmarshal(rp.body, &pg) != nil {
			return nil, errors.New("product discovery returned a malformed body")
		}
		for _, p := range pg.Products {
			if p.Active && p.Currency == "USD" && p.Available >= 1 && skuRe.MatchString(p.SKU) {
				out = append(out, p)
			}
		}
		if pg.NextCursor == nil || !skuRe.MatchString(*pg.NextCursor) {
			break
		}
		after = *pg.NextCursor
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SKU < out[j].SKU })
	if len(out) == 0 {
		return nil, errors.New("the catalog has no usable products")
	}
	return out, nil
}

// ---------- the workflow ----------

type orderBody struct {
	CustomerEmail string      `json:"customer_email"`
	Items         []orderItem `json:"items"`
}
type orderItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

// Plan builds workflow i deterministically from (seed, i).
func Plan(products []product, seed int64, tag string, i int) orderBody {
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(i)))
	n := 1 + rng.IntN(min(3, len(products)))
	perm := rng.Perm(len(products))[:n]
	items := make([]orderItem, 0, n)
	for _, idx := range perm {
		items = append(items, orderItem{SKU: products[idx].SKU, Quantity: 1 + rng.IntN(3)})
	}
	return orderBody{CustomerEmail: fmt.Sprintf("lt-%s-%d@example.test", tag, i), Items: items}
}

var payOutcomes = map[string]bool{"succeeded": true, "declined": true, "temporary_failure": true}

// Workload is what the workers share.
type Workload struct {
	cl       *Client
	products []product
	seed     int64
	tag      string
	rec      *Recorder
	next     atomic.Int64
}

func (w *Workload) record(stage string, rp reply, k reqKind, cat string) {
	if rp.fail != kExpected {
		k, cat = rp.fail, rp.cat
		if k == kCancelled {
			cat = ""
		} else if cat != "" {
			cat = stage + ": " + cat
		}
	}
	w.rec.Request(stage, rp.status, rp.lat, k, cat)
}

func (w *Workload) workflow(ctx context.Context, i int) {
	w.rec.WorkflowStart()
	t0 := time.Now()
	end := w.run(ctx, i)
	if ctx.Err() != nil && end == endFailed {
		end = endInterrupted
	}
	w.rec.WorkflowEnd(end, time.Since(t0))
}

func (w *Workload) run(ctx context.Context, i int) wfEnd {
	hdr := map[string]string{"Content-Type": "application/json", "Idempotency-Key": fmt.Sprintf("lt-%s-%d-order", w.tag, i)}
	body, _ := json.Marshal(Plan(w.products, w.seed, w.tag, i))
	rp := w.cl.do(ctx, http.MethodPost, "/v1/orders", hdr, body)
	var orderID string
	k, cat := kUnexpected, ""
	stock := false
	switch {
	case rp.fail != kExpected:
	case rp.status == http.StatusCreated:
		var o struct {
			OrderID string `json:"order_id"`
		}
		if json.Unmarshal(rp.body, &o) == nil && orderIDRe.MatchString(o.OrderID) {
			k, orderID = kExpected, o.OrderID
			w.rec.OrderOut("created")
		} else {
			cat = "order_create: 201 with a malformed body"
		}
	case rp.status == http.StatusConflict || rp.status == http.StatusNotFound:
		switch code := errCode(rp.body); code {
		case "insufficient_inventory":
			k, stock = kBusiness, true
			w.rec.OrderOut("stock_conflict")
		case "product_not_found", "mixed_currency", "unsupported_currency":
			k = kBusiness
			w.rec.OrderOut("other_rejection:" + code)
		default:
			cat = fmt.Sprintf("order_create: http %d %s", rp.status, code)
		}
	default:
		cat = fmt.Sprintf("order_create: http %d %s", rp.status, errCode(rp.body))
	}
	w.record("order_create", rp, k, cat)
	if rp.fail == kCancelled {
		return endInterrupted
	}
	if k == kBusiness {
		if stock {
			return endRejectedStock
		}
		return endRejectedOther
	}
	if k != kExpected {
		return endFailed
	}
	orderDone := rp.doneAt
	hdr = map[string]string{"Idempotency-Key": fmt.Sprintf("lt-%s-%d-pay", w.tag, i)}
	rp = w.cl.do(ctx, http.MethodPost, "/v1/orders/"+orderID+"/payment-attempts", hdr, nil)
	if !rp.sentAt.IsZero() {
		w.rec.Gap(rp.sentAt.Sub(orderDone)) // client-side overhead between the two requests
	}
	k, cat = kUnexpected, ""
	switch {
	case rp.fail != kExpected:
	case rp.status == http.StatusOK:
		var p struct {
			Outcome string `json:"outcome"`
		}
		if json.Unmarshal(rp.body, &p) == nil && payOutcomes[p.Outcome] {
			k = kExpected
			w.rec.Payment(p.Outcome)
		} else {
			cat = "payment: 200 with an unrecognized outcome"
		}
	default:
		cat = fmt.Sprintf("payment: http %d %s", rp.status, errCode(rp.body))
	}
	w.record("payment", rp, k, cat)
	if rp.fail == kCancelled {
		return endInterrupted
	}
	if k != kExpected {
		return endFailed
	}
	return endCompleted
}

// ---------- phases and results ----------

// Phase is one segment of closed-loop load. A phase ends when Workflows have been
// started (if > 0) or Duration has elapsed (if > 0), whichever comes first.
type Phase struct {
	Name        string
	Concurrency int
	Workflows   int
	Duration    time.Duration
}

// PhaseResult summarizes one phase from the workflow records.
type PhaseResult struct {
	Name            string       `json:"name"`
	Concurrency     int          `json:"concurrency"`
	StartS          float64      `json:"start_s"`
	EndS            float64      `json:"end_s"`
	DurationS       float64      `json:"duration_s"`               // EndS - StartS, stored so a short tail window is visible as such
	Partial         bool         `json:"partial_window,omitempty"` // shorter than the nominal window: not comparable with full windows
	Completed       int          `json:"completed_workflows"`
	CompletedPerSec *float64     `json:"completed_per_s"`
	WorkflowLatency LatencyStats `json:"workflow_latency"`
	Requests        int          `json:"requests"`
	Errors          int          `json:"errors"`
}

// LoadResult is everything measured by one RunLoad.
type LoadResult struct {
	Label         string         `json:"label"`
	StartedAt     time.Time      `json:"started_at"`
	EndedAt       time.Time      `json:"ended_at"`
	ElapsedS      float64        `json:"elapsed_s"`
	Seed          int64          `json:"seed"`
	Tag           string         `json:"run_tag"`
	Phases        []PhaseResult  `json:"phases"`
	Started       int            `json:"workflows_started"`
	Completed     int            `json:"completed"`
	RejectedStock int            `json:"rejected_stock_conflict"`
	RejectedOther int            `json:"rejected_other_business"`
	Failed        int            `json:"failed"`
	Interrupted   int            `json:"interrupted"`
	MaxActive     int            `json:"max_observed_concurrency"`
	Payment       map[string]int `json:"payment_outcomes"`
	OrderOutcomes map[string]int `json:"order_outcomes"`
	Stages        []StageResult  `json:"stages"`
	WorkflowLat   LatencyStats   `json:"workflow_latency"`
	WfPerSec      *float64       `json:"completed_workflows_per_s"`
	ReqPerSec     *float64       `json:"requests_per_s"`
	ErrorRatePct  *float64       `json:"http_failure_rate_pct"`
	Timeline      []SecPoint     `json:"timeline"`
	Errors        map[string]int `json:"errors"`
	// Load-generator self-check.
	GeneratorCores   float64       `json:"generator_cpu_cores_avg"`
	GapP99US         *float64      `json:"generator_gap_p99_us"`
	GeneratorSuspect bool          `json:"generator_may_be_bottleneck"`
	GeneratorNote    string        `json:"generator_note"`
	PhaseMarks       []PhaseMark   `json:"phase_marks"`
	Windows          []PhaseResult `json:"windows"`
}

type PhaseMark struct {
	Name   string  `json:"name"`
	StartS float64 `json:"start_s"`
	EndS   float64 `json:"end_s"`
}

func procCPUSeconds() float64 {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	f := strings.Fields(s[i+2:])
	if len(f) < 13 {
		return 0
	}
	var ut, st float64
	fmt.Sscan(f[11], &ut)
	fmt.Sscan(f[12], &st)
	return (ut + st) / 100 // USER_HZ is 100 on Linux
}

// RunLoad executes the phases in order against base using the given products and
// returns the measurements. It stops early if ctx is cancelled.
func RunLoad(ctx context.Context, base, label, tag string, seed int64, timeout time.Duration, products []product, phases []Phase, indexBase int) *LoadResult {
	maxC := 1
	for _, p := range phases {
		maxC = max(maxC, p.Concurrency)
	}
	cl := NewClient(base, timeout, min(maxC, HardMaxConcurrency))
	defer cl.Close()
	rec := NewRecorder()
	w := &Workload{cl: cl, products: products, seed: seed, tag: tag, rec: rec}
	w.next.Store(int64(indexBase))
	res := &LoadResult{Label: label, Seed: seed, Tag: tag}

	cpu0 := procCPUSeconds()
	start := time.Now()
	rec.mu.Lock()
	rec.origin = start
	rec.mu.Unlock()
	res.StartedAt = start
	for _, ph := range phases {
		conc := min(ph.Concurrency, HardMaxConcurrency)
		if ph.Workflows > 0 {
			conc = min(conc, ph.Workflows)
		}
		pctx := ctx
		var cancel context.CancelFunc
		deadline := time.Time{}
		if ph.Duration > 0 {
			deadline = time.Now().Add(ph.Duration)
		}
		pctx, cancel = context.WithCancel(ctx)
		first := int(w.next.Load())
		ps := time.Since(start).Seconds()
		var wg sync.WaitGroup
		for k := 0; k < conc; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for pctx.Err() == nil {
					if !deadline.IsZero() && !time.Now().Before(deadline) {
						return
					}
					i := int(w.next.Add(1)) - 1
					if ph.Workflows > 0 && i-first >= ph.Workflows {
						return
					}
					w.workflow(pctx, i)
				}
			}()
		}
		wg.Wait()
		cancel()
		res.PhaseMarks = append(res.PhaseMarks, PhaseMark{Name: ph.Name, StartS: ps, EndS: time.Since(start).Seconds()})
		if ctx.Err() != nil {
			break
		}
	}
	end := time.Now()
	res.EndedAt = end
	cpu := procCPUSeconds() - cpu0
	elapsed := end.Sub(start).Seconds()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	res.ElapsedS = float64(int(elapsed*1000)) / 1000
	res.Started, res.Completed, res.RejectedStock, res.RejectedOther = rec.started, rec.completed, rec.rejStock, rec.rejOther
	res.Failed, res.Interrupted, res.MaxActive = rec.failed, rec.interrupted, rec.maxActive
	res.Payment, res.OrderOutcomes, res.Errors = rec.payment, rec.orderOut, rec.errs
	res.Stages = []StageResult{rec.stages["order_create"].result("order_create"), rec.stages["payment"].result("payment")}
	var wfSamples []int64
	for _, p := range rec.wf {
		wfSamples = append(wfSamples, int64(p.latUS))
	}
	res.WorkflowLat = LatencyOf(wfSamples)
	res.Stages = append(res.Stages, StageResult{Name: "workflow", Latency: res.WorkflowLat})
	if elapsed > 0 {
		res.WfPerSec = r3(float64(rec.completed) / elapsed)
		res.ReqPerSec = r3(float64(rec.stages["order_create"].c.Requests+rec.stages["payment"].c.Requests) / elapsed)
	}
	var all Counts
	for _, n := range []string{"order_create", "payment"} {
		c := rec.stages[n].c
		all.Requests += c.Requests
		all.Cancellations += c.Cancellations
		all.Unexpected += c.Unexpected
		all.Transport += c.Transport
		all.Timeouts += c.Timeouts
	}
	res.ErrorRatePct = failureRatePct(all)
	res.Timeline = append([]SecPoint(nil), rec.sec...)
	for i, ph := range phases {
		if i >= len(res.PhaseMarks) {
			break
		}
		m := res.PhaseMarks[i]
		pr := PhaseResult{Name: ph.Name, Concurrency: ph.Concurrency, StartS: round3(m.StartS), EndS: round3(m.EndS), DurationS: round3(m.EndS - m.StartS)}
		var lat []int64
		for _, p := range rec.wf {
			t := float64(p.atMS) / 1000
			if t >= m.StartS && t < m.EndS+0.0005 {
				lat = append(lat, int64(p.latUS))
			}
		}
		pr.Completed = len(lat)
		pr.WorkflowLatency = LatencyOf(lat)
		if d := m.EndS - m.StartS; d > 0 {
			pr.CompletedPerSec = r3(float64(pr.Completed) / d)
		}
		for _, s := range rec.sec {
			if float64(s.T) >= m.StartS-0.999 && float64(s.T) < m.EndS {
				pr.Requests += s.Requests
				pr.Errors += s.Errors
			}
		}
		res.Phases = append(res.Phases, pr)
	}
	// Fixed windows (10 s, widened so there are at most 120) for trend analysis.
	wsec := 10.0
	if elapsed/wsec > 120 {
		wsec = float64(int(elapsed/120) + 1)
	}
	for ws := 0.0; ws < elapsed; ws += wsec {
		we := min(ws+wsec, elapsed)
		// The last window is usually a short tail (for example the in-flight
		// workflows finishing just after the deadline). It is kept, with its real
		// duration and Partial set, but trend comparisons only use full windows.
		pr := PhaseResult{Name: fmt.Sprintf("%.1f-%.1fs", ws, we), StartS: round3(ws), EndS: round3(we), DurationS: round3(we - ws), Partial: we-ws < wsec*0.999}
		var lat []int64
		for _, p := range rec.wf {
			t := float64(p.atMS) / 1000
			if t >= ws && t < we {
				lat = append(lat, int64(p.latUS))
			}
		}
		pr.Completed, pr.WorkflowLatency = len(lat), LatencyOf(lat)
		if d := we - ws; d > 0 {
			pr.CompletedPerSec = r3(float64(pr.Completed) / d)
		}
		for _, sp := range rec.sec {
			if float64(sp.T) >= ws && float64(sp.T) < we {
				pr.Requests += sp.Requests
				pr.Errors += sp.Errors
			}
		}
		res.Windows = append(res.Windows, pr)
	}
	// Is the generator the bottleneck? Two heuristics, both reported.
	if elapsed > 0 {
		res.GeneratorCores = round3(cpu / elapsed)
	}
	if len(rec.gaps) > 0 {
		g := append([]int64(nil), rec.gaps...)
		sort.Slice(g, func(i, j int) bool { return g[i] < g[j] })
		v, _ := Percentile(g, 99)
		res.GapP99US = r3(float64(v))
		if v > 5000 {
			res.GeneratorSuspect = true
			res.GeneratorNote = "p99 client-side gap between the order and payment requests exceeded 5 ms: the generator may be adding delay"
		}
	}
	if res.GeneratorCores > 0.5*float64(runtime.NumCPU()) || res.GeneratorCores > 4 {
		res.GeneratorSuspect = true
		res.GeneratorNote += " generator CPU use is high relative to the host"
	}
	if !res.GeneratorSuspect {
		res.GeneratorNote = "no sign that the load generator limited the run (low CPU, small client-side gaps)"
	}
	return res
}

func round3(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }
