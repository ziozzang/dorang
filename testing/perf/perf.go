// Package perf measures the gateway's own overhead, separated from the
// upstream's latency.
//
// # Why the separation is the whole problem
//
// DESIGN §15.1 publishes a warm-local p50 of 200 µs and fixes the measurement
// boundary in the same paragraph:
//
//	Gateway overhead = from the last byte of the request line+headers being
//	read, to the first byte written upstream, plus from the last upstream byte
//	to the last byte written to the client. It EXCLUDES upstream time.
//
// A wall-clock stopwatch around a request measures the upstream, which on a
// real backend is three orders of magnitude larger and on a fake one is
// whatever the loopback happens to cost that second. Subtracting two
// distributions does not work either: the median of a difference is not the
// difference of medians, and a p99 needs a per-request figure. So the
// separation has to be per request, and it has to be instrumented rather than
// inferred.
//
// This package instruments it at three points, none of which is inside the
// system under test:
//
//   - t0/t3 — a wrapping [http.Handler] around the assembled gateway, entered
//     after net/http has read the request line and headers and left after the
//     handler's last write. This is exactly §15.1's outer boundary.
//   - the upstream span — an [http.RoundTripper] wrapped around the upstream
//     client the gateway was handed, plus an [httptrace.ClientTrace] for the
//     moment a connection was acquired, plus a wrapper on the response body
//     that times every Read.
//   - the first client byte — a [http.ResponseWriter] wrapper, for the
//     streaming TTFT figure.
//
// Gateway overhead is then
//
//	(t3 - t0) - upstreamWait
//
// where upstreamWait is the time spent inside RoundTrip after a connection was
// in hand, plus every nanosecond blocked in a Read of the upstream body. That
// is a stronger statement than "total minus a fixed sleep": it holds for a
// streaming relay, where the gateway's work is interleaved with the upstream's
// and no fixed subtraction can separate them.
//
// # What is charged to which side
//
// Two boundaries are approximations, and both of them charge gateway work to
// the upstream, so the reported overhead is a slight UNDER-estimate:
//
//   - Writing the upstream request. §15.1 ends the gateway's first half at "the
//     first byte written upstream", and the span starts at GotConn, which is
//     just before that byte. Encoding the upstream body happens earlier and is
//     charged to the gateway, which is correct.
//   - Parsing the upstream response headers, which net/http does inside
//     RoundTrip on the gateway's behalf.
//
// Both are microseconds at most and neither can turn a failing budget into a
// passing one. The load driver also reports end-to-end client latency, which is
// an over-estimate of the same quantity by one loopback hop, so the true figure
// is bracketed rather than asserted.
package perf

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"sort"
	"sync/atomic"
	"time"
)

// -----------------------------------------------------------------------------
// the per-request span
// -----------------------------------------------------------------------------

// span is one request's timing, carried on the request context so that the
// transport can find it.
//
// It is written by exactly one goroutine — the one serving the request — from
// t0 to t3, with the single exception of a streaming relay whose upstream reads
// happen on the same goroutine anyway. No lock, and none needed: the handler
// wrapper hands the span to the sink only after the inner handler has returned.
type span struct {
	t0 time.Time // handler entry: headers already read
	t3 time.Time // handler return: last byte written to the client

	// conns counts upstream round trips. The warm-local profile is one; more
	// than one means a retry or a fallback happened and the sample is not a
	// warm-local sample.
	conns int
	// reused counts round trips that got an already-open connection. A dial in
	// the middle of a measurement is a cold sample.
	reused int

	// upstreamWait is time the gateway spent waiting for the upstream: inside
	// RoundTrip with a connection in hand, plus blocked in Read.
	upstreamWait time.Duration
	// reads counts Read calls on upstream bodies, for the streaming arms.
	reads int

	// firstClientWrite is when the first response byte reached the client.
	firstClientWrite time.Time
	// firstUpstreamByte is when the first byte of upstream response CONTENT was
	// handed back to the gateway — the return of the first Read that produced
	// bytes, not the status line.
	firstUpstreamByte time.Time
	// connAt is when the first round trip got its connection.
	connAt time.Time
	// roundTripStart is when the CURRENT round trip got its connection, and is
	// cleared when that round trip returns. It is separate from connAt because
	// a request that falls back makes two round trips and both of their waits
	// belong to the upstream, while only the first one's start bounds TTFT.
	roundTripStart time.Time

	// bytesOut is what the client received.
	bytesOut int64
}

// Overhead is §15.1's quantity: everything the gateway did, with upstream time
// removed.
func (s *span) Overhead() time.Duration { return s.t3.Sub(s.t0) - s.upstreamWait }

// Total is what the client waited, measured inside the process.
func (s *span) Total() time.Duration { return s.t3.Sub(s.t0) }

// AddedTTFT is the §15.1 streaming figure: how much later the client's first
// byte was than it would have been if the gateway were a wire.
//
// It is (client TTFT) - (upstream TTFT), where upstream TTFT runs from the
// connection being in hand to the first upstream content byte. Everything else
// in the interval is the gateway: the decode, the routing, the capacity
// acquisition, the upstream encode, and the relay of the first frame.
func (s *span) AddedTTFT() (time.Duration, bool) {
	if s.firstClientWrite.IsZero() || s.firstUpstreamByte.IsZero() || s.connAt.IsZero() {
		return 0, false
	}
	client := s.firstClientWrite.Sub(s.t0)
	upstream := s.firstUpstreamByte.Sub(s.connAt)
	return client - upstream, true
}

type spanKey struct{}

func spanFrom(ctx context.Context) *span {
	sp, _ := ctx.Value(spanKey{}).(*span)
	return sp
}

// -----------------------------------------------------------------------------
// the handler wrapper
// -----------------------------------------------------------------------------

// Handler wraps the assembled gateway and records one [span] per request.
//
// It is the outer boundary of §15.1: net/http calls it once the request line
// and headers are read, and it returns once the handler has written its last
// byte. Nothing between those two points is anything but the gateway.
type Handler struct {
	inner http.Handler
	sink  func(*span)
}

// NewHandler wraps h. sink receives every completed span and must not block.
func NewHandler(h http.Handler, sink func(*span)) *Handler {
	return &Handler{inner: h, sink: sink}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sp := &span{}
	ctx := httptrace.WithClientTrace(
		context.WithValue(r.Context(), spanKey{}, sp),
		&httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				sp.conns++
				if info.Reused {
					sp.reused++
				}
				now := time.Now()
				if sp.connAt.IsZero() {
					sp.connAt = now
				}
				sp.roundTripStart = now
			},
		})
	r = r.WithContext(ctx)
	tw := &timedWriter{ResponseWriter: w, sp: sp}

	sp.t0 = time.Now()
	h.inner.ServeHTTP(tw, r)
	sp.t3 = time.Now()

	if h.sink != nil {
		h.sink(sp)
	}
}

// timedWriter records when the client's first byte left and how many followed.
//
// It implements [http.Flusher] unconditionally: the streaming relay flushes per
// frame, and a wrapper that swallowed the assertion would turn every stream in
// the measurement into a buffered response — which is precisely the thing whose
// cost is being measured.
type timedWriter struct {
	http.ResponseWriter
	sp *span
}

func (w *timedWriter) Write(p []byte) (int, error) {
	if w.sp.firstClientWrite.IsZero() {
		w.sp.firstClientWrite = time.Now()
	}
	n, err := w.ResponseWriter.Write(p)
	w.sp.bytesOut += int64(n)
	return n, err
}

func (w *timedWriter) WriteHeader(code int) {
	if w.sp.firstClientWrite.IsZero() {
		w.sp.firstClientWrite = time.Now()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *timedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *timedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("perf: the underlying writer cannot be hijacked")
	}
	return h.Hijack()
}

// Unwrap lets net/http's own helpers reach the real writer.
func (w *timedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// -----------------------------------------------------------------------------
// the upstream transport
// -----------------------------------------------------------------------------

// timedTransport charges upstream time to the upstream.
//
// The span starts at GotConn rather than at RoundTrip entry: acquiring a
// connection is the gateway's own work, and on a warm pool it is a map lookup
// whose cost belongs in the overhead figure rather than hidden in it.
type timedTransport struct{ base http.RoundTripper }

func (t *timedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	sp := spanFrom(r.Context())
	if sp == nil {
		return t.base.RoundTrip(r)
	}
	resp, err := t.base.RoundTrip(r)
	// GotConn set roundTripStart. Anything before it — DNS, dial, TLS — is a
	// cold-connection cost that the warm-local profile excludes by construction
	// and that [span.reused] reports when it happens anyway.
	if !sp.roundTripStart.IsZero() {
		sp.upstreamWait += time.Since(sp.roundTripStart)
		sp.roundTripStart = time.Time{}
	}
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &timedBody{rc: resp.Body, sp: sp}
	return resp, nil
}

// timedBody times every Read of the upstream body.
//
// This is what makes the streaming arms measurable. A relay's gateway work is
// interleaved with the upstream's generation, so no fixed subtraction can
// separate them — but every nanosecond the relay spends waiting for the next
// frame is spent blocked in exactly this Read, and every nanosecond it spends
// scanning, rewriting and forwarding that frame is spent outside it.
type timedBody struct {
	rc interface {
		Read([]byte) (int, error)
		Close() error
	}
	sp *span
}

func (b *timedBody) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := b.rc.Read(p)
	done := time.Now()
	b.sp.upstreamWait += done.Sub(start)
	b.sp.reads++
	if n > 0 && b.sp.firstUpstreamByte.IsZero() {
		b.sp.firstUpstreamByte = done
	}
	return n, err
}

func (b *timedBody) Close() error { return b.rc.Close() }

// UpstreamClient returns an [http.Client] to hand to the gateway as its
// upstream client. It is [backend.NewClient]'s transport with the timing
// wrapper on top, so no transport setting the production client depends on —
// no redirects, no compression, HTTP/2 attempted — is quietly different here.
func UpstreamClient(base *http.Client) *http.Client {
	c := *base
	c.Transport = &timedTransport{base: base.Transport}
	return &c
}

// -----------------------------------------------------------------------------
// samples
// -----------------------------------------------------------------------------

// Sample is one request's measurement, in nanoseconds.
type Sample struct {
	Overhead  time.Duration
	Total     time.Duration
	Upstream  time.Duration
	AddedTTFT time.Duration
	// TTFTValid says whether AddedTTFT means anything: a non-streaming reply
	// has no separate first byte and does not produce one.
	TTFTValid bool
	Conns     int
	Reused    int
	Reads     int
	BytesOut  int64
	// E2E is the client-observed round trip, filled by the driver rather than
	// by the handler. It brackets Overhead from above: it includes the two
	// loopback hops the §15.1 boundary excludes.
	E2E time.Duration
}

// Collector accumulates samples from the handler.
//
// It is sharded and lock-free on the write path, and that is not premature
// tuning — it is a correction. The first version was one mutex over one growing
// slice, and at 20k req/s the WARM mutex profile put 164 ms of contention in
// [Collector.Sink]: the instrument had become the fourth most contended lock in
// the process it was measuring. A measurement whose own bookkeeping serializes
// the path reports its own bookkeeping.
//
// Each shard is a fixed slice claimed by an atomic index. Nothing grows, so no
// shard ever takes the allocator's lock mid-flight; a shard that fills drops
// the sample and says so through [Collector.Dropped], because silently
// truncating the tail of a latency distribution removes exactly the samples a
// p99 is about.
type Collector struct {
	shards [collectorShards]shard
	next   atomic.Uint64
	on     atomic.Bool
	drop   atomic.Int64
}

// collectorShards is a power of two so the index is a mask. It is comfortably
// more than any plausible GOMAXPROCS, so two goroutines sharing a shard is
// rare and costs one contended atomic rather than a park.
const collectorShards = 128

type shard struct {
	n   atomic.Int64
	buf []Sample
	// pad keeps two shards' counters off one cache line. Without it the atomic
	// increments false-share and the instrument is a bus storm.
	_ [64 - 8]byte
}

// NewCollector returns a Collector that is not yet recording.
func NewCollector() *Collector { return &Collector{} }

// Start begins recording and discards anything already held. capacity is the
// total number of samples to make room for, divided evenly across the shards.
func (c *Collector) Start(capacity int) {
	per := capacity/collectorShards + 8
	for i := range c.shards {
		c.shards[i].buf = make([]Sample, per)
		c.shards[i].n.Store(0)
	}
	c.drop.Store(0)
	c.on.Store(true)
}

// Stop ends recording.
func (c *Collector) Stop() { c.on.Store(false) }

// Dropped is how many samples did not fit.
func (c *Collector) Dropped() int64 { return c.drop.Load() }

// Sink is the [NewHandler] callback.
func (c *Collector) Sink(sp *span) {
	if !c.on.Load() {
		return
	}
	added, ok := sp.AddedTTFT()
	s := Sample{
		Overhead: sp.Overhead(), Total: sp.Total(), Upstream: sp.upstreamWait,
		AddedTTFT: added, TTFTValid: ok,
		Conns: sp.conns, Reused: sp.reused, Reads: sp.reads, BytesOut: sp.bytesOut,
	}
	sh := &c.shards[c.next.Add(1)&(collectorShards-1)]
	i := sh.n.Add(1) - 1
	if i >= int64(len(sh.buf)) {
		c.drop.Add(1)
		return
	}
	sh.buf[i] = s
}

// Samples returns a copy of what was recorded.
func (c *Collector) Samples() []Sample {
	var out []Sample
	for i := range c.shards {
		n := c.shards[i].n.Load()
		if n > int64(len(c.shards[i].buf)) {
			n = int64(len(c.shards[i].buf))
		}
		out = append(out, c.shards[i].buf[:n]...)
	}
	return out
}

// -----------------------------------------------------------------------------
// distributions
// -----------------------------------------------------------------------------

// Dist is a latency distribution.
type Dist struct {
	N               int
	Min, Max        time.Duration
	P50, P90, P95   time.Duration
	P99, P999       time.Duration
	Mean            time.Duration
	Name            string
	NonWarm         int
	MultipleAttempt int
}

// Percentile returns the p-th percentile of a sorted slice, p in [0,1].
//
// Nearest-rank, not interpolated: an interpolated p99 of 200 samples invents a
// value between two observations and reports a latency nothing experienced.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted))*p+0.5) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// Describe summarizes one field of a sample set.
func Describe(name string, samples []Sample, pick func(Sample) time.Duration) Dist {
	vals := make([]time.Duration, 0, len(samples))
	d := Dist{Name: name}
	var sum time.Duration
	for _, s := range samples {
		vals = append(vals, pick(s))
		sum += pick(s)
		if s.Conns > 0 && s.Reused < s.Conns {
			d.NonWarm++
		}
		if s.Conns > 1 {
			d.MultipleAttempt++
		}
	}
	if len(vals) == 0 {
		return d
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	d.N = len(vals)
	d.Min, d.Max = vals[0], vals[len(vals)-1]
	d.P50 = percentile(vals, 0.50)
	d.P90 = percentile(vals, 0.90)
	d.P95 = percentile(vals, 0.95)
	d.P99 = percentile(vals, 0.99)
	d.P999 = percentile(vals, 0.999)
	d.Mean = sum / time.Duration(len(vals))
	return d
}

func (d Dist) String() string {
	return fmt.Sprintf("%-34s n=%-6d p50=%-9v p90=%-9v p95=%-9v p99=%-9v p99.9=%-9v max=%v",
		d.Name, d.N, rnd(d.P50), rnd(d.P90), rnd(d.P95), rnd(d.P99), rnd(d.P999), rnd(d.Max))
}

// rnd trims a duration to three significant figures so a table of them lines
// up and nobody reads meaning into the last nanosecond.
func rnd(d time.Duration) time.Duration {
	switch {
	case d > time.Millisecond:
		return d.Round(time.Microsecond)
	case d > time.Microsecond:
		return d.Round(10 * time.Nanosecond)
	default:
		return d
	}
}

// sortDurations sorts in place.
func sortDurations(d []time.Duration) {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
}

// sortSamplesByTotal sorts by in-process total, which is the rank the
// client-observed times are paired against.
func sortSamplesByTotal(s []Sample) {
	sort.Slice(s, func(i, j int) bool { return s[i].Total < s[j].Total })
}
