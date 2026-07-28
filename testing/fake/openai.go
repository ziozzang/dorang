package fake

import (
	"net/http"
	"time"
)

// The wire shapes below are declared here rather than imported from
// internal/wire/openai on purpose (see doc.go). Field order matters: Go emits
// struct fields in declaration order and COMPATIBILITY 2.2 fixes the order of a
// plain text chunk.

type oaChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []oaChunkChoice `json:"choices"`
	Usage   *oaUsage        `json:"usage,omitempty"`
}

type oaChunkChoice struct {
	Index int     `json:"index"`
	Delta oaDelta `json:"delta"`
	// FinishReason is a pointer so an in-flight chunk omits it rather than
	// sending "finish_reason":null (COMPATIBILITY 2.1).
	FinishReason *string `json:"finish_reason,omitempty"`
}

type oaDelta struct {
	Role             *string           `json:"role,omitempty"`
	Content          *string           `json:"content,omitempty"`
	ReasoningContent *string           `json:"reasoning_content,omitempty"`
	ToolCalls        []oaToolCallDelta `json:"tool_calls,omitempty"`
}

type oaToolCallDelta struct {
	// Index is REQUIRED on every fragment (COMPATIBILITY 5.1), so it is not a
	// pointer and carries no omitempty: index 0 must appear.
	Index    int              `json:"index"`
	ID       *string          `json:"id,omitempty"`
	Type     *string          `json:"type,omitempty"`
	Function *oaFunctionDelta `json:"function,omitempty"`
}

type oaFunctionDelta struct {
	Name      *string `json:"name,omitempty"`
	Arguments *string `json:"arguments,omitempty"`
}

type oaUsage struct {
	PromptTokens            int                `json:"prompt_tokens"`
	CompletionTokens        int                `json:"completion_tokens"`
	TotalTokens             int                `json:"total_tokens"`
	PromptTokensDetails     *oaPromptDetails   `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *oaCompletionDetls `json:"completion_tokens_details,omitempty"`
	// CacheCreationInputTokens is the widely-read cache-write extension
	// (COMPATIBILITY 3.5). OpenAI itself has no such field.
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
}

type oaPromptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type oaCompletionDetls struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type oaMessage struct {
	Role      string       `json:"role"`
	Content   *string      `json:"content,omitempty"`
	ToolCalls []oaToolCall `json:"tool_calls,omitempty"`
	// ReasoningContent is the de-facto field for non-streamed reasoning text.
	ReasoningContent *string `json:"reasoning_content,omitempty"`
}

type oaToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function oaFunctionCall `json:"function"`
}

type oaFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaChoice struct {
	Index        int       `json:"index"`
	Message      oaMessage `json:"message"`
	FinishReason *string   `json:"finish_reason,omitempty"`
}

type oaResponse struct {
	ID      string     `json:"id"`
	Object  string     `json:"object"`
	Created int64      `json:"created"`
	Model   string     `json:"model"`
	Choices []oaChoice `json:"choices"`
	Usage   *oaUsage   `json:"usage,omitempty"`
}

type oaError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

type oaEnvelope struct {
	Error oaError `json:"error"`
}

func oaEncodeUsage(u Usage) *oaUsage {
	w := &oaUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.Total(),
	}
	if u.CacheReadTokens > 0 {
		w.PromptTokensDetails = &oaPromptDetails{CachedTokens: u.CacheReadTokens}
	}
	if u.ReasoningTokens > 0 {
		w.CompletionTokensDetails = &oaCompletionDetls{ReasoningTokens: u.ReasoningTokens}
	}
	if u.CacheWriteTokens > 0 {
		w.CacheCreationInputTokens = ptrOf(u.CacheWriteTokens)
	}
	return w
}

// oaTerminal resolves the terminal finish_reason.
//
// COMPATIBILITY 4.4, described there as the highest-value single line in the
// document: when the backend never sent one it is synthesized as "stop" — and
// UPGRADED to "tool_calls" if any tool call was seen. A gateway that merely
// forwards emits "stop" on a tool-call turn and breaks every agentic client.
func oaTerminal(s Script) string {
	if s.Finish != "" {
		return s.Finish
	}
	if len(s.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func (u *Upstream) writeJSON(w http.ResponseWriter, _ *Recorded, s Script) {
	switch u.Shape {
	case ShapeAnthropic:
		u.antJSON(w, s)
		return
	}
	msg := oaMessage{Role: "assistant"}
	if s.Thinking != "" {
		msg.ReasoningContent = ptrOf(s.Thinking)
	}
	if s.Text != "" {
		msg.Content = ptrOf(s.Text)
	}
	for _, tc := range s.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, oaToolCall{
			ID: tc.ID, Type: "function",
			Function: oaFunctionCall{Name: tc.Name, Arguments: tc.Arguments},
		})
	}
	resp := oaResponse{
		ID:      s.ID,
		Object:  "chat.completion",
		Created: s.Created,
		Model:   s.Model,
		Choices: []oaChoice{{Index: 0, Message: msg, FinishReason: ptrOf(oaTerminal(s))}},
		Usage:   oaEncodeUsage(s.Usage),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(marshal(resp))
}

func (u *Upstream) writeError(w http.ResponseWriter, bh Behaviour) {
	status := bh.status()
	typ := bh.ErrorType
	if typ == "" {
		if u.Shape == ShapeAnthropic {
			typ = antTypeForStatus(status)
		} else {
			typ = oaTypeForStatus(status)
		}
	}
	msg := bh.ErrorMessage
	if msg == "" {
		msg = http.StatusText(status)
	}
	code := bh.ErrorCode
	if code == "" {
		if bh.QuotaExhausted {
			code = "insufficient_quota"
		} else {
			// COMPATIBILITY 7.1: code is a STRING, never a number.
			code = http.StatusText(status)
		}
	}
	if bh.QuotaExhausted && bh.ErrorMessage == "" {
		msg = "You exceeded your current quota, please check your plan and billing details."
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if u.Shape == ShapeAnthropic {
		_, _ = w.Write(marshal(antEnvelope{
			Type:  "error",
			Error: antError{Type: typ, Message: msg, Param: nil, Code: code},
		}))
		return
	}
	_, _ = w.Write(marshal(oaEnvelope{
		Error: oaError{Message: msg, Type: typ, Param: nil, Code: code},
	}))
}

func oaTypeForStatus(status int) string {
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
	case status == 429:
		return "rate_limit_error"
	case status == 503:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "api_error"
	}
}

// writeStream emits the streaming form of the script for whichever shape this
// upstream speaks.
func (u *Upstream) writeStream(w http.ResponseWriter, rec *Recorded, s Script, bh Behaviour) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f := newFlusher(w)

	// COMPATIBILITY 1.4: an empty upstream stream is an empty body with NO
	// [DONE]. It is written before the TTFT delay because there is nothing to
	// delay.
	if bh.EmptyStream {
		return
	}
	if bh.TTFT > 0 {
		time.Sleep(bh.TTFT)
	}
	if u.Shape == ShapeAnthropic {
		u.antStream(f, s, bh)
		return
	}
	u.oaStream(f, rec, s, bh)
}

const oaDoneFrame = "data: [DONE]\n\n"

func oaFrame(v any) []byte {
	b := make([]byte, 0, 128)
	b = append(b, "data: "...)
	b = append(b, marshal(v)...)
	b = append(b, '\n', '\n')
	return b
}

func (u *Upstream) oaStream(f *flusher, rec *Recorded, s Script, bh Behaviour) {
	base := func(choices []oaChunkChoice) oaChunk {
		return oaChunk{
			ID:      s.ID,
			Object:  "chat.completion.chunk",
			Created: s.Created,
			Model:   s.Model,
			Choices: choices,
		}
	}

	// The opening role frame is not a content frame and is not counted by
	// FailAfter or TruncateAfter: those knobs are about how far into the
	// GENERATION the failure lands.
	if !f.write(oaFrame(base([]oaChunkChoice{{Index: 0, Delta: oaDelta{Role: ptrOf("assistant")}}}))) {
		return
	}

	content := 0
	sawTool := false
	// step emits one content frame and reports whether the stream should keep
	// going. It is where FailAfter and TruncateAfter are enforced, so both are
	// counted in the same units.
	step := func(c oaChunkChoice) (keepGoing bool) {
		if bh.InterFrame > 0 {
			time.Sleep(bh.InterFrame)
		}
		if !f.write(oaFrame(base([]oaChunkChoice{c}))) {
			return false
		}
		content++
		if bh.TruncateAfter > 0 && content >= bh.TruncateAfter {
			return false
		}
		if bh.FailAfter > 0 && content >= bh.FailAfter {
			// COMPATIBILITY 1.3: in band, then [DONE]. The status is already
			// 200 and cannot change.
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
			_ = f.write(oaFrame(oaEnvelope{Error: oaError{Message: msg, Type: typ, Param: nil, Code: code}}))
			_ = f.write([]byte(oaDoneFrame))
			return false
		}
		return true
	}

	for _, part := range splitInto(s.Thinking, 1) {
		if !step(oaChunkChoice{Index: 0, Delta: oaDelta{ReasoningContent: ptrOf(part)}}) {
			return
		}
	}
	for _, part := range splitInto(s.Text, s.TextChunks) {
		if !step(oaChunkChoice{Index: 0, Delta: oaDelta{Content: ptrOf(part)}}) {
			return
		}
	}
	for i, tc := range s.ToolCalls {
		sawTool = true
		open := oaToolCallDelta{
			Index: i, ID: ptrOf(tc.ID), Type: ptrOf("function"),
			Function: &oaFunctionDelta{Name: ptrOf(tc.Name)},
		}
		if !step(oaChunkChoice{Index: 0, Delta: oaDelta{ToolCalls: []oaToolCallDelta{open}}}) {
			return
		}
		for _, frag := range splitInto(tc.Arguments, tc.ArgChunks) {
			frag := frag
			d := oaToolCallDelta{Index: i, Function: &oaFunctionDelta{Arguments: &frag}}
			if !step(oaChunkChoice{Index: 0, Delta: oaDelta{ToolCalls: []oaToolCallDelta{d}}}) {
				return
			}
		}
	}

	finish := s.Finish
	if finish == "" {
		finish = "stop"
		if sawTool {
			finish = "tool_calls"
		}
	}
	if bh.InterFrame > 0 {
		time.Sleep(bh.InterFrame)
	}
	if !f.write(oaFrame(base([]oaChunkChoice{{Index: 0, Delta: oaDelta{}, FinishReason: ptrOf(finish)}}))) {
		return
	}

	// COMPATIBILITY 3.1/3.2: the usage chunk goes out ONLY when the request
	// carried stream_options.include_usage exactly true. Without it usage is
	// computed and stays off the wire.
	if rec.IncludeUsage {
		c := base([]oaChunkChoice{{Index: 0, Delta: oaDelta{}}})
		c.Usage = oaEncodeUsage(s.Usage)
		if !f.write(oaFrame(c)) {
			return
		}
	}
	_ = f.write([]byte(oaDoneFrame))
}
