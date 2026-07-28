package canonical

import "encoding/json"

// StopReason is why generation ended, in the richer of the two enumerations.
//
// It is deliberately larger than the OpenAI finish_reason set, because
// collapsing "the model refused" and "the model finished" into one value is
// exactly the loss [CapRichStopReasons] exists to report (DESIGN §10.1).
type StopReason string

const (
	StopUnspecified   StopReason = ""
	StopEndTurn       StopReason = "end_turn"
	StopMaxTokens     StopReason = "max_tokens"
	StopToolUse       StopReason = "tool_use"
	StopStopSequence  StopReason = "stop_sequence"
	StopContentFilter StopReason = "content_filter"
	StopRefusal       StopReason = "refusal"
	StopSafety        StopReason = "safety"
	StopRecitation    StopReason = "recitation"
	StopError         StopReason = "error"
	StopFunctionCall  StopReason = "function_call"
	StopPauseTurn     StopReason = "pause_turn"
)

// Expressible reports whether the OpenAI finish_reason enumeration can carry
// this reason without collapsing it onto a different meaning.
func (s StopReason) Expressible() bool {
	switch s {
	case StopUnspecified, StopEndTurn, StopMaxTokens, StopToolUse, StopContentFilter, StopFunctionCall:
		return true
	}
	return false
}

// Usage counts tokens.
//
// InputTokens is the FULL prompt count including anything served from or
// written to cache, matching OpenAI's prompt_tokens. Protocols that report a
// cache-exclusive input count (Anthropic) subtract on the way out; doing the
// arithmetic in the encoder rather than here keeps one definition instead of
// two conventions that look identical in a struct dump.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
}

// TotalTokens is input plus output. Cache counts are a breakdown of the input
// and are not added again.
func (u Usage) TotalTokens() int { return u.InputTokens + u.OutputTokens }

// Empty reports whether nothing was counted.
func (u Usage) Empty() bool { return u == Usage{} }

// Add accumulates, which is what a streaming accumulator needs when a backend
// reports usage incrementally.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.ReasoningTokens += o.ReasoningTokens
}

// Response is a complete non-streaming response.
type Response struct {
	ID string
	// Model is the name to report to the client. §7.2: the body always carries
	// the name the client asked for, never the upstream id.
	Model             string
	Created           int64
	Choices           []Choice
	Usage             *Usage
	SystemFingerprint string
	ServiceTier       string
	Extra             map[string]json.RawMessage
}

// Choice is one completion.
type Choice struct {
	Index   int
	Message Message
	// StopReason is the normalized reason.
	StopReason StopReason
	// NativeStopReason is the backend's own string, preserved verbatim so that
	// normalization loses nothing (COMPATIBILITY 4.3). It is empty when the
	// backend value already equalled the normalized one.
	NativeStopReason string
	// Logprobs is kept raw; dorang does not model it and per-backend fidelity is
	// explicitly unverified (COMPATIBILITY 10).
	Logprobs json.RawMessage
}

// EventType discriminates a [StreamEvent].
type EventType string

const (
	// EventStart carries stream identity: id, model and created. A frontend uses
	// it to pin those values across every subsequent chunk (COMPATIBILITY 2.4).
	EventStart EventType = "start"
	// EventDelta carries incremental content.
	EventDelta EventType = "delta"
	// EventStop carries a terminal reason for one choice.
	EventStop EventType = "stop"
	// EventUsage carries token counts. It may legitimately arrive AFTER an
	// EventStop (COMPATIBILITY 3.4) and must not be dropped for that reason.
	EventUsage EventType = "usage"
	// EventError carries an in-band error (COMPATIBILITY 1.3).
	EventError EventType = "error"
)

// StreamEvent is one increment of a streaming response.
type StreamEvent struct {
	Type    EventType
	ID      string
	Model   string
	Created int64
	// Choice is the choice index this event belongs to.
	Choice int
	Delta  Delta
	Usage  *Usage
	Err    *Error
}

// Delta is the incremental payload of an [EventDelta] or [EventStop].
type Delta struct {
	// Role is set on the first delta of a choice and empty afterwards.
	Role Role
	// Content is the incremental block list. Text arrives as KindText fragments
	// and reasoning as KindThinking fragments; neither is a complete block.
	Content []Block
	// ToolCalls are incremental tool-call fragments. Index is required on each
	// (COMPATIBILITY 5.1).
	ToolCalls []ToolCallDelta
	// Refusal is an incremental refusal string.
	Refusal string

	StopReason       StopReason
	NativeStopReason string
}

// ToolCallDelta is one fragment of a streaming tool call.
type ToolCallDelta struct {
	// Index is REQUIRED and non-optional (COMPATIBILITY 5.1). It is the only
	// thing that lets a client reassemble interleaved parallel calls, so it is a
	// plain int and not a pointer: there is no "absent" case.
	Index int
	ID    string
	Name  string
	// Arguments is a fragment of the JSON argument object, not a complete one.
	Arguments string
	// Type is "function" unless the backend named something else.
	Type string
}

// Error is a protocol-neutral error.
type Error struct {
	// StatusCode is the HTTP status dorang will use. Zero means 500.
	StatusCode int
	Message    string
	// Type is the coarse class ("invalid_request_error", "rate_limit_error").
	Type string
	// Param names the offending request field, when there is one. It is a
	// pointer because the wire form distinguishes an absent param from a null
	// one (COMPATIBILITY 7.1).
	Param *string
	// Code is a string. Some backends send a number here; every encoder in
	// dorang stringifies it, because a client that reads code as a string
	// crashes on a number (COMPATIBILITY 7.1).
	Code string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Message
}

// RequiredCapabilities reports what a response uses that a smaller protocol
// cannot express.
//
// This is where [CapRichStopReasons] is actually decided: a request never needs
// it, but a response that ended in "refusal" or "recitation" cannot say so
// through OpenAI's five finish_reason values.
func (r *Response) RequiredCapabilities() Capability {
	if r == nil {
		return 0
	}
	var c Capability
	for i := range r.Choices {
		if !r.Choices[i].StopReason.Expressible() {
			c |= CapRichStopReasons
		}
		c |= contentCapabilities(r.Choices[i].Message.Content)
	}
	if r.Usage != nil && (r.Usage.CacheReadTokens > 0 || r.Usage.CacheWriteTokens > 0) {
		c |= CapCacheBreakpoints
	}
	return c
}
