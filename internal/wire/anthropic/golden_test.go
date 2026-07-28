package anthropic

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TestFullStreamGolden freezes a complete stream, byte for byte, from
// message_start to message_stop.
//
// It covers every row of COMPATIBILITY §6 that has a wire consequence:
//
//	6.1 event: AND data: lines on every frame
//	6.2 the six event types, no ping, no [DONE], one message_stop
//	6.3 text, tool_use and thinking blocks all present
//	6.4 stop_reason on the terminal message_delta
//	6.5 stop_sequence null
//	6.6 synthesized block boundaries and the held message_delta — note that the
//	    stop arrives BEFORE the usage in the input and the content_block_stop of
//	    block 2 still precedes the message_delta
//	6.7 cache counters seeded at zero, filled at the end, present only when > 0,
//	    and input_tokens = 100 - 40 - 10
//	6.8 no total_tokens anywhere in a stream
func TestFullStreamGolden(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, StreamConfig{
		ID:    "msg_test",
		Model: "claude-x",
		// Seeded with the prompt count only: the cache counters are not known
		// when the stream opens.
		Usage: canonical.Usage{InputTokens: 50},
	})

	events := []canonical.StreamEvent{
		{Type: canonical.EventStart, ID: "msg_test", Model: "claude-x"},
		{Type: canonical.EventDelta, Delta: canonical.Delta{
			Content: []canonical.Block{canonical.ThinkingBlock("I should check the weather.", "")},
		}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{
			Content: []canonical.Block{canonical.ThinkingBlock("", "ErUBCkYIAxgCIkAsig")},
		}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{
			Content: []canonical.Block{canonical.TextBlock("Let me look.")},
		}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{
			ToolCalls: []canonical.ToolCallDelta{{Index: 0, ID: "toolu_01", Name: "get_weather", Type: "function"}},
		}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{
			ToolCalls: []canonical.ToolCallDelta{{Index: 0, Arguments: `{"city":`}},
		}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{
			ToolCalls: []canonical.ToolCallDelta{{Index: 0, Arguments: `"Seoul"}`}},
		}},
		// The stop arrives while block 2 is still open, and usage arrives after
		// the stop (COMPATIBILITY 3.4). Both are held.
		{Type: canonical.EventStop, Delta: canonical.Delta{StopReason: canonical.StopToolUse}},
		{Type: canonical.EventUsage, Usage: &canonical.Usage{
			InputTokens: 100, OutputTokens: 25, CacheReadTokens: 40, CacheWriteTokens: 10,
		}},
	}
	for _, ev := range events {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	const want = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":50,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"I should check the weather."}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"ErUBCkYIAxgCIkAsig"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me look."}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"Seoul\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":2}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":40,"output_tokens":25}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	if got := buf.String(); got != want {
		t.Errorf("stream mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// 6.2 negatives, asserted rather than assumed.
	if strings.Contains(buf.String(), "[DONE]") {
		t.Error("stream contains [DONE]; this protocol has no such terminator (6.2)")
	}
	if strings.Contains(buf.String(), "event: ping") {
		t.Error("stream contains a ping; dorang emits none (6.2)")
	}
	if n := strings.Count(buf.String(), "event: message_stop"); n != 1 {
		t.Errorf("message_stop appeared %d times, want exactly 1 (6.2)", n)
	}
}

// TestStreamNoCacheOmitsCacheFields is the other half of 6.7: with no cache
// activity, neither cache key appears anywhere.
func TestStreamNoCacheOmitsCacheFields(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, StreamConfig{ID: "msg_1", Model: "m"})
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		Content: []canonical.Block{canonical.TextBlock("hi")},
	}})
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventUsage, Usage: &canonical.Usage{
		InputTokens: 7, OutputTokens: 3,
	}})
	_ = w.Close()

	for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
		if strings.Contains(buf.String(), key) {
			t.Errorf("%s present with a zero count; 6.7 says cache fields appear only when > 0", key)
		}
	}
	// 6.8: the streaming shape never carries the non-spec total.
	if strings.Contains(buf.String(), "total_tokens") {
		t.Error("total_tokens present in a stream; 6.8 says the streaming shape omits it")
	}
}

// TestSynthesizedStopReason is the mirror of COMPATIBILITY 4.4. A backend that
// ends without a terminal reason on a tool-call turn must not be reported as
// end_turn, or the client treats the turn as final text and never runs the tool.
func TestSynthesizedStopReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool bool
		want string
	}{
		{"text only", false, `"stop_reason":"end_turn"`},
		{"tool call", true, `"stop_reason":"tool_use"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			var warned []Warning
			w := NewStreamWriter(&buf, StreamConfig{
				ID: "msg_1", Model: "m",
				Warn: func(x Warning) { warned = append(warned, x) },
			})
			if tc.tool {
				_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
					ToolCalls: []canonical.ToolCallDelta{{Index: 0, ID: "t1", Name: "f"}},
				}})
			} else {
				_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
					Content: []canonical.Block{canonical.TextBlock("hi")},
				}})
			}
			_ = w.Close()
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("want %s in\n%s", tc.want, buf.String())
			}
			if !hasWarning(warned, WarnSynthesizedStop) {
				t.Error("synthesizing a stop reason must warn")
			}
		})
	}
}

// TestStopReasonCollapse is COMPATIBILITY 6.4. The wire says end_turn and the
// header says what actually happened.
func TestStopReasonCollapse(t *testing.T) {
	cases := []struct {
		reason canonical.StopReason
		wire   string
		native string
	}{
		{canonical.StopEndTurn, "end_turn", ""},
		{canonical.StopMaxTokens, "max_tokens", ""},
		{canonical.StopToolUse, "tool_use", ""},
		// Everything else collapses. A filtered turn is reported as a normal end
		// — which is the harm the header exists to undo.
		{canonical.StopContentFilter, "end_turn", "content_filter"},
		{canonical.StopRefusal, "end_turn", "refusal"},
		{canonical.StopStopSequence, "end_turn", "stop_sequence"},
		{canonical.StopError, "end_turn", "error"},
		{canonical.StopSafety, "end_turn", "safety"},
		{canonical.StopPauseTurn, "end_turn", "pause_turn"},
	}
	for _, tc := range cases {
		wire, native := StopReasonOf(tc.reason, StopReasonCollapse)
		if wire != tc.wire || native != tc.native {
			t.Errorf("StopReasonOf(%q) = (%q,%q), want (%q,%q)", tc.reason, wire, native, tc.wire, tc.native)
		}
	}
	// With the collapse turned off, this family's own enumeration is reachable.
	if wire, _ := StopReasonOf(canonical.StopStopSequence, StopReasonNative); wire != "stop_sequence" {
		t.Errorf("native mode collapsed stop_sequence to %q", wire)
	}
	if wire, native := StopReasonOf(canonical.StopContentFilter, StopReasonNative); wire != "refusal" || native != "content_filter" {
		t.Errorf("native mode: got (%q,%q), want (refusal,content_filter)", wire, native)
	}
}

// TestResponseGolden freezes the non-streaming shape, including 6.5's null
// stop_sequence and 6.8's non-spec total_tokens.
func TestResponseGolden(t *testing.T) {
	r := &canonical.Response{
		ID:    "msg_1",
		Model: "claude-x",
		Choices: []canonical.Choice{{
			Message:    canonical.Message{Role: canonical.RoleAssistant, Content: canonical.Content{canonical.TextBlock("Hello")}},
			StopReason: canonical.StopEndTurn,
		}},
		Usage: &canonical.Usage{InputTokens: 100, OutputTokens: 25, CacheReadTokens: 40, CacheWriteTokens: 10},
	}
	got, err := MarshalResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	const want = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":40,"output_tokens":25,"total_tokens":125}}`
	if string(got) != want {
		t.Errorf("response mismatch\ngot:  %s\nwant: %s", got, want)
	}

	// 6.8 behind its compat flag: the strict vendor shape has no total.
	got, err = MarshalResponse(r, &ResponseOptions{TotalTokens: TotalTokensOmit})
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	if strings.Contains(string(got), "total_tokens") {
		t.Errorf("TotalTokensOmit still emitted total_tokens: %s", got)
	}
}

// TestResponseNativeStopReasonHeader asserts the out-of-band half of 6.4.
func TestResponseNativeStopReasonHeader(t *testing.T) {
	r := &canonical.Response{
		ID: "msg_1", Model: "m",
		Choices: []canonical.Choice{{
			Message:    canonical.Message{Role: canonical.RoleAssistant, Content: canonical.Content{canonical.TextBlock("...")}},
			StopReason: canonical.StopContentFilter,
		}},
	}
	enc, err := EncodeResponse(r, nil)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if enc.Response.StopReason == nil || *enc.Response.StopReason != "end_turn" {
		t.Errorf("wire stop_reason = %v, want end_turn (6.4)", enc.Response.StopReason)
	}
	if enc.NativeStopReason != "content_filter" {
		t.Errorf("native stop reason = %q, want content_filter; without it the collapse is a lie", enc.NativeStopReason)
	}
	if enc.Response.StopSequence != nil {
		t.Error("stop_sequence must be null on the adapter path (6.5)")
	}
}

// TestCountTokensGolden is COMPATIBILITY 6.9: exactly one key.
func TestCountTokensGolden(t *testing.T) {
	b, err := MarshalCountTokens(42)
	if err != nil {
		t.Fatalf("MarshalCountTokens: %v", err)
	}
	if string(b) != `{"input_tokens":42}` {
		t.Errorf("count_tokens body = %s, want {\"input_tokens\":42}", b)
	}
}

// TestCountTokensRequestNeedsNoMaxTokens: the ceiling is required on
// /v1/messages and meaningless on count_tokens.
func TestCountTokensRequestNeedsNoMaxTokens(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := DecodeRequest(body); err == nil {
		t.Error("DecodeRequest accepted a body with no max_tokens; the field is required (§10.7)")
	}
	req, err := DecodeCountTokensRequest(body)
	if err != nil {
		t.Fatalf("DecodeCountTokensRequest: %v", err)
	}
	if req.MaxTokens != nil {
		t.Error("count_tokens must not invent a ceiling")
	}
}

// TestErrorEnvelopeGolden asserts the union of the two contracts: this family's
// outer discriminator plus COMPATIBILITY 7.1's four inner keys, with code a
// STRING.
func TestErrorEnvelopeGolden(t *testing.T) {
	b, err := EncodeError(NewError(400, TypeInvalidRequest, "boom"))
	if err != nil {
		t.Fatalf("EncodeError: %v", err)
	}
	const want = `{"type":"error","error":{"type":"invalid_request_error","message":"boom","param":null,"code":"400"}}`
	if string(b) != want {
		t.Errorf("error envelope = %s\nwant %s", b, want)
	}

	// 7.1's defensive half: a numeric code from a backend never reaches a client
	// as a number.
	var warned []Warning
	e, err := DecodeError([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down","code":429}}`),
		func(w Warning) { warned = append(warned, w) })
	if err != nil {
		t.Fatalf("DecodeError: %v", err)
	}
	if e.Code != "429" {
		t.Errorf("code = %q, want the string 429", e.Code)
	}
	if !hasWarning(warned, WarnNumericErrorCode) {
		t.Error("a numeric code must warn, not be normalized in silence")
	}
}

// TestHTMLEscapingOff is COMPATIBILITY 2.1a applied here: the serializer is
// normative, and Go's default would turn "&&" into "\u0026\u0026".
func TestHTMLEscapingOff(t *testing.T) {
	r := &canonical.Response{
		ID: "msg_1", Model: "m",
		Choices: []canonical.Choice{{
			Message:    canonical.Message{Role: canonical.RoleAssistant, Content: canonical.Content{canonical.TextBlock("a && b <tag>")}},
			StopReason: canonical.StopEndTurn,
		}},
	}
	b, err := MarshalResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	if !strings.Contains(string(b), `"a && b <tag>"`) {
		t.Errorf("HTML escaping is on: %s", b)
	}
}

// TestInBandErrorEndsStream is COMPATIBILITY 1.3 on this surface. Once a frame
// is out the status is 200 and cannot change, so the error rides in band — and
// it is NOT followed by message_stop, which would claim the message completed.
func TestInBandErrorEndsStream(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, StreamConfig{ID: "msg_1", Model: "m"})
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		Content: []canonical.Block{canonical.TextBlock("partial")},
	}})
	if err := w.WriteError(NewError(500, TypeAPIError, "upstream died")); err != nil {
		t.Fatalf("WriteError: %v", err)
	}
	_ = w.Close()

	got := buf.String()
	if !strings.Contains(got, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream died\"") {
		t.Errorf("missing in-band error frame:\n%s", got)
	}
	if strings.Contains(got, "message_stop") {
		t.Errorf("message_stop after an error claims the message completed:\n%s", got)
	}
}

// TestEmptyStreamIsStillWellFormed. COMPATIBILITY 1.4's "empty upstream, empty
// body" is a chat-completions rule; here the SDK treats a stream with no
// message_stop as a transport failure.
func TestEmptyStreamIsStillWellFormed(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, StreamConfig{ID: "msg_1", Model: "m"})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"event: message_start", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(got, want) {
			t.Errorf("empty stream is missing %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "content_block") {
		t.Errorf("empty stream invented a content block:\n%s", got)
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
