package server

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
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
	// Form is the parsed multipart body, non-nil only on a route that declares
	// Multipart. It is parsed in the gate rather than in the handler, because
	// the model on those routes is a form field and the allow-list check has to
	// see it (COMPATIBILITY 2.0, DESIGN §18 W10).
	Form *canonical.Form

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

	// modelAuthorized records that the allow-list has actually been consulted
	// for this request, by the gate or by the handler. It is the evidence
	// [Server.serve] checks before it lets a ModelAuthHandler route write a
	// response, so a handler that forgets to call [Request.AuthorizeModel]
	// fails the request instead of dispatching it unchecked.
	modelAuthorized bool

	// cap is the shadow capture buffer, non-nil only when this request was
	// sampled (DESIGN §14.1). It comes from its own pool rather than living
	// inline, so the 95% of requests that are never sampled do not each retain
	// a quarter of a megabyte for the life of the request pool.
	cap *capture
}

// AuthorizeModel enforces the calling key's model allow-list for one named
// model, and records that the check happened.
//
// This is the only place the allow-list is consulted. It is a method on the
// request rather than a free function over the principal because the *fact of
// having asked* is the half that kept going missing: three separate routes held
// a principal, never asked it anything, and dispatched. Routing the question
// through the request lets [Server.serve] refuse a ModelAuthHandler route that
// never asked — a check that is skipped now fails the request instead of
// passing it.
//
// A route may call it many times; a batch input file names a model per row and
// every one of them has to clear the same list.
//
// The status is 401, not 403: COMPATIBILITY §7.2 puts a model outside a key's
// allow-list there, and the reference proxy agrees. It reads oddly — the caller
// authenticated fine — but it is what deployed clients branch on.
func (rq *Request) AuthorizeModel(model string) error {
	if model == "" {
		return NewError(http.StatusBadRequest, TypeInvalidRequest,
			"the request did not name a model").
			WithCode("missing_model").WithParam("model")
	}
	if rq.Principal == nil {
		// A public route has no subject to restrict. Recording the check as
		// done is correct rather than lenient: there is no allow-list.
		rq.modelAuthorized = true
		return nil
	}
	if !rq.Principal.AllowsModel(model) {
		return NewError(http.StatusUnauthorized, TypeAuthentication,
			"this key is not allowed to use the requested model").
			WithCode("model_not_allowed").WithParam("model")
	}
	// The full envelope too, not only the allow-list: a model named by a
	// handler rather than scanned by the gate has never been through
	// Authorize, so its blocked/expired/budget/rate limits are unchecked at
	// this point.
	if err := rq.Principal.Authorize(Access{Model: model, Route: rq.Path}); err != nil {
		return asError(err, http.StatusForbidden, TypePermission)
	}
	rq.modelAuthorized = true
	return nil
}

// ModelAuthorized reports whether the allow-list has been consulted for this
// request. It exists for [Server.serve]'s post-condition and for tests that
// assert a handler reached the check.
func (rq *Request) ModelAuthorized() bool { return rq.modelAuthorized }

// MarkModelAuthorized records that a handler established, by a route-specific
// argument, that this request cannot reach a model call the allow-list would
// have refused.
//
// It is deliberately clumsy to say. The only legitimate users are handlers
// whose request names no model at all and whose downstream work is authorized
// elsewhere against a durable owner — batch retrieval, file listing — and every
// call site has to write down which of those it is.
func (rq *Request) MarkModelAuthorized(because string) {
	_ = because
	rq.modelAuthorized = true
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

// WithParams sets captured path parameters and returns rq.
//
// It exists for the packages that SUPPLY routes to this one — internal/app
// mounts the batch, files and Responses sub-resources — whose handler tests
// otherwise have no way to build a request that has any parameters at all: the
// capture array is filled by the matcher and is not exported. Nothing on the
// request path calls it.
func (rq *Request) WithParams(params ...Param) *Request {
	rq.nparams = 0
	for _, p := range params {
		if rq.nparams >= maxParams {
			break
		}
		rq.params[rq.nparams] = p
		rq.nparams++
	}
	return rq
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
	rq.modelAuthorized = false
	rq.Detail = false
	rq.UsageEvents = false
	rq.Start = time.Time{}
	rq.Body = nil
	rq.body = Body{}
	rq.Form = nil
	rq.Result.reset()
	rq.ctx = nil
	rq.rw = responseWriter{}
	rq.nparams = 0
	rq.bytesIn = 0
	if rq.cap != nil {
		putCapture(rq.cap)
		rq.cap = nil
	}
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

// Limit is the configured process-wide budget of DESIGN §15.4. Zero or less
// means retention is disabled, which is not the same fact as a full budget.
func (b *replayBudget) Limit() int64 {
	if b == nil {
		return 0
	}
	return b.limit
}

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
