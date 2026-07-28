package backend

import (
	"io"
	"net/http"
)

// unsupportedProvider is a provider kind dorang refuses to serve, by name.
//
// # Why a 501 and not an adapter
//
// Each of these three kinds carries an `api` in the catalog that names a wire
// shape the real service does not serve at the URL dorang would build. Before
// this package existed they were dispatched on that named adapter, which sent a
// request to a route that does not exist, carrying a credential in a spelling
// the host does not accept — and the operator got a vendor 404 or 403 with no
// indication that dorang had never been able to address the service at all.
//
// A half-working adapter would be worse than either. What each of these needs
// is an input the configuration does not carry, and a gateway that invents one
// is a gateway that is confidently wrong:
//
//   - bedrock: request signing (SigV4) over a region, an access key pair and a
//     session token, and the Converse request shape. The catalog's entry says
//     anthropic-messages because naming an API no adapter implements would be a
//     worse lie; it is still not the wire.
//   - vertex: a project id, a location, and a Google service-account OAuth
//     exchange, in a path of the form
//     /v1/projects/{p}/locations/{l}/publishers/google/models/{m}:generateContent.
//     The gemini adapter addresses generativelanguage.googleapis.com, which
//     authenticates by API key and has no project in the path.
//   - azure: a deployment name in the path and a MANDATORY api-version query
//     parameter, with the key in an `api-key` header rather than a bearer
//     token. There is no configuration field for the api-version, and a
//     hardcoded default is a value dorang would be choosing on the operator's
//     behalf that changes which model behaviour they get.
//
// The refusal names the missing input, so the message is a work order rather
// than a dead end.
type unsupportedProvider struct {
	code   string
	reason string
}

func (u unsupportedProvider) Error() string { return u.reason }

var unsupportedKinds = map[string]unsupportedAdapter{
	"bedrock": {
		code: "bedrock_unsupported",
		reason: "the bedrock kind has no adapter in this build: it needs SigV4 request signing and the Converse " +
			"request shape, neither of which dorang implements. Front Bedrock with a gateway that speaks " +
			"anthropic-messages or openai-chat, and point a provider at that.",
	},
	"vertex": {
		code: "vertex_unsupported",
		reason: "the vertex kind has no adapter in this build: it needs a project id, a location and a Google " +
			"service-account OAuth exchange in a project-scoped path, none of which this configuration carries. " +
			"The gemini adapter serves generativelanguage.googleapis.com only.",
	},
	"azure": {
		code: "azure_unsupported",
		reason: "the azure kind has no adapter in this build: its route carries the deployment name in the path " +
			"and a mandatory api-version query parameter, and the credential is an api-key header rather than a " +
			"bearer token. Set base_url to a route that serves /v1/chat/completions to use the openai-chat adapter.",
	},
}

// unsupportedAdapter refuses every operation with the same named 501.
type unsupportedAdapter struct {
	code   string
	reason string
}

func (u unsupportedAdapter) endpoint(*Provider, Operation, string, bool) (string, error) {
	return "", unsupportedProvider{code: u.code, reason: u.reason}
}

func (unsupportedAdapter) credential(string, http.Header) {}

func (unsupportedAdapter) headers(http.Header) {}

func (u unsupportedAdapter) encode(*exchange) ([]byte, error) {
	return nil, unsupportedProvider{code: u.code, reason: u.reason}
}

func (u unsupportedAdapter) decode([]byte, *exchange) (*decoded, error) {
	return nil, unsupportedProvider{code: u.code, reason: u.reason}
}

func (u unsupportedAdapter) source(io.Reader, *exchange) (eventSource, error) {
	return nil, unsupportedProvider{code: u.code, reason: u.reason}
}
