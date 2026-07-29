package wirejson

import (
	"bytes"
	"encoding"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// The differential that has to exist before the appender is trusted anywhere.
//
// The claim being tested is not "the appender is correct" — it is "the appender
// and encoding/json produce the same bytes", which is stronger and is the only
// form of the claim COMPATIBILITY 2.1a can be checked against. Everything below
// therefore compares [Append] against [Marshal] rather than against a literal.

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%#v): %v", v, err)
	}
	return b
}

// agree is the whole differential in one function: the two encoders, the same
// value, the same bytes — or the same error.
func agree(t *testing.T, v any) {
	t.Helper()
	want, wantErr := Marshal(v)
	got, gotErr := Append([]byte("PRE"), v)
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("%#v: Marshal err=%v, Append err=%v", v, wantErr, gotErr)
	}
	if wantErr != nil {
		return
	}
	if !bytes.HasPrefix(got, []byte("PRE")) {
		t.Fatalf("%#v: Append clobbered the caller's bytes: %q", v, got)
	}
	if string(got[3:]) != string(want) {
		t.Fatalf("%#v:\n  Marshal %s\n  Append  %s", v, want, got[3:])
	}
}

// ---------------------------------------------------------------------------
// the shapes the wire types are made of
// ---------------------------------------------------------------------------

type probeInner struct {
	A string  `json:"a"`
	B *int    `json:"b,omitempty"`
	C float64 `json:"c"`
}

type probe struct {
	Str        string             `json:"str"`
	StrOmit    string             `json:"str_omit,omitempty"`
	Int        int                `json:"int"`
	Int64      int64              `json:"i64,omitempty"`
	Uint       uint16             `json:"u16"`
	Bool       bool               `json:"bool,omitempty"`
	F64        float64            `json:"f64"`
	F32        float32            `json:"f32,omitempty"`
	PtrStr     *string            `json:"pstr,omitempty"`
	PtrBool    *bool              `json:"pbool,omitempty"`
	PtrF       *float64           `json:"pf,omitempty"`
	Inner      probeInner         `json:"inner"`
	PtrInner   *probeInner        `json:"pinner,omitempty"`
	Slice      []probeInner       `json:"slice,omitempty"`
	Strings    []string           `json:"strings,omitempty"`
	MapStr     map[string]string  `json:"mstr,omitempty"`
	MapF       map[string]float64 `json:"mf,omitempty"`
	MapRaw     map[string]json.RawMessage
	Raw        json.RawMessage `json:"raw,omitempty"`
	Skipped    string          `json:"-"`
	unexported string          //nolint:unused // present so the planner has to skip it
	NoTag      int
}

func TestAppendAgreesOnEveryShape(t *testing.T) {
	i := 7
	s := "ptr"
	b := true
	f := 1.5
	cases := []any{
		probe{},
		probe{Str: "x", Int: -3, Uint: 65535, Bool: true, F64: 0.1, F32: 2.5,
			PtrStr: &s, PtrBool: &b, PtrF: &f,
			Inner:    probeInner{A: "in", B: &i, C: 1e21},
			PtrInner: &probeInner{A: "p", C: -1e-7},
			Slice:    []probeInner{{A: "1"}, {A: "2", B: &i}},
			Strings:  []string{"a", "b"},
			MapStr:   map[string]string{"z": "1", "a": "2", "": "3"},
			MapF:     map[string]float64{"k": 3.25},
			MapRaw:   map[string]json.RawMessage{"r": json.RawMessage(`  {"x" :  1}  `)},
			Raw:      json.RawMessage("[1,\n2]"),
			Skipped:  "never", NoTag: 9},
		&probe{Str: "through a pointer"},
		[]probeInner{{A: "a"}},
		map[string]probeInner{"k": {A: "v"}},
		"a && b <tag> \u2028\u2029 \x00\x1f\x7f é 😀",
		3.14159, float32(0.5), -0.0, 1e-6, 1e-7, 1e21, 1e20, 123456789.0,
		true, 42, uint64(1 << 63), []string(nil), map[string]string(nil),
		json.RawMessage(` { "a" : [ 1 , 2 ] } `),
	}
	for _, c := range cases {
		agree(t, c)
	}
}

// TestAppendRejectsWhatItDoesNotModel is the safety property: a shape the
// planner does not cover must produce NO plan, so the value falls back to
// encoding/json rather than being encoded by an approximation of it.
func TestAppendRejectsWhatItDoesNotModel(t *testing.T) {
	type embedded struct{ probeInner }
	type quoted struct {
		N int `json:"n,string"`
	}
	type omitzero struct {
		N int `json:"n,omitzero"`
	}
	type number struct {
		N json.Number `json:"n"`
	}
	type iface struct {
		V any `json:"v"`
	}
	type textual struct {
		T textMarshaler `json:"t"`
	}
	type recursive struct {
		Next *recursive `json:"next,omitempty"`
	}
	type bytesField struct {
		B []byte `json:"b"`
	}
	type badName struct {
		X int `json:"a\"b"`
	}
	for _, v := range []any{
		embedded{}, quoted{}, omitzero{}, number{}, iface{}, textual{},
		recursive{}, bytesField{}, badName{},
	} {
		if fn := planOf(reflect.TypeOf(v)); fn != nil {
			t.Errorf("%T was planned; it must fall back to encoding/json", v)
		}
		// Falling back is not an excuse for being wrong.
		agree(t, v)
	}
}

type textMarshaler struct{}

func (textMarshaler) MarshalText() ([]byte, error) { return []byte("t"), nil }

var _ encoding.TextMarshaler = textMarshaler{}

// ptrOnlyMarshaler is a Marshaler only through its pointer, which encoding/json
// honours or not depending on addressability. The planner refuses it.
type ptrOnlyMarshaler struct{ N int }

func (p *ptrOnlyMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"ptr"`), nil }

func TestAppendRefusesPointerOnlyMarshaler(t *testing.T) {
	type holder struct {
		P ptrOnlyMarshaler `json:"p"`
	}
	if planOf(reflect.TypeOf(holder{})) != nil {
		t.Fatal("a pointer-only Marshaler was planned")
	}
	agree(t, holder{})
	agree(t, &holder{})
}

// TestAppendErrorsWhereMarshalErrors pins the failure cases, which are part of
// the contract too: a NaN is not silently emitted as something.
func TestAppendErrorsWhereMarshalErrors(t *testing.T) {
	for _, v := range []any{
		math.NaN(), math.Inf(1), math.Inf(-1),
		probe{F64: math.NaN()},
		probe{Raw: json.RawMessage("{")},
		// A non-nil EMPTY RawMessage is a member with no value, and
		// encoding/json refuses it. Nil is "null" and empty is an error, and the
		// two are one byte apart in the struct that holds them.
		probe{MapRaw: map[string]json.RawMessage{"k": {}}},
		map[string]float64{"k": math.Inf(1)},
	} {
		if _, err := Append(nil, v); err == nil {
			t.Errorf("%v: Append accepted what Marshal rejects", v)
		}
		agree(t, v)
	}
}

// ---------------------------------------------------------------------------
// Extra
// ---------------------------------------------------------------------------

// spliced is MarshalWithExtra's result as its CALLER sees it.
//
// MarshalWithExtra splices an unmodelled member's bytes verbatim, whitespace
// and all — and every one of its callers is a MarshalJSON, whose result
// encoding/json then compacts on the way into whatever holds it. So the bytes
// that reach the wire are the compacted ones, and those are what
// AppendWithExtra has to match. Comparing against the uncompacted intermediate
// would be comparing against a form that has never been on a wire.
func spliced(t *testing.T, b []byte, err error) []byte {
	t.Helper()
	if err != nil {
		return nil
	}
	var buf bytes.Buffer
	if cerr := json.Compact(&buf, b); cerr != nil {
		t.Fatalf("MarshalWithExtra produced invalid JSON: %v (%s)", cerr, b)
	}
	return buf.Bytes()
}

func TestAppendWithExtraMatchesMarshalWithExtra(t *testing.T) {
	known := KnownKeys("str", "int")
	for _, extra := range []map[string]json.RawMessage{
		nil,
		{},
		{"z": json.RawMessage(`1`), "a": json.RawMessage(`{"n":2}`)},
		{"str": json.RawMessage(`"collides and is dropped"`)},
		{"str": json.RawMessage(`1`), "kept": json.RawMessage(`true`)},
		{"\"quoted\"": json.RawMessage(`null`), "\u2028": json.RawMessage(`0`)},
	} {
		raw, wantErr := MarshalWithExtra(probe{Str: "s", Int: 1}, extra, known)
		want := spliced(t, raw, wantErr)
		got, gotErr := AppendWithExtra(nil, probe{Str: "s", Int: 1}, extra, known)
		if (wantErr == nil) != (gotErr == nil) {
			t.Fatalf("%v: errors differ: %v vs %v", extra, wantErr, gotErr)
		}
		if wantErr == nil && string(want) != string(got) {
			t.Fatalf("%v:\n  MarshalWithExtra %s\n  AppendWithExtra  %s", extra, want, got)
		}
	}
}

// TestAppendWithExtraRefusesNonObject keeps the splice from producing bytes no
// parser reads: an Extra map on a value that is not an object is an error, not
// a concatenation.
func TestAppendWithExtraRefusesNonObject(t *testing.T) {
	extra := map[string]json.RawMessage{"a": json.RawMessage(`1`)}
	if _, err := AppendWithExtra(nil, []int{1}, extra, nil); err == nil {
		t.Fatal("spliced into an array")
	}
	if _, err := MarshalWithExtra([]int{1}, extra, nil); err == nil {
		t.Fatal("MarshalWithExtra spliced into an array")
	}
}

// ---------------------------------------------------------------------------
// fuzz
// ---------------------------------------------------------------------------

// FuzzAppendString is the escaping differential.
//
// It is separate from the struct one because it is the part with a stated
// contract: 2.1a says '<', '>' and '&' stay as themselves and non-ASCII stays
// raw, and an appender that quietly reintroduced Go's HTML escaping would
// produce bytes no other server produces while every structural test still
// passed.
func FuzzAppendString(f *testing.F) {
	for _, s := range []string{
		"", "a && b", "<tag>", "\u2028\u2029", "\x00\x01\x1f\x7f",
		"\"\\/\b\f\n\r\t", "é😀", "\xff\xfe", "a\xffb", "\xed\xa0\x80",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		want := mustMarshal(t, s)
		got := AppendString([]byte("PRE"), s)
		if string(got[3:]) != string(want) {
			t.Fatalf("%q:\n  Marshal %s\n  Append  %s", s, want, got[3:])
		}
	})
}

// FuzzAppendAgreesWithMarshal is the structural differential: an arbitrary
// value of a struct carrying every shape the wire types use, encoded both ways.
func FuzzAppendAgreesWithMarshal(f *testing.F) {
	f.Add("", int64(0), 0.0, false, "", "")
	f.Add("a && b <x>", int64(-1), 1e-7, true, `{"k":  1}`, "key")
	f.Add("\xff", int64(1<<40), 1e21, false, `[ 1 , 2 ]`, "\u2028")
	f.Add("é", int64(-5), math.SmallestNonzeroFloat64, true, `null`, "")
	f.Fuzz(func(t *testing.T, s string, n int64, x float64, b bool, raw, key string) {
		v := probe{
			Str: s, StrOmit: s, Int: int(n), Int64: n, Uint: uint16(n), Bool: b,
			F64: x, F32: float32(x),
			PtrStr: &s, PtrBool: &b, PtrF: &x,
			Inner:    probeInner{A: s, B: ptrTo(int(n)), C: x},
			PtrInner: &probeInner{A: key, C: -x},
			Slice:    []probeInner{{A: s, C: x}, {A: key}},
			Strings:  []string{s, key},
			MapStr:   map[string]string{key: s, s: key, "": s},
			MapF:     map[string]float64{key: x},
			NoTag:    int(n),
		}
		if json.Valid([]byte(raw)) {
			v.Raw = json.RawMessage(raw)
			v.MapRaw = map[string]json.RawMessage{key: json.RawMessage(raw)}
		}
		agree(t, v)
		agree(t, &v)

		extra := map[string]json.RawMessage{}
		if json.Valid([]byte(raw)) {
			extra[key] = json.RawMessage(raw)
			extra[s] = json.RawMessage(raw)
		}
		known := KnownKeys("str", "int", key)
		wantRaw, wantErr := MarshalWithExtra(v, extra, known)
		want := spliced(t, wantRaw, wantErr)
		got, gotErr := AppendWithExtra(nil, v, extra, known)
		if (wantErr == nil) != (gotErr == nil) {
			t.Fatalf("extra errors differ: %v vs %v", wantErr, gotErr)
		}
		if wantErr == nil && string(want) != string(got) {
			t.Fatalf("extra:\n  MarshalWithExtra %s\n  AppendWithExtra  %s", want, got)
		}
	})
}

func ptrTo[T any](v T) *T { return &v }

// TestMarshalAppenderMatchesMarshal pins the shim every wire type's MarshalJSON
// becomes.
func TestMarshalAppenderMatchesMarshal(t *testing.T) {
	a := appenderProbe{S: strings.Repeat("x", 4096)}
	got, err := MarshalAppender(a)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustMarshal(t, appenderProbeAlias(a)); string(got) != string(want) {
		t.Fatalf("MarshalAppender %s, want %s", got, want)
	}
	// The pooled buffer must not be visible in the result: a second render has
	// to leave the first one alone.
	first, _ := MarshalAppender(appenderProbe{S: "one"})
	second, _ := MarshalAppender(appenderProbe{S: "two"})
	if string(first) != `{"s":"one"}` || string(second) != `{"s":"two"}` {
		t.Fatalf("pooled buffer leaked between renders: %s / %s", first, second)
	}
}

type appenderProbe struct {
	S string `json:"s"`
}

type appenderProbeAlias appenderProbe

func (a appenderProbe) AppendJSON(dst []byte) ([]byte, error) {
	return Append(dst, appenderProbeAlias(a))
}

// TestAppenderFieldIsWrittenInPlace is the property the whole file exists for:
// a nested Appender must be written into the parent's buffer, not marshalled
// and compacted into it. It is observable — the nested type's MarshalJSON is
// never called.
func TestAppenderFieldIsWrittenInPlace(t *testing.T) {
	type holder struct {
		A countingAppender `json:"a"`
	}
	var h holder
	if _, err := Append(nil, h); err != nil {
		t.Fatal(err)
	}
	if h.A.marshalled != nil {
		t.Fatal("planner used the value's Marshaler")
	}
	got, err := Append(nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":{"n":0}}` {
		t.Fatalf("got %s", got)
	}
	if marshalCalls != 0 {
		t.Fatalf("MarshalJSON was called %d times on a type with AppendJSON", marshalCalls)
	}
}

var marshalCalls int

type countingAppender struct {
	N          int `json:"n"`
	marshalled []byte
}

type countingAlias struct {
	N int `json:"n"`
}

func (c countingAppender) AppendJSON(dst []byte) ([]byte, error) {
	return Append(dst, countingAlias{N: c.N})
}

func (c countingAppender) MarshalJSON() ([]byte, error) {
	marshalCalls++
	return MarshalAppender(c)
}
