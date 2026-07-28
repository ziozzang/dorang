package backend

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
)

// Error codes this package authors. Every one names a condition an operator can
// act on; none of them is a paraphrase of an upstream string.
const (
	// CodeUpstreamUnreachable is a request that produced no HTTP response.
	CodeUpstreamUnreachable = "upstream_unreachable"
	// CodeTimeout is COMPATIBILITY §11.2's row for an upstream timeout.
	CodeTimeout = "timeout"
	// CodeUpstreamRedirect is a 30x that dorang refused to follow (§10.6).
	CodeUpstreamRedirect = "upstream_redirect"
	// CodeUpstreamBody is a response whose body could not be read.
	CodeUpstreamBody = "upstream_body"
	// CodeUpstreamTooLarge is a non-streaming response past the buffering
	// ceiling.
	CodeUpstreamTooLarge = "upstream_response_too_large"
	// CodeUpstreamDecode is a response dorang could not understand.
	CodeUpstreamDecode = "upstream_decode"
	// CodeUpstreamShape is a relayed answer that was not a JSON object.
	CodeUpstreamShape = "upstream_shape"
	// CodeResponseEncode is a neutral answer that would not render in the
	// caller's protocol.
	CodeResponseEncode = "response_encode"
	// CodeConversionFailed is a request the target deployment cannot express.
	CodeConversionFailed = "conversion_failed"
	// CodeCredentialUnavailable is a credential dorang holds but cannot use —
	// an OAuth token that failed to refresh, most often.
	CodeCredentialUnavailable = "credential_unavailable"
	// CodeUpstreamRequest is a request dorang could not even construct.
	CodeUpstreamRequest = "upstream_request"
	// CodeMalformedToolArguments is a stream that ended on a tool call whose
	// arguments are not valid JSON. dorang does not execute tools and cannot
	// repair the call; the client was told in band, and this is the counted half
	// of the same fact (DESIGN §12.4).
	CodeMalformedToolArguments = "malformed_tool_arguments"
	// CodeUpstreamStreamError is an upstream failure delivered IN BAND, inside a
	// stream the upstream had already answered 200 to (COMPATIBILITY 1.3).
	//
	// It is dorang's code for the condition rather than the upstream's own,
	// because the upstream's own is a code for a 500 that arrived on a 200 and
	// an operator reading "internal_error" next to "status 200" learns nothing.
	// The upstream's words are where they always are: NativeType and
	// NativeMessage.
	CodeUpstreamStreamError = "upstream_stream_error"
	// CodeUpstreamStreamTruncated is a stream that stopped without its family's
	// end-of-stream marker: no [DONE], no message_stop, no stop reason. The
	// generation was cut off, and the part that arrived is a prefix of an answer
	// rather than an answer.
	CodeUpstreamStreamTruncated = "upstream_stream_truncated"
)

// streamError classifies a relay that did not end cleanly.
//
// A tool call whose arguments never parsed is not "dorang could not understand
// this answer": the frames were all readable and the stream was relayed in full.
// It gets its own code because the operator's next step differs — one is a
// broken backend envelope, the other a model that stopped mid-call — and because
// the client has already been told, in band, which no other branch here can say.
func streamError(err error) *server.Error {
	var rf *relayFailure
	if errors.As(err, &rf) {
		return rf.serverError()
	}
	if errors.Is(err, openai.ErrMalformedToolArguments) ||
		errors.Is(err, anthropic.ErrMalformedToolArguments) {
		return server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"the upstream ended a tool call whose arguments are not valid JSON").
			WithCode(CodeMalformedToolArguments)
	}
	return server.NewError(http.StatusBadGateway, server.TypeAPIError,
		"the upstream stream ended abnormally: "+err.Error()).WithCode(CodeUpstreamDecode)
}

// relayFailure is a stream the upstream answered 200 to and then did not
// complete.
//
// It exists because the relay's return value was the one place these two
// conditions had nowhere to go. A 200 is spent the moment the first frame is
// written (DESIGN §7.6), so the client is told in band (COMPATIBILITY 1.3) —
// but "the client was told" is not the same fact as "the attempt failed", and
// only the second one reaches internal/health, the meter and the fail-back
// boundary. Returning nil from the relay asserted the second, and it was not
// true.
//
// It is a type rather than two sentinels because the in-band case carries the
// upstream's own envelope and the truncation case carries nothing at all: there
// is no envelope, which IS the condition.
type relayFailure struct {
	// err is the failure, already normalized into dorang's envelope and already
	// scrubbed. Never nil.
	err *server.Error
}

func (e *relayFailure) Error() string {
	if e == nil || e.err == nil {
		return "backend: the upstream stream did not complete"
	}
	return e.err.Message
}

func (e *relayFailure) serverError() *server.Error { return e.err }

// truncatedStream is a stream that stopped without its family's end marker.
//
// COMPATIBILITY §4.4 permits synthesizing a terminal chunk when the backend
// never sent one. That licence is for a stream that ENDED — the frames all
// arrived, the backend simply omitted the finish_reason its own protocol makes
// optional. It is not for a stream that BROKE, and the difference is not
// cosmetic: dressing a cut-off generation in finish_reason "stop" hands the
// client a half answer labelled complete, which no client can detect and every
// client will act on.
func truncatedStream() *relayFailure {
	return &relayFailure{err: server.NewError(http.StatusBadGateway, server.TypeAPIError,
		"the upstream stream ended without its terminator, so the answer is incomplete").
		WithCode(CodeUpstreamStreamTruncated)}
}

// inBandFailure records an upstream error frame.
//
// The upstream's own type is kept only when it is outside dorang's vocabulary,
// which is COMPATIBILITY §11.3's split applied to the one envelope
// [server.Normalize] never sees: an error that arrived as a frame rather than
// as a body. Its message goes to NativeMessage and stays there — the client has
// already been told in band, and §11.3's reason for keeping upstream text out of
// dorang's own envelope (several servers quote the offending key into it) is
// exactly as true of a frame as of a body.
func inBandFailure(e *canonical.Error, secrets []string) *relayFailure {
	out := server.NewError(http.StatusBadGateway, server.TypeAPIError,
		"the upstream failed inside a stream it had already begun").
		WithCode(CodeUpstreamStreamError)
	if e == nil {
		return &relayFailure{err: out}
	}
	if server.KnownTypes(e.Type) {
		out.Type = e.Type
	} else if e.Type != "" {
		out.NativeType = scrub(e.Type, secrets)
	}
	out.NativeMessage = scrub(e.Message, secrets)
	return &relayFailure{err: out}
}

// normalizedFailure records an upstream error frame the relay saw as bytes
// rather than as a decoded event.
//
// [server.Normalize] is the single implementation of COMPATIBILITY §11's
// taxonomy and is used here for the same reason [upstreamError] uses it: an
// in-band envelope is one of the same five shapes, and a second reading of that
// table is how two parts of one gateway come to disagree about what a backend
// said.
func normalizedFailure(frame []byte, secrets []string) *relayFailure {
	e := server.Normalize(http.StatusBadGateway, frame)
	e.Message = "the upstream failed inside a stream it had already begun"
	e.NativeMessage = scrub(e.NativeMessage, secrets)
	e.NativeType = scrub(e.NativeType, secrets)
	e.Code = CodeUpstreamStreamError
	return &relayFailure{err: e}
}

// credentialHeaders are the headers this package puts key material into. They
// are the whole list, per adapter: bearer for the OpenAI-shaped families and the
// two non-chat vendors, x-api-key for messages, x-goog-api-key for Gemini.
var credentialHeaders = [...]string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"}

// redacted replaces a credential in text that is about to leave dorang.
const redacted = "[redacted]"

// collectSecrets reads back what was actually applied to an outbound request.
//
// It reads the HEADERS rather than the credential table because an OAuth token
// is applied by code this package does not own and never sees (DESIGN §11.2b),
// and the header is the one place a static key and a refreshed token look the
// same. Both the whole header value and the token inside a "Bearer …" are kept,
// because an upstream that echoes a credential may echo either form.
func collectSecrets(h http.Header) []string {
	var out []string
	for _, name := range credentialHeaders {
		v := h.Get(name)
		if v == "" {
			continue
		}
		out = append(out, v)
		if tok, ok := strings.CutPrefix(v, "Bearer "); ok && tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// scrub removes every credential from a string that is about to leave dorang.
//
// DESIGN §10.6's fourth rule: "Credentials are stripped in both directions.
// 'Never reach the client' was stated only for the request path. A backend that
// echoes back the key it was given would have that relayed straight through."
// An upstream error body is exactly that channel — [server.Normalize] quotes the
// upstream's own message, and a bounded excerpt of an unrecognizable body, into
// the envelope dorang answers with. Both are places a 401 saying "invalid key:
// sk-…" or a 500 HTML page rendering the request headers lands the credential in
// the client's hands, and neither needs an attacker: a helpful error message is
// enough.
func scrub(s string, secrets []string) string {
	if s == "" {
		return s
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, redacted)
		}
	}
	return s
}

// scrubBytes is [scrub] over a body that is relayed rather than parsed.
//
// It returns nil for an empty body so that "the upstream said nothing" stays
// distinguishable from "the upstream said the empty string", and it copies
// rather than aliasing the response buffer, because the result outlives the
// request that produced it.
func scrubBytes(body []byte, secrets []string) []byte {
	if len(body) == 0 {
		return nil
	}
	return []byte(scrub(string(body), secrets))
}

// upstreamError normalizes an upstream failure into dorang's envelope.
//
// The heavy lifting is [server.Normalize], which is the single implementation
// of COMPATIBILITY §11's taxonomy: five envelope shapes in, one out, the
// upstream's `type` replaced by the canonical vocabulary and preserved
// out-of-band, a numeric `code` stringified, and an unrecognizable body quoted
// only as a bounded excerpt. Duplicating that here would be two
// implementations of one normative table, which is the defect §11 exists to
// prevent.
//
// What this adds is the part that needs the HTTP response and not just the
// body: §11.4's Retry-After. A 429 that reaches a client without one degrades
// every SDK's backoff to a fixed guess, and the provider's own value is the
// only accurate one anybody has.
func upstreamError(status int, body []byte, h http.Header, secrets []string) *server.Error {
	e := server.Normalize(status, body)
	// Every field of the normalized error that came from the upstream's bytes,
	// scrubbed before anything can render it.
	//
	// COMPATIBILITY §11.3 is what decides where each one goes: Normalize no
	// longer copies the upstream's text into Error.Message at all — the body
	// gets dorang's own canonical wording — so the upstream's words live in
	// NativeMessage, which reaches the ledger and the log. Scrubbing has to
	// follow the text: scrubbing Message and not NativeMessage would have been
	// a scrubber pointed at a field that no longer carries anything.
	//
	// Message is scrubbed anyway. It costs a strings.Contains over a short
	// constant and it is the field a future branch would leak into.
	e.Message = scrub(e.Message, secrets)
	e.NativeMessage = scrub(e.NativeMessage, secrets)
	e.NativeType = scrub(e.NativeType, secrets)
	e.Code = scrub(e.Code, secrets)
	if w := retryAfter(h); w > 0 {
		secs := int(w / time.Second)
		if secs < 1 {
			secs = 1
		}
		e.RetryAfterSeconds = secs
	}
	return e
}

// redirectError is a 30x that dorang refused to follow.
//
// The Location is deliberately absent from the message. It is a string the
// UPSTREAM chose, and this response is the one place an operator would paste
// into a ticket; a host an attacker picked has no business travelling further
// than the packet it arrived in. The status and the rule are enough to
// diagnose, and §10.6's whole point is that the redirect target is not to be
// trusted.
func redirectError(status int) *server.Error {
	return server.NewError(http.StatusBadGateway, server.TypeAPIError,
		"the upstream answered with a redirect, which dorang does not follow").
		WithCode(CodeUpstreamRedirect)
}

// transportError is a request that produced no HTTP response.
//
// The transport error's own text is deliberately NOT relayed. It carries no
// credential — net/http builds it from the URL and the connection, neither of
// which this package puts key material into — but it does carry the operator's
// internal hostname, port and IP, e.g. `dial tcp 10.0.3.14:8000: connect:
// connection refused`, and that is a map of the internal network handed to
// anyone holding an ordinary key. internal/server/passthrough.go states the
// rule for the same condition and has since the relay was written; the
// dispatch path states it now too. Three call sites, one answer.
//
// The detailed error is not lost: it is returned alongside for the caller to
// log, which is where a hostname belongs.
func transportError(err error) (e *server.Error, timeout bool) {
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return server.NewError(http.StatusGatewayTimeout, server.TypeAPIError,
			"the upstream did not answer in time").WithCode(CodeTimeout), true
	}
	if errors.Is(err, context.Canceled) {
		return server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"the request was cancelled before the upstream answered").
			WithCode(CodeUpstreamUnreachable), false
	}
	return server.NewError(http.StatusBadGateway, server.TypeAPIError,
		"could not reach the upstream provider").WithCode(CodeUpstreamUnreachable), false
}

// isTimeout reports whether err is a net timeout without depending on its type.
func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// unsupportedError renders an operation this deployment does not serve.
//
// §11.2 fixes the type for a declared-but-unimplemented route at `api_error`
// with a 501; the CODE names which thing is unimplemented, because "not
// implemented" without a subject sends the operator to read dorang's source.
func unsupportedError(code, message string) *server.Error {
	return server.NewError(http.StatusNotImplemented, server.TypeAPIError, message).
		WithCode(code)
}

// encodeError is a request the selected deployment cannot express.
//
// An [anthropic.OpaqueError] is not flattened. It is dorang declining to invent
// opaque state, and it carries the two fields that make the refusal actionable:
// Reason, which its own ToError renders as the wire `code`, and Construct, which
// is the value §10.1 says the caller "can put in x-dorang-allow-lossy and retry
// deliberately". Rendering both into a sentence under the generic
// conversion_failed code is what made that mechanism unusable — a caller was
// told what went wrong in prose and given nothing to act on.
//
// The construct id is in the message rather than in Param because Param names an
// offending REQUEST FIELD and the construct is not one: `thinking_block` is a
// capability, and a client that fed it to a field-locating SDK would be pointed
// at a member that does not exist.
func encodeError(err error) *server.Error {
	var oe *anthropic.OpaqueError
	if errors.As(err, &oe) {
		we := oe.ToError()
		msg := we.Message
		if oe.Construct != "" {
			msg += "; retry with x-dorang-allow-lossy: " + oe.Construct
		}
		e := server.NewError(we.Status(), server.TypeInvalidRequest, msg)
		// The reason IS the code, which is the whole of the machine-readable
		// half: a caller branching on it can map the refusal to the construct
		// without parsing the sentence.
		return e.WithCode(we.Code)
	}
	return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
		"the request cannot be expressed by the selected deployment: "+err.Error()).
		WithCode(CodeConversionFailed)
}

// credentialError is a credential dorang holds but cannot use.
//
// The credential id is named and nothing else is. The applier's own error does
// not appear: DESIGN §11.2b requires that a refresher's message never be
// wrapped, because it is provider code and its errors can carry the token it
// failed to exchange.
func credentialError(id string) *server.Error {
	return server.NewError(http.StatusBadGateway, server.TypeAPIError,
		"the credential selected for this request is not usable: "+id).
		WithCode(CodeCredentialUnavailable)
}

// retryAfter reads a provider-signalled cooldown from a response.
func retryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
