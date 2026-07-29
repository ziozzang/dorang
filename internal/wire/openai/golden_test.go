package openai

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Real model names, used everywhere below. They contain ':' and must survive as
// single opaque strings — nothing in dorang splits a model name (DESIGN §2.1).
var opaqueModels = []string{
	"gemma4:31b",
	"zai:glm-5.1",
	"deepseek-v4-flash:cloud",
	"qwen3.5:397b",
}

const (
	testID      = "chatcmpl-0123456789abcdef"
	testCreated = int64(1753660800)
	testModel   = "qwen3.5:397b"
)

func newTestWriter(w *bytes.Buffer, mut ...func(*StreamConfig)) *StreamWriter {
	cfg := StreamConfig{ID: testID, Created: testCreated, Model: testModel}
	for _, f := range mut {
		f(&cfg)
	}
	return NewStreamWriter(w, cfg)
}

// TestMinimalChunkGolden is the COMPATIBILITY 2.1 and 2.2 test: a plain text
// chunk is exactly {id, object, created, model, choices:[{index, delta,
// finish_reason?}]} and NOTHING more. In particular there is no
// "logprobs":null and no "system_fingerprint":null, which is what a Go struct
// without pointers-plus-omitempty would emit while still passing a
// decode-your-own-output test.
func TestMinimalChunkGolden(t *testing.T) {
	c := &Chunk{
		ID:      testID,
		Object:  ObjectChunk,
		Created: testCreated,
		Model:   testModel,
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: ptr("Hi")}}},
	}
	got, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":[{"index":0,"delta":{"content":"Hi"}}]}`
	if string(got) != want {
		t.Fatalf("chunk bytes\n got: %s\nwant: %s", got, want)
	}
	for _, forbidden := range []string{"null", "logprobs", "system_fingerprint", "usage", "service_tier"} {
		if bytes.Contains(got, []byte(forbidden)) {
			t.Errorf("chunk contains %q, which COMPATIBILITY 2.1/2.2 forbids: %s", forbidden, got)
		}
	}
}

// TestRoleChunkGolden covers delta:{role} — the opening frame.
func TestRoleChunkGolden(t *testing.T) {
	c := RoleChunk("assistant")
	c.ID, c.Created, c.Model = testID, testCreated, testModel
	got, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":[{"index":0,"delta":{"role":"assistant"}}]}`
	if string(got) != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}

// TestEmptyDeltaMarshalsToObject guards the usage chunk's delta shape.
func TestEmptyDeltaMarshalsToObject(t *testing.T) {
	got, err := Marshal(Delta{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{}" {
		t.Fatalf("empty delta = %s, want {}", got)
	}
}

// TestToolCallDeltaSequenceGolden covers COMPATIBILITY 5.1: index is required
// and non-optional on every fragment, including index 0 and including the
// fragments that carry only an argument slice.
func TestToolCallDeltaSequenceGolden(t *testing.T) {
	var buf bytes.Buffer
	s := newTestWriter(&buf)

	events := []canonical.StreamEvent{
		{Type: canonical.EventDelta, Delta: canonical.Delta{Role: canonical.RoleAssistant}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
			{Index: 0, ID: "call_abc", Name: "get_weather", Arguments: ""},
		}}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
			{Index: 0, Arguments: `{"city":`},
		}}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
			{Index: 0, Arguments: `"Seoul"}`},
		}}},
		{Type: canonical.EventStop, Delta: canonical.Delta{StopReason: canonical.StopToolUse}},
	}
	for _, ev := range events {
		if err := s.WriteEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		`data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		"",
		`data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather"}}]}}]}`,
		"",
		`data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		"",
		`data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Seoul\"}"}}]}}]}`,
		"",
		`data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
		"",
	}, "\n")
	if buf.String() != want {
		t.Fatalf("stream bytes\n got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

// TestUsageChunkGolden covers COMPATIBILITY 3.1 and 3.3.
func TestUsageChunkGolden(t *testing.T) {
	usage := canonical.Usage{InputTokens: 12, OutputTokens: 5}

	t.Run("include_usage true, reference-proxy shape", func(t *testing.T) {
		var buf bytes.Buffer
		s := newTestWriter(&buf, func(c *StreamConfig) { c.IncludeUsage = true })
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
			Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("x")}}})
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventStop,
			Delta: canonical.Delta{StopReason: canonical.StopEndTurn}})
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventUsage, Usage: &usage})
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		want := `data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}` // pragma: allowlist secret — test fixture
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("usage chunk missing or wrong\n got:\n%s\nwant frame:\n%s", buf.String(), want)
		}
	})

	t.Run("compat usage_chunk_choices=empty is strict OpenAI", func(t *testing.T) {
		var buf bytes.Buffer
		s := newTestWriter(&buf, func(c *StreamConfig) {
			c.IncludeUsage = true
			c.UsageChunkChoices = UsageChunkChoicesEmpty
		})
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
			Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("x")}}})
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventStop,
			Delta: canonical.Delta{StopReason: canonical.StopEndTurn}})
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventUsage, Usage: &usage})
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		want := `data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}` // pragma: allowlist secret — test fixture
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("strict usage chunk wrong\n got:\n%s\nwant frame:\n%s", buf.String(), want)
		}
		if strings.Contains(buf.String(), `"choices":null`) {
			t.Fatal(`"choices":null on the wire — the empty slice must marshal as []`)
		}
	})

	t.Run("no stream_options means no usage on the wire", func(t *testing.T) {
		// COMPATIBILITY 3.2: usage is computed but not put on the wire.
		var buf bytes.Buffer
		s := newTestWriter(&buf)
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
			Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("x")}}})
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventStop,
			Delta: canonical.Delta{StopReason: canonical.StopEndTurn}})
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventUsage, Usage: &usage})
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "usage") {
			t.Fatalf("usage reached the wire without stream_options:\n%s", buf.String())
		}
		if u, ok := s.Usage(); !ok || u.InputTokens != 12 {
			t.Fatalf("usage was not computed: %+v ok=%v", u, ok)
		}
	})
}

// TestLateUsageAccepted covers COMPATIBILITY 3.4: usage arriving AFTER
// finish_reason must be accepted, not dropped.
func TestLateUsageAccepted(t *testing.T) {
	var warned []Warning
	var buf bytes.Buffer
	s := newTestWriter(&buf, func(c *StreamConfig) {
		c.IncludeUsage = true
		c.Warn = func(w Warning) { warned = append(warned, w) }
	})
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
		Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("x")}}})
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventStop,
		Delta: canonical.Delta{StopReason: canonical.StopEndTurn}})
	u := canonical.Usage{InputTokens: 7, OutputTokens: 3, CacheReadTokens: 4, ReasoningTokens: 2}
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventUsage, Usage: &u})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	want := `"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2}}` // pragma: allowlist secret — test fixture
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("late usage dropped or wrong\n got:\n%s\nwant substring:\n%s", buf.String(), want)
	}
	if !hasWarning(warned, WarnLateUsage) {
		t.Errorf("late usage was accepted but not reported: %+v", warned)
	}
}

// TestSynthesizedTerminalChunk is COMPATIBILITY 4.4 — described there as the
// highest-value single line in the document.
func TestSynthesizedTerminalChunk(t *testing.T) {
	t.Run("plain turn defaults to stop", func(t *testing.T) {
		var buf bytes.Buffer
		var warned []Warning
		s := newTestWriter(&buf, func(c *StreamConfig) {
			c.Warn = func(w Warning) { warned = append(warned, w) }
		})
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
			Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("hello")}}})
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		want := `data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + DoneFrame
		if !strings.HasSuffix(buf.String(), want) {
			t.Fatalf("terminal chunk\n got:\n%s\nwant suffix:\n%s", buf.String(), want)
		}
		if !hasWarning(warned, WarnSynthesizedTerminal) {
			t.Errorf("synthesis was not reported: %+v", warned)
		}
	})

	t.Run("tool-call turn is upgraded to tool_calls", func(t *testing.T) {
		// A gateway that merely forwards emits "stop" here and breaks every
		// agentic client: the client sees a final text turn and never runs the
		// tool.
		var buf bytes.Buffer
		s := newTestWriter(&buf)
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
			Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
				{Index: 0, ID: "call_1", Name: "search", Arguments: "{}"},
			}}})
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		want := `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("terminal chunk was not upgraded to tool_calls\n got:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), `"finish_reason":"stop"`) {
			t.Fatal(`emitted finish_reason "stop" on a tool-call turn`)
		}
	})

	t.Run("no synthesis when the backend already finished", func(t *testing.T) {
		var buf bytes.Buffer
		var warned []Warning
		s := newTestWriter(&buf, func(c *StreamConfig) {
			c.Warn = func(w Warning) { warned = append(warned, w) }
		})
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
			Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("hi")}}})
		mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventStop,
			Delta: canonical.Delta{StopReason: canonical.StopEndTurn}})
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(buf.String(), "finish_reason"); n != 1 {
			t.Fatalf("finish_reason appears %d times, want 1:\n%s", n, buf.String())
		}
		if hasWarning(warned, WarnSynthesizedTerminal) {
			t.Errorf("synthesized a terminal chunk the backend already sent")
		}
	})
}

// TestEmptyStreamHasNoDone covers COMPATIBILITY 1.4.
func TestEmptyStreamHasNoDone(t *testing.T) {
	var buf bytes.Buffer
	s := newTestWriter(&buf)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("empty upstream stream produced %q, want an empty body", buf.String())
	}
}

// TestFullStreamGolden is the first-frame-to-[DONE] byte contract.
func TestFullStreamGolden(t *testing.T) {
	var buf bytes.Buffer
	s := newTestWriter(&buf, func(c *StreamConfig) { c.IncludeUsage = true })

	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
		Delta: canonical.Delta{Role: canonical.RoleAssistant}})
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
		Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("Hello")}}})
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
		Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock(" world")}}})
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventStop,
		Delta: canonical.Delta{StopReason: canonical.StopEndTurn}})
	u := canonical.Usage{InputTokens: 9, OutputTokens: 2}
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventUsage, Usage: &u})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	const p = `data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1753660800,"model":"qwen3.5:397b","choices":`
	want := p + `[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
		p + `[{"index":0,"delta":{"content":"Hello"}}]}` + "\n\n" +
		p + `[{"index":0,"delta":{"content":" world"}}]}` + "\n\n" +
		p + `[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		p + `[{"index":0,"delta":{}}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}` + "\n\n" +
		"data: [DONE]\n\n"
	if buf.String() != want {
		t.Fatalf("full stream\n got:\n%q\nwant:\n%q", buf.String(), want)
	}
}

// TestStreamIdentityPinned covers COMPATIBILITY 2.4 and 2.5: id and created are
// pinned across every chunk, model is restamped on every chunk. A backend that
// sends its own values in each frame must not be able to leak them.
func TestStreamIdentityPinned(t *testing.T) {
	var buf bytes.Buffer
	s := newTestWriter(&buf)
	for i, upstream := range opaqueModels {
		c := &Chunk{
			ID:      "upstream-id-" + string(rune('a'+i)),
			Object:  "something.else",
			Created: 1,
			Model:   upstream,
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: ptr("x")}}},
		}
		if err := s.WriteChunk(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	frames := strings.Split(strings.TrimSuffix(body, DoneFrame), "\n\n")
	n := 0
	for _, f := range frames {
		if f == "" {
			continue
		}
		n++
		if !strings.Contains(f, `"id":"`+testID+`"`) {
			t.Errorf("frame lost the pinned id: %s", f)
		}
		if !strings.Contains(f, `"created":1753660800`) {
			t.Errorf("frame lost the pinned created: %s", f)
		}
		if !strings.Contains(f, `"model":"`+testModel+`"`) {
			t.Errorf("frame was not restamped with the client-facing model: %s", f)
		}
		if !strings.Contains(f, `"object":"chat.completion.chunk"`) {
			t.Errorf("object is not chat.completion.chunk: %s", f)
		}
		for _, m := range opaqueModels[:len(opaqueModels)-1] {
			if strings.Contains(f, m) && m != testModel {
				t.Errorf("upstream model %q leaked into the frame: %s", m, f)
			}
		}
	}
	if n != len(opaqueModels)+1 { // + the synthesized terminal chunk
		t.Fatalf("got %d frames, want %d", n, len(opaqueModels)+1)
	}
}

// TestErrorEnvelopeGolden covers COMPATIBILITY 7.1.
func TestErrorEnvelopeGolden(t *testing.T) {
	e := NewError(400, "", "model not found").WithParam("model")
	got, err := EncodeError(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"error":{"message":"model not found","type":"invalid_request_error","param":"model","code":"400"}}`
	if string(got) != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}

	// param is null, not absent, when there is no offending field.
	got, err = EncodeError(NewError(429, "", "no healthy deployment"))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"error":{"message":"no healthy deployment","type":"rate_limit_error","param":null,"code":"429"}}`
	if string(got) != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}

// TestErrorCodeIsAlwaysAString covers the "code is a string, not a number" half
// of 7.1, in the direction that actually happens: a backend sends a number.
func TestErrorCodeIsAlwaysAString(t *testing.T) {
	var warned []Warning
	e, err := DecodeError([]byte(`{"error":{"message":"slow down","type":"rate_limit_error","param":null,"code":429}}`),
		func(w Warning) { warned = append(warned, w) })
	if err != nil {
		t.Fatal(err)
	}
	if e.Code != "429" {
		t.Fatalf("code = %q, want the string \"429\"", e.Code)
	}
	if !hasWarning(warned, WarnNumericErrorCode) {
		t.Errorf("numeric code was normalized silently: %+v", warned)
	}
	out, err := EncodeError(e)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"code":"429"`)) {
		t.Fatalf("re-encoded with a non-string code: %s", out)
	}
}

// TestMidStreamErrorIsInBand covers COMPATIBILITY 1.3.
func TestMidStreamErrorIsInBand(t *testing.T) {
	var buf bytes.Buffer
	s := newTestWriter(&buf)
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
		Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("partial")}}})
	if err := s.WriteError(NewError(500, "", "upstream exploded")); err != nil {
		t.Fatal(err)
	}
	want := `data: {"error":{"message":"upstream exploded","type":"api_error","param":null,"code":"500"}}` + "\n\n" + DoneFrame
	if !strings.HasSuffix(buf.String(), want) {
		t.Fatalf("mid-stream error\n got:\n%s\nwant suffix:\n%s", buf.String(), want)
	}
	// Close after an in-band error must not append a second [DONE].
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "[DONE]"); n != 1 {
		t.Fatalf("[DONE] appears %d times, want 1", n)
	}
}

// TestSSEFramingHasNoEventOrIDLines covers COMPATIBILITY 1.1.
func TestSSEFramingHasNoEventOrIDLines(t *testing.T) {
	var buf bytes.Buffer
	s := newTestWriter(&buf)
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
		Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("x")}}})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("non-data line on a chat-completions stream: %q", line)
		}
	}
}

// TestNonStreamingResponseGolden pins the non-streaming shape, including the
// preserved native finish reason of COMPATIBILITY 4.3.
func TestNonStreamingResponseGolden(t *testing.T) {
	r := &canonical.Response{
		ID:      testID,
		Model:   testModel,
		Created: testCreated,
		Choices: []canonical.Choice{{
			Index:            0,
			Message:          canonical.TextMessage(canonical.RoleAssistant, "done"),
			StopReason:       canonical.StopStopSequence,
			NativeStopReason: "stop_sequence",
		}},
		Usage: &canonical.Usage{InputTokens: 4, OutputTokens: 1},
	}
	got, err := MarshalResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"chatcmpl-0123456789abcdef","object":"chat.completion","created":1753660800,"model":"qwen3.5:397b",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop",` +
		`"provider_specific_fields":{"native_finish_reason":"stop_sequence"}}],` +
		`"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`
	if string(got) != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}

// TestNoHTMLEscaping guards the one Go-specific serialization divergence.
func TestNoHTMLEscaping(t *testing.T) {
	got, err := Marshal(TextChunk("a && b <tag>"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"content":"a && b <tag>"`)) {
		t.Fatalf("HTML-escaped output diverges from every other server: %s", got)
	}
}

// TestNoHTMLEscapingOnTheRequestPath is the same rule where it was NOT held.
//
// [TestNoHTMLEscaping] above passes on a chunk because Delta.Content is a plain
// struct field, encoded by this package's escaping-off encoder. Message content
// is not a plain field — it is [Content], a string-or-array type with its own
// MarshalJSON — and that method called encoding/json.Marshal, whose escaping is
// ON by default. So the single most caller-controlled string in the whole
// protocol went upstream as "a && b" while every field around it went
// as itself, and no test looked: the escaping tests all sat on the response
// side, on fields that never took that path. [StopSequences] and Anthropic's
// [anthropic.BlockList] had it for the same reason.
//
// It is a divergence a client sees. A caller that sends "<think>" and reads the
// upstream's echo of its own prompt back gets bytes it did not send, and a
// substring match on the raw frame — which COMPATIBILITY 2.1a exists for — fails
// against every other OpenAI-compatible server.
func TestNoHTMLEscapingOnTheRequestPath(t *testing.T) {
	const text = "a && b <tag>"
	req := &canonical.Request{
		Model: "m",
		Messages: []canonical.Message{
			{Role: canonical.RoleUser, Content: canonical.Content{canonical.TextBlock(text)}},
			{Role: canonical.RoleUser, Content: canonical.Content{
				canonical.TextBlock(text),
				{Kind: canonical.KindImage, Source: &canonical.Source{
					Kind: canonical.SourceURL, Data: "https://example.test/a?x=1&y=2"}},
			}},
		},
		Stop: []string{text},
	}
	got, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"content":"a && b <tag>"`,               // the string form
		`"text":"a && b <tag>"`,                  // the array form
		`"stop":"a && b <tag>"`,                  // the string-or-array stop field
		`"url":"https://example.test/a?x=1&y=2"`, // a plain field, unchanged
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if bytes.Contains(got, []byte(`\u00`)) || bytes.Contains(got, []byte(`\u003`)) {
		t.Errorf("HTML escaping is on somewhere in:\n%s", got)
	}
}

func mustWrite(t *testing.T, s *StreamWriter, ev canonical.StreamEvent) {
	t.Helper()
	if err := s.WriteEvent(ev); err != nil {
		t.Fatal(err)
	}
}

func hasWarning(ws []Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}
