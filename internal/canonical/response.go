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

// Family names the wire shape a response arrived in.
//
// It exists so that an encoder can tell a SAME-family crossing from a
// conversion. Unknown members of an upstream answer are safe to forward when
// the answer is going back out in the shape it arrived in, and are not safe
// otherwise: splicing a llama.cpp `timings` object into an Anthropic message,
// or an Anthropic `container` into a chat completion, invents a field the
// target protocol does not have, which is its own compatibility break. DESIGN
// §10.7 governs what a conversion may carry, and it carries only what the table
// names.
//
// The values match the catalog's API identifiers where one exists. They are
// spelled out here rather than imported because internal/canonical is the
// bottom of the dependency graph and must stay that way.
type Family string

// The wire shapes. Two of these are both "OpenAI" and neither can pass the
// other's unknown members: a chat completion and a text completion disagree
// about the shape of a choice, and Responses disagrees with both.
const (
	// FamilyUnknown means nothing recorded where the members came from, so
	// nothing may be spliced anywhere.
	FamilyUnknown Family = ""
	// FamilyOpenAIChat is the chat.completion shape.
	FamilyOpenAIChat Family = "openai-chat"
	// FamilyOpenAICompletions is the legacy text_completion shape.
	FamilyOpenAICompletions Family = "openai-completions"
	// FamilyOpenAIResponses is the Responses shape.
	FamilyOpenAIResponses Family = "openai-responses"
	// FamilyAnthropicMessages is the Messages shape.
	FamilyAnthropicMessages Family = "anthropic-messages"
)

// UsageField names one counter of [Usage].
//
// It exists because a token counter has THREE states on the wire and an int
// holds two: the backend said 7, the backend said 0, or the backend said
// nothing at all. A billing integration reads those last two differently —
// "the cache returned nothing this time" is a measurement, "this deployment has
// no cache" is a capability — and an encoder that omits a measured zero makes
// them indistinguishable, so a customer's cache savings vanish from their own
// accounting with no error anywhere.
//
// Presence rides beside the value instead of turning every counter into a
// pointer. That keeps [Usage] a comparable value type, which matters: the
// streaming accumulators copy it by value on every frame, and a pointer or a
// map inside it would be aliased by every copy.
type UsageField uint8

// The counters whose presence is tracked.
const (
	// UsageInput — prompt_tokens / input_tokens was stated.
	UsageInput UsageField = 1 << iota
	// UsageOutput — completion_tokens / output_tokens was stated.
	UsageOutput
	// UsageCacheRead — the cache-read breakdown was stated, even as zero.
	UsageCacheRead
	// UsageCacheWrite — the cache-write breakdown was stated, even as zero.
	UsageCacheWrite
	// UsageReasoning — the reasoning breakdown was stated, even as zero.
	UsageReasoning
)

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

	// Reported is the set of counters the backend actually stated. A decoder
	// sets it; an encoder consults it to decide whether a zero is a measurement
	// worth emitting. Zero — nothing reported — is the value every
	// hand-constructed Usage has, so a Usage that was never decoded from a wire
	// behaves exactly as it did before this field existed.
	Reported UsageField
}

// TotalTokens is input plus output. Cache counts are a breakdown of the input
// and are not added again.
func (u Usage) TotalTokens() int { return u.InputTokens + u.OutputTokens }

// Empty reports whether nothing was counted.
//
// A backend that explicitly reported zeroes is NOT empty: it measured, and the
// measurement is what the client asked for.
func (u Usage) Empty() bool { return u == Usage{} }

// Reports whether every field of want was stated by the backend.
func (u Usage) Reports(want UsageField) bool { return u.Reported&want == want }

// Report marks fields as stated by the backend.
func (u *Usage) Report(f UsageField) { u.Reported |= f }

// Add accumulates, which is what a streaming accumulator needs when a backend
// reports usage incrementally. Presence unions: a field stated in any increment
// was stated.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.Reported |= o.Reported
}

// UsageExtra carries the members of an upstream usage object that no canonical
// counter names.
//
// It hangs off [Response] rather than off [Usage] for the reason [UsageField]
// gives: Usage is copied by value on the streaming path and a map inside it
// would be shared by every copy. The three maps mirror the three nesting levels
// the OpenAI shape actually has, because flattening them loses which object a
// member belonged to and there is no way to put it back.
type UsageExtra struct {
	// Usage is the unmodelled members of the usage object itself.
	Usage map[string]json.RawMessage
	// PromptDetails is the unmodelled members of the prompt-token breakdown —
	// audio_tokens and the rest, which are real counts on a real invoice.
	PromptDetails map[string]json.RawMessage
	// CompletionDetails is the unmodelled members of the completion-token
	// breakdown, including accepted_prediction_tokens.
	CompletionDetails map[string]json.RawMessage
}

// Empty reports whether nothing unmodelled was carried.
func (u *UsageExtra) Empty() bool {
	return u == nil || (len(u.Usage) == 0 && len(u.PromptDetails) == 0 && len(u.CompletionDetails) == 0)
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
	// Extra carries members of the response object dorang does not model, keyed
	// by wire name. It is forwarded only by an encoder for [ExtraFamily].
	Extra map[string]json.RawMessage
	// UsageExtra carries the unmodelled members of the usage object, under the
	// same family rule as Extra.
	UsageExtra *UsageExtra
	// ExtraFamily names the shape Extra, UsageExtra and the per-choice Extra and
	// ProviderFields were read from. An encoder for any other shape must ignore
	// them; see [Family].
	ExtraFamily Family
}

// SameFamily reports whether an encoder for f may forward the unmodelled
// members this response carries.
//
// It is deliberately false for [FamilyUnknown] on both sides: a response that
// did not record where its members came from cannot have them spliced anywhere,
// and an encoder that does not name its own shape cannot be trusted to receive
// them.
func (r *Response) SameFamily(f Family) bool {
	return r != nil && f != FamilyUnknown && r.ExtraFamily == f
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
	// Extra carries members of the choice object dorang does not model, under
	// [Response.ExtraFamily]'s rule.
	Extra map[string]json.RawMessage
	// ProviderFields is the choice's provider_specific_fields as the backend
	// sent it, INCLUDING its native_finish_reason if there was one. dorang
	// writes its own native_finish_reason over that key on the way out
	// (COMPATIBILITY 4.3) and leaves every other member alone, which is the only
	// way a second gateway in the chain does not erase the first one's context.
	ProviderFields map[string]json.RawMessage
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
