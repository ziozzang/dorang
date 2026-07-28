package server

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// durationBuckets are the cumulative histogram bounds in seconds.
//
// They are dense around the warm-local p50 of 200 µs and the p99 of 2 ms
// (DESIGN §15.1), because a histogram whose lowest bucket is 5 ms cannot tell
// anyone whether that target is being met — which is the only question this
// histogram exists to answer.
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

// handleMetrics serves the Prometheus text exposition format.
//
// Hand-rolled, because a metrics endpoint is not worth a dependency and because
// DESIGN §0.2 requires the notebook profile to have zero required dependencies.
func (s *Server) handleMetrics(w http.ResponseWriter, rq *Request) error {
	m := &s.metrics
	buf := getBuf()
	defer putBuf(buf)
	b := *buf

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
