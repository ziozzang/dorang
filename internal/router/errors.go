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

	numCauses
)

var causeNames = [numCauses]string{
	"none", "rate_limit", "quota_exhausted", "context_window",
	"content_policy", "upstream_5xx", "timeout", "budget_exceeded", "auth",
}

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
		if n == s && Cause(i) != CauseNone {
			return Cause(i), true
		}
	}
	return CauseNone, false
}

// Chainable reports whether this cause may fall back at all. It is a property
// of the cause, not of configuration: DESIGN §7.6 gives budget_exceeded and
// auth an empty chain, and internal/config refuses a configuration that tries
// to give them one.
func (c Cause) Chainable() bool {
	return c != CauseBudgetExceeded && c != CauseAuth && c != CauseNone
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
	CodeContextWindow        = "context_window_exceeded"
	CodeQuotaExhausted       = "quota_exhausted"
	CodeNoCapacity           = "no_capacity"
	CodeNoCandidate          = "no_candidate"
	CodeFallbackExhausted    = "fallback_exhausted"
	CodeHopsExhausted        = "max_hops_exhausted"
	CodeBudgetElapsed        = "fallback_budget_elapsed"
	CodeStreamCommitted      = "stream_committed"
	CodeNotChainable         = "not_a_fallback_condition"
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
