package openai

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// [decodeSelf] replaced json.Unmarshal at the top of the two decoders on the
// measured path, on the argument that json.Unmarshal's only contribution there
// was to walk the body twice and then call the method that walks it a third
// time. That argument is worth exactly as much as the evidence for it, so this
// file runs both forms over the same bytes and requires the same answer —
// including the same error text, because a decode error reaches a client as a
// 400 body.
//
// The pairs are enumerated rather than discovered so that adding a decodeSelf
// call site without adding it here is a visible omission and not a silent one.

// selfDecoders is every type decodeSelf is applied to in this package.
var selfDecoders = []struct {
	name string
	// fresh returns a zero value of the type, twice, so the two forms cannot
	// share state.
	fresh func() (json.Unmarshaler, any)
}{
	{"Request", func() (json.Unmarshaler, any) {
		var v Request
		return &v, &v
	}},
	{"Response", func() (json.Unmarshaler, any) {
		var v Response
		return &v, &v
	}},
}

// selfCorpus is the shapes a decoder actually meets, plus the ones where the two
// forms could differ: a bare null, whitespace, trailing bytes, a non-object, and
// documents that are invalid in each of the places a walk might stop early.
var selfCorpus = []string{
	`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"vendor_x":{"a":1}}`,
	`{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":true}}`,
	`{"Model":"m","messages":[]}`,
	`{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`,
	`{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	`{"id":"c","object":"chat.completion","choices":[],"Usage":{"prompt_tokens":1},"timings":{"ms":2}}`,

	// The two documented near-differences.
	`null`,
	`  {"model":"m"}  `,
	"\n\t{\"model\":\"m\"}\n",

	// Shapes that are not an object at all.
	`[]`,
	`"s"`,
	`5`,
	`true`,
	`[{"model":"m"}]`,

	// Malformed, including trailing data, which is the case json.Unmarshal
	// rejects in checkValid and the method has to reject on its own.
	``,
	` `,
	`{`,
	`{}}`,
	`{"model":"m"} trailing`,
	`{"model":"m"}{"model":"n"}`,
	`{"model":"m",}`,
	`{"model":tru}`,
	`{"messages":[{"role":"user","content":}]}`,
	`{"model":01}`,
	`nul`,
	`{"model":"m"`,
}

func TestDirectDecodeMatchesUnmarshal(t *testing.T) {
	for _, d := range selfDecoders {
		for _, in := range selfCorpus {
			t.Run(fmt.Sprintf("%s/%q", d.name, in), func(t *testing.T) {
				checkDirectDecode(t, d.name, d.fresh, []byte(in))
			})
		}
	}
}

// checkDirectDecode is the property: for these types, on these bytes,
// json.Unmarshal and the method are the same function.
func checkDirectDecode(t *testing.T, name string, fresh func() (json.Unmarshaler, any), b []byte) {
	t.Helper()

	viaMethod, methodVal := fresh()
	errMethod := decodeSelf(b, viaMethod)

	_, unmarshalVal := fresh()
	errUnmarshal := json.Unmarshal(b, unmarshalVal)

	if (errMethod == nil) != (errUnmarshal == nil) {
		t.Fatalf("%s on %q: method err %v, json.Unmarshal err %v", name, b, errMethod, errUnmarshal)
	}
	if errMethod != nil {
		if errMethod.Error() != errUnmarshal.Error() {
			t.Fatalf("%s on %q: different error text\n  method %q\n  json   %q",
				name, b, errMethod, errUnmarshal)
		}
		return
	}
	if !reflect.DeepEqual(methodVal, unmarshalVal) {
		t.Fatalf("%s on %q: different values\n  method %+v\n  json   %+v",
			name, b, methodVal, unmarshalVal)
	}
	// The value is what a client eventually receives, so compare the bytes it
	// would be re-serialized as too. DeepEqual on a struct holding
	// json.RawMessage would pass on two maps that render differently.
	mb, mErr := Marshal(methodVal)
	ub, uErr := Marshal(unmarshalVal)
	if (mErr == nil) != (uErr == nil) || string(mb) != string(ub) {
		t.Fatalf("%s on %q: re-encodes differently\n  method %s (%v)\n  json   %s (%v)",
			name, b, mb, mErr, ub, uErr)
	}
}

// FuzzDirectDecode is [TestDirectDecodeMatchesUnmarshal] with the corpus
// generated. The seeds are real bodies, because the inputs that separate the two
// forms are one byte away from a valid one.
func FuzzDirectDecode(f *testing.F) {
	for _, in := range selfCorpus {
		f.Add([]byte(in))
	}
	for _, in := range strictCorpus() {
		f.Add([]byte(in))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, d := range selfDecoders {
			checkDirectDecode(t, d.name, d.fresh, b)
		}
	})
}

// strictCorpus is a handful of bodies built around the members that carry the
// case-sensitivity rule, so the mutator starts from documents where a flipped
// case bit means something.
func strictCorpus() []string {
	var out []string
	for _, key := range []string{"model", "Model", "MODEL", `model`, "stream", "ſtream", "messages", "usage", "Usage"} {
		for _, val := range []string{`"m"`, `true`, `null`, `[]`, `{}`, `[{"role":"user","content":"hi"}]`, `{"prompt_tokens":1}`} {
			out = append(out, fmt.Sprintf(`{%q:%s,"model":"base"}`, key, val))
		}
	}
	out = append(out, `{"model":"m","messages":[`+strings.Repeat(`{"role":"user","content":"x"},`, 3)+`{"role":"user","content":"y"}]}`)
	return out
}
