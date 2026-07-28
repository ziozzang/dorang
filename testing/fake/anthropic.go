package fake

import (
	"encoding/json"
	"net/http"
	"time"
)

// Anthropic wire shapes, declared here rather than imported (see doc.go).
// Field order is the vendor's order, which is what makes a golden byte
// comparison meaningful.

type antResponse struct {
	ID      string     `json:"id"`
	Type    string     `json:"type"`
	Role    string     `json:"role"`
	Model   string     `json:"model"`
	Content []antBlock `json:"content"`

	// StopReason and StopSequence have NO omitempty: message_start carries
	// "stop_reason":null and every response carries "stop_sequence":null
	// (COMPATIBILITY 6.5). This is the documented opposite of 2.1, which is
	// scoped to chat-completions chunks.
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`

	Usage *antUsage `json:"usage,omitempty"`
}

type antBlock struct {
	Type string `json:"type"`
	// Text and Thinking are POINTERS: content_block_start opens a text block
	// with {"type":"text","text":""}, and with a plain string plus omitempty
	// that opener collapses to {"type":"text"} — not the shape an SDK
	// accumulates into.
	Text     *string `json:"text,omitempty"`
	Thinking *string `json:"thinking,omitempty"`
	// Signature is integrity material. It is replayed verbatim and never
	// fabricated (DESIGN §10.2).
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type antUsage struct {
	InputTokens              *int `json:"input_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens             *int `json:"output_tokens,omitempty"`
	// TotalTokens is NOT in the vendor specification. COMPATIBILITY 6.8: the
	// non-streaming shape carries it and the streaming shape does not, and the
	// two therefore differ by exactly one field.
	TotalTokens *int `json:"total_tokens,omitempty"`
}

type antError struct {
	Type    string  `json:"type"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

type antEnvelope struct {
	Type  string   `json:"type"`
	Error antError `json:"error"`
}

type antMessageStart struct {
	Type    string      `json:"type"`
	Message antResponse `json:"message"`
}

type antBlockStart struct {
	Type         string   `json:"type"`
	Index        int      `json:"index"`
	ContentBlock antBlock `json:"content_block"`
}

type antBlockDelta struct {
	Type  string   `json:"type"`
	Index int      `json:"index"`
	Delta antDelta `json:"delta"`
}

type antDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type antBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type antMessageDelta struct {
	Type  string      `json:"type"`
	Delta antStopInfo `json:"delta"`
	Usage *antUsage   `json:"usage,omitempty"`
}

type antStopInfo struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type antMessageStop struct {
	Type string `json:"type"`
}

// antStreamUsage renders usage for a stream: cache-exclusive input, cache
// fields only when greater than zero, and NO total_tokens (COMPATIBILITY 6.7,
// 6.8).
func antStreamUsage(u Usage) *antUsage {
	w := &antUsage{
		InputTokens:  ptrOf(u.ExclusiveInput()),
		OutputTokens: ptrOf(u.OutputTokens),
	}
	if u.CacheWriteTokens > 0 {
		w.CacheCreationInputTokens = ptrOf(u.CacheWriteTokens)
	}
	if u.CacheReadTokens > 0 {
		w.CacheReadInputTokens = ptrOf(u.CacheReadTokens)
	}
	return w
}

// antJSONUsage adds the non-spec total_tokens of COMPATIBILITY 6.8, which the
// non-streaming shape carries and the streaming shape omits.
func antJSONUsage(u Usage) *antUsage {
	w := antStreamUsage(u)
	w.TotalTokens = ptrOf(u.Total())
	return w
}

// antTerminal resolves the terminal stop_reason in this family's spelling.
//
// An empty Script.Finish means the backend never sent one, and it is
// synthesized: "end_turn", upgraded to "tool_use" when a tool call was seen —
// the mirror of COMPATIBILITY 4.4.
func antTerminal(s Script) string {
	switch s.Finish {
	case "":
		if len(s.ToolCalls) > 0 {
			return "tool_use"
		}
		return "end_turn"
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		// A native value passes through. That includes the non-standard values
		// a real deployment fronting this shape emits, which a scenario may
		// want to inject deliberately.
		return s.Finish
	}
}

func antTypeForStatus(status int) string {
	switch {
	case status == 400:
		return "invalid_request_error"
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 408:
		return "timeout_error"
	case status == 413:
		return "request_too_large"
	case status == 429:
		return "rate_limit_error"
	case status == 529:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "api_error"
	}
}

func (u *Upstream) antJSON(w http.ResponseWriter, s Script) {
	var blocks []antBlock
	if s.Thinking != "" || s.Signature != "" {
		blocks = append(blocks, antBlock{
			Type: "thinking", Thinking: ptrOf(s.Thinking), Signature: s.Signature,
		})
	}
	if s.Text != "" {
		blocks = append(blocks, antBlock{Type: "text", Text: ptrOf(s.Text)})
	}
	for _, tc := range s.ToolCalls {
		args := tc.Arguments
		if args == "" {
			args = "{}"
		}
		blocks = append(blocks, antBlock{
			Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: json.RawMessage(args),
		})
	}
	if blocks == nil {
		blocks = []antBlock{}
	}
	resp := antResponse{
		ID:           s.ID,
		Type:         "message",
		Role:         "assistant",
		Model:        s.Model,
		Content:      blocks,
		StopReason:   ptrOf(antTerminal(s)),
		StopSequence: nil, // COMPATIBILITY 6.5
		Usage:        antJSONUsage(s.Usage),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(marshal(resp))
}

// antFrame is COMPATIBILITY 6.1's framing: event: AND data:, both lines. A
// chat-completions writer that emits only the data line produces a stream this
// family's SDK silently discards, because it dispatches on the event name.
func antFrame(name string, v any) []byte {
	b := make([]byte, 0, 160)
	b = append(b, "event: "...)
	b = append(b, name...)
	b = append(b, '\n')
	b = append(b, "data: "...)
	b = append(b, marshal(v)...)
	b = append(b, '\n', '\n')
	return b
}

func (u *Upstream) antStream(f *flusher, s Script, bh Behaviour) {
	// message_start seeds the prompt count only: the cache counters are not
	// known when the stream opens (COMPATIBILITY 6.7).
	seed := &antUsage{InputTokens: ptrOf(s.Usage.ExclusiveInput()), OutputTokens: ptrOf(0)}
	start := antMessageStart{
		Type: "message_start",
		Message: antResponse{
			ID: s.ID, Type: "message", Role: "assistant", Model: s.Model,
			Content: []antBlock{}, StopReason: nil, StopSequence: nil, Usage: seed,
		},
	}
	if !f.write(antFrame("message_start", start)) {
		return
	}

	content := 0
	failed := false
	// step emits one content frame. It returns false when the stream must end,
	// and sets failed when it ended by delivering an in-band error — after
	// which there is NO message_stop, because the message did not stop, it
	// failed (COMPATIBILITY 6.2).
	step := func(name string, v any) bool {
		if bh.InterFrame > 0 {
			time.Sleep(bh.InterFrame)
		}
		if !f.write(antFrame(name, v)) {
			return false
		}
		content++
		if bh.TruncateAfter > 0 && content >= bh.TruncateAfter {
			return false
		}
		if bh.FailAfter > 0 && content >= bh.FailAfter {
			msg := bh.ErrorMessage
			if msg == "" {
				msg = "upstream failed mid-stream"
			}
			typ := bh.ErrorType
			if typ == "" {
				typ = "api_error"
			}
			code := bh.ErrorCode
			if code == "" {
				code = "internal_error"
			}
			_ = f.write(antFrame("error", antEnvelope{
				Type: "error", Error: antError{Type: typ, Message: msg, Param: nil, Code: code},
			}))
			failed = true
			return false
		}
		return true
	}

	idx := 0
	// block emits a complete content block: start, deltas, stop. Every frame
	// goes through step, so a failure lands wherever the behaviour asked for it
	// — including between a block's start and its stop, which is exactly the
	// state a client has to survive.
	block := func(open antBlock, deltas []antDelta) bool {
		if !step("content_block_start", antBlockStart{
			Type: "content_block_start", Index: idx, ContentBlock: open,
		}) {
			return false
		}
		for _, d := range deltas {
			if !step("content_block_delta", antBlockDelta{
				Type: "content_block_delta", Index: idx, Delta: d,
			}) {
				return false
			}
		}
		if !step("content_block_stop", antBlockStop{Type: "content_block_stop", Index: idx}) {
			return false
		}
		idx++
		return true
	}

	if s.Thinking != "" || s.Signature != "" {
		var ds []antDelta
		if s.Thinking != "" {
			ds = append(ds, antDelta{Type: "thinking_delta", Thinking: s.Thinking})
		}
		if s.Signature != "" {
			ds = append(ds, antDelta{Type: "signature_delta", Signature: s.Signature})
		}
		if !block(antBlock{Type: "thinking", Thinking: ptrOf("")}, ds) {
			return
		}
	}
	if s.Text != "" {
		var ds []antDelta
		for _, part := range splitInto(s.Text, s.TextChunks) {
			ds = append(ds, antDelta{Type: "text_delta", Text: part})
		}
		if !block(antBlock{Type: "text", Text: ptrOf("")}, ds) {
			return
		}
	}
	for _, tc := range s.ToolCalls {
		var ds []antDelta
		for _, frag := range splitInto(tc.Arguments, tc.ArgChunks) {
			ds = append(ds, antDelta{Type: "input_json_delta", PartialJSON: frag})
		}
		open := antBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: json.RawMessage("{}")}
		if !block(open, ds) {
			return
		}
	}
	_ = failed

	// COMPATIBILITY 6.6: a content_block_stop always precedes the terminal
	// message_delta, which is why every block above closed itself before this
	// point rather than leaving the close to the terminal frame.
	if bh.InterFrame > 0 {
		time.Sleep(bh.InterFrame)
	}
	if !f.write(antFrame("message_delta", antMessageDelta{
		Type:  "message_delta",
		Delta: antStopInfo{StopReason: ptrOf(antTerminal(s)), StopSequence: nil},
		Usage: antStreamUsage(s.Usage),
	})) {
		return
	}
	// message_stop exactly once, and no [DONE] anywhere in this family.
	_ = f.write(antFrame("message_stop", antMessageStop{Type: "message_stop"}))
}
