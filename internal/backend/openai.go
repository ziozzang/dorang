package backend

import (
	"bufio"
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
		return joinVersioned(p.base, "/v1", pathResponses), nil
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
	case OpChat:
		return openai.MarshalRequest(x.req, &openai.EncodeOptions{Model: x.target.UpstreamModel})
	}
	return encodeT1(x)
}

func (openaiAdapter) decode(body []byte, x *exchange) (*decoded, error) {
	switch x.call.Op {
	case OpEmbeddings:
		return relayResponse(body, x.call.Model)
	case OpRerank:
		return decodeRerank(body, rerank.FlavorGeneric, x)
	case OpChat:
		resp, err := openai.DecodeResponse(body, &openai.DecodeOptions{Model: x.call.Model})
		if err != nil {
			return nil, err
		}
		return &decoded{resp: resp}, nil
	}
	return decodeT1(body, x)
}

func (openaiAdapter) source(r io.Reader, x *exchange) (eventSource, error) {
	return &openaiSource{br: bufio.NewReaderSize(r, 8<<10), engine: x.prov.engine}, nil
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
