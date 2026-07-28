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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/router"
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

// Backend is where one deployment's traffic goes.
type Backend struct {
	// URL is the upstream's base URL.
	URL string
	// Family is the wire protocol the upstream speaks, which is not necessarily
	// the client's.
	Family Family
	// MaxTokens is the catalog's output ceiling for this deployment, used when
	// crossing into a family that requires one (DESIGN §10.7, trap one).
	MaxTokens int
}

// Gateway is the composition under test.
type Gateway struct {
	Router   *router.Router
	Backends map[string]Backend
	Client   *http.Client

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
	gw := &Gateway{Router: r.router, Backends: map[string]Backend{}, MaxHops: cfg.Fallback.MaxHops}
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
		gw.Backends[id] = Backend{URL: u.URL, Family: fam, MaxTokens: 4096}
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
	// §10.5a requires the prompt estimate to err pessimistic: an over-estimate
	// costs an unnecessary route to a larger model, an under-estimate costs a
	// hard failure the router cannot see. Four bytes per token with a fixed
	// margin is deliberately on the high side of the usual rule of thumb.
	rr.InputTokens = int64(len(c.Body)/3 + 16)
	if c.Prefix {
		// The chain is seeded with the model group so two groups can never
		// share an entry (DESIGN §7.4b). The group is what the alias resolves
		// to, not the name the client typed.
		group, _ := g.Router.Resolve(req.Model)
		rr.Digests = prefix.Compute(group, c.Body, 0)
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

// dispatch encodes the neutral request for the target's family, sends it, and
// converts the answer back into the client's family.
func (g *Gateway) dispatch(ctx context.Context, c Call, req *canonical.Request, d *router.Decision) (dispatchResult, router.Outcome) {
	be, ok := g.Backends[d.Deployment]
	if !ok {
		return dispatchResult{}, router.Outcome{Err: fmt.Errorf("no backend for deployment %q", d.Deployment)}
	}

	// DESIGN §7.2: upstream always receives the REAL model id.
	upBody, err := encodeRequest(be, req, d.UpstreamModel)
	if err != nil {
		return dispatchResult{}, router.Outcome{Err: err}
	}
	// The priority on the decision is already direction-normalized for this
	// engine (§7.5): it is the number that goes on the wire, not the canonical
	// class value. Emitting it is the transport's job, and this is the
	// transport.
	if upBody, err = emitPriority(upBody, d); err != nil {
		return dispatchResult{}, router.Outcome{Err: err}
	}

	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, be.URL+be.Family.shape().Path(), bytes.NewReader(upBody))
	if err != nil {
		return dispatchResult{}, router.Outcome{Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// §15.2.3: identity encoding upstream. The relay has to read the terminal
	// usage frame anyway, so it never wanted compressed bytes.
	httpReq.Header.Set("Accept-Encoding", "identity")

	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return dispatchResult{}, router.Outcome{Err: err, Total: time.Since(start)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		oc := router.Outcome{
			Err:    fmt.Errorf("upstream %d", resp.StatusCode),
			Status: resp.StatusCode,
			Total:  time.Since(start),
		}
		if ra := resp.Header.Get("retry-after"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				oc.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		if reset := resp.Header.Get("x-ratelimit-reset-requests"); reset != "" {
			if at, err := time.Parse(time.RFC3339, reset); err == nil {
				oc.ResetAt = at
			}
		}
		return dispatchResult{status: resp.StatusCode, body: body}, oc
	}

	if req.Stream {
		return g.relayStream(c, req, d, be, resp, start)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return dispatchResult{status: 200}, router.Outcome{Err: err, Total: time.Since(start)}
	}
	out, u, err := convertResponse(be.Family, c.Family, body, req.Model)
	if err != nil {
		return dispatchResult{status: 200}, router.Outcome{Err: err, Total: time.Since(start)}
	}
	oc := router.Outcome{Status: 200, Total: time.Since(start)}
	if u != nil {
		oc.InputTokens, oc.OutputTokens = int64(u.InputTokens), int64(u.OutputTokens)
	}
	return dispatchResult{status: 200, body: out}, oc
}

// relayStream converts an upstream stream into the client's family.
//
// Two paths, and the difference is the point of §15.2.3:
//   - same family, and the model name has to change: the single-pass scanner,
//     which never decodes a frame.
//   - crossing families: decode to neutral events and re-encode, because there
//     is no byte-level correspondence between the two framings.
func (g *Gateway) relayStream(c Call, req *canonical.Request, d *router.Decision, be Backend, resp *http.Response, start time.Time) (dispatchResult, router.Outcome) {
	var out bytes.Buffer
	oc := router.Outcome{Status: 200}

	if be.Family == FamilyOpenAI && c.Family == FamilyOpenAI {
		sc := openai.NewScanner(&out, openai.ScannerOptions{
			From: d.UpstreamModel, To: req.Model, CollectUsage: true,
		})
		n, err := io.Copy(sc, resp.Body)
		_ = sc.Flush()
		oc.Total = time.Since(start)
		if n > 0 {
			oc.FirstByteSent = true
		}
		if err != nil {
			oc.Err = err
			return dispatchResult{status: 200, body: out.Bytes(), firstByte: n > 0}, oc
		}
		if u, ok := sc.Usage(); ok {
			oc.InputTokens, oc.OutputTokens = int64(u.InputTokens), int64(u.OutputTokens)
		}
		// An in-band error frame is a failure the status can no longer report
		// (COMPATIBILITY 1.3). It is surfaced as one so the scenario can assert
		// that no further hop follows it.
		if bytes.Contains(out.Bytes(), []byte(`"error":`)) {
			oc.Err = errMidStream
		}
		return dispatchResult{status: 200, body: out.Bytes(), firstByte: n > 0}, oc
	}

	events, usage, midErr, err := decodeStream(be.Family, resp.Body)
	oc.Total = time.Since(start)
	if err != nil {
		oc.Err = err
		return dispatchResult{status: 200}, oc
	}
	body, werr := encodeStream(c.Family, req, events, usage)
	if werr != nil {
		oc.Err = werr
		return dispatchResult{status: 200}, oc
	}
	if usage != nil {
		oc.InputTokens, oc.OutputTokens = int64(usage.InputTokens), int64(usage.OutputTokens)
	}
	oc.FirstByteSent = len(body) > 0
	if midErr != nil {
		oc.Err = midErr
	}
	return dispatchResult{status: 200, body: body, firstByte: len(body) > 0}, oc
}

var errMidStream = errors.New("scenario: upstream failed mid-stream")

// -----------------------------------------------------------------------------
// wire conversion
// -----------------------------------------------------------------------------

func decodeRequest(f Family, b []byte) (*canonical.Request, error) {
	if f == FamilyAnthropic {
		return anthropic.DecodeRequest(b)
	}
	return openai.DecodeRequest(b)
}

// emitPriority splices the decision's wire priority into an encoded request.
//
// It is a splice rather than a field on the neutral request because the value
// is per-ENGINE: the same canonical class produces +10 on vLLM and -10 on
// SGLang, so it cannot be decided before the deployment is known. A neutral
// request carrying one number would be an inversion on one of the two engines,
// which is the failure DESIGN §7.5 exists to prevent.
func emitPriority(body []byte, d *router.Decision) ([]byte, error) {
	if d.PriorityField == "" && d.PriorityTier == "" {
		return body, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if d.PriorityField != "" {
		obj[d.PriorityField] = json.RawMessage(strconv.Itoa(d.Priority))
	}
	if d.PriorityTier != "" {
		tier, err := json.Marshal(d.PriorityTier)
		if err != nil {
			return nil, err
		}
		obj["service_tier"] = tier
	}
	return json.Marshal(obj)
}

func encodeRequest(be Backend, req *canonical.Request, upstreamModel string) ([]byte, error) {
	if be.Family == FamilyAnthropic {
		max := be.MaxTokens
		if max == 0 {
			max = 4096
		}
		return anthropic.MarshalRequest(req, &anthropic.EncodeOptions{
			Model: upstreamModel, DefaultMaxTokens: max,
		})
	}
	return openai.MarshalRequest(req, &openai.EncodeOptions{Model: upstreamModel})
}

// convertResponse renders an upstream response in the client's family, with the
// model field carrying the name the CLIENT asked for (DESIGN §7.2).
func convertResponse(from, to Family, body []byte, clientModel string) ([]byte, *canonical.Usage, error) {
	var (
		r   *canonical.Response
		err error
	)
	if from == FamilyAnthropic {
		r, err = anthropic.DecodeResponse(body, &anthropic.DecodeOptions{Model: clientModel})
	} else {
		r, err = openai.DecodeResponse(body, &openai.DecodeOptions{Model: clientModel})
	}
	if err != nil {
		return nil, nil, err
	}
	r.Model = clientModel

	var out []byte
	if to == FamilyAnthropic {
		out, err = anthropic.MarshalResponse(r, &anthropic.ResponseOptions{Model: clientModel})
	} else {
		out, err = openai.MarshalResponse(r, &openai.ResponseOptions{Model: clientModel})
	}
	return out, r.Usage, err
}

// decodeStream reads an upstream stream into neutral events. midErr is non-nil
// when the upstream delivered an in-band error.
func decodeStream(from Family, body io.Reader) (events []canonical.StreamEvent, usage *canonical.Usage, midErr error, err error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, nil, err
	}
	if from == FamilyAnthropic {
		evs, derr := anthropic.DecodeStream(raw, nil)
		if derr != nil {
			return nil, nil, nil, derr
		}
		for i := range evs {
			ev := evs[i]
			if ev.Type == canonical.EventError {
				midErr = errMidStream
				continue
			}
			if ev.Usage != nil {
				u := *ev.Usage
				usage = &u
			}
			events = append(events, ev)
		}
		return events, usage, midErr, nil
	}

	for _, frame := range sseFrames(raw) {
		payload := strings.TrimPrefix(frame, "data: ")
		if payload == "[DONE]" {
			continue
		}
		if strings.HasPrefix(payload, `{"error"`) {
			midErr = errMidStream
			continue
		}
		c, evs, derr := openai.DecodeChunk([]byte(payload), nil)
		if derr != nil {
			return nil, nil, nil, derr
		}
		if c.Usage != nil {
			usage = &canonical.Usage{
				InputTokens:  c.Usage.PromptTokens,
				OutputTokens: c.Usage.CompletionTokens,
			}
		}
		events = append(events, evs...)
	}
	return events, usage, midErr, nil
}

// streamWriter is the half of a wire package's StreamWriter this relay uses.
// Both families satisfy it, which is what keeps the two branches below to their
// one real difference — which encoder is constructed.
type streamWriter interface {
	WriteEvent(canonical.StreamEvent) error
	Close() error
}

func encodeStream(to Family, req *canonical.Request, events []canonical.StreamEvent, usage *canonical.Usage) ([]byte, error) {
	// buf.Bytes() captures the length at the moment it is called, so it must
	// never be evaluated before Close: `return buf.Bytes(), w.Close()` silently
	// truncates the terminal frames, which on the Anthropic side means a stream
	// with no message_delta and no message_stop — a shape this family's SDK
	// treats as a transport failure.
	var buf bytes.Buffer
	var w streamWriter
	if to == FamilyAnthropic {
		w = anthropic.NewStreamWriter(&buf, anthropic.StreamConfig{Model: req.Model})
	} else {
		w = openai.NewStreamWriter(&buf, openai.StreamConfig{
			Model: req.Model, IncludeUsage: req.IncludeUsage(),
		})
	}
	for _, ev := range events {
		if err := w.WriteEvent(ev); err != nil {
			return buf.Bytes(), err
		}
	}
	if usage != nil {
		if err := w.WriteEvent(canonical.StreamEvent{Type: canonical.EventUsage, Usage: usage}); err != nil {
			return buf.Bytes(), err
		}
	}
	err := w.Close()
	return buf.Bytes(), err
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
