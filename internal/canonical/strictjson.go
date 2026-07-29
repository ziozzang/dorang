package canonical

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Case-sensitive JSON decoding (COMPATIBILITY 2.0, DESIGN §18 W10).
//
// # The defect this closes
//
// Go's encoding/json matches an object member to a struct field by exact name
// first and, failing that, by CASE-FOLDED name. A struct with `json:"model"`
// is therefore filled from a member spelled "Model", "MODEL" or "moDel" — and,
// because the fold is Unicode simple folding and not merely ASCII case, also
// from "ſtream" for `json:"stream"` (U+017F LATIN SMALL LETTER LONG S folds to
// 's') and from "model" spelled with U+212A KELVIN SIGN wherever a name
// contains a 'k'.
//
// No other implementation does this. The authorization gate
// (internal/server/peek.go) compares key bytes exactly, and every Python
// backend downstream compares them exactly. So a body carrying
// {"Model":"expensive-model"} was authorized as having NO model — the API
// key's model allow-list never fired — and then dispatched as having one.
// That is an allow-list bypass, which is why request decoding is strict here
// even though it costs a scan.
//
// # The rule
//
// A member fills a field only when its key, after JSON unescaping, is
// byte-identical to the field's name. A member whose key is not byte-identical
// but WOULD be fold-matched by encoding/json is REMOVED from the document
// before decoding.
//
// Removed, not renamed and not preserved: an adapter that kept "Model" in its
// pass-through Extra map would hand it to the next hop, and any hop that
// parses with encoding/json — the next dorang, a Go-based OpenAI-compatible
// server — re-creates the bypass one link further down. A key that collides
// under folding has no legitimate sender: the field it collides with is
// spelled exactly one way on every wire dorang speaks.
//
// # Duplicate keys
//
// JSON permits an object to repeat a key and implementations disagree on which
// one wins. THE RULE IS LAST-WINS, EXCEPT THAT A JSON null NEVER OVERWRITES AN
// EARLIER VALUE, because that is what both encoding/json and the gate's scanner
// do. encoding/json overwrites the field on every matching member and documents
// that "unmarshaling a JSON null into any other Go type has no effect on the
// value"; the gate assigns to `model` on every matching member whose value is a
// string, so a later null leaves the earlier name standing there too.
// {"model":"a","model":"b"} is "b" on both sides and {"model":"a","model":null}
// is "a" on both sides.
//
// Filtering does not disturb that — it only removes members that were never the
// same key to begin with, so {"model":"a","MODEL":"b"} resolves to "a"
// everywhere instead of "a" at the gate and "b" in the adapter.
//
// # Where strictness stops
//
// A type that decodes itself (implements json.Unmarshaler) takes over: the
// filter does not descend into it, because only that method knows what its
// bytes mean. Every such type in the wire adapters calls back into
// [StrictBytes] on the alias it decodes, so the chain stays strict. Raw
// pass-through values — json.RawMessage, map[string]json.RawMessage, any — are
// never rewritten either. That is not a gap but a requirement: a tool's
// `parameters` is a caller-authored JSON Schema whose own members are named
// "type", "name" and "description", and rewriting inside it would corrupt
// caller data to fix a problem that does not exist there.

// StrictUnmarshal decodes data into v with case-SENSITIVE field matching.
//
// It is json.Unmarshal with the fold-matching removed; see the file comment.
// Errors, including the exact text of decode errors, are encoding/json's.
func StrictUnmarshal(data []byte, v any) error {
	return json.Unmarshal(StrictBytes(data, v), v)
}

// StrictBytes returns data with every object member removed whose key would be
// fold-matched to a field of v's type by encoding/json without being
// byte-identical to that field's name.
//
// It returns data itself — the same backing array, not a copy — when there is
// nothing to remove, which is every well-formed request. The cost on that path
// is one structural scan that never enters a string and never allocates.
//
// v is the value that will be decoded into, and is used for its TYPE only;
// nothing is read from or written to it. When that type decodes itself,
// StrictBytes returns data unchanged and leaves strictness to its
// UnmarshalJSON.
func StrictBytes(data []byte, v any) []byte {
	ti := infoOf(reflect.TypeOf(v))
	if ti == nil || !ti.deep {
		return data
	}
	c := cutter{b: data}
	end := c.value(jsonSkipSpace(data, 0), ti)
	if end < 0 || c.out == nil {
		// Nothing removed, or the scan gave up on a body encoding/json is
		// about to reject anyway. Hand back the original so the error the
		// caller sees is the reference parser's error and not this scanner's
		// opinion of it.
		return data
	}
	return append(c.out, data[c.copied:]...)
}

// ---------------------------------------------------------------------------
// Rewriting
// ---------------------------------------------------------------------------

// cutter walks a document and excises the members that must not be matched.
//
// The output is the input minus those spans, byte for byte — whitespace,
// number spelling and escape spelling are all preserved. Rebuilding the
// document from parsed tokens would have been easier to write and would have
// silently normalized bodies that encoding/json rejects, turning the filter
// into a second, more permissive parser sitting in front of the real one.
type cutter struct {
	b      []byte
	out    []byte
	copied int
}

// drop excises the member whose key literal starts at start and whose value
// ends at end, together with the one comma that separated it from its
// neighbours.
func (c *cutter) drop(start, end int) {
	if k := jsonSkipSpace(c.b, end); k < len(c.b) && c.b[k] == ',' {
		end = k + 1
	} else {
		p := start
		for p > 0 && isJSONSpace(c.b[p-1]) {
			p--
		}
		if p > 0 && c.b[p-1] == ',' {
			start = p - 1
		}
	}
	if start < c.copied {
		// The preceding member was dropped too and took this comma with it.
		start = c.copied
	}
	if c.out == nil {
		c.out = make([]byte, 0, len(c.b))
	}
	c.out = append(c.out, c.b[c.copied:start]...)
	c.copied = end
}

// value consumes one JSON value at b[i], descending only where ti says the
// members carry meaning. It returns the index just past the value, or -1 when
// the document is malformed.
func (c *cutter) value(i int, ti *typeInfo) int {
	if i < 0 || i >= len(c.b) {
		return -1
	}
	if ti == nil || !ti.deep {
		return jsonSkipValue(c.b, i)
	}
	switch c.b[i] {
	case '{':
		if ti.kind == kStruct || ti.kind == kMap {
			return c.object(i, ti)
		}
	case '[':
		if ti.kind == kSlice {
			return c.array(i, ti)
		}
	}
	// The body's shape does not match the target's. encoding/json will say so.
	return jsonSkipValue(c.b, i)
}

func (c *cutter) object(i int, ti *typeInfo) int {
	i++ // past '{'
	for {
		i = jsonSkipSpace(c.b, i)
		if i >= len(c.b) {
			return -1
		}
		switch c.b[i] {
		case '}':
			return i + 1
		case ',':
			i++
			continue
		case '"':
		default:
			return -1
		}
		keyStart := i
		keyEnd, esc, ok := jsonScanString(c.b, i)
		if !ok {
			return -1
		}
		j := jsonSkipSpace(c.b, keyEnd)
		if j >= len(c.b) || c.b[j] != ':' {
			return -1
		}
		j = jsonSkipSpace(c.b, j+1)
		if j >= len(c.b) {
			return -1
		}
		child, collides := ti.member(c.b[keyStart:keyEnd], esc)
		if collides {
			end := jsonSkipValue(c.b, j)
			if end < 0 {
				return -1
			}
			c.drop(keyStart, end)
			i = end
			continue
		}
		end := c.value(j, child)
		if end < 0 {
			return -1
		}
		i = end
	}
}

func (c *cutter) array(i int, ti *typeInfo) int {
	i++ // past '['
	for {
		i = jsonSkipSpace(c.b, i)
		if i >= len(c.b) {
			return -1
		}
		switch c.b[i] {
		case ']':
			return i + 1
		case ',':
			i++
			continue
		}
		end := c.value(i, ti.elem)
		if end < 0 {
			return -1
		}
		i = end
	}
}

// ---------------------------------------------------------------------------
// Type information
// ---------------------------------------------------------------------------

type infoKind uint8

const (
	kOpaque infoKind = iota // never descended into
	kStruct
	kSlice
	kMap
)

// typeInfo is what the filter needs to know about a target type: the exact
// member names that mean something at this level, the folded forms those names
// collide with, and where to go next.
type typeInfo struct {
	kind infoKind
	// deep reports whether anything under this type has named members. A
	// []string or a map[string]float64 has none and is skipped whole.
	deep   bool
	elem   *typeInfo
	fields map[string]*typeInfo
	folded map[string]struct{}
	// lower is set when every field name is ASCII with no upper-case letter,
	// which lets an all-lower-case key skip the fold entirely: for two such
	// names, folding equal implies bytes equal.
	lower bool
}

// member resolves one key literal (quotes included).
//
// It returns the type to descend into, and whether the member must be dropped
// because encoding/json would fold it onto a field it is not spelled as.
func (ti *typeInfo) member(lit []byte, esc bool) (*typeInfo, bool) {
	switch ti.kind {
	case kMap:
		// Map keys are matched exactly by encoding/json — there is no folding
		// to undo — so nothing is ever dropped from a map, only descended into.
		return ti.elem, false
	case kStruct:
	default:
		return nil, false
	}
	if esc {
		// "model" IS the key "model" to a conforming parser, and the
		// escape must be resolved before either comparison. This is the same
		// reason the gate keeps a slow path for escaped keys.
		var s string
		if err := json.Unmarshal(lit, &s); err != nil {
			return nil, false
		}
		if child, ok := ti.fields[s]; ok {
			return child, false
		}
		_, collides := ti.folded[string(foldJSONName([]byte(s)))]
		return nil, collides
	}
	key := lit[1 : len(lit)-1]
	if child, ok := ti.fields[string(key)]; ok {
		return child, false
	}
	if ti.lower && !foldable(key) {
		return nil, false
	}
	var arr [64]byte
	fk := appendFoldedJSONName(arr[:0], key)
	_, collides := ti.folded[string(fk)]
	return nil, collides
}

var (
	infoCache      sync.Map // reflect.Type -> *typeInfo
	unmarshalerTyp = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	opaqueInfo     = &typeInfo{kind: kOpaque}
)

func infoOf(t reflect.Type) *typeInfo {
	if t == nil {
		return nil
	}
	if v, ok := infoCache.Load(t); ok {
		return v.(*typeInfo)
	}
	ti := buildInfo(t, map[reflect.Type]*typeInfo{})
	infoCache.Store(t, ti)
	return ti
}

func buildInfo(t reflect.Type, seen map[reflect.Type]*typeInfo) *typeInfo {
	if ti, ok := seen[t]; ok {
		return ti
	}
	if decodesItself(t) {
		return opaqueInfo
	}
	switch t.Kind() {
	case reflect.Pointer:
		return buildInfo(t.Elem(), seen)
	case reflect.Slice, reflect.Array:
		e := buildInfo(t.Elem(), seen)
		if !e.deep {
			return opaqueInfo
		}
		return &typeInfo{kind: kSlice, deep: true, elem: e}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return opaqueInfo
		}
		e := buildInfo(t.Elem(), seen)
		if !e.deep {
			return opaqueInfo
		}
		return &typeInfo{kind: kMap, deep: true, elem: e}
	case reflect.Struct:
		ti := &typeInfo{
			kind:   kStruct,
			deep:   true, // provisional, so a self-referential type terminates
			lower:  true,
			fields: make(map[string]*typeInfo),
			folded: make(map[string]struct{}),
		}
		seen[t] = ti
		addFields(t, ti, seen, false)
		addFields(t, ti, seen, true)
		ti.deep = len(ti.fields) > 0
		return ti
	}
	return opaqueInfo
}

// decodesItself reports whether encoding/json hands a value of this type to its
// own UnmarshalJSON. Struct fields are addressable, so a pointer-receiver
// method counts.
func decodesItself(t reflect.Type) bool {
	return t.Implements(unmarshalerTyp) || reflect.PointerTo(t).Implements(unmarshalerTyp)
}

// addFields records one level of fields. It runs twice — declared fields
// first, then the promoted fields of embedded structs — so that a shallower
// name wins, which is encoding/json's precedence.
func addFields(t reflect.Type, ti *typeInfo, seen map[reflect.Type]*typeInfo, embedded bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && !decodesItself(ft) {
				if embedded {
					addFields(ft, ti, seen, false)
					addFields(ft, ti, seen, true)
				}
				continue
			}
		}
		if embedded || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, dup := ti.fields[name]; dup {
			continue
		}
		ti.fields[name] = buildInfo(f.Type, seen)
		ti.folded[string(foldJSONName([]byte(name)))] = struct{}{}
		if foldable([]byte(name)) {
			ti.lower = false
		}
	}
}

// ---------------------------------------------------------------------------
// Folding
// ---------------------------------------------------------------------------

// foldJSONName reproduces encoding/json's foldName exactly: ASCII lower-case is
// raised, and every other rune is replaced by the smallest rune of its simple
// fold set. Reproducing it — rather than approximating it with
// strings.ToLower — is the whole point, because the interesting collisions are
// the non-ASCII ones (U+017F ſ against 's', U+212A K against 'k') that an ASCII
// approximation would miss and that encoding/json would then match.
func foldJSONName(in []byte) []byte {
	var arr [32]byte
	return appendFoldedJSONName(arr[:0], in)
}

func appendFoldedJSONName(out, in []byte) []byte {
	for i := 0; i < len(in); {
		if c := in[i]; c < utf8.RuneSelf {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			out = append(out, c)
			i++
			continue
		}
		r, n := utf8.DecodeRune(in[i:])
		out = utf8.AppendRune(out, foldJSONRune(r))
		i += n
	}
	return out
}

func foldJSONRune(r rune) rune {
	for {
		r2 := unicode.SimpleFold(r)
		if r2 <= r {
			return r2
		}
		r = r2
	}
}

// foldable reports whether folding s can change it — i.e. whether it holds an
// upper-case ASCII letter or any byte outside ASCII.
func foldable(s []byte) bool {
	for _, c := range s {
		if c >= utf8.RuneSelf || ('A' <= c && c <= 'Z') {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Scanning
//
// These mirror internal/server/peek.go byte for byte in behaviour, and they do
// so on purpose: the gate and the adapter have to agree about where a value
// ends before they can agree about what it says. Keeping the two walks
// structurally identical is what makes the differential test meaningful rather
// than a coincidence.
// ---------------------------------------------------------------------------

// ScanSpace, ScanString and ScanValue are the walk above, exported for
// [github.com/ziozzang/dorang/internal/wire/wirejson], which splits the
// unmodelled members out of an object without re-parsing it.
//
// They are exported rather than copied because the adapter and the filter walk
// THE SAME BYTES of the same request, one immediately after the other, and a
// third opinion about where a value ends is a third thing that can disagree with
// the gate. There are two copies of this walk in dorang — this one and
// internal/server/peek.go's — and that is two on purpose, held together by a
// differential test. A third would not be.
func ScanSpace(b []byte, i int) int { return jsonSkipSpace(b, i) }

// ScanString consumes a string literal starting at b[i] == '"'. See
// [ScanSpace] for why this is exported.
func ScanString(b []byte, i int) (end int, escaped, ok bool) { return jsonScanString(b, i) }

// ScanValue consumes one JSON value and returns the index just past it, or -1.
// See [ScanSpace] for why this is exported.
func ScanValue(b []byte, i int) int { return jsonSkipValue(b, i) }

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func jsonSkipSpace(b []byte, i int) int {
	if i < 0 {
		return i
	}
	for i < len(b) && isJSONSpace(b[i]) {
		i++
	}
	return i
}

// jsonScanString consumes a string literal starting at b[i] == '"' and returns
// the index just past the closing quote, whether the literal contained a
// backslash escape, and whether it terminated.
func jsonScanString(b []byte, i int) (end int, escaped, ok bool) {
	start := i + 1
	j := start
	for {
		n := bytes.IndexByte(b[j:], '"')
		if n < 0 {
			return len(b), true, false
		}
		k := j + n
		bs := 0
		for k-1-bs >= start && b[k-1-bs] == '\\' {
			bs++
		}
		if bs%2 == 0 {
			return k + 1, bytes.IndexByte(b[start:k], '\\') >= 0, true
		}
		j = k + 1
	}
}

// jsonSkipValue consumes one JSON value and returns the index just past it,
// or -1.
func jsonSkipValue(b []byte, i int) int {
	if i < 0 || i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		end, _, ok := jsonScanString(b, i)
		if !ok {
			return -1
		}
		return end
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				end, _, ok := jsonScanString(b, i)
				if !ok {
					return -1
				}
				i = end
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return -1
	default:
		for i < len(b) {
			switch b[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i
			}
			i++
		}
		return i
	}
}
