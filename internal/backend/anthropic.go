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
	case OpChat, OpCompletions, OpResponses:
		// The three chat-shaped surfaces all reach this family through the same
		// route, because by the time a request is here it is a [canonical.Request]
		// and the differences between the three are entirely in how the ANSWER is
		// rendered (see encodeT1Client). Refusing the legacy and Responses
		// frontends on a messages deployment would make the choice of backend
		// visible to a caller who never chose it, which is what §4.4's
		// single-surface commitment forbids.
		//
		// A STREAMING legacy completion is still refused, but not here: the relay
		// emits chat.completion.chunk frames and a /v1/completions client reads
		// text_completion ones, so that crossing is turned away by the frontend
		// before a deployment is chosen at all.
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
		//
		// No Loss, deliberately. The ceiling of 1 is dorang's, not the caller's,
		// and it puts every reasoning budget below MinThinkingBudget — so a report
		// from this call would tell a caller their reasoning was disabled on a
		// route that generates nothing and bills no generation. Same reasoning as
		// [refuseMaterialLoss]'s exemption for this operation.
		return anthropic.MarshalRequest(x.req, &anthropic.EncodeOptions{
			Model:            x.target.UpstreamModel,
			DefaultMaxTokens: 1,
		})
	}
	return anthropic.MarshalRequest(x.req, &anthropic.EncodeOptions{
		Model: x.target.UpstreamModel,
		// The set the §10.1 gate just cleared this request against. Letting the
		// encoder fall back to its own family default instead would mean a
		// deployment that expresses LESS than its family is refused on one set
		// and encoded against another.
		Capabilities: x.capabilities(),
		// Where the located losses go. Passing nothing here is what left §10.2's
		// budget collapse — "reasoning is disabled for this request, and
		// x-dorang-dropped-params says so" — with no writer at all: encodeThinking
		// has recorded the drop since it was written, into a report the adapter
		// threw away.
		Loss:             x.loss,
		AllowLossy:       x.call.AllowLossy,
		DefaultMaxTokens: x.call.DefaultMaxTokens,
	})
}

func (anthropicAdapter) decode(body []byte, x *exchange) (*decoded, error) {
	if x.call.Op == OpCountTokens {
		// The count is the whole answer; there is nothing to convert and no
		// usage to price beyond the request itself. It is still checked for
		// being an answer at all — see [relayCountTokens].
		return relayCountTokens(body)
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
