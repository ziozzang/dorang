package app

import (
	"bufio"
	"fmt"
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

// eventSink writes neutral events in the client's protocol.
type eventSink interface {
	write(canonical.StreamEvent) error
	close() error
	usage() canonical.Usage
}

func newEventSource(api catalog.API, r io.Reader) (eventSource, error) {
	switch api {
	case catalog.APIAnthropicMessages:
		return &anthropicSource{d: anthropic.NewStreamDecoder(r, nil)}, nil
	case catalog.APIOpenAIChat:
		return &openaiSource{br: bufio.NewReaderSize(r, 8<<10)}, nil
	}
	return nil, fmt.Errorf("app: no streaming decoder for wire adapter %q", api)
}

func newEventSink(c *call, w http.ResponseWriter) (eventSink, error) {
	fw := &flushWriter{w: w}
	switch c.clientAPI {
	case catalog.APIAnthropicMessages:
		return &anthropicSink{w: anthropic.NewStreamWriter(fw, anthropic.StreamConfig{Model: c.model})}, nil
	case catalog.APIOpenAIChat:
		return &openaiSink{w: openai.NewStreamWriter(fw, openai.StreamConfig{
			Model:        c.model,
			IncludeUsage: c.allowUsg,
		})}, nil
	}
	return nil, fmt.Errorf("app: no streaming encoder for wire adapter %q", c.clientAPI)
}

// anthropicSource wraps the package's own stream decoder.
type anthropicSource struct{ d *anthropic.StreamDecoder }

func (s *anthropicSource) next() ([]canonical.StreamEvent, error) { return s.d.Next() }

// openaiSource reads chat-completion SSE frames.
//
// internal/wire/openai exposes a chunk decoder and a relay scanner but no frame
// reader, because the relay path never needs one — it forwards bytes. A frame
// reader is only needed when the stream has to CROSS families, which is this
// path, so it lives here rather than being pushed into that package.
type openaiSource struct {
	br   *bufio.Reader
	data strings.Builder
	done bool
}

func (s *openaiSource) next() ([]canonical.StreamEvent, error) {
	if s.done {
		return nil, io.EOF
	}
	for {
		line, err := s.br.ReadString('\n')
		if line == "" && err != nil {
			s.done = true
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")

		if trimmed == "" { // end of frame
			payload := strings.TrimSpace(s.data.String())
			s.data.Reset()
			switch {
			case payload == "":
				// A comment-only or empty frame: a keep-alive, not an event.
			case payload == "[DONE]":
				s.done = true
				return nil, io.EOF
			default:
				_, events, derr := openai.DecodeChunk([]byte(payload), nil)
				if derr != nil {
					return nil, derr
				}
				if len(events) > 0 {
					return events, nil
				}
			}
			if err != nil {
				s.done = true
				return nil, io.EOF
			}
			continue
		}

		if strings.HasPrefix(trimmed, ":") {
			// An SSE comment. Keep-alives arrive as one, and they carry nothing.
		} else if v, ok := strings.CutPrefix(trimmed, "data:"); ok {
			if s.data.Len() > 0 {
				s.data.WriteByte('\n')
			}
			s.data.WriteString(strings.TrimPrefix(v, " "))
		}
		// Every other field (event:, id:, retry:) is not carried by the
		// chat-completions shape and is dropped rather than guessed at.

		if err != nil {
			s.done = true
			return nil, io.EOF
		}
	}
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
