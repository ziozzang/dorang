package backend

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/server"
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
)

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
	// scrubbed before anything can render it: the message goes in the envelope,
	// the native type goes in a response header, and the code goes in both.
	e.Message = scrub(e.Message, secrets)
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
// The transport error's own text is included because it names the host and the
// syscall, which is what makes an outage debuggable — and because it cannot
// carry a credential: it is produced by net/http from the URL and the
// connection, neither of which this package ever puts key material into.
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
		"upstream request failed: "+err.Error()).WithCode(CodeUpstreamUnreachable), false
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
func encodeError(err error) *server.Error {
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
