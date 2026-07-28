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

// checkDecimal verifies decimal syntax without converting through a float.
func checkDecimal(s string) error {
	bad := fmt.Errorf("invalid decimal %q", s)
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return bad
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		exp := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			exp++
		}
		if exp == 0 {
			return bad
		}
	}
	if i != len(s) {
		return bad
	}
	return nil
}
