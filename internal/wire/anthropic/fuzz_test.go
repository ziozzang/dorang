package anthropic

import (
	"bytes"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// FuzzEmitter drives the content-block state machine of COMPATIBILITY 6.6 with
// arbitrary event sequences.
//
// 6.6 says this state machine "gets its own fuzz target", and the reason is
// that its correctness is a property of ORDER, not of any single frame: a
// backend can interleave tool calls, restate usage, finish twice, emit content
// after finishing, or emit nothing at all, and every one of those has to come
// out as a well-formed message. The invariants below are what "well-formed"
// means, and they are asserted on the event stream rather than on the bytes so
// that a failure names the rule it broke.
func FuzzEmitter(f *testing.F) {
	f.Add([]byte{opText, opToolStart0, opToolArgs0, opStopEndTurn}, uint8(0))
	f.Add([]byte{opThinking, opSignature, opText, opStopFiltered, opUsage}, uint8(0))
	f.Add([]byte{opStopEndTurn, opText, opUsage}, uint8(1))
	f.Add([]byte{opToolStart0, opToolStart1, opToolArgs0, opToolArgs1}, uint8(0))
	f.Add([]byte{}, uint8(0))
	f.Add([]byte{opStart, opEmptyText, opUsage}, uint8(1))

	f.Fuzz(func(t *testing.T, ops []byte, mode uint8) {
		cfg := StreamConfig{ID: "msg_fuzz", Model: "claude-x"}
		if mode&1 == 1 {
			cfg.StopReason = StopReasonNative
		}
		em := NewEmitter(cfg)

		var events []Event
		for _, op := range ops {
			events = em.Push(events, opEvent(op))
		}
		events = em.Finish(events)

		checkStreamInvariants(t, events)

		// Whatever the machine produced must also frame and parse: an event that
		// cannot be marshaled would abort a live stream mid-response.
		var buf bytes.Buffer
		if err := WriteEvents(&buf, events); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
		if _, err := DecodeStream(buf.Bytes(), nil); err != nil {
			t.Fatalf("this package cannot read what it wrote: %v\n%s", err, buf.String())
		}
	})
}

// Fuzz opcodes. They are a small alphabet on purpose: the interesting failures
// are in the ORDER of a handful of transitions, not in the variety of payloads.
const (
	opText byte = iota
	opEmptyText
	opThinking
	opSignature
	opToolStart0
	opToolArgs0
	opToolStart1
	opToolArgs1
	opStopEndTurn
	opStopFiltered
	opUsage
	opStart
	opRefusal
	opUnknownBlock
	opCount
)

func opEvent(op byte) canonical.StreamEvent {
	switch op % opCount {
	case opText:
		return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			Content: []canonical.Block{canonical.TextBlock("a")}}}
	case opEmptyText:
		return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			Content: []canonical.Block{canonical.TextBlock("")}}}
	case opThinking:
		return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			Content: []canonical.Block{canonical.ThinkingBlock("t", "")}}}
	case opSignature:
		return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			Content: []canonical.Block{canonical.ThinkingBlock("", "sig")}}}
	case opToolStart0:
		return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			ToolCalls: []canonical.ToolCallDelta{{Index: 0, ID: "t0", Name: "f"}}}}
	case opToolArgs0:
		return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			ToolCalls: []canonical.ToolCallDelta{{Index: 0, Arguments: `{"a":1}`}}}}
	case opToolStart1:
		return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			ToolCalls: []canonical.ToolCallDelta{{Index: 1, ID: "t1", Name: "g"}}}}
	case opToolArgs1:
		return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			ToolCalls: []canonical.ToolCallDelta{{Index: 1, Arguments: `{"b":2}`}}}}
	case opStopEndTurn:
		return canonical.StreamEvent{Type: canonical.EventStop, Delta: canonical.Delta{
			StopReason: canonical.StopEndTurn}}
	case opStopFiltered:
		return canonical.StreamEvent{Type: canonical.EventStop, Delta: canonical.Delta{
			StopReason: canonical.StopContentFilter}}
	case opUsage:
		return canonical.StreamEvent{Type: canonical.EventUsage, Usage: &canonical.Usage{
			InputTokens: 100, OutputTokens: 5, CacheReadTokens: 30}}
	case opStart:
		return canonical.StreamEvent{Type: canonical.EventStart, ID: "msg_fuzz", Model: "claude-x"}
	case opRefusal:
		return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{Refusal: "no"}}
	default: // opUnknownBlock
		return canonical.StreamEvent{Type: canonical.EventDelta, Delta: canonical.Delta{
			Content: []canonical.Block{{Kind: canonical.BlockKind("citation"), Text: "c"}}}}
	}
}

// checkStreamInvariants asserts everything COMPATIBILITY 6.2 and 6.6 promise
// about a finished stream.
func checkStreamInvariants(t *testing.T, events []Event) {
	t.Helper()

	starts, stops, deltas := 0, 0, 0
	open := -1
	next := 0
	ended := false

	for i, ev := range events {
		switch ev.Type {
		case EventMessageStart:
			starts++
			if i != 0 {
				t.Fatalf("message_start at position %d, must be first", i)
			}
		case EventContentBlockStart:
			p, ok := ev.Payload.(*ContentBlockStartEvent)
			if !ok {
				t.Fatalf("content_block_start carries %T", ev.Payload)
			}
			if ended {
				t.Fatalf("content_block_start after the terminal message_delta (6.6)")
			}
			if open >= 0 {
				t.Fatalf("content_block_start at index %d while %d is still open: this protocol has one open block at a time", p.Index, open)
			}
			if p.Index != next {
				t.Fatalf("block index %d, want %d: indexes are dense and allocated by the emitter", p.Index, next)
			}
			open = p.Index
			next++
		case EventContentBlockDelta:
			p, ok := ev.Payload.(*ContentBlockDeltaEvent)
			if !ok {
				t.Fatalf("content_block_delta carries %T", ev.Payload)
			}
			if open != p.Index {
				t.Fatalf("delta for block %d but block %d is open", p.Index, open)
			}
			if p.Delta.Type == "" {
				t.Fatalf("delta with no type at block %d", p.Index)
			}
		case EventContentBlockStop:
			p, ok := ev.Payload.(*ContentBlockStopEvent)
			if !ok {
				t.Fatalf("content_block_stop carries %T", ev.Payload)
			}
			if open != p.Index {
				t.Fatalf("content_block_stop for %d but %d is open", p.Index, open)
			}
			open = -1
		case EventMessageDelta:
			deltas++
			if open >= 0 {
				t.Fatalf("block %d still open at the terminal message_delta; 6.6 requires a content_block_stop first", open)
			}
			ended = true
		case EventMessageStop:
			stops++
			if i != len(events)-1 {
				t.Fatalf("message_stop at position %d of %d, must be last", i, len(events))
			}
		case EventError:
			// An in-band failure ends the stream without claiming completion.
			if i != len(events)-1 {
				t.Fatalf("error frame at position %d, must be last", i)
			}
			return
		case EventPing:
			t.Fatal("dorang emits no ping (6.2)")
		default:
			t.Fatalf("unknown event type %q (6.2 lists six)", ev.Type)
		}
	}

	if starts != 1 {
		t.Fatalf("message_start appeared %d times, want exactly 1", starts)
	}
	if stops != 1 {
		t.Fatalf("message_stop appeared %d times, want exactly 1 (6.2)", stops)
	}
	if deltas != 1 {
		t.Fatalf("message_delta appeared %d times, want exactly 1", deltas)
	}
	if open >= 0 {
		t.Fatalf("block %d was never closed", open)
	}
	if n := len(events); n < 3 || events[n-2].Type != EventMessageDelta {
		t.Fatalf("message_delta must immediately precede message_stop: %+v", eventTypes(events))
	}
}

func eventTypes(events []Event) []string {
	out := make([]string, len(events))
	for i := range events {
		out[i] = events[i].Type
	}
	return out
}
