package anthropic

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The cases below are the ones a tool-call path actually breaks on. Every one
// of them is a shape a real backend has produced; none of them is a shape
// dorang may repair, because dorang does not execute tools — the client does.
// The rule they are all decided by is: relay faithfully, or fail visibly.

// emit runs a sequence of neutral tool-call fragments through an Emitter and
// returns the finished event list and everything that was warned about.
func emit(t *testing.T, frags ...canonical.ToolCallDelta) ([]Event, []Warning) {
	t.Helper()
	var warned []Warning
	em := NewEmitter(StreamConfig{ID: "m1", Model: "m", Warn: func(w Warning) { warned = append(warned, w) }})
	var evs []Event
	for i := range frags {
		evs = em.Push(evs, canonical.StreamEvent{
			Type:  canonical.EventDelta,
			Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{frags[i]}},
		})
	}
	return em.Finish(evs), warned
}

// TestFragmentedToolNameIsAccumulated. Assigning rather than appending keeps
// only the last piece — and content_block_start has already gone out carrying
// the first, so the client calls a tool that does not exist under a name the
// gateway invented by truncation.
func TestFragmentedToolNameIsAccumulated(t *testing.T) {
	evs, _ := emit(t,
		canonical.ToolCallDelta{Index: 0, ID: "a", Name: "get_"},
		canonical.ToolCallDelta{Index: 0, Name: "weather"},
		canonical.ToolCallDelta{Index: 0, Arguments: `{}`},
	)
	checkStreamInvariants(t, evs)
	blocks := toolBlocksOf(evs)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d:\n%s", len(blocks), dumpEvents(evs))
	}
	if blocks[0].name != "get_weather" {
		t.Errorf("name = %q, want get_weather", blocks[0].name)
	}
}

// TestFragmentThatIsItselfACompleteName is the trap fragmentation sets: "aa" is
// a whole tool name AND the first half of "aaaa". Emitting content_block_start
// the moment a name looks complete calls the wrong tool, and nothing later can
// take it back.
func TestFragmentThatIsItselfACompleteName(t *testing.T) {
	evs, _ := emit(t,
		canonical.ToolCallDelta{Index: 0, ID: "a", Name: "aa"},
		canonical.ToolCallDelta{Index: 0, Name: "aa"},
		canonical.ToolCallDelta{Index: 0, Arguments: `{}`},
	)
	blocks := toolBlocksOf(evs)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d:\n%s", len(blocks), dumpEvents(evs))
	}
	// This side of the crossing has no tool table, so it cannot tell a
	// restatement from a fragment and takes the shape that actually occurs. The
	// OpenAI decoder does have the table and resolves it there; see
	// openai.TestFragmentedNameDisambiguatedByTheDeclaredTools.
	if blocks[0].name != "aa" {
		t.Errorf("name = %q; with no declared-tool table a repeat is read as a restatement", blocks[0].name)
	}
	// What must NOT happen either way is a second block: an emitter that starts
	// the block on the first fragment has to open another one when the rest of
	// the name arrives, and the client sees two calls where the model made one.
	if len(blocks) != 1 {
		t.Errorf("one call produced %d blocks", len(blocks))
	}
}

// TestRepeatedToolNameIsNotDoubled: a backend that restates the whole name on
// every fragment must not produce "get_weatherget_weatherget_weather".
func TestRepeatedToolNameIsNotDoubled(t *testing.T) {
	evs, warned := emit(t,
		canonical.ToolCallDelta{Index: 0, ID: "a", Name: "get_weather"},
		canonical.ToolCallDelta{Index: 0, Name: "get_weather"},
		canonical.ToolCallDelta{Index: 0, Name: "get_weather", Arguments: `{"c":1}`},
	)
	blocks := toolBlocksOf(evs)
	if len(blocks) != 1 || blocks[0].name != "get_weather" {
		t.Fatalf("name = %q, want get_weather:\n%s", blocks[0].name, dumpEvents(evs))
	}
	if !hasWarning(warned, WarnRepeatedToolName) {
		t.Errorf("repeated name metadata is out of contract and must be reported: %+v", warned)
	}
}

// TestNameIncompleteAtStreamEnd: a call whose arguments never arrive still has
// to reach the client, name and all. Holding the name until arguments settle it
// would otherwise drop it entirely on a zero-argument call.
func TestNameIncompleteAtStreamEnd(t *testing.T) {
	evs, _ := emit(t, canonical.ToolCallDelta{Index: 0, ID: "a", Name: "ping"})
	checkStreamInvariants(t, evs)
	blocks := toolBlocksOf(evs)
	if len(blocks) != 1 || blocks[0].name != "ping" || blocks[0].id != "a" {
		t.Fatalf("a zero-argument call was lost:\n%s", dumpEvents(evs))
	}
	if blocks[0].input != "" {
		t.Errorf("input deltas = %q, want none: content_block_start already carries {}", blocks[0].input)
	}
	if !blocks[0].sawInputSet {
		t.Error("content_block_start must carry input:{} so the call parses as zero-argument")
	}
}

// TestArgumentsBeforeName. The block cannot open until the name is known: an
// empty name in content_block_start is a call the client cannot dispatch, and
// the name that arrives afterwards has nowhere to go.
func TestArgumentsBeforeName(t *testing.T) {
	evs, _ := emit(t,
		canonical.ToolCallDelta{Index: 0, ID: "a", Arguments: `{"x":1}`},
		canonical.ToolCallDelta{Index: 0, Name: "late"},
	)
	checkStreamInvariants(t, evs)
	blocks := toolBlocksOf(evs)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d:\n%s", len(blocks), dumpEvents(evs))
	}
	if blocks[0].name != "late" {
		t.Errorf("name = %q, want late: the block must wait for it", blocks[0].name)
	}
	if blocks[0].input != `{"x":1}` {
		t.Errorf("input = %q, want the arguments that arrived first", blocks[0].input)
	}
}

// TestMissingToolCallID: dorang does not mint one. The id is what the client
// echoes back in tool_result and what the backend matches on the next turn, so a
// gateway-invented id is a correlation neither end agreed to.
func TestMissingToolCallID(t *testing.T) {
	evs, warned := emit(t,
		canonical.ToolCallDelta{Index: 0, Name: "f"},
		canonical.ToolCallDelta{Index: 0, Arguments: `{}`},
	)
	blocks := toolBlocksOf(evs)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d:\n%s", len(blocks), dumpEvents(evs))
	}
	if blocks[0].id != "" {
		t.Errorf("id = %q; dorang must not invent one", blocks[0].id)
	}
	if !hasWarning(warned, WarnToolCallMissingID) {
		t.Errorf("a call with no id must be reported: %+v", warned)
	}
}

// TestNonFunctionToolTypeStillReachesTheClient. This family has one tool block
// type, so a call the other family typed as something else still becomes
// tool_use. Dropping it would lose a call the model made.
func TestNonFunctionToolTypeStillReachesTheClient(t *testing.T) {
	evs, _ := emit(t,
		canonical.ToolCallDelta{Index: 0, ID: "a", Name: "shell", Type: "custom"},
		canonical.ToolCallDelta{Index: 0, Arguments: `{"cmd":"ls"}`},
	)
	checkStreamInvariants(t, evs)
	blocks := toolBlocksOf(evs)
	if len(blocks) != 1 || blocks[0].name != "shell" {
		t.Fatalf("a non-function tool call was dropped:\n%s", dumpEvents(evs))
	}
}

// TestNegativeToolIndex: an index no client array can address is still a call.
// It gets a block; what it must not do is collide with index 0.
func TestNegativeToolIndex(t *testing.T) {
	evs, _ := emit(t,
		canonical.ToolCallDelta{Index: -1, ID: "a", Name: "f"},
		canonical.ToolCallDelta{Index: -1, Arguments: `{"a":1}`},
		canonical.ToolCallDelta{Index: 0, ID: "b", Name: "g"},
		canonical.ToolCallDelta{Index: 0, Arguments: `{"b":2}`},
	)
	checkStreamInvariants(t, evs)
	blocks := toolBlocksOf(evs)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 — a negative index is a distinct call:\n%s", len(blocks), dumpEvents(evs))
	}
	if blocks[0].id != "a" || blocks[1].id != "b" {
		t.Errorf("ids = %q,%q, want a,b", blocks[0].id, blocks[1].id)
	}
}

// TestSecondIDUnderOneIndexIsASecondCall is COMPATIBILITY 5.1 read from the
// receiving side. A backend that omits index — or sends zero for every call —
// otherwise merges parallel calls into one whose arguments are several JSON
// documents end to end, and nothing anywhere says so.
func TestSecondIDUnderOneIndexIsASecondCall(t *testing.T) {
	evs, warned := emit(t,
		canonical.ToolCallDelta{Index: 0, ID: "a", Name: "f"},
		canonical.ToolCallDelta{Index: 0, Arguments: `{"x":1}`},
		canonical.ToolCallDelta{Index: 0, ID: "b", Name: "g"},
		canonical.ToolCallDelta{Index: 0, Arguments: `{"y":2}`},
	)
	checkStreamInvariants(t, evs)
	blocks := toolBlocksOf(evs)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2: two ids are two calls:\n%s", len(blocks), dumpEvents(evs))
	}
	if blocks[0].input != `{"x":1}` || blocks[1].input != `{"y":2}` {
		t.Errorf("inputs = %q,%q; the two documents were merged", blocks[0].input, blocks[1].input)
	}
	if !hasWarning(warned, WarnToolCallIndexReused) {
		t.Errorf("the reused index must be reported: %+v", warned)
	}
}

// TestInvalidArgumentsFailInBand is finding 4's decision, pinned.
//
// A stream that ends mid-JSON must not be finished with stop_reason tool_use: an
// agentic client reads that as "the call is ready" and fails on its own parse,
// naming a JSON error rather than this gateway. dorang cannot complete the
// document — inventing the missing bytes invents an argument — so it says so
// where the client is still listening.
func TestInvalidArgumentsFailInBand(t *testing.T) {
	evs, warned := emit(t,
		canonical.ToolCallDelta{Index: 0, ID: "a", Name: "f"},
		canonical.ToolCallDelta{Index: 0, Arguments: `{"city":`},
	)
	checkStreamInvariants(t, evs)
	if !hasWarning(warned, WarnMalformedToolArguments) {
		t.Errorf("malformed arguments must be reported: %+v", warned)
	}
	last := evs[len(evs)-1]
	if last.Type != EventError {
		t.Fatalf("stream ended with %q, want an in-band error:\n%s", last.Type, dumpEvents(evs))
	}
	for i := range evs {
		if evs[i].Type == EventMessageStop {
			t.Fatal("message_stop after a broken tool call tells the client the message completed")
		}
		if md, ok := evs[i].Payload.(*MessageDeltaEvent); ok && md.Delta.StopReason != nil {
			t.Errorf("stop_reason %q went out over arguments that do not parse", *md.Delta.StopReason)
		}
	}
}

// TestValidZeroArgumentCallDoesNotFail guards the other side of the same rule:
// no argument fragments at all is a zero-argument call, not a broken one.
func TestValidZeroArgumentCallDoesNotFail(t *testing.T) {
	evs, warned := emit(t, canonical.ToolCallDelta{Index: 0, ID: "a", Name: "now"})
	if hasWarning(warned, WarnMalformedToolArguments) {
		t.Errorf("a zero-argument call was reported as malformed: %+v", warned)
	}
	if evs[len(evs)-1].Type != EventMessageStop {
		t.Fatalf("stream did not complete:\n%s", dumpEvents(evs))
	}
}

// TestStreamWriterReportsMalformedArguments: the in-band frame tells the client,
// and the returned error is what makes the exchange COUNT as failed rather than
// be recorded as a clean 200.
func TestStreamWriterReportsMalformedArguments(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, StreamConfig{ID: "m1", Model: "m"})
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		ToolCalls: []canonical.ToolCallDelta{{Index: 0, ID: "a", Name: "f", Arguments: `{"x":`}},
	}})
	err := w.Close()
	if err != ErrMalformedToolArguments {
		t.Fatalf("Close = %v, want ErrMalformedToolArguments", err)
	}
	got := buf.String()
	if !strings.Contains(got, "event: error") {
		t.Errorf("no in-band error frame:\n%s", got)
	}
	if strings.Contains(got, "message_stop") {
		t.Errorf("the stream claimed completion:\n%s", got)
	}
}

// TestSemanticDataAfterStopIsDeliveredAndReported. Out of contract, and still
// forwarded: the held message_delta exists precisely so late content lands
// inside the message rather than after it (COMPATIBILITY 6.6).
func TestSemanticDataAfterStopIsDeliveredAndReported(t *testing.T) {
	var warned []Warning
	em := NewEmitter(StreamConfig{ID: "m1", Model: "m", Warn: func(w Warning) { warned = append(warned, w) }})
	var evs []Event
	evs = em.Push(evs, canonical.StreamEvent{Type: canonical.EventStop, Delta: canonical.Delta{
		StopReason: canonical.StopEndTurn,
	}})
	evs = em.Push(evs, canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		Content: []canonical.Block{canonical.TextBlock("late")},
	}})
	evs = em.Finish(evs)

	checkStreamInvariants(t, evs)
	if !hasWarning(warned, WarnDataAfterFinish) {
		t.Errorf("content after the terminal reason must be reported: %+v", warned)
	}
	if !strings.Contains(dumpEvents(evs), "late") {
		t.Errorf("late content was dropped:\n%s", dumpEvents(evs))
	}
}

// ---------------------------------------------------------------------------
// The decoding side: this family as the BACKEND
// ---------------------------------------------------------------------------

// TestZeroArgumentToolCallCarriesEmptyObject. content_block_start's input was
// dropped outright, so a call with no arguments reached an OpenAI-shaped client
// with the arguments field ABSENT rather than "{}" — and the clients that do
// json.loads(arguments) on it raise instead of calling the tool.
func TestZeroArgumentToolCallCarriesEmptyObject(t *testing.T) {
	stream := buildStream(t,
		frame(EventMessageStart, `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null}}`),
		frame(EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"now","input":{}}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":0}`),
		frame(EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null}}`),
		frame(EventMessageStop, `{"type":"message_stop"}`),
	)
	events, err := DecodeStream(stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := argsOf(events, 0); got != "{}" {
		t.Errorf("arguments = %q, want {}", got)
	}
}

// TestNonEmptyContentBlockStartInput: a backend that puts the whole argument
// object in the start event rather than in deltas. Dropping it turns a real call
// into a zero-argument one, which is a different call.
func TestNonEmptyContentBlockStartInput(t *testing.T) {
	stream := buildStream(t,
		frame(EventMessageStart, `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null}}`),
		frame(EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"f","input":{"city":"Seoul"}}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":0}`),
		frame(EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null}}`),
		frame(EventMessageStop, `{"type":"message_stop"}`),
	)
	events, err := DecodeStream(stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := argsOf(events, 0); got != `{"city":"Seoul"}` {
		t.Errorf("arguments = %q, want the object content_block_start carried", got)
	}
}

// TestStartInputAndDeltasConflict: both spellings for one call. They cannot be
// concatenated — that is two JSON documents in one field — so the incremental
// form wins, because it is the one this protocol defines as cumulative, and the
// contradiction is reported.
func TestStartInputAndDeltasConflict(t *testing.T) {
	var warned []Warning
	opt := &DecodeOptions{Warn: func(w Warning) { warned = append(warned, w) }}
	stream := buildStream(t,
		frame(EventMessageStart, `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null}}`),
		frame(EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"f","input":{"stale":true}}}`),
		frame(EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"fresh\":true}"}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":0}`),
		frame(EventMessageStop, `{"type":"message_stop"}`),
	)
	events, err := DecodeStream(stream, opt)
	if err != nil {
		t.Fatal(err)
	}
	if got := argsOf(events, 0); got != `{"fresh":true}` {
		t.Errorf("arguments = %q, want only the incremental form", got)
	}
	if !hasWarning(warned, WarnToolInputConflict) {
		t.Errorf("the contradiction must be reported: %+v", warned)
	}
}

// TestDuplicateContentBlockStop and a stop for a block that never started. A
// stray frame must not allocate a tool index: doing so invents a parallel call
// out of nothing and renumbers every real one after it.
func TestStrayContentBlockStops(t *testing.T) {
	var warned []Warning
	opt := &DecodeOptions{Warn: func(w Warning) { warned = append(warned, w) }}
	stream := buildStream(t,
		frame(EventMessageStart, `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":7}`),
		frame(EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"f","input":{}}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":0}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":0}`),
		frame(EventMessageStop, `{"type":"message_stop"}`),
	)
	events, err := DecodeStream(stream, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(warned, WarnUnknownBlockStop) {
		t.Errorf("a stop for a block that never started must be reported: %+v", warned)
	}
	if !hasWarning(warned, WarnDuplicateBlockStop) {
		t.Errorf("a duplicate stop must be reported: %+v", warned)
	}
	calls := 0
	for i := range events {
		calls += len(events[i].Delta.ToolCalls)
	}
	// One start, one close. The two stray stops produce nothing.
	if calls != 2 {
		t.Errorf("tool-call fragments = %d, want 2; a stray stop invented a call", calls)
	}
	if got := argsOf(events, 0); got != "{}" {
		t.Errorf("arguments = %q, want {}", got)
	}
}

// TestEOFBeforeTheTerminalEvent: a stream cut off before message_stop.
//
// COMPATIBILITY 4.4 still applies — the terminal is synthesized and upgraded to
// tool_use, because a client told the turn ended in text never runs the tool —
// but only over a call whose arguments actually parse. The truncated-arguments
// case is TestInvalidArgumentsFailInBand, and it fails instead.
func TestEOFBeforeTheTerminalEvent(t *testing.T) {
	stream := []byte(frame(EventMessageStart, `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null}}`) +
		frame(EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"f","input":{}}}`) +
		frame(EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`))
	events, err := DecodeStream(stream, nil)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	w := NewStreamWriter(&buf, StreamConfig{ID: "m2", Model: "m"})
	for _, ev := range events {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if !strings.Contains(buf.String(), `"stop_reason":"tool_use"`) {
		t.Errorf("a truncated tool-call turn must still be terminated as tool_use (4.4):\n%s", buf.String())
	}
}

// argsOf concatenates every argument fragment of one tool-call index.
func argsOf(events []canonical.StreamEvent, index int) string {
	var b strings.Builder
	for i := range events {
		for _, tc := range events[i].Delta.ToolCalls {
			if tc.Index == index {
				b.WriteString(tc.Arguments)
			}
		}
	}
	return b.String()
}

// TestInterleavingIsReportedOnlyWhenItHappens. Two parallel calls sent one after
// the other are not interleaved and warning about them trains an operator to
// ignore the warning that matters.
func TestInterleavingIsReportedOnlyWhenItHappens(t *testing.T) {
	t.Run("sequential parallel calls", func(t *testing.T) {
		evs, warned := emit(t,
			canonical.ToolCallDelta{Index: 0, ID: "a", Name: "f"},
			canonical.ToolCallDelta{Index: 0, Arguments: `{"x":1}`},
			canonical.ToolCallDelta{Index: 1, ID: "b", Name: "g"},
			canonical.ToolCallDelta{Index: 1, Arguments: `{"y":2}`},
		)
		checkStreamInvariants(t, evs)
		if hasWarning(warned, WarnInterleavedToolCalls) {
			t.Errorf("two calls in sequence are not interleaved: %+v", warned)
		}
		if b := toolBlocksOf(evs); len(b) != 2 || b[0].input != `{"x":1}` || b[1].input != `{"y":2}` {
			t.Errorf("blocks = %+v", b)
		}
	})
	t.Run("genuinely interleaved", func(t *testing.T) {
		evs, warned := emit(t,
			canonical.ToolCallDelta{Index: 0, ID: "a", Name: "f", Arguments: `{"x":`},
			canonical.ToolCallDelta{Index: 1, ID: "b", Name: "g", Arguments: `{"y":`},
			canonical.ToolCallDelta{Index: 0, Arguments: `1}`},
			canonical.ToolCallDelta{Index: 1, Arguments: `2}`},
		)
		checkStreamInvariants(t, evs)
		if !hasWarning(warned, WarnInterleavedToolCalls) {
			t.Errorf("a call resumed after another started must be reported: %+v", warned)
		}
		b := toolBlocksOf(evs)
		if len(b) != 2 {
			t.Fatalf("blocks = %d, want 2:\n%s", len(b), dumpEvents(evs))
		}
		if b[0].input != `{"x":1}` || b[1].input != `{"y":2}` {
			t.Errorf("inputs = %q,%q, want each call's document whole", b[0].input, b[1].input)
		}
	})
}
