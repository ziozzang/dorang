package admin

import "sync/atomic"

// Metrics is the administration surface's counter set (§12.3).
//
// Fixed cardinality, deliberately: nothing here is labelled by a route, an
// object id, or anything else a caller supplies. The per-object numbers an
// operator wants — spend by key, health by credential, occupancy by axis — are
// served by the endpoints themselves, from the ledger and from the live
// reporters, which is where §9 says they belong.
type Metrics struct {
	requests      atomic.Uint64
	uiRequests    atomic.Uint64
	authFailures  atomic.Uint64
	unimplemented atomic.Uint64
	serverErrors  atomic.Uint64

	// uiMutations counts credential-lifecycle actions taken from the operator
	// UI, and uiForgeries the unsafe requests to it that could not be proved to
	// have come from it.
	//
	// The second is the one to alert on. A cookie-authenticated mutating
	// surface is a forgery target, and the difference between "a session
	// expired while a form was open" and "somebody is posting at this gateway
	// from another origin" is a rate, which is a thing a counter can answer and
	// prose cannot.
	uiMutations atomic.Uint64
	uiForgeries atomic.Uint64

	mutations atomic.Uint64
	// auditFailures counts mutations that were applied but could not be
	// audited. It is separate from serverErrors because it is the one 500 that
	// means the trail is incomplete, which is an operational fact rather than a
	// transient failure.
	auditFailures atomic.Uint64
	// invalidations counts changes announced to the rest of the fleet, and
	// invalidationFailures the ones that were applied and could not be
	// announced. The second is the number that matters: a deployment where it
	// is non-zero is a deployment whose revocations are landing on the
	// credential cache TTL rather than within the published bound, and that has
	// to be visible rather than inferred.
	invalidations        atomic.Uint64
	invalidationFailures atomic.Uint64
	// keysIssued counts credentials minted. The secret is returned once; this
	// counter is the only lasting trace of the count.
	keysIssued atomic.Uint64
	// rangeRefusals counts ledger queries refused for an absent or too-wide
	// time range. A deployment where this is large has a client that expects
	// unbounded search, which is worth knowing before it becomes a support
	// ticket about slowness.
	rangeRefusals atomic.Uint64
	// notionalMissing counts answers where the notional figure was reported
	// unavailable (§8.5 rule 5: missing is reported, never zero).
	notionalMissing atomic.Uint64
}

// MetricsSnapshot is a consistent-enough read of [Metrics] for export.
type MetricsSnapshot struct {
	Requests   uint64
	UIRequests uint64
	// UIMutations counts lifecycle actions taken from the operator UI, and
	// UIForgeries the unsafe requests to it that were refused for want of proof
	// that the UI sent them.
	UIMutations   uint64
	UIForgeries   uint64
	AuthFailures  uint64
	Unimplemented uint64
	ServerErrors  uint64
	Mutations     uint64
	AuditFailures uint64
	Invalidations uint64
	// InvalidationFailures counts mutations that were applied and could not be
	// announced, so the fleet is converging on the credential cache TTL.
	InvalidationFailures uint64
	KeysIssued           uint64
	RangeRefusals        uint64
	NotionalMissing      uint64
}

func (m *Metrics) snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		Requests:      m.requests.Load(),
		UIRequests:    m.uiRequests.Load(),
		UIMutations:   m.uiMutations.Load(),
		UIForgeries:   m.uiForgeries.Load(),
		AuthFailures:  m.authFailures.Load(),
		Unimplemented: m.unimplemented.Load(),
		ServerErrors:  m.serverErrors.Load(),
		Mutations:     m.mutations.Load(),
		AuditFailures: m.auditFailures.Load(),

		Invalidations:        m.invalidations.Load(),
		InvalidationFailures: m.invalidationFailures.Load(),

		KeysIssued:      m.keysIssued.Load(),
		RangeRefusals:   m.rangeRefusals.Load(),
		NotionalMissing: m.notionalMissing.Load(),
	}
}
