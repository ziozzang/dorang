package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/luaext"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/redact"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// Request headers dorang reads that internal/server does not name itself.
const (
	// HeaderAllowLossy is the §10.1 opt-in: a construct listed here may be
	// downgraded instead of refusing the request.
	HeaderAllowLossy = "X-Dorang-Allow-Lossy"
	// HeaderSession names the conversation for session stickiness (§7.4a).
	HeaderSession = "X-Dorang-Session"
)

// dispatcher owns everything past the gate: conversion, routing, capacity, the
// upstream call, and writing the response body. It satisfies server.Dispatcher.
//
// It holds its subsystems behind an atomic pointer because a hot reload rebuilds
// the router, the price catalog and the upstream table, and an in-flight request
// must keep the snapshot it started with (DESIGN §4.1, §15.2).
type dispatcher struct {
	st     atomic.Pointer[dispatchState]
	client *http.Client
	logf   func(string, ...any)
	now    func() time.Time
}

type dispatchState struct {
	router    *router.Router
	pricing   *pricing.Catalog
	catalog   *catalog.Catalog
	upstreams *upstreamTable
	quota     *quotaSet
	// budget is the durable spend gate (DESIGN §6.4, §9.6). Nil leaves budgets
	// unenforced, which is only the case when no store is configured.
	budget *budgetGate
	// responses is the Responses API's server-side state (DESIGN §9.2
	// [R1-C7]). Nil when no store is configured, which makes `store: true`
	// answer a named 501 rather than silently not storing.
	responses *responsesStore

	// hooks are the §11.5 extension points. Nil is the disabled engine and
	// every call through it is one branch, which is what makes "disabled by
	// default on the hot path" true rather than aspirational.
	hooks *luaext.Engine

	prefixOn bool
	chunk    int

	// maxResponseBytes bounds a non-streaming upstream body. Zero uses
	// DefaultMaxResponseBytes.
	maxResponseBytes int64
}

// DefaultMaxResponseBytes bounds the non-streaming upstream response.
//
// The error path on this same function has always been bounded at 1 MiB; the
// success path was an unbounded io.ReadAll, which made a hostile or merely
// broken backend able to OOM the gateway from the far side of the trust
// boundary — and at three to four times the body size, because the buffer is
// then unmarshalled and re-marshalled.
//
// It matches DefaultMaxBodyBytes rather than the 1 MiB error cap because a
// legitimate answer can be large: an embeddings response for a big batch of
// inputs is megabytes of floats, and a ceiling below the request cap would
// refuse answers to requests dorang itself accepted.
const DefaultMaxResponseBytes int64 = 32 << 20

// errUpstreamTooLarge is the sentinel for a body that hit the ceiling.
var errUpstreamTooLarge = errors.New("upstream response exceeded the configured ceiling")

// readUpstreamBody reads a complete upstream answer under a hard ceiling.
//
// One byte past the limit is read so that hitting it is detected rather than
// silently truncating the answer — a truncated JSON body would fail to decode
// somewhere further along and be reported as a protocol error, which sends
// whoever debugs it to the wrong place entirely.
func readUpstreamBody(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultMaxResponseBytes
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errUpstreamTooLarge
	}
	return b, nil
}

func newDispatcher(client *http.Client, logf func(string, ...any), now func() time.Time) *dispatcher {
	return &dispatcher{client: client, logf: logf, now: now}
}

func (d *dispatcher) swap(st *dispatchState) { d.st.Store(st) }
func (d *dispatcher) state() *dispatchState  { return d.st.Load() }

// call is one client request, decoded once and reused across fail-back hops.
//
// Exactly one of the typed request pointers is set, selected by kind. They are
// separate fields rather than an `any` because every consumer switches on kind
// already, and a type assertion would add a second, independent way for the two
// to disagree.
type call struct {
	kind      callKind
	clientAPI catalog.API
	model     string
	body      []byte
	creq      *canonical.Request
	rreq      router.Request
	stream    bool
	allowUsg  bool

	rerankReq *canonical.RerankRequest
	modReq    *canonical.ModerationRequest
	speechReq *canonical.SpeechRequest
	transReq  *canonical.TranscriptionRequest
	imageReq  *canonical.ImageRequest

	// respEcho carries the Responses-API request fields that surface echoes
	// back on its answer. They belong to the request, so regenerating them from
	// dorang's defaults would tell the client dorang changed settings it never
	// touched.
	respEcho *openai.ResponsesOptions
	// responseID is dorang's own id for a stored Responses exchange. dorang owns
	// the state (DESIGN §9.2 [R1-C7]), so it owns the id: an upstream id would
	// be meaningless to the store and would change on a fail-back hop.
	responseID string
}

type callKind uint8

const (
	callChat callKind = iota
	callCountTokens
	callEmbeddings

	// The T1 surface of COMPATIBILITY §0.
	callCompletions
	callResponses
	callModerations
	callRerank
	callSpeech
	callTranscription
	callImages
)

// callKindNames is the label set, a table rather than a switch so that a new
// kind without a name is a compile-time hole rather than a silent "chat".
var callKindNames = [...]string{
	callChat:          "chat",
	callCountTokens:   "count_tokens",
	callEmbeddings:    "embeddings",
	callCompletions:   "completions",
	callResponses:     "responses",
	callModerations:   "moderations",
	callRerank:        "rerank",
	callSpeech:        "audio.speech",
	callTranscription: "audio.transcription",
	callImages:        "images",
}

// String names the kind for a human-readable message.
func (k callKind) String() string {
	if int(k) < len(callKindNames) && callKindNames[k] != "" {
		return callKindNames[k]
	}
	return "chat"
}

// code is the kind's stem in a machine-readable error code. It is the name with
// the dot replaced, because a code is matched by application code and a '.' in
// one reads as a namespace separator that dorang does not have.
func (k callKind) code() string {
	name := k.String()
	out := []byte(name)
	for i := range out {
		if out[i] == '.' {
			out[i] = '_'
		}
	}
	return string(out)
}

// Dispatch implements server.Dispatcher.
func (d *dispatcher) Dispatch(ctx context.Context, rq *server.Request, w http.ResponseWriter) error {
	st := d.state()
	if st == nil {
		return server.NewError(http.StatusServiceUnavailable, server.TypeAPIError,
			"the gateway is not configured").WithCode("not_configured")
	}
	c, err := d.decode(st, rq)
	if err != nil {
		return err
	}

	// on_request (DESIGN §11.5). The Enabled test is the whole cost when hooks
	// are off; the view is built only when one is registered. A hook that fails
	// any of its ceilings returns the zero decision, which permits — fail-open
	// — and only a hook that ran to completion can refuse.
	if st.hooks.Enabled(luaext.HookRequest) {
		v := requestView(rq, c)
		if hd := st.hooks.OnRequest(ctx, &v); hd.Denied {
			return hookDenied(&hd)
		}
	}

	var lastErr error
	for {
		dec, rerr := st.router.Route(ctx, c.rreq)
		if rerr != nil {
			if lastErr != nil {
				// A hop that cannot be taken reports the failure that made us
				// look for one, not "the hop budget is spent": the caller needs
				// to know what went wrong upstream, not how dorang gave up.
				return lastErr
			}
			return routeError(rerr)
		}
		// on_route (DESIGN §11.5). It sees the decision and may refuse it. It
		// cannot ask for a different one — see luaext.RouteDecision — so a
		// refusal is terminal.
		//
		// The reservation is released directly rather than through
		// Router.Report, because nothing happened upstream: Report would either
		// record a success the deployment never served, resetting its failure
		// counter, or a failure it never caused, opening its circuit.
		if st.hooks.Enabled(luaext.HookRoute) {
			rv := routeView(rq, c, dec)
			if rd := st.hooks.OnRoute(ctx, &rv); rd.Denied {
				dec.Reservation.Release()
				return server.NewError(http.StatusForbidden, server.TypePermission, rd.Reason).
					WithCode(rd.Code)
			}
		}
		// The budget is held before the request goes upstream and released the
		// moment it is clear no money was spent (DESIGN §6.4, R1-20). It is
		// taken per HOP: a fail-back to another deployment is a different
		// price, so the estimate is retaken rather than carried over.
		hold, berr := st.budget.reserve(ctx, st, c, dec, rq)
		if berr != nil {
			// Exceeding a budget is terminal and is NOT a fallback condition
			// (§6.4): there is no cheaper deployment that makes the money
			// reappear, and trying one would spend a different subject's
			// budget on a model the caller never asked for.
			st.router.Report(dec, router.Outcome{Err: berr, Cause: router.CauseBudgetExceeded})
			return berr
		}

		res := d.attempt(ctx, st, c, dec, rq, w)
		st.router.Report(dec, res.outcome)

		if res.err == nil {
			// Pricing and token counts are filled BEFORE the body goes out, so
			// the extension headers can carry them: they are stamped on the
			// first write and anything set afterwards is invisible to the
			// client (DESIGN §10.4). A stream has already written by now, which
			// is why its post-hoc values travel by the opt-in usage event
			// instead.
			d.settle(st, c, dec, rq, res)
			// The estimate was an upper bound, so settlement only ever releases
			// budget — which is why settling after the answer is safe.
			hold.settle(rq.Result.CostNanoUSD)
			// on_response (DESIGN §11.5). It runs after pricing so the cost is
			// in the view, and before the body is written so its wall-clock
			// ceiling is a bound the caller can see rather than one nobody
			// waits for. It observes; there is nothing left to refuse.
			if st.hooks.Enabled(luaext.HookResponse) {
				pv := responseView(rq, dec, &res, http.StatusOK)
				st.hooks.OnResponse(ctx, &pv)
			}
			if res.body != nil {
				// The Responses API's server-side state is written here, after
				// the answer is complete and before it goes out: a stored
				// response the client never received is a reference it can
				// resolve to a turn it never saw.
				if err := d.storeResponse(ctx, st, c, rq, res.body); err != nil {
					return err
				}
				return writeBody(w, res.body, res.contentType)
			}
			return nil
		}
		// The attempt produced no answer, so it cost nothing. Refunding in full
		// is R1-20: a request that never reached an upstream must not consume
		// budget, and a hop that failed is such a request.
		hold.release()
		lastErr = res.err
		if res.outcome.FirstByteSent || !res.retryable {
			return res.err
		}
		c.rreq.Previous = dec
	}
}

// decode turns one HTTP request into a routing question.
func (d *dispatcher) decode(st *dispatchState, rq *server.Request) (*call, error) {
	body := rq.Body.Bytes()
	c := &call{model: rq.Model, body: body, stream: rq.Stream}

	switch rq.Route.Family {
	case server.FamilyOpenAIChat:
		creq, err := openai.DecodeRequest(body)
		if err != nil {
			return nil, server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
				err.Error()).WithCode("invalid_request")
		}
		c.kind, c.clientAPI, c.creq = callChat, catalog.APIOpenAIChat, creq
		c.allowUsg = creq.IncludeUsage()
	case server.FamilyAnthropicMessages:
		creq, err := anthropic.DecodeRequest(body)
		if err != nil {
			return nil, server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
				err.Error()).WithCode("invalid_request")
		}
		c.kind, c.clientAPI, c.creq = callChat, catalog.APIAnthropicMessages, creq
	case server.FamilyAnthropicCountTokens:
		creq, err := anthropic.DecodeCountTokensRequest(body)
		if err != nil {
			return nil, server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
				err.Error()).WithCode("invalid_request")
		}
		c.kind, c.clientAPI, c.creq = callCountTokens, catalog.APIAnthropicMessages, creq
	case server.FamilyOpenAIEmbeddings:
		// Embeddings has no neutral representation, so it is relayed rather
		// than converted: the body goes upstream with only the model name
		// replaced, and the answer comes back with the client's name restored.
		// That is a real limit — an embeddings request cannot cross protocol
		// families — and it is refused explicitly below rather than mangled.
		c.kind, c.clientAPI = callEmbeddings, catalog.APIOpenAIChat
	default:
		if err := d.decodeT1(st, rq, c); err != nil {
			return nil, err
		}
	}

	// The model-in-the-path aliases resolved the model from the URL, and the
	// gate authorized THAT name. The neutral request has to carry the same one,
	// or the request is dispatched under a name the allow-list never saw.
	if c.creq != nil && rq.Model != "" {
		c.creq.Model = rq.Model
	}

	tenant := tenantOf(rq.Principal)
	c.rreq = router.Request{
		Model:     rq.Model,
		Principal: principalID(rq),
		// The leading component of the session pin (§7.4a) and of the prefix
		// chain (§7.4b). It was never assigned outside the test harness, which
		// made both keys tenant-scoped in their type and process-wide in the
		// shipped binary: two tenants presenting the same session id shared a
		// pin, and a prefix entry recorded by one tenant answered another
		// tenant's lookup.
		Tenant:          tenant,
		Session:         rq.HTTP.Header.Get(HeaderSession),
		AllowLossy:      parseAllowLossy(rq.HTTP.Header.Get(HeaderAllowLossy)),
		InputTokens:     estimateInputTokens(body),
		MaxOutputTokens: maxOutputTokens(c.creq),
		Stream:          rq.Stream,
		PrincipalMax:    principalMaxParallel(rq.Principal),
	}
	if c.creq != nil {
		c.rreq.Required = c.creq.RequiredCapabilities()
	}
	if st.prefixOn && st.chunk > 0 {
		// The chain is seeded with the TENANT and then the client-facing model
		// group, so two models never share a prefix entry and neither do two
		// tenants. Cut at byte boundaries — there is no tokenizer on this path
		// (§7.4b).
		c.rreq.Digests = prefix.Compute(tenant, rq.Model, body, st.chunk)
	}
	return c, nil
}

// tenantOf names the isolation boundary a cache-affinity key leads with.
//
// Team first, because a team is what §7.4a means by a tenant: colleagues
// sharing a conversation and a prompt cache is the behaviour session
// stickiness and prefix affinity exist to produce. A key with no team falls
// back to its user and then to itself, which is narrower than the truth rather
// than wider — the failure mode is a missed cache hit, not a shared one.
func tenantOf(p server.Principal) string {
	if p == nil {
		return ""
	}
	if t := p.TeamID(); t != "" {
		return "team:" + t
	}
	if u := p.UserID(); u != "" {
		return "user:" + u
	}
	if k := p.KeyID(); k != "" {
		return "key:" + k
	}
	return ""
}

// principalMaxParallel is the concurrency ceiling the calling subject carries,
// the most restrictive across key, user and team.
//
// It is read here rather than inside internal/capacity because capacity has
// only the static YAML table, keyed by principal id, and the per-key column
// lives on the authorization snapshot the request already holds. That gap is
// why max_parallel_requests enforced nothing: the value was stored, imported
// and administered, and the broker never saw it.
func principalMaxParallel(p server.Principal) int {
	l, ok := p.(interface{ MaxParallel() int })
	if !ok {
		return 0
	}
	return l.MaxParallel()
}

// result is one attempt's outcome, in the two shapes its two consumers need:
// a router.Outcome for Report, and an error for the client.
type result struct {
	outcome   router.Outcome
	err       error
	retryable bool
	usage     canonical.Usage
	ttft      time.Duration
	total     time.Duration
	// body is the converted non-streaming answer, held rather than written so
	// that pricing lands on the request before the headers are stamped.
	body []byte
	// contentType labels body. It is not always application/json: speech
	// answers audio and a transcription asked for in srt or vtt answers text,
	// and mislabelling either gives the client something it cannot play or
	// parse.
	contentType string
}

// attempt makes one upstream call and relays its answer.
func (d *dispatcher) attempt(ctx context.Context, st *dispatchState, c *call,
	dec *router.Decision, rq *server.Request, w http.ResponseWriter) result {

	var res result
	start := d.now()

	up, ok := st.upstreams.provider(dec.Provider)
	if !ok {
		res.err = server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"no upstream is configured for provider "+dec.Provider).WithCode("no_upstream")
		res.outcome = router.Outcome{Err: res.err, Cause: router.CauseUpstream5xx}
		return res
	}
	if err := checkFamily(c, up.api); err != nil {
		res.err = err
		res.outcome = router.Outcome{Err: err}
		return res
	}

	call, err := d.encodeUpstream(c, dec, up)
	if err != nil {
		res.err = err
		res.outcome = router.Outcome{Err: err}
		return res
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, call.endpoint, bytes.NewReader(call.body))
	if err != nil {
		res.err = server.NewError(http.StatusBadGateway, server.TypeAPIError, err.Error()).
			WithCode("upstream_request")
		res.outcome = router.Outcome{Err: res.err, Cause: router.CauseUpstream5xx}
		return res
	}
	hreq.Header.Set("Content-Type", call.contentType)
	// Identity encoding, always: a compressed stream cannot be relayed frame by
	// frame and the terminal usage frame has to be readable (see openai.Scanner).
	hreq.Header.Set("Accept-Encoding", "identity")
	if c.stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	applyCredential(up.api, st.upstreams.secret(dec.Credential), hreq.Header)

	resp, err := d.client.Do(hreq)
	if err != nil {
		res.total = d.now().Sub(start)
		cause := router.CauseUpstream5xx
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			cause, status = router.CauseTimeout, http.StatusGatewayTimeout
		}
		// The transport error text names the upstream URL, which is an internal
		// host and port, and Go's url.Error keeps the username too. The
		// passthrough engine already refuses to relay it for exactly that
		// reason; this path used to hand the caller a map of the operator's
		// network. It goes to the log instead.
		d.logf("app: upstream %s/%s unreachable: %v", dec.Provider, dec.Deployment, err)
		res.err = server.NewError(status, server.TypeAPIError,
			"could not reach the upstream provider").WithCode("upstream_unreachable")
		res.outcome = router.Outcome{Err: err, Cause: cause, Total: res.total}
		res.retryable = true
		return res
	}
	defer resp.Body.Close()
	res.ttft = d.now().Sub(start)

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		res.total = d.now().Sub(start)
		e := server.Normalize(resp.StatusCode, body)
		// Normalize kept the upstream's text out of the client's envelope. What
		// is left is the recorded copy, and that copy can still carry the key —
		// a provider echoing "Invalid API key: sk-…", or a hostile backend
		// answering with the x-api-key header it was just handed. The secret is
		// known exactly here: it is the one this attempt sent.
		if e.NativeMessage != "" || e.NativeType != "" {
			secret := st.upstreams.secret(dec.Credential) // pragma: allowlist secret — a lookup, not a literal
			e.NativeMessage = redact.Text(e.NativeMessage, secret)
			e.NativeType = redact.Text(e.NativeType, secret)
			if e.NativeMessage != "" {
				d.logf("app: upstream %s/%s answered %d: %s",
					dec.Provider, dec.Deployment, resp.StatusCode, e.NativeMessage)
			}
		}
		cause := router.Classify(resp.StatusCode, nil)
		res.err = e
		res.outcome = router.Outcome{
			Err: e, Status: resp.StatusCode, Cause: cause,
			TTFT: res.ttft, Total: res.total,
			RetryAfter: retryAfter(resp.Header),
		}
		res.retryable = cause.Chainable()
		return res
	}

	// Everything the client can still learn from the headers is decided here,
	// before the first byte: the extension headers are stamped on the first
	// write and anything set afterwards is invisible (DESIGN §10.4).
	fillRouteResult(rq, dec)

	if c.stream {
		usage, err := d.relayStream(c, dec, up, resp, w)
		res.total = d.now().Sub(start)
		res.usage = usage
		if err != nil {
			res.err = err
			res.outcome = router.Outcome{Err: err, Cause: router.CauseUpstream5xx,
				TTFT: res.ttft, Total: res.total, FirstByteSent: true}
			return res
		}
		res.outcome = router.Outcome{
			TTFT: res.ttft, Total: res.total, FirstByteSent: true,
			InputTokens: int64(usage.InputTokens), OutputTokens: int64(usage.OutputTokens),
		}
		return res
	}

	body, err := readUpstreamBody(resp.Body, st.maxResponseBytes)
	if err != nil {
		res.total = d.now().Sub(start)
		if errors.Is(err, errUpstreamTooLarge) {
			// Not retryable: the same deployment will send the same oversized
			// answer, and a fail-back hop would buffer another one.
			res.err = server.NewError(http.StatusBadGateway, server.TypeAPIError,
				"the upstream answer is larger than this gateway will buffer").
				WithCode("upstream_response_too_large")
			res.outcome = router.Outcome{Err: err, Cause: router.CauseUpstream5xx, Total: res.total}
			return res
		}
		res.err = server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"could not read the upstream response").WithCode("upstream_body")
		res.outcome = router.Outcome{Err: err, Cause: router.CauseUpstream5xx, Total: res.total}
		res.retryable = true
		return res
	}
	out, ct, usage, err := d.convertResponse(c, dec, up, body, resp.Header)
	res.total = d.now().Sub(start)
	if err != nil {
		res.err = err
		res.outcome = router.Outcome{Err: err, Cause: router.CauseUpstream5xx, Total: res.total}
		return res
	}
	res.usage = usage
	res.body = out
	res.contentType = ct
	res.outcome = router.Outcome{
		TTFT: res.ttft, Total: res.total,
		InputTokens: int64(usage.InputTokens), OutputTokens: int64(usage.OutputTokens),
	}
	return res
}

// writeBody sends a complete non-streaming answer.
func writeBody(w http.ResponseWriter, body []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/json"
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	_, err := w.Write(body)
	return err
}

// upstreamRequest is one rendered upstream call.
type upstreamRequest struct {
	body        []byte
	endpoint    string
	contentType string
}

func jsonUpstream(body []byte, endpoint string) upstreamRequest {
	return upstreamRequest{body: body, endpoint: endpoint, contentType: "application/json"}
}

// encodeUpstream renders the request for the deployment that will serve it and
// names the endpoint it goes to.
func (d *dispatcher) encodeUpstream(c *call, dec *router.Decision, up *upstream) (upstreamRequest, error) {
	switch c.kind {
	case callEmbeddings:
		body, err := replaceModel(c.body, dec.UpstreamModel)
		if err != nil {
			return upstreamRequest{}, server.NewError(http.StatusBadRequest,
				server.TypeInvalidRequest, err.Error()).WithCode("invalid_request")
		}
		return jsonUpstream(body, up.endpoint(pathEmbeddings)), nil

	case callCountTokens:
		if up.api != catalog.APIAnthropicMessages {
			return upstreamRequest{}, server.NewError(http.StatusNotImplemented,
				server.TypeNotImplemented,
				"count_tokens has no equivalent on this deployment's protocol").
				WithCode("count_tokens_unsupported")
		}
		body, err := anthropic.MarshalRequest(c.creq, &anthropic.EncodeOptions{
			Model:            dec.UpstreamModel,
			DefaultMaxTokens: 1,
		})
		if err != nil {
			return upstreamRequest{}, encodeError(err)
		}
		return jsonUpstream(body, up.endpoint(pathCountTokens)), nil

	case callRerank, callModerations, callSpeech, callTranscription, callImages:
		return d.encodeT1Upstream(c, dec, up)
	}

	req := *c.creq
	if dec.PriorityField != "" {
		// The engine's own priority field, already direction-normalized for
		// THIS engine by the router (§7.5). Extra is the neutral request's
		// pass-through map, so it reaches the wire without either encoder
		// needing to know the field exists.
		req.Extra = cloneExtra(req.Extra)
		req.Extra[dec.PriorityField] = json.RawMessage(strconv.Itoa(dec.Priority))
	}

	switch up.api {
	case catalog.APIAnthropicMessages:
		body, err := anthropic.MarshalRequest(&req, &anthropic.EncodeOptions{
			Model:            dec.UpstreamModel,
			AllowLossy:       c.rreq.AllowLossy,
			DefaultMaxTokens: d.defaultMaxTokens(dec),
		})
		if err != nil {
			return upstreamRequest{}, encodeError(err)
		}
		return jsonUpstream(body, up.endpoint(pathMessages)), nil

	default:
		// The openai-responses KIND is served on the chat route deliberately.
		// internal/wire/openai has a Responses request/response encoder but no
		// Responses STREAM decoder — that surface's events are a typed sequence
		// with nothing in common with a chat chunk — so sending this kind to
		// /v1/responses would trade a working streaming deployment for a
		// non-streaming one. DESIGN §4.3 records that the hosts declaring the
		// kind serve BOTH routes, so the chat route is a correct address for
		// them. dorang's own /v1/responses FRONTEND is unaffected: it decodes to
		// the neutral form and reaches whatever the deployment speaks.
		if c.kind == callCompletions {
			body, err := openai.MarshalCompletionRequest(&req, &openai.EncodeOptions{Model: dec.UpstreamModel})
			if err != nil {
				return upstreamRequest{}, encodeError(err)
			}
			return jsonUpstream(body, up.endpoint(pathCompletions)), nil
		}
		body, err := openai.MarshalRequest(&req, &openai.EncodeOptions{Model: dec.UpstreamModel})
		if err != nil {
			return upstreamRequest{}, encodeError(err)
		}
		return jsonUpstream(body, up.endpoint(pathChatCompletions)), nil
	}
}

// convertResponse turns the upstream answer into the client's protocol.
//
// A same-family exchange still goes through the neutral form rather than being
// copied: §7.2 requires the body to carry the name the client asked for, and the
// name is not the only place the two differ once a deployment's upstream model
// id is not the client's.
func (d *dispatcher) convertResponse(c *call, dec *router.Decision, up *upstream,
	body []byte, header http.Header) ([]byte, string, canonical.Usage, error) {

	switch c.kind {
	case callEmbeddings:
		out, err := replaceModel(body, c.model)
		if err != nil {
			return nil, "", canonical.Usage{}, server.NewError(http.StatusBadGateway,
				server.TypeAPIError, "the upstream answer was not a JSON object").
				WithCode("upstream_shape")
		}
		u, _ := scanEmbeddingUsage(body)
		return out, jsonContentType, u, nil

	case callCountTokens:
		// The count is the whole answer; there is nothing to convert and no
		// usage to price beyond the request itself.
		return body, jsonContentType, canonical.Usage{}, nil

	case callRerank, callModerations, callSpeech, callTranscription, callImages:
		return d.convertT1Response(c, up, body, header)
	}

	var (
		cresp *canonical.Response
		err   error
	)
	switch up.api {
	case catalog.APIAnthropicMessages:
		cresp, err = anthropic.DecodeResponse(body, &anthropic.DecodeOptions{Model: c.model})
	default:
		if c.kind == callCompletions {
			cresp, err = openai.DecodeCompletionResponse(body, &openai.DecodeOptions{Model: c.model})
		} else {
			cresp, err = openai.DecodeResponse(body, &openai.DecodeOptions{Model: c.model})
		}
	}
	if err != nil {
		return nil, "", canonical.Usage{}, server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"could not read the upstream response: "+err.Error()).WithCode("upstream_decode")
	}
	var usage canonical.Usage
	if cresp.Usage != nil {
		usage = *cresp.Usage
	}

	var out []byte
	switch {
	case c.kind == callCompletions:
		out, err = openai.MarshalCompletionResponse(cresp, &openai.ResponseOptions{Model: c.model})
	case c.kind == callResponses:
		if c.responseID != "" {
			cresp.ID = c.responseID
		}
		out, err = openai.MarshalResponsesResponse(cresp, c.respEcho)
	case c.clientAPI == catalog.APIAnthropicMessages:
		out, err = anthropic.MarshalResponse(cresp, &anthropic.ResponseOptions{Model: c.model})
	default:
		out, err = openai.MarshalResponse(cresp, &openai.ResponseOptions{Model: c.model})
	}
	if err != nil {
		return nil, "", usage, server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"could not render the response: "+err.Error()).WithCode("response_encode")
	}
	return out, jsonContentType, usage, nil
}

// jsonContentType is the answer of every route whose body is JSON, which is all
// of them but speech and a subtitle-formatted transcript.
const jsonContentType = "application/json"

// relayStream forwards an event stream to the client.
//
// OpenAI to OpenAI is the one case that is not decoded: internal/wire/openai's
// Scanner rewrites the model field in place and reads the terminal usage frame
// without parsing a single chunk, which is what keeps the per-token path free of
// a JSON round trip. Every other combination goes through the neutral event
// stream, because the two families disagree about block boundaries and there is
// nothing to relay verbatim.
func (d *dispatcher) relayStream(c *call, dec *router.Decision, up *upstream,
	resp *http.Response, w http.ResponseWriter) (canonical.Usage, error) {

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	if c.clientAPI == catalog.APIOpenAIChat && up.api == catalog.APIOpenAIChat {
		sc := openai.NewScanner(&flushWriter{w: w}, openai.ScannerOptions{
			From:         dec.UpstreamModel,
			To:           c.model,
			CollectUsage: true,
		})
		if _, err := io.Copy(sc, resp.Body); err != nil {
			return canonical.Usage{}, err
		}
		if err := sc.Flush(); err != nil {
			return canonical.Usage{}, err
		}
		u, _ := sc.Usage()
		return u, nil
	}

	events, err := newEventSource(up.api, resp.Body)
	if err != nil {
		return canonical.Usage{}, err
	}
	sink, err := newEventSink(c, w)
	if err != nil {
		return canonical.Usage{}, err
	}
	for {
		batch, err := events.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sink.usage(), err
		}
		for _, ev := range batch {
			if err := sink.write(ev); err != nil {
				return sink.usage(), err
			}
		}
	}
	if err := sink.close(); err != nil {
		return sink.usage(), err
	}
	return sink.usage(), nil
}

// settle prices a completed request, feeds the quota meters and fills the parts
// of the result the meter reads.
func (d *dispatcher) settle(st *dispatchState, c *call, dec *router.Decision,
	rq *server.Request, res result) {

	rq.Result.Tokens = server.Usage{
		Input:      int64(res.usage.InputTokens),
		Output:     int64(res.usage.OutputTokens),
		CacheRead:  int64(res.usage.CacheReadTokens),
		CacheWrite: int64(res.usage.CacheWriteTokens),
		Reasoning:  int64(res.usage.ReasoningTokens),
		Total:      int64(res.usage.TotalTokens()),
	}
	rq.Result.TTFTNS = res.ttft.Nanoseconds()
	rq.Result.LatencyNS = res.total.Nanoseconds()

	// The tokens-per-minute ceiling is fed here, because this is where the
	// token count first exists. It bounds the NEXT request from this subject
	// rather than this one, which is the only thing a post-hoc counter can do
	// and is what a tpm limit means everywhere else it is published.
	if p, ok := rq.Principal.(*principal); ok {
		p.recordTokens(rq.Result.Tokens.Total)
	}

	if st.pricing == nil {
		return
	}
	now := d.now()
	cost, err := st.pricing.Settle(pricing.Request{
		Provider:         dec.Provider,
		Model:            dec.UpstreamModel,
		Credential:       dec.Credential,
		Deployment:       dec.Deployment,
		InputTokens:      int64(res.usage.InputTokens),
		OutputTokens:     int64(res.usage.OutputTokens),
		CacheReadTokens:  int64(res.usage.CacheReadTokens),
		CacheWriteTokens: int64(res.usage.CacheWriteTokens),
		ReasoningTokens:  int64(res.usage.ReasoningTokens),
		Requests:         1,
		Seconds:          res.total.Seconds(),
		At:               now,
	})
	if err != nil {
		d.logf("app: pricing %s/%s: %v", dec.Provider, dec.UpstreamModel, err)
		return
	}
	if cost.Missing {
		// §8.3: an unpriced model warns rather than costing zero in silence.
		d.logf("app: no marginal price rule matched %s/%s; the request is unpriced",
			dec.Provider, dec.UpstreamModel)
	} else {
		rq.Result.CostNanoUSD = cost.TotalNano
		rq.Result.Priced = true
	}
	if !cost.NotionalMissing {
		rq.Result.NotionalNanoUSD = cost.NotionalNano
		// §8.5 rule 5: missing is reported, never zero. Without this flag the
		// meter cannot tell a subscription with no notional_rate rule from one
		// whose list-rate equivalent genuinely came to nothing, and the first
		// of those makes the plan look infinitely efficient.
		rq.Result.NotionalPriced = true
	}

	st.quota.record(dec.Credential, now, quota.Usage{
		CostNanoUSD:  cost.TotalNano,
		TokensInput:  int64(res.usage.InputTokens),
		TokensOutput: int64(res.usage.OutputTokens),
		Requests:     1,
	})
}

// fillRouteResult stamps the routing decision onto the request before the first
// byte goes out.
func fillRouteResult(rq *server.Request, dec *router.Decision) {
	// The deployment still on the Result is the one the previous attempt used,
	// which is the only moment it is knowable — one line later it is gone.
	prev := rq.Result.Deployment
	rq.Result.Provider = dec.Provider
	rq.Result.Credential = dec.Credential
	rq.Result.Deployment = dec.Deployment
	rq.Result.UpstreamModel = dec.UpstreamModel
	rq.Result.Attempt = dec.Attempt
	rq.Result.RouteReason = dec.Reason
	if dec.Attempt > 1 {
		rq.Result.FallbackFrom = dec.Reason
		rq.Result.FallbackFromDeployment = prev
	}
	if names := dec.Dropped.Params(); len(names) > 0 {
		rq.Result.DroppedParams = strings.Join(names, ",")
	}
}

// routeError renders a routing refusal as an HTTP answer. router.Error already
// carries the status, the code and a message written for a human, so nothing is
// paraphrased here.
func routeError(err error) error {
	var re *router.Error
	if errors.As(err, &re) {
		status := re.Status
		if status == 0 {
			status = http.StatusServiceUnavailable
		}
		e := server.NewError(status, server.TypeForStatus(status), re.Message)
		if re.Code != "" {
			e = e.WithCode(re.Code)
		}
		if !re.ResetAt.IsZero() {
			e.RetryAfterSeconds = int(time.Until(re.ResetAt).Seconds()) + 1
		}
		return e
	}
	return err
}

func encodeError(err error) error {
	return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
		"the request cannot be expressed by the selected deployment: "+err.Error()).
		WithCode("conversion_failed")
}

// defaultMaxTokens is the output ceiling handed to the Anthropic encoder when
// the caller named none. The field is required on that surface, and a constant
// would silently cap output at a number the caller never chose, so the
// deployment's catalogued ceiling is used and nothing is invented (§10.7).
// A model the catalog does not declare a ceiling for yields zero, which the
// encoder turns into an explicit error rather than a guess.
func (d *dispatcher) defaultMaxTokens(dec *router.Decision) int {
	st := d.state()
	if st == nil || st.catalog == nil {
		return 0
	}
	return st.catalog.Model(dec.Kind, dec.UpstreamModel).MaxOutputTokens
}

// MasterOwnerID is the owner recorded for an object the master credential
// created.
//
// The master credential has no api_keys row by construction (DESIGN §2.4), so
// its KeyID() is "". Writing that into a record's OwnerKeyID produced an
// UNOWNED object, and batch.ownedBy used to read an unowned object as public —
// every `sk-` key in the deployment could read, use and delete a file the
// operator had uploaded. The identity has to be a real string for the ownership
// comparison to mean anything.
//
// The "master:" prefix cannot collide with a key id: key ids are generated
// identifiers and this one contains a colon, which none of them does.
const MasterOwnerID = "master:credential"

// principalID names the owner of an object this request creates.
//
// A request with no principal returns "" and that is deliberate: it is an
// internal or unauthenticated path, it creates nothing, and an object it did
// create would be refused a reader rather than handed to all of them.
func principalID(rq *server.Request) string {
	if rq.Principal == nil {
		return ""
	}
	if m, ok := rq.Principal.(interface{ IsMaster() bool }); ok && m.IsMaster() {
		return MasterOwnerID
	}
	return rq.Principal.KeyID()
}

func maxOutputTokens(c *canonical.Request) int64 {
	if c == nil || c.MaxTokens == nil {
		return 0
	}
	return int64(*c.MaxTokens)
}

// estimateInputTokens is the pessimistic prompt-size estimate of §10.5a.
//
// It errs high on purpose: an over-estimate costs an unnecessary route to a
// larger model, an under-estimate costs a hard failure the router cannot see.
// Three bytes per token is below every tokenizer's real ratio for prose and is
// arithmetic rather than a tokenizer on the hot path.
func estimateInputTokens(body []byte) int64 { return int64((len(body) + 2) / 3) }

// parseAllowLossy reads the x-dorang-allow-lossy opt-in.
func parseAllowLossy(v string) canonical.Capability {
	if v == "" {
		return 0
	}
	var out canonical.Capability
	for _, name := range strings.Split(v, ",") {
		if c, ok := canonical.ParseCapability(strings.TrimSpace(name)); ok {
			out |= c
		}
	}
	return out
}

func cloneExtra(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// retryAfter reads a provider-signalled cooldown.
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// replaceModel swaps the top-level "model" of a JSON object.
//
// It decodes into a raw-message map, so every other field survives byte for
// byte and the model name is copied whole: nothing here splits it (§2.1).
func replaceModel(body []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("body is not a JSON object")
	}
	enc, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = enc
	return json.Marshal(obj)
}

// scanEmbeddingUsage reads the usage object of an embeddings answer, which is
// the only part of that body dorang looks inside.
func scanEmbeddingUsage(body []byte) (canonical.Usage, bool) {
	var shape struct {
		Usage *struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &shape); err != nil || shape.Usage == nil {
		return canonical.Usage{}, false
	}
	return canonical.Usage{InputTokens: shape.Usage.PromptTokens}, true
}

// flushWriter pushes every relayed chunk to the client immediately. Without it
// a streaming response is buffered and arrives as one block, which is not a
// stream.
type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
