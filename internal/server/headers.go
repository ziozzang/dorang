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
	// HeaderServedModel names the model that ACTUALLY answered, and is present
	// only when that is a different model from HeaderUpstreamModel — see
	// [Result.ServedModel]. Its absence is the ordinary case and asserts
	// nothing.
	//
	// It is here because the third thing a silent substitution costs, after the
	// price and the window, is the caller's own contract: a client that asked
	// for one model got another and was told nothing. §7.2 makes that worse
	// rather than better on the way out — the body is REQUIRED to carry the
	// client's name, so dorang overwrites the one field that disagreed. This
	// header is the only place the disagreement survives.
	//
	// A streamed answer does not carry it, for the reason
	// [Result.Downgraded] does not either: the header block closes on the first
	// write, and on a stream the first write is the frame the reading is taken
	// from.
	HeaderServedModel = "X-Dorang-Served-Model"
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
	// HeaderUtilizationMultiplier is the factor a utilization-priced rule
	// applied to this request's rate, as a six-place decimal (DESIGN §8.6).
	//
	// It is in the ALWAYS-ON set, beside the cost it explains, and it is the one
	// addition to that set this feature makes. §10.4 bounds the always-on
	// headers to identification and cost; a factor the charge was multiplied by
	// is not telemetry about the charge, it is a term of it, and a caller who
	// receives x-dorang-cost-usd without it cannot reproduce the number from any
	// rate card. It is emitted whenever the rule declares a factor — including
	// when the factor is 1.000000 — because the whole distinction the feature
	// rests on is between "the backend was idle" and "we did not observe it",
	// and both of those charge 1.000000x.
	HeaderUtilizationMultiplier = "X-Dorang-Utilization-Multiplier"
	// HeaderUtilizationSource names the observation the factor came from, or the
	// refusal that stood in for it: `observed`, `streamed`, `no_load_header`,
	// `zero_is_ambiguous`, `not_a_fraction`, and the rest of
	// internal/loadsignal's vocabulary. Always-on for the same reason: without
	// it a 1.000000x is unattributable.
	HeaderUtilizationSource = "X-Dorang-Utilization-Source"
	// HeaderUtilizationCeiling is the most the factor could have been, from the
	// rule's own max_multiplier. It is what makes the price PREDICTABLE despite
	// moving: a caller cannot know the occupancy in advance but can bound the
	// bill, which is DESIGN §5.6's rule about publishing a maximum as a number
	// rather than as a reassurance.
	HeaderUtilizationCeiling = "X-Dorang-Utilization-Ceiling"
	// HeaderUtilization is the measured occupancy itself, as a fraction. It is
	// behind x-dorang-detail rather than always on, because it is a statement
	// about the OPERATOR's fleet rather than about the caller's request — how
	// full their machines are is not something every tenant needs on every
	// response — while the factor above is a term of that tenant's own bill.
	HeaderUtilization = "X-Dorang-Utilization"
	// The cumulative spend view.
	HeaderSpendUSD           = "X-Dorang-Spend-Usd"
	HeaderBudgetUSD          = "X-Dorang-Budget-Usd"
	HeaderBudgetRemainingUSD = "X-Dorang-Budget-Remaining-Usd"
	// HeaderDroppedParams lists what conversion removed.
	HeaderDroppedParams = "X-Dorang-Dropped-Params"
	// HeaderDowngraded lists the structural constructs this request actually
	// lost, by construct id (DESIGN §10.1).
	//
	// It is the other half of x-dorang-allow-lossy and not a restatement of it.
	// The opt-in names what MAY be lost, once, for every request a client sends;
	// this names what WAS lost, on this one. A caller who sets
	// `x-dorang-allow-lossy: document_block` because one conversation in fifty
	// carries a PDF has consented to the loss and still cannot tell which fifty
	// answers were the degraded ones — and "which requests lost the document" is
	// the whole question the consent was given in order to be able to ask.
	//
	// The value is the construct id list, comma separated, in the same
	// vocabulary and the same spelling x-dorang-allow-lossy accepts, so a client
	// can compare the two directly. The per-instance location
	// ("messages[2].content[1]: application/pdf") is deliberately not here: that
	// detail belongs to the 400 body, which is what a caller who did NOT consent
	// receives, and a header is not a place to put an unbounded list of
	// positions.
	HeaderDowngraded = "X-Dorang-Downgraded"
	// HeaderNativeStopReason carries the backend's own stop reason, which the
	// wire mapping loses (COMPATIBILITY §4.2a).
	HeaderNativeStopReason = "X-Dorang-Native-Stop-Reason"
	// HeaderNativeErrorType carries an upstream error type outside the
	// canonical vocabulary.
	HeaderNativeErrorType = "X-Dorang-Native-Error-Type"
	// HeaderNativeErrorCode carries the upstream's own error code when dorang
	// could not put it in the envelope — a number where §7.1 requires a string,
	// or a JSON object or array where it requires a scalar.
	//
	// The envelope then carries dorang's canonical code and this carries what
	// the backend actually sent, which is the pair COMPATIBILITY §11.3 promises.
	// A backend that sent a plain string code does not set this: that code is
	// already in the envelope, and repeating it would make the header's presence
	// mean nothing.
	HeaderNativeErrorCode = "X-Dorang-Native-Error-Code"
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

// The legacy header spellings dorang mirrors when compat.legacy_headers is on
// (COMPATIBILITY §7.7).
//
// They are the reference proxy's names, and they are here because a cutover
// breaks SILENTLY without them: a dashboard, a cost exporter or a support script
// that reads x-litellm-response-cost does not error when the header stops
// arriving, it reports zero. §0.3's "run alongside, then take over" is not a
// migration anyone can perform if taking over quietly zeroes the numbers.
//
// Only names dorang has a real value for are mirrored. The reference proxy emits
// several more; each one dorang does NOT mirror is listed in §7.7 with the
// reason, because a header emitted with an invented value is worse than an
// absent one — the reader cannot tell it apart from a real measurement. In
// particular there is no x-litellm-version (dorang is not that proxy and would
// have to lie about which one it is) and no x-litellm-model-api-base (the
// upstream's URL is not a tenant's business).
//
// Each mirror is stamped beside the dorang header it copies, so it inherits the
// same §10.4 detail gating: a mirror that arrived when its source did not would
// be a second, disagreeing answer to "what is always on".
const (
	// LegacyHeaderCallID mirrors HeaderRequestID.
	LegacyHeaderCallID = "X-Litellm-Call-Id"
	// LegacyHeaderModelID mirrors HeaderDeployment: the reference proxy's
	// "model id" is the id of the DEPLOYMENT that served the request, which is
	// what dorang calls a deployment. It is deliberately not HeaderUpstreamModel,
	// which is the provider-side model name.
	LegacyHeaderModelID = "X-Litellm-Model-Id"
	// LegacyHeaderResponseCost mirrors HeaderCostUSD, and — unlike it — carries
	// 0 when no price rule matched instead of being omitted.
	//
	// This is the one mirror that is not a straight copy of its source, and the
	// asymmetry is deliberate. x-dorang-cost-usd is absent on an unpriced
	// request because dorang's own vocabulary distinguishes "no price rule
	// matched" from "this request was free". The legacy name has no such
	// distinction — the proxy it belongs to always sends the header — so a
	// cost exporter built against it treats absence as zero anyway, or raises
	// on a missing key. Suppressing the mirror therefore reproduces the exact
	// failure §7.7a exists to prevent, silently, for every model without a
	// price rule.
	//
	// The pair is the discriminator, and COMPATIBILITY §7.7a says so: legacy
	// header present with 0 AND x-dorang-cost-usd absent means "not priced";
	// both present and 0 means "priced, and free".
	//
	// # The one case where it is omitted
	//
	// A streamed answer is priced AFTER its last frame, and the headers went out
	// before its first. The number does not exist when this header is written,
	// and a mirror that claims 0 there says "not priced" under the discriminator
	// above — for every streamed request, which in an agent deployment is all of
	// them. That was worse than the absence it replaced: the absence claimed
	// nothing, the zero claims a measurement. So the mirror is written when the
	// cost is known and when it is knowably absent, and omitted only when it is
	// not yet decidable; the number for a stream travels on §10.4's usage event
	// and in the ledger, joined by x-dorang-request-id.
	LegacyHeaderResponseCost = "X-Litellm-Response-Cost"
	// LegacyHeaderKeySpend mirrors HeaderSpendUSD.
	LegacyHeaderKeySpend = "X-Litellm-Key-Spend"
	// LegacyHeaderKeyMaxBudget mirrors HeaderBudgetUSD.
	LegacyHeaderKeyMaxBudget = "X-Litellm-Key-Max-Budget"
	// LegacyHeaderAttemptedRetries mirrors HeaderAttempt, less one: dorang
	// counts attempts and the reference proxy counts retries, so the first
	// attempt is 1 here and 0 there. Copying the number across unchanged would
	// report one retry for every request that never retried.
	LegacyHeaderAttemptedRetries = "X-Litellm-Attempted-Retries"
	// LegacyHeaderResponseDuration mirrors HeaderLatencyMS.
	LegacyHeaderResponseDuration = "X-Litellm-Response-Duration-Ms"
)

// stampLegacyHeaders mirrors the legacy spellings for the values dorang has.
//
// It is called only when compat.legacy_headers is on, which is off by default:
// these are another vendor's names on dorang's responses, and emitting them
// unasked would make dorang claim to be a proxy it is not.
func stampLegacyHeaders(h http.Header, rq *Request, r *Result, costDeferred bool) {
	h.Set(LegacyHeaderCallID, rq.ID)
	if r.UpstreamModel != "" {
		h.Set(HeaderRealModel, r.UpstreamModel)
	}
	if r.Deployment != "" {
		h.Set(LegacyHeaderModelID, r.Deployment)
	}
	// See LegacyHeaderResponseCost. An unpriced request reports 0 here and
	// nothing on x-dorang-cost-usd, which is what tells the two apart — but only
	// while the 0 is a measurement. On a stream the cost is settled after the
	// last frame and these headers went out before the first, so the mirror is
	// omitted rather than made to claim a number nobody has computed.
	if !costDeferred {
		var cb [32]byte
		cost := int64(0)
		if r.Priced {
			cost = r.CostNanoUSD
		}
		h.Set(LegacyHeaderResponseCost, string(appendNanoUSD(cb[:0], cost)))
	}
	if !rq.Detail {
		return
	}
	if r.Attempt > 0 {
		h.Set(LegacyHeaderAttemptedRetries, strconv.Itoa(r.Attempt-1))
	}
	setMillis(h, LegacyHeaderResponseDuration, r.LatencyNS)
	if r.Priced {
		var b [32]byte
		// The mirror follows the original: a spend nobody looked up is absent
		// here too. A dashboard reading the legacy spelling would otherwise get
		// the flat zero the dorang spelling stopped claiming.
		if r.SpendKnown {
			h.Set(LegacyHeaderKeySpend, string(appendNanoUSD(b[:0], r.SpendNanoUSD)))
		}
		if r.BudgetNanoUSD > 0 {
			h.Set(LegacyHeaderKeyMaxBudget, string(appendNanoUSD(b[:0], r.BudgetNanoUSD)))
		}
	}
}

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
// Retry-After on the statuses [retryAfterStatus] admits, and the rate-limit set.
//
// Both read [Result], and both now have a producer. `internal/app` fills
// [Result.RateLimit] from [router.Decision.Quota] — the chosen credential's own
// view at the enforcement point — at SELECTION time rather than on acceptance,
// so a 502 or a refused hop still tells a client how much of its allowance is
// left. That is when it matters most.
//
// It was not always so, and the shape of the defect is worth keeping: this
// block rendered both families faithfully while nothing anywhere set either, so
// `x-ratelimit-*` had never left this gateway even though COMPATIBILITY §7.8
// cited it as the reason not to mirror LiteLLM's own header and an OPERATIONS
// runbook told an operator to read it off a response. A renderer is not a
// feature; DESIGN §17.1.
//
// costDeferred says that this response will be priced after its headers are on
// the wire, which is every streamed answer: the dispatcher settles once the last
// frame is written (DESIGN §10.4). It is not a guess from the Content-Type — the
// caller computes it from the response it is about to send — and the only thing
// it changes is that a cost header is omitted rather than published as zero.
func (s *Server) stampHeaders(h http.Header, rq *Request, status int, costDeferred bool) {
	cfg := rq.srv.snap.Load()
	r := &rq.Result

	h.Set(HeaderRequestID, rq.ID)
	if rq.Model != "" {
		h.Set(HeaderModel, rq.Model)
	}
	if r.UpstreamModel != "" {
		h.Set(HeaderUpstreamModel, r.UpstreamModel)
	}
	if r.ServedModel != "" {
		h.Set(HeaderServedModel, r.ServedModel)
	}
	if r.Deployment != "" {
		h.Set(HeaderDeployment, r.Deployment)
	}
	if r.Priced {
		var b [32]byte
		h.Set(HeaderCostUSD, string(appendNanoUSD(b[:0], r.CostNanoUSD)))
	}
	// Beside the cost and under the same condition a stream imposes on it: for a
	// streamed answer these headers went out before the price existed, so they
	// are omitted rather than made to claim a factor nobody has computed — the
	// same rule LegacyHeaderResponseCost follows, for the same reason. The
	// number for a stream travels in the ledger, joined by x-dorang-request-id.
	if r.UtilizationPriced && !costDeferred {
		h.Set(HeaderUtilizationMultiplier, ppmDecimal(r.UtilizationMultiplierPPM))
		h.Set(HeaderUtilizationCeiling, ppmDecimal(r.UtilizationCeilingPPM))
		if r.UtilizationSource != "" {
			h.Set(HeaderUtilizationSource, r.UtilizationSource)
		}
	}
	if cfg.legacyHeaders {
		stampLegacyHeaders(h, rq, r, costDeferred)
	}

	if retryAfterStatus(status) && r.RetryAfterSeconds > 0 {
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

	if r.UtilizationPriced && !costDeferred && r.UtilizationSource == utilizationObserved {
		// Only when it was actually observed. Emitting 0.000000 on a fallback
		// would be a measurement of an idle backend that nobody made, which is
		// the single failure this feature is built around.
		h.Set(HeaderUtilization, ppmDecimal(int64(r.UtilizationPPM)))
	}
	if r.NotionalPriced {
		// NotionalPriced, not Priced. They are separate flags because they are
		// separate questions (DESIGN §8.5 rule 5): a request can be billed
		// exactly and still have no list-rate equivalent, which is the normal
		// case on a subscription nobody has written a notional_rate rule for.
		// Emitting 0 there is the one thing §8.5 forbids by name — it makes a
		// subscription look infinitely efficient — and gating this on Priced
		// did exactly that, while internal/app's metric for the same value
		// already honoured the flag. Two answers to one question.
		var b [32]byte
		h.Set(HeaderNotionalUSD, string(appendNanoUSD(b[:0], r.NotionalNanoUSD)))
	}
	if r.Priced {
		var b [32]byte
		// SpendKnown, not Priced. The two are separate questions and this pair
		// answered the wrong one: the budget hold is hydrated from the durable
		// counter by the RESERVATION, and a request whose estimated cost is zero
		// takes no reservation, so on the first zero-rated request after a
		// process start these read a spend of 0 against a full budget for a key
		// that had already spent most of its ceiling. "Not loaded" is not "zero"
		// — the same rule [setInt] applies to a token count and DESIGN §8.5
		// applies to the notional figure, and the streamed cost header answers
		// it the same way: absent rather than a number nobody looked up.
		//
		// The CEILING is still emitted, because that one is known: it comes from
		// the authorization snapshot, not from the counter. What goes with the
		// spend is the REMAINING, which is derived from it.
		if r.SpendKnown {
			h.Set(HeaderSpendUSD, string(appendNanoUSD(b[:0], r.SpendNanoUSD)))
		}
		if r.BudgetNanoUSD > 0 {
			h.Set(HeaderBudgetUSD, string(appendNanoUSD(b[:0], r.BudgetNanoUSD)))
			if r.SpendKnown {
				rem := r.BudgetNanoUSD - r.SpendNanoUSD
				if rem < 0 {
					rem = 0
				}
				h.Set(HeaderBudgetRemainingUSD, string(appendNanoUSD(b[:0], rem)))
			}
		}
	}
	for window, pct := range r.QuotaUsedPct {
		h.Set(quotaPrefix+textproto.CanonicalMIMEHeaderKey(window)+quotaSuffix,
			strconv.Itoa(pct))
	}
	if r.DroppedParams != "" {
		h.Set(HeaderDroppedParams, r.DroppedParams)
	}
	// Beside the drop list rather than in the always-on set, because the two
	// answer one question — "what did dorang do to this request that the request
	// did not ask for" — and two headers of one class that arrive under different
	// conditions is a second, disagreeing answer to when a loss is visible.
	if r.Downgraded != "" {
		h.Set(HeaderDowngraded, r.Downgraded)
	}
	if r.NativeStopReason != "" {
		h.Set(HeaderNativeStopReason, r.NativeStopReason)
	}
	if rq.Body != nil && !rq.Body.Replayable() {
		h.Set(HeaderReplayable, "false")
	}
}

// utilizationObserved is internal/loadsignal's token for a reading that was actually
// taken. It is a literal rather than an import because internal/server imports no
// sibling of internal/app's; the two spellings agreeing is asserted by
// TestUtilizationHeadersMatchTheLoadSignalVocabulary.
const utilizationObserved = "observed"

// ppmDecimal renders a parts-per-million figure as a six-place decimal, allocating one
// small string. A factor is a term of the bill and has to be readable as a number, not
// as 1450000.
func ppmDecimal(ppm int64) string {
	if ppm < 0 {
		ppm = 0
	}
	var b [24]byte
	out := strconv.AppendInt(b[:0], ppm/1_000_000, 10)
	out = append(out, '.')
	frac := ppm % 1_000_000
	for div := int64(100_000); div > 0; div /= 10 {
		out = append(out, byte('0'+(frac/div)%10))
	}
	return string(out)
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
