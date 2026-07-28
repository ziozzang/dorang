package backend

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// exchange is one request in flight, reduced to what an adapter needs.
//
// req is NOT c.Request: it is the prepared copy, with the engine
// normalizations of selfhosted.go and the priority field of §7.5 already
// applied. An adapter reads it and never mutates it, because a fail-back hop
// re-prepares from the caller's original.
type exchange struct {
	call   *Call
	target *Target
	prov   *Provider
	req    *canonical.Request

	// ctype is the Content-Type of the encoded request body. An adapter that
	// produces something other than JSON — the multipart audio and image
	// surfaces — sets it; everything else leaves it empty and [Backend.send]
	// uses application/json.
	ctype string
	// bnd is the multipart boundary, generated once per exchange so that encode
	// and the Content-Type header cannot disagree about it.
	bnd string
	// respType is the Content-Type the upstream labelled its answer with. It is
	// the only way to tell a JSON transcript from an srt one, because the two
	// arrive on the same route with the same status.
	respType string
	// attempt is the in-provider attempt number, used only to keep a retried
	// multipart body's boundary distinct from the first one's.
	attempt int
	// secrets is what was actually put on the outbound request's credential
	// headers, kept so that anything the upstream echoes back can be scrubbed
	// before it reaches a client (DESIGN §10.6 rule 4). It is never logged, never
	// compared, and never leaves this package.
	secrets []string
}

// boundary returns this exchange's multipart boundary, generating it on first
// use.
//
// It is derived from the exchange rather than randomly generated so that a
// golden test over an outgoing request compares bytes rather than a fresh UUID.
// It only has to not appear in the payload, and this one cannot: multipart
// boundaries are compared against whole lines beginning "--", and this value is
// hyphen-free ASCII of a fixed shape.
func (x *exchange) boundary() string {
	if x.bnd == "" {
		x.bnd = "dorangMultipartBoundary" + itoa(len(x.call.Body)) + "z" + itoa(x.attempt)
	}
	return x.bnd
}

// decoded is what an adapter makes of a successful upstream answer.
//
// Exactly one of resp and raw is set. resp is the neutral form of a chat
// answer, which [Backend] then renders in the caller's protocol — the N+M rule
// of §10.1, which is why an adapter never sees the caller's family. raw is an
// answer that has no neutral representation and is relayed with only the model
// name restored: an embeddings vector list and a token count are both of that
// kind, and inventing a canonical shape for them would be a conversion nobody
// asked for.
type decoded struct {
	resp  *canonical.Response
	raw   []byte
	usage canonical.Usage
	// ctype labels raw when it is not JSON. Speech answers audio and a
	// transcription asked for in srt or vtt answers text; mislabelling either
	// gives the client something it cannot play or parse.
	ctype string
}

// adapter is one wire shape (DESIGN §4.3's `api`).
type adapter interface {
	// endpoint names the URL an operation goes to, or reports that this shape
	// does not serve it.
	endpoint(p *Provider, op Operation, model string, stream bool) (string, error)
	// credential applies a static provider secret in this family's spelling.
	credential(secret string, h http.Header)
	// headers sets family headers that are not the credential — a mandatory
	// version header, a beta opt-in. It runs whether or not a credential
	// exists.
	headers(h http.Header)
	// encode renders the request body.
	encode(x *exchange) ([]byte, error)
	// decode converts a successful upstream answer.
	decode(body []byte, x *exchange) (*decoded, error)
	// source opens a neutral event source over a streaming body.
	source(r io.Reader, x *exchange) (eventSource, error)
}

// adapterFor selects the adapter for a wire shape.
//
// Kind is consulted FIRST, and only to refuse. Three kinds — bedrock, vertex
// and azure — carry an `api` in the catalog that names a shape they do not
// actually serve: bedrock's entry says anthropic-messages while the real
// surface is Converse, vertex's says gemini while the real surface is a
// project-scoped path with Google OAuth, and azure's says openai-chat while the
// real surface is /openai/deployments/{d}/…?api-version=. Dispatching them on
// the named adapter produces a request to a URL that does not exist, carrying a
// credential in a spelling the host does not accept. Refusing by name is worse
// for nobody and better for the operator, who gets a sentence naming what is
// missing instead of a vendor 404.
func adapterFor(api catalog.API, kind string) (adapter, error) {
	if u, ok := unsupportedKinds[kind]; ok {
		return u, nil
	}
	switch api {
	case catalog.APIOpenAIChat, catalog.APIOpenAIResponses:
		return openaiAdapter{}, nil
	case catalog.APIAnthropicMessages:
		return anthropicAdapter{}, nil
	case catalog.APIGemini:
		return geminiAdapter{}, nil
	case catalog.APICohere:
		return cohereAdapter{}, nil
	case catalog.APIJina:
		return jinaAdapter{}, nil
	case catalog.APIEcho:
		// The catalog's echo kind is a deterministic in-process adapter for
		// tests (DESIGN §4.3). It has no HTTP surface, so it is refused here
		// rather than silently served by the OpenAI adapter — which is what
		// happened before this package existed, and which sent a test kind's
		// traffic to a real URL.
		return unsupportedAdapter{
			code:   "echo_not_implemented",
			reason: "the echo kind is an in-process test adapter and has no HTTP backend in this build",
		}, nil
	}
	return nil, errors.New("backend: no adapter for wire shape " + string(api))
}

// errNoOperation is the shape-level refusal an adapter returns for an operation
// it does not serve. [Backend.Do] turns it into a named 501.
type errNoOperation struct {
	api    string
	op     Operation
	detail string
}

func (e *errNoOperation) Error() string {
	if e.detail != "" {
		return "backend: " + e.api + " does not serve " + e.op.String() + ": " + e.detail
	}
	return "backend: " + e.api + " does not serve " + e.op.String()
}

// code is the error code the refusal carries. It names the OPERATION rather
// than the shape, because that is what the caller asked for and what they can
// change; which adapter refused is in the message.
func (e *errNoOperation) code() string {
	switch e.op {
	case OpEmbeddings:
		return "embeddings_unsupported"
	case OpCountTokens:
		return "count_tokens_unsupported"
	case OpRerank:
		return "rerank_unsupported"
	default:
		return "chat_unsupported"
	}
}

// errNilRequest is an encode with nothing to encode: a programming error, not a
// caller's.
var errNilRequest = errors.New("backend: the request has no neutral form")

// errNotAnObject is a relayed body that is not a JSON object.
var errNotAnObject = errors.New("backend: the body is not a JSON object")

// noOperation builds an errNoOperation.
func noOperation(api string, op Operation, detail string) error {
	return &errNoOperation{api: api, op: op, detail: detail}
}

// ---------------------------------------------------------------------------
// Shared relay helpers
// ---------------------------------------------------------------------------

// relayRequest renders an operation that has no neutral representation: the
// caller's body goes upstream with only the model name replaced.
//
// It decodes into a raw-message map, so every other field survives byte for
// byte and the model name is copied whole — nothing here splits it (§2.1).
func relayRequest(body []byte, model string) ([]byte, error) {
	return replaceModel(body, model)
}

// relayResponse restores the client-facing model name and reads the usage
// object, which is the only part of such a body dorang looks inside.
func relayResponse(body []byte, model string) (*decoded, error) {
	out, err := replaceModel(body, model)
	if err != nil {
		return nil, err
	}
	u, _ := scanRelayUsage(body)
	return &decoded{raw: out, usage: u}, nil
}

// replaceModel swaps the top-level "model" of a JSON object.
func replaceModel(body []byte, model string) ([]byte, error) {
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
	return json.Marshal(obj)
}

// scanRelayUsage reads the usage object of an embeddings-shaped answer.
func scanRelayUsage(body []byte) (canonical.Usage, bool) {
	var shape struct {
		Usage *struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &shape); err != nil || shape.Usage == nil {
		return canonical.Usage{}, false
	}
	return canonical.Usage{InputTokens: shape.Usage.PromptTokens}, true
}

// itoa is strconv.Itoa under a shorter name, used where a number is spliced
// into a generated identifier.
func itoa(v int) string { return strconv.Itoa(v) }
