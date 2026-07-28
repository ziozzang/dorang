package fake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
)

// The self-check.
//
// A fake that is wrong makes every test that passes against it worthless, so
// this file does not assert that the fake decodes its own output — which any
// self-consistent encoder passes. It re-encodes the SAME logical content with
// internal/wire and compares the two byte streams. The fake's frames are built
// from types declared in this package (openai.go, anthropic.go); the comparison
// frames are built by the encoder the gateway actually ships. Two independent
// encoders agreeing byte-for-byte is evidence; one encoder agreeing with itself
// is not.
//
// Every subtest names the COMPATIBILITY row it protects.

const (
	selfModel = "qwen3.5:397b" // opaque: nothing splits it (DESIGN §2.1)
	selfID    = "chatcmpl-selfcheck"
	selfMsgID = "msg_selfcheck"
)

// canonicalEvents renders a Script as the neutral event sequence a backend
// adapter would produce from it. This is the INPUT both encoders receive; the
// bytes are the output being compared.
func canonicalEvents(s Script) []canonical.StreamEvent {
	var out []canonical.StreamEvent
	out = append(out, canonical.StreamEvent{
		Type:  canonical.EventDelta,
		Delta: canonical.Delta{Role: canonical.RoleAssistant},
	})
	if s.Thinking != "" {
		out = append(out, canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			Content: []canonical.Block{canonical.ThinkingBlock(s.Thinking, "")},
		}})
	}
	for _, part := range splitInto(s.Text, s.TextChunks) {
		out = append(out, canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			Content: []canonical.Block{canonical.TextBlock(part)},
		}})
	}
	for i, tc := range s.ToolCalls {
		out = append(out, canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			ToolCalls: []canonical.ToolCallDelta{{Index: i, ID: tc.ID, Name: tc.Name}},
		}})
		for _, frag := range splitInto(tc.Arguments, tc.ArgChunks) {
			out = append(out, canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
				ToolCalls: []canonical.ToolCallDelta{{Index: i, Arguments: frag}},
			}})
		}
	}
	if s.Finish != "" {
		out = append(out, canonical.StreamEvent{Type: canonical.EventStop, Delta: canonical.Delta{
			StopReason: canonicalStop(s.Finish),
		}})
	}
	u := canonicalUsage(s.Usage)
	if !u.Empty() {
		out = append(out, canonical.StreamEvent{Type: canonical.EventUsage, Usage: &u})
	}
	return out
}

func canonicalStop(finish string) canonical.StopReason {
	switch finish {
	case "stop", "end_turn":
		return canonical.StopEndTurn
	case "length", "max_tokens":
		return canonical.StopMaxTokens
	case "tool_calls", "tool_use":
		return canonical.StopToolUse
	default:
		return canonical.StopEndTurn
	}
}

func canonicalUsage(u Usage) canonical.Usage {
	return canonical.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		ReasoningTokens:  u.ReasoningTokens,
	}
}

// wireOpenAIStream renders the script through internal/wire/openai.
func wireOpenAIStream(t *testing.T, s Script, includeUsage bool) string {
	t.Helper()
	var buf bytes.Buffer
	w := openai.NewStreamWriter(&buf, openai.StreamConfig{
		ID: s.ID, Created: s.Created, Model: s.Model, IncludeUsage: includeUsage,
	})
	for _, ev := range canonicalEvents(s) {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatalf("wire WriteEvent: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wire Close: %v", err)
	}
	return buf.String()
}

// wireAnthropicStream renders the script through internal/wire/anthropic.
func wireAnthropicStream(t *testing.T, s Script) string {
	t.Helper()
	var buf bytes.Buffer
	w := anthropic.NewStreamWriter(&buf, anthropic.StreamConfig{
		ID:    s.ID,
		Model: s.Model,
		// The seed is the prompt count only: at message_start the cache
		// counters are not known (COMPATIBILITY 6.7). The fake seeds the same
		// way, which is what makes the first frame comparable at all.
		Usage: canonical.Usage{InputTokens: s.Usage.ExclusiveInput()},
	})
	for _, ev := range canonicalEvents(s) {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatalf("wire WriteEvent: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wire Close: %v", err)
	}
	return buf.String()
}

// fetch drives one request against an upstream and returns status, headers and
// body.
func fetch(t *testing.T, u *Upstream, body string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Post(u.Endpoint(), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", u.Endpoint(), err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

func upstream(t *testing.T, shape Shape, s Script, bh *Behaviour) *Upstream {
	t.Helper()
	opts := Options{
		Shape:  shape,
		Script: func(*Recorded) Script { return s },
	}
	if bh != nil {
		opts.Behaviour = func(*Recorded) Behaviour { return *bh }
	}
	u := New(opts)
	t.Cleanup(u.Close)
	return u
}

// -----------------------------------------------------------------------------
// OpenAI: the fake's bytes against the encoder the gateway ships
// -----------------------------------------------------------------------------

func TestOpenAIStreamMatchesWireEncoder(t *testing.T) {
	cases := []struct {
		name         string
		script       Script
		includeUsage bool
	}{
		{
			name: "text only, terminal synthesized",
			script: Script{
				ID: selfID, Created: fixedCreated, Model: selfModel,
				Text: "Hello, world", TextChunks: 3,
				Usage: Usage{InputTokens: 12, OutputTokens: 5},
			},
		},
		{
			name: "text with usage chunk",
			script: Script{
				ID: selfID, Created: fixedCreated, Model: selfModel,
				Text: "Hi", TextChunks: 1,
				Usage: Usage{InputTokens: 12, OutputTokens: 5},
			},
			includeUsage: true,
		},
		{
			name: "cache and reasoning detail in usage",
			script: Script{
				ID: selfID, Created: fixedCreated, Model: selfModel,
				Text: "cached", TextChunks: 1,
				Usage: Usage{InputTokens: 100, OutputTokens: 25, CacheReadTokens: 40,
					CacheWriteTokens: 10, ReasoningTokens: 7},
			},
			includeUsage: true,
		},
		{
			name: "tool call, terminal upgraded to tool_calls",
			script: Script{
				ID: selfID, Created: fixedCreated, Model: selfModel,
				ToolCalls: []ToolCall{{ID: "call_abc", Name: "get_weather",
					Arguments: `{"city":"Seoul"}`, ArgChunks: 2}},
				Usage: Usage{InputTokens: 9, OutputTokens: 3},
			},
		},
		{
			name: "text then two tool calls",
			script: Script{
				ID: selfID, Created: fixedCreated, Model: selfModel,
				Text: "Checking.", TextChunks: 1,
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "a", Arguments: `{"x":1}`, ArgChunks: 2},
					{ID: "call_2", Name: "b", Arguments: `{"y":2}`, ArgChunks: 2},
				},
				Usage: Usage{InputTokens: 9, OutputTokens: 3},
			},
		},
		{
			name: "explicit backend finish reason",
			script: Script{
				ID: selfID, Created: fixedCreated, Model: selfModel,
				Text: "done", TextChunks: 1, Finish: "stop",
				Usage: Usage{InputTokens: 4, OutputTokens: 1},
			},
		},
		{
			name: "reasoning text",
			script: Script{
				ID: selfID, Created: fixedCreated, Model: selfModel,
				Thinking: "I should check.", Text: "Checked.", TextChunks: 1,
				Usage: Usage{InputTokens: 4, OutputTokens: 1},
			},
		},
		{
			name: "html-significant bytes are not escaped",
			script: Script{
				ID: selfID, Created: fixedCreated, Model: selfModel,
				Text: "a && b <tag> \"quoted\" 한국어", TextChunks: 1,
				Usage: Usage{InputTokens: 4, OutputTokens: 1},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := upstream(t, ShapeOpenAI, tc.script, nil)
			body := `{"model":"` + selfModel + `","messages":[],"stream":true`
			if tc.includeUsage {
				body += `,"stream_options":{"include_usage":true}`
			}
			body += `}`
			_, _, got := fetch(t, u, body)
			want := wireOpenAIStream(t, tc.script, tc.includeUsage)
			if got != want {
				t.Fatalf("the fake and internal/wire/openai disagree.\n"+
					"A disagreement here means every scenario that passes against this fake is\n"+
					"proving nothing.\n\nfake:\n%s\nwire:\n%s", got, want)
			}
		})
	}
}

func TestOpenAINonStreamMatchesWireEncoder(t *testing.T) {
	s := Script{
		ID: selfID, Created: fixedCreated, Model: selfModel,
		Text: "done", Finish: "stop",
		Usage: Usage{InputTokens: 4, OutputTokens: 1},
	}
	u := upstream(t, ShapeOpenAI, s, nil)
	_, _, got := fetch(t, u, `{"model":"`+selfModel+`","messages":[]}`)

	r := &canonical.Response{
		ID: s.ID, Model: s.Model, Created: s.Created,
		Choices: []canonical.Choice{{
			Index:      0,
			Message:    canonical.TextMessage(canonical.RoleAssistant, s.Text),
			StopReason: canonical.StopEndTurn,
		}},
		Usage: func() *canonical.Usage { u := canonicalUsage(s.Usage); return &u }(),
	}
	want, err := openai.MarshalResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	if got != string(want) {
		t.Fatalf("non-streaming shape disagrees.\nfake: %s\nwire: %s", got, want)
	}
}

// TestOpenAIFramingContract covers the framing rows that a byte comparison
// against one script cannot: they are properties of every stream.
func TestOpenAIFramingContract(t *testing.T) {
	s := Script{
		ID: selfID, Created: fixedCreated, Model: selfModel,
		Text: "one two three", TextChunks: 3,
		Usage: Usage{InputTokens: 12, OutputTokens: 5},
	}

	t.Run("1.1 every frame is data: <json> and nothing else", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, nil)
		_, _, body := fetch(t, u, `{"model":"m","stream":true}`)
		for _, frame := range splitFrames(body) {
			if !strings.HasPrefix(frame, "data: ") {
				t.Fatalf("frame is not a data line: %q", frame)
			}
			if strings.Contains(frame, "event:") || strings.Contains(frame, "\nid:") {
				t.Fatalf("chat completions carries no event: or id: line: %q", frame)
			}
		}
	})

	t.Run("1.2 the stream terminates with [DONE]", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, nil)
		_, _, body := fetch(t, u, `{"model":"m","stream":true}`)
		if !strings.HasSuffix(body, "data: [DONE]\n\n") {
			t.Fatalf("no [DONE] terminator:\n%s", body)
		}
	})

	t.Run("1.4 an empty stream has no [DONE]", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, &Behaviour{EmptyStream: true})
		_, _, body := fetch(t, u, `{"model":"m","stream":true}`)
		if body != "" {
			t.Fatalf("an empty upstream stream must be an empty body, got %q", body)
		}
	})

	t.Run("2.1 absent fields are omitted, never null", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, nil)
		_, _, body := fetch(t, u, `{"model":"m","stream":true}`)
		for _, forbidden := range []string{"null", "logprobs", "system_fingerprint", "service_tier"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("stream contains %q, which COMPATIBILITY 2.1/2.2 forbids:\n%s", forbidden, body)
			}
		}
	})

	t.Run("2.3/2.4 object is fixed and id and created are pinned", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, nil)
		_, _, body := fetch(t, u, `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`)
		n := 0
		for _, frame := range splitFrames(body) {
			payload := strings.TrimPrefix(frame, "data: ")
			if payload == "[DONE]" {
				continue
			}
			var c struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				Created int64  `json:"created"`
				Model   string `json:"model"`
			}
			if err := json.Unmarshal([]byte(payload), &c); err != nil {
				t.Fatalf("frame is not JSON: %q: %v", payload, err)
			}
			if c.Object != "chat.completion.chunk" {
				t.Errorf("object = %q, want chat.completion.chunk", c.Object)
			}
			if c.ID != selfID || c.Created != fixedCreated {
				t.Errorf("id/created not pinned: %q/%d", c.ID, c.Created)
			}
			if c.Model != selfModel {
				t.Errorf("model not restamped on every chunk: %q", c.Model)
			}
			n++
		}
		if n < 3 {
			t.Fatalf("expected several frames, got %d", n)
		}
	})

	t.Run("3.1 usage only on an exact include_usage", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, nil)
		_, _, with := fetch(t, u, `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`)
		if !strings.Contains(with, `"usage":{"prompt_tokens":12`) {
			t.Fatalf("include_usage true did not produce a usage chunk:\n%s", with)
		}

		// The inverse. A fake that emits usage unconditionally passes every
		// usage assertion above while making 3.2 untestable, so the absence is
		// asserted as hard as the presence.
		t.Run("inverse: no stream_options means no usage on the wire", func(t *testing.T) {
			_, _, without := fetch(t, u, `{"model":"m","stream":true}`)
			if strings.Contains(without, "usage") {
				t.Fatalf("usage reached the wire without stream_options (3.2):\n%s", without)
			}
		})
		t.Run("inverse: a truthy include_usage is not true", func(t *testing.T) {
			// COMPATIBILITY 3.1 says "exactly true. Truthy is not enough."
			for _, truthy := range []string{`1`, `"true"`, `"1"`} {
				body := `{"model":"m","stream":true,"stream_options":{"include_usage":` + truthy + `}}`
				_, _, got := fetch(t, u, body)
				if strings.Contains(got, "usage") {
					t.Errorf("include_usage %s produced a usage chunk; truthy is not true", truthy)
				}
			}
		})
	})

	t.Run("4.4 synthesized terminal is upgraded to tool_calls", func(t *testing.T) {
		tool := Script{
			ID: selfID, Created: fixedCreated, Model: selfModel,
			ToolCalls: []ToolCall{{ID: "call_1", Name: "search", Arguments: "{}"}},
		}
		u := upstream(t, ShapeOpenAI, tool, nil)
		_, _, body := fetch(t, u, `{"model":"m","stream":true}`)
		if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
			t.Fatalf("a tool-call turn must terminate with tool_calls, not stop:\n%s", body)
		}

		// The inverse: a plain turn must NOT be tool_calls. Without this a fake
		// that hard-codes tool_calls passes the row above and silently makes
		// every non-tool scenario wrong.
		t.Run("inverse: a plain turn stays stop", func(t *testing.T) {
			u := upstream(t, ShapeOpenAI, s, nil)
			_, _, body := fetch(t, u, `{"model":"m","stream":true}`)
			if strings.Contains(body, `"finish_reason":"tool_calls"`) {
				t.Fatalf("a turn with no tool call must terminate with stop:\n%s", body)
			}
			if !strings.Contains(body, `"finish_reason":"stop"`) {
				t.Fatalf("no terminal chunk at all:\n%s", body)
			}
		})
	})

	t.Run("5.1 tool_calls index is present on every fragment", func(t *testing.T) {
		tool := Script{
			ID: selfID, Created: fixedCreated, Model: selfModel,
			ToolCalls: []ToolCall{{ID: "c", Name: "n", Arguments: `{"a":1}`, ArgChunks: 3}},
		}
		u := upstream(t, ShapeOpenAI, tool, nil)
		_, _, body := fetch(t, u, `{"model":"m","stream":true}`)
		frags := 0
		for _, frame := range splitFrames(body) {
			payload := strings.TrimPrefix(frame, "data: ")
			if !strings.Contains(payload, "tool_calls") {
				continue
			}
			frags++
			if !strings.Contains(payload, `"index":0`) {
				t.Errorf("tool-call fragment without an index: %s", payload)
			}
		}
		if frags < 2 {
			t.Fatalf("expected the argument object to arrive in fragments, saw %d", frags)
		}
	})
}

// -----------------------------------------------------------------------------
// Anthropic
// -----------------------------------------------------------------------------

func TestAnthropicStreamMatchesWireEncoder(t *testing.T) {
	cases := []struct {
		name   string
		script Script
	}{
		{
			name: "text only",
			script: Script{
				ID: selfMsgID, Model: "claude-x", Text: "Hello there", TextChunks: 2,
				Usage: Usage{InputTokens: 50, OutputTokens: 12},
			},
		},
		{
			name: "thinking, text and a tool call with cache counters",
			script: Script{
				ID: selfMsgID, Model: "claude-x",
				Thinking: "I should check the weather.",
				Text:     "Let me look.", TextChunks: 1,
				ToolCalls: []ToolCall{{ID: "toolu_01", Name: "get_weather",
					Arguments: `{"city":"Seoul"}`, ArgChunks: 2}},
				Usage: Usage{InputTokens: 100, OutputTokens: 25,
					CacheReadTokens: 40, CacheWriteTokens: 10},
			},
		},
		{
			name: "explicit max_tokens finish",
			script: Script{
				ID: selfMsgID, Model: "claude-x", Text: "cut off", TextChunks: 1,
				Finish: "max_tokens",
				Usage:  Usage{InputTokens: 8, OutputTokens: 4},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := upstream(t, ShapeAnthropic, tc.script, nil)
			_, _, got := fetch(t, u, `{"model":"claude-x","messages":[],"stream":true,"max_tokens":64}`)
			want := wireAnthropicStream(t, tc.script)
			if got != want {
				t.Fatalf("the fake and internal/wire/anthropic disagree.\n\nfake:\n%s\nwire:\n%s", got, want)
			}
		})
	}
}

func TestAnthropicNonStreamMatchesWireEncoder(t *testing.T) {
	s := Script{
		ID: selfMsgID, Model: "claude-x", Text: "Hello", Finish: "end_turn",
		Usage: Usage{InputTokens: 100, OutputTokens: 25, CacheReadTokens: 40, CacheWriteTokens: 10},
	}
	u := upstream(t, ShapeAnthropic, s, nil)
	_, _, got := fetch(t, u, `{"model":"claude-x","messages":[],"max_tokens":64}`)

	cu := canonicalUsage(s.Usage)
	r := &canonical.Response{
		ID: s.ID, Model: s.Model,
		Choices: []canonical.Choice{{
			Index:      0,
			Message:    canonical.TextMessage(canonical.RoleAssistant, s.Text),
			StopReason: canonical.StopEndTurn,
		}},
		Usage: &cu,
	}
	want, err := anthropic.MarshalResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	if got != string(want) {
		t.Fatalf("non-streaming shape disagrees.\nfake: %s\nwire: %s", got, want)
	}

	// 6.8: the streaming shape omits total_tokens and the non-streaming one
	// carries it. The two differ by exactly that field, and reproducing the
	// asymmetry is the point.
	t.Run("6.8 total_tokens is on the non-streaming shape only", func(t *testing.T) {
		if !strings.Contains(got, `"total_tokens":125`) {
			t.Errorf("non-streaming response must carry the non-spec total_tokens: %s", got)
		}
		_, _, stream := fetch(t, u, `{"model":"claude-x","messages":[],"stream":true,"max_tokens":64}`)
		if strings.Contains(stream, "total_tokens") {
			t.Errorf("a stream must NOT carry total_tokens:\n%s", stream)
		}
	})
}

func TestAnthropicFramingContract(t *testing.T) {
	s := Script{
		ID: selfMsgID, Model: "claude-x",
		Thinking: "hmm", Text: "Hello there", TextChunks: 2,
		ToolCalls: []ToolCall{{ID: "toolu_1", Name: "t", Arguments: `{"a":1}`, ArgChunks: 2}},
		Usage:     Usage{InputTokens: 50, OutputTokens: 12},
	}
	u := upstream(t, ShapeAnthropic, s, nil)
	_, _, body := fetch(t, u, `{"model":"claude-x","messages":[],"stream":true,"max_tokens":64}`)

	t.Run("6.1 every frame carries BOTH an event: and a data: line", func(t *testing.T) {
		for _, frame := range splitFrames(body) {
			lines := strings.Split(frame, "\n")
			if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
				t.Fatalf("frame is not event:+data: %q", frame)
			}
		}
	})

	t.Run("6.2 no ping, no [DONE], message_stop exactly once", func(t *testing.T) {
		if strings.Contains(body, "event: ping") {
			t.Error("this family's stream must not carry ping")
		}
		if strings.Contains(body, "[DONE]") {
			t.Error("this family's stream must not carry [DONE]")
		}
		if n := strings.Count(body, "event: message_stop"); n != 1 {
			t.Errorf("message_stop appeared %d times, want exactly 1", n)
		}
		for _, name := range []string{"message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop"} {
			if !strings.Contains(body, "event: "+name) {
				t.Errorf("event type %s never appeared", name)
			}
		}
	})

	t.Run("6.5 stop_sequence is null", func(t *testing.T) {
		if !strings.Contains(body, `"stop_sequence":null`) {
			t.Errorf("stop_sequence must be present and null:\n%s", body)
		}
	})

	t.Run("6.6 a content_block_stop always precedes the terminal message_delta", func(t *testing.T) {
		lastStop := strings.LastIndex(body, "event: content_block_stop")
		delta := strings.Index(body, "event: message_delta")
		if lastStop < 0 || delta < 0 {
			t.Fatalf("missing frames:\n%s", body)
		}
		if lastStop > delta {
			t.Fatalf("the open block was not closed before message_delta:\n%s", body)
		}

		// The inverse a naive emitter gets wrong: block indexes must be
		// allocated per block and must not repeat, or the client accumulates
		// the tool call into the text block.
		t.Run("inverse: block indexes are distinct and ascending", func(t *testing.T) {
			var seen []int
			for _, frame := range splitFrames(body) {
				if !strings.HasPrefix(frame, "event: content_block_start") {
					continue
				}
				var ev struct {
					Index int `json:"index"`
				}
				payload := frame[strings.Index(frame, "data: ")+len("data: "):]
				if err := json.Unmarshal([]byte(payload), &ev); err != nil {
					t.Fatalf("bad frame %q: %v", payload, err)
				}
				seen = append(seen, ev.Index)
			}
			if len(seen) != 3 {
				t.Fatalf("expected three blocks (thinking, text, tool_use), got %d: %v", len(seen), seen)
			}
			for i, idx := range seen {
				if idx != i {
					t.Fatalf("block indexes are not 0,1,2…: %v", seen)
				}
			}
		})
	})

	t.Run("6.7 cache fields appear only when greater than zero", func(t *testing.T) {
		if strings.Contains(body, "cache_read_input_tokens") || strings.Contains(body, "cache_creation_input_tokens") {
			t.Errorf("a stream with no cache activity must omit the cache fields:\n%s", body)
		}
		cached := Script{
			ID: selfMsgID, Model: "claude-x", Text: "x", TextChunks: 1,
			Usage: Usage{InputTokens: 100, OutputTokens: 5, CacheReadTokens: 40, CacheWriteTokens: 10},
		}
		cu := upstream(t, ShapeAnthropic, cached, nil)
		_, _, cb := fetch(t, cu, `{"model":"claude-x","messages":[],"stream":true,"max_tokens":8}`)
		// input_tokens on the wire is EXCLUSIVE: 100 - 40 - 10 = 50.
		if !strings.Contains(cb, `"input_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":40,"output_tokens":5`) {
			t.Fatalf("terminal usage is not the cache-exclusive shape:\n%s", cb)
		}
	})
}

// -----------------------------------------------------------------------------
// The fake's output through the gateway's own decoders
// -----------------------------------------------------------------------------

func TestFakeOutputDecodesThroughWirePackages(t *testing.T) {
	t.Run("openai frames decode", func(t *testing.T) {
		s := Script{
			ID: selfID, Created: fixedCreated, Model: selfModel,
			Text: "Hello", TextChunks: 2,
			ToolCalls: []ToolCall{{ID: "c", Name: "n", Arguments: `{"a":1}`, ArgChunks: 2}},
			Usage:     Usage{InputTokens: 3, OutputTokens: 2},
		}
		u := upstream(t, ShapeOpenAI, s, nil)
		_, _, body := fetch(t, u, `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`)
		text, sawTool, finish := "", false, ""
		for _, frame := range splitFrames(body) {
			payload := strings.TrimPrefix(frame, "data: ")
			if payload == "[DONE]" {
				continue
			}
			c, _, err := openai.DecodeChunk([]byte(payload), nil)
			if err != nil {
				t.Fatalf("DecodeChunk(%s): %v", payload, err)
			}
			for _, ch := range c.Choices {
				if ch.Delta.Content != nil {
					text += *ch.Delta.Content
				}
				if len(ch.Delta.ToolCalls) > 0 {
					sawTool = true
				}
				if ch.FinishReason != nil {
					finish = *ch.FinishReason
				}
			}
		}
		if text != "Hello" {
			t.Errorf("reassembled text = %q, want Hello", text)
		}
		if !sawTool {
			t.Error("no tool call survived the round trip")
		}
		if finish != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls", finish)
		}
	})

	t.Run("anthropic stream decodes", func(t *testing.T) {
		s := Script{
			ID: selfMsgID, Model: "claude-x", Text: "Hello", TextChunks: 2,
			Usage: Usage{InputTokens: 3, OutputTokens: 2},
		}
		u := upstream(t, ShapeAnthropic, s, nil)
		_, _, body := fetch(t, u, `{"model":"claude-x","messages":[],"stream":true,"max_tokens":8}`)
		events, err := anthropic.DecodeStream([]byte(body), nil)
		if err != nil {
			t.Fatalf("DecodeStream: %v", err)
		}
		var text string
		for _, ev := range events {
			for _, b := range ev.Delta.Content {
				text += b.Text
			}
		}
		if text != "Hello" {
			t.Errorf("reassembled text = %q, want Hello", text)
		}
	})
}

// -----------------------------------------------------------------------------
// Injectable behaviours
// -----------------------------------------------------------------------------

func TestBehaviourErrorResponses(t *testing.T) {
	s := Script{ID: selfID, Created: fixedCreated, Model: selfModel, Text: "x"}

	t.Run("error status carries a string code and retry-after", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, &Behaviour{Status: 429, RetryAfter: 3 * time.Second})
		status, h, body := fetch(t, u, `{"model":"m"}`)
		if status != 429 {
			t.Fatalf("status = %d, want 429", status)
		}
		if h.Get("retry-after") != "3" {
			t.Errorf("retry-after = %q, want 3", h.Get("retry-after"))
		}
		var env struct {
			Error struct {
				Code json.RawMessage `json:"code"`
				Type string          `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("error body is not the envelope: %s", body)
		}
		// COMPATIBILITY 7.1: code is a STRING, not a number.
		if !bytes.HasPrefix(env.Error.Code, []byte(`"`)) {
			t.Errorf("code must be a string, got %s", env.Error.Code)
		}
		if env.Error.Type != "rate_limit_error" {
			t.Errorf("type = %q, want rate_limit_error", env.Error.Type)
		}
	})

	t.Run("quota exhaustion names the reset instant", func(t *testing.T) {
		reset := time.Date(2026, 7, 28, 18, 0, 0, 0, time.UTC)
		u := upstream(t, ShapeOpenAI, s, &Behaviour{QuotaExhausted: true, QuotaResetAt: reset})
		status, h, body := fetch(t, u, `{"model":"m"}`)
		if status != 429 {
			t.Fatalf("status = %d, want 429", status)
		}
		if got := h.Get("x-ratelimit-reset-requests"); got != reset.Format(time.RFC3339) {
			t.Errorf("reset header = %q, want %s", got, reset.Format(time.RFC3339))
		}
		if h.Get("x-ratelimit-remaining-requests") != "0" {
			t.Error("an exhausted quota must report zero remaining")
		}
		if !strings.Contains(body, "insufficient_quota") {
			t.Errorf("quota exhaustion must be distinguishable from a plain rate limit: %s", body)
		}
	})

	t.Run("anthropic error envelope carries the type discriminator", func(t *testing.T) {
		u := upstream(t, ShapeAnthropic, s, &Behaviour{Status: 529})
		_, _, body := fetch(t, u, `{"model":"claude-x","messages":[]}`)
		var env struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("bad envelope %s: %v", body, err)
		}
		if env.Type != "error" {
			t.Errorf("this family's SDK dispatches on the outer type; got %q", env.Type)
		}
		if env.Error.Type != "overloaded_error" {
			t.Errorf("529 is this family's overloaded_error, got %q", env.Error.Type)
		}
	})
}

func TestBehaviourMidStreamFailure(t *testing.T) {
	s := Script{
		ID: selfID, Created: fixedCreated, Model: selfModel,
		Text: "one two three four", TextChunks: 4,
	}

	t.Run("1.3 openai delivers the error in band, then [DONE]", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, &Behaviour{FailAfter: 2})
		status, _, body := fetch(t, u, `{"model":"m","stream":true}`)
		if status != 200 {
			t.Fatalf("status = %d; once a frame is out the status is already 200", status)
		}
		frames := splitFrames(body)
		if len(frames) < 4 {
			t.Fatalf("expected role + 2 content + error + DONE, got %d:\n%s", len(frames), body)
		}
		if !strings.Contains(frames[len(frames)-2], `"error"`) {
			t.Fatalf("no in-band error frame:\n%s", body)
		}
		if frames[len(frames)-1] != "data: [DONE]" {
			t.Fatalf("the error frame must be followed by [DONE]:\n%s", body)
		}
		if strings.Contains(body, `"finish_reason"`) {
			t.Fatalf("a failed stream must not claim a normal terminal reason:\n%s", body)
		}
	})

	t.Run("anthropic delivers an error event and NO message_stop", func(t *testing.T) {
		u := upstream(t, ShapeAnthropic, s, &Behaviour{FailAfter: 2})
		_, _, body := fetch(t, u, `{"model":"claude-x","messages":[],"stream":true,"max_tokens":8}`)
		if !strings.Contains(body, "event: error\n") {
			t.Fatalf("no in-band error event:\n%s", body)
		}
		// The message did not stop, it failed. An SDK that sees message_stop
		// after an error reports a successful empty turn.
		if strings.Contains(body, "event: message_stop") {
			t.Fatalf("message_stop must not follow an error:\n%s", body)
		}
		if strings.Contains(body, "event: message_delta") {
			t.Fatalf("a failed stream must not carry a terminal message_delta:\n%s", body)
		}
	})
}

func TestBehaviourSilentTruncation(t *testing.T) {
	s := Script{
		ID: selfID, Created: fixedCreated, Model: selfModel,
		Text: "one two three four", TextChunks: 4,
	}
	u := upstream(t, ShapeOpenAI, s, &Behaviour{TruncateAfter: 2})
	_, _, body := fetch(t, u, `{"model":"m","stream":true}`)

	// Silent truncation is the failure a client cannot see from any single
	// frame: every frame is well formed and the ending simply never arrives.
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("a silently truncated stream must not terminate:\n%s", body)
	}
	if strings.Contains(body, "finish_reason") {
		t.Fatalf("a silently truncated stream must not carry a terminal reason:\n%s", body)
	}
	if n := len(splitFrames(body)); n != 3 { // role + two content frames
		t.Fatalf("expected 3 frames before truncation, got %d:\n%s", n, body)
	}
}

func TestBehaviourTiming(t *testing.T) {
	s := Script{ID: selfID, Created: fixedCreated, Model: selfModel, Text: "abc", TextChunks: 3}

	t.Run("latency delays the headers", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, &Behaviour{Latency: 40 * time.Millisecond})
		start := time.Now()
		resp, err := http.Post(u.Endpoint(), "application/json", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(start)
		_ = resp.Body.Close()
		if elapsed < 40*time.Millisecond {
			t.Fatalf("headers arrived after %v, want at least 40ms", elapsed)
		}
	})

	t.Run("TTFT delays the first frame but not the headers", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, &Behaviour{TTFT: 40 * time.Millisecond})
		start := time.Now()
		resp, err := http.Post(u.Endpoint(), "application/json", strings.NewReader(`{"model":"m","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		headers := time.Since(start)
		buf := make([]byte, 1)
		if _, err := resp.Body.Read(buf); err != nil {
			t.Fatalf("read first byte: %v", err)
		}
		first := time.Since(start)
		if first < 40*time.Millisecond {
			t.Fatalf("first byte after %v, want at least 40ms", first)
		}
		// This is the distinction DESIGN §7.6 turns on: the response has
		// committed to 200 long before the first byte, so a fallback after this
		// point cannot change the status.
		if headers > 30*time.Millisecond {
			t.Logf("headers took %v; the split is best-effort under load", headers)
		}
	})

	t.Run("slow generation spaces the frames out", func(t *testing.T) {
		u := upstream(t, ShapeOpenAI, s, &Behaviour{InterFrame: 15 * time.Millisecond})
		start := time.Now()
		_, _, body := fetch(t, u, `{"model":"m","stream":true}`)
		elapsed := time.Since(start)
		frames := len(splitFrames(body))
		if elapsed < 45*time.Millisecond {
			t.Fatalf("%d frames at 15ms apart took only %v", frames, elapsed)
		}
	})
}

// TestBehaviourVariesByRequest is what makes the fallback scenarios expressible
// without shared mutable state in the test.
func TestBehaviourVariesByRequest(t *testing.T) {
	u := New(Options{
		Shape:  ShapeOpenAI,
		Script: func(*Recorded) Script { return Script{Text: "ok"} },
		Behaviour: func(r *Recorded) Behaviour {
			if r.Seq == 1 {
				return Behaviour{Status: 503}
			}
			return Behaviour{}
		},
	})
	t.Cleanup(u.Close)

	if s, _, _ := fetch(t, u, `{"model":"m"}`); s != 503 {
		t.Fatalf("first attempt status = %d, want 503", s)
	}
	if s, _, _ := fetch(t, u, `{"model":"m"}`); s != 200 {
		t.Fatalf("second attempt status = %d, want 200", s)
	}
	if u.Count() != 2 {
		t.Fatalf("recorded %d requests, want 2", u.Count())
	}
}

// TestRequestPeekIsCaseSensitive covers COMPATIBILITY 2.0, which is a security
// property rather than a style choice: a struct decode fills a `model` field
// from a key spelled `Model`, and a hand-written scanner does not. A fake that
// is case-insensitive would hide a gateway that is.
func TestRequestPeekIsCaseSensitive(t *testing.T) {
	u := upstream(t, ShapeOpenAI, Script{Text: "x"}, nil)
	fetch(t, u, `{"Model":"expensive","messages":[]}`)
	rec := u.Last()
	if rec.Model != "" {
		t.Fatalf(`a key spelled "Model" must not fill the model field, got %q`, rec.Model)
	}
	fetch(t, u, `{"model":"cheap","messages":[]}`)
	if u.Last().Model != "cheap" {
		t.Fatalf(`model = %q, want cheap`, u.Last().Model)
	}
}

// splitFrames cuts an SSE body into frames, dropping the trailing empty piece.
func splitFrames(body string) []string {
	if body == "" {
		return nil
	}
	parts := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
