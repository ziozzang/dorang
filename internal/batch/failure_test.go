package batch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// flakyStore fails row writes after a while, standing in for a database that
// goes away mid-batch.
type flakyStore struct {
	*MemStore
	mu    sync.Mutex
	after int
	saves int
}

func (f *flakyStore) SaveRow(ctx context.Context, r *RowRecord) error {
	f.mu.Lock()
	f.saves++
	n := f.saves
	f.mu.Unlock()
	if f.after > 0 && n > f.after {
		return errors.New("store: disk on fire")
	}
	return f.MemStore.SaveRow(ctx, r)
}

// A row whose result cannot be recorded is worse than a row that failed: after a
// restart it would be executed, and paid for, a second time. So a store that
// will not accept results stops the batch instead of running it blind — and
// that, unlike a row failing, is a failed batch.
func TestStoreFailureFailsTheBatchRatherThanRunningBlind(t *testing.T) {
	store := &flakyStore{MemStore: NewMemStore(), after: 10}
	h := newHarness(t, func(c *Config) {
		c.Store = store
		c.RowConcurrency = 1
		c.MaxAttempts = 1
	})

	fid := h.upload(jsonlFile(40, "m1"))
	b := h.create(fid)
	final := h.await(b.ID, StatusFailed, StatusCompleted)

	if final.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", final.Status)
	}
	if final.Errors == nil || len(final.Errors.Data) == 0 {
		t.Fatal("a failed batch must say why")
	}
	if got := final.Errors.Data[0].Code; got != "dispatch_failed" {
		t.Errorf("code = %q, want dispatch_failed", got)
	}
	if !strings.Contains(final.Errors.Data[0].Message, "disk on fire") {
		t.Errorf("message does not carry the cause: %q", final.Errors.Data[0].Message)
	}
	// The rows that were recorded before the store gave up are still recorded.
	var recorded int
	if err := store.MemStore.EachRow(context.Background(), b.ID, func(r *RowRecord) error {
		if r.Status.Terminal() {
			recorded++
		}
		return nil
	}); err != nil {
		t.Fatalf("EachRow: %v", err)
	}
	if recorded == 0 || recorded > 40 {
		t.Errorf("%d rows recorded before the failure", recorded)
	}
	// The operator hears about it.
	var logged bool
	for _, line := range h.logged() {
		if strings.Contains(line, "disk on fire") {
			logged = true
		}
	}
	if !logged {
		t.Errorf("the store failure was not logged: %v", h.logged())
	}
}

// A row executed against a backend that answers nothing at all is retried, then
// recorded with an error object rather than a response.
func TestTransportFailureIsRetriedThenRecorded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxAttempts = 3; c.RowConcurrency = 1 })
	h.exec.fn = func(ctx context.Context, req *ExecRequest, attempt int) (*ExecResult, error) {
		if req.CustomID == "req-0" {
			return nil, errors.New("dial tcp: connection refused")
		}
		return okResult(req), nil
	}

	fid := h.upload(jsonlFile(3, "m1"))
	b := h.create(fid)
	final := h.await(b.ID, StatusCompleted)

	if final.RequestCounts != (RequestCounts{Total: 3, Completed: 2, Failed: 1}) {
		t.Fatalf("counts = %+v", final.RequestCounts)
	}
	if got := h.exec.tries("req-0"); got != 3 {
		t.Errorf("tried %d times, want 3", got)
	}
	rows := h.lines(final.ErrorFileID)
	if len(rows) != 1 || rows[0].Error == nil || rows[0].Response != nil {
		t.Fatalf("error file = %+v", rows)
	}
	if !strings.Contains(rows[0].Error.Message, "connection refused") {
		t.Errorf("error message = %q", rows[0].Error.Message)
	}
	// Backoff actually happened between the attempts rather than a hot loop.
	h.clock.mu.Lock()
	slept := h.clock.slept
	h.clock.mu.Unlock()
	if slept <= 0 {
		t.Error("retries did not back off")
	}
}

// An executor that panics must not take a capacity slot with it. The reservation
// is released on every path out.
func TestExecutorPanicIsContained(t *testing.T) {
	c := newFakeCapacity(0)
	c.limit(axisModel, "p1\x00up-1", 2)
	h := newHarness(t, func(cfg *Config) {
		cfg.Capacity = c
		cfg.RowConcurrency = 2
		cfg.MaxAttempts = 1
	})
	h.exec.fn = func(ctx context.Context, req *ExecRequest, attempt int) (*ExecResult, error) {
		if req.CustomID == "req-1" {
			panic("executor exploded")
		}
		return okResult(req), nil
	}

	fid := h.upload(jsonlFile(6, "m1"))
	b := h.create(fid)
	final := h.await(b.ID, StatusCompleted)

	if final.RequestCounts != (RequestCounts{Total: 6, Completed: 5, Failed: 1}) {
		t.Fatalf("counts = %+v", final.RequestCounts)
	}
	if got := c.inUseOf(axisModel, "p1\x00up-1"); got != 0 {
		t.Errorf("%d capacity slots leaked by the panic", got)
	}
}

func TestSystemClock(t *testing.T) {
	var c SystemClock
	if c.Now().IsZero() {
		t.Error("Now returned the zero time")
	}
	start := time.Now()
	if err := c.Sleep(context.Background(), 2*time.Millisecond); err != nil {
		t.Errorf("Sleep: %v", err)
	}
	if time.Since(start) < time.Millisecond {
		t.Error("Sleep returned early")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep on a cancelled context = %v", err)
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	base := Config{
		Store: NewMemStore(), Blobs: NewMemBlobs(), Executor: newFakeExecutor(),
		Capacity: newFakeCapacity(0), Models: newFakeModels(),
	}
	for _, tc := range []struct {
		name string
		drop func(*Config)
	}{
		{"store", func(c *Config) { c.Store = nil }},
		{"blobs", func(c *Config) { c.Blobs = nil }},
		{"executor", func(c *Config) { c.Executor = nil }},
		{"capacity", func(c *Config) { c.Capacity = nil }},
		{"models", func(c *Config) { c.Models = nil }},
	} {
		cfg := base
		tc.drop(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("New without %s succeeded", tc.name)
		}
	}
	cfg := base
	cfg.SweepInterval = -1
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := svc.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := svc.Create(context.Background(), CreateRequest{
		InputFileID: "file-x", Endpoint: "/v1/chat/completions",
	}); !errors.Is(err, ErrClosed) {
		t.Errorf("Create after Close = %v, want ErrClosed", err)
	}
}
