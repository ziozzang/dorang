package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
)

// Error is the one error shape that leaves dorang.
//
// COMPATIBILITY §7.1 fixes the envelope at
// {"error":{"message":str,"type":str,"param":str|null,"code":"<string>"}} with
// **code a string, not a number**. Clients written against OpenAI do
// err.code.startswith(...) or switch on a string; an integer there raises inside
// the SDK before the message is ever surfaced.
//
// Backends disagree about all of this. vLLM sends an integer code and a Python
// exception name as the type (VLLM.md §2.2). SGLang serves five different
// envelope shapes from one process — flat for OpenAI routes, nested for the same
// errors when streaming, OpenAI-nested for /v1/responses, Anthropic-shaped for
// /v1/messages, and a bare string from its auth middleware (SGLANG.md §6.2). And
// DESIGN §4.3 records one host serving two routes with two different envelopes,
// so "the provider" is not a unit of consistency either. [Normalize] takes all of
// them and produces this.
type Error struct {
	// Message is the human-readable text. Never empty on the wire.
	Message string
	// Type is one of the canonical vocabulary in [KnownTypes], in the ANTHROPIC
	// family's spelling. A native type outside it is replaced and preserved in
	// NativeType. [Error.forFamily] projects it onto the caller's family before
	// it goes on the wire, so this is the value a raiser sets and not
	// necessarily the value a client reads.
	Type string
	// AltType is the Anthropic family's spelling when it cannot be derived from
	// the status — COMPATIBILITY §11.2's two capacity rows, where a 429 is
	// `rate_limit_error` to an OpenAI client and `overloaded_error` to an
	// Anthropic one. Empty means the status decides, which is the ordinary case.
	AltType string
	// Param names the offending request field, or is nil for "not about a
	// parameter". The distinction is on the wire as null and clients read it,
	// so it is a pointer rather than an empty string.
	Param *string
	// Code is always a string. When the backend sent nothing usable it is the
	// stringified HTTP status, which is what the reference proxy does and what
	// clients already have in hand.
	Code string

	// Status is the HTTP status dorang answers with. Not on the wire.
	Status int
	// RetryAfterSeconds populates Retry-After on the three retry-signalling
	// statuses ([retryAfterStatus]). Not in the envelope.
	RetryAfterSeconds int
	// NativeType is the upstream's own type string when it was outside the
	// canonical vocabulary. It is surfaced as x-dorang-native-error-type,
	// never in the envelope — the same out-of-band treatment COMPATIBILITY
	// §4.2a gives a native stop reason.
	NativeType string
	// NativeMessage is the upstream's own message text.
	//
	// It is recorded — ledger, operator log — and is NEVER put in the response
	// body. COMPATIBILITY §11.3 says so, and the reason is concrete rather
	// than tidy: several OpenAI-compatible servers answer 401 with the
	// offending key quoted in the message, so a gateway that copies the
	// upstream's text into its own envelope hands the operator's provider
	// credential to whichever tenant happened to send a request while a
	// credential was invalid, revoked, or mid-rotation. A hostile backend does
	// not have to wait for that: it can answer any request with the x-api-key
	// header it was just given.
	//
	// Callers that record this must scrub it first. internal/backend is the
	// caller on the dispatch path, and it scrubs with the exact secret that
	// request carried, read back off the outbound headers. internal/redact
	// states the rule; internal/app does not apply it and never did.
	NativeMessage string
	// NativeCode is the upstream's own code when it could not become the
	// envelope's code — a number where §7.1 requires a string, or a JSON object
	// or array where it requires a scalar. It is surfaced as
	// x-dorang-native-error-code and never in the envelope.
	//
	// It is empty when the upstream sent a usable string code, because that code
	// IS the envelope's code: a header that repeated it would be present on
	// every error and so would signal nothing.
	//
	// Unlike NativeMessage this is safe to put on a response header. A code is a
	// short token in every backend anyone has observed, and the credential-echo
	// problem NativeMessage's comment describes is a property of free-text
	// message fields, not of an enumerated one. It is clamped and CTL-checked at
	// the header anyway, because "no backend does that" is not a control.
	NativeCode string
	// NativeCodeWasNumeric records that the upstream sent a number where the
	// contract requires a string. Diagnostic only.
	NativeCodeWasNumeric bool
	// Shape records which upstream envelope [Normalize] recognized.
	Shape Shape

	// family is the caller's protocol family, and it decides which ENVELOPE
	// this error is rendered in — not only which spelling of Type goes inside
	// it. COMPATIBILITY §11.1 gives the two families different objects: the
	// OpenAI one is `{"error":{…}}` and the Anthropic one wraps it in an outer
	// `{"type":"error"}` discriminator that that SDK dispatches on. An
	// Anthropic client handed the OpenAI object reads a missing key and cannot
	// classify the failure at all.
	//
	// It is written only by [Error.ForFamily], which §11.2 already names as
	// "the single path every error response takes", so the envelope and the
	// type are chosen by the same call and cannot disagree. The zero value is
	// [FamilyNone], whose Anthropic() is false, so an error that never met a
	// route renders as it always did.
	family Family
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Message
}

// StatusCode is the status to answer with; zero means 500.
func (e *Error) StatusCode() int {
	if e == nil || e.Status == 0 {
		return http.StatusInternalServerError
	}
	return e.Status
}

// WithParam names the offending field and returns e, for chaining.
func (e *Error) WithParam(p string) *Error { e.Param = &p; return e }

// WithCode overrides the code. It takes a string, and only a string.
func (e *Error) WithCode(c string) *Error { e.Code = c; return e }

// Shape identifies the upstream envelope an error was decoded from. It exists
// so a deployment can be told *which* of the five shapes its backend produces
// without anyone having to read a packet capture.
type Shape uint8

// The recognized upstream envelope shapes.
const (
	// ShapeNative is an error dorang authored itself.
	ShapeNative Shape = iota
	// ShapeNested is OpenAI's {"error":{…}}.
	ShapeNested
	// ShapeFlat is {"object":"error","message",…} — SGLang's OpenAI routes.
	ShapeFlat
	// ShapeAnthropic is {"type":"error","error":{"type","message"}}.
	ShapeAnthropic
	// ShapeBareString is {"error":"Unauthorized"} — SGLang's auth middleware.
	ShapeBareString
	// ShapeDetail is FastAPI's {"detail":…} validation failure.
	ShapeDetail
	// ShapeOpaque is a body that is not a recognizable error envelope at all:
	// HTML from a load balancer, a plain-text 502, an empty body.
	ShapeOpaque
)

// String names the shape for logs and headers.
func (s Shape) String() string {
	switch s {
	case ShapeNative:
		return "native"
	case ShapeNested:
		return "nested"
	case ShapeFlat:
		return "flat"
	case ShapeAnthropic:
		return "anthropic"
	case ShapeBareString:
		return "bare_string"
	case ShapeDetail:
		return "detail"
	default:
		return "opaque"
	}
}

// The canonical error type vocabulary. A client that branches on type has to
// see one of these; SGLANG.md §6.2 observes "BadRequestError", "BadRequest",
// "Bad Request" and the stringified status "400" arriving in that field from
// four call paths of a single server, and concludes dorang "must not branch on
// type". Nor may it forward one, for the same reason.
const (
	TypeInvalidRequest     = "invalid_request_error"
	TypeAuthentication     = "authentication_error"
	TypePermission         = "permission_error"
	TypeNotFound           = "not_found_error"
	TypeRequestTooLarge    = "request_too_large"
	TypeRateLimit          = "rate_limit_error"
	TypeAPIError           = "api_error"
	TypeOverloaded         = "overloaded_error"
	TypeTimeout            = "timeout_error"
	TypeNotImplemented     = "not_implemented_error"
	TypeServiceUnavailable = "service_unavailable_error"
)

// The error codes COMPATIBILITY §11.2 pins by name, for the conditions this
// package raises. They are constants rather than literals because §11.2 is a
// contract with clients and a literal is how the code and the table drifted
// apart in the first place — `invalid_body` sat where §11.2 says
// `invalid_request` through two audits.
//
// Codes for conditions raised elsewhere live with the condition:
// internal/router's routing refusals, internal/auth's credential refusals.
const (
	// CodeInvalidRequest is §11.2's "Malformed request body". Every way a body
	// fails to parse — not an object, not multipart, unreadable — is that one
	// condition, and a client that branches on it should not have to know which.
	CodeInvalidRequest = "invalid_request"
	// CodeRequestTooLarge is §11.2's "Body over the size cap".
	CodeRequestTooLarge = "request_too_large"
	// CodeModelNotFound is §11.2's "Unknown model".
	CodeModelNotFound = "model_not_found"
	// CodeRouteNotImplemented is §11.2's "Route declared but unimplemented".
	CodeRouteNotImplemented = "route_not_implemented"
	// CodeTimeout is §11.2's "Upstream timeout".
	CodeTimeout = "timeout"
	// CodeInternalError is §11.2's "Gateway fault".
	CodeInternalError = "internal_error"
)

// knownTypes is the set a native type is checked against.
var knownTypes = map[string]struct{}{
	TypeInvalidRequest:     {},
	TypeAuthentication:     {},
	TypePermission:         {},
	TypeNotFound:           {},
	TypeRequestTooLarge:    {},
	TypeRateLimit:          {},
	TypeAPIError:           {},
	TypeOverloaded:         {},
	TypeTimeout:            {},
	TypeNotImplemented:     {},
	TypeServiceUnavailable: {},
}

// KnownTypes reports whether t is in the canonical vocabulary.
func KnownTypes(t string) bool { _, ok := knownTypes[t]; return ok }

// TypeForStatus is the canonical type for an HTTP status, in the ANTHROPIC
// family's spelling.
//
// COMPATIBILITY §11.2 gives one type per condition PER FAMILY, and the two
// columns are not the same function: a 404 is `not_found_error` to an Anthropic
// client and `invalid_request_error` to an OpenAI one, and a 413 is
// `request_too_large` and `invalid_request_error` respectively. This function
// answers the Anthropic column because that is the one that is a function of the
// status alone; [TypeForFamily] projects it onto whichever family is asking, and
// is what the response path calls.
//
// 429 for "no healthy deployment" rather than 503 is COMPATIBILITY §7.2's rule
// and lives at the call sites, not here; this function only maps a status that
// has already been decided.
func TypeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return TypeAuthentication
	case status == http.StatusForbidden:
		return TypePermission
	case status == http.StatusNotFound:
		return TypeNotFound
	case status == http.StatusRequestEntityTooLarge:
		return TypeRequestTooLarge
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return TypeTimeout
	case status == http.StatusTooManyRequests:
		return TypeRateLimit
	case status == http.StatusNotImplemented:
		return TypeNotImplemented
	case status == http.StatusServiceUnavailable, status == 529:
		return TypeOverloaded
	case status >= 400 && status < 500:
		return TypeInvalidRequest
	default:
		return TypeAPIError
	}
}

// TypeForFamily is the type family f spells for a condition whose Anthropic
// spelling is t. It is the projection COMPATIBILITY §11's opening paragraph
// requires: "dorang emits the vendor's own error vocabulary, chosen by which
// family the caller is speaking".
//
// The columns of §11.2 differ only where the OpenAI family has no separate type
// for a condition the Anthropic family names:
//
//	Unknown model / unknown resource   404   not_found_error    → invalid_request_error
//	Body over the size cap             413   request_too_large  → invalid_request_error
//	Overloaded / unavailable           503   overloaded_error   → api_error
//
// `overloaded_error` is in no OpenAI column of §11.2 — the two 429 rows that
// spell it in the Anthropic column read `rate_limit_error` in the OpenAI one —
// so it is Anthropic vocabulary and an OpenAI client branching on it lands in a
// default case. §11.2 has no 503 row at all, and `api_error` is what the rest of
// its 5xx rows use.
//
// Everything else is spelled the same in both columns, so this is a three-entry
// fold rather than a table. It is idempotent: an error that already carries the
// OpenAI spelling passes through, which is what lets a handler that knows its own
// family set the type directly (`handleModelRetrieve` does) without this
// undoing it.
//
// The 429 rows of §11.2 that read `overloaded_error` in the Anthropic column —
// no healthy deployment, capacity wait timed out — are NOT folded here, because
// they are chosen by condition rather than by status: a 429 is `rate_limit_error`
// to both families when it is an actual rate limit. Those two set [Error.AltType]
// at the point they are raised.
//
// Three types fold on BOTH families. `timeout_error`, `not_implemented_error`
// and `service_unavailable_error` are in dorang's own [knownTypes] and in
// NEITHER vendor's vocabulary; §11.2's rows for those conditions — upstream
// timeout, route declared but unimplemented — read `api_error` in both columns.
// §11 opens by saying that a gateway which invents its own vocabulary "is as
// incompatible as one that changes a field name", so they do not go on the wire.
// Nothing is lost: what a client acts on for a 501 is the CODE
// (`route_not_implemented` versus `route_unknown`, DESIGN §0.3), and the status
// carries the rest.
//
// They stay in [knownTypes] because that set decides whether an UPSTREAM's type
// needs preserving on x-dorang-native-error-type, which is a different question.
func TypeForFamily(f Family, t string) string {
	switch t {
	case TypeTimeout, TypeNotImplemented, TypeServiceUnavailable:
		return TypeAPIError
	}
	if f.Anthropic() {
		return t
	}
	switch t {
	case TypeNotFound, TypeRequestTooLarge:
		return TypeInvalidRequest
	case TypeOverloaded:
		return TypeAPIError
	}
	return t
}

// compat11Types is the type vocabulary of COMPATIBILITY §11.2, as data.
//
// It exists so that "dorang never emits a `type` outside this table" is a
// checkable statement rather than a promise; TestEveryTypeOnTheWireIsInTheTable
// walks every status and both families through [TypeForFamily] and requires the
// answer to be in here.
var compat11Types = map[string]struct{}{
	TypeInvalidRequest:  {},
	TypeAuthentication:  {},
	TypePermission:      {},
	TypeNotFound:        {},
	TypeRequestTooLarge: {},
	TypeRateLimit:       {},
	TypeAPIError:        {},
	TypeOverloaded:      {},
}

// ForFamily resolves the type for the family the caller is speaking and returns
// e, for chaining. It is what the response path calls, and it is the only place
// a family reaches an envelope.
//
// It resolves TWO things, and for a long time it resolved only one. The type is
// projected onto §11.2's column for f, and the family is recorded so that
// [appendEnvelope] renders §11.1's object for f. Choosing the vocabulary and
// then serializing it into the other family's envelope is not half right: the
// Anthropic SDK dispatches on the outer `"type":"error"` member, so it never
// reaches the correctly-spelled type inside.
//
// It is exported so that a compatibility test can assert the bytes a client of
// each family receives for a given condition. §11.2 is a per-family contract,
// and a test that can only see one family's column can only check half of it.
func (e *Error) ForFamily(f Family) *Error {
	if e == nil {
		return nil
	}
	e.family = f
	if f.Anthropic() && e.AltType != "" {
		e.Type = e.AltType
		return e
	}
	e.Type = TypeForFamily(f, e.Type)
	return e
}

// Errorf is deliberately absent. Constructing an error message with a format
// verb is prohibited on the hot path (DESIGN §15.5), and an error that is not
// on the hot path is still better off with a constant message and a param.

// NewError builds a dorang-authored error. An empty type is filled from the
// status; the code defaults to the stringified status.
func NewError(status int, typ, message string) *Error {
	if typ == "" {
		typ = TypeForStatus(status)
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return &Error{
		Message: message,
		Type:    typ,
		Code:    strconv.Itoa(status),
		Status:  status,
		Shape:   ShapeNative,
	}
}

// excerptLimit bounds how much of an unrecognizable upstream body is quoted
// back to the client. Enough to identify an HTML error page or a proxy banner;
// not enough to relay a stack trace.
const excerptLimit = 256

// Normalize converts any upstream error body into dorang's envelope.
//
// The only discriminators it trusts are the HTTP status and the *structure* of
// the body. It never branches on the upstream's type string (SGLANG.md §6.2:
// "the only stable discriminators are the HTTP status and object == error"),
// and it never lets a numeric code through.
//
// # No upstream byte reaches Message
//
// Every branch below puts the upstream's text in [Error.NativeMessage] and
// leaves [Error.Message] to [canonicalMessage], which is written here and
// depends only on the status. That is COMPATIBILITY §11.3, and it is enforced
// structurally rather than by care: there is no assignment from the decoded
// body to Message anywhere in this function, so a new branch cannot leak by
// forgetting a rule — it would have to write the leak deliberately.
//
// The previous arrangement copied the upstream's message into the envelope on
// all five branches, four of them unbounded. The bounded one carried a comment
// explaining that its 256-byte excerpt stopped a credential being "relayed
// wholesale"; an sk- key is 51 to 164 characters and fits with room to spare.
//
// It always returns a usable error, including for an empty body, a body that is
// not JSON, and a body that is JSON but not an error.
func Normalize(status int, body []byte) *Error {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		e := NewError(status, "", canonicalMessage(status))
		e.Shape = ShapeOpaque
		// finishError, not a bare return: http.StatusText is empty for a
		// status nobody registered, and a backend is perfectly capable of
		// answering 368. An envelope with an empty message is a client-side
		// crash waiting to happen.
		return finishError(e, status)
	}
	if body[0] != '{' {
		return opaqueError(status, body)
	}

	var probe struct {
		Error   json.RawMessage `json:"error"`
		Object  string          `json:"object"`
		Type    json.RawMessage `json:"type"`
		Message string          `json:"message"`
		Param   *string         `json:"param"`
		Code    json.RawMessage `json:"code"`
		Detail  json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return opaqueError(status, body)
	}

	e := &Error{Status: status}
	inner := bytes.TrimSpace(probe.Error)

	switch {
	case len(inner) > 0 && inner[0] == '"':
		// SGLang's auth middleware: {"error": "Unauthorized"}. A bare string
		// where every SDK expects an object.
		e.Shape = ShapeBareString
		var s string
		if err := json.Unmarshal(inner, &s); err != nil {
			return opaqueError(status, body)
		}
		e.NativeMessage = s

	case len(inner) > 0 && inner[0] == '{':
		// Either OpenAI-nested or Anthropic. Both put the useful fields in the
		// same place, so they are decoded identically and only labelled apart.
		var in struct {
			Message string          `json:"message"`
			Type    json.RawMessage `json:"type"`
			Param   *string         `json:"param"`
			Code    json.RawMessage `json:"code"`
		}
		if err := json.Unmarshal(inner, &in); err != nil {
			return opaqueError(status, body)
		}
		e.Shape = ShapeNested
		if rawString(probe.Type) == "error" {
			e.Shape = ShapeAnthropic
		}
		e.NativeMessage = in.Message
		e.Param = in.Param
		e.Type, e.NativeType = canonicalType(rawString(in.Type), status)
		e.Code, e.NativeCode, e.NativeCodeWasNumeric = canonicalCode(in.Code, status)
		return finishError(e, status)

	case len(bytes.TrimSpace(probe.Detail)) > 0:
		// FastAPI validation failure, which is what a request-schema rejection
		// looks like on both vLLM and SGLang. The detail is a string or an
		// array of objects; either way it is the only text there is.
		e.Shape = ShapeDetail
		d := bytes.TrimSpace(probe.Detail)
		if d[0] == '"' {
			var s string
			if err := json.Unmarshal(d, &s); err == nil {
				e.NativeMessage = s
			}
		}
		if e.NativeMessage == "" {
			e.NativeMessage = string(clampBytes(d, nativeMessageLimit))
		}

	case probe.Object == "error" || probe.Message != "":
		// SGLang's flat envelope, and anything else that put the fields at the
		// top level. object == "error" is the stable discriminator; a bare
		// "message" is accepted too because vLLM's third validation shape has
		// no object field.
		e.Shape = ShapeFlat
		e.NativeMessage = probe.Message
		e.Param = probe.Param
		e.Type, e.NativeType = canonicalType(rawString(probe.Type), status)
		e.Code, e.NativeCode, e.NativeCodeWasNumeric = canonicalCode(probe.Code, status)
		return finishError(e, status)

	default:
		return opaqueError(status, body)
	}

	e.Type, e.NativeType = canonicalType("", status)
	e.Code, e.NativeCode, e.NativeCodeWasNumeric = canonicalCode(nil, status)
	return finishError(e, status)
}

// finishError fills the fields no shape supplied.
func finishError(e *Error, status int) *Error {
	// Always dorang's own words. The upstream's are in NativeMessage and stay
	// there: this is the single assignment that decides what a client reads,
	// and it can see only the status.
	e.NativeMessage = clampString(e.NativeMessage, nativeMessageLimit)
	if e.Message == "" {
		e.Message = canonicalMessage(status)
	}
	if e.Type == "" {
		e.Type = TypeForStatus(status)
	}
	if e.Code == "" {
		e.Code = strconv.Itoa(status)
	}
	return e
}

// opaqueError wraps a body that is not an error envelope: an HTML page from a
// load balancer, a plain-text 502, a JSON object with none of the known fields.
//
// The excerpt is included because the alternative — "upstream error" and
// nothing else — is the single most common way a gateway makes an outage
// undebuggable. It is bounded so that a stack trace or a credential echoed into
// a 500 page cannot be relayed wholesale.
func opaqueError(status int, body []byte) *Error {
	e := &Error{Status: status, Shape: ShapeOpaque}
	if b := clampBytes(bytes.TrimSpace(body), nativeMessageLimit); len(b) > 0 {
		e.NativeMessage = string(b)
	}
	e.Type, _ = canonicalType("", status)
	e.Code, _, _ = canonicalCode(nil, status)
	return finishError(e, status)
}

// nativeMessageLimit bounds the recorded native text.
//
// It is a ledger column and a log line, not a response body, so the bound is
// about storage and about not letting a backend write a megabyte into every
// row — the confidentiality question is answered by the text not being relayed
// at all, and by internal/backend scrubbing what is kept against the credential
// that request carried (internal/redact states that rule).
const nativeMessageLimit = 512

// nativeCodeHeaderLimit bounds the x-dorang-native-error-code header value, and
// bounds what canonicalCode retains from a code that is a JSON object or array.
// Same reasoning as nativeTypeHeaderLimit: a code is a short token, and a
// megabyte one is a body pretending to be one.
const nativeCodeHeaderLimit = 128

// nativeTypeHeaderLimit bounds the x-dorang-native-error-type header value.
// A type string is a short token in every backend anyone has observed; this is
// generous for one and refuses a body pretending to be one.
const nativeTypeHeaderLimit = 128

// canonicalMessage is dorang's own wording for an upstream failure.
//
// It depends on the status and on nothing else. That is the point: it is the
// only source of the string a client reads for an upstream error, so there is
// no input to it that an upstream controls.
func canonicalMessage(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "the upstream provider rejected this gateway's credential"
	case status == http.StatusForbidden:
		return "the upstream provider refused this request"
	case status == http.StatusNotFound:
		return "the upstream provider does not serve this model or route"
	case status == http.StatusRequestEntityTooLarge:
		return "the upstream provider refused the request as too large"
	case status == http.StatusTooManyRequests:
		return "the upstream provider is rate limiting this gateway"
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return "the upstream provider did not answer in time"
	case status == http.StatusServiceUnavailable, status == 529:
		return "the upstream provider is overloaded"
	case status >= 400 && status < 500:
		return "the upstream provider rejected the request"
	case status >= 500:
		return "the upstream provider failed to serve the request"
	default:
		if s := http.StatusText(status); s != "" {
			return "the upstream provider answered " + s
		}
		return "the upstream provider returned an unexpected answer"
	}
}

// clampString truncates without splitting a UTF-8 sequence.
func clampString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return string(clampBytes([]byte(s), n))
}

// clampBytes truncates b to at most n bytes without splitting a UTF-8 sequence.
func clampBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	b = b[:n]
	for len(b) > 0 && b[len(b)-1]&0xC0 == 0x80 {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1]&0x80 != 0 {
		b = b[:len(b)-1]
	}
	return b
}

// rawString decodes a JSON value that should be a string, returning "" for
// anything else rather than failing.
func rawString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// canonicalType maps a native type into the canonical vocabulary, returning the
// canonical value and the native one when they differ.
func canonicalType(native string, status int) (canonical, keep string) {
	if native == "" {
		return TypeForStatus(status), ""
	}
	if KnownTypes(native) {
		return native, ""
	}
	return TypeForStatus(status), native
}

// canonicalCode turns whatever the backend put in "code" into a string, and
// says what the backend actually sent when the two differ.
//
// This is the defensive half of §7.1. dorang cannot stop a backend sending
// {"code": 429}; it can stop that reaching a client. The envelope gets a code
// that satisfies the contract and `native` gets the upstream's own spelling,
// which [WriteError] puts on x-dorang-native-error-code — §11.3's rule that the
// native code is preserved out of band rather than dropped or forwarded.
//
// The previous arrangement put the raw text in the envelope: a numeric code
// arrived as "429" and a JSON object arrived as its own source text, up to 256
// bytes of it, in a field clients switch on. Both are now canonical in the body
// and intact on the header.
func canonicalCode(raw json.RawMessage, status int) (code, native string, wasNumeric bool) {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0, bytes.Equal(raw, []byte("null")):
		return strconv.Itoa(status), "", false
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			return strconv.Itoa(status), "", false
		}
		// A string code already satisfies the contract, so it IS the envelope's
		// code and there is nothing left over to carry out of band.
		return s, "", false
	case raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9'):
		return strconv.Itoa(status), string(clampBytes(raw, nativeCodeHeaderLimit)), true
	default:
		return strconv.Itoa(status), string(clampBytes(raw, nativeCodeHeaderLimit)), false
	}
}

// appendEnvelope writes the envelope of the family the error was projected
// onto by [Error.ForFamily].
//
// COMPATIBILITY §11.1 defines two objects, not one with a different `type`
// inside it. Everything that is not the Anthropic family gets §7.1's four-key
// object, which is the whitelist [Family.Anthropic] documents: a family that is
// neither — models, health, metrics, admin, passthrough, and the zero value —
// is answered in the OpenAI shape deliberately rather than by omission.
func appendEnvelope(dst []byte, e *Error) []byte {
	if e.family.Anthropic() {
		return appendAnthropicEnvelope(dst, e)
	}
	return appendOpenAIEnvelope(dst, e)
}

// appendAnthropicEnvelope renders §11.1's Anthropic object through
// internal/wire/anthropic, which owns that family's serializer.
//
// It calls that package rather than hand-rolling a second renderer here. The
// reason is not tidiness: the two would be free to drift, and this defect —
// one family's envelope emitted on the other family's route — is what drift
// between two renderers of the same contract looks like. That package's
// [anthropic.Error] carries `param` and `code` alongside the outer
// discriminator, which is deliberate and documented there: §11.1's Anthropic
// object omits both, §7.1 requires both, and emitting the union satisfies each
// reader without either seeing a field it must reject.
//
// The crossing goes through [canonical.Error] because that is the neutral form
// [anthropic.ErrorFrom] takes, and it is a type this package already imports.
//
// This costs a reflective marshal and a copy on a path the OpenAI side renders
// by hand. That is a deliberate trade: an error response is not the steady
// state, and DESIGN §15.5's hand-rolled-appender rule exists to keep a
// serializer off the per-token path, not to justify a second implementation of
// an envelope that another package already renders and golden-tests.
func appendAnthropicEnvelope(dst []byte, e *Error) []byte {
	b, err := anthropic.EncodeError(anthropicError(e))
	if err != nil {
		// Unreachable: the value is four strings and a *string, and
		// encoding/json cannot fail on those. A body in the other family's
		// shape is still a body a client can parse and still carries the
		// status, which is more than nothing at all.
		return appendOpenAIEnvelope(dst, e)
	}
	return append(dst, b...)
}

// anthropicError projects a dorang error onto that family's own type.
//
// Type and Code are passed through rather than re-derived: [Error.ForFamily]
// has already chosen §11.2's Anthropic column, and letting anthropic.NewError
// fill an empty type from the status would quietly re-decide a question that
// was already answered — including for the two 429 capacity rows, whose
// `overloaded_error` is chosen by condition and is NOT a function of the status.
func anthropicError(e *Error) *anthropic.Error {
	return anthropic.ErrorFrom(&canonical.Error{
		StatusCode: e.StatusCode(),
		Message:    e.Message,
		Type:       e.Type,
		Param:      e.Param,
		Code:       e.Code,
	})
}

// appendOpenAIEnvelope writes the four-key envelope. Key order is fixed here
// rather than inherited from a struct definition, because it is part of the
// golden bytes.
func appendOpenAIEnvelope(dst []byte, e *Error) []byte {
	dst = append(dst, `{"error":{"message":`...)
	dst = appendJSONString(dst, e.Message)
	dst = append(dst, `,"type":`...)
	dst = appendJSONString(dst, e.Type)
	dst = append(dst, `,"param":`...)
	if e.Param == nil {
		dst = append(dst, `null`...)
	} else {
		dst = appendJSONString(dst, *e.Param)
	}
	dst = append(dst, `,"code":`...)
	dst = appendJSONString(dst, e.Code)
	return append(dst, '}', '}')
}

// EncodeError renders the envelope exactly as it goes on the wire, in the
// family the error was projected onto by [Error.ForFamily]. An error that has
// met no route renders in §7.1's object, which is what a caller outside this
// package holding an un-projected error should see.
func EncodeError(e *Error) []byte {
	if e == nil {
		e = NewError(http.StatusInternalServerError, TypeAPIError, "unknown error")
	}
	return appendEnvelope(make([]byte, 0, 128), e)
}

// retryAfterStatus reports whether Retry-After means anything on this status.
//
// COMPATIBILITY §11.4. The three admitted statuses are the ones whose whole
// content is "come back later": 429, 503, and the Anthropic family's 529, which
// [TypeForStatus] and [canonicalMessage] already treat as 503's other spelling.
//
// This used to be `status == 429` alone, and the gap was not theoretical. A
// provider's own `Retry-After` on an overloaded 503 or 529 is parsed by
// internal/backend, carried on [Error.RetryAfterSeconds] the whole way here, and
// was then dropped on the floor — a value read, stored, documented, and never
// emitted, which is DESIGN §17.1's dominant class. §11.4's own argument is
// STRONGER for a 503 than for a 429: a client that is rate limited can at least
// infer a window from its own request rate, and a client told the far side is
// overloaded has nothing at all to guess with.
//
// The other 5xx are deliberately excluded rather than forgotten. A 500 or a 502
// is the upstream reporting that something went wrong, not that it will be ready
// at a stated time, and dorang never computes a delay for one; forwarding a
// header a broken backend happened to emit would publish a number under a status
// that makes no claim about recovery.
func retryAfterStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, 529:
		return true
	}
	return false
}

// WriteError writes the envelope as a complete HTTP response.
//
// It is safe to call with headers already stamped; it does not stamp them.
// Retry-After is attached on every status [retryAfterStatus] admits, whether or
// not detail headers were asked for — it is a standard header a client acts on,
// not a dorang extension whose absence is merely inconvenient (§10.4's bounding
// rule is about the x-dorang-* set).
func WriteError(w http.ResponseWriter, e *Error) {
	if e == nil {
		e = NewError(http.StatusInternalServerError, TypeAPIError, "unknown error")
	}
	buf := getBuf()
	defer putBuf(buf)
	*buf = appendEnvelope(*buf, e)

	h := w.Header()
	h.Set("Content-Type", "application/json")
	var lb [20]byte
	h.Set("Content-Length", string(strconv.AppendInt(lb[:0], int64(len(*buf)), 10)))
	if retryAfterStatus(e.Status) && e.RetryAfterSeconds > 0 {
		h.Set("Retry-After", string(strconv.AppendInt(lb[:0], int64(e.RetryAfterSeconds), 10)))
	}
	// An UPSTREAM-controlled string about to become a response header value.
	// unimplemented() already checks the client-controlled path for exactly
	// this, on the stated reasoning that "the framework probably catches it" is
	// not a security argument; a backend is a less trusted source than the
	// caller, so it does not get the weaker treatment. The clamp is separate:
	// the value is decoded from a body read under a 1 MiB cap, and a 1 MiB
	// response header is a denial of service against whatever parses it.
	if nt := clampString(e.NativeType, nativeTypeHeaderLimit); nt != "" && safeHeaderValue(nt) {
		h.Set(HeaderNativeErrorType, nt)
	}
	// The other half of §11.3, and the half that was never built: the envelope
	// carries a code that satisfies §7.1 and this carries the one the backend
	// actually sent. Same clamp and same CTL check, for the same reason.
	if nc := clampString(e.NativeCode, nativeCodeHeaderLimit); nc != "" && safeHeaderValue(nc) {
		h.Set(HeaderNativeErrorCode, nc)
	}
	w.WriteHeader(e.StatusCode())
	_, _ = w.Write(*buf)
}

// writeSSEError delivers a mid-stream error in the SSE conventions of the
// family the caller is speaking.
//
// Once the first chunk has gone out the HTTP status is already 200 and cannot
// change. That is exactly the boundary DESIGN §7.6 refuses to cross with a
// fallback, and it is why this exists at all: the only channel left is the body.
//
// The two families do not frame it the same way, and the difference is not
// cosmetic. COMPATIBILITY §1.1 says a chat-completions frame carries no
// `event:` line and §1.2 terminates the stream with `data: [DONE]`; §6.1 says
// every frame of this other protocol carries BOTH lines and §6.2 says it has no
// `[DONE]` at all and sends no `message_stop` after an error. A client reading
// an Anthropic stream dispatches on the event NAME, so a data-only frame is not
// a frame it mis-parses — it is one it silently discards, leaving a failed
// exchange indistinguishable from a truncated one.
//
// Write errors are discarded here for the same reason the caller discards
// them: the only channel left has just failed, and there is no second one.
func writeSSEError(w io.Writer, e *Error) {
	if e != nil && e.family.Anthropic() {
		// The framing, the event name and the absence of a terminator all come
		// from that package's own writer. Constructing one per failed stream
		// costs a message id that this frame does not use; a stream that is
		// already failing is not the place to optimize that away by copying
		// its framing rules into this file.
		_ = anthropic.NewStreamWriter(w, anthropic.StreamConfig{}).
			WriteError(anthropicError(e))
		return
	}
	buf := getBuf()
	defer putBuf(buf)
	*buf = appendOpenAISSEError(*buf, e)
	_, _ = w.Write(*buf)
}

// appendOpenAISSEError renders a mid-stream error as the two frames
// COMPATIBILITY §1.3 requires: the error in band, then the terminator.
//
// It is the chat-completions framing specifically — §1.1's data-only frame and
// §1.2's `[DONE]` — so it renders §7.1's envelope directly rather than through
// [appendEnvelope]. Routing this through the family switch would let an
// Anthropic envelope out inside chat-completions framing, which is a shape
// neither family's client can read.
func appendOpenAISSEError(dst []byte, e *Error) []byte {
	dst = append(dst, "data: "...)
	dst = appendOpenAIEnvelope(dst, e)
	return append(dst, "\n\ndata: [DONE]\n\n"...)
}
