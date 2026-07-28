package server

import (
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

// The six authentication header names dorang accepts (COMPATIBILITY §7.3).
//
// Any one of them authenticates. All six are stripped before anything is
// forwarded upstream, whichever one was used and whether or not it was valid —
// a client credential that reaches a provider is a credential leak even when it
// is the wrong credential.
const (
	// HeaderAuthorization carries "Bearer <token>".
	HeaderAuthorization = "Authorization"
	// HeaderAPIKey is the plain proxy-style header.
	HeaderAPIKey = "API-Key"
	// HeaderXAPIKey is the Anthropic-style header.
	HeaderXAPIKey = "X-Api-Key"
	// HeaderXGoogAPIKey is the Google-style header.
	HeaderXGoogAPIKey = "X-Goog-Api-Key"
	// HeaderAzureAPIKey is the Azure API Management style header.
	HeaderAzureAPIKey = "Ocp-Apim-Subscription-Key" // pragma: allowlist secret — a header name
	// HeaderDorangAPIKey is dorang's own proxy-specific header.
	HeaderDorangAPIKey = "X-Dorang-Api-Key" // pragma: allowlist secret — a header name
)

// authHeaders is the six, in canonical MIME form so that deleting them from a
// server-parsed header map is a plain map delete.
var authHeaders = [6]string{
	HeaderAuthorization,
	HeaderAPIKey,
	HeaderXAPIKey,
	HeaderXGoogAPIKey,
	HeaderAzureAPIKey,
	HeaderDorangAPIKey,
}

// AuthHeaders returns the accepted authentication header names.
func AuthHeaders() []string {
	out := make([]string, len(authHeaders))
	copy(out, authHeaders[:])
	return out
}

// StripAuthHeaders removes every accepted authentication header from h.
//
// The canonical names cover a header map parsed off the wire. The second pass
// covers a map assembled in code, which may hold non-canonical spellings that a
// map delete would miss — a forwarded credential is not a mistake worth making
// cheaply recoverable.
func StripAuthHeaders(h http.Header) {
	if len(h) == 0 {
		return
	}
	for _, name := range authHeaders {
		delete(h, name)
	}
	for k := range h {
		for _, name := range authHeaders {
			if strings.EqualFold(k, name) {
				delete(h, k)
				break
			}
		}
	}
}

// dorang's own headers.
const (
	// HeaderRequestID is the join key for logs and the ledger, and is always
	// present on every response.
	HeaderRequestID = "X-Dorang-Request-Id"
	// HeaderModel is the name the client asked for.
	HeaderModel = "X-Dorang-Model"
	// HeaderUpstreamModel is the real model id.
	HeaderUpstreamModel = "X-Dorang-Upstream-Model"
	// HeaderRealModel is the legacy spelling of the same value, mirrored only
	// when legacy headers are on (COMPATIBILITY §7.7).
	HeaderRealModel = "X-Dorang-Real-Model"
	// HeaderProvider, HeaderCredential and HeaderDeployment identify the
	// selected target by id. Never a secret.
	HeaderProvider   = "X-Dorang-Provider"
	HeaderCredential = "X-Dorang-Credential"
	HeaderDeployment = "X-Dorang-Deployment"
	// HeaderAttempt, HeaderFallbackFrom and HeaderRouteReason describe the
	// routing decision.
	HeaderAttempt      = "X-Dorang-Attempt"
	HeaderFallbackFrom = "X-Dorang-Fallback-From"
	HeaderRouteReason  = "X-Dorang-Route-Reason"
	// The latency breakdown.
	HeaderQueueMS   = "X-Dorang-Queue-Ms"
	HeaderTTFTMS    = "X-Dorang-Ttft-Ms"
	HeaderLatencyMS = "X-Dorang-Latency-Ms"
	// The token counters.
	HeaderTokensInput      = "X-Dorang-Tokens-Input"
	HeaderTokensOutput     = "X-Dorang-Tokens-Output"
	HeaderTokensCacheRead  = "X-Dorang-Tokens-Cache-Read"
	HeaderTokensCacheWrite = "X-Dorang-Tokens-Cache-Write"
	HeaderTokensReasoning  = "X-Dorang-Tokens-Reasoning"
	// HeaderCostUSD is this request's cost.
	HeaderCostUSD = "X-Dorang-Cost-Usd"
	// HeaderNotionalUSD is the list-rate equivalent (DESIGN §8.5) — an
	// estimate, never billed.
	HeaderNotionalUSD = "X-Dorang-Notional-Usd"
	// The cumulative spend view.
	HeaderSpendUSD           = "X-Dorang-Spend-Usd"
	HeaderBudgetUSD          = "X-Dorang-Budget-Usd"
	HeaderBudgetRemainingUSD = "X-Dorang-Budget-Remaining-Usd"
	// HeaderDroppedParams lists what conversion removed.
	HeaderDroppedParams = "X-Dorang-Dropped-Params"
	// HeaderNativeStopReason carries the backend's own stop reason, which the
	// wire mapping loses (COMPATIBILITY §4.2a).
	HeaderNativeStopReason = "X-Dorang-Native-Stop-Reason"
	// HeaderNativeErrorType carries an upstream error type outside the
	// canonical vocabulary.
	HeaderNativeErrorType = "X-Dorang-Native-Error-Type"
	// HeaderReplayable reports whether the body was retained for a fallback
	// hop (DESIGN §15.4).
	HeaderReplayable = "X-Dorang-Replayable"
	// HeaderUnimplemented names the path a 501 refused, so a client can log
	// what it asked for without parsing the message.
	HeaderUnimplemented = "X-Dorang-Unimplemented"

	// HeaderDetail is the request header that asks for the full set.
	HeaderDetail = "X-Dorang-Detail"
	// HeaderUsageEvents is the request header that opts in to post-hoc
	// streaming values.
	HeaderUsageEvents = "X-Dorang-Usage-Events"

	// quotaPrefix is the per-window credential quota header prefix; the
	// window name is appended, e.g. x-dorang-quota-minute-used-pct.
	quotaPrefix = "X-Dorang-Quota-"
	quotaSuffix = "-Used-Pct"
)

// The standard-form rate-limit headers.
const (
	HeaderRateLimitLimitRequests     = "X-Ratelimit-Limit-Requests"
	HeaderRateLimitRemainingRequests = "X-Ratelimit-Remaining-Requests"
	HeaderRateLimitResetRequests     = "X-Ratelimit-Reset-Requests"
	HeaderRateLimitLimitTokens       = "X-Ratelimit-Limit-Tokens"
	HeaderRateLimitRemainingTokens   = "X-Ratelimit-Remaining-Tokens"
	HeaderRateLimitResetTokens       = "X-Ratelimit-Reset-Tokens"
	HeaderRetryAfter                 = "Retry-After"
)

// InboundRequestIDHeaders are the request headers honored as the request id
// when a client sets one (COMPATIBILITY §7.8). The first non-empty one wins.
var InboundRequestIDHeaders = []string{
	HeaderRequestID,
	"X-Request-Id",
	"X-Correlation-Id",
}

// stampHeaders attaches the extension headers, once, just before the status
// line goes out.
//
// The always-on set is bounded to identification and cost (DESIGN §10.4).
// Revision 1 attached roughly thirty headers to every response, which risks
// intermediary header-size limits and puts bytes ahead of the first streamed
// byte — against the very latency target the headers were serving [R1-C9]. The
// rest arrive only when the caller asks with x-dorang-detail: full or the
// deployment sets observability.always_full_headers.
//
// Two headers are attached regardless of the detail flag because they are
// standard HTTP a client acts on rather than dorang telemetry it merely reads:
// Retry-After on a 429, and the rate-limit set when the dispatcher populated it.
func (s *Server) stampHeaders(h http.Header, rq *Request, status int) {
	cfg := rq.srv.snap.Load()
	r := &rq.Result

	h.Set(HeaderRequestID, rq.ID)
	if rq.Model != "" {
		h.Set(HeaderModel, rq.Model)
	}
	if r.UpstreamModel != "" {
		h.Set(HeaderUpstreamModel, r.UpstreamModel)
		if cfg.legacyHeaders {
			h.Set(HeaderRealModel, r.UpstreamModel)
		}
	}
	if r.Deployment != "" {
		h.Set(HeaderDeployment, r.Deployment)
	}
	if r.Priced {
		var b [32]byte
		h.Set(HeaderCostUSD, string(appendNanoUSD(b[:0], r.CostNanoUSD)))
	}

	if status == http.StatusTooManyRequests && r.RetryAfterSeconds > 0 {
		h.Set(HeaderRetryAfter, strconv.Itoa(r.RetryAfterSeconds))
	}
	if r.RateLimit.Set {
		rl := &r.RateLimit
		setInt(h, HeaderRateLimitLimitRequests, rl.LimitRequests)
		setInt(h, HeaderRateLimitRemainingRequests, rl.RemainingRequests)
		if rl.ResetRequests != "" {
			h.Set(HeaderRateLimitResetRequests, rl.ResetRequests)
		}
		setInt(h, HeaderRateLimitLimitTokens, rl.LimitTokens)
		setInt(h, HeaderRateLimitRemainingTokens, rl.RemainingTokens)
		if rl.ResetTokens != "" {
			h.Set(HeaderRateLimitResetTokens, rl.ResetTokens)
		}
	}

	if !rq.Detail {
		return
	}

	if r.Provider != "" {
		h.Set(HeaderProvider, r.Provider)
	}
	if r.Credential != "" {
		h.Set(HeaderCredential, r.Credential)
	}
	if r.Attempt > 0 {
		h.Set(HeaderAttempt, strconv.Itoa(r.Attempt))
	}
	if r.FallbackFrom != "" {
		h.Set(HeaderFallbackFrom, r.FallbackFrom)
	}
	if r.RouteReason != "" {
		h.Set(HeaderRouteReason, r.RouteReason)
	}
	if r.QueueNS > 0 {
		setMillis(h, HeaderQueueMS, r.QueueNS)
	}
	if r.TTFTNS > 0 {
		setMillis(h, HeaderTTFTMS, r.TTFTNS)
	}
	setMillis(h, HeaderLatencyMS, r.LatencyNS)

	u := &r.Tokens
	setInt(h, HeaderTokensInput, u.Input)
	setInt(h, HeaderTokensOutput, u.Output)
	setInt(h, HeaderTokensCacheRead, u.CacheRead)
	setInt(h, HeaderTokensCacheWrite, u.CacheWrite)
	setInt(h, HeaderTokensReasoning, u.Reasoning)

	if r.Priced {
		var b [32]byte
		h.Set(HeaderNotionalUSD, string(appendNanoUSD(b[:0], r.NotionalNanoUSD)))
		h.Set(HeaderSpendUSD, string(appendNanoUSD(b[:0], r.SpendNanoUSD)))
		if r.BudgetNanoUSD > 0 {
			h.Set(HeaderBudgetUSD, string(appendNanoUSD(b[:0], r.BudgetNanoUSD)))
			rem := r.BudgetNanoUSD - r.SpendNanoUSD
			if rem < 0 {
				rem = 0
			}
			h.Set(HeaderBudgetRemainingUSD, string(appendNanoUSD(b[:0], rem)))
		}
	}
	for window, pct := range r.QuotaUsedPct {
		h.Set(quotaPrefix+textproto.CanonicalMIMEHeaderKey(window)+quotaSuffix,
			strconv.Itoa(pct))
	}
	if r.DroppedParams != "" {
		h.Set(HeaderDroppedParams, r.DroppedParams)
	}
	if r.NativeStopReason != "" {
		h.Set(HeaderNativeStopReason, r.NativeStopReason)
	}
	if rq.Body != nil && !rq.Body.Replayable() {
		h.Set(HeaderReplayable, "false")
	}
}

// setInt sets a header to a non-zero integer, and omits it at zero. An absent
// counter and a counter of zero are different claims; only one of them is safe
// to make without evidence.
func setInt(h http.Header, name string, v int64) {
	if v == 0 {
		return
	}
	var b [24]byte
	h.Set(name, string(appendInt(b[:0], v)))
}

// setMillis renders a nanosecond duration as whole milliseconds.
func setMillis(h http.Header, name string, ns int64) {
	if ns <= 0 {
		return
	}
	var b [24]byte
	h.Set(name, string(appendInt(b[:0], ns/1e6)))
}

// authHeaderUsed reports which of the six accepted header names carried a
// credential, in the order they are consulted.
//
// It is recorded for diagnostics, not for the decision: any one of them
// authenticates (COMPATIBILITY §7.3), and which one it was matters only when a
// client is misconfigured — an Anthropic SDK pointed at an OpenAI-shaped base
// URL sends x-api-key, works, and leaves no other trace of why the deployment's
// Authorization-based key rotation appears to be doing nothing.
func authHeaderUsed(h http.Header) string {
	for _, name := range authHeaders {
		if h.Get(name) != "" {
			return name
		}
	}
	return ""
}
