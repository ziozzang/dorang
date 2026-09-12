package backend

import (
	"net/url"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// vertexAdapter is the Gemini wire shape on Vertex AI.
//
// Everything the model sees is the Gemini adapter's — the request, the
// response, the stream. What differs is where the route lives and who is
// asking: `/v1/projects/{project}/locations/{location}/publishers/google/
// models/{model}:generateContent` on the location's regional host, with a
// Google OAuth bearer (a service-account key through `auth: oauth`,
// `format: gcp-service-account`) or an API key in x-goog-api-key.
//
// Not measured against a live project — the operator has none. The path is
// Google's published one and is pinned against a fake.
type vertexAdapter struct{ geminiAdapter }

func (vertexAdapter) endpoint(p *Provider, op Operation, model string, stream bool) (string, error) {
	if op != OpChat {
		return "", noOperation("vertex", op,
			"this adapter serves generateContent only; embeddings and rerank are separate surfaces with "+
				"different request shapes")
	}
	method := ":generateContent"
	if stream {
		method = ":streamGenerateContent?alt=sse"
	}
	base := trimBase(p.base)
	if !hasVersionSegment(base) {
		base += "/v1"
	}
	return base + "/projects/" + url.PathEscape(p.vertexProject) +
		"/locations/" + url.PathEscape(p.vertexLocation) +
		"/publishers/google/models/" + url.PathEscape(model) + method, nil
}

// vertexHost is the regional host a location implies when no base_url is
// configured. `global` is the one location without a regional prefix.
func vertexHost(location string) string {
	if location == "" {
		return ""
	}
	if location == "global" {
		return "https://aiplatform.googleapis.com"
	}
	return "https://" + location + "-aiplatform.googleapis.com"
}

// checkVertexParams requires the two things the path cannot be built without
// on the vertex kind, and refuses them anywhere else (CONFIG §23.2).
func checkVertexParams(s Spec, api catalog.API) error {
	if api == catalog.APIVertex {
		if s.VertexProject == "" || s.VertexLocation == "" {
			return errVertexNeedsProject
		}
		return nil
	}
	if s.VertexProject != "" {
		return errParamOnWrongKind("project", s.Kind, string(api), "the Vertex AI project the route is scoped to, which no other host takes")
	}
	if s.VertexLocation != "" {
		return errParamOnWrongKind("location", s.Kind, string(api), "the Vertex AI location the route is scoped to, which no other host takes")
	}
	return nil
}
