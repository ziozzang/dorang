package scenario

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/batch"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/testing/fake"
)

// DESIGN §14 scenario 14 — a batch of 1000 with partial failures, and
// interactive latency on the CONTENDED MODEL AXIS within bound.
//
// The second half is the part that is easy to fake into passing. §11.1's
// correction was that reserving only on the credential axis lets batch take a
// model's entire limit while staying under its credential share — so the
// contention has to be created on the model axis, and the interactive request
// has to be measured while it is real. This runs against the actual
// internal/capacity broker rather than a stand-in, because the reserve is that
// package's behaviour and a stand-in would be asserting the test's own
// arithmetic.

const (
	batchProvider = "self-hosted"
	batchModel    = "qwen3.5:397b"
	batchGroup    = "chat-large"
)

// brokerReserver adapts internal/capacity to what a batch row needs.
type brokerReserver struct {
	b *capacity.Broker
	// waits records how long each acquisition blocked, batch and interactive
	// alike, so the scenario can compare them.
	mu             sync.Mutex
	interactive    []time.Duration
	interactiveErr int
}

func (r *brokerReserver) Acquire(ctx context.Context, req batch.CapacityRequest) (batch.Reservation, error) {
	start := time.Now()
	res, err := r.b.Acquire(ctx, capacity.Request{
		Provider:      req.Provider,
		Model:         req.UpstreamModel,
		ProviderGroup: req.ProviderGroup,
		PrincipalID:   req.PrincipalID,
		Candidates:    []capacity.Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
		Batch:         req.Batch,
	})
	if err != nil {
		return nil, err
	}
	if !req.Batch {
		r.record(time.Since(start), false)
	}
	return res, nil
}

// interactiveAcquire is the front door an ordinary request comes through. It is
// the same broker and the same axes; only Batch differs.
func (r *brokerReserver) interactiveAcquire(ctx context.Context) (*capacity.Reservation, error) {
	start := time.Now()
	res, err := r.b.Acquire(ctx, capacity.Request{
		Provider:   batchProvider,
		Model:      batchModel,
		Candidates: []capacity.Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
	})
	r.record(time.Since(start), err != nil)
	return res, err
}

func (r *brokerReserver) record(d time.Duration, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interactive = append(r.interactive, d)
	if failed {
		r.interactiveErr++
	}
}

func (r *brokerReserver) latencies() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]time.Duration(nil), r.interactive...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// oneModel resolves every name to the single deployment this scenario runs.
type oneModel struct{}

func (oneModel) ResolveModel(name string) (batch.Target, bool) {
	// The name is opaque and is compared whole (DESIGN §2.1).
	if name != batchGroup {
		return batch.Target{}, false
	}
	return batch.Target{Provider: batchProvider, UpstreamModel: batchModel}, true
}

// httpExecutor runs one row as a real HTTP request against a fake upstream, so
// the batch path exercises the same wire the interactive path does.
type httpExecutor struct {
	url    string
	client *http.Client
	calls  atomic.Int64
}

func (e *httpExecutor) Execute(ctx context.Context, req *batch.ExecRequest) (*batch.ExecResult, error) {
	e.calls.Add(1)
	// Upstream receives the REAL model id (§7.2), which for a batch row is what
	// the resolver produced rather than what the row's body said.
	body, err := replaceModel(req.Body, req.UpstreamModel)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-batch-custom-id", req.CustomID)
	resp, err := e.client.Do(httpReq)
	if err != nil {
		// A returned error means no HTTP answer was produced at all.
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// A non-2xx answer is NOT an error: the error file must carry the
	// upstream's own envelope, not dorang's paraphrase of it.
	return &batch.ExecResult{
		StatusCode: resp.StatusCode,
		Body:       out,
		RequestID:  "req_" + req.CustomID,
	}, nil
}

func replaceModel(body json.RawMessage, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	m, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = m
	return json.Marshal(obj)
}

// fate decides what happens to row i. The mix is deliberate: a permanent
// failure, a transient one that recovers on retry, and one that never does.
type fate int

const (
	fateOK fate = iota
	fatePermanent
	fateFlaky
	fateAlwaysDown
)

func fateOf(i int) fate {
	switch {
	case i%97 == 3:
		return fatePermanent
	case i%89 == 5:
		return fateFlaky
	case i%101 == 7:
		return fateAlwaysDown
	}
	return fateOK
}

func batchJSONL(n int) string {
	var b strings.Builder
	for i := range n {
		body, _ := json.Marshal(map[string]any{
			"model":    batchGroup,
			"messages": []map[string]string{{"role": "user", "content": fmt.Sprintf("question %d", i)}},
		})
		line, _ := json.Marshal(map[string]any{
			"custom_id": fmt.Sprintf("req-%d", i),
			"method":    "POST",
			"url":       "/v1/chat/completions",
			"body":      json.RawMessage(body),
		})
		b.WriteString(string(line))
		b.WriteByte('\n')
	}
	return b.String()
}

// batchUpstream answers according to the row's fate, read from the header the
// executor set. Attempt counting lives here so a flaky row recovers on retry.
func batchUpstream(t *testing.T) *fake.Upstream {
	var mu sync.Mutex
	attempts := map[string]int{}
	u := fake.New(fake.Options{
		Shape: fake.ShapeOpenAI,
		Script: func(r *fake.Recorded) fake.Script {
			return fake.Script{Model: r.Model, Text: "ok", Usage: fake.Usage{InputTokens: 8, OutputTokens: 2}}
		},
		Behaviour: func(r *fake.Recorded) fake.Behaviour {
			id := r.Header.Get("x-batch-custom-id")
			var i int
			if _, err := fmt.Sscanf(id, "req-%d", &i); err != nil {
				return fake.Behaviour{}
			}
			mu.Lock()
			attempts[id]++
			n := attempts[id]
			mu.Unlock()
			// Every row costs real time upstream. Without it the batch
			// finishes before the model axis is ever contended, and the
			// latency half of the scenario measures an idle broker.
			const rowLatency = 2 * time.Millisecond
			switch fateOf(i) {
			case fatePermanent:
				return fake.Behaviour{Latency: rowLatency, Status: 400, ErrorMessage: "bad request for " + id}
			case fateFlaky:
				if n < 3 {
					return fake.Behaviour{Latency: rowLatency, Status: 503, ErrorMessage: "warming up"}
				}
				return fake.Behaviour{Latency: rowLatency}
			case fateAlwaysDown:
				return fake.Behaviour{Latency: rowLatency, Status: 503, ErrorMessage: "always down"}
			}
			return fake.Behaviour{Latency: rowLatency}
		},
	})
	t.Cleanup(u.Close)
	return u
}

func TestScenario14_ThousandRowBatchWithPartialFailuresAndInteractiveLatency(t *testing.T) {
	const (
		rows      = 1000
		axisLimit = 8
		reserve   = 0.25 // batch may occupy at most floor(8 * 0.75) = 6
	)
	ctx := context.Background()

	up := batchUpstream(t)
	broker := capacity.New(capacity.Config{
		Models:             []capacity.ModelLimit{{Provider: batchProvider, Model: batchModel, Max: axisLimit}},
		InteractiveReserve: reserve,
		SweepInterval:      -1,
	})
	t.Cleanup(broker.Close)

	res := &brokerReserver{b: broker}
	exec := &httpExecutor{url: up.Endpoint(), client: &http.Client{}}
	svc, err := batch.New(batch.Config{
		Store: batch.NewMemStore(), Blobs: batch.NewMemBlobs(),
		Executor: exec, Capacity: res, Models: oneModel{},
		RowConcurrency: 16, MaxAttempts: 3,
		BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond,
		SweepInterval: -1,
	})
	if err != nil {
		t.Fatalf("batch.New: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	file, err := svc.UploadFile(ctx, batch.UploadRequest{
		Filename: "in.jsonl", Purpose: batch.PurposeBatch, OwnerKeyID: "key-1",
		Content: strings.NewReader(batchJSONL(rows)), Authorize: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	b, err := svc.Create(ctx, batch.CreateRequest{
		InputFileID: file.ID, Endpoint: "/v1/chat/completions",
		OwnerKeyID: "key-1", PrincipalID: "team-a",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// While the batch runs, interactive traffic keeps arriving on the SAME
	// (provider, model) axis. This is the contention §11.1 is about, and the
	// interactive reserve is what is supposed to make it survivable.
	stop := make(chan struct{})
	var probeWG sync.WaitGroup
	probeWG.Add(1)
	go func() {
		defer probeWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			r, err := res.interactiveAcquire(c)
			cancel()
			if err == nil {
				// Hold it briefly, as a real request would.
				time.Sleep(200 * time.Microsecond)
				r.Release()
			}
			time.Sleep(500 * time.Microsecond)
		}
	}()

	final := awaitBatch(t, svc, b.ID)
	close(stop)
	probeWG.Wait()

	if final.Status != batch.StatusCompleted {
		t.Fatalf("status = %s; partial failure is not batch failure", final.Status)
	}

	// ---- the output and error files ----------------------------------------
	wantCompleted, wantFailed := 0, 0
	for i := range rows {
		if f := fateOf(i); f == fateOK || f == fateFlaky {
			wantCompleted++
		} else {
			wantFailed++
		}
	}
	if final.RequestCounts.Total != rows {
		t.Fatalf("total = %d, want %d", final.RequestCounts.Total, rows)
	}
	if final.RequestCounts.Completed != wantCompleted || final.RequestCounts.Failed != wantFailed {
		t.Fatalf("counts = %d/%d completed/failed, want %d/%d",
			final.RequestCounts.Completed, final.RequestCounts.Failed, wantCompleted, wantFailed)
	}

	okLines := readJSONL(t, svc, final.OutputFileID)
	errLines := readJSONL(t, svc, final.ErrorFileID)
	if len(okLines) != wantCompleted {
		t.Errorf("output file has %d lines, want %d", len(okLines), wantCompleted)
	}
	if len(errLines) != wantFailed {
		t.Errorf("error file has %d lines, want %d", len(errLines), wantFailed)
	}

	seen := map[string]bool{}
	for _, l := range okLines {
		if l.CustomID == "" {
			t.Fatalf("an output line has no custom_id: %+v", l)
		}
		if seen[l.CustomID] {
			t.Fatalf("%s appears twice in the output file", l.CustomID)
		}
		seen[l.CustomID] = true
		if l.Response == nil || l.Response.StatusCode != 200 {
			t.Fatalf("%s is in the output file with response %+v", l.CustomID, l.Response)
		}
		if l.Error != nil {
			t.Fatalf("%s carries both a response and an error", l.CustomID)
		}
	}
	for _, l := range errLines {
		if seen[l.CustomID] {
			t.Fatalf("%s is in BOTH files", l.CustomID)
		}
		if l.Response == nil {
			t.Fatalf("%s has no response; every failure here got an HTTP answer: %+v", l.CustomID, l)
		}
		if l.Response.StatusCode < 400 {
			t.Fatalf("%s is in the error file with status %d", l.CustomID, l.Response.StatusCode)
		}
		// The upstream's own envelope, unaltered.
		if !bytes.Contains(l.Response.Body, []byte(`"error"`)) {
			t.Fatalf("%s does not carry the upstream error envelope: %s", l.CustomID, l.Response.Body)
		}
	}

	// A flaky row must have been retried and then succeeded, or "partial
	// failures" would just mean "some rows are permanently broken".
	if exec.calls.Load() <= int64(rows) {
		t.Errorf("the executor was called %d times for %d rows: nothing was retried",
			exec.calls.Load(), rows)
	}

	// ---- interactive latency on the contended model axis --------------------
	lat := res.latencies()
	if len(lat) < 50 {
		t.Fatalf("only %d interactive probes ran; the measurement is not meaningful", len(lat))
	}
	if res.interactiveErr != 0 {
		t.Errorf("%d interactive acquisitions failed while the batch ran", res.interactiveErr)
	}
	p50 := lat[len(lat)/2]
	p99 := lat[(len(lat)*99)/100]
	// The bound: an interactive request must not be made to wait on batch work.
	// It is generous against a wall clock under -race; what it rules out is an
	// interactive request queueing behind a saturated batch, which shows up as
	// tens or hundreds of milliseconds, not microseconds.
	const bound = 100 * time.Millisecond
	t.Logf("interactive acquisition on the contended model axis: p50 %v, p99 %v, max %v (%d probes)",
		p50, p99, lat[len(lat)-1], len(lat))
	if p99 > bound {
		t.Errorf("interactive p99 = %v, want under %v: batch work is crowding out interactive traffic",
			p99, bound)
	}

	t.Run("inverse: with no interactive reserve, batch does crowd it out", func(t *testing.T) {
		// The same axis, the same limit, the same saturation — only the reserve
		// removed. If the interactive request were fast here too, the bound
		// above would be measuring an uncontended broker rather than the
		// reserve.
		broker := capacity.New(capacity.Config{
			Models:        []capacity.ModelLimit{{Provider: batchProvider, Model: batchModel, Max: axisLimit}},
			SweepInterval: -1,
		})
		t.Cleanup(broker.Close)
		batchReq := capacity.Request{
			Provider: batchProvider, Model: batchModel, Batch: true,
			Candidates: []capacity.Candidate{{ID: "acct-1"}},
		}
		interactive := capacity.Request{
			Provider: batchProvider, Model: batchModel,
			Candidates: []capacity.Candidate{{ID: "acct-1"}},
		}
		var held []*capacity.Reservation
		for range axisLimit {
			r, ok := broker.TryAcquire(batchReq)
			if !ok {
				t.Fatalf("batch could not fill the axis with no reserve configured")
			}
			held = append(held, r)
		}
		if !blocks(broker, interactive) {
			t.Fatal("with no reserve, batch filling the whole axis must block interactive traffic")
		}
		for _, r := range held {
			r.Release()
		}

		// And with the reserve, batch cannot reach the ceiling at all.
		reserved := capacity.New(capacity.Config{
			Models:             []capacity.ModelLimit{{Provider: batchProvider, Model: batchModel, Max: axisLimit}},
			InteractiveReserve: reserve,
			SweepInterval:      -1,
		})
		t.Cleanup(reserved.Close)
		n := 0
		for {
			r, ok := reserved.TryAcquire(batchReq)
			if !ok {
				break
			}
			held = append(held, r)
			n++
			if n > axisLimit {
				t.Fatal("batch exceeded the axis limit")
			}
		}
		want := int(float64(axisLimit) * (1 - reserve))
		if n != want {
			t.Fatalf("batch took %d of %d slots, want floor(%d*(1-%v)) = %d",
				n, axisLimit, axisLimit, reserve, want)
		}
		if blocks(reserved, interactive) {
			t.Fatal("interactive traffic was blocked despite the reserve")
		}
	})
}

func awaitBatch(t *testing.T, svc *batch.Service, id string) *batch.Batch {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		b, err := svc.Retrieve(context.Background(), id, "key-1")
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		switch b.Status {
		case batch.StatusCompleted, batch.StatusFailed, batch.StatusCancelled, batch.StatusExpired:
			return b
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the batch never reached a terminal state")
	return nil
}

func readJSONL(t *testing.T, svc *batch.Service, fileID string) []batch.OutputRow {
	t.Helper()
	if fileID == "" {
		return nil
	}
	rc, _, err := svc.FileContent(context.Background(), fileID, "key-1")
	if err != nil {
		t.Fatalf("FileContent(%s): %v", fileID, err)
	}
	defer rc.Close()
	var out []batch.OutputRow
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var row batch.OutputRow
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("output line is not JSON: %s: %v", line, err)
		}
		out = append(out, row)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading %s: %v", fileID, err)
	}
	return out
}
