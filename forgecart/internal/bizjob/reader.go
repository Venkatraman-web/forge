package bizjob

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Reader gathers the shop reads of jobs that need an order at the same moment into
// ONE statement. Every job of a worker reads the same three things (its event,
// its order, its lines); with many handlers running at once the same statement
// would otherwise run once per job, and the shop database spends more time on the
// round trips and statement set-up than on the rows. Each handler still gets its
// own row and applies exactly the same checks (paidRow.verified); only the
// transport is shared. The job input is unchanged: three IDs, no customer data.
type Reader struct {
	db       *sql.DB
	maxBatch int
	window   time.Duration
	reqs     chan *readReq
	done     chan struct{} // closed when Run returns, so a waiting Read gives up
}

type readReq struct {
	in   Input
	resp chan readResult // buffered: the reader never blocks on a handler that left
}

type readResult struct {
	row paidRow
	ok  bool // false: no such event
	err error
}

// batchedQuery reads many (event, order) pairs. The pair is joined, not the event
// alone, so a job naming an order that is not the event's order is reported as
// event_mismatch by the same checks as before.
var batchedQuery = `SELECT w.event_id, w.order_id,
	e.event_type, e.order_id, e.payload ->> 'payment_attempt_id',
	o.status, o.inventory_status, o.paid_attempt_id, o.customer_email, o.currency, o.subtotal_minor, o.total_minor,
	` + linesColumn("w.order_id") + `
FROM unnest($1::text[], $2::text[]) AS w(event_id, order_id)
JOIN outbox_events e ON e.id = w.event_id
LEFT JOIN orders o ON o.id = w.order_id`

// NewReader makes a reader for db. maxBatch is the most reads one statement
// carries (the worker's concurrency is a natural choice); the first read waits at
// most about a millisecond for others to arrive.
func NewReader(db *sql.DB, maxBatch int) *Reader {
	if maxBatch < 1 {
		maxBatch = 1
	}
	return &Reader{db: db, maxBatch: maxBatch, window: time.Millisecond, reqs: make(chan *readReq, maxBatch), done: make(chan struct{})}
}

// Run serves reads until ctx ends. It is the only goroutine that touches the
// batch, so no lock is needed.
func (r *Reader) Run(ctx context.Context) {
	defer close(r.done)
	for {
		var first *readReq
		select {
		case <-ctx.Done():
			return
		case first = <-r.reqs:
		}
		batch := []*readReq{first}
		timer := time.NewTimer(r.window)
	collect:
		for len(batch) < r.maxBatch {
			select {
			case q := <-r.reqs:
				batch = append(batch, q)
			case <-timer.C:
				break collect
			case <-ctx.Done():
				break collect
			}
		}
		timer.Stop()
		r.flush(ctx, batch)
	}
}

func (r *Reader) flush(ctx context.Context, batch []*readReq) {
	ids := make([]string, len(batch))
	orders := make([]string, len(batch))
	for i, q := range batch {
		ids[i], orders[i] = q.in.EventID, q.in.OrderID
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	found := map[[2]string]paidRow{}
	err := func() error {
		rows, err := r.db.QueryContext(qctx, batchedQuery, ids, orders)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key [2]string
			var row paidRow
			if err := rows.Scan(append([]any{&key[0], &key[1]}, row.fields()...)...); err != nil {
				return err
			}
			found[key] = row
		}
		return rows.Err()
	}()
	for _, q := range batch {
		if err != nil {
			q.resp <- readResult{err: err}
			continue
		}
		row, ok := found[[2]string{q.in.EventID, q.in.OrderID}]
		q.resp <- readResult{row: row, ok: ok}
	}
}

// read queues one read and waits for its row. withLines asks for the lines to be
// checked as well (see ReadPaidOrder); the statement fetches them either way.
func (r *Reader) read(ctx context.Context, eventType string, in Input, withLines bool) (PaidOrder, Lines, error) {
	q := &readReq{in: in, resp: make(chan readResult, 1)}
	select {
	case r.reqs <- q:
	case <-ctx.Done():
		return PaidOrder{}, nil, ctx.Err()
	case <-r.done:
		return PaidOrder{}, nil, errors.New("shop reader stopped")
	}
	select {
	case res := <-q.resp:
		if res.err != nil {
			return PaidOrder{}, nil, res.err
		}
		if !res.ok {
			return PaidOrder{}, nil, Reject("unknown_event")
		}
		return res.row.verified(eventType, in, withLines)
	case <-ctx.Done():
		return PaidOrder{}, nil, ctx.Err()
	case <-r.done:
		return PaidOrder{}, nil, errors.New("shop reader stopped")
	}
}
