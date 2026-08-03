package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that reads the forms the design writes:
// "180s", "500ms", "1h". A bare number is read as seconds, which is what
// foreign proxy configurations use for timeouts.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// IsZero reports whether the duration is unset.
func (d Duration) IsZero() bool { return d == 0 }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: duration must be a scalar such as \"180s\"", n.Line)
	}
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// ParseDuration parses a duration with time.ParseDuration semantics. A bare
// number is read as a whole number of seconds. Negative values are rejected.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "~" || s == "null" {
		return 0, nil
	}
	if v, err := time.ParseDuration(s); err == nil {
		if v < 0 {
			return 0, fmt.Errorf("duration %q is negative", s)
		}
		return v, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		if f < 0 {
			return 0, fmt.Errorf("duration %q is negative", s)
		}
		secs := f * float64(time.Second)
		if secs > float64(math.MaxInt64) {
			return 0, fmt.Errorf("duration %q overflows", s)
		}
		return time.Duration(secs), nil
	}
	return 0, fmt.Errorf("invalid duration %q: want a value such as \"250ms\", \"30s\" or \"1h\"", s)
}

// NoDeadline is the [Deadline] spelling for "do not bound this at all".
const NoDeadline = "none"

// Deadline is a connection deadline: a duration, or `none` for no bound.
//
// It is not a [Duration] because absence and "off" are DIFFERENT answers here
// and Duration has one spelling for both. An absent key — and a literal `0` —
// means "take the documented default"; `none` means "do not bound this", which
// is a real answer for a deployment behind a proxy that already enforces the
// same deadline, and the only way back to the unbounded behaviour these
// deadlines replaced. A setting whose "off" value collides with its "unset"
// value is how a disabled feature turns back on at the next refactor, which is
// the reason [CacheTTL] exists one type above and the reason this one does.
//
// The Go fields it feeds spell "no bound" as a NEGATIVE duration
// (server.Options.ReadTimeout and its two siblings), so `none` renders as -1 and
// a literal negative duration is accepted as a synonym for it. One rule —
// negative is no bound — and the readable spelling is the one the documentation
// uses.
type Deadline time.Duration

// Duration returns the value in the form the server's Options field takes:
// positive is a bound, zero means "use the default", negative means none.
func (d Deadline) Duration() time.Duration { return time.Duration(d) }

// IsZero reports whether the value was left unset, which means "take the
// default". It is FALSE for `none`, which is a value and not an absence.
func (d Deadline) IsZero() bool { return d == 0 }

// Unbounded reports the explicit "no bound".
func (d Deadline) Unbounded() bool { return d < 0 }

func (d Deadline) String() string {
	switch {
	case d < 0:
		return NoDeadline
	case d == 0:
		return ""
	}
	return time.Duration(d).String()
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Deadline) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a deadline must be a scalar such as %q or %q",
			n.Line, "30s", NoDeadline)
	}
	s := strings.TrimSpace(n.Value)
	switch s {
	case "", "~", "null":
		*d = 0
		return nil
	case NoDeadline, "off", "unbounded":
		*d = -1
		return nil
	}
	// A literal negative duration is the same answer as `none`, and it is
	// normalized to one value: -1s and -5s cannot mean two different amounts of
	// "not bounded", so they must not be storable as two different numbers.
	if strings.HasPrefix(s, "-") {
		if _, err := ParseDuration(strings.TrimPrefix(s, "-")); err == nil {
			*d = -1
			return nil
		}
	}
	v, err := ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w (or %q to remove the bound entirely, for a "+
			"deployment whose proxy already enforces one)", n.Line, err, NoDeadline)
	}
	*d = Deadline(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Deadline) MarshalYAML() (any, error) {
	if d == 0 {
		return nil, nil
	}
	return d.String(), nil
}

// UntilEvicted is the CacheTTL spelling for "this entry does not expire on a
// clock; it lives until the table's byte budget evicts it".
const UntilEvicted = "until_evicted"

// CacheTTL is how long a cache-affinity entry stays believable. It reads either
// a duration ("5m", "1h") or the word "until_evicted".
//
// It is not a Duration because the honest answer for a self-hosted engine is not
// a duration at all. vLLM and SGLang hold prefix blocks until LRU eviction under
// memory pressure; there is no clock involved, and any number written there is a
// guess that throws away hits it still had. The hosted services do run a clock —
// five minutes, ten minutes, an hour depending on vendor and tier — and those
// need a real duration. One type carries both facts so a configuration can state
// which kind of cache it is talking to.
type CacheTTL struct {
	d       time.Duration
	forever bool
	set     bool
}

// TTL builds a CacheTTL from a duration.
func TTL(d time.Duration) CacheTTL { return CacheTTL{d: d, set: true} }

// Forever builds the "until_evicted" CacheTTL.
func ForeverTTL() CacheTTL { return CacheTTL{forever: true, set: true} }

// IsZero reports whether the value was left unset, which means "inherit".
func (c CacheTTL) IsZero() bool { return !c.set }

// IsForever reports whether entries never expire on a clock.
func (c CacheTTL) IsForever() bool { return c.forever }

// Duration returns the clock lifetime, or zero when the value is unset or
// "until_evicted". Callers that must tell those apart read [CacheTTL.IsForever].
func (c CacheTTL) Duration() time.Duration {
	if c.forever {
		return 0
	}
	return c.d
}

// TableTTL renders the value the way internal/prefix reads it: a positive
// duration expires, and a NEGATIVE one means "never expire on a clock". Zero is
// not usable for that, because internal/prefix already spends zero on "use the
// default" — and a setting whose "off" value collides with its "unset" value is
// how a disabled feature turns back on at the next refactor.
func (c CacheTTL) TableTTL() time.Duration {
	if c.forever {
		return -1
	}
	return c.d
}

// Or returns c when it is set, and fallback otherwise. This is the whole
// inheritance rule: deployment, then provider, then the global default.
func (c CacheTTL) Or(fallback CacheTTL) CacheTTL {
	if c.set {
		return c
	}
	return fallback
}

func (c CacheTTL) String() string {
	switch {
	case !c.set:
		return ""
	case c.forever:
		return UntilEvicted
	}
	return c.d.String()
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (c *CacheTTL) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a cache lifetime must be a scalar such as \"5m\" or %q",
			n.Line, UntilEvicted)
	}
	s := strings.TrimSpace(n.Value)
	switch s {
	case "", "~", "null":
		*c = CacheTTL{}
		return nil
	case UntilEvicted:
		*c = ForeverTTL()
		return nil
	}
	v, err := ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w (or %q for a cache with no clock, such as vLLM or SGLang)",
			n.Line, err, UntilEvicted)
	}
	*c = TTL(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (c CacheTTL) MarshalYAML() (any, error) {
	if !c.set {
		return nil, nil
	}
	return c.String(), nil
}

// byteUnits maps a lower-cased unit suffix to its multiplier. Binary units are
// powers of 1024; decimal units are powers of 1000. Both spellings appear in
// the design (64MiB, 8GiB) and in configurations written by hand.
var byteUnits = []struct {
	suffix string
	mult   int64
}{
	{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40}, {"pib", 1 << 50},
	{"kb", 1e3}, {"mb", 1e6}, {"gb", 1e9}, {"tb", 1e12}, {"pb", 1e15},
	{"b", 1},
}

// ByteSize is a size in bytes written as "4096", "64MiB", or "8GB".
type ByteSize int64

// Bytes returns the size in bytes.
func (b ByteSize) Bytes() int64 { return int64(b) }

// IsZero reports whether the size is unset.
func (b ByteSize) IsZero() bool { return b == 0 }

func (b ByteSize) String() string {
	n := int64(b)
	if n == 0 {
		return "0"
	}
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"PiB", 1 << 50}, {"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n%u.mult == 0 {
			return strconv.FormatInt(n/u.mult, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: size must be a scalar such as \"64MiB\"", n.Line)
	}
	v, err := ParseByteSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = ByteSize(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (b ByteSize) MarshalYAML() (any, error) { return b.String(), nil }

// ParseByteSize parses a byte size. Recognized units are B, KB, MB, GB, TB, PB
// (powers of 1000) and KiB, MiB, GiB, TiB, PiB (powers of 1024), case
// insensitively, with optional space before the unit. A bare number is bytes.
func ParseByteSize(s string) (int64, error) {
	orig := s
	s = strings.TrimSpace(s)
	if s == "" || s == "~" || s == "null" {
		return 0, nil
	}
	lower := strings.ToLower(s)
	mult := int64(1)
	for _, u := range byteUnits {
		if strings.HasSuffix(lower, u.suffix) {
			mult = u.mult
			lower = strings.TrimSpace(lower[:len(lower)-len(u.suffix)])
			break
		}
	}
	if lower == "" {
		return 0, fmt.Errorf("invalid size %q: no number before the unit", orig)
	}
	if strings.ContainsAny(lower, "_ ") {
		return 0, fmt.Errorf("invalid size %q", orig)
	}
	if i := strings.IndexFunc(lower, func(r rune) bool {
		return !(r >= '0' && r <= '9') && r != '.' && r != '+'
	}); i >= 0 {
		return 0, fmt.Errorf("invalid size %q: want a number with an optional unit (B, KB, MB, GB, KiB, MiB, GiB)", orig)
	}
	f, err := strconv.ParseFloat(lower, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: want a number with an optional unit (B, KB, MB, GB, KiB, MiB, GiB)", orig)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, fmt.Errorf("invalid size %q", orig)
	}
	total := f * float64(mult)
	if total > float64(math.MaxInt64) {
		return 0, fmt.Errorf("size %q overflows", orig)
	}
	return int64(total), nil
}

// Decimal is an exact decimal literal held as text. Design §8.3 requires prices
// to parse as exact decimals and never through binary floating point, so the
// literal from the file is preserved verbatim and only checked for syntax here.
type Decimal string

func (d Decimal) String() string { return string(d) }

// IsZero reports whether the decimal is unset.
func (d Decimal) IsZero() bool { return d == "" }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Decimal) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: decimal must be a scalar", n.Line)
	}
	s := strings.TrimSpace(n.Value)
	if s == "" || s == "~" || s == "null" {
		*d = ""
		return nil
	}
	if err := checkDecimal(s); err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Decimal(s)
	return nil
}

// maxDecimalScale is the largest number of fractional digits a decimal may
// carry. It is internal/pricing's maxPriceScale, restated here because this
// package is the one that has to REFUSE what that one cannot parse.
const maxDecimalScale = 12

// maxDecimalUnits is the largest significand internal/pricing can hold: its
// decimal keeps the digits in a uint64.
const maxDecimalUnits = "18446744073709551615"

// checkDecimal verifies decimal syntax without converting through a float.
//
// It accepts exactly what internal/pricing's parseDecimal accepts, and that
// agreement is the whole point of it. A validator more permissive than the
// consumer is worse than no validator: `dorangctl config lint` answered `ok` on
// a rate card written in exponent notation — `1.25e-7` — and the gateway then
// refused to assemble the catalog with "exponent notation is not accepted",
// after the deploy had been signed off. That is the `rates.images` defect again
// (§8.3), one field over.
//
// The disagreement is settled by REFUSING at both ends rather than by teaching
// the engine exponents. A rate card is transcribed from a vendor's published
// price, and no vendor prints one in exponent notation; a `1.25e-7` in a rate
// field is far more likely to be a per-token figure that arrived through a
// program's float formatter than a considered price (§13.1c). Writing the
// digits out is also the only form in which a human reading the file can see
// which side of a million the number is on.
//
// The one place exponent notation IS accepted is [ImportProxyConfig], whose
// input is a foreign file written by a program that prints floats. It converts
// the value and writes the digits out, so what this function guards never sees
// one.
func checkDecimal(s string) error {
	bad := fmt.Errorf("invalid decimal %q", s)
	body := s
	if body != "" && (body[0] == '+' || body[0] == '-') {
		body = body[1:]
	}
	if body == "" {
		return bad
	}
	if strings.ContainsAny(body, "eE") {
		return fmt.Errorf("invalid decimal %q: exponent notation is not accepted, "+
			"write the digits out — a rate card is read by people and a price is "+
			"per a stated quantity (§13.1c)", s)
	}
	intPart, fracPart := body, ""
	if dot := strings.IndexByte(body, '.'); dot >= 0 {
		intPart, fracPart = body[:dot], body[dot+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return bad
		}
	}
	if intPart == "" && fracPart == "" {
		return bad
	}
	if !decimalDigits(intPart) || !decimalDigits(fracPart) {
		return bad
	}
	// Trailing zeros carry no value and internal/pricing drops them before it
	// counts, so "2.500000000000000" is twelve digits there and must be here.
	frac := strings.TrimRight(fracPart, "0")
	if len(frac) > maxDecimalScale {
		return fmt.Errorf("invalid decimal %q: at most %d fractional digits are supported",
			s, maxDecimalScale)
	}
	units := strings.TrimLeft(intPart, "0") + frac
	if len(units) > len(maxDecimalUnits) ||
		(len(units) == len(maxDecimalUnits) && units > maxDecimalUnits) {
		return fmt.Errorf("invalid decimal %q: too many significant digits to be held exactly", s)
	}
	return nil
}

// decimalDigits reports whether s is all ASCII digits. The empty string is,
// which is what makes "5." and ".5" the same shape as "5.0" here.
func decimalDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// decimalParts splits a decimal literal into its sign, its digits and the power
// of ten they carry: value = (-1)^neg × digits × 10^exp.
//
// Exponent notation is accepted HERE and refused by [checkDecimal], and the
// asymmetry is deliberate: the only callers are the importer, whose input is a
// foreign file a program wrote, and the rate-scale advisory, which has to be
// able to judge a number it would not accept.
func decimalParts(s string) (neg bool, digits string, exp int, ok bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return false, "", 0, false
	}
	if t[0] == '+' || t[0] == '-' {
		neg = t[0] == '-'
		t = t[1:]
	}
	mant := t
	if i := strings.IndexAny(t, "eE"); i >= 0 {
		mant = t[:i]
		e, err := strconv.Atoi(t[i+1:])
		if err != nil {
			return false, "", 0, false
		}
		exp = e
	}
	intPart, fracPart := mant, ""
	if d := strings.IndexByte(mant, '.'); d >= 0 {
		intPart, fracPart = mant[:d], mant[d+1:]
	}
	if (intPart == "" && fracPart == "") || !decimalDigits(intPart) || !decimalDigits(fracPart) {
		return false, "", 0, false
	}
	return neg, intPart + fracPart, exp - len(fracPart), true
}

// maxDecimalShift bounds how far [scaleDecimal] will move a decimal point. It
// is far beyond any rate a vendor publishes and it stops "1e999999999" from
// asking for a gigabyte of zeros.
const maxDecimalShift = 30

// scaleDecimal returns lit × 10^n written out in full decimal notation. It is
// exact: moving the decimal point is the whole operation, so no value passes
// through a float and nothing is rounded.
//
// This is how a per-token rate becomes a per-million-token rate. The conversion
// is a fact about the two files' units and belongs in one place, which is
// [priceFieldByKey] — this function is only the arithmetic.
func scaleDecimal(lit string, n int) (string, bool) {
	neg, digits, exp, ok := decimalParts(lit)
	if !ok || len(digits) > 40 {
		return "", false
	}
	exp += n
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		// Zero has no sign and no scale: "-0.000" is "0".
		return "0", true
	}
	if t := strings.TrimRight(digits, "0"); t != "" {
		exp += len(digits) - len(t)
		digits = t
	}
	if exp > maxDecimalShift || exp < -maxDecimalShift {
		return "", false
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	switch {
	case exp >= 0:
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", exp))
	case -exp >= len(digits):
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -exp-len(digits)))
		b.WriteString(digits)
	default:
		point := len(digits) + exp
		b.WriteString(digits[:point])
		b.WriteByte('.')
		b.WriteString(digits[point:])
	}
	return b.String(), true
}

// decimalBelowPow10 reports whether lit is a non-zero magnitude smaller than
// 10^e. It compares exponents rather than values, so it is exact and needs no
// float.
func decimalBelowPow10(lit string, e int) bool {
	_, digits, exp, ok := decimalParts(lit)
	if !ok {
		return false
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return false // zero is not a suspiciously small price, it is a free model
	}
	// digits × 10^exp lies in [10^(len-1+exp), 10^(len+exp)), so the magnitude is
	// below 10^e exactly when len+exp <= e.
	return len(digits)+exp <= e
}
