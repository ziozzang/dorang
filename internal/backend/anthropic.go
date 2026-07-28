package backend

import (
	"io"
	"net/http"

	"github.com/ziozzang/dorang/internal/wire/anthropic"
)

// anthropicAdapter is the /v1/messages wire shape.
type anthropicAdapter struct{}

func (anthropicAdapter) endpoint(p *Provider, op Operation, _ string, _ bool) (string, error) {
	switch op {
	case OpChat:
		return joinVersioned(p.base, "/v1", pathMessages), nil
	case OpCountTokens:
		return joinVersioned(p.base, "/v1", pathCountTokens), nil
	default:
		return "", noOperation("anthropic-messages", op,
			"this family serves messages and token counts only")
	}
}

// credential is the one family that does not use a bearer token.
func (anthropicAdapter) credential(secret string, h http.Header) { h.Set("x-api-key", secret) }

// headers supplies the mandatory version header. It runs whether the credential
// is a static key or an OAuth token, because the header is a property of the
// SURFACE and a request without it is refused outright.
func (anthropicAdapter) headers(h http.Header) {
	if h.Get("anthropic-version") == "" {
		h.Set("anthropic-version", DefaultAnthropicVersion)
	}
}

func (anthropicAdapter) encode(x *exchange) ([]byte, error) {
	if x.call.Op == OpCountTokens {
		// The count endpoint takes the same request shape and ignores the
		// output ceiling, so the smallest legal value satisfies a field this
		// family makes mandatory without capping anything.
		return anthropic.MarshalRequest(x.req, &anthropic.EncodeOptions{
			Model:            x.target.UpstreamModel,
			DefaultMaxTokens: 1,
		})
	}
	return anthropic.MarshalRequest(x.req, &anthropic.EncodeOptions{
		Model:            x.target.UpstreamModel,
		AllowLossy:       x.call.AllowLossy,
		DefaultMaxTokens: x.call.DefaultMaxTokens,
	})
}

func (anthropicAdapter) decode(body []byte, x *exchange) (*decoded, error) {
	if x.call.Op == OpCountTokens {
		// The count is the whole answer; there is nothing to convert and no
		// usage to price beyond the request itself.
		return &decoded{raw: body}, nil
	}
	resp, err := anthropic.DecodeResponse(body, &anthropic.DecodeOptions{Model: x.call.Model})
	if err != nil {
		return nil, err
	}
	return &decoded{resp: resp}, nil
}

func (anthropicAdapter) source(r io.Reader, _ *exchange) (eventSource, error) {
	return &anthropicSource{d: anthropic.NewStreamDecoder(r, nil)}, nil
}
