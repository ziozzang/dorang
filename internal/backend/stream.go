package backend

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// eventSource reads an upstream stream as neutral events. next returns io.EOF
// when the stream ends cleanly.
type eventSource interface {
	next() ([]canonical.StreamEvent, error)
}

// eventSink writes neutral events in the caller's protocol.
type eventSink interface {
	write(canonical.StreamEvent) error
	close() error
	usage() canonical.Usage
}

// relay forwards an event stream to the client.
//
// # Two paths, and why
//
// OpenAI to OpenAI is not decoded. internal/wire/openai's Scanner rewrites the
// model field in place and reads the terminal usage frame without parsing a
// single chunk, which is what keeps the per-token path free of a JSON round
// trip. Every other combination goes through the neutral event stream, because
// the two families disagree about block boundaries and there is nothing to
// relay verbatim.
//
// Neither path buffers. The scanner writes through as it scans; the crossing
// path holds at most one frame, which is the unit the wire already defines.
func (b *Backend) relay(x *exchange, resp *http.Response, w http.ResponseWriter) (canonical.Usage, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	fw := &flushWriter{w: w}

	// The byte relay can only run when nothing in the body has to change beyond
	// the model name. Two things are exactly such a change:
	//
	//   - A tool name that was shortened on the way out. The upstream calls the
	//     short name, the client never saw it, and no amount of scanning for
	//     "model" restores it. One over-long tool name therefore costs this
	//     request its fast path — and nothing else does, because the registry is
	//     empty on every request that had none.
	//   - A §10.5b transform. The relay rewrites one field per line without
	//     decoding, and unmasking needs a bounded tail carried *inside* a JSON
	//     string across frames — surgery on a scanner that deliberately does not
	//     parse. §10.5 settles the trade: reconstruction is the normal case, so a
	//     filtered request takes the neutral path, and only a filtered one pays.
	if x.call.ClientAPI == catalog.APIOpenAIChat && x.prov.api == catalog.APIOpenAIChat &&
		x.names.Len() == 0 && x.call.Transform == nil {
		sc := openai.NewScanner(fw, openai.ScannerOptions{
			From:         x.target.UpstreamModel,
			To:           x.call.Model,
			CollectUsage: true,
		})
		if _, err := io.Copy(sc, resp.Body); err != nil {
			return canonical.Usage{}, err
		}
		if err := sc.Flush(); err != nil {
			return canonical.Usage{}, err
		}
		u, _ := sc.Usage()
		return u, nil
	}

	src, err := x.prov.ad.source(resp.Body, x)
	if err != nil {
		return canonical.Usage{}, err
	}
	sink, err := newEventSink(x.call, fw)
	if err != nil {
		return canonical.Usage{}, err
	}
	// The transform composes into this one pass (§10.5): it takes decoded events
	// and returns decoded events, holding at most a placeholder's width minus one
	// byte across a frame boundary. Nil is the ordinary case and every call
	// through tr below is one nil check.
	var tr StreamTransform
	if x.call.Transform != nil {
		tr = x.call.Transform.Stream()
	}
	for {
		batch, err := src.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// The client has already seen output, so the status is spent
			// (DESIGN §7.6) and the only channel left is the stream itself
			// (COMPATIBILITY 1.3). Ending the body without saying anything leaves
			// a client waiting on a terminator that will never come.
			_ = sink.write(canonical.StreamEvent{
				Type: canonical.EventError,
				Err: &canonical.Error{
					StatusCode: http.StatusBadGateway,
					Type:       "api_error",
					Message:    "the upstream stream could not be read: " + err.Error(),
					Code:       CodeUpstreamDecode,
				},
			})
			return sink.usage(), err
		}
		for i := range batch {
			if tr != nil {
				if flush := tr.Rewrite(&batch[i]); flush != nil {
					if err := sink.write(*flush); err != nil {
						return sink.usage(), err
					}
				}
			}
			if err := sink.write(batch[i]); err != nil {
				return sink.usage(), err
			}
		}
	}
	// Whatever the tail still holds is the caller's text: it only looked like the
	// beginning of a placeholder.
	if tr != nil {
		if flush := tr.Flush(); flush != nil {
			if err := sink.write(*flush); err != nil {
				return sink.usage(), err
			}
		}
	}
	if err := sink.close(); err != nil {
		return sink.usage(), err
	}
	return sink.usage(), nil
}

// newEventSink builds the writer for the caller's protocol.
func newEventSink(c *Call, w io.Writer) (eventSink, error) {
	switch c.ClientAPI {
	case catalog.APIAnthropicMessages:
		return &anthropicSink{w: anthropic.NewStreamWriter(w, anthropic.StreamConfig{Model: c.Model})}, nil
	case catalog.APIOpenAIChat:
		return &openaiSink{w: openai.NewStreamWriter(w, openai.StreamConfig{
			Model:        c.Model,
			IncludeUsage: c.IncludeUsage,
		})}, nil
	}
	return nil, noOperation(string(c.ClientAPI), OpChat, "no streaming encoder for this caller protocol")
}

type openaiSink struct{ w *openai.StreamWriter }

func (s *openaiSink) write(ev canonical.StreamEvent) error { return s.w.WriteEvent(ev) }
func (s *openaiSink) close() error                         { return s.w.Close() }
func (s *openaiSink) usage() canonical.Usage {
	u, _ := s.w.Usage()
	return u
}

type anthropicSink struct{ w *anthropic.StreamWriter }

func (s *anthropicSink) write(ev canonical.StreamEvent) error { return s.w.WriteEvent(ev) }
func (s *anthropicSink) close() error                         { return s.w.Close() }
func (s *anthropicSink) usage() canonical.Usage {
	u, _ := s.w.Usage()
	return u
}

// anthropicSource wraps that package's own stream decoder.
type anthropicSource struct{ d *anthropic.StreamDecoder }

func (s *anthropicSource) next() ([]canonical.StreamEvent, error) { return s.d.Next() }

// openaiSource reads chat-completion SSE frames.
//
// internal/wire/openai exposes a chunk decoder and a relay scanner but no frame
// reader, because the relay path never needs one — it forwards bytes. A frame
// reader is only needed when the stream has to CROSS families, which is this
// path, so it lives here rather than being pushed into that package.
type openaiSource struct {
	br     *bufio.Reader
	data   strings.Builder
	done   bool
	engine Engine

	// opt carries the per-stream tool state. It is ONE value for the whole
	// stream, not one per frame: a tool name can arrive split across frames and
	// cannot be restored from either half alone.
	opt *openai.DecodeOptions
	// flushed records that the held tool fragments have already been drained.
	flushed bool
	// id, model and created are the stream identity, kept so the flush events
	// carry the same values every other frame did.
	id      string
	model   string
	created int64
}

func newOpenAISource(r io.Reader, x *exchange) *openaiSource {
	return &openaiSource{
		br:     bufio.NewReaderSize(r, 8<<10),
		engine: x.prov.engine,
		opt: &openai.DecodeOptions{
			ToolNames: x.names,
			Tools:     openai.NewToolStream(x.names, nil),
		},
	}
}

func (s *openaiSource) next() ([]canonical.StreamEvent, error) {
	for {
		if s.done {
			return s.flush()
		}
		payload, err := nextSSEData(s.br, &s.data, maxStreamFrame)
		if err != nil {
			s.done = true
			if err != io.EOF {
				return nil, err
			}
			return s.flush()
		}
		if payload == "[DONE]" {
			s.done = true
			return s.flush()
		}
		raw := []byte(payload)
		_, events, derr := openai.DecodeChunk(raw, s.opt)
		if derr != nil {
			return nil, derr
		}
		events = s.adoptReasoning(raw, events)
		if len(events) > 0 {
			s.note(events)
			return events, nil
		}
	}
}

// flush drains the tool fragments the tracker held back — a name that never
// settled because its call carried no arguments, or arrived after them — and
// then reports the end of the stream.
func (s *openaiSource) flush() ([]canonical.StreamEvent, error) {
	if s.flushed {
		return nil, io.EOF
	}
	s.flushed = true
	if evs := s.opt.Tools.Flush(s.id, s.model, s.created); len(evs) > 0 {
		return evs, nil
	}
	return nil, io.EOF
}

func (s *openaiSource) note(evs []canonical.StreamEvent) {
	if s.id != "" {
		return
	}
	for i := range evs {
		if evs[i].ID != "" {
			s.id, s.model, s.created = evs[i].ID, evs[i].Model, evs[i].Created
			return
		}
	}
}

// maxStreamFrame bounds ONE SSE frame: one line, and the data payload a frame
// accumulates across its `data:` lines.
//
// The stream as a whole stays unbounded, which is the point of a stream. What is
// bounded is how much of it dorang holds at once — and here it was not bounded
// at all. [nextSSEData] read a line with bufio.Reader.ReadString, whose contract
// is to keep growing until it finds a newline no matter how far away that is,
// and appended it to a strings.Builder nothing measured. An upstream that
// answers a streaming request with `data: ` and then megabytes without a newline
// made the gateway buffer every byte, per concurrent stream, from the far side
// of the trust boundary. It is the streaming twin of the unbounded success-path
// read [readUpstreamBody] exists to prevent, and both openaiSource and
// geminiSource reach it.
//
// The value is the repository's existing answer to the same question rather than
// a new one. Both other SSE readers on this path already bound a frame at
// [openai.DefaultMaxFrameBytes] — internal/wire/anthropic's FrameReader through
// bufio.Scanner.Buffer, and internal/wire/openai's relay Scanner through
// checkOverflow — and this reader sitting between them with no ceiling was the
// omission, not the number.
//
// What differs is the response to hitting it. The relay Scanner degrades: it
// forwards the over-long frame and stops trying to rewrite it, which it can
// afford because it is copying bytes. This reader has to DECODE a frame to cross
// protocol families, so there is nothing to forward and refusing is the only
// answer available.
const maxStreamFrame = openai.DefaultMaxFrameBytes

// errStreamFrameTooLarge is the sentinel for a frame that hit the ceiling. It
// surfaces to the client as an error event on the stream itself, because by the
// time a frame is being read the status is already spent (§7.6).
var errStreamFrameTooLarge = errors.New(
	"upstream stream frame exceeded the size this gateway will buffer")

// nextSSEData reads one frame's data payload.
//
// It returns io.EOF at the end of the stream, and never returns an empty
// payload: a comment-only frame is a keep-alive and an empty one is padding,
// and neither is an event. A final frame with no terminating blank line is
// discarded rather than delivered — every one of these protocols terminates its
// stream explicitly, so an unterminated trailing frame is a truncated stream,
// and half an event is worse than none.
//
// limit bounds both the line and the accumulated payload; see [maxStreamFrame].
func nextSSEData(br *bufio.Reader, buf *strings.Builder, limit int) (string, error) {
	for {
		line, err := readLineLimited(br, limit)
		if err == errStreamFrameTooLarge {
			buf.Reset()
			return "", err
		}
		if line == "" && err != nil {
			buf.Reset()
			if err == io.EOF {
				return "", io.EOF
			}
			return "", err
		}
		trimmed := strings.TrimRight(line, "\r\n")

		if trimmed == "" { // end of frame
			payload := strings.TrimSpace(buf.String())
			buf.Reset()
			if err != nil {
				return "", io.EOF
			}
			if payload != "" {
				return payload, nil
			}
			continue
		}

		if strings.HasPrefix(trimmed, ":") {
			// An SSE comment. Keep-alives arrive as one, and they carry nothing.
		} else if v, ok := strings.CutPrefix(trimmed, "data:"); ok {
			v = strings.TrimPrefix(v, " ")
			// A frame may spread its payload over many data: lines, so the
			// ceiling has to be checked against the accumulation and not only
			// against the line that was just read.
			if buf.Len()+len(v)+1 > limit {
				buf.Reset()
				return "", errStreamFrameTooLarge
			}
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(v)
		}
		// Every other field (event:, id:, retry:) is not carried by these
		// shapes and is dropped rather than guessed at.

		if err != nil {
			buf.Reset()
			return "", io.EOF
		}
	}
}

// readLineLimited reads one line, including its newline, holding at most limit
// bytes plus whatever bufio's own buffer already held.
//
// It replaces bufio.Reader.ReadString, whose contract is to keep growing until
// it finds the delimiter. ReadSlice returns what fits and says so, which is what
// makes the ceiling enforceable while the bytes are still arriving rather than
// after they have all been buffered.
func readLineLimited(br *bufio.Reader, limit int) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := br.ReadSlice('\n')
		if sb.Len()+len(chunk) > limit {
			return "", errStreamFrameTooLarge
		}
		sb.Write(chunk)
		if err == bufio.ErrBufferFull {
			continue
		}
		return sb.String(), err
	}
}

// adoptReasoning turns a self-hosted engine's own reasoning field into a
// thinking delta.
//
// internal/wire/openai's Delta models reasoning_content and nothing else, so a
// vLLM frame's `reasoning` is dropped by the chunk decoder. On this path — a
// crossing between protocol families — dropping it means an Anthropic-speaking
// caller sees no reasoning at all from a vLLM deployment that was configured to
// produce it (VLLM.md §2.5).
//
// The same-family relay does not come through here: it forwards bytes
// untouched, so an OpenAI caller sees the engine's own spelling. Normalizing
// that would mean decoding every frame on the per-token path, which is the cost
// the Scanner exists to avoid.
func (s *openaiSource) adoptReasoning(raw []byte, events []canonical.StreamEvent) []canonical.StreamEvent {
	if !s.engine.SelfHosted() {
		return events
	}
	var probe struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Reasoning *string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return events
	}
	for i := range probe.Choices {
		text := probe.Choices[i].Delta.Reasoning
		if text == nil || *text == "" {
			continue
		}
		ev := canonical.StreamEvent{
			Type:   canonical.EventDelta,
			Choice: probe.Choices[i].Index,
			Delta:  canonical.Delta{Content: []canonical.Block{canonical.ThinkingBlock(*text, "")}},
		}
		events = append(events, ev)
	}
	return events
}

// flushWriter pushes every relayed chunk to the client immediately. Without it
// a streaming response is buffered and arrives as one block, which is not a
// stream.
type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
