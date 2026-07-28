package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// rowFate is what the fake executor does to a given row of the 1000-row batch.
type rowFate int

const (
	fateOK       rowFate = iota
	fatePermFail         // a 400 the upstream itself returned; never retried
	fateTerminal         // a routing error that says it is terminal
	fateFlaky            // 503 twice, then 200
	fateAlways503
)

func fateOf(i int) rowFate {
	switch {
	case i%10 == 3:
		return fatePermFail
	case i%50 == 7:
		return fateTerminal
	case i%25 == 11:
		return fateFlaky
	case i%100 == 17:
		return fateAlways503
	}
	return fateOK
}

// A thousand rows with injected partial failures. The batch is completed, not
// failed: rows failing is the normal case, and the only thing that makes a batch
// fail is the batch itself being unworkable.
func TestThousandRowBatchWithPartialFailures(t *testing.T) {
	const n = 1000
	h := newHarness(t, func(c *Config) {
		c.RowConcurrency = 16
		c.MaxAttempts = 3
	})

	h.exec.fn = func(ctx context.Context, req *ExecRequest, attempt int) (*ExecResult, error) {
		var i int
		if _, err := fmt.Sscanf(req.CustomID, "req-%d", &i); err != nil {
			t.Errorf("unexpected custom_id %q", req.CustomID)
			return okResult(req), nil
		}
		switch fateOf(i) {
		case fatePermFail:
			return errResult(400, "bad request for "+req.CustomID), nil
		case fateTerminal:
			return nil, &terminalError{msg: "no deployment for " + req.CustomID}
		case fateFlaky:
			if attempt < 3 {
				return errResult(503, "warming up"), nil
			}
			return okResult(req), nil
		case fateAlways503:
			return errResult(503, "always down"), nil
		}
		return okResult(req), nil
	}

	fid := h.upload(jsonlFile(n, "m1"))
	b := h.create(fid)
	final := h.await(b.ID, StatusCompleted, StatusFailed, StatusCancelled, StatusExpired)

	if final.Status != StatusCompleted {
		t.Fatalf("status = %s, want completed — partial failure is not batch failure", final.Status)
	}

	wantCompleted, wantFailed := 0, 0
	for i := 0; i < n; i++ {
		switch fateOf(i) {
		case fateOK, fateFlaky:
			wantCompleted++
		default:
			wantFailed++
		}
	}
	want := RequestCounts{Total: n, Completed: wantCompleted, Failed: wantFailed}
	if final.RequestCounts != want {
		t.Errorf("request_counts = %+v, want %+v", final.RequestCounts, want)
	}
	if final.OutputFileID == "" || final.ErrorFileID == "" {
		t.Fatalf("want both an output and an error file, got %q and %q", final.OutputFileID, final.ErrorFileID)
	}

	out := h.lines(final.OutputFileID)
	bad := h.lines(final.ErrorFileID)
	if len(out) != wantCompleted {
		t.Errorf("output file has %d lines, want %d", len(out), wantCompleted)
	}
	if len(bad) != wantFailed {
		t.Errorf("error file has %d lines, want %d", len(bad), wantFailed)
	}

	// Every row appears exactly once, in one file or the other.
	seen := make(map[string]int, n)
	for _, r := range out {
		seen[r.CustomID]++
		if r.Response == nil || r.Response.StatusCode != 200 {
			t.Errorf("%s in the output file without a 2xx response: %+v", r.CustomID, r.Response)
		}
		if r.Error != nil {
			t.Errorf("%s in the output file carries an error: %+v", r.CustomID, r.Error)
		}
		if r.ID == "" || !strings.HasPrefix(r.ID, "batch_req_") {
			t.Errorf("%s has row id %q", r.CustomID, r.ID)
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r.Response.Body, &body); err != nil || body.ID != "resp_"+r.CustomID {
			t.Errorf("%s carries the wrong body: %s", r.CustomID, r.Response.Body)
		}
	}
	for _, r := range bad {
		seen[r.CustomID]++
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("req-%d", i)
		if seen[id] != 1 {
			t.Fatalf("%s appears %d times across the result files, want 1", id, seen[id])
		}
	}

	// Output order is input order, which makes a diff against the input file
	// meaningful.
	prev := -1
	for _, r := range out {
		var i int
		fmt.Sscanf(r.CustomID, "req-%d", &i)
		if i <= prev {
			t.Fatalf("output file is not in input order: %d after %d", i, prev)
		}
		prev = i
	}

	// The two shapes of failure are distinguished: an upstream that answered
	// keeps its own status and body, an upstream that never answered gets an
	// error object.
	byID := make(map[string]OutputRow, len(bad))
	for _, r := range bad {
		byID[r.CustomID] = r
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("req-%d", i)
		row, present := byID[id]
		switch fateOf(i) {
		case fatePermFail:
			if !present || row.Response == nil || row.Response.StatusCode != 400 {
				t.Fatalf("%s: want a 400 response in the error file, got %+v", id, row)
			}
			if row.Error != nil {
				t.Errorf("%s: an upstream 400 should not be reported as a dorang error", id)
			}
			if h.exec.tries(id) != 1 {
				t.Errorf("%s: a 400 was retried %d times", id, h.exec.tries(id))
			}
		case fateTerminal:
			if !present || row.Error == nil || row.Response != nil {
				t.Fatalf("%s: want an error object and no response, got %+v", id, row)
			}
			if h.exec.tries(id) != 1 {
				t.Errorf("%s: a terminal error was retried %d times", id, h.exec.tries(id))
			}
		case fateAlways503:
			if !present || row.Response == nil || row.Response.StatusCode != 503 {
				t.Fatalf("%s: want a 503 response in the error file, got %+v", id, row)
			}
			if h.exec.tries(id) != 3 {
				t.Errorf("%s: tried %d times, want 3 (MaxAttempts)", id, h.exec.tries(id))
			}
		case fateFlaky:
			if present {
				t.Fatalf("%s recovered on retry and must not be in the error file", id)
			}
			if h.exec.tries(id) != 3 {
				t.Errorf("%s: tried %d times, want 3 (two 503s then a 200)", id, h.exec.tries(id))
			}
		}
	}

	// Every admission was marked as batch. Without that flag the interactive
	// reserve of DESIGN §11.1 applies to nothing at all.
	if batch, plain := h.cap.counts(); plain != 0 || batch == 0 {
		t.Errorf("capacity saw %d batch and %d non-batch requests, want all batch", batch, plain)
	}
}

// Rows sharing a prefix are grouped, and the group is computed over the request
// body rather than the JSONL line — the line begins with a unique custom_id, so
// hashing it would produce one group per row and no grouping at all.
func TestRowsGroupByPrefixHash(t *testing.T) {
	h := newHarness(t, nil)
	prefixA := strings.Repeat("system prompt alpha. ", 40)
	prefixB := strings.Repeat("system prompt beta. ", 40)

	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString(jsonlRow(fmt.Sprintf("a-%d", i), "m1", "/v1/chat/completions", prefixA+fmt.Sprintf(" q%d", i)))
		sb.WriteByte('\n')
	}
	for i := 0; i < 10; i++ {
		sb.WriteString(jsonlRow(fmt.Sprintf("b-%d", i), "m1", "/v1/chat/completions", prefixB+fmt.Sprintf(" q%d", i)))
		sb.WriteByte('\n')
	}
	for i := 0; i < 5; i++ {
		sb.WriteString(jsonlRow(fmt.Sprintf("u-%d", i), "m1", "/v1/chat/completions", fmt.Sprintf("unique %d", i)))
		sb.WriteByte('\n')
	}

	fid := h.upload(sb.String())
	b := h.create(fid)
	h.await(b.ID, StatusCompleted)

	hashes := map[string]string{}
	if err := h.store.EachRow(context.Background(), b.ID, func(r *RowRecord) error {
		hashes[r.CustomID] = r.PrefixHash
		return nil
	}); err != nil {
		t.Fatalf("EachRow: %v", err)
	}
	for i := 1; i < 10; i++ {
		if hashes[fmt.Sprintf("a-%d", i)] != hashes["a-0"] {
			t.Errorf("a-%d does not share a-0's group", i)
		}
		if hashes[fmt.Sprintf("b-%d", i)] != hashes["b-0"] {
			t.Errorf("b-%d does not share b-0's group", i)
		}
	}
	if hashes["a-0"] == hashes["b-0"] {
		t.Error("two different system prompts landed in the same group")
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("u-%d", i)
		if hashes[id] == hashes["a-0"] || hashes[id] == hashes["b-0"] {
			t.Errorf("%s should not share a group with a long shared prefix", id)
		}
	}
}

func TestGroupRowsChunking(t *testing.T) {
	rows := []*RowRecord{
		{CustomID: "1", PrefixHash: "a"},
		{CustomID: "2", PrefixHash: "b"},
		{CustomID: "3", PrefixHash: "a"},
		{CustomID: "4", PrefixHash: "a"},
		{CustomID: "5", PrefixHash: "a"},
		{CustomID: "6", PrefixHash: "b"},
	}
	got := groupRows(rows, 2)
	want := [][]string{{"1", "3"}, {"4", "5"}, {"2", "6"}}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d: %v", len(got), len(want), chunkIDs(got))
	}
	for i, chunk := range got {
		for j, row := range chunk {
			if row.CustomID != want[i][j] {
				t.Fatalf("chunks = %v, want %v", chunkIDs(got), want)
			}
		}
	}
}

func chunkIDs(chunks [][]*RowRecord) [][]string {
	out := make([][]string, 0, len(chunks))
	for _, c := range chunks {
		ids := make([]string, 0, len(c))
		for _, r := range c {
			ids = append(ids, r.CustomID)
		}
		out = append(out, ids)
	}
	return out
}

// A batch whose every row fails is still completed. The distinction is between
// the rows failing and the batch being unworkable, and nothing here is the
// latter.
func TestEveryRowFailingStillCompletes(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxAttempts = 1 })
	h.exec.fn = func(ctx context.Context, req *ExecRequest, attempt int) (*ExecResult, error) {
		return errResult(400, "no"), nil
	}
	fid := h.upload(jsonlFile(10, "m1"))
	b := h.create(fid)
	final := h.await(b.ID, StatusCompleted, StatusFailed)

	if final.Status != StatusCompleted {
		t.Errorf("status = %s, want completed", final.Status)
	}
	if final.RequestCounts != (RequestCounts{Total: 10, Failed: 10}) {
		t.Errorf("counts = %+v", final.RequestCounts)
	}
	if final.OutputFileID != "" {
		t.Errorf("no row succeeded, so there should be no output file, got %q", final.OutputFileID)
	}
	if final.ErrorFileID == "" {
		t.Error("want an error file")
	}
}

// A model that disappeared between upload and create is caught by the
// create-time validation pass, which is why that pass exists at all: the file
// was valid when it was uploaded.
func TestModelRemovedBetweenUploadAndCreate(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RowConcurrency = 1 })
	fid := h.upload(jsonlFile(6, "m1"))
	h.model.remove("m1")

	b := h.create(fid)
	final := h.await(b.ID, StatusCompleted, StatusFailed)
	// Validation runs again at create time and catches it there.
	if final.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", final.Status)
	}
	if final.Errors == nil || final.Errors.Data[0].Code != CodeModelNotFound {
		t.Fatalf("errors = %+v, want %s", final.Errors, CodeModelNotFound)
	}
}

func TestBatchListAndRetrieveScopedByOwner(t *testing.T) {
	h := newHarness(t, nil)
	fid := h.upload(jsonlFile(2, "m1"))
	b := h.create(fid)
	h.await(b.ID, StatusCompleted)

	if _, err := h.svc.Retrieve(context.Background(), b.ID, "someone-else"); err == nil {
		t.Error("another key retrieved the batch")
	}
	list, err := h.svc.List(context.Background(), BatchQuery{OwnerKeyID: "key-1", Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != b.ID {
		t.Fatalf("list = %+v", list.Data)
	}
	if list.Object != ObjectList || list.FirstID != b.ID {
		t.Errorf("list envelope = %+v", list)
	}
	empty, err := h.svc.List(context.Background(), BatchQuery{OwnerKeyID: "nobody", Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(empty.Data) != 0 {
		t.Errorf("another key listed %d batches", len(empty.Data))
	}
}

// The internal queued state is not in OpenAI's status enum, so it renders as
// validating unless an operator asks for the truth.
func TestQueuedStatusRendersAsValidating(t *testing.T) {
	if got := StatusQueued.Wire(); got != StatusValidating {
		t.Errorf("queued renders as %q, want %q", got, StatusValidating)
	}
	rec := &BatchRecord{ID: "b", Status: StatusQueued}
	if got := rec.API(false).Status; got != StatusValidating {
		t.Errorf("API(false) = %q, want %q", got, StatusValidating)
	}
	if got := rec.API(true).Status; got != StatusQueued {
		t.Errorf("API(true) = %q, want %q", got, StatusQueued)
	}
}

func TestExpiredBatchKeepsFinishedRows(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RowConcurrency = 1 })
	g := newGate()
	defer g.openAll()
	h.exec.fn = g.handler

	fid := h.upload(jsonlFile(20, "m1"))
	b := h.create(fid)

	// Let three rows through, then push the clock past the completion window.
	g.allow(3)
	g.waitFinished(t, 3)
	h.clock.advance(25 * time.Hour)
	h.svc.Sweep(context.Background())
	g.openAll()

	final := h.await(b.ID, StatusExpired, StatusCompleted)
	if final.Status != StatusExpired {
		t.Fatalf("status = %s, want expired", final.Status)
	}
	if final.RequestCounts.Completed == 0 {
		t.Fatal("an expired batch discarded every finished row")
	}
	if final.RequestCounts.Completed >= final.RequestCounts.Total {
		t.Fatalf("counts = %+v: the batch was not actually cut short", final.RequestCounts)
	}
	out := h.lines(final.OutputFileID)
	if len(out) != final.RequestCounts.Completed {
		t.Errorf("output file has %d lines, counts say %d", len(out), final.RequestCounts.Completed)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", within)
}
