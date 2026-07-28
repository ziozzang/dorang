// Package scenario implements the numbered scenario list of docs/DESIGN.md §14
// end to end.
//
// # Why this composes packages rather than the binary
//
// Every scenario below is stated as a property of the whole gateway — "the 7th
// waits", "traffic continues on another credential", "zero duplicated output
// after the first byte" — and none of them is a property of any single package.
// [Gateway] is the composition: internal/router decides, internal/capacity
// admits, internal/quota refuses, internal/prefix remembers, internal/wire
// converts, and testing/fake is the backend on the other end of a real socket.
//
// cmd/dorang assembles the same parts behind an HTTP listener. Switching this
// harness onto the assembled binary is a change to [Gateway.Do] and nothing
// else: the scenarios speak in [Call] and [Reply], which are already the shape
// of an HTTP exchange.
//
// # The index
//
//	§14.1  capacity_test.go    TestScenario01_TwoAccountsThreeEachBlocksTheSeventh
//	§14.2  capacity_test.go    TestScenario02_PerModelLimitReachesFourteenAcrossTwoModels
//	§14.3  capacity_test.go    TestScenario03_AccountTotalBindsBeforeTheModelLimit
//	§14.4  quota_test.go       TestScenario04_ExhaustedCredentialStepsAsideAndReturns
//	§14.5  quota_test.go       TestScenario05_BudgetNeverExceededUnderConcurrency
//	§14.6  fallback_test.go    TestScenario06_GroupThenClassDelegation
//	§14.7  fallback_test.go    TestScenario07_StreamFallsBackOnlyBeforeTheFirstByte
//	§14.8  alias_test.go       TestScenario08_AliasRealModelUpstreamRequestedNameInBody
//	§14.9  affinity_test.go    TestScenario09_SamePrefixSameTargetReorderedIsFree
//	§14.10 affinity_test.go    TestScenario10_StickyTTLElapsedReRoutes
//	§14.13 credential_test.go  TestScenario13_LegacyCredentialImport
//	§14.14 batch_test.go       TestScenario14_ThousandRowBatchWithPartialFailuresAndInteractiveLatency
//	§14.15 protocol_test.go    TestScenario15_AnthropicRequestThroughOpenAIBackendTwoTurns
//	§14.16 metering_test.go    TestScenario16_MeteringOnVsOffWithinBound
//	§14.17 capacity_test.go    TestScenario17_SaturationBoundedWakeupsFIFONoStarvation
//	§14.18 partition_test.go   TestScenario18_MidnightRolloverWithMeteringActive
//
//	§7.4a2 affinity_test.go    TestCredentialAffinityNeverSpillsAStatefulConversation
//	§7.4a2 affinity_test.go    TestCredentialAffinityNamesTheAccountAndResetWhenQuotaIsExhausted
//	§7.5   priority_test.go    TestPriorityEmitsOppositeWireValuesOnTheTwoSelfHostedEngines
//	§7.5   priority_test.go    TestPriorityWireValuesReachTheBackend
//	§10.5  priority_test.go    TestClientSuppliedPriorityIsIgnoredByDefault
//	§2.4   credential_test.go  TestLegacyCredentialUpgradesThroughTheAuthenticator
//
// §14.11 (two nodes, one killed) and §14.12 (configuration import golden) are
// not here. 11 needs a second process and a shared coordination backend, which
// is clustering work rather than scenario work; 12 is a golden comparison that
// belongs with internal/config, where the importer and its testdata live.
package scenario

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/backend"
	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/tokenest"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
	"github.com/ziozzang/dorang/testing/fake"
)

// -----------------------------------------------------------------------------
// clock
// -----------------------------------------------------------------------------

// clock is an injectable clock that is safe under -race. Every scenario that
// depends on a window resetting, a TTL elapsing or a partition rolling over
// advances this rather than sleeping.
type clock struct{ ns atomic.Int64 }

// epoch is the instant every scenario starts from. It is deliberately not
// midnight: the midnight rollover scenario needs to cross a boundary it did not
// start on.
var epoch = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

func newClock() *clock {
	c := &clock{}
	c.ns.Store(epoch.UnixNano())
	return c
}

func (c *clock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) advance(d time.Duration) { c.ns.Add(int64(d)) }
func (c *clock) set(t time.Time)         { c.ns.Store(t.UnixNano()) }

// -----------------------------------------------------------------------------
// polling helpers
// -----------------------------------------------------------------------------

// waitFor blocks until cond holds, or fails the test. Scenarios that involve
// blocked goroutines cannot assert on a snapshot taken too early, and a fixed
// sleep is both slower and flakier than a poll.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	// Spin first. A goroutine that is about to park usually has already parked
	// by the time the next scheduling slice comes round, and scenario 17 runs
	// this a thousand times in sequence — a flat 200µs sleep there costs more
	// than the whole rest of the suite.
	for range 64 {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Microsecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitWaiting blocks until exactly n Acquire calls are parked. It is how a
// scenario establishes arrival order: a waiter is launched only once the
// previous one is known to be queued, so "FIFO" is a statement about a known
// sequence rather than about goroutine scheduling.
func waitWaiting(t *testing.T, b *capacity.Broker, n int) {
	t.Helper()
	waitFor(t, func() bool { return b.Snapshot().Waiting == n }, fmt.Sprintf("%d parked waiters", n))
}

// blocks reports whether a request cannot be admitted right now. It uses
// TryAcquire, which never enqueues, so asking the question does not perturb the
// queues the scenario is about to assert on.
func blocks(b *capacity.Broker, req capacity.Request) bool {
	res, ok := b.TryAcquire(req)
	if ok {
		res.Release()
		return false
	}
	return true
}

// -----------------------------------------------------------------------------
// router rig
// -----------------------------------------------------------------------------

// rigOpts are the knobs a scenario turns when building a [rig].
type rigOpts struct {
	// clock, when set, is shared rather than created. A scenario whose quota
	// meters or budgets must move in step with the router's clock builds the
	// clock first and passes it here.
	clock    *clock
	capacity capacity.Config
	health   health.Options
	prefix   bool
	prefixTL time.Duration
	pricing  string
	quota    router.QuotaSource
	catalog  *catalog.Catalog
}

// rig is a router wired to the real subsystems with one injected clock, which is
// what lets a scenario advance a TTL or a quota window without sleeping.
type rig struct {
	t        *testing.T
	clock    *clock
	broker   *capacity.Broker
	health   *health.Tracker
	prefix   *prefix.Table
	interner *prefix.Interner
	router   *router.Router
}

func newRig(t *testing.T, cfg router.Config, o rigOpts) *rig {
	t.Helper()
	r := &rig{t: t, clock: o.clock}
	if r.clock == nil {
		r.clock = newClock()
	}

	o.capacity.Now = r.clock.now
	if o.capacity.SweepInterval == 0 {
		o.capacity.SweepInterval = -1
	}
	r.broker = capacity.New(o.capacity)
	t.Cleanup(r.broker.Close)

	o.health.Now = r.clock.now
	r.health = health.New(o.health)
	r.interner = prefix.NewInterner()

	deps := router.Deps{
		Capacity: r.broker,
		Health:   r.health,
		Interner: r.interner,
		Quota:    o.quota,
		Catalog:  o.catalog,
	}
	if o.prefix {
		ttl := o.prefixTL
		if ttl == 0 {
			ttl = time.Hour
		}
		r.prefix = prefix.NewTable(prefix.Options{TTL: ttl, Now: r.clock.now})
		deps.Prefix = r.prefix
	}
	if o.pricing != "" {
		c, err := pricing.ParseCatalog([]byte(o.pricing))
		if err != nil {
			t.Fatalf("pricing catalog: %v", err)
		}
		deps.Pricing = c
	}

	cfg.Now = r.clock.now
	rt, err := router.New(cfg, deps)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	r.router = rt
	return r
}

// route runs one routing question and fails the test if it is refused.
func (r *rig) route(req router.Request) *router.Decision {
	r.t.Helper()
	d, err := r.router.Route(context.Background(), req)
	if err != nil {
		r.t.Fatalf("Route(%s): %v", req.Model, err)
	}
	return d
}

// routeErr runs one routing question and requires a refusal.
func (r *rig) routeErr(req router.Request) *router.Error {
	r.t.Helper()
	d, err := r.router.Route(context.Background(), req)
	if err == nil {
		r.t.Fatalf("Route(%s): expected a refusal, got deployment %s credential %s",
			req.Model, d.Deployment, d.Credential)
	}
	var re *router.Error
	if !errors.As(err, &re) {
		r.t.Fatalf("Route(%s): expected *router.Error, got %T: %v", req.Model, err, err)
	}
	return re
}

// ok reports a successful attempt, which is what releases the reservation.
func (r *rig) ok(d *router.Decision) {
	r.router.Report(d, router.Outcome{Status: 200, Total: 10 * time.Millisecond})
}

// fail reports a failed attempt with an explicit cause.
func (r *rig) fail(d *router.Decision, c router.Cause) {
	r.router.Report(d, router.Outcome{Err: errAttempt, Cause: c, Total: 5 * time.Millisecond})
}

type attemptErr struct{}

func (attemptErr) Error() string { return "upstream refused" }

var errAttempt error = attemptErr{}

// -----------------------------------------------------------------------------
// the composed gateway
// -----------------------------------------------------------------------------

// Family is a wire protocol family.
type Family uint8

const (
	// FamilyOpenAI is chat completions.
	FamilyOpenAI Family = iota
	// FamilyAnthropic is the messages surface.
	FamilyAnthropic
)

func (f Family) String() string {
	if f == FamilyAnthropic {
		return "anthropic"
	}
	return "openai"
}

func (f Family) shape() fake.Shape {
	if f == FamilyAnthropic {
		return fake.ShapeAnthropic
	}
	return fake.ShapeOpenAI
}

// api is the wire shape this family is spelled as everywhere below the
// frontend. It is [catalog]'s vocabulary because that is what internal/backend
// selects an adapter and a client encoder by.
func (f Family) api() catalog.API {
	if f == FamilyAnthropic {
		return catalog.APIAnthropicMessages
	}
	return catalog.APIOpenAIChat
}

// Backend is where one deployment's traffic goes.
type Backend struct {
	// Provider is the resolved upstream, built by [backend.NewProvider] from
	// the fake's URL. It is a real Provider and not a description of one: the
	// endpoint derivation, the credential spelling, the wire adapter and the
	// self-hosted engine profile all come from the package that decides them in
	// production.
	Provider *backend.Provider
	// Family is the wire protocol the upstream speaks, which is not necessarily
	// the client's.
	Family Family
	// MaxTokens is the catalog's output ceiling for this deployment, used when
	// crossing into a family that requires one (DESIGN §10.7, trap one).
	MaxTokens int
}

// Gateway is the composition under test.
//
// L5 is the real [backend.Backend]. Nothing in this file encodes a request,
// converts a response or relays a stream: those are what the system under test
// is responsible for producing, and a harness that produced its own copy would
// hold its own bugs — which is how a complete second dispatch path once carried
// four tool-call fixes that production never got, with a green suite either
// side of it (DESIGN §17.1).
type Gateway struct {
	Router   *router.Router
	L5       *backend.Backend
	Backends map[string]Backend

	// MaxHops bounds fail-back attempts, mirroring router.FallbackConfig.
	MaxHops int
}

// Call is one client request.
type Call struct {
	// Family is the CLIENT's protocol, which the reply is rendered back into.
	Family Family
	// Body is the client's request bytes, verbatim.
	Body []byte

	Principal string
	Tenant    string
	Session   string
	Pins      []router.Pin
	// PriorityClass names the principal's class. A client-supplied priority in
	// the body is ignored by default (DESIGN §10.5).
	PriorityClass string
	Batch         bool
	// Prefix enables prefix affinity for this call.
	Prefix bool
}

// Reply is what the client sees.
type Reply struct {
	Status int
	Header http.Header
	Body   []byte

	// Decision is the attempt that produced the reply.
	Decision *router.Decision
	// Attempts lists every deployment tried, in order. Its length is the
	// fail-back hop count plus one.
	Attempts []string
	// UpstreamModel is what the last attempt actually sent upstream.
	UpstreamModel string
	// FirstByteSent records whether any response byte had already reached the
	// client when the last attempt failed. Once it is true no further hop is
	// permitted (DESIGN §7.6).
	FirstByteSent bool
	// RouteError is the routing refusal, when routing refused.
	RouteError *router.Error
}

// gwRig is a [rig] with a [Gateway] and its fake upstreams attached.
type gwRig struct {
	*rig
	gw  *Gateway
	ups map[string]*fake.Upstream
}

// newGateway builds a router and one fake upstream per deployment.
//
// backends is keyed by deployment id, which is what makes the mapping from a
// routing decision to a socket total: a decision naming a deployment with no
// backend is a wiring bug the harness reports rather than a silent skip.
func newGateway(t *testing.T, cfg router.Config, o rigOpts, backends map[string]fake.Options) *gwRig {
	t.Helper()
	r := newRig(t, cfg, o)
	g := &gwRig{rig: r, ups: make(map[string]*fake.Upstream, len(backends))}
	gw := &Gateway{
		Router:   r.router,
		L5:       backend.New(backend.Options{Now: r.clock.now}),
		Backends: map[string]Backend{},
		MaxHops:  cfg.Fallback.MaxHops,
	}
	// The deployment's KIND selects the self-hosted engine profile, and that
	// profile is load-bearing: it decides the direction of the priority field
	// and whether service_tier may be sent at all (§4.4, §7.5). Reading it off
	// the same router.Config the router was built from is what keeps the
	// engine the transport talks to and the engine the router normalized for
	// from being two different engines.
	kinds := make(map[string]string, len(backends))
	for gi := range cfg.Groups {
		for di := range cfg.Groups[gi].Deployments {
			d := &cfg.Groups[gi].Deployments[di]
			kinds[d.ID] = d.Kind
		}
	}
	for id, opts := range backends {
		if opts.Name == "" {
			opts.Name = id
		}
		u := fake.New(opts)
		t.Cleanup(u.Close)
		g.ups[id] = u
		fam := FamilyOpenAI
		if opts.Shape == fake.ShapeAnthropic {
			fam = FamilyAnthropic
		}
		p, err := backend.NewProvider(backend.Spec{
			Name: "prov-" + id, Kind: kinds[id], API: fam.api(), BaseURL: u.URL,
		})
		if err != nil {
			t.Fatalf("backend.NewProvider(%s): %v", id, err)
		}
		gw.Backends[id] = Backend{Provider: p, Family: fam, MaxTokens: 4096}
	}
	g.gw = gw
	return g
}

// do runs one call and fails the test on a transport error, returning the reply
// and any routing refusal.
func (g *gwRig) do(t *testing.T, c Call) (*Reply, error) {
	t.Helper()
	rep, err := g.gw.Do(context.Background(), c)
	if rep == nil {
		t.Fatalf("Do returned no reply: %v", err)
	}
	return rep, err
}

// Do runs one client request through the whole pipeline.
//
//	decode -> prefix digests -> route -> encode upstream -> dispatch
//	  -> convert back -> report (-> fail back and repeat)
func (g *Gateway) Do(ctx context.Context, c Call) (*Reply, error) {
	req, err := decodeRequest(c.Family, c.Body)
	if err != nil {
		return nil, fmt.Errorf("decode client request: %w", err)
	}

	rr := router.Request{
		Model:         req.Model,
		Principal:     c.Principal,
		Tenant:        c.Tenant,
		Session:       c.Session,
		Pins:          c.Pins,
		PriorityClass: c.PriorityClass,
		Stream:        req.Stream,
		Batch:         c.Batch,
		Required:      req.RequiredCapabilities(),
	}
	if req.MaxTokens != nil {
		rr.MaxOutputTokens = int64(*req.MaxTokens)
	}
	// §10.5a requires the prompt estimate to err pessimistic, and to be
	// script-aware rather than a division of the body length: a base64 image is
	// body bytes, and dividing those by three charges a photograph several
	// hundred times what it costs. internal/tokenest is the same estimator the
	// dispatcher uses, walking the same decoded request.
	est := tokenest.Request(req)
	rr.InputTokens = est.Tokens
	rr.InputTokensExact, rr.InputTokensMethod = est.Exact, est.Method
	if c.Prefix {
		// The chain is seeded with the tenant and then the model group, so
		// neither two groups nor two tenants can share an entry (DESIGN §7.4b
		// as amended). The group is what the alias resolves to, not the name
		// the client typed.
		group, _ := g.Router.Resolve(req.Model)
		rr.Digests = prefix.Compute(c.Tenant, group, c.Body, 0)
	}

	reply := &Reply{Header: http.Header{}}

	for attempt := 0; ; attempt++ {
		d, rerr := g.Router.Route(ctx, rr)
		if rerr != nil {
			var re *router.Error
			if errors.As(rerr, &re) {
				reply.RouteError = re
				// Once a byte has reached the client the status is already 200
				// and cannot change (COMPATIBILITY 1.3). Overwriting it here
				// would report a refusal the client never saw.
				if !reply.FirstByteSent {
					reply.Status = re.Status
				}
			}
			return reply, rerr
		}
		reply.Decision = d
		reply.Attempts = append(reply.Attempts, d.Deployment)
		reply.UpstreamModel = d.UpstreamModel

		out, oc := g.dispatch(ctx, c, req, d)
		g.Router.Report(d, oc)

		if oc.OK() {
			reply.Status = out.status
			reply.Body = out.body
			reply.FirstByteSent = out.firstByte
			g.stampHeaders(reply, req.Model, d)
			return reply, nil
		}

		reply.Status = out.status
		reply.Body = out.body
		reply.FirstByteSent = out.firstByte

		// DESIGN §7.6: after the first byte a fail-back is refused, and the
		// refusal is a routing error rather than a silent retry. Setting
		// Previous is what asks for the next hop; the router decides whether
		// there is one.
		if attempt >= g.MaxHops {
			g.stampHeaders(reply, req.Model, d)
			// The hop budget is spent and the attempt failed. Returning nil
			// here would report a failure as a success, which is the one thing
			// a fail-back harness must never do.
			return reply, oc.Err
		}
		rr.Previous = d
	}
}

// stampHeaders attaches the identification set DESIGN §10.4 always attaches.
func (g *Gateway) stampHeaders(r *Reply, requested string, d *router.Decision) {
	r.Header.Set("x-dorang-model", requested)
	r.Header.Set("x-dorang-upstream-model", d.UpstreamModel)
	r.Header.Set("x-dorang-deployment", d.Deployment)
	r.Header.Set("x-dorang-provider", d.Provider)
	r.Header.Set("x-dorang-credential", d.Credential)
	r.Header.Set("x-dorang-attempt", strconv.Itoa(d.Attempt))
	r.Header.Set("x-dorang-route-reason", d.Reason)
}

type dispatchResult struct {
	status    int
	body      []byte
	firstByte bool
}

// dispatch runs one attempt through internal/backend — the same L5 the
// interactive dispatcher runs — and renders its result the way internal/app
// does.
//
// What is faked here is a dependency and nothing else: the upstream is a fake
// server on a real socket, and the client's connection is an
// [httptest.ResponseRecorder]. What is NOT faked is anything the system under
// test produces. The request encoding, the priority splice, the endpoint, the
// response conversion and the streaming relay all come from internal/backend,
// so a defect in any of them fails a scenario instead of being papered over by
// a second implementation that happens to be right (DESIGN §17.1).
func (g *Gateway) dispatch(ctx context.Context, c Call, req *canonical.Request, d *router.Decision) (dispatchResult, router.Outcome) {
	be, ok := g.Backends[d.Deployment]
	if !ok {
		return dispatchResult{}, router.Outcome{Err: fmt.Errorf("no backend for deployment %q", d.Deployment)}
	}

	// The client's socket. A stream is written through it frame by frame by the
	// real relay; a recorder is where those frames land instead of a network
	// connection. It implements http.Flusher, so the relay's per-frame flush is
	// exercised rather than skipped.
	w := httptest.NewRecorder()

	res := g.L5.Do(ctx, backend.Target{
		Provider:      be.Provider,
		Credential:    d.Credential,
		UpstreamModel: d.UpstreamModel,
		// Already direction-normalized for this engine by internal/router
		// (§7.5). It is passed through untouched: the harness computing its own
		// wire value is how a suite ends up agreeing with a router that has the
		// sign backwards.
		PriorityField: d.PriorityField,
		Priority:      d.Priority,
		PriorityTier:  d.PriorityTier,
	}, &backend.Call{
		Op:        backend.OpChat,
		ClientAPI: c.Family.api(),
		// DESIGN §7.2: the CLIENT's name goes in the answer, and the real
		// upstream id goes on the wire. Both halves are the backend's to apply.
		Model:            req.Model,
		Body:             c.Body,
		Request:          req,
		Stream:           req.Stream,
		DefaultMaxTokens: be.MaxTokens,
		IncludeUsage:     req.IncludeUsage(),
	}, w)

	return backendResult(res, w)
}

// backendResult renders one exchange in the two shapes the loop above needs: a
// [router.Outcome] for Report, and the bytes the client saw.
//
// It mirrors internal/app's function of the same name, and that mirroring is
// the one duplication left in this file. It exists because app's version is
// unexported; see the note in DESIGN §17.1.
func backendResult(res backend.Result, w *httptest.ResponseRecorder) (dispatchResult, router.Outcome) {
	out := dispatchResult{status: res.Status, firstByte: res.FirstByteSent}
	switch {
	case res.FirstByteSent:
		// Once a byte is out the status is 200 and cannot change
		// (COMPATIBILITY 1.3), and the body is whatever reached the client.
		out.status, out.body = http.StatusOK, w.Body.Bytes()
	case res.Body != nil:
		out.body = res.Body
	case res.ErrorBody != nil:
		// The upstream's own envelope, credential-scrubbed. A scenario that
		// asserts on an error body is asserting on what the vendor said.
		out.body = res.ErrorBody
	}

	if res.Err == nil {
		return out, router.Outcome{
			Status: res.Status, TTFT: res.TTFT, Total: res.Total,
			FirstByteSent: res.FirstByteSent,
			InputTokens:   int64(res.Usage.InputTokens),
			OutputTokens:  int64(res.Usage.OutputTokens),
		}
	}
	oc := router.Outcome{
		Err: res.Err, Status: res.Status, Cause: upstreamCause(res),
		TTFT: res.TTFT, Total: res.Total,
		FirstByteSent: res.FirstByteSent, RetryAfter: res.RetryAfter,
	}
	return out, oc
}

// upstreamCause is the half of the classification a frontend owns.
//
// [router.Classify] reads the status line and stops there, deliberately, so
// [router.Report] can do the rest for itself. The one thing it cannot do is
// recognise a context overflow, because a 400 that overflowed and a 400 that
// was malformed differ only in the body — which is why [router.Outcome.Cause]
// documents reading it as the caller's job. internal/backend has already
// decoded that body out of whichever of the five upstream envelope shapes
// arrived; this asks the decoded fields the one question.
//
// Everything else is left zero on purpose. Handing the router a cause derived
// from the status line here would be this harness deciding something the router
// decides in production, and the two could then disagree without any test
// noticing.
//
// The field is NativeMessage and not Message, exactly as internal/app's
// upstreamCause reads it: COMPATIBILITY §11.3 gives the envelope dorang's own
// canonical wording, and the upstream's own sentence — the only place an
// overflow signature exists — survives beside it. Reading Message here scans
// dorang's constant for vLLM's phrasing and never matches.
func upstreamCause(res backend.Result) router.Cause {
	if res.Status < 400 || res.Err == nil {
		return router.CauseNone
	}
	return router.ClassifyBody(res.Status, res.Err.Code, res.Err.NativeMessage)
}

// -----------------------------------------------------------------------------
// the client's own protocol
// -----------------------------------------------------------------------------

// decodeRequest is the frontend's half: turning the caller's bytes into the
// neutral form. internal/server does this in the assembled binary, over the
// same two wire packages.
func decodeRequest(f Family, b []byte) (*canonical.Request, error) {
	if f == FamilyAnthropic {
		return anthropic.DecodeRequest(b)
	}
	return openai.DecodeRequest(b)
}

// sseFrames cuts an SSE body into frames.
func sseFrames(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n\n")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
