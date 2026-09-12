package backend

import (
	"net/url"
	"strings"
)

// Operation is what a request asks a provider to do. It is not the caller's
// route: /v1/messages and /v1/chat/completions are both [OpChat], because the
// operation is what the BACKEND must be asked for, and the caller's protocol is
// a separate axis.
type Operation uint8

// The operations a backend can be asked for.
const (
	OpChat Operation = iota
	OpEmbeddings
	OpCountTokens
	OpRerank

	// The T1 surface of COMPATIBILITY §0. OpCompletions is the legacy text
	// surface and OpResponses the Responses API; both are chat-shaped in the
	// neutral form and differ only in what the wire calls things, which is why
	// they are operations here rather than a flag on OpChat: the ENDPOINT
	// differs, and that is what an Operation selects.
	OpCompletions
	OpResponses
	OpModerations
	OpSpeech
	OpTranscription
	OpTranslation
	OpImageGenerate
	OpImageEdit
	OpImageVariation
)

// operationNames is the label set. A table rather than a switch so that a new
// operation without a name is a compile-time hole rather than a silent "chat".
var operationNames = [...]string{
	OpChat:           "chat",
	OpEmbeddings:     "embeddings",
	OpCountTokens:    "count_tokens",
	OpRerank:         "rerank",
	OpCompletions:    "completions",
	OpResponses:      "responses",
	OpModerations:    "moderations",
	OpSpeech:         "audio.speech",
	OpTranscription:  "audio.transcription",
	OpTranslation:    "audio.translation",
	OpImageGenerate:  "images.generation",
	OpImageEdit:      "images.edit",
	OpImageVariation: "images.variation",
}

// String names the operation for an error message.
func (o Operation) String() string {
	if int(o) < len(operationNames) && operationNames[o] != "" {
		return operationNames[o]
	}
	return "chat"
}

// ChatShaped reports whether an operation's neutral form is a
// [canonical.Request] — which is what decides whether the request goes through
// prepareRequest and whether a stream has a neutral event source.
func (o Operation) ChatShaped() bool {
	switch o {
	case OpChat, OpCountTokens, OpCompletions, OpResponses:
		return true
	}
	return false
}

// Multipart reports whether an operation's request body is form data rather
// than JSON.
func (o Operation) Multipart() bool {
	switch o {
	case OpTranscription, OpTranslation, OpImageEdit, OpImageVariation:
		return true
	}
	return false
}

// Endpoint joins the provider's base URL with the URL for one operation.
//
// The derivation is per wire shape, not one rule with exceptions, because the
// shapes disagree about where the version segment lives and what the path even
// contains. openai-chat puts the operation in the path and the model in the
// body; gemini puts the model IN the path and the operation after a colon;
// cohere versions per route rather than per host. One rule covering all three
// would have to be wrong about two of them.
//
// model is the upstream model id, used only by shapes that put it in the path.
// stream selects a streaming route where the shape has a separate one.
func (p *Provider) Endpoint(op Operation, model string, stream bool) (string, error) {
	return p.ad.endpoint(p, op, model, stream)
}

// Endpoint suffixes for the OpenAI-shaped families.
const (
	pathChatCompletions = "/chat/completions"
	pathCompletions     = "/completions"
	// pathResponses is addressed without a version segment: a Responses-only
	// host publishes its base with the version already in it.
	pathResponses      = "/responses"
	pathEmbeddings     = "/embeddings"
	pathRerank         = "/rerank"
	pathMessages       = "/messages"
	pathCountTokens    = "/messages/count_tokens"
	pathModerations    = "/moderations"
	pathSpeech         = "/audio/speech"
	pathTranscriptions = "/audio/transcriptions"
	pathTranslations   = "/audio/translations"
	pathImageGenerate  = "/images/generations"
	pathImageEdit      = "/images/edits"
	pathImageVariation = "/images/variations"
)

// joinVersioned joins a suffix onto a base URL, supplying the family's default
// version segment when the base does not already carry one.
//
// THE RULE, in one sentence, and it is the sentence docs/CONFIG.md §6 states to
// operators: **dorang appends the version segment unless the configured
// `base_url` already contains one.**
//
// Configured base URLs come in three shapes in the wild and all three are
// correct:
//
//   - a bare host — "https://api.anthropic.com"
//   - a host plus a version — "https://api.example.com/v1",
//     "https://api.z.ai/api/coding/paas/v4", "https://api.deepinfra.com/v1/openai"
//   - a host plus a path and NO version — "https://api.z.ai/api/anthropic",
//     "https://api.minimax.io/anthropic"
//
// The rule this replaced supplied the version only for the FIRST shape, so the
// third got neither: `<host>/api/anthropic` was addressed as
// `<host>/api/anthropic/messages`, and the real route is
// `<host>/api/anthropic/v1/messages`. That third shape is not exotic — it is
// what every Anthropic-compatible coding plan hands an operator, because it is
// what a Claude-Code-shaped client puts in ANTHROPIC_BASE_URL and that client
// appends "/v1/messages" itself. dorang appended nothing, addressed a route that
// does not exist, and (before the shape check in the wire decoders) rendered the
// vendor's 200-with-an-error-body as an empty successful turn.
//
// "Already contains one" is deliberately not "already ENDS in one": the version
// is not always last. https://api.deepinfra.com/v1/openai is a catalogued base
// whose route is /v1/openai/chat/completions, and a rule keyed on the final
// segment would address /v1/openai/v1/chat/completions.
func joinVersioned(base, version, suffix string) string {
	base = trimBase(base)
	if strings.HasSuffix(base, suffix) {
		return base
	}
	if !hasVersionSegment(base) {
		base += version
	}
	return base + suffix
}

// join appends a suffix that carries its own version segment, so a bare host
// gets nothing inserted. Cohere's routes are /v2/rerank on the bare host; a
// "/v1" inserted here would address a route that does not exist.
func join(base, suffix string) string {
	base = trimBase(base)
	if strings.HasSuffix(base, suffix) {
		return base
	}
	// A base that already carries the version prefix of the suffix takes only
	// the remainder: "https://api.cohere.com/v2" + "/v2/rerank" is not a URL.
	if i := strings.Index(suffix, "/"); i == 0 {
		if j := strings.Index(suffix[1:], "/"); j >= 0 {
			ver := suffix[:j+1]
			if strings.HasSuffix(base, ver) {
				return base + suffix[j+1:]
			}
		}
	}
	return base + suffix
}

// hasVersionSegment reports whether the base URL's path already names an API
// version, anywhere in it.
//
// A bare host trivially has none, so this is a strict widening of the "is this
// a bare host" test it replaced: the two answers differ only for a base that
// has a path, which is exactly the case [joinVersioned] was getting wrong.
func hasVersionSegment(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		// Unparseable. Adding a segment to a string that is not a URL cannot
		// improve it, so nothing is added and the request fails at the transport
		// with the operator's own value in the message.
		return true
	}
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if versionSegment(seg) {
			return true
		}
	}
	return false
}

// versionSegment recognizes one path segment as an API version: "v" or "V"
// followed by a digit, and then anything ("v1", "v2", "v4", "v1beta",
// "v1alpha1").
//
// The digit is what keeps it from matching the ordinary path segments these
// URLs are full of — "venice", "v2ray"-shaped names do not appear, but
// "/openai", "/anthropic", "/gateway" and "/inference" do, and none of them
// starts "v<digit>".
func versionSegment(s string) bool {
	return len(s) >= 2 && (s[0] == 'v' || s[0] == 'V') && s[1] >= '0' && s[1] <= '9'
}
