package wirejson

import (
	"bytes"
	"encoding"
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// Writing a nested wire type into its parent's buffer instead of into a slice
// of its own.
//
// # The pass this removes
//
// encoding/json's contract for a [json.Marshaler] is that the method returns a
// finished document, and the encoder then runs `compact` over it to fold it into
// the buffer it is building. compact is a full scan: it walks every byte through
// the JSON state machine to find the whitespace it is allowed to drop, and with
// HTML escaping off — which is what COMPATIBILITY 2.1a requires — it drops
// nothing and copies everything.
//
// A wire type implements MarshalJSON because that is how the Extra splice, the
// field order and the omission rules are made exact. So every one of them pays
// that scan, and it is a scan PER NESTING LEVEL: a chat request is an object
// holding messages, each holding a content value, each part an object, and the
// text of a message is re-scanned by the part's marshal, the content's, the
// message's and the request's. Measured on this package's own benchmark, compact
// and its state machine were 70% of MarshalRequest — more than the encoding.
//
// # What replaces it
//
// [Appender] is the same method one argument different: it writes into the
// caller's buffer rather than returning a slice. A type that implements it is
// written straight into its parent, so the document is built once and scanned
// never.
//
// MarshalJSON STAYS, and stays as it was — reflective, reaching its own nested
// types through encoding/json. It is not reimplemented in terms of AppendJSON,
// and that is the point rather than an omission: a wire type now has two
// independent serializers that must produce identical bytes, which is a
// property a fuzzer can refute. Implementing one in terms of the other would
// leave nothing to compare, and every caller that hands a wire type to
// encoding/json — the goldens above all — would be exercising the new path
// while appearing to check the old one.
//
// # Why this is allowed to be a second serializer
//
// It is not a second serializer. It is the same one, and the rule that keeps it
// so is that this file REFUSES anything it is not certain of.
//
// [Append] plans a type by walking it with reflect. Where the plan covers every
// field, the fast path runs. Where it meets anything the plan does not model it
// returns no plan at all and the value goes to [Marshal], which is
// encoding/json, unchanged. An unhandled shape is therefore SLOW and never
// wrong.
//
// The refused set is a list rather than a principle, because a promise stated as
// a principle is one nobody can check. In full, and in the order [plan] and
// [planStruct] test them:
//
//   - a recursive type;
//   - [json.Number];
//   - an INTERFACE-typed value, including one whose interface is [Appender] or
//     [json.Marshaler];
//   - a type that is a Marshaler, an Appender or a [encoding.TextMarshaler]
//     only through its pointer;
//   - an [encoding.TextMarshaler];
//   - `[]byte` and its named forms;
//   - a map whose key is not a plain string kind;
//   - a channel, a function, a complex number, an unsafe pointer;
//   - a container any of whose elements is refused;
//   - an embedded (anonymous) field;
//   - a tag option other than `omitempty` — `,string`, `,omitzero`, anything
//     added later;
//   - a field name that would need JSON escaping;
//   - TWO fields resolving to the same JSON name;
//   - a struct any of whose fields is refused.
//
// Two of those entries were added after the list was found to be aspirational
// rather than descriptive. The interface entry existed in this comment and not
// in the code — an interface type implements itself, so `Appender` and
// `json.Marshaler` fields were planned and then PANICKED on a nil dynamic value
// where encoding/json writes `null`. The duplicate-name entry existed nowhere:
// encoding/json drops both colliding fields and this planner wrote both, which
// is a different document. Neither was reachable from any wire type, which is
// exactly the point of a refusal list — it is what makes the type nobody has
// written yet slow instead of wrong.
//
// Being slow is still a regression, so it is also a test: [Delegations] counts
// the values an append run handed back, each adapter's TestRequestSubtreeIsPlanned
// requires that count to be zero for a request, and TestEveryMarshalerIsAnAppender
// lists the types that must carry an AppendJSON at all.
//
// The rest is a differential. FuzzAppendAgreesWithMarshal compares the two
// encoders byte for byte on arbitrary values of every wire type, and the golden
// suites compare both against captured reference bytes.

// Appender is a type that can write its own JSON into a caller's buffer.
//
// The contract is [json.Marshaler]'s with the allocation removed:
// AppendJSON(dst) must append exactly the bytes MarshalJSON would return, and
// must leave dst untouched below its original length. A type that implements
// both must agree with itself; FuzzAppendAgreesWithMarshal is what says so.
type Appender interface {
	AppendJSON(dst []byte) ([]byte, error)
}

// Append writes v's JSON into dst.
//
// It is [Marshal] with the caller's buffer: for a v whose type is planned the
// document is built in one pass, and for any other v it falls back to Marshal
// and appends the result, which is the same bytes at the same cost as before.
func Append(dst []byte, v any) ([]byte, error) {
	if v == nil {
		return append(dst, "null"...), nil
	}
	rv := reflect.ValueOf(v)
	if fn := planOf(rv.Type()); fn != nil {
		return fn(dst, rv)
	}
	delegated.Add(1)
	b, err := Marshal(v)
	if err != nil {
		return dst, err
	}
	return append(dst, b...), nil
}

// delegated counts the values an append run handed back to encoding/json,
// either because the planner refused their type or because they are a
// [json.Marshaler] with no AppendJSON.
//
// It exists so that "this type's subtree is on the fast path" can be a TEST
// rather than a claim. Being on the fast path is not observable from the bytes —
// both paths produce the same ones, which is the whole point — and an allocation
// count is a proxy that drifts. This is the property itself. The counter is
// touched only on the slow path, so the fast one pays nothing for it.
var delegated atomic.Int64

// Delegations returns the running count. It is a diagnostic, and a test that
// reads it must take a difference rather than an absolute: other packages
// encode too.
func Delegations() int64 { return delegated.Load() }

// MarshalAppender renders an Appender as a standalone document: one pooled
// buffer, one copy out.
//
// It takes an [Appender] rather than an any on purpose. Nothing in production
// calls it — the wire types' MarshalJSON stays reflective, which is what leaves
// something for the differential to compare — and the differential is its
// caller, where accepting a value with no AppendJSON would mean the test could
// pass without running the encoder under test.
func MarshalAppender(a Appender) ([]byte, error) {
	p := getScratch()
	b, err := a.AppendJSON((*p)[:0])
	if err != nil {
		putScratch(p, b)
		return nil, err
	}
	out := append([]byte(nil), b...)
	putScratch(p, b)
	return out, nil
}

// MarshalAppended is [MarshalAppender] for a value that may or may not be one:
// a plain wire struct with no Extra map has no AppendJSON of its own and is
// still planned field by field, and a value with no plan at all falls back to
// [Marshal]. It is what a top-level Marshal entry point calls.
//
// It is deliberately NOT what [MarshalAppender] does, because a differential
// that accepted either path would pass without ever running the one it is
// testing.
func MarshalAppended(v any) ([]byte, error) {
	p := getScratch()
	b, err := Append((*p)[:0], v)
	if err != nil {
		putScratch(p, b)
		return nil, err
	}
	out := append([]byte(nil), b...)
	putScratch(p, b)
	return out, nil
}

// AppendWithExtra is [MarshalWithExtra] writing into dst.
//
// The splice is byte-for-byte the same operation: the modelled object first,
// then the unmodelled members in sorted key order, so a pass-through re-emits
// what it was handed in an order that does not depend on a map iteration.
func AppendWithExtra(dst []byte, v any, extra map[string]json.RawMessage, known map[string]struct{}) ([]byte, error) {
	start := len(dst)
	dst, err := Append(dst, v)
	if err != nil {
		return dst, err
	}
	if len(extra) == 0 {
		return dst, nil
	}
	keys := extraKeys(extra, known)
	if len(keys) == 0 {
		return dst, nil
	}
	b := dst[start:]
	if len(b) < 2 || b[len(b)-1] != '}' {
		return dst, errNotObject
	}
	empty := len(b) == 2 // "{}"
	dst = dst[:len(dst)-1]
	for i, k := range keys {
		if i > 0 || !empty {
			dst = append(dst, ',')
		}
		dst = AppendString(dst, k)
		dst = append(dst, ':')
		// COMPACTED, not spliced verbatim, and the differential is what found
		// it. [MarshalWithExtra] does splice verbatim — but its result is
		// always handed back to encoding/json, which compacts a Marshaler's
		// document on the way into whatever is holding it, so the whitespace a
		// caller wrote inside an unmodelled member has never reached the wire.
		// Appending is the first path with no encoder above it, and an appender
		// that kept the whitespace would be changing the bytes 2.1a fixes.
		var err error
		if dst, err = appendCompacted(dst, extra[k]); err != nil {
			return dst, err
		}
	}
	return append(dst, '}'), nil
}

// appendCompacted appends src with its insignificant whitespace removed. It is
// encoding/json's own compact, so the acceptance rule and the error are that
// package's and not this one's opinion of them.
func appendCompacted(dst, src []byte) ([]byte, error) {
	buf := bytes.NewBuffer(dst)
	if err := json.Compact(buf, src); err != nil {
		return dst, err
	}
	return buf.Bytes(), nil
}

// extraKeys is the sorted set of unmodelled members to splice. A key that
// collides with a modelled one is dropped: the struct already emitted that
// member, and emitting it twice is a body with a duplicate key.
func extraKeys(extra map[string]json.RawMessage, known map[string]struct{}) []string {
	keys := make([]string, 0, len(extra))
	for k := range extra {
		if _, clash := known[k]; clash {
			continue
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ---------------------------------------------------------------------------
// scratch buffers
// ---------------------------------------------------------------------------

var scratchPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

func getScratch() *[]byte { return scratchPool.Get().(*[]byte) }

// putScratch returns a scratch buffer, keeping whatever capacity the append run
// grew it to. A buffer that ballooned once is dropped rather than retained for
// the life of the process, which is [PutBuf]'s rule and for the same reason.
func putScratch(p *[]byte, grown []byte) {
	if cap(grown) > 1<<20 {
		return
	}
	if cap(grown) > cap(*p) {
		*p = grown[:0]
	}
	scratchPool.Put(p)
}

// ---------------------------------------------------------------------------
// the plan
// ---------------------------------------------------------------------------

// appendFn writes one value. It is the planned equivalent of encoding/json's
// encoderFunc, and like that one it is resolved per type and cached.
type appendFn func(dst []byte, v reflect.Value) ([]byte, error)

// planCache maps a type to its plan. A stored nil means "no fast path" and is
// as much a result as a function is: it is what sends the value to [Marshal].
var planCache sync.Map // reflect.Type -> appendFn

func planOf(t reflect.Type) appendFn {
	if p, ok := planCache.Load(t); ok {
		fn, _ := p.(appendFn)
		return fn
	}
	fn := plan(t, map[reflect.Type]bool{})
	planCache.Store(t, fn)
	return fn
}

var (
	appenderType      = reflect.TypeFor[Appender]()
	marshalerType     = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	numberType        = reflect.TypeFor[json.Number]()
	rawMessageType    = reflect.TypeFor[json.RawMessage]()
)

// plan builds the appender for t, or returns nil to mean "encoding/json owns
// this one".
//
// open holds the types on the current path. A type that reaches itself is
// refused rather than planned, which is what makes a value cycle unreachable
// here: encoding/json needs ptrSeen because it plans recursive types, and this
// does not plan them at all.
func plan(t reflect.Type, open map[reflect.Type]bool) appendFn {
	if open[t] {
		return nil // recursive type
	}
	if t == numberType {
		// json.Number is a string kind that encodes as a bare number, and it
		// validates its contents while doing so. Not modelled.
		return nil
	}
	if t.Kind() == reflect.Interface {
		// An INTERFACE-typed value, refused here rather than by the `default`
		// arm below, which is where the file's promise used to claim it landed.
		//
		// It did not. An interface type implements itself, so `Appender` and
		// `json.Marshaler` fields reached `t.Implements(appenderType)` two lines
		// down and were PLANNED — and then [appendViaAppender] asserts on the
		// dynamic value, which panics twice over where encoding/json writes
		// `null`: once on a nil interface, whose `v.Interface()` is a nil `any`
		// that no single-value assertion accepts; and once on any ADDRESSABLE
		// interface field, nil or not, because `v.Addr()` has type *Appender and
		// a pointer to an interface implements nothing. Every element of a
		// slice is addressable, so `[]Appender` panicked on its first entry.
		//
		// No wire type has such a field today, which is why nothing was
		// observed. That is the argument for closing it rather than against: a
		// refusal list exists so the next type nobody has written yet is slow
		// rather than a panic on the response path.
		return nil
	}
	// A Marshaler reachable only through the pointer is encoded or not depending
	// on whether encoding/json found the value addressable. Reproducing that
	// rule exactly is not worth its risk; refusing is exact by construction.
	if t.Kind() != reflect.Pointer {
		if pt := reflect.PointerTo(t); !t.Implements(marshalerType) &&
			(pt.Implements(appenderType) || pt.Implements(marshalerType) || pt.Implements(textMarshalerType)) {
			return nil
		}
	}
	if t.Implements(appenderType) {
		return appendViaAppender
	}
	if t.Implements(textMarshalerType) {
		return nil
	}
	if t.Implements(marshalerType) {
		if t == rawMessageType {
			return appendRaw
		}
		return appendViaMarshaler
	}

	open[t] = true
	defer delete(open, t)

	switch t.Kind() {
	case reflect.Bool:
		return func(dst []byte, v reflect.Value) ([]byte, error) {
			return strconv.AppendBool(dst, v.Bool()), nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return func(dst []byte, v reflect.Value) ([]byte, error) {
			return strconv.AppendInt(dst, v.Int(), 10), nil
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return func(dst []byte, v reflect.Value) ([]byte, error) {
			return strconv.AppendUint(dst, v.Uint(), 10), nil
		}
	case reflect.Float32:
		return func(dst []byte, v reflect.Value) ([]byte, error) { return appendFloat(dst, v, 32) }
	case reflect.Float64:
		return func(dst []byte, v reflect.Value) ([]byte, error) { return appendFloat(dst, v, 64) }
	case reflect.String:
		return func(dst []byte, v reflect.Value) ([]byte, error) {
			return AppendString(dst, v.String()), nil
		}
	case reflect.Pointer:
		elem := plan(t.Elem(), open)
		if elem == nil {
			return nil
		}
		return func(dst []byte, v reflect.Value) ([]byte, error) {
			if v.IsNil() {
				return append(dst, "null"...), nil
			}
			return elem(dst, v.Elem())
		}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			// []byte and its named forms are base64 on this wire, and a named one
			// may or may not be depending on its methods. json.RawMessage is
			// already handled above; nothing else here is worth the risk.
			return nil
		}
		elem := plan(t.Elem(), open)
		if elem == nil {
			return nil
		}
		return func(dst []byte, v reflect.Value) ([]byte, error) {
			if v.IsNil() {
				return append(dst, "null"...), nil
			}
			return appendList(dst, v, elem)
		}
	case reflect.Array:
		elem := plan(t.Elem(), open)
		if elem == nil {
			return nil
		}
		return func(dst []byte, v reflect.Value) ([]byte, error) {
			return appendList(dst, v, elem)
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String || t.Key().Implements(textMarshalerType) {
			return nil
		}
		elem := plan(t.Elem(), open)
		if elem == nil {
			return nil
		}
		return func(dst []byte, v reflect.Value) ([]byte, error) {
			if v.IsNil() {
				return append(dst, "null"...), nil
			}
			return appendMap(dst, v, elem)
		}
	case reflect.Struct:
		return planStruct(t, open)
	default:
		return nil
	}
}

func appendViaAppender(dst []byte, v reflect.Value) ([]byte, error) {
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return append(dst, "null"...), nil
	}
	// Through the ADDRESS where there is one. Putting a struct value into an
	// interface copies it to the heap, and the elements of a []Message are
	// struct values: that is one allocation per message, per part and per tool,
	// for nothing. A pointer into an interface allocates nothing, and a value
	// receiver reached through a pointer is the same method on the same value.
	if v.Kind() != reflect.Pointer && v.CanAddr() {
		return v.Addr().Interface().(Appender).AppendJSON(dst)
	}
	return v.Interface().(Appender).AppendJSON(dst)
}

// appendRaw is [json.RawMessage]'s encoding: the bytes, compacted.
//
// encoding/json compacts a Marshaler's result and RawMessage is a Marshaler, so
// a member carrying the caller's own whitespace is folded flat on the way out
// and has been for as long as the goldens have existed. bytes.Buffer over dst
// makes that an append rather than a copy, and on a syntax error it writes
// nothing, which is compact's own rule.
func appendRaw(dst []byte, v reflect.Value) ([]byte, error) {
	if v.IsNil() {
		// NIL, not empty. json.RawMessage.MarshalJSON returns "null" only for a
		// nil value; a non-nil empty one returns no bytes at all and the compact
		// that follows reports "unexpected end of JSON input". Treating the two
		// the same would make this the one encoder that accepts a member with no
		// value, which the differential caught.
		return append(dst, "null"...), nil
	}
	raw := v.Bytes()
	buf := bytes.NewBuffer(dst)
	if err := json.Compact(buf, raw); err != nil {
		return dst, err
	}
	return buf.Bytes(), nil
}

func appendViaMarshaler(dst []byte, v reflect.Value) ([]byte, error) {
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return append(dst, "null"...), nil
	}
	delegated.Add(1)
	b, err := v.Interface().(json.Marshaler).MarshalJSON()
	if err != nil {
		return dst, &json.MarshalerError{Type: v.Type(), Err: err}
	}
	buf := bytes.NewBuffer(dst)
	if err := json.Compact(buf, b); err != nil {
		return dst, &json.MarshalerError{Type: v.Type(), Err: err}
	}
	return buf.Bytes(), nil
}

func appendList(dst []byte, v reflect.Value, elem appendFn) ([]byte, error) {
	dst = append(dst, '[')
	var err error
	for i, n := 0, v.Len(); i < n; i++ {
		if i > 0 {
			dst = append(dst, ',')
		}
		if dst, err = elem(dst, v.Index(i)); err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

// appendMap emits a map with its keys sorted, which is encoding/json's rule and
// the only reason a body carrying one is reproducible at all.
func appendMap(dst []byte, v reflect.Value, elem appendFn) ([]byte, error) {
	keys := v.MapKeys()
	slices.SortFunc(keys, func(a, b reflect.Value) int {
		return strings.Compare(a.String(), b.String())
	})
	dst = append(dst, '{')
	var err error
	for i, k := range keys {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = AppendString(dst, k.String())
		dst = append(dst, ':')
		if dst, err = elem(dst, v.MapIndex(k)); err != nil {
			return dst, err
		}
	}
	return append(dst, '}'), nil
}

// appendFloat is encoding/json's floatEncoder: ES6 number-to-string, with the
// exponent cutoffs and the e-09 fixup that make Go's output match what every
// other JSON generator prints.
func appendFloat(dst []byte, v reflect.Value, bits int) ([]byte, error) {
	f := v.Float()
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return dst, &json.UnsupportedValueError{Value: v, Str: strconv.FormatFloat(f, 'g', -1, bits)}
	}
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 {
		if bits == 64 && (abs < 1e-6 || abs >= 1e21) ||
			bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			format = 'e'
		}
	}
	dst = strconv.AppendFloat(dst, f, format, -1, bits)
	if format == 'e' {
		// clean up e-09 to e-9
		n := len(dst)
		if n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst, nil
}

// ---------------------------------------------------------------------------
// structs
// ---------------------------------------------------------------------------

type plannedField struct {
	// prefix is `"name":`, pre-quoted. encoding/json builds the same literal by
	// concatenation and writes it without escaping when HTML escaping is off, so
	// a name needing an escape would produce the same broken bytes there; the
	// planner refuses those instead.
	prefix    string
	index     int
	omitEmpty bool
	fn        appendFn
}

// planStruct mirrors encoding/json's typeFields for the shapes the wire types
// use, and refuses the ones they do not: an embedded field (whose members are
// promoted, with a conflict rule), any tag option other than omitempty, and two
// fields that resolve to the same JSON name.
func planStruct(t reflect.Type, open map[reflect.Type]bool) appendFn {
	var fields []plannedField
	// seen is the duplicate-name guard. encoding/json resolves a collision with
	// dominantField: at equal depth and equal tagged-ness it drops EVERY field
	// with that name, and where exactly one is tagged the tagged one wins. This
	// planner has no such rule and never did — it wrote each field in
	// declaration order, so `struct{A int "json:\"x\""; B int "json:\"x\""}`
	// produced `{"x":0,"x":0}` where encoding/json produces `{}`. That is a
	// different document, not a slower one, which is the one thing this file
	// promises cannot happen.
	//
	// Implementing dominantField instead of refusing was the alternative and is
	// the wrong trade: the rule is subtle, its inputs (depth, tagged-ness)
	// mostly matter for promoted fields this planner already refuses, and being
	// exact by construction is what the rest of the refusal list is worth.
	seen := make(map[string]bool, t.NumField())
	for i := range t.NumField() {
		sf := t.Field(i)
		if sf.Anonymous {
			return nil
		}
		if sf.PkgPath != "" {
			continue // unexported
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		omitEmpty := false
		for opts != "" {
			var o string
			o, opts, _ = strings.Cut(opts, ",")
			if o != "omitempty" {
				return nil // ",string", ",omitzero", anything new
			}
			omitEmpty = true
		}
		if name == "" {
			name = sf.Name
		}
		if !plainName(name) {
			return nil
		}
		if seen[name] {
			return nil
		}
		seen[name] = true
		fn := plan(sf.Type, open)
		if fn == nil {
			return nil
		}
		fields = append(fields, plannedField{
			prefix:    `"` + name + `":`,
			index:     i,
			omitEmpty: omitEmpty,
			fn:        fn,
		})
	}
	return func(dst []byte, v reflect.Value) ([]byte, error) {
		next := byte('{')
		var err error
		for i := range fields {
			f := &fields[i]
			fv := v.Field(f.index)
			if f.omitEmpty && isEmptyValue(fv) {
				continue
			}
			dst = append(dst, next)
			next = ','
			dst = append(dst, f.prefix...)
			if dst, err = f.fn(dst, fv); err != nil {
				return dst, err
			}
		}
		if next == '{' {
			return append(dst, '{', '}'), nil
		}
		return append(dst, '}'), nil
	}
}

// plainName reports whether a member name is its own JSON literal.
func plainName(s string) bool {
	for i := range len(s) {
		if c := s[i]; c < 0x20 || c >= utf8.RuneSelf || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// isEmptyValue is encoding/json's, verbatim. omitempty's meaning is part of the
// byte contract — it is what keeps "finish_reason":null off an in-flight chunk —
// so it is copied rather than approximated.
func isEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}

// ---------------------------------------------------------------------------
// strings
// ---------------------------------------------------------------------------

const hexDigits = "0123456789abcdef"

// AppendString appends s as a JSON string literal with HTML escaping OFF.
//
// It is encoding/json's appendString with escapeHTML false, which is the
// serializer COMPATIBILITY 2.1a names: '<', '>' and '&' stay as themselves,
// non-ASCII stays raw UTF-8, invalid UTF-8 becomes U+FFFD, and U+2028/U+2029 are
// escaped unconditionally because they are not valid JavaScript source.
func AppendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch b {
			case '\\', '"':
				dst = append(dst, '\\', b)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}
		n := min(len(s)-i, utf8.UTFMax)
		c, size := utf8.DecodeRuneInString(s[i : i+n])
		if c == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, "\\ufffd"...)
			i += size
			start = i
			continue
		}
		if c == '\u2028' || c == '\u2029' {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}
