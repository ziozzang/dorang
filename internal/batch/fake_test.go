package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

// fakeExecutor answers rows however the test says, and records what it was
// asked. attempt is 1-based and counts per custom_id, so a test can make a row
// fail twice and then succeed.
type fakeExecutor struct {
	mu       sync.Mutex
	attempts map[string]int
	models   map[string]int
	batches  map[string]int
	classes  map[string]int
	fn       func(ctx context.Context, req *ExecRequest, attempt int) (*ExecResult, error)
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{
		attempts: make(map[string]int),
		models:   make(map[string]int),
		batches:  make(map[string]int),
		classes:  make(map[string]int),
	}
}

func (f *fakeExecutor) Execute(ctx context.Context, req *ExecRequest) (*ExecResult, error) {
	f.mu.Lock()
	f.attempts[req.CustomID]++
	n := f.attempts[req.CustomID]
	f.models[req.Model]++
	f.batches[req.BatchID]++
	f.classes[req.PriorityClass]++
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, req, n)
	}
	return okResult(req), nil
}

func (f *fakeExecutor) tries(customID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[customID]
}

func (f *fakeExecutor) distinctRows() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.attempts)
}

func (f *fakeExecutor) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.attempts {
		n += v
	}
	return n
}

func (f *fakeExecutor) rowsOfBatch(batchID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.batches[batchID]
}

// gate lets a test hold rows inside the executor and release them one at a time,
// which is how "cancel while rows are in flight" is made deterministic.
type gate struct {
	tokens chan struct{}
	open   chan struct{}
	once   sync.Once

	mu       sync.Mutex
	started  []string
	finished []string
}

func newGate() *gate {
	return &gate{tokens: make(chan struct{}, 1<<16), open: make(chan struct{})}
}

func (g *gate) handler(ctx context.Context, req *ExecRequest, attempt int) (*ExecResult, error) {
	g.mu.Lock()
	g.started = append(g.started, req.CustomID)
	g.mu.Unlock()
	select {
	case <-g.tokens:
	case <-g.open:
	case <-ctx.Done():
		// A hard stop must not leave a worker wedged inside the executor.
		return nil, ctx.Err()
	}
	g.mu.Lock()
	g.finished = append(g.finished, req.CustomID)
	g.mu.Unlock()
	return okResult(req), nil
}

func (g *gate) allow(n int) {
	for i := 0; i < n; i++ {
		g.tokens <- struct{}{}
	}
}

func (g *gate) openAll() { g.once.Do(func() { close(g.open) }) }

func (g *gate) counts() (started, finished int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.started), len(g.finished)
}

func (g *gate) finishedIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.finished...)
}

func (g *gate) waitFinished(t *testing.T, n int) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		_, done := g.counts()
		return done >= n
	})
}

func (g *gate) waitStarted(t *testing.T, n int) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		s, _ := g.counts()
		return s >= n
	})
}

func okResult(req *ExecRequest) *ExecResult {
	body, _ := json.Marshal(map[string]any{
		"id":    "resp_" + req.CustomID,
		"model": req.Model,
		"choices": []map[string]any{{
			"index":   0,
			"message": map[string]string{"role": "assistant", "content": "ok " + req.CustomID},
		}},
	})
	return &ExecResult{StatusCode: 200, Body: body, RequestID: "req_" + req.CustomID}
}

func errResult(status int, msg string) *ExecResult {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": "bad"},
	})
	return &ExecResult{StatusCode: status, Body: body, RequestID: "req_err"}
}

// terminalError is an executor error that must not be retried. It has the same
// Terminal() bool shape internal/router's *Error has, which is how this package
// recognizes one without importing the router.
type terminalError struct{ msg string }

func (e *terminalError) Error() string  { return e.msg }
func (e *terminalError) Terminal() bool { return true }

// ---------------------------------------------------------------------------
// capacity
// ---------------------------------------------------------------------------

// axis names used by fakeCapacity. They stand for the axes of DESIGN §5.1 that
// a batch row touches.
const (
	axisModel  = "model"
	axisCred   = "cred"
	axisGlobal = "global"
)

// fakeCapacity is a multi-axis reserver with an interactive reserve, modelled on
// internal/capacity's behaviour closely enough to reproduce the defect §11.1
// records: reserveOn selects which axes the reserve is applied to, so a test can
// run the same scenario against revision 1's credential-only reserve and against
// revision 2's every-axis reserve.
type fakeCapacity struct {
	mu        sync.Mutex
	limits    map[string]int
	inUse     map[string]int
	peak      map[string]int
	reserve   float64
	reserveOn map[string]bool
	wake      chan struct{}

	// stripBatch simulates the other half of the defect: work that IS batch but
	// is not marked as such. No amount of reserve protects anything then.
	stripBatch bool

	seen      []CapacityRequest
	batchSeen int
	plainSeen int

	// pick chooses which of a provider's credentials the reservation lands on,
	// exactly as the real broker does when it walks a candidate list. Nil means
	// the provider offered no candidates, which is "" and the pre-existing
	// behaviour of every other test here.
	pick func(CapacityRequest) string
	// granted records every credential a reservation was actually taken
	// against, so a test can compare it against what was dispatched.
	granted []string
}

func newFakeCapacity(reserve float64, reserveOn ...string) *fakeCapacity {
	c := &fakeCapacity{
		limits:    make(map[string]int),
		inUse:     make(map[string]int),
		peak:      make(map[string]int),
		reserve:   reserve,
		reserveOn: make(map[string]bool),
		wake:      make(chan struct{}),
	}
	if len(reserveOn) == 0 {
		reserveOn = []string{axisModel, axisCred, axisGlobal}
	}
	for _, a := range reserveOn {
		c.reserveOn[a] = true
	}
	return c
}

func (c *fakeCapacity) limit(axis, key string, n int) *fakeCapacity {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limits[axis+":"+key] = n
	return c
}

func (c *fakeCapacity) keysFor(req CapacityRequest) []string {
	return []string{
		axisModel + ":" + req.Provider + "\x00" + req.UpstreamModel,
		axisCred + ":" + req.Provider,
		axisGlobal + ":",
	}
}

func (c *fakeCapacity) effLimit(key string, batch bool) (int, bool) {
	lim, ok := c.limits[key]
	if !ok {
		return math.MaxInt32, true // unconfigured axes are unlimited
	}
	axis := key[:strings.IndexByte(key, ':')]
	if !batch || !c.reserveOn[axis] || c.reserve <= 0 {
		return lim, true
	}
	return int(math.Floor(float64(lim)*(1-c.reserve) + 1e-9)), true
}

func (c *fakeCapacity) Acquire(ctx context.Context, req CapacityRequest) (Reservation, error) {
	c.mu.Lock()
	c.seen = append(c.seen, req)
	if req.Batch {
		c.batchSeen++
	} else {
		c.plainSeen++
	}
	c.mu.Unlock()

	batch := req.Batch && !c.stripBatch
	keys := c.keysFor(req)
	for {
		c.mu.Lock()
		blocked := false
		for _, k := range keys {
			eff, _ := c.effLimit(k, batch)
			if eff <= 0 {
				c.mu.Unlock()
				return nil, fmt.Errorf("capacity: %s can never admit this request", k)
			}
			if c.inUse[k] >= eff {
				blocked = true
				break
			}
		}
		if !blocked {
			for _, k := range keys {
				c.inUse[k]++
				if c.inUse[k] > c.peak[k] {
					c.peak[k] = c.inUse[k]
				}
			}
			cred := ""
			if c.pick != nil {
				cred = c.pick(req)
			}
			c.granted = append(c.granted, cred)
			c.mu.Unlock()
			return &fakeReservation{c: c, keys: keys, cred: cred}, nil
		}
		w := c.wake
		c.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *fakeCapacity) peakOf(axis, key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak[axis+":"+key]
}

func (c *fakeCapacity) counts() (batch, plain int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.batchSeen, c.plainSeen
}

type fakeReservation struct {
	c    *fakeCapacity
	keys []string
	cred string
	once sync.Once
}

func (r *fakeReservation) CredentialID() string { return r.cred }

func (r *fakeReservation) Release() {
	r.once.Do(func() {
		r.c.mu.Lock()
		for _, k := range r.keys {
			r.c.inUse[k]--
		}
		close(r.c.wake)
		r.c.wake = make(chan struct{})
		r.c.mu.Unlock()
	})
}

// ---------------------------------------------------------------------------
// clock
// ---------------------------------------------------------------------------

// fakeClock never really sleeps. Backoff still happens — the clock records that
// it was asked to wait, and time advances — but a test with three retries per
// row does not cost three retries' worth of wall time.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.slept += d
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

type fakeModels struct {
	mu sync.Mutex
	m  map[string]Target
}

func newFakeModels() *fakeModels {
	return &fakeModels{m: map[string]Target{
		"m1":            {Provider: "p1", UpstreamModel: "up-1", ProviderGroup: "g1"},
		"m2":            {Provider: "p1", UpstreamModel: "up-2", ProviderGroup: "g1"},
		"vendor:model2": {Provider: "p2", UpstreamModel: "vendor:model2", ProviderGroup: "g1"},
	}}
}

func (f *fakeModels) ResolveModel(name string) (Target, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.m[name]
	return t, ok
}

func (f *fakeModels) remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, name)
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	t     *testing.T
	svc   *Service
	store *MemStore
	blobs *MemBlobs
	exec  *fakeExecutor
	cap   *fakeCapacity
	clock *fakeClock
	model *fakeModels
	cfg   Config

	logMu sync.Mutex
	logs  []string
}

// logged returns everything the service logged. It is collected rather than
// forwarded to t.Logf because a background goroutine logging after the test has
// finished would panic.
func (h *harness) logged() []string {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return append([]string(nil), h.logs...)
}

func newHarness(t *testing.T, tune func(*Config)) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		store: NewMemStore(),
		blobs: NewMemBlobs(),
		exec:  newFakeExecutor(),
		cap:   newFakeCapacity(0),
		clock: newFakeClock(),
		model: newFakeModels(),
	}
	h.cfg = Config{
		Store:         h.store,
		Blobs:         h.blobs,
		Executor:      h.exec,
		Capacity:      h.cap,
		Models:        h.model,
		Clock:         h.clock,
		SweepInterval: -1, // no background sweeper; tests drive Sweep
		BackoffBase:   time.Millisecond,
		BackoffMax:    2 * time.Millisecond,
		Logf: func(format string, args ...any) {
			h.logMu.Lock()
			defer h.logMu.Unlock()
			h.logs = append(h.logs, fmt.Sprintf(format, args...))
		},
	}
	if tune != nil {
		tune(&h.cfg)
	}
	svc, err := New(h.cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.svc = svc
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	})
	return h
}

// restart closes the service and builds a new one over the same store and blobs,
// as a process restart would. The store keeps only what was persisted, because
// MemStore hands out clones.
func (h *harness) restart(tune func(*Config)) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.svc.Close(ctx); err != nil {
		h.t.Fatalf("Close: %v", err)
	}
	cfg := h.cfg
	if tune != nil {
		tune(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		h.t.Fatalf("New after restart: %v", err)
	}
	h.svc = svc
	h.t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = svc.Close(c)
	})
}

// allowAllModels is the authorizer these tests use when the point under test is
// not the allow-list. It is written out rather than defaulted inside the
// service, because a nil authorizer is exactly the omission UploadFile refuses.
func allowAllModels(string) error { return nil }

// upload stores content as a batch input file and returns its id.
func (h *harness) upload(content string) string {
	h.t.Helper()
	f, err := h.svc.UploadFile(context.Background(), UploadRequest{
		Filename: "input.jsonl", Purpose: PurposeBatch, OwnerKeyID: "key-1",
		Content: strings.NewReader(content), Authorize: allowAllModels,
	})
	if err != nil {
		h.t.Fatalf("UploadFile: %v", err)
	}
	return f.ID
}

// create submits a batch over the given input file.
func (h *harness) create(fileID string) *Batch {
	h.t.Helper()
	b, err := h.svc.Create(context.Background(), CreateRequest{
		InputFileID: fileID, Endpoint: "/v1/chat/completions", OwnerKeyID: "key-1",
	})
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	return b
}

// await polls until the batch reaches one of the wanted states.
func (h *harness) await(id string, want ...Status) *Batch {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last *Batch
	for time.Now().Before(deadline) {
		b, err := h.svc.Retrieve(context.Background(), id, "")
		if err != nil {
			h.t.Fatalf("Retrieve: %v", err)
		}
		last = b
		for _, w := range want {
			if b.Status == w {
				return b
			}
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("batch %s never reached %v (stuck at %s, counts %+v)", id, want, last.Status, last.RequestCounts)
	return nil
}

// lines reads a result file back and returns its parsed rows.
func (h *harness) lines(fileID string) []OutputRow {
	h.t.Helper()
	rd, _, err := h.svc.FileContent(context.Background(), fileID, "")
	if err != nil {
		h.t.Fatalf("FileContent(%s): %v", fileID, err)
	}
	defer rd.Close()
	dec := json.NewDecoder(rd)
	var out []OutputRow
	for {
		var row OutputRow
		if err := dec.Decode(&row); err != nil {
			break
		}
		out = append(out, row)
	}
	return out
}

// ---------------------------------------------------------------------------
// input builders
// ---------------------------------------------------------------------------

// count reports how many blobs are stored, so a test can assert that a rejected
// upload left nothing behind.
func (m *MemBlobs) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.data)
}

// inUseOf reports current occupancy of one axis.
func (c *fakeCapacity) inUseOf(axis, key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inUse[axis+":"+key]
}

// jsonlRow builds one input line.
func jsonlRow(customID, model, url, content string) string {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": content}},
	})
	line, _ := json.Marshal(InputRow{CustomID: customID, Method: "POST", URL: url, Body: body})
	return string(line)
}

// jsonlFile builds n rows against one model.
func jsonlFile(n int, model string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(jsonlRow(fmt.Sprintf("req-%d", i), model, "/v1/chat/completions",
			fmt.Sprintf("question %d", i)))
		b.WriteByte('\n')
	}
	return b.String()
}
