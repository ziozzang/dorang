package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRequestBody bounds an administrative request body. Administrative payloads
// are small; a megabyte is two orders of magnitude of headroom and still small
// enough that a malformed or hostile caller cannot make the process pay for it.
const maxRequestBody = 1 << 20

// ---------------------------------------------------------------------------
// Money
// ---------------------------------------------------------------------------

// Money is an exact monetary amount in nano-units of the deployment currency
// (1e-9), which is the unit the store and the pricing engine both use.
//
// It renders as a JSON *number* so that a script reading `spend` gets a number,
// as it does from the incumbent — but the digits are produced by integer
// formatting, never by float64. §8.3 is explicit that prices never pass through
// binary floating point, and a renderer that divides by 1e9 to print would
// reintroduce exactly the error the rest of the pipeline went to trouble to
// avoid.
type Money int64

// MarshalJSON renders the amount as an exact decimal.
func (m Money) MarshalJSON() ([]byte, error) {
	return []byte(formatNano(int64(m))), nil
}

// UnmarshalJSON accepts a JSON number or a decimal string, exactly. Exponent
// notation and more than nano precision are refused rather than rounded: a
// budget the caller wrote and the gateway silently altered is worse than a
// rejected request.
func (m *Money) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*m = 0
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
	}
	v, err := parseNano(s)
	if err != nil {
		return err
	}
	*m = Money(v)
	return nil
}

// String renders the amount for a template.
func (m Money) String() string { return formatNano(int64(m)) }

// formatNano renders nano-units as an exact decimal, trimming trailing zeros.
// It allocates and is not for a hot path.
func formatNano(v int64) string {
	neg := v < 0
	// Negation of math.MinInt64 overflows; the store range-checks amounts to
	// 1e18, so this cannot arrive from a stored value, and clamping the one
	// unrepresentable input beats a wrong sign.
	if v == -9223372036854775808 {
		return "-9223372036.854775808"
	}
	if neg {
		v = -v
	}
	whole := v / 1_000_000_000
	frac := v % 1_000_000_000
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(strconv.FormatInt(whole, 10))
	if frac != 0 {
		digits := strconv.FormatInt(frac, 10)
		digits = strings.Repeat("0", 9-len(digits)) + digits
		digits = strings.TrimRight(digits, "0")
		b.WriteByte('.')
		b.WriteString(digits)
	}
	return b.String()
}

var errBadAmount = errors.New("amount must be an exact decimal with at most nine fractional digits")

// parseNano converts an exact decimal to nano-units without touching float64.
func parseNano(s string) (int64, error) {
	if s == "" {
		return 0, errBadAmount
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return 0, errBadAmount
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if intPart == "" && !hasDot {
		return 0, errBadAmount
	}
	if hasDot && intPart == "" && fracPart == "" {
		return 0, errBadAmount
	}
	if !allDigits(intPart) || !allDigits(fracPart) {
		return 0, errBadAmount
	}
	if len(fracPart) > 9 {
		return 0, errBadAmount
	}
	var whole int64
	if intPart != "" {
		v, err := strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return 0, errBadAmount
		}
		whole = v
	}
	// 1e18 is the store's own ceiling for a single amount; staying an order of
	// magnitude below the int64 nano ceiling leaves headroom for aggregation.
	if whole > 1_000_000_000 {
		return 0, errBadAmount
	}
	frac := int64(0)
	if fracPart != "" {
		padded := fracPart + strings.Repeat("0", 9-len(fracPart))
		v, err := strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, errBadAmount
		}
		frac = v
	}
	out := whole*1_000_000_000 + frac
	if neg {
		out = -out
	}
	return out, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// moneyPtr renders a nullable nano amount.
func moneyPtr(v *int64) *Money {
	if v == nil {
		return nil
	}
	m := Money(*v)
	return &m
}

// nanoPtr converts a decoded amount back to the store's representation.
func nanoPtr(m *Money) *int64 {
	if m == nil {
		return nil
	}
	v := int64(*m)
	return &v
}

// ---------------------------------------------------------------------------
// Time
// ---------------------------------------------------------------------------

// Stamp renders an instant as RFC 3339 in UTC, and a zero instant as null.
// "never" and "the epoch" are different answers and the wire must not claim the
// latter for the former.
type Stamp time.Time

// MarshalJSON implements json.Marshaler.
func (s Stamp) MarshalJSON() ([]byte, error) {
	t := time.Time(s)
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format(time.RFC3339Nano))
}

// UnmarshalJSON accepts RFC 3339, a bare date, or null.
func (s *Stamp) UnmarshalJSON(b []byte) error {
	if strings.TrimSpace(string(b)) == "null" {
		*s = Stamp(time.Time{})
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	if strings.TrimSpace(str) == "" {
		*s = Stamp(time.Time{})
		return nil
	}
	t, err := parseInstant(str)
	if err != nil {
		return err
	}
	*s = Stamp(t)
	return nil
}

// Time returns the wrapped instant.
func (s Stamp) Time() time.Time { return time.Time(s) }

var instantLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseInstant accepts the layouts an operator or a script actually sends. A
// bare date is midnight UTC, which is what "start_date=2026-07-01" means to
// everyone who types it.
func parseInstant(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range instantLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	// A unix seconds value, which some scripts pass.
	if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
		return time.Unix(v, 0).UTC(), nil
	}
	return time.Time{}, errors.New("not a recognized timestamp")
}

// ---------------------------------------------------------------------------
// Request and response
// ---------------------------------------------------------------------------

// decodeBody reads a JSON request body into v.
//
// Unknown fields are accepted rather than rejected. The whole point of a
// shape-compatible surface is that a script written against another gateway
// keeps working, and such a script sends fields dorang has no column for;
// refusing them would defeat §2.3 for no safety gain, since an unknown field is
// simply not read. Output, by contrast, is strict: the response shape is a
// contract.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	if r.Body == nil {
		return badRequest("a JSON request body is required")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return badRequest("a JSON request body is required")
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return badRequest("request body exceeds %d bytes", maxRequestBody)
		}
		return badRequest("malformed JSON request body: %s", err)
	}
	// A second value in the stream means the caller sent two documents and is
	// about to be surprised by which one took effect.
	if dec.More() {
		return badRequest("request body must contain exactly one JSON document")
	}
	return nil
}

// writeJSON renders a successful response.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		writeFault(w, r, newFault(http.StatusInternalServerError, CodeInternal, typeAPI,
			"response could not be encoded"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r != nil && r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
}

// ---------------------------------------------------------------------------
// Query parameters
// ---------------------------------------------------------------------------

// queryString returns the first non-empty value among names.
func queryString(r *http.Request, names ...string) string {
	q := r.URL.Query()
	for _, n := range names {
		if v := strings.TrimSpace(q.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

// queryInt reads a bounded integer, returning def when absent.
func queryInt(r *http.Request, name string, def int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, badRequest("%s must be an integer", name).withParam(name)
	}
	return v, nil
}

// queryBool reads an optional boolean.
func queryBool(r *http.Request, name string) (*bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, badRequest("%s must be true or false", name).withParam(name)
	}
	return &v, nil
}

// queryList reads a repeated or comma-separated parameter.
func queryList(r *http.Request, name string) []string {
	var out []string
	for _, raw := range r.URL.Query()[name] {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func (f *fault) withParam(p string) *fault { f.Param = p; return f }
