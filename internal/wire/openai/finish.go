package openai

import "github.com/ziozzang/dorang/internal/canonical"

// OpenAI finish_reason values. This enumeration is closed: nothing else may
// reach a client (COMPATIBILITY 4.2).
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
	FinishFunctionCall  = "function_call"
)

// Warning is a structured compatibility warning.
//
// It is a struct rather than a format string because DESIGN §15.5 bars
// formatted string construction on the hot path, and an unmapped finish reason
// is observed once per response on a misconfigured backend — often enough that
// building a message for a warning nobody has subscribed to is real cost.
type Warning struct {
	// Code is a stable machine id.
	Code string
	// Detail is the offending value, verbatim.
	Detail string
}

// Warning codes.
const (
	WarnUnmappedFinishReason = "unmapped_finish_reason"
	WarnSynthesizedTerminal  = "synthesized_terminal_chunk"
	WarnLateUsage            = "late_usage_after_finish"
	WarnToolNameTruncated    = "tool_name_truncated"
	WarnNumericErrorCode     = "numeric_error_code"
)

// WarnFunc receives a compatibility warning. It must not block and must not
// retain the argument. A nil WarnFunc is valid and means "discard".
type WarnFunc func(Warning)

func (w WarnFunc) warn(code, detail string) {
	if w != nil {
		w(Warning{Code: code, Detail: detail})
	}
}

// finishTable maps a backend-native terminal value to dorang's richer neutral
// enumeration (COMPATIBILITY 4.1). Mapping to the neutral value rather than
// straight to an OpenAI string is deliberate: an Anthropic-shaped frontend must
// be able to see that a turn ended in "refusal" rather than only that OpenAI
// would have called it content_filter.
//
// The table is case-sensitive and lists both spellings where providers differ,
// because case-insensitive matching would fold distinct provider values
// together and there is no evidence any provider relies on that.
var finishTable = map[string]canonical.StopReason{
	// OpenAI's own values, so a pass-through backend is a table hit and not a
	// warning.
	"stop":           canonical.StopEndTurn,
	"length":         canonical.StopMaxTokens,
	"tool_calls":     canonical.StopToolUse,
	"content_filter": canonical.StopContentFilter,
	"function_call":  canonical.StopFunctionCall,

	// Anthropic.
	"end_turn":      canonical.StopEndTurn,
	"stop_sequence": canonical.StopStopSequence,
	"max_tokens":    canonical.StopMaxTokens,
	"tool_use":      canonical.StopToolUse,
	"refusal":       canonical.StopRefusal,
	"pause_turn":    canonical.StopPauseTurn,

	// Cohere.
	"COMPLETE":      canonical.StopEndTurn,
	"ERROR":         canonical.StopError,
	"ERROR_TOXIC":   canonical.StopContentFilter,
	"ERROR_LIMIT":   canonical.StopMaxTokens,
	"MAX_TOKENS":    canonical.StopMaxTokens,
	"STOP_SEQUENCE": canonical.StopStopSequence,
	"TOOL_CALL":     canonical.StopToolUse,

	// Local runtimes.
	"eos_token":     canonical.StopEndTurn, // pragma: allowlist secret — a finish_reason value, not a credential
	"eos":           canonical.StopEndTurn,
	"length_capped": canonical.StopMaxTokens,

	// Gemini / Vertex.
	"STOP":                      canonical.StopEndTurn,
	"SAFETY":                    canonical.StopSafety,
	"RECITATION":                canonical.StopRecitation,
	"BLOCKLIST":                 canonical.StopContentFilter,
	"PROHIBITED_CONTENT":        canonical.StopContentFilter,
	"SPII":                      canonical.StopContentFilter,
	"IMAGE_SAFETY":              canonical.StopSafety,
	"MALFORMED_FUNCTION_CALL":   canonical.StopError,
	"OTHER":                     canonical.StopEndTurn,
	"LANGUAGE":                  canonical.StopContentFilter,
	"FINISH_REASON_UNSPECIFIED": canonical.StopUnspecified,

	// Gateways and guardrails.
	"network_error":        canonical.StopError,
	"sensitive":            canonical.StopContentFilter,
	"guardrail_intervened": canonical.StopContentFilter,
}

// LookupStopReason resolves a backend-native terminal value against the table.
func LookupStopReason(native string) (canonical.StopReason, bool) {
	r, ok := finishTable[native]
	return r, ok
}

// FinishReasonOf renders a neutral stop reason as an OpenAI finish_reason.
//
// This is where COMPATIBILITY 4.1's richer values collapse into the closed
// five. The collapse is lossy by construction — that is what
// canonical.CapRichStopReasons reports — and the native value is preserved
// alongside (4.3) so nothing is actually lost.
func FinishReasonOf(r canonical.StopReason) string {
	switch r {
	case canonical.StopMaxTokens:
		return FinishLength
	case canonical.StopToolUse:
		return FinishToolCalls
	case canonical.StopFunctionCall:
		return FinishFunctionCall
	case canonical.StopContentFilter, canonical.StopRefusal, canonical.StopSafety,
		canonical.StopRecitation:
		return FinishContentFilter
	default:
		// StopEndTurn, StopStopSequence, StopError, StopPauseTurn and anything
		// unset. "stop" is the only honest answer OpenAI has for a turn that
		// ended for a reason it cannot name.
		return FinishStop
	}
}

// StopReasonOf is the inverse used when decoding an OpenAI-shaped response.
func StopReasonOf(finish string) canonical.StopReason {
	if r, ok := finishTable[finish]; ok {
		return r
	}
	return canonical.StopEndTurn
}

// NormalizeFinishReason maps a backend-native value onto the OpenAI enumeration.
//
// COMPATIBILITY 4.2: an unmapped value becomes "stop" and warns. It is NEVER
// passed through raw — a client that switches on finish_reason has no case for
// "ERROR_TOXIC" and the ones that do not crash silently treat it as an
// unfinished stream.
//
// The second return is the native value to preserve out of band (4.3), empty
// when the native value already equalled the normalized one and there is
// therefore nothing to preserve.
func NormalizeFinishReason(native string, warn WarnFunc) (finish, preserve string) {
	if native == "" {
		return "", ""
	}
	r, ok := LookupStopReason(native)
	if !ok {
		warn.warn(WarnUnmappedFinishReason, native)
		return FinishStop, native
	}
	finish = FinishReasonOf(r)
	if finish == native {
		return finish, ""
	}
	return finish, native
}

// nativeFinishKey is where the preserved value hangs (COMPATIBILITY 4.3).
const nativeFinishKey = "native_finish_reason"
