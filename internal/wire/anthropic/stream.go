package anthropic

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
)

// blockKind is what the currently open content block is.
type blockKind uint8

const (
	blockNone blockKind = iota
	blockKindText
	blockKindThinking
	blockKindToolUse
)

// StreamConfig configures an [Emitter] and a [StreamWriter].
type StreamConfig struct {
	// ID is pinned for the whole stream. Empty generates one.
	ID string
	// Model is stamped into message_start. It is the CLIENT-FACING name
	// (DESIGN §7.2), and unlike chat completions this protocol names the model
	// exactly once, so there is nothing to restamp per frame.
	Model string

	// Usage seeds message_start. COMPATIBILITY 6.7: the cache counters start at
	// zero and the final message_delta fills them.
	Usage canonical.Usage

	// StopReason selects 6.4's collapse or this family's own enumeration.
	StopReason StopReasonMode

	Warn WarnFunc
}

func (c *StreamConfig) warn() WarnFunc {
	if c == nil {
		return nil
	}
	return c.Warn
}

// Emitter is the stateful content-block machine of COMPATIBILITY 6.6.
//
// # Why this is the hard part
//
// Chat completions has IMPLICIT block boundaries: a text delta followed by a
// tool-call delta simply changes what the delta contains. This protocol has
// EXPLICIT ones, so the same transition has to become
// content_block_stop(i) -> content_block_start(i+1) — synthesized, because the
// upstream never said it. DESIGN §10.7 calls this the costliest asymmetry in
// the whole table.
//
// Three pieces of state carry it, and COMPATIBILITY 6.6 names all three:
//
//  1. A HELD message_delta. The terminal event cannot go out when the stop
//     arrives, because a block is still open and "a content_block_stop must
//     always precede the terminal message_delta". It is held until [Finish].
//     Holding it is also what makes COMPATIBILITY 3.4's late usage work: usage
//     that arrives after the finish still lands in the event that reports it.
//  2. A CHUNK QUEUE. One neutral event can produce three wire events — close
//     the old block, open the new one, deliver the delta — so [Push] appends to
//     a slice instead of returning one event.
//  3. The block index itself, which is allocated by this machine and never by
//     the upstream, because the upstream has no such concept.
//
// An Emitter is not safe for concurrent use: an SSE stream is a single ordered
// byte sequence and interleaving producers would corrupt the ordering
// regardless of any locking here.
type Emitter struct {
	cfg StreamConfig

	started bool
	stopped bool
	failed  bool

	open      bool
	openIndex int
	openKind  blockKind
	openTool  int
	next      int

	sawTool bool
	// tools remembers each tool call's identity by its neutral index, so a
	// backend that interleaves two calls can have its block reopened with the
	// right id instead of an empty one.
	tools map[int]toolMeta

	haveStop bool
	stop     canonical.StopReason
	native   string

	usage     canonical.Usage
	haveUsage bool
}

type toolMeta struct {
	id   string
	name string
}

// NewEmitter returns an emitter for one stream.
func NewEmitter(cfg StreamConfig) *Emitter {
	if cfg.ID == "" {
		cfg.ID = NewMessageID()
	}
	e := &Emitter{cfg: cfg}
	// The seed is the accumulator's initial value, not a separate field: the
	// prompt count is usually known before the first token and the output count
	// never is, so message_start reports what is known and the held
	// message_delta reports the rest (COMPATIBILITY 6.7).
	if !cfg.Usage.Empty() {
		e.usage = cfg.Usage
		e.haveUsage = true
	}
	return e
}

// ID is the pinned message id.
func (e *Emitter) ID() string { return e.cfg.ID }

// Usage returns the accumulated counts and whether any arrived.
func (e *Emitter) Usage() (canonical.Usage, bool) { return e.usage, e.haveUsage }

// NativeStopReason is the true terminal condition when 6.4's collapse hid it.
//
// On a NON-streaming response it goes in [NativeStopReasonHeader]. On a stream
// it cannot: the value is only known at the terminal message_delta, by which
// time the headers are long gone. DESIGN §10.4 already settles what happens
// then — post-hoc values for a stream reach the caller through the ledger, keyed
// by x-dorang-request-id, and only ride the stream itself when the client opted
// in with x-dorang-usage-events. COMPATIBILITY 6.4 says "reports the true reason
// in x-dorang-native-stop-reason" without noticing that half of its own
// requests cannot carry a header.
func (e *Emitter) NativeStopReason() string { return e.native }

// SawToolCall reports whether any tool call was observed. It is what upgrades a
// synthesized stop reason from end_turn to tool_use — the mirror of
// COMPATIBILITY 4.4, which calls that the highest-value single line in the
// document. A gateway that defaults to end_turn on a tool-call turn breaks
// every agentic client.
func (e *Emitter) SawToolCall() bool { return e.sawTool }

// Push converts one neutral stream event, appending to dst.
func (e *Emitter) Push(dst []Event, ev canonical.StreamEvent) []Event {
	if e.stopped || e.failed {
		return dst
	}
	switch ev.Type {
	case canonical.EventStart:
		if ev.Usage != nil {
			e.recordUsage(*ev.Usage)
		}
		return e.ensureStart(dst, ev.ID, ev.Model)

	case canonical.EventUsage:
		if ev.Usage != nil {
			if e.haveStop {
				// COMPATIBILITY 3.4: some backends send usage AFTER the finish.
				// The held message_delta is exactly what lets it still be
				// reported instead of arriving to a closed accumulator.
				e.cfg.warn().warn(WarnLateUsage, "")
			}
			e.recordUsage(*ev.Usage)
		}
		return dst

	case canonical.EventStop:
		dst = e.ensureStart(dst, ev.ID, ev.Model)
		if ev.Usage != nil {
			e.recordUsage(*ev.Usage)
		}
		// HELD. Nothing goes on the wire here: the open block has to be closed
		// first, and more content may still arrive.
		e.haveStop = true
		e.stop = ev.Delta.StopReason
		if ev.Delta.NativeStopReason != "" {
			e.native = ev.Delta.NativeStopReason
		}
		return dst

	case canonical.EventError:
		return e.Fail(dst, ErrorFrom(ev.Err))

	default: // canonical.EventDelta
		dst = e.ensureStart(dst, ev.ID, ev.Model)
		return e.pushDelta(dst, &ev.Delta)
	}
}

func (e *Emitter) pushDelta(dst []Event, d *canonical.Delta) []Event {
	for i := range d.Content {
		b := &d.Content[i]
		switch b.Kind {
		case canonical.KindThinking:
			if b.Text != "" {
				dst = e.ensureBlock(dst, blockKindThinking, 0)
				dst = append(dst, Event{Type: EventContentBlockDelta, Payload: &ContentBlockDeltaEvent{
					Type:  EventContentBlockDelta,
					Index: e.openIndex,
					Delta: BlockDelta{Type: DeltaThinking, Thinking: b.Text},
				}})
			}
			if b.Thinking != nil && b.Thinking.Signature != "" {
				// Integrity material, forwarded byte-identically. dorang never
				// mints one (DESIGN §10.2).
				dst = e.ensureBlock(dst, blockKindThinking, 0)
				dst = append(dst, Event{Type: EventContentBlockDelta, Payload: &ContentBlockDeltaEvent{
					Type:  EventContentBlockDelta,
					Index: e.openIndex,
					Delta: BlockDelta{Type: DeltaSignature, Signature: b.Thinking.Signature},
				}})
			}
		case canonical.KindText:
			if b.Text == "" {
				continue
			}
			dst = e.ensureBlock(dst, blockKindText, 0)
			dst = append(dst, Event{Type: EventContentBlockDelta, Payload: &ContentBlockDeltaEvent{
				Type:  EventContentBlockDelta,
				Index: e.openIndex,
				Delta: BlockDelta{Type: DeltaText, Text: b.Text},
			}})
		default:
			// Every other block kind arrives complete rather than incremental in
			// a stream; there is no delta shape for it here. Its text, if any,
			// is carried as text rather than dropped.
			if b.Text == "" {
				continue
			}
			dst = e.ensureBlock(dst, blockKindText, 0)
			dst = append(dst, Event{Type: EventContentBlockDelta, Payload: &ContentBlockDeltaEvent{
				Type:  EventContentBlockDelta,
				Index: e.openIndex,
				Delta: BlockDelta{Type: DeltaText, Text: b.Text},
			}})
		}
	}

	if d.Refusal != "" {
		// No refusal block exists here. Dropping the text would leave the client
		// with an empty turn; the stop reason carries the fact that it was a
		// refusal, and 6.4 moves that to a header.
		dst = e.ensureBlock(dst, blockKindText, 0)
		dst = append(dst, Event{Type: EventContentBlockDelta, Payload: &ContentBlockDeltaEvent{
			Type:  EventContentBlockDelta,
			Index: e.openIndex,
			Delta: BlockDelta{Type: DeltaText, Text: d.Refusal},
		}})
	}

	for i := range d.ToolCalls {
		tc := &d.ToolCalls[i]
		e.sawTool = true
		if tc.ID != "" || tc.Name != "" {
			if e.tools == nil {
				e.tools = make(map[int]toolMeta, 2)
			}
			m := e.tools[tc.Index]
			if tc.ID != "" {
				m.id = tc.ID
			}
			if tc.Name != "" {
				m.name = tc.Name
			}
			e.tools[tc.Index] = m
		}
		dst = e.ensureBlock(dst, blockKindToolUse, tc.Index)
		if tc.Arguments != "" {
			dst = append(dst, Event{Type: EventContentBlockDelta, Payload: &ContentBlockDeltaEvent{
				Type:  EventContentBlockDelta,
				Index: e.openIndex,
				Delta: BlockDelta{Type: DeltaInputJSON, PartialJSON: tc.Arguments},
			}})
		}
	}
	return dst
}

// ensureStart emits message_start exactly once.
func (e *Emitter) ensureStart(dst []Event, id, model string) []Event {
	if e.started {
		return dst
	}
	e.started = true
	if id != "" && e.cfg.ID == "" {
		e.cfg.ID = id
	}
	if e.cfg.Model == "" {
		e.cfg.Model = model
	}
	msg := Response{
		ID:      e.cfg.ID,
		Type:    TypeMessage,
		Role:    RoleAssistant,
		Model:   e.cfg.Model,
		Content: []ContentBlock{},
		// stop_reason and stop_sequence are present and null here. They are the
		// two fields COMPATIBILITY 2.1's "omit, never null" rule does NOT cover,
		// because that rule is scoped to chat-completions chunks.
		StopReason:   nil,
		StopSequence: nil,
		Usage:        seedUsage(e.usage),
	}
	return append(dst, Event{Type: EventMessageStart, Payload: &MessageStartEvent{
		Type: EventMessageStart, Message: msg,
	}})
}

// ensureBlock opens the right block, closing the current one first.
//
// This is the synthesis DESIGN §10.7 describes: the boundary did not exist
// upstream and is manufactured here, in the one place that knows what is open.
func (e *Emitter) ensureBlock(dst []Event, kind blockKind, toolIndex int) []Event {
	if e.open {
		if e.openKind == kind && (kind != blockKindToolUse || e.openTool == toolIndex) {
			return dst
		}
		if kind == blockKindToolUse && e.openKind == blockKindToolUse {
			e.cfg.warn().warn(WarnInterleavedToolCalls, strconv.Itoa(toolIndex))
		}
		dst = e.closeBlock(dst)
	}
	e.open = true
	e.openKind = kind
	e.openTool = toolIndex
	e.openIndex = e.next
	e.next++

	var cb ContentBlock
	switch kind {
	case blockKindThinking:
		cb = ContentBlock{Type: BlockThinking, Thinking: ptr("")}
	case blockKindToolUse:
		m := e.tools[toolIndex]
		cb = ContentBlock{Type: BlockToolUse, ID: m.id, Name: m.name, Input: json.RawMessage("{}")}
	default:
		cb = ContentBlock{Type: BlockText, Text: ptr("")}
	}
	return append(dst, Event{Type: EventContentBlockStart, Payload: &ContentBlockStartEvent{
		Type: EventContentBlockStart, Index: e.openIndex, ContentBlock: cb,
	}})
}

func (e *Emitter) closeBlock(dst []Event) []Event {
	if !e.open {
		return dst
	}
	e.open = false
	return append(dst, Event{Type: EventContentBlockStop, Payload: &ContentBlockStopEvent{
		Type: EventContentBlockStop, Index: e.openIndex,
	}})
}

// Finish closes the stream: the open block, then the held message_delta, then
// message_stop — exactly once (COMPATIBILITY 6.2).
//
// An entirely empty upstream still produces a complete, valid message here.
// COMPATIBILITY 1.4's "empty upstream yields an empty body" is a
// chat-completions rule; this protocol's SDK treats a stream that ended without
// message_stop as a transport failure, so an empty body would turn a
// zero-token answer into an exception.
func (e *Emitter) Finish(dst []Event) []Event {
	if e.stopped || e.failed {
		return dst
	}
	dst = e.ensureStart(dst, "", "")
	// 6.6: a content_block_stop must ALWAYS precede the terminal message_delta.
	dst = e.closeBlock(dst)
	e.stopped = true

	stop := e.stop
	if !e.haveStop {
		// The mirror of COMPATIBILITY 4.4. Defaulting to end_turn on a turn that
		// called a tool tells an agentic client the assistant is done talking,
		// and the tool never runs.
		stop = canonical.StopEndTurn
		if e.sawTool {
			stop = canonical.StopToolUse
		}
		e.cfg.warn().warn(WarnSynthesizedStop, string(stop))
	}
	wire, native := StopReasonOf(stop, e.cfg.StopReason)
	if native != "" {
		e.native = native
		e.cfg.warn().warn(WarnCollapsedStopReason, native)
	}
	md := &MessageDeltaEvent{
		Type: EventMessageDelta,
		Delta: MessageDelta{
			StopSequence: StopSequenceValue(stop, "", e.cfg.StopReason),
		},
	}
	if wire != "" {
		md.Delta.StopReason = ptr(wire)
	}
	if e.haveUsage {
		md.Usage = finalUsage(e.usage)
	}
	dst = append(dst, Event{Type: EventMessageDelta, Payload: md})
	return append(dst, Event{Type: EventMessageStop, Payload: &MessageStopEvent{Type: EventMessageStop}})
}

// Fail delivers an error in band (COMPATIBILITY 1.3) and ends the stream.
//
// There is no message_stop after it. Once the first frame is out the status is
// already 200 and cannot change — the boundary DESIGN §7.6 refuses to cross
// with a fallback — so the error frame is the only way the client finds out,
// and following it with message_stop would tell the client the message
// completed normally.
func (e *Emitter) Fail(dst []Event, err *Error) []Event {
	if e.stopped || e.failed {
		return dst
	}
	e.failed = true
	if err == nil {
		err = NewError(500, TypeAPIError, "unknown error")
	}
	return append(dst, Event{Type: EventError, Payload: &ErrorEvent{Type: EventError, Error: *err}})
}

func (e *Emitter) recordUsage(u canonical.Usage) {
	if u.Empty() {
		return
	}
	// Replace rather than add: a backend reporting cumulative usage per chunk
	// would otherwise be summed into nonsense. Incremental backends are handled
	// by the accumulator upstream of this emitter.
	e.usage = u
	e.haveUsage = true
}

// seedUsage renders message_start's usage.
//
// COMPATIBILITY 6.7: the counters start at zero and cache fields appear only
// when greater than zero — so a stream with no cache activity emits neither
// cache key here, and the final message_delta fills in whatever the backend
// eventually reported.
func seedUsage(u canonical.Usage) *Usage {
	w := &Usage{
		InputTokens:  ptr(ExclusiveInputTokens(u)),
		OutputTokens: ptr(0),
	}
	if u.CacheWriteTokens > 0 {
		w.CacheCreationInputTokens = ptr(u.CacheWriteTokens)
	}
	if u.CacheReadTokens > 0 {
		w.CacheReadInputTokens = ptr(u.CacheReadTokens)
	}
	return w
}

// finalUsage renders the terminal message_delta's usage.
//
// output_tokens is always present; everything else appears only when non-zero.
// total_tokens is NEVER here: COMPATIBILITY 6.8 says the streaming and
// non-streaming shapes differ by exactly that field, and reproducing the
// asymmetry is the point.
func finalUsage(u canonical.Usage) *Usage {
	w := &Usage{OutputTokens: ptr(u.OutputTokens)}
	if in := ExclusiveInputTokens(u); in > 0 {
		w.InputTokens = ptr(in)
	}
	if u.CacheWriteTokens > 0 {
		w.CacheCreationInputTokens = ptr(u.CacheWriteTokens)
	}
	if u.CacheReadTokens > 0 {
		w.CacheReadInputTokens = ptr(u.CacheReadTokens)
	}
	return w
}

// ---------------------------------------------------------------------------
// Framing
// ---------------------------------------------------------------------------

// StreamWriter frames [Emitter]'s events onto an io.Writer.
//
// It is not safe for concurrent use, for the same reason [Emitter] is not.
type StreamWriter struct {
	w  io.Writer
	em *Emitter

	buf   bytes.Buffer
	queue []Event

	closed bool
}

// NewStreamWriter returns a writer over w.
func NewStreamWriter(w io.Writer, cfg StreamConfig) *StreamWriter {
	return &StreamWriter{w: w, em: NewEmitter(cfg)}
}

// Emitter exposes the state machine, for callers that need its accounting.
func (s *StreamWriter) Emitter() *Emitter { return s.em }

// ID is the pinned message id.
func (s *StreamWriter) ID() string { return s.em.ID() }

// NativeStopReason is the value for [NativeStopReasonHeader].
func (s *StreamWriter) NativeStopReason() string { return s.em.NativeStopReason() }

// Usage returns the accumulated counts and whether any arrived.
func (s *StreamWriter) Usage() (canonical.Usage, bool) { return s.em.Usage() }

// WriteEvent converts and frames one neutral event.
func (s *StreamWriter) WriteEvent(ev canonical.StreamEvent) error {
	if s.closed {
		return errStreamClosed
	}
	s.queue = s.em.Push(s.queue[:0], ev)
	return s.flush()
}

// WriteError delivers an error in band and closes the stream.
func (s *StreamWriter) WriteError(e *Error) error {
	if s.closed {
		return errStreamClosed
	}
	s.queue = s.em.Fail(s.queue[:0], e)
	if err := s.flush(); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// Close completes the stream.
func (s *StreamWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.queue = s.em.Finish(s.queue[:0])
	return s.flush()
}

func (s *StreamWriter) flush() error {
	for i := range s.queue {
		if err := s.writeEvent(&s.queue[i]); err != nil {
			return err
		}
	}
	s.queue = s.queue[:0]
	return nil
}

// writeEvent emits exactly "event: <type>\ndata: <json>\n\n"
// (COMPATIBILITY 6.1).
//
// BOTH lines. A writer that emits only the data line — which is what a
// chat-completions writer does — produces a stream this family's SDK silently
// discards, because it dispatches on the event name and never looks at the
// payload's own "type" member.
func (s *StreamWriter) writeEvent(ev *Event) error {
	s.buf.Reset()
	s.buf.WriteString("event: ")
	s.buf.WriteString(ev.Type)
	s.buf.WriteString("\ndata: ")
	if err := marshalTo(&s.buf, ev.Payload); err != nil {
		return err
	}
	s.buf.WriteString("\n\n")
	_, err := s.w.Write(s.buf.Bytes())
	return err
}

// WriteEvents frames a slice of events. It exists for tests and for callers
// driving [Emitter] directly.
func WriteEvents(w io.Writer, events []Event) error {
	s := &StreamWriter{w: w}
	for i := range events {
		if err := s.writeEvent(&events[i]); err != nil {
			return err
		}
	}
	return nil
}
