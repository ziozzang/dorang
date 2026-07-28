package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TestDecoderRemapsBlockIndexToToolIndex is the mistake that looks like it
// works: a block index is not a tool-call index. Here the first tool call sits
// at block index 1 because a thinking block came first, and the second at block
// index 2 — but downstream they are calls 0 and 1.
func TestDecoderRemapsBlockIndexToToolIndex(t *testing.T) {
	stream := buildStream(t,
		frame(EventMessageStart, `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
		frame(EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		frame(EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"t"}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":0}`),
		frame(EventContentBlockStart, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_a","name":"f","input":{}}}`),
		frame(EventContentBlockDelta, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":1}`),
		frame(EventContentBlockStart, `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_b","name":"g","input":{}}}`),
		frame(EventContentBlockDelta, `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":2}`),
		frame(EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}`),
		frame(EventMessageStop, `{"type":"message_stop"}`),
	)
	events, err := DecodeStream(stream, nil)
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	seen := map[string]int{}
	for i := range events {
		for _, tc := range events[i].Delta.ToolCalls {
			if tc.ID != "" {
				seen[tc.ID] = tc.Index
			}
		}
	}
	if seen["toolu_a"] != 0 || seen["toolu_b"] != 1 {
		t.Errorf("tool indexes = %v, want toolu_a=0 toolu_b=1; copying the block index misnumbers every parallel call", seen)
	}
}

// TestDecoderIgnoresPing. dorang emits no ping (6.2) but the vendor's own
// stream sends them, and treating one as an unknown event would abort a
// perfectly good stream.
func TestDecoderIgnoresPing(t *testing.T) {
	stream := buildStream(t,
		frame(EventMessageStart, `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null}}`),
		frame(EventPing, `{"type":"ping"}`),
		": keep-alive comment\n\n",
		frame(EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		frame(EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":0}`),
		frame(EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`),
		frame(EventMessageStop, `{"type":"message_stop"}`),
	)
	events, err := DecodeStream(stream, nil)
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	var text string
	stops := 0
	for i := range events {
		for _, b := range events[i].Delta.Content {
			text += b.Text
		}
		if events[i].Type == canonical.EventStop {
			stops++
		}
	}
	if text != "hi" || stops != 1 {
		t.Errorf("text = %q, stops = %d, want \"hi\" and 1", text, stops)
	}
}

// TestDecoderTruncatedFinalFrame: a frame that arrived without its blank-line
// terminator is still an event. Dropping it loses the last thing the backend
// said, which on a truncated stream is the only clue about why it ended.
func TestDecoderTruncatedFinalFrame(t *testing.T) {
	stream := []byte(frame(EventMessageStart, `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null}}`) +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\",\"stop_sequence\":null}}")
	events, err := DecodeStream(stream, nil)
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	found := false
	for i := range events {
		if events[i].Type == canonical.EventStop && events[i].Delta.StopReason == canonical.StopMaxTokens {
			found = true
		}
	}
	if !found {
		t.Errorf("the unterminated final frame was dropped: %+v", events)
	}
}

// TestDecoderInBandError maps an error frame back to a neutral error with a
// status, which the wire has nowhere to put.
func TestDecoderInBandError(t *testing.T) {
	stream := buildStream(t,
		frame(EventMessageStart, `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null}}`),
		frame(EventError, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	)
	events, err := DecodeStream(stream, nil)
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != canonical.EventError || last.Err == nil {
		t.Fatalf("last event = %+v, want an error", last)
	}
	if last.Err.StatusCode != 529 {
		t.Errorf("status = %d, want 529 for overloaded_error", last.Err.StatusCode)
	}
}

// TestHeldMessageDeltaSurvivesLateContent is COMPATIBILITY 6.6's held terminal
// event, tested where it actually matters: content that arrives AFTER the
// backend reported its finish reason.
//
// A writer that emitted message_delta on the stop would have to either drop the
// late content or emit a content block after the message ended. Holding it does
// neither.
func TestHeldMessageDeltaSurvivesLateContent(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, StreamConfig{ID: "msg_1", Model: "m"})
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		Content: []canonical.Block{canonical.TextBlock("first")},
	}})
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventStop, Delta: canonical.Delta{
		StopReason: canonical.StopEndTurn,
	}})
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		Content: []canonical.Block{canonical.TextBlock(" second")},
	}})
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventUsage, Usage: &canonical.Usage{
		InputTokens: 5, OutputTokens: 2,
	}})
	_ = w.Close()

	got := buf.String()
	if !strings.Contains(got, `"text":" second"`) {
		t.Errorf("late content was dropped:\n%s", got)
	}
	iStop := strings.Index(got, "event: content_block_stop")
	iDelta := strings.Index(got, "event: message_delta")
	if iStop < 0 || iDelta < 0 || iStop > iDelta {
		t.Errorf("a content_block_stop must precede the terminal message_delta (6.6):\n%s", got)
	}
	if strings.Count(got, "event: message_delta") != 1 {
		t.Errorf("message_delta must appear exactly once:\n%s", got)
	}
	if !strings.Contains(got, `"output_tokens":2`) {
		t.Errorf("usage that arrived after the stop was lost (3.4):\n%s", got)
	}
}

// TestWriterDecoderRoundTrip: what this package writes, it can read.
func TestWriterDecoderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, StreamConfig{ID: "msg_1", Model: "claude-x", Usage: canonical.Usage{InputTokens: 11}})
	in := []canonical.StreamEvent{
		{Type: canonical.EventDelta, Delta: canonical.Delta{Content: []canonical.Block{canonical.ThinkingBlock("think", "")}}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{Content: []canonical.Block{canonical.ThinkingBlock("", "sig")}}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("say")}}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{{Index: 0, ID: "t1", Name: "f"}}}},
		{Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{{Index: 0, Arguments: `{"a":1}`}}}},
		{Type: canonical.EventStop, Delta: canonical.Delta{StopReason: canonical.StopToolUse}},
		{Type: canonical.EventUsage, Usage: &canonical.Usage{InputTokens: 11, OutputTokens: 4}},
	}
	for _, ev := range in {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}
	_ = w.Close()

	out, err := DecodeStream(buf.Bytes(), nil)
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	msg := accumulate(out)
	if len(msg.Content) != 3 {
		t.Fatalf("blocks = %d, want thinking + text + tool_use: %+v", len(msg.Content), msg.Content)
	}
	if msg.Content[0].Thinking == nil || msg.Content[0].Thinking.Signature != "sig" || msg.Content[0].Text != "think" {
		t.Errorf("thinking block = %+v", msg.Content[0])
	}
	if msg.Content[1].Text != "say" {
		t.Errorf("text = %q", msg.Content[1].Text)
	}
	if msg.Content[2].ToolUse == nil || msg.Content[2].ToolUse.ID != "t1" ||
		string(msg.Content[2].ToolUse.Input) != `{"a":1}` {
		t.Errorf("tool call = %+v", msg.Content[2])
	}
	stops := 0
	for i := range out {
		if out[i].Type == canonical.EventStop {
			stops++
			if out[i].Delta.StopReason != canonical.StopToolUse {
				t.Errorf("stop reason = %q", out[i].Delta.StopReason)
			}
		}
	}
	if stops != 1 {
		t.Errorf("stop events = %d, want 1", stops)
	}
}

// TestBothFramingLinesArePresent is COMPATIBILITY 6.1. A data-only stream is
// what a chat-completions writer produces and this family's SDK discards it.
func TestBothFramingLinesArePresent(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, StreamConfig{ID: "msg_1", Model: "m"})
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		Content: []canonical.Block{canonical.TextBlock("x")},
	}})
	_ = w.Close()

	for _, block := range strings.Split(strings.TrimSuffix(buf.String(), "\n\n"), "\n\n") {
		lines := strings.Split(block, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("frame is not event+data:\n%q", block)
		}
	}
}

// TestParallelToolCallsEachGetOneBlock is the shape the previous revision got
// wrong, and the reason its test passed anyway.
//
// The old emitter closed and reopened a block every time the tool index changed.
// Over a fixture whose calls arrive whole that produces a plausible-looking
// stream. Over the ordinary chat-completions shape — where ONE frame carries
// argument fragments for several parallel calls — it produces one tool_use block
// per fragment: the same id repeated, each block holding a slice of JSON that
// does not parse. The fixture below fragments the arguments, which is exactly
// what the old fixture omitted.
func TestParallelToolCallsEachGetOneBlock(t *testing.T) {
	em := NewEmitter(StreamConfig{ID: "m1", Model: "m"})
	var evs []Event
	push := func(cs ...canonical.ToolCallDelta) {
		evs = em.Push(evs, canonical.StreamEvent{
			Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: cs},
		})
	}
	push(canonical.ToolCallDelta{Index: 0, ID: "a", Name: "f"},
		canonical.ToolCallDelta{Index: 1, ID: "b", Name: "g"})
	// One frame, both calls, both fragments. This is the ordinary vLLM/OpenAI
	// shape for parallel calls and not an edge case.
	push(canonical.ToolCallDelta{Index: 0, Arguments: `{"x":`},
		canonical.ToolCallDelta{Index: 1, Arguments: `{"y":`})
	push(canonical.ToolCallDelta{Index: 0, Arguments: `1}`},
		canonical.ToolCallDelta{Index: 1, Arguments: `2}`})
	evs = em.Finish(evs)

	checkStreamInvariants(t, evs)
	blocks := toolBlocksOf(evs)
	if len(blocks) != 2 {
		t.Fatalf("got %d tool_use blocks, want exactly 2 — one per call:\n%s", len(blocks), dumpEvents(evs))
	}
	want := []struct{ id, name, input string }{{"a", "f", `{"x":1}`}, {"b", "g", `{"y":2}`}}
	for i, w := range want {
		got := blocks[i]
		if got.id != w.id || got.name != w.name {
			t.Errorf("block %d = %q/%q, want %q/%q", i, got.id, got.name, w.id, w.name)
		}
		if got.input != w.input {
			t.Errorf("block %d input = %q, want %q", i, got.input, w.input)
		}
		if !json.Valid([]byte(got.input)) {
			t.Errorf("block %d carries JSON that does not parse on its own: %q", i, got.input)
		}
	}
}

// TestToolCallResumedAfterItsBlockClosedReopens is the one case where reopening
// survives, and it is kept deliberately.
//
// Once a text block has been emitted the tool block behind it is closed and this
// protocol has no way to amend it. Reopening under the same id is the only shape
// left that a client can still act on; dropping the fragment would lose the call
// and buffering it would put the block after content that came later.
func TestToolCallResumedAfterItsBlockClosedReopens(t *testing.T) {
	var warned []Warning
	em := NewEmitter(StreamConfig{ID: "m1", Model: "m", Warn: func(w Warning) { warned = append(warned, w) }})
	var evs []Event
	evs = em.Push(evs, canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		ToolCalls: []canonical.ToolCallDelta{{Index: 0, ID: "a", Name: "f", Arguments: `{"x":`}},
	}})
	evs = em.Push(evs, canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		Content: []canonical.Block{canonical.TextBlock("interrupting")},
	}})
	evs = em.Push(evs, canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		ToolCalls: []canonical.ToolCallDelta{{Index: 0, Arguments: `1}`}},
	}})
	evs = em.Finish(evs)

	checkStreamInvariants(t, evs)
	if !hasWarning(warned, WarnInterleavedToolCalls) {
		t.Errorf("a call resumed after its block closed must warn: %+v", warned)
	}
	blocks := toolBlocksOf(evs)
	if len(blocks) != 2 {
		t.Fatalf("want the reopened block, got %d:\n%s", len(blocks), dumpEvents(evs))
	}
	// The fragment is delivered, in order, under the id it belongs to. The two
	// halves land in two blocks that share an id, which is all this protocol
	// leaves once the boundary has gone out — and is why the condition warns.
	for i, w := range []string{`{"x":`, `1}`} {
		if blocks[i].id != "a" {
			t.Errorf("block %d id = %q, want a", i, blocks[i].id)
		}
		if blocks[i].input != w {
			t.Errorf("block %d input = %q, want %q", i, blocks[i].input, w)
		}
	}
}

// seenToolBlock is one tool_use block as it appeared on the wire, with every
// input_json_delta of that block concatenated.
type seenToolBlock struct {
	index       int
	id          string
	name        string
	input       string
	sawInputSet bool
}

func toolBlocksOf(evs []Event) []seenToolBlock {
	var out []seenToolBlock
	cur := -1
	for i := range evs {
		switch p := evs[i].Payload.(type) {
		case *ContentBlockStartEvent:
			if p.ContentBlock.Type != BlockToolUse {
				cur = -1
				continue
			}
			out = append(out, seenToolBlock{
				index: p.Index, id: p.ContentBlock.ID, name: p.ContentBlock.Name,
				sawInputSet: len(p.ContentBlock.Input) > 0,
			})
			cur = len(out) - 1
		case *ContentBlockDeltaEvent:
			if cur >= 0 && p.Delta.Type == DeltaInputJSON {
				out[cur].input += p.Delta.PartialJSON
			}
		case *ContentBlockStopEvent:
			cur = -1
		}
	}
	return out
}

func dumpEvents(evs []Event) string {
	var b strings.Builder
	for i := range evs {
		b.WriteString(evs[i].Type)
		b.WriteByte(' ')
		if data, err := Marshal(evs[i].Payload); err == nil {
			b.Write(data)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
