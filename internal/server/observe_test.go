package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeObserver is the twenty-line implementation deps.go promises the
// interfaces are narrow enough for.
type fakeObserver struct {
	mu sync.Mutex

	// sample decides; nil samples everything.
	sample func(id string, h http.Header) bool
	// onObserve runs inside Observe, so a test can prove what the contract
	// forbids — blocking, panicking — is survivable.
	onObserve func(*Observation)

	sampled  []string
	captured []captured
	health   []byte
	metrics  []byte
}

// captured is a deep copy of an Observation, since the real one is borrowed.
type captured struct {
	id             string
	method, path   string
	rawQuery       string
	route          string
	model          string
	status         int
	reqBody        string
	head, tail     string
	bodyBytes      int64
	truncated      bool
	respHeaderKeys []string
	reqAuth        string
	cost           int64
	priced         bool
	duration       time.Duration
}

func (o *fakeObserver) Sample(id string, h http.Header) bool {
	if o.sample != nil && !o.sample(id, h) {
		return false
	}
	o.mu.Lock()
	o.sampled = append(o.sampled, id)
	o.mu.Unlock()
	return true
}

func (o *fakeObserver) Observe(ob *Observation) {
	if o.onObserve != nil {
		o.onObserve(ob)
	}
	c := captured{
		id: ob.RequestID, method: ob.Method, path: ob.Path, rawQuery: ob.RawQuery,
		route: ob.Route, model: ob.Model, status: ob.Status,
		reqBody: string(ob.RequestBody), head: string(ob.ResponseHead),
		tail: string(ob.ResponseTail), bodyBytes: ob.BodyBytes,
		truncated: ob.Truncated, cost: ob.CostNanoUSD, priced: ob.Priced,
		duration: ob.Duration,
	}
	if ob.RequestHeader != nil {
		c.reqAuth = ob.RequestHeader.Get(HeaderAuthorization)
	}
	for k := range ob.ResponseHeader {
		c.respHeaderKeys = append(c.respHeaderKeys, k)
	}
	o.mu.Lock()
	o.captured = append(o.captured, c)
	o.mu.Unlock()
}

func (o *fakeObserver) Health(dst []byte) []byte  { return append(dst, o.health...) }
func (o *fakeObserver) Metrics(dst []byte) []byte { return append(dst, o.metrics...) }

func (o *fakeObserver) all() []captured {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]captured, len(o.captured))
	copy(out, o.captured)
	return out
}

func (o *fakeObserver) one(t *testing.T) captured {
	t.Helper()
	all := o.all()
	if len(all) != 1 {
		t.Fatalf("want exactly one observation, got %d", len(all))
	}
	return all[0]
}

func TestObserverSeesTheRequestAndTheResponseTheClientGot(t *testing.T) {
	obs := &fakeObserver{}
	s := newTestServer(t, func(o *Options) {
		o.Observer = obs
		o.Dispatcher = jsonDispatcher(`{"ok":true,"choices":[]}`)
	})

	r := post("/v1/chat/completions?beta=true", `{"model":"model-x","messages":[]}`)
	w := do(s, r)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}

	c := obs.one(t)
	if c.method != http.MethodPost || c.path != "/v1/chat/completions" {
		t.Errorf("method/path = %s %s", c.method, c.path)
	}
	if c.rawQuery != "beta=true" {
		t.Errorf("rawQuery = %q; a query string that is not replayed asks the reference a different question", c.rawQuery)
	}
	if c.route != "chat_completions" {
		t.Errorf("route = %q", c.route)
	}
	if c.model != "model-x" {
		t.Errorf("model = %q", c.model)
	}
	if c.status != 200 {
		t.Errorf("status = %d", c.status)
	}
	if c.head != `{"ok":true,"choices":[]}` {
		t.Errorf("captured body = %q", c.head)
	}
	if c.bodyBytes != int64(len(c.head)) {
		t.Errorf("bodyBytes = %d, want %d", c.bodyBytes, len(c.head))
	}
	if c.truncated {
		t.Error("a body this small is not truncated")
	}
	if c.reqBody != `{"model":"model-x","messages":[]}` {
		t.Errorf("captured request body = %q", c.reqBody)
	}
	if !c.priced || c.cost != 123456 {
		t.Errorf("cost = %d priced = %v; the observer needs dorang's own price as its cost estimate",
			c.cost, c.priced)
	}
	// The response header map is the one that was sent, extension headers
	// included — that is what makes a header-key comparison possible.
	var sawRequestID bool
	for _, k := range c.respHeaderKeys {
		if strings.EqualFold(k, HeaderRequestID) {
			sawRequestID = true
		}
	}
	if !sawRequestID {
		t.Errorf("response header snapshot missing %s: %v", HeaderRequestID, c.respHeaderKeys)
	}
	// The inbound header is handed over unmodified, credentials included; the
	// observer is documented as building its outgoing request from an
	// allow-list rather than trusting a strip here.
	if c.reqAuth == "" {
		t.Error("the inbound header map should be the real one")
	}
}

func TestObserverIsNotCalledWhenNotSampled(t *testing.T) {
	obs := &fakeObserver{sample: func(string, http.Header) bool { return false }}
	s := newTestServer(t, func(o *Options) { o.Observer = obs })

	for i := 0; i < 5; i++ {
		if w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`)); w.Code != 200 {
			t.Fatalf("status %d", w.Code)
		}
	}
	if got := len(obs.all()); got != 0 {
		t.Fatalf("observed %d unsampled requests", got)
	}
}

func TestObserverSampleSeesTheRequestIDAndTheHeaders(t *testing.T) {
	// The id is what makes sampling deterministic across a retry; the headers
	// are what carries the loop guard.
	var gotID string
	var gotHeader string
	obs := &fakeObserver{sample: func(id string, h http.Header) bool {
		gotID = id
		gotHeader = h.Get("X-Dorang-Shadow")
		return false
	}}
	s := newTestServer(t, func(o *Options) { o.Observer = obs })

	r := post("/v1/chat/completions", `{"model":"model-x"}`)
	r.Header.Set("X-Request-Id", "client-supplied-id")
	r.Header.Set("X-Dorang-Shadow", "1")
	do(s, r)

	if gotID != "client-supplied-id" {
		t.Errorf("Sample saw id %q; a client-supplied call id must reach it or a retry re-samples", gotID)
	}
	if gotHeader != "1" {
		t.Errorf("Sample saw no loop-guard header: %q", gotHeader)
	}
}

func TestShadowedRequestProducesExactlyOneMeterEvent(t *testing.T) {
	// The accounting claim, asserted. If the reference call were metered — or
	// if the observer hook double-recorded — every cost and quota figure would
	// be wrong by the sample rate for the sampled fraction.
	meter := &recordingMeter{}
	obs := &fakeObserver{}
	s := newTestServer(t, func(o *Options) {
		o.Meter = meter
		o.Observer = obs
	})

	for i := 0; i < 7; i++ {
		if w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`)); w.Code != 200 {
			t.Fatalf("status %d", w.Code)
		}
	}

	meter.mu.Lock()
	n := len(meter.events)
	meter.mu.Unlock()
	if n != 7 {
		t.Fatalf("7 shadowed requests produced %d ledger rows, want 7", n)
	}
	if got := len(obs.all()); got != 7 {
		t.Fatalf("observed %d of 7", got)
	}
	ev := meter.last(t)
	if ev.Result.CostNanoUSD != 123456 {
		t.Errorf("the ledger row's cost changed under shadowing: %d", ev.Result.CostNanoUSD)
	}
}

func TestObserverPanicNeverFailsTheRequest(t *testing.T) {
	obs := &fakeObserver{onObserve: func(*Observation) { panic("observer exploded") }}
	s := newTestServer(t, func(o *Options) { o.Observer = obs })

	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	if w.Code != 200 {
		t.Fatalf("status %d; an observer panic must not reach the client", w.Code)
	}
	if w.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q", w.Body.String())
	}
	if s.metrics.observerPanics.Load() != 1 {
		t.Error("an observer panic must be counted, not merely swallowed")
	}
}

func TestObserverDoesNotDelayTheResponse(t *testing.T) {
	// The contract is that Observe does not block. This asserts the server
	// calls it after the last byte rather than before, which is what makes the
	// contract enforceable at all: a blocking Observe delays the *next*
	// request's turn on this goroutine, never this one's body.
	var bodyWritten, observeCalled time.Time
	obs := &fakeObserver{onObserve: func(*Observation) { observeCalled = time.Now() }}
	s := newTestServer(t, func(o *Options) {
		o.Observer = obs
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, `{"ok":true}`)
			bodyWritten = time.Now()
			return err
		})
	})
	do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	if observeCalled.Before(bodyWritten) {
		t.Fatal("Observe ran before the response body was written")
	}
}

func TestCaptureKeepsHeadAndTailOfALargeStream(t *testing.T) {
	// A long generation must not be retained whole, and the terminator must
	// survive anyway — it is one of the things §14.1 requires compared.
	const chunks = 400
	obs := &fakeObserver{}
	s := newTestServer(t, func(o *Options) {
		o.Observer = obs
		o.CaptureHeadBytes = 512
		o.CaptureTailBytes = 64
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; i < chunks; i++ {
				if _, err := io.WriteString(w,
					"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tokentoken\"}}]}\n\n"); err != nil {
					return err
				}
			}
			_, err := io.WriteString(w, "data: [DONE]\n\n")
			return err
		})
	})

	do(s, post("/v1/chat/completions", `{"model":"model-x","stream":true}`))
	c := obs.one(t)

	if !c.truncated {
		t.Fatal("a stream this long must be reported truncated, not silently trimmed")
	}
	if len(c.head) > 512 {
		t.Errorf("head is %d bytes, cap is 512", len(c.head))
	}
	if len(c.tail) > 64 {
		t.Errorf("tail is %d bytes, cap is 64", len(c.tail))
	}
	if !strings.HasPrefix(c.head, "data: {") {
		t.Errorf("head does not start at the beginning of the body: %q", c.head)
	}
	if !strings.HasSuffix(c.tail, "data: [DONE]\n\n") {
		t.Errorf("the terminator was lost; tail = %q", c.tail)
	}
	if c.bodyBytes < int64(chunks*10) {
		t.Errorf("bodyBytes = %d, want the true length", c.bodyBytes)
	}
}

func TestCaptureWindowsMeetForAModeratelySizedBody(t *testing.T) {
	// Head and tail overlap whenever the body is bigger than the head and
	// smaller than head+tail. Left untrimmed, the comparison would see the same
	// bytes twice and count the terminator twice.
	obs := &fakeObserver{}
	body := strings.Repeat("x", 300)
	s := newTestServer(t, func(o *Options) {
		o.Observer = obs
		o.CaptureHeadBytes = 200
		o.CaptureTailBytes = 200
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, body)
			return err
		})
	})
	do(s, post("/v1/chat/completions", `{"model":"model-x"}`))

	c := obs.one(t)
	if c.truncated {
		t.Error("a body that fits both windows together is not truncated")
	}
	if got := c.head + c.tail; got != body {
		t.Fatalf("head+tail = %d bytes, want the whole %d-byte body", len(got), len(body))
	}
}

func TestCaptureSurvivesManySmallWrites(t *testing.T) {
	obs := &fakeObserver{}
	s := newTestServer(t, func(o *Options) {
		o.Observer = obs
		o.CaptureHeadBytes = 16
		o.CaptureTailBytes = 8
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/plain")
			for i := 0; i < 100; i++ {
				if _, err := io.WriteString(w, "ab"); err != nil {
					return err
				}
			}
			return nil
		})
	})
	do(s, post("/v1/chat/completions", `{"model":"model-x"}`))

	c := obs.one(t)
	if c.head != strings.Repeat("ab", 8) {
		t.Errorf("head = %q", c.head)
	}
	if c.tail != strings.Repeat("ab", 4) {
		t.Errorf("tail = %q", c.tail)
	}
	if c.bodyBytes != 200 {
		t.Errorf("bodyBytes = %d, want 200", c.bodyBytes)
	}
}

func TestObserverSeesErrorResponsesToo(t *testing.T) {
	// An error envelope divergence is exactly what COMPATIBILITY §11 says a
	// client's retry loop breaks on, so the gate has to be able to see one.
	obs := &fakeObserver{}
	s := newTestServer(t, func(o *Options) { o.Observer = obs })

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-x"}`))
	r.Header.Set(HeaderAuthorization, "Bearer wrong")
	w := do(s, r)
	if w.Code != 401 {
		t.Fatalf("status %d", w.Code)
	}
	c := obs.one(t)
	if c.status != 401 {
		t.Errorf("observed status %d, want 401", c.status)
	}
	if !strings.Contains(c.head, `"authentication_error"`) {
		t.Errorf("the error envelope was not captured: %q", c.head)
	}
}

func TestObserverSeesUnimplementedRoutes(t *testing.T) {
	// A 501 from dorang against a 404 or a 200 from the incumbent is a real
	// migration blocker, and the sampling decision is taken before routing so
	// that it can be seen.
	obs := &fakeObserver{}
	s := newTestServer(t, func(o *Options) { o.Observer = obs })

	w := do(s, get("/v1/files"))
	if w.Code != 501 {
		t.Fatalf("status %d", w.Code)
	}
	c := obs.one(t)
	if c.status != 501 || c.route != "" {
		t.Errorf("observed %d route=%q, want 501 and no route", c.status, c.route)
	}
}

// hijackRecorder is a recorder that can be hijacked, which httptest's cannot.
// The connection it hands back is a pipe nobody reads; the relay under test
// never writes to it.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c1, c2 := net.Pipe()
	h.conn = c2
	return c1, bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1)), nil
}

func TestHijackedConnectionIsNotObserved(t *testing.T) {
	// Everything after a hijack bypasses Write, so the capture buffer stays
	// empty. Reporting it as "the response" would put a fabricated comparison
	// in a report a production cutover is decided on.
	obs := &fakeObserver{}
	var hijacked bool
	s := newTestServer(t, func(o *Options) {
		o.Observer = obs
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("the server's response writer must forward Hijack")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return err
			}
			hijacked = true
			return conn.Close()
		})
	})

	rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.ServeHTTP(rec, post("/v1/chat/completions", `{"model":"model-x"}`))
	if rec.conn != nil {
		_ = rec.conn.Close()
	}

	if !hijacked {
		t.Fatal("the dispatcher did not hijack")
	}
	if got := len(obs.all()); got != 0 {
		t.Fatalf("a hijacked connection produced %d observations; the capture never saw its bytes", got)
	}
}

func TestObserverHealthAndMetricsAreSurfaced(t *testing.T) {
	obs := &fakeObserver{
		health:  []byte(`{"mode":"compare","cost_capped":true}`),
		metrics: []byte("dorang_shadow_cost_capped 1\n"),
	}
	s := newTestServer(t, func(o *Options) { o.Observer = obs })

	// A cost ceiling that stopped shadowing has to be visible where an operator
	// already looks — and must not make the gateway unhealthy, since a stopped
	// diagnostic is not a serving failure.
	w := do(s, get("/health/readiness"))
	if w.Code != 200 {
		t.Fatalf("readiness status %d; a stopped shadow must not take the pod out of rotation", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"shadow":{"mode":"compare","cost_capped":true}`) {
		t.Errorf("health body does not carry the shadow section: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"ready"`) {
		t.Errorf("health body lost its own status: %s", w.Body.String())
	}
	if got, want := w.Header().Get("Content-Length"), w.Body.Len(); got != itoa(want) {
		t.Errorf("Content-Length = %s, body is %d bytes", got, want)
	}

	// Liveness stays a constant: it answers whether the process is up, and a
	// probe that starts failing because a diagnostic stopped is a worse probe.
	w = do(s, get("/health/liveliness"))
	if strings.Contains(w.Body.String(), "shadow") {
		t.Errorf("liveness must stay minimal: %s", w.Body.String())
	}

	w = do(s, get("/metrics"))
	if !strings.Contains(w.Body.String(), "dorang_shadow_cost_capped 1") {
		t.Error("observer metrics are not on the scrape")
	}
	if !strings.Contains(w.Body.String(), "dorang_shadow_observed_total") {
		t.Error("the server's own observation counter is missing")
	}
}

func TestHealthBodyIsUnchangedWithoutAnObserver(t *testing.T) {
	s := newTestServer(t, nil)
	for path, want := range map[string]string{
		"/health/liveliness": `{"status":"alive"}`,
		"/health/readiness":  `{"status":"ready"}`,
		"/health":            `{"status":"healthy"}`,
	} {
		w := do(s, get(path))
		if got := w.Body.String(); got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
}

func TestObserverThatReturnsNoHealthOmitsTheSection(t *testing.T) {
	obs := &fakeObserver{} // Health appends nothing
	s := newTestServer(t, func(o *Options) { o.Observer = obs })
	w := do(s, get("/health/readiness"))
	if got := w.Body.String(); got != `{"status":"ready"}` {
		t.Errorf("body = %s, want the section omitted entirely", got)
	}
}

func TestCapturePoolDoesNotLeakBetweenRequests(t *testing.T) {
	// The capture buffer is pooled. A request that got a recycled buffer must
	// not see the previous request's body — which is a cross-tenant leak, not a
	// cosmetic bug.
	obs := &fakeObserver{}
	bodies := []string{strings.Repeat("A", 300), "B", strings.Repeat("C", 50)}
	idx := 0
	s := newTestServer(t, func(o *Options) {
		o.Observer = obs
		o.CaptureHeadBytes = 256
		o.CaptureTailBytes = 32
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, bodies[idx])
			return err
		})
	})
	for i := range bodies {
		idx = i
		do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	}

	all := obs.all()
	if len(all) != len(bodies) {
		t.Fatalf("observed %d of %d", len(all), len(bodies))
	}
	for i, c := range all {
		if int64(len(bodies[i])) != c.bodyBytes {
			t.Errorf("request %d: bodyBytes = %d, want %d", i, c.bodyBytes, len(bodies[i]))
		}
		if strings.ContainsAny(c.head+c.tail, "ABC"[:1]) && i != 0 {
			if strings.Contains(c.head, "A") {
				t.Errorf("request %d saw the previous request's body: %q", i, c.head)
			}
		}
	}
	if all[1].head != "B" {
		t.Errorf("second observation head = %q, want %q", all[1].head, "B")
	}
}

func TestObserverUnderConcurrency(t *testing.T) {
	obs := &fakeObserver{}
	s := newTestServer(t, func(o *Options) { o.Observer = obs })

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`)); w.Code != 200 {
				t.Errorf("status %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if got := len(obs.all()); got != 50 {
		t.Fatalf("observed %d of 50", got)
	}
	ids := map[string]bool{}
	for _, c := range obs.all() {
		if ids[c.id] {
			t.Fatalf("duplicate observation for request %q", c.id)
		}
		ids[c.id] = true
		if c.head != `{"ok":true}` {
			t.Fatalf("torn capture: %q", c.head)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
