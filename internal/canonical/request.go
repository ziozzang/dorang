package canonical

import "encoding/json"

// Request is a protocol-neutral inference request.
//
// Every optional scalar is a pointer, because "absent" and "zero" are different
// on the wire and collapsing them is how a gateway silently sets temperature to
// 0 on a request that never mentioned it.
type Request struct {
	// Model is an opaque string. No component splits it on any character
	// (DESIGN §2.1).
	Model string

	// System is the Anthropic-shaped top-level system prompt, which may be a
	// list of blocks with per-block attributes. A protocol with only a system
	// *message* loses those attributes ([CapStructuredSystem]).
	System Content

	Messages []Message

	Tools      []Tool
	ToolChoice *ToolChoice

	MaxTokens   *int
	Temperature *float64
	TopP        *float64
	TopK        *int
	Stop        []string

	Stream bool
	// StreamOptions is nil when the client did not send the field at all. The
	// distinction matters: a usage chunk is emitted only when include_usage is
	// exactly true (COMPATIBILITY 3.1), and a same-protocol round trip must not
	// invent the object.
	StreamOptions *StreamOptions

	Seed *int64
	User string

	ResponseFormat *ResponseFormat
	Reasoning      *Reasoning

	// Metadata is opaque caller-supplied key/value data.
	Metadata map[string]string

	// Priority is a client hint, clamped to the principal's permitted range by
	// the router (DESIGN §10.5).
	Priority *int

	N                 *int
	FrequencyPenalty  *float64
	PresencePenalty   *float64
	LogitBias         map[string]float64
	Logprobs          *bool
	TopLogprobs       *int
	ParallelToolCalls *bool
	ServiceTier       string

	// Extra carries every field dorang does not model, keyed by its wire name,
	// so a same-protocol crossing is a pass-through rather than a filter.
	Extra map[string]json.RawMessage
}

// StreamOptions mirrors the OpenAI stream_options object.
type StreamOptions struct {
	IncludeUsage bool
}

// Tool is a callable the caller declared.
type Tool struct {
	// Type is "function" for the ordinary case and a provider-native tool type
	// otherwise (a server-side search tool, a computer-use tool). A native type
	// is carried through Extra.
	Type string
	// Name is the caller's declared name at full length. Wire limits are applied
	// by encoders, reversibly (COMPATIBILITY 5.3).
	Name        string
	Description string
	// Parameters is a JSON Schema object, kept raw so key order survives.
	Parameters json.RawMessage
	Strict     *bool
	// CacheControl marks the tool block as a cache breakpoint.
	CacheControl *CacheControl
	Extra        map[string]json.RawMessage
}

// ToolChoiceMode is how the caller constrained tool selection.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceTool names a specific tool in ToolChoice.Name.
	ToolChoiceTool ToolChoiceMode = "tool"
)

// ToolChoice constrains which tool the model may call.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
	// DisableParallel is Anthropic's disable_parallel_tool_use, the inverse of
	// OpenAI's parallel_tool_calls. One neutral field, folded either way.
	DisableParallel *bool
}

// ResponseFormat constrains the response shape.
type ResponseFormat struct {
	// Kind is "text", "json_object" or "json_schema".
	Kind        string
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      *bool
}

const (
	FormatText       = "text"
	FormatJSONObject = "json_object"
	FormatJSONSchema = "json_schema"
)

// Reasoning is dorang's one neutral reasoning control (DESIGN §10.2). Each
// backend folds it onto its own shape, keyed by (kind, model capability) rather
// than by provider kind, and omits it — reporting the omission — when the
// catalog has not verified how that model accepts it.
type Reasoning struct {
	// Enabled is nil when the caller did not say. A backend must not infer
	// "enabled" from the presence of an effort level alone if its wire form
	// needs an explicit flag.
	Enabled *bool
	// Effort is one of the Effort* constants.
	Effort string
	// BudgetTokens is a thinking budget. It is a function of the request, never
	// a constant: dorang clamps it strictly below the output ceiling and never
	// raises a caller's max_tokens to accommodate it (DESIGN §10.2).
	BudgetTokens int
	// Summary is one of the Summary* constants.
	Summary string
}

const (
	EffortNone    = "none"
	EffortMinimal = "minimal"
	EffortLow     = "low"
	EffortMedium  = "medium"
	EffortHigh    = "high"
	EffortXHigh   = "xhigh"
	EffortMax     = "max"

	SummaryNone     = "none"
	SummaryConcise  = "concise"
	SummaryDetailed = "detailed"
)

// IncludeUsage reports whether a usage chunk was explicitly requested.
//
// COMPATIBILITY 3.1: only an exact true counts. A present stream_options object
// with include_usage absent or false does not.
func (r *Request) IncludeUsage() bool {
	return r != nil && r.StreamOptions != nil && r.StreamOptions.IncludeUsage
}
