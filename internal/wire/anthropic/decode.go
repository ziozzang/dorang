package anthropic

import (
	"encoding/json"

	"github.com/ziozzang/dorang/internal/canonical"
)

// DecodeOptions controls an Anthropic -> canonical conversion.
type DecodeOptions struct {
	// Model overrides the model reported to the caller. DESIGN §7.2: the body
	// always carries the name the client asked for, never the upstream id.
	Model string
	Warn  WarnFunc
}

func (o *DecodeOptions) warn() WarnFunc {
	if o == nil {
		return nil
	}
	return o.Warn
}

// DecodeRequest parses POST /v1/messages bytes into the neutral form.
//
// max_tokens is required here and its absence is a 400, not a default. That is
// the frontend half of DESIGN §10.7's first trap: this family demands the
// field, so a client that omitted it gets the same answer from dorang as from
// the vendor rather than a surprise further down. "Max_Tokens" is an absence
// too: the decode is case-SENSITIVE (COMPATIBILITY 2.0, [strictUnmarshal]),
// which is also what makes {"Model":"x"} carry no model here and no model
// through the authorization gate.
func DecodeRequest(b []byte) (*canonical.Request, error) {
	var w Request
	// [decodeSelf], not strictUnmarshal: Request.UnmarshalJSON applies the strict
	// filter itself, and going through json.Unmarshal to reach it costs two extra
	// walks of the whole body.
	if err := decodeSelf(b, &w); err != nil {
		return nil, err
	}
	if w.MaxTokens == nil {
		return nil, NewError(400, TypeInvalidRequest,
			"max_tokens: field required").WithParam("max_tokens")
	}
	return RequestToCanonical(&w, nil)
}

// DecodeCountTokensRequest parses a POST /v1/messages/count_tokens body.
//
// It is the same shape minus the output ceiling: counting the prompt does not
// need one, and requiring it would reject every well-formed call
// (COMPATIBILITY 6.9).
func DecodeCountTokensRequest(b []byte) (*canonical.Request, error) {
	var w Request
	if err := decodeSelf(b, &w); err != nil {
		return nil, err
	}
	return RequestToCanonical(&w, nil)
}

// RequestToCanonical converts a decoded wire request.
func RequestToCanonical(w *Request, opt *DecodeOptions) (*canonical.Request, error) {
	if w == nil {
		return nil, errNilRequest
	}
	out := &canonical.Request{
		// The model name is copied verbatim. It is opaque: nothing splits it on
		// ':' or '/' (DESIGN §2.1).
		Model:       w.Model,
		MaxTokens:   w.MaxTokens,
		Temperature: w.Temperature,
		TopP:        w.TopP,
		TopK:        w.TopK,
		Stop:        w.StopSequences,
		Stream:      w.Stream,
		// Extra is the whole of §10.5a and EXTENSIONS §B: context_management,
		// cache_edits, mcp_servers and everything unmodelled ride here, raw, and
		// are spliced back byte-identically on the way out.
		Extra: w.Extra,
	}
	if w.System != nil {
		out.System = blockListToCanonical(w.System)
	}
	if w.Metadata != nil {
		out.User = w.Metadata.UserID
		if len(w.Metadata.Extra) > 0 {
			out.Metadata = metadataToCanonical(w.Metadata.Extra, opt.warn())
		}
	}
	if w.Thinking != nil {
		out.Reasoning = thinkingToCanonical(w.Thinking)
	}

	if len(w.Tools) > 0 {
		out.Tools = make([]canonical.Tool, 0, len(w.Tools))
		for i := range w.Tools {
			t := &w.Tools[i]
			ct := canonical.Tool{
				Type:        t.Type,
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
				Extra:       t.Extra,
			}
			if ct.Type == "" {
				// An untyped declaration is the ordinary custom tool, which the
				// neutral form spells "function".
				ct.Type = "function"
			}
			if t.CacheControl != nil {
				ct.CacheControl = cacheControlToCanonical(t.CacheControl)
			}
			out.Tools = append(out.Tools, ct)
		}
	}
	if w.ToolChoice != nil {
		out.ToolChoice = toolChoiceToCanonical(w.ToolChoice)
		if w.ToolChoice.DisableParallelToolUse != nil {
			// Both spellings are filled, and the OpenAI-shaped one is INVERTED
			// (DESIGN §10.7). Filling only one leaves the next encoder guessing;
			// filling the OpenAI one without inverting reverses the caller's
			// intent with no error anywhere.
			out.ParallelToolCalls = ptr(!*w.ToolChoice.DisableParallelToolUse)
		}
	}

	out.Messages = make([]canonical.Message, 0, len(w.Messages))
	for i := range w.Messages {
		out.Messages = append(out.Messages, messageToCanonical(&w.Messages[i]))
	}
	return out, nil
}

// messageToCanonical converts one wire message.
//
// A tool_result block stays inside the user message it arrived in. The neutral
// form allows that on purpose — internal/wire/openai splits such a message into
// one tool message per result on the way out — and moving it here would destroy
// the ordering information that the split needs.
func messageToCanonical(m *Message) canonical.Message {
	return canonical.Message{
		Role:    canonical.Role(m.Role),
		Content: blockListToCanonical(&m.Content),
		Extra:   m.Extra,
	}
}

func blockListToCanonical(l *BlockList) canonical.Content {
	if l == nil {
		return nil
	}
	if l.Blocks == nil {
		if l.Text == "" {
			return nil
		}
		return canonical.Content{canonical.TextBlock(l.Text)}
	}
	out := make(canonical.Content, 0, len(l.Blocks))
	for i := range l.Blocks {
		out = append(out, blockToCanonical(&l.Blocks[i]))
	}
	return out
}

func blockToCanonical(b *ContentBlock) canonical.Block {
	var out canonical.Block
	switch b.Type {
	case BlockText:
		out = canonical.Block{Kind: canonical.KindText}
		if b.Text != nil {
			out.Text = *b.Text
		}
	case BlockImage:
		out = canonical.Block{Kind: canonical.KindImage, Source: sourceToCanonical(b.Source, "")}
	case BlockDocument:
		out = canonical.Block{Kind: canonical.KindDocument, Source: sourceToCanonical(b.Source, b.Title)}
	case BlockToolUse:
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		out = canonical.Block{Kind: canonical.KindToolUse, ToolUse: &canonical.ToolUse{
			ID: b.ID, Name: b.Name, Input: input,
		}}
	case BlockToolResult:
		out = canonical.Block{Kind: canonical.KindToolResult, ToolResult: &canonical.ToolResult{
			ToolUseID: b.ToolUseID,
			Content:   blockListToCanonical(b.Content),
			IsError:   b.IsError,
		}}
	case BlockThinking:
		out = canonical.Block{Kind: canonical.KindThinking}
		if b.Thinking != nil {
			out.Text = *b.Thinking
		}
		if b.Signature != "" {
			// Stored verbatim. dorang replays these bytes and never re-signs
			// them (DESIGN §10.2).
			out.Thinking = &canonical.Thinking{Signature: b.Signature}
		}
	case BlockRedactedThinking:
		// canonical.Thinking has Redacted but no field for the payload
		// (EXTENSIONS §B18). Parking it in Extra under its own wire key is what
		// makes the round trip reconstruct the block WITH its data instead of an
		// empty shell — the same class of defect §10.2 already fixed for
		// signatures.
		out = canonical.Block{
			Kind:     canonical.KindThinking,
			Thinking: &canonical.Thinking{Redacted: true},
		}
		if b.Data != "" {
			if raw, err := Marshal(b.Data); err == nil {
				out.Extra = map[string]json.RawMessage{"data": raw}
			}
		}
	default:
		// An unmodelled block type. Keep the raw members so a same-protocol
		// crossing is a pass-through rather than a filter.
		out = canonical.Block{Kind: canonical.BlockKind(b.Type)}
		if b.Text != nil {
			out.Text = *b.Text
		}
	}
	if len(b.Extra) > 0 {
		if out.Extra == nil {
			out.Extra = b.Extra
		} else {
			for k, v := range b.Extra {
				out.Extra[k] = v
			}
		}
	}
	if b.CacheControl != nil {
		out.CacheControl = cacheControlToCanonical(b.CacheControl)
	}
	return out
}

func sourceToCanonical(s *Source, title string) *canonical.Source {
	if s == nil {
		return nil
	}
	out := &canonical.Source{MediaType: s.MediaType, Name: title}
	switch s.Type {
	case SourceURL:
		out.Kind = canonical.SourceURL
		out.Data = s.URL
	case SourceFile:
		out.Kind = canonical.SourceFileID
		out.Data = s.FileID
	case SourceText:
		out.Kind = canonical.SourceText
		out.Data = s.Data
	default:
		out.Kind = canonical.SourceBase64
		out.Data = s.Data
	}
	return out
}

func cacheControlToCanonical(c *CacheControl) *canonical.CacheControl {
	if c == nil {
		return nil
	}
	return &canonical.CacheControl{Type: c.Type, TTL: c.TTL}
}

func toolChoiceToCanonical(tc *ToolChoice) *canonical.ToolChoice {
	out := &canonical.ToolChoice{DisableParallel: tc.DisableParallelToolUse}
	switch tc.Type {
	case ToolChoiceAny:
		out.Mode = canonical.ToolChoiceRequired
	case ToolChoiceTool:
		out.Mode = canonical.ToolChoiceTool
		out.Name = tc.Name
	case ToolChoiceNone:
		out.Mode = canonical.ToolChoiceNone
	case ToolChoiceAuto:
		out.Mode = canonical.ToolChoiceAuto
	default:
		out.Mode = canonical.ToolChoiceMode(tc.Type)
	}
	return out
}

// thinkingToCanonical folds the wire reasoning control onto the neutral one.
//
// The effort level is DERIVED from the budget, which DESIGN §10.2 calls
// best-effort and non-reversible: the neutral form carries both, so a target
// that wants a budget gets the exact number and a target that wants a level
// gets the nearest one rather than nothing.
func thinkingToCanonical(t *Thinking) *canonical.Reasoning {
	out := &canonical.Reasoning{}
	switch t.Type {
	case ThinkingEnabled:
		out.Enabled = ptr(true)
	case ThinkingDisabled:
		out.Enabled = ptr(false)
	}
	if t.BudgetTokens != nil {
		out.BudgetTokens = *t.BudgetTokens
		out.Effort = effortForBudget(*t.BudgetTokens)
	}
	return out
}

// metadataToCanonical carries the non-user_id members.
//
// canonical.Request.Metadata is a map[string]string, so a member whose value is
// not a JSON string has nowhere to live. It is reported rather than dropped in
// silence — this is a real limit of the neutral type, not of this family.
func metadataToCanonical(extra map[string]json.RawMessage, warn WarnFunc) map[string]string {
	var out map[string]string
	for k, v := range extra {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			warn.warn(WarnDroppedMetadata, k)
			continue
		}
		if out == nil {
			out = make(map[string]string, len(extra))
		}
		out[k] = s
	}
	return out
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// DecodeResponse parses a non-streaming response into the neutral form.
//
// This does NOT apply the strict filter, on purpose: the bytes are a backend's,
// not a caller's, and the reasoning is in [strictUnmarshal].
//
// It returns [ErrNotAResponse] for a JSON object that is not a Messages
// response. Parsing without error is not the same fact as "this is an answer",
// and treating it as one is what turned a vendor's 200-wrapped error body into a
// silent empty success; see [ErrNotAResponse] and [IsMessagesResponse].
func DecodeResponse(b []byte, opt *DecodeOptions) (*canonical.Response, error) {
	var w Response
	if err := decodeSelf(b, &w); err != nil {
		return nil, err
	}
	if !IsMessagesResponse(&w) {
		return nil, ErrNotAResponse
	}
	return ResponseToCanonical(&w, opt)
}

// IsMessagesResponse reports whether a decoded body is a response of this
// family.
//
// # The test, and what it deliberately does not do
//
// It accepts on EITHER of two grounds, never on both being required:
//
//  1. the discriminator names this family — `"type": "message"`; or
//  2. at least one payload-bearing member is PRESENT — `content`, `usage` or
//     `stop_reason`.
//
// Presence means the key was in the document, not that its value is interesting.
// `{"type":"message","content":[]}` is a valid, minimal answer — a model can
// legitimately produce an empty turn — and so is a body from a vendor that has
// never sent a `type` member but does send `content`. Requiring the
// discriminator would refuse the second; requiring non-empty content would
// refuse the first. Both are answers and both are accepted.
//
// What it refuses is a body with NEITHER: no `type: message`, no `content`, no
// `usage`, no `stop_reason`. A vendor error envelope has none of the four, which
// is the point; so does a JSON object from a completely different API. The cost
// of the rule is that a hypothetical vendor that answers with only `{"id":…,
// "role":"assistant","model":…}` and no content at all is refused — which is
// the same empty turn the caller could not have used anyway, now with a 502 that
// names the condition instead of a 200 that hides it.
func IsMessagesResponse(w *Response) bool {
	if w == nil {
		return false
	}
	if w.Type == TypeMessage {
		return true
	}
	return w.Content != nil || w.Usage != nil || w.StopReason != nil
}

// ResponseToCanonical converts a decoded wire response.
func ResponseToCanonical(w *Response, opt *DecodeOptions) (*canonical.Response, error) {
	if w == nil {
		return nil, errNilResponse
	}
	out := &canonical.Response{
		ID: w.ID,
		// Model is what the client is told; ServedModel is what the upstream
		// said, kept because opt.Model below is about to overwrite the only
		// copy of it. See [canonical.Response.ServedModel].
		Model:       w.Model,
		ServedModel: w.Model,
		Extra:       w.Extra,
		// Tagging the shape is what keeps these members OUT of a chat
		// completion. Before the tag existed only this family produced them and
		// only this family read them; now that every adapter does, an untagged
		// map would be spliced by whichever encoder ran (DESIGN §10.7).
		ExtraFamily: canonical.FamilyAnthropicMessages,
	}
	if opt != nil && opt.Model != "" {
		out.Model = opt.Model
	}
	if w.Usage != nil {
		out.Usage = UsageToCanonical(w.Usage)
		if len(w.Usage.Extra) > 0 {
			out.UsageExtra = &canonical.UsageExtra{Usage: w.Usage.Extra}
		}
	}

	msg := canonical.Message{Role: canonical.RoleAssistant}
	msg.Content = make(canonical.Content, 0, len(w.Content))
	for i := range w.Content {
		msg.Content = append(msg.Content, blockToCanonical(&w.Content[i]))
	}
	choice := canonical.Choice{Index: 0, Message: msg}
	if w.StopReason != nil && *w.StopReason != "" {
		choice.StopReason, choice.NativeStopReason = stopReasonOfWire(*w.StopReason, opt.warn())
	}
	out.Choices = []canonical.Choice{choice}
	return out, nil
}

// stopReasonOfWire resolves a wire stop_reason, preserving the native string
// when it is not already the neutral spelling (COMPATIBILITY 4.3).
func stopReasonOfWire(native string, warn WarnFunc) (canonical.StopReason, string) {
	r, ok := LookupStopReason(native)
	if !ok {
		warn.warn(WarnUnmappedStopReason, native)
		return canonical.StopEndTurn, native
	}
	if string(r) == native {
		return r, ""
	}
	return r, native
}

// UsageToCanonical converts wire counts to neutral counts.
//
// THE ARITHMETIC IS THE POINT. This family reports input_tokens EXCLUSIVE of
// cache — cache reads and cache creations are counted beside it, not inside it
// — while canonical.Usage.InputTokens is the full prompt count, inclusive,
// matching the OpenAI convention that internal/wire/openai implements. So the
// cache counts are added back here and subtracted again in [EncodeUsage].
//
// Skipping this produces no error and no warning. It produces a wrong invoice
// on every cached request, which in an agentic workload is most of them.
func UsageToCanonical(u *Usage) *canonical.Usage {
	if u == nil {
		return nil
	}
	out := &canonical.Usage{}
	if u.OutputTokens != nil {
		out.OutputTokens = *u.OutputTokens
		out.Report(canonical.UsageOutput)
	}
	if u.CacheReadInputTokens != nil {
		out.CacheReadTokens = *u.CacheReadInputTokens
		// The vendor emits explicit zeros here (COMPATIBILITY 6.7). Recording
		// that it did is what lets the encoder put them back, so a capture from
		// dorang matches a capture from the vendor instead of differing by two
		// keys on every uncached request.
		out.Report(canonical.UsageCacheRead)
	}
	if u.CacheCreationInputTokens != nil {
		out.CacheWriteTokens = *u.CacheCreationInputTokens
		out.Report(canonical.UsageCacheWrite)
	}
	if u.InputTokens != nil {
		out.InputTokens = *u.InputTokens + out.CacheReadTokens + out.CacheWriteTokens
		out.Report(canonical.UsageInput)
	}
	// total_tokens, when a backend sent COMPATIBILITY 6.8's non-spec field, is
	// deliberately ignored: canonical.Usage derives it, and trusting a
	// backend-computed total over the parts is how a disagreement between them
	// becomes an invoice nobody can reconstruct.
	return out
}
