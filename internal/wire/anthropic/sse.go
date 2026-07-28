package anthropic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
)

// DefaultMaxFrameBytes bounds one SSE frame. An upstream that never sends a
// newline must not be able to grow dorang's heap without limit.
const DefaultMaxFrameBytes = 4 << 20

// Frame is one SSE event: the event name and the concatenated data lines.
type Frame struct {
	Event string
	// Data is valid until the next call to [FrameReader.Next]. The reader reuses
	// its buffer so that a steady stream does not allocate per frame.
	Data []byte
}

// FrameReader splits an SSE body into frames.
//
// Both lines matter here, unlike chat completions (COMPATIBILITY 6.1): the
// event name is the dispatch key and the data payload's own "type" member is a
// redundant copy of it. This reader keeps them together so a decoder can check
// that they agree.
type FrameReader struct {
	sc   *bufio.Scanner
	data []byte
}

// NewFrameReader returns a reader over an SSE body.
func NewFrameReader(r io.Reader) *FrameReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8192), DefaultMaxFrameBytes)
	return &FrameReader{sc: sc}
}

// Next returns the next frame, or io.EOF.
func (f *FrameReader) Next() (*Frame, error) {
	f.data = f.data[:0]
	event := ""
	got := false
	for f.sc.Scan() {
		line := f.sc.Bytes()
		if len(line) == 0 {
			if got {
				return &Frame{Event: event, Data: f.data}, nil
			}
			continue
		}
		if line[0] == ':' {
			// A comment. Some intermediaries send these as keep-alives; they are
			// not events and must not be dispatched.
			continue
		}
		field, value := splitField(line)
		switch string(field) {
		case "event":
			event = string(value)
			got = true
		case "data":
			if len(f.data) > 0 {
				f.data = append(f.data, '\n')
			}
			f.data = append(f.data, value...)
			got = true
		default:
			// id:, retry: and anything else are not part of this protocol.
		}
	}
	if err := f.sc.Err(); err != nil {
		return nil, err
	}
	if got {
		// A final frame that arrived without its blank-line terminator. Dropping
		// it would silently lose the last event of a truncated stream.
		return &Frame{Event: event, Data: f.data}, nil
	}
	return nil, io.EOF
}

func splitField(line []byte) (field, value []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return line, nil
	}
	field = line[:i]
	value = line[i+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}

// ---------------------------------------------------------------------------
// Decoding a stream into neutral events
// ---------------------------------------------------------------------------

// StreamDecoder converts an inbound Messages stream to neutral events, which is
// what dorang needs when this family is the BACKEND rather than the client.
//
// The block-index remapping is the part that is easy to get wrong. A block
// index is not a tool-call index: a stream whose first block is thinking and
// whose second is a tool call has that call at block index 1, while the neutral
// form — and every OpenAI-shaped client downstream — numbers tool calls from
// zero independently of any other content. Copying the block index across
// misnumbers every parallel call after the first.
type StreamDecoder struct {
	fr   *FrameReader
	opt  *DecodeOptions
	warn WarnFunc

	id    string
	model string

	// blockTool maps a content-block index to the neutral tool-call index.
	blockTool map[int]int
	nextTool  int
	// blocks is the per-content-block state needed to close a tool block
	// correctly: whether it was started, whether it was already stopped, and what
	// content_block_start put in its input.
	blocks map[int]*decodedBlock

	inputExclusive int
	cacheRead      int
	cacheWrite     int
	output         int
	haveUsage      bool

	done bool
}

// decodedBlock is one inbound content block's state.
type decodedBlock struct {
	tool bool
	// input is what content_block_start carried, kept only when it is a
	// non-trivial object. The vendor's own streams send {} there and put the real
	// document in input_json_delta, but a backend that emits the whole call in
	// one block puts it here, and dropping it turns a real argument object into
	// no arguments at all.
	input []byte
	// sawArgs records that at least one input_json_delta arrived.
	sawArgs bool
	stopped bool
}

// NewStreamDecoder returns a decoder over an SSE body.
func NewStreamDecoder(r io.Reader, opt *DecodeOptions) *StreamDecoder {
	return &StreamDecoder{fr: NewFrameReader(r), opt: opt, warn: opt.warn()}
}

// Usage returns the counts read so far, normalized to the neutral convention
// (cache-INCLUSIVE input; see [UsageToCanonical]).
func (d *StreamDecoder) Usage() (canonical.Usage, bool) {
	return canonical.Usage{
		InputTokens:      d.inputExclusive + d.cacheRead + d.cacheWrite,
		OutputTokens:     d.output,
		CacheReadTokens:  d.cacheRead,
		CacheWriteTokens: d.cacheWrite,
	}, d.haveUsage
}

// Next returns the neutral events of the next frame that produces any, or
// io.EOF.
func (d *StreamDecoder) Next() ([]canonical.StreamEvent, error) {
	for {
		f, err := d.fr.Next()
		if err != nil {
			return nil, err
		}
		evs, err := d.frame(f)
		if err != nil {
			return nil, err
		}
		if len(evs) > 0 {
			return evs, nil
		}
	}
}

// DecodeStream reads a whole stream. It exists for tests and for the
// non-incremental paths; the relay uses [StreamDecoder.Next].
func DecodeStream(b []byte, opt *DecodeOptions) ([]canonical.StreamEvent, error) {
	d := NewStreamDecoder(bytes.NewReader(b), opt)
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

func (d *StreamDecoder) frame(f *Frame) ([]canonical.StreamEvent, error) {
	switch f.Event {
	case EventPing, "":
		// 6.2 says dorang emits no ping. Receiving one is normal — the vendor's
		// own stream sends them — and it carries nothing.
		return nil, nil
	case EventMessageStop:
		// Block boundaries are explicit here and implicit in the neutral form,
		// which is the asymmetry DESIGN §10.7 describes from the other side.
		// Nothing to carry.
		return nil, nil
	}

	var raw rawEvent
	if err := json.Unmarshal(f.Data, &raw); err != nil {
		return nil, err
	}

	switch f.Event {
	case EventContentBlockStop:
		return d.closeBlock(raw.Index), nil

	case EventMessageStart:
		if raw.Message == nil {
			return nil, nil
		}
		d.id = raw.Message.ID
		d.model = raw.Message.Model
		if d.opt != nil && d.opt.Model != "" {
			d.model = d.opt.Model
		}
		ev := canonical.StreamEvent{Type: canonical.EventStart, ID: d.id, Model: d.model}
		if raw.Message.Usage != nil {
			d.mergeUsage(raw.Message.Usage)
			u, _ := d.Usage()
			ev.Usage = &u
		}
		return []canonical.StreamEvent{ev}, nil

	case EventContentBlockStart:
		if raw.ContentBlock == nil {
			return nil, nil
		}
		b := d.block(raw.Index)
		if raw.ContentBlock.Type != BlockToolUse {
			return nil, nil
		}
		b.tool, b.stopped = true, false
		if in := trimSpace(raw.ContentBlock.Input); len(in) > 0 &&
			string(in) != "{}" && string(in) != "null" {
			// A backend that put the whole argument object in the start event.
			// Held rather than emitted: if input_json_delta frames follow, the
			// two spellings contradict each other and the incremental one — the
			// only one the protocol defines as cumulative — has to win.
			b.input = append(b.input[:0], in...)
		}
		if raw.ContentBlock.ID == "" {
			d.warn.warn(WarnToolCallMissingID, raw.ContentBlock.Name)
		}
		idx := d.toolIndex(raw.Index)
		return []canonical.StreamEvent{d.event(canonical.Delta{
			ToolCalls: []canonical.ToolCallDelta{{
				Index: idx,
				ID:    raw.ContentBlock.ID,
				Name:  raw.ContentBlock.Name,
				Type:  "function",
			}},
		})}, nil

	case EventContentBlockDelta:
		var bd BlockDelta
		if len(raw.Delta) > 0 {
			if err := json.Unmarshal(raw.Delta, &bd); err != nil {
				return nil, err
			}
		}
		switch bd.Type {
		case DeltaText:
			if bd.Text == "" {
				return nil, nil
			}
			return []canonical.StreamEvent{d.event(canonical.Delta{
				Content: []canonical.Block{canonical.TextBlock(bd.Text)},
			})}, nil
		case DeltaThinking:
			if bd.Thinking == "" {
				return nil, nil
			}
			return []canonical.StreamEvent{d.event(canonical.Delta{
				Content: []canonical.Block{canonical.ThinkingBlock(bd.Thinking, "")},
			})}, nil
		case DeltaSignature:
			if bd.Signature == "" {
				return nil, nil
			}
			// Carried as its own fragment so the accumulator attaches it to the
			// block it belongs to without dorang ever recomputing it.
			return []canonical.StreamEvent{d.event(canonical.Delta{
				Content: []canonical.Block{{
					Kind:     canonical.KindThinking,
					Thinking: &canonical.Thinking{Signature: bd.Signature},
				}},
			})}, nil
		case DeltaInputJSON:
			if bd.PartialJSON == "" {
				return nil, nil
			}
			b := d.block(raw.Index)
			if len(b.input) > 0 {
				// content_block_start already carried a complete input and now the
				// incremental form contradicts it. Concatenating produces two JSON
				// documents in one field; the deltas are the form the protocol
				// defines, so the pre-seeded object is dropped and the conflict is
				// reported.
				d.warn.warn(WarnToolInputConflict, string(b.input))
				b.input = b.input[:0]
			}
			b.sawArgs = true
			return []canonical.StreamEvent{d.event(canonical.Delta{
				ToolCalls: []canonical.ToolCallDelta{{
					Index:     d.toolIndex(raw.Index),
					Arguments: bd.PartialJSON,
				}},
			})}, nil
		}
		return nil, nil

	case EventMessageDelta:
		var md MessageDelta
		if len(raw.Delta) > 0 {
			if err := json.Unmarshal(raw.Delta, &md); err != nil {
				return nil, err
			}
		}
		out := make([]canonical.StreamEvent, 0, 2)
		if md.StopReason != nil && *md.StopReason != "" {
			stop, native := stopReasonOfWire(*md.StopReason, d.warn)
			ev := d.event(canonical.Delta{StopReason: stop, NativeStopReason: native})
			ev.Type = canonical.EventStop
			out = append(out, ev)
		}
		if raw.Usage != nil {
			d.mergeUsage(raw.Usage)
			u, _ := d.Usage()
			ev := canonical.StreamEvent{Type: canonical.EventUsage, ID: d.id, Model: d.model, Usage: &u}
			out = append(out, ev)
		}
		return out, nil

	case EventError:
		e := &Error{}
		if len(raw.Error) > 0 {
			if err := json.Unmarshal(raw.Error, e); err != nil {
				return nil, err
			}
		}
		if e.StatusCode == 0 {
			e.StatusCode = statusForType(e.Type)
		}
		return []canonical.StreamEvent{{
			Type: canonical.EventError, ID: d.id, Model: d.model, Err: e.ToCanonical(),
		}}, nil
	}
	return nil, nil
}

func (d *StreamDecoder) event(delta canonical.Delta) canonical.StreamEvent {
	return canonical.StreamEvent{
		Type:  canonical.EventDelta,
		ID:    d.id,
		Model: d.model,
		Delta: delta,
	}
}

// block returns the per-content-block state, creating it on first sight.
func (d *StreamDecoder) block(index int) *decodedBlock {
	if d.blocks == nil {
		d.blocks = make(map[int]*decodedBlock, 2)
	}
	b := d.blocks[index]
	if b == nil {
		b = &decodedBlock{}
		d.blocks[index] = b
	}
	return b
}

// closeBlock handles content_block_stop.
//
// It exists for one reason that is easy to miss: a tool call with no arguments
// produces content_block_start with input:{} and then NO input_json_delta at
// all. Carrying nothing across leaves an OpenAI-shaped client with a tool call
// whose arguments field is absent rather than "{}", and the clients that do
// json.loads(arguments) on it raise instead of calling the tool.
//
// It is also where the two malformed shapes are absorbed: a second stop for a
// block already stopped, and a stop for a block that never started. Neither may
// allocate a tool index — doing so invents a parallel call out of a stray frame.
func (d *StreamDecoder) closeBlock(index int) []canonical.StreamEvent {
	b := d.blocks[index]
	if b == nil {
		d.warn.warn(WarnUnknownBlockStop, strconv.Itoa(index))
		return nil
	}
	if b.stopped {
		d.warn.warn(WarnDuplicateBlockStop, strconv.Itoa(index))
		return nil
	}
	b.stopped = true
	if !b.tool || b.sawArgs {
		return nil
	}
	args := "{}"
	if len(b.input) > 0 {
		args = string(b.input)
	}
	return []canonical.StreamEvent{d.event(canonical.Delta{
		ToolCalls: []canonical.ToolCallDelta{{
			Index:     d.toolIndex(index),
			Arguments: args,
		}},
	})}
}

// toolIndex maps a content-block index to a dense tool-call index.
func (d *StreamDecoder) toolIndex(block int) int {
	if d.blockTool == nil {
		d.blockTool = make(map[int]int, 2)
	}
	if i, ok := d.blockTool[block]; ok {
		return i
	}
	i := d.nextTool
	d.nextTool++
	d.blockTool[block] = i
	return i
}

// mergeUsage folds one wire usage object into the running counts.
//
// The wire counts are cache-exclusive and the neutral ones are inclusive, so
// the addition happens once, in [StreamDecoder.Usage], rather than per frame —
// adding at merge time would compound every time the backend restated a field.
func (d *StreamDecoder) mergeUsage(u *Usage) {
	if u == nil {
		return
	}
	if u.CacheReadInputTokens != nil {
		d.cacheRead = *u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil {
		d.cacheWrite = *u.CacheCreationInputTokens
	}
	if u.InputTokens != nil {
		d.inputExclusive = *u.InputTokens
	}
	if u.OutputTokens != nil {
		d.output = *u.OutputTokens
	}
	d.haveUsage = true
}

// statusForType is the inverse of [TypeForStatus], for an in-band error frame
// that carries a type but no status (the wire has nowhere to put one).
func statusForType(typ string) int {
	switch typ {
	case TypeAuthentication:
		return 401
	case TypePermission:
		return 403
	case TypeNotFound:
		return 404
	case TypeRequestTooLarge:
		return 413
	case TypeRateLimit:
		return 429
	case TypeTimeout:
		return 408
	case TypeOverloaded:
		return 529
	case TypeInvalidRequest:
		return 400
	default:
		return 500
	}
}
