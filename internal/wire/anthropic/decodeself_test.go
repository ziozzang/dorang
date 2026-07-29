package anthropic

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// The messages path's half of openai/decodeself_test.go. [decodeSelf] replaced
// json.Unmarshal at the top of this package's three decoders, and the claim that
// the two are the same function is checked here rather than asserted.
//
// The pairs are enumerated rather than discovered so that adding a decodeSelf
// call site without adding it here is a visible omission and not a silent one.

var selfDecoders = []struct {
	name  string
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

var selfCorpus = []string{
	`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"context_management":{"a":1}}`,
	`{"model":"m","max_tokens":16,"messages":[],"system":"be brief","tools":[{"name":"t","input_schema":{"type":"object"}}]}`,
	`{"Model":"m","max_tokens":16,"messages":[]}`,
	`{"model":"m","max_tokens":16,"metadata":{"user_id":"u"},"stream":true}`,
	`{"id":"m1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2}}`,
	`{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"Usage":{"input_tokens":1},"vendor":{"z":1}}`,

	// The two documented near-differences.
	`null`,
	`  {"model":"m"}  `,
	"\n\t{\"model\":\"m\"}\n",

	// Not an object.
	`[]`,
	`"s"`,
	`5`,
	`true`,

	// Malformed. The last is the case the fuzzer found in the openai package:
	// the strict filter excises a case-colliding member, so a syntax error
	// inside it is only caught if the document is validated first.
	``,
	` `,
	`{`,
	`{}}`,
	`{"model":"m"} trailing`,
	`{"model":"m",}`,
	`{"model":tru}`,
	`{"model":"m"`,
	`{"Model":A}`,
	`{"MODEL":[1,2,}`,
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
	mb, mErr := Marshal(methodVal)
	ub, uErr := Marshal(unmarshalVal)
	if (mErr == nil) != (uErr == nil) || string(mb) != string(ub) {
		t.Fatalf("%s on %q: re-encodes differently\n  method %s (%v)\n  json   %s (%v)",
			name, b, mb, mErr, ub, uErr)
	}
}

func FuzzDirectDecode(f *testing.F) {
	for _, in := range selfCorpus {
		f.Add([]byte(in))
	}
	for _, key := range []string{"model", "Model", "MODEL", "system", "Syſtem", "messages", "usage", "Usage", "max_tokens"} {
		for _, val := range []string{`"m"`, `true`, `null`, `[]`, `{}`, `16`, `[{"role":"user","content":"hi"}]`} {
			f.Add([]byte(fmt.Sprintf(`{%q:%s,"model":"base","max_tokens":8}`, key, val)))
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, d := range selfDecoders {
			checkDirectDecode(t, d.name, d.fresh, b)
		}
	})
}
