package anthropic

import "encoding/json"

// Event names (COMPATIBILITY 6.2).
//
// There is no ping and no [DONE]. The vendor's own stream does send periodic
// ping events; dorang does not emit them, and the decoder ignores an inbound
// one rather than treating it as an unknown event.
const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"

	// EventPing is accepted inbound and never emitted (6.2).
	EventPing = "ping"
	// EventError is how a mid-stream failure is delivered in band
	// (COMPATIBILITY 1.3). 6.2's list omits it, which is a gap in that row: once
	// the first frame is out the status is already 200 and there is no other
	// channel. The stream ends after it — there is no message_stop, because the
	// message did not stop, it failed.
	EventError = "error"
)

// Delta types carried by content_block_delta.
const (
	DeltaText      = "text_delta"
	DeltaInputJSON = "input_json_delta"
	DeltaThinking  = "thinking_delta"
	DeltaSignature = "signature_delta"
)

// Event is one framed SSE event: an event name and a payload that marshals to
// the data line.
//
// [Emitter] produces these and [StreamWriter] frames them. The split exists so
// that the state machine of COMPATIBILITY 6.6 can be driven and asserted
// without an io.Writer, which is what [FuzzEmitter] does.
type Event struct {
	Type    string
	Payload any
}

// MessageStartEvent opens the stream.
type MessageStartEvent struct {
	Type    string   `json:"type"`
	Message Response `json:"message"`
}

// ContentBlockStartEvent opens a content block. The index is this protocol's
// explicit block boundary, the thing chat-completions leaves implicit.
type ContentBlockStartEvent struct {
	Type         string       `json:"type"`
	Index        int          `json:"index"`
	ContentBlock ContentBlock `json:"content_block"`
}

// ContentBlockDeltaEvent carries an increment for the open block.
type ContentBlockDeltaEvent struct {
	Type  string     `json:"type"`
	Index int        `json:"index"`
	Delta BlockDelta `json:"delta"`
}

// BlockDelta is the increment itself.
type BlockDelta struct {
	Type string `json:"type"`
	// Text is a text_delta increment.
	Text string `json:"text,omitempty"`
	// Thinking is a thinking_delta increment.
	Thinking string `json:"thinking,omitempty"`
	// Signature is a signature_delta increment: integrity material, forwarded
	// verbatim and never synthesized (DESIGN §10.2).
	Signature string `json:"signature,omitempty"`
	// PartialJSON is an input_json_delta increment — a FRAGMENT of the tool
	// argument object, not a complete one.
	PartialJSON string `json:"partial_json,omitempty"`
}

// ContentBlockStopEvent closes a content block. Crossing in from a protocol
// with implicit boundaries means synthesizing one at every transition
// (DESIGN §10.7).
type ContentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

// MessageDeltaEvent is the terminal event before message_stop. It carries the
// stop reason and the final usage, and a content_block_stop must always precede
// it (COMPATIBILITY 6.6).
type MessageDeltaEvent struct {
	Type  string       `json:"type"`
	Delta MessageDelta `json:"delta"`
	Usage *Usage       `json:"usage,omitempty"`
}

// MessageDelta is the terminal delta. Both fields render even when null.
type MessageDelta struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

// MessageStopEvent ends the stream. Exactly once (COMPATIBILITY 6.2).
type MessageStopEvent struct {
	Type string `json:"type"`
}

// ErrorEvent is the in-band error frame.
type ErrorEvent struct {
	Type  string `json:"type"`
	Error Error  `json:"error"`
}

// rawEvent is the decoder's view of a frame before its type is known.
type rawEvent struct {
	Type  string          `json:"type"`
	Index int             `json:"index"`
	Delta json.RawMessage `json:"delta"`
	Usage *Usage          `json:"usage"`

	Message      *Response       `json:"message"`
	ContentBlock *ContentBlock   `json:"content_block"`
	Error        json.RawMessage `json:"error"`
}
