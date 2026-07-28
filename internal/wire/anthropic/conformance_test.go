package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TestTwoTurnToolUseConformance is the case DESIGN §10.2 says a naive
// implementation fails — on the SECOND turn of an agentic flow, not the first,
// "which is the worst possible place to discover it".
//
// Turn 1: the backend streams a thinking block carrying integrity material, and
// a tool call. dorang decodes it and the client sees both.
// Turn 2: the client echoes the assistant turn back, signature included, along
// with the tool result. The signature must arrive at the backend byte-identical
// to the bytes that were minted, because nothing on this side can re-derive it.
func TestTwoTurnToolUseConformance(t *testing.T) {
	const signature = "ErUBCkYIAxgCIkDx7Q8sIGNvbnRlbnQtc2ln"

	// ---- turn 1: a backend stream, decoded -------------------------------
	turn1 := buildStream(t,
		frame(EventMessageStart, `{"type":"message_start","message":{"id":"msg_t1","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`),
		frame(EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		frame(EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weather first"}}`),
		frame(EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"`+signature+`"}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":0}`),
		frame(EventContentBlockStart, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`),
		frame(EventContentBlockDelta, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Seoul\"}"}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":1}`),
		frame(EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}`),
		frame(EventMessageStop, `{"type":"message_stop"}`),
	)
	events, err := DecodeStream(turn1, nil)
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	assistant := accumulate(events)

	if len(assistant.Content) != 2 {
		t.Fatalf("accumulated %d blocks, want thinking + tool_use: %+v", len(assistant.Content), assistant.Content)
	}
	th := assistant.Content[0]
	if th.Kind != canonical.KindThinking || th.Thinking == nil || th.Thinking.Signature != signature {
		t.Fatalf("turn 1 lost the signature: %+v", th)
	}
	tu := assistant.Content[1]
	if tu.Kind != canonical.KindToolUse || string(tu.ToolUse.Input) != `{"city":"Seoul"}` {
		t.Fatalf("turn 1 tool call: %+v", tu)
	}

	// ---- turn 2: the client echoes it back -------------------------------
	turn2 := &canonical.Request{
		Model: "claude-x", MaxTokens: ptr(1024),
		Messages: []canonical.Message{
			canonical.TextMessage(canonical.RoleUser, "weather in Seoul?"),
			assistant,
			{Role: canonical.RoleTool, Content: canonical.Content{
				canonical.ToolResultBlock("toolu_1", canonical.TextBlock("sunny, 21C")),
			}},
		},
	}
	body, err := MarshalRequest(turn2, nil)
	if err != nil {
		t.Fatalf("turn 2 MarshalRequest: %v", err)
	}
	if !bytes.Contains(body, []byte(`"signature":"`+signature+`"`)) {
		t.Fatalf("turn 2 did not replay the signature byte-identically:\n%s", body)
	}
	// The whole point of replay: the bytes are the bytes that arrived, not a
	// re-encoding of a parse of them.
	if n := bytes.Count(body, []byte(signature)); n != 1 {
		t.Errorf("signature appears %d times, want once", n)
	}
}

// TestTwoTurnEchoWithoutIntegrityMaterialIsAHardError is the same flow with the
// reasoning derived from another family — an OpenAI backend's reasoning_content
// has no signature and dorang will not mint one.
//
// Turn 1 must succeed and turn 2 must fail with a NAMED reason. Both halves
// matter: a package that rejected the block on turn 1 would be unusable, and one
// that dropped it in silence on turn 2 would produce a backend error naming a
// field the caller never wrote.
func TestTwoTurnEchoWithoutIntegrityMaterialIsAHardError(t *testing.T) {
	turn1 := &canonical.Request{
		Model: "claude-x", MaxTokens: ptr(1024),
		Messages: []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")},
	}
	if _, err := EncodeRequest(turn1, nil); err != nil {
		t.Fatalf("turn 1 must succeed: %v", err)
	}

	turn2 := &canonical.Request{
		Model: "claude-x", MaxTokens: ptr(1024),
		Messages: []canonical.Message{
			canonical.TextMessage(canonical.RoleUser, "hi"),
			{Role: canonical.RoleAssistant, Content: canonical.Content{
				// Derived from a plain-text reasoning field: no signature.
				canonical.ThinkingBlock("let me think", ""),
				canonical.ToolUseBlock("toolu_1", "f", json.RawMessage(`{}`)),
			}},
			{Role: canonical.RoleTool, Content: canonical.Content{
				canonical.ToolResultBlock("toolu_1", canonical.TextBlock("ok")),
			}},
		},
	}
	_, err := EncodeRequest(turn2, nil)
	var oe *OpaqueError
	if !asOpaque(err, &oe) {
		t.Fatalf("turn 2 error = %v, want an OpaqueError", err)
	}
	if oe.Reason != ReasonUnsignedThinking {
		t.Errorf("reason = %q, want %q", oe.Reason, ReasonUnsignedThinking)
	}
	if oe.Construct != canonical.ConstructThinkingBlock {
		t.Errorf("construct = %q; the caller needs it for x-dorang-allow-lossy", oe.Construct)
	}
	if !strings.Contains(oe.Detail, "messages[1]") {
		t.Errorf("detail = %q, want the offending block located", oe.Detail)
	}
	if oe.Status() != 400 {
		t.Errorf("status = %d, want 400", oe.Status())
	}

	// The caller may opt in explicitly (DESIGN §10.1's x-dorang-allow-lossy), in
	// which case the block is dropped AND reported — never dropped in silence.
	var loss canonical.LossReport
	w, err := EncodeRequest(turn2, &EncodeOptions{
		AllowLossy: canonical.CapThinkingBlocks,
		Loss:       &loss,
	})
	if err != nil {
		t.Fatalf("with the opt-in, EncodeRequest: %v", err)
	}
	if !loss.HasStructural() {
		t.Error("the opt-in must still report the downgrade")
	}
	if bytes.Contains(mustMarshal(t, w), []byte(`"thinking"`)) {
		t.Error("an unsigned thinking block was forwarded anyway")
	}
}

// TestDerivedReasoningIsMarkedOnTheWayOut is the other half of DESIGN §10.2.
// Reasoning CONTENT is normalized best-effort into the caller's shape and
// marked derived; reasoning BLOCKS with integrity material are replayed. Only
// the second can be echoed back, and the caller learns which one they have on
// turn one instead of turn two.
func TestDerivedReasoningIsMarkedOnTheWayOut(t *testing.T) {
	derived := &canonical.Response{
		ID: "msg_1", Model: "m",
		Choices: []canonical.Choice{{Message: canonical.Message{
			Role:    canonical.RoleAssistant,
			Content: canonical.Content{canonical.ThinkingBlock("from another family", "")},
		}, StopReason: canonical.StopEndTurn}},
	}
	enc, err := EncodeResponse(derived, nil)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !enc.ReasoningDerived {
		t.Error("unsigned reasoning must be reported as derived (x-dorang-reasoning)")
	}

	native := &canonical.Response{
		ID: "msg_1", Model: "m",
		Choices: []canonical.Choice{{Message: canonical.Message{
			Role:    canonical.RoleAssistant,
			Content: canonical.Content{canonical.ThinkingBlock("native", "sig")},
		}, StopReason: canonical.StopEndTurn}},
	}
	enc, err = EncodeResponse(native, nil)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if enc.ReasoningDerived {
		t.Error("a signed block is not derived")
	}
	if enc.Response.Content[0].Signature != "sig" {
		t.Errorf("signature lost on the way out: %+v", enc.Response.Content[0])
	}
}

// TestRedactedThinkingPayloadSurvives is EXTENSIONS §B18: canonical.Thinking
// models Redacted but has no field for the payload, so a round trip through the
// neutral form used to reconstruct the block WITHOUT its data.
func TestRedactedThinkingPayloadSurvives(t *testing.T) {
	const body = `{"model":"m","messages":[{"role":"assistant","content":[` +
		`{"type":"redacted_thinking","data":"EroBCkYIAxgCKkBopaque"}]}],"max_tokens":16}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blk := req.Messages[0].Content[0]
	if blk.Kind != canonical.KindThinking || blk.Thinking == nil || !blk.Thinking.Redacted {
		t.Fatalf("redacted block = %+v", blk)
	}
	out, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}
	if string(out) != body {
		t.Errorf("redacted payload was lost\ngot:  %s\nwant: %s", out, body)
	}

	// A redacted block whose payload was lost on an earlier hop cannot be
	// reconstructed: an empty one is a fabrication, not a smaller version.
	stripped := &canonical.Request{
		Model: "m", MaxTokens: ptr(16),
		Messages: []canonical.Message{{Role: canonical.RoleAssistant, Content: canonical.Content{
			{Kind: canonical.KindThinking, Thinking: &canonical.Thinking{Redacted: true}},
		}}},
	}
	_, err = EncodeRequest(stripped, nil)
	var oe *OpaqueError
	if !asOpaque(err, &oe) || oe.Reason != ReasonRedactedWithoutData {
		t.Fatalf("err = %v, want %s", err, ReasonRedactedWithoutData)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func frame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

func buildStream(t *testing.T, frames ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, f := range frames {
		b.WriteString(f)
	}
	return b.Bytes()
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

// accumulate folds neutral stream events back into one assistant message, the
// way a client (or dorang's own accumulator) would. It is deliberately naive:
// if the wire adapter is right, naive is enough.
func accumulate(events []canonical.StreamEvent) canonical.Message {
	msg := canonical.Message{Role: canonical.RoleAssistant}
	var text, thinking strings.Builder
	var signature string
	type call struct {
		id, name string
		args     strings.Builder
	}
	var calls []*call

	for i := range events {
		ev := &events[i]
		if ev.Type != canonical.EventDelta {
			continue
		}
		for j := range ev.Delta.Content {
			b := &ev.Delta.Content[j]
			switch b.Kind {
			case canonical.KindText:
				text.WriteString(b.Text)
			case canonical.KindThinking:
				thinking.WriteString(b.Text)
				if b.Thinking != nil && b.Thinking.Signature != "" {
					signature = b.Thinking.Signature
				}
			}
		}
		for j := range ev.Delta.ToolCalls {
			tc := &ev.Delta.ToolCalls[j]
			for len(calls) <= tc.Index {
				calls = append(calls, &call{})
			}
			c := calls[tc.Index]
			if tc.ID != "" {
				c.id = tc.ID
			}
			if tc.Name != "" {
				c.name = tc.Name
			}
			c.args.WriteString(tc.Arguments)
		}
	}
	if thinking.Len() > 0 || signature != "" {
		msg.Content = append(msg.Content, canonical.ThinkingBlock(thinking.String(), signature))
	}
	if text.Len() > 0 {
		msg.Content = append(msg.Content, canonical.TextBlock(text.String()))
	}
	for _, c := range calls {
		args := c.args.String()
		if args == "" {
			args = "{}"
		}
		msg.Content = append(msg.Content, canonical.ToolUseBlock(c.id, c.name, json.RawMessage(args)))
	}
	return msg
}
