package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/batch"
	"github.com/ziozzang/dorang/internal/store"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// countingExecutor answers every row and counts how many times each custom id
// was executed. That count is the whole point of the restart test: a row
// executed twice was paid for twice.
type countingExecutor struct {
	mu    sync.Mutex
	tries map[string]int
	// hold, when non-nil, blocks a row until the test releases it, so a batch
	// can be caught mid-flight rather than raced against.
	hold chan struct{}
	// stop is closed to release everything still waiting.
	stop chan struct{}
}

func newCountingExecutor() *countingExecutor {
	return &countingExecutor{tries: map[string]int{}, stop: make(chan struct{})}
}

func (e *countingExecutor) Execute(ctx context.Context, req *batch.ExecRequest) (*batch.ExecResult, error) {
	e.mu.Lock()
	e.tries[req.CustomID]++
	hold := e.hold
	e.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-e.stop:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	body, _ := json.Marshal(map[string]any{"id": "resp-" + req.CustomID, "model": req.Model})
	return &batch.ExecResult{StatusCode: 200, Body: body, RequestID: "req-" + req.CustomID}, nil
}

func (e *countingExecutor) count(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tries[id]
}

func (e *countingExecutor) done() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.tries)
}

// unlimitedReserver admits everything. Capacity is not what this test is about;
// it is about what survives the process.
type unlimitedReserver struct{}

func (unlimitedReserver) Acquire(context.Context, batch.CapacityRequest) (batch.Reservation, error) {
	return nopReservation{}, nil
}

type nopReservation struct{}

func (nopReservation) CredentialID() string { return "cred-test" }
func (nopReservation) Release()             {}

// oneModelResolver resolves the single model these rows name.
type oneModelResolver struct{}

func (oneModelResolver) ResolveModel(name string) (batch.Target, bool) {
	if name != "m1" {
		return batch.Target{}, false
	}
	return batch.Target{Provider: "p1", UpstreamModel: "up-1"}, true
}

// jsonlRows renders n valid input rows.
func jsonlRows(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"custom_id":"req-%d","method":"POST","url":"/v1/chat/completions",`+
			`"body":{"model":"m1","messages":[{"role":"user","content":"ping %d"}]}}`+"\n", i, i)
	}
	return b.String()
}

// batchRig is one batch service over a persistent store, rebuildable the way a
// restart rebuilds it.
type batchRig struct {
	t     *testing.T
	st    *store.Store
	blobs *batch.MemBlobs
	exec  *countingExecutor
	svc   *batch.Service
}

func newBatchRig(t *testing.T, st *store.Store, blobs *batch.MemBlobs, exec *countingExecutor) *batchRig {
	t.Helper()
	svc, err := batch.New(batch.Config{
		Store:          &batchStore{st: st},
		Blobs:          blobs,
		Executor:       exec,
		Capacity:       unlimitedReserver{},
		Models:         oneModelResolver{},
		RowConcurrency: 4,
		SweepInterval:  -1,
		// One progress write per finished row. The default batches them
		// thirty-two at a time, which is right in production and useless to a
		// test that has to catch a batch mid-flight.
		CountFlush:       1,
		EmitQueuedStatus: true,
		Logf:             t.Logf,
	})
	if err != nil {
		t.Fatalf("batch.New: %v", err)
	}
	return &batchRig{t: t, st: st, blobs: blobs, exec: exec, svc: svc}
}

func (r *batchRig) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.svc.Close(ctx); err != nil {
		r.t.Fatalf("batch Close: %v", err)
	}
}

func (r *batchRig) await(id string, want ...batch.Status) *batch.Batch {
	r.t.Helper()
	var got *batch.Batch
	waitFor(r.t, "batch "+id+" to reach "+fmt.Sprint(want), func() bool {
		b, err := r.svc.Retrieve(context.Background(), id, "")
		if err != nil {
			return false
		}
		for _, w := range want {
			if b.Status == w {
				got = b
				return true
			}
		}
		return false
	})
	return got
}

// ---------------------------------------------------------------------------
// the test
// ---------------------------------------------------------------------------

// A batch interrupted mid-flight resumes from the database, and the rows that
// already finished are not run again.
//
// Re-running them is not a slow resume, it is a second bill: every finished row
// has already been sent upstream and paid for, and DESIGN §9.2 gives
// batch_requests a response_body column precisely so a resumed batch can rebuild
// its output file from the answers it already has rather than asking for them
// twice. With only an in-memory Store the whole batch vanished at exit, and
// Recover — which is wired — found nothing to recover.
func TestBatchSurvivesARestartWithoutRerunningFinishedRows(t *testing.T) {
	const rows = 40
	ctx := context.Background()

	st, dsn := openTestStore(t, nil)
	blobs := batch.NewMemBlobs()
	exec := newCountingExecutor()
	// Hold every row inside the executor so the batch can be stopped while it
	// is genuinely mid-flight.
	exec.hold = make(chan struct{})

	rig := newBatchRig(t, st, blobs, exec)

	file, err := rig.svc.UploadFile(ctx, batch.UploadRequest{
		Filename:   "in.jsonl",
		Purpose:    batch.PurposeBatch,
		OwnerKeyID: "key-1",
		Content:    strings.NewReader(jsonlRows(rows)),
		Authorize:  func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	created, err := rig.svc.Create(ctx, batch.CreateRequest{
		InputFileID: file.ID,
		Endpoint:    "/v1/chat/completions",
		OwnerKeyID:  "key-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Let some rows through, then stop the process with the rest still queued.
	const before = 12
	for i := 0; i < before; i++ {
		exec.hold <- struct{}{}
	}
	waitFor(t, "the first rows to be recorded", func() bool {
		b, err := rig.svc.Retrieve(ctx, created.ID, "")
		return err == nil && b.RequestCounts.Completed >= before-4
	})

	// Release the workers currently blocked inside the executor so the close
	// drains, then end the process.
	go func() { close(exec.stop) }()
	rig.close()

	mid, err := rig.svc.Retrieve(ctx, created.ID, "")
	if err != nil {
		t.Fatalf("Retrieve before restart: %v", err)
	}
	if mid.Status.Terminal() {
		t.Fatalf("the batch finished before the restart (%s); this test would prove nothing",
			mid.Status)
	}
	completedBefore := mid.RequestCounts.Completed
	if completedBefore == 0 {
		t.Fatal("no row finished before the restart; there is nothing for recovery to skip")
	}
	if completedBefore == rows {
		t.Fatal("every row finished before the restart; this test would prove nothing")
	}

	// The restart. A NEW store handle over the SAME file, exactly as a second
	// process would open it — nothing in memory carries over.
	if err := st.Close(); err != nil {
		t.Fatalf("store Close: %v", err)
	}
	st2 := openTestStoreAt(t, dsn, nil)

	exec2 := newCountingExecutor()
	rig2 := newBatchRig(t, st2, blobs, exec2)
	defer rig2.close()

	n, err := rig2.svc.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recover resumed %d batches, want 1 — the batch did not survive the restart", n)
	}

	final := rig2.await(created.ID, batch.StatusCompleted)
	if final.RequestCounts.Total != rows || final.RequestCounts.Completed != rows {
		t.Fatalf("counts after recovery = %+v, want %d completed of %d",
			final.RequestCounts, rows, rows)
	}

	// The point of the test: the second process executed only what was left.
	if got := exec2.done(); got != rows-completedBefore {
		t.Errorf("the resumed process executed %d rows, want %d (%d had already finished) — "+
			"re-running a finished row means paying for it twice",
			got, rows-completedBefore, completedBefore)
	}
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("req-%d", i)
		if total := exec.count(id) + exec2.count(id); total != 1 {
			t.Fatalf("%s executed %d times across the restart, want exactly 1", id, total)
		}
	}

	// And the output file is complete, which it can only be if the rows that
	// finished BEFORE the restart kept their response bodies.
	rc, rec, err := rig2.svc.FileContent(ctx, final.OutputFileID, "")
	if err != nil {
		t.Fatalf("FileContent: %v", err)
	}
	defer rc.Close()
	buf := make([]byte, rec.Bytes)
	if _, err := rc.Read(buf); err != nil && rec.Bytes > 0 {
		t.Fatalf("read output file: %v", err)
	}
	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(string(buf)), "\n") {
		if l == "" {
			continue
		}
		lines++
		var row struct {
			CustomID string `json:"custom_id"`
			Response *struct {
				StatusCode int             `json:"status_code"`
				Body       json.RawMessage `json:"body"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(l), &row); err != nil {
			t.Fatalf("output line is not JSON: %v\n%s", err, l)
		}
		if row.Response == nil || row.Response.StatusCode != 200 || len(row.Response.Body) == 0 {
			t.Fatalf("output row %s has no response body; a row finished before the "+
				"restart lost its answer and would have to be re-run", row.CustomID)
		}
	}
	if lines != rows {
		t.Errorf("output file has %d lines, want %d", lines, rows)
	}
}

// The store round-trips every field of a batch, its rows and its files. A field
// dropped here is a field that silently resets on the next restart.
func TestBatchStoreRoundTripsEveryField(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, nil)
	bs := &batchStore{st: st}

	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := &batch.BatchRecord{
		ID:               "batch_rt",
		OwnerKeyID:       "key-1",
		PrincipalID:      "team-9",
		Endpoint:         "/v1/chat/completions",
		InputFileID:      "file-in",
		OutputFileID:     "file-out",
		ErrorFileID:      "file-err",
		CompletionWindow: 24 * time.Hour,
		Status:           batch.StatusFailed,
		Counts:           batch.RequestCounts{Total: 9, Completed: 5, Failed: 4},
		Errors: []batch.BatchError{
			{Code: "invalid_json", Message: "line 3 is not JSON", Line: 3},
			{Code: "model_not_found", Message: "no deployment serves m9", Param: "body.model"},
		},
		Metadata:     map[string]string{"team": "research", "run": "17"},
		CreatedAt:    now,
		UpdatedAt:    now.Add(time.Second),
		InProgressAt: now.Add(2 * time.Second),
		FinalizingAt: now.Add(3 * time.Second),
		CompletedAt:  now.Add(4 * time.Second),
		FailedAt:     now.Add(5 * time.Second),
		CancellingAt: now.Add(6 * time.Second),
		CancelledAt:  now.Add(7 * time.Second),
		ExpiredAt:    now.Add(8 * time.Second),
		ExpiresAt:    now.Add(9 * time.Second),
	}
	if err := bs.CreateBatch(ctx, rec); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if err := bs.CreateBatch(ctx, rec); err == nil {
		t.Error("CreateBatch accepted a duplicate id")
	}

	got, err := bs.GetBatch(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint(rec) {
		t.Errorf("batch did not round-trip\n got %+v\nwant %+v", got, rec)
	}

	row := &batch.RowRecord{
		BatchID:    rec.ID,
		CustomID:   "req-0",
		Seq:        0,
		PrefixHash: "abc123",
		Model:      "m1",
		URL:        "/v1/chat/completions",
		Offset:     4096,
		Length:     512,
		Status:     batch.RowCompleted,
		Attempts:   2,
		StatusCode: 200,
		RequestID:  "dorang-req-1",
		Result:     []byte(`{"id":"resp"}`),
		ErrCode:    "",
		ErrMessage: "",
		CreatedAt:  now,
		UpdatedAt:  now.Add(time.Second),
	}
	if err := bs.PutRows(ctx, []*batch.RowRecord{row}); err != nil {
		t.Fatalf("PutRows: %v", err)
	}

	// PutRows is idempotent by (batch, custom id): a second call with a RESET
	// row must not overwrite the recorded result, or a crash during validation
	// costs every finished row a second execution.
	reset := row.Clone()
	reset.Status = batch.RowQueued
	reset.StatusCode = 0
	reset.Result = nil
	if err := bs.PutRows(ctx, []*batch.RowRecord{reset}); err != nil {
		t.Fatalf("PutRows (repeat): %v", err)
	}

	var seen []*batch.RowRecord
	if err := bs.EachRow(ctx, rec.ID, func(r *batch.RowRecord) error {
		seen = append(seen, r)
		return nil
	}); err != nil {
		t.Fatalf("EachRow: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("EachRow saw %d rows, want 1", len(seen))
	}
	if fmt.Sprint(seen[0]) != fmt.Sprint(row) {
		t.Errorf("row did not round-trip\n got %+v\nwant %+v", seen[0], row)
	}

	// Only non-terminal batches come back from recovery.
	active, err := bs.ActiveBatches(ctx)
	if err != nil {
		t.Fatalf("ActiveBatches: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("a failed batch is still active: %+v", active)
	}
	rec.Status = batch.StatusInProgress
	if err := bs.SaveBatch(ctx, rec); err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}
	active, err = bs.ActiveBatches(ctx)
	if err != nil {
		t.Fatalf("ActiveBatches: %v", err)
	}
	if len(active) != 1 || active[0].ID != rec.ID {
		t.Errorf("ActiveBatches = %+v, want the in-progress batch", active)
	}

	frec := &batch.FileRecord{
		ID:            "file-in",
		OwnerKeyID:    "key-1",
		Purpose:       batch.PurposeBatch,
		Filename:      "in.jsonl",
		Bytes:         1234,
		SHA256:        "deadbeef",
		StorageRef:    "blob-1",
		Status:        batch.FileProcessed,
		StatusDetails: "validated",
		CreatedAt:     now,
		ExpiresAt:     now.Add(time.Hour),
	}
	if err := bs.CreateFile(ctx, frec); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	gotFile, err := bs.GetFile(ctx, frec.ID)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if fmt.Sprint(gotFile) != fmt.Sprint(frec) {
		t.Errorf("file did not round-trip\n got %+v\nwant %+v", gotFile, frec)
	}
	if err := bs.DeleteFile(ctx, frec.ID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := bs.GetFile(ctx, frec.ID); err == nil {
		t.Error("GetFile found a deleted file")
	}
}
