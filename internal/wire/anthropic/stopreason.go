package anthropic

import "github.com/ziozzang/dorang/internal/canonical"

// stop_reason values.
const (
	StopEndTurn      = "end_turn"
	StopMaxTokens    = "max_tokens"
	StopToolUse      = "tool_use"
	StopStopSequence = "stop_sequence"
	StopRefusal      = "refusal"
	StopPauseTurn    = "pause_turn"
	// StopContextWindowExceeded is a non-standard terminal value observed on a
	// GLM deployment fronting this shape (EXTENSIONS §A.4). COMPATIBILITY §4's
	// table has no row for it; it is mapped here so it does not reach a client
	// raw, and it is reported as a defect rather than silently absorbed.
	StopContextWindowExceeded = "model_context_window_exceeded"
)

// NativeStopReasonHeader carries the true terminal condition when the wire
// value collapsed it (COMPATIBILITY 4.2a, 6.4).
//
// This is the entire mitigation for 6.4: the wire keeps the three values every
// existing client was built against, and the caller who wants to know whether a
// turn was filtered reads one header. Dropping the header turns a lossy-but-
// compatible mapping into a lie.
const NativeStopReasonHeader = "x-dorang-native-stop-reason"

// StopReasonMode selects how much of the neutral enumeration reaches the wire.
type StopReasonMode string

const (
	// StopReasonCollapse is the default and reproduces COMPATIBILITY 6.4: three
	// values, everything else end_turn. It is what the deployed clients expect.
	StopReasonCollapse StopReasonMode = ""
	// StopReasonNative emits this family's own richer enumeration — refusal,
	// stop_sequence, pause_turn — which the vendor's real endpoint sends and
	// the reference proxy does not. Choosing it is a decision, not an accident
	// (the same framing as COMPATIBILITY 2.1).
	StopReasonNative StopReasonMode = "native"
)

// stopTable maps a wire value to the neutral enumeration. It is the decode
// direction and it is NOT the inverse of the collapse: decoding must recover
// everything the wire actually carried, even the values the encoder would have
// collapsed away.
var stopTable = map[string]canonical.StopReason{
	StopEndTurn:      canonical.StopEndTurn,
	StopMaxTokens:    canonical.StopMaxTokens,
	StopToolUse:      canonical.StopToolUse,
	StopStopSequence: canonical.StopStopSequence,
	StopRefusal:      canonical.StopRefusal,
	StopPauseTurn:    canonical.StopPauseTurn,
	// A context overflow is not a normal length stop: the input did not fit, so
	// nothing was generated. Calling it max_tokens would tell the caller their
	// output was truncated, which is the optimistic direction DESIGN §10.5a
	// warns about.
	StopContextWindowExceeded: canonical.StopError,
}

// LookupStopReason resolves a wire stop_reason against the table.
func LookupStopReason(native string) (canonical.StopReason, bool) {
	r, ok := stopTable[native]
	return r, ok
}

// StopReasonOf renders a neutral stop reason on the wire.
//
// COMPATIBILITY 6.4, verbatim: stop→end_turn, length→max_tokens,
// tool_calls→tool_use, EVERYTHING ELSE → end_turn. So a content-filtered turn
// is reported to the client as a normal end. dorang reproduces that for
// compatibility and returns the true reason as the second value, which the
// caller puts in [NativeStopReasonHeader].
//
// native is empty when nothing was lost — when the wire value already says what
// the neutral value said.
func StopReasonOf(r canonical.StopReason, mode StopReasonMode) (wire, native string) {
	if r == canonical.StopUnspecified {
		return "", ""
	}
	if mode == StopReasonNative {
		switch r {
		case canonical.StopEndTurn, canonical.StopMaxTokens, canonical.StopToolUse,
			canonical.StopStopSequence, canonical.StopRefusal, canonical.StopPauseTurn:
			return string(r), ""
		case canonical.StopContentFilter, canonical.StopSafety, canonical.StopRecitation:
			// This family has refusal, which is the nearest true statement: the
			// turn did not end normally.
			return StopRefusal, string(r)
		}
		return StopEndTurn, string(r)
	}
	switch r {
	case canonical.StopEndTurn:
		return StopEndTurn, ""
	case canonical.StopMaxTokens:
		return StopMaxTokens, ""
	case canonical.StopToolUse:
		return StopToolUse, ""
	default:
		// stop_sequence, content_filter, refusal, safety, recitation, error,
		// pause_turn and function_call all land here. The information is not
		// destroyed, it is moved to a header.
		return StopEndTurn, string(r)
	}
}

// StopSequenceValue decides what goes in the stop_sequence field.
//
// COMPATIBILITY 6.5: null on the adapter path. It is a pointer so that "null"
// is what renders — the field is always present (see [Response]).
// [StopReasonNative] restores the real value when the turn actually ended on a
// stop sequence and the caller opted out of the collapse.
func StopSequenceValue(r canonical.StopReason, matched string, mode StopReasonMode) *string {
	if mode == StopReasonNative && r == canonical.StopStopSequence && matched != "" {
		return &matched
	}
	return nil
}
