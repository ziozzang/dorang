package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors a dependency may return. They are matched with errors.Is and
// mapped to a status and a code exactly once, in [faultFor], so that every
// endpoint refuses the same way for the same reason.
var (
	// ErrNotFound means a lookup by primary key found nothing.
	ErrNotFound = errors.New("admin: not found")

	// ErrConflict means the object already exists, or the write lost a race.
	ErrConflict = errors.New("admin: object already exists")

	// ErrUnboundedRange means a ledger query arrived without a bounded,
	// ordered time range. DESIGN §9.3: unbounded search is refused, not
	// answered slowly and not silently capped.
	ErrUnboundedRange = errors.New("admin: ledger query requires a bounded time range")

	// ErrRangeTooWide means the range exceeded the configured maximum. It is a
	// distinct error from ErrUnboundedRange because the caller's fix is
	// different: narrow the window, rather than supply one.
	ErrRangeTooWide = errors.New("admin: ledger time range exceeds the configured maximum")

	// ErrUnauthenticated means no usable credential was presented.
	ErrUnauthenticated = errors.New("admin: no administrative credential")

	// ErrForbidden means a valid credential without an administrative role.
	ErrForbidden = errors.New("admin: credential is not administrative")

	// ErrUnsupported means the request is well-formed but asks for something
	// this build cannot do — an absent optional dependency, or a shape the
	// schema cannot represent. It becomes a 501 with a code, never a 404.
	ErrUnsupported = errors.New("admin: not implemented")
)

// Machine-readable error codes. Scripts switch on these, so they are part of
// the contract and change only with a deliberate break.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeUnboundedRange   = "unbounded_range"
	CodeRangeTooWide     = "range_too_wide"
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodeNotFound         = "not_found"
	CodeConflict         = "conflict"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeNotImplemented   = "not_implemented"
	CodeDependencyOff    = "dependency_not_configured"
	CodeInternal         = "internal_error"
	// CodeAuditWriteFailed reports a mutation that was applied but could not be
	// audited. It is its own code because the operator's follow-up is different
	// from any other 500: the change happened and the trail is incomplete.
	CodeAuditWriteFailed = "audit_write_failed"
)

// Error envelope types. The vocabulary matches the one dorang's inference
// surface uses (COMPATIBILITY §7.1) so a client library that already classifies
// gateway errors classifies these too.
const (
	typeInvalidRequest = "invalid_request_error"
	typeAuthentication = "authentication_error"
	typePermission     = "permission_error"
	typeNotFound       = "not_found_error"
	typeAPI            = "api_error"
	typeNotImplemented = "not_implemented_error"
)

// fault is one refusal on its way to the client.
//
// The envelope is {"error":{"message","type","param","code"}} with **code a
// string, not a number** — a client that does err.code.startswith(...) or
// switches on a string raises inside its own SDK when handed an integer, before
// the message is ever surfaced.
type fault struct {
	Status  int
	Code    string
	Type    string
	Message string
	Param   string
	// Detail carries machine-readable specifics for refusals where the caller
	// needs more than prose: which dependency is missing, what the maximum
	// range is. It is omitted when empty.
	Detail map[string]any
}

func (f *fault) Error() string { return f.Message }

// newFault builds a refusal.
func newFault(status int, code, typ, format string, a ...any) *fault {
	return &fault{Status: status, Code: code, Type: typ, Message: fmt.Sprintf(format, a...)}
}

func badRequest(format string, a ...any) *fault {
	return newFault(http.StatusBadRequest, CodeInvalidRequest, typeInvalidRequest, format, a...)
}

func notFound(kind, id string) *fault {
	return newFault(http.StatusNotFound, CodeNotFound, typeNotFound, "no %s with id %q", kind, id)
}

// unimplemented is the answer for a route dorang has not filled in. §0.2 is
// explicit that this is a 501 with a reason and never a silent 404, because a
// caller must be able to tell "dorang does not implement this yet" from "you
// typed the URL wrong", and only the code tells them.
func unimplemented(code, format string, a ...any) *fault {
	return newFault(http.StatusNotImplemented, code, typeNotImplemented, format, a...)
}

// dependencyOff is the answer when an optional dependency is not wired into
// this process. It is a 501 rather than a 500 for the same reason: the surface
// exists, this build cannot serve it, and the code says which piece is absent.
func dependencyOff(dep, what string) *fault {
	f := unimplemented(CodeDependencyOff,
		"%s is not configured in this process, so %s cannot be served", dep, what)
	f.Detail = map[string]any{"dependency": dep}
	return f
}

// faultFor maps any error to the refusal that leaves dorang.
//
// An unrecognized error becomes a 500 with a fixed message. The original is not
// echoed: an error string from a database driver can carry a DSN, and a
// component of that DSN can carry a password.
func faultFor(err error) *fault {
	var f *fault
	if errors.As(err, &f) {
		return f
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return newFault(http.StatusNotFound, CodeNotFound, typeNotFound, "not found")
	case errors.Is(err, ErrConflict):
		return newFault(http.StatusConflict, CodeConflict, typeInvalidRequest, "already exists")
	case errors.Is(err, ErrUnboundedRange):
		return rangeFault(CodeUnboundedRange,
			"a ledger query requires a bounded time range: supply start_date and end_date")
	case errors.Is(err, ErrRangeTooWide):
		return rangeFault(CodeRangeTooWide,
			"the requested time range is wider than this deployment allows; narrow it rather than expecting a partial answer")
	case errors.Is(err, ErrUnauthenticated):
		return newFault(http.StatusUnauthorized, CodeUnauthorized, typeAuthentication,
			"an administrative credential is required")
	case errors.Is(err, ErrForbidden):
		return newFault(http.StatusForbidden, CodeForbidden, typePermission,
			"this credential is not administrative")
	case errors.Is(err, ErrUnsupported):
		return unimplemented(CodeNotImplemented, "not implemented")
	}
	return newFault(http.StatusInternalServerError, CodeInternal, typeAPI, "internal error")
}

// rangeFault builds the two range refusals, which share a param.
func rangeFault(code, msg string) *fault {
	f := newFault(http.StatusBadRequest, code, typeInvalidRequest, "%s", msg)
	f.Param = "start_date"
	return f
}

// errorBody is the wire shape of a refusal.
type errorBody struct {
	Error errorEnvelope `json:"error"`
}

type errorEnvelope struct {
	Message string         `json:"message"`
	Type    string         `json:"type"`
	Param   *string        `json:"param"`
	Code    string         `json:"code"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// writeFault renders a refusal. It never writes a body for a HEAD request and
// never claims a status it has not set.
func writeFault(w http.ResponseWriter, r *http.Request, f *fault) {
	var param *string
	if f.Param != "" {
		p := f.Param
		param = &p
	}
	body := errorBody{Error: errorEnvelope{
		Message: f.Message,
		Type:    f.Type,
		Param:   param,
		Code:    f.Code,
		Detail:  f.Detail,
	}}
	buf, err := json.Marshal(body)
	if err != nil {
		// Unreachable for this shape; a hand-written fallback beats a panic.
		buf = []byte(`{"error":{"message":"internal error","type":"api_error","param":null,"code":"internal_error"}}`)
		f.Status = http.StatusInternalServerError
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Dorang-Error-Code", f.Code)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(f.Status)
	if r != nil && r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
}
