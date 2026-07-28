package backend

import (
	"io"
	"net/http"

	"github.com/ziozzang/dorang/internal/wire/rerank"
)

// cohereAdapter is the vendor's own v2 surface.
//
// Only rerank is served, and the two refusals below are the interesting part of
// this file.
//
// # Why not chat
//
// The vendor's /v2/chat is a third chat protocol — its own message shape, its
// own tool schema, its own streaming event names. The catalog's note records
// the alternative the vendor themselves publish: an OpenAI-compatibility route
// at https://api.cohere.ai/compatibility/v1, which a deployment reaches by
// setting `api: openai-chat` and that base URL. Refusing here points the
// operator at a route that works today rather than at a wire adapter that does
// not exist.
//
// # Why not embeddings
//
// /v2/embed requires `input_type` — search_document, search_query,
// classification or clustering — and the vector it returns DIFFERS by that
// value. There is no OpenAI equivalent, so dorang would have to pick one on the
// caller's behalf, and picking wrong does not fail: it silently returns vectors
// from a different embedding space than the ones already in the operator's
// index. That is the "wrong invoice" class of defect DESIGN §4.4 warns about
// with the numbers swapped for vectors, and it is exactly the kind of value
// §4.3 forbids inventing.
type cohereAdapter struct{}

func (cohereAdapter) endpoint(p *Provider, op Operation, _ string, _ bool) (string, error) {
	switch op {
	case OpRerank:
		// Versioned per route, not per host: /v2/rerank joins onto the bare
		// host, and inserting a "/v1" the way the OpenAI shapes do would
		// address a route that does not exist.
		return join(p.base, "/v2/rerank"), nil
	case OpChat:
		return "", noOperation("cohere", op,
			"dorang has no adapter for the native v2 chat surface; the vendor's OpenAI-compatibility route "+
				"(api: openai-chat with base_url https://api.cohere.ai/compatibility/v1) is served today")
	default:
		return "", noOperation("cohere", op,
			"the v2 embed surface requires input_type, which has no equivalent in the request dorang received; "+
				"choosing one would return vectors from a different embedding space with no error")
	}
}

func (cohereAdapter) credential(secret string, h http.Header) { bearer(secret, h) }

func (cohereAdapter) headers(http.Header) {}

func (cohereAdapter) encode(x *exchange) ([]byte, error) {
	return rerank.MarshalRequest(x.call.Rerank, &rerank.EncodeOptions{
		Model:  x.target.UpstreamModel,
		Flavor: rerank.FlavorCohere,
	})
}

func (cohereAdapter) decode(body []byte, x *exchange) (*decoded, error) {
	return decodeRerank(body, rerank.FlavorCohere, x)
}

func (cohereAdapter) source(io.Reader, *exchange) (eventSource, error) {
	return nil, noOperation("cohere", OpChat, "this surface does not stream")
}
