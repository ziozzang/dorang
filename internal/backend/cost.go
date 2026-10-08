package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// A reported cost is the upstream's own statement of what a request cost — OpenRouter's
// `usage.cost`, DeepInfra's `usage.estimated_cost` — as opposed to the figure dorang
// computes from its own rate card.
//
// It is read here, at the backend, from the upstream's bytes and not from any wire
// family's decoder, for the reason the per-family usage readers show: each family has
// its own usage struct, its own unknown-member bag and its own stream accumulator, and
// a figure that has to survive all of them is a figure one of them drops. The kind
// declares WHERE the figure is ([catalog.KindDefaults.ReportedCost]); nothing else
// about the host is assumed.
//
// It travels on [Result], beside canonical.Usage rather than inside it. Usage's
// zero value means "nothing was counted" to every encoder that decides whether to
// emit a usage object at all, and a cost-only usage would make those encoders invent
// zero token counts.

// costSpec is a parsed reported-cost declaration.
type costSpec struct {
	path []string
	// leaf is `"<last segment>"`, the cheap pre-check before any decode.
	leaf []byte
	// scale is the power of ten that turns the stated figure into nano-USD.
	scale int
}

// newCostSpec resolves a kind's declaration. An empty path is a host that states no
// cost, and returns nil.
func newCostSpec(path, unit string) (*costSpec, error) {
	if path == "" {
		return nil, nil
	}
	if err := catalog.CheckCostPath(path); err != nil {
		return nil, fmt.Errorf("backend: reported_cost: %w", err)
	}
	s := &costSpec{path: strings.Split(path, ".")}
	s.leaf = []byte(`"` + s.path[len(s.path)-1] + `"`)
	switch unit {
	case "", catalog.ReportedCostUSD:
		s.scale = 9
	case catalog.ReportedCostUSDCents:
		s.scale = 7
	default:
		return nil, fmt.Errorf("backend: reported_cost_unit %q is not %q or %q",
			unit, catalog.ReportedCostUSD, catalog.ReportedCostUSDCents)
	}
	return s, nil
}

// costState is what one document said about the cost. Absent, null and invalid
// are three different facts and none of them is zero.
type costState uint8

const (
	costAbsent costState = iota
	costNull
	costInvalid
	costStated
)

// read evaluates the path against one JSON document.
func (s *costSpec) read(doc []byte) (int64, costState) {
	if s == nil || !bytes.Contains(doc, s.leaf) {
		return 0, costAbsent
	}
	cur := json.RawMessage(doc)
	for _, seg := range s.path {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cur, &obj); err != nil {
			return 0, costAbsent
		}
		next, ok := obj[seg]
		if !ok {
			return 0, costAbsent
		}
		cur = next
	}
	lex := bytes.TrimSpace(cur)
	if string(lex) == "null" {
		return 0, costNull
	}
	if len(lex) >= 2 && lex[0] == '"' && lex[len(lex)-1] == '"' {
		// A figure some vendors quote as a string; the same lexeme rules apply.
		lex = lex[1 : len(lex)-1]
	}
	n, ok := scaleDecimal(lex, s.scale)
	if !ok {
		return 0, costInvalid
	}
	return n, costStated
}

// scaleDecimal converts a JSON number lexeme to an integer count of 10^-scale units,
// rounding half to even on the digits it drops. It never goes through float64:
// `3.4e-07` USD is exactly 340 nano-USD and a binary float is not.
//
// It refuses a negative figure — a cost below zero is a credit, and a credit is the
// operator's to state in an adjustment rule, not an upstream's to inject — and one
// that does not fit an int64.
func scaleDecimal(lex []byte, scale int) (int64, bool) {
	s := string(lex)
	if s == "" {
		return 0, false
	}
	if s[0] == '-' {
		s = s[1:]
		if strings.Trim(s, "0.eE+-") != "" {
			return 0, false
		}
		return 0, true // -0, -0.0: zero
	}
	mant, exp := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant = s[:i]
		e := s[i+1:]
		neg := false
		if e != "" && (e[0] == '+' || e[0] == '-') {
			neg = e[0] == '-'
			e = e[1:]
		}
		if e == "" || len(e) > 4 {
			return 0, false
		}
		for _, r := range e {
			if r < '0' || r > '9' {
				return 0, false
			}
			exp = exp*10 + int(r-'0')
		}
		if neg {
			exp = -exp
		}
	}
	intPart, frac := mant, ""
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		intPart, frac = mant[:i], mant[i+1:]
		if frac == "" {
			return 0, false
		}
	}
	if intPart == "" {
		return 0, false
	}
	digits := intPart + frac
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	// value = digits × 10^(exp - len(frac)); wanted = value × 10^scale.
	shift := exp - len(frac) + scale
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return 0, true
	}
	if shift >= 0 {
		if len(digits)+shift > 19 {
			return 0, false
		}
		digits += strings.Repeat("0", shift)
		return parseU63(digits)
	}
	// Drop -shift trailing digits, rounding half to even.
	drop := -shift
	if drop > len(digits) {
		// Everything is below the last kept place; only a value of at least half a
		// unit could round up, and with drop > len(digits) it is below a tenth.
		return 0, true
	}
	keep, gone := digits[:len(digits)-drop], digits[len(digits)-drop:]
	n := int64(0)
	if keep != "" {
		v, ok := parseU63(keep)
		if !ok {
			return 0, false
		}
		n = v
	}
	half := "5" + strings.Repeat("0", len(gone)-1)
	switch c := strings.Compare(gone, half); {
	case c > 0, c == 0 && n%2 == 1:
		if n == 1<<63-1 {
			return 0, false
		}
		n++
	}
	return n, true
}

func parseU63(d string) (int64, bool) {
	if len(d) > 19 {
		return 0, false
	}
	var n uint64
	for _, r := range d {
		n = n*10 + uint64(r-'0')
	}
	if n > 1<<63-1 {
		return 0, false
	}
	return int64(n), true
}

// costTapMaxEvent bounds the bytes one SSE event may hold while the tap watches it.
// An event larger than this is not inspected; the stream itself is unaffected.
const costTapMaxEvent = 1 << 20

// costTap watches an SSE body for the event that states the cost.
//
// It is OBSERVATIONAL: every read is passed through unchanged, nothing it finds can
// fail the stream, and a malformed or oversized event is skipped rather than
// reported. It inspects whole EVENTS — the `data:` lines up to the blank line that
// ends one, joined with "\n" — because one JSON payload may span several data lines.
// The last event that states a valid figure wins; an event that states an invalid one
// or null erases nothing.
type costTap struct {
	r    io.ReadCloser
	spec *costSpec

	line  []byte // the line being read, up to its newline
	data  []byte // the current event's data
	skip  bool   // the current event outgrew costTapMaxEvent
	found bool
	nano  int64
}

func newCostTap(r io.ReadCloser, spec *costSpec) *costTap {
	return &costTap{r: r, spec: spec}
}

func (t *costTap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.observe(p[:n])
	if err == io.EOF {
		// A stream that ends without a closing blank line still ended its event.
		t.endLine()
		t.endEvent()
	}
	return n, err
}

func (t *costTap) Close() error { return t.r.Close() }

func (t *costTap) observe(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			t.appendLine(b)
			return
		}
		t.appendLine(b[:i])
		t.endLine()
		b = b[i+1:]
	}
}

func (t *costTap) appendLine(b []byte) {
	if t.skip {
		return
	}
	if len(t.line)+len(b) > costTapMaxEvent {
		t.skip, t.line, t.data = true, t.line[:0], t.data[:0]
		return
	}
	t.line = append(t.line, b...)
}

// endLine handles one complete line.
func (t *costTap) endLine() {
	line := bytes.TrimSuffix(t.line, []byte{'\r'})
	t.line = t.line[:0]
	switch {
	case len(line) == 0:
		t.endEvent()
	case t.skip:
	case bytes.HasPrefix(line, []byte("data:")):
		v := bytes.TrimPrefix(line[5:], []byte{' '})
		if len(t.data)+len(v)+1 > costTapMaxEvent {
			t.skip, t.data = true, t.data[:0]
			return
		}
		if len(t.data) > 0 {
			t.data = append(t.data, '\n')
		}
		t.data = append(t.data, v...)
	}
}

func (t *costTap) endEvent() {
	if !t.skip && len(t.data) > 0 {
		if n, st := t.spec.read(t.data); st == costStated {
			t.nano, t.found = n, true
		}
	}
	t.data, t.skip = t.data[:0], false
}

// reported returns the figure the stream stated, if any did.
func (t *costTap) reported() (int64, bool) {
	if t == nil {
		return 0, false
	}
	return t.nano, t.found
}
