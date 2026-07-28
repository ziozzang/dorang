package openai

import (
	"encoding/json"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
)

// ResponseOptions controls a canonical -> OpenAI response conversion.
type ResponseOptions struct {
	// ID, Created and Model override the neutral values. Model in particular is
	// the CLIENT-FACING name: the body never carries the upstream id
	// (DESIGN §7.2).
	ID      string
	Created int64
	Model   string

	// Capabilities is what the client's view can express. Zero means
	// [DefaultCapabilities].
	Capabilities canonical.Capability
	Loss         *canonical.LossReport
	ToolNames    *ToolNames
	Warn         WarnFunc
}

func (o *ResponseOptions) caps() canonical.Capability {
	if o == nil || o.Capabilities == 0 {
		return DefaultCapabilities
	}
	return o.Capabilities
}

func (o *ResponseOptions) loss() *canonical.LossReport {
	if o == nil {
		return nil
	}
	return o.Loss
}

// MarshalResponse encodes a neutral response as OpenAI response bytes.
func MarshalResponse(r *canonical.Response, opt *ResponseOptions) ([]byte, error) {
	w, err := EncodeResponse(r, opt)
	if err != nil {
		return nil, err
	}
	return Marshal(w)
}

// EncodeResponse converts a neutral response to the OpenAI wire shape.
func EncodeResponse(r *canonical.Response, opt *ResponseOptions) (*Response, error) {
	if r == nil {
		return nil, errorString("openai: nil response")
	}
	out := &Response{
		ID:      r.ID,
		Object:  ObjectCompletion,
		Created: r.Created,
		Model:   r.Model,
	}
	if opt != nil {
		if opt.ID != "" {
			out.ID = opt.ID
		}
		if opt.Created != 0 {
			out.Created = opt.Created
		}
		if opt.Model != "" {
			out.Model = opt.Model
		}
	}
	if r.SystemFingerprint != "" {
		out.SystemFingerprint = ptr(r.SystemFingerprint)
	}
	if r.ServiceTier != "" {
		out.ServiceTier = ptr(r.ServiceTier)
	}
	if r.Usage != nil {
		out.Usage = EncodeUsage(*r.Usage)
	}

	caps := opt.caps()
	loss := opt.loss()
	out.Choices = make([]Choice, 0, len(r.Choices))
	for i := range r.Choices {
		c := &r.Choices[i]
		wc := Choice{Index: c.Index, Logprobs: c.Logprobs}
		wc.Message = encodeAssistantMessage(&c.Message, caps, loss, "choices["+strconv.Itoa(i)+"].message")

		if c.StopReason != canonical.StopUnspecified {
			wc.FinishReason = ptr(FinishReasonOf(c.StopReason))
			native := c.NativeStopReason
			if native == "" && !c.StopReason.Expressible() {
				native = string(c.StopReason)
			}
			if native != "" {
				// COMPATIBILITY 4.3: the original survives out of band, so the
				// collapse of 4.1 loses nothing that a second hop needs.
				wc.ProviderSpecificFields = nativeFinishFields(native)
				if !caps.Has(canonical.CapRichStopReasons) && !c.StopReason.Expressible() {
					loss.Downgrade(canonical.ConstructRichStopReason,
						"choices["+strconv.Itoa(i)+"]: "+native)
				}
			}
		}
		out.Choices = append(out.Choices, wc)
	}
	return out, nil
}

func nativeFinishFields(native string) map[string]json.RawMessage {
	b, err := Marshal(native)
	if err != nil {
		return nil
	}
	return map[string]json.RawMessage{nativeFinishKey: b}
}

// encodeAssistantMessage renders a completed assistant message.
func encodeAssistantMessage(m *canonical.Message, caps canonical.Capability, loss *canonical.LossReport, where string) Message {
	out := Message{Role: string(m.Role), Name: m.Name, Extra: m.Extra}
	if out.Role == "" {
		out.Role = string(canonical.RoleAssistant)
	}
	if m.Refusal != "" {
		out.Refusal = ptr(m.Refusal)
	}

	var carried canonical.Content
	var reasoning []byte
	for i := range m.Content {
		b := &m.Content[i]
		at := where + ".content[" + strconv.Itoa(i) + "]"
		switch b.Kind {
		case canonical.KindThinking:
			if b.Thinking != nil && b.Thinking.Signature != "" {
				loss.Downgrade(canonical.ConstructThinkingBlock, at+": signature")
			}
			if !caps.Has(canonical.CapThinkingBlocks) {
				loss.Downgrade(canonical.ConstructThinkingBlock, at)
				continue
			}
			reasoning = append(reasoning, b.Text...)
		case canonical.KindToolUse:
			if b.ToolUse == nil {
				continue
			}
			args := string(b.ToolUse.Input)
			if args == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:       b.ToolUse.ID,
				Type:     "function",
				Function: FunctionCall{Name: b.ToolUse.Name, Arguments: args},
			})
		default:
			carried = append(carried, *b)
		}
	}
	if reasoning != nil {
		out.ReasoningContent = ptr(string(reasoning))
	}
	if len(carried) > 0 {
		out.Content = encodeContent(carried, caps, loss, where)
	} else if len(out.ToolCalls) == 0 {
		// An assistant message with neither content nor tool calls still needs
		// a content field; omitting it makes some clients render nothing at all.
		out.Content = TextContent("")
	}
	return out
}

// EncodeUsage renders neutral counts on the OpenAI wire.
//
// The detail sub-objects are attached only when non-zero, so a backend with no
// cache does not report a cache breakdown of zeroes on every response.
func EncodeUsage(u canonical.Usage) *Usage {
	w := &Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens(),
	}
	if u.CacheReadTokens > 0 {
		w.PromptTokensDetails = &PromptTokensDetails{CachedTokens: u.CacheReadTokens}
	}
	if u.ReasoningTokens > 0 {
		w.CompletionTokensDetails = &CompletionTokensDetails{ReasoningTokens: u.ReasoningTokens}
	}
	if u.CacheWriteTokens > 0 {
		w.CacheCreationInputTokens = ptr(u.CacheWriteTokens)
	}
	return w
}
