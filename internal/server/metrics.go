package server

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// durationBuckets are the cumulative histogram bounds in seconds.
//
// They are dense around the warm-local p50 and p99 of DESIGN §15.1 — measured
// at 249 µs and 2 ms — because a histogram whose lowest bucket is 5 ms cannot
// tell anyone whether that target is being met, which is the only question this
// histogram exists to answer.
//
// The bounds were chosen when §15.1 published a 200 µs p50, and the figure has
// been corrected three times since: to 480 µs when it was first measured, then
// to 375 µs and 249 µs as the codec got faster twice. They are left alone
// anyway, and the honest reason is not that they still bracket it well — they
// do not. 249 µs falls in (200 µs, 500 µs], a bucket 2.5× wide, and interpolating
// across it recovers the median about 29% high. It is left alone because this
// block is the zero-configuration fallback: [handleMetrics] serves it only when
// no [Options.Metrics] registry is configured, and every assembled gateway wires
// one. internal/metrics.DurationBounds is what an operator actually scrapes, it
// has bounds at 50/100/150/200/300 µs, and it recovers the same median within
// 2%. Sharpening a fallback nobody scrapes would buy nothing and would change
// an exported bucket layout.
var durationBuckets = [...]float64{
	0.0001, 0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.025,
	0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
}

// metrics is the fixed-cardinality counter set behind GET /metrics.
//
// Nothing here is labelled by a request path, a model name or a key id. A
// gateway that labels a counter with a caller-supplied string has handed every
// caller a way to grow its own memory (and its scrape) without limit; the
// per-model and per-key numbers belong in the ledger, which is what DESIGN §9
// is for.
type metrics struct {
	requests      atomic.Uint64
	byClass       [6]atomic.Uint64 // index 0 unused, 1xx..5xx
	bytesIn       atomic.Uint64
	bytesOut      atomic.Uint64
	buckets       [len(durationBuckets) + 1]atomic.Uint64
	durationSumNS atomic.Uint64

	unimplemented atomic.Uint64
	authFailures  atomic.Uint64
	lateErrors    atomic.Uint64
	panics        atomic.Uint64
	meterPanics   atomic.Uint64

	passthrough   atomic.Uint64
	wsUpgrades    atomic.Uint64
	replayRefused atomic.Uint64
	bodyTooLarge  atomic.Uint64

	observed       atomic.Uint64
	observerPanics atomic.Uint64
}

// observe records one finished request.
func (m *metrics) observe(status int, d time.Duration, in, out int64) {
	m.requests.Add(1)
	class := status / 100
	if class >= 1 && class <= 5 {
		m.byClass[class].Add(1)
	}
	m.bytesIn.Add(uint64(in))
	m.bytesOut.Add(uint64(out))
	m.durationSumNS.Add(uint64(d.Nanoseconds()))

	secs := d.Seconds()
	i := 0
	for ; i < len(durationBuckets); i++ {
		if secs <= durationBuckets[i] {
			break
		}
	}
	m.buckets[i].Add(1)
}

// Stats is a point-in-time view of the HTTP surface's own counters.
//
// It exists so that the complete DESIGN §12.3 surface can be assembled outside
// this package without moving these counters out of it or exporting the fields
// they live in. internal/metrics renders them; this package keeps owning them.
type Stats struct {
	// Requests is every request the surface finished, health probes and the
	// metrics scrape included.
	Requests uint64
	// ByClass counts responses by status class; index 0 is unused.
	ByClass [6]uint64

	BytesIn  uint64
	BytesOut uint64
	// DurationSumNS is the summed gateway duration. The distribution is
	// rendered by internal/metrics, which keeps it per model.
	DurationSumNS uint64

	Unimplemented uint64
	AuthFailures  uint64
	LateErrors    uint64
	Panics        uint64
	MeterPanics   uint64

	Passthrough   uint64
	WSUpgrades    uint64
	ReplayRefused uint64
	BodyTooLarge  uint64

	Observed       uint64
	ObserverPanics uint64

	InFlight    int64
	ReplayBytes int64
	// ReplayLimit is the process-wide replay budget of DESIGN §15.4.
	ReplayLimit int64
	Ready       bool
	Uptime      time.Duration
}

// Stats returns the surface's counters. It takes no lock: every field is an
// atomic, which is what lets a metrics scrape read them while the configuration
// mutex is held (TestSnapshotReadTakesNoLock is the same property from the
// other side).
func (s *Server) Stats() Stats {
	m := &s.metrics
	return Stats{
		Requests: m.requests.Load(),
		ByClass: [6]uint64{
			0, m.byClass[1].Load(), m.byClass[2].Load(), m.byClass[3].Load(),
			m.byClass[4].Load(), m.byClass[5].Load(),
		},
		BytesIn:        m.bytesIn.Load(),
		BytesOut:       m.bytesOut.Load(),
		DurationSumNS:  m.durationSumNS.Load(),
		Unimplemented:  m.unimplemented.Load(),
		AuthFailures:   m.authFailures.Load(),
		LateErrors:     m.lateErrors.Load(),
		Panics:         m.panics.Load(),
		MeterPanics:    m.meterPanics.Load(),
		Passthrough:    m.passthrough.Load(),
		WSUpgrades:     m.wsUpgrades.Load(),
		ReplayRefused:  m.replayRefused.Load(),
		BodyTooLarge:   m.bodyTooLarge.Load(),
		Observed:       m.observed.Load(),
		ObserverPanics: m.observerPanics.Load(),
		InFlight:       s.inflight.Load(),
		ReplayBytes:    s.replay.Used(),
		ReplayLimit:    s.replay.Limit(),
		Ready:          s.Ready(),
		Uptime:         time.Since(s.started),
	}
}

// handleMetrics serves the Prometheus text exposition format.
//
// Hand-rolled, because a metrics endpoint is not worth a dependency and because
// DESIGN §0.2 requires the notebook profile to have zero required dependencies.
//
// When [Options.Metrics] is configured, the whole body comes from there instead.
// It is a replacement rather than an addition on purpose: internal/metrics
// renders `dorang_requests_total` with the labels DESIGN §12.3 specifies, and
// emitting the unlabelled family below alongside it would put two `# TYPE`
// lines for one name on the page, which makes Prometheus reject the entire
// scrape. The built-in block stays as the zero-configuration default so that a
// server assembled without a registry still has a usable endpoint.
func (s *Server) handleMetrics(w http.ResponseWriter, rq *Request) error {
	m := &s.metrics
	buf := getBuf()
	defer putBuf(buf)
	b := *buf

	if src := rq.srv.snap.Load().metrics; src != nil {
		b = src.Metrics(b)
		return writeExposition(w, rq, buf, b)
	}

	b = counter(b, "dorang_requests_total",
		"Requests served by the HTTP surface.", m.requests.Load())
	for class := 1; class <= 5; class++ {
		if class == 1 {
			b = append(b, "# HELP dorang_responses_total Responses by status class.\n"...)
			b = append(b, "# TYPE dorang_responses_total counter\n"...)
		}
		b = append(b, "dorang_responses_total{class=\""...)
		b = strconv.AppendInt(b, int64(class), 10)
		b = append(b, "xx\"} "...)
		b = strconv.AppendUint(b, m.byClass[class].Load(), 10)
		b = append(b, '\n')
	}

	b = append(b, "# HELP dorang_request_duration_seconds Gateway request duration.\n"...)
	b = append(b, "# TYPE dorang_request_duration_seconds histogram\n"...)
	var cum uint64
	for i, bound := range durationBuckets {
		cum += m.buckets[i].Load()
		b = append(b, "dorang_request_duration_seconds_bucket{le=\""...)
		b = strconv.AppendFloat(b, bound, 'g', -1, 64)
		b = append(b, "\"} "...)
		b = strconv.AppendUint(b, cum, 10)
		b = append(b, '\n')
	}
	cum += m.buckets[len(durationBuckets)].Load()
	b = append(b, "dorang_request_duration_seconds_bucket{le=\"+Inf\"} "...)
	b = strconv.AppendUint(b, cum, 10)
	b = append(b, "\ndorang_request_duration_seconds_sum "...)
	b = strconv.AppendFloat(b, float64(m.durationSumNS.Load())/1e9, 'g', -1, 64)
	b = append(b, "\ndorang_request_duration_seconds_count "...)
	b = strconv.AppendUint(b, cum, 10)
	b = append(b, '\n')

	b = counter(b, "dorang_request_bytes_total",
		"Request body bytes read.", m.bytesIn.Load())
	b = counter(b, "dorang_response_bytes_total",
		"Response body bytes written.", m.bytesOut.Load())
	b = counter(b, "dorang_unimplemented_total",
		"Requests answered 501 because the route is not implemented.", m.unimplemented.Load())
	b = counter(b, "dorang_auth_failures_total",
		"Requests refused by authentication or authorization.", m.authFailures.Load())
	b = counter(b, "dorang_late_errors_total",
		"Errors delivered in band because the response had already started.", m.lateErrors.Load())
	b = counter(b, "dorang_handler_panics_total",
		"Panics recovered in a handler.", m.panics.Load())
	b = counter(b, "dorang_meter_panics_total",
		"Panics recovered in the meter; requests were unaffected.", m.meterPanics.Load())
	b = counter(b, "dorang_passthrough_requests_total",
		"Requests served by the generic passthrough engine.", m.passthrough.Load())
	b = counter(b, "dorang_websocket_upgrades_total",
		"WebSocket upgrades relayed.", m.wsUpgrades.Load())
	b = counter(b, "dorang_replay_refused_total",
		"Requests marked non-replayable because the replay budget was full.", m.replayRefused.Load())
	b = counter(b, "dorang_body_too_large_total",
		"Requests refused for exceeding max_body_bytes.", m.bodyTooLarge.Load())
	b = counter(b, "dorang_shadow_observed_total",
		"Requests captured for shadow comparison.", m.observed.Load())
	b = counter(b, "dorang_observer_panics_total",
		"Panics recovered in the shadow observer; requests were unaffected.",
		m.observerPanics.Load())

	b = gauge(b, "dorang_inflight_requests",
		"Requests currently being served.", s.inflight.Load())
	b = gauge(b, "dorang_replay_bytes",
		"Request-body bytes retained for replay.", s.replay.Used())
	var ready int64
	if s.Ready() {
		ready = 1
	}
	b = gauge(b, "dorang_ready", "1 when the server accepts new work.", ready)
	b = gauge(b, "dorang_uptime_seconds", "Seconds since start.",
		int64(time.Since(s.started).Seconds()))

	// The observer's own numbers — sample rate, drops, diffs, and whether the
	// daily cost ceiling has stopped shadowing — are appended verbatim. They
	// belong on the same scrape as everything else: an operator watching a
	// cutover should not need a second endpoint to find out that the gate
	// stopped running (DESIGN §14.1).
	if o := rq.srv.snap.Load().observer; o != nil {
		b = o.Metrics(b)
	}

	return writeExposition(w, rq, buf, b)
}

// writeExposition answers the scrape.
func writeExposition(w http.ResponseWriter, rq *Request, buf *[]byte, b []byte) error {
	*buf = b
	h := w.Header()
	h.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if rq.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
	return nil
}

func counter(b []byte, name, help string, v uint64) []byte {
	b = append(b, "# HELP "...)
	b = append(b, name...)
	b = append(b, ' ')
	b = append(b, help...)
	b = append(b, "\n# TYPE "...)
	b = append(b, name...)
	b = append(b, " counter\n"...)
	b = append(b, name...)
	b = append(b, ' ')
	b = strconv.AppendUint(b, v, 10)
	return append(b, '\n')
}

func gauge(b []byte, name, help string, v int64) []byte {
	b = append(b, "# HELP "...)
	b = append(b, name...)
	b = append(b, ' ')
	b = append(b, help...)
	b = append(b, "\n# TYPE "...)
	b = append(b, name...)
	b = append(b, " gauge\n"...)
	b = append(b, name...)
	b = append(b, ' ')
	b = strconv.AppendInt(b, v, 10)
	return append(b, '\n')
}
