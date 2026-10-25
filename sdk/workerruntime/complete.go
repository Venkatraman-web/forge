package workerruntime

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/Venkatraman-web/forge/sdk/workerclient"
)

// maxBatchResultBytes caps the results of one completion batch, so a batch of
// large results is split into several requests instead of exceeding the
// server's body limit.
const maxBatchResultBytes = 1 << 20

// completer turns the completions of all slots into batched requests.
//
// Writing a finished job costs the database one commit however small it is, and
// commits are what limit throughput on a real disk. So instead of every slot
// sending its own request, a slot hands its completion to the completer and
// waits. One flusher goroutine at a time sends whatever has piled up (up to
// workerclient.MaxClaimBatch jobs) as one request, then looks again. It never
// waits to fill a batch: with a single job finishing, the job is sent at once;
// batches form on their own while a request is in flight. At most one flusher
// runs, so this adds no per-job goroutines.
type completer struct {
	mu       sync.Mutex
	pending  []*completion
	flushing bool
}

type completion struct {
	item      workerclient.CompleteItem
	done      chan error // buffered(1); receives exactly one answer
	abandoned bool       // the caller gave up; guarded by completer.mu
}

// complete reports one job as succeeded through the batching path and returns
// what workerclient.Complete would: nil, an ownership-lost error, or an error
// for which IsUncertain tells whether the job may have been completed.
func (w *Worker) complete(ctx context.Context, claim workerclient.ClaimedJob, result json.RawMessage) error {
	c := &completion{item: workerclient.CompleteItem{Job: claim, Result: result}, done: make(chan error, 1)}
	q := &w.completions
	q.mu.Lock()
	q.pending = append(q.pending, c)
	start := !q.flushing
	q.flushing = true
	q.mu.Unlock()
	if start {
		go w.flushCompletions()
	}

	select {
	case err := <-c.done:
		return err
	case <-ctx.Done():
		// Not yet sent: drop it so the caller's retry is the only request. Already
		// in flight: the outcome is unknown, which is what the error says.
		q.mu.Lock()
		c.abandoned = true
		q.mu.Unlock()
		return &workerclient.TransportError{Op: "complete", Err: ctx.Err()}
	}
}

// flushCompletions sends batches until nothing is pending, then exits.
func (w *Worker) flushCompletions() {
	q := &w.completions
	for {
		q.mu.Lock()
		batch := q.takeBatch()
		if len(batch) == 0 {
			q.flushing = false
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()

		items := make([]workerclient.CompleteItem, len(batch))
		for i, c := range batch {
			items[i] = c.item
		}
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		errs, err := w.client.CompleteBatch(ctx, items)
		cancel()
		for i, c := range batch {
			if err != nil {
				c.done <- err
			} else {
				c.done <- errs[i]
			}
		}
	}
}

// takeBatch removes the next batch from pending (the caller holds q.mu).
func (q *completer) takeBatch() []*completion {
	var batch []*completion
	size := 0
	rest := q.pending[:0]
	for i, c := range q.pending {
		if c.abandoned {
			continue
		}
		if len(batch) == workerclient.MaxClaimBatch || (len(batch) > 0 && size+len(c.item.Result) > maxBatchResultBytes) {
			rest = append(rest, q.pending[i:]...)
			break
		}
		batch = append(batch, c)
		size += len(c.item.Result)
	}
	q.pending = rest
	return batch
}
