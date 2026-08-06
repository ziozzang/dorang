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
