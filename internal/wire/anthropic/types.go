package anthropic

import (
	"encoding/json"
	"errors"
)

// Object discriminators.
const (
	// TypeMessage is the value of the top-level "type" on a Messages response.
	TypeMessage = "message"
	// RoleAssistant is the only role a response ever carries.
	RoleAssistant = "assistant"
	// RoleUser is the role that carries tool results: this family puts them in a
	// user turn, not in a turn of their own.
	RoleUser = "user"
)

// Content block types (COMPATIBILITY 6.3 names the three that appear on the
// adapter path; the rest exist because a request may carry them inbound).
const (
	BlockText             = "text"
	BlockImage            = "image"
	BlockDocument         = "document"
	BlockToolUse          = "tool_use"
	BlockToolResult       = "tool_result"
	BlockThinking         = "thinking"
	BlockRedactedThinking = "redacted_thinking"
)

// Source types.
const (
	SourceBase64 = "base64"
	SourceURL    = "url"
	SourceFile   = "file"
	SourceText   = "text"
)

// ---------------------------------------------------------------------------
// Content
// ---------------------------------------------------------------------------

// ContentBlock is one element of a content array.
//
// It is one struct rather than a union per type because the kind-specific
// fields are disjoint, and because encoding/json emits struct fields in
// declaration order: the order below is the order the vendor emits, which is
// what makes a golden byte comparison meaningful.
//
// Text and Thinking are POINTERS to string. That is not an accident and not the
// usual "absent vs zero" argument: content_block_start opens a text block with
// {"type":"text","text":""} and a thinking block with
// {"type":"thinking","thinking":""}. With a plain string plus omitempty the
// empty opener collapses to {"type":"text"}, which is not the shape the SDK
// accumulates into.
type ContentBlock struct {
	Type string `json:"type"`

	Text     *string `json:"text,omitempty"`
	Thinking *string `json:"thinking,omitempty"`
	// Signature is integrity material. dorang stores and replays it
	// byte-identically and never fabricates or re-signs one (DESIGN §10.2).
	Signature string `json:"signature,omitempty"`
	// Data is the opaque payload of a redacted_thinking block. EXTENSIONS §B18
	// records that canonical.Thinking models Redacted but has nowhere to put the
	// payload; this package parks it in canonical Block.Extra under the same key
	// so that a round trip reconstructs the block WITH its payload.
	Data string `json:"data,omitempty"`

	Source *Source `json:"source,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string     `json:"tool_use_id,omitempty"`
	Content   *BlockList `json:"content,omitempty"`
	IsError   bool       `json:"is_error,omitempty"`

	Title string `json:"title,omitempty"`

	// CacheControl is a prompt-cache breakpoint. Its TTL is opaque
	// (EXTENSIONS §B21): CapCacheBreakpoints models that a breakpoint exists,
	// not its lifetime, so the value is carried rather than interpreted.
	CacheControl *CacheControl `json:"cache_control,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var blockKnown = knownKeys("type", "text", "thinking", "signature", "data", "source",
	"id", "name", "input", "tool_use_id", "content", "is_error", "title", "cache_control")

// blockTypeOnly is the reserved-key set for a block type this package does not
// model. Everything but the discriminator rides in Extra.
var blockTypeOnly = knownKeys("type")

// modelledBlockType reports whether the fields of [ContentBlock] describe this
// type.
//
// The distinction is load-bearing, not cosmetic. A vendor block this package
// has never seen may use a member name that IS modelled here with a different
// shape — a search_result block's "source" is a string where an image's is an
// object — and a strict decode rejects the whole request with an error naming a
// field the caller never wrote. So an unmodelled type is relayed as opaque
// members instead of being parsed.
func modelledBlockType(t string) bool {
	switch t {
	case BlockText, BlockImage, BlockDocument, BlockToolUse, BlockToolResult,
		BlockThinking, BlockRedactedThinking:
		return true
	}
	return false
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	type alias ContentBlock
	known := blockKnown
	if !modelledBlockType(b.Type) {
		// Only the discriminator is reserved: an opaque block's members must be
		// spliced back even when they share a name with a modelled field.
		known = blockTypeOnly
	}
	return marshalWithExtra(alias(b), b.Extra, known)
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type string `json:"type"`
	}
	// The discriminator decides everything below it, so it is resolved
	// case-sensitively first: {"Type":"text"} declares no type here, exactly as
	// it declares none to the backend. Filtering against the probe rather than
	// against the whole block is deliberate — at this point the block may still
	// turn out to be an opaque one, whose members are nobody's business.
	typed := strictBytes(data, &probe)
	if err := json.Unmarshal(typed, &probe); err != nil {
		return err
	}
	if !modelledBlockType(probe.Type) {
		// Only the discriminator is reserved on an opaque block, so only a
		// collision with "type" was removed: a member spelled like a modelled
		// field is relayed exactly as it arrived, which is the whole point of
		// the opaque path.
		extra, err := splitExtra(typed, blockTypeOnly)
		if err != nil {
			return err
		}
		*b = ContentBlock{Type: probe.Type, Extra: extra}
		return nil
	}
	type alias ContentBlock
	var a alias
	data = strictBytes(data, &a)
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	extra, err := splitExtra(data, blockKnown)
	if err != nil {
		return err
	}
	*b = ContentBlock(a)
	b.Extra = extra
	return nil
}

// Source is an image or document payload.
type Source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
}

// CacheControl is a prompt-cache breakpoint marker.
type CacheControl struct {
	Type string `json:"type"`
	// TTL is "5m" or "1h". Opaque: relayed, not interpreted (EXTENSIONS §B21).
	TTL string `json:"ttl,omitempty"`
}

// BlockList is the string-or-array form, used by system, by a message's content
// and by a tool_result's content.
//
// Which form arrived is preserved. A tool result that came as an array of text
// plus an image must not be silently rewritten to a string on a same-protocol
// crossing — that is the CapMultiBlockToolResult downgrade, and it is a
// structural loss when it is forced, not a formatting choice when it is not.
type BlockList struct {
	// Text is meaningful when Blocks is nil.
	Text string
	// Blocks is non-nil exactly when the array form is in use.
	Blocks []ContentBlock
}

// TextList builds the string form.
func TextList(s string) *BlockList { return &BlockList{Text: s} }

// BlocksList builds the array form. A nil slice still selects the array form,
// so it marshals as [] rather than "".
func BlocksList(blocks []ContentBlock) *BlockList {
	if blocks == nil {
		blocks = []ContentBlock{}
	}
	return &BlockList{Blocks: blocks}
}

// IsBlocks reports whether the array form is in use.
func (l *BlockList) IsBlocks() bool { return l != nil && l.Blocks != nil }

// String concatenates the text, which is lossy for every other block kind.
func (l *BlockList) String() string {
	if l == nil {
		return ""
	}
	if l.Blocks == nil {
		return l.Text
	}
	n := 0
	for i := range l.Blocks {
		if l.Blocks[i].Type == BlockText && l.Blocks[i].Text != nil {
			n += len(*l.Blocks[i].Text)
		}
	}
	out := make([]byte, 0, n)
	for i := range l.Blocks {
		if l.Blocks[i].Type == BlockText && l.Blocks[i].Text != nil {
			out = append(out, *l.Blocks[i].Text...)
		}
	}
	return string(out)
}

func (l BlockList) MarshalJSON() ([]byte, error) {
	if l.Blocks != nil {
		return json.Marshal(l.Blocks)
	}
	return json.Marshal(l.Text)
}

func (l *BlockList) UnmarshalJSON(b []byte) error {
	b = trimSpace(b)
	if len(b) == 0 {
		return errors.New("anthropic: empty content")
	}
	switch b[0] {
	case 'n': // null
		*l = BlockList{}
		return nil
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*l = BlockList{Text: s}
		return nil
	case '[':
		var blocks []ContentBlock
		if err := json.Unmarshal(b, &blocks); err != nil {
			return err
		}
		if blocks == nil {
			blocks = []ContentBlock{}
		}
		*l = BlockList{Blocks: blocks}
		return nil
	default:
		return errors.New("anthropic: content must be a string or an array")
	}
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// Message is one turn. Only "user" and "assistant" are legal here: the system
// prompt is a top-level field, not a message (DESIGN §10.7).
type Message struct {
	Role    string    `json:"role"`
	Content BlockList `json:"content"`

	Extra map[string]json.RawMessage `json:"-"`
}

var messageKnown = knownKeys("role", "content")

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

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

// Tool is a declared callable. The schema field is input_schema, and the
// declaration is flat — there is no function wrapper (DESIGN §10.7).
type Tool struct {
	// Type is empty for an ordinary custom tool and names a server-side tool
	// version otherwise ("web_search_20250305", "computer_20241022"). Those
	// carry their own fields, which ride in Extra.
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`

	CacheControl *CacheControl `json:"cache_control,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var toolKnown = knownKeys("type", "name", "description", "input_schema", "cache_control")

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

// Tool choice types.
const (
	ToolChoiceAuto = "auto"
	// ToolChoiceAny is this family's spelling of "required".
	ToolChoiceAny  = "any"
	ToolChoiceTool = "tool"
	ToolChoiceNone = "none"
)

// ToolChoice constrains tool selection.
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	// DisableParallelToolUse is the INVERSE of OpenAI's parallel_tool_calls
	// (DESIGN §10.7, one of the three named traps). A naive copy reverses the
	// meaning: parallel_tool_calls=true becomes disable_parallel_tool_use=true
	// and the model stops calling tools in parallel on every request.
	DisableParallelToolUse *bool `json:"disable_parallel_tool_use,omitempty"`
}

// Thinking is the reasoning control (DESIGN §10.2, capability
// "thinking_budget").
type Thinking struct {
	// Type is "enabled" or "disabled".
	Type string `json:"type"`
	// BudgetTokens is a function of the request, never a constant: it is clamped
	// strictly below the output ceiling and dorang never raises max_tokens to
	// accommodate it (DESIGN §10.2).
	BudgetTokens *int `json:"budget_tokens,omitempty"`
}

// Thinking types.
const (
	ThinkingEnabled  = "enabled"
	ThinkingDisabled = "disabled"
)

// Metadata is the metadata object. user_id is this family's spelling of the
// end-user identifier (DESIGN §10.7).
type Metadata struct {
	UserID string `json:"user_id,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var metadataKnown = knownKeys("user_id")

func (m Metadata) MarshalJSON() ([]byte, error) {
	type alias Metadata
	return marshalWithExtra(alias(m), m.Extra, metadataKnown)
}

func (m *Metadata) UnmarshalJSON(b []byte) error {
	type alias Metadata
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, metadataKnown)
	if err != nil {
		return err
	}
	*m = Metadata(a)
	m.Extra = extra
	return nil
}

// ---------------------------------------------------------------------------
// Request
// ---------------------------------------------------------------------------

// Request is a POST /v1/messages body.
//
// Unknown members — context_management, cache_edits, mcp_servers, container and
// whatever ships next month — land in Extra and are relayed byte-identically.
// context_management in particular must NOT be modelled by name: the same name
// carries a different shape on the OpenAI Responses surface, so a neutral type
// keyed on it would be wrong for one of the two (EXTENSIONS §A.4a, §B17/B22).
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	// MaxTokens is REQUIRED here and optional everywhere else (DESIGN §10.7,
	// trap one of three). The pointer exists so a decoder can tell "absent" from
	// zero and reject it; every encoder in this package guarantees it is set.
	MaxTokens *int `json:"max_tokens,omitempty"`

	Metadata      *Metadata  `json:"metadata,omitempty"`
	StopSequences []string   `json:"stop_sequences,omitempty"`
	Stream        bool       `json:"stream,omitempty"`
	System        *BlockList `json:"system,omitempty"`
	Temperature   *float64   `json:"temperature,omitempty"`
	Thinking      *Thinking  `json:"thinking,omitempty"`

	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`
	Tools      []Tool      `json:"tools,omitempty"`

	TopK *int     `json:"top_k,omitempty"`
	TopP *float64 `json:"top_p,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var requestKnown = knownKeys("model", "messages", "max_tokens", "metadata",
	"stop_sequences", "stream", "system", "temperature", "thinking",
	"tool_choice", "tools", "top_k", "top_p")

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

// ---------------------------------------------------------------------------
// Response
// ---------------------------------------------------------------------------

// Response is a complete non-streaming message, and — with Content empty — the
// message envelope inside message_start.
//
// StopReason and StopSequence have NO omitempty. That is deliberate and it is
// the opposite of COMPATIBILITY 2.1, which is scoped to chat-completions
// chunks: here message_start carries "stop_reason":null and every response
// carries "stop_sequence":null (6.5), and a client that reads the key
// unconditionally breaks when it is omitted.
type Response struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Role    string         `json:"role"`
	Model   string         `json:"model"`
	Content []ContentBlock `json:"content"`

	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`

	Usage *Usage `json:"usage,omitempty"`

	// Extra carries the response half of an opaque request field —
	// context_management round-trips on the assistant message
	// (EXTENSIONS §A.4a) — and anything else the vendor added since.
	Extra map[string]json.RawMessage `json:"-"`
}

var responseKnown = knownKeys("id", "type", "role", "model", "content",
	"stop_reason", "stop_sequence", "usage")

func (r Response) MarshalJSON() ([]byte, error) {
	type alias Response
	return marshalWithExtra(alias(r), r.Extra, responseKnown)
}

func (r *Response) UnmarshalJSON(b []byte) error {
	type alias Response
	var a alias
	// Response-only type: json.Unmarshal, not strictBytes. See [strictUnmarshal]
	// for why the line is drawn here and not around every decode.
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

// Usage is the token accounting object.
//
// Every field is a pointer, because COMPATIBILITY 6.7 turns on exactly which
// keys are present: cache fields appear ONLY when greater than zero, the final
// message_delta carries output_tokens where message_start carried both counts,
// and 6.8's non-spec total_tokens appears on the non-streaming shape only.
//
// The counts here are cache-EXCLUSIVE, which is this family's convention and
// the inverse of canonical.Usage. See [EncodeUsage] and [UsageToCanonical].
type Usage struct {
	InputTokens              *int `json:"input_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens             *int `json:"output_tokens,omitempty"`

	// TotalTokens is NOT in the vendor specification. COMPATIBILITY 6.8: the
	// reference proxy emits it on non-streaming responses and omits it while
	// streaming, so the two shapes differ by one field. dorang reproduces the
	// asymmetry behind compat.anthropic_total_tokens.
	TotalTokens *int `json:"total_tokens,omitempty"`

	// Extra carries the usage members dorang does not model. This family keeps
	// adding them and every one is a number somebody is billed for:
	// cache_creation's per-TTL breakdown, server_tool_use's web-search request
	// count, service_tier. Re-serializing from the fields above without this
	// map returns 200 having deleted them.
	Extra map[string]json.RawMessage `json:"-"`
}

var usageKnown = knownKeys("input_tokens", "cache_creation_input_tokens",
	"cache_read_input_tokens", "output_tokens", "total_tokens")

// MarshalJSON implements [encoding/json.Marshaler].
func (u Usage) MarshalJSON() ([]byte, error) {
	type alias Usage
	return marshalWithExtra(alias(u), u.Extra, usageKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler]. Response-only type, so
// json.Unmarshal rather than the strict filter — see [strictUnmarshal].
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

// ---------------------------------------------------------------------------
// count_tokens
// ---------------------------------------------------------------------------

// CountTokensResponse is the entire body of POST /v1/messages/count_tokens.
//
// COMPATIBILITY 6.9: exactly {"input_tokens": <number>}. Not a usage object,
// not an envelope. The route also accepts ?beta=true, which changes nothing
// about the response shape — see [BetaQueryParam].
type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// BetaQueryParam is the query parameter the count_tokens route accepts
// (COMPATIBILITY 6.9). It is accepted and has no effect on the response shape,
// which is why it is a constant here rather than a field anywhere.
const BetaQueryParam = "beta"

// MarshalCountTokens renders the count_tokens body.
func MarshalCountTokens(inputTokens int) ([]byte, error) {
	return Marshal(CountTokensResponse{InputTokens: inputTokens})
}
