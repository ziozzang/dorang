package backend

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// eventSource reads an upstream stream as neutral events. next returns io.EOF
// when the stream ends.
//
// io.EOF is not by itself the end of a stream. A connection cut mid-generation
// ends the body too, and the difference between the two is the difference
// between an answer and half an answer — which is invisible to the client
// unless this package makes it visible. terminated is how a source reports
// which of the two it just saw.
type eventSource interface {
	next() ([]canonical.StreamEvent, error)
	// terminated reports that the FAMILY's own end-of-stream marker arrived. It
	// is consulted only after next returned io.EOF.
	//
	// A family that defines no such marker reports false and the relay falls
	// back to the stop event, which every family expresses and which is the
	// statement "the generation ended, and here is why". Reporting false is
	// therefore not a weaker answer, only a differently sourced one.
	terminated() bool
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
//
// # What it reports
//
// sent is the number of bytes that actually reached the client, which is the
// only honest source for [Result.FirstByteSent]: that flag closes fail-back for
// good (§7.6), so a stream that committed nothing must not set it and a stream
// that committed one byte must.
//
// A non-nil error is a [relayFailure] whenever the upstream answered 200 and
// then did not finish — an in-band error frame, or a body that stopped without
// its terminator. The client-facing half of both is unchanged: bytes already
// sent stay sent, and the failure is delivered in band because the status is
// spent. What changes is that the attempt is now reported as one.
func (b *Backend) relay(x *exchange, resp *http.Response, w http.ResponseWriter) (canonical.Usage, int64, error) {
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
	//   - compat.usage_chunk_choices asking for the strict shape (§3.3). On this
	//     path the usage chunk is the UPSTREAM's bytes, forwarded whole, so
	//     dorang does not decide its `choices` array — the backend does. An
	//     operator who selects `empty` is asking for a guarantee the relay cannot
	//     give, and honouring it on the crossing path while ignoring it here
	//     would be the same "loads and does nothing" defect one layer down. Only
	//     the non-default value pays: `stub` and unset both stay on the relay,
	//     which is what all but a handful of deployments run.
	if x.call.ClientAPI == catalog.APIOpenAIChat && x.prov.api == catalog.APIOpenAIChat &&
		x.names.Len() == 0 && x.call.Transform == nil &&
		x.call.UsageChunkChoices != openai.UsageChunkChoicesEmpty {
		// The scanner forwards bytes, so nothing on this path decodes a frame —
		// and nothing on it noticed an upstream that stopped answering either.
		// [relayWatch] sits between the scanner and the client and reads the two
		// facts that decide whether the exchange succeeded, at the same cost per
		// line the scanner's own usage hunt already pays.
		watch := &relayWatch{w: fw, secrets: x.secrets}
		sc := openai.NewScanner(watch, openai.ScannerOptions{
			From:         x.target.UpstreamModel,
			To:           x.call.Model,
			CollectUsage: true,
		})
		if _, err := io.Copy(sc, resp.Body); err != nil {
			return canonical.Usage{}, fw.n, err
		}
		if err := sc.Flush(); err != nil {
			return canonical.Usage{}, fw.n, err
		}
		if err := watch.flush(); err != nil {
			return canonical.Usage{}, fw.n, err
		}
		u, _ := sc.Usage()
		return u, fw.n, watch.failure(x.secrets)
	}

	src, err := x.prov.ad.source(resp.Body, x)
	if err != nil {
		return canonical.Usage{}, fw.n, err
	}
	sink, err := newEventSink(x.call, fw, b.now)
	if err != nil {
		return canonical.Usage{}, fw.n, err
	}
	// The transform composes into this one pass (§10.5): it takes decoded events
	// and returns decoded events, holding at most a placeholder's width minus one
	// byte across a frame boundary. Nil is the ordinary case and every call
	// through tr below is one nil check.
	var tr StreamTransform
	if x.call.Transform != nil {
		tr = x.call.Transform.Stream()
	}
	// fail is the upstream's own in-band failure, kept because the sink consumes
	// the event and the caller of this function needs the fact. sawStop is the
	// fallback end-of-stream marker for the families that define no terminator
	// frame; see [eventSource.terminated].
	var fail *relayFailure
	sawStop := false
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
			return sink.usage(), fw.n, err
		}
		for i := range batch {
			switch batch[i].Type {
			case canonical.EventStop:
				sawStop = true
			case canonical.EventError:
				// COMPATIBILITY 1.3 in both directions: the frame goes on to the
				// client below, exactly as it did before, AND the attempt is
				// recorded as the failure it is. Only the first of those two used
				// to happen.
				if fail == nil {
					fail = inBandFailure(batch[i].Err, x.secrets)
				}
				// DESIGN §10.6 rule 4, the response direction. The 4xx/5xx path
				// scrubs the upstream's body before any of it can be rendered
				// ([upstreamError]); this one re-encodes the upstream's own
				// sentence into a frame bound for the client and scrubbed
				// nothing. A backend that answers "invalid x-api-key: sk-…" mid
				// stream — and a helpful error message is all it takes — hands
				// the operator's provider credential to whichever tenant was
				// talking to it.
				scrubEvent(&batch[i], x.secrets)
			}
			if tr != nil {
				if flush := tr.Rewrite(&batch[i]); flush != nil {
					if err := sink.write(*flush); err != nil {
						return sink.usage(), fw.n, err
					}
				}
			}
			if err := sink.write(batch[i]); err != nil {
				return sink.usage(), fw.n, err
			}
		}
	}
	// Whatever the tail still holds is the caller's text: it only looked like the
	// beginning of a placeholder.
	if tr != nil {
		if flush := tr.Flush(); flush != nil {
			if err := sink.write(*flush); err != nil {
				return sink.usage(), fw.n, err
			}
		}
	}
	if fail != nil {
		// The sink already terminated the stream on the error event; closing it
		// again would append a second ending.
		return sink.usage(), fw.n, fail
	}
	if !sawStop && !src.terminated() {
		trunc := truncatedStream()
		// sink.close is deliberately NOT called. It is what synthesizes the
		// terminal chunk of COMPATIBILITY §4.4, and a broken stream given a
		// finish_reason of "stop" is a truncated answer handed to the client as a
		// complete one — the one failure here no client can detect. What goes out
		// instead is the truth, in band (1.3).
		//
		// Unless nothing went out at all: an upstream stream with no frames is an
		// empty body with no terminator (1.4), and inventing an error frame for a
		// client that received nothing would be dorang authoring the only bytes on
		// a response it can still fail back instead.
		if fw.n > 0 {
			_ = sink.write(canonical.StreamEvent{
				Type: canonical.EventError,
				Err: &canonical.Error{
					StatusCode: http.StatusBadGateway,
					Type:       "api_error",
					Message:    trunc.Error(),
					Code:       CodeUpstreamStreamTruncated,
				},
			})
		}
		return sink.usage(), fw.n, trunc
	}
	if err := sink.close(); err != nil {
		return sink.usage(), fw.n, err
	}
	return sink.usage(), fw.n, nil
}

// scrubEvent removes every credential from an in-band error before it is
// re-encoded for the client.
//
// It runs on the neutral form, which is why it covers every protocol pairing at
// once, and it runs only on the error event: no other event carries upstream
// prose, and scanning a content delta for the key would put a search over every
// token on the hot path to protect a field that cannot hold one.
func scrubEvent(ev *canonical.StreamEvent, secrets []string) {
	if len(secrets) == 0 || ev.Err == nil {
		return
	}
	ev.Err.Message = scrub(ev.Err.Message, secrets)
	ev.Err.Type = scrub(ev.Err.Type, secrets)
	ev.Err.Code = scrub(ev.Err.Code, secrets)
}

// newEventSink builds the writer for the caller's protocol.
//
// now is the BACKEND's clock, not the wire package's default. The two paths of
// one conversion must agree about `created` (DESIGN §10.7, and see
// [encodeClient]), and they cannot agree if one of them reads a clock a test
// can move and the other reads time.Now directly.
func newEventSink(c *Call, w io.Writer, now func() time.Time) (eventSink, error) {
	switch c.ClientAPI {
	case catalog.APIAnthropicMessages:
		return &anthropicSink{w: anthropic.NewStreamWriter(w, anthropic.StreamConfig{Model: c.Model})}, nil
	case catalog.APIOpenAIChat:
		return &openaiSink{w: openai.NewStreamWriter(w, openai.StreamConfig{
			Model:        c.Model,
			IncludeUsage: c.IncludeUsage,
			Now:          now,
			// COMPATIBILITY §3.3. Empty is UsageChunkChoicesStub inside
			// NewStreamWriter, so an unset call keeps the reference proxy's
			// shape and nothing branches for the default.
			UsageChunkChoices: c.UsageChunkChoices,
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

// terminated reports false, and the relay reads the stop event instead.
//
// This family's terminator frame is message_stop, and
// internal/wire/anthropic's StreamDecoder does not surface it: the frame
// carries no content and is dispatched to `return nil, nil`. The stop event is
// the available signal and it is a sound one — the protocol requires the
// message_delta that carries stop_reason BEFORE message_stop, so a stream that
// reached its ending has one and a stream cut mid-generation has neither.
func (s *anthropicSource) terminated() bool { return false }

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
	// sawDone records the [DONE] terminator (COMPATIBILITY 1.2), which is this
	// family's end-of-stream marker and the thing that separates a stream that
	// ended from one whose connection did.
	sawDone bool

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
			s.done, s.sawDone = true, true
			return s.flush()
		}
		raw := []byte(payload)
		// An in-band error envelope (COMPATIBILITY 1.3). It has no choices, so
		// openai.DecodeChunk decodes it to zero events and this reader used to
		// step straight over it to the [DONE] that follows — an upstream failure
		// that vanished on its way across the family boundary, leaving the client
		// with a stream that simply stopped and dorang with an attempt it called
		// a success.
		if ev, ok := errorFrame(raw); ok {
			// The error frame ends the stream; there is nothing after it worth
			// reading, and the [DONE] the shape puts behind it is not a
			// terminator for a message that failed.
			s.done = true
			return []canonical.StreamEvent{ev}, nil
		}
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

// terminated reports the [DONE] frame (COMPATIBILITY 1.2).
func (s *openaiSource) terminated() bool { return s.sawDone }

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

// errorMember is the prefilter for an in-band error envelope.
//
// Every shape COMPATIBILITY §11 recognizes spells the member the same way, and
// a chat-completion chunk does not carry it. The scan is the same per-line cost
// internal/wire/openai's Scanner already pays hunting for the usage frame, and
// like that one it is only paid until the thing it is looking for is found.
var errorMember = []byte(`"error"`)

// errorFrame reports whether one SSE data payload is an in-band error envelope
// and decodes it (COMPATIBILITY 1.3).
//
// The gate is a non-empty top-level `error` member — or `"object":"error"`,
// which is the flat envelope SGLang's OpenAI routes produce and which
// [server.Normalize] already recognizes as ShapeFlat. It is deliberately not
// the presence of the word: a model writing about errors produces frames full
// of it, and answering a request with a paragraph about error handling must
// neither end the stream nor be rewritten.
func errorFrame(payload []byte) (canonical.StreamEvent, bool) {
	if !bytes.Contains(payload, errorMember) {
		return canonical.StreamEvent{}, false
	}
	var probe struct {
		Error  json.RawMessage `json:"error"`
		Object string          `json:"object"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return canonical.StreamEvent{}, false
	}
	inner := bytes.TrimSpace(probe.Error)
	nested := len(inner) > 0 && !bytes.Equal(inner, jsonNull)
	if !nested && probe.Object != "error" {
		return canonical.StreamEvent{}, false
	}
	e, err := openai.DecodeError(payload, nil)
	if err != nil || e == nil {
		return canonical.StreamEvent{}, false
	}
	if e.StatusCode == 0 {
		e.StatusCode = http.StatusBadGateway
	}
	return canonical.StreamEvent{Type: canonical.EventError, Err: e.ToCanonical()}, true
}

var jsonNull = []byte("null")

// relayWatch is the byte relay's failure detector and its credential scrubber.
//
// The fast path forwards bytes without decoding a frame, which is what keeps
// the per-token path free of a JSON round trip — and is also why it could tell
// neither a completed stream from a broken one nor an ordinary frame from one
// carrying the operator's provider credential back out. This reads both facts
// from the line boundaries the wire already provides.
//
// # Why it inspects before it forwards
//
// The first version wrote the bytes and then looked at them, on the reasoning
// that a relay must not withhold. That is the wrong boundary. DESIGN §7.6 is
// about not RETRACTING output once it is committed; it says nothing about
// deciding what to commit, and DESIGN §10.6 rule 4 — "credentials are stripped
// in both directions" — is unconditional. An upstream that answers mid-stream
// with "invalid x-api-key: sk-…", which is a real message from a real server,
// had that sentence relayed verbatim to whichever tenant was talking to it. The
// 4xx/5xx path has scrubbed exactly this since it was written; the streaming
// path scrubbed nothing.
//
// # What it costs
//
// One line. Not one frame, not a window — the bytes between two newlines, which
// this type already accumulated in order to inspect them, so the ceiling is the
// same [maxStreamFrame] and the memory is the same buffer. Nothing else is
// delayed: an SSE client cannot act on half a `data:` line, so a line held until
// its own newline arrives is invisible to it. In practice it makes the relay
// write LESS often, not more: internal/wire/openai's Scanner emits a rewritten
// model line in three pieces, and those three now leave as one.
//
// Only an error frame is rewritten. Every other line is forwarded byte for byte,
// including the run of clean lines before and after one — which is the same
// batching rule the Scanner itself applies, for the same reason.
type relayWatch struct {
	w io.Writer
	// secrets is what went out on this request's credential headers, read back
	// from the headers themselves (see [collectSecrets]).
	secrets []string

	// line holds the current line until its newline arrives. Reused, so a steady
	// stream does not allocate after the first frame.
	//
	// This is a bound this type ADDED rather than one it found: nothing else in
	// the byte relay accumulates, so an upstream that never sends a newline would
	// otherwise grow the heap through the detector. over is the ceiling.
	line []byte
	// over marks a line that outgrew [maxStreamFrame]. What is held is forwarded
	// and the rest of that line is passed through uninspected, which is the same
	// degradation internal/wire/openai's Scanner applies to an oversized frame
	// and for the same reason: faithful forwarding beats an unbounded buffer. No
	// error envelope any vendor sends is four megabytes on one line.
	over bool

	// done records the [DONE] terminator (COMPATIBILITY 1.2).
	done bool
	// frame is the in-band error envelope, already scrubbed, kept whole so that
	// [server.Normalize] reads it rather than a second implementation of §11.
	frame []byte

	err error
}

// Write forwards p, holding back at most the trailing partial line.
//
// It always reports len(p) consumed on success. The held bytes are this type's
// responsibility from here, not the caller's, and a short count would make the
// Scanner believe a stream it fully handed over was only partly taken.
func (r *relayWatch) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	total := len(p)
	// start indexes the run of clean bytes waiting to be forwarded in one write.
	start, pos := 0, 0
	for pos < len(p) {
		i := bytes.IndexByte(p[pos:], '\n')
		if i < 0 {
			break
		}
		end := pos + i + 1 // the newline belongs to the line it terminates
		if len(r.line) > 0 || r.over {
			// The line began in an earlier write, so nothing before end can be
			// pending: the held bytes were never forwarded.
			r.hold(p[pos:end])
			r.emitHeld()
			start = end
		} else if repl, rewritten := r.inspect(p[pos:end]); rewritten {
			r.write(p[start:pos])
			r.write(repl)
			start = end
		}
		pos = end
	}
	r.write(p[start:pos])
	// The tail has no newline. It is the beginning of a line and is held rather
	// than forwarded, which is the whole of the delay this type introduces.
	if pos < len(p) {
		r.hold(p[pos:])
	}
	if r.err != nil {
		return 0, r.err
	}
	return total, nil
}

// flush emits a final line that never got its newline.
//
// COMPATIBILITY 1.5 leaves an unterminated trailing frame to the Scanner, which
// re-frames it and hands the newlines down; this exists so that a body which
// ends without either is not silently swallowed by the detector.
func (r *relayWatch) flush() error {
	if r.err == nil && (len(r.line) > 0 || r.over) {
		r.emitHeld()
	}
	return r.err
}

// hold accumulates the current line, degrading past the ceiling.
func (r *relayWatch) hold(b []byte) {
	if r.over {
		r.write(b)
		return
	}
	if len(r.line)+len(b) > maxStreamFrame {
		r.over = true
		r.write(r.line)
		r.line = r.line[:0]
		r.write(b)
		return
	}
	r.line = append(r.line, b...)
}

// emitHeld inspects and forwards the line that has just been completed.
func (r *relayWatch) emitHeld() {
	if r.over {
		// Already forwarded as it arrived, and never inspected.
		r.over = false
		return
	}
	if repl, rewritten := r.inspect(r.line); rewritten {
		r.write(repl)
	} else {
		r.write(r.line)
	}
	r.line = r.line[:0]
}

// inspect reads one complete line, including its newline, and returns a
// replacement when the line must not go out as it stands.
//
// The replacement is the scrubbed WHOLE line rather than a spliced payload: a
// credential cannot occur in `data: ` or in a line terminator, so the two edits
// have the same result and this one has no seams to get wrong.
func (r *relayWatch) inspect(line []byte) ([]byte, bool) {
	body := bytes.TrimRight(line, "\r\n")
	rest, ok := bytes.CutPrefix(body, []byte("data:"))
	if !ok {
		return nil, false
	}
	payload := bytes.TrimSpace(rest)
	if len(payload) == 0 {
		return nil, false
	}
	if string(payload) == "[DONE]" {
		r.done = true
		return nil, false
	}
	if _, isErr := errorFrame(payload); !isErr {
		return nil, false
	}
	scrubbed := scrub(string(payload), r.secrets)
	if r.frame == nil {
		// Copied, and scrubbed: payload aliases a buffer the caller reuses, and
		// what is recorded, logged and metered is read back out of this.
		r.frame = []byte(scrubbed)
	}
	if len(scrubbed) == len(payload) && scrubbed == string(payload) {
		// The ordinary case: the upstream echoed nothing. Nothing is rewritten,
		// and its own bytes reach the client exactly as they always did.
		return nil, false
	}
	return []byte(scrub(string(line), r.secrets)), true
}

func (r *relayWatch) write(b []byte) {
	if r.err != nil || len(b) == 0 {
		return
	}
	if _, err := r.w.Write(b); err != nil {
		r.err = err
	}
}

// failure is the verdict on the whole stream.
func (r *relayWatch) failure(secrets []string) error {
	if r.frame != nil {
		return normalizedFailure(r.frame, secrets)
	}
	if !r.done {
		return truncatedStream()
	}
	return nil
}

// flushWriter pushes every relayed chunk to the client immediately. Without it
// a streaming response is buffered and arrives as one block, which is not a
// stream.
//
// It also counts, because the count is what [Result.FirstByteSent] means: not
// "this exchange was a stream" but "the client has already seen output", which
// is the fact §7.6 refuses to fail back across.
type flushWriter struct {
	w http.ResponseWriter
	n int64
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	f.n += int64(n)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
