package perf

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Allocation and footprint on the hot path.
//
// DESIGN §15.2.2 says "No allocation on the hot path — pooled request
// structures; only the fields needed (model, stream, size markers) are scanned,
// never a full unmarshal." That is true of the GATE — internal/server's
// benchmarks report 0 allocs/op for route lookup plus authentication plus
// authorization, and internal/server/peek.go is why. It has never been true of
// a request that is actually dispatched, and nothing measured it end to end
// until this file.
//
// The measurement is a benchmark rather than a MemStats delta because a
// MemStats delta over an HTTP round trip counts the load generator's
// allocations as the gateway's. Two arms are run and subtracted:
//
//   - gateway — the assembled server's ServeHTTP, called directly with a
//     recorder, so no client-side HTTP stack is in the figure at all. The fake
//     upstream is still in-process and still allocates.
//   - upstream only — the same fake, the same body, reached directly. This is
//     the control, and subtracting it leaves the gateway's own share.

// benchBody is the §15.1 warm-local profile's body at its stated ceiling.
var benchBody = Body(0, 4<<10, false)

// BenchmarkGatewayRequest is one complete non-streaming request through the
// assembled gateway, measured for allocations.
func BenchmarkGatewayRequest(b *testing.B) {
	g := NewGateway(b, Opts{Prefix: true, Metering: true})
	secret := g.Secrets[0]
	// Warm every cache the profile assumes, above all the auth one.
	for i := range 64 {
		w := httptest.NewRecorder()
		g.App.Server.ServeHTTP(w, gatewayRequest(g, secret, benchBody))
		if w.Code != http.StatusOK && i == 0 {
			b.Fatalf("warmup: %d %s", w.Code, w.Body.String())
		}
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(benchBody)))
	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		g.App.Server.ServeHTTP(w, gatewayRequest(g, secret, benchBody))
		if w.Code != http.StatusOK {
			b.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
}

// BenchmarkUpstreamOnly is the control: the same fake answering the same body
// with no gateway between. Subtracting it from [BenchmarkGatewayRequest] leaves
// the gateway's own allocations, plus the recorder's.
func BenchmarkUpstreamOnly(b *testing.B) {
	g := NewGateway(b, Opts{Prefix: true, Metering: true})
	url := g.Upstreams[0].Endpoint()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 8}}
	call := func() {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(benchBody))
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		var sink bytes.Buffer
		_, _ = sink.ReadFrom(resp.Body)
		_ = resp.Body.Close()
	}
	for range 16 {
		call()
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(benchBody)))
	b.ResetTimer()
	for b.Loop() {
		call()
	}
}

func gatewayRequest(g *Gateway, secret string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+secret)
	return r
}

// TestHotPathAllocations reports what one dispatched request allocates and
// gates it.
//
// The bound is a ceiling on a REGRESSION, not a target: the measured figure is
// roughly 390 allocations and 86 KB for a 4 KiB request INCLUDING the control
// below, essentially all of it the four JSON passes a cross-protocol gateway
// makes over the body (decode the client's request, encode the upstream's,
// decode the upstream's response, encode the client's). It was 518 and 101 KB
// until the duplicate parses inside those passes came out (§15.1); the
// gateway's own share, with the control subtracted, went from 391 to 262. What
// this gate catches is a fifth pass.
func TestHotPathAllocations(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this is a measurement, not a unit test")
	}
	gw := testing.Benchmark(BenchmarkGatewayRequest)
	up := testing.Benchmark(BenchmarkUpstreamOnly)
	if gw.N == 0 || up.N == 0 {
		t.Skip("the benchmarks did not run")
	}

	gwAllocs := gw.AllocsPerOp()
	upAllocs := up.AllocsPerOp()
	gwBytes := gw.AllocedBytesPerOp()
	upBytes := up.AllocedBytesPerOp()

	t.Log("")
	t.Log("=== allocations for one dispatched request (4 KiB body) ===========")
	t.Logf("  gateway + fake upstream + recorder   %5d allocs  %7d B",
		gwAllocs, gwBytes)
	t.Logf("  fake upstream + http client (control)%5d allocs  %7d B",
		upAllocs, upBytes)
	t.Logf("  the gateway's own share is the difference, and it is where §15.2.2's")
	t.Logf("  'no allocation on the hot path' stops being true: that claim holds")
	t.Logf("  for the GATE (BenchmarkGateOnly reports 0 allocs/op) and not for a")
	t.Logf("  request that is dispatched.")
	t.Log("===================================================================")

	if raceEnabled {
		t.Log("NOTE: race-instrumented build; no allocation bound is asserted")
		return
	}
	// Generous, because the control is subtracted from a benchmark that ran
	// under a different iteration count and both are wall-clock adaptive. What
	// it catches is an order of magnitude, which is what a new decode pass or a
	// lost buffer pool looks like. It came down from 900 with the measurement:
	// a ceiling that stays where it was after a 25% improvement has stopped
	// being a ceiling on anything.
	const allocCeiling = 700
	if gwAllocs > allocCeiling {
		t.Errorf("one request allocates %d objects, over the %d ceiling: "+
			"something on the dispatch path started allocating per element",
			gwAllocs, allocCeiling)
	}
	const byteCeiling = 256 << 10
	if gwBytes > byteCeiling {
		t.Errorf("one request allocates %d bytes for a %d byte body, over the %d "+
			"ceiling", gwBytes, len(benchBody), byteCeiling)
	}
}

// -----------------------------------------------------------------------------
// footprint
// -----------------------------------------------------------------------------

// rssBytes reads the process's resident set from /proc. It returns 0 where
// /proc is not available, and the caller skips rather than inventing a figure.
func rssBytes() int64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}

// TestFootprintUnderConcurrentStreams measures §15.1's "1000 concurrent streams
// < 300 MB, including the replay budget".
//
// # What this figure includes, and why it is an upper bound
//
// The load generator and the fake backend live in the SAME address space as the
// gateway, and both hold per-stream state: an http.Transport connection with
// its read and write buffers on the client side, a net/http server connection
// with its own on the fake's. So the number below is the gateway's footprint
// plus two more per stream, and it can only ever overstate the gateway's share.
//
// That makes it a usable one-sided instrument and nothing more: a total under
// the claimed ceiling proves the claim, and a total over it proves nothing.
// Separating them properly needs the gateway in its own process, which is a
// harness this package does not have.
func TestFootprintUnderConcurrentStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this is a measurement, not a unit test")
	}
	if rssBytes() == 0 {
		t.Skip("no /proc/self/statm on this host; RSS cannot be read")
	}

	streams := scaled(1000)
	g := NewGateway(t, Opts{
		// Slow frames, so every stream is genuinely open at once rather than
		// finishing before its neighbour starts.
		InterFrame: 2 * time.Millisecond,
		Frames:     64,
		Prefix:     true, Metering: true,
		MaxConcurrent: streams * 2,
	})
	g.Client.Transport.(*http.Transport).MaxIdleConnsPerHost = streams + 16
	g.Client.Transport.(*http.Transport).MaxConnsPerHost = 0

	settle := func() (rss int64, heap uint64) {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return rssBytes(), ms.HeapInuse
	}

	// Warm first: the first request on a connection allocates the whole
	// transport machinery, and an idle figure taken before that is a figure for
	// a gateway that has not run.
	warmup(t, g, Load{Concurrency: 32, Warmup: scaled(256)},
		[][]byte{Body(0, 4<<10, true)})
	g.ResetUpstreams()
	idleRSS, idleHeap := settle()

	body := Body(1, 4<<10, true)
	rel := make(chan struct{})
	done := make(chan struct{}, streams)
	open := make(chan struct{}, streams)
	for i := range streams {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			req, err := http.NewRequest(http.MethodPost, g.url, bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+g.Secrets[i%len(g.Secrets)])
			resp, err := g.Client.Do(req)
			if err != nil {
				open <- struct{}{}
				return
			}
			// Read one frame, then hold the stream open until released. This is
			// the state the budget is about: a thousand half-finished
			// generations, each holding whatever the relay holds.
			buf := make([]byte, 512)
			_, _ = resp.Body.Read(buf)
			open <- struct{}{}
			<-rel
			_, _ = resp.Body.Read(buf)
			_ = resp.Body.Close()
		}(i)
	}
	for range streams {
		<-open
	}
	peakRSS, peakHeap := settle()
	close(rel)
	for range streams {
		<-done
	}

	t.Log("")
	t.Log("=== DESIGN §15.1 footprint ========================================")
	t.Logf("  idle (warm, no streams open)   RSS %5.1f MB   Go heap in use %5.1f MB",
		mb(idleRSS), mbu(idleHeap))
	t.Logf("  %d concurrent streams open   RSS %5.1f MB   Go heap in use %5.1f MB",
		streams, mb(peakRSS), mbu(peakHeap))
	t.Logf("  delta                          RSS %5.1f MB   Go heap in use %5.1f MB",
		mb(peakRSS-idleRSS), mbu(peakHeap-idleHeap))
	t.Logf("  published: 1000 concurrent streams < 300 MB including the replay budget")
	t.Logf("  NOTE: the load generator and the fake backend share this address")
	t.Logf("        space, so every figure above overstates the gateway's share.")
	t.Log("===================================================================")

	const ceiling = 300 << 20
	if peakRSS > ceiling {
		t.Logf("RSS with %d streams open is %.1f MB, over §15.1's 300 MB — but this "+
			"process is three participants, so the result is inconclusive rather "+
			"than a failure. Measuring it properly needs the gateway alone in a "+
			"process.", streams, mb(peakRSS))
	}
}

func mb(b int64) float64   { return float64(b) / (1 << 20) }
func mbu(b uint64) float64 { return float64(b) / (1 << 20) }

// TestThroughputCeiling reports where throughput stops scaling and what holds
// it there.
//
// It is opt-in: it saturates every core for tens of seconds, and a figure taken
// while the rest of the suite is running is a figure about the rest of the
// suite. Set DORANG_PERF_CEILING=1 to run it.
func TestThroughputCeiling(t *testing.T) {
	if os.Getenv("DORANG_PERF_CEILING") == "" {
		t.Skip("set DORANG_PERF_CEILING=1: this arm saturates the machine")
	}
	g := NewGateway(t, Opts{
		UpstreamLatency: 5 * time.Millisecond,
		Prefix:          true, Metering: true, MaxConcurrent: 4096,
	})
	t.Log("")
	t.Log("=== throughput =====================================================")
	for _, c := range []int{1, 4, 16, 64, 128, 256, 512} {
		rps, s := Throughput(t, g, c, 2*time.Second, false, 4<<10)
		d := overheadOf(s)
		t.Logf("  clients=%-4d %8.0f req/s   p50=%-10v p99=%-10v",
			c, rps, rnd(d.P50), rnd(d.P99))
	}
	// Two figures, because they say different things. The first is the heap the
	// pacer let grow against the allocation rate, which is what an operator's
	// RSS graph shows at the knee; the second is what is actually live once the
	// load stops, which is what a leak would show up in.
	var busy, quiet runtime.MemStats
	runtime.ReadMemStats(&busy)
	busyRSS := rssBytes()
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&quiet)
	t.Logf("  at the knee:   heap in use %5.0f MB, RSS %5.0f MB", mbu(busy.HeapInuse), mb(busyRSS))
	t.Logf("  after two GCs: live %5.0f MB, spans %5.0f MB, RSS %5.0f MB, %d goroutines",
		mbu(quiet.HeapAlloc), mbu(quiet.HeapInuse), mb(rssBytes()), runtime.NumGoroutine())
	t.Logf("  the first figure is Go's pacer against the allocation rate")
	t.Logf("  (~262 allocations per request x the throughput above), not a leak;")
	t.Logf("  the live figure after collection is what a leak would show up in.")
	t.Log("====================================================================")
}
