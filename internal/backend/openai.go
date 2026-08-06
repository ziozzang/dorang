package backend

import (
	"io"
	"net/http"

	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/internal/wire/rerank"
)

// openaiAdapter is the chat-completions wire shape: ten of the catalogued kinds
// and every OpenAI-compatible self-hosted engine.
//
// It also serves the openai-responses KIND, deliberately, and that is worth
// stating because it looks like an omission. internal/wire/openai gained a
// Responses request/response encoder, but no Responses STREAM decoder — that
// surface's events are a typed sequence (response.output_text.delta and
// friends) with nothing in common with a chat chunk. Routing the kind to
// /v1/responses today would trade a working streaming deployment for a
// non-streaming one. DESIGN §4.3 records that the hosts declaring this kind
// serve BOTH routes, so the chat route is a correct address for them, and
// switching the kind over is one line in adapterFor the day a stream decoder
// exists.
type openaiAdapter struct{}

func (openaiAdapter) endpoint(p *Provider, op Operation, _ string, _ bool) (string, error) {
	switch op {
	case OpChat:
		return joinVersioned(p.base, "/v1", pathChatCompletions), nil
	case OpEmbeddings:
		return joinVersioned(p.base, "/v1", pathEmbeddings), nil
	case OpRerank:
		// vLLM, SGLang, TEI and Infinity all serve this route; a hosted OpenAI
		// endpoint does not, and answers 404, which is a clearer signal than
		// dorang refusing on its behalf.
		return joinVersioned(p.base, "/v1", pathRerank), nil
	case OpCompletions:
		return joinVersioned(p.base, "/v1", pathCompletions), nil
	case OpResponses:
		// dorang's own /v1/responses FRONTEND, deliberately sent to the chat
		// route. See the comment on [openaiAdapter] for the reasoning; the short
		// version is that /v1/responses is served by a strict subset of the
		// deployments /v1/chat/completions is, and a caller's Responses request
		// is already in the neutral form by the time it gets here — so the chat
		// route reaches every deployment and the Responses route reaches some.
		// The answer is still rendered as a Response object, by encodeT1Client.
		return joinVersioned(p.base, "/v1", pathChatCompletions), nil
	case OpModerations:
		return joinVersioned(p.base, "/v1", pathModerations), nil
	case OpSpeech:
		return joinVersioned(p.base, "/v1", pathSpeech), nil
	case OpTranscription:
		return joinVersioned(p.base, "/v1", pathTranscriptions), nil
	case OpTranslation:
		return joinVersioned(p.base, "/v1", pathTranslations), nil
	case OpImageGenerate:
		return joinVersioned(p.base, "/v1", pathImageGenerate), nil
	case OpImageEdit:
		return joinVersioned(p.base, "/v1", pathImageEdit), nil
	case OpImageVariation:
		return joinVersioned(p.base, "/v1", pathImageVariation), nil
	default:
		return "", noOperation("openai-chat", op,
			"this family has no token-count route; the count is only available where the model is served by anthropic-messages")
	}
}

func (openaiAdapter) credential(secret string, h http.Header) { bearer(secret, h) }

func (openaiAdapter) headers(http.Header) {}

func (openaiAdapter) encode(x *exchange) ([]byte, error) {
	switch x.call.Op {
	case OpEmbeddings:
		// Embeddings has no neutral representation, so it is relayed rather
		// than converted: the body goes upstream with only the model name
		// replaced, and the answer comes back with the client's name restored.
		return relayRequest(x.call.Body, x.target.UpstreamModel)
	case OpRerank:
		return rerank.MarshalRequest(x.call.Rerank, &rerank.EncodeOptions{
			Model:  x.target.UpstreamModel,
			Flavor: rerank.FlavorGeneric,
		})
	case OpChat, OpResponses:
		return openai.MarshalRequest(x.req, &openai.EncodeOptions{
			Model: x.target.UpstreamModel,
			// The set the §10.1 gate cleared this request against; see the same
			// line in the anthropic adapter.
			Capabilities: x.capabilities(),
			// Where the located losses go. Passing nothing here is what left a
			// thinking block's SIGNATURE dropped in silence on every crossing into
			// this family: the encoder raises that downgrade while HOLDING
			// CapThinkingBlocks, so no capability mask outside can reconstruct it.
			Loss: x.loss,
			// The SAME registry the decoders read. Shortening into a mapping
			// nobody keeps is what turns COMPATIBILITY 5.3 into a one-way
			// destruction of any tool name over 64 bytes.
			ToolNames: x.toolNames(),
			// COMPATIBILITY §5.5. The two spellings are not interchangeable and
			// no default is right everywhere, so the OPERATOR selects it per
			// deployment; empty keeps max_tokens, which is what every request
			// has always carried. This field had both constants and a working
			// consumer in the encoder, and nothing that could reach either.
			MaxTokensField: x.target.MaxTokensField,
		})
	}
	return encodeT1(x)
}

func (openaiAdapter) decode(body []byte, x *exchange) (*decoded, error) {
	switch x.call.Op {
	case OpEmbeddings:
		return relayResponse(body, x.call.Model)
	case OpRerank:
		return decodeRerank(body, rerank.FlavorGeneric, x)
	case OpChat, OpResponses:
		resp, err := openai.DecodeResponse(body, &openai.DecodeOptions{
			Model: x.call.Model, ToolNames: x.names,
		})
		if err != nil {
			return nil, err
		}
		// The unmodelled counts ride the exchange to the Result, and from there
		// to pricing. Some of them are charges — a server-side web search is
		// billed per search — and none is reachable from canonical.Usage.
		x.usageExtra = resp.UsageExtra
		return &decoded{resp: resp}, nil
	}
	return decodeT1(body, x)
}

func (openaiAdapter) source(r io.Reader, x *exchange) (eventSource, error) {
	return newOpenAISource(r, x), nil
}

// decodeRerank converts a rerank answer and renders it in the one client-facing
// rerank shape. The three dialects differ in which billing block they fill, not
// in what a client reads, so there is a single answer shape and the adapter
// finishes the conversion here rather than through the chat encoder.
func decodeRerank(body []byte, flavor rerank.Flavor, x *exchange) (*decoded, error) {
	cresp, err := rerank.DecodeResponse(body, flavor, x.call.Model)
	if err != nil {
		return nil, err
	}
	out, err := rerank.MarshalResponse(cresp)
	if err != nil {
		return nil, err
	}
	d := &decoded{raw: out}
	if cresp.Usage != nil {
		d.usage = *cresp.Usage
	}
	return d, nil
}
