package anthropic

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Error is the inner object of the error envelope.
//
// Two contracts meet here and COMPATIBILITY does not say which wins.
// 7.1 fixes the inner object at {message, type, param, code} with **code a
// string, not a number**; this family's own envelope is
// {"type":"error","error":{"type":…,"message":…}} and its SDK dispatches on the
// OUTER type. Emitting only one of the two breaks something: drop the outer
// type and the SDK cannot classify the failure, drop param/code and a client
// written against 7.1 reads a missing key.
//
// dorang emits the union. The outer discriminator is present, and param and
// code are present with 7.1's types. Both readers are satisfied and neither
// sees a field it must reject, because every SDK in this family tolerates
// unknown members of the error object.
type Error struct {
	Type    string  `json:"type"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`

	// StatusCode is dorang's HTTP status. It is not on the wire.
	StatusCode int `json:"-"`
	// CodeWasNumeric records that the backend sent a numeric code that dorang
	// stringified. Not on the wire; it exists so the decode path can warn once
	// rather than silently normalizing a protocol violation.
	CodeWasNumeric bool `json:"-"`
}

// Envelope is the outer error object, including the discriminator this family's
// SDK dispatches on.
type Envelope struct {
	Type  string `json:"type"`
	Error Error  `json:"error"`
}

// Error type strings. These are this family's spellings. They mostly coincide
// with the OpenAI package's, with two differences that matter: request_too_large
// exists only here, and overloaded_error is the 529 this family uses where the
// other reaches it from 503.
const (
	TypeInvalidRequest  = "invalid_request_error"
	TypeAuthentication  = "authentication_error"
	TypePermission      = "permission_error"
	TypeNotFound        = "not_found_error"
	TypeRequestTooLarge = "request_too_large"
	TypeRateLimit       = "rate_limit_error"
	TypeAPIError        = "api_error"
	TypeOverloaded      = "overloaded_error"
	TypeTimeout         = "timeout_error"
)

// NewError builds an error whose code defaults to the stringified status, for
// the same reason the OpenAI package does: 7.1 fixes code as a string but not
// what it holds when the backend supplied nothing, and the status is more
// useful to a client than an empty string.
func NewError(status int, typ, message string) *Error {
	if typ == "" {
		typ = TypeForStatus(status)
	}
	return &Error{
		Type:       typ,
		Message:    message,
		Code:       strconv.Itoa(status),
		StatusCode: status,
	}
}

// TypeForStatus is the default error type for an HTTP status.
//
// 429 is deliberate: COMPATIBILITY 7.2 routes "no healthy deployment" to 429
// rather than 503, because clients retry a 429 with backoff and treat a 503 as
// a dead gateway.
func TypeForStatus(status int) string {
	switch {
	case status == 401:
		return TypeAuthentication
	case status == 403:
		return TypePermission
	case status == 404:
		return TypeNotFound
	case status == 408 || status == 504:
		return TypeTimeout
	case status == 413:
		return TypeRequestTooLarge
	case status == 429:
		return TypeRateLimit
	case status == 529 || status == 503:
		return TypeOverloaded
	case status >= 400 && status < 500:
		return TypeInvalidRequest
	default:
		return TypeAPIError
	}
}

// WithParam names the offending request field.
func (e *Error) WithParam(param string) *Error {
	e.Param = &param
	return e
}

// WithCode overrides the code. It takes a string, and only a string.
func (e *Error) WithCode(code string) *Error {
	e.Code = code
	return e
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Message
}

// Status is the HTTP status to answer with. Zero means 500.
func (e *Error) Status() int {
	if e == nil || e.StatusCode == 0 {
		return 500
	}
	return e.StatusCode
}

// UnmarshalJSON accepts a numeric code and stringifies it (COMPATIBILITY 7.1).
// dorang cannot stop a backend sending {"code": 429}; it can stop that reaching
// a client.
func (e *Error) UnmarshalJSON(b []byte) error {
	var raw struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Param   *string         `json:"param"`
		Code    json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	e.Type = raw.Type
	e.Message = raw.Message
	e.Param = raw.Param
	e.Code = ""
	e.CodeWasNumeric = false

	c := trimSpace(raw.Code)
	switch {
	case len(c) == 0, string(c) == "null":
	case c[0] == '"':
		if err := json.Unmarshal(c, &e.Code); err != nil {
			return err
		}
	default:
		e.Code = string(c)
		e.CodeWasNumeric = true
	}
	return nil
}

// EncodeError renders the envelope.
func EncodeError(e *Error) ([]byte, error) {
	if e == nil {
		e = NewError(500, TypeAPIError, "unknown error")
	}
	return Marshal(Envelope{Type: EventError, Error: *e})
}

// DecodeError parses an error body, tolerating a bare inner object.
func DecodeError(b []byte, warn WarnFunc) (*Error, error) {
	b = trimSpace(b)
	if len(b) == 0 {
		return nil, errors.New("anthropic: empty error body")
	}
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	e := env.Error
	if e.Message == "" && e.Type == "" && e.Code == "" {
		var bare Error
		if err := json.Unmarshal(b, &bare); err != nil {
			return nil, err
		}
		e = bare
	}
	if e.CodeWasNumeric {
		warn.warn(WarnNumericErrorCode, e.Code)
	}
	return &e, nil
}

// ErrorFrom converts a neutral error.
func ErrorFrom(c *canonical.Error) *Error {
	if c == nil {
		return NewError(500, TypeAPIError, "unknown error")
	}
	status := c.StatusCode
	if status == 0 {
		status = 500
	}
	e := NewError(status, c.Type, c.Message)
	e.Param = c.Param
	if c.Code != "" {
		e.Code = c.Code
	}
	return e
}

// ToCanonical converts to the neutral error.
func (e *Error) ToCanonical() *canonical.Error {
	if e == nil {
		return nil
	}
	return &canonical.Error{
		StatusCode: e.Status(),
		Message:    e.Message,
		Type:       e.Type,
		Param:      e.Param,
		Code:       e.Code,
	}
}

// ---------------------------------------------------------------------------
// Conversion failures that are NOT wire errors
// ---------------------------------------------------------------------------

// Reasons a conversion refuses rather than degrades. These are the
// machine-readable strings a 400 body names (DESIGN §10.2: "fails with 400 and
// names the reason").
const (
	// ReasonUnsignedThinking is an assistant reasoning block with no integrity
	// material. dorang cannot mint one — the signature's correctness is
	// established on the server that issued it (EXTENSIONS §B.1) — so the block
	// can be neither forwarded nor fabricated.
	ReasonUnsignedThinking = "unsigned_reasoning_block"
	// ReasonRedactedWithoutData is a redacted reasoning block whose opaque
	// payload was lost on an earlier hop (EXTENSIONS §B18). Same rule: the
	// payload IS the block, and an empty one is not a smaller version of it.
	ReasonRedactedWithoutData = "redacted_reasoning_block_without_payload"
	// ReasonMaxTokensUnknown is a crossing into this family with no output
	// ceiling and no catalog value to supply (DESIGN §10.7, trap one).
	ReasonMaxTokensUnknown = "max_tokens_unknown"
)

// OpaqueError is a refusal to fabricate opaque state.
//
// It is separate from [Error] because it is not something a backend said — it
// is dorang declining to invent a value whose correctness it cannot establish.
// The caller turns it into a 400 naming Reason and Construct; EXTENSIONS §B.3
// adds that the same condition must also stop §7.6 from failing over, because a
// sibling deployment produces the identical rejection.
type OpaqueError struct {
	// Reason is one of the Reason* constants.
	Reason string
	// Construct is the canonical construct id, so the caller can put it in
	// x-dorang-allow-lossy and retry deliberately.
	Construct string
	// Detail locates the offending block.
	Detail string
}

func (e *OpaqueError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "anthropic: " + e.Reason + " at " + e.Detail
}

// Status is 400: the request cannot be served as written, and no retry against
// a sibling deployment changes that.
func (e *OpaqueError) Status() int { return 400 }

// ToError renders it as a wire error.
func (e *OpaqueError) ToError() *Error {
	if e == nil {
		return nil
	}
	return NewError(400, TypeInvalidRequest,
		"cannot convert request: "+e.Reason+" at "+e.Detail).WithCode(e.Reason)
}

// ErrNotAResponse is a JSON object that parsed cleanly and is not a response of
// this family.
//
// It is a distinct condition from a decode failure, and the difference is the
// whole reason it exists. A vendor that answers HTTP 200 with
// `{"code":500,"msg":"404 NOT_FOUND","success":false}` — which is a real answer
// from a real coding-plan host, given to a request addressed at a route it does
// not serve — unmarshals into a zero-valued [Response] without error. Handed
// straight to [ResponseToCanonical] that becomes a successful assistant turn
// with an empty content array and a synthesized id, which no caller can tell
// from a real answer: retries never fire, the fallback chain never engages,
// health counts a success, and metering records zero tokens.
//
// The upstream that returns an honest 404 for the same misconfiguration is
// strictly easier to operate. This makes the dishonest one look like it.
var ErrNotAResponse = errorString("anthropic: the body is a JSON object but not a Messages response")

var (
	errNilRequest  = errorString("anthropic: nil request")
	errNilResponse = errorString("anthropic: nil response")
	// ErrMaxTokensRequired is returned when a crossing into this family has no
	// output ceiling to supply. Omitting the field is not an option — it is
	// required (DESIGN §10.7) — and picking a constant would silently cap the
	// caller's output, so the encoder demands one from the catalog.
	ErrMaxTokensRequired = &OpaqueError{
		Reason:    ReasonMaxTokensUnknown,
		Construct: "max_tokens",
		Detail:    "max_tokens: required by this family; supply EncodeOptions.DefaultMaxTokens from the model catalog",
	}
	errStreamClosed = errorString("anthropic: stream already closed")
)

// ---------------------------------------------------------------------------
// Warnings
// ---------------------------------------------------------------------------

// Warning is a structured compatibility warning. It is a struct rather than a
// format string because DESIGN §15.5 bars formatted string construction on the
// hot path.
type Warning struct {
	Code   string
	Detail string
}

// Warning codes.
const (
	WarnUnmappedStopReason = "unmapped_stop_reason"
	// WarnCollapsedStopReason fires when 6.4's collapse actually loses
	// information — a filtered turn reported as a normal end. The true value
	// goes out in NativeStopReasonHeader; this is the log half of the same fact.
	WarnCollapsedStopReason = "collapsed_stop_reason"
	// WarnSynthesizedStop fires when the backend never sent a terminal reason
	// and one was synthesized. It is upgraded to tool_use when a tool call was
	// seen, which is the mirror of COMPATIBILITY 4.4.
	WarnSynthesizedStop = "synthesized_stop_reason"
	// WarnInterleavedToolCalls fires when a backend goes back to a tool call
	// after another one has already started. This protocol has one open block at
	// a time, so parallel calls are assembled and emitted whole; only a call that
	// resumes after its block was closed cannot be expressed at all, and that is
	// what this reports.
	WarnInterleavedToolCalls = "interleaved_tool_calls"
	// WarnToolCallIndexReused fires when a second tool-call id appears under an
	// index a live call already holds — the shape a backend that omits index, or
	// always sends zero, produces for parallel calls.
	WarnToolCallIndexReused = "tool_call_index_reused"
	// WarnRepeatedToolName fires when name metadata arrives more than once for
	// one call.
	WarnRepeatedToolName = "repeated_tool_name"
	// WarnToolCallMissingID fires on a tool call the backend never named with an
	// id. dorang does not invent one.
	WarnToolCallMissingID = "tool_call_missing_id"
	// WarnMalformedToolArguments fires when a tool call's accumulated arguments
	// are not valid JSON at the end of the stream. dorang does not execute tools
	// and cannot repair the call, so the client is told in band rather than
	// handed a tool_use stop reason over a document that does not parse.
	WarnMalformedToolArguments = "malformed_tool_arguments"
	// WarnDataAfterFinish fires when a backend sends semantic content after it
	// already reported a terminal reason. The content is still delivered — the
	// held message_delta exists precisely so it can be — and the condition is
	// reported because it is out of contract.
	WarnDataAfterFinish = "data_after_stop_reason"
	// WarnToolInputConflict fires when content_block_start carried a complete
	// tool input AND input_json_delta frames followed. The two cannot both be the
	// document; the incremental form wins because it is the one this protocol
	// defines as cumulative.
	WarnToolInputConflict = "tool_input_conflict"
	// WarnDuplicateBlockStop fires on a second content_block_stop for a block
	// already stopped.
	WarnDuplicateBlockStop = "duplicate_content_block_stop"
	// WarnUnknownBlockStop fires on a content_block_stop for a block that never
	// started. It allocates no tool index: a stray frame must not invent a call.
	WarnUnknownBlockStop = "content_block_stop_without_start"
	WarnLateUsage        = "late_usage_after_stop"
	WarnNumericErrorCode = "numeric_error_code"
	// WarnDroppedMetadata fires when a metadata member could not be carried
	// through canonical.Request.Metadata, which is a map[string]string.
	WarnDroppedMetadata = "dropped_metadata_member"
	// WarnReasoningDisabled fires when a thinking budget could not be kept
	// strictly below the output ceiling (DESIGN §10.2).
	WarnReasoningDisabled = "reasoning_disabled_budget_too_small"
)

// WarnFunc receives a compatibility warning. It must not block and must not
// retain the argument. A nil WarnFunc is valid and means "discard".
type WarnFunc func(Warning)

func (w WarnFunc) warn(code, detail string) {
	if w != nil {
		w(Warning{Code: code, Detail: detail})
	}
}
