package openai

import (
	"encoding/json"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
)

// EncodeOptions controls a canonical -> OpenAI conversion.
type EncodeOptions struct {
	// Capabilities is what the target can express. Zero means
	// [DefaultCapabilities].
	Capabilities canonical.Capability

	// Loss accumulates what the conversion removed. A nil Loss discards the
	// report, which is only correct for a same-protocol pass-through where
	// nothing can be lost.
	//
	// The encoder records; it does not decide. Whether a structural downgrade is
	// a 400 or an accepted loss depends on the caller's x-dorang-allow-lossy
	// header, which the encoder never sees (DESIGN §10.1).
	Loss *canonical.LossReport

	// ToolNames records tool-name shortening (COMPATIBILITY 5.3). It must be the
	// SAME value the response decoder of this exchange is given: the rule has two
	// halves and a mapping only one of them can reach is worse than no mapping at
	// all.
	//
	// A nil ToolNames does not shorten. That is deliberate and it is the reason
	// this field is no longer filled in lazily: an encoder that allocates its own
	// mapping shortens the name, discards the mapping with its options struct,
	// and leaves the caller holding a truncated name it never declared and cannot
	// match. An over-long name forwarded intact is refused by the upstream with a
	// message naming the tool, which is a failure an operator can act on.
	ToolNames *ToolNames

	// Model overrides the model field, which is how the real upstream id
	// reaches the backend while the client-facing name stays in the response
	// (DESIGN §7.2). Empty keeps the canonical value.
	Model string

	// MaxTokensField selects the spelling of the output ceiling. Empty means
	// "max_tokens".
	//
	// COMPATIBILITY.md does not cover this and the two spellings are not
	// interchangeable in either direction: the OpenAI-compatible ecosystem
	// (vLLM, llama.cpp, ollama and most gateways) accepts only max_tokens,
	// while current OpenAI reasoning models reject it and require
	// max_completion_tokens. Defaulting to the widely-accepted spelling and
	// letting the backend adapter override is the only choice that does not
	// silently break one whole class of deployment.
	MaxTokensField string

	Warn WarnFunc
	// ForceStream and StoreFalse are for a host that refuses anything else.
	//
	// The ChatGPT Codex surface requires `stream: true` AND `store: false`, each
	// refused separately with its own 400, and they are the contract rather than
	// options an operator forgot. Applied here so a deployment states them once
	// instead of every caller discovering the refusal.
	//
	// Forcing the stream does not change what the CALLER gets: dorang's relay
	// reads it back, so a non-streaming caller still receives one buffered
	// answer. The shape is the caller's choice and the transport is not.
	ForceStream bool
	StoreFalse  bool
}

// Output-ceiling spellings for EncodeOptions.MaxTokensField.
const (
	FieldMaxTokens           = "max_tokens"
	FieldMaxCompletionTokens = "max_completion_tokens"
)

func (o *EncodeOptions) caps() canonical.Capability {
	if o == nil || o.Capabilities == 0 {
		return DefaultCapabilities
	}
	return o.Capabilities
}

func (o *EncodeOptions) loss() *canonical.LossReport {
	if o == nil {
		return nil
	}
	return o.Loss
}

func (o *EncodeOptions) warn() WarnFunc {
	if o == nil {
		return nil
	}
	return o.Warn
}

// toolNames returns the caller's mapping, which may be nil.
func (o *EncodeOptions) toolNames() *ToolNames {
	if o == nil {
		return nil
	}
	return o.ToolNames
}

// MarshalRequest encodes a neutral request as OpenAI request bytes.
func MarshalRequest(req *canonical.Request, opt *EncodeOptions) ([]byte, error) {
	w, err := EncodeRequest(req, opt)
	if err != nil {
		return nil, err
	}
	return marshalAppender(w)
}

// EncodeRequest converts a neutral request to the OpenAI wire shape.
//
// It never fails on loss. Losses are recorded in opt.Loss: droppable parameters
// by name, structural downgrades by construct and location.
func EncodeRequest(req *canonical.Request, opt *EncodeOptions) (*Request, error) {
	if req == nil {
		return nil, errNilRequest
	}
	caps := opt.caps()
	loss := opt.loss()

	// Droppable parameters first: one bit-mask subtraction reports every knob
	// the target does not have, by wire name.
	missing := caps.Missing(req.RequiredCapabilities())
	loss.DropCapability(missing.Droppable())
	// The MATERIAL half of the same subtraction — stop, n, logprobs,
	// service_tier — is a downgrade rather than a dropped knob, because its
	// absence changes the answer or the price (canonical.Material). It is
	// recorded, not refused: this encoder still does not decide policy.
	req.MaterialLoss(caps, loss)

	out := &Request{
		Model:  req.Model,
		Stream: req.Stream,
	}
	if opt != nil && opt.Model != "" {
		out.Model = opt.Model
	}
	if len(req.Extra) > 0 {
		out.Extra = req.Extra
	}

	// System: OpenAI has no top-level system field, so it becomes the first
	// message. With CapStructuredSystem the per-block attributes survive as
	// content parts; without it they do not, and that is a structural loss.
	n := len(req.Messages)
	if len(req.System) > 0 {
		n++
	}
	out.Messages = make([]Message, 0, n)
	if len(req.System) > 0 {
		sysCaps := caps
		if !caps.Has(canonical.CapStructuredSystem) {
			// Force the string form: the target wants one flat system message.
			sysCaps &^= canonical.CapMultiBlockContent
			if _, plain := req.System.Plain(); !plain {
				loss.Downgrade(canonical.ConstructStructuredSystem, "system")
			}
		}
		out.Messages = append(out.Messages, Message{
			Role:    string(canonical.RoleSystem),
			Content: encodeContent(req.System, sysCaps, loss, "system"),
		})
	}
	for i := range req.Messages {
		encodeMessage(&out.Messages, &req.Messages[i], caps, loss, opt, "messages["+strconv.Itoa(i)+"]")
	}

	// Tools.
	if len(req.Tools) > 0 {
		if !caps.Has(canonical.CapToolCalls) {
			loss.Downgrade(canonical.ConstructToolCalls, "tools")
		} else {
			names := opt.toolNames()
			out.Tools = make([]Tool, 0, len(req.Tools))
			for i := range req.Tools {
				t := &req.Tools[i]
				// A tool that is not a function — the Responses surface's
				// server-side tools (web_search, image_gen), and the namespace
				// containers some callers declare — has no spelling on the chat
				// surface, which carries function tools only. Forwarding the
				// type verbatim produces `{"type":"namespace"}`, a tool no
				// chat backend parses: measured as z.ai 1214
				// "tools[N].type: type is illegal" from a codex caller. The
				// declaration is dropped and reported on the loss ledger
				// rather than downgraded structurally: a backend that cannot
				// execute the tool would ignore the declaration at best, and
				// the caller sees the tool missing by name, which is a
				// parameter fact, not a broken conversation structure.
				if t.Type != "" && t.Type != "function" {
					loss.DropParam("tools[" + strconv.Itoa(i) + "]")
					continue
				}
				// Declared whether or not it needs shortening: the set is what
				// tells a fragmented name on the way back from a shorter name
				// that happens to be a prefix of it (COMPATIBILITY 5.3).
				names.Declare(t.Name)
				wt := Tool{Type: "function", Extra: t.Extra}
				wt.Function = &ToolFunction{
					Name:        names.Shorten(t.Name, opt.warn()),
					Description: t.Description,
					Parameters:  t.Parameters,
					Strict:      t.Strict,
				}
				if t.CacheControl != nil {
					if caps.Has(canonical.CapCacheBreakpoints) {
						wt.CacheControl = encodeCacheControl(t.CacheControl)
					} else {
						loss.Downgrade(canonical.ConstructCacheBreakpoints, "tools["+strconv.Itoa(i)+"]")
					}
				}
				out.Tools = append(out.Tools, wt)
			}
		}
	}
	if req.ToolChoice != nil && caps.Has(canonical.CapToolCalls) {
		tc, err := encodeToolChoice(req.ToolChoice, opt.toolNames(), opt.warn())
		if err != nil {
			return nil, err
		}
		out.ToolChoice = tc
		if req.ToolChoice.DisableParallel != nil && caps.Has(canonical.CapParallelToolCalls) {
			out.ParallelToolCalls = ptr(!*req.ToolChoice.DisableParallel)
		}
	}
	if req.ParallelToolCalls != nil && caps.Has(canonical.CapParallelToolCalls) {
		out.ParallelToolCalls = req.ParallelToolCalls
	}

	// Scalars. Each is gated on the capability that names it, so a target that
	// lacks the knob gets a request without the field and the caller gets the
	// field name in x-dorang-dropped-params.
	if opt != nil && opt.MaxTokensField == FieldMaxCompletionTokens {
		out.MaxCompletionTokens = req.MaxTokens
	} else {
		out.MaxTokens = req.MaxTokens
	}
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	if req.TopK != nil && caps.Has(canonical.CapTopK) {
		out.TopK = req.TopK
	}
	if len(req.Stop) > 0 && caps.Has(canonical.CapStopSequences) {
		out.Stop = StopSequences(req.Stop)
	}
	if req.StreamOptions != nil {
		out.StreamOptions = &StreamOptions{IncludeUsage: req.StreamOptions.IncludeUsage}
	}
	if req.N != nil && caps.Has(canonical.CapMultipleChoices) {
		out.N = req.N
	}
	if caps.Has(canonical.CapPenalties) {
		out.FrequencyPenalty = req.FrequencyPenalty
		out.PresencePenalty = req.PresencePenalty
	}
	if caps.Has(canonical.CapLogitBias) {
		out.LogitBias = req.LogitBias
	}
	if caps.Has(canonical.CapLogprobs) {
		out.Logprobs = req.Logprobs
		out.TopLogprobs = req.TopLogprobs
	}
	if caps.Has(canonical.CapSeed) {
		out.Seed = req.Seed
	}
	if caps.Has(canonical.CapUser) {
		out.User = req.User
	}
	if caps.Has(canonical.CapServiceTier) {
		out.ServiceTier = req.ServiceTier
	}
	if caps.Has(canonical.CapMetadata) {
		out.Metadata = req.Metadata
	}

	if req.ResponseFormat != nil {
		if req.ResponseFormat.Kind == canonical.FormatJSONSchema && !caps.Has(canonical.CapJSONSchema) {
			loss.Downgrade(canonical.ConstructJSONSchema, "response_format")
		} else {
			out.ResponseFormat = encodeResponseFormat(req.ResponseFormat)
		}
	}

	// Reasoning. This is the generic fold; per-model folding keyed on catalog
	// capability (DESIGN §10.2) belongs to the backend adapter, which knows the
	// model. Here the neutral control is emitted in the two spellings an
	// OpenAI-shaped endpoint accepts and nothing is invented.
	if req.Reasoning != nil && caps.Has(canonical.CapReasoningControl) {
		r := req.Reasoning
		if r.Effort != "" {
			out.ReasoningEffort = r.Effort
		}
		if r.Summary != "" || r.BudgetTokens > 0 || r.Enabled != nil {
			out.Reasoning = &Reasoning{
				Effort:    r.Effort,
				Summary:   r.Summary,
				MaxTokens: r.BudgetTokens,
				Enabled:   r.Enabled,
			}
		}
	}

	return out, nil
}

// encodeMessage appends one or more wire messages for a neutral message.
//
// One neutral message can produce several, because a neutral tool message
// carries every result of a parallel tool call while an OpenAI tool message
// carries exactly one.
func encodeMessage(dst *[]Message, m *canonical.Message, caps canonical.Capability, loss *canonical.LossReport, opt *EncodeOptions, where string) {
	// Tool results become their own messages.
	if m.Role == canonical.RoleTool {
		emitted := false
		for i := range m.Content {
			b := &m.Content[i]
			if b.Kind != canonical.KindToolResult || b.ToolResult == nil {
				continue
			}
			at := where + ".content[" + strconv.Itoa(i) + "]"
			*dst = append(*dst, Message{
				Role:       string(canonical.RoleTool),
				ToolCallID: b.ToolResult.ToolUseID,
				Content:    encodeToolResultContent(b.ToolResult, caps, loss, at),
				Extra:      b.Extra,
			})
			emitted = true
		}
		if emitted {
			return
		}
		// A tool message with no tool_result block is malformed but survivable:
		// forward the text so the turn is not silently emptied.
		*dst = append(*dst, Message{
			Role:    string(canonical.RoleTool),
			Content: encodeContent(m.Content, caps, loss, where),
			Extra:   m.Extra,
		})
		return
	}

	out := Message{Role: string(m.Role), Name: m.Name, Extra: m.Extra}
	before := len(*dst)

	// Split the blocks: tool_use leaves the content array for tool_calls, and
	// thinking leaves it for reasoning_content.
	var carried canonical.Content
	var reasoning []byte
	for i := range m.Content {
		b := &m.Content[i]
		at := where + ".content[" + strconv.Itoa(i) + "]"
		switch b.Kind {
		case canonical.KindToolUse:
			if b.ToolUse == nil {
				continue
			}
			if !caps.Has(canonical.CapToolCalls) {
				loss.Downgrade(canonical.ConstructToolCalls, at+": "+b.ToolUse.Name)
				continue
			}
			args := string(b.ToolUse.Input)
			if args == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:   b.ToolUse.ID,
				Type: "function",
				Function: FunctionCall{
					Name:      opt.toolNames().Shorten(b.ToolUse.Name, opt.warn()),
					Arguments: args,
				},
				// Index is intentionally NOT set: COMPATIBILITY 5.2 strips it
				// from inbound assistant messages before forwarding, so a client
				// echoing back a message it received from a stream is not
				// rejected by an upstream that refuses the field.
			})
		case canonical.KindToolResult:
			// A tool result inside a non-tool message (the Anthropic shape puts
			// them in a user message). Emit it as its own tool message, in
			// place, so ordering is preserved.
			if b.ToolResult == nil {
				continue
			}
			if len(carried) > 0 || len(out.ToolCalls) > 0 || reasoning != nil {
				flushMessage(dst, &out, carried, reasoning, caps, loss, where)
				carried, reasoning = nil, nil
			}
			*dst = append(*dst, Message{
				Role:       string(canonical.RoleTool),
				ToolCallID: b.ToolResult.ToolUseID,
				Content:    encodeToolResultContent(b.ToolResult, caps, loss, at),
			})
		case canonical.KindThinking:
			if b.Thinking != nil && b.Thinking.Signature != "" {
				// No OpenAI field carries integrity material, and dorang never
				// fabricates or re-signs one (DESIGN §10.2). The loss lands on
				// the second turn of an agentic flow, so it is reported now.
				loss.Downgrade(canonical.ConstructThinkingBlock, at+": signature")
			}
			if !caps.Has(canonical.CapThinkingBlocks) {
				loss.Downgrade(canonical.ConstructThinkingBlock, at)
				continue
			}
			reasoning = append(reasoning, b.Text...)
		default:
			carried = append(carried, *b)
		}
	}
	// Emit the trailing remainder — but only if there is one, or if this
	// neutral message produced nothing at all so far. Without that second
	// condition, the Anthropic shape (tool results inside a USER message)
	// leaves a trailing {"role":"user"} with no content, which several
	// upstreams reject outright.
	if len(carried) > 0 || len(out.ToolCalls) > 0 || reasoning != nil || len(*dst) == before {
		flushMessage(dst, &out, carried, reasoning, caps, loss, where)
	}
}

func flushMessage(dst *[]Message, out *Message, carried canonical.Content, reasoning []byte, caps canonical.Capability, loss *canonical.LossReport, where string) {
	if len(carried) > 0 {
		out.Content = encodeContent(carried, caps, loss, where)
	}
	if reasoning != nil {
		out.ReasoningContent = ptr(string(reasoning))
	}
	if out.Content == nil && len(out.ToolCalls) == 0 {
		// A message with no content field at all is rejected by stricter
		// upstreams. An empty string is the shape they accept, and it decodes
		// back to no blocks, so the round trip is stable.
		out.Content = TextContent("")
	}
	*dst = append(*dst, *out)
	*out = Message{Role: out.Role, Name: out.Name}
}

// encodeContent renders neutral blocks as the string or array content form.
func encodeContent(blocks canonical.Content, caps canonical.Capability, loss *canonical.LossReport, where string) *Content {
	if len(blocks) == 0 {
		return nil
	}
	if s, plain := blocks.Plain(); plain {
		return TextContent(s)
	}
	if !caps.Has(canonical.CapMultiBlockContent) {
		// The target takes one string. Everything that is not text is destroyed
		// and the caller must be told which piece.
		loss.Downgrade(canonical.ConstructMultiBlockContent, where+".content")
		reportLostBlocks(blocks, loss, where+".content")
		return TextContent(blocks.Flatten())
	}
	parts := make([]Part, 0, len(blocks))
	for i := range blocks {
		if p, ok := encodePart(&blocks[i], caps, loss, where+".content["+strconv.Itoa(i)+"]"); ok {
			parts = append(parts, p)
		}
	}
	return PartsContent(parts)
}

// encodeToolResultContent renders one tool result.
func encodeToolResultContent(tr *canonical.ToolResult, caps canonical.Capability, loss *canonical.LossReport, where string) *Content {
	if len(tr.Content) == 0 {
		return TextContent("")
	}
	if s, plain := canonical.Content(tr.Content).Plain(); plain {
		return TextContent(s)
	}
	if !caps.Has(canonical.CapMultiBlockToolResult) {
		// This is the row of DESIGN §10.1 that reads "multi-block tool results
		// (text + image) -> a single-string tool message | the non-text blocks".
		loss.Downgrade(canonical.ConstructMultiBlockToolResult, where)
		reportLostBlocks(tr.Content, loss, where)
		return TextContent(canonical.Content(tr.Content).Flatten())
	}
	parts := make([]Part, 0, len(tr.Content))
	for i := range tr.Content {
		if p, ok := encodePart(&tr.Content[i], caps, loss, where+".content["+strconv.Itoa(i)+"]"); ok {
			parts = append(parts, p)
		}
	}
	return PartsContent(parts)
}

func reportLostBlocks(blocks canonical.Content, loss *canonical.LossReport, where string) {
	for i := range blocks {
		at := where + "[" + strconv.Itoa(i) + "]"
		switch blocks[i].Kind {
		case canonical.KindImage:
			loss.Downgrade(canonical.ConstructImageBlock, at)
		case canonical.KindDocument:
			loss.Downgrade(canonical.ConstructDocumentBlock, at)
		case canonical.KindThinking:
			loss.Downgrade(canonical.ConstructThinkingBlock, at)
		}
		if blocks[i].CacheControl != nil {
			loss.Downgrade(canonical.ConstructCacheBreakpoints, at)
		}
	}
}

// encodePart renders one block as a content part. The bool is false when the
// block could not be carried at all.
func encodePart(b *canonical.Block, caps canonical.Capability, loss *canonical.LossReport, where string) (Part, bool) {
	p := Part{Extra: b.Extra}
	switch b.Kind {
	case canonical.KindText:
		p.Type = PartText
		p.Text = b.Text
	case canonical.KindImage:
		if !caps.Has(canonical.CapImageBlocks) || b.Source == nil {
			loss.Downgrade(canonical.ConstructImageBlock, where)
			return Part{}, false
		}
		p.Type = PartImageURL
		p.ImageURL = &ImageURL{URL: b.Source.DataURL(), Detail: b.Source.Detail}
	case canonical.KindDocument:
		if !caps.Has(canonical.CapDocumentBlocks) || b.Source == nil {
			// Returning 200 having silently discarded a PDF is the case
			// DESIGN §10.1 calls worse than an error.
			loss.Downgrade(canonical.ConstructDocumentBlock, where+": "+sourceMediaType(b.Source))
			return Part{}, false
		}
		p.Type = PartFile
		p.File = &FilePart{Filename: b.Source.Name}
		if b.Source.Kind == canonical.SourceFileID {
			p.File.FileID = b.Source.Data
		} else {
			p.File.FileData = b.Source.DataURL()
		}
	case canonical.KindThinking:
		loss.Downgrade(canonical.ConstructThinkingBlock, where)
		return Part{}, false
	default:
		loss.Downgrade(canonical.ConstructMultiBlockContent, where+": "+string(b.Kind))
		return Part{}, false
	}
	if b.CacheControl != nil {
		if caps.Has(canonical.CapCacheBreakpoints) {
			p.CacheControl = encodeCacheControl(b.CacheControl)
		} else {
			// Losing a breakpoint changes the caching topology and therefore
			// the bill, invisibly. It is structural for that reason.
			loss.Downgrade(canonical.ConstructCacheBreakpoints, where)
		}
	}
	return p, true
}

func encodeCacheControl(c *canonical.CacheControl) *CacheControl {
	if c == nil {
		return nil
	}
	t := c.Type
	if t == "" {
		t = "ephemeral"
	}
	return &CacheControl{Type: t, TTL: c.TTL}
}

func encodeResponseFormat(f *canonical.ResponseFormat) *ResponseFormat {
	out := &ResponseFormat{Type: f.Kind}
	if f.Kind == canonical.FormatJSONSchema {
		out.JSONSchema = &JSONSchema{
			Name:        f.Name,
			Description: f.Description,
			Schema:      f.Schema,
			Strict:      f.Strict,
		}
	}
	return out
}

func encodeToolChoice(tc *canonical.ToolChoice, names *ToolNames, warn WarnFunc) (json.RawMessage, error) {
	switch tc.Mode {
	case canonical.ToolChoiceNone, canonical.ToolChoiceAuto, canonical.ToolChoiceRequired:
		return Marshal(string(tc.Mode))
	case canonical.ToolChoiceTool:
		return Marshal(struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}{
			Type: "function",
			Function: struct {
				Name string `json:"name"`
			}{Name: names.Shorten(tc.Name, warn)},
		})
	case "":
		return nil, nil
	default:
		return Marshal(string(tc.Mode))
	}
}

func sourceMediaType(s *canonical.Source) string {
	if s == nil || s.MediaType == "" {
		return "unknown"
	}
	return s.MediaType
}

var errNilRequest = errorString("openai: nil request")

type errorString string

func (e errorString) Error() string { return string(e) }
