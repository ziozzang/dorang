package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/backend"
	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/loadsignal"
	"github.com/ziozzang/dorang/internal/luaext"
	"github.com/ziozzang/dorang/internal/mask"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/quota"
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

// dispatcher owns everything past the gate: decoding, routing, capacity,
// budgets, and writing the response body. It satisfies server.Dispatcher.
//
// It does NOT own the upstream call. That is internal/backend's, and the
// division is DESIGN §1's L4/L5 line: this type turns a configuration into
// providers and a decision into a target, and never builds an HTTP request,
// spells a credential, or parses a provider's error envelope.
//
// It holds its subsystems behind an atomic pointer because a hot reload rebuilds
// the router, the price catalog and the upstream table, and an in-flight request
// must keep the snapshot it started with (DESIGN §4.1, §15.2).
type dispatcher struct {
	st atomic.Pointer[dispatchState]
	// backend is L5 (DESIGN §1): everything between "the router chose a
	// deployment" and "the client's protocol has an answer". It is built once
	// and is immutable — a hot reload rebuilds providers, which live on the
	// state, not this.
	backend *backend.Backend
	// oauth is the §11.2b credential set. It lives on the dispatcher rather than
	// on the state because a credential holds a token, a backoff and a
	// background loop that outlive a reload — the same reason the broker and the
	// authenticator are not rebuilt either. Nil when nothing authenticates by
	// OAuth, which makes the lookup below one nil check.
	oauth *auth.OAuthManager
	logf  func(string, ...any)
	now   func() time.Time
	// filter are the §10.5b transform-filter counters. They live on the
	// dispatcher rather than the state because a configuration reload must not
	// reset them: a counter that restarts on SIGHUP is a counter nobody can
	// alert on.
	filter filterCounters
	// subs remembers which (deployment, served model) pairs have already been
	// logged, for the same reason and with the same lifetime.
	subs substitutionLog
}

// substitutionLog keeps a model substitution from becoming a log flood.
//
// The measured case substitutes on EVERY request — z.ai's `glm-5.1` answered as
// `glm-5.2` three runs out of three — so a line per occurrence would be a line
// per request for the life of the deployment, which is how a real signal gets
// filtered out of an operator's log pipeline and then out of their attention.
// The COUNT is the metric's job. The line's job is to say, once, what is
// happening and to which deployment.
//
// The set is capped rather than unbounded. Its key is (deployment, served
// model) and both halves are bounded by configuration — the deployment list and
// the catalog — but a cap costs nothing and the failure mode of not having one
// is a map that grows with whatever an upstream chooses to say.
type substitutionLog struct {
	mu   sync.Mutex
	seen map[[2]string]struct{}
}

// maxLoggedSubstitutions bounds [substitutionLog]. Past it the metric still
// counts every occurrence; only the once-per-pair line stops.
const maxLoggedSubstitutions = 256

// first reports whether this pair has not been logged before.
func (s *substitutionLog) first(deployment, served string) bool {
	k := [2]string{deployment, served}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[k]; ok {
		return false
	}
	if len(s.seen) >= maxLoggedSubstitutions {
		return false
	}
	if s.seen == nil {
		s.seen = make(map[[2]string]struct{}, 8)
	}
	s.seen[k] = struct{}{}
	return true
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

	// filters are the §10.5b transform filters, resolved by client-facing model
	// name. Nil, or a model with no entry, costs one map lookup that is skipped
	// entirely when nothing is configured.
	filters *filterTable

	prefixOn bool
	chunk    int

	// usageChunkChoices and anthropicTotalTokens are COMPATIBILITY §3.3 and
	// §6.8, resolved from `compat:` once per load and carried onto every
	// backend.Call. They live here rather than on the dispatcher because they
	// are hot-reloadable like everything else in this struct, and they are
	// resolved to the WIRE package's types here rather than in the backend so
	// that the mapping from a configured spelling to a wire constant happens
	// once, in the layer that reads configuration.
	usageChunkChoices    openai.UsageChunkChoices
	anthropicTotalTokens anthropic.TotalTokensMode
}

// The non-streaming upstream body is bounded in internal/backend, which is the
// layer that now makes the call: backend.Options.MaxResponseBytes, its
// DefaultMaxResponseBytes and the non-retryable upstream_response_too_large it
// answers with. This package held a second copy of that read while it had its
// own HTTP client; the client moved down a layer and the copy went with it,
// rather than staying behind as the version nothing calls.

func newDispatcher(client *http.Client, oauth *auth.OAuthManager,
	logf func(string, ...any), now func() time.Time) *dispatcher {

	d := &dispatcher{oauth: oauth, logf: logf, now: now}
	d.backend = backend.New(backend.Options{Client: client, Credentials: d, Now: now})
	return d
}

func (d *dispatcher) swap(st *dispatchState) { d.st.Store(st) }
func (d *dispatcher) state() *dispatchState  { return d.st.Load() }

// Credential implements backend.Credentials.
//
// It resolves through the CURRENT state rather than through a table captured at
// construction, because credentials hot-reload and the backend does not: a
// rotated key has to reach the next request without rebuilding L5 under the
// in-flight ones.
//
// An OAuth credential is resolved here and nowhere else. That comment used to
// read "OAuth returns nil today… wiring it is a lookup here and nothing in L5",
// and it was right on both counts: this is the lookup, and L5 did not change.
// internal/backend still declares [backend.Applier], still calls Apply on
// whatever it is handed, and still has no idea what a token is or where one
// comes from — which is what the seam was for.
//
// The OAuth branch comes first because the two are alternatives, not a fallback
// chain: a credential that authenticates by OAuth has no static secret, and
// internal/config refuses one that claims both. Returning an empty secret
// alongside the applier says the same thing to [backend.Provider.ApplyCredential],
// which spells the credential from the applier and ignores the secret entirely.
func (d *dispatcher) Credential(id string) (string, backend.Applier) {
	if d.oauth != nil {
		if c, ok := d.oauth.Credential(id); ok {
			return "", c
		}
	}
	st := d.state()
	if st == nil || st.upstreams == nil {
		return "", nil
	}
	return st.upstreams.secret(id), nil
}

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
	// prevResponseID is the caller's `previous_response_id`, kept for the
	// stored row after it has been resolved and REMOVED from the upstream
	// request: it names a dorang exchange, and an upstream that resolves
	// references would look it up and find nothing.
	prevResponseID string

	// mask is this request's reversible mask (§10.5b). It is nil unless a
	// transform filter with a pattern set is configured for the model, it lives
	// exactly as long as this call, and it is never written anywhere: the ledger,
	// the trace, the log and the metric labels all take counts from
	// [call.filterStats] and have no path to the table.
	mask *mask.Session
	// noAffinity suppresses the prefix claim for this request.
	//
	// It is set by a filter whose scope is per-request, whose placeholders are
	// therefore different on every turn. The upstream bytes cannot repeat, so a
	// prefix claim would be a claim about bytes no backend holds. Saying so is
	// the point: a scope that quietly made another subsystem useless would be
	// the defect this codebase keeps finding.
	noAffinity bool
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
	c, err := d.decode(ctx, st, rq)
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
		// The client's remaining allowance is known HERE, at selection, and does
		// not depend on whether the upstream then answered. It is filled before
		// anything is attempted so that a 502, a 429 or a refused hop still
		// carries it — a caller being told how much of its quota is left matters
		// most when things are failing, and fillRouteResult runs only from the
		// Accepted callback, which a connection failure never reaches.
		fillQuotaResult(rq, dec)

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
			// The mask's counts are folded in here, after every unmasking pass
			// has run: counts only, and the session itself goes out of scope
			// with the call (§10.5b rule 1).
			d.observeFilter(c)
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
func (d *dispatcher) decode(ctx context.Context, st *dispatchState, rq *server.Request) (*call, error) {
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
		MaxOutputTokens: maxOutputTokens(c.creq),
		Stream:          rq.Stream,
	}
	// The caller's own concurrency ceiling, their priority class and their
	// priority hint, all three of which were loaded and dropped. This is the one
	// place they are read; PrincipalMax is deliberately not also set in the
	// literal above, because two writers of one field is how the value that
	// loses stops being noticed.
	applyPrincipalPolicy(rq, &c.rreq)
	if c.creq != nil {
		c.rreq.Required = c.creq.RequiredCapabilities()
	}
	// §10.5b's transform filters run here: after decoding, before routing, and
	// before either of the two things below that describe what the upstream will
	// be sent. A filter that ran later would be metered as though it had not run
	// and would hash bytes the backend never receives.
	if err := d.filterRequest(ctx, st, rq, c); err != nil {
		d.filter.refused.Add(1)
		return nil, err
	}

	// The prompt-size estimate is taken here, after the filter, for the reason
	// §10.5b rule 5 gives: what a transform masked is what the upstream is sent,
	// so it is also what the budget hold and the context-window filter have to be
	// about. A filter rewrites the canonical request in place, so estimating from
	// it afterwards gets the masked size with no second pass over the bytes.
	//
	// It is structural, not a division of the body length. A base64 image is body
	// bytes, and the length rule charged a 2 MB photograph ~900,000 tokens against
	// a real cost near 1,600 — large enough that no window admits it, so ordinary
	// multimodal traffic was refused everywhere rather than routed anywhere.
	est := c.estimate()
	c.rreq.InputTokens = est.Tokens
	c.rreq.InputTokensExact, c.rreq.InputTokensMethod = est.Exact, est.Method

	// image is the bytes the prefix claim is *about*. Without a filter it is the
	// client's body, as before. With one it is the post-filter canonical
	// request, because the backend keys its cache on what it received, and a
	// digest over pre-filter bytes would claim a prefix the backend never saw.
	//
	// The invariant, so the next person does not move this back above the
	// filter: same client prefix + same filter output ⇒ same digest. Both halves
	// are needed. The filter's own identity — its pattern set and its derivation
	// secret — is folded in for free, because changing either changes the masked
	// bytes, so an edited pattern set *misses* the cache instead of corrupting a
	// claim.
	image := body
	if c.mask != nil {
		image = prefixImage(c, body)
	}
	if st.prefixOn && st.chunk > 0 && !c.noAffinity {
		// The chain is seeded with the TENANT and then the client-facing model
		// group, and cut at byte boundaries — there is no tokenizer on this path
		// (§7.4b).
		//
		// The tenant is in the seed because a prompt cache belongs to an account
		// (§7.4a2). Without it two tenants sending identical bodies produce
		// identical digests, so the affinity table pins tenant B to the
		// deployment tenant A warmed — for a hit that cannot happen when they
		// authenticate with different credentials, and, worse, for one B can
		// *observe*: cache_read_input_tokens, or simply TTFT, answers "has
		// somebody else recently sent exactly this?". prefix.NewChain takes the
		// tenant as its own parameter, separated by a NUL, so the seed cannot be
		// built without one and two components cannot run together.
		c.rreq.Digests = prefix.Compute(tenant, rq.Model, image, st.chunk)
	}
	return c, nil
}

// prefixImage renders the post-filter request for hashing.
//
// It marshals the canonical request rather than re-encoding the upstream body,
// because the upstream encoding is chosen by routing and the digest is needed
// before routing. It is deterministic — struct field order is fixed and
// encoding/json sorts map keys — which is the only property the chain needs.
func prefixImage(c *call, body []byte) []byte {
	if c.creq == nil {
		return body
	}
	b, err := json.Marshal(c.creq)
	if err != nil {
		return body
	}
	return b
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

// result is one attempt's outcome, in the two shapes its two consumers need:
// a router.Outcome for Report, and an error for the client.
type result struct {
	outcome   router.Outcome
	err       error
	retryable bool
	usage     canonical.Usage
	// usageExtra is what the upstream reported that no counter names. Pricing
	// charges part of it: a server-side web search is billed per search and a
	// generated image's tokens at a rate unrelated to chat tokens.
	usageExtra *canonical.UsageExtra
	ttft       time.Duration
	total      time.Duration
	// body is the converted non-streaming answer, held rather than written so
	// that pricing lands on the request before the headers are stamped.
	body []byte
	// loadMetrics is the engine's own occupancy report from this response, if it
	// sent one (VLLM.md §3.2). Empty means no report — never an idle backend.
	loadMetrics string
	// contentType labels body. It is not always application/json: speech
	// answers audio and a transcription asked for in srt or vtt answers text,
	// and mislabelling either gives the client something it cannot play or
	// parse.
	contentType string
	// servedModel is the name the upstream put in its own body when that name
	// disagreed with the `upstream_model` dorang sent, and modelAgreement is the
	// verdict. Empty and [canonical.ModelUnobserved] are the ordinary case.
	servedModel    string
	modelAgreement canonical.ModelAgreement
}

// attempt makes one upstream call and relays its answer.
//
// Everything past this point is internal/backend's: the endpoint, the
// credential, the request bytes, the retry policy, the error taxonomy and the
// streaming relay. What stays here is what L5 deliberately does not take — the
// routing decision it is handed, and the router's view of how the attempt went.
func (d *dispatcher) attempt(ctx context.Context, st *dispatchState, c *call,
	dec *router.Decision, rq *server.Request, w http.ResponseWriter) result {

	var res result

	up, ok := st.upstreams.provider(dec.Provider)
	if !ok {
		res.err = server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"no upstream is configured for provider "+dec.Provider).WithCode("no_upstream")
		res.outcome = router.Outcome{Err: res.err, Cause: router.CauseUpstream5xx}
		return res
	}
	// The crossing check stays in front of the backend rather than inside it,
	// because it is a question about the CALLER's route and the deployment's
	// family together — "a rerank request on a messages deployment" — and L5 is
	// only ever told which operation to perform, not which frontend asked.
	if err := checkFamily(c, up.API()); err != nil {
		res.err = err
		res.outcome = router.Outcome{Err: err}
		return res
	}

	target := backend.Target{
		Provider:      up,
		Credential:    dec.Credential,
		UpstreamModel: dec.UpstreamModel,
		// Arms the substitution reading (§17.1 rule 3): the catalog has figures
		// for this model, so a different model answering is something the
		// operator can be shown rather than a name dorang cannot place.
		ModelKnown: dec.ModelKnown,
		// Direction-normalized for THIS engine by internal/router (§7.5).
		// Nothing on this side negates, offsets or clamps it: two
		// implementations of one negation is how vLLM and SGLang end up agreeing
		// about a number they must disagree about.
		PriorityField: dec.PriorityField,
		Priority:      dec.Priority,
		PriorityTier:  dec.PriorityTier,
		// The set this candidate was FILTERED on, so it is also the set the
		// request is ENCODED against (§10.1). Left unset, internal/backend
		// computed its own from the wire shape — a second answer to a question
		// this package had already answered when it built the routing table, and
		// the day the two stop agreeing routing admits what encoding then drops.
		Capabilities: dec.Capabilities,
		// COMPATIBILITY §5.5's output-ceiling spelling, resolved per deployment
		// at assembly and carried on the decision — so a fail-back hop encodes
		// with the spelling of the deployment it landed on rather than the one
		// it left.
		MaxTokensField: dec.MaxTokensField,
	}
	return backendResult(d.backend.Do(ctx, target, d.backendCall(st, c, dec, rq), w))
}

// backendCall renders one hop's request in the terms L5 takes.
//
// It is rebuilt per hop rather than once per request because two of its fields
// are properties of the DEPLOYMENT — the catalogued output ceiling, and the
// callback that stamps the routing decision — and a hop that reused the previous
// deployment's would describe an attempt that did not happen.
func (d *dispatcher) backendCall(st *dispatchState, c *call, dec *router.Decision,
	rq *server.Request) *backend.Call {

	return &backend.Call{
		Op:            c.operation(),
		ClientAPI:     c.clientAPI,
		Model:         c.model,
		Body:          c.body,
		Request:       c.creq,
		Rerank:        c.rerankReq,
		Moderation:    c.modReq,
		Speech:        c.speechReq,
		Transcription: c.transReq,
		Image:         c.imageReq,
		ResponseID:    c.responseID,
		ResponseEcho:  c.respEcho,
		Stream:        c.stream,
		AllowLossy:    c.rreq.AllowLossy,
		// The deployment's catalogued ceiling, for the wire shapes that make the
		// field mandatory. Nothing is invented: a model the catalog declares no
		// ceiling for yields zero, which the encoder turns into an explicit error
		// rather than a guess (§10.7).
		DefaultMaxTokens: d.defaultMaxTokens(dec.Kind, dec.UpstreamModel),
		IncludeUsage:     c.allowUsg,
		// COMPATIBILITY §3.3 and §6.8, read off the SAME state snapshot the rest
		// of this attempt used rather than re-loaded here. A reload that changed
		// `compat:` must not leave a request in flight rendering one shape on
		// its second attempt and the other on its third.
		UsageChunkChoices:    st.usageChunkChoices,
		AnthropicTotalTokens: st.anthropicTotalTokens,
		Transform:            c.transform(),
		// The extension headers are stamped on the first write and anything set
		// afterwards is invisible (DESIGN §10.4). For a stream the first write
		// happens inside the backend, so this is the last moment the routing
		// decision — and the encoder's own loss report, which is why the callback
		// carries one — can still reach the client.
		Accepted: func(loss *canonical.LossReport) { fillRouteResult(rq, dec, c, loss) },
	}
}

// operation is which backend operation this call asks for.
//
// It is a mapping and not a shared type on purpose: a callKind is a FRONTEND
// route and an Operation is what the backend must be asked for, and the two
// coincide everywhere except audio and images, where one client kind covers
// several upstream routes.
func (c *call) operation() backend.Operation {
	switch c.kind {
	case callCountTokens:
		return backend.OpCountTokens
	case callEmbeddings:
		return backend.OpEmbeddings
	case callCompletions:
		return backend.OpCompletions
	case callResponses:
		return backend.OpResponses
	case callModerations:
		return backend.OpModerations
	case callRerank:
		return backend.OpRerank
	case callSpeech:
		return backend.OpSpeech
	case callTranscription:
		if c.transReq != nil && c.transReq.Translate {
			return backend.OpTranslation
		}
		return backend.OpTranscription
	case callImages:
		if c.imageReq != nil {
			switch c.imageReq.Op {
			case canonical.ImageEdit:
				return backend.OpImageEdit
			case canonical.ImageVariation:
				return backend.OpImageVariation
			}
		}
		return backend.OpImageGenerate
	}
	return backend.OpChat
}

// backendResult renders one exchange in the two shapes this package's consumers
// need: a [router.Outcome] for Report, and an error for the client.
func backendResult(br backend.Result) result {
	res := result{
		usage:       br.Usage,
		usageExtra:  br.UsageExtra,
		ttft:        br.TTFT,
		total:       br.Total,
		body:        br.Body,
		contentType: br.ContentType,
		loadMetrics: br.LoadMetrics,

		servedModel:    br.ServedModel,
		modelAgreement: br.ModelAgreement,
	}
	if br.Err == nil {
		res.outcome = router.Outcome{
			TTFT: br.TTFT, Total: br.Total, FirstByteSent: br.FirstByteSent,
			InputTokens:  int64(br.Usage.InputTokens),
			OutputTokens: int64(br.Usage.OutputTokens),
		}
		return res
	}
	cause := backendCause(br)
	res.err = br.Err
	// Retryable here means "another DEPLOYMENT may be tried", never "call this
	// provider again": the in-provider retry was already made and spent. A
	// failure the fallback chain has no target for is terminal even when the
	// backend was willing to hand it on.
	res.retryable = br.Retryable && (br.Status < 400 || cause.Chainable())
	res.outcome = router.Outcome{
		Err: br.Err, Status: br.Status, Cause: cause,
		TTFT: br.TTFT, Total: br.Total,
		FirstByteSent: br.FirstByteSent, RetryAfter: br.RetryAfter,
	}
	return res
}

// backendCause classifies one failed exchange into a fail-back class.
func backendCause(br backend.Result) router.Cause {
	switch {
	case br.Status >= 400:
		// The status line alone cannot tell a context overflow from any other
		// 400, which is why [router.Outcome.Cause] documents itself as the
		// frontend's job for exactly this condition: the signature is in the
		// body, and the backend's normalization has already decoded it out of
		// whichever of the five upstream envelope shapes arrived. Consulting it
		// is what makes §10.5a's "route to a larger window" reachable from an
		// upstream signal at all — and it is the ONLY overflow path a deployment
		// that declares no window has, which is the ordinary self-hosted case.
		return upstreamCause(br.Status, br.Err)
	case br.Timeout:
		return router.CauseTimeout
	case br.Err.Code == backend.CodeCredentialUnavailable:
		// A credential dorang holds but cannot use. It is not this deployment's
		// fault and not a transport failure; another deployment authenticating
		// with a different credential may well serve the request.
		return router.CauseAuth
	case br.Transport, br.Status > 0:
		// No response at all, or one whose body could not be read, converted or
		// relayed. Both are the upstream's failure to answer.
		return router.CauseUpstream5xx
	}
	// Nothing reached the wire: the request could not be addressed, encoded, or
	// built. There is no upstream to blame.
	return router.CauseNone
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

// settle prices a completed request, feeds the quota meters and fills the parts
// of the result the meter reads.
// noteSubstitution finishes the model-identity check of DESIGN §17.1 rule 3.
//
// # Where A and B were observed
//
// A is `models[].deployments[].upstream_model`, read out of dorang's own
// configuration and put on the wire by internal/backend. B is the `model`
// member of the body the provider sent back, read by internal/wire and carried
// out on [backend.Result]. Two genuinely different origins, one of them outside
// dorang's control — so this is a check that can fire, and not documentation.
//
// It fires on the operator's own subscriptions today: the z.ai coding plan
// answers `glm-5.1` and `glm-5` with `glm-5.2`, and `glm-4.5-air` with
// `glm-4.7`, at HTTP 200 with a well-formed body and real tokens. Nothing else
// dorang checks can see that. The status is 200, the body parses, the usage is
// present, and a `/models` listing shows all eight ids as if all eight served.
//
// # Two gates, and both are load-bearing
//
// A substitution is claimed only when BOTH hold:
//
//  1. The names disagree in a way that is not a refinement
//     ([canonical.CompareModel]). Without this, ordinary version pinning fires:
//     the catalog carries `claude-haiku-4-5` AND `claude-haiku-4-5-20251001` as
//     separate anthropic entries with identical figures, and Anthropic answers
//     the first with the second on every request.
//
//  2. BOTH names resolve to catalog entries under this deployment's kind, and
//     to DIFFERENT entries. Without this, an alias the upstream resolved for
//     itself fires: an operator's `local` deployment answered by
//     `…-Q6_K.gguf`, or an OpenRouter id answered as
//     `private/openrouter/…` with its `:free` suffix dropped. Both are in the
//     operator's live configuration, both are correct behaviour, and neither
//     name is a model dorang has ever had figures for.
//
// Gate 2 is also what makes the claim MEAN something. The harm of a
// substitution is that a figure was wrong — the request was priced at the asked
// model's rate for another model's work, and §10.5a's context-window fallback
// used the asked model's declared window — and that harm is only provable when
// dorang holds a second set of figures it should have used instead. Where the
// returned name is not an entry, nothing was mispriced against anything,
// because there was no other figure.
//
// Gate 2's cheap half — is the ASKED name an entry — was decided at compile and
// rides on [router.Decision.ModelKnown], which is what keeps a deployment like
// `local` from paying for this check at all.
//
// # Detection is not policy
//
// Nothing here rewrites the request, reroutes it, refuses it, re-prices it or
// marks the deployment unhealthy. It records. What an operator does about a
// substituting deployment is their decision, and the two things they need to
// make it — that it is happening, and which model actually answered — are what
// the counter and the line carry.
func (d *dispatcher) noteSubstitution(st *dispatchState, dec *router.Decision,
	rq *server.Request, res result) {

	// Empty is the ordinary case by construction: internal/backend copies the
	// name out only when its own comparison found a substitution against a
	// model the catalog knows.
	if res.servedModel == "" || !res.modelAgreement.Substituted() || st.catalog == nil {
		return
	}
	if !st.catalog.Model(dec.Kind, res.servedModel).ModelKnown {
		// A name dorang has no entry for. Not a substitution — a decoration, or
		// an alias the upstream resolved for itself. Silence is correct.
		return
	}
	rq.Result.ServedModel = res.servedModel

	if !d.subs.first(dec.Deployment, res.servedModel) {
		return
	}
	// Ids and model names only. Nothing here is a secret, and nothing here is
	// caller-supplied.
	if dec.PinnedTo != "" {
		// The case where a substitution destroys the whole point of the
		// configuration: a pin promises this deployment and no other, so there
		// is no fallback that could have caught it.
		d.logf("app: deployment %s (pinned by %s) was sent upstream_model %q and answered as "+
			"%q, a different model this catalog also knows; the request was priced and "+
			"windowed as %q. dorang did not reroute or re-price it (DESIGN §17.1)",
			dec.Deployment, dec.PinnedTo, dec.UpstreamModel, res.servedModel, dec.UpstreamModel)
		return
	}
	d.logf("app: deployment %s was sent upstream_model %q and answered as %q, a different "+
		"model this catalog also knows; the request was priced and windowed as %q. dorang "+
		"did not reroute or re-price it (DESIGN §17.1). Further occurrences on this pair "+
		"are counted in dorang_model_substitutions_total and not logged",
		dec.Deployment, dec.UpstreamModel, res.servedModel, dec.UpstreamModel)
}

func (d *dispatcher) settle(st *dispatchState, c *call, dec *router.Decision,
	rq *server.Request, res result) {

	// Before the pricing gate, so an unpriced deployment still tells the
	// caller what the upstream reported: the counts are the answer's, whether
	// or not anyone has put a rate on them.
	searches, imgIn, imgOut := toolQuantities(res.usageExtra)
	rq.Result.Tokens = server.Usage{
		Input:       int64(res.usage.InputTokens),
		Output:      int64(res.usage.OutputTokens),
		CacheRead:   int64(res.usage.CacheReadTokens),
		CacheWrite:  int64(res.usage.CacheWriteTokens),
		Reasoning:   int64(res.usage.ReasoningTokens),
		Total:       int64(res.usage.TotalTokens()),
		WebSearches: searches,
		ImageInput:  imgIn,
		ImageOutput: imgOut,
	}
	rq.Result.TTFTNS = res.ttft.Nanoseconds()
	rq.Result.LatencyNS = res.total.Nanoseconds()

	// Before the pricing gate below, and deliberately: a substitution is a fact
	// about the ANSWER, and it is true whether or not this deployment is priced.
	d.noteSubstitution(st, dec, rq, res)

	// The tokens-per-minute ceiling is fed from App.recordMetrics, at the
	// meter, because that is the one point every finished request passes and it
	// carries all three subject ids. Feeding it a second time here would count
	// every interactive request's tokens twice.

	if st.pricing == nil {
		return
	}
	now := d.now()
	util, refusal := observeUtilization(c, dec, res, now)
	cost, err := st.pricing.Settle(pricing.Request{
		Provider:          dec.Provider,
		Model:             dec.UpstreamModel,
		Credential:        dec.Credential,
		Deployment:        dec.Deployment,
		InputTokens:       int64(res.usage.InputTokens),
		OutputTokens:      int64(res.usage.OutputTokens),
		CacheReadTokens:   int64(res.usage.CacheReadTokens),
		CacheWriteTokens:  int64(res.usage.CacheWriteTokens),
		WebSearches:       searches,
		ImageInputTokens:  imgIn,
		ImageOutputTokens: imgOut,
		ReasoningTokens:   int64(res.usage.ReasoningTokens),
		Requests:          1,
		// The two second axes, and they are two on purpose (DESIGN §10.7). Seconds
		// is how long THIS REQUEST took, which is what a GPU-second rate is quoted
		// against. AudioSeconds is how much recorded media the vendor billed for,
		// which is what a transcription rate is quoted against — and it was reaching
		// canonical.TranscriptionResponse and stopping there, so a ten-minute
		// recording transcribed in eight seconds was charged as eight seconds.
		// Billed carries the vendor's own `usage.type` so a rule that prices the
		// wrong axis is refused rather than applied to whichever number is present.
		Seconds:      res.total.Seconds(),
		AudioSeconds: res.usage.AudioSeconds,
		Billed:       billedUnit(res.usage.Billed),
		// How contended the backend was while this request ran, when it said
		// (DESIGN §8.6). Zero with the flag clear is the ABSENCE of a reading and
		// prices at the base rate; it is never read as an idle machine.
		UtilizationPPM:      util,
		UtilizationObserved: refusal == loadsignal.RefusalNone,
		At:                  now,
	})
	if err != nil {
		d.logf("app: pricing %s/%s: %v", dec.Provider, dec.UpstreamModel, err)
		return
	}
	if cost.Floored {
		// The catalog's adjustments came to more than the request they applied
		// to. pricing floors the total at zero, because the ledger row and the
		// quota counter below both read it as an amount SPENT and a negative one
		// would hand back budget and quota nobody paid for. A clamp that nobody
		// is told about is a catalog error that never gets fixed, so it is
		// logged for the same reason an unpriced model is.
		d.logf("app: adjustment rules on %s/%s exceed the request's cost; the total "+
			"was clamped to zero (a credit may zero a request out, not pay the caller)",
			dec.Provider, dec.UpstreamModel)
	}
	// charged is the ONE figure this request costs, and every counter below
	// takes it from here.
	//
	// It used to be read twice: the ledger row read it inside the switch and the
	// quota counter read cost.TotalNano from a line outside it. Two arms of that
	// switch record nothing, and the line outside them charged the plan share
	// anyway — quota +32.26 USD against a ledger row of 0.00, on one request
	// that produced no billable line, with the plan accumulator advanced past it
	// for good. A single variable is the fix, and it is the shape rather than the
	// value: there is no longer a path through this function on which the two can
	// differ, because there is no second read.
	var charged int64
	switch {
	case cost.Missing:
		// §8.3: an unpriced model warns rather than costing zero in silence.
		d.logf("app: no marginal price rule matched %s/%s; the request has no marginal price",
			dec.Provider, dec.UpstreamModel)
		// It is not necessarily free, and this is the arm where that matters
		// most. A flat plan is a catalog with NO marginal_usage rule at all
		// (§8.1), so every one of its requests arrives here, and the plan
		// share pricing attributed is the whole of what this row costs.
		// Recording nothing would have left `subscription_spend` without a
		// producer for exactly the plans it exists for, while the period's
		// accumulator advanced on every request — the plan cost apportioned
		// to rows that all report zero.
		charged = cost.TotalNano
		rq.Result.CostNanoUSD = charged
		rq.Result.MarginalNanoUSD = cost.MarginalNano
		rq.Result.SubscriptionNanoUSD = cost.SubscriptionNano
		rq.Result.Priced = charged != 0 || cost.SubscriptionNano != 0
	case cost.NoPrice != pricing.NoPriceNone:
		// A rule matched and could not be applied, which is a DIFFERENT catalog
		// error from having no rule and has a different fix: the catalog says the
		// wrong thing about this model rather than nothing. It is reported at the
		// same volume, because the two are indistinguishable in the ledger — both
		// record the request unpriced — and because what the flag exists to
		// forbid is charging one billing unit's rate against another unit's
		// number, which produces a plausible figure and no error at all.
		d.logf("app: price rule %s matched %s/%s and could not price it on %s: %s; "+
			"the request is unpriced and charges nothing — no marginal cost, no plan "+
			"share, no quota", cost.NoPriceRule, dec.Provider, dec.UpstreamModel,
			cost.NoPriceQuantity, cost.NoPrice.Why())
		// charged stays zero, and internal/pricing has already made every
		// class of the returned Cost zero as well: an unpriceable request
		// does not advance the plan accumulator, so the share it would have
		// taken is still there for the next row of the period.
	default:
		charged = cost.TotalNano
		rq.Result.CostNanoUSD = charged
		// The decomposition of the number on the line above, by pricing class —
		// and it travels WITH that number rather than beside it, so a row can
		// never report a plan share larger than the total it is part of.
		//
		// This is what gives `subscription_spend` a producer. Before it, the
		// meter carried a single CostNano, internal/app wrote it to BOTH the
		// total and `marginal_cost_nano`, and a flat plan's share was filed
		// under `marginal_spend` on every row: DESIGN §8.1's "the two are
		// separate fields, never conflated", conflated. The column, its reader
		// in /spend/logs and its JSON name all existed; nothing wrote it.
		//
		rq.Result.MarginalNanoUSD = cost.MarginalNano
		rq.Result.SubscriptionNanoUSD = cost.SubscriptionNano
		rq.Result.Priced = true
	}
	if cost.UtilizationPriced {
		// The factor and where it came from, on every priced request whose rule
		// declares one — INCLUDING the fallbacks. A response that carries a
		// multiplier only when the price moved leaves a caller unable to tell
		// "the backend was idle" from "we never looked", which is the one
		// distinction this whole feature turns on.
		rq.Result.UtilizationPriced = true
		rq.Result.UtilizationMultiplierPPM = cost.UtilizationMultiplierPPM
		rq.Result.UtilizationCeilingPPM = cost.UtilizationCeilingPPM
		rq.Result.UtilizationPPM = cost.UtilizationPPM
		rq.Result.UtilizationSource = refusal.String()
		if cost.UtilizationFallback != pricing.UtilFallbackNone {
			// At the same volume as an unpriced model, and for the same reason:
			// a price that quietly stopped moving is a price nobody audits.
			d.logf("app: utilization pricing on %s/%s fell back to the base rate (%s): %s",
				dec.Provider, dec.UpstreamModel, refusal, refusal.Why())
		}
	}
	if !cost.NotionalMissing {
		rq.Result.NotionalNanoUSD = cost.NotionalNano
		// §8.5 rule 5: missing is reported, never zero. Without this flag the
		// meter cannot tell a subscription with no notional_rate rule from one
		// whose list-rate equivalent genuinely came to nothing, and the first
		// of those makes the plan look infinitely efficient.
		rq.Result.NotionalPriced = true
	} else {
		// The other half of the same rule, and it is recorded rather than
		// inferred. Reaching pricing and finding no notional rule is a fact
		// about the CATALOG; never reaching pricing is a fact about the
		// request. Only the first makes an aggregate's notional sum an
		// understatement, and only a flag set here can tell the two apart by
		// the time the rollup is a row in a table.
		rq.Result.NotionalMissing = true
	}

	// The same figure the ledger row took, never a second reading of the Cost.
	// The token and request counters are measurements and are recorded whatever
	// the catalog could or could not say about the price.
	st.quota.record(dec.Credential, now, quota.Usage{
		CostNanoUSD:  charged,
		TokensInput:  int64(res.usage.InputTokens),
		TokensOutput: int64(res.usage.OutputTokens),
		Requests:     1,
	})
}

// observeUtilization turns this response into the occupancy figure the price engine
// takes, or into the named reason there is none.
//
// The three refusals it can produce itself are the three the transport knows about, and
// they are checked in the order that keeps the most specific one: a streamed answer never
// had a report to read, so reporting "no header" for it would send an operator looking for
// a header that was never going to arrive.
//
// It never returns a usable figure alongside a refusal, and never a refusal alongside a
// figure. That is the invariant the whole feature rests on — a fraction of zero is a
// measurement and prices at the base rate; an absent measurement is not, and prices at the
// base rate too, while saying something different in the row.
func observeUtilization(c *call, dec *router.Decision, res result, now time.Time) (int32, loadsignal.Refusal) {
	var obs loadsignal.Observation
	switch {
	case c != nil && c.stream:
		// vLLM's load report is non-streaming only (VLLM.md §3.2), and no scrape
		// stands behind it in this build — see internal/loadsignal's package
		// documentation for why a polled gauge is a good routing signal and a bad
		// pricing one. Streamed traffic is charged the base rate and says so.
		obs = loadsignal.Refuse(dec.Deployment, now, loadsignal.RefusalStreamed)
	default:
		obs = loadsignal.Parse(res.loadMetrics, dec.Deployment, now)
	}
	if r := obs.Usable(dec.Deployment, now, maxObservationAge); r != loadsignal.RefusalNone {
		return 0, r
	}
	ppm, err := pricing.UtilizationFromFraction(obs.Fraction)
	if err != nil {
		// Unreachable through Parse, which applies the same range rule. It is
		// checked anyway because the two guards are meant to be independent: this
		// one is the last line before a rate is multiplied by the number.
		return 0, loadsignal.RefusalNotAFraction
	}
	return ppm, loadsignal.RefusalNone
}

// maxObservationAge bounds how long an occupancy reading may travel between the response
// that produced it and the settlement of that same request.
//
// It is not a poll interval — nothing here is polled, and the real span is microseconds. It
// is a guard on the wiring, so that a later refactor cannot start carrying one request's
// contention into another request's price without the ledger row saying `stale`.
const maxObservationAge = 30 * time.Second

// billedUnit carries the vendor's stated billing unit across the one package
// boundary that separates the decoders from the price engine.
//
// It is a translation and not a shared type because internal/pricing imports
// nothing of dorang's — it is the package DESIGN §8.3 keeps free of binary
// floating point and of every other package's shape — so the two enumerations are
// declared apart and mapped here, where a new member on either side is a compile
// error rather than a silent default.
func billedUnit(u canonical.BilledUnit) pricing.BilledUnit {
	switch u {
	case canonical.BilledTokens:
		return pricing.BilledTokens
	case canonical.BilledDuration:
		return pricing.BilledDuration
	}
	return pricing.BilledUnstated
}

// fillRouteResult stamps the routing decision onto the request before the first
// byte goes out.
//
// c carries the decoded request, which the §10.1 downgrade report needs: the
// neutral form, and the opt-in already parsed onto the routing request rather
// than read off the headers a second time. loss is what the ENCODER removed,
// handed over by internal/backend at the one moment the header block is still
// writable; it is nil for an operation with no neutral request.
func fillRouteResult(rq *server.Request, dec *router.Decision, c *call, loss *canonical.LossReport) {
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
	rq.Result.DroppedParams = droppedParams(dec, c, loss)
	rq.Result.Downgraded = downgraded(c, dec, loss)
}

// fillQuotaResult renders the chosen credential's quota state into the two
// header families internal/server has always been able to stamp and has never
// been given anything to stamp.
//
// `x-ratelimit-*` and `x-dorang-quota-<window>-used-pct` were both rendered by
// [server.stampHeaders] off fields nothing produced, so neither had ever left
// this gateway — while COMPATIBILITY §7.8 cited the first as the reason not to
// mirror LiteLLM's own header, and an OPERATIONS runbook told an operator to
// read them off a response. The figures come from [router.Decision.Quota],
// which is the enforcement point's own view, so a client is told the number
// that will actually refuse it.
//
// ABSENT, NOT ZERO, in three places, and each is a different fact:
//   - no rule at all: the credential is unmetered. `Set` stays false and the
//     whole `x-ratelimit-*` family is omitted, because a limit of 0 says the
//     opposite of "no limit".
//   - a rule on a metric that has no standard header (cost, input-only or
//     output-only tokens): counted in the per-window percentage and left out
//     of `x-ratelimit-*`, which has no spelling for it.
//   - limit of zero: skipped, because a percentage of it is undefined rather
//     than 100.
func fillQuotaResult(rq *server.Request, dec *router.Decision) {
	v := &dec.Quota
	if v.N == 0 {
		return
	}
	rl := &rq.Result.RateLimit
	for i := 0; i < v.N; i++ {
		r := &v.Rules[i]
		if r.Limit <= 0 {
			continue
		}
		remaining := r.Limit - r.Used
		if remaining < 0 {
			remaining = 0
		}
		// The per-window percentage covers every metric, because "this
		// credential is at 90% of something" is actionable whatever the
		// something is.
		pct := int(r.Used * 100 / r.Limit)
		if pct > 100 {
			pct = 100
		}
		rq.Result.SetQuotaUsedPct(r.Window.String(), pct)

		// Only the two metrics the standard headers can spell reach them, and
		// the tightest rule wins: a client that obeys the number it is given
		// must not be told the looser of two limits it is actually under.
		switch r.Metric {
		case quota.MetricRequests:
			if !rl.Set || rl.LimitRequests == 0 || remaining < rl.RemainingRequests {
				rl.LimitRequests, rl.RemainingRequests = r.Limit, remaining
				rl.ResetRequests = quotaReset(r.ResetAt)
			}
			rl.Set = true
		case quota.MetricTokensTotal:
			if !rl.Set || rl.LimitTokens == 0 || remaining < rl.RemainingTokens {
				rl.LimitTokens, rl.RemainingTokens = r.Limit, remaining
				rl.ResetTokens = quotaReset(r.ResetAt)
			}
			rl.Set = true
		}
	}
}

// quotaReset renders a window boundary the way every provider spells it: whole
// seconds from now with an `s`. An empty string for a boundary nobody stated,
// because "0s" would tell a client to retry immediately.
func quotaReset(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	d := time.Until(at)
	if d < 0 {
		d = 0
	}
	return strconv.FormatInt(int64(d.Seconds()+0.5), 10) + "s"
}

// downgraded is what x-dorang-downgraded carries: the structural constructs this
// deployment cannot express that the caller opted into losing.
//
// DESIGN §10.1 has promised this header since revision 2 and nothing wrote it.
// The gap was arguable — x-dorang-allow-lossy is per construct, so the consent
// already named the thing — and the argument does not survive contact with how
// the opt-in is actually used. A client sets the header once, in its transport
// layer, for every request it will ever send; the header answers a different
// question, about ONE response. Consent says a PDF may be dropped. This says it
// was.
//
// Two sources, and both are needed because neither sees the other's cases.
//
// The MASK half subtracts the deployment's capability set from what the request
// uses. It is the only source for a wire shape whose adapter builds no loss
// report — the Gemini adapter converts without one — and it is one subtraction
// rather than a second walk over the request. What it cannot see is a loss the
// encoder raises while HOLDING the bit: crossing a thinking block's SIGNATURE
// into the OpenAI family is a downgrade even though CapThinkingBlocks is
// present, because no field there carries integrity material and §10.2 forbids
// fabricating one.
//
// The ENCODER half is that report, threaded out of internal/backend on
// [backend.Call.Accepted]. It used to exist and reach nobody: the adapters
// passed no Loss to either MarshalRequest, so a signature dropped on the second
// turn of an agentic flow — the worst possible place to discover it, per §10.2 —
// left with a 200 and no header.
//
// The opt-in is intersected into the mask half deliberately. A structural loss
// the caller did NOT consent to is a 400 from routing or from the material gate
// and never reaches a response at all, so a header naming one would be describing
// a request that was refused. It is NOT intersected into the encoder half, for
// the same reason that half exists: nothing refused the signature loss, because
// no gate could see it, so gating the report on a consent that was never asked
// for would reproduce the silence exactly.
func downgraded(c *call, dec *router.Decision, loss *canonical.LossReport) string {
	var names []string
	if c != nil && c.creq != nil && dec.Capabilities != 0 && c.rreq.AllowLossy != 0 {
		lost := dec.Capabilities.Missing(c.creq.RequiredCapabilities()).Structural() & c.rreq.AllowLossy
		names = lost.Names()
	}
	names = appendUnique(names, loss.Constructs()...)
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, ",")
}

// droppedParams is what x-dorang-dropped-params carries: everything the caller
// sent that dorang did not apply.
//
// Four sources, and they belong together because a caller asking "was what I
// sent used" does not care which layer decided otherwise:
//
//   - the capability conversion removed, and the constructs this deployment is
//     ABLE to carry that §4.4 declines to send it ([router.Decision.Dropped]);
//   - the client priority hint, when this principal has no §10.5 grant;
//   - the caller's service_tier, when §10.5's fold sent a different one;
//   - what the ENCODER dropped for a reason no capability mask states.
//
// The fourth is §10.2's, and until the loss report travelled out of
// internal/backend it had no writer: "if budget < min_thinking_budget: reasoning
// is disabled for this request, and x-dorang-dropped-params says so". The
// collapse depends on the caller's own max_tokens, so it is not a property of the
// deployment and the first source cannot contain it — internal/wire/anthropic's
// encodeThinking has recorded it since it was written, into a report the adapter
// discarded.
//
// All four are here for the reason §10.3 gives — silently discarding something
// a caller sent leaves them believing it took effect — and §10.5 names this
// header specifically. Each of them was, at some point, discarded in complete
// silence, and each was easy to miss because the value was also never read.
func droppedParams(dec *router.Decision, c *call, loss *canonical.LossReport) string {
	names := dec.Dropped.Params()
	if dec.PriorityHintDropped {
		names = append(names, HeaderClientPriority)
	}
	if tierOverridden(dec, c) {
		names = append(names, canonical.ConstructServiceTier)
	}
	if loss != nil {
		// Deduplicated rather than concatenated: the encoder computes the same
		// droppable subtraction the router does, so every knob this deployment
		// lacks is named by both and a caller would read `top_k,top_k`.
		names = appendUnique(names, loss.Dropped...)
	}
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, ",")
}

// appendUnique appends the names not already in dst, preserving the order they
// were first seen in. Empty names are skipped: a header entry with no name is a
// comma a client has to parse around.
func appendUnique(dst []string, names ...string) []string {
	for _, n := range names {
		if n == "" {
			continue
		}
		dup := false
		for _, have := range dst {
			if have == n {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, n)
		}
	}
	return dst
}

// tierOverridden reports that the caller named a service tier and dorang sent a
// different one.
//
// §10.5's tier fold puts DORANG's priority class in the field, and §7.5 says why
// — priority is an operator grant and honouring the caller's value here would be
// the self-elevation §10.5 refuses. What it does not say is that the caller can
// find out, and they could not: their `service_tier: priority` left as
// `service_tier: flex` with a 200 and nothing else. service_tier is material
// precisely because it selects a PRICE BAND, so of every parameter dorang
// rewrites this is the one a caller most needs told.
//
// It is decided here rather than on the routing decision because it needs the
// value the caller wrote. The router knows only that this deployment has a fold,
// and reporting on that alone would claim an override on every request where
// dorang's class happened to choose the tier the caller already asked for.
//
// `auto` is not an override: it delegates the band to the provider, so the
// caller expressed no band for dorang to have replaced. That matches
// [canonical.Request.RequiredCapabilities], which raises no bit for it either.
func tierOverridden(dec *router.Decision, c *call) bool {
	if dec.PriorityTier == "" || c == nil || c.creq == nil {
		return false
	}
	got := c.creq.ServiceTier
	return got != "" && !strings.EqualFold(got, canonical.ServiceTierAuto) &&
		!strings.EqualFold(got, dec.PriorityTier)
}

// routeError renders a routing refusal as an HTTP answer. router.Error already
// carries the status, the code and a message written for a human, so nothing is
// paraphrased here.
//
// What it does add is the two things §11 fixes per CONDITION rather than per
// status: the offending parameter, and the Anthropic family's spelling where it
// differs. A router refusal is the most common error a client sees from a
// gateway, and it was the one leaving with `param: null` where §11.1's own
// worked example shows `"model"`.
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
		switch re.Code {
		case router.CodeModelNotFound:
			// §11.1's OpenAI envelope names the field: the model the caller
			// asked for is the offending parameter, and a client that renders
			// `param` has nothing to show without it.
			e = e.WithParam("model")
		case router.CodeContextWindow:
			e = e.WithParam("messages")
		case router.CodeUnsupportedConstruct:
			// §10.1's "machine-readable body naming the unsupported construct",
			// which [router.Error.Constructs] computes and nothing carried out.
			// The refusal is raised at two gates on purpose — routing asks
			// whether ANY deployment expresses the request, the backend asks
			// whether the chosen one does — and only the second one named the
			// construct and filled `param`. A caller who trips the first gate is
			// the one who has never seen the construct vocabulary before, and
			// they were handed the sentence with none of it in.
			if len(re.Constructs) > 0 {
				list := strings.Join(re.Constructs, ", ")
				e.Message += ": " + list + "; retry with " + HeaderAllowLossy + ": " +
					strings.Join(re.Constructs, ",")
				// The MATERIAL half only. Its construct id is a real request
				// field; the located half is a capability, and naming
				// `thinking_block` as a param would send a field-locating SDK to
				// a member that does not exist — the same rule
				// internal/backend's materialRefusal applies to the identical
				// refusal one layer down.
				if p := constructParam(re.Constructs); p != "" {
					e = e.WithParam(p)
				}
			}
		case router.CodeNoCapacity, router.CodeNoCandidate:
			// §11.2's two capacity rows are the only ones whose type is chosen
			// by condition rather than by status: 429 is `rate_limit_error` to
			// an OpenAI client and `overloaded_error` to an Anthropic one.
			e.AltType = server.TypeOverloaded
		}
		if !re.ResetAt.IsZero() {
			e.RetryAfterSeconds = int(time.Until(re.ResetAt).Seconds()) + 1
		}
		return e
	}
	return err
}

// constructParam is the wire parameter a refused construct list can point at,
// or "" when none of them is a request field.
//
// Only [canonical.Material] constructs have one, and for those the construct id
// IS the parameter name — that is the whole difference between the two halves of
// [canonical.Structural].
func constructParam(constructs []string) string {
	for _, name := range constructs {
		c, ok := canonical.ParseCapability(name)
		if !ok {
			continue
		}
		if p := c.Material().Params(); len(p) > 0 {
			return p[0]
		}
	}
	return ""
}

// defaultMaxTokens is the output ceiling handed to an encoder when the caller
// named none. The field is required on the messages surface, and a constant
// would silently cap output at a number the caller never chose, so the
// deployment's catalogued ceiling is used and nothing is invented (§10.7).
// A model the catalog does not declare a ceiling for yields zero, which the
// encoder turns into an explicit error rather than a guess.
func (d *dispatcher) defaultMaxTokens(kind, upstreamModel string) int {
	st := d.state()
	if st == nil || st.catalog == nil {
		return 0
	}
	return st.catalog.Model(kind, upstreamModel).MaxOutputTokens
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

// maxOutputTokens is the ceiling the CALLER named, and nothing else.
//
// Zero means they named none, which on the OpenAI family is the common case
// rather than an edge one — max_tokens is optional there. It is deliberately not
// filled in with a default here: the number that belongs in its place is the
// serving deployment's own declared ceiling, and no deployment has been chosen
// yet. internal/router applies it per candidate, where it is knowable.
func maxOutputTokens(c *canonical.Request) int64 {
	if c == nil || c.MaxTokens == nil {
		return 0
	}
	return int64(*c.MaxTokens)
}

// estimateInputTokens is gone. It divided the raw request body by three, which
// made a base64 image cost several hundred times what an image costs; the
// replacement is internal/tokenest, reached through (*call).estimate.

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

// toolQuantities is the priced part of the server-side tool report.
//
// It exists as its own function because the wiring is the thing worth pinning.
// A test that exercised [toolCount] alone would prove the reader works and say
// nothing about whether the dispatcher hands the numbers to pricing — which is
// exactly the failure this whole change closes, one level up: a count that is
// read, carried, and charged by nobody.
func toolQuantities(extra *canonical.UsageExtra) (searches, imageIn, imageOut int64) {
	return toolCount(extra, "web_search", "num_requests"),
		toolCount(extra, "image_gen", "input_tokens"),
		toolCount(extra, "image_gen", "output_tokens")
}

// toolCount reads one number out of the server-side tool report.
//
// The report is carried as raw JSON rather than a struct because its shape is
// the vendor's and grows without a version bump — a struct would silently drop
// whatever was added, and what is added here is a charge. Reading the two
// members dorang prices, and leaving the rest intact for the ledger, keeps the
// bill checkable without pretending to model a surface nobody has fixed.
//
// A member that is absent, not a number, or negative yields zero: a charge
// dorang cannot read is not a charge it may invent.
func toolCount(extra *canonical.UsageExtra, tool, member string) int64 {
	if extra == nil || len(extra.ToolUsage) == 0 {
		return 0
	}
	raw, ok := extra.ToolUsage[tool]
	if !ok {
		return 0
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	v, ok := m[member]
	if !ok {
		return 0
	}
	var n int64
	if err := json.Unmarshal(v, &n); err != nil || n < 0 {
		return 0
	}
	return n
}
