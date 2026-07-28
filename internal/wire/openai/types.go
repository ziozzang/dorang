package openai

import (
	"encoding/json"
	"errors"

	"github.com/ziozzang/dorang/internal/wire/wirejson"
)

// Object values.
const (
	ObjectChunk      = "chat.completion.chunk" // COMPATIBILITY 2.3
	ObjectCompletion = "chat.completion"
	ObjectModel      = "model"
)

// DoneFrame is the stream terminator (COMPATIBILITY 1.2).
const DoneFrame = "data: [DONE]\n\n"

// ---------------------------------------------------------------------------
// Streaming chunk
// ---------------------------------------------------------------------------

// Chunk is one streamed frame.
//
// Field order is the field order of COMPATIBILITY 2.2 because encoding/json
// emits struct fields in declaration order and the golden tests compare bytes.
// Everything after Choices is omitempty, so a plain text chunk marshals to
// exactly {id, object, created, model, choices:[…]} and nothing more.
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`

	// Usage rides on the terminal chunk only, and only when the client asked
	// (COMPATIBILITY 3.1).
	Usage *Usage `json:"usage,omitempty"`
	// SystemFingerprint is a pointer purely so that an absent value is OMITTED
	// rather than emitted as null (COMPATIBILITY 2.1).
	SystemFingerprint *string `json:"system_fingerprint,omitempty"`
	ServiceTier       *string `json:"service_tier,omitempty"`
}

// ChunkChoice is one choice of a streamed frame.
type ChunkChoice struct {
	Index int   `json:"index"`
	Delta Delta `json:"delta"`
	// FinishReason is a pointer so an in-flight chunk omits it entirely rather
	// than sending "finish_reason":null (COMPATIBILITY 2.1).
	FinishReason *string `json:"finish_reason,omitempty"`
	// Logprobs is carried raw. Per-backend fidelity is explicitly unverified
	// (COMPATIBILITY 10), so dorang forwards rather than models it — and it must
	// never appear as "logprobs":null.
	Logprobs json.RawMessage `json:"logprobs,omitempty"`
	// ProviderSpecificFields preserves the backend's own finish reason under
	// native_finish_reason (COMPATIBILITY 4.3). Placement is on the choice
	// because that is where finish_reason lives; the contract fixes the key
	// path, not the object it hangs from.
	ProviderSpecificFields map[string]json.RawMessage `json:"provider_specific_fields,omitempty"`
}

// Delta is the incremental payload of a streamed choice.
//
// Every field is a pointer or a slice with omitempty, so an empty Delta
// marshals to {} — which is exactly what the usage chunk needs
// (COMPATIBILITY 3.3) — and a delta carrying only content marshals to
// {"content":"…"} with no null siblings.
type Delta struct {
	Role    *string `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
	// ReasoningContent is the de-facto field for streamed reasoning text. It is
	// not part of the OpenAI specification; dorang emits it because the clients
	// that read reasoning read this name.
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
	Refusal          *string         `json:"refusal,omitempty"`
	FunctionCall     *FunctionDelta  `json:"function_call,omitempty"`
}

// Empty reports whether the delta carries nothing.
func (d Delta) Empty() bool {
	return d.Role == nil && d.Content == nil && d.ReasoningContent == nil &&
		len(d.ToolCalls) == 0 && d.Refusal == nil && d.FunctionCall == nil
}

// ToolCallDelta is one fragment of a streaming tool call.
//
// COMPATIBILITY 5.1: Index is REQUIRED and non-optional — a plain int with no
// omitempty, so index 0 is still emitted. id, type and function are optional.
// Getting this backwards (omitempty on Index) drops "index":0 from the first
// fragment of every tool call and clients cannot reassemble the arguments.
type ToolCallDelta struct {
	Index    int            `json:"index"`
	ID       *string        `json:"id,omitempty"`
	Type     *string        `json:"type,omitempty"`
	Function *FunctionDelta `json:"function,omitempty"`
}

// FunctionDelta is the function half of a tool-call fragment.
type FunctionDelta struct {
	Name      *string `json:"name,omitempty"`
	Arguments *string `json:"arguments,omitempty"`
}

// ---------------------------------------------------------------------------
// Non-streaming response
// ---------------------------------------------------------------------------

// Response is a complete chat completion.
//
// Extra is what makes a same-family non-streaming answer a PASS-THROUGH rather
// than a filter. Without it dorang re-serializes from this struct and every
// member it does not model disappears — llama.cpp's `timings`, a vendor's
// `citations`, whatever ships next month — while the streaming path, which
// forwards frames, keeps them. One request losing data or not depending on
// whether the caller asked for a stream is not a defect anyone debugs
// successfully.
type Response struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`

	SystemFingerprint *string `json:"system_fingerprint,omitempty"`
	ServiceTier       *string `json:"service_tier,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var responseKnown = knownKeys("id", "object", "created", "model", "choices",
	"usage", "system_fingerprint", "service_tier")

// MarshalJSON implements [encoding/json.Marshaler].
func (r Response) MarshalJSON() ([]byte, error) {
	type alias Response
	return marshalWithExtra(alias(r), r.Extra, responseKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
//
// Response-only type: json.Unmarshal, not strictBytes. See [strictUnmarshal] —
// these bytes are a backend's, not a caller's, and nothing is authorized
// against a response.
func (r *Response) UnmarshalJSON(b []byte) error {
	type alias Response
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, responseKnown)
	if err != nil {
		return err
	}
	*r = Response(a)
	r.Extra = extra
	return nil
}

// Choice is one completed choice.
type Choice struct {
	Index                  int                        `json:"index"`
	Message                Message                    `json:"message"`
	FinishReason           *string                    `json:"finish_reason,omitempty"`
	Logprobs               json.RawMessage            `json:"logprobs,omitempty"`
	ProviderSpecificFields map[string]json.RawMessage `json:"provider_specific_fields,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var choiceKnown = knownKeys("index", "message", "finish_reason", "logprobs",
	"provider_specific_fields")

// MarshalJSON implements [encoding/json.Marshaler].
func (c Choice) MarshalJSON() ([]byte, error) {
	type alias Choice
	return marshalWithExtra(alias(c), c.Extra, choiceKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler]. Response-only type, so
// json.Unmarshal rather than the strict filter — but [Message] inside it is
// still strict, because that type also decodes request messages.
func (c *Choice) UnmarshalJSON(b []byte) error {
	type alias Choice
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, choiceKnown)
	if err != nil {
		return err
	}
	*c = Choice(a)
	c.Extra = extra
	return nil
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// Usage is the token accounting object.
//
// The detail sub-objects are pointers, and a detail object is attached when the
// backend REPORTED that breakdown — not when the number happens to be positive.
// The distinction is the whole of DESIGN §10.7's "token accounting is the part
// that must be exactly right": `cached_tokens: 0` means the cache returned
// nothing on this request, an absent `prompt_tokens_details` means the
// deployment never mentioned a cache, and a billing integration prices those
// differently. dorang's own synthesized usage reports nothing, so a count it
// invented still emits no breakdown at all.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`

	// CacheCreationInputTokens is the widely-read cache-write extension
	// (COMPATIBILITY 3.5). OpenAI has no such field; the clients that care about
	// cache cost read this name.
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var usageKnown = knownKeys("prompt_tokens", "completion_tokens", "total_tokens",
	"prompt_tokens_details", "completion_tokens_details", "cache_creation_input_tokens")

// MarshalJSON implements [encoding/json.Marshaler].
func (u Usage) MarshalJSON() ([]byte, error) {
	type alias Usage
	return marshalWithExtra(alias(u), u.Extra, usageKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (u *Usage) UnmarshalJSON(b []byte) error {
	type alias Usage
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, usageKnown)
	if err != nil {
		return err
	}
	*u = Usage(a)
	u.Extra = extra
	return nil
}

// PromptTokensDetails breaks down the prompt count.
//
// CachedTokens has NO omitempty. A zero here is a measurement — see [Usage].
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`

	// Extra carries audio_tokens and every other breakdown member dorang does
	// not model. They are counts on somebody's invoice, not decoration.
	Extra map[string]json.RawMessage `json:"-"`
}

var promptDetailsKnown = knownKeys("cached_tokens")

// MarshalJSON implements [encoding/json.Marshaler].
func (d PromptTokensDetails) MarshalJSON() ([]byte, error) {
	type alias PromptTokensDetails
	return marshalWithExtra(alias(d), d.Extra, promptDetailsKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (d *PromptTokensDetails) UnmarshalJSON(b []byte) error {
	type alias PromptTokensDetails
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, promptDetailsKnown)
	if err != nil {
		return err
	}
	*d = PromptTokensDetails(a)
	d.Extra = extra
	return nil
}

// CompletionTokensDetails breaks down the completion count.
type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`

	// Extra carries accepted_prediction_tokens, rejected_prediction_tokens,
	// audio_tokens and anything else the backend broke out.
	Extra map[string]json.RawMessage `json:"-"`
}

var completionDetailsKnown = knownKeys("reasoning_tokens")

// MarshalJSON implements [encoding/json.Marshaler].
func (d CompletionTokensDetails) MarshalJSON() ([]byte, error) {
	type alias CompletionTokensDetails
	return marshalWithExtra(alias(d), d.Extra, completionDetailsKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (d *CompletionTokensDetails) UnmarshalJSON(b []byte) error {
	type alias CompletionTokensDetails
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, completionDetailsKnown)
	if err != nil {
		return err
	}
	*d = CompletionTokensDetails(a)
	d.Extra = extra
	return nil
}

// ---------------------------------------------------------------------------
// Messages and content
// ---------------------------------------------------------------------------

// Message is one request or response message.
type Message struct {
	Role    string   `json:"role"`
	Content *Content `json:"content,omitempty"`
	Name    string   `json:"name,omitempty"`

	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`

	Refusal          *string `json:"refusal,omitempty"`
	ReasoningContent *string `json:"reasoning_content,omitempty"`

	// Extra preserves fields dorang does not model so a same-protocol crossing
	// is a pass-through, not a filter.
	Extra map[string]json.RawMessage `json:"-"`
}

var messageKnown = knownKeys("role", "content", "name", "tool_calls", "tool_call_id",
	"refusal", "reasoning_content")

func (m Message) MarshalJSON() ([]byte, error) {
	type alias Message
	return marshalWithExtra(alias(m), m.Extra, messageKnown)
}

func (m *Message) UnmarshalJSON(b []byte) error {
	type alias Message
	var a alias
	// Filter once, then decode the SAME bytes twice: a key dropped from the
	// struct decode must be dropped from Extra too, or it is relayed to the
	// next hop and re-creates the bypass there (COMPATIBILITY 2.0).
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, messageKnown)
	if err != nil {
		return err
	}
	*m = Message(a)
	m.Extra = extra
	return nil
}

// Content is the string-or-array content field.
//
// The two forms are not interchangeable: a string cannot carry an image, a
// cache breakpoint or a per-part attribute. Which form arrived is preserved so
// that a same-protocol round trip does not silently rewrite a caller's request.
type Content struct {
	// Text is meaningful when Parts is nil.
	Text string
	// Parts is non-nil exactly when the array form was used or is required.
	Parts []Part
}

// TextContent builds the string form.
func TextContent(s string) *Content { return &Content{Text: s} }

// PartsContent builds the array form. A nil slice still selects the array form,
// so it marshals as [] rather than "".
func PartsContent(parts []Part) *Content {
	if parts == nil {
		parts = []Part{}
	}
	return &Content{Parts: parts}
}

// IsParts reports whether the array form is in use.
func (c *Content) IsParts() bool { return c != nil && c.Parts != nil }

// String returns the string form, concatenating text parts when the array form
// is in use. Callers that need to know whether that was lossy must inspect
// Parts themselves.
func (c *Content) String() string {
	if c == nil {
		return ""
	}
	if c.Parts == nil {
		return c.Text
	}
	n := 0
	for i := range c.Parts {
		if c.Parts[i].Type == PartText {
			n += len(c.Parts[i].Text)
		}
	}
	out := make([]byte, 0, n)
	for i := range c.Parts {
		if c.Parts[i].Type == PartText {
			out = append(out, c.Parts[i].Text...)
		}
	}
	return string(out)
}

func (c Content) MarshalJSON() ([]byte, error) {
	if c.Parts != nil {
		return json.Marshal(c.Parts)
	}
	return json.Marshal(c.Text)
}

func (c *Content) UnmarshalJSON(b []byte) error {
	b = trimSpace(b)
	if len(b) == 0 {
		return errors.New("openai: empty content")
	}
	switch b[0] {
	case 'n': // null — an assistant message with only tool calls
		*c = Content{}
		return nil
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = Content{Text: s}
		return nil
	case '[':
		var parts []Part
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		if parts == nil {
			parts = []Part{}
		}
		*c = Content{Parts: parts}
		return nil
	default:
		return errors.New("openai: content must be a string or an array")
	}
}

// Part types.
const (
	PartText       = "text"
	PartImageURL   = "image_url"
	PartInputAudio = "input_audio"
	PartFile       = "file"
	// PartRefusal appears in assistant messages echoed back by a client.
	PartRefusal = "refusal"
)

// Part is one element of the array content form.
type Part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	File     *FilePart `json:"file,omitempty"`
	Refusal  string    `json:"refusal,omitempty"`

	// CacheControl is not part of the OpenAI specification. It is the de-facto
	// way a prompt-cache breakpoint crosses an OpenAI-shaped wire, and dorang
	// emits it so that breakpoints survive a round trip instead of becoming a
	// structural downgrade on every hop.
	CacheControl *CacheControl `json:"cache_control,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var partKnown = knownKeys("type", "text", "image_url", "file", "refusal", "cache_control")

func (p Part) MarshalJSON() ([]byte, error) {
	type alias Part
	return marshalWithExtra(alias(p), p.Extra, partKnown)
}

func (p *Part) UnmarshalJSON(b []byte) error {
	type alias Part
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, partKnown)
	if err != nil {
		return err
	}
	*p = Part(a)
	p.Extra = extra
	return nil
}

// ImageURL is an image part payload. URL may be an http(s) URL or a data: URL.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// FilePart is a document part payload.
type FilePart struct {
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// CacheControl is a prompt-cache breakpoint marker.
type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

// ToolCall is a completed tool call on an assistant message.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
	// Index is accepted inbound and STRIPPED before forwarding
	// (COMPATIBILITY 5.2), so a client that echoes back a full assistant message
	// it received from a stream is not rejected by an upstream that refuses the
	// field. It is a pointer so that a stripped index is absent, not zero.
	Index *int `json:"index,omitempty"`
}

// FunctionCall is the function half of a completed tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a declared callable.
type Tool struct {
	Type     string        `json:"type"`
	Function *ToolFunction `json:"function,omitempty"`
	// CacheControl marks the tool block as a cache breakpoint. As on Part, this
	// is an extension, not OpenAI specification.
	CacheControl *CacheControl              `json:"cache_control,omitempty"`
	Extra        map[string]json.RawMessage `json:"-"`
}

var toolKnown = knownKeys("type", "function", "cache_control")

func (t Tool) MarshalJSON() ([]byte, error) {
	type alias Tool
	return marshalWithExtra(alias(t), t.Extra, toolKnown)
}

func (t *Tool) UnmarshalJSON(b []byte) error {
	type alias Tool
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, toolKnown)
	if err != nil {
		return err
	}
	*t = Tool(a)
	t.Extra = extra
	return nil
}

// ToolFunction is a function tool declaration.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ---------------------------------------------------------------------------
// Request
// ---------------------------------------------------------------------------

// Request is a chat-completions request.
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	Tools []Tool `json:"tools,omitempty"`
	// ToolChoice is "none" | "auto" | "required" | {"type":"function",…}. It is
	// kept raw on the wire type and interpreted during conversion, because a
	// provider-native form dorang does not model must still pass through.
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`

	MaxTokens *int `json:"max_tokens,omitempty"`
	// MaxCompletionTokens is the current spelling. Both are accepted inbound;
	// the newer one wins when both are present.
	MaxCompletionTokens *int     `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	// TopK is not an OpenAI parameter. It is accepted inbound so an
	// Anthropic-origin request that crossed here keeps the value visible, and
	// dropped when the target cannot take it.
	TopK *int `json:"top_k,omitempty"`
	// Stop is a string or an array of strings on the wire.
	Stop StopSequences `json:"stop,omitempty"`

	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	N                *int               `json:"n,omitempty"`
	FrequencyPenalty *float64           `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64           `json:"presence_penalty,omitempty"`
	LogitBias        map[string]float64 `json:"logit_bias,omitempty"`
	Logprobs         *bool              `json:"logprobs,omitempty"`
	TopLogprobs      *int               `json:"top_logprobs,omitempty"`

	Seed           *int64          `json:"seed,omitempty"`
	User           string          `json:"user,omitempty"`
	ServiceTier    string          `json:"service_tier,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	ReasoningEffort string     `json:"reasoning_effort,omitempty"`
	Reasoning       *Reasoning `json:"reasoning,omitempty"`

	Metadata map[string]string `json:"metadata,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var requestKnown = knownKeys("model", "messages", "tools", "tool_choice",
	"parallel_tool_calls", "max_tokens", "max_completion_tokens", "temperature",
	"top_p", "top_k", "stop", "stream", "stream_options", "n", "frequency_penalty",
	"presence_penalty", "logit_bias", "logprobs", "top_logprobs", "seed", "user",
	"service_tier", "response_format", "reasoning_effort", "reasoning", "metadata")

func (r Request) MarshalJSON() ([]byte, error) {
	type alias Request
	return marshalWithExtra(alias(r), r.Extra, requestKnown)
}

func (r *Request) UnmarshalJSON(b []byte) error {
	type alias Request
	var a alias
	// The gate scanned these same bytes for "model" and "stream" with exact
	// key comparison. This is the line that makes the adapter agree with it.
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, requestKnown)
	if err != nil {
		return err
	}
	*r = Request(a)
	r.Extra = extra
	return nil
}

// StreamOptions is the stream_options object.
type StreamOptions struct {
	// IncludeUsage must be exactly true for a usage chunk to be emitted
	// (COMPATIBILITY 3.1). Truthy is not enough, which is why this is a plain
	// bool decoded from a JSON boolean rather than an any.
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ResponseFormat is the response_format object.
type ResponseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

// JSONSchema is the json_schema sub-object.
type JSONSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// Reasoning is the Responses-style reasoning object, accepted here because
// callers send it and dorang folds it onto whatever the target takes.
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
	// MaxTokens is the budget spelling used by several OpenAI-compatible
	// backends.
	MaxTokens int   `json:"max_tokens,omitempty"`
	Enabled   *bool `json:"enabled,omitempty"`
}

// StopSequences is the string-or-array stop field.
type StopSequences []string

func (s StopSequences) MarshalJSON() ([]byte, error) {
	if len(s) == 1 {
		return json.Marshal(s[0])
	}
	return json.Marshal([]string(s))
}

func (s *StopSequences) UnmarshalJSON(b []byte) error {
	b = trimSpace(b)
	if len(b) == 0 || b[0] == 'n' {
		*s = nil
		return nil
	}
	if b[0] == '"' {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*s = StopSequences{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = StopSequences(many)
	return nil
}

// ---------------------------------------------------------------------------
// Models list
// ---------------------------------------------------------------------------

// Model is one entry of GET /v1/models (COMPATIBILITY 7.4).
type Model struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	// Created is a FIXED CONSTANT, never time.Now: clients cache on it.
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the GET /v1/models envelope.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// ---------------------------------------------------------------------------
// Shared JSON helpers
// ---------------------------------------------------------------------------

func knownKeys(names ...string) map[string]struct{} { return wirejson.KnownKeys(names...) }

// splitExtra returns the members of a JSON object that are not in known. It is
// for types filtered by [strictBytes] first — every REQUEST type.
func splitExtra(b []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	return wirejson.SplitExtra(b, known)
}

// splitExtraFold is splitExtra for a RESPONSE type, which is decoded with plain
// json.Unmarshal. See [wirejson.SplitExtraFold]: without the fold, a backend
// that spells a modelled key "Usage" gets it read into the struct AND left in
// the map, and the client receives the object twice.
func splitExtraFold(b []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	return wirejson.SplitExtraFold(b, known)
}

// marshalWithExtra marshals v and splices extra's members into the resulting
// object. Keys are sorted so the bytes are deterministic, which the golden
// tests require.
func marshalWithExtra(v any, extra map[string]json.RawMessage, known map[string]struct{}) ([]byte, error) {
	return wirejson.MarshalWithExtra(v, extra, known)
}

func trimSpace(b []byte) []byte { return wirejson.TrimSpace(b) }
