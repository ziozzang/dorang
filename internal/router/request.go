package router

import (
	"context"
	"errors"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/quota"
)

// PinStrength is how strongly a request is bound to where it must go.
type PinStrength uint8

const (
	// Preferred is cache-driven affinity: a better choice, not the only one.
	// When the preferred target is at capacity, spill moves elsewhere and
	// accepts a cold cache. Correct for stateless traffic, which is most of it.
	Preferred PinStrength = iota
	// Pinned is a correctness constraint: the conversation carries state that
	// only this target can interpret, so anywhere else is wrong rather than
	// worse. Spill must not apply (DESIGN §7.4a2).
	Pinned
)

// String returns the strength's name.
func (p PinStrength) String() string {
	if p == Pinned {
		return "pinned"
	}
	return "preferred"
}

// Pin binds a request to where its opaque state can be interpreted
// (EXTENSIONS §B, DESIGN §7.4a2, §7.6).
//
// The operative test for whether a field produces a pin is EXTENSIONS §B.1: a
// value is opaque state when its correctness was established somewhere other
// than in dorang and dorang cannot re-derive it. Integrity-protected reasoning
// blocks, server-side response handles and cache partition keys all qualify.
//
// The pin is inferred from the request, never taken from configuration.
// Stickiness configuration expresses a preference about cost; this is not a
// question about cost, so a stateful request pins whatever the setting says.
type Pin struct {
	// Kind names the state that pins, using the EXTENSIONS §B.2 item id —
	// "reasoning.encrypted_content", "previous_response_id",
	// "x-codex-turn-state". It appears verbatim in the refusal, because a
	// caller who has to start a new session deserves to know which field
	// forced it.
	Kind string
	// Strength is Preferred or Pinned. A Preferred pin is a ranking input; a
	// Pinned one is a filter whose failure is terminal.
	Strength PinStrength
	// Family restricts the request to deployments of one protocol/model family.
	// Empty means the pin does not constrain the family.
	Family string
	// Credential restricts the request to one account. Empty means the pin does
	// not constrain the account.
	//
	// This is the level §7.4a2 adds over §7.6's family pin: a server-side
	// response handle does not resolve on a sibling account of the same family,
	// and an integrity-protected reasoning block cannot be validated by an
	// account that did not issue it.
	Credential string
	// Deployment restricts the request to one deployment, for state that is
	// scoped even more narrowly — a backend affinity token, say.
	Deployment string
}

// Request is one routing question.
type Request struct {
	// Model is the client-facing name the caller asked for. It is opaque and is
	// compared whole; nothing splits it (§2.1). Upstream receives the
	// deployment's real model id instead (§7.2).
	Model string
	// Principal is the api key, user or team id. It is the principal capacity
	// axis and the priority class holder.
	Principal string
	// Tenant leads the sticky key so two tenants never share a pin (§7.4a).
	Tenant string
	// Session identifies the conversation for session stickiness. Empty means
	// no pin is created and none is consulted.
	Session string

	// Required is what this request actually uses (§10.1). Its structural bits
	// are a filter: a deployment that cannot express them is removed before
	// ranking, never chosen and silently downgraded.
	Required canonical.Capability
	// AllowLossy is what the caller opted into losing through
	// x-dorang-allow-lossy. Those bits are removed from the filter, so a caller
	// who accepts the loss can reach a backend that would otherwise be excluded.
	AllowLossy canonical.Capability

	// Pins are the opaque-state constraints inferred from the request body and
	// headers. A Pinned entry that cannot be satisfied is a terminal failure.
	Pins []Pin

	// Digests is the request's prefix hash chain (§7.4b), sealed or partial.
	// Empty disables prefix affinity for this request.
	Digests []prefix.Digest

	// PriorityClass names the caller's class on the canonical scale.
	PriorityClass string
	// PriorityHint is a client hint, clamped to the principal's permitted range
	// (§10.5). Nil means the class alone decides.
	PriorityHint *int

	// InputTokens is the estimated prompt size. §10.5a requires the estimate to
	// err pessimistic: an over-estimate costs an unnecessary route to a larger
	// model, an under-estimate costs a hard failure the router cannot see.
	InputTokens int64
	// MaxOutputTokens is the caller's output ceiling, used for the context
	// check and for the cost estimate.
	MaxOutputTokens int64

	// Stream reports that the response will be streamed. It arms the fail-back
	// boundary: once the first byte has reached the client, no further hop is
	// permitted (§7.6).
	Stream bool
	// Batch subjects the request to the interactive reserve on every capacity
	// axis (§11.1).
	Batch bool

	// Previous continues an existing routing session after a failed attempt.
	// Set it to the [Decision] that failed, having first passed that decision
	// to [Router.Report] with the outcome. Nil starts a new session.
	Previous *Decision
}

// Decision is where one attempt is going.
type Decision struct {
	// Deployment, Provider, Credential and UpstreamModel identify the target.
	// UpstreamModel is the REAL model id: upstream always receives it, and the
	// client's requested name goes back in the response body (§7.2).
	Deployment    string
	Provider      string
	Credential    string
	UpstreamModel string

	// Reservation holds the capacity axes this attempt occupies. [Router.Report]
	// releases it; releasing it twice is safe.
	Reservation *capacity.Reservation

	// Reason says why this candidate won: "prefix_hit:depth=3", "sticky",
	// "lowest_cost", "fallback:rate_limit". It is drawn from a table, never
	// formatted, because §15.5 forbids formatted string construction here.
	Reason string
	// Attempt counts from one. Attempt > 1 is a fail-back hop.
	Attempt int
	// Priority is already direction-normalized for the target engine: it is the
	// number that goes on the wire, not the canonical class value. On a
	// descending engine it is the negation of the canonical value, so comparing
	// it against another engine's number is meaningless by construction.
	Priority int
	// Estimate is this attempt's predicted cost. Routing read only
	// Estimate.MarginalNano to get here; the other fields are for accounting.
	Estimate pricing.Cost

	// Group is the model group that was resolved, after aliasing.
	Group string
	// Class is the group's model class, the scope fail-back may delegate within.
	Class string
	// Kind is the provider kind, which selected the priority emit rule.
	Kind string
	// Family is the deployment's protocol/model family.
	Family string
	// PriorityField is the wire field Priority goes in, empty when this engine
	// takes none. PriorityTier is the non-numeric fold (service_tier), empty
	// when this engine has none.
	PriorityField string
	PriorityTier  string
	// PriorityUnverified reports that the engine accepts a priority but the
	// operator has not declared the flag that makes it take effect. Both
	// self-hosted engines return 200 and ignore it by default, and no response
	// says so, so this is the only place the caller can learn it.
	PriorityUnverified bool
	// CanonicalPriority is the class value before direction normalization,
	// kept so a fail-back hop onto a different engine re-normalizes rather than
	// re-negating an already-negated number.
	CanonicalPriority int
	// PrefixDepth is the matched prefix depth, zero when nothing matched.
	PrefixDepth int
	// PinnedTo names the pin that constrained this decision, empty when none did.
	PinnedTo string
	// Stream mirrors Request.Stream, so Report knows whether the first-byte
	// boundary applies.
	Stream bool
	// Dropped names the droppable parameters this deployment cannot apply. They
	// are reported, not fatal: the request still means what it meant (§10.1).
	Dropped canonical.Capability

	interned uint32
	st       *sessionState
}

// sessionState is the part of a routing session that survives a hop. It lives
// behind the decision rather than in a table on the Router, so a session needs
// no id, no eviction and no lock: it is reachable only from the caller holding
// the decision it belongs to.
type sessionState struct {
	started   time.Time
	tried     []string
	digests   []prefix.Digest
	cause     Cause
	failed    bool
	firstByte bool
	reported  bool
	attempts  int
	lastErr   error
	// lastWindow is the context window of the deployment that just failed. It
	// is what same_class_larger compares against: "larger" is only meaningful
	// relative to the one that overflowed.
	lastWindow int
	resetAt    time.Time
	sticky     stickyKey
	stickySet  bool
}

// Outcome is what happened to one attempt. [Router.Report] consumes it.
type Outcome struct {
	// Err is nil on success.
	Err error
	// Status is the upstream HTTP status, zero when there was none.
	Status int
	// Cause classifies the failure. Leave it CauseNone and Report derives it
	// from Status and Err with [Classify]; set it when the caller knows better,
	// which is the normal case for a context-window or content-policy 400,
	// because those are recognised by a body signature the router never sees.
	Cause Cause
	// FirstByteSent reports that at least one byte of this response has already
	// reached the client. It closes fail-back for good: after that point an
	// error event ends the stream, because duplicated output is worse than a
	// visible failure (§7.6).
	FirstByteSent bool
	// TTFT is time to the first byte from upstream; Total is the whole request.
	TTFT  time.Duration
	Total time.Duration
	// InputTokens and OutputTokens are the measured usage.
	InputTokens  int64
	OutputTokens int64
	// RetryAfter is a provider-signalled cooldown. It takes the deployment out
	// of selection for at least that long.
	RetryAfter time.Duration
	// ResetAt is when an exhausted quota window recovers, when the provider
	// said so.
	ResetAt time.Time
}

// OK reports whether the attempt succeeded.
func (o Outcome) OK() bool { return o.Err == nil && o.Cause == CauseNone }

// Classify maps a status and an error onto a fail-back class.
//
// It recognises what can be recognised from a status line and an error value.
// It deliberately does NOT sniff response bodies: a context-window overflow and
// a content-policy refusal are both 400 with a vendor-specific signature, and
// §15.5 forbids regular expressions on this path. The frontend that decoded the
// body sets [Outcome.Cause] for those two.
func Classify(status int, err error) Cause {
	if err != nil {
		if quota.IsTerminal(err) && errors.Is(err, quota.ErrBudgetExceeded) {
			return CauseBudgetExceeded
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return CauseTimeout
		}
	}
	switch {
	case status == 0:
		if err != nil {
			return CauseUpstream5xx
		}
		return CauseNone
	case status == 401:
		return CauseAuth
	case status == 403:
		// 403 is ambiguous on purpose: every vendor uses it for at least two of
		// authentication, permission and content policy. Auth is the safer
		// reading because it has no chain — mis-reading a content refusal as
		// auth costs one unnecessary credential cooldown, while mis-reading auth
		// as content policy sends a broken credential around the whole class.
		return CauseAuth
	case status == 408 || status == 504:
		return CauseTimeout
	case status == 429:
		return CauseRateLimit
	case status >= 500:
		return CauseUpstream5xx
	}
	return CauseNone
}
