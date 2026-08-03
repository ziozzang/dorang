package server

import (
	"context"
	"net/http"
	"time"
)

// Everything internal/server consumes is declared here, as an interface narrow
// enough to be implemented by a twenty-line fake.
//
// This is not decoupling for its own sake. The gateway's HTTP surface, its
// router, its authenticator and its metering pipeline are built in parallel and
// change on different schedules; a compile-time edge from this package to any of
// them means the HTTP surface cannot be tested until all of them exist, and that
// every benchmark of the request path is really a benchmark of whatever those
// packages happened to be doing that week. The concrete wiring happens once, in
// cmd/dorang.

// Principal is an authenticated caller, as the HTTP surface needs it: an
// identity for logs and metering, an authorization decision, and a model
// allow-list for filtering GET /v1/models (COMPATIBILITY §7.4).
//
// It carries no credential material and the server never asks it for any.
//
// The identity is three ids, not one. DESIGN §9.4 materializes usage by team
// per day and §9.2 gives the ledger a user_id column; both are filled from the
// request path or not at all, because nothing downstream can recover a team
// from a key id without a store lookup the hot path is forbidden to make
// (§9.6). A Principal that exposed only KeyID left usage_by_team_day a table
// nothing writes.
type Principal interface {
	// KeyID identifies the credential row. Fixed cardinality; safe to log.
	KeyID() string
	// UserID is the owning user, or "" when the credential has none. Fixed
	// cardinality; safe to log.
	UserID() string
	// TeamID is the owning team, or "" when the credential has none. Fixed
	// cardinality; safe to log.
	TeamID() string
	// Authorize enforces this caller's limits for one request. A returned
	// *Error is answered verbatim; any other error becomes a 403.
	Authorize(Access) error
	// AllowsModel reports whether a client-facing model name is permitted.
	AllowsModel(model string) bool
}

// AdminPrincipal is the optional half of [Principal] that answers whether a
// caller may reach an administrative route.
//
// It is an optional interface rather than a sixth method on Principal because
// the server has no administrative surface of its own: exactly one route asks
// the question, and a Principal that cannot answer it is not an incomplete
// implementation, it is a deployment with no administrative credential. A
// Principal that does not implement this is not an administrator — the absence
// of an answer is a "no", never a skipped check.
type AdminPrincipal interface {
	// IsAdmin reports whether this caller may read administrative routes.
	IsAdmin() bool
}

// isAdmin reports whether p may reach an Admin route.
func isAdmin(p Principal) bool {
	a, ok := p.(AdminPrincipal)
	return ok && a.IsAdmin()
}

// ModelRestricted is an optional [Principal] capability: it reports whether the
// caller's model allow-list refuses anything at all.
//
// It exists for the one question [Principal.AllowsModel] cannot answer — "is
// there a list?" — which is what a route has to know when it could not
// determine the model. A passthrough body that is not JSON names no model this
// package can read; whether that is fine or fatal depends entirely on whether
// the key was restricted in the first place.
//
// A Principal that does not implement it is treated as restricted, so the
// absence of the method costs availability rather than enforcement.
type ModelRestricted interface {
	// ModelsRestricted reports whether any subject's allow-list is non-empty.
	ModelsRestricted() bool
}

// Access describes the request being authorized.
type Access struct {
	// Model is the client-facing model name, "" when the route has none.
	Model string
	// Route is the request path.
	Route string
}

// Authenticator resolves a request's credentials to a principal.
//
// It is handed the whole header map rather than a token because which of the
// six accepted header names (COMPATIBILITY §7.3) supplied the credential is the
// authenticator's business, not the mux's — and because the server must strip
// all six regardless of which one won.
//
// A returned *Error is answered verbatim; any other error becomes a 401.
type Authenticator interface {
	AuthenticateHeader(ctx context.Context, h http.Header) (Principal, error)
}

// Dispatcher owns everything past the gate: conversion, routing, capacity,
// the upstream call, and writing the response body.
//
// The contract with the server is narrow and has one ordering rule: fill
// rq.Result before writing the first byte. Extension headers are stamped on the
// first WriteHeader or Write, which for a stream is before the first frame, so
// anything set afterwards is invisible to the client (DESIGN §10.4). Post-hoc
// values for a stream travel by the opt-in usage event instead — see
// [Request.WantsUsageEvents].
//
// Returning an error after bytes have been written is legal and handled: the
// server emits the in-band SSE error of COMPATIBILITY §1.3 rather than
// pretending it can still change the status.
type Dispatcher interface {
	Dispatch(ctx context.Context, rq *Request, w http.ResponseWriter) error
}

// DispatchFunc adapts a function to [Dispatcher].
type DispatchFunc func(ctx context.Context, rq *Request, w http.ResponseWriter) error

// Dispatch implements [Dispatcher].
func (f DispatchFunc) Dispatch(ctx context.Context, rq *Request, w http.ResponseWriter) error {
	return f(ctx, rq, w)
}

// Model is one entry of GET /v1/models.
type Model struct {
	// ID is the client-facing model name. Opaque: no component splits it on
	// any character (DESIGN §2.1).
	ID string
	// OwnedBy fills the owned_by field. Empty renders as [DefaultOwnedBy].
	OwnedBy string
}

// ModelLister supplies the client-facing model list. The server filters it by
// the calling key's allow-list; the lister itself is unaware of principals.
type ModelLister interface {
	// Models returns the visible models in a stable order. The slice is not
	// retained or mutated by the server.
	Models() []Model
}

// SecretPrincipal is the optional half of [Principal] that names WHICH of a
// key's secrets authenticated the request (DESIGN §11.2c).
//
// A key has a durable id and one or more secrets; during a rotation's grace
// period two of them authenticate to the same principal. The ledger records the
// secret so an operator can see whether the client actually rolled before the
// window closes, instead of finding out when it shuts — and the key id alone
// cannot answer that question.
type SecretPrincipal interface {
	// SecretID identifies the api_key_secrets row. Fixed cardinality; safe to
	// log. It is never any part of the secret itself.
	SecretID() string
}

// ModelSlice adapts a fixed list to [ModelLister].
type ModelSlice []Model

// Models implements [ModelLister].
func (m ModelSlice) Models() []Model { return m }

// Event is what the server hands the meter when a request finishes. It is
// deliberately flat and free of pointers into the pooled request struct, so the
// meter may keep it past the request's lifetime.
type Event struct {
	RequestID string
	KeyID     string
	// SecretID names which of the key's secrets authenticated, or "" for a
	// principal that does not track them (DESIGN §11.2c).
	SecretID string
	// UserID and TeamID are the calling principal's owners, copied here so the
	// rollups and the ledger of DESIGN §9.2/§9.4 can be keyed by them. They are
	// empty for a public route and for a credential that has neither.
	UserID     string
	TeamID     string
	Route      string
	Family     Family
	Method     string
	Status     int
	Model      string
	Result     Result
	DurationNS int64
	BytesIn    int64
	BytesOut   int64
	// Passthrough marks an event produced by the generic engine, where the
	// body was never parsed and the numbers are counts and bytes rather than
	// tokens (DESIGN §10.6 step 5).
	Passthrough bool
	// Streamed reports that the client was answered with an event stream.
	//
	// It is taken from the Content-Type actually written, not from the
	// `stream: true` the request asked for: a request that asks to stream and
	// is refused before the first frame did not stream, and the ledger column
	// is read to answer "was this a streamed turn", not "what was requested".
	Streamed bool
	// ErrorShape records which upstream envelope shape produced the error,
	// when the request failed.
	ErrorShape Shape
}

// Meter absorbs one finished request.
//
// It is called off the response path, after the client's last byte, and its
// failure is never the request's failure — DESIGN §10.6 step 5 says so for
// passthrough and there is no reason for the rest of the surface to be
// different. The server recovers a panic from Record and keeps going.
type Meter interface {
	Record(ev Event)
}

// MeterFunc adapts a function to [Meter].
type MeterFunc func(Event)

// Record implements [Meter].
func (f MeterFunc) Record(ev Event) { f(ev) }

// nopMeter is the default when none is configured.
type nopMeter struct{}

func (nopMeter) Record(Event) {}

// Observer watches a sampled fraction of finished requests so they can be
// compared against a reference gateway out of band (DESIGN §14.1).
//
// It is the migration gate, not a feature: "run alongside, then take over"
// (§0.3) needs proof from real traffic, and the proof is an empty structural
// diff report. None of that may cost the client anything, so the contract has
// three rules and the server enforces all three.
//
//  1. Nothing here may block. Sample runs on the request path; Observe runs
//     where the meter runs, after the client's last byte. An implementation
//     that wants to do I/O queues it (§9.6 rule 3: back-pressure surfaces as a
//     visible drop, never as a wait).
//  2. Nothing here may fail the request. A panic is recovered and counted, the
//     same treatment [Meter] gets.
//  3. Nothing here may retain what it is handed. Every slice and header map in
//     an [Observation] belongs to the pooled request and is reused the moment
//     Observe returns; an implementation that keeps anything copies it.
//
// The server has no opinion about what a comparison is, what a reference
// gateway costs, or where a report goes. It supplies the bytes and the
// sampling hook; internal/shadow supplies the rest.
type Observer interface {
	// Sample reports whether this request should be captured. It is called
	// once, before the response starts, so that the capture tap can be armed
	// before the first byte — a decision made afterwards would miss the whole
	// response.
	//
	// The decision must be deterministic in requestID (§14.1): a retry of the
	// same call must not be sampled a second time, or the cost ceiling is
	// spent twice on one logical request. h is the inbound header map, which
	// carries the loop guard — a request that is itself a shadow copy must not
	// be shadowed again.
	Sample(requestID string, h http.Header) bool

	// Observe absorbs one captured request.
	Observe(*Observation)

	// Health appends a JSON object describing the observer's state to dst, for
	// the health body under "shadow". Returning dst unchanged omits it.
	//
	// It exists because a cost ceiling that stops shadowing has to be visible
	// somewhere an operator already looks, and a metric alone is not that
	// place when the question is "is the cutover gate still running".
	Health(dst []byte) []byte

	// Metrics appends Prometheus text-exposition lines to dst.
	Metrics(dst []byte) []byte
}

// HealthReporter contributes one named object to the health body.
//
// It exists because DESIGN §12.1 promises that a metering drop "is never
// silent", and a Prometheus gauge alone does not keep that promise: an operator
// diagnosing a gateway looks at /health first, and a subsystem that is losing
// data while /health says "healthy" is silent in every way that matters at 3am.
//
// The contract is the one [Observer.Health] already follows, and for the same
// reason: a reporter appends raw JSON and returns dst unchanged to omit itself,
// so a subsystem with nothing to say costs one call and no bytes. A reporter
// must not block, must not perform I/O, and must not change the status code —
// degraded metering is not a serving failure and must not take a pod out of
// rotation.
type HealthReporter interface {
	// HealthName is the object key, a fixed string such as "metering".
	HealthName() string
	// Health appends a JSON value to dst. Returning dst unchanged omits the key.
	Health(dst []byte) []byte
}

// ReadinessGate is a dependency that can decide this node must stop receiving
// new work, without the node draining and without it dying.
//
// It exists because readiness had exactly one input — the drain — and a drain is
// something this process decides about itself. Everything else a node needs in
// order to serve is outside it, and the credential store is the sharpest case:
// with it unreachable, cached principals keep serving, so nothing aggregate
// looks wrong, while every caller whose key this node has not recently seen gets
// a 503 from a load balancer that still believes in it. §13 draws readiness as
// the signal that decides whether new work should arrive, and a node that cannot
// authenticate anyone new is a node that should not be sent any.
//
// It is emphatically NOT a liveness input. The outage this models recovers on
// its own — reconnection is transparent, the restart count stays zero, and work
// buffered during the gap is written when the dependency returns — so a probe
// that killed the process would turn a recoverable blip into a cold start, which
// is the same mistake as pointing a liveness probe at /health/readiness.
//
// Two rules, both because [Server.Ready] is on the path of every container
// probe and every metrics scrape:
//
//   - It must not block and must not perform I/O. It is answering a request.
//     Whatever consults the dependency does so somewhere else and leaves an
//     answer here.
//   - It must not be a hair trigger. "A query failed" is not "cannot serve new
//     work". [health.Dependency] is the implementation this was written for and
//     states the threshold it uses and why.
type ReadinessGate interface {
	// GateName is a fixed, low-cardinality key for the health body, such as
	// "store".
	GateName() string
	// ReadyForWork reports whether new work may arrive, and a fixed-cardinality
	// reason when it may not. The reason is rendered into the health body; ""
	// is legal and renders as an absent field.
	ReadyForWork() (ok bool, reason string)
}

// MetricsSource renders the whole Prometheus scrape.
//
// It is one method wide for the same reason [Observer] is: internal/metrics
// pulls from every subsystem in the process, and a compile-time edge from the
// HTTP surface to that assembly would put the router, the store and the cluster
// ledger behind every test of this package. The concrete wiring happens once,
// in internal/app.
type MetricsSource interface {
	// Metrics appends the exposition body to dst and returns it. It must not
	// block and must not perform I/O: it is answering a request.
	Metrics(dst []byte) []byte
}

// Observation is one captured request and the response the client actually
// received.
//
// Every slice and map in it is borrowed. It is valid for the duration of the
// [Observer.Observe] call and no longer.
type Observation struct {
	// RequestID is the join key. It is what the reference call is tagged with,
	// so a difference found here can be traced to a row on both sides.
	RequestID string

	Method   string
	Path     string
	RawQuery string
	// Route is the matched route's fixed-cardinality name, "" when nothing
	// matched — which is itself worth comparing, since a 501 on one gateway and
	// a 404 on the other is a divergence a client notices.
	Route  string
	Family Family
	Model  string
	Stream bool

	// RequestHeader is the inbound header map, credentials included and
	// unmodified. It is not pre-stripped: an observer that replays the request
	// builds its outgoing header from an allow-list, which is a stronger
	// guarantee than trusting a deny-list applied here to have been complete.
	// [StripAuthHeaders] is available for defence in depth.
	RequestHeader http.Header
	// RequestBody is the capped body, nil for a route that does not read one.
	RequestBody []byte

	// Status is what the client was answered. It is the status actually
	// written, not the status the handler intended.
	Status int
	// ResponseHeader is the header map as sent, extension headers included.
	ResponseHeader http.Header

	// ResponseHead is the first bytes of the response body and ResponseTail the
	// last, with a gap between them when the body was larger than the two
	// windows. Two windows rather than one because a stream's terminator is the
	// last thing on the wire and a head buffer alone loses exactly it.
	ResponseHead []byte
	ResponseTail []byte
	// BodyBytes is the true body length, however much of it was kept.
	BodyBytes int64
	// Truncated marks that ResponseHead and ResponseTail do not meet, so a
	// comparison over the body is a comparison over a prefix and a suffix.
	Truncated bool

	// Duration is what the client waited.
	Duration time.Duration
	// CostNanoUSD is dorang's own cost for this request, and Priced says
	// whether it means anything. It is the only honest estimate of what the
	// reference call is about to cost, since the reference serves the same
	// request to the same model.
	CostNanoUSD int64
	Priced      bool
}

// Usage is the token accounting of one request.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	Total      int64
}

// RateLimit is the standard-form rate-limit view returned to the client.
type RateLimit struct {
	LimitRequests     int64
	RemainingRequests int64
	ResetRequests     string
	LimitTokens       int64
	RemainingTokens   int64
	ResetTokens       string
	// Set marks the struct as populated. A zero limit and an absent limit are
	// different answers and the headers must not claim the former for the
	// latter.
	Set bool
}

// Result is what the dispatcher reports about a request. It is stamped into
// extension headers (DESIGN §10.4) and copied into the meter event.
//
// It is a plain struct on the pooled request: the dispatcher writes fields, the
// server reads them. Nothing here is a secret — provider, credential and
// deployment are ids.
type Result struct {
	Provider      string
	Credential    string
	Deployment    string
	UpstreamModel string

	Attempt int
	// FallbackFrom is the cause that sent this request down a fallback chain,
	// spelled "fallback:<cause>". It is a reason, not a place, and it is what
	// X-Dorang-Fallback-From has always carried.
	FallbackFrom string
	// FallbackFromDeployment is the deployment the request left. It is a
	// separate field because DESIGN §12.3's fallback counter is keyed by
	// {from, to, reason} and the first of those is a deployment id: a counter
	// with an empty `from` cannot answer "which backend is shedding traffic",
	// which is the only question it is asked.
	FallbackFromDeployment string
	RouteReason            string

	QueueNS   int64
	TTFTNS    int64
	LatencyNS int64

	Tokens Usage

	CostNanoUSD int64
	// MarginalNanoUSD and SubscriptionNanoUSD are CostNanoUSD's decomposition by
	// pricing class (DESIGN §8.1). They reach the ledger's `marginal_cost_nano`
	// and `subscription_cost_nano` columns, which /spend/logs reports as
	// `marginal_spend` and `subscription_spend`.
	//
	// They are carried rather than derived because neither can be recovered from
	// the total, and because §8.1 requires them to be separate figures: routing
	// compares the marginal one alone, so that a sunk plan cost cannot make a
	// saturated plan look cheap. They do not have to sum to CostNanoUSD — an
	// `adjustment` rule is a third class applied on top of both.
	MarginalNanoUSD     int64
	SubscriptionNanoUSD int64
	NotionalNanoUSD     int64
	SpendNanoUSD        int64
	BudgetNanoUSD       int64
	// SpendKnown says whether SpendNanoUSD was looked up or merely defaulted.
	//
	// A request whose estimated cost is zero takes no reservation, so nothing
	// hydrates the budget hold from the durable counter and the figure behind it
	// is an empty block rather than this subject's spend. That is "not loaded",
	// not "zero", and the two must not read alike on the wire — the same
	// absent-versus-zero rule [setInt] applies to the token counters and DESIGN
	// §8.5 applies to the notional figure.
	SpendKnown bool
	// Priced marks the cost fields as meaningful. An unpriced request must not
	// claim to have cost zero.
	Priced bool
	// NotionalPriced marks NotionalNanoUSD as meaningful, and it is a separate
	// flag from Priced because the two rules are separate: a request can be
	// billed exactly and still have no list-rate equivalent, which is the
	// normal case on a subscription that nobody has written a notional_rate
	// rule for. DESIGN §8.5 rule 5 requires that case to be reported as missing
	// and to increment a counter — "silently returning zero would make a
	// subscription look infinitely efficient", and without this flag zero is
	// exactly what a reader of NotionalNanoUSD sees.
	NotionalPriced bool

	// QuotaUsedPct is the credential's quota-window consumption, keyed by
	// window name ("minute", "day", …). Nil when no probe reported.
	QuotaUsedPct map[string]int

	// NativeStopReason is the backend's own stop reason, preserved out of band
	// because the wire mapping to OpenAI's vocabulary is lossy in a way that
	// tells the client a failed turn ended normally (COMPATIBILITY §4.2a).
	NativeStopReason string
	// NativeErrorType and NativeErrorMessage are the upstream's own error
	// fields, recorded here for the ledger.
	//
	// This is the other half of COMPATIBILITY §11.3: the text is kept out of
	// the response body, so it has to be kept SOMEWHERE or an outage becomes
	// undebuggable. The ledger is that somewhere. Whoever fills these is
	// responsible for having scrubbed provider credentials out of them first —
	// a backend that echoes the key it was given puts it in exactly this
	// string, and a ledger row is not a safe place for one either.
	NativeErrorType    string
	NativeErrorMessage string
	// DroppedParams lists what parameter conversion removed (DESIGN §10.3).
	DroppedParams string
	// Downgraded lists the structural constructs this request lost with the
	// caller's consent, by construct id (DESIGN §10.1).
	//
	// It is empty on the overwhelming majority of requests, because a structural
	// loss without x-dorang-allow-lossy is a 400 and never reaches a response at
	// all. That is what makes it safe to stamp: the header appears only when the
	// caller asked for the mechanism and the mechanism fired.
	Downgraded string

	// UtilizationPriced reports that the winning price rule scales its rate by
	// how contended the backend was (DESIGN §8.6), so the four fields below carry
	// a meaning and are published. It is false for every ordinary rate card,
	// where the absence of these headers means "this price does not move".
	UtilizationPriced bool
	// UtilizationMultiplierPPM is the factor that was APPLIED, in parts per
	// million: 1_000_000 is 1.0x. On a fallback it is exactly 1_000_000 and
	// UtilizationSource says why.
	UtilizationMultiplierPPM int64
	// UtilizationCeilingPPM is the rule's declared maximum. A price that moves
	// has to publish the most it can move to, as a number, beside the charge it
	// bounds — DESIGN §5.6's rule for a bounded-error mechanism, applied to a
	// bounded-variance price.
	UtilizationCeilingPPM int64
	// UtilizationPPM is the occupancy the factor came from, and is meaningful
	// only when UtilizationSource is "observed".
	UtilizationPPM int32
	// UtilizationSource names where the reading came from, or which refusal
	// stood in for it — the internal/loadsignal vocabulary, verbatim. It is what
	// makes an absent premium auditable rather than invisible: "observed" and
	// "no_load_header" produce the same charge on an idle backend and are not the
	// same fact about it.
	UtilizationSource string

	RateLimit         RateLimit
	RetryAfterSeconds int
}

// reset clears a Result for reuse. Written out rather than assigned from a zero
// value so the quota map's capacity survives the request.
func (r *Result) reset() {
	m := r.QuotaUsedPct
	*r = Result{}
	if m != nil {
		clear(m)
		r.QuotaUsedPct = m
	}
}
