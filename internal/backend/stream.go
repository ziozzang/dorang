package backend

import (
	"bufio"
	"encoding/json"
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

	if x.call.ClientAPI == catalog.APIOpenAIChat && x.prov.api == catalog.APIOpenAIChat {
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
	for {
		batch, err := src.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sink.usage(), err
		}
		for i := range batch {
			if err := sink.write(batch[i]); err != nil {
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
}

func (s *openaiSource) next() ([]canonical.StreamEvent, error) {
	for {
		if s.done {
			return nil, io.EOF
		}
		payload, err := nextSSEData(s.br, &s.data)
		if err != nil {
			s.done = true
			return nil, err
		}
		if payload == "[DONE]" {
			s.done = true
			return nil, io.EOF
		}
		raw := []byte(payload)
		_, events, derr := openai.DecodeChunk(raw, nil)
		if derr != nil {
			return nil, derr
		}
		events = s.adoptReasoning(raw, events)
		if len(events) > 0 {
			return events, nil
		}
	}
}

// nextSSEData reads one frame's data payload.
//
// It returns io.EOF at the end of the stream, and never returns an empty
// payload: a comment-only frame is a keep-alive and an empty one is padding,
// and neither is an event. A final frame with no terminating blank line is
// discarded rather than delivered — every one of these protocols terminates its
// stream explicitly, so an unterminated trailing frame is a truncated stream,
// and half an event is worse than none.
func nextSSEData(br *bufio.Reader, buf *strings.Builder) (string, error) {
	for {
		line, err := br.ReadString('\n')
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
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(strings.TrimPrefix(v, " "))
		}
		// Every other field (event:, id:, retry:) is not carried by these
		// shapes and is dropped rather than guessed at.

		if err != nil {
			buf.Reset()
			return "", io.EOF
		}
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
