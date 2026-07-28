package backend

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/ziozzang/dorang/internal/wire/rerank"
)

// jinaAdapter is the embedding vendor's surface: /v1/embeddings and /v1/rerank.
//
// Embeddings here is close enough to the OpenAI shape to relay — same route,
// same `model` and `input` members, same data/usage answer — with one
// difference that is not cosmetic: `input` must be an ARRAY. OpenAI accepts a
// bare string and a great deal of client code sends one, so a relay that did
// not normalize it would turn a request that works against OpenAI into a 422
// against this vendor, for a reason nothing in the error names.
type jinaAdapter struct{}

func (jinaAdapter) endpoint(p *Provider, op Operation, _ string, _ bool) (string, error) {
	switch op {
	case OpEmbeddings:
		return joinVersioned(p.base, "/v1", pathEmbeddings), nil
	case OpRerank:
		return joinVersioned(p.base, "/v1", pathRerank), nil
	default:
		return "", noOperation("jina", op, "this vendor serves embeddings and rerank only")
	}
}

func (jinaAdapter) credential(secret string, h http.Header) { bearer(secret, h) }

func (jinaAdapter) headers(http.Header) {}

func (jinaAdapter) encode(x *exchange) ([]byte, error) {
	if x.call.Op == OpRerank {
		return rerank.MarshalRequest(x.call.Rerank, &rerank.EncodeOptions{
			Model:  x.target.UpstreamModel,
			Flavor: rerank.FlavorJina,
		})
	}
	return jinaEmbeddingRequest(x.call.Body, x.target.UpstreamModel)
}

func (jinaAdapter) decode(body []byte, x *exchange) (*decoded, error) {
	if x.call.Op == OpRerank {
		return decodeRerank(body, rerank.FlavorJina, x)
	}
	return relayResponse(body, x.call.Model)
}

func (jinaAdapter) source(io.Reader, *exchange) (eventSource, error) {
	return nil, noOperation("jina", OpChat, "this surface does not stream")
}

// jinaEmbeddingRequest replaces the model and lifts a bare string input into a
// one-element array. Every other member survives byte for byte.
func jinaEmbeddingRequest(body []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errNotAnObject
	}
	enc, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = enc
	if in, ok := obj["input"]; ok && len(in) > 0 && in[0] == '"' {
		wrapped := make([]byte, 0, len(in)+2)
		wrapped = append(wrapped, '[')
		wrapped = append(wrapped, in...)
		obj["input"] = append(wrapped, ']')
	}
	return json.Marshal(obj)
}
