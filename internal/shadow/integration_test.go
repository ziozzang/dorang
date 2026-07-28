package shadow

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/server"
)

// The tests here run the whole path: an HTTP request into internal/server, the
// capture tap, the queue, a real reference gateway over TCP, the structural
// diff, and the report. Everything below the package boundary is exercised by
// the unit tests; these prove the pieces fit and that the one property §14.1
// cannot compromise on — that none of it touches the client — actually holds.

type testPrincipal struct{}

func (testPrincipal) KeyID() string                 { return "key-1" }
func (testPrincipal) UserID() string                { return "user-1" }
func (testPrincipal) TeamID() string                { return "team-1" }
func (testPrincipal) Authorize(server.Access) error { return nil }
func (testPrincipal) AllowsModel(string) bool       { return true }

type testAuth struct{}

func (testAuth) AuthenticateHeader(context.Context, http.Header) (server.Principal, error) {
	return testPrincipal{}, nil
}

// countingMeter is the ledger, for the one-row assertion.
type countingMeter struct {
	n atomic.Int64
	// costs records what each row was charged, so a shadowed run can be
	// compared against an unshadowed one.
	mu    sync.Mutex
	costs []int64
}

func (m *countingMeter) Record(ev server.Event) {
	m.n.Add(1)
	m.mu.Lock()
	m.costs = append(m.costs, ev.Result.CostNanoUSD)
	m.mu.Unlock()
}

const chatBody = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// newGateway builds a dorang HTTP surface answering body, optionally shadowed.
func newGateway(t *testing.T, obs server.Observer, meter server.Meter, body string) *server.Server {
	t.Helper()
	srv, err := server.New(server.Options{
		Auth:  testAuth{},
		Meter: meter,
		Dispatcher: server.DispatchFunc(func(_ context.Context, rq *server.Request, w http.ResponseWriter) error {
			rq.Result.Deployment = "dep-1"
			rq.Result.UpstreamModel = "upstream-m"
			rq.Result.CostNanoUSD = 1_000_000
			rq.Result.Priced = true
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, body)
			return err
		}),
		Observer: obs,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv
}

func chatRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer client-key") // pragma: allowlist secret — a test literal
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestClientLatencyIsUnchangedByAReferenceThatBlocksForever(t *testing.T) {
	// §14.1: "a slow or broken reference must not add a microsecond to what the
	// client sees". The reference here never answers at all.
	release := make(chan struct{})
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(ref.Close)
	t.Cleanup(func() { close(release) })

	const n = 200

	// Baseline: the same gateway with no observer at all.
	plain := newGateway(t, nil, &countingMeter{}, chatBody)
	base := timeRequests(t, plain, n)

	s, err := New(Options{
		Mode: ModeCompare, ReferenceURL: ref.URL, SampleRate: 1, Structural: true,
		MaxCostNanoUSDPerDay: 1_000_000_000_000, ReportWriter: &syncWriter{},
		ReferenceTimeout: time.Hour, Workers: 4, QueueSize: 16,
		CloseGrace: 100 * time.Millisecond, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	shadowed := newGateway(t, s, &countingMeter{}, chatBody)
	with := timeRequests(t, shadowed, n)

	// Four workers are stuck in the reference call forever and the queue holds
	// sixteen more. If any of that were on the request path, `with` would be
	// n × forever rather than a handful of milliseconds.
	if with.total > time.Second {
		t.Fatalf("%d requests took %v against a reference that never answers", n, with.total)
	}
	if with.max > 250*time.Millisecond {
		t.Fatalf("slowest request took %v; a hanging reference must not reach the client", with.max)
	}
	// A generous comparison against the baseline: the capture tap is a memcpy
	// and a queue push, so some overhead is real, but it is not proportional to
	// the reference's latency.
	slack := 20*time.Millisecond + 8*base.total
	if with.total > slack {
		t.Errorf("shadowed %v vs baseline %v (allowed %v)", with.total, base.total, slack)
	}
	if st := s.Stats(); st.Dropped == 0 {
		t.Error("a bounded queue against a hanging reference must produce visible drops")
	}
}

type timing struct {
	total time.Duration
	max   time.Duration
}

func timeRequests(t *testing.T, srv *server.Server, n int) timing {
	t.Helper()
	var out timing
	start := time.Now()
	for i := 0; i < n; i++ {
		r := chatRequest()
		r.Header.Set("X-Request-Id", fmt.Sprintf("lat-%d", i))
		w := httptest.NewRecorder()
		t0 := time.Now()
		srv.ServeHTTP(w, r)
		if d := time.Since(t0); d > out.max {
			out.max = d
		}
		if w.Code != 200 {
			t.Fatalf("request %d: status %d (%s)", i, w.Code, w.Body.String())
		}
	}
	out.total = time.Since(start)
	return out
}

func TestBrokenReferenceNeverFailsAClientRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  func(t *testing.T) string
	}{
		{"500", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(500)
			}))
			t.Cleanup(srv.Close)
			return srv.URL
		}},
		{"connection reset", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hj, ok := w.(http.Hijacker); ok {
					c, _, _ := hj.Hijack()
					_ = c.Close()
				}
			}))
			t.Cleanup(srv.Close)
			return srv.URL
		}},
		{"unreachable", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			u := srv.URL
			srv.Close()
			return u
		}},
		{"garbage body", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("\x00\x01\x02not json at all"))
			}))
			t.Cleanup(srv.Close)
			return srv.URL
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &syncWriter{}
			s, err := New(Options{
				Mode: ModeCompare, ReferenceURL: tc.ref(t), SampleRate: 1, Structural: true,
				MaxCostNanoUSDPerDay: 1_000_000_000_000, ReportWriter: w,
				ReferenceTimeout: 500 * time.Millisecond, Workers: 2, QueueSize: 16,
				CloseGrace: time.Second, Logf: func(string, ...any) {},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })

			meter := &countingMeter{}
			gw := newGateway(t, s, meter, chatBody)
			for i := 0; i < 5; i++ {
				r := chatRequest()
				r.Header.Set("X-Request-Id", fmt.Sprintf("%s-%d", tc.name, i))
				rec := httptest.NewRecorder()
				gw.ServeHTTP(rec, r)
				if rec.Code != 200 {
					t.Fatalf("request %d answered %d; a broken reference must never fail a client request",
						i, rec.Code)
				}
				if rec.Body.String() != chatBody {
					t.Fatalf("request %d body changed under shadowing", i)
				}
			}
			if got := meter.n.Load(); got != 5 {
				t.Fatalf("5 requests produced %d ledger rows", got)
			}
			waitFor(t, 5*time.Second, "the reference calls to resolve", func() bool {
				st := s.Stats()
				return st.Sent >= 5
			})
			if st := s.Stats(); st.Clean != 0 {
				t.Errorf("clean = %d; a broken reference is never a clean comparison", st.Clean)
			}
		})
	}
}

func TestEndToEndComparisonFindsARealDivergence(t *testing.T) {
	// The reference is a plausible incumbent: same JSON, but its terminal chunk
	// carries no usage and its error code is a number rather than a string
	// (COMPATIBILITY §11.1). Both are things a client breaks on.
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderShadow) == "" {
			t.Error("the reference received an unmarked shadow call")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"other","object":"chat.completion","created":9,"model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"a totally different answer"},` +
			`"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(ref.Close)

	w := &syncWriter{}
	s, err := New(Options{
		Mode: ModeCompare, ReferenceURL: ref.URL, SampleRate: 1, Structural: true,
		MaxCostNanoUSDPerDay: 1_000_000_000_000, ReportWriter: w,
		Workers: 1, QueueSize: 8, CloseGrace: time.Second, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	meter := &countingMeter{}
	gw := newGateway(t, s, meter, chatBody)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, chatRequest())
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}

	waitFor(t, 5*time.Second, "the comparison", func() bool { return s.Stats().Compared > 0 })
	recs := w.lines()
	if len(recs) != 1 {
		t.Fatalf("want one report record, got %d: %+v", len(recs), recs)
	}
	got := map[string]bool{}
	for _, d := range recs[0].Diffs {
		got[d.Path] = true
	}
	for _, want := range []string{"$.usage.total_tokens", "usage"} {
		if !got[want] {
			t.Errorf("missing diff at %q; got %v", want, got)
		}
	}
	// And the two things that vary between any two answers do not appear.
	for _, unwanted := range []string{"$.id", "$.created", "$.choices[].message.content"} {
		if got[unwanted] {
			t.Errorf("%q must not be a difference: model output and ids are never compared", unwanted)
		}
	}
	// One request, one ledger row, unchanged cost.
	if n := meter.n.Load(); n != 1 {
		t.Fatalf("one shadowed request produced %d ledger rows", n)
	}
	meter.mu.Lock()
	defer meter.mu.Unlock()
	if meter.costs[0] != 1_000_000 {
		t.Errorf("the ledger row's cost is %d; the reference call must not appear in it", meter.costs[0])
	}
}

func TestEndToEndStreamComparison(t *testing.T) {
	const dorangStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	// The incumbent forgets the [DONE] sentinel, which hangs every client that
	// waits for it (COMPATIBILITY §1.2).
	const refStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"zzz\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"

	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, refStream)
	}))
	t.Cleanup(ref.Close)

	w := &syncWriter{}
	s, err := New(Options{
		Mode: ModeCompare, ReferenceURL: ref.URL, SampleRate: 1, Structural: true,
		MaxCostNanoUSDPerDay: 1_000_000_000_000, ReportWriter: w,
		Workers: 1, QueueSize: 8, CloseGrace: time.Second, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	srv, err := server.New(server.Options{
		Auth:     testAuth{},
		Observer: s,
		Dispatcher: server.DispatchFunc(func(_ context.Context, rq *server.Request, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			_, err := io.WriteString(w, dorangStream)
			return err
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, chatRequest())
	if rec.Body.String() != dorangStream {
		t.Fatal("the client's stream changed under shadowing")
	}

	waitFor(t, 5*time.Second, "the comparison", func() bool { return s.Stats().Compared > 0 })
	recs := w.lines()
	if len(recs) != 1 {
		t.Fatalf("want one record, got %d: %+v", len(recs), recs)
	}
	var sawTerminator bool
	for _, d := range recs[0].Diffs {
		if d.Path == "stream/terminator" {
			sawTerminator = true
			if d.Dorang != "done" || d.Reference != "finish" {
				t.Errorf("terminator diff = %+v", d)
			}
		}
	}
	if !sawTerminator {
		t.Fatalf("a missing [DONE] must be reported: %+v", recs[0].Diffs)
	}
	if s.Stats().Clean != 0 {
		t.Error("this comparison is not clean")
	}
}

func TestIdenticalGatewaysProduceAnEmptyReport(t *testing.T) {
	// The completion criterion, end to end: two gateways answering the same
	// shape write nothing at all.
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Different id, different created, different text, different token
		// counts — everything that legitimately varies.
		_, _ = w.Write([]byte(`{"id":"chatcmpl-999","object":"chat.completion","created":88,"model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"an entirely different completion"},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":9,"total_tokens":49}}`))
	}))
	t.Cleanup(ref.Close)

	w := &syncWriter{}
	s, err := New(Options{
		Mode: ModeCompare, ReferenceURL: ref.URL, SampleRate: 1, Structural: true,
		MaxCostNanoUSDPerDay: 1_000_000_000_000, ReportWriter: w,
		Workers: 2, QueueSize: 32, CloseGrace: 2 * time.Second, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	gw := newGateway(t, s, &countingMeter{}, chatBody)
	const n = 25
	for i := 0; i < n; i++ {
		r := chatRequest()
		r.Header.Set("X-Request-Id", fmt.Sprintf("same-%d", i))
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, r)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st := s.Stats()
	if st.Compared != n || st.Clean != n {
		t.Fatalf("compared %d clean %d of %d: %+v", st.Compared, st.Clean, n, st)
	}
	if recs := w.lines(); len(recs) != 0 {
		t.Fatalf("an empty diff report is the completion criterion; got %+v", recs)
	}
	if v := gateVerdict(st); v != "clean" {
		t.Errorf("gate verdict = %q, want clean", v)
	}
}

func TestSamplingFractionIsHonoredEndToEnd(t *testing.T) {
	var refHits atomic.Int64
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody))
	}))
	t.Cleanup(ref.Close)

	const rate = 0.1
	const n = 3000
	s, err := New(Options{
		Mode: ModeCompare, ReferenceURL: ref.URL, SampleRate: rate, Structural: true,
		MaxCostNanoUSDPerDay: 1_000_000_000_000, ReportWriter: &syncWriter{},
		Workers: 8, QueueSize: 1024, CloseGrace: 5 * time.Second, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	meter := &countingMeter{}
	gw := newGateway(t, s, meter, chatBody)
	for i := 0; i < n; i++ {
		r := chatRequest()
		r.Header.Set("X-Request-Id", fmt.Sprintf("frac-%d", i))
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, r)
		if rec.Code != 200 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Every request produced exactly one ledger row, whether or not it was
	// shadowed. This is the whole accounting claim in one assertion.
	if got := meter.n.Load(); got != n {
		t.Fatalf("%d requests produced %d ledger rows", n, got)
	}
	st := s.Stats()
	frac := float64(st.Sampled) / n
	if frac < rate*0.7 || frac > rate*1.3 {
		t.Errorf("sampled %v of traffic, want ~%v", frac, rate)
	}
	if int64(st.Queued) != refHits.Load() {
		t.Errorf("queued %d but the reference saw %d", st.Queued, refHits.Load())
	}
	// And the reference saw roughly the sampled fraction, not all of it.
	if refHits.Load() > int64(float64(n)*rate*1.5) {
		t.Errorf("the reference saw %d of %d requests at rate %v", refHits.Load(), n, rate)
	}
}
