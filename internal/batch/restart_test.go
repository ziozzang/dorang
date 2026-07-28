package batch

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A batch interrupted by a restart resumes from persisted state. The rows that
// finished are not run again — re-running them would charge the caller twice for
// work dorang already has the answer to.
func TestRestartResumesWithoutRerunningFinishedRows(t *testing.T) {
	const rows = 200
	h := newHarness(t, func(c *Config) { c.RowConcurrency = 4 })
	g := newGate()
	h.exec.fn = g.handler

	b := h.create(h.upload(jsonlFile(rows, "m1")))
	g.allow(30)
	g.waitFinished(t, 30)

	// Release only enough for the rows already inside the executor, so the
	// close drains cleanly without letting the batch run to completion.
	go g.allow(2 * 4)
	h.restart(nil)

	mid, err := h.svc.Retrieve(context.Background(), b.ID, "")
	if err != nil {
		t.Fatalf("Retrieve after restart: %v", err)
	}
	if mid.Status.Terminal() {
		t.Fatalf("the batch finished before the restart (%s); this test would prove nothing", mid.Status)
	}
	if mid.RequestCounts.Completed < 30 {
		t.Fatalf("only %d rows were recorded before the restart, want at least 30", mid.RequestCounts.Completed)
	}
	before := mid.RequestCounts.Completed

	g.openAll()
	n, err := h.svc.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recover resumed %d batches, want 1", n)
	}

	final := h.await(b.ID, StatusCompleted)
	if final.RequestCounts != (RequestCounts{Total: rows, Completed: rows}) {
		t.Fatalf("counts = %+v, want %d completed", final.RequestCounts, rows)
	}
	if len(h.lines(final.OutputFileID)) != rows {
		t.Errorf("output file has %d lines, want %d", len(h.lines(final.OutputFileID)), rows)
	}

	// The point of the test: exactly-once across a graceful restart.
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("req-%d", i)
		if got := h.exec.tries(id); got != 1 {
			t.Fatalf("%s executed %d times across the restart, want 1 (recorded before restart: %d)",
				id, got, before)
		}
	}
}

// A cancel accepted just before the process ended is honoured by the next one:
// recovery drains a cancelling batch to cancelled and writes out the rows that
// had finished, rather than resuming work the caller has already stopped paying
// for.
//
// The state is built directly in the store because that is exactly what recovery
// sees — a batch left cancelling by a process that is no longer running.
func TestRecoverHonoursAnAcceptedCancel(t *testing.T) {
	h := newHarness(t, nil)
	content := jsonlFile(5, "m1")
	fid := h.upload(content)

	id := "batch_halfcancelled"
	var rows []*RowRecord
	ve, err := validateInput(strings.NewReader(content), h.svc.validateConfig("/v1/chat/completions"),
		func(r *RowRecord) {
			r.BatchID = id
			rows = append(rows, r)
		})
	if err != nil || ve != nil {
		t.Fatalf("validateInput: %v %v", err, ve)
	}
	// Two rows finished before the process ended.
	for i := 0; i < 2; i++ {
		rows[i].Status = RowCompleted
		rows[i].StatusCode = 200
		rows[i].RequestID = "req_pre"
		rows[i].Result = []byte(`{"id":"pre"}`)
	}
	if err := h.store.PutRows(context.Background(), rows); err != nil {
		t.Fatalf("PutRows: %v", err)
	}
	rec := &BatchRecord{
		ID: id, OwnerKeyID: "key-1", PrincipalID: "key-1",
		Endpoint: "/v1/chat/completions", InputFileID: fid,
		Status: StatusCancelling, Counts: RequestCounts{Total: 5},
		CreatedAt: h.clock.Now(), UpdatedAt: h.clock.Now(), CancellingAt: h.clock.Now(),
	}
	if err := h.store.CreateBatch(context.Background(), rec); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	n, err := h.svc.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recover resumed %d batches, want 1", n)
	}

	final := h.await(id, StatusCancelled)
	if final.RequestCounts != (RequestCounts{Total: 5, Completed: 2}) {
		t.Fatalf("counts = %+v, want the two rows that had finished", final.RequestCounts)
	}
	if final.OutputFileID == "" {
		t.Fatal("the rows that finished before the cancel were not written out")
	}
	if got := len(h.lines(final.OutputFileID)); got != 2 {
		t.Errorf("output file has %d lines, want 2", got)
	}
	if n := h.exec.rowsOfBatch(id); n != 0 {
		t.Errorf("recovery executed %d rows of a cancelling batch, want 0", n)
	}
}

// Shutting down must never turn an untouched batch into a completed one. A
// batch waiting for a dispatch slot when the process ends is still waiting when
// it comes back.
func TestCloseLeavesAWaitingBatchForTheNextProcess(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.MaxActiveBatches = 1
		c.RowConcurrency = 1
		c.EmitQueuedStatus = true
	})
	g := newGate()
	h.exec.fn = g.handler

	first := h.create(h.upload(jsonlFile(30, "m1")))
	g.waitStarted(t, 1)
	second := h.create(h.upload(jsonlFile(8, "m1")))
	h.await(second.ID, StatusQueued)

	go g.allow(4)
	h.restart(nil)

	waiting, err := h.svc.Retrieve(context.Background(), second.ID, "")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if waiting.Status != StatusQueued {
		t.Fatalf("a batch that never dispatched a row is %s after shutdown, want queued", waiting.Status)
	}
	if waiting.OutputFileID != "" {
		t.Error("a batch that never ran produced an output file")
	}

	g.openAll()
	if _, err := h.svc.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	final := h.await(second.ID, StatusCompleted)
	if final.RequestCounts != (RequestCounts{Total: 8, Completed: 8}) {
		t.Errorf("counts after recovery = %+v", final.RequestCounts)
	}
	h.await(first.ID, StatusCompleted)
}

// Recovery of a batch interrupted during validation re-validates rather than
// failing it, and does not reset rows that already ran.
func TestRecoverRevalidatesAnInterruptedBatch(t *testing.T) {
	h := newHarness(t, nil)
	fid := h.upload(jsonlFile(5, "m1"))

	// A batch left in validating by a process that died mid-prepare.
	rec := &BatchRecord{
		ID: "batch_interrupted", OwnerKeyID: "key-1", PrincipalID: "key-1",
		Endpoint: "/v1/chat/completions", InputFileID: fid,
		Status: StatusValidating, CreatedAt: h.clock.Now(), UpdatedAt: h.clock.Now(),
	}
	if err := h.store.CreateBatch(context.Background(), rec); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	n, err := h.svc.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recover resumed %d batches, want 1", n)
	}
	final := h.await(rec.ID, StatusCompleted)
	if final.RequestCounts != (RequestCounts{Total: 5, Completed: 5}) {
		t.Errorf("counts = %+v", final.RequestCounts)
	}
}
