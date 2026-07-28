package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
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
	// Type is one of the canonical vocabulary in [KnownTypes]. A native type
	// outside it is replaced and preserved in NativeType.
	Type string
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
	// RetryAfterSeconds populates Retry-After on a 429. Not in the envelope.
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
	// Callers that record this must scrub it first; internal/redact is the
	// scrubber, and internal/app applies it with the exact secret that request
	// carried.
	NativeMessage string
	// NativeCodeWasNumeric records that the upstream sent a number where the
	// contract requires a string. Diagnostic only.
	NativeCodeWasNumeric bool
	// Shape records which upstream envelope [Normalize] recognized.
	Shape Shape
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

// TypeForStatus is the canonical type for an HTTP status.
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
		e.Code, e.NativeCodeWasNumeric = canonicalCode(in.Code, status)
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
		e.Code, e.NativeCodeWasNumeric = canonicalCode(probe.Code, status)
		return finishError(e, status)

	default:
		return opaqueError(status, body)
	}

	e.Type, e.NativeType = canonicalType("", status)
	e.Code, e.NativeCodeWasNumeric = canonicalCode(nil, status)
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
	e.Code, _ = canonicalCode(nil, status)
	return finishError(e, status)
}

// nativeMessageLimit bounds the recorded native text.
//
// It is a ledger column and a log line, not a response body, so the bound is
// about storage and about not letting a backend write a megabyte into every
// row — the confidentiality question is answered by the text not being relayed
// at all, and by internal/redact scrubbing what is kept.
const nativeMessageLimit = 512

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

// canonicalCode turns whatever the backend put in "code" into a string.
//
// This is the defensive half of §7.1. dorang cannot stop a backend sending
// {"code": 429}; it can stop that reaching a client. A code that is an object or
// an array is carried as its raw text rather than dropped, because losing it
// entirely is worse than carrying something odd.
func canonicalCode(raw json.RawMessage, status int) (code string, wasNumeric bool) {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0, bytes.Equal(raw, []byte("null")):
		return strconv.Itoa(status), false
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			return strconv.Itoa(status), false
		}
		return s, false
	case raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9'):
		return string(raw), true
	default:
		return string(clampBytes(raw, excerptLimit)), true
	}
}

// appendEnvelope writes the four-key envelope. Key order is fixed here rather
// than inherited from a struct definition, because it is part of the golden
// bytes.
func appendEnvelope(dst []byte, e *Error) []byte {
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

// EncodeError renders the envelope exactly as it goes on the wire.
func EncodeError(e *Error) []byte {
	if e == nil {
		e = NewError(http.StatusInternalServerError, TypeAPIError, "unknown error")
	}
	return appendEnvelope(make([]byte, 0, 128), e)
}

// WriteError writes the envelope as a complete HTTP response.
//
// It is safe to call with headers already stamped; it does not stamp them.
// Retry-After is attached on a 429 whether or not detail headers were asked
// for — it is a standard header a client acts on, not a dorang extension whose
// absence is merely inconvenient (§10.4's bounding rule is about the x-dorang-*
// set).
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
	if e.Status == http.StatusTooManyRequests && e.RetryAfterSeconds > 0 {
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
	w.WriteHeader(e.StatusCode())
	_, _ = w.Write(*buf)
}

// appendSSEError renders a mid-stream error as the two frames COMPATIBILITY
// §1.3 requires: the error in band, then the terminator.
//
// Once the first chunk has gone out the HTTP status is already 200 and cannot
// change. That is exactly the boundary DESIGN §7.6 refuses to cross with a
// fallback, and it is why this exists at all: the only channel left is the body.
func appendSSEError(dst []byte, e *Error) []byte {
	dst = append(dst, "data: "...)
	dst = appendEnvelope(dst, e)
	return append(dst, "\n\ndata: [DONE]\n\n"...)
}
