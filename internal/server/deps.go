package server

import (
	"context"
	"net/http"
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
type Principal interface {
	// KeyID identifies the credential row. Fixed cardinality; safe to log.
	KeyID() string
	// Authorize enforces this caller's limits for one request. A returned
	// *Error is answered verbatim; any other error becomes a 403.
	Authorize(Access) error
	// AllowsModel reports whether a client-facing model name is permitted.
	AllowsModel(model string) bool
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

// ModelSlice adapts a fixed list to [ModelLister].
type ModelSlice []Model

// Models implements [ModelLister].
func (m ModelSlice) Models() []Model { return m }

// Event is what the server hands the meter when a request finishes. It is
// deliberately flat and free of pointers into the pooled request struct, so the
// meter may keep it past the request's lifetime.
type Event struct {
	RequestID  string
	KeyID      string
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

	Attempt      int
	FallbackFrom string
	RouteReason  string

	QueueNS   int64
	TTFTNS    int64
	LatencyNS int64

	Tokens Usage

	CostNanoUSD     int64
	NotionalNanoUSD int64
	SpendNanoUSD    int64
	BudgetNanoUSD   int64
	// Priced marks the cost fields as meaningful. An unpriced request must not
	// claim to have cost zero.
	Priced bool

	// QuotaUsedPct is the credential's quota-window consumption, keyed by
	// window name ("minute", "day", …). Nil when no probe reported.
	QuotaUsedPct map[string]int

	// NativeStopReason is the backend's own stop reason, preserved out of band
	// because the wire mapping to OpenAI's vocabulary is lossy in a way that
	// tells the client a failed turn ended normally (COMPATIBILITY §4.2a).
	NativeStopReason string
	// DroppedParams lists what parameter conversion removed (DESIGN §10.3).
	DroppedParams string

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
