package openai

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Error is the inner object of the OpenAI error envelope.
//
// COMPATIBILITY 7.1: the envelope is
// {"error":{"message":str,"type":str,"param":str|null,"code":"<string>"}} and
// **code is a string, not a number**. Several backends send a number there;
// clients written against OpenAI do `err.code.startswith(...)` or switch on a
// string and break on an int. Every code that leaves dorang is a string.
//
// Param and Code have no omitempty: the envelope's shape is fixed at four keys.
// Param is a pointer so that "no offending parameter" renders as null, which is
// what the contract specifies and what clients test for.
type Error struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`

	// StatusCode is dorang's HTTP status. It is not on the wire.
	StatusCode int `json:"-"`
	// CodeWasNumeric records that the backend sent a numeric code that dorang
	// stringified. Not on the wire; it exists so the decode path can warn once
	// rather than silently normalizing a protocol violation.
	CodeWasNumeric bool `json:"-"`
}

// Envelope is the outer error object.
type Envelope struct {
	Error Error `json:"error"`
}

// Error type strings.
const (
	TypeInvalidRequest    = "invalid_request_error"
	TypeAuthentication    = "authentication_error"
	TypePermission        = "permission_error"
	TypeNotFound          = "not_found_error"
	TypeRateLimit         = "rate_limit_error"
	TypeAPIError          = "api_error"
	TypeOverloaded        = "overloaded_error"
	TypeTimeout           = "timeout_error"
	TypeInvalidResponse   = "invalid_response_error"
	TypeServiceUnavailabe = "service_unavailable_error"
)

// NewError builds an error whose code defaults to the stringified status.
//
// The default exists because the contract fixes code as a string but does not
// say what it holds when the backend supplied nothing, and an empty string is
// less useful to a client than the status it already has in hand. The reference
// proxy puts the status there, so dorang does too.
func NewError(status int, typ, message string) *Error {
	if typ == "" {
		typ = TypeForStatus(status)
	}
	return &Error{
		Message:    message,
		Type:       typ,
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

// UnmarshalJSON accepts a numeric code and stringifies it.
//
// This is the defensive half of 7.1. dorang cannot stop a backend sending
// {"code": 429}; it can stop that reaching a client.
func (e *Error) UnmarshalJSON(b []byte) error {
	var raw struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Param   *string         `json:"param"`
		Code    json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	e.Message = raw.Message
	e.Type = raw.Type
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
	case c[0] == '-' || (c[0] >= '0' && c[0] <= '9'):
		e.Code = string(c)
		e.CodeWasNumeric = true
	default:
		// An object or an array. Carry the raw text rather than losing it.
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
	return Marshal(Envelope{Error: *e})
}

// DecodeError parses an error envelope, tolerating a bare inner object.
func DecodeError(b []byte, warn WarnFunc) (*Error, error) {
	b = trimSpace(b)
	if len(b) == 0 {
		return nil, errors.New("openai: empty error body")
	}
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	e := env.Error
	if e.Message == "" && e.Type == "" && e.Code == "" {
		// Not enveloped; try the bare form.
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
