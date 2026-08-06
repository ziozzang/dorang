package openai

import (
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
