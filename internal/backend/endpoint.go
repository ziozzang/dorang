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
	pathResponses       = "/responses"
	pathEmbeddings      = "/embeddings"
	pathRerank          = "/rerank"
	pathMessages        = "/messages"
	pathCountTokens     = "/messages/count_tokens"
	pathModerations     = "/moderations"
	pathSpeech          = "/audio/speech"
	pathTranscriptions  = "/audio/transcriptions"
	pathTranslations    = "/audio/translations"
	pathImageGenerate   = "/images/generations"
	pathImageEdit       = "/images/edits"
	pathImageVariation  = "/images/variations"
)

// joinVersioned joins a suffix onto a base URL, supplying "/v1" when the base
// is a bare host.
//
// Configured base URLs come in two shapes in the wild and both are correct: one
// already carries the version segment ("https://api.example.com/v1",
// "https://api.z.ai/api/coding/paas/v4") and one is a bare host
// ("https://api.anthropic.com"). A bare host gets the default version and a
// versioned one does not, which is the only rule that leaves every catalogued
// kind pointing at a real endpoint.
func joinVersioned(base, version, suffix string) string {
	base = trimBase(base)
	if strings.HasSuffix(base, suffix) {
		return base
	}
	if bare(base) {
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

// bare reports whether a base URL names a host with no path of its own.
func bare(base string) bool {
	u, err := url.Parse(base)
	return err == nil && (u.Path == "" || u.Path == "/")
}
