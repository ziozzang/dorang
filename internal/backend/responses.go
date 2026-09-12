package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/openai"
)

// responsesAdapter addresses `/responses` and nothing else.
//
// # Why it is separate from [openaiAdapter]
//
// The chat adapter serves `api: openai-responses` by sending it to
// `/v1/chat/completions`, and that is correct wherever a host serves both
// routes: the Responses route is offered by a strict subset of the deployments
// the chat route is, so the chat address reaches more of them. Measured against
// the operator's own plans, Ollama Cloud and a local llama.cpp serve all three
// surfaces and the qwen token plan serves chat and responses — for every one of
// those the existing behaviour is right and this adapter is not used.
//
// It is wrong for a host that serves ONLY `/responses`. Measured against the
// ChatGPT Codex surface: `POST /backend-api/codex/chat/completions` answers
// 403, `POST /backend-api/codex/responses` answers 400 for a body problem. The
// chat adapter's own comment explains its routing with "the hosts declaring
// this kind serve BOTH routes" — true when it was written, false for this host,
// and a conclusion built on it fails here rather than degrading.
//
// So the split is on what the HOST serves, which only the catalog kind knows,
// and not on the wire shape, which both share.
//
// # Two fields this surface requires
//
// `store` and `stream` are mandatory and refused separately:
//
//	store:false + stream:true  -> 200
//	stream:true alone          -> 400  Store must be set to false
//	store:false alone          -> 400  Stream must be set to true
//	neither                    -> 400
//
// They are not options an operator forgot; they are the contract. [Provider]
// carries them so a deployment states them once rather than every caller
// discovering the 400 for themselves, and the encoder applies them below.
type responsesAdapter struct {
	// versioned addresses the route under the versioned base, which is where
	// a host that serves BOTH routes keeps it (ollama.com/v1/responses). The
	// Responses-only host is the other case: no `/v1`, the route at the bare
	// base the operator configured — chatgpt.com/backend-api/codex/responses —
	// and joining a version segment would produce a path it does not serve.
	versioned bool
}

func (a responsesAdapter) endpoint(p *Provider, op Operation, _ string, _ bool) (string, error) {
	switch op {
	case OpChat, OpResponses:
		if a.versioned {
			return joinVersioned(p.base, "/v1", pathResponses), nil
		}
		return join(p.base, pathResponses), nil
	case OpEmbeddings, OpRerank, OpCompletions, OpModerations:
		return "", unsupportedProvider{
			code: "responses_only_surface",
			reason: "this provider serves the Responses surface only: it has no " +
				"chat-completions, embeddings, rerank or moderations route. " +
				"Point a separate provider at a host that serves them.",
		}
	}
	return "", unsupportedProvider{
		code:   "responses_only_surface",
		reason: "this operation has no route on a Responses-only provider",
	}
}

func (responsesAdapter) credential(secret string, h http.Header) {
	h.Set("Authorization", "Bearer "+secret)
}

func (responsesAdapter) headers(http.Header) {}

func (responsesAdapter) encode(x *exchange) ([]byte, error) {
	opt := &openai.EncodeOptions{
		Model: x.target.UpstreamModel,
		Loss:  x.loss,
		// The same registry the chat adapter hands its encoder: a tool name
		// over the wire limit is shortened reversibly (COMPATIBILITY 5.3) and
		// restored by the decoders below. Without it the encoder sends the
		// caller's name unchanged and this host refuses the request.
		ToolNames:   x.toolNames(),
		ForceStream: x.prov.ResponsesForceStream,
		StoreFalse:  x.prov.ResponsesStoreFalse,
	}
	return openai.MarshalResponsesRequest(x.req, opt)
}

// decode converts a successful answer, whichever transport it arrived on.
//
// A Responses-only host answers a buffered caller with an event stream, because
// it refuses `stream: false` outright. That is the host's contract and not the
// caller's choice, so the two are kept apart: the transport is read for what it
// is, and the SHAPE the caller receives is decided where it is for every other
// adapter — in [Backend.convert], from the neutral form this returns. The
// collected answer therefore passes through the same served-model note, tool
// argument check, §10.5b transform and client encoder as a JSON one, which is
// the point of returning a [decoded] rather than rendered bytes.
func (responsesAdapter) decode(body []byte, x *exchange) (*decoded, error) {
	// Normalised ONCE, here, so that detection and both decoders read the same
	// bytes: a byte-order mark stripped in the detector alone left the JSON
	// decoder refusing a document the detector had just accepted.
	body = bytes.TrimPrefix(body, utf8BOM)
	if eventStream(x.respType, body) {
		return collectResponses(body, x)
	}
	// The chat decoder's own two-step rule. A body that is not JSON at all is
	// a decode failure, terminal: on a forced stream it is most often a
	// document cut off by the transport, and what was cut off was generated
	// and billed. A body that IS JSON and is not a response of this family —
	// an error envelope on a 200, `{}`, `null` — is [openai.ErrNotAResponse],
	// which the shape rule offers to the fallback chain because nothing was
	// generated.
	if !json.Valid(body) {
		return nil, errNotJSON
	}
	if !openai.IsResponsesAnswer(body) {
		return nil, openai.ErrNotAResponse
	}
	// A document that reports its own failure is a failure, on this path as
	// on the stream: the same scrubbed envelope `response.failed` produces.
	if err := failedDocument(body, x); err != nil {
		return nil, err
	}
	resp, err := openai.DecodeResponsesResponse(body, &openai.DecodeOptions{
		Model: x.call.Model, ToolNames: x.names,
	})
	if err != nil {
		return nil, err
	}
	x.usageExtra = resp.UsageExtra
	return &decoded{resp: resp}, nil
}

// errNotJSON is a buffered body that is not a JSON document.
var errNotJSON = errors.New("openai: the body is not JSON")

// failedDocument refuses a Responses document whose status is failed or
// cancelled, carrying the upstream's own reason scrubbed of the credential.
func failedDocument(body []byte, x *exchange) error {
	var probe struct {
		Status string `json:"status"`
		Error  *struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return nil
	}
	if probe.Status != openai.StatusFailed && probe.Status != openai.StatusCancelled {
		return nil
	}
	e := &canonical.Error{Message: "the upstream reported the response " + probe.Status}
	if probe.Error != nil {
		if probe.Error.Message != "" {
			e.Message = probe.Error.Message
		}
		e.Type, e.Code = probe.Error.Type, probe.Error.Code
	}
	out := inBandFailure(e, x.secrets).err
	out.Message = "the upstream answered 200 and reported the response " + probe.Status
	out.Code = CodeUpstreamFailed
	return out
}

// eventStream reports whether a buffered body is a server-sent event stream.
//
// The BODY decides, and the label only breaks a tie. A JSON document begins
// with `{` and no SSE stream does, so a JSON answer under a `text/event-stream`
// label is read as the JSON it is — reading it as a stream would find no
// events and offer a completed, billed generation to the fallback chain as
// "the upstream said nothing". A body that begins with an SSE field name is a
// stream whatever the label says, because the relay already tolerates a
// mislabelled stream and a buffered reading of the same host should not be
// stricter. The label decides only for a body that begins with neither: a
// comment line, a stream of nothing.
//
// A byte-order mark is skipped first. It is not part of either format and a
// proxy that re-encoded the body may have put one there.
func eventStream(ctype string, body []byte) bool {
	b := bytes.TrimLeft(bytes.TrimPrefix(body, utf8BOM), " \t\r\n")
	if len(b) > 0 && b[0] == '{' {
		return false
	}
	// A comment line (`: keepalive`) is an SSE field too, and a stream that
	// opens with one is still a stream; no JSON document begins with a colon.
	for _, field := range [...]string{"data:", "event:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(b, []byte(field)) {
			return true
		}
	}
	mt := ctype
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return strings.EqualFold(strings.TrimSpace(mt), "text/event-stream")
}

// utf8BOM is the byte-order mark; see [eventStream].
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func (responsesAdapter) source(r io.Reader, x *exchange) (eventSource, error) {
	return &responsesSource{d: openai.NewResponsesStreamDecoder(r, &openai.DecodeOptions{
		Model: x.call.Model, ToolNames: x.names,
	})}, nil
}

// responsesSource adapts the Responses stream decoder to [eventSource].
type responsesSource struct {
	d *openai.ResponsesStreamDecoder
}

func (s *responsesSource) next() ([]canonical.StreamEvent, error) { return s.d.Next() }

// terminated reports whether this family's own end-of-stream marker arrived.
//
// It does not have one. `response.completed` is a terminal EVENT rather than a
// marker outside the event stream, and the decoder already turns it into a stop
// — so reporting false lets the relay fall back to that stop, which is the
// statement "the generation ended, and here is why". A family with no marker
// reporting false is not a weaker answer, only a differently sourced one.
func (s *responsesSource) terminated() bool { return false }

// collectResponses reads a whole event stream into one neutral answer.
//
// Nothing is invented. Text and reasoning are concatenated in arrival order,
// tool-call fragments are joined by the index that COMPATIBILITY §5.1 requires
// on each, and the stop reason and usage are the ones the terminal event
// carried — a stream that never reported usage yields a response with none,
// rather than a zero that would read as a request that cost nothing.
//
// # What is refused, and whether a fallback may follow
//
// The failures are returned as [server.Error] values so that [Backend.convert]
// passes them through with their codes intact, because the code is what decides
// the fallback:
//
//   - An error EVENT is the upstream failing inside a generation it had begun.
//     It is [CodeUpstreamStreamError], scrubbed of the credential exactly as the
//     relay scrubs it, and it is not offered to the fallback chain: the host
//     bills the generation it started whether or not it finished, and a hop to
//     a sibling deployment would buy a second one.
//   - A stream that ends without its terminal event after the generation has
//     produced something — a content or reasoning fragment, a tool-call
//     fragment, a refusal, a count — is [CodeUpstreamStreamTruncated] and
//     terminal for the same reason. What arrived is a prefix of an answer, and
//     a 200 carrying a prefix is the §10.5a objection in its buffered form.
//   - A stream that ends before anything was generated — no events at all, or
//     only the opening `response.created` — is [CodeUpstreamShape]: the host
//     answered 200 and produced nothing, and a sibling deployment is exactly
//     the right next hop. This is the relay's own rule seen from the buffered
//     side: the relay offers a truncated stream to the chain when nothing
//     reached the client, and the chat sink writes nothing for the start
//     event. (The Anthropic sink writes `message_start` for it, so a relayed
//     stream to THAT caller is committed one event earlier — the committed-
//     byte rule, which a buffered answer never meets.)
//   - A `data:` payload that is not JSON is [CodeUpstreamDecode] and terminal:
//     the bytes before it may be a real prefix, and the decoder refuses to
//     skip a frame and hand on an answer with a hole in it.
func collectResponses(body []byte, x *exchange) (*decoded, error) {
	src, err := x.ad.source(bytes.NewReader(body), x)
	if err != nil {
		return nil, server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"the upstream stream could not be read").WithCode(CodeUpstreamShape)
	}

	resp := &canonical.Response{Model: x.call.Model}
	choice := canonical.Choice{Message: canonical.Message{Role: canonical.RoleAssistant}}
	var text, thinking, refusal strings.Builder
	type call struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*call{}
	var order []int
	var usage canonical.Usage
	var haveUsage, terminal bool
	// progress records that the generation produced something, which is what
	// decides whether a cut-off stream may be retried elsewhere.
	var progress bool

	for {
		evs, err := src.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A frame the decoder could not read. The bytes before it may be a
			// real prefix of an answer, so this is not retried either.
			return nil, server.NewError(http.StatusBadGateway, server.TypeAPIError,
				"the upstream stream could not be decoded: "+scrub(err.Error(), x.secrets)).
				WithCode(CodeUpstreamDecode)
		}
		for i := range evs {
			e := &evs[i]
			if e.ID != "" {
				resp.ID = e.ID
			}
			if e.Model != "" {
				resp.ServedModel = e.Model
			}
			if e.Created != 0 {
				resp.Created = e.Created
			}
			switch e.Type {
			case canonical.EventError:
				// The same envelope the relay writes for the same event, which
				// is what keeps the credential out of it: the upstream quotes
				// the bearer it rejected into its message, and this text is
				// about to leave dorang.
				return nil, inBandFailure(e.Err, x.secrets).err
			case canonical.EventUsage:
				progress = true
				if e.Usage != nil {
					usage, haveUsage = *e.Usage, true
				}
				if e.UsageExtra != nil {
					x.usageExtra = e.UsageExtra
				}
			case canonical.EventStop:
				terminal = true
				if e.Delta.StopReason != "" {
					choice.StopReason = e.Delta.StopReason
				}
			}
			for _, blk := range e.Delta.Content {
				progress = true
				switch blk.Kind {
				case canonical.KindText:
					text.WriteString(blk.Text)
				case canonical.KindThinking:
					thinking.WriteString(blk.Text)
				}
			}
			if e.Delta.Refusal != "" {
				progress = true
				refusal.WriteString(e.Delta.Refusal)
			}
			for _, tc := range e.Delta.ToolCalls {
				progress = true
				c, ok := calls[tc.Index]
				if !ok {
					c = &call{}
					calls[tc.Index] = c
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					c.id = tc.ID
				}
				if tc.Name != "" {
					c.name = tc.Name
				}
				c.args.WriteString(tc.Arguments)
			}
		}
	}
	if !terminal && !progress {
		return nil, server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"the upstream answered 200 with an event stream that produced nothing").
			WithCode(CodeUpstreamShape)
	}
	if !terminal {
		return nil, server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"the upstream stream ended before its terminal event; what arrived is a "+
				"prefix of an answer, not an answer").
			WithCode(CodeUpstreamStreamTruncated)
	}

	if s := thinking.String(); s != "" {
		choice.Message.Content = append(choice.Message.Content, canonical.ThinkingBlock(s, ""))
	}
	if s := text.String(); s != "" {
		choice.Message.Content = append(choice.Message.Content, canonical.TextBlock(s))
	}
	if s := refusal.String(); s != "" {
		choice.Message.Refusal = s
		// The JSON decoder's own ordering: a turn that ended in a call is a
		// call, a turn with nothing but a refusal is a refusal.
		if choice.StopReason == canonical.StopEndTurn && text.Len() == 0 && len(order) == 0 {
			choice.StopReason = canonical.StopRefusal
		}
	}
	sort.Ints(order)
	for _, i := range order {
		c := calls[i]
		// The fragments are joined verbatim, an empty document included. A
		// call whose arguments never streamed is rendered as an empty object
		// by EVERY client encoder (openai chat and responses, anthropic), which
		// is where that rule lives; a second copy here would be a second place
		// for it to drift. [malformedToolCall] in [Backend.convert] skips the
		// empty case for the same reason.
		choice.Message.Content = append(choice.Message.Content,
			canonical.ToolUseBlock(c.id, c.name, json.RawMessage(c.args.String())))
	}
	resp.Choices = []canonical.Choice{choice}
	if haveUsage {
		resp.Usage = &usage
	}
	return &decoded{resp: resp}, nil
}
