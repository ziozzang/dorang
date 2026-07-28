package probe

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Providers spell the same three things — an instant, a proportion, an amount —
// in every shape JSON allows. These decode them, and every one of them answers
// "unknown" rather than a plausible number when the value is ambiguous. That
// direction is the whole of rule 5: an over-optimistic quota reading routes
// traffic into an exhausted account, and a reset instant guessed to the wrong
// zone spikes §7.5a(c)'s urgency at a moment unrelated to the real reset.

// Timestamps outside this range are refused. They are not resets; they are a
// unit confusion or a sentinel, and a reset in the year 5138 makes a window
// look infinitely long, which drives urgency to zero forever.
var (
	minPlausible = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	maxPlausible = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// unixMilliThreshold separates unix seconds from unix milliseconds. It is
// unambiguous by three orders of magnitude in each direction: 1e11 seconds is
// the year 5138 and 1e11 milliseconds is March 1973, so every instant this
// package will ever see falls clearly on one side.
const unixMilliThreshold = 1e11

// resetTime is a provider's reset instant, which the same provider may report
// as unix milliseconds on one field and as an ISO-8601 string on another — z.ai
// does exactly that. The zero value means unknown, which §7.5a(c) scores as no
// urgency rather than as imminent.
type resetTime struct {
	Time time.Time
}

// UnmarshalJSON accepts a number (unix seconds or milliseconds), a string
// holding either a number or an RFC 3339 instant, and null. Anything else
// leaves the zero value: a malformed reset must not fail the whole read,
// because the percentage beside it is still true.
func (r *resetTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return nil
		}
		r.Time = parseResetString(str)
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	r.Time = fromUnixNumber(f)
	return nil
}

// parseResetString reads a string reset instant.
//
// Only zone-bearing spellings are accepted. A zone-less "2026-04-11T07:00:00"
// is refused rather than assumed to be UTC: several of these providers publish
// from UTC+8, and being eight hours wrong about a five-hour window inverts the
// signal it feeds.
func parseResetString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return fromUnixNumber(f)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, time.RFC1123, time.RFC1123Z} {
		if t, err := time.Parse(layout, s); err == nil {
			return plausible(t.UTC())
		}
	}
	return time.Time{}
}

// fromUnixNumber reads a numeric instant, choosing seconds or milliseconds by
// magnitude.
func fromUnixNumber(f float64) time.Time {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return time.Time{}
	}
	if f >= unixMilliThreshold {
		return plausible(time.UnixMilli(int64(f)).UTC())
	}
	return plausible(time.Unix(int64(f), 0).UTC())
}

// plausible refuses an instant that cannot be a reset.
func plausible(t time.Time) time.Time {
	if t.Before(minPlausible) || t.After(maxPlausible) {
		return time.Time{}
	}
	return t
}

// usedPercent normalizes a provider's consumed proportion into [0,100].
//
// It never rescales. A provider that reports 0–1 and a provider that reports
// 0–100 are indistinguishable from a single reading of 0.5 — it is either half
// a percent or half the allowance — and guessing costs an order of magnitude in
// the direction that keeps routing into an exhausted account. Each source
// states the scale it is documented to use; a value outside that scale is
// unknown.
func usedPercent(v float64) (float64, bool) {
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		return 0, false
	case v < 0:
		// Not zero usage: a negative proportion means the field was not what
		// this decoder thinks it is.
		return 0, false
	case v > 100:
		// Over-consumption is real — a provider can let a request finish past
		// the ceiling. Clamping to full is the pessimistic direction, which is
		// the safe one for a figure that gates traffic.
		return 100, true
	}
	return v, true
}

// nanoScale is the fixed-point scale for money, matching quota's nano-USD.
const nanoScale = 1_000_000_000

// errNotAnAmount reports a balance field that is not a decimal amount.
var errNotAnAmount = errors.New("not a decimal amount")

// parseNano reads a decimal amount into nano-units exactly.
//
// Providers publish balances as strings ("110.00") precisely so that they are
// not rounded, so this parses the digits rather than going through a float and
// inheriting its error.
func parseNano(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errNotAnAmount
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, errNotAnAmount
	}
	// Pad or truncate the fraction to nine digits. Truncating is deliberate:
	// a balance is not made larger by rounding it up.
	if len(frac) > 9 {
		frac = frac[:9]
	}
	var f int64
	if frac != "" {
		if _, err := strconv.ParseUint(frac, 10, 64); err != nil {
			return 0, errNotAnAmount
		}
		v, err := strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
		if err != nil {
			return 0, errNotAnAmount
		}
		f = v
	}
	if w > (math.MaxInt64-f)/nanoScale {
		return 0, errNotAnAmount
	}
	n := w*nanoScale + f
	if neg {
		n = -n
	}
	return n, nil
}

// number is a JSON value that may arrive as a number or as a stringified one.
type number struct {
	Value float64
	Set   bool
}

// UnmarshalJSON accepts a number, a numeric string, and null.
func (n *number) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return nil
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
		if err != nil {
			return nil
		}
		n.Value, n.Set = f, true
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	n.Value, n.Set = f, true
	return nil
}

// Int returns the value as an integer when it is one.
func (n number) Int() (int64, bool) {
	if !n.Set || math.IsNaN(n.Value) || math.IsInf(n.Value, 0) {
		return 0, false
	}
	if n.Value != math.Trunc(n.Value) {
		return 0, false
	}
	if n.Value > math.MaxInt64 || n.Value < math.MinInt64 {
		return 0, false
	}
	return int64(n.Value), true
}
