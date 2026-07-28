package batch

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors. The wiring layer maps them onto status codes: ErrNotFound to
// 404, ErrInvalidRequest to 400, ErrInvalidState to 409, ErrClosed to 503.
var (
	// ErrNotFound is returned for an unknown batch or file id.
	ErrNotFound = errors.New("batch: not found")
	// ErrInvalidRequest is returned for a malformed argument.
	ErrInvalidRequest = errors.New("batch: invalid request")
	// ErrInvalidState is returned when an operation does not apply to the
	// object's current state — cancelling a completed batch, or deleting the
	// input file of a batch that is still running.
	ErrInvalidState = errors.New("batch: invalid state")
	// ErrClosed is returned once the service has been closed.
	ErrClosed = errors.New("batch: service closed")
	// ErrExists is returned by a store when an id is already taken.
	ErrExists = errors.New("batch: already exists")
)

// Validation failure codes. They are stable strings, because a client branches
// on them (COMPATIBILITY §7.1: code is a string, never a number).
const (
	CodeEmptyFile        = "empty_file"
	CodeFileTooLarge     = "file_too_large"
	CodeTooManyRequests  = "too_many_requests_in_file"
	CodeLineTooLarge     = "line_too_large"
	CodeInvalidJSON      = "invalid_json_line"
	CodeMissingCustomID  = "missing_custom_id"
	CodeCustomIDTooLong  = "custom_id_too_long"
	CodeDuplicateID      = "duplicate_custom_id"
	CodeInvalidMethod    = "invalid_method"
	CodeInvalidURL       = "invalid_url"
	CodeEndpointMismatch = "endpoint_mismatch"
	CodeMissingBody      = "missing_body"
	CodeInvalidBody      = "invalid_body"
	CodeMissingModel     = "missing_model"
	CodeModelNotFound    = "model_not_found"
	CodeReadFailed       = "read_failed"
)

// ValidationError is one problem with one line of an input file.
//
// Line is 1-based and counts physical lines of the file, including the blank
// ones that are skipped, so that the number a caller is given is the number
// their editor shows. A failure that is not line-scoped — the file is empty, or
// too large — carries Line 0.
type ValidationError struct {
	Line    int
	Code    string
	Param   string
	Message string
}

// Error names the offending line, because "invalid method" against a 50 000-line
// file is not an actionable message.
func (e *ValidationError) Error() string {
	if e.Line > 0 {
		return "line " + strconv.Itoa(e.Line) + ": " + e.Message
	}
	return e.Message
}

// BatchError renders the failure as the wire object. The message keeps its line
// prefix in addition to the line field: clients display the message and ignore
// the rest.
func (e *ValidationError) BatchError() BatchError {
	return BatchError{Code: e.Code, Message: e.Error(), Param: e.Param, Line: e.Line}
}

// ValidationErrors is every problem found, in line order, up to
// Config.MaxValidationErrors. Truncated reports that scanning stopped early.
type ValidationErrors struct {
	Errs      []*ValidationError
	Truncated bool
}

// Len reports how many failures were recorded.
func (v *ValidationErrors) Len() int { return len(v.Errs) }

// Error joins the failures, each naming its line.
func (v *ValidationErrors) Error() string {
	if len(v.Errs) == 0 {
		return "batch: input file is invalid"
	}
	parts := make([]string, 0, len(v.Errs)+1)
	for _, e := range v.Errs {
		parts = append(parts, e.Error())
	}
	if v.Truncated {
		parts = append(parts, "(scanning stopped here; there may be more)")
	}
	return "batch: invalid input file: " + strings.Join(parts, "; ")
}

// Is makes errors.Is(err, ErrInvalidRequest) true for a validation failure, so
// the wiring layer needs one rule rather than a type switch.
func (v *ValidationErrors) Is(target error) bool { return target == ErrInvalidRequest }

// List renders the failures as wire objects for a batch's errors field.
func (v *ValidationErrors) List() []BatchError {
	out := make([]BatchError, 0, len(v.Errs))
	for _, e := range v.Errs {
		out = append(out, e.BatchError())
	}
	return out
}

// add records a failure and reports whether scanning should continue. Reaching
// the cap sets Truncated, which says scanning stopped rather than that the file
// is now known to be clean — the two are not the same and a caller fixing the
// file needs to know which it was told.
func (v *ValidationErrors) add(max int, e *ValidationError) bool {
	v.Errs = append(v.Errs, e)
	if max > 0 && len(v.Errs) >= max {
		v.Truncated = true
		return false
	}
	return true
}

// terminal is the interface an executor error implements to say it must not be
// retried. internal/router's *Error already has this method, so a routing
// failure that the router itself judged terminal is not retried here either.
type terminal interface {
	Terminal() bool
}

// IsTerminal reports whether an executor error forbids a retry. Errors that say
// nothing are retried: a transport failure with no answer is the retryable case
// by default, and a context that ended is handled separately.
func IsTerminal(err error) bool {
	if err == nil {
		return false
	}
	var t terminal
	if errors.As(err, &t) {
		return t.Terminal()
	}
	return false
}

// formatWindow renders a completion window the way OpenAI spells it ("24h").
func formatWindow(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	if d%time.Minute == 0 {
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.String()
}

// parseWindow accepts a completion window as written on the wire. OpenAI accepts
// only "24h"; dorang accepts any Go duration in the permitted range, because an
// operator running their own backends has no reason to be held to a vendor's
// scheduling window. An empty string selects the default.
func parseWindow(s string, def, min, max time.Duration) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%w: completion_window %q is not a duration", ErrInvalidRequest, s)
	}
	if d < min || d > max {
		return 0, fmt.Errorf("%w: completion_window %s is outside [%s, %s]",
			ErrInvalidRequest, formatWindow(d), formatWindow(min), formatWindow(max))
	}
	return d, nil
}
