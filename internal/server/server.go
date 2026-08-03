package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/bits"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for the zero fields of [Options].
const (
	// DefaultMaxBodyBytes caps one request body. Large enough for a long
	// conversation with images; small enough that a thousand of them do not
	// exhaust a notebook profile's memory target (DESIGN §0.2).
	DefaultMaxBodyBytes = 32 << 20
	// DefaultReplayBudgetBytes is the process-wide retained-body budget
	// (DESIGN §15.4). It is deliberately much smaller than
	// DefaultMaxBodyBytes × concurrency: the point of a process-wide budget is
	// that it is not the per-request cap multiplied by anything.
	DefaultReplayBudgetBytes = 256 << 20
	// DefaultRequestTimeout matches the timeout a comparable deployment runs
	// with (COMPATIBILITY §8). Long, because a long generation is not a hung
	// request.
	DefaultRequestTimeout = 6000 * time.Second
	// DefaultShutdownGrace is how long in-flight requests get after a SIGTERM.
	DefaultShutdownGrace = 30 * time.Second

	// DefaultReadHeaderTimeout bounds the client that connects and then says
	// nothing at all. That client never reaches a handler, so it costs no
	// capacity slot — what it exhausts is the listener's accept queue, which is
	// why this is the shortest of the three.
	DefaultReadHeaderTimeout = 30 * time.Second
	// DefaultReadTimeout bounds the WHOLE request read — headers and body —
	// measured from the moment the connection produced its first byte.
	//
	// It is the deadline the slow-body client needs, and that client is worse
	// than the silent one: by the time a body is dribbling, the request has been
	// admitted, [Server.ServeHTTP] has taken its in-flight slot, and the handler
	// is parked inside Body.read. Without this it stayed there forever, holding
	// the capacity reservation the drain and dorang_inflight_requests both count.
	//
	// Two minutes, and the number is the UPLOAD budget, not the generation
	// budget. Thirty seconds of it can be spent waiting for headers; the
	// remaining ninety carry a body up to [DefaultMaxBodyBytes], which is 32 MiB
	// — about 2.9 Mbit/s for a maximal body, and hundreds of times the margin a
	// normal chat completion needs. A deployment that accepts large audio or
	// batch-input uploads from slow links raises it, which is what
	// [Options.ReadTimeout] is for.
	//
	// It is emphatically NOT a write deadline. A legitimate response streams for
	// minutes and a WriteTimeout would cut it; net/http clears the read deadline
	// the moment the request body hits EOF (and again on Hijack, which is what
	// keeps the WebSocket relay working), so a read deadline bounds only the
	// half that is genuinely finished long before the answer begins.
	DefaultReadTimeout = 2 * time.Minute
	// DefaultIdleTimeout bounds a keep-alive connection between requests.
	//
	// Go's default for this is ReadTimeout, which would silently make the two
	// one setting; they answer different questions and are set apart.
	//
	// Two minutes, chosen to be LONGER than the idle timeout of whatever sits in
	// front: 60s on an AWS ALB, 75s for nginx's keepalive_timeout. The side that
	// closes an idle connection should be the side that knows it is idle. If the
	// gateway closes first, the proxy discovers the close by reusing a dead
	// connection, and the client gets a 502 for a request that never left.
	DefaultIdleTimeout = 2 * time.Minute
	// DefaultOwnedBy fills the owned_by field of a model with no owner.
	DefaultOwnedBy = "dorang"

	defaultMaxBodyBytes = DefaultMaxBodyBytes
)

// MetricsAccess selects who may read GET /metrics (DESIGN §12.3).
//
// The zero value is the restrictive one on purpose. A scrape endpoint reads
// like infrastructure rather than data, which is exactly why it was public: the
// body is a page of numbers with no obvious owner. It is not — it names every
// model a deployment routes to, every credential id, and what each key has
// spent. Opening it is a decision, and a decision has to be written down.
type MetricsAccess uint8

const (
	// MetricsAdmin requires the master credential or an [AdminPrincipal].
	MetricsAdmin MetricsAccess = iota
	// MetricsPublic serves the scrape to anyone who can reach the listener.
	MetricsPublic
	// MetricsOff does not register the route at all.
	MetricsOff
)

// ModelsCreated is the constant `created` of every GET /v1/models item.
//
// COMPATIBILITY §7.4: it is a fixed constant, not the current time, because
// clients cache on it. Emitting time.Now() there makes every poll look like a
// changed catalog and defeats every client-side cache in front of the gateway.
// The value is the one the reference proxy has always emitted.
const ModelsCreated int64 = 1677610602

// Options configures a [Server]. Every field is optional; the zero value
// produces a server that answers health, metrics and 501.
type Options struct {
	// Auth resolves credentials. Nil makes every non-public route answer 401:
	// a gateway with no authenticator is not an open gateway.
	Auth Authenticator
	// Dispatcher owns the upstream half. Nil makes inference routes answer
	// 501 with a reason rather than 404 or a nil dereference.
	Dispatcher Dispatcher
	// Models supplies GET /v1/models.
	Models ModelLister
	// Meter absorbs finished requests. Nil discards them.
	Meter Meter
	// Observer compares a sampled fraction of traffic against a reference
	// gateway (DESIGN §14.1). Nil captures nothing and costs nothing: the
	// request path reads one nil pointer and skips the whole mechanism.
	Observer Observer

	// Metrics renders GET /metrics when it is set, replacing the built-in
	// block (DESIGN §12.3). internal/metrics assembles the full surface by
	// pulling from every subsystem, including this one through [Server.Stats];
	// this package neither knows nor imports it.
	//
	// It replaces rather than extends because the two would otherwise both
	// emit `dorang_requests_total` — the built-in one unlabelled, §12.3's with
	// five labels — and a second `# TYPE` line for one name makes Prometheus
	// reject the whole scrape.
	Metrics MetricsSource

	// MetricsAccess selects who may read GET /metrics. The zero value
	// authenticates and requires an administrative caller, which is the safe
	// default: the scrape carries per-key spend, per-credential quota state and
	// every configured model name.
	MetricsAccess MetricsAccess

	// HealthReporters contribute named objects to the /health body, in order.
	// Nil adds nothing and the body keeps the shape it always had.
	HealthReporters []HealthReporter

	// ReadinessGates are the dependencies that can take this node out of
	// rotation without draining it. Empty leaves the drain as readiness's only
	// input, which is what it always was.
	//
	// A gate is not a health reporter and the two are not interchangeable. A
	// reporter says a subsystem is unhappy and deliberately does NOT change the
	// status: degraded metering is not a serving failure and must not take a pod
	// out of rotation. A gate says this node cannot serve new work, which is the
	// one thing readiness is for.
	ReadinessGates []ReadinessGate

	// CaptureHeadBytes and CaptureTailBytes bound what a sampled response
	// retains for comparison; 0 uses [DefaultCaptureHeadBytes] and
	// [DefaultCaptureTailBytes]. They are here rather than in the observer
	// because the server owns the buffer that the tap writes into.
	CaptureHeadBytes int
	CaptureTailBytes int

	// MaxBodyBytes caps one request body; 0 uses [DefaultMaxBodyBytes].
	MaxBodyBytes int64
	// ReplayBudgetBytes is the process-wide retained-body budget; 0 uses
	// [DefaultReplayBudgetBytes]. Negative disables retention entirely, which
	// marks every request non-replayable.
	ReplayBudgetBytes int64
	// RequestTimeout bounds one request; 0 uses [DefaultRequestTimeout].
	RequestTimeout time.Duration
	// ReadHeaderTimeout bounds reading the request headers; 0 uses
	// [DefaultReadHeaderTimeout]. Negative removes the bound.
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds reading the whole request, body included; 0 uses
	// [DefaultReadTimeout]. Negative removes the bound, which is what a
	// deployment fronted by a proxy that already enforces one would say, and is
	// the only way back to the unbounded behaviour.
	//
	// Raise it for large uploads over slow links; it does not bound the
	// response, so a long generation is unaffected by it. See
	// [DefaultReadTimeout] for why a read deadline is the right tool here and a
	// write deadline is not.
	ReadTimeout time.Duration
	// IdleTimeout bounds a keep-alive connection between requests; 0 uses
	// [DefaultIdleTimeout]. Negative removes the bound.
	IdleTimeout time.Duration
	// ShutdownGrace bounds the drain; 0 uses [DefaultShutdownGrace].
	ShutdownGrace time.Duration
	// PreStopDelay is how long [Server.Serve] keeps serving after readiness
	// goes false and before the listener closes, so that a polling load
	// balancer has time to notice (DESIGN §13).
	//
	// Zero means none, and is NOT defaulted. A zero grace would mean "cut every
	// request instantly", which nobody wants and which is why ShutdownGrace
	// treats it as absence; a zero pre-stop delay means "there is no balancer to
	// wait for", which is the right answer for a single node and for every
	// embedder that did not ask for one. internal/config supplies the
	// deployment default (`server.pre_stop_delay`, 10s).
	PreStopDelay time.Duration

	// AlwaysFullHeaders attaches the full extension-header set to every
	// response without the caller asking (observability.always_full_headers).
	AlwaysFullHeaders bool
	// LegacyHeaders mirrors the widely-read legacy header spellings
	// (COMPATIBILITY §7.7).
	LegacyHeaders bool

	// Passthrough opens provider-native routes (DESIGN §10.6). An empty slice
	// serves none: unmapped prefixes are not served, and this is not an open
	// proxy.
	Passthrough []PassthroughRoute
	// PassthroughClient issues passthrough requests. Nil builds a default that
	// does not follow redirects.
	PassthroughClient *http.Client

	// Routes are additional routes, merged into the table by specificity.
	Routes []Route

	// Now overrides the clock, for tests.
	Now func() time.Time
	// Logf receives diagnostics. Successful requests are metrics, not log
	// lines (DESIGN §15.2.7), so this is called only for panics, drops and
	// refusals.
	Logf func(format string, args ...any)
}

// snapshot is the immutable configuration the request path reads.
//
// DESIGN §15.2.1: configuration swaps by pointer and the read path takes no
// lock. TestSnapshotReadTakesNoLock holds the server's only mutex and asserts a
// request still completes.
type snapshot struct {
	routes     *routeTable
	auth       Authenticator
	dispatcher Dispatcher
	models     ModelLister
	meter      Meter
	observer   Observer
	metrics    MetricsSource
	reporters  []HealthReporter
	gates      []ReadinessGate

	captureHead int
	captureTail int

	maxBody        int64
	requestTimeout time.Duration
	shutdownGrace  time.Duration
	preStopDelay   time.Duration
	// The three connection deadlines, already resolved: a negative Option is
	// stored here as zero, which is what net/http spells "no deadline".
	readHeaderTimeout time.Duration
	readTimeout       time.Duration
	idleTimeout       time.Duration

	alwaysFull    bool
	legacyHeaders bool

	now  func() time.Time
	logf func(string, ...any)
}

// Server is dorang's HTTP surface.
type Server struct {
	snap atomic.Pointer[snapshot]

	// mu serializes [Server.Reload] against itself. It is never taken on the
	// request path; that is the invariant TestSnapshotReadTakesNoLock proves.
	mu sync.Mutex

	pool     sync.Pool
	replay   *replayBudget
	metrics  metrics
	inflight atomic.Int64
	draining atomic.Bool
	started  time.Time

	idSeq  atomic.Uint64
	idSeed uint64

	ptClient *http.Client
}

// New builds a server with the T0 route set of COMPATIBILITY §0 registered.
func New(opts Options) (*Server, error) {
	s := &Server{}
	s.pool.New = func() any { return &Request{} }
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	s.idSeed = binary.LittleEndian.Uint64(seed[:])
	s.started = time.Now()
	if err := s.Reload(opts); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload swaps the configuration. In-flight requests keep the snapshot they
// started with; the next request sees the new one.
func (s *Server) Reload(opts Options) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := &snapshot{
		auth:           opts.Auth,
		dispatcher:     opts.Dispatcher,
		models:         opts.Models,
		meter:          opts.Meter,
		observer:       opts.Observer,
		metrics:        opts.Metrics,
		reporters:      opts.HealthReporters,
		gates:          opts.ReadinessGates,
		captureHead:    opts.CaptureHeadBytes,
		captureTail:    opts.CaptureTailBytes,
		maxBody:        opts.MaxBodyBytes,
		requestTimeout: opts.RequestTimeout,
		shutdownGrace:  opts.ShutdownGrace,
		preStopDelay:   opts.PreStopDelay,

		readHeaderTimeout: orDeadline(opts.ReadHeaderTimeout, DefaultReadHeaderTimeout),
		readTimeout:       orDeadline(opts.ReadTimeout, DefaultReadTimeout),
		idleTimeout:       orDeadline(opts.IdleTimeout, DefaultIdleTimeout),

		alwaysFull:    opts.AlwaysFullHeaders,
		legacyHeaders: opts.LegacyHeaders,
		now:           opts.Now,
		logf:          opts.Logf,
	}
	if snap.meter == nil {
		snap.meter = nopMeter{}
	}
	if snap.captureHead <= 0 {
		snap.captureHead = DefaultCaptureHeadBytes
	}
	if snap.captureTail < 0 {
		snap.captureTail = 0
	} else if snap.captureTail == 0 {
		snap.captureTail = DefaultCaptureTailBytes
	}
	if snap.maxBody <= 0 {
		snap.maxBody = DefaultMaxBodyBytes
	}
	if snap.requestTimeout <= 0 {
		snap.requestTimeout = DefaultRequestTimeout
	}
	if snap.shutdownGrace <= 0 {
		snap.shutdownGrace = DefaultShutdownGrace
	}
	if snap.preStopDelay < 0 {
		snap.preStopDelay = 0
	}
	if snap.readTimeout > 0 && snap.readTimeout < snap.readHeaderTimeout {
		// net/http measures both from the same instant, so a read timeout inside
		// the header timeout makes the header timeout unreachable. Saying so is
		// better than silently honouring the smaller number under the other
		// name.
		return fmt.Errorf("server: ReadTimeout (%s) is shorter than ReadHeaderTimeout "+
			"(%s); both are measured from the connection's first byte, so the header "+
			"timeout could never fire and the whole-request deadline would be "+
			"enforcing it instead",
			snap.readTimeout, snap.readHeaderTimeout)
	}
	if snap.now == nil {
		snap.now = time.Now
	}
	if snap.logf == nil {
		snap.logf = func(string, ...any) {}
	}

	routes := s.baseRoutes(opts.MetricsAccess)
	pt, err := compilePassthrough(opts.Passthrough)
	if err != nil {
		return err
	}
	routes = append(routes, pt...)
	for i := range opts.Routes {
		r := opts.Routes[i]
		routes = append(routes, &r)
	}
	// A caller-supplied route REPLACES a built-in one on the same pattern.
	//
	// The built-in table declares the whole T0+T1 surface, including routes
	// whose stateful half lives in internal/app (the Responses sub-resources,
	// batches, files). Without this rule the app's real handler would be
	// registered behind the built-in one and silently never reached — the same
	// class of bug COMPATIBILITY §7.5 is about, one layer up. Within a single
	// set a duplicate is still a configuration error, which newRouteTable
	// enforces for exact patterns.
	routes = overrideByPattern(routes)
	table, err := newRouteTable(routes)
	if err != nil {
		return err
	}
	snap.routes = table

	if s.replay == nil {
		budget := opts.ReplayBudgetBytes
		if budget == 0 {
			budget = DefaultReplayBudgetBytes
		}
		s.replay = newReplayBudget(budget)
	}
	if s.ptClient == nil {
		s.ptClient = opts.PassthroughClient
	}
	if s.ptClient == nil {
		s.ptClient = defaultPassthroughClient()
	}

	s.snap.Store(snap)
	return nil
}

// Ready reports whether the server is accepting new work. It goes false the
// instant a drain starts, and false again — reversibly — while any configured
// [ReadinessGate] says this node cannot serve new work.
//
// The two are not the same condition and the health body tells them apart. A
// drain is terminal and this process is on its way out; a closed gate is a
// dependency that is expected back, and the node returns to rotation by itself
// when it is. Neither of them is a liveness signal.
func (s *Server) Ready() bool {
	if s.draining.Load() {
		return false
	}
	// The gates are read from the snapshot rather than the Server so that a
	// reload swaps them by pointer like everything else on this path.
	for _, g := range s.snap.Load().gates {
		if ok, _ := g.ReadyForWork(); !ok {
			return false
		}
	}
	return true
}

// InFlight is the number of requests currently being served.
func (s *Server) InFlight() int64 { return s.inflight.Load() }

// ReplayBytesUsed is the currently retained replay budget, for tests and the
// metrics endpoint.
func (s *Server) ReplayBytesUsed() int64 { return s.replay.Used() }

// acquire takes a request struct from the pool.
func (s *Server) acquire() *Request {
	rq := s.pool.Get().(*Request)
	rq.srv = s
	return rq
}

// release returns it.
func (s *Server) release(rq *Request) {
	if rq.Body != nil {
		rq.Body.release()
	}
	rq.reset()
	s.pool.Put(rq)
}

// newRequestID generates a request id into dst.
//
// A counter mixed with a per-process seed: unique within the process by
// construction, unguessable enough that a caller cannot enumerate other
// callers' ledger rows, and free of any syscall or lock on the hot path.
func (s *Server) newRequestID(dst []byte) []byte {
	n := s.idSeq.Add(1)
	x := s.idSeed ^ (n * 0x9E3779B97F4A7C15)
	y := bits.RotateLeft64(s.idSeed, 32) ^ (n * 0xC2B2AE3D27D4EB4F)
	dst = appendHex64(dst, x)
	return appendHex64(dst, y)
}

// appendHex64 appends v as sixteen lowercase hex digits.
func appendHex64(dst []byte, v uint64) []byte {
	for i := 60; i >= 0; i -= 4 {
		dst = append(dst, hexDigits[(v>>uint(i))&0xf])
	}
	return dst
}

// orDeadline resolves one connection deadline. Zero is absence and takes the
// default; negative is the explicit "no deadline" and becomes the zero net/http
// reads as one. They are different answers for the same reason
// [Options.PreStopDelay]'s zero is.
func orDeadline(v, def time.Duration) time.Duration {
	switch {
	case v == 0:
		return def
	case v < 0:
		return 0
	}
	return v
}

// ServeHTTP implements [net/http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)

	cfg := s.snap.Load()
	rq := s.acquire()
	defer s.release(rq)

	rq.HTTP = r
	rq.Method = r.Method
	rq.Path = r.URL.Path
	rq.Start = cfg.now()
	rq.Detail = cfg.alwaysFull || detailRequested(r.Header)
	rq.UsageEvents = usageEventsRequested(r.Header)
	rq.ID = inboundRequestID(r.Header)
	if rq.ID == "" {
		rq.ID = string(s.newRequestID(rq.idbuf[:0]))
	}

	rw := &rq.rw
	rw.reset(w, rq)

	ctx := r.Context()
	var cancel context.CancelFunc
	if cfg.requestTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, cfg.requestTimeout)
		defer cancel()
	}
	rq.ctx = ctx

	defer func() {
		if v := recover(); v != nil {
			s.metrics.panics.Add(1)
			cfg.logf("server: panic serving %s %s: %v", rq.Method, rq.Path, v)
			if !rw.wrote {
				// COMPATIBILITY §11.2's "Gateway fault" row: `internal_error`,
				// not the stringified status NewError would otherwise default
				// to. A 500 is the one status a client cannot infer anything
				// from, so the code is the whole of what it learns.
				e := NewError(http.StatusInternalServerError,
					TypeAPIError, "internal error").WithCode(CodeInternalError)
				// And §11.1's envelope for whichever family was being spoken.
				// This path does not go through [Server.fail] — it is the one
				// that runs when fail itself could not — so the projection has
				// to be applied here too. rq.Route is nil for a panic raised
				// before the route was resolved, which is FamilyNone and the
				// OpenAI envelope, and is the honest answer when nobody has
				// established what the caller was speaking.
				if rq.Route != nil {
					e = e.ForFamily(rq.Route.Family)
				}
				WriteError(rw, e)
			}
		}
		s.finish(cfg, rq, rw)
	}()

	// The capture tap is armed here: after the recover, so a panicking observer
	// cannot escape into net/http and kill the connection, and before any
	// handler can write, because a sampling decision taken later would miss the
	// response it exists to compare (DESIGN §14.1). The call itself is one hash
	// of the request id, and only when an observer is configured at all.
	if cfg.observer != nil && cfg.observer.Sample(rq.ID, r.Header) {
		rq.cap = getCapture(cfg.captureHead, cfg.captureTail)
		rw.cap = rq.cap
	}

	if err := s.serve(cfg, rw, rq); err != nil {
		s.fail(rw, rq, err)
	}
}

// serve runs the gate and the handler.
func (s *Server) serve(cfg *snapshot, rw *responseWriter, rq *Request) error {
	rt, np, res := cfg.routes.lookup(rq.Method, rq.Path, &rq.params)
	switch res {
	case lookupMiss:
		return s.unimplemented(rw, rq)
	case lookupMethod:
		rw.Header().Set("Allow", rt.allow)
		return NewError(http.StatusMethodNotAllowed, TypeInvalidRequest,
			"method not allowed on this route").WithCode("method_not_allowed")
	}
	rq.Route = rt
	rq.nparams = np

	if !rt.Public {
		if cfg.auth == nil {
			return NewError(http.StatusUnauthorized, TypeAuthentication,
				"no authenticator is configured").WithCode("no_authenticator")
		}
		rq.AuthHeader = authHeaderUsed(rq.HTTP.Header)
		p, err := cfg.auth.AuthenticateHeader(rq.ctx, rq.HTTP.Header)
		if err != nil {
			s.metrics.authFailures.Add(1)
			return asError(err, http.StatusUnauthorized, TypeAuthentication)
		}
		rq.Principal = p
		if rt.Admin && !isAdmin(p) {
			s.metrics.authFailures.Add(1)
			return NewError(http.StatusForbidden, TypePermission,
				"this route is administrative: it needs the master credential or an admin key").
				WithCode("admin_required")
		}
	}

	if rt.NeedsBody {
		rq.Body = &rq.body
		if e := rq.body.read(rq.HTTP, cfg.maxBody, s.replay); e != nil {
			if e.Status == http.StatusRequestEntityTooLarge {
				s.metrics.bodyTooLarge.Add(1)
			}
			return e
		}
		rq.bytesIn = int64(rq.body.Len())
		if rq.bytesIn > 0 && !rq.body.Replayable() {
			s.metrics.replayRefused.Add(1)
		}
		if b := rq.body.Bytes(); len(b) > 0 {
			if rt.Multipart {
				form, e := parseMultipart(rq.HTTP, b)
				if e != nil {
					return e
				}
				rq.Form = form
				// An exact field lookup, matching the strict JSON rule: the gate
				// and the adapter must resolve the same model from the same
				// bytes (COMPATIBILITY 2.0).
				rq.Model = form.Get("model")
				rq.Stream, _ = form.Bool("stream")
			} else {
				model, stream, ok := peekRequest(b)
				if !ok {
					return NewError(http.StatusBadRequest, TypeInvalidRequest,
						"request body is not a JSON object").WithCode(CodeInvalidRequest)
				}
				rq.Model, rq.Stream = model, stream
			}
		}
	}

	// The deployment-in-the-path aliases name the model in the URL, and there
	// the path is authoritative: an Azure-shaped client sends the deployment
	// there and often nothing in the body. This runs BEFORE the allow-list
	// check below, so the key is authorized against the name that will actually
	// be dispatched — resolving it in the handler instead would authorize a
	// request as having no model and dispatch it as having one, which is W10.
	if rt.ModelParam != "" {
		if m := rq.Param(rt.ModelParam); m != "" {
			rq.Model = m
		}
	}

	if rq.Principal != nil {
		if err := rq.Principal.Authorize(Access{Model: rq.Model, Route: rq.Path}); err != nil {
			s.metrics.authFailures.Add(1)
			return asError(err, http.StatusForbidden, TypePermission)
		}
	}

	// The allow-list, at the gate, for every route that declared the gate
	// enforces it.
	//
	// Authorize above is not that check. It skips the model when rq.Model is
	// "" (internal/auth: `a.Model != "" && …`), which is the correct rule for
	// a route that names no model and the wrong one for a route that names a
	// model the gate simply failed to find. Asking AllowsModel here — rather
	// than in one handler, which is where it used to live — is what makes the
	// answer the same for every route that shares this mode.
	if rq.Route.ModelAuth == ModelAuthGate && rq.Model != "" {
		if err := rq.AuthorizeModel(rq.Model); err != nil {
			s.metrics.authFailures.Add(1)
			return err
		}
	}

	err := rt.Handler(rw, rq)

	// Post-condition, not a second check: a ModelAuthHandler route promised to
	// name its models and did not. Failing the request is the only answer that
	// does not silently reinstate the bypass — an omission here is exactly the
	// shape of every model-allow-list defect this mechanism replaced. It runs
	// after the handler because the handler is what was supposed to do it, and
	// it cannot un-send bytes, so it also reports when it is too late.
	if err == nil && rt.ModelAuth == ModelAuthHandler && !rq.modelAuthorized {
		s.metrics.authFailures.Add(1)
		cfg.logf("server: route %s declares ModelAuthHandler and never called AuthorizeModel", rt.Name)
		if rw.wrote {
			return NewError(http.StatusInternalServerError, TypeAPIError,
				"the request was served without a model authorization decision").
				WithCode("model_auth_missing")
		}
		return NewError(http.StatusInternalServerError, TypeAPIError,
			"this route did not make a model authorization decision").
			WithCode("model_auth_missing")
	}
	return err
}

// fail turns a handler error into a response.
//
// Once the first byte is out the status is 200 and cannot change — the boundary
// DESIGN §7.6 refuses to cross. For a stream the only channel left is the body,
// so the error goes in band as COMPATIBILITY §1.3 specifies; for anything else
// there is nothing to do but stop writing.
func (s *Server) fail(rw *responseWriter, rq *Request, err error) {
	e := asError(err, http.StatusInternalServerError, TypeAPIError)
	// A drain that reached its grace boundary cancelled this request on
	// purpose, and it is the only party that knows that. Whatever the handler
	// reported on the way out — a context error, a broken pipe, an upstream
	// read that ended — the answer the client needs is "the gateway is
	// restarting, retry", not a generic 500 and not a TCP reset. For a stream
	// this replaces the last frame the client will ever see (DESIGN §13).
	if se := shutdownError(rq.ctx); se != nil {
		e = se
	}
	rw.errShape = e.Shape
	// The upstream's own words, kept for the ledger and kept out of the body.
	// The dispatcher has already scrubbed the credential it sent from these.
	rq.Result.NativeErrorType = e.NativeType
	rq.Result.NativeErrorMessage = e.NativeMessage
	// The vendor's own vocabulary AND the vendor's own envelope, chosen by which
	// family the caller is speaking (COMPATIBILITY §11's opening rule). This is
	// the one place a family reaches an envelope, and every error response goes
	// through it.
	if rq.Route != nil {
		e = e.ForFamily(rq.Route.Family)
	}
	s.logUpstreamError(rq, e)
	if !rw.wrote {
		WriteError(rw, e)
		return
	}
	if rw.sse {
		writeSSEError(rw, e)
		rw.flush()
	}
	rq.srv.metrics.lateErrors.Add(1)
}

// logUpstreamError puts the backend's own words somewhere an operator can read
// them without a database round trip.
//
// This is the deliberate answer to "what carries the upstream's message", and
// the answer is: the operator log, not the client.
//
// COMPATIBILITY §11.3 keeps the backend's message out of the response body, and
// the reason is concrete rather than tidy — several OpenAI-compatible servers
// quote the offending key back in a 401 message, so a gateway that relays that
// text hands the operator's provider credential to whichever tenant happened to
// be calling. That reasoning applies to a RESPONSE HEADER exactly as it applies
// to a body: both are read by the same client, over the same connection, by
// every HTTP library ever written. So there is no x-dorang-native-error-message
// and there deliberately will not be one. What §11.3 promises out of band is the
// native TYPE and CODE — enumerated tokens, not free text — and those are on
// headers.
//
// The free text goes here. Before this it was assigned to
// Result.NativeErrorMessage and read by nothing anywhere in the tree, so §11.3's
// other promise — "recorded in the ledger" — was not true either and an upstream
// reason was erased with no recovery path at all. A gateway whose only record of
// why a backend refused is a field nobody reads is a gateway that makes an
// outage undebuggable, which is the failure mode §11.3's own excerpt rule exists
// to avoid.
//
// The text is already scrubbed: internal/backend removes the exact secret that
// request carried — read back off the outbound headers, so a refreshed OAuth
// token counts too — before the error ever reaches here. internal/redact is the
// authoritative statement of that rule; this comment names the caller rather
// than the package because an auditor following it must land on the code that
// actually runs, and until internal/backend imports internal/redact the two are
// not the same file.
func (s *Server) logUpstreamError(rq *Request, e *Error) {
	if e == nil || (e.NativeMessage == "" && e.NativeType == "" && e.NativeCode == "") {
		return
	}
	s.snap.Load().logf(
		"server: upstream error: request=%s status=%d code=%s native_type=%q native_code=%q native_message=%q",
		rq.ID, e.StatusCode(), e.Code, e.NativeType, e.NativeCode, e.NativeMessage)
}

// asError coerces any error into an *Error with a default status.
func asError(err error, status int, typ string) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		if e.Status == 0 {
			e.Status = status
		}
		return e
	}
	if ctxErr := err; ctxErr == context.DeadlineExceeded {
		return NewError(http.StatusGatewayTimeout, TypeTimeout,
			"request exceeded the configured timeout").WithCode(CodeTimeout)
	}
	return NewError(status, typ, err.Error())
}

// unimplemented answers a route dorang does not serve.
//
// DESIGN §0.2 and COMPATIBILITY §9: never a silent 404. A 404 tells a client
// "you asked for the wrong thing"; a 501 with a code tells it "dorang has not
// built this yet", which is the truth and is machine-readable. The two codes
// distinguish a path that is part of the planned surface from one that is not,
// so a client can tell "wait for a release" from "you have a typo".
func (s *Server) unimplemented(rw *responseWriter, rq *Request) error {
	s.metrics.unimplemented.Add(1)
	// The path is client-controlled and is about to become a header value.
	// net/http drops an invalid one at write time, but a response writer that
	// is not net/http's will not, so the check happens here — CRLF in a header
	// value is response splitting, and "the framework probably catches it" is
	// not a security argument.
	if safeHeaderValue(rq.Path) {
		rw.Header().Set(HeaderUnimplemented, rq.Path)
	}
	code := "route_unknown"
	msg := "no such route; dorang does not serve this path"
	if plannedSurface(rq.Path) {
		code = CodeRouteNotImplemented
		msg = "route is part of dorang's declared surface but is not implemented yet"
	}
	return NewError(http.StatusNotImplemented, TypeNotImplemented, msg).
		WithCode(code).WithParam(rq.Path)
}

// plannedRoutePrefixes are the administrative and protocol families DESIGN §2.1
// and §2.3 declare but this milestone has not built. A path under one of them
// is "not implemented yet" rather than "does not exist".
// A path here that is ALSO a registered route never reaches this function; the
// overlap is deliberate, because a route can be registered by one build and not
// another (internal/app mounts the batch, files and Responses sub-resources
// only when their subsystems exist) and the honest answer for the build without
// them is "declared, not built" rather than "no such path".
var plannedRoutePrefixes = []string{
	"/v1/responses", "/v1/ocr", "/v1/files", "/v1/batches",
	"/v1/audio/", "/v1/images/", "/v1/vector_stores", "/v1/assistants",
	"/key/", "/user/", "/team/", "/model/", "/model_group/", "/budget/",
	"/spend/", "/global/spend/", "/health/history",
}

// plannedSurface reports whether a path belongs to a declared-but-unbuilt
// family.
func plannedSurface(path string) bool {
	for _, p := range plannedRoutePrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	switch path {
	case "/v1/files", "/v1/batches":
		return true
	}
	return false
}

// inboundRequestID honors a client-supplied call id (COMPATIBILITY §7.8).
func inboundRequestID(h http.Header) string {
	for _, name := range InboundRequestIDHeaders {
		if v := h.Get(name); v != "" && len(v) <= 128 && safeHeaderValue(v) {
			return v
		}
	}
	return ""
}

// safeHeaderValue rejects a value that cannot be echoed back into a header.
// A client-controlled string is about to become a response header, so it is
// checked rather than trusted.
func safeHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return false
		}
	}
	return true
}

// finish records metrics, hands the request to the observer, and then to the
// meter.
//
// Metering runs after the client's last byte and never fails the request: a
// panic in a Meter implementation is counted and swallowed (DESIGN §10.6 step
// 5, generalized — there is no reason the rest of the surface should be less
// forgiving than passthrough).
//
// The observer runs here too, and exactly once — which is the point. A shadowed
// request is one request in dorang's ledger, not two (DESIGN §12): the
// reference call is issued by internal/shadow against the reference gateway's
// own credential and never re-enters this path, so cost, quota and budget for
// the sampled fraction stay the numbers they would have been with shadowing
// off. Metering the copy would make every figure wrong by the sample rate.
func (s *Server) finish(cfg *snapshot, rq *Request, rw *responseWriter) {
	status := rw.status
	if !rw.wrote {
		status = http.StatusOK
	}
	dur := cfg.now().Sub(rq.Start)
	s.metrics.observe(status, dur, rq.bytesIn, rw.n)

	if cfg.observer != nil && rq.cap != nil && !rq.cap.abandoned {
		s.metrics.observed.Add(1)
		s.observe(cfg, rq, rw, status, dur)
	}

	defer func() {
		if v := recover(); v != nil {
			s.metrics.meterPanics.Add(1)
			cfg.logf("server: meter panicked, request unaffected: %v", v)
		}
	}()
	name, family := "", FamilyNone
	if rq.Route != nil {
		name, family = rq.Route.Name, rq.Route.Family
	}
	keyID, userID, teamID := principalIDs(rq.Principal)
	cfg.meter.Record(Event{
		RequestID:   rq.ID,
		KeyID:       keyID,
		SecretID:    principalSecret(rq.Principal),
		UserID:      userID,
		TeamID:      teamID,
		Route:       name,
		Family:      family,
		Method:      rq.Method,
		Status:      status,
		Model:       rq.Model,
		Result:      rq.Result,
		DurationNS:  dur.Nanoseconds(),
		BytesIn:     rq.bytesIn,
		BytesOut:    rw.n,
		Passthrough: family == FamilyPassthrough,
		Streamed:    rw.sse,
		ErrorShape:  rw.errShape,
	})
}

func principalID(p Principal) string {
	if p == nil {
		return ""
	}
	return p.KeyID()
}

// principalIDs reads the whole identity in one place. A public route has no
// principal at all, which is three empty strings rather than a nil dereference.
func principalIDs(p Principal) (keyID, userID, teamID string) {
	if p == nil {
		return "", "", ""
	}
	return p.KeyID(), p.UserID(), p.TeamID()
}

// principalSecret reads the optional secret identity (DESIGN §11.2c).
//
// It is an optional interface rather than a sixth method on [Principal] for the
// same reason [AdminPrincipal] is one: a Principal that cannot name a secret is
// not an incomplete implementation, it is a deployment whose credentials are
// not rotated. The empty string is the honest answer there, and it is what the
// ledger column holds.
func principalSecret(p Principal) string {
	sp, ok := p.(SecretPrincipal)
	if !ok {
		return ""
	}
	return sp.SecretID()
}
