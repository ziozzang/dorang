package server

import (
	"sync"
	"time"
)

// The capture tap.
//
// Comparing dorang against a reference gateway (DESIGN §14.1) needs dorang's
// own response, and the only place that exists is on its way to the client. So
// a sampled request grows a tap: [responseWriter.Write] copies what it just
// wrote into a bounded buffer, after the bytes have gone to the client, and the
// comparison happens later from the copy.
//
// Three properties, each of which a simpler design gets wrong:
//
//   - Bounded. A 200 MiB generation must not become 200 MiB of retained buffer,
//     and §9.6 rule 1 says so in general terms. The cap is two windows.
//   - Two windows, not one. A single head buffer captures the beginning of a
//     stream and loses the terminator, and the terminator is one of the things
//     §14.1 requires the comparison to check. A head and a tail keep both ends
//     and lose the middle, which is the half worth losing: the middle of a
//     stream is model output, which is not comparable anyway.
//   - Off the pooled request. The buffers come from their own pool, taken when
//     a request is sampled and returned when it finishes. Hanging them on the
//     pooled [Request] would make every request in the pool retain a capture
//     buffer forever, for the 95% of traffic that is never sampled.
//
// The tap is not free — it is a memcpy per Write on a sampled request — but it
// runs after the underlying writer has already taken the bytes, so it is not
// between the client and the response.

// capture holds the head and tail of one response body.
type capture struct {
	head []byte
	// tail is a fixed-size ring. pos is the write cursor; full says the ring
	// has wrapped, which is what distinguishes "16 KiB of tail" from "16 KiB
	// of zeroes and 200 bytes of body".
	tail []byte
	pos  int
	full bool

	total   int64
	headMax int
	tailMax int
	// linear records that the whole body fit in the head window, so head is
	// the body and the tail is redundant.
	linear bool
	// abandoned marks a response the tap never saw — a hijacked connection,
	// where the bytes leave through a socket the server no longer owns. The
	// observation is dropped rather than reported as an empty body, because a
	// comparison that silently invents "no body" is worse than no comparison.
	abandoned bool
}

// capturePool recycles capture buffers. A sampled fraction of traffic keeps
// this pool small, which is the point of taking the buffers here rather than
// hanging them on the request pool.
var capturePool = sync.Pool{New: func() any { return &capture{} }}

func getCapture(headMax, tailMax int) *capture {
	c := capturePool.Get().(*capture)
	c.reset(headMax, tailMax)
	return c
}

// putCapture returns c. A capture that grew past its configured window — which
// happens when the configuration is reloaded to a smaller one — is dropped
// rather than retained.
func putCapture(c *capture) {
	if c == nil {
		return
	}
	if cap(c.head) > captureReturnLimit || cap(c.tail) > captureReturnLimit {
		return
	}
	c.head = c.head[:0]
	c.tail = c.tail[:0]
	capturePool.Put(c)
}

// captureReturnLimit is the largest buffer worth keeping in the pool. Past it,
// one unusually large configured window would pin memory for the process
// lifetime — the same leak shape [Body.putBuf] avoids.
const captureReturnLimit = 1 << 20

func (c *capture) reset(headMax, tailMax int) {
	c.head = c.head[:0]
	c.tail = c.tail[:0]
	c.pos = 0
	c.full = false
	c.total = 0
	c.headMax = headMax
	c.tailMax = tailMax
	c.linear = true
	c.abandoned = false
}

// add copies p into the two windows.
func (c *capture) add(p []byte) {
	if len(p) == 0 {
		return
	}
	c.total += int64(len(p))

	if n := c.headMax - len(c.head); n > 0 {
		if n > len(p) {
			n = len(p)
		}
		c.head = append(c.head, p[:n]...)
	}
	if len(c.head) >= c.headMax && c.total > int64(len(c.head)) {
		c.linear = false
	}
	if c.tailMax <= 0 {
		return
	}
	if cap(c.tail) < c.tailMax {
		c.tail = make([]byte, c.tailMax)
	}
	c.tail = c.tail[:c.tailMax]

	// Keep only the last tailMax bytes of p; anything earlier is overwritten by
	// what follows it in this same call.
	if len(p) >= c.tailMax {
		copy(c.tail, p[len(p)-c.tailMax:])
		c.pos = 0
		c.full = true
		return
	}
	n := copy(c.tail[c.pos:], p)
	c.pos += n
	if c.pos >= c.tailMax {
		c.pos = 0
		c.full = true
	}
	if rest := len(p) - n; rest > 0 {
		copy(c.tail, p[n:])
		c.pos = rest
	}
}

// windows returns the captured head and tail and whether a gap separates them.
//
// The two windows overlap whenever the body was larger than the head but
// smaller than head+tail, because the tail ring keeps the last bytes regardless
// of what the head already holds. The overlap is trimmed off the front of the
// tail, so head and tail are always disjoint and in order. When they then meet
// exactly, the whole body was captured and truncated is false — which is the
// common case for an ordinary response and is what keeps its comparison
// conclusive.
func (c *capture) windows() (head, tail []byte, truncated bool) {
	if c == nil {
		return nil, nil, false
	}
	if c.linear || c.total <= int64(len(c.head)) {
		return c.head, nil, false
	}
	if c.tailMax <= 0 {
		return c.head, nil, true
	}
	if !c.full {
		tail = c.tail[:c.pos]
	} else {
		// The ring wrapped: the oldest byte is at pos.
		out := make([]byte, 0, c.tailMax)
		out = append(out, c.tail[c.pos:]...)
		out = append(out, c.tail[:c.pos]...)
		tail = out
	}
	gap := c.total - int64(len(c.head)) - int64(len(tail))
	if gap < 0 {
		tail = tail[-gap:]
		gap = 0
	}
	return c.head, tail, gap > 0
}

// DefaultCaptureHeadBytes and DefaultCaptureTailBytes are the windows used when
// [Options] does not name them. The head is large enough that an ordinary chat
// response is captured whole, which is what keeps its comparison conclusive.
const (
	DefaultCaptureHeadBytes = 256 << 10
	DefaultCaptureTailBytes = 16 << 10
)

// observe hands a finished sampled request to the observer.
//
// It runs from [Server.finish], in the same deferred block as the meter and
// under the same rule: after the client's last byte, and never able to fail the
// request. A panic here is counted and swallowed — an observer is a diagnostic,
// and a diagnostic that can take down the request path has inverted its own
// purpose.
func (s *Server) observe(cfg *snapshot, rq *Request, rw *responseWriter, status int, dur time.Duration) {
	defer func() {
		if v := recover(); v != nil {
			s.metrics.observerPanics.Add(1)
			cfg.logf("server: observer panicked, request unaffected: %v", v)
		}
	}()

	head, tail, truncated := rq.cap.windows()
	name := ""
	family := FamilyNone
	if rq.Route != nil {
		name, family = rq.Route.Name, rq.Route.Family
	}
	var reqHeader, respHeader = rq.HTTP.Header, rw.Header()
	rawQuery := ""
	if rq.HTTP.URL != nil {
		rawQuery = rq.HTTP.URL.RawQuery
	}

	ob := Observation{
		RequestID:      rq.ID,
		Method:         rq.Method,
		Path:           rq.Path,
		RawQuery:       rawQuery,
		Route:          name,
		Family:         family,
		Model:          rq.Model,
		Stream:         rq.Stream,
		RequestHeader:  reqHeader,
		RequestBody:    rq.Body.Bytes(),
		Status:         status,
		ResponseHeader: respHeader,
		ResponseHead:   head,
		ResponseTail:   tail,
		BodyBytes:      rq.cap.total,
		Truncated:      truncated,
		Duration:       dur,
		CostNanoUSD:    rq.Result.CostNanoUSD,
		Priced:         rq.Result.Priced,
	}
	cfg.observer.Observe(&ob)
}
