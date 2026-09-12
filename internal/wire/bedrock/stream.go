package bedrock

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/ziozzang/dorang/internal/canonical"
)

// StreamDecoder turns converse-stream messages into neutral events.
//
// The events are a flat sequence keyed by content block index — messageStart,
// contentBlockStart, contentBlockDelta, contentBlockStop, messageStop,
// metadata — which is closer to the neutral form than either OpenAI's chunks
// or Anthropic's typed tree. A tool call's identity arrives on
// contentBlockStart and its input on contentBlockDelta, joined by the index;
// reasoning arrives as its own delta kind and is thinking, not text.
type StreamDecoder struct {
	es   *EventStreamReader
	opt  *DecodeOptions
	sent bool
	// blocks maps a content block index to what opened there, so a delta is
	// attributed without re-reading the start.
	blocks map[int]string
	// sawStop records messageStop; done records the metadata event, which is
	// Converse's TRUE terminator — it carries usage and always follows the
	// stop. A stream that ends at messageStop without metadata was cut off
	// between them, and Terminated reports that as not-terminated so the relay
	// marks it truncated rather than billing a generation as free.
	sawStop bool
	done    bool
	usage   canonical.Usage
	used    bool
}

// NewStreamDecoder wraps a converse-stream body.
func NewStreamDecoder(r io.Reader, opt *DecodeOptions) *StreamDecoder {
	return &StreamDecoder{es: NewEventStreamReader(r), opt: opt, blocks: map[int]string{}}
}

// Terminated reports that the stream's true end — the metadata event — arrived.
func (d *StreamDecoder) Terminated() bool { return d.done }

// Usage returns the counts read so far.
func (d *StreamDecoder) Usage() (canonical.Usage, bool) { return d.usage, d.used }

type streamFrame struct {
	Role              string `json:"role"`
	ContentBlockIndex int    `json:"contentBlockIndex"`
	Start             *struct {
		ToolUse *struct {
			ToolUseID string `json:"toolUseId"`
			Name      string `json:"name"`
		} `json:"toolUse"`
	} `json:"start"`
	Delta *struct {
		Text    string `json:"text"`
		ToolUse *struct {
			Input string `json:"input"`
		} `json:"toolUse"`
		ReasoningContent *struct {
			Text            string `json:"text"`
			Signature       string `json:"signature"`
			RedactedContent string `json:"redactedContent"`
		} `json:"reasoningContent"`
	} `json:"delta"`
	StopReason string `json:"stopReason"`
	Usage      *Usage `json:"usage"`
	Message    string `json:"message"`
}

// Next returns the neutral events of the next message that produces any, or
// io.EOF.
func (d *StreamDecoder) Next() ([]canonical.StreamEvent, error) {
	for {
		m, err := d.es.Next()
		if err != nil {
			return nil, err
		}
		evs, err := d.frame(m)
		if err != nil {
			return nil, err
		}
		if len(evs) > 0 {
			return evs, nil
		}
	}
}

func (d *StreamDecoder) frame(m *EventMessage) ([]canonical.StreamEvent, error) {
	// An exception is processed whenever it arrives, including after the stop:
	// Bedrock can throttle or fault between messageStop and metadata, and a
	// dropped exception there is a failure recorded as a success.
	if mt := m.MessageType(); mt == "exception" || mt == "error" {
		// The upstream's own text is what the operator needs; the type is the
		// exception class Bedrock names (throttlingException, ...).
		d.done = true
		var body struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(m.Payload, &body)
		msg := body.Message
		if msg == "" {
			msg = "the upstream reported an error mid-stream"
		}
		return []canonical.StreamEvent{{Type: canonical.EventError,
			Err: &canonical.Error{Message: msg, Type: m.ExceptionType()}}}, nil
	}
	var v streamFrame
	if len(m.Payload) > 0 {
		if err := json.Unmarshal(m.Payload, &v); err != nil {
			return nil, fmt.Errorf("%w: %s payload is not JSON", ErrEventStreamFrame, m.EventType())
		}
	}
	switch m.EventType() {
	case "messageStart":
		return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{Type: canonical.EventStart})}, nil
	case "contentBlockStart":
		if v.Start != nil && v.Start.ToolUse != nil {
			d.blocks[v.ContentBlockIndex] = "toolUse"
			return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
				Type: canonical.EventDelta,
				Delta: canonical.Delta{Role: d.roleOnce(), ToolCalls: []canonical.ToolCallDelta{{
					Index: v.ContentBlockIndex, ID: v.Start.ToolUse.ToolUseID, Name: v.Start.ToolUse.Name,
				}}},
			})}, nil
		}
		return nil, nil
	case "contentBlockDelta":
		if v.Delta == nil {
			return nil, nil
		}
		switch {
		case v.Delta.ToolUse != nil:
			if v.Delta.ToolUse.Input == "" {
				return nil, nil
			}
			return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
				Type: canonical.EventDelta,
				Delta: canonical.Delta{ToolCalls: []canonical.ToolCallDelta{{
					Index: v.ContentBlockIndex, Arguments: v.Delta.ToolUse.Input,
				}}},
			})}, nil
		case v.Delta.ReasoningContent != nil:
			rc := v.Delta.ReasoningContent
			var blk canonical.Block
			switch {
			case rc.Text != "":
				blk = canonical.ThinkingBlock(rc.Text, "")
			case rc.Signature != "":
				blk = canonical.ThinkingBlock("", rc.Signature)
			case rc.RedactedContent != "":
				blk = canonical.ThinkingBlock(rc.RedactedContent, "")
				blk.Thinking.Redacted = true
			default:
				return nil, nil
			}
			return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
				Type:  canonical.EventDelta,
				Delta: canonical.Delta{Role: d.roleOnce(), Content: []canonical.Block{blk}},
			})}, nil
		case v.Delta.Text != "":
			return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
				Type:  canonical.EventDelta,
				Delta: canonical.Delta{Role: d.roleOnce(), Content: []canonical.Block{canonical.TextBlock(v.Delta.Text)}},
			})}, nil
		}
		return nil, nil
	case "messageStop":
		d.sawStop = true
		stop, _ := stopReasonOf(v.StopReason)
		if stop == canonical.StopUnspecified {
			stop = canonical.StopEndTurn
		}
		return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{
			Type: canonical.EventStop, Delta: canonical.Delta{StopReason: stop},
		})}, nil
	case "metadata":
		// The true terminator, whether or not it carries usage: reaching it is
		// what tells the relay the stream ended cleanly.
		d.done = true
		if v.Usage == nil {
			return nil, nil
		}
		u := usageOf(v.Usage)
		d.usage, d.used = *u, true
		return []canonical.StreamEvent{d.stamp(canonical.StreamEvent{Type: canonical.EventUsage, Usage: u})}, nil
	}
	// contentBlockStop and anything this build does not know: boundaries the
	// neutral form does not have.
	return nil, nil
}

// stamp is where an id, model and created would go. Converse's stream carries
// none of them — no message id, no served-model name — so nothing is stamped:
// a Model written here would be the CLIENT's name presented as the upstream's
// and would pass the substitution check by construction.
func (d *StreamDecoder) stamp(e canonical.StreamEvent) canonical.StreamEvent { return e }

func (d *StreamDecoder) roleOnce() canonical.Role {
	if d.sent {
		return ""
	}
	d.sent = true
	return canonical.RoleAssistant
}
