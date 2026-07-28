package mask

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A PatternSpec names one thing an operator wants removed from a request.
//
// Either it names a built-in (Regexp empty) or it carries the operator's own
// expression. DESIGN §10.5b is explicit that the pattern set is the operator's:
// "a gateway cannot ship the right pattern set for every jurisdiction". The
// built-ins are worked examples, not a policy.
type PatternSpec struct {
	// Name identifies the pattern in counters and in the plugin surface. It is
	// never part of a placeholder, so it cannot leak into upstream text.
	Name string
	// Regexp is an RE2 expression. Empty means "the built-in called Name".
	Regexp string
}

// builtins are the worked examples of DESIGN §10.5b.
//
// # krrn — Korean resident registration number (주민등록번호)
//
// YYMMDD-Sxxxxxx. The pattern checks the date part and the century/gender digit
// and deliberately does **not** verify the trailing check digit, which is the
// interesting part: since October 2020 the six digits after the gender digit are
// randomly assigned, so the pre-2020 checksum no longer holds for a number
// issued after that date. A checksum test would therefore reject exactly the
// newest numbers — the ones most likely to belong to someone still filling in
// forms. For a masking filter a false positive costs a masked six-digit run and
// a false negative costs an identity number leaving the operator's control, and
// those are not the same price.
//
// # email
//
// Deliberately loose on the local part and anchored on a dotted domain. An
// address is not a secret in the way an identity number is, but it is the most
// common thing an operator's disclosure rules name after one.
var builtins = map[string]string{
	"krrn":  `\b\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])[-\x{2013}\x{2014}]?[1-8]\d{6}\b`,
	"email": `\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+\b`,
}

// Builtins lists the built-in pattern names in a stable order.
func Builtins() []string {
	out := make([]string, 0, len(builtins))
	for n := range builtins {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ErrEmptyMatch reports a pattern that can match the empty string.
//
// It is a load error rather than a runtime curiosity: a pattern that matches
// nothing at every position would issue a placeholder between every pair of
// characters, which is not a mask but a denial of service written in
// configuration.
var ErrEmptyMatch = errors.New("mask: pattern matches the empty string")

type pattern struct {
	name string
	re   *regexp.Regexp
	// group is this pattern's capturing-group number inside the set's combined
	// expression, which is how one scan reports *which* pattern matched.
	group int
}

// PatternSet is a compiled, immutable set of patterns, and one expression.
//
// §10.5c's reasoning applies here too: the set changes only when the
// configuration is reloaded, so it is compiled once at load and shared by every
// request that uses it.
//
// # One pass, not one pass per pattern
//
// The patterns are compiled into a single alternation and applied in one
// left-to-right scan. Applying them in sequence — mask for pattern 1, then scan
// the result for pattern 2 — is the shape that produces the bug jikji's
// `internal/compositor/chat_proof.go` pins with a 200-trial regression test:
// once a replacement has been written into the text, a later pattern can match
// across it, or match the replacement itself, and the result depends on the
// order the passes happened to run in. One scan over one expression cannot do
// that, because nothing it writes is ever looked at again.
//
// Overlap resolves leftmost-longest, which is a property of the combined
// expression rather than of the order an operator happened to list their
// patterns in. That matters for safety, not just for tidiness: with two
// patterns `\d{6}` and a full resident registration number, leftmost-longest
// masks the whole number, and leftmost-first would mask its first six digits and
// leave the rest in the request.
type PatternSet struct {
	pats []pattern
	re   *regexp.Regexp
	// escapeGroup is the group of the built-in first alternative, which matches
	// the placeholder sentinel itself. It is part of the same scan so that
	// caller text that looks like gateway machinery is escaped by the same rule
	// as everything else, and cannot be produced by an earlier pass for a later
	// one to find.
	escapeGroup int
}

// NewPatternSet compiles specs into one expression.
func NewPatternSet(specs []PatternSpec) (*PatternSet, error) {
	if len(specs) == 0 {
		return nil, errors.New("mask: a filter needs at least one pattern")
	}
	set := &PatternSet{pats: make([]pattern, 0, len(specs))}
	seen := make(map[string]bool, len(specs))
	for i, sp := range specs {
		name := strings.TrimSpace(sp.Name)
		if name == "" {
			return nil, fmt.Errorf("mask: patterns[%d]: needs a name", i)
		}
		if seen[name] {
			return nil, fmt.Errorf("mask: patterns[%d]: %q is listed twice", i, name)
		}
		seen[name] = true

		expr := sp.Regexp
		if expr == "" {
			b, ok := builtins[name]
			if !ok {
				return nil, fmt.Errorf("mask: patterns[%d]: %q is not a built-in pattern "+
					"(built-ins are %s); give it a regexp of its own", i, name,
					strings.Join(Builtins(), ", "))
			}
			expr = b
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("mask: patterns[%d] (%s): %w", i, name, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("mask: patterns[%d] (%s): %w", i, name, ErrEmptyMatch)
		}
		set.pats = append(set.pats, pattern{name: name, re: re})
	}

	// The combined expression. The sentinel goes first so that its group number
	// is fixed; every operator pattern is wrapped in a group of its own, and a
	// pattern's own capturing groups shift the numbering of the ones after it,
	// which is why the offsets are computed here rather than assumed.
	var b strings.Builder
	b.WriteString("(")
	b.WriteString(regexp.QuoteMeta(Sentinel))
	b.WriteString(")")
	set.escapeGroup = 1
	next := 2
	for i := range set.pats {
		b.WriteString("|(")
		b.WriteString(set.pats[i].re.String())
		b.WriteString(")")
		set.pats[i].group = next
		next += 1 + set.pats[i].re.NumSubexp()
	}
	combined, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("mask: combining patterns: %w", err)
	}
	combined.Longest()
	set.re = combined
	return set, nil
}

// Names lists the patterns in application order.
func (s *PatternSet) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.pats))
	for i := range s.pats {
		out[i] = s.pats[i].name
	}
	return out
}

// Len reports how many patterns are in the set.
func (s *PatternSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.pats)
}
