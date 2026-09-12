package backend

import (
	"bytes"
	"encoding/json"
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
type responsesAdapter struct{}

func (responsesAdapter) endpoint(p *Provider, op Operation, _ string, _ bool) (string, error) {
	switch op {
	case OpChat, OpResponses:
		// No `/v1`. This surface is addressed at the base the operator
		// configured — chatgpt.com/backend-api/codex/responses — and joining a
		// version segment would produce a path the host does not serve.
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
		Model:       x.target.UpstreamModel,
		Loss:        x.loss,
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
	if eventStream(x.respType, body) {
		return collectResponses(body, x)
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

// eventStream reports whether a buffered body is a server-sent event stream.
//
// The label decides when it is present, and the first bytes decide when it is
// not: a JSON document begins with `{`, and an SSE stream begins with a field
// name. Both are consulted because the relay already tolerates a mislabelled
// stream ("an upstream that mislabels a real stream must not be turned away
// over a header"), and a buffered reading of the same host should not be
// stricter than a streaming one.
func eventStream(ctype string, body []byte) bool {
	if mt := ctype; mt != "" {
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = mt[:i]
		}
		if strings.EqualFold(strings.TrimSpace(mt), "text/event-stream") {
			return true
		}
	}
	b := bytes.TrimLeft(body, " \t\r\n")
	for _, field := range [...]string{"data:", "event:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(b, []byte(field)) {
			return true
		}
	}
	return false
}

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
//   - A stream that ends without its terminal event is
//     [CodeUpstreamStreamTruncated] and terminal for the same reason. What
//     arrived is a prefix of an answer, and a 200 carrying a prefix is the §10.5a
//     objection in its buffered form.
//   - A stream with no events at all is [CodeUpstreamShape]: the host answered
//     200 and said nothing, no generation began, and a sibling deployment is
//     exactly the right next hop.
func collectResponses(body []byte, x *exchange) (*decoded, error) {
	src, err := x.prov.ad.source(bytes.NewReader(body), x)
	if err != nil {
		return nil, server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"the upstream stream could not be read").WithCode(CodeUpstreamShape)
	}

	resp := &canonical.Response{Model: x.call.Model}
	choice := canonical.Choice{Message: canonical.Message{Role: canonical.RoleAssistant}}
	var text, thinking strings.Builder
	type call struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*call{}
	var order []int
	var usage canonical.Usage
	var haveUsage, terminal bool
	var seen int

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
			seen++
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
				switch blk.Kind {
				case canonical.KindText:
					text.WriteString(blk.Text)
				case canonical.KindThinking:
					thinking.WriteString(blk.Text)
				}
			}
			for _, tc := range e.Delta.ToolCalls {
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
	if seen == 0 {
		return nil, server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"the upstream answered 200 with an event stream carrying no events").
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
	sort.Ints(order)
	for _, i := range order {
		c := calls[i]
		// The fragments are joined verbatim. A call whose arguments never
		// streamed is handed on as an empty document, and [malformedToolCall]
		// in [Backend.convert] decides what that is — the same rule the stream
		// sinks apply, applied once, here in the same place as for a JSON
		// answer.
		choice.Message.Content = append(choice.Message.Content,
			canonical.ToolUseBlock(c.id, c.name, json.RawMessage(c.args.String())))
	}
	resp.Choices = []canonical.Choice{choice}
	if haveUsage {
		resp.Usage = &usage
	}
	return &decoded{resp: resp}, nil
}
