package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// measureAlloc reports total bytes allocated before and after fn, which is the
// figure that distinguishes a streaming relay from a buffering one. HeapAlloc
// would be confounded by the garbage collector; TotalAlloc is cumulative and
// monotonic, so the difference is exactly what fn asked the allocator for.
func measureAlloc(fn func()) (before, after uint64) {
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	before = m.TotalAlloc
	fn()
	runtime.ReadMemStats(&m)
	return before, m.TotalAlloc
}

// staticAuth is the leanest possible authenticator: one comparison, one
// pre-built principal, no bookkeeping. The allocation tests need a fake that
// contributes nothing of its own, or they measure the fake.
type staticAuth struct {
	token string
	p     *fakePrincipal
}

func (a *staticAuth) AuthenticateHeader(_ context.Context, h http.Header) (Principal, error) {
	for _, name := range authHeaders {
		v := h.Get(name)
		if v == "" {
			continue
		}
		if name == HeaderAuthorization && len(v) > 7 && v[:7] == "Bearer " {
			v = v[7:]
		}
		if v == a.token {
			return a.p, nil
		}
		break
	}
	return nil, NewError(http.StatusUnauthorized, TypeAuthentication, "bad key")
}

// TestNoAllocsRoutingAndAuth is DESIGN §15.2.2 as an assertion: the pooled
// request struct is the only thing the authenticate-and-route path takes from
// the allocator.
//
// It is scoped to that path deliberately. Everything after it — reading a body,
// materializing a model name, setting a response header — allocates by
// construction, and a test that mixed them in would be a budget rather than an
// invariant. The invariant is that adding a route or a key does not add an
// allocation.
func TestNoAllocsRoutingAndAuth(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Auth = &staticAuth{token: "good", p: &fakePrincipal{id: "k"}}
	})
	cfg := s.snap.Load()
	h := http.Header{}
	h.Set(HeaderAuthorization, "Bearer good")
	ctx := context.Background()

	allocs := testing.AllocsPerRun(2000, func() {
		rq := s.acquire()
		rt, np, res := cfg.routes.lookup(http.MethodPost, "/v1/chat/completions", &rq.params)
		if res != lookupHit {
			panic("route miss")
		}
		rq.Route, rq.nparams = rt, np
		p, err := cfg.auth.AuthenticateHeader(ctx, h)
		if err != nil {
			panic(err)
		}
		rq.Principal = p
		if err := p.Authorize(Access{Model: "model-x", Route: "/v1/chat/completions"}); err != nil {
			panic(err)
		}
		s.release(rq)
	})
	if allocs != 0 {
		t.Errorf("authenticate-and-route allocated %.0f times per request, want 0", allocs)
	}
}

// TestNoAllocsPatternedRouteLookup: capturing a path parameter must not
// allocate either, which is why the parameters live in a fixed array on the
// pooled struct rather than in a map or in the request context.
func TestNoAllocsPatternedRouteLookup(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Passthrough = []PassthroughRoute{
			{Prefix: "/anthropic", Provider: "p", BaseURL: "http://127.0.0.1:1"},
		}
		o.Routes = []Route{{
			Pattern: "/openai/deployments/{model}/chat/completions",
			Methods: MethodPOST, Name: "azure", ModelAuth: ModelAuthNone,
			Handler: func(http.ResponseWriter, *Request) error { return nil },
		}}
	})
	cfg := s.snap.Load()
	var params [maxParams]Param

	allocs := testing.AllocsPerRun(2000, func() {
		if _, _, res := cfg.routes.lookup(http.MethodPost,
			"/openai/deployments/gpt-4o/chat/completions", &params); res != lookupHit {
			panic("route miss")
		}
		if _, _, res := cfg.routes.lookup(http.MethodPost,
			"/anthropic/v1/messages", &params); res != lookupHit {
			panic("passthrough miss")
		}
	})
	if allocs != 0 {
		t.Errorf("patterned lookup allocated %.0f times, want 0", allocs)
	}
}

// TestSnapshotReadTakesNoLock is the decisive form of DESIGN §15.2.1.
//
// The claim "the read path takes no lock" cannot be proved by a throughput
// number — a mutex held for microseconds looks like nothing. So the test holds
// the server's only mutex for the whole duration of a request and requires the
// request to finish anyway. If the request path ever learns to take that lock,
// this deadlocks and the test fails on the timeout.
func TestSnapshotReadTakesNoLock(t *testing.T) {
	s := newTestServer(t, nil)

	s.mu.Lock()
	done := make(chan int, 1)
	go func() {
		done <- do(s, post("/v1/chat/completions", `{"model":"model-x"}`)).Code
	}()
	select {
	case code := <-done:
		s.mu.Unlock()
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
	case <-time.After(3 * time.Second):
		s.mu.Unlock()
		t.Fatal("a request blocked while the configuration mutex was held: " +
			"the read path is taking a lock")
	}
}

// TestSnapshotSwapIsAtomic runs reloads against concurrent traffic. Under -race
// this is what catches a snapshot field read that did not go through the
// pointer.
func TestSnapshotSwapIsAtomic(t *testing.T) {
	s := newTestServer(t, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var bad, reloads atomic.Int64

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := s.Reload(Options{
				Auth:       newFakeAuth("good"),
				Dispatcher: jsonDispatcher(`{"ok":true}`),
				Models:     ModelSlice{{ID: "model-x"}},
			}); err != nil {
				t.Error(err)
				return
			}
			reloads.Add(1)
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if do(s, post("/v1/chat/completions", `{"model":"model-x"}`)).Code != 200 {
					bad.Add(1)
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	// The reload goroutine is in the same WaitGroup, so stop it once the
	// traffic goroutines have had their turn.
	time.Sleep(100 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("requests did not complete alongside continuous reloads")
	}
	if bad.Load() != 0 {
		t.Errorf("%d requests failed while the configuration was being swapped", bad.Load())
	}
	if reloads.Load() == 0 {
		t.Error("no reload actually ran")
	}
}

// discardWriter is a response writer that keeps its header map across
// iterations, so the allocation budget below measures the handler rather than
// the recorder.
type discardWriter struct {
	hdr    http.Header
	status int
	n      int64
}

func (d *discardWriter) Header() http.Header         { return d.hdr }
func (d *discardWriter) WriteHeader(code int)        { d.status = code }
func (d *discardWriter) Write(p []byte) (int, error) { d.n += int64(len(p)); return len(p), nil }

// handlerAllocBudget is what one end-to-end non-streaming request may allocate.
//
// It is a budget rather than zero because a few allocations are inherent to
// answering an HTTP request at all: the request id string, the model name
// lifted out of the pooled body buffer, and one []string per response header
// that http.Header.Set creates. The useful property is not "zero" — it is that
// adding work to the request path shows up here immediately.
const handlerAllocBudget = 40

func TestHandlerAllocationBudget(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Auth = &staticAuth{token: "good", p: &fakePrincipal{id: "k"}}
	})
	body := `{"model":"model-x","messages":[{"role":"user","content":"hello"}]}`
	w := &discardWriter{hdr: make(http.Header, 8)}

	allocs := testing.AllocsPerRun(500, func() {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set(HeaderAuthorization, "Bearer good")
		s.ServeHTTP(w, r)
	})
	t.Logf("end-to-end handler: %.0f allocs/op (budget %d)", allocs, handlerAllocBudget)
	if allocs > handlerAllocBudget {
		t.Errorf("handler allocated %.0f times per request, budget is %d", allocs, handlerAllocBudget)
	}
}

// TestStreamRelayDoesNotBuffer pushes 16 MiB through the server's own response
// wrapper. The passthrough engine has its own version of this test; this one
// covers the wrapper every dispatcher writes through, which is the piece that
// would be tempting to give a growable buffer for byte counting.
func TestStreamRelayDoesNotBuffer(t *testing.T) {
	const size = 16 << 20
	chunk := make([]byte, 32<<10)

	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			for written := 0; written < size; written += len(chunk) {
				if _, err := w.Write(chunk); err != nil {
					return err
				}
			}
			return nil
		})
	})

	cw := &countingWriter{ResponseWriter: httptest.NewRecorder()}
	r := post("/v1/chat/completions", `{"model":"model-x","stream":true}`)

	before, after := measureAlloc(func() { s.ServeHTTP(cw, r) })

	if cw.total != size {
		t.Fatalf("wrote %d bytes, want %d", cw.total, size)
	}
	if cw.maxWrite > len(chunk) {
		t.Errorf("a single write of %d bytes — the wrapper is coalescing", cw.maxWrite)
	}
	if grew := after - before; grew > 1<<20 {
		t.Errorf("streaming %d bytes allocated %d bytes; the response was buffered", size, grew)
	}
}

// TestBodyBufferIsReturnedToThePool asserts reuse directly rather than by
// measuring allocation.
//
// A measurement-based version of this test was written first and was wrong:
// runtime.GC() empties a sync.Pool, and a loop that allocates enough to trigger
// a GC empties it again mid-run, so the number moves with the collector rather
// than with the code. Comparing the backing array's address across iterations
// asks the question the test actually means.
func TestBodyBufferIsReturnedToThePool(t *testing.T) {
	budget := newReplayBudget(1 << 20)
	payload := `{"model":"model-x","pad":"` + strings.Repeat("x", 4096) + `"}`

	const iterations = 8
	var prev *byte
	reused := 0
	for i := 0; i < iterations; i++ {
		var b Body
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(payload))
		if e := b.read(r, 1<<20, budget); e != nil {
			t.Fatalf("read: %v", e)
		}
		if len(b.buf) != len(payload) {
			t.Fatalf("read %d bytes, want %d", len(b.buf), len(payload))
		}
		if !b.Replayable() {
			t.Fatal("a body that fits the budget was not retained")
		}
		if cur := &b.buf[0]; prev != nil && cur == prev {
			reused++
		} else {
			prev = cur
		}
		b.release()
		if b.Replayable() {
			t.Fatal("release left the body marked replayable")
		}
	}
	// A garbage collection between iterations legitimately drops the pooled
	// buffer, and under -race the per-P pool shuffles more, so the assertion is
	// that reuse is the norm rather than that it is universal. Removing the
	// pool entirely takes this to zero.
	if reused < (iterations-1)/2 {
		t.Errorf("the body buffer was reused %d times out of %d: it is not being pooled",
			reused, iterations-1)
	}
	if budget.Used() != 0 {
		t.Errorf("the replay budget leaked %d bytes", budget.Used())
	}
}

// TestReplayBudgetIsProcessWide is the distinction DESIGN §15.4 draws: a
// per-request cap alone is not a bound, because many concurrent requests each
// just under it exceed the whole memory target.
func TestReplayBudgetIsProcessWide(t *testing.T) {
	const each = 1024
	budget := newReplayBudget(3 * each)
	payload := `{"model":"m","pad":"` + strings.Repeat("x", each-24) + `"}`

	var held []*Body
	retained := 0
	for i := 0; i < 10; i++ {
		b := &Body{}
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
		if e := b.read(r, 1<<20, budget); e != nil {
			t.Fatalf("read: %v", e)
		}
		if b.Replayable() {
			retained++
		}
		held = append(held, b)
	}
	if retained == 0 || retained > 3 {
		t.Errorf("%d of 10 bodies retained against a %d-byte budget for %d-byte bodies",
			retained, 3*each, len(payload))
	}
	if used := budget.Used(); used > 3*each {
		t.Errorf("budget overshot: %d bytes used against a %d limit", used, 3*each)
	}
	for _, b := range held {
		b.release()
	}
	if budget.Used() != 0 {
		t.Errorf("the replay budget leaked %d bytes", budget.Used())
	}
}

// TestUsageScanning covers the bounded tee the passthrough meter reads.
//
// It also pins the normalization, which is the difference between one
// definition of "input tokens" in the binary and two. The OpenAI family's
// prompt_tokens ALREADY contains the cached prefix; the Anthropic family's
// input_tokens does not and counts it beside. A relayed body is scanned into
// the same shape a converted one produces — Input inclusive, Total = Input +
// Output — because both end up in the same ledger column.
func TestUsageScanning(t *testing.T) {
	cases := []struct {
		body string
		want Usage
		ok   bool
	}{
		{`{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			Usage{Input: 10, Output: 5, Total: 15}, true},
		// Anthropic spelling: input_tokens 3 EXCLUDES the 2 cached, so the
		// inclusive input is 5 and the total the client is owed is 9.
		{`{"id":"x","usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":2}}`, // pragma: allowlist secret — test fixture
			Usage{Input: 5, Output: 4, CacheRead: 2, Total: 9}, true},
		// OpenAI spelling: the 7 cached are already inside prompt_tokens... which
		// is 1, so this body is self-contradictory. It is scanned as stated
		// rather than repaired: inventing an input count is worse than relaying
		// the upstream's own arithmetic.
		{`{"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":7}}}`, // pragma: allowlist secret — test fixture
			Usage{Input: 1, CacheRead: 7, Total: 1}, true},
		// A stated total that contradicts its own parts is NOT kept. It used to
		// be — "the number the client was handed" — and that made it the second
		// of three answers to "how many tokens was that": it reached the
		// tokens-per-minute ceiling, which reads server.Usage.Total, and no
		// further, because meter.Tokens carries the five breakdown fields and
		// re-derives the total from them. The same passthrough request was
		// counted 99 against the ceiling and 15 in the ledger. A total dorang
		// cannot attribute to a prompt or a completion is not one it can carry.
		{`{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":99}}`,
			Usage{Input: 10, Output: 5, Total: 15}, true},
		// A total with NEITHER half stated is a different thing: an embedding
		// and a rerank have no completion, so the total IS the prompt count.
		// Reading it that way is what internal/backend's scanRelayUsage already
		// does; without it a passthrough embedding metered as zero tokens, and a
		// request of zero tokens is one §11.6's token guard cannot see.
		{`{"usage":{"total_tokens":4}}`, Usage{Input: 4, Total: 4}, true},
		{`{"choices":[{"usage":{"prompt_tokens":99}}]}`, Usage{}, false}, // nested, not top level
		{`{"no":"usage"}`, Usage{}, false},
		{`not json`, Usage{}, false},
	}
	for _, c := range cases {
		got, ok := scanUsage([]byte(c.body))
		if ok != c.ok || got != c.want {
			t.Errorf("scanUsage(%s) = (%+v,%v), want (%+v,%v)", c.body, got, ok, c.want, c.ok)
		}
	}
}

// TestRequestPoolDoesNotLeakState is what makes pooling safe: a request must
// not be able to see the previous one's principal, body or result.
func TestRequestPoolDoesNotLeakState(t *testing.T) {
	s := newTestServer(t, nil)
	rq := s.acquire()
	rq.Model = "leaked-model"
	rq.Principal = &fakePrincipal{id: "leaked-key"}
	rq.Result.Provider = "leaked-provider"
	rq.Result.QuotaUsedPct = map[string]int{"minute": 90}
	rq.ID = "leaked-id"
	rq.nparams = 2
	rq.params[0] = Param{Name: "model", Value: "leaked"}
	s.release(rq)

	got := s.acquire()
	defer s.release(got)
	if got.Model != "" || got.Principal != nil || got.ID != "" ||
		got.Result.Provider != "" || got.nparams != 0 || got.params[0].Value != "" {
		t.Errorf("pooled request retained state: %+v", got)
	}
	if len(got.Result.QuotaUsedPct) != 0 {
		t.Errorf("quota map retained %d entries", len(got.Result.QuotaUsedPct))
	}
}

// TestRequestIDsAreUnique: the id is the ledger's join key, so a collision
// silently merges two callers' rows.
func TestRequestIDsAreUnique(t *testing.T) {
	s := newTestServer(t, nil)
	seen := make(map[string]struct{}, 4096)
	var buf [32]byte
	for i := 0; i < 4096; i++ {
		id := string(s.newRequestID(buf[:0]))
		if len(id) != 32 {
			t.Fatalf("id %q has length %d, want 32", id, len(id))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate request id %q after %d ids", id, i)
		}
		seen[id] = struct{}{}
	}
}
