package batch

import (
	"context"
	"errors"
	"testing"
)

// Cancelling a running batch must not throw away rows that already finished.
// The caller has already paid for those tokens, so losing them is data loss, not
// tidiness.
func TestCancelMidFlightPreservesCompletedRows(t *testing.T) {
	const rows = 100
	h := newHarness(t, func(c *Config) {
		c.RowConcurrency = 4
		c.CountFlush = 1
	})
	g := newGate()
	defer g.openAll()
	h.exec.fn = g.handler

	fid := h.upload(jsonlFile(rows, "m1"))
	b := h.create(fid)

	// Twenty rows through, and every worker now blocked inside the executor
	// holding a row in flight.
	g.allow(20)
	g.waitFinished(t, 20)
	g.waitStarted(t, 21)

	cancelling, err := h.svc.Cancel(context.Background(), b.ID, "key-1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelling.Status != StatusCancelling {
		t.Fatalf("status right after Cancel = %s, want cancelling — in-flight rows have to drain first",
			cancelling.Status)
	}
	if cancelling.CancellingAt == 0 {
		t.Error("cancelling_at not stamped")
	}

	// Let the in-flight rows answer. They must survive the cancel.
	g.openAll()
	final := h.await(b.ID, StatusCancelled)

	if final.CancelledAt == 0 {
		t.Error("cancelled_at not stamped")
	}
	if final.RequestCounts.Total != rows {
		t.Errorf("total = %d, want %d", final.RequestCounts.Total, rows)
	}
	if final.RequestCounts.Completed < 20 {
		t.Fatalf("completed = %d, want at least the 20 rows that had finished before the cancel",
			final.RequestCounts.Completed)
	}
	if final.RequestCounts.Completed+final.RequestCounts.Failed >= rows {
		t.Fatalf("counts = %+v: the cancel did not actually stop anything", final.RequestCounts)
	}
	if final.OutputFileID == "" {
		t.Fatal("a cancelled batch with completed rows has no output file")
	}

	out := h.lines(final.OutputFileID)
	if len(out) != final.RequestCounts.Completed {
		t.Errorf("output file has %d lines, request_counts says %d completed",
			len(out), final.RequestCounts.Completed)
	}
	got := make(map[string]bool, len(out))
	for _, r := range out {
		got[r.CustomID] = true
	}
	// Every row the executor answered — including the ones that were in flight
	// when the cancel arrived — is in the output file.
	for _, id := range g.finishedIDs() {
		if !got[id] {
			t.Fatalf("%s was executed and answered, but is missing from the output file", id)
		}
	}
}

// A batch that has not started running has nothing to drain, so the cancel is
// synchronous: the call itself returns cancelled and the very next read agrees.
func TestCancelWhileQueuedIsImmediate(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.MaxActiveBatches = 1
		c.RowConcurrency = 1
		c.EmitQueuedStatus = true // so the test can see the state it is testing
	})
	g := newGate()
	defer g.openAll()
	h.exec.fn = g.handler

	first := h.create(h.upload(jsonlFile(50, "m1")))
	g.waitStarted(t, 1) // the only dispatch slot is taken

	second := h.create(h.upload(jsonlFile(10, "m1")))
	h.await(second.ID, StatusQueued) // validated, waiting for a slot

	got, err := h.svc.Cancel(context.Background(), second.ID, "key-1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled immediately", got.Status)
	}
	// Immediately means the next read, with no polling.
	now, err := h.svc.Retrieve(context.Background(), second.ID, "key-1")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if now.Status != StatusCancelled {
		t.Fatalf("status on the next read = %s, want cancelled", now.Status)
	}
	if now.CancelledAt == 0 || now.CancellingAt == 0 {
		t.Errorf("timestamps not stamped: cancelling_at=%d cancelled_at=%d", now.CancellingAt, now.CancelledAt)
	}
	if now.OutputFileID != "" || now.ErrorFileID != "" {
		t.Errorf("a batch that never ran produced result files: %q %q", now.OutputFileID, now.ErrorFileID)
	}
	if n := h.exec.rowsOfBatch(second.ID); n != 0 {
		t.Errorf("the cancelled batch executed %d rows", n)
	}

	g.openAll()
	h.await(first.ID, StatusCompleted)
}

func TestCancelTerminalBatchIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	b := h.create(h.upload(jsonlFile(3, "m1")))
	h.await(b.ID, StatusCompleted)

	_, err := h.svc.Cancel(context.Background(), b.ID, "key-1")
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("Cancel on a completed batch = %v, want ErrInvalidState", err)
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RowConcurrency = 2 })
	g := newGate()
	defer g.openAll()
	h.exec.fn = g.handler

	b := h.create(h.upload(jsonlFile(20, "m1")))
	g.waitStarted(t, 1)

	if _, err := h.svc.Cancel(context.Background(), b.ID, "key-1"); err != nil {
		t.Fatalf("first Cancel: %v", err)
	}
	if _, err := h.svc.Cancel(context.Background(), b.ID, "key-1"); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	g.openAll()
	h.await(b.ID, StatusCancelled)
}

// Deleting the input file of a live batch would break it halfway through, so it
// is refused while the batch is running and allowed once it is not.
func TestInputFileOfRunningBatchCannotBeDeleted(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RowConcurrency = 1 })
	g := newGate()
	defer g.openAll()
	h.exec.fn = g.handler

	fid := h.upload(jsonlFile(20, "m1"))
	b := h.create(fid)
	g.waitStarted(t, 1)

	if _, err := h.svc.DeleteFile(context.Background(), fid, "key-1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("DeleteFile on a running batch's input = %v, want ErrInvalidState", err)
	}

	g.openAll()
	h.await(b.ID, StatusCompleted)

	if _, err := h.svc.DeleteFile(context.Background(), fid, "key-1"); err != nil {
		t.Fatalf("DeleteFile after the batch finished: %v", err)
	}
}
