package bedrock

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

func intPtr(n int) *int { return &n }

// The neutral request crosses into Converse with what Converse can carry, and
// reports what it cannot.
func TestARequestCrossesIntoConverse(t *testing.T) {
	yes := true
	req := &canonical.Request{
		Model:  "m",
		System: canonical.Content{{Kind: canonical.KindText, Text: "be brief", CacheControl: &canonical.CacheControl{Type: "ephemeral"}}},
		Messages: []canonical.Message{
			canonical.TextMessage(canonical.RoleUser, "hi"),
			{Role: canonical.RoleAssistant, Content: canonical.Content{
				canonical.ThinkingBlock("weighing", "sig1"),
				canonical.ToolUseBlock("call_1", "lookup", json.RawMessage(`{"q":"seoul"}`)),
			}},
			{Role: canonical.RoleTool, Content: canonical.Content{canonical.ToolResultBlock("call_1", canonical.TextBlock("sunny"))}},
			canonical.TextMessage(canonical.RoleUser, "thanks"),
		},
		Tools:          []canonical.Tool{{Name: "lookup", Description: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:     &canonical.ToolChoice{Mode: canonical.ToolChoiceRequired},
		MaxTokens:      intPtr(64),
		Stop:           []string{"END"},
		Reasoning:      &canonical.Reasoning{Enabled: &yes, BudgetTokens: 2048},
		Metadata:       map[string]string{"trace": "t1"},
		Seed:           func() *int64 { v := int64(7); return &v }(),
		TopK:           intPtr(40),
		ResponseFormat: &canonical.ResponseFormat{Kind: canonical.FormatJSONSchema, Schema: json.RawMessage(`{"type":"object"}`)},
	}
	loss := &canonical.LossReport{}
	out, err := EncodeRequest(req, &EncodeOptions{Loss: loss})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(out.System) != 2 || out.System[0].Text != "be brief" || out.System[1].CachePoint == nil {
		t.Errorf("system = %+v, want the text and a cache point after it", out.System)
	}
	// user, assistant, user(tool result + "thanks" merged: consecutive same-role turns)
	if len(out.Messages) != 3 {
		t.Fatalf("%d messages, want 3 (the tool result and the next user turn merge): %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[1].Role != "assistant" || out.Messages[1].Content[0].ReasoningContent == nil ||
		out.Messages[1].Content[0].ReasoningContent.ReasoningText.Signature != "sig1" || out.Messages[1].Content[1].ToolUse == nil {
		t.Errorf("assistant turn = %+v", out.Messages[1].Content)
	}
	if tr := out.Messages[2].Content[0].ToolResult; tr == nil || tr.ToolUseID != "call_1" || tr.Content[0].Text != "sunny" {
		t.Errorf("tool result = %+v", out.Messages[2].Content[0])
	}
	if out.Messages[2].Content[1].Text != "thanks" {
		t.Errorf("merged user text = %+v", out.Messages[2].Content)
	}
	if out.InferenceConfig == nil || *out.InferenceConfig.MaxTokens != 64 || out.InferenceConfig.StopSequences[0] != "END" {
		t.Errorf("inference config = %+v", out.InferenceConfig)
	}
	if out.ToolConfig == nil || out.ToolConfig.Tools[0].ToolSpec.Name != "lookup" || out.ToolConfig.ToolChoice.Any == nil {
		t.Errorf("tool config = %+v", out.ToolConfig)
	}
	if th, _ := out.AdditionalModelRequestFields["thinking"].(map[string]any); th == nil || th["budget_tokens"] != 2048 {
		t.Errorf("thinking = %v", out.AdditionalModelRequestFields)
	}
	if out.RequestMetadata["trace"] != "t1" {
		t.Errorf("metadata = %v", out.RequestMetadata)
	}
	dropped := strings.Join(loss.Dropped, ",")
	for _, want := range []string{"seed", "top_k"} {
		if !strings.Contains(dropped, want) {
			t.Errorf("%s was not reported dropped: %v", want, loss.Dropped)
		}
	}
	var rf bool
	for _, d := range loss.Downgrades {
		if strings.HasPrefix(d.Construct, "response_format") {
			rf = true
		}
	}
	if !rf {
		t.Errorf("response_format was not reported: %+v", loss.Downgrades)
	}
}

// A Converse answer decodes with its blocks, its stop reason and its usage in
// the neutral convention.
func TestAConverseAnswerDecodes(t *testing.T) {
	body := `{"output":{"message":{"role":"assistant","content":[
	  {"reasoningContent":{"reasoningText":{"text":"thinking","signature":"s"}}},
	  {"text":"yes"},
	  {"toolUse":{"toolUseId":"t1","name":"lookup","input":{"q":"seoul"}}}
	]}},"stopReason":"tool_use","usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15,"cacheReadInputTokens":30,"cacheWriteInputTokens":2}}`
	if !IsResponse([]byte(body)) {
		t.Fatal("a Converse answer was not recognised")
	}
	resp, err := DecodeResponse([]byte(body), &DecodeOptions{Model: "client-model"})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	c := resp.Choices[0]
	if len(c.Message.Content) != 3 || c.Message.Content[0].Kind != canonical.KindThinking ||
		c.Message.Content[1].Text != "yes" || c.Message.Content[2].ToolUse == nil || c.Message.Content[2].ToolUse.Name != "lookup" {
		t.Errorf("content = %+v", c.Message.Content)
	}
	if c.StopReason != canonical.StopToolUse {
		t.Errorf("stop = %q", c.StopReason)
	}
	// §10.7: input INCLUDES what was read from and written to cache.
	if resp.Usage == nil || resp.Usage.InputTokens != 42 || resp.Usage.CacheReadTokens != 30 || resp.Usage.CacheWriteTokens != 2 || resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Model != "client-model" {
		t.Errorf("model = %q", resp.Model)
	}
	if _, err := DecodeResponse([]byte(`{"error":"nope"}`), nil); err == nil {
		t.Error("a non-answer decoded")
	}
}

// The binary framing round-trips, and a corrupt frame is refused by name.
func TestEventStreamFramesRoundTripAndCheck(t *testing.T) {
	msg := EncodeEventMessage(map[string]string{":message-type": "event", ":event-type": "messageStart", ":content-type": "application/json"}, []byte(`{"role":"assistant"}`))
	r := NewEventStreamReader(bytes.NewReader(msg))
	m, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if m.EventType() != "messageStart" || string(m.Payload) != `{"role":"assistant"}` {
		t.Errorf("message = %+v", m)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("after the last message: %v, want EOF", err)
	}
	bad := append([]byte(nil), msg...)
	bad[len(bad)-1] ^= 0xff
	if _, err := NewEventStreamReader(bytes.NewReader(bad)).Next(); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("a corrupt frame was accepted: %v", err)
	}
	if _, err := NewEventStreamReader(bytes.NewReader(msg[:7])).Next(); err == nil {
		t.Error("a truncated prelude was accepted")
	}
}

func frames(events ...[2]string) []byte {
	var out []byte
	for _, e := range events {
		out = append(out, EncodeEventMessage(map[string]string{":message-type": "event", ":event-type": e[0], ":content-type": "application/json"}, []byte(e[1]))...)
	}
	return out
}

// A converse-stream becomes the neutral event sequence: a tool call joined by
// its index, reasoning as thinking, the stop, and the usage that arrives after
// the stop.
func TestAConverseStreamDecodes(t *testing.T) {
	body := frames(
		[2]string{"messageStart", `{"role":"assistant"}`},
		[2]string{"contentBlockStart", `{"contentBlockIndex":0,"start":{}}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"hmm"}}}`},
		[2]string{"contentBlockStop", `{"contentBlockIndex":0}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"text":"ye"}}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"text":"s"}}`},
		[2]string{"contentBlockStart", `{"contentBlockIndex":2,"start":{"toolUse":{"toolUseId":"t1","name":"lookup"}}}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":"{\"q\":"}}}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":"\"seoul\"}"}}}`},
		[2]string{"messageStop", `{"stopReason":"tool_use"}`},
		[2]string{"metadata", `{"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15},"metrics":{"latencyMs":100}}`},
	)
	d := NewStreamDecoder(bytes.NewReader(body), &DecodeOptions{Model: "client-model"})
	var text, thinking, args, name string
	var stop canonical.StopReason
	var usage *canonical.Usage
	var starts int
	for {
		evs, err := d.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		for _, e := range evs {
			switch e.Type {
			case canonical.EventStart:
				starts++
			case canonical.EventStop:
				stop = e.Delta.StopReason
			case canonical.EventUsage:
				usage = e.Usage
			}
			if e.Model != "" {
				t.Errorf("an event carries a model name %q the stream never said", e.Model)
			}
			for _, b := range e.Delta.Content {
				if b.Kind == canonical.KindThinking {
					thinking += b.Text
				} else {
					text += b.Text
				}
			}
			for _, tc := range e.Delta.ToolCalls {
				if tc.Index != 2 {
					t.Errorf("tool fragment on index %d", tc.Index)
				}
				name += tc.Name
				args += tc.Arguments
			}
		}
	}
	if starts != 1 || text != "yes" || thinking != "hmm" || name != "lookup" || args != `{"q":"seoul"}` {
		t.Errorf("starts=%d text=%q thinking=%q name=%q args=%q", starts, text, thinking, name, args)
	}
	if stop != canonical.StopToolUse {
		t.Errorf("stop = %q", stop)
	}
	if usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Errorf("usage after the stop was lost: %+v", usage)
	}
	if !d.Terminated() {
		t.Error("the decoder did not see the stop")
	}

	// An exception frame is an error the caller sees, with the class Bedrock named.
	ex := EncodeEventMessage(map[string]string{":message-type": "exception", ":exception-type": "throttlingException", ":content-type": "application/json"}, []byte(`{"message":"Too many requests"}`))
	d2 := NewStreamDecoder(bytes.NewReader(append(frames([2]string{"messageStart", `{"role":"assistant"}`}), ex...)), nil)
	var gotErr *canonical.Error
	for {
		evs, err := d2.Next()
		if err != nil {
			break
		}
		for _, e := range evs {
			if e.Type == canonical.EventError {
				gotErr = e.Err
			}
		}
	}
	if gotErr == nil || gotErr.Message != "Too many requests" || gotErr.Type != "throttlingException" {
		t.Errorf("exception = %+v", gotErr)
	}
}

// A stream that stops without its metadata event is not a clean end.
//
// metadata is Converse's true terminator and always carries the usage; a
// stream cut off between messageStop and metadata would otherwise be billed
// as a zero-token generation. Terminated() reports it as not terminated so
// the relay marks it truncated.
func TestAStreamWithoutMetadataIsNotTerminated(t *testing.T) {
	body := frames(
		[2]string{"messageStart", `{"role":"assistant"}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"yes"}}`},
		[2]string{"messageStop", `{"stopReason":"end_turn"}`},
	)
	d := NewStreamDecoder(bytes.NewReader(body), nil)
	var sawStop bool
	for {
		evs, err := d.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.Type == canonical.EventStop {
				sawStop = true
			}
		}
	}
	if !sawStop {
		t.Fatal("the stop event was not emitted")
	}
	if d.Terminated() {
		t.Error("a stream that never sent metadata reported a clean end; its generation would bill as free")
	}
	if _, ok := d.Usage(); ok {
		t.Error("usage was reported without a metadata event")
	}
}

// An exception after messageStop is still an error, not silence.
func TestAnExceptionAfterTheStopIsNotDiscarded(t *testing.T) {
	body := append(frames(
		[2]string{"messageStart", `{"role":"assistant"}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"par"}}`},
		[2]string{"messageStop", `{"stopReason":"end_turn"}`},
	), EncodeEventMessage(map[string]string{":message-type": "exception", ":exception-type": "modelStreamErrorException", ":content-type": "application/json"}, []byte(`{"message":"the model faulted"}`))...)
	d := NewStreamDecoder(bytes.NewReader(body), nil)
	var gotErr *canonical.Error
	for {
		evs, err := d.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.Type == canonical.EventError {
				gotErr = e.Err
			}
		}
	}
	if gotErr == nil || gotErr.Message != "the model faulted" {
		t.Errorf("the exception after the stop was discarded: %+v", gotErr)
	}
}

// An empty tool result serialises to a valid Converse union member, not `{}`.
func TestAnEmptyToolResultIsAValidUnionMember(t *testing.T) {
	req := &canonical.Request{
		Model: "m",
		Messages: []canonical.Message{
			{Role: canonical.RoleTool, Content: canonical.Content{canonical.ToolResultBlock("call_1")}},
			canonical.TextMessage(canonical.RoleUser, "go"),
		},
	}
	b, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"content":[{}]`) {
		t.Errorf("an empty tool result serialised to an invalid union member:\n%s", b)
	}
	// It carries a valid member: the empty json string.
	if !strings.Contains(string(b), `"json":""`) {
		t.Errorf("the empty result has no valid content member:\n%s", b)
	}
}
