package router

import (
	"errors"
	"strings"
	"time"
)

// Cause is one fallback class (DESIGN §7.6). Each class has its own chain, and
// two of them have none at all: exceeding a budget and failing authentication
// are not fallback conditions, because no other deployment makes the money
// reappear or the credential valid.
type Cause uint8

const (
	// CauseNone is the zero value: no failure was classified.
	CauseNone Cause = iota
	// CauseRateLimit is a 429 from the upstream.
	CauseRateLimit
	// CauseQuotaExhausted is a local or provider-reported quota refusal.
	CauseQuotaExhausted
	// CauseContextWindow is a pre-computed overflow or a 400 signature.
	CauseContextWindow
	// CauseContentPolicy is a 400/403 content refusal.
	CauseContentPolicy
	// CauseUpstream5xx is any 5xx.
	CauseUpstream5xx
	// CauseTimeout is a deadline.
	CauseTimeout
	// CauseBudgetExceeded is dorang's own budget. It has no chain.
	CauseBudgetExceeded
	// CauseAuth is a 401/403 on the credential. It has no chain, and the
	// credential is marked exhausted.
	CauseAuth
	// CauseBadRequest is a 4xx that names no other class: the REQUEST was
	// refused, and the deployment that refused it answered correctly and
	// promptly. It has no chain — a malformed body is malformed on every
	// backend, so a hop only spends the caller's money to reach the identical
	// refusal — and it is the one failure that counts for NOTHING against
	// availability; see [countsAgainstAvailability].
	//
	// It is not a configuration key. §4.2's fallbacks.on table is the set of
	// conditions an operator can route around, and this is the one condition
	// that must never be routed around, so [ParseCause] refuses it.
	CauseBadRequest

	numCauses
)

var causeNames = [numCauses]string{
	"none", "rate_limit", "quota_exhausted", "context_window",
	"content_policy", "upstream_5xx", "timeout", "budget_exceeded", "auth",
	"bad_request",
}

// configurable reports whether a cause may appear as a fallbacks.on key. The two
// that may not are the zero value and [CauseBadRequest]: neither names a
// condition an operator chooses a chain for.
func (c Cause) configurable() bool { return c != CauseNone && c != CauseBadRequest }

// String returns the configuration spelling of the cause, matching the keys of
// fallbacks.on in DESIGN §4.2.
func (c Cause) String() string {
	if c < numCauses {
		return causeNames[c]
	}
	return "unknown"
}

// ParseCause decodes a configured fallback cause.
func ParseCause(s string) (Cause, bool) {
	for i, n := range causeNames {
		if n == s && Cause(i).configurable() {
			return Cause(i), true
		}
	}
	return CauseNone, false
}

// Chainable reports whether this cause may fall back at all. It is a property
// of the cause, not of configuration: DESIGN §7.6 gives budget_exceeded and
// auth an empty chain, and internal/config refuses a configuration that tries
// to give them one. bad_request joins them for the same kind of reason — the
// next deployment refuses the same body — and it is not a configuration key at
// all, so no configuration can override this.
func (c Cause) Chainable() bool {
	switch c {
	case CauseNone, CauseBudgetExceeded, CauseAuth, CauseBadRequest:
		return false
	}
	return true
}

// Target is where a fallback chain looks next (DESIGN §7.6).
type Target uint8

const (
	// TargetSameGroup is another deployment behind the same client-facing name.
	TargetSameGroup Target = iota
	// TargetSameClass is a different model group of the same class — the
	// delegation to an equivalent model that satisfies R6.
	TargetSameClass
	// TargetSameClassLarger is the same class, restricted to a deployment whose
	// declared context window is strictly larger than the one that overflowed.
	TargetSameClassLarger
)

var targetNames = [...]string{"same_group", "same_class", "same_class_larger"}

// String returns the configuration spelling.
func (t Target) String() string {
	if int(t) < len(targetNames) {
		return targetNames[t]
	}
	return "unknown"
}

// ParseTarget decodes a configured fallback target.
func ParseTarget(s string) (Target, bool) {
	for i, n := range targetNames {
		if n == s {
			return Target(i), true
		}
	}
	return 0, false
}

// Error codes. They are a stable machine-readable vocabulary, not log text: a
// client distinguishes "the pin cannot be honoured" from "everything is busy"
// by the code, never by the message.
const (
	CodeModelNotFound        = "model_not_found"
	CodeUnsupportedConstruct = "unsupported_construct"
	CodeStatePinUnroutable   = "state_pin_unroutable"
	CodeCredentialPinLost    = "credential_pin_unroutable"
	CodeCredentialExhausted  = "credential_pin_exhausted"
	CodeCredentialSaturated  = "credential_pin_saturated"
	// CodeContextWindow and CodeQuotaExhausted are spelled as COMPATIBILITY
	// §11.2's table spells them, not as the fallback cause of §7.6 is spelled.
	// The two vocabularies are separate on purpose: `context_window` and
	// `quota_exhausted` are configuration keys under fallbacks.on, and
	// `context_length_exceeded` and `insufficient_quota` are what a client
	// branches on in the error envelope. §11 opens by describing exactly this
	// failure — a gateway whose error code does not match the contract it
	// publishes — so the code follows the document rather than the config key.
	CodeContextWindow  = "context_length_exceeded"
	CodeQuotaExhausted = "insufficient_quota"
	// CodeNoCapacity and CodeNoCandidate follow §11.2 for the same reason
	// CodeContextWindow and CodeQuotaExhausted do. They used to read
	// `no_capacity` and `no_candidate`, which are dorang's internal words for
	// two conditions §11.2 names `capacity_unavailable` and
	// `no_healthy_deployment` — and a client cannot branch on a vocabulary it
	// was never given. Both answer 429 rather than 503, which §11.2 argues for
	// explicitly: this is back-pressure, and every SDK retries a 429 with
	// backoff while treating a 503 as a dead gateway.
	CodeNoCapacity        = "capacity_unavailable"
	CodeNoCandidate       = "no_healthy_deployment"
	CodeFallbackExhausted = "fallback_exhausted"
	CodeHopsExhausted     = "max_hops_exhausted"
	CodeBudgetElapsed     = "fallback_budget_elapsed"
	CodeStreamCommitted   = "stream_committed"
	CodeNotChainable      = "not_a_fallback_condition"
)

// Error is a routing refusal, carrying everything a caller needs to act on it
// without parsing a sentence.
//
// Terminal is the load-bearing field. It is spelled as a method so that
// [quota.IsTerminal] recognises it through the same interface it recognises a
// budget refusal by: there is one terminality predicate in dorang, not two.
type Error struct {
	// Status is the HTTP status this refusal maps to.
	Status int
	// Code is one of the Code* constants.
	Code string
	// Message says what happened, in words, without naming key material.
	Message string
	// Cause is the fallback class this refusal belongs to, when it has one.
	Cause Cause
	// Constructs names the capability constructs no candidate could express.
	// It is the machine-readable body §10.1 requires of a structural 400.
	Constructs []string
	// Pin names the opaque state that pinned the request, when one did. It is
	// the item id from EXTENSIONS §B.2, e.g. "reasoning.encrypted_content".
	Pin string
	// Family is the protocol or model family the pin required.
	Family string
	// Credential is the pinned account, when a credential pin refused. It is an
	// id, never key material.
	Credential string
	// ResetAt is when an exhausted quota window recovers. Zero when unknown, or
	// when the exhaustion does not recover on its own.
	ResetAt time.Time
	// Attempt is how many attempts had been made when this refusal was raised.
	Attempt int

	// Estimated reports that the size in Message is dorang's own estimate rather
	// than a count anything measured. A caller refused for exceeding a context
	// window is being refused on the strength of a number, and whether that
	// number is a measurement decides what they should do about it.
	Estimated bool
	// EstimateMethod names the rule that produced the size, matching
	// internal/tokenest's method constants. Empty when no size was involved.
	EstimateMethod string

	terminal bool
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("router: ")
	b.WriteString(e.Code)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Terminal reports that this refusal must not be turned into a fallback.
// [quota.IsTerminal] finds it through this method.
func (e *Error) Terminal() bool { return e.terminal }

// Is makes every routing Error match ErrNoRoute, so a caller can separate a
// routing refusal from a transport error without a type switch.
func (e *Error) Is(target error) bool {
	_, ok := target.(*Error)
	return ok
}

// ErrNoRoute matches any routing [Error] under errors.Is.
var ErrNoRoute error = &Error{}

// IsTerminal reports whether err must not be retried on another deployment.
// It is a thin alias over quota.IsTerminal, kept so callers of this package do
// not have to import internal/quota to ask a routing question.
func IsTerminal(err error) bool {
	var t interface{ Terminal() bool }
	return errors.As(err, &t) && t.Terminal()
}
