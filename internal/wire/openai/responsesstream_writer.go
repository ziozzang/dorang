package openai

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The client-facing half of the Responses event stream.
//
// [ResponsesStreamDecoder] (responsesstream.go) is the upstream half and the
// vocabulary here is deliberately its mirror: the events emitted are exactly
// the ones that decoder consumes, and the ones it ignores — `response.in_
// progress`, the `content_part` boundaries, the part-level `.done` echoes — are
// exactly the ones never written. A relay that forwarded structural
// bookkeeping would be inventing content; a relay that omits it hands the
// client everything the deltas and `output_item.done` already carry. A dorang
// fed by a dorang runs on that omission today, which is the symmetry argument
// in its observable form.
//
// This is a tree announced in order — items open, deltas arrive inside them,
// items close — where the neutral form has only flat fragments, so the writer
// owns the boundaries the same way the Anthropic emitter does for content
// blocks: it allocates the output index, mints the item id, and decides when
// an item closes. Unlike that emitter it may hold SEVERAL items open at once:
// this protocol addresses every delta by item_id and output_index, so a tool
// call streaming beside an open message item is not an interleaving to be
// held — it is two addresses on one stream.

// ResponsesStreamConfig configures a [ResponsesEmitter] and a
// [ResponsesStreamWriter].
type ResponsesStreamConfig struct {
	// Echo carries the request fields this surface echoes on every response
	// object, with the stream's pinned identity already in it: ID is dorang's
	// `resp_…` and Model is the CLIENT-FACING name (DESIGN §7.2). Nil mints an
	// identity instead; that path exists for symmetry with the other writers,
	// not because the backend ever takes it — the dispatcher always has one.
	Echo *ResponsesOptions
	// Now stamps `created_at`, pinned for the whole stream. Nil reads
	// time.Now, which makes the two paths of one conversion disagree about
	// `created` (DESIGN §10.7) — callers pass the backend's clock.
	Now  func() time.Time
	Warn WarnFunc
}

// respWireEvent is one framed event: the SSE name and its payload.
type respWireEvent struct {
	name    string
	payload any
}

// The payload shapes. sequence_number, output_index, content_index and
// summary_index have NO omitempty: zero is a valid value — the first item, the
// first part, the first event — and a field that folds to absence at zero
// breaks every client that indexes on it. `type` leads and sequence_number
// trails, matching the capture the decoder is tested against.
type respCreatedEvent struct {
	Type           string             `json:"type"`
	Response       *ResponsesResponse `json:"response"`
	SequenceNumber int64              `json:"sequence_number"`
}

type respItemEvent struct {
	Type           string       `json:"type"`
	OutputIndex    int          `json:"output_index"`
	Item           ResponseItem `json:"item"`
	SequenceNumber int64        `json:"sequence_number"`
}

type respTextDeltaEvent struct {
	Type           string `json:"type"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Delta          string `json:"delta"`
	SequenceNumber int64  `json:"sequence_number"`
}

type respReasonDeltaEvent struct {
	Type           string `json:"type"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	Delta          string `json:"delta"`
	SequenceNumber int64  `json:"sequence_number"`
}

type respArgsDeltaEvent struct {
	Type           string `json:"type"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	Delta          string `json:"delta"`
	SequenceNumber int64  `json:"sequence_number"`
}

type respTerminalEvent struct {
	Type           string             `json:"type"`
	Response       *ResponsesResponse `json:"response"`
	SequenceNumber int64              `json:"sequence_number"`
}

// respItemKind is what the currently open content item is.
type respItemKind uint8

const (
	respItemNone respItemKind = iota
	respItemMessage
	respItemReasoning
)

// ResponsesEmitter is the stateful item machine of the Responses event stream.
//
// Three pieces of state carry it, the same three COMPATIBILITY 6.6 names for
// the Anthropic machine:
//
//  1. A HELD terminal. The stop cannot go out when it arrives, because items
//     are still open and usage may still follow it (COMPATIBILITY 3.4). The
//     response object `response.completed` carries is assembled at [Finish],
//     from the final output and counts.
//  2. A CHUNK QUEUE. Closing an item, opening the next and delivering a delta
//     is one neutral event producing several wire events, so [Push] appends to
//     a slice rather than returning one event.
//  3. The output indexes and item ids, allocated here and never upstream,
//     because the neutral form has neither concept.
//
// A ResponsesEmitter is not safe for concurrent use: an SSE stream is a single
// ordered byte sequence and interleaving producers would corrupt the ordering
// regardless of any locking here.
type ResponsesEmitter struct {
	cfg  ResponsesStreamConfig
	echo ResponsesOptions
	id   string
	// created is pinned at construction so every event, including the ones the
	// queue had to reorder, reports the same timestamp.
	created int64

	started bool
	stopped bool
	failed  bool

	// seq is the next sequence_number. It starts at zero and climbs by one on
	// every framed event, failure frames included — the counter describes the
	// stream, not the success. Every append to the queue goes through one of
	// the emit sites, and each of them consumes exactly one number, which is
	// the whole of the numbering discipline.
	seq int64
	// nextItem allocates output_index in the order items open, which is the
	// order the completed response's output array carries them in.
	nextItem int
	// itemSeq mints per-stream item ids (msg_0, rs_1, fc_2, …). They correlate
	// a delta with the item.done that closes it and are stream-scoped: the
	// buffered body's items carry none, and inventing durable ones would be a
	// claim about storage this surface does not make.
	itemSeq int

	// The open content item: its kind, its output index, its id, and its
	// assembled text. Tool items track their own, because this protocol
	// addresses them and therefore they need no single-open-holder.
	openKind respItemKind
	openIdx  int
	openID   string
	openText strings.Builder

	tools map[int]*respToolItem
	order []*respToolItem

	// msg accumulates the assistant turn in arrival order — content blocks and
	// tool-use blocks alike — and is what the terminal renders from, so the
	// streamed `response.completed` and a buffered answer of the same exchange
	// are built by one encoder and cannot drift (DESIGN §10.7).
	msg     canonical.Message
	sawTool bool

	haveStop bool
	stop     canonical.StopReason

	usage     canonical.Usage
	haveUsage bool

	// completedBody is the exact bytes of the terminal event's response member,
	// kept for the store: `GET /v1/responses/{id}` serves it verbatim, exactly
	// as the buffered path serves the body it wrote.
	completedBody []byte

	badArgs bool
}

// respToolItem is one function call being streamed.
type respToolItem struct {
	item ResponseItem
	idx  int
	args strings.Builder
	// block is the tool-use entry in the accumulating turn. It joins at first
	// sight — so the completed output keeps arrival order against content
	// items — and its Input is filled at Finish from the assembled arguments.
	block *canonical.ToolUse
}

// NewResponseID generates a Responses id. The shape is this family's own
// (`resp_…`), shared by the buffered and streamed paths for the same reason
// [NewStreamID] is: two generators are how the two paths come to hand a client
// different-looking identifiers for one conversion.
func NewResponseID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a time-derived fallback keeps
		// the stream identifiable rather than panicking mid-response.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * (i % 8)))
		}
	}
	const prefix = "resp_"
	out := make([]byte, len(prefix)+len(b)*2)
	copy(out, prefix)
	for i, v := range b {
		out[len(prefix)+i*2] = hexDigits[v>>4]
		out[len(prefix)+i*2+1] = hexDigits[v&0xf]
	}
	return string(out)
}

// NewResponsesEmitter returns an emitter for one stream.
func NewResponsesEmitter(cfg ResponsesStreamConfig) *ResponsesEmitter {
	e := &ResponsesEmitter{cfg: cfg}
	if cfg.Echo != nil {
		e.echo = *cfg.Echo
	} else {
		e.echo = ResponsesOptions{ID: NewResponseID()}
	}
	e.id = e.echo.ID
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	e.created = now().Unix()
	if e.echo.Created == 0 {
		e.echo.Created = e.created
	}
	return e
}

// ID is the pinned response id.
func (e *ResponsesEmitter) ID() string { return e.id }

// Usage returns the counts observed on the stream.
func (e *ResponsesEmitter) Usage() (canonical.Usage, bool) { return e.usage, e.haveUsage }

// MalformedToolArguments reports that the stream was failed because a tool
// call's arguments never became valid JSON.
func (e *ResponsesEmitter) MalformedToolArguments() bool { return e.badArgs }

// Completed returns the assembled terminal response object, nil until the
// stream completed or failed.
func (e *ResponsesEmitter) Completed() []byte { return e.completedBody }

// Push converts one neutral stream event, appending to dst.
func (e *ResponsesEmitter) Push(dst []respWireEvent, ev canonical.StreamEvent) []respWireEvent {
	if e.stopped || e.failed {
		return dst
	}
	switch ev.Type {
	case canonical.EventStart:
		if ev.Usage != nil {
			e.recordUsage(*ev.Usage)
		}
		return e.ensureCreated(dst)

	case canonical.EventUsage:
		if ev.Usage != nil {
			if e.haveStop {
				// COMPATIBILITY 3.4: some backends send usage AFTER the finish.
				// The held terminal is exactly what still has a place for it.
				e.cfg.Warn.warn(WarnLateUsage, "")
			}
			e.recordUsage(*ev.Usage)
		}
		return dst

	case canonical.EventStop:
		dst = e.ensureCreated(dst)
		if ev.Usage != nil {
			e.recordUsage(*ev.Usage)
		}
		// HELD. The items are still open and the counts may still arrive; the
		// terminal is assembled at Finish, from the final state.
		e.haveStop = true
		e.stop = ev.Delta.StopReason
		return dst

	case canonical.EventError:
		return e.Fail(dst, ErrorFrom(ev.Err))

	default: // canonical.EventDelta
		dst = e.ensureCreated(dst)
		if e.haveStop {
			// Out of contract, and still delivered: the held terminal exists so
			// late content lands in the turn rather than after it.
			e.cfg.Warn.warn(WarnDataAfterFinish, "")
		}
		return e.pushDelta(dst, &ev.Delta)
	}
}

func (e *ResponsesEmitter) pushDelta(dst []respWireEvent, d *canonical.Delta) []respWireEvent {
	for i := range d.Content {
		b := &d.Content[i]
		switch b.Kind {
		case canonical.KindThinking:
			if b.Text == "" {
				continue
			}
			dst = e.ensureContentItem(dst, respItemReasoning)
			dst = append(dst, respWireEvent{name: respEventReasonDelta, payload: &respReasonDeltaEvent{
				Type: respEventReasonDelta, ItemID: e.openID, OutputIndex: e.openIdx,
				Delta: b.Text, SequenceNumber: e.seq,
			}})
			e.seq++
			e.openText.WriteString(b.Text)
			e.foldIntoTurn(canonical.KindThinking, b.Text, thinkingSignature(b))
		default:
			if b.Text == "" {
				continue
			}
			dst = e.ensureContentItem(dst, respItemMessage)
			dst = append(dst, respWireEvent{name: respEventTextDelta, payload: &respTextDeltaEvent{
				Type: respEventTextDelta, ItemID: e.openID, OutputIndex: e.openIdx,
				Delta: b.Text, SequenceNumber: e.seq,
			}})
			e.seq++
			e.openText.WriteString(b.Text)
			e.foldIntoTurn(canonical.KindText, b.Text, "")
		}
	}

	if d.Refusal != "" {
		// This surface has a part for it, unlike the chat one: `refusal.delta`
		// streams under the open message item and the part rides item.done.
		dst = e.ensureContentItem(dst, respItemMessage)
		dst = append(dst, respWireEvent{name: respEventRefusalDelta, payload: &respTextDeltaEvent{
			Type: respEventRefusalDelta, ItemID: e.openID, OutputIndex: e.openIdx,
			Delta: d.Refusal, SequenceNumber: e.seq,
		}})
		e.seq++
		e.msg.Refusal += d.Refusal
	}

	for i := range d.ToolCalls {
		dst = e.pushToolCall(dst, &d.ToolCalls[i])
	}
	return dst
}

// foldIntoTurn appends one content fragment to the accumulating turn, MERGING
// it into the previous block when the kind is unchanged.
//
// The turn is what the terminal renders from, and the buffered renderer puts
// every text block of one message on its own part. A stream that said "hel"
// and "lo" as two deltas would therefore complete with two parts where the
// same exchange, asked for without streaming, completes with one — two paths
// of one conversion disagreeing about the answer (DESIGN §10.7). Fragments of
// one run of text are one block; a kind change ends the run and starts
// another.
func (e *ResponsesEmitter) foldIntoTurn(kind canonical.BlockKind, text, signature string) {
	if n := len(e.msg.Content); n > 0 && e.msg.Content[n-1].Kind == kind {
		e.msg.Content[n-1].Text += text
		if sig := e.msg.Content[n-1].Thinking; sig != nil && signature != "" {
			sig.Signature = signature
		}
		return
	}
	if kind == canonical.KindThinking {
		e.msg.Content = append(e.msg.Content, canonical.ThinkingBlock(text, signature))
		return
	}
	e.msg.Content = append(e.msg.Content, canonical.TextBlock(text))
}

// pushToolCall streams one tool-call fragment under its own item.
//
// Nothing is held. The chat shape's single-open-block constraint does not
// apply here — a delta names its item, so a fragment for call two while call
// one is still streaming is two addresses, not an interleaving. The first
// fragment opens the item with whatever identity it carries (COMPATIBILITY
// 5.1's rule read from this side), and every argument fragment goes out as it
// arrives.
func (e *ResponsesEmitter) pushToolCall(dst []respWireEvent, tc *canonical.ToolCallDelta) []respWireEvent {
	e.sawTool = true
	t, opened := e.toolAt(tc)
	if tc.ID != "" && t.item.CallID == "" {
		t.item.CallID = tc.ID
		t.block.ID = tc.ID
	}
	if tc.Name != "" {
		// APPEND, not assign. A name can arrive split across frames, and an
		// assignment keeps only the last piece — after output_item.added has
		// already gone out carrying the first.
		t.item.Name = mergeToolName(t.item.Name, tc.Name, e.cfg.Warn)
		t.block.Name = t.item.Name
	}
	if opened {
		// The item is announced with whatever identity the first fragment
		// carried — the call_id above all, which is what the client matches
		// on. A name that arrives later is merged before the client needs it:
		// only the done item is executed from.
		dst = append(dst, respWireEvent{name: respEventOutputAdded, payload: &respItemEvent{
			Type: respEventOutputAdded, OutputIndex: t.idx, Item: t.item, SequenceNumber: e.seq,
		}})
		e.seq++
	}
	if tc.Arguments == "" {
		return dst
	}
	dst = append(dst, respWireEvent{name: respEventArgsDelta, payload: &respArgsDeltaEvent{
		Type: respEventArgsDelta, ItemID: t.item.ID, OutputIndex: t.idx,
		Delta: tc.Arguments, SequenceNumber: e.seq,
	}})
	e.seq++
	t.args.WriteString(tc.Arguments)
	return dst
}

// toolAt returns the call bound to a neutral index, opening its item on first
// sight and reporting that it did. The neutral index is this surface's own
// output_index — the decoder uses it that way — so a dorang fed by a dorang
// reassembles its own stream.
func (e *ResponsesEmitter) toolAt(tc *canonical.ToolCallDelta) (*respToolItem, bool) {
	if e.tools == nil {
		e.tools = make(map[int]*respToolItem, 2)
	}
	if t := e.tools[tc.Index]; t != nil {
		return t, false
	}
	t := &respToolItem{
		item: ResponseItem{Type: ItemFunctionCall, ID: "fc_" + strconv.Itoa(e.itemSeq), CallID: tc.ID},
		idx:  e.nextItem,
	}
	e.itemSeq++
	e.nextItem++
	e.tools[tc.Index] = t
	e.order = append(e.order, t)
	t.block = &canonical.ToolUse{ID: tc.ID, Name: tc.Name}
	e.msg.Content = append(e.msg.Content, canonical.Block{Kind: canonical.KindToolUse, ToolUse: t.block})
	if tc.ID == "" {
		// dorang does not mint one. The call_id is what the client sends back
		// in its function_call_output; a gateway-invented id is a correlation
		// neither end agreed to.
		e.cfg.Warn.warn(WarnToolCallMissingID, tc.Name)
	}
	return t, true
}

// ensureContentItem opens the right item, closing the current one first.
//
// The boundary did not exist upstream — the neutral form has no items — and is
// manufactured here, in the one place that knows what is open. Tool items are
// never closed by this: they have their own addresses and close at Finish.
func (e *ResponsesEmitter) ensureContentItem(dst []respWireEvent, kind respItemKind) []respWireEvent {
	if e.openKind == kind && kind != respItemNone {
		return dst
	}
	dst = e.closeContentItem(dst)
	prefix := "msg_"
	if kind == respItemReasoning {
		prefix = "rs_"
	}
	e.openKind = kind
	e.openID = prefix + strconv.Itoa(e.itemSeq)
	e.itemSeq++
	e.openIdx = e.nextItem
	e.nextItem++
	e.openText.Reset()
	var item ResponseItem
	if kind == respItemReasoning {
		item = ResponseItem{Type: ItemReasoning, ID: e.openID, Summary: []ResponsePart{}}
	} else {
		item = ResponseItem{
			Type: ItemMessage, ID: e.openID, Status: StatusInProgress,
			Role:    string(canonical.RoleAssistant),
			Content: &ResponseContent{Parts: []ResponsePart{{Type: PartOutputText, Annotations: emptyAnnotations()}}},
		}
	}
	dst = append(dst, respWireEvent{name: respEventOutputAdded, payload: &respItemEvent{
		Type: respEventOutputAdded, OutputIndex: e.openIdx, Item: item, SequenceNumber: e.seq,
	}})
	e.seq++
	return dst
}

// closeContentItem closes the open content item with its assembled body.
//
// The item rendered here is built by the same encoder the terminal uses —
// outputItems over the item's own accumulated block — so the streamed
// item.done and the completed response's output entry cannot disagree. The
// turn's refusal, if any, rides the message item that is closing, which is
// also where the buffered renderer puts it.
func (e *ResponsesEmitter) closeContentItem(dst []respWireEvent) []respWireEvent {
	if e.openKind == respItemNone {
		return dst
	}
	idx, id, text := e.openIdx, e.openID, e.openText.String()
	kind := e.openKind
	e.openKind = respItemNone
	var block canonical.Block
	if kind == respItemReasoning {
		block = canonical.ThinkingBlock(text, "")
	} else {
		block = canonical.TextBlock(text)
	}
	items := outputItems(&canonical.Message{Content: []canonical.Block{block}, Refusal: e.msg.Refusal}, e.echo.ToolNames)
	var item ResponseItem
	if len(items) > 0 {
		item = items[0]
	}
	item.ID = id
	item.Status = StatusCompleted
	dst = append(dst, respWireEvent{name: respEventOutputDone, payload: &respItemEvent{
		Type: respEventOutputDone, OutputIndex: idx, Item: item, SequenceNumber: e.seq,
	}})
	e.seq++
	return dst
}

// Finish closes the stream: every open item, then the held terminal.
//
// An entirely empty upstream still produces a valid created/completed pair
// here. COMPATIBILITY 1.4's "empty upstream yields an empty body" is a
// chat-completions rule; this family's SDK treats a stream that ended without
// `response.completed` as a transport failure, so an empty body would turn a
// zero-token answer into an exception.
func (e *ResponsesEmitter) Finish(dst []respWireEvent) []respWireEvent {
	if e.stopped || e.failed {
		return dst
	}
	dst = e.ensureCreated(dst)

	// A tool call whose arguments never became a JSON document is the one thing
	// that must not be finished normally: the completed status would tell an
	// agentic client a call is ready, and it is not. dorang cannot repair it —
	// it does not execute tools — so the stream fails in band instead.
	if bad := e.malformedToolCall(); bad != nil {
		e.cfg.Warn.warn(WarnMalformedToolArguments, bad.item.Name)
		e.badArgs = true
		dst = e.closeContentItem(dst)
		e.failed = true
		return e.failWith(dst, malformedResponsesToolError(bad))
	}

	dst = e.closeContentItem(dst)
	dst = e.closeToolItems(dst)
	e.stopped = true

	stop := e.stop
	if !e.haveStop {
		// The mirror of COMPATIBILITY 4.4. Defaulting to end_turn on a turn
		// that called a tool tells an agentic client the assistant is done
		// talking, and the tool never runs.
		stop = canonical.StopEndTurn
		if e.sawTool {
			stop = canonical.StopToolUse
		}
		e.cfg.Warn.warn(WarnSynthesizedTerminal, string(stop))
	}
	resp := &canonical.Response{
		ID:      e.id,
		Model:   e.echo.Model,
		Created: e.created,
		Choices: []canonical.Choice{{Message: e.msg, StopReason: stop}},
	}
	if e.haveUsage {
		u := e.usage
		resp.Usage = &u
	}
	return e.appendTerminal(dst, respEventCompleted, resp)
}

// closeToolItems emits the output_item.done of every call, in first-seen
// order, with its assembled arguments. They were streaming the whole time; the
// done event is the completed item the client executes from.
func (e *ResponsesEmitter) closeToolItems(dst []respWireEvent) []respWireEvent {
	for _, t := range e.order {
		args := t.args.String()
		if args == "" {
			args = "{}"
		}
		t.item.Arguments = args
		t.item.Status = StatusCompleted
		t.block.Input = json.RawMessage(args)
		dst = append(dst, respWireEvent{name: respEventOutputDone, payload: &respItemEvent{
			Type: respEventOutputDone, OutputIndex: t.idx, Item: t.item, SequenceNumber: e.seq,
		}})
		e.seq++
	}
	return dst
}

// Fail delivers the failure in band and ends the stream.
//
// `response.failed` is this family's own terminal failure frame: it carries
// the pinned identity and whatever output had assembled, and — like the
// Anthropic error event — NOTHING follows it. A `response.completed` after it
// would tell the client the generation finished, which is exactly the fact it
// did not.
func (e *ResponsesEmitter) Fail(dst []respWireEvent, err *Error) []respWireEvent {
	if e.stopped || e.failed {
		return dst
	}
	e.failed = true
	if err == nil {
		err = NewError(500, TypeAPIError, "unknown error")
	}
	return e.failWith(dst, err)
}

// failWith renders the failure frame over whatever output exists. Callers set
// e.failed first; the terminal rendering here is shared by the error event and
// the malformed-arguments path.
func (e *ResponsesEmitter) failWith(dst []respWireEvent, err *Error) []respWireEvent {
	resp := &canonical.Response{
		ID:      e.id,
		Model:   e.echo.Model,
		Created: e.created,
		Choices: []canonical.Choice{{Message: e.msg, StopReason: canonical.StopError}},
	}
	return e.appendTerminal(dst, respEventFailed, resp, func(out *ResponsesResponse) {
		out.Status = StatusFailed
		out.Error = errorMember(err)
		out.IncompleteDetails = nil
	})
}

type respTerminalPatch func(*ResponsesResponse)

// appendTerminal assembles the response object, applies the family's own
// patches (the completed mapping is [EncodeResponsesResponse]'s default), and
// frames the terminal event. The assembled object is also kept as
// completedBody for the store.
func (e *ResponsesEmitter) appendTerminal(dst []respWireEvent, name string, resp *canonical.Response, patch ...respTerminalPatch) []respWireEvent {
	out, err := EncodeResponsesResponse(resp, &e.echo)
	if err != nil {
		out = &ResponsesResponse{ID: e.id, Object: ObjectResponse, CreatedAt: e.created, Model: e.echo.Model}
	}
	for _, p := range patch {
		p(out)
	}
	if body, mErr := marshalAppender(out); mErr == nil {
		e.completedBody = body
	}
	return append(dst, respWireEvent{name: name, payload: &respTerminalEvent{
		Type: name, Response: out, SequenceNumber: e.seq,
	}})
}

// ensureCreated emits response.created exactly once, with the response object
// in its opening state: status in_progress, no output, no usage. The client
// builds its placeholder response from this object; the terminal one replaces
// it wholesale.
func (e *ResponsesEmitter) ensureCreated(dst []respWireEvent) []respWireEvent {
	if e.started {
		return dst
	}
	e.started = true
	out, err := EncodeResponsesResponse(&canonical.Response{
		ID: e.id, Model: e.echo.Model, Created: e.created,
	}, &e.echo)
	if err != nil {
		out = &ResponsesResponse{ID: e.id, Object: ObjectResponse, CreatedAt: e.created, Model: e.echo.Model}
	}
	out.Status = StatusInProgress
	out.Output = []ResponseItem{}
	out.Usage = nil
	out.IncompleteDetails = nil
	dst = append(dst, respWireEvent{name: respEventCreated, payload: &respCreatedEvent{
		Type: respEventCreated, Response: out, SequenceNumber: e.seq,
	}})
	e.seq++
	return dst
}

// malformedToolCall returns the first call whose accumulated arguments are not
// a JSON value, in first-seen order. An empty body is not malformed: a
// zero-argument call sends no fragments at all.
func (e *ResponsesEmitter) malformedToolCall() *respToolItem {
	for _, t := range e.order {
		s := strings.TrimSpace(t.args.String())
		if len(s) == 0 {
			continue
		}
		if !json.Valid([]byte(s)) {
			return t
		}
	}
	return nil
}

func (e *ResponsesEmitter) recordUsage(u canonical.Usage) {
	if u.Empty() {
		return
	}
	// Replace rather than add: a backend reporting cumulative usage per chunk
	// would otherwise be summed into nonsense.
	e.usage = u
	e.haveUsage = true
}

// ---------------------------------------------------------------------------
// Framing
// ---------------------------------------------------------------------------

// ResponsesStreamWriter frames [ResponsesEmitter]'s events onto an io.Writer.
//
// It is not safe for concurrent use, for the same reason [ResponsesEmitter]
// is not.
type ResponsesStreamWriter struct {
	w  io.Writer
	em *ResponsesEmitter

	buf   bytes.Buffer
	queue []respWireEvent

	closed bool
}

// NewResponsesStreamWriter returns a writer over w.
func NewResponsesStreamWriter(w io.Writer, cfg ResponsesStreamConfig) *ResponsesStreamWriter {
	return &ResponsesStreamWriter{w: w, em: NewResponsesEmitter(cfg)}
}

// Emitter exposes the state machine, for callers that need its accounting.
func (s *ResponsesStreamWriter) Emitter() *ResponsesEmitter { return s.em }

// ID is the pinned response id.
func (s *ResponsesStreamWriter) ID() string { return s.em.ID() }

// Usage returns the accumulated counts and whether any arrived.
func (s *ResponsesStreamWriter) Usage() (canonical.Usage, bool) { return s.em.Usage() }

// Completed returns the assembled terminal response object, for the store.
func (s *ResponsesStreamWriter) Completed() []byte { return s.em.Completed() }

// WriteEvent converts and frames one neutral event.
func (s *ResponsesStreamWriter) WriteEvent(ev canonical.StreamEvent) error {
	if s.closed {
		return errStreamClosed
	}
	s.queue = s.em.Push(s.queue[:0], ev)
	return s.flush()
}

// WriteError delivers an error in band and closes the stream.
func (s *ResponsesStreamWriter) WriteError(e *Error) error {
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
//
// It returns [ErrMalformedToolArguments] when the stream ended on a tool call
// whose arguments do not parse. The failure frame has already been written by
// then; the return exists so the exchange is counted as failed rather than
// recorded as a clean 200 (DESIGN §12.4).
func (s *ResponsesStreamWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.queue = s.em.Finish(s.queue[:0])
	if err := s.flush(); err != nil {
		return err
	}
	if s.em.MalformedToolArguments() {
		return ErrMalformedToolArguments
	}
	return nil
}

func (s *ResponsesStreamWriter) flush() error {
	for i := range s.queue {
		if err := s.writeEvent(&s.queue[i]); err != nil {
			return err
		}
	}
	s.queue = s.queue[:0]
	return nil
}

// writeEvent emits exactly "event: <type>\ndata: <json>\n\n" — BOTH lines,
// the framing this family's SDK dispatches on (the capture the decoder is
// tested against, and COMPATIBILITY 6.1's rule for the other named-event
// family: a data-only frame is a stream this one silently discards).
func (s *ResponsesStreamWriter) writeEvent(ev *respWireEvent) error {
	s.buf.Reset()
	s.buf.WriteString("event: ")
	s.buf.WriteString(ev.name)
	s.buf.WriteString("\ndata: ")
	if err := marshalTo(&s.buf, ev.payload); err != nil {
		return err
	}
	s.buf.WriteString("\n\n")
	_, err := s.w.Write(s.buf.Bytes())
	return err
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// mergeToolName folds a name fragment into the name accumulated so far.
//
// A repeated fragment is ambiguous — a backend restating a whole name and a
// backend splitting "aaaa" into "aa" and "aa" produce identical bytes — and
// this side of the crossing has no tool table to settle it with. The chat
// decoder does (openai.ToolStream) and resolves the name before it reaches a
// writer; this is the defence for every other producer, and it prefers
// restatement, which is the shape that actually occurs.
func mergeToolName(have, frag string, warn WarnFunc) string {
	switch {
	case have == "":
		return frag
	case frag == have:
		warn.warn(WarnRepeatedToolName, frag)
		return have
	default:
		return have + frag
	}
}

// thinkingSignature returns a thinking block's signature, empty when absent.
// It rides the item as encrypted_content when set, forwarded byte-identically:
// dorang never mints one (DESIGN §10.2).
func thinkingSignature(b *canonical.Block) string {
	if b.Thinking != nil {
		return b.Thinking.Signature
	}
	return ""
}

// errorMember renders a neutral error as this family's `error` member:
// the four fields the SDK reads, null param included, in the member order
// the buffered error envelope uses (§11.1).
func errorMember(e *Error) json.RawMessage {
	b, err := json.Marshal(struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    string  `json:"code"`
	}{Message: e.Message, Type: e.Type, Param: e.Param, Code: e.Code})
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// malformedResponsesToolError is this family's spelling of the malformed
// tool-argument failure (the chat one is [malformedArgumentsError]); the
// sentence names this surface so a log line says which protocol failed.
func malformedResponsesToolError(t *respToolItem) *Error {
	msg := "the upstream ended a tool call whose arguments are not valid JSON"
	if t.item.Name != "" {
		msg += ": " + t.item.Name
	} else if t.item.CallID != "" {
		msg += ": " + t.item.CallID
	}
	return NewError(502, TypeInvalidResponse, msg).WithCode(WarnMalformedToolArguments)
}
