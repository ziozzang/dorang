package backend

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/internal/wire/rerank"
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
	// served is the model name the UPSTREAM put in its own answer and agree is
	// how it compares with target.UpstreamModel — the name dorang sent.
	//
	// They live on the exchange for the same reason respType does: they are read
	// from the answer, deep inside a conversion whose signature has nothing to
	// carry them, and needed by [Backend.Do], which holds neither the body nor
	// the decoded response. See [canonical.CompareModel] for what the comparison
	// means and why a naive inequality is not it.
	//
	// Nothing in this package acts on them. A substitution is a fact to report;
	// recording that an endpoint substitutes is not permission to substitute,
	// and it is not permission to reroute or to fail the request either.
	served string
	agree  canonical.ModelAgreement
	// names is the request's tool-name registry, shared by the encoder that
	// shortens an over-long name and by every decoder that has to restore it
	// (COMPATIBILITY 5.3). It lives on the exchange because that is the only
	// object whose lifetime spans both directions of one call: an encoder-local
	// mapping is written, used once and thrown away, which shortens on every
	// request and restores on none.
	names *openai.ToolNames
	// secrets is what was actually put on the outbound request's credential
	// headers, kept so that anything the upstream echoes back can be scrubbed
	// before it reaches a client (DESIGN §10.6 rule 4). It is never logged, never
	// compared, and never leaves this package.
	secrets []string
	// loss is what the encoder removed on the way to this deployment's wire
	// shape, filled during encode and handed to [Call.Accepted].
	//
	// It exists because the encoders already computed it and nobody took it: the
	// adapters passed no Loss to MarshalRequest, so every located downgrade was
	// discarded at the point it was produced. Two documented promises had no
	// writer as a direct result — §10.2's "reasoning is disabled … and
	// x-dorang-dropped-params says so", and the thinking-signature downgrade
	// crossing into the OpenAI family, which no capability mask can see because
	// the bit is HELD while the encoder drops the signature.
	//
	// It is nil for an operation with no neutral request, which is the same
	// condition [Call.Request] is nil under. Every LossReport method tolerates a
	// nil receiver, so the encoders need no branch for it.
	loss *canonical.LossReport
	// accepted records that [Call.Accepted] has been handed the report, so that
	// "exactly once" is a property of this struct rather than of the reader of
	// two call sites in [Backend.finish].
	accepted bool
}

// accept hands the loss report to [Call.Accepted], once.
//
// The two call sites are the two moments a header block closes — before a
// stream's first write, and when a buffered answer's rendering is done — and
// which one runs is decided by whether the answer streams. See [Call.Accepted]
// for why that is one callback at two instants rather than two callbacks.
func (x *exchange) accept() {
	if x.accepted || x.call.Accepted == nil {
		return
	}
	x.accepted = true
	x.call.Accepted(x.loss)
}

// noteServedModel records how the upstream's own answer compares with the name
// dorang sent it.
//
// The classification is unconditional and free — it reads two strings and
// allocates nothing. The NAME is copied out only when the comparison found a
// substitution AND the target's model is one the catalog knows, because that is
// the only combination in which a caller can prove anything, and copying it
// otherwise would put an allocation on every request to a deployment whose
// upstream answers under an alias it resolved for itself.
func (x *exchange) noteServedModel(served string) {
	if served == "" {
		return
	}
	x.agree = canonical.CompareModel(x.target.UpstreamModel, served)
	if x.agree.Substituted() && x.target.ModelKnown {
		x.served = served
	}
}

// noteServedAgreement is [exchange.noteServedModel] for the byte relay, whose
// scanner has already classified the name against the same comparand without
// ever turning it into a string. name materializes it, and is called only when
// the verdict makes the string worth having.
func (x *exchange) noteServedAgreement(agree canonical.ModelAgreement, name func() string) {
	x.agree = agree
	if agree.Substituted() && x.target.ModelKnown {
		x.served = name()
	}
}

// clientCapabilities is what the CALLER's protocol can carry, which is the set a
// response is encoded against.
//
// It is the mirror of [exchange.capabilities] and it is a different question:
// that one is what the DEPLOYMENT can express, and it decides the request half.
// Answering the response half with the request's set would report the upstream's
// limitations to a client that never had them.
//
// It exists as an accessor because the alternative was leaving it unset and
// letting each encoder's `opt.caps()` fall back to its own DefaultCapabilities.
// That fallback happens to produce this same value today — anthropic's default
// IS CapabilitiesForAPI(APIAnthropicMessages) — so the response direction was
// converted against the right set BY COINCIDENCE, through a nil check whose
// comment says "none supplied" rather than "the caller's". A coincidence is not
// a decision, and the first per-request client capability set would have ended
// it silently.
func (x *exchange) clientCapabilities() canonical.Capability {
	return CapabilitiesForAPI(x.call.ClientAPI)
}

// toolNames returns the shared tool-name registry, allocating it on first use.
//
// It is allocated eagerly rather than lazily by the encoder because the encoder
// is not the only reader: a stream's decoder runs long after the encoder
// returned, and a mapping the encoder allocated into its own options struct is
// gone by then.
func (x *exchange) toolNames() *openai.ToolNames {
	if x.names == nil {
		x.names = openai.NewToolNames()
	}
	return x.names
}

// capabilities is what this exchange's deployment can express: the target's own
// declaration when it made one, and its wire shape's set otherwise.
//
// One accessor, read by both the §10.1 gate and the encoders, because the two
// answering the question separately is exactly how a request gets refused
// against one capability set and encoded against another.
func (x *exchange) capabilities() canonical.Capability {
	if x.target.Capabilities != 0 {
		return x.target.Capabilities
	}
	return CapabilitiesForAPI(x.prov.api)
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

// CapabilitiesForAPI is what a deployment of this wire shape can express
// (DESIGN §10.1).
//
// # One function, and it used to be two
//
// This answers the same question internal/app answers when it builds a routing
// table, and for a while both spelled it: routing PREFERS a deployment that can
// express the request, and [exchange.capabilities] is the gate that runs once one
// has been chosen. Two answers to one question is how a request gets routed on
// one capability set and encoded against another, so internal/app now CALLS this
// rather than restating it, and the value it computes travels onto the router's
// Deployment, out on its Decision, and back in on [Target.Capabilities].
//
// Each answer is a constant declared beside the encoder that has to honour it —
// [anthropic.DefaultCapabilities], [openai.DefaultCapabilities],
// [GeminiCapabilities] — because a set kept anywhere else drifts from the code
// that converts against it. Gemini is the proof: it had no constant, fell to the
// default arm below, and was handed the OpenAI set while [encodeGemini] wrote no
// cache_control, no logprobs, no service_tier and no thinking block.
//
// # cohere and jina
//
// Both resolve through the default arm and therefore claim the OpenAI set, and
// both claims are cosmetic rather than wrong: [cohereAdapter.endpoint] and
// [jinaAdapter.endpoint] refuse OpChat outright, and [Backend.Do] resolves the
// endpoint BEFORE the capability gate — so a chat request to either is a named
// 501 and no capability of theirs is ever consulted. Rerank and embeddings carry
// no [canonical.Request] and raise no capability bits at all. The set is left at
// the permissive default rather than zeroed because the routing table reads it
// for every kind, and an empty set there refuses every structural request instead
// of expressing none.
//
// # Why the default is permissive
//
// A shape whose real capabilities are unknown must not acquire refusals it never
// had: an overstated capability drops a knob and says so, while an understated
// one turns working traffic into 400s. That argument covers an unlisted
// OpenAI-compatible server, which is what the default arm is for. It never
// covered a shape dorang has an encoder for — there, the encoder is the evidence,
// and Gemini is what the argument's absence cost.
func CapabilitiesForAPI(api catalog.API) canonical.Capability {
	switch api {
	case catalog.APIAnthropicMessages:
		return anthropic.DefaultCapabilities
	case catalog.APIGemini:
		return GeminiCapabilities
	}
	return openai.DefaultCapabilities
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

// errNotAResponse is a relayed body that IS a JSON object and carries none of
// the members an answer of its shape has.
//
// It is the relay's half of the wire decoders' ErrNotAResponse, and it exists
// for the same reason: an upstream that answers 200 with an error envelope
// parses cleanly everywhere, and on a relay path the envelope reaches the
// client verbatim.
var errNotAResponse = errors.New("backend: the body is a JSON object but not a response")

// isNotAResponse reports the whole defect class: an upstream 200 whose body
// parsed cleanly and is not an answer of the surface it arrived on.
//
// Every wire package raises its own sentinel rather than a shared one, because
// the message names the family and that is what an operator reading a log line
// needs. They are collected here so [Backend.convert] has one condition to test
// and so a new surface's sentinel has an obvious place to be added — the
// alternative, an errors.Is chain that grows inline at the call site, is how one
// of these gets left out.
func isNotAResponse(err error) bool {
	return errors.Is(err, errNotAResponse) ||
		errors.Is(err, anthropic.ErrNotAResponse) ||
		errors.Is(err, openai.ErrNotAResponse) ||
		errors.Is(err, openai.ErrNotAModerationResponse) ||
		errors.Is(err, openai.ErrNotAnImageResponse) ||
		errors.Is(err, openai.ErrNotATranscriptionResponse) ||
		errors.Is(err, rerank.ErrNotAResponse)
}

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
//
// It is the embeddings surface's half of D2, found by looking for the same
// class rather than reported from the wire. A relay is the WORST place for the
// defect: a vendor error body wearing a 200 is not merely rendered as an empty
// answer here, it is handed to the client verbatim with a `model` member
// spliced into it. Requiring one of the two members every embeddings answer has
// is what separates that from an answer.
func relayResponse(body []byte, model string) (*decoded, error) {
	obj, err := jsonObject(body)
	if err != nil {
		return nil, err
	}
	// `data` is the vectors and `usage` the counts. An answer has at least one;
	// an error envelope has neither. `object: "list"` is deliberately not in the
	// list — it is a discriminator some vendors omit, and accepting on it would
	// also accept `object: "error"`.
	if !hasAnyMember(obj, "data", "usage") {
		return nil, errNotAResponse
	}
	out, err := marshalWithModel(obj, model)
	if err != nil {
		return nil, err
	}
	u, _ := scanRelayUsage(body)
	return &decoded{raw: out, usage: u}, nil
}

// relayCountTokens relays a token-count answer.
//
// The count IS the whole answer, so there is nothing to convert — but that also
// means there is nothing between a vendor's 200-wrapped error body and a client
// reading `input_tokens` off it and getting zero. This is the same check
// [relayResponse] makes, against the one member this answer has.
func relayCountTokens(body []byte) (*decoded, error) {
	obj, err := jsonObject(body)
	if err != nil {
		return nil, err
	}
	if !hasAnyMember(obj, "input_tokens") {
		return nil, errNotAResponse
	}
	return &decoded{raw: body}, nil
}

// jsonObject decodes a relayed body as a JSON object, distinguishing "not an
// object" from a parse failure.
func jsonObject(body []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errNotAnObject
	}
	return obj, nil
}

// hasAnyMember reports whether the object carries at least one of the named
// keys. Presence is the test, not the value: an empty `data` array is a valid
// answer to a request for no embeddings, and a measured zero is a measurement.
func hasAnyMember(obj map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		if _, ok := obj[k]; ok {
			return true
		}
	}
	return false
}

// replaceModel swaps the top-level "model" of a JSON object.
func replaceModel(body []byte, model string) ([]byte, error) {
	obj, err := jsonObject(body)
	if err != nil {
		return nil, err
	}
	return marshalWithModel(obj, model)
}

// marshalWithModel re-encodes a decoded object with its model member replaced.
func marshalWithModel(obj map[string]json.RawMessage, model string) ([]byte, error) {
	enc, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = enc
	return json.Marshal(obj)
}

// scanRelayUsage reads the usage object of an embeddings-shaped answer.
//
// total_tokens is the FALLBACK and not the ignored field it used to be. An
// embedding has no completion half, so on this shape the total IS the prompt
// count — and the two vendors measured here disagree only about which key
// carries it: Jina direct answers `{"total_tokens":4}` with no prompt_tokens at
// all, and the same model through LiteLLM answers `{"prompt_tokens":0,
// "total_tokens":4}`. Reading prompt_tokens alone returned zero for both.
//
// Zero is not a harmless number here. [Result.Usage] is the single source for
// the X-Dorang-Tokens-* headers, for internal/meter and for pricing, and
// §11.6's token guard triggers on a positive token total — so an embedding
// priced at zero is also an embedding the guard cannot see. Nothing fails, so
// nothing alerts, and in a retrieval-heavy deployment the budgets, the per-key
// spend and the rate guard silently exclude most of the traffic. The rerank
// path on the same run metered correctly, which is what makes this an oversight
// rather than a policy.
//
// prompt_tokens wins whenever it is non-zero: a vendor that states both and
// means different things by them is stating the prompt count in the field named
// for it. input_tokens is read for the same reason total_tokens is — it is a
// third real spelling of the one count this shape has, used by the Anthropic
// family and by the OpenAI Responses generation, and a host that answers
// embeddings in it metered as zero under a reader that knew only the other two.
//
// The three-family ambiguity DESIGN §10.7 records does not reach here. What
// makes `input_tokens` undecidable elsewhere is whether it CONTAINS the cached
// prefix, and an embeddings answer states no cache breakdown under any spelling
// — there is nothing to place inside or beside the count, so both readings give
// the same number. If this shape ever grows one, the breakdown object settles
// the family, exactly as it does in internal/wire/openai's usageToCanonical and
// internal/server's scanUsage.
func scanRelayUsage(body []byte) (canonical.Usage, bool) {
	var shape struct {
		Usage *struct {
			PromptTokens int `json:"prompt_tokens"`
			InputTokens  int `json:"input_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &shape); err != nil || shape.Usage == nil {
		return canonical.Usage{}, false
	}
	in := shape.Usage.PromptTokens
	if in == 0 {
		in = shape.Usage.InputTokens
	}
	if in == 0 {
		in = shape.Usage.TotalTokens
	}
	u := canonical.Usage{InputTokens: in}
	// The backend stated a prompt count, whichever key it used. Recording that
	// it did is what separates a measured zero from an unmeasured one for every
	// encoder downstream (see [canonical.Usage.Reported]).
	u.Report(canonical.UsageInput)
	return u, true
}

// itoa is strconv.Itoa under a shorter name, used where a number is spliced
// into a generated identifier.
func itoa(v int) string { return strconv.Itoa(v) }
