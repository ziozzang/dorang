package openai

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// A real Responses stream, decoded.
//
// The fixture is a verbatim capture from the ChatGPT Codex surface with the
// identifiers scrubbed — the surface that answers 403 on `/v1/chat/completions`
// and REQUIRES `stream: true`, so this decoder is the only way to reach it at
// all. Hand-written events would have proved the decoder consistent with
// somebody's idea of the protocol; a capture proves it consistent with the
// protocol.
func TestARealResponsesStreamDecodes(t *testing.T) {
	body, err := os.ReadFile("testdata/codex_responses_stream.sse")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	evs, err := DecodeResponsesStream(body, &DecodeOptions{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("a stream carrying an answer decoded to nothing")
	}

	var text, thinking strings.Builder
	var start, stop, usage int
	var u *canonical.Usage
	for _, e := range evs {
		switch e.Type {
		case canonical.EventStart:
			start++
		case canonical.EventStop:
			stop++
		case canonical.EventUsage:
			usage++
			u = e.Usage
		case canonical.EventDelta:
			for _, b := range e.Delta.Content {
				switch b.Kind {
				case canonical.KindText:
					text.WriteString(b.Text)
				case canonical.KindThinking:
					thinking.WriteString(b.Text)
				}
			}
		}
	}

	if got := text.String(); got != "yes" {
		t.Errorf("assembled text = %q, want %q", got, "yes")
	}
	if start != 1 {
		t.Errorf("%d start events, want exactly 1", start)
	}
	if stop != 1 {
		t.Errorf("%d stop events, want exactly 1", stop)
	}

	// Usage is its own event AFTER the stop, which is COMPATIBILITY §3.4: a
	// client that stops reading at the stop reason still gets a correct stop,
	// and one that reads on gets the counts.
	if usage != 1 || u == nil {
		t.Fatalf("%d usage events; a stream whose counts never arrive is a request "+
			"nobody can bill", usage)
	}
	if u.InputTokens != 11 || u.OutputTokens != 20 {
		t.Errorf("usage = in %d / out %d, want 11 / 20 as the capture reports",
			u.InputTokens, u.OutputTokens)
	}
	// The reasoning count is the one an operator cannot get any other way: it is
	// billed inside output_tokens and invisible without it.
	if u.ReasoningTokens != 13 {
		t.Errorf("reasoning tokens = %d, want 13", u.ReasoningTokens)
	}
}

// A reasoning item's deltas are thinking, not text.
//
// This is the case that decides whether the decoder is correct or merely
// working. The two arrive as the same event type and are told apart only by the
// item that opened, so a decoder that ignored `output_item.added` would hand a
// caller the model's private reasoning as its answer.
func TestReasoningIsNotDeliveredAsTheAnswer(t *testing.T) {
	const sse = `data: {"type":"response.created","response":{"id":"r","model":"m","created_at":1}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs"}}

data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"weighing it up"}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant","id":"m1"}}

data: {"type":"response.output_text.delta","output_index":1,"delta":"the answer"}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

data: [DONE]

`
	evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var text, thinking strings.Builder
	for _, e := range evs {
		for _, b := range e.Delta.Content {
			switch b.Kind {
			case canonical.KindText:
				text.WriteString(b.Text)
			case canonical.KindThinking:
				thinking.WriteString(b.Text)
			}
		}
	}
	if strings.Contains(text.String(), "weighing") {
		t.Errorf("reasoning reached the caller as the answer: text = %q", text.String())
	}
	if got := text.String(); got != "the answer" {
		t.Errorf("text = %q, want %q", got, "the answer")
	}
	if got := thinking.String(); got != "weighing it up" {
		t.Errorf("thinking = %q, want the reasoning fragment", got)
	}
}

// A tool call's name arrives when the item opens and its arguments stream after.
//
// Both fragments must carry the same index, because that is the only thing
// joining them — COMPATIBILITY §5.1 requires an index on every tool-call delta
// for exactly this reason.
func TestAToolCallIsAssembledFromTwoEvents(t *testing.T) {
	const sse = `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"lookup","call_id":"call_1"}}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":"}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"seoul\"}"}

data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"function_call","name":"lookup"}]}}

`
	evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var name, id, args string
	var stop canonical.StopReason
	for _, e := range evs {
		for _, tc := range e.Delta.ToolCalls {
			if tc.Index != 0 {
				t.Errorf("a tool-call fragment carries index %d; the two halves are joined "+
					"by nothing else", tc.Index)
			}
			if tc.Name != "" {
				name = tc.Name
			}
			if tc.ID != "" {
				id = tc.ID
			}
			args += tc.Arguments
		}
		if e.Type == canonical.EventStop {
			stop = e.Delta.StopReason
		}
	}
	if name != "lookup" || id != "call_1" {
		t.Errorf("tool call identity lost: name=%q id=%q", name, id)
	}
	if args != `{"q":"seoul"}` {
		t.Errorf("arguments = %q, want the two fragments joined", args)
	}
	if stop != canonical.StopToolUse {
		t.Errorf("stop reason = %q, want %q — a turn that ended in a call did not say so",
			stop, canonical.StopToolUse)
	}
}

// An unknown event produces nothing rather than failing the stream.
//
// This surface gains events without a version bump. A decoder that refused an
// unrecognised shape would take a working deployment down on the vendor's
// release schedule; one that guessed would invent content. Silence is the third
// option and the only correct one.
func TestAnUnknownEventIsIgnoredRatherThanFatal(t *testing.T) {
	const sse = `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.something.nobody.has.seen","output_index":9,"delta":"?"}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}

data: {"type":"response.output_text.delta","output_index":0,"delta":"ok"}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`
	evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
	if err != nil {
		t.Fatalf("an unknown event failed the whole stream: %v", err)
	}
	var text strings.Builder
	for _, e := range evs {
		for _, b := range e.Delta.Content {
			if b.Kind == canonical.KindText {
				text.WriteString(b.Text)
			}
		}
	}
	if got := text.String(); got != "ok" {
		t.Errorf("text = %q; the unknown event either broke the stream or leaked into it", got)
	}
}

// `response.incomplete` is a ceiling, not a completion.
//
// A caller that reads `end_turn` for a truncated answer believes the model
// finished, which is the §10.5a objection in its smallest form.
func TestAnIncompleteResponseReportsTheCeiling(t *testing.T) {
	const sse = `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete"}}

`
	evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range evs {
		if e.Type == canonical.EventStop && e.Delta.StopReason != canonical.StopMaxTokens {
			t.Errorf("stop reason = %q for an incomplete response, want %q",
				e.Delta.StopReason, canonical.StopMaxTokens)
		}
	}
}

// Server-side tool spend reaches the ledger, on both paths.
//
// `tool_usage` is a SIBLING of `usage`, not a member, so no amount of digging in
// the usage object finds it — and it is the only place a server-side web search
// or image generation is priced. `web_search.num_requests` is billed per
// request; `image_gen` carries its own token counts at a different rate from
// chat tokens. Dropping it means a bill nobody can check against a ledger.
//
// The two paths are asserted together on purpose. The buffered decoder kept
// UsageExtra and the streaming one had nowhere to put it, so a caller who asked
// for a stream lost every unmodelled count and a caller who did not kept them:
// two identical requests, two different ledgers, decided by whether the answer
// was wanted incrementally.
func TestServerSideToolSpendSurvivesBothPaths(t *testing.T) {
	const toolUsage = `{"web_search":{"num_requests":3},"image_gen":{"output_tokens":120}}`

	t.Run("stream", func(t *testing.T) {
		sse := `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.completed","response":{"id":"r","status":"completed",` +
			`"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":7}},` +
			`"tool_usage":` + toolUsage + `}}

`
		evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		var extra *canonical.UsageExtra
		for _, e := range evs {
			if e.Type == canonical.EventUsage {
				extra = e.UsageExtra
			}
		}
		if extra == nil {
			t.Fatal("the usage event carries no extras; server-side tool spend left with the stream")
		}
		if _, ok := extra.ToolUsage["web_search"]; !ok {
			t.Errorf("web_search spend is missing: %v", extra.ToolUsage)
		}
		if _, ok := extra.ToolUsage["image_gen"]; !ok {
			t.Errorf("image_gen spend is missing: %v", extra.ToolUsage)
		}
		// cache_write_tokens is not a modelled counter and is a real charge.
		if _, ok := extra.PromptDetails["cache_write_tokens"]; !ok {
			t.Errorf("cache_write_tokens did not survive: %v", extra.PromptDetails)
		}
	})

	t.Run("buffered", func(t *testing.T) {
		body := `{"id":"r","object":"response","status":"completed","model":"m",` +
			`"output":[],"usage":{"input_tokens":10,"output_tokens":5,` +
			`"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":7}},` +
			`"tool_usage":` + toolUsage + `}`
		var w ResponsesResponse
		if err := w.UnmarshalJSON([]byte(body)); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := w.Extra["tool_usage"]; !ok {
			t.Fatal("tool_usage was dropped by the response decoder; it is not a member of " +
				"usage, so nothing else would have caught it")
		}
		got := responsesUsageExtra(w.Usage, toolUsageOf(&w))
		if got == nil || len(got.ToolUsage) == 0 {
			t.Fatalf("buffered path reports no tool spend: %+v", got)
		}
		if _, ok := got.PromptDetails["cache_write_tokens"]; !ok {
			t.Errorf("cache_write_tokens did not survive the buffered path: %v", got.PromptDetails)
		}
	})
}

// A `data:` payload that is not JSON is an error, not a skipped frame.
//
// The two cases look alike and are opposites. An event whose TYPE this build
// does not know is the vendor extending an open set, and ignoring it invents
// nothing. A payload that does not parse is corruption, and skipping it would
// drop a text delta and hand on the rest as a whole answer — which is exactly
// the case below: the corrupted frame is the middle of the text.
func TestAMalformedFrameIsAnErrorNotAHole(t *testing.T) {
	const sse = `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}

data: {"type":"response.output_text.delta","output_index":0,"delta":"the "}

data: {"type":"response.output_text.delta","output_index":0,"delta":"mid

data: {"type":"response.output_text.delta","output_index":0,"delta":"dle"}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`
	_, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
	if err == nil {
		t.Fatal("a frame that is not JSON was skipped, and the answer went on without the " +
			"delta it carried")
	}
}

// One event's `data:` lines are one payload.
//
// The SSE rule: every `data:` line up to the dispatching blank line, joined by
// newlines. This vendor never splits a payload and a proxy re-wrapping the
// stream may; a decoder reading each line as its own frame would find two
// halves of one document and parse neither.
func TestAMultiLineDataFieldIsOneFrame(t *testing.T) {
	const sse = "data: {\"type\":\"response.created\",\n" +
		"data: \"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"whole\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
	if err != nil {
		t.Fatalf("a payload split across two data lines did not decode: %v", err)
	}
	var text strings.Builder
	var start bool
	for _, e := range evs {
		if e.Type == canonical.EventStart {
			start = true
		}
		for _, b := range e.Delta.Content {
			text.WriteString(b.Text)
		}
	}
	if !start || text.String() != "whole" {
		t.Errorf("start=%t text=%q; the split frame was not reassembled", start, text.String())
	}
}

// A byte-order mark does not hide the first event.
func TestALeadingBOMDoesNotHideTheFirstEvent(t *testing.T) {
	sse := "\xEF\xBB\xBFdata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"bom-model\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(evs) == 0 || evs[0].Type != canonical.EventStart || evs[0].Model != "bom-model" {
		t.Errorf("the first event was lost behind the BOM: %+v", evs)
	}
}

// `response.failed` is an error the caller sees, not a stop.
//
// The generation did not finish and the upstream said why, inside
// `response.error`. Rendering that as a stop reason hands the caller a clean
// end of turn over an answer that was never produced — and, through the relay,
// records no failure anywhere.
func TestAFailedResponseIsAnErrorNotAStop(t *testing.T) {
	const sse = `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"lookup","call_id":"c"}}

data: {"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"server_error","message":"the model crashed"},"output":[{"type":"function_call","name":"lookup"}]}}

`
	evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var errEv *canonical.StreamEvent
	for i := range evs {
		switch evs[i].Type {
		case canonical.EventStop:
			t.Errorf("a failed response produced a stop with reason %q; a client reading it "+
				"believes the turn ended", evs[i].Delta.StopReason)
		case canonical.EventError:
			errEv = &evs[i]
		}
	}
	if errEv == nil || errEv.Err == nil {
		t.Fatal("no error event for a failed response")
	}
	if errEv.Err.Message != "the model crashed" || errEv.Err.Code != "server_error" {
		t.Errorf("the upstream's reason was lost: %+v", errEv.Err)
	}
}

// A ceiling that cut off a tool call is a ceiling.
//
// `response.incomplete` with a function call in the partial output used to be
// reported as tool_use, because the call was there — but a call the ceiling
// interrupted is not a turn that ended in a call, and a client acting on it
// would run a tool the model had not finished asking for.
func TestAnIncompleteToolCallIsNotAToolUseStop(t *testing.T) {
	const sse = `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete","output":[{"type":"function_call","name":"lookup"}]}}

`
	evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var stops int
	for _, e := range evs {
		if e.Type == canonical.EventStop {
			stops++
			if e.Delta.StopReason != canonical.StopMaxTokens {
				t.Errorf("stop reason = %q for an incomplete response carrying a call, want %q",
					e.Delta.StopReason, canonical.StopMaxTokens)
			}
		}
	}
	if stops != 1 {
		t.Errorf("%d stop events for an incomplete response, want exactly 1", stops)
	}
}

// A tool name shortened for the wire comes back as the caller declared it.
func TestAShortenedToolNameIsRestoredFromTheStream(t *testing.T) {
	long := strings.Repeat("very_long_function_name_", 4) // 96 bytes, over the limit
	names := NewToolNames()
	short := names.Shorten(long, nil)
	if short == long || len(short) > MaxToolNameLen {
		t.Fatalf("Shorten(%d bytes) = %q", len(long), short)
	}
	sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"name\":\"" + short + "\",\"call_id\":\"c\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{ToolNames: names})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got string
	for _, e := range evs {
		for _, tc := range e.Delta.ToolCalls {
			if tc.Name != "" {
				got = tc.Name
			}
		}
	}
	if got != long {
		t.Errorf("tool name on the neutral stream = %q, want the caller's %d-byte name restored", got, len(long))
	}
}

// One event cannot grow without bound.
//
// The scanner bounds a LINE. An upstream that sends `data:` lines forever
// without the blank line that dispatches them would otherwise be assembled
// into one payload until the process died — a stream, not a frame, is the
// attacker's unit.
func TestAnUnterminatedEventCannotGrowWithoutBound(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1024) + "\n"
	r := io.MultiReader(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"),
		&repeatReader{s: line, n: (maxResponsesFrameBytes / 1024) + 8})
	d := NewResponsesStreamDecoder(r, &DecodeOptions{})
	var err error
	for {
		_, err = d.Next()
		if err != nil {
			break
		}
	}
	if err == io.EOF || err == nil {
		t.Fatal("an event assembled past the frame limit was not refused")
	}
	if !strings.Contains(err.Error(), "frame limit") {
		t.Errorf("err = %v, want the frame limit named", err)
	}
}

// repeatReader yields s, n times.
type repeatReader struct {
	s    string
	n    int
	rest string
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.rest == "" {
		if r.n == 0 {
			return 0, io.EOF
		}
		r.n--
		r.rest = r.s
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

// An `event:` line inside an unfinished payload does not dispatch it.
//
// The spec permits any field order inside one event, so `data:` then
// `event:` then `data:` is ONE event that dispatches at the blank line. The
// missing-separator compatibility rule (an `event:` line opening the next
// event) applies only when what is pending is already a whole document.
func TestAnEventLineInsideAnUnfinishedPayloadDoesNotDispatch(t *testing.T) {
	const sse = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\n" +
		"event: response.output_text.delta\n" +
		"data: \"output_index\":0,\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	evs, err := DecodeResponsesStream([]byte(sse), &DecodeOptions{})
	if err != nil {
		t.Fatalf("a spec-ordered event was split and refused: %v", err)
	}
	var text strings.Builder
	for _, e := range evs {
		for _, b := range e.Delta.Content {
			text.WriteString(b.Text)
		}
	}
	if text.String() != "hello" {
		t.Errorf("text = %q, want the payload assembled across the event line", text.String())
	}
}
