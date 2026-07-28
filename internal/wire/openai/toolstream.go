package openai

import (
	"strconv"
	"strings"

	"github.com/ziozzang/dorang/internal/canonical"
)

// ToolStream is the per-stream state a chat-completions chunk decoder needs to
// read tool calls correctly. It is what [DecodeOptions.Tools] holds.
//
// # Why a chunk decoder cannot be stateless about tool calls
//
// Three of the four things a tool-call fragment carries survive a stateless
// decode and one does not:
//
//   - The ARGUMENTS are a fragment by contract; the client concatenates them and
//     dorang forwards them untouched.
//   - The ID and TYPE arrive once, complete.
//   - The NAME is a fragment too, and that is the trap. [ToolNames.Restore] is an
//     exact lookup, so a shortened name split across two frames is restored by
//     neither half and the caller receives a name it never declared. Restoring
//     can only happen once the whole name is in hand, which means holding it.
//
// The INDEX is the second trap. COMPATIBILITY 5.1 makes it required, but a
// backend that omits it decodes as zero, and two parallel calls both at index
// zero merge into one call whose arguments are two interleaved JSON documents.
// The id is the identity a client actually matches on, so a second id under a
// live index is a second call and is given an index of its own.
//
// A ToolStream is not safe for concurrent use: it tracks one ordered stream.
type ToolStream struct {
	names *ToolNames
	warn  WarnFunc

	// cur is the live state for a wire (choice, index) pair. A pair is re-bound
	// when a new id appears under it.
	cur map[toolKey]*toolCall
	// order is the live states in first-seen order, so Flush is deterministic.
	order []*toolCall
	// next is the next free downstream index per choice.
	next map[int]int
}

type toolKey struct{ choice, index int }

type toolCall struct {
	choice   int
	outIndex int

	id   string
	typ  string
	name string

	idSent   bool
	typSent  bool
	nameSent bool
	sawArgs  bool
}

// NewToolStream returns a tracker for one streaming exchange. names may be nil,
// in which case nothing is restored and the fragment heuristics run on no
// evidence.
func NewToolStream(names *ToolNames, warn WarnFunc) *ToolStream {
	return &ToolStream{names: names, warn: warn}
}

// Track rewrites the tool-call fragments of one chunk's events in place and
// returns the events still worth forwarding.
//
// Events with no tool calls pass through untouched. An event whose every tool
// fragment was fully absorbed — a frame that carried nothing but a piece of a
// name — is dropped, because forwarding {"tool_calls":[{"index":0}]} is a frame
// that says nothing and that some clients count as a call.
func (t *ToolStream) Track(evs []canonical.StreamEvent) []canonical.StreamEvent {
	if t == nil {
		return evs
	}
	out := evs[:0]
	for i := range evs {
		ev := &evs[i]
		if len(ev.Delta.ToolCalls) == 0 {
			out = append(out, *ev)
			continue
		}
		kept := ev.Delta.ToolCalls[:0]
		for j := range ev.Delta.ToolCalls {
			if d, ok := t.fragment(ev.Choice, &ev.Delta.ToolCalls[j]); ok {
				kept = append(kept, d)
			}
		}
		ev.Delta.ToolCalls = kept
		if ev.Type != canonical.EventDelta || !emptyDelta(&ev.Delta) {
			out = append(out, *ev)
		}
	}
	return out
}

// emptyDelta reports that a delta would carry nothing to a client.
func emptyDelta(d *canonical.Delta) bool {
	return d.Role == "" && len(d.Content) == 0 && len(d.ToolCalls) == 0 &&
		d.Refusal == "" && d.StopReason == "" && d.NativeStopReason == ""
}

// fragment folds one wire fragment into the tracked call and returns what to
// forward.
func (t *ToolStream) fragment(choice int, in *canonical.ToolCallDelta) (canonical.ToolCallDelta, bool) {
	key := toolKey{choice: choice, index: in.Index}
	if t.cur == nil {
		t.cur = make(map[toolKey]*toolCall, 2)
		t.next = make(map[int]int, 1)
	}
	st := t.cur[key]
	if st == nil {
		st = t.open(key)
	} else if in.ID != "" && st.id != "" && in.ID != st.id {
		// A second call arrived under an index the first one still holds. The id
		// is the identity the client matches on, so these are two calls and
		// merging their arguments would produce one call with a body that is two
		// JSON documents end to end.
		t.warn.warn(WarnToolCallIndexReused, in.ID)
		st = t.open(key)
	}

	if in.ID != "" {
		st.id = in.ID
	}
	if in.Type != "" {
		if in.Type != "function" {
			// Newer OpenAI surfaces name other tool types. dorang does not model
			// them and does not rewrite them either: the type is forwarded and the
			// condition is reported.
			t.warn.warn(WarnNonFunctionToolCall, in.Type)
		}
		st.typ = in.Type
	}
	if in.Name != "" {
		if st.nameSent {
			// The name already went downstream; appending now would produce a
			// second, different name for one call.
			t.warn.warn(WarnRepeatedToolName, in.Name)
		} else {
			st.name = t.mergeName(st.name, in.Name)
		}
	}

	out := canonical.ToolCallDelta{Index: st.outIndex}
	any := false
	if st.id != "" && !st.idSent {
		out.ID, st.idSent, any = st.id, true, true
	}
	if st.typ != "" && !st.typSent {
		out.Type, st.typSent, any = st.typ, true, true
	}
	if in.Arguments != "" {
		// Arguments settle the name: every shape in this family sends the name
		// before the body, so a fragment that carries arguments is proof that no
		// more of the name is coming.
		st.sawArgs = true
		out.Arguments, any = in.Arguments, true
		if !st.nameSent && st.name != "" {
			out.Name, st.nameSent = t.names.Restore(st.name), true
		}
	}
	return out, any
}

// open binds a fresh call to a wire index and allocates its downstream index.
func (t *ToolStream) open(key toolKey) *toolCall {
	if key.index < 0 {
		// A negative index addresses no slot in any client's array. It is still a
		// call, so it is given a dense one of its own rather than dropped or
		// folded onto zero. Reported once per call, not once per fragment.
		t.warn.warn(WarnToolCallIndexInvalid, strconv.Itoa(key.index))
	}
	st := &toolCall{choice: key.choice, outIndex: t.next[key.choice]}
	t.next[key.choice]++
	t.cur[key] = st
	t.order = append(t.order, st)
	return st
}

// Flush emits the fragments held back for names that never settled: a call whose
// arguments never arrived, or that arrived before the name did.
//
// It is called once, when the upstream stream ends. Without it a zero-argument
// tool call reaches the client with an id and no name at all.
func (t *ToolStream) Flush(id, model string, created int64) []canonical.StreamEvent {
	if t == nil {
		return nil
	}
	var out []canonical.StreamEvent
	for _, st := range t.order {
		if st.id == "" {
			// dorang does not mint one. The id is what the client echoes back in
			// its tool result and what the backend matches on the next turn; a
			// gateway-invented id is a correlation neither end agreed to.
			t.warn.warn(WarnToolCallMissingID, st.name)
		}
		d := canonical.ToolCallDelta{Index: st.outIndex}
		any := false
		if st.id != "" && !st.idSent {
			d.ID, st.idSent, any = st.id, true, true
		}
		if st.typ != "" && !st.typSent {
			d.Type, st.typSent, any = st.typ, true, true
		}
		if st.name != "" && !st.nameSent {
			d.Name, st.nameSent, any = t.names.Restore(st.name), true, true
		}
		if !any {
			continue
		}
		out = append(out, canonical.StreamEvent{
			Type: canonical.EventDelta, ID: id, Model: model, Created: created,
			Choice: st.choice, Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{d}},
		})
	}
	return out
}

// mergeName folds a name fragment into the name accumulated so far.
//
// COMPATIBILITY does not say whether repeated name metadata is a restatement or
// a fragment, and the two are indistinguishable from the bytes alone: a backend
// that sends "get_weather" twice for one call and a backend that splits "aaaa"
// into "aa" and "aa" produce the same shape. The declared tool set settles it
// whenever it can, which in dorang is every request that carries tools, and the
// no-evidence branch prefers restatement because a backend restating a whole
// name is a shape that exists and a name that fragments into identical halves
// is a shape that has to be constructed.
func (t *ToolStream) mergeName(have, frag string) string {
	if have == "" {
		return frag
	}
	cat := have + frag
	switch {
	case frag == have:
		t.warn.warn(WarnRepeatedToolName, frag)
		if t.names.Known(cat) {
			return cat
		}
		return have
	case t.names.Known(cat):
		return cat
	case t.names.Known(frag) && strings.HasPrefix(frag, have) && !t.names.Known(have):
		// A cumulative restatement: the backend resent the name it had built so
		// far rather than only the new bytes.
		t.warn.warn(WarnRepeatedToolName, frag)
		return frag
	default:
		return cat
	}
}
