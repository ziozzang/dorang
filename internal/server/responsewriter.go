package server

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
)

// responseWriter is the server's view of the response.
//
// It exists for one reason: extension headers must be attached exactly once,
// after the dispatcher has decided where the request went and before the first
// byte leaves. Stamping them earlier means they cannot carry a routing
// decision; stamping them later means a streaming client never sees them, since
// its first frame has already gone out under whatever headers were set at the
// time (DESIGN §10.4).
//
// It lives inside the pooled [Request], so wrapping the response costs no
// allocation.
type responseWriter struct {
	http.ResponseWriter
	rq *Request

	wrote  bool
	status int
	n      int64
	// sse records that the response is an event stream, which is what decides
	// whether a late error can still be delivered in band.
	sse bool
	// usageEmitted stops the opt-in usage event being written twice when a
	// dispatcher emitted it itself at the correct place in the frame order.
	usageEmitted bool
	// errShape records the upstream envelope shape, for the meter event.
	errShape Shape
	// cap is the shadow capture tap, non-nil only on a sampled request
	// (DESIGN §14.1). It is written after the underlying writer has taken the
	// bytes, so it is never between the client and the response.
	cap *capture
}

func (w *responseWriter) reset(under http.ResponseWriter, rq *Request) {
	*w = responseWriter{ResponseWriter: under, rq: rq, status: http.StatusOK}
}

// WriteHeader stamps the extension headers and sends the status line.
func (w *responseWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	h := w.ResponseWriter.Header()
	w.rq.srv.stampHeaders(h, w.rq, code)
	w.sse = strings.HasPrefix(h.Get("Content-Type"), "text/event-stream")
	w.ResponseWriter.WriteHeader(code)
}

// Write sends body bytes, stamping headers first if nothing has yet.
func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	if w.cap != nil {
		// After the write, deliberately: the client already has these bytes,
		// so the copy cannot delay them. What it can delay is the *next* chunk,
		// which is why the windows are bounded and why this runs for a sampled
		// fraction of traffic rather than all of it.
		w.cap.add(p[:n])
	}
	return n, err
}

// Flush pushes buffered bytes to the client. A streaming relay calls it after
// every chunk; a response writer that cannot flush is not an error, it is a
// test recorder.
func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *responseWriter) flush() { w.Flush() }

// Unwrap exposes the underlying writer to [net/http.ResponseController].
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// errNotHijackable is returned when the underlying writer cannot be hijacked.
var errNotHijackable = errors.New("server: response writer does not support hijacking")

// Hijack takes over the connection, which the WebSocket relay of DESIGN §10.6
// step 6 needs.
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errNotHijackable
	}
	// A hijacked connection is no longer the server's to write to, so it
	// counts as written: the deferred error path must not try to send an
	// envelope down a socket somebody else now owns. The capture tap goes with
	// it — everything after this point bypasses Write, and reporting the empty
	// buffer as "the response" would put a fabricated comparison in the report.
	w.wrote = true
	if w.cap != nil {
		w.cap.abandoned = true
		w.cap = nil
	}
	return hj.Hijack()
}

// EmitUsageEvent writes the opt-in post-hoc usage frame for a streaming
// response, and reports whether it wrote anything.
//
// It is a no-op unless the caller opted in with x-dorang-usage-events: 1
// (DESIGN §10.4 [R1-C9]). Without the opt-in the stream stays byte-faithful to
// upstream and the same numbers are available from the ledger by
// x-dorang-request-id, which is a response header and always present.
//
// A dispatcher that must place the frame before its protocol's terminator —
// before `data: [DONE]` for chat completions, before `message_stop` for
// messages — calls this itself at that point. Otherwise the server calls it
// after the dispatcher returns, which is correct for any stream whose
// terminator is not load-bearing.
func (rq *Request) EmitUsageEvent(w http.ResponseWriter) bool {
	rw, ok := w.(*responseWriter)
	if !ok {
		rw = &rq.rw
	}
	if !rq.UsageEvents || rw.usageEmitted || !rw.sse {
		return false
	}
	rw.usageEmitted = true
	buf := getBuf()
	defer putBuf(buf)
	*buf = appendUsageEvent(*buf, rq)
	_, _ = rw.Write(*buf)
	rw.Flush()
	return true
}

// appendUsageEvent renders the dorang usage frame.
//
// It is named with an event: line so that a client reading the stream with a
// conforming SSE parser can filter it out by name, and so that a client using
// the chat-completions convention — which never sends an event: line
// (COMPATIBILITY §1.1) — can recognize it as not-a-chunk without parsing the
// JSON.
func appendUsageEvent(dst []byte, rq *Request) []byte {
	r := &rq.Result
	dst = append(dst, "event: dorang.usage\ndata: {\"request_id\":"...)
	dst = appendJSONString(dst, rq.ID)
	dst = append(dst, ",\"model\":"...)
	dst = appendJSONString(dst, rq.Model)
	if r.UpstreamModel != "" {
		dst = append(dst, ",\"upstream_model\":"...)
		dst = appendJSONString(dst, r.UpstreamModel)
	}
	if r.Deployment != "" {
		dst = append(dst, ",\"deployment\":"...)
		dst = appendJSONString(dst, r.Deployment)
	}
	dst = append(dst, ",\"usage\":{\"input_tokens\":"...)
	dst = appendInt(dst, r.Tokens.Input)
	dst = append(dst, ",\"output_tokens\":"...)
	dst = appendInt(dst, r.Tokens.Output)
	if r.Tokens.CacheRead > 0 {
		dst = append(dst, ",\"cache_read_tokens\":"...)
		dst = appendInt(dst, r.Tokens.CacheRead)
	}
	if r.Tokens.CacheWrite > 0 {
		dst = append(dst, ",\"cache_write_tokens\":"...)
		dst = appendInt(dst, r.Tokens.CacheWrite)
	}
	if r.Tokens.Reasoning > 0 {
		dst = append(dst, ",\"reasoning_tokens\":"...)
		dst = appendInt(dst, r.Tokens.Reasoning)
	}
	dst = append(dst, '}')
	if r.Priced {
		dst = append(dst, ",\"cost_usd\":\""...)
		dst = appendNanoUSD(dst, r.CostNanoUSD)
		dst = append(dst, '"')
	}
	if r.TTFTNS > 0 {
		dst = append(dst, ",\"ttft_ms\":"...)
		dst = appendInt(dst, r.TTFTNS/1e6)
	}
	if r.NativeStopReason != "" {
		dst = append(dst, ",\"native_stop_reason\":"...)
		dst = appendJSONString(dst, r.NativeStopReason)
	}
	return append(dst, "}\n\n"...)
}
