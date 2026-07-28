package auth

import (
	"errors"
	"fmt"
	"net/http"
)

// Reason is why a credential was refused. It is the machine-readable half of
// an [Error] and maps to the "code" field of the error envelope
// (COMPATIBILITY §7.1), which is a string.
type Reason uint8

// The refusal reasons. Every one of them is a refusal: there is no reason
// value that means "allowed".
const (
	// ReasonNone is the zero value and is never carried by a returned error.
	ReasonNone Reason = iota
	// ReasonMissingCredential: no accepted header carried a credential.
	ReasonMissingCredential
	// ReasonMalformed: the credential does not start with KeyPrefix. Checked
	// before any lookup so a stored digest cannot be replayed (R1-A).
	ReasonMalformed
	// ReasonUnknownKey: no row has this index key.
	ReasonUnknownKey
	// ReasonDigestMismatch: the row exists but the credential does not verify.
	ReasonDigestMismatch
	// ReasonSchemeUnsupported: the row carries an unknown hash scheme.
	ReasonSchemeUnsupported
	// ReasonLegacyDisabled: a legacy_sha256 row with auth.legacy.enabled false.
	ReasonLegacyDisabled
	// ReasonLegacyWindowClosed: a legacy_sha256 row after auth.legacy.until.
	ReasonLegacyWindowClosed
	// ReasonExpired: the key, user or team has passed its expiry. An expired
	// credential is refused and never resurrected (R1-A).
	ReasonExpired
	// ReasonBlocked: the key, user or team is blocked.
	ReasonBlocked
	// ReasonModelNotAllowed: the model is outside the allow-list.
	ReasonModelNotAllowed
	// ReasonRouteNotAllowed: the route is outside the allow-list.
	ReasonRouteNotAllowed
	// ReasonBudgetExceeded: recorded spend has reached the ceiling. Not a
	// fallback condition — failing is the correct outcome (DESIGN §6.4).
	ReasonBudgetExceeded
	// ReasonRateLimited: an observed rate has reached the key's rpm/tpm limit.
	ReasonRateLimited
	// ReasonNoPrincipal: the key carries no owning user or team where one is
	// required.
	ReasonNoPrincipal
	// ReasonUnavailable: the store could not be consulted for an unknown key.
	ReasonUnavailable
	// ReasonPended: the key is pended by the token guard (DESIGN §11.6).
	//
	// It is a distinct, documented refusal and not a second spelling of
	// "blocked", because the two are different judgements that fail
	// differently. Blocked is a decision an operator made; pended is a
	// statistical judgement that might be wrong, and it is released in one
	// action without reissuing a credential. A caller who cannot tell them
	// apart cannot tell an outage from a policy, and a support ticket that says
	// "blocked" sends the operator to the wrong screen.
	ReasonPended
	// ReasonSecretRetired: the key is fine and this SECRET is not — its
	// rotation grace period ended, or an operator cut it short (§11.2c).
	//
	// Distinct from ReasonExpired for the reason that makes rotation worth
	// having: the fix is "use the secret you were issued", not "ask for a new
	// key". Reporting a retired secret as an expired key sends a caller to
	// re-provisioning, which is the thing rotation exists to avoid.
	ReasonSecretRetired
)

// String returns the wire code for the reason.
//
// The first five collapse onto COMPATIBILITY §11.2's `invalid_api_key`, which is
// what OpenAI, Anthropic and the reference proxy all put in `code` for "this
// credential is not usable". §11.2 has one row for "Missing or malformed
// credential" and one for "Expired or revoked credential" and both name that
// code, so five distinct spellings here were five ways for a client's
// `code == "invalid_api_key"` branch to miss — the exact defect §11 opens by
// describing.
//
// What is NOT collapsed: a refusal whose fix is different from "fix your key".
// `secret_retired` sends the caller to the secret they were issued by the last
// rotation rather than to re-provisioning; `credential_pended` and `key_blocked`
// are a statistical judgement and an operator decision, and
// telling them apart is what decides whether a support ticket goes to the
// security screen or the release notes. Those keep their own codes, and §11.2
// lists them as dorang's own rows rather than pretending they are one of
// OpenAI's.
func (r Reason) String() string {
	switch r {
	case ReasonMissingCredential:
		return "invalid_api_key"
	case ReasonMalformed:
		return "invalid_api_key"
	case ReasonUnknownKey:
		return "invalid_api_key"
	case ReasonDigestMismatch:
		return "invalid_api_key"
	case ReasonSchemeUnsupported:
		return "unsupported_hash_scheme"
	case ReasonLegacyDisabled:
		return "legacy_scheme_disabled"
	case ReasonLegacyWindowClosed:
		return "legacy_window_closed"
	case ReasonExpired:
		return "invalid_api_key"
	case ReasonBlocked:
		// COMPATIBILITY §11.2's "Key blocked" row spells it this way.
		return "key_blocked"
	case ReasonModelNotAllowed:
		return "model_not_allowed"
	case ReasonRouteNotAllowed:
		return "route_not_allowed"
	case ReasonBudgetExceeded:
		return "budget_exceeded"
	case ReasonRateLimited:
		return "rate_limit_exceeded"
	case ReasonNoPrincipal:
		return "no_principal"
	case ReasonUnavailable:
		return "auth_unavailable"
	case ReasonPended:
		return "credential_pended"
	case ReasonSecretRetired:
		return "secret_retired"
	}
	return "unknown"
}

// message is the human half. It never mentions a credential, and no code path
// interpolates one into it.
func (r Reason) message() string {
	switch r {
	case ReasonMissingCredential:
		return "no API key supplied"
	case ReasonMalformed:
		return "API key is not in the expected format"
	case ReasonUnknownKey, ReasonDigestMismatch:
		return "invalid API key"
	case ReasonSchemeUnsupported:
		return "stored credential uses an unsupported hash scheme"
	case ReasonLegacyDisabled:
		return "stored credential uses the legacy hash scheme, which is disabled"
	case ReasonLegacyWindowClosed:
		return "stored credential uses the legacy hash scheme and the import window has closed"
	case ReasonExpired:
		return "API key has expired"
	case ReasonBlocked:
		return "API key is blocked"
	case ReasonModelNotAllowed:
		return "API key is not allowed to use this model"
	case ReasonRouteNotAllowed:
		return "API key is not allowed to use this route"
	case ReasonBudgetExceeded:
		return "budget has been exceeded"
	case ReasonRateLimited:
		return "rate limit exceeded"
	case ReasonNoPrincipal:
		return "API key has no owning user or team"
	case ReasonUnavailable:
		return "authentication is temporarily unavailable"
	case ReasonPended:
		return "API key is pended pending review and can be released by an operator"
	case ReasonSecretRetired:
		return "this API key secret has been retired; use the secret issued by the most recent rotation"
	}
	return "authentication failed"
}

// Error is a refusal. It carries a machine-readable reason, the subject the
// refusal came from (key, user or team — the most restrictive wins, so which
// one refused is worth reporting), and a detail that is safe to show.
//
// An Error never contains a credential. Nothing in this package interpolates a
// token, a pepper or a master key into one, and a test asserts it.
type Error struct {
	// Reason is why the request was refused.
	Reason Reason
	// Subject is "key", "user", "team" or "" for refusals that precede any row.
	Subject string
	// Detail is optional extra context, never a secret.
	Detail string
}

// Error implements error.
func (e *Error) Error() string {
	msg := e.Reason.message()
	switch {
	case e.Subject != "" && e.Detail != "":
		return fmt.Sprintf("auth: %s (%s: %s)", msg, e.Subject, e.Detail)
	case e.Subject != "":
		return fmt.Sprintf("auth: %s (%s)", msg, e.Subject)
	case e.Detail != "":
		return fmt.Sprintf("auth: %s (%s)", msg, e.Detail)
	}
	return "auth: " + msg
}

// Is makes every error with the same reason match the exported sentinel for
// that reason, so callers can write errors.Is(err, auth.ErrExpired).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Reason == e.Reason
}

// Code returns the string used in the error envelope's "code" field.
func (e *Error) Code() string { return e.Reason.String() }

// Status maps a refusal to an HTTP status.
//
// Authentication failures are 401. Policy refusals against an authenticated
// key — blocked, model, route — are 403. A rate limit is 429. A budget refusal
// is 400 and terminal: it is deliberately not 429, because 429 is a fallback
// condition in this gateway and exceeding a budget is not (DESIGN §6.4).
func (e *Error) Status() int {
	switch e.Reason {
	case ReasonBlocked, ReasonModelNotAllowed, ReasonRouteNotAllowed, ReasonNoPrincipal:
		return http.StatusForbidden
	case ReasonPended:
		// 403, with the key authenticated: the credential is genuine and the
		// policy refused it. A 401 would tell a caller to check their key,
		// which is the one thing that will not help.
		return http.StatusForbidden
	case ReasonRateLimited:
		return http.StatusTooManyRequests
	case ReasonBudgetExceeded:
		return http.StatusBadRequest
	case ReasonUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusUnauthorized
}

// Terminal reports whether the refusal must not be retried against another
// deployment. Budget refusals are terminal (DESIGN §6.4, fallbacks.on
// budget_exceeded: []); so is every authentication failure.
func (e *Error) Terminal() bool { return true }

// Sentinels for errors.Is. Each matches any Error carrying the same reason.
var (
	ErrMissingCredential  = &Error{Reason: ReasonMissingCredential}
	ErrMalformed          = &Error{Reason: ReasonMalformed}
	ErrUnknownKey         = &Error{Reason: ReasonUnknownKey}
	ErrDigestMismatch     = &Error{Reason: ReasonDigestMismatch}
	ErrSchemeUnsupported  = &Error{Reason: ReasonSchemeUnsupported}
	ErrLegacyDisabled     = &Error{Reason: ReasonLegacyDisabled}
	ErrLegacyWindowClosed = &Error{Reason: ReasonLegacyWindowClosed}
	ErrExpired            = &Error{Reason: ReasonExpired}
	ErrBlocked            = &Error{Reason: ReasonBlocked}
	ErrModelNotAllowed    = &Error{Reason: ReasonModelNotAllowed}
	ErrRouteNotAllowed    = &Error{Reason: ReasonRouteNotAllowed}
	ErrBudgetExceeded     = &Error{Reason: ReasonBudgetExceeded}
	ErrRateLimited        = &Error{Reason: ReasonRateLimited}
	ErrNoPrincipal        = &Error{Reason: ReasonNoPrincipal}
	ErrUnavailable        = &Error{Reason: ReasonUnavailable}
	ErrPended             = &Error{Reason: ReasonPended}
	ErrSecretRetired      = &Error{Reason: ReasonSecretRetired}
)

// ReasonOf extracts the reason from an error, or ReasonNone.
func ReasonOf(err error) Reason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ReasonNone
}

// refuse builds a refusal. It is the only constructor used inside the package,
// which is what makes "no error carries a credential" checkable by reading one
// function's callers.
func refuse(r Reason, subject, detail string) *Error {
	return &Error{Reason: r, Subject: subject, Detail: detail}
}
