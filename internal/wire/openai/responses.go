package openai

import (
	"encoding/json"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The Responses API: POST /v1/responses and its sub-resources.
//
// It is the third of the three shapes DESIGN §10.7 makes normative, and the
// only one that carries server-side conversation state. Everything about the
// naming differences — input[] rather than messages[], instructions rather than
// a system message, max_output_tokens rather than max_completion_tokens, flat
// tools rather than {"type":"function","function":{…}} — comes from that table
// rather than from this file's judgement.
//
// Two things here are not naming:
//
//   - The item list is a FLAT sequence of typed items, not a list of messages.
//     A tool call and its result are siblings of the message that preceded them,
//     where chat-completions nests one inside an assistant message and puts the
//     other in a message of its own. Converting is a regroup, not a rename.
//   - `store` and `previous_response_id` have no wire equivalent in either
//     other family (§10.7), so they cross by being STATE rather than by being
//     translated. internal/app owns the store; this file only carries the
//     fields.

// Object values on this surface.
const (
	ObjectResponse     = "response"
	ObjectResponseList = "list"
)

// Response statuses.
const (
	StatusCompleted  = "completed"
	StatusIncomplete = "incomplete"
	StatusFailed     = "failed"
	StatusInProgress = "in_progress"
	StatusCancelled  = "cancelled"
)

// Item types on the Responses wire.
const (
	ItemMessage            = "message"
	ItemFunctionCall       = "function_call"
	ItemFunctionCallOutput = "function_call_output"
	ItemReasoning          = "reasoning"
)

// Content part types on the Responses wire.
const (
	PartInputText   = "input_text"
	PartInputImage  = "input_image"
	PartInputFile   = "input_file"
	PartOutputText  = "output_text"
	PartSummaryText = "summary_text"
)

// ---------------------------------------------------------------------------
// Request
// ---------------------------------------------------------------------------

// ResponsesRequest is a Responses-API request.
type ResponsesRequest struct {
	Model string        `json:"model"`
	Input ResponseInput `json:"input"`

	Instructions *string `json:"instructions,omitempty"`

	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`

	Tools []ResponsesTool `json:"tools,omitempty"`
	// ToolChoice is kept raw for the same reason the chat surface keeps it raw:
	// a provider-native form dorang does not model must still pass through.
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`

	Text      *ResponsesText      `json:"text,omitempty"`
	Reasoning *ResponsesReasoning `json:"reasoning,omitempty"`

	// Store defaults to true on this surface, so a nil pointer is NOT false.
	Store              *bool  `json:"store,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`

	Stream      bool              `json:"stream,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	User        string            `json:"user,omitempty"`
	ServiceTier string            `json:"service_tier,omitempty"`
	Include     []string          `json:"include,omitempty"`
	Truncation  string            `json:"truncation,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var responsesRequestKnown = knownKeys("model", "input", "instructions",
	"max_output_tokens", "temperature", "top_p", "tools", "tool_choice",
	"parallel_tool_calls", "text", "reasoning", "store", "previous_response_id",
	"stream", "metadata", "user", "service_tier", "include", "truncation")

// MarshalJSON implements [encoding/json.Marshaler].
func (r ResponsesRequest) MarshalJSON() ([]byte, error) {
	type alias ResponsesRequest
	return marshalWithExtra(alias(r), r.Extra, responsesRequestKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler] with case-SENSITIVE
// field matching (COMPATIBILITY 2.0).
func (r *ResponsesRequest) UnmarshalJSON(b []byte) error {
	type alias ResponsesRequest
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, responsesRequestKnown)
	if err != nil {
		return err
	}
	*r = ResponsesRequest(a)
	r.Extra = extra
	return nil
}

// ResponseInput is the string-or-array input field.
type ResponseInput struct {
	// Text is meaningful when Items is nil.
	Text  string
	Items []ResponseItem
}

// MarshalJSON implements [encoding/json.Marshaler].
func (i ResponseInput) MarshalJSON() ([]byte, error) {
	if i.Items != nil {
		return Marshal(i.Items)
	}
	return Marshal(i.Text)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (i *ResponseInput) UnmarshalJSON(b []byte) error {
	b = trimSpace(b)
	*i = ResponseInput{}
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	switch b[0] {
	case '"':
		return json.Unmarshal(b, &i.Text)
	case '[':
		var items []ResponseItem
		if err := json.Unmarshal(b, &items); err != nil {
			return err
		}
		if items == nil {
			items = []ResponseItem{}
		}
		i.Items = items
		return nil
	default:
		return errorString("openai: responses input must be a string or an array")
	}
}

// ResponseItem is one element of the flat item sequence.
//
// The type field is optional inbound — a bare {"role","content"} is a message —
// and always present outbound.
type ResponseItem struct {
	Type   string `json:"type,omitempty"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`

	// Message fields. Content is a POINTER so that an item which is not a
	// message — a function_call, a reasoning block — omits it entirely. A
	// struct with its own MarshalJSON is never "empty" to encoding/json, so
	// omitempty on a value would emit `"content":""` on every tool call.
	Role    string           `json:"role,omitempty"`
	Content *ResponseContent `json:"content,omitempty"`

	// Function-call fields.
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// Function-call-output fields.
	Output string `json:"output,omitempty"`

	// Reasoning fields.
	Summary          []ResponsePart `json:"summary,omitempty"`
	EncryptedContent *string        `json:"encrypted_content,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var responseItemKnown = knownKeys("type", "id", "status", "role", "content",
	"call_id", "name", "arguments", "output", "summary", "encrypted_content")

// MarshalJSON implements [encoding/json.Marshaler].
func (i ResponseItem) MarshalJSON() ([]byte, error) {
	type alias ResponseItem
	return marshalWithExtra(alias(i), i.Extra, responseItemKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (i *ResponseItem) UnmarshalJSON(b []byte) error {
	type alias ResponseItem
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, responseItemKnown)
	if err != nil {
		return err
	}
	*i = ResponseItem(a)
	i.Extra = extra
	return nil
}

// ResponseContent is the string-or-array content of a message item.
type ResponseContent struct {
	Text  string
	Parts []ResponsePart
}

// MarshalJSON implements [encoding/json.Marshaler].
func (c ResponseContent) MarshalJSON() ([]byte, error) {
	if c.Parts != nil {
		return Marshal(c.Parts)
	}
	return Marshal(c.Text)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (c *ResponseContent) UnmarshalJSON(b []byte) error {
	b = trimSpace(b)
	*c = ResponseContent{}
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	switch b[0] {
	case '"':
		return json.Unmarshal(b, &c.Text)
	case '[':
		var parts []ResponsePart
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		if parts == nil {
			parts = []ResponsePart{}
		}
		c.Parts = parts
		return nil
	default:
		return errorString("openai: responses content must be a string or an array")
	}
}

// ResponsePart is one typed content part.
type ResponsePart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// ImageURL is a URL or a data: URL on an input_image part.
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	Detail   string `json:"detail,omitempty"`

	Refusal string `json:"refusal,omitempty"`
	// Annotations is emitted as [] rather than omitted on an output_text part,
	// because clients index into it. It is a POINTER for exactly that reason:
	// omitempty drops an empty slice, so a value field could not distinguish
	// "no annotations" from "not an output_text part".
	Annotations *[]json.RawMessage `json:"annotations,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var responsePartKnown = knownKeys("type", "text", "image_url", "file_id",
	"filename", "file_data", "detail", "refusal", "annotations")

// MarshalJSON implements [encoding/json.Marshaler].
func (p ResponsePart) MarshalJSON() ([]byte, error) {
	type alias ResponsePart
	return marshalWithExtra(alias(p), p.Extra, responsePartKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (p *ResponsePart) UnmarshalJSON(b []byte) error {
	type alias ResponsePart
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, responsePartKnown)
	if err != nil {
		return err
	}
	*p = ResponsePart(a)
	p.Extra = extra
	return nil
}

// ResponsesTool is a flat tool declaration.
//
// "Flat" is the whole difference from the chat surface: name, description and
// parameters sit on the tool rather than inside a function sub-object
// (DESIGN §10.7).
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var responsesToolKnown = knownKeys("type", "name", "description", "parameters", "strict")

// MarshalJSON implements [encoding/json.Marshaler].
func (t ResponsesTool) MarshalJSON() ([]byte, error) {
	type alias ResponsesTool
	return marshalWithExtra(alias(t), t.Extra, responsesToolKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (t *ResponsesTool) UnmarshalJSON(b []byte) error {
	type alias ResponsesTool
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, responsesToolKnown)
	if err != nil {
		return err
	}
	*t = ResponsesTool(a)
	t.Extra = extra
	return nil
}

// ResponsesText is the text object, which is where response_format moved to.
type ResponsesText struct {
	Format *ResponsesFormat `json:"format,omitempty"`
}

// ResponsesFormat is text.format — response_format flattened by one level.
type ResponsesFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ResponsesReasoning is the reasoning object.
type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ---------------------------------------------------------------------------
// Response
// ---------------------------------------------------------------------------

// ResponsesResponse is a complete Responses-API answer.
type ResponsesResponse struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	CreatedAt int64  `json:"created_at"`
	Status    string `json:"status"`

	Error              json.RawMessage     `json:"error"`
	IncompleteDetails  *IncompleteDetails  `json:"incomplete_details"`
	Instructions       *string             `json:"instructions"`
	MaxOutputTokens    *int                `json:"max_output_tokens"`
	Model              string              `json:"model"`
	Output             []ResponseItem      `json:"output"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID *string             `json:"previous_response_id"`
	Reasoning          *ResponsesReasoning `json:"reasoning,omitempty"`
	Store              *bool               `json:"store,omitempty"`
	Temperature        *float64            `json:"temperature"`
	Text               *ResponsesText      `json:"text,omitempty"`
	ToolChoice         json.RawMessage     `json:"tool_choice,omitempty"`
	Tools              []ResponsesTool     `json:"tools"`
	TopP               *float64            `json:"top_p"`
	Truncation         string              `json:"truncation,omitempty"`
	Usage              *ResponsesUsage     `json:"usage,omitempty"`
	User               *string             `json:"user,omitempty"`
	Metadata           map[string]string   `json:"metadata,omitempty"`
	ServiceTier        string              `json:"service_tier,omitempty"`
}

// IncompleteDetails says why a response stopped early.
type IncompleteDetails struct {
	Reason string `json:"reason"`
}

// ResponsesUsage is this family's usage block.
//
// input_tokens is INCLUSIVE of cached reads, which is the convention
// canonical.Usage documents and the one dorang normalizes to (DESIGN §10.7).
// Reading it as exclusive is the inversion that section records: it would bill
// every cached request roughly 1.8x over with no error anywhere.
type ResponsesUsage struct {
	InputTokens         int                  `json:"input_tokens"`
	InputTokensDetails  *InputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        int                  `json:"output_tokens"`
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         int                  `json:"total_tokens"`

	Extra map[string]json.RawMessage `json:"-"`
}

var responsesUsageKnown = knownKeys("input_tokens", "input_tokens_details",
	"output_tokens", "output_tokens_details", "total_tokens")

// MarshalJSON implements [encoding/json.Marshaler].
func (u ResponsesUsage) MarshalJSON() ([]byte, error) {
	type alias ResponsesUsage
	return marshalWithExtra(alias(u), u.Extra, responsesUsageKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (u *ResponsesUsage) UnmarshalJSON(b []byte) error {
	type alias ResponsesUsage
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, responsesUsageKnown)
	if err != nil {
		return err
	}
	*u = ResponsesUsage(a)
	u.Extra = extra
	return nil
}

// InputTokensDetails breaks the prompt count down.
//
// CachedTokens has NO omitempty. A zero here is a measurement — see [Usage].
type InputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`

	// Extra carries text_tokens, image_tokens, audio_tokens and every other
	// breakdown member dorang does not model. They are counts on somebody's
	// invoice, not decoration.
	Extra map[string]json.RawMessage `json:"-"`
}

var inputDetailsKnown = knownKeys("cached_tokens")

// MarshalJSON implements [encoding/json.Marshaler].
func (d InputTokensDetails) MarshalJSON() ([]byte, error) {
	type alias InputTokensDetails
	return marshalWithExtra(alias(d), d.Extra, inputDetailsKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (d *InputTokensDetails) UnmarshalJSON(b []byte) error {
	type alias InputTokensDetails
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, inputDetailsKnown)
	if err != nil {
		return err
	}
	*d = InputTokensDetails(a)
	d.Extra = extra
	return nil
}

// OutputTokensDetails breaks the completion count down.
type OutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`

	Extra map[string]json.RawMessage `json:"-"`
}

var outputDetailsKnown = knownKeys("reasoning_tokens")

// MarshalJSON implements [encoding/json.Marshaler].
func (d OutputTokensDetails) MarshalJSON() ([]byte, error) {
	type alias OutputTokensDetails
	return marshalWithExtra(alias(d), d.Extra, outputDetailsKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (d *OutputTokensDetails) UnmarshalJSON(b []byte) error {
	type alias OutputTokensDetails
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, outputDetailsKnown)
	if err != nil {
		return err
	}
	*d = OutputTokensDetails(a)
	d.Extra = extra
	return nil
}

// ---------------------------------------------------------------------------
// Decode: wire -> canonical
// ---------------------------------------------------------------------------

// DecodeResponsesRequest parses Responses-API request bytes.
func DecodeResponsesRequest(b []byte) (*canonical.Request, error) {
	var w ResponsesRequest
	if err := strictUnmarshal(b, &w); err != nil {
		return nil, err
	}
	return ResponsesRequestToCanonical(&w)
}

// ResponsesRequestToCanonical converts a decoded Responses request.
func ResponsesRequestToCanonical(w *ResponsesRequest) (*canonical.Request, error) {
	if w == nil {
		return nil, errNilRequest
	}
	out := &canonical.Request{
		// Opaque, copied whole (DESIGN §2.1).
		Model:              w.Model,
		MaxTokens:          w.MaxOutputTokens,
		Temperature:        w.Temperature,
		TopP:               w.TopP,
		Stream:             w.Stream,
		Metadata:           w.Metadata,
		User:               w.User,
		ServiceTier:        w.ServiceTier,
		ParallelToolCalls:  w.ParallelToolCalls,
		PreviousResponseID: w.PreviousResponseID,
		Store:              w.Store,
		Extra:              w.Extra,
	}
	if w.Instructions != nil {
		// instructions IS the system prompt on this wire (DESIGN §10.7), so it
		// decodes to System and needs no flag: every encoder that has an
		// instructions field emits System into it, and every encoder that does
		// not emits a system message. Recording which spelling arrived would be
		// a field nothing reads.
		out.System = canonical.Content{canonical.TextBlock(*w.Instructions)}
	}
	if w.Text != nil && w.Text.Format != nil {
		f := w.Text.Format
		out.ResponseFormat = &canonical.ResponseFormat{
			Kind: f.Type, Name: f.Name, Description: f.Description,
			Schema: f.Schema, Strict: f.Strict,
		}
	}
	if w.Reasoning != nil {
		out.Reasoning = &canonical.Reasoning{Effort: w.Reasoning.Effort, Summary: w.Reasoning.Summary}
	}
	for i := range w.Tools {
		t := &w.Tools[i]
		out.Tools = append(out.Tools, canonical.Tool{
			Type: t.Type, Name: t.Name, Description: t.Description,
			Parameters: t.Parameters, Strict: t.Strict, Extra: t.Extra,
		})
	}
	if len(w.ToolChoice) > 0 {
		tc, err := decodeToolChoice(w.ToolChoice)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = tc
	}
	if w.ParallelToolCalls != nil && out.ToolChoice != nil {
		out.ToolChoice.DisableParallel = ptr(!*w.ParallelToolCalls)
	}
	out.Messages = itemsToMessages(w.Input)
	return out, nil
}

// itemsToMessages regroups the flat item sequence into messages.
//
// This is the conversion that is not a rename. A function_call is a sibling of
// the assistant message it belongs to here and a member of it in the other
// family, so consecutive assistant-ish items are folded into one message and a
// function_call_output becomes a tool message of its own.
func itemsToMessages(in ResponseInput) []canonical.Message {
	if in.Items == nil {
		if in.Text == "" {
			return nil
		}
		return []canonical.Message{canonical.TextMessage(canonical.RoleUser, in.Text)}
	}
	var out []canonical.Message
	appendTo := func(role canonical.Role, blocks ...canonical.Block) {
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, canonical.Message{Role: role, Content: blocks})
	}
	for i := range in.Items {
		it := &in.Items[i]
		switch it.Type {
		case ItemFunctionCall:
			appendTo(canonical.RoleAssistant, canonical.ToolUseBlock(
				callID(it), it.Name, rawOrEmptyObject(it.Arguments)))
		case ItemFunctionCallOutput:
			appendTo(canonical.RoleTool, canonical.ToolResultBlock(
				callID(it), canonical.TextBlock(it.Output)))
		case ItemReasoning:
			b := canonical.Block{Kind: canonical.KindThinking, Text: summaryText(it.Summary)}
			if it.EncryptedContent != nil && *it.EncryptedContent != "" {
				// The encrypted reasoning handle is integrity material. dorang
				// stores and replays it byte-identically and never synthesizes
				// one (DESIGN §10.2).
				b.Thinking = &canonical.Thinking{Signature: *it.EncryptedContent}
			}
			appendTo(canonical.RoleAssistant, b)
		default: // ItemMessage and the bare {"role","content"} form
			role := canonical.Role(it.Role)
			if role == "" {
				role = canonical.RoleUser
			}
			appendTo(role, partsToBlocks(it.Content)...)
		}
	}
	return out
}

// emptyAnnotations is the [] an output_text part always carries.
func emptyAnnotations() *[]json.RawMessage {
	a := []json.RawMessage{}
	return &a
}

func callID(it *ResponseItem) string {
	if it.CallID != "" {
		return it.CallID
	}
	return it.ID
}

func summaryText(parts []ResponsePart) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0].Text
	}
	n := 0
	for i := range parts {
		n += len(parts[i].Text) + 1
	}
	out := make([]byte, 0, n)
	for i := range parts {
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, parts[i].Text...)
	}
	return string(out)
}

func partsToBlocks(c *ResponseContent) []canonical.Block {
	if c == nil {
		return nil
	}
	if c.Parts == nil {
		if c.Text == "" {
			return nil
		}
		return []canonical.Block{canonical.TextBlock(c.Text)}
	}
	out := make([]canonical.Block, 0, len(c.Parts))
	for i := range c.Parts {
		p := &c.Parts[i]
		var b canonical.Block
		switch p.Type {
		case PartInputText, PartOutputText, PartSummaryText, PartText:
			b = canonical.TextBlock(p.Text)
		case PartInputImage:
			switch {
			case p.FileID != "":
				b = canonical.Block{Kind: canonical.KindImage, Source: &canonical.Source{
					Kind: canonical.SourceFileID, Data: p.FileID,
				}}
			default:
				b = canonical.ImageURLBlock(p.ImageURL)
			}
			if b.Source != nil {
				b.Source.Detail = p.Detail
			}
		case PartInputFile:
			switch {
			case p.FileID != "":
				b = canonical.Block{Kind: canonical.KindDocument, Source: &canonical.Source{
					Kind: canonical.SourceFileID, Data: p.FileID, Name: p.Filename,
				}}
			default:
				mt, data, ok := splitDataURL(p.FileData)
				if ok {
					b = canonical.DocumentBlock(mt, data, p.Filename)
				} else {
					b = canonical.Block{Kind: canonical.KindDocument, Source: &canonical.Source{
						Kind: canonical.SourceURL, Data: p.FileData, Name: p.Filename,
					}}
				}
			}
		case PartRefusal:
			b = canonical.TextBlock(p.Refusal)
		default:
			b = canonical.Block{Kind: canonical.BlockKind(p.Type), Text: p.Text, Extra: p.Extra}
			out = append(out, b)
			continue
		}
		if len(p.Extra) > 0 {
			b.Extra = p.Extra
		}
		out = append(out, b)
	}
	return out
}

// MarshalResponsesItems renders a conversation as a Responses item array.
//
// It is what the server-side store persists (DESIGN §9.2 [R1-C7]): a stored
// response has to be replayable as the history of the next one, and the item
// array is the form that replay reads.
func MarshalResponsesItems(msgs []canonical.Message) ([]byte, error) {
	items := messagesToItems(msgs, nil, nil)
	if items == nil {
		items = []ResponseItem{}
	}
	return Marshal(items)
}

// ResponsesItemsToMessages is the inverse: a stored item array read back as
// conversation history.
func ResponsesItemsToMessages(b []byte) ([]canonical.Message, error) {
	var items []ResponseItem
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, err
	}
	return itemsToMessages(ResponseInput{Items: items}), nil
}

// ItemsToMessages converts an already-decoded item list.
func ItemsToMessages(items []ResponseItem) []canonical.Message {
	return itemsToMessages(ResponseInput{Items: items})
}

// IsResponsesAnswer reports whether raw response bytes are an answer of the
// Responses family rather than a chat completion.
//
// It exists because both shapes arrive on the same route. internal/backend
// addresses OpResponses at /v1/chat/completions today — a caller's Responses
// request is already in the neutral form by then, and the chat route reaches
// every deployment while the Responses route reaches some — but the answer's
// shape is the HOST's choice, not that route's: a host that serves both APIs may
// answer either, and switching the operation to /v1/responses the day a stream
// decoder exists makes this shape the ordinary one.
//
// Read by the chat decoder, a Responses answer is not merely mis-metered. Its
// assistant turn is in `output` rather than `choices` and its counts are in
// `input_tokens` rather than `prompt_tokens`, so it decodes to a successful
// response with no content and no usage at all — the shape [ErrNotAResponse]
// exists to refuse, arriving through a decoder that accepted it.
//
// The test is a discriminator OR a payload member, the same either-ground rule
// [IsChatCompletion] states, with one addition: a `choices` array REFUSES. Chat
// is this package's default shape and the one every OpenAI-compatible host
// emits; a body carrying `choices` is a chat completion whatever else it also
// carries, and the usage-block half of the same disagreement is settled inside
// [usageToCanonical] where it belongs.
func IsResponsesAnswer(b []byte) bool {
	if !isJSONObject(b) {
		return false
	}
	var probe struct {
		Object  string          `json:"object"`
		Choices json.RawMessage `json:"choices"`
		Output  json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return false
	}
	if probe.Choices != nil {
		return false
	}
	return probe.Object == ObjectResponse || probe.Output != nil
}

// DecodeResponsesResponse parses a Responses answer into the neutral form.
//
// json.Unmarshal and not the strict filter: these bytes are a backend's, not a
// caller's (see [DecodeResponse]).
func DecodeResponsesResponse(b []byte, opt *DecodeOptions) (*canonical.Response, error) {
	var w ResponsesResponse
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return ResponsesResponseToCanonical(&w, opt)
}

// ResponsesResponseToCanonical converts a decoded Responses answer.
func ResponsesResponseToCanonical(w *ResponsesResponse, opt *DecodeOptions) (*canonical.Response, error) {
	if w == nil {
		return nil, errorString("openai: nil responses response")
	}
	out := &canonical.Response{ID: w.ID, Model: w.Model, Created: w.CreatedAt}
	if opt != nil && opt.Model != "" {
		out.Model = opt.Model
	}
	if w.ServiceTier != "" {
		out.ServiceTier = w.ServiceTier
	}
	if w.Usage != nil {
		u := &canonical.Usage{
			// Inclusive, as this family reports it (DESIGN §10.7).
			InputTokens:  w.Usage.InputTokens,
			OutputTokens: w.Usage.OutputTokens,
			Reported:     canonical.UsageInput | canonical.UsageOutput,
		}
		if w.Usage.InputTokensDetails != nil {
			u.CacheReadTokens = w.Usage.InputTokensDetails.CachedTokens
			u.Report(canonical.UsageCacheRead)
		}
		if w.Usage.OutputTokensDetails != nil {
			u.ReasoningTokens = w.Usage.OutputTokensDetails.ReasoningTokens
			u.Report(canonical.UsageReasoning)
		}
		out.Usage = u
		out.UsageExtra = responsesUsageExtra(w.Usage)
	}

	msg := canonical.Message{Role: canonical.RoleAssistant}
	for i := range w.Output {
		it := &w.Output[i]
		switch it.Type {
		case ItemFunctionCall:
			msg.Content = append(msg.Content, canonical.ToolUseBlock(
				callID(it), opt.names().Restore(it.Name), rawOrEmptyObject(it.Arguments)))
		case ItemReasoning:
			b := canonical.Block{Kind: canonical.KindThinking, Text: summaryText(it.Summary)}
			if it.EncryptedContent != nil && *it.EncryptedContent != "" {
				b.Thinking = &canonical.Thinking{Signature: *it.EncryptedContent}
			}
			msg.Content = append(msg.Content, b)
		default:
			for j := range it.Content.Parts {
				if it.Content.Parts[j].Type == PartRefusal {
					msg.Refusal = it.Content.Parts[j].Refusal
				}
			}
			msg.Content = append(msg.Content, partsToBlocks(it.Content)...)
		}
	}
	choice := canonical.Choice{Message: msg}
	choice.StopReason, choice.NativeStopReason = responsesStopReason(w, msg)
	out.Choices = []canonical.Choice{choice}
	return out, nil
}

// responsesUsageExtra collects the members of a Responses usage object, and of
// its two breakdown sub-objects, that no canonical counter names.
//
// It fills the SAME two maps [usageExtraOf] does. The families spell the
// breakdown differently and mean the same level of the same concept by it, so a
// member that arrived under one spelling must be readable by an encoder for the
// other; keeping a third pair of maps would only record which vendor's word for
// "details" the upstream happened to use.
func responsesUsageExtra(u *ResponsesUsage) *canonical.UsageExtra {
	if u == nil {
		return nil
	}
	out := &canonical.UsageExtra{Usage: u.Extra}
	if u.InputTokensDetails != nil {
		out.PromptDetails = u.InputTokensDetails.Extra
	}
	if u.OutputTokensDetails != nil {
		out.CompletionDetails = u.OutputTokensDetails.Extra
	}
	if out.Empty() {
		return nil
	}
	return out
}

// responsesStopReason maps status + incomplete_details.reason onto the neutral
// enumeration (DESIGN §10.7).
func responsesStopReason(w *ResponsesResponse, msg canonical.Message) (canonical.StopReason, string) {
	if w.IncompleteDetails != nil && w.IncompleteDetails.Reason != "" {
		native := w.IncompleteDetails.Reason
		switch native {
		case "max_output_tokens":
			return canonical.StopMaxTokens, ""
		case "content_filter":
			return canonical.StopContentFilter, ""
		}
		return canonical.StopEndTurn, native
	}
	switch w.Status {
	case "", StatusCompleted:
		for i := range msg.Content {
			if msg.Content[i].Kind == canonical.KindToolUse {
				return canonical.StopToolUse, ""
			}
		}
		if msg.Refusal != "" {
			return canonical.StopRefusal, ""
		}
		return canonical.StopEndTurn, ""
	case StatusIncomplete:
		return canonical.StopMaxTokens, w.Status
	case StatusFailed, StatusCancelled:
		return canonical.StopError, w.Status
	default:
		return canonical.StopUnspecified, w.Status
	}
}

// ---------------------------------------------------------------------------
// Encode: canonical -> wire
// ---------------------------------------------------------------------------

// MarshalResponsesRequest encodes a neutral request as Responses bytes.
func MarshalResponsesRequest(req *canonical.Request, opt *EncodeOptions) ([]byte, error) {
	w, err := EncodeResponsesRequest(req, opt)
	if err != nil {
		return nil, err
	}
	return Marshal(w)
}

// EncodeResponsesRequest converts a neutral request to the Responses shape.
func EncodeResponsesRequest(req *canonical.Request, opt *EncodeOptions) (*ResponsesRequest, error) {
	if req == nil {
		return nil, errNilRequest
	}
	loss := opt.loss()
	out := &ResponsesRequest{
		Model:              req.Model,
		MaxOutputTokens:    req.MaxTokens,
		Temperature:        req.Temperature,
		TopP:               req.TopP,
		Stream:             req.Stream,
		Metadata:           req.Metadata,
		User:               req.User,
		ServiceTier:        req.ServiceTier,
		ParallelToolCalls:  req.ParallelToolCalls,
		Store:              req.Store,
		PreviousResponseID: req.PreviousResponseID,
		Extra:              req.Extra,
	}
	if opt != nil && opt.Model != "" {
		out.Model = opt.Model
	}
	if len(req.System) > 0 {
		s, exact := req.System.Plain()
		if !exact {
			// instructions is one string. Per-block attributes on a structured
			// system prompt have nowhere to go (DESIGN §10.1).
			loss.Downgrade(canonical.ConstructStructuredSystem, "instructions")
		}
		out.Instructions = ptr(s)
	}
	if len(req.Stop) > 0 {
		// This family has no stop_sequences at all (§10.7).
		loss.DropParam("stop")
	}
	if req.Seed != nil {
		loss.DropParam("seed")
	}
	if req.TopK != nil {
		loss.DropParam("top_k")
	}
	if req.ResponseFormat != nil {
		f := req.ResponseFormat
		out.Text = &ResponsesText{Format: &ResponsesFormat{
			Type: f.Kind, Name: f.Name, Description: f.Description,
			Schema: f.Schema, Strict: f.Strict,
		}}
	}
	if req.Reasoning != nil {
		out.Reasoning = &ResponsesReasoning{
			Effort: req.Reasoning.Effort, Summary: req.Reasoning.Summary,
		}
	}
	names := opt.toolNames()
	for i := range req.Tools {
		t := &req.Tools[i]
		names.Declare(t.Name)
		typ := t.Type
		if typ == "" {
			typ = "function"
		}
		out.Tools = append(out.Tools, ResponsesTool{
			Type: typ, Name: names.Shorten(t.Name, opt.warn()), Description: t.Description,
			Parameters: t.Parameters, Strict: t.Strict, Extra: t.Extra,
		})
		if t.CacheControl != nil {
			loss.Downgrade(canonical.ConstructCacheBreakpoints, "tools")
		}
	}
	if req.ToolChoice != nil {
		tc, err := encodeResponsesToolChoice(req.ToolChoice, names)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = tc
		if req.ToolChoice.DisableParallel != nil && out.ParallelToolCalls == nil {
			out.ParallelToolCalls = ptr(!*req.ToolChoice.DisableParallel)
		}
	}
	out.Input = ResponseInput{Items: messagesToItems(req.Messages, names, loss)}
	return out, nil
}

func encodeResponsesToolChoice(tc *canonical.ToolChoice, names *ToolNames) (json.RawMessage, error) {
	if tc.Mode == canonical.ToolChoiceTool && tc.Name != "" {
		return Marshal(map[string]string{"type": "function", "name": names.Shorten(tc.Name, nil)})
	}
	if tc.Mode == "" {
		return nil, nil
	}
	return Marshal(string(tc.Mode))
}

// messagesToItems flattens messages back into the item sequence.
func messagesToItems(msgs []canonical.Message, names *ToolNames, loss *canonical.LossReport) []ResponseItem {
	out := make([]ResponseItem, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		var parts []ResponsePart
		input := m.Role != canonical.RoleAssistant
		flush := func() {
			if len(parts) == 0 {
				return
			}
			out = append(out, ResponseItem{
				Type: ItemMessage, Role: string(m.Role),
				Content: &ResponseContent{Parts: parts},
			})
			parts = nil
		}
		for j := range m.Content {
			b := &m.Content[j]
			switch b.Kind {
			case canonical.KindToolUse:
				flush()
				if b.ToolUse == nil {
					continue
				}
				args := string(b.ToolUse.Input)
				if args == "" {
					args = "{}"
				}
				out = append(out, ResponseItem{
					Type: ItemFunctionCall, CallID: b.ToolUse.ID,
					Name: names.Shorten(b.ToolUse.Name, nil), Arguments: args,
				})
			case canonical.KindToolResult:
				flush()
				if b.ToolResult == nil {
					continue
				}
				text, exact := canonical.Content(b.ToolResult.Content).Plain()
				if !exact {
					// function_call_output carries one string. A result that
					// held text plus an image loses the image (§10.1).
					loss.Downgrade(canonical.ConstructMultiBlockToolResult, "input.function_call_output")
				}
				out = append(out, ResponseItem{
					Type: ItemFunctionCallOutput, CallID: b.ToolResult.ToolUseID, Output: text,
				})
			case canonical.KindThinking:
				flush()
				it := ResponseItem{Type: ItemReasoning}
				if b.Text != "" {
					it.Summary = []ResponsePart{{Type: PartSummaryText, Text: b.Text}}
				}
				if b.Thinking != nil && b.Thinking.Signature != "" {
					it.EncryptedContent = ptr(b.Thinking.Signature)
				}
				out = append(out, it)
			default:
				parts = append(parts, blockToResponsePart(b, input, loss))
			}
			if b.CacheControl != nil {
				loss.Downgrade(canonical.ConstructCacheBreakpoints, "input")
			}
		}
		if m.Refusal != "" {
			parts = append(parts, ResponsePart{Type: PartRefusal, Refusal: m.Refusal})
		}
		flush()
	}
	return out
}

func blockToResponsePart(b *canonical.Block, input bool, loss *canonical.LossReport) ResponsePart {
	switch b.Kind {
	case canonical.KindImage:
		p := ResponsePart{Type: PartInputImage, Extra: b.Extra}
		if b.Source != nil {
			p.Detail = b.Source.Detail
			if b.Source.Kind == canonical.SourceFileID {
				p.FileID = b.Source.Data
			} else {
				p.ImageURL = b.Source.DataURL()
			}
		}
		return p
	case canonical.KindDocument:
		p := ResponsePart{Type: PartInputFile, Extra: b.Extra}
		if b.Source != nil {
			p.Filename = b.Source.Name
			if b.Source.Kind == canonical.SourceFileID {
				p.FileID = b.Source.Data
			} else {
				p.FileData = b.Source.DataURL()
			}
		}
		return p
	default:
		typ := PartOutputText
		if input {
			typ = PartInputText
		}
		if b.Kind != canonical.KindText {
			loss.Downgrade(canonical.ConstructMultiBlockContent, "input."+string(b.Kind))
		}
		return ResponsePart{Type: typ, Text: b.Text, Extra: b.Extra}
	}
}

// ResponsesOptions controls a canonical -> Responses answer conversion.
//
// The echoed request fields are here rather than on the neutral response
// because they belong to the REQUEST: this family echoes what it was asked
// with, and a gateway that regenerates them from its own defaults tells the
// client it changed settings it never touched.
type ResponsesOptions struct {
	ID      string
	Created int64
	Model   string

	Instructions       *string
	MaxOutputTokens    *int
	Temperature        *float64
	TopP               *float64
	Tools              []ResponsesTool
	ToolChoice         json.RawMessage
	ParallelToolCalls  *bool
	Text               *ResponsesText
	Reasoning          *ResponsesReasoning
	Store              *bool
	PreviousResponseID string
	Truncation         string
	Metadata           map[string]string
	User               string

	ToolNames *ToolNames
}

// MarshalResponsesResponse encodes a neutral response as Responses bytes.
func MarshalResponsesResponse(r *canonical.Response, opt *ResponsesOptions) ([]byte, error) {
	w, err := EncodeResponsesResponse(r, opt)
	if err != nil {
		return nil, err
	}
	return Marshal(w)
}

// EncodeResponsesResponse converts a neutral response to the Responses shape.
func EncodeResponsesResponse(r *canonical.Response, opt *ResponsesOptions) (*ResponsesResponse, error) {
	if r == nil {
		return nil, errorString("openai: nil response")
	}
	out := &ResponsesResponse{
		ID:        r.ID,
		Object:    ObjectResponse,
		CreatedAt: r.Created,
		Model:     r.Model,
		Status:    StatusCompleted,
		// The three fields below are emitted as JSON null rather than omitted:
		// the SDK reads them unconditionally and a missing key is an
		// AttributeError where a null is None.
		Error:       json.RawMessage("null"),
		Output:      []ResponseItem{},
		Tools:       []ResponsesTool{},
		ServiceTier: r.ServiceTier,
	}
	if opt != nil {
		if opt.ID != "" {
			out.ID = opt.ID
		}
		if opt.Created != 0 {
			out.CreatedAt = opt.Created
		}
		if opt.Model != "" {
			out.Model = opt.Model
		}
		out.Instructions = opt.Instructions
		out.MaxOutputTokens = opt.MaxOutputTokens
		out.Temperature = opt.Temperature
		out.TopP = opt.TopP
		out.ParallelToolCalls = opt.ParallelToolCalls
		out.Text = opt.Text
		out.Reasoning = opt.Reasoning
		out.Store = opt.Store
		out.ToolChoice = opt.ToolChoice
		out.Truncation = opt.Truncation
		out.Metadata = opt.Metadata
		if opt.Tools != nil {
			out.Tools = opt.Tools
		}
		if opt.PreviousResponseID != "" {
			out.PreviousResponseID = ptr(opt.PreviousResponseID)
		}
		if opt.User != "" {
			out.User = ptr(opt.User)
		}
	}
	if r.Usage != nil {
		u := &ResponsesUsage{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
			TotalTokens:  r.Usage.TotalTokens(),
		}
		if r.UsageExtra != nil {
			u.Extra = r.UsageExtra.Usage
		}
		// Reported, not `> 0`: a breakdown the backend stated is emitted as
		// stated, zero included, and one dorang synthesized is still omitted.
		// The same rule as [EncodeUsage], for the same billing reason — this
		// surface's clients read input_tokens_details.cached_tokens exactly the
		// way a chat client reads prompt_tokens_details.cached_tokens.
		if r.Usage.CacheReadTokens > 0 || r.Usage.Reports(canonical.UsageCacheRead) {
			u.InputTokensDetails = &InputTokensDetails{CachedTokens: r.Usage.CacheReadTokens}
			if r.UsageExtra != nil {
				u.InputTokensDetails.Extra = r.UsageExtra.PromptDetails
			}
		}
		if r.Usage.ReasoningTokens > 0 || r.Usage.Reports(canonical.UsageReasoning) {
			u.OutputTokensDetails = &OutputTokensDetails{ReasoningTokens: r.Usage.ReasoningTokens}
			if r.UsageExtra != nil {
				u.OutputTokensDetails.Extra = r.UsageExtra.CompletionDetails
			}
		}
		out.Usage = u
	}
	if len(r.Choices) == 0 {
		return out, nil
	}
	c := &r.Choices[0]
	var names *ToolNames
	if opt != nil {
		names = opt.ToolNames
	}
	out.Output = outputItems(&c.Message, names)
	switch c.StopReason {
	case canonical.StopMaxTokens:
		out.Status = StatusIncomplete
		out.IncompleteDetails = &IncompleteDetails{Reason: "max_output_tokens"}
	case canonical.StopContentFilter:
		out.Status = StatusIncomplete
		out.IncompleteDetails = &IncompleteDetails{Reason: "content_filter"}
	case canonical.StopError:
		out.Status = StatusFailed
	}
	return out, nil
}

// outputItems renders an assistant message as output items.
func outputItems(m *canonical.Message, names *ToolNames) []ResponseItem {
	out := make([]ResponseItem, 0, len(m.Content)+1)
	var parts []ResponsePart
	flush := func() {
		if len(parts) == 0 {
			return
		}
		out = append(out, ResponseItem{
			Type: ItemMessage, Status: StatusCompleted,
			Role: string(canonical.RoleAssistant), Content: &ResponseContent{Parts: parts},
		})
		parts = nil
	}
	for i := range m.Content {
		b := &m.Content[i]
		switch b.Kind {
		case canonical.KindToolUse:
			flush()
			if b.ToolUse == nil {
				continue
			}
			args := string(b.ToolUse.Input)
			if args == "" {
				args = "{}"
			}
			out = append(out, ResponseItem{
				Type: ItemFunctionCall, Status: StatusCompleted,
				CallID: b.ToolUse.ID, Name: names.Restore(b.ToolUse.Name), Arguments: args,
			})
		case canonical.KindThinking:
			flush()
			it := ResponseItem{Type: ItemReasoning, Summary: []ResponsePart{}}
			if b.Text != "" {
				it.Summary = []ResponsePart{{Type: PartSummaryText, Text: b.Text}}
			}
			if b.Thinking != nil && b.Thinking.Signature != "" {
				it.EncryptedContent = ptr(b.Thinking.Signature)
			}
			out = append(out, it)
		default:
			parts = append(parts, ResponsePart{
				Type: PartOutputText, Text: b.Text, Annotations: emptyAnnotations(),
			})
		}
	}
	if m.Refusal != "" {
		parts = append(parts, ResponsePart{Type: PartRefusal, Refusal: m.Refusal})
	}
	flush()
	return out
}
