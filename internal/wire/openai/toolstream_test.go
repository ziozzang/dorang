package openai

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// track runs a sequence of wire fragments through a decoder that has stream
// state, and returns the flattened neutral fragments plus the warnings.
func track(t *testing.T, names *ToolNames, frames ...string) ([]canonical.ToolCallDelta, []Warning) {
	t.Helper()
	var warned []Warning
	warn := WarnFunc(func(w Warning) { warned = append(warned, w) })
	opt := &DecodeOptions{ToolNames: names, Tools: NewToolStream(names, warn), Warn: warn}

	var out []canonical.ToolCallDelta
	for _, f := range frames {
		_, evs, err := DecodeChunk([]byte(f), opt)
		if err != nil {
			t.Fatalf("DecodeChunk(%s): %v", f, err)
		}
		for i := range evs {
			out = append(out, evs[i].Delta.ToolCalls...)
		}
	}
	for _, ev := range opt.Tools.Flush("id", "m", 0) {
		out = append(out, ev.Delta.ToolCalls...)
	}
	return out, warned
}

// chunk builds one streamed frame carrying one tool-call fragment.
func chunk(body string) string {
	return `{"id":"1","object":"chat.completion.chunk","created":1,"model":"m",` +
		`"choices":[{"index":0,"delta":{"tool_calls":[` + body + `]}}]}`
}

// nameOf and argsOf reassemble what a client would.
func nameOf(ds []canonical.ToolCallDelta, index int) string {
	var b strings.Builder
	for _, d := range ds {
		if d.Index == index {
			b.WriteString(d.Name)
		}
	}
	return b.String()
}

func argsOf(ds []canonical.ToolCallDelta, index int) string {
	var b strings.Builder
	for _, d := range ds {
		if d.Index == index {
			b.WriteString(d.Arguments)
		}
	}
	return b.String()
}

func idOf(ds []canonical.ToolCallDelta, index int) string {
	for _, d := range ds {
		if d.Index == index && d.ID != "" {
			return d.ID
		}
	}
	return ""
}

func indexesOf(ds []canonical.ToolCallDelta) map[int]bool {
	out := map[int]bool{}
	for _, d := range ds {
		out[d.Index] = true
	}
	return out
}

// TestFragmentedShortenedNameIsRestored is COMPATIBILITY 5.3 in the one shape
// that actually breaks it.
//
// Restore is an exact map lookup. A shortened name split across two frames
// matches neither half, so a per-fragment restore returns both pieces unchanged
// and the client receives a name it never declared — in two parts.
func TestFragmentedShortenedNameIsRestored(t *testing.T) {
	long := "mcp__github__create_pull_request_review_comment_on_a_specific_line_number"
	names := NewToolNames()
	names.Declare(long)
	short := names.Shorten(long, nil)
	if short == long {
		t.Fatal("fixture name is not over the limit")
	}
	half := len(short) / 2

	got, _ := track(t, names,
		chunk(`{"index":0,"id":"call_1","type":"function","function":{"name":`+quote(short[:half])+`}}`),
		chunk(`{"index":0,"function":{"name":`+quote(short[half:])+`}}`),
		chunk(`{"index":0,"function":{"arguments":"{}"}}`),
	)
	if n := nameOf(got, 0); n != long {
		t.Errorf("name = %q, want %q", n, long)
	}
}

// TestFragmentedNameDisambiguatedByTheDeclaredTools is the "aa versus aaaa"
// case. Two identical fragments are either a backend restating a whole name or a
// backend splitting a longer one, and the bytes alone cannot tell. The set of
// tools the caller declared can, and dorang has it on every request that carries
// tools.
func TestFragmentedNameDisambiguatedByTheDeclaredTools(t *testing.T) {
	t.Run("both declared: the concatenation wins", func(t *testing.T) {
		names := NewToolNames()
		names.Declare("aa")
		names.Declare("aaaa")
		got, _ := track(t, names,
			chunk(`{"index":0,"id":"c","function":{"name":"aa"}}`),
			chunk(`{"index":0,"function":{"name":"aa"}}`),
			chunk(`{"index":0,"function":{"arguments":"{}"}}`),
		)
		if n := nameOf(got, 0); n != "aaaa" {
			t.Errorf("name = %q, want aaaa: both are declared, so the repeat is a fragment", n)
		}
	})
	t.Run("only the short one declared: a restatement", func(t *testing.T) {
		names := NewToolNames()
		names.Declare("get_weather")
		got, warned := track(t, names,
			chunk(`{"index":0,"id":"c","function":{"name":"get_weather"}}`),
			chunk(`{"index":0,"function":{"name":"get_weather","arguments":"{}"}}`),
		)
		if n := nameOf(got, 0); n != "get_weather" {
			t.Errorf("name = %q, want get_weather: the doubled name is not a declared tool", n)
		}
		if !hasWarning(warned, WarnRepeatedToolName) {
			t.Errorf("repeated name metadata must be reported: %+v", warned)
		}
	})
	t.Run("no table: a restatement", func(t *testing.T) {
		got, _ := track(t, nil,
			chunk(`{"index":0,"id":"c","function":{"name":"f"}}`),
			chunk(`{"index":0,"function":{"name":"f","arguments":"{}"}}`),
		)
		if n := nameOf(got, 0); n != "f" {
			t.Errorf("name = %q; with no evidence a repeat is read as a restatement", n)
		}
	})
}

// TestNameSettlesOnArguments. The name is held only until something proves no
// more of it is coming; every shape in this family sends the name before the
// body, so the first argument fragment is that proof.
func TestNameSettlesOnArguments(t *testing.T) {
	got, _ := track(t, nil,
		chunk(`{"index":0,"id":"c","type":"function","function":{"name":"f","arguments":"{\"a\":"}}`),
		chunk(`{"index":0,"function":{"arguments":"1}"}}`),
	)
	if n := nameOf(got, 0); n != "f" {
		t.Errorf("name = %q, want f", n)
	}
	if a := argsOf(got, 0); a != `{"a":1}` {
		t.Errorf("arguments = %q", a)
	}
	if id := idOf(got, 0); id != "c" {
		t.Errorf("id = %q", id)
	}
}

// TestZeroArgumentCallStillCarriesItsName. A call that never sends arguments
// never settles its name during the stream, so the flush at the end is what
// keeps it from vanishing entirely.
func TestZeroArgumentCallStillCarriesItsName(t *testing.T) {
	got, warned := track(t, nil, chunk(`{"index":0,"id":"c","function":{"name":"ping"}}`))
	if n := nameOf(got, 0); n != "ping" {
		t.Errorf("name = %q, want ping: a zero-argument call was dropped", n)
	}
	if hasWarning(warned, WarnToolCallMissingID) {
		t.Errorf("this call has an id: %+v", warned)
	}
}

// TestMissingToolCallIDIsReportedNotInvented.
func TestMissingToolCallIDIsReportedNotInvented(t *testing.T) {
	got, warned := track(t, nil,
		chunk(`{"index":0,"function":{"name":"f","arguments":"{}"}}`),
	)
	if id := idOf(got, 0); id != "" {
		t.Errorf("id = %q; dorang must not mint one", id)
	}
	if !hasWarning(warned, WarnToolCallMissingID) {
		t.Errorf("a call with no id must be reported: %+v", warned)
	}
}

// TestAbsentIndexDoesNotMergeParallelCalls. A backend that omits index decodes
// as zero for every call. Two calls at index zero merge into one whose arguments
// are two JSON documents end to end, and nothing anywhere says so — which is
// exactly the failure a client cannot diagnose.
func TestAbsentIndexDoesNotMergeParallelCalls(t *testing.T) {
	got, warned := track(t, nil,
		chunk(`{"id":"a","function":{"name":"f","arguments":"{\"x\":1}"}}`),
		chunk(`{"id":"b","function":{"name":"g","arguments":"{\"y\":2}"}}`),
	)
	if len(indexesOf(got)) != 2 {
		t.Fatalf("the two calls were merged onto one index: %+v", got)
	}
	if !hasWarning(warned, WarnToolCallIndexReused) {
		t.Errorf("the reused index must be reported: %+v", warned)
	}
	if a := argsOf(got, 0); a != `{"x":1}` {
		t.Errorf("call 0 arguments = %q", a)
	}
	if a := argsOf(got, 1); a != `{"y":2}` {
		t.Errorf("call 1 arguments = %q", a)
	}
}

// TestNegativeIndexGetsItsOwnSlot.
func TestNegativeIndexGetsItsOwnSlot(t *testing.T) {
	got, warned := track(t, nil,
		chunk(`{"index":-1,"id":"a","function":{"name":"f","arguments":"{}"}}`),
		chunk(`{"index":0,"id":"b","function":{"name":"g","arguments":"{}"}}`),
	)
	if len(indexesOf(got)) != 2 {
		t.Fatalf("a negative index collided with index 0: %+v", got)
	}
	for i := range got {
		if got[i].Index < 0 {
			t.Errorf("a negative index reached the client: %+v", got[i])
		}
	}
	if !hasWarning(warned, WarnToolCallIndexInvalid) {
		t.Errorf("a negative index must be reported: %+v", warned)
	}
}

// TestNonFunctionToolTypeIsForwarded. dorang does not model other tool types and
// does not rewrite them either: the type goes through and the condition is
// reported, because a gateway that silently retypes a call has changed what the
// model asked for.
func TestNonFunctionToolTypeIsForwarded(t *testing.T) {
	got, warned := track(t, nil,
		chunk(`{"index":0,"id":"a","type":"custom","function":{"name":"shell","arguments":"{}"}}`),
	)
	typ := ""
	for _, d := range got {
		if d.Type != "" {
			typ = d.Type
		}
	}
	if typ != "custom" {
		t.Errorf("type = %q, want custom", typ)
	}
	if !hasWarning(warned, WarnNonFunctionToolCall) {
		t.Errorf("a non-function tool call must be reported: %+v", warned)
	}
}

// TestArgumentsBeforeName: the name still reaches the client, at the flush.
func TestArgumentsBeforeNameOnTheDecodeSide(t *testing.T) {
	got, _ := track(t, nil,
		chunk(`{"index":0,"id":"a","function":{"arguments":"{\"x\":1}"}}`),
		chunk(`{"index":0,"function":{"name":"late"}}`),
	)
	if n := nameOf(got, 0); n != "late" {
		t.Errorf("name = %q, want late", n)
	}
	if a := argsOf(got, 0); a != `{"x":1}` {
		t.Errorf("arguments = %q", a)
	}
}

// TestNameOnlyFrameProducesNoEvent. Forwarding {"tool_calls":[{"index":0}]} is a
// frame that says nothing, and some clients count it as a call.
func TestNameOnlyFrameProducesNoEvent(t *testing.T) {
	names := NewToolNames()
	opt := &DecodeOptions{ToolNames: names, Tools: NewToolStream(names, nil)}
	_, evs, err := DecodeChunk([]byte(chunk(`{"index":0,"function":{"name":"f"}}`)), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Errorf("a name-only fragment produced %d events: %+v", len(evs), evs)
	}
}

func quote(s string) string {
	b, err := Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// The writing side
// ---------------------------------------------------------------------------

// TestTerminalChunkIsNotUpgradedOverBrokenArguments is finding 4's decision for
// a chat-completions client.
//
// COMPATIBILITY 4.4 upgrades the synthesized terminal to tool_calls the moment a
// call is seen, which is right — and over arguments that stop mid-document it
// tells the client a call is ready that it cannot execute. dorang does not run
// tools and cannot complete the JSON, so it reports the condition where the
// client is still listening rather than dressing it up as a finished turn.
func TestTerminalChunkIsNotUpgradedOverBrokenArguments(t *testing.T) {
	var warned []Warning
	var buf bytes.Buffer
	s := newTestWriter(&buf, func(c *StreamConfig) {
		c.Warn = func(w Warning) { warned = append(warned, w) }
	})
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
		Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
			{Index: 0, ID: "call_1", Name: "search", Arguments: `{"q":`},
		}}})
	err := s.Close()
	if err != ErrMalformedToolArguments {
		t.Fatalf("Close = %v, want ErrMalformedToolArguments", err)
	}
	got := buf.String()
	if strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Errorf("the terminal chunk claimed a broken call was ready:\n%s", got)
	}
	if !strings.Contains(got, `"error"`) {
		t.Errorf("no in-band error frame (1.3):\n%s", got)
	}
	if !strings.HasSuffix(got, DoneFrame) {
		t.Errorf("the stream was not terminated:\n%s", got)
	}
	if !hasWarning(warned, WarnMalformedToolArguments) {
		t.Errorf("the condition must be counted: %+v", warned)
	}
}

// TestCompleteArgumentsStillUpgradeTheTerminal is the other half: the check must
// not fire on a call that is fine, and must not fire on a zero-argument call
// either — no argument fragments at all means {}, not a broken document.
func TestCompleteArgumentsStillUpgradeTheTerminal(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"complete", `{"q":"x"}`},
		{"zero-argument", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := newTestWriter(&buf)
			mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
				Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{
					{Index: 0, ID: "call_1", Name: "search", Arguments: tc.args},
				}}})
			if err := s.Close(); err != nil {
				t.Fatalf("Close = %v", err)
			}
			if !strings.Contains(buf.String(), `"finish_reason":"tool_calls"`) {
				t.Errorf("terminal was not upgraded (4.4):\n%s", buf.String())
			}
		})
	}
}

// TestSemanticDataAfterFinishReasonIsForwardedAndReported. Out of contract, and
// still forwarded: what the model produced is not dorang's to discard.
func TestSemanticDataAfterFinishReasonIsForwardedAndReported(t *testing.T) {
	var warned []Warning
	var buf bytes.Buffer
	s := newTestWriter(&buf, func(c *StreamConfig) {
		c.Warn = func(w Warning) { warned = append(warned, w) }
	})
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventStop,
		Delta: canonical.Delta{StopReason: canonical.StopEndTurn}})
	mustWrite(t, s, canonical.StreamEvent{Type: canonical.EventDelta,
		Delta: canonical.Delta{Content: []canonical.Block{canonical.TextBlock("late")}}})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "late") {
		t.Errorf("content after the finish was dropped:\n%s", buf.String())
	}
	if !hasWarning(warned, WarnDataAfterFinish) {
		t.Errorf("the condition must be reported: %+v", warned)
	}
}
