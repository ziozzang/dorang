package openai

import (
	"bytes"
	"crypto/rand"
	"io"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
)

// UsageChunkChoices selects the shape of the usage chunk's choices array.
//
// COMPATIBILITY 3.3 is a deliberate divergence from OpenAI: the reference proxy
// sends "choices":[{"index":0,"delta":{}}] where OpenAI sends "choices":[].
// dorang follows the reference proxy, because the clients that exist were built
// against it, and exposes this switch — named for the compat flag
// compat.usage_chunk_choices — for callers who want the strict shape.
type UsageChunkChoices string

const (
	// UsageChunkChoicesStub is the default: [{"index":0,"delta":{}}].
	UsageChunkChoicesStub UsageChunkChoices = "stub"
	// UsageChunkChoicesEmpty is strict OpenAI: [].
	UsageChunkChoicesEmpty UsageChunkChoices = "empty"
)

// StreamConfig configures a [StreamWriter].
type StreamConfig struct {
	// ID is pinned across every chunk of the stream (COMPATIBILITY 2.4). Empty
	// generates one. Regenerating it per chunk breaks clients that use it as a
	// stream identity.
	ID string
	// Created is pinned across every chunk (COMPATIBILITY 2.4). Zero uses Now.
	Created int64
	// Model is restamped on EVERY chunk to the client-facing name
	// (COMPATIBILITY 2.5, DESIGN §7.2). It is a stream property, not a chunk
	// property, which is why it lives here and not on Chunk.
	Model string

	// IncludeUsage gates the usage chunk. It must come from an exact
	// stream_options.include_usage == true (COMPATIBILITY 3.1); truthy is not
	// enough, and without stream_options usage is computed but never put on the
	// wire (3.2).
	IncludeUsage bool
	// UsageChunkChoices selects the compat shape. Empty means
	// UsageChunkChoicesStub.
	UsageChunkChoices UsageChunkChoices

	// ToolNames restores tool names shortened for the upstream
	// (COMPATIBILITY 5.3) on the raw-chunk path.
	ToolNames *ToolNames

	Warn WarnFunc

	// Now is injectable for tests. Nil means time.Now.
	Now func() time.Time
}

// StreamWriter emits an OpenAI SSE stream and enforces the framing, identity
// and terminal-chunk contracts.
//
// It is not safe for concurrent use: an SSE stream is a single ordered byte
// sequence and interleaving writers would corrupt the framing regardless of any
// locking here.
type StreamWriter struct {
	w   io.Writer
	cfg StreamConfig

	buf bytes.Buffer

	started bool
	closed  bool

	// sawToolCall drives the terminal-chunk upgrade of COMPATIBILITY 4.4.
	sawToolCall bool
	// finished records which choice indexes already carried a finish_reason.
	finished []bool

	usage    canonical.Usage
	hasUsage bool
}

// NewStreamWriter returns a writer over w.
func NewStreamWriter(w io.Writer, cfg StreamConfig) *StreamWriter {
	if cfg.ID == "" {
		cfg.ID = NewStreamID()
	}
	if cfg.Created == 0 {
		cfg.Created = cfg.now().Unix()
	}
	if cfg.UsageChunkChoices == "" {
		cfg.UsageChunkChoices = UsageChunkChoicesStub
	}
	return &StreamWriter{w: w, cfg: cfg}
}

func (c StreamConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// ID is the pinned stream id.
func (s *StreamWriter) ID() string { return s.cfg.ID }

// Created is the pinned creation timestamp.
func (s *StreamWriter) Created() int64 { return s.cfg.Created }

// Usage returns the accumulated counts and whether any arrived. Usage is
// available even when it was never put on the wire, which is exactly the case
// COMPATIBILITY 3.2 describes.
func (s *StreamWriter) Usage() (canonical.Usage, bool) { return s.usage, s.hasUsage }

// SawToolCall reports whether any tool call was observed. It is what upgrades
// the synthesized terminal chunk from "stop" to "tool_calls".
func (s *StreamWriter) SawToolCall() bool { return s.sawToolCall }

// WriteChunk stamps and emits one chunk.
//
// Stamping is unconditional: whatever id, created and model the backend put in
// the frame are replaced with the stream's pinned values (COMPATIBILITY 2.4)
// and the client-facing model name (2.5).
func (s *StreamWriter) WriteChunk(c *Chunk) error {
	if s.closed {
		return errStreamClosed
	}
	c.ID = s.cfg.ID
	c.Object = ObjectChunk
	c.Created = s.cfg.Created
	if s.cfg.Model != "" {
		c.Model = s.cfg.Model
	}
	if c.Choices == nil {
		// A nil slice marshals as "choices":null, which is not a shape any
		// client handles. The empty array is (COMPATIBILITY 3.3's strict form).
		c.Choices = []ChunkChoice{}
	}

	for i := range c.Choices {
		ch := &c.Choices[i]
		if len(ch.Delta.ToolCalls) > 0 {
			s.sawToolCall = true
			if s.cfg.ToolNames != nil {
				for j := range ch.Delta.ToolCalls {
					fn := ch.Delta.ToolCalls[j].Function
					if fn != nil && fn.Name != nil {
						restored := s.cfg.ToolNames.Restore(*fn.Name)
						fn.Name = &restored
					}
				}
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.markFinished(ch.Index)
			if *ch.FinishReason == FinishToolCalls {
				s.sawToolCall = true
			}
		}
	}
	if c.Usage != nil {
		s.recordUsage(*usageToCanonical(c.Usage))
	}
	return s.frame(c)
}

// WriteEvent emits a neutral stream event.
func (s *StreamWriter) WriteEvent(ev canonical.StreamEvent) error {
	if s.closed {
		return errStreamClosed
	}
	switch ev.Type {
	case canonical.EventStart:
		// Identity only; nothing goes on the wire. The first content chunk
		// carries the pinned values.
		return nil

	case canonical.EventUsage:
		// COMPATIBILITY 3.4: some backends send usage AFTER finish_reason. The
		// accumulator must accept it rather than closing on the finish.
		if ev.Usage != nil {
			if s.anyFinished() {
				s.cfg.Warn.warn(WarnLateUsage, "")
			}
			s.recordUsage(*ev.Usage)
		}
		return nil

	case canonical.EventError:
		return s.WriteError(ErrorFrom(ev.Err))

	case canonical.EventStop:
		finish := FinishReasonOf(ev.Delta.StopReason)
		ch := ChunkChoice{Index: ev.Choice, FinishReason: &finish}
		native := ev.Delta.NativeStopReason
		if native == "" && !ev.Delta.StopReason.Expressible() {
			native = string(ev.Delta.StopReason)
		}
		if native != "" {
			ch.ProviderSpecificFields = nativeFinishFields(native)
		}
		c := &Chunk{Choices: []ChunkChoice{ch}}
		if ev.Usage != nil {
			s.recordUsage(*ev.Usage)
		}
		return s.WriteChunk(c)

	default: // canonical.EventDelta
		ch := ChunkChoice{Index: ev.Choice, Delta: deltaFrom(&ev.Delta)}
		return s.WriteChunk(&Chunk{Choices: []ChunkChoice{ch}})
	}
}

func deltaFrom(d *canonical.Delta) Delta {
	var out Delta
	if d.Role != "" {
		out.Role = ptr(string(d.Role))
	}
	var text, reasoning []byte
	sawText, sawReasoning := false, false
	for i := range d.Content {
		b := &d.Content[i]
		switch b.Kind {
		case canonical.KindText:
			text = append(text, b.Text...)
			sawText = true
		case canonical.KindThinking:
			reasoning = append(reasoning, b.Text...)
			sawReasoning = true
		}
	}
	if sawText {
		out.Content = ptr(string(text))
	}
	if sawReasoning {
		out.ReasoningContent = ptr(string(reasoning))
	}
	if d.Refusal != "" {
		out.Refusal = ptr(d.Refusal)
	}
	for i := range d.ToolCalls {
		tc := &d.ToolCalls[i]
		// COMPATIBILITY 5.1: index is always emitted. id, type and function are
		// attached only when this fragment carries them.
		w := ToolCallDelta{Index: tc.Index}
		if tc.ID != "" {
			w.ID = ptr(tc.ID)
		}
		typ := tc.Type
		if typ == "" && tc.ID != "" {
			typ = "function"
		}
		if typ != "" {
			w.Type = ptr(typ)
		}
		if tc.Name != "" || tc.Arguments != "" {
			fn := &FunctionDelta{}
			if tc.Name != "" {
				fn.Name = ptr(tc.Name)
			}
			if tc.Arguments != "" {
				fn.Arguments = ptr(tc.Arguments)
			}
			w.Function = fn
		}
		out.ToolCalls = append(out.ToolCalls, w)
	}
	return out
}

// WriteError delivers an error IN BAND and terminates the stream
// (COMPATIBILITY 1.3).
//
// Once the first chunk has gone out the HTTP status is already 200 and cannot
// change, which is the boundary DESIGN §7.6 refuses to cross with a fallback.
// The error frame is the only way the client finds out.
func (s *StreamWriter) WriteError(e *Error) error {
	if s.closed {
		return errStreamClosed
	}
	b, err := EncodeError(e)
	if err != nil {
		return err
	}
	if err := s.writeFrame(b); err != nil {
		return err
	}
	s.closed = true
	return s.writeDone()
}

// Close completes the stream.
//
// In order: the synthesized terminal chunk if the backend never sent one
// (COMPATIBILITY 4.4), the usage chunk if the client asked for it (3.1), then
// [DONE] (1.2). An entirely empty upstream stream produces an entirely empty
// body with no [DONE] at all (1.4), so Close on a writer that never received
// anything writes nothing.
func (s *StreamWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if !s.started && !s.hasUsage {
		return nil // COMPATIBILITY 1.4
	}

	if !s.anyFinished() {
		if err := s.writeTerminal(); err != nil {
			return err
		}
	}
	if s.cfg.IncludeUsage && s.hasUsage {
		if err := s.writeUsageChunk(); err != nil {
			return err
		}
	}
	return s.writeDone()
}

// writeTerminal synthesizes the missing terminal chunk.
//
// COMPATIBILITY 4.4, described there as the highest-value single line in the
// document: the default is "stop", but it is upgraded to "tool_calls" if any
// tool call was seen in the stream. A gateway that merely forwards emits "stop"
// on a tool-call turn, and every agentic client then treats the turn as final
// text and never executes the tool.
func (s *StreamWriter) writeTerminal() error {
	finish := FinishStop
	if s.sawToolCall {
		finish = FinishToolCalls
	}
	s.cfg.Warn.warn(WarnSynthesizedTerminal, finish)
	return s.frame(&Chunk{
		ID:      s.cfg.ID,
		Object:  ObjectChunk,
		Created: s.cfg.Created,
		Model:   s.cfg.Model,
		Choices: []ChunkChoice{{Index: 0, FinishReason: &finish}},
	})
}

func (s *StreamWriter) writeUsageChunk() error {
	c := &Chunk{
		ID:      s.cfg.ID,
		Object:  ObjectChunk,
		Created: s.cfg.Created,
		Model:   s.cfg.Model,
		Usage:   EncodeUsage(s.usage),
	}
	if s.cfg.UsageChunkChoices == UsageChunkChoicesEmpty {
		// Strict OpenAI. The slice must be non-nil so it marshals as [] and not
		// as null — "choices":null is not a shape any client handles.
		c.Choices = []ChunkChoice{}
	} else {
		c.Choices = []ChunkChoice{{Index: 0}}
	}
	return s.frame(c)
}

func (s *StreamWriter) frame(c *Chunk) error {
	s.buf.Reset()
	if err := marshalTo(&s.buf, c); err != nil {
		return err
	}
	return s.writeFrame(s.buf.Bytes())
}

// writeFrame emits exactly "data: <json>\n\n" (COMPATIBILITY 1.1): no event:
// line, no id: line.
func (s *StreamWriter) writeFrame(payload []byte) error {
	if _, err := io.WriteString(s.w, "data: "); err != nil {
		return err
	}
	if _, err := s.w.Write(payload); err != nil {
		return err
	}
	if _, err := io.WriteString(s.w, "\n\n"); err != nil {
		return err
	}
	s.started = true
	return nil
}

func (s *StreamWriter) writeDone() error {
	_, err := io.WriteString(s.w, DoneFrame)
	return err
}

func (s *StreamWriter) recordUsage(u canonical.Usage) {
	if u.Empty() {
		return
	}
	// Replace rather than add: a backend that reports cumulative usage per
	// chunk would otherwise be summed into nonsense. A backend that reports
	// increments is the rarer case and is handled by the accumulator upstream
	// of this writer.
	s.usage = u
	s.hasUsage = true
}

func (s *StreamWriter) markFinished(index int) {
	if index < 0 {
		index = 0
	}
	for len(s.finished) <= index {
		s.finished = append(s.finished, false)
	}
	s.finished[index] = true
}

func (s *StreamWriter) anyFinished() bool {
	for _, f := range s.finished {
		if f {
			return true
		}
	}
	return false
}

// TextChunk builds a plain text chunk. Its marshaled form is exactly
// {id, object, created, model, choices:[{index, delta:{content}}]}
// (COMPATIBILITY 2.2).
func TextChunk(content string) *Chunk {
	return &Chunk{
		Object:  ObjectChunk,
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: ptr(content)}}},
	}
}

// RoleChunk builds the opening chunk that announces the assistant role.
func RoleChunk(role string) *Chunk {
	return &Chunk{
		Object:  ObjectChunk,
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: ptr(role)}}},
	}
}

// NewStreamID generates a chat-completion id.
func NewStreamID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a time-derived fallback keeps
		// the stream identifiable rather than panicking mid-response.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * (i % 8)))
		}
	}
	const prefix = "chatcmpl-"
	out := make([]byte, len(prefix)+len(b)*2)
	copy(out, prefix)
	for i, v := range b {
		out[len(prefix)+i*2] = hexDigits[v>>4]
		out[len(prefix)+i*2+1] = hexDigits[v&0xf]
	}
	return string(out)
}

var errStreamClosed = errorString("openai: stream already closed")
