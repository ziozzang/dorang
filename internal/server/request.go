package server

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Request is dorang's view of one in-flight request.
//
// It is pooled. Nothing on the authenticate-and-route path allocates beyond
// acquiring it (DESIGN §15.2.2), which TestNoAllocsRoutingAndAuth asserts with
// [testing.AllocsPerRun]. Fields are grouped by who writes them.
type Request struct {
	// HTTP is the underlying request. Its Body has already been read into
	// [Request.Body] for routes that declare NeedsBody.
	HTTP *http.Request

	// ID is the join key for logs, metering and the ledger. It is the inbound
	// call-id header when the client set one (COMPATIBILITY §7.8), otherwise
	// generated.
	ID string
	// Method and Path are copied off the request so the hot path does not
	// chase pointers through http.Request and URL.
	Method string
	Path   string

	// Route is the matched route. Never nil inside a handler.
	Route *Route
	// Principal is the authenticated caller, nil on a public route.
	Principal Principal
	// AuthHeader names which of the six accepted headers supplied the
	// credential, "" on a public route.
	AuthHeader string

	// Model is the client-facing model name scanned out of the body, "" when
	// the route has none or the body did not name one.
	Model string
	// Stream is the request's stream flag as scanned from the body.
	Stream bool

	// Detail is true when the caller asked for the full extension-header set
	// with x-dorang-detail: full, or the server is configured to always send
	// it (DESIGN §10.4).
	Detail bool
	// UsageEvents is true when the caller opted in to post-hoc streaming
	// values with x-dorang-usage-events: 1.
	UsageEvents bool

	// Start is when the server took the request.
	Start time.Time

	// Body is the capped request body, nil for routes that do not read one.
	Body *Body

	// Result is the dispatcher's report. Fill it before writing.
	Result Result

	srv     *Server
	ctx     context.Context
	rw      responseWriter
	body    Body
	params  [maxParams]Param
	nparams int
	idbuf   [32]byte
	bytesIn int64
}

// Context is the request's context, already bounded by the configured request
// timeout. Handlers and dispatchers use it rather than rq.HTTP.Context(), which
// carries no timeout.
func (rq *Request) Context() context.Context {
	if rq.ctx == nil {
		return context.Background()
	}
	return rq.ctx
}

// Param returns a captured path parameter.
func (rq *Request) Param(name string) string {
	for i := 0; i < rq.nparams; i++ {
		if rq.params[i].Name == name {
			return rq.params[i].Value
		}
	}
	return ""
}

// WantsUsageEvents reports whether the caller opted in to post-hoc streaming
// values.
//
// DESIGN §10.4 [R1-C9] withdrew HTTP trailers as a delivery channel: mainstream
// LLM client libraries read the SSE body and never surface them, so promising
// "a terminal event and trailers" meant "one channel, plus an unused one". And
// injecting a dorang-authored frame into an upstream stream contradicts
// forwarding upstream bytes untouched — so it happens only when asked for.
// Without the opt-in the stream is byte-faithful and the numbers are available
// from the ledger by x-dorang-request-id, which is always present.
func (rq *Request) WantsUsageEvents() bool { return rq.UsageEvents }

// reset returns the struct to its zero state for the pool.
func (rq *Request) reset() {
	rq.HTTP = nil
	rq.ID = ""
	rq.Method = ""
	rq.Path = ""
	rq.Route = nil
	rq.Principal = nil
	rq.AuthHeader = ""
	rq.Model = ""
	rq.Stream = false
	rq.Detail = false
	rq.UsageEvents = false
	rq.Start = time.Time{}
	rq.Body = nil
	rq.body = Body{}
	rq.Result.reset()
	rq.ctx = nil
	rq.rw = responseWriter{}
	rq.nparams = 0
	rq.bytesIn = 0
	for i := range rq.params {
		rq.params[i] = Param{}
	}
}

// Body is a request body that has been read into memory under two independent
// caps.
//
// max_body_bytes is a hard limit: past it the request is refused with 413.
// The replay budget is soft and process-wide (DESIGN §15.4): a per-request cap
// alone is not a bound, because many concurrent requests each just under it
// exceed the whole memory target. A body that does not fit the remaining budget
// is marked non-replayable rather than retained anyway, which costs that one
// request its fallback and costs the process nothing.
type Body struct {
	bufp       *[]byte
	buf        []byte
	replayable bool
	reserved   int64
	budget     *replayBudget
}

// Bytes returns the body. The slice is valid until the request completes.
func (b *Body) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buf
}

// Len is the body length in bytes.
func (b *Body) Len() int {
	if b == nil {
		return 0
	}
	return len(b.buf)
}

// Replayable reports whether the body may be resent on a fallback hop. A false
// answer is reported to the client as x-dorang-replayable: false so that a
// caller who cares can see that a retry will not happen.
func (b *Body) Replayable() bool { return b != nil && b.replayable }

// release returns the reservation and the buffer.
func (b *Body) release() {
	if b == nil {
		return
	}
	if b.budget != nil && b.reserved > 0 {
		b.budget.release(b.reserved)
		b.reserved = 0
	}
	b.replayable = false
	b.putBuf()
	b.buf = nil
	b.budget = nil
}

// putBuf returns the working buffer to the pool. An oversized buffer is dropped
// rather than retained: one 32 MiB request must not pin 32 MiB for the life of
// the process, which is the shape of leak a naive pool creates.
func (b *Body) putBuf() {
	if b.bufp == nil {
		return
	}
	if cap(b.buf) <= bodyReturnLimit {
		*b.bufp = b.buf[:0]
		bodyPool.Put(b.bufp)
	}
	b.bufp = nil
}

const bodyReturnLimit = 256 << 10

var bodyPool = sync.Pool{New: func() any { b := make([]byte, 0, 8<<10); return &b }}

// replayBudget is the process-wide retained-body accounting of DESIGN §15.4.
type replayBudget struct {
	limit int64
	used  atomic.Int64
}

func newReplayBudget(limit int64) *replayBudget { return &replayBudget{limit: limit} }

// reserve takes n bytes if they fit, reporting whether they did.
func (b *replayBudget) reserve(n int64) bool {
	if b == nil || b.limit <= 0 {
		return false
	}
	for {
		cur := b.used.Load()
		if cur+n > b.limit {
			return false
		}
		if b.used.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

// release returns n bytes.
func (b *replayBudget) release(n int64) {
	if b == nil {
		return
	}
	b.used.Add(-n)
}

// Used is the currently reserved byte count.
func (b *replayBudget) Used() int64 { return b.used.Load() }

// read fills b from r under both caps.
//
// The hard cap is enforced against what actually arrives rather than against
// Content-Length: a Content-Length may lie, and a chunked body has none at all.
// Reading one byte past the limit is what distinguishes "exactly at the cap"
// from "over it".
//
// The replay reservation is taken once, at the end, against the real length.
// Reserving optimistically from Content-Length would let a lying header consume
// the whole process budget with an empty body.
func (b *Body) read(r *http.Request, max int64, budget *replayBudget) *Error {
	b.budget = budget
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	if max > 0 && r.ContentLength > max {
		return tooLarge(max)
	}
	limit := max
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}

	b.bufp = bodyPool.Get().(*[]byte)
	b.buf = (*b.bufp)[:0]
	if r.ContentLength > 0 && int64(cap(b.buf)) < r.ContentLength {
		// One right-sized allocation beats a dozen doublings. The grown slice
		// is stored back into the same pool entry on release, so a deployment
		// that habitually sees 200 KiB bodies stops allocating for them after
		// the first few requests.
		b.buf = make([]byte, 0, r.ContentLength)
	}

	var total int64
	for {
		if len(b.buf) == cap(b.buf) {
			b.buf = append(b.buf, 0)[:len(b.buf)]
		}
		n, err := r.Body.Read(b.buf[len(b.buf):cap(b.buf)])
		b.buf = b.buf[:len(b.buf)+n]
		total += int64(n)
		if total > limit {
			b.putBuf()
			b.buf = nil
			return tooLarge(limit)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			b.putBuf()
			b.buf = nil
			return NewError(http.StatusBadRequest, TypeInvalidRequest,
				"could not read request body").WithCode("unreadable_body")
		}
	}

	if n := int64(len(b.buf)); n > 0 && budget.reserve(n) {
		b.replayable = true
		b.reserved = n
	}
	return nil
}

// tooLarge builds the 413. The limit is named in the message because a client
// that hits it can do nothing without knowing what it is.
func tooLarge(max int64) *Error {
	buf := getBuf()
	defer putBuf(buf)
	*buf = append(*buf, "request body exceeds max_body_bytes ("...)
	*buf = appendInt(*buf, max)
	*buf = append(*buf, " bytes)"...)
	e := NewError(http.StatusRequestEntityTooLarge, TypeRequestTooLarge, string(*buf))
	return e.WithCode("request_too_large")
}
