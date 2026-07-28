package openai

import (
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TestAnthropicShapedToolResultsProduceNoEmptyMessage covers the crossing that
// actually happens: the Anthropic form puts tool results inside a USER message,
// and a naive splitter leaves a trailing {"role":"user"} behind it. Several
// upstreams reject a user message with no content, so the failure is a 400 on
// the second turn of a tool-use exchange.
func TestAnthropicShapedToolResultsProduceNoEmptyMessage(t *testing.T) {
	req := &canonical.Request{
		Model: "zai:glm-5.1",
		Messages: []canonical.Message{{
			Role: canonical.RoleUser,
			Content: canonical.Content{
				canonical.ToolResultBlock("c1", canonical.TextBlock("42")),
				canonical.ToolResultBlock("c2", canonical.TextBlock("7")),
			},
		}},
	}
	w, err := EncodeRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Messages) != 2 {
		t.Fatalf("got %d messages, want exactly the two tool messages: %+v", len(w.Messages), w.Messages)
	}
	for i, m := range w.Messages {
		if m.Role != "tool" {
			t.Errorf("message %d has role %q", i, m.Role)
		}
		if m.ToolCallID == "" {
			t.Errorf("message %d has no tool_call_id", i)
		}
	}

	// Text before the results still produces its own message, in order.
	req.Messages[0].Content = append(canonical.Content{canonical.TextBlock("here you go")},
		req.Messages[0].Content...)
	w, err = EncodeRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Messages) != 3 || w.Messages[0].Role != "user" || w.Messages[0].Content.String() != "here you go" {
		t.Fatalf("ordering was not preserved: %+v", w.Messages)
	}
}

// TestNoMessageIsEmitedWithoutAContentField guards the stricter upstreams.
func TestNoMessageIsEmitedWithoutAContentField(t *testing.T) {
	req := &canonical.Request{
		Model: "m",
		Messages: []canonical.Message{
			{Role: canonical.RoleUser},
			{Role: canonical.RoleAssistant, Content: canonical.Content{
				canonical.ToolUseBlock("c1", "f", nil),
			}},
		},
	}
	b, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `{"role":"user"}`) {
		t.Fatalf("a message with no content field reached the wire: %s", b)
	}
	// An assistant message that carries tool calls legitimately has no content.
	if !strings.Contains(string(b), `"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]`) {
		t.Fatalf("tool call shape wrong: %s", b)
	}
}

// TestChunkChoicesIsNeverNull guards the one field whose nil slice is visible.
func TestChunkChoicesIsNeverNull(t *testing.T) {
	var buf strings.Builder
	s := newTestWriterS(&buf)
	if err := s.WriteChunk(&Chunk{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"choices":null`) {
		t.Fatalf(`"choices":null reached the wire: %s`, buf.String())
	}
	if !strings.Contains(buf.String(), `"choices":[]`) {
		t.Fatalf("expected an empty array: %s", buf.String())
	}
}

// TestMaxTokensSpelling covers the field name that COMPATIBILITY.md does not
// pin and that is not interchangeable between deployment classes.
func TestMaxTokensSpelling(t *testing.T) {
	req := &canonical.Request{
		Model:     "gemma4:31b",
		Messages:  []canonical.Message{canonical.TextMessage(canonical.RoleUser, "x")},
		MaxTokens: ptr(256),
	}
	b, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"max_tokens":256`) {
		t.Fatalf("default spelling is not max_tokens: %s", b)
	}
	if strings.Contains(string(b), "max_completion_tokens") {
		t.Fatalf("both spellings were emitted: %s", b)
	}

	b, err = MarshalRequest(req, &EncodeOptions{MaxTokensField: FieldMaxCompletionTokens})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"max_completion_tokens":256`) {
		t.Fatalf("override ignored: %s", b)
	}

	// Both spellings are accepted inbound; the newer one wins.
	got, err := DecodeRequest([]byte(`{"model":"m","messages":[],"max_tokens":1,"max_completion_tokens":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 2 {
		t.Fatalf("MaxTokens = %v, want 2", got.MaxTokens)
	}
}

// TestUpstreamModelOverride covers DESIGN §7.2's request half: the upstream
// always receives the REAL model id.
func TestUpstreamModelOverride(t *testing.T) {
	req := &canonical.Request{
		Model:    "my-alias",
		Messages: []canonical.Message{canonical.TextMessage(canonical.RoleUser, "x")},
	}
	b, err := MarshalRequest(req, &EncodeOptions{Model: "deepseek-v4-flash:cloud"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"model":"deepseek-v4-flash:cloud"`) {
		t.Fatalf("upstream did not receive the real id: %s", b)
	}
	if strings.Contains(string(b), "my-alias") {
		t.Fatalf("the alias leaked upstream: %s", b)
	}
}

// TestStopSequencesStringOrArray covers the wire form both ways.
func TestStopSequencesStringOrArray(t *testing.T) {
	one, err := DecodeRequest([]byte(`{"model":"m","messages":[],"stop":"END"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Stop) != 1 || one.Stop[0] != "END" {
		t.Fatalf("Stop = %v", one.Stop)
	}
	many, err := DecodeRequest([]byte(`{"model":"m","messages":[],"stop":["A","B"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(many.Stop) != 2 {
		t.Fatalf("Stop = %v", many.Stop)
	}
	b, err := MarshalRequest(many, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"stop":["A","B"]`) {
		t.Fatalf("stop array not re-emitted: %s", b)
	}
}

// TestToolChoiceForms covers the string and object forms.
func TestToolChoiceForms(t *testing.T) {
	for _, c := range []struct {
		body string
		mode canonical.ToolChoiceMode
		name string
	}{
		{`{"model":"m","messages":[],"tool_choice":"auto"}`, canonical.ToolChoiceAuto, ""},
		{`{"model":"m","messages":[],"tool_choice":"required"}`, canonical.ToolChoiceRequired, ""},
		{`{"model":"m","messages":[],"tool_choice":"none"}`, canonical.ToolChoiceNone, ""},
		{`{"model":"m","messages":[],"tool_choice":{"type":"function","function":{"name":"f"}}}`, canonical.ToolChoiceTool, "f"},
	} {
		req, err := DecodeRequest([]byte(c.body))
		if err != nil {
			t.Fatal(err)
		}
		if req.ToolChoice == nil || req.ToolChoice.Mode != c.mode || req.ToolChoice.Name != c.name {
			t.Fatalf("%s -> %+v", c.body, req.ToolChoice)
		}
		b, err := MarshalRequest(req, nil)
		if err != nil {
			t.Fatal(err)
		}
		back, err := DecodeRequest(b)
		if err != nil {
			t.Fatal(err)
		}
		if back.ToolChoice.Mode != c.mode || back.ToolChoice.Name != c.name {
			t.Fatalf("%s did not round-trip: %+v", c.body, back.ToolChoice)
		}
	}
}

// TestThinkingSignatureIsAlwaysReported covers DESIGN §10.2: no OpenAI field
// carries integrity material, and the loss lands on the SECOND turn of an
// agentic flow, so it must be reported on the first.
func TestThinkingSignatureIsAlwaysReported(t *testing.T) {
	req := &canonical.Request{
		Model: "m",
		Messages: []canonical.Message{{
			Role: canonical.RoleAssistant,
			Content: canonical.Content{
				canonical.ThinkingBlock("reasoning", "sig-abc"),
				canonical.TextBlock("answer"),
			},
		}},
	}
	var loss canonical.LossReport
	// Even with the FULL default capability set, which carries reasoning text.
	if _, err := EncodeRequest(req, &EncodeOptions{Loss: &loss}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range loss.Downgrades {
		if d.Construct == canonical.ConstructThinkingBlock && strings.Contains(d.Detail, "signature") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a reasoning signature was dropped without a report: %+v", loss.Downgrades)
	}
}

func newTestWriterS(w *strings.Builder) *StreamWriter {
	return NewStreamWriter(w, StreamConfig{ID: testID, Created: testCreated, Model: testModel})
}
