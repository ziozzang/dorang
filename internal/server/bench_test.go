package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The gateway-overhead budget is DESIGN §15.1's warm-local p50 for the whole
// gateway — auth, routing, capacity, pricing, parameter conversion and metering
// enqueue — MEASURED at 480 µs by testing/perf, against 200 µs published. This
// package is the HTTP surface only, so what these benchmarks report is how much
// of that budget the surface consumes before the router or the upstream have
// done anything, and the answer is: almost none of it. The whole non-streaming
// handler with a nop dispatcher is ~1.6 µs, a third of one percent.

// benchWriter is a response writer that neither allocates nor keeps anything,
// so a benchmark measures the handler rather than a recorder.
type benchWriter struct {
	hdr http.Header
	n   int64
}

func (b *benchWriter) Header() http.Header         { return b.hdr }
func (b *benchWriter) WriteHeader(int)             {}
func (b *benchWriter) Write(p []byte) (int, error) { b.n += int64(len(p)); return len(p), nil }

// benchServer is the server under test, with fakes that do the least work an
// honest implementation could.
func benchServer(b *testing.B, mut func(*Options)) *Server {
	b.Helper()
	opts := Options{
		Auth:       &staticAuth{token: "good", p: &fakePrincipal{id: "k"}},
		Dispatcher: jsonDispatcher(`{"id":"chatcmpl-bench","object":"chat.completion","choices":[]}`),
		Models:     ModelSlice{{ID: "model-x"}, {ID: "model-y"}},
	}
	if mut != nil {
		mut(&opts)
	}
	s, err := New(opts)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	return s
}

// body4KiB is the §15.1 warm-local profile's body size: "≤ 4 KiB body".
var body4KiB = `{"model":"model-x","messages":[{"role":"user","content":"` +
	strings.Repeat("x", 4000) + `"}]}`

const bodySmall = `{"model":"model-x","messages":[{"role":"user","content":"hello"}]}`

// BenchmarkNonStreamingHandler is the headline number: one complete
// non-streaming request through the HTTP surface end to end — route, six-header
// auth, body cap, model scan, authorization, dispatch, extension headers,
// metering enqueue.
func BenchmarkNonStreamingHandler(b *testing.B) {
	for _, c := range []struct {
		name string
		body string
	}{
		{"small", bodySmall},
		{"4KiB", body4KiB},
	} {
		b.Run(c.name, func(b *testing.B) {
			s := benchServer(b, nil)
			w := &benchWriter{hdr: make(http.Header, 8)}
			b.ReportAllocs()
			b.SetBytes(int64(len(c.body)))
			// The request is built once: httptest.NewRequest costs more
			// than the handler does, and a benchmark that rebuilds it every
			// iteration reports the constructor.
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			r.Header.Set(HeaderAuthorization, "Bearer good")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Body = io.NopCloser(strings.NewReader(c.body))
				r.ContentLength = int64(len(c.body))
				s.ServeHTTP(w, r)
			}
		})
	}
}

// BenchmarkNonStreamingHandlerParallel runs the same path on every core, which
// is where a shared mutex or a contended pool would show up and a single-thread
// benchmark would not.
func BenchmarkNonStreamingHandlerParallel(b *testing.B) {
	s := benchServer(b, nil)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		w := &benchWriter{hdr: make(http.Header, 8)}
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set(HeaderAuthorization, "Bearer good")
		for pb.Next() {
			r.Body = io.NopCloser(strings.NewReader(bodySmall))
			r.ContentLength = int64(len(bodySmall))
			s.ServeHTTP(w, r)
		}
	})
}

// BenchmarkGateOnly isolates the part the surface is actually responsible for:
// route lookup plus authentication plus authorization, with no body, no
// dispatch and no response. This is the number that must not grow when a route
// or a key is added.
func BenchmarkGateOnly(b *testing.B) {
	s := benchServer(b, nil)
	cfg := s.snap.Load()
	h := http.Header{}
	h.Set(HeaderAuthorization, "Bearer good")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rq := s.acquire()
		rt, np, res := cfg.routes.lookup(http.MethodPost, "/v1/chat/completions", &rq.params)
		if res != lookupHit {
			b.Fatal("route miss")
		}
		rq.Route, rq.nparams = rt, np
		p, err := cfg.auth.AuthenticateHeader(ctx, h)
		if err != nil {
			b.Fatal(err)
		}
		if err := p.Authorize(Access{Model: "model-x", Route: "/v1/chat/completions"}); err != nil {
			b.Fatal(err)
		}
		s.release(rq)
	}
}

// BenchmarkRouteLookup separates the exact-map hit that every T0 path takes
// from the specificity scan that a patterned route takes, because the second
// one is the cost of COMPATIBILITY §7.5 and is worth knowing.
func BenchmarkRouteLookup(b *testing.B) {
	s := benchServer(b, func(o *Options) {
		o.Passthrough = []PassthroughRoute{
			{Prefix: "/anthropic", Provider: "p", BaseURL: "http://127.0.0.1:1"},
			{Prefix: "/vllm", Provider: "p", BaseURL: "http://127.0.0.1:1"},
		}
		o.Routes = []Route{{
			Pattern: "/openai/deployments/{model}/chat/completions",
			Methods: MethodPOST, Name: "azure",
			Handler: func(http.ResponseWriter, *Request) error { return nil },
		}}
	})
	table := s.snap.Load().routes
	var params [maxParams]Param

	b.Run("exact", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			table.lookup(http.MethodPost, "/v1/chat/completions", &params)
		}
	})
	b.Run("patterned", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			table.lookup(http.MethodPost, "/openai/deployments/gpt-4o/chat/completions", &params)
		}
	})
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			table.lookup(http.MethodPost, "/no/such/route/at/all", &params)
		}
	})
}

// BenchmarkPeekRequest is the body scan that replaces a full unmarshal
// (DESIGN §15.2.2). The comparison that matters is against json.Unmarshal of
// the same body, which is what a gateway that skipped this optimization pays.
func BenchmarkPeekRequest(b *testing.B) {
	body := []byte(body4KiB)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if m, _, ok := peekRequest(body); !ok || m != "model-x" {
			b.Fatal("scan failed")
		}
	}
}

// BenchmarkErrorEnvelope is the normalizer plus the encoder, which run on every
// refused request and are therefore on a path a hostile client controls.
func BenchmarkErrorEnvelope(b *testing.B) {
	upstream := []byte(`{"object":"error","message":"the request was malformed","type":"BadRequestError","param":null,"code":400}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		e := Normalize(400, upstream)
		if len(EncodeError(e)) == 0 {
			b.Fatal("empty envelope")
		}
	}
}

// BenchmarkModelList measures GET /v1/models with an allow-list filter, which
// tooling polls far more often than anyone expects.
func BenchmarkModelList(b *testing.B) {
	models := make(ModelSlice, 200)
	for i := range models {
		models[i] = Model{ID: "model-" + strings.Repeat("x", i%17), OwnedBy: "owner"}
	}
	s := benchServer(b, func(o *Options) { o.Models = models })
	w := &benchWriter{hdr: make(http.Header, 8)}
	b.ReportAllocs()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set(HeaderAuthorization, "Bearer good")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.ServeHTTP(w, r)
	}
}

// BenchmarkStreamingRelay measures the per-frame cost of the response wrapper,
// which sits between every streamed byte and the client. DESIGN §15.1 budgets
// added TTFT at p99 < 1 ms, so the wrapper's share of it must be invisible.
func BenchmarkStreamingRelay(b *testing.B) {
	frame := []byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\"," +
		"\"created\":1700000000,\"model\":\"model-x\"," +
		"\"choices\":[{\"index\":0,\"delta\":{\"content\":\"token\"}}]}\n\n")
	const frames = 64

	s := benchServer(b, func(o *Options) {
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; i < frames; i++ {
				if _, err := w.Write(frame); err != nil {
					return err
				}
			}
			_, err := io.WriteString(w, "data: [DONE]\n\n")
			return err
		})
	})
	w := &benchWriter{hdr: make(http.Header, 8)}
	b.ReportAllocs()
	b.SetBytes(int64(len(frame) * frames))
	const streamBody = `{"model":"model-x","stream":true}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(HeaderAuthorization, "Bearer good")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Body = io.NopCloser(strings.NewReader(streamBody))
		r.ContentLength = int64(len(streamBody))
		s.ServeHTTP(w, r)
	}
}
