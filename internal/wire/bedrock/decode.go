package bedrock

import (
	"encoding/json"
	"errors"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Response is the Converse response body.
type Response struct {
	Output     *Output `json:"output"`
	StopReason string  `json:"stopReason"`
	Usage      *Usage  `json:"usage"`
	Metrics    *struct {
		LatencyMs int64 `json:"latencyMs"`
	} `json:"metrics"`
}

type Output struct {
	Message *Message `json:"message"`
}

// Usage is Converse's token report. The cache counts are separate members
// and inputTokens EXCLUDES them, unlike the neutral convention (§10.7), so
// they are added back on the way in.
type Usage struct {
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	TotalTokens           int `json:"totalTokens"`
	CacheReadInputTokens  int `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
}

// ErrNotAResponse is a JSON object that is not a Converse response.
var ErrNotAResponse = errors.New("bedrock: the body is a JSON object but not a Converse response")

// DecodeOptions configures [DecodeResponse].
type DecodeOptions struct {
	// Model is the client-facing name restored into the answer (§7.2).
	Model string
}

// IsResponse reports whether a body is a Converse response: it carries
// `output` or `stopReason`.
func IsResponse(b []byte) bool {
	var probe struct {
		Output     json.RawMessage `json:"output"`
		StopReason string          `json:"stopReason"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return false
	}
	return probe.Output != nil || probe.StopReason != ""
}

// DecodeResponse converts a Converse response to the neutral form.
func DecodeResponse(b []byte, opt *DecodeOptions) (*canonical.Response, error) {
	var w Response
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	if w.Output == nil && w.StopReason == "" {
		return nil, ErrNotAResponse
	}
	out := &canonical.Response{}
	if opt != nil {
		out.Model = opt.Model
	}
	msg := canonical.Message{Role: canonical.RoleAssistant}
	if w.Output != nil && w.Output.Message != nil {
		msg.Content = decodeBlocks(w.Output.Message.Content)
	}
	stop, native := stopReasonOf(w.StopReason)
	out.Choices = []canonical.Choice{{Message: msg, StopReason: stop, NativeStopReason: native}}
	if w.Usage != nil {
		out.Usage = usageOf(w.Usage)
	}
	return out, nil
}

func usageOf(u *Usage) *canonical.Usage {
	return &canonical.Usage{
		// §10.7: the neutral input count includes what was read from cache.
		InputTokens:      u.InputTokens + u.CacheReadInputTokens + u.CacheWriteInputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheWriteInputTokens,
	}
}

func decodeBlocks(in []ContentBlock) canonical.Content {
	var out canonical.Content
	for _, b := range in {
		switch {
		case b.Text != "":
			out = append(out, canonical.TextBlock(b.Text))
		case b.ToolUse != nil:
			input := b.ToolUse.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out = append(out, canonical.ToolUseBlock(b.ToolUse.ToolUseID, b.ToolUse.Name, input))
		case b.ReasoningContent != nil:
			rc := b.ReasoningContent
			if rc.ReasoningText != nil {
				out = append(out, canonical.ThinkingBlock(rc.ReasoningText.Text, rc.ReasoningText.Signature))
			} else if rc.RedactedContent != "" {
				blk := canonical.ThinkingBlock(rc.RedactedContent, "")
				blk.Thinking.Redacted = true
				out = append(out, blk)
			}
		}
	}
	return out
}

// stopReasonOf maps Converse's reasons; the native word is kept beside the
// neutral one so nothing is lost when the two differ.
func stopReasonOf(s string) (canonical.StopReason, string) {
	switch s {
	case "end_turn":
		return canonical.StopEndTurn, ""
	case "tool_use":
		return canonical.StopToolUse, ""
	case "max_tokens":
		return canonical.StopMaxTokens, ""
	case "stop_sequence":
		return canonical.StopStopSequence, ""
	case "guardrail_intervened", "content_filtered":
		return canonical.StopContentFilter, s
	case "":
		return canonical.StopUnspecified, ""
	}
	return canonical.StopEndTurn, s
}
