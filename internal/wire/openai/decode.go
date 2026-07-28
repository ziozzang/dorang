package openai

import (
	"encoding/json"

	"github.com/ziozzang/dorang/internal/canonical"
)

// DecodeOptions controls an OpenAI -> canonical conversion.
type DecodeOptions struct {
	// ToolNames restores tool names that the request encoder shortened
	// (COMPATIBILITY 5.3). Without it, a call to a shortened name reaches the
	// caller under a name it never declared.
	ToolNames *ToolNames
	// Tools carries the per-STREAM tool-call state. It is required on a
	// streaming exchange and meaningless on a non-streaming one, where every
	// name and every argument object arrives whole. Without it a shortened name
	// split across two frames is restored by neither half; see [ToolStream].
	Tools *ToolStream
	// Model overrides the model reported to the caller. DESIGN §7.2: the body
	// always carries the name the client asked for, never the upstream id.
	Model string
	Warn  WarnFunc
}

func (o *DecodeOptions) names() *ToolNames {
	if o == nil {
		return nil
	}
	return o.ToolNames
}

func (o *DecodeOptions) tools() *ToolStream {
	if o == nil {
		return nil
	}
	return o.Tools
}

func (o *DecodeOptions) warn() WarnFunc {
	if o == nil {
		return nil
	}
	return o.Warn
}

// DecodeRequest parses OpenAI request bytes into the neutral representation.
//
// The decode is case-SENSITIVE (COMPATIBILITY 2.0): {"Model":"x"} carries no
// model here, exactly as it carries none through the authorization gate and
// none into the backend. See [strictUnmarshal].
func DecodeRequest(b []byte) (*canonical.Request, error) {
	var w Request
	if err := strictUnmarshal(b, &w); err != nil {
		return nil, err
	}
	return RequestToCanonical(&w)
}

// RequestToCanonical converts a decoded wire request.
func RequestToCanonical(w *Request) (*canonical.Request, error) {
	if w == nil {
		return nil, errNilRequest
	}
	out := &canonical.Request{
		// The model name is copied verbatim. It is opaque: nothing splits it on
		// ':' or '/' (DESIGN §2.1).
		Model:             w.Model,
		Temperature:       w.Temperature,
		TopP:              w.TopP,
		TopK:              w.TopK,
		Stream:            w.Stream,
		N:                 w.N,
		FrequencyPenalty:  w.FrequencyPenalty,
		PresencePenalty:   w.PresencePenalty,
		LogitBias:         w.LogitBias,
		Logprobs:          w.Logprobs,
		TopLogprobs:       w.TopLogprobs,
		Seed:              w.Seed,
		User:              w.User,
		ServiceTier:       w.ServiceTier,
		Metadata:          w.Metadata,
		ParallelToolCalls: w.ParallelToolCalls,
		Extra:             w.Extra,
	}
	// max_completion_tokens is the current spelling and wins when both arrived.
	out.MaxTokens = w.MaxTokens
	if w.MaxCompletionTokens != nil {
		out.MaxTokens = w.MaxCompletionTokens
	}
	if len(w.Stop) > 0 {
		out.Stop = []string(w.Stop)
	}
	if w.StreamOptions != nil {
		out.StreamOptions = &canonical.StreamOptions{IncludeUsage: w.StreamOptions.IncludeUsage}
	}
	if w.ResponseFormat != nil {
		out.ResponseFormat = decodeResponseFormat(w.ResponseFormat)
	}
	if w.ReasoningEffort != "" || w.Reasoning != nil {
		r := &canonical.Reasoning{Effort: w.ReasoningEffort}
		if w.Reasoning != nil {
			if w.Reasoning.Effort != "" {
				r.Effort = w.Reasoning.Effort
			}
			r.Summary = w.Reasoning.Summary
			r.BudgetTokens = w.Reasoning.MaxTokens
			r.Enabled = w.Reasoning.Enabled
		}
		out.Reasoning = r
	}

	if len(w.Tools) > 0 {
		out.Tools = make([]canonical.Tool, 0, len(w.Tools))
		for i := range w.Tools {
			t := &w.Tools[i]
			ct := canonical.Tool{Type: t.Type, Extra: t.Extra}
			if t.Function != nil {
				ct.Name = t.Function.Name
				ct.Description = t.Function.Description
				ct.Parameters = t.Function.Parameters
				ct.Strict = t.Function.Strict
			}
			if t.CacheControl != nil {
				ct.CacheControl = decodeCacheControl(t.CacheControl)
			}
			out.Tools = append(out.Tools, ct)
		}
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

	out.Messages = make([]canonical.Message, 0, len(w.Messages))
	for i := range w.Messages {
		out.Messages = append(out.Messages, messageToCanonical(&w.Messages[i], nil))
	}
	return out, nil
}

// messageToCanonical converts one wire message.
//
// names is non-nil only on the response path, where a tool name may have been
// shortened on the way out and must be restored on the way back.
func messageToCanonical(m *Message, names *ToolNames) canonical.Message {
	out := canonical.Message{Role: canonical.Role(m.Role), Name: m.Name, Extra: m.Extra}
	if m.Refusal != nil {
		out.Refusal = *m.Refusal
	}

	// Reasoning text arrives beside the content, not inside it, and belongs at
	// the front of the block list: it preceded the visible output.
	if m.ReasoningContent != nil && *m.ReasoningContent != "" {
		out.Content = append(out.Content, canonical.ThinkingBlock(*m.ReasoningContent, ""))
	}

	if m.Role == string(canonical.RoleTool) {
		// One OpenAI tool message is one result. Its content becomes the
		// result's block list, which is why ToolResult holds blocks and not a
		// string: an array-form tool message survives the crossing intact.
		out.Content = append(out.Content, canonical.Block{
			Kind: canonical.KindToolResult,
			ToolResult: &canonical.ToolResult{
				ToolUseID: m.ToolCallID,
				Content:   contentToBlocks(m.Content),
			},
		})
		return out
	}

	out.Content = append(out.Content, contentToBlocks(m.Content)...)

	for i := range m.ToolCalls {
		tc := &m.ToolCalls[i]
		// COMPATIBILITY 5.2: the inbound index is read and DISCARDED. The
		// neutral form has no index because ordering in the block list is the
		// ordering, and re-emitting an index a client echoed back is what some
		// upstreams reject.
		out.Content = append(out.Content, canonical.ToolUseBlock(
			tc.ID,
			names.Restore(tc.Function.Name),
			rawOrEmptyObject(tc.Function.Arguments),
		))
	}
	return out
}

func rawOrEmptyObject(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

// contentToBlocks converts the string or array content form to blocks.
func contentToBlocks(c *Content) canonical.Content {
	if c == nil {
		return nil
	}
	if c.Parts == nil {
		if c.Text == "" {
			return nil
		}
		return canonical.Content{canonical.TextBlock(c.Text)}
	}
	out := make(canonical.Content, 0, len(c.Parts))
	for i := range c.Parts {
		p := &c.Parts[i]
		var b canonical.Block
		switch p.Type {
		case PartText:
			b = canonical.TextBlock(p.Text)
		case PartRefusal:
			// No neutral refusal block exists; a refusal part is text plus a
			// stop reason, and the stop reason travels on the choice.
			b = canonical.TextBlock(p.Refusal)
		case PartImageURL:
			if p.ImageURL == nil {
				continue
			}
			b = canonical.ImageURLBlock(p.ImageURL.URL)
			if b.Source != nil {
				b.Source.Detail = p.ImageURL.Detail
			}
		case PartFile:
			if p.File == nil {
				continue
			}
			switch {
			case p.File.FileID != "":
				b = canonical.Block{Kind: canonical.KindDocument, Source: &canonical.Source{
					Kind: canonical.SourceFileID, Data: p.File.FileID, Name: p.File.Filename,
				}}
			default:
				mt, data, ok := splitDataURL(p.File.FileData)
				if ok {
					b = canonical.DocumentBlock(mt, data, p.File.Filename)
				} else {
					b = canonical.Block{Kind: canonical.KindDocument, Source: &canonical.Source{
						Kind: canonical.SourceURL, Data: p.File.FileData, Name: p.File.Filename,
					}}
				}
			}
		default:
			// An unmodeled part type. Keep the raw members so a same-protocol
			// crossing is a pass-through rather than a filter.
			b = canonical.Block{Kind: canonical.BlockKind(p.Type), Text: p.Text, Extra: p.Extra}
			out = append(out, b)
			continue
		}
		if p.CacheControl != nil {
			b.CacheControl = decodeCacheControl(p.CacheControl)
		}
		if len(p.Extra) > 0 {
			b.Extra = p.Extra
		}
		out = append(out, b)
	}
	return out
}

func decodeCacheControl(c *CacheControl) *canonical.CacheControl {
	if c == nil {
		return nil
	}
	return &canonical.CacheControl{Type: c.Type, TTL: c.TTL}
}

func decodeResponseFormat(f *ResponseFormat) *canonical.ResponseFormat {
	out := &canonical.ResponseFormat{Kind: f.Type}
	if f.JSONSchema != nil {
		out.Name = f.JSONSchema.Name
		out.Description = f.JSONSchema.Description
		out.Schema = f.JSONSchema.Schema
		out.Strict = f.JSONSchema.Strict
	}
	return out
}

func decodeToolChoice(raw json.RawMessage) (*canonical.ToolChoice, error) {
	b := trimSpace(raw)
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, err
		}
		return &canonical.ToolChoice{Mode: canonical.ToolChoiceMode(s)}, nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	// Request path: tool_choice is carried raw on the wire type and decoded
	// here, so this is where its members get the case-sensitive treatment.
	if err := strictUnmarshal(b, &obj); err != nil {
		return nil, err
	}
	name := obj.Function.Name
	if name == "" {
		name = obj.Name
	}
	if name == "" {
		return &canonical.ToolChoice{Mode: canonical.ToolChoiceMode(obj.Type)}, nil
	}
	return &canonical.ToolChoice{Mode: canonical.ToolChoiceTool, Name: name}, nil
}

// splitDataURL recognizes "data:<media-type>;base64,<payload>".
func splitDataURL(s string) (mediaType, data string, ok bool) {
	const prefix = "data:"
	const marker = ";base64,"
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", "", false
	}
	rest := s[len(prefix):]
	for i := 0; i+len(marker) <= len(rest); i++ {
		if rest[i:i+len(marker)] == marker {
			return rest[:i], rest[i+len(marker):], true
		}
	}
	return "", "", false
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// DecodeResponse parses a non-streaming completion into the neutral form.
//
// This is json.Unmarshal and not [strictUnmarshal] on purpose. The bytes are a
// backend's, not a caller's: a differently-cased key here is a vendor quirk
// that can lose usage counts if refused and cannot bypass authorization if
// accepted, because nothing is authorized against a response. [Message] is
// still strict, since the same type decodes request messages.
func DecodeResponse(b []byte, opt *DecodeOptions) (*canonical.Response, error) {
	var w Response
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return ResponseToCanonical(&w, opt)
}

// ResponseToCanonical converts a decoded wire response.
func ResponseToCanonical(w *Response, opt *DecodeOptions) (*canonical.Response, error) {
	if w == nil {
		return nil, errorString("openai: nil response")
	}
	out := &canonical.Response{
		ID:      w.ID,
		Model:   w.Model,
		Created: w.Created,
	}
	if opt != nil && opt.Model != "" {
		out.Model = opt.Model
	}
	if w.SystemFingerprint != nil {
		out.SystemFingerprint = *w.SystemFingerprint
	}
	if w.ServiceTier != nil {
		out.ServiceTier = *w.ServiceTier
	}
	if w.Usage != nil {
		out.Usage = usageToCanonical(w.Usage)
	}
	out.Choices = make([]canonical.Choice, 0, len(w.Choices))
	for i := range w.Choices {
		c := &w.Choices[i]
		cc := canonical.Choice{
			Index:    c.Index,
			Message:  messageToCanonical(&c.Message, opt.names()),
			Logprobs: c.Logprobs,
		}
		cc.StopReason, cc.NativeStopReason = stopReasonOfChoice(c.FinishReason, c.ProviderSpecificFields, opt.warn())
		out.Choices = append(out.Choices, cc)
	}
	return out, nil
}

// stopReasonOfChoice resolves the terminal reason, preferring the preserved
// native value (COMPATIBILITY 4.3) over the already-collapsed finish_reason so
// that a second crossing does not lose what the first one kept.
func stopReasonOfChoice(finish *string, psf map[string]json.RawMessage, warn WarnFunc) (canonical.StopReason, string) {
	native := ""
	if raw, ok := psf[nativeFinishKey]; ok {
		_ = json.Unmarshal(raw, &native)
	}
	if native != "" {
		if r, ok := LookupStopReason(native); ok {
			return r, native
		}
		warn.warn(WarnUnmappedFinishReason, native)
		return canonical.StopEndTurn, native
	}
	if finish == nil || *finish == "" {
		return canonical.StopUnspecified, ""
	}
	if r, ok := LookupStopReason(*finish); ok {
		return r, ""
	}
	warn.warn(WarnUnmappedFinishReason, *finish)
	return canonical.StopEndTurn, *finish
}

func usageToCanonical(u *Usage) *canonical.Usage {
	out := &canonical.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	if u.CacheCreationInputTokens != nil {
		out.CacheWriteTokens = *u.CacheCreationInputTokens
	}
	return out
}

// DecodeChunk parses one streamed frame's JSON into the neutral form.
//
// It is used on the accumulating path only. The relay path uses [Scanner],
// which never decodes a frame at all.
func DecodeChunk(b []byte, opt *DecodeOptions) (*Chunk, []canonical.StreamEvent, error) {
	var c Chunk
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, nil, err
	}
	return &c, ChunkToEvents(&c, opt), nil
}

// ChunkToEvents converts a decoded chunk to neutral stream events.
//
// One chunk can produce two events, because a backend may put a content delta
// and a finish reason in the same frame.
func ChunkToEvents(c *Chunk, opt *DecodeOptions) []canonical.StreamEvent {
	if c == nil {
		return nil
	}
	model := c.Model
	if opt != nil && opt.Model != "" {
		model = opt.Model
	}
	tools := opt.tools()
	events := make([]canonical.StreamEvent, 0, len(c.Choices)+1)
	for i := range c.Choices {
		ch := &c.Choices[i]
		d := canonical.Delta{}
		if ch.Delta.Role != nil {
			d.Role = canonical.Role(*ch.Delta.Role)
		}
		if ch.Delta.Content != nil {
			d.Content = append(d.Content, canonical.TextBlock(*ch.Delta.Content))
		}
		if ch.Delta.ReasoningContent != nil {
			d.Content = append(d.Content, canonical.ThinkingBlock(*ch.Delta.ReasoningContent, ""))
		}
		if ch.Delta.Refusal != nil {
			d.Refusal = *ch.Delta.Refusal
		}
		for j := range ch.Delta.ToolCalls {
			tc := &ch.Delta.ToolCalls[j]
			cd := canonical.ToolCallDelta{Index: tc.Index}
			if tc.ID != nil {
				cd.ID = *tc.ID
			}
			if tc.Type != nil {
				cd.Type = *tc.Type
			}
			if tc.Function != nil {
				if tc.Function.Name != nil {
					cd.Name = *tc.Function.Name
					if tools == nil {
						// No stream state to accumulate into, so the best that can
						// be done is an exact lookup on this fragment alone. A
						// fragmented name is returned unchanged.
						cd.Name = opt.names().Restore(cd.Name)
					}
				}
				if tc.Function.Arguments != nil {
					cd.Arguments = *tc.Function.Arguments
				}
			}
			d.ToolCalls = append(d.ToolCalls, cd)
		}
		if !ch.Delta.Empty() {
			events = append(events, canonical.StreamEvent{
				Type: canonical.EventDelta, ID: c.ID, Model: model, Created: c.Created,
				Choice: ch.Index, Delta: d,
			})
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			stop, native := stopReasonOfChoice(ch.FinishReason, ch.ProviderSpecificFields, opt.warn())
			events = append(events, canonical.StreamEvent{
				Type: canonical.EventStop, ID: c.ID, Model: model, Created: c.Created,
				Choice: ch.Index,
				Delta:  canonical.Delta{StopReason: stop, NativeStopReason: native},
			})
		}
	}
	if c.Usage != nil {
		events = append(events, canonical.StreamEvent{
			Type: canonical.EventUsage, ID: c.ID, Model: model, Created: c.Created,
			Usage: usageToCanonical(c.Usage),
		})
	}
	return tools.Track(events)
}

// StripToolCallIndexes removes the index field from every tool call of every
// assistant message (COMPATIBILITY 5.2).
//
// It exists separately from the canonical conversion for the pass-through path,
// where a request is forwarded without being decoded to the neutral form and
// would otherwise carry back the indexes a client copied out of a stream.
func StripToolCallIndexes(msgs []Message) {
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			msgs[i].ToolCalls[j].Index = nil
		}
	}
}
