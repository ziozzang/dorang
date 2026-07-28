package batch

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"testing"
)

// Many rows, many workers, several batches at once, and readers polling the
// whole time. Run under -race this is the package's concurrency gate: dispatch,
// progress writes, finalization and the read paths all touch the same records.
func TestConcurrentRowsAndReaders(t *testing.T) {
	const perBatch = 300
	h := newHarness(t, func(c *Config) {
		c.RowConcurrency = 32
		c.MaxActiveBatches = 3
		c.CountFlush = 1 // write progress on every row, maximizing contention
		c.MaxAttempts = 2
	})
	h.exec.fn = func(ctx context.Context, req *ExecRequest, attempt int) (*ExecResult, error) {
		switch bucket(req.CustomID) % 11 {
		case 0:
			if attempt == 1 {
				return errResult(503, "flapping"), nil
			}
			return okResult(req), nil
		case 1:
			return errResult(400, "no"), nil
		case 2:
			return nil, &terminalError{msg: "no route"}
		}
		return okResult(req), nil
	}

	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		ids = append(ids, h.create(h.upload(jsonlFile(perBatch, "m1"))).ID)
	}

	// A fourth batch, cancelled while everything else is running.
	g := newGate()
	defer g.openAll()
	cancelled := h.create(h.upload(jsonlFile(50, "m2")))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var readErrs []error
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := h.svc.Retrieve(ctx, ids[i%len(ids)], "key-1"); err != nil {
					mu.Lock()
					readErrs = append(readErrs, err)
					mu.Unlock()
				}
				if _, err := h.svc.List(ctx, BatchQuery{OwnerKeyID: "key-1", Limit: 10}); err != nil {
					mu.Lock()
					readErrs = append(readErrs, err)
					mu.Unlock()
				}
				if _, err := h.svc.ListFiles(ctx, FileQuery{OwnerKeyID: "key-1", Limit: 10}); err != nil {
					mu.Lock()
					readErrs = append(readErrs, err)
					mu.Unlock()
				}
			}
		}(i)
	}

	if _, err := h.svc.Cancel(context.Background(), cancelled.ID, "key-1"); err != nil {
		t.Errorf("Cancel: %v", err)
	}

	for _, id := range ids {
		final := h.await(id, StatusCompleted)
		if final.RequestCounts.Total != perBatch {
			t.Errorf("%s total = %d, want %d", id, final.RequestCounts.Total, perBatch)
		}
		if final.RequestCounts.Completed+final.RequestCounts.Failed != perBatch {
			t.Errorf("%s counts = %+v do not add up to %d", id, final.RequestCounts, perBatch)
		}
		out := len(h.lines(final.OutputFileID))
		bad := 0
		if final.ErrorFileID != "" {
			bad = len(h.lines(final.ErrorFileID))
		}
		if out != final.RequestCounts.Completed || bad != final.RequestCounts.Failed {
			t.Errorf("%s: files hold %d/%d, counts say %+v", id, out, bad, final.RequestCounts)
		}
	}
	h.await(cancelled.ID, StatusCancelled)

	close(stop)
	wg.Wait()
	if len(readErrs) > 0 {
		t.Errorf("%d read errors during dispatch, first: %v", len(readErrs), readErrs[0])
	}
	if batch, plain := h.cap.counts(); plain != 0 || batch == 0 {
		t.Errorf("capacity saw %d batch and %d non-batch requests", batch, plain)
	}
}

func bucket(s string) uint32 {
	h := fnv.New32a()
	fmt.Fprint(h, s)
	return h.Sum32()
}
