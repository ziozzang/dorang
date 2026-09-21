package openai

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
)

// fixedNow is the backend clock the tests move: `created` must be pinned from
// it, and pinning is what keeps the streamed and buffered renderings of one
// conversion identical (DESIGN §10.7).
func fixedNow() time.Time { return time.Unix(1700000000, 0) }

func testEcho() *ResponsesOptions {
	t := 0.25
	return &ResponsesOptions{
		ID: "resp_test", Model: "client-model", Created: fixedNow().Unix(),
		Temperature: &t,
	}
}

// feed drives a writer over one neutral event sequence and returns the framed
// body and the writer (for its accounting).
func feed(t *testing.T, evs ...canonical.StreamEvent) (string, *ResponsesStreamWriter) {
	t.Helper()
	var sb strings.Builder
	w := NewResponsesStreamWriter(&sb, ResponsesStreamConfig{Echo: testEcho(), Now: fixedNow})
	for _, ev := range evs {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatalf("write %v: %v", ev.Type, err)
		}
	}
	if err := w.Close(); err != nil && !errors.Is(err, ErrMalformedToolArguments) {
		t.Fatalf("close: %v", err)
	}
	return sb.String(), w
}

// eventNames walks the framed body and returns the `event:` names in order.
func eventNames(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			out = append(out, after)
		}
	}
	return out
}

// seqNumbers walks the framed body and returns every sequence_number.
func seqNumbers(body string) (out []int64) {
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		// The last member of every payload is sequence_number; find it without
		// a full parse so a framing bug cannot hide behind a JSON decoder.
		i := strings.LastIndex(data, `"sequence_number":`)
		if i < 0 {
			continue
		}
		rest := data[i+len(`"sequence_number":`):]
		if j := strings.IndexAny(rest, "},"); j >= 0 {
			rest = rest[:j]
		}
		var n int64
		for _, c := range strings.TrimSpace(rest) {
			if c < '0' || c > '9' {
				continue
			}
			n = n*10 + int64(c-'0')
		}
		out = append(out, n)
	}
	return out
}

func textEvent(s string) canonical.StreamEvent {
	return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		Content: []canonical.Block{canonical.TextBlock(s)},
	}}
}

func thinkEvent(s string) canonical.StreamEvent {
	return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		Content: []canonical.Block{canonical.ThinkingBlock(s, "")},
	}}
}

func startEvent() canonical.StreamEvent {
	return canonical.StreamEvent{Type: canonical.EventStart, ID: "resp_test", Model: "client-model"}
}

func stopEvent(r canonical.StopReason) canonical.StreamEvent {
	return canonical.StreamEvent{Type: canonical.EventStop, Delta: canonical.Delta{StopReason: r}}
}

func usageEvent(in, out int) canonical.StreamEvent {
	u := canonical.Usage{InputTokens: in, OutputTokens: out}
	return canonical.StreamEvent{Type: canonical.EventUsage, Usage: &u}
}

// The emitted set is the decoder's mirror plus the part lifecycle the
// deployed protocol's clients key on (the reference client — codex — logs a
// delta with no open part): item added, part added, deltas, part done, item
// done. Everything else the vendor sends is bookkeeping nothing consumed
// here needs.
func TestResponsesWriterEmitsTheMinimalEventSet(t *testing.T) {
	body, _ := feed(t, startEvent(), textEvent("hi"), stopEvent(canonical.StopEndTurn), usageEvent(3, 5))
	names := eventNames(body)
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(names) != len(want) {
		t.Fatalf("events = %v, want exactly %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("events = %v, want %v", names, want)
		}
	}
	// `in_progress` as a STATUS on response.created is the protocol's own
	// opening state; the `response.in_progress` EVENT is what must never be
	// written, so the ban is on the event line.
	for _, banned := range []string{
		"event: response.in_progress", "event: response.output_text.done",
		"[DONE]", "event: ping",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("body contains %q, which the writer must not emit", banned)
		}
	}
}

// sequence_number starts at zero and climbs by one, on every event including
// the terminal — the counter describes the stream, not the success.
func TestResponsesWriterSequenceNumbersStartAtZeroAndClimb(t *testing.T) {
	body, _ := feed(t, startEvent(), textEvent("a"), textEvent("b"), stopEvent(canonical.StopEndTurn))
	got := seqNumbers(body)
	if len(got) == 0 {
		t.Fatal("no sequence numbers in body")
	}
	if got[0] != 0 {
		t.Errorf("first sequence_number = %d, want 0", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("sequence_numbers = %v, want each +1 (the numbering discipline is one counter, no gaps)", got)
		}
	}
}

// Identity is pinned: the id is the caller's `resp_…`, `created_at` fixed at
// construction, and the CLIENT-FACING model on every response object — never
// whatever the upstream served.
func TestResponsesWriterPinsTheStreamIdentity(t *testing.T) {
	body, _ := feed(t, startEvent(), textEvent("x"), stopEvent(canonical.StopEndTurn))
	if !strings.Contains(body, `"id":"resp_test"`) {
		t.Error("body does not carry the pinned response id")
	}
	if !strings.Contains(body, `"created_at":1700000000`) {
		t.Error("body does not carry the pinned created_at")
	}
	if !strings.Contains(body, `"model":"client-model"`) {
		t.Error("body does not carry the client-facing model")
	}
	if strings.Count(body, `"model":"client-model"`) < 2 {
		t.Error("identity should appear on more than one response object")
	}
}

// A kind transition closes the open item and opens the next: two added/done
// pairs with output indexes 0 and 1, each done exactly once.
func TestResponsesWriterClosesItemsOnKindTransition(t *testing.T) {
	body, _ := feed(t, startEvent(), thinkEvent("why"), textEvent("because"), stopEvent(canonical.StopEndTurn))
	names := eventNames(body)
	count := func(n string) int {
		c := 0
		for _, x := range names {
			if x == n {
				c++
			}
		}
		return c
	}
	if c := count("response.output_item.added"); c != 2 {
		t.Errorf("%d output_item.added, want 2 (reasoning then message)", c)
	}
	if c := count("response.output_item.done"); c != 2 {
		t.Errorf("%d output_item.done, want 2, each item exactly once", c)
	}
	if !strings.Contains(body, `"output_index":0`) || !strings.Contains(body, `"output_index":1`) {
		t.Error("output indexes 0 and 1 both expected")
	}
	if !strings.Contains(body, `"type":"reasoning"`) || !strings.Contains(body, `"type":"message"`) {
		t.Error("both a reasoning and a message item expected")
	}
}

// Tool calls stream their arguments and close with the assembled item.
func TestResponsesWriterAssemblesToolCallsFromFragments(t *testing.T) {
	body, w := feed(t,
		startEvent(),
		canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
			{Index: 0, ID: "call_1", Name: "get_wea"},
		}}},
		canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
			{Index: 0, Arguments: `{"city":"Se`},
		}}},
		canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
			{Index: 0, Name: "ther"},
		}}},
		canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
			{Index: 0, Arguments: `oul"}`},
		}}},
		stopEvent(canonical.StopToolUse),
	)
	names := eventNames(body)
	var argsDeltas int
	for _, n := range names {
		if n == "response.function_call_arguments.delta" {
			argsDeltas++
		}
	}
	if argsDeltas != 2 {
		t.Errorf("%d arguments.delta, want 2 (one per argument fragment)", argsDeltas)
	}
	if !strings.Contains(body, `"call_id":"call_1"`) {
		t.Error("the call's own id, never a minted one")
	}
	if !strings.Contains(body, `"name":"get_weather"`) {
		t.Error("split name fragments must merge: get_wea + ther")
	}
	if !strings.Contains(body, `"arguments":"{\"city\":\"Seoul\"}"`) {
		t.Error("assembled arguments on the closing item")
	}
	if !strings.Contains(body, `"function_call"`) {
		t.Error("a function_call item expected")
	}
	if w.ID() != "resp_test" {
		t.Errorf("id = %q", w.ID())
	}
}

// Reasoning deltas stream as reasoning_summary_text.delta and the item closes
// with a summary part — never as the answer's text.
func TestResponsesWriterRendersReasoningAsAReasoningItem(t *testing.T) {
	body, _ := feed(t, startEvent(), thinkEvent("pondering"), textEvent("answer"), stopEvent(canonical.StopEndTurn))
	if !strings.Contains(body, "response.reasoning_summary_text.delta") {
		t.Error("reasoning delta event expected")
	}
	if !strings.Contains(body, `"summary_text"`) {
		t.Error("the reasoning item closes with a summary_text part")
	}
	// The reasoning text must not appear as an output_text delta of the answer.
	if strings.Count(body, `"delta":"pondering"`) != 1 {
		t.Errorf("reasoning text should ride exactly one delta event")
	}
}

// A refusal streams under the message item and lands as a refusal part.
func TestResponsesWriterStreamsARefusal(t *testing.T) {
	body, _ := feed(t, startEvent(),
		canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{Refusal: "cannot"}},
		stopEvent(canonical.StopRefusal))
	if !strings.Contains(body, "response.refusal.delta") {
		t.Error("refusal delta event expected")
	}
	if !strings.Contains(body, `"type":"refusal"`) {
		t.Error("the refusal part on the closing item")
	}
}

// Usage that arrives after the stop still lands in the terminal — the held
// completed event is what late counts report through (COMPATIBILITY 3.4).
func TestResponsesWriterHoldsTheTerminalForLateUsage(t *testing.T) {
	body, _ := feed(t, startEvent(), textEvent("x"), stopEvent(canonical.StopEndTurn), usageEvent(7, 11))
	if !strings.Contains(body, `"input_tokens":7`) || !strings.Contains(body, `"output_tokens":11`) {
		t.Error("late usage must land inside response.completed's response object")
	}
	if strings.Count(body, `"input_tokens":7`) != 1 {
		t.Error("usage appears once")
	}
}

// A failure is response.failed, exactly once, and nothing follows it.
func TestResponsesWriterFailsInBandAndStops(t *testing.T) {
	var sb strings.Builder
	w := NewResponsesStreamWriter(&sb, ResponsesStreamConfig{Echo: testEcho(), Now: fixedNow})
	_ = w.WriteEvent(startEvent())
	_ = w.WriteEvent(textEvent("partial"))
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventError, Err: &canonical.Error{
		StatusCode: 502, Type: "api_error", Message: "the upstream stream could not be read", Code: "upstream_decode",
	}})
	if err := w.Close(); err != nil {
		t.Fatalf("close after fail: %v", err)
	}
	body := sb.String()
	// The event line and the payload's own `type` both carry the name, so the
	// count is of framed events, not substring hits.
	if c := strings.Count(body, "event: response.failed"); c != 1 {
		t.Fatalf("%d response.failed frames, want exactly 1", c)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Error("a completed after a failed tells the client the generation finished, which it did not")
	}
	if !strings.Contains(body, `"status":"failed"`) {
		t.Error("the failed response object must say so")
	}
	if !strings.Contains(body, `"code":"upstream_decode"`) {
		t.Error("the neutral error's code must ride the error member")
	}
	// The partial output survives in the failed object: the client saw those
	// deltas and the object should not pretend they never existed.
	if !strings.Contains(body, `"partial"`) {
		t.Error("assembled partial output expected on the failed object")
	}
}

// A missing stop is synthesized — tool_use when a call was seen, end_turn
// otherwise — the mirror of COMPATIBILITY 4.4.
func TestResponsesWriterSynthesizesTheStopReason(t *testing.T) {
	_, w := feed(t, startEvent(), textEvent("x"),
		canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
			{Index: 0, ID: "c", Name: "f", Arguments: `{}`},
		}}})
	// No stop event was fed; the completed object's output carries the call,
	// which is what maps to the tool-call stop on the wire.
	if !w.Emitter().sawTool {
		t.Fatal("tool call not observed")
	}
}

// A tool call whose arguments never became JSON fails the stream in band and
// Close reports it, so the exchange is counted failed rather than clean.
func TestResponsesWriterRefusesMalformedToolArguments(t *testing.T) {
	var sb strings.Builder
	w := NewResponsesStreamWriter(&sb, ResponsesStreamConfig{Echo: testEcho(), Now: fixedNow})
	_ = w.WriteEvent(startEvent())
	_ = w.WriteEvent(canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
		ToolCalls: []canonical.ToolCallDelta{{Index: 0, ID: "c1", Name: "f", Arguments: `{"a":`}},
	}})
	err := w.Close()
	if !errors.Is(err, ErrMalformedToolArguments) {
		t.Fatalf("Close() = %v, want ErrMalformedToolArguments", err)
	}
	if !strings.Contains(sb.String(), "response.failed") {
		t.Error("the failure frame is the terminal, in band")
	}
	if !strings.Contains(sb.String(), "malformed_tool_arguments") {
		t.Error("the code names the failure")
	}
}

// The streamed terminal and the buffered body of the same exchange are one
// rendering: Completed() must equal what MarshalResponsesResponse produces
// for the equivalent buffered response. Two paths, one encoder, no drift.
func TestResponsesWriterMatchesTheBufferedBody(t *testing.T) {
	body, w := feed(t, startEvent(), textEvent("hel"), textEvent("lo"), stopEvent(canonical.StopEndTurn), usageEvent(2, 3))
	if !strings.Contains(body, "response.completed") {
		t.Fatal("no terminal event")
	}
	buf := &canonical.Response{
		ID: "resp_test", Model: "client-model", Created: fixedNow().Unix(),
		Choices: []canonical.Choice{{
			Message:    canonical.Message{Content: []canonical.Block{canonical.TextBlock("hello")}},
			StopReason: canonical.StopEndTurn,
		}},
		Usage: &canonical.Usage{InputTokens: 2, OutputTokens: 3},
	}
	want, err := MarshalResponsesResponse(buf, testEcho())
	if err != nil {
		t.Fatalf("marshal buffered: %v", err)
	}
	if got := string(w.Completed()); got != string(want) {
		t.Errorf("streamed terminal body:\n%s\nwant buffered body:\n%s", got, want)
	}
}

// The writer's output is the decoder's input: a dorang fed by a dorang
// reassembles its own stream.
func TestResponsesWriterDecoderRoundTrip(t *testing.T) {
	body, _ := feed(t, startEvent(), textEvent("round"), textEvent(" trip"), stopEvent(canonical.StopEndTurn), usageEvent(1, 2))
	evs, err := DecodeResponsesStream([]byte(body), &DecodeOptions{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var text strings.Builder
	var start, stop int
	for _, e := range evs {
		switch e.Type {
		case canonical.EventStart:
			start++
		case canonical.EventStop:
			stop++
		case canonical.EventDelta:
			for _, b := range e.Delta.Content {
				if b.Kind == canonical.KindText {
					text.WriteString(b.Text)
				}
			}
		}
	}
	if text.String() != "round trip" {
		t.Errorf("round-trip text = %q", text.String())
	}
	if start != 1 || stop != 1 {
		t.Errorf("start=%d stop=%d, want 1 and 1", start, stop)
	}
}

// Every frame is "event: <type>\ndata: <json>\n\n" — both lines, the framing
// this family dispatches on.
func TestResponsesWriterEmitsBothFramingLines(t *testing.T) {
	body, _ := feed(t, startEvent(), textEvent("x"), stopEvent(canonical.StopEndTurn))
	frames := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	if len(frames) < 3 {
		t.Fatalf("%d frames, want at least created/delta/completed", len(frames))
	}
	for _, f := range frames {
		lines := strings.Split(f, "\n")
		if len(lines) != 2 {
			t.Fatalf("frame %q has %d lines, want exactly 2 (event: and data:)", f, len(lines))
		}
		if !strings.HasPrefix(lines[0], "event: response.") {
			t.Errorf("frame %q: first line is not an event name", f)
		}
		if !strings.HasPrefix(lines[1], "data: {") {
			t.Errorf("frame %q: second line is not a data payload", f)
		}
	}
}

// An empty upstream still produces a valid created/completed pair: this
// family's SDK treats a stream without a terminal as a transport failure.
func TestResponsesWriterCompletesAnEmptyStream(t *testing.T) {
	body, _ := feed(t, startEvent(), stopEvent(canonical.StopEndTurn))
	if !strings.Contains(body, "response.created") || !strings.Contains(body, "response.completed") {
		t.Error("an empty stream must still open and close")
	}
	if !strings.Contains(body, `"output":[]`) {
		t.Error("the completed object carries an empty output array")
	}
}
