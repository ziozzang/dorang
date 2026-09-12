package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Responses stream event names.
//
// The set is deliberately narrow: these are the events that carry something the
// neutral form has a place for. Everything else — `response.in_progress`,
// `response.content_part.added`, the `.done` echoes that repeat what the deltas
// already said — is structural bookkeeping for a client rebuilding the response
// object, and a relay that forwarded it would be inventing content.
const (
	respEventCreated      = "response.created"
	respEventOutputAdded  = "response.output_item.added"
	respEventOutputDone   = "response.output_item.done"
	respEventTextDelta    = "response.output_text.delta"
	respEventRefusalDelta = "response.refusal.delta"
	respEventArgsDelta    = "response.function_call_arguments.delta"
	respEventReasonDelta  = "response.reasoning_summary_text.delta"
	respEventCompleted    = "response.completed"
	respEventIncomplete   = "response.incomplete"
	respEventFailed       = "response.failed"
	respEventError        = "error"
)

// respStreamFrame is one event's payload, read leniently.
//
// Every field is optional because the event set is open: OpenAI adds events to
// this surface without a version bump, and a decoder that refused an unknown
// shape would turn a working deployment into a broken one on the vendor's
// schedule. What is NOT lenient is the mapping — an event this build does not
// recognise produces no neutral event at all, rather than a guess.
type respStreamFrame struct {
	Type     string             `json:"type"`
	Delta    string             `json:"delta"`
	ItemID   string             `json:"item_id"`
	Item     *respStreamItem    `json:"item"`
	Response *ResponsesResponse `json:"response"`
	Error    *respStreamErr     `json:"error"`
	// OutputIndex orders items within one response, and is what a tool-call
	// fragment is keyed by: COMPATIBILITY §5.1 requires an index on every
	// [canonical.ToolCallDelta] and this surface's is per output item.
	OutputIndex int `json:"output_index"`
}

type respStreamItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Role      string `json:"role"`
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
}

type respStreamErr struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ResponsesStreamDecoder turns the Responses event sequence into neutral
// events.
//
// # Why this exists
//
// `internal/backend`'s adapter comment named the absence of this decoder as the
// one thing keeping `api: openai-responses` pointed at `/v1/chat/completions`.
// That indirection is harmless where a host serves both routes and fatal where
// it does not: the ChatGPT Codex surface answers 403 on the chat route, serves
// only `/responses`, and REQUIRES `stream: true` — so without a stream decoder
// there is no way to reach it at all.
//
// # The shape of the problem
//
// A chat stream is one flat sequence of choice deltas. This one is a tree
// announced in order: items open and close, parts open and close inside them,
// and text arrives as deltas inside a part. The neutral form has no notion of
// any of that — it has content fragments and tool-call fragments — so the
// decoder's whole job is to keep just enough state to know WHICH KIND of
// fragment a delta belongs to, and to emit nothing for the boundaries.
//
// Reasoning is the case that matters. A `reasoning` item's deltas are thinking
// fragments and a `message` item's are text, and the two are told apart only by
// the item that opened. A decoder that ignored `output_item.added` would deliver
// a model's private reasoning to a caller as its answer.
type ResponsesStreamDecoder struct {
	sc   *bufio.Scanner
	opt  *DecodeOptions
	sent bool
	// items maps output_index to what that item is, so a delta can be attributed
	// without re-reading the item that opened it.
	items map[int]string
	// id, model and created come from response.created and are stamped on
	// every event, because a neutral consumer may see any event first.
	id      string
	model   string
	created int64

	usage    canonical.Usage
	haveUsed bool
	done     bool
	// started records that the first line has been read, which is the only
	// line a byte-order mark can precede.
	started bool
	// overflow records that one event's data fields, assembled, exceeded
	// [maxResponsesFrameBytes]; Next reports it as an error once the scan
	// stops. The scanner bounds one LINE, and an upstream that never sends the
	// blank line could otherwise grow the assembled payload without bound.
	overflow bool
}

// errResponsesFrameTooLarge is one event whose assembled payload exceeds
// [maxResponsesFrameBytes]: either a frame this decoder will not buffer, or a
// stream that never dispatches and would be buffered until the process died.
var errResponsesFrameTooLarge = errorString("openai: a Responses stream event exceeds the frame limit")

// utf8BOM is the byte-order mark some proxies prepend to a body they
// re-encoded. It is not part of any event.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// errResponsesFrame is a `data:` payload that is not JSON.
//
// It is an ERROR and not a skipped frame, unlike an event whose type this build
// does not know: an unknown type is the vendor extending an open set, and
// ignoring it invents nothing; a payload that does not parse is corruption, and
// skipping it would drop a text delta and hand on an answer with a hole in it as
// though it were whole.
var errResponsesFrame = errorString("openai: a Responses stream frame is not JSON")

// NewResponsesStreamDecoder returns a decoder over an SSE body.
func NewResponsesStreamDecoder(r io.Reader, opt *DecodeOptions) *ResponsesStreamDecoder {
	sc := bufio.NewScanner(r)
	// A Responses event carries the whole response object on completion, and
	// that object contains the model's output. 64 KiB is the default and far too
	// small for it; a truncated frame would be a stream that silently lost its
	// last event.
	sc.Buffer(make([]byte, 0, 64*1024), maxResponsesFrameBytes)
	return &ResponsesStreamDecoder{sc: sc, opt: opt, items: map[int]string{}}
}

// Usage returns the counts read so far.
//
// The Responses surface reports `input_tokens` INCLUSIVE of cached tokens,
// which is already the neutral convention (§10.7), so nothing is added back.
// Reasoning tokens are part of `output_tokens` and are reported separately as
// well; carrying both is what lets a bill be checked rather than believed.
func (d *ResponsesStreamDecoder) Usage() (canonical.Usage, bool) {
	return d.usage, d.haveUsed
}

// Next returns the neutral events of the next frame that produces any, or
// io.EOF.
func (d *ResponsesStreamDecoder) Next() ([]canonical.StreamEvent, error) {
	for {
		data, ok := d.nextData()
		if !ok {
			if d.overflow {
				return nil, errResponsesFrameTooLarge
			}
			if err := d.sc.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		evs, err := d.frame(data)
		if err != nil {
			return nil, err
		}
		if len(evs) > 0 {
			return evs, nil
		}
	}
}

// DecodeResponsesStream reads a whole stream. It exists for tests and for the
// non-incremental paths; a relay uses [ResponsesStreamDecoder.Next].
func DecodeResponsesStream(b []byte, opt *DecodeOptions) ([]canonical.StreamEvent, error) {
	d := NewResponsesStreamDecoder(bytes.NewReader(b), opt)
	var out []canonical.StreamEvent
	for {
		evs, err := d.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, evs...)
	}
}

func (d *ResponsesStreamDecoder) frame(data []byte) ([]canonical.StreamEvent, error) {
	if d.done || len(data) == 0 {
		return nil, nil
	}
	var v respStreamFrame
	if err := json.Unmarshal(data, &v); err != nil {
		// See [errResponsesFrame]: an unknown TYPE is skipped below, a payload
		// that is not JSON is not.
		return nil, errResponsesFrame
	}
	// The event name is on the SSE line and repeated in the payload. The payload
	// wins: a proxy that rewrote the line still carries the original here.
	name := v.Type

	switch name {
	case respEventCreated:
		if v.Response != nil {
			d.id, d.model, d.created = v.Response.ID, v.Response.Model, v.Response.CreatedAt
		}
		return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{Type: canonical.EventStart})}, nil

	case respEventOutputAdded:
		if v.Item != nil {
			d.items[v.OutputIndex] = v.Item.Type
			if v.Item.Type == "function_call" {
				// The call's identity arrives here and its arguments stream
				// later, so the opening fragment carries the name and nothing
				// else. A consumer that waited for both in one event would
				// never see either.
				//
				// The name is the one the CALLER declared: a name shortened
				// for the wire (COMPATIBILITY 5.3) comes back through the same
				// registry the chat stream uses, or the caller receives a call
				// to a function it never defined.
				name := v.Item.Name
				if d.opt != nil && d.opt.ToolNames != nil {
					name = d.opt.ToolNames.Restore(name)
				}
				return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
					Type: canonical.EventDelta,
					Delta: canonical.Delta{
						Role: canonical.RoleAssistant,
						ToolCalls: []canonical.ToolCallDelta{{
							Index: v.OutputIndex,
							ID:    firstNonEmpty(v.Item.CallID, v.Item.ID),
							Name:  name,
						}},
					},
				})}, nil
			}
		}
		return nil, nil

	case respEventTextDelta:
		if v.Delta == "" {
			return nil, nil
		}
		return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
			Type: canonical.EventDelta,
			Delta: canonical.Delta{
				Role:    d.roleOnce(),
				Content: []canonical.Block{canonical.TextBlock(v.Delta)},
			},
		})}, nil

	case respEventReasonDelta:
		if v.Delta == "" {
			return nil, nil
		}
		// A reasoning fragment is NOT text. Delivering it as content would hand
		// a caller the model's private reasoning as its answer.
		return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
			Type: canonical.EventDelta,
			Delta: canonical.Delta{
				Role:    d.roleOnce(),
				Content: []canonical.Block{canonical.ThinkingBlock(v.Delta, "")},
			},
		})}, nil

	case respEventRefusalDelta:
		if v.Delta == "" {
			return nil, nil
		}
		return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
			Type:  canonical.EventDelta,
			Delta: canonical.Delta{Role: d.roleOnce(), Refusal: v.Delta},
		})}, nil

	case respEventArgsDelta:
		if v.Delta == "" {
			return nil, nil
		}
		return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
			Type: canonical.EventDelta,
			Delta: canonical.Delta{
				ToolCalls: []canonical.ToolCallDelta{{
					Index:     v.OutputIndex,
					Arguments: v.Delta,
				}},
			},
		})}, nil

	case respEventCompleted, respEventIncomplete:
		return d.terminal(name, &v), nil

	case respEventFailed:
		// A failure is an ERROR, not a stop with an unusual reason. The
		// generation did not finish and the upstream said why, in
		// `response.error`; rendering it as a stop would hand the caller a
		// clean end-of-turn over an answer that was never produced, and the
		// relay would record no failure at all.
		d.done = true
		e := &canonical.Error{Message: "the upstream reported the response failed"}
		if v.Response != nil && len(v.Response.Error) > 0 {
			var re respStreamErr
			if json.Unmarshal(v.Response.Error, &re) == nil {
				e.Message, e.Code, e.Type = firstNonEmpty(re.Message, e.Message), re.Code, re.Type
			}
		}
		return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
			Type: canonical.EventError,
			Err:  e,
		})}, nil

	case respEventError:
		d.done = true
		msg, code := "the upstream reported an error", ""
		if v.Error != nil {
			msg, code = firstNonEmpty(v.Error.Message, msg), v.Error.Code
		}
		return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
			Type: canonical.EventError,
			Err:  &canonical.Error{Message: msg, Code: code},
		})}, nil
	}
	// output_item.done, content_part.added/done, output_text.done,
	// response.in_progress: boundaries the neutral form does not have. Emitting
	// anything for them would duplicate content the deltas already carried.
	return nil, nil
}

// terminal renders the closing events: a stop reason, then usage.
//
// Usage is a SEPARATE event and comes after the stop, which is
// COMPATIBILITY §3.4's rule — a client that stops reading at the stop reason
// still gets a correct stop, and one that reads to the end gets the counts.
func (d *ResponsesStreamDecoder) terminal(name string, v *respStreamFrame) []canonical.StreamEvent {
	d.done = true
	reason := canonical.StopEndTurn
	switch name {
	case respEventIncomplete:
		// The surface's own word for "the ceiling stopped it", which is a
		// different fact from a model that finished — and one a tool call in
		// the partial output does not override: a call cut off by the ceiling
		// is a ceiling, not a turn that ended in a call.
		reason = canonical.StopMaxTokens
	case respEventCompleted:
		if v.Response != nil {
			for _, it := range v.Response.Output {
				if it.Type == "function_call" {
					reason = canonical.StopToolUse
					break
				}
			}
		}
	}
	out := []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
		Type:  canonical.EventStop,
		Delta: canonical.Delta{StopReason: reason},
	})}
	if v.Response != nil && v.Response.Usage != nil {
		u := responsesUsage(v.Response.Usage)
		d.usage, d.haveUsed = u, true
		// The unmodelled counts travel with the counts they qualify. Without
		// this a caller who asked for a stream lost every one of them —
		// server-side tool spend among them — and a caller who did not kept
		// them: two identical requests, two different ledgers, decided by
		// whether the answer was wanted incrementally.
		out = append(out, d.stamp(canonical.StreamEvent{
			Type:       canonical.EventUsage,
			Usage:      &u,
			UsageExtra: responsesUsageExtra(v.Response.Usage, toolUsageOf(v.Response)),
		}))
	}
	return out
}

// stamp puts the stream's identity on every event.
func (d *ResponsesStreamDecoder) stamp(e canonical.StreamEvent) canonical.StreamEvent {
	e.ID, e.Model, e.Created = d.id, d.model, d.created
	return e
}

// roleOnce returns the assistant role on the first content delta and empty
// afterwards, which is what [canonical.Delta.Role] documents.
func (d *ResponsesStreamDecoder) roleOnce() canonical.Role {
	if d.sent {
		return ""
	}
	d.sent = true
	return canonical.RoleAssistant
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// maxResponsesFrameBytes bounds one SSE data line.
//
// The terminal event carries the entire response object, so this is sized for a
// long answer rather than for a delta. A frame past it ends the stream with an
// error rather than being silently cut: a truncated JSON object decodes to
// nothing, and nothing is indistinguishable from a stream that simply stopped.
const maxResponsesFrameBytes = 8 << 20

// nextData returns the payload of the next `data:` line.
func (d *ResponsesStreamDecoder) nextData() ([]byte, bool) {
	// One event is every `data:` line up to the blank line that dispatches it,
	// joined with newlines — the SSE rule, which this vendor does not exercise
	// and a proxy re-wrapping the stream may. Reading each line as its own
	// frame would split one JSON document into two unparseable halves.
	var buf []byte
	have := false
	flush := func() ([]byte, bool) {
		payload := bytes.TrimSpace(buf)
		buf, have = buf[:0], false
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			return nil, false
		}
		// The scanner reuses its buffer, and a caller may hold an event past
		// the next Scan; the copy is one per frame and buys that safety.
		out := make([]byte, len(payload))
		copy(out, payload)
		return out, true
	}
	for d.sc.Scan() {
		line := bytes.TrimRight(d.sc.Bytes(), "\r")
		if !d.started {
			d.started = true
			line = bytes.TrimPrefix(line, utf8BOM)
		}
		if len(line) == 0 {
			if out, ok := flush(); ok {
				return out, true
			}
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			// An `event:` line while data is pending opens the NEXT event on
			// a stream that omits the blank line between them — which the
			// capture this decoder is tested against does, and which the
			// spec's dispatch rule alone would read as one event with two
			// payloads. The spec also allows the field order the other way
			// round, `data:` then `event:` inside ONE event, so the line
			// dispatches only a payload that is already a whole document; an
			// unfinished one keeps accumulating to the blank line. The name
			// on the line is not needed: the payload repeats it, and the
			// payload wins.
			if have && bytes.HasPrefix(line, []byte("event:")) && json.Valid(bytes.TrimSpace(buf)) {
				if out, ok := flush(); ok {
					return out, true
				}
			}
			continue
		}
		payload := line[len("data:"):]
		if len(payload) > 0 && payload[0] == ' ' {
			payload = payload[1:]
		}
		if len(buf)+1+len(payload) > maxResponsesFrameBytes {
			d.overflow = true
			return nil, false
		}
		if have {
			buf = append(buf, '\n')
		}
		buf = append(buf, payload...)
		have = true
	}
	// A stream that ends without the dispatching blank line still said what
	// it said.
	if have {
		return flush()
	}
	return nil, false
}
