package wirejson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// SplitExtra used to be a second json.Unmarshal of the same bytes, and its
// replacement is a hand-written walk. The only thing that makes that trade
// acceptable is being able to show the two produce the same answer, so these
// tests are written as a differential against the implementation that was
// removed — [splitExtraSlow], kept in the package for exactly this.
//
// A test that asserted the walk's own expectations instead would be a test of
// the walk's author, and this is the file where an optimization that is subtly
// wrong is supposed to be caught.

var testKnown = KnownKeys("model", "messages", "stream", "max_tokens", "usage", "id")

// cases is every shape that has ever mattered here, plus the ones that must keep
// not mattering.
var cases = []string{
	// Ordinary.
	`{}`,
	`{"model":"m"}`,
	`{"model":"m","x":1}`,
	`{"x":1,"y":"two","z":[1,2,3],"w":{"a":{"b":null}}}`,
	`{"messages":[{"role":"user","content":"hi"}],"model":"m","timings":{"ms":1.5}}`,

	// Not an object, and null. Both go to encoding/json.
	`null`,
	`[]`,
	`"s"`,
	`5`,
	`true`,
	``,

	// Whitespace in every position it is legal.
	"  {  \"model\" : \"m\" , \"x\" : [ 1 , 2 ] }  ",
	"{\n\t\"x\"\r:\n1\n}",

	// Duplicate keys. encoding/json's map takes the last, including a null.
	`{"x":1,"x":2}`,
	`{"x":1,"x":null}`,
	`{"model":"a","model":"b"}`,
	`{"x":null,"x":1}`,

	// Escaped keys. "\u006dodel" IS "model" and must be recognized as modelled;
	// an escaped unknown key must land in the map under its DECODED spelling.
	`{"\u006dodel":"m"}`,
	`{"\u0078":1}`,
	`{"a\u0062c":1}`,
	`{"a\"b":1}`,
	`{"a\\b":1}`,
	`{"\ud83d\ude00":1}`,

	// Keys that are not what they look like: control bytes, invalid UTF-8, an
	// empty name. encoding/json coerces bad UTF-8 to U+FFFD and the map key it
	// produces is what MarshalWithExtra re-emits, so the two must agree.
	`{"":1}`,
	"{\"a\\u0000b\":1}",
	"{\"\x80\":1}",
	"{\"\xff\xfe\":1}",
	"{\"caf\u00e9\":1}",

	// Case. Exact for SplitExtra, folded for SplitExtraFold.
	`{"Model":"m"}`,
	`{"MODEL":"m","model":"n"}`,
	`{"Usage":{"prompt_tokens":1}}`,
	`{"ſtream":true}`,
	`{"MAX_TOKENS":1,"max_Tokens":2}`,

	// Values whose extent is easy to get wrong.
	`{"x":-1.5e10}`,
	`{"x":"a\"b"}`,
	`{"x":"}"}`,
	`{"x":"{\"y\":1}"}`,
	`{"x":[{"y":"]"}]}`,
	`{"x":""}`,

	// Malformed, in every way the walk might be tempted to tolerate.
	`{`,
	`{"x"}`,
	`{"x":}`,
	`{"x":1,}`,
	`{"x":1}}`,
	`{"x":1} x`,
	`{"x":tru}`,
	`{"model":tru}`,
	`{"x":[1,2,}`,
	`{"x":01}`,
	`{x:1}`,
	`{'x':1}`,
	"{\"x\":\"\n\"}",
	`{"x":1`,
}

func TestSplitExtraAgreesWithUnmarshal(t *testing.T) {
	for _, in := range cases {
		for _, fold := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/fold=%v", in, fold), func(t *testing.T) {
				checkSplitAgreement(t, []byte(in), testKnown, fold)
				checkSplitAgreement(t, []byte(in), emptyKnown, fold)
			})
		}
	}
}

// checkSplitAgreement is the property, stated once.
//
// Three claims, and the third is the one that makes the second precise:
//
//  1. On any input encoding/json ACCEPTS, the walk and the reference return the
//     same members with the same bytes and the same error. That is the whole
//     contract, because the precondition says nothing else ever arrives.
//
//  2. On an input encoding/json rejects, the walk may skip past the defect when
//     it is inside a MODELLED member, whose value it never looks at. It may not
//     report an error the reference does not, and it may not keep a member whose
//     bytes no parser will read — those go back to the next hop verbatim.
//
//  3. With an EMPTY known set nothing is modelled, so claim 2's exemption has
//     nowhere to apply and the two must agree on every input, valid or not.
//     That is what confines the gap to modelled members instead of leaving it as
//     a general licence to be lenient, and it is checked on every fuzz
//     execution.
func checkSplitAgreement(t *testing.T, b []byte, known map[string]struct{}, fold bool) {
	t.Helper()
	split := SplitExtra
	if fold {
		split = SplitExtraFold
	}
	got, gotErr := split(b, known)
	want, wantErr := splitExtraSlow(b, known, fold)

	// Claim 2's floor, checked on every input including the accepted ones.
	for k, v := range got {
		if !json.Valid(v) {
			t.Fatalf("walk kept an unparseable value for %q from %q: %s", k, b, v)
		}
	}
	if gotErr != nil && wantErr == nil {
		t.Fatalf("walk reported %v on %q where the reference succeeded", gotErr, b)
	}

	if !json.Valid(b) && len(known) > 0 {
		return
	}
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("error disagreement on %q: walk %v, reference %v", b, gotErr, wantErr)
	}
	if gotErr != nil && gotErr.Error() != wantErr.Error() {
		t.Fatalf("different errors on %q: walk %q, reference %q", b, gotErr, wantErr)
	}
	if !sameExtra(got, want) {
		t.Fatalf("different members on %q:\n  walk      %s\n  reference %s",
			b, showExtra(got), showExtra(want))
	}
}

// emptyKnown is claim 3's set: nothing is modelled, so nothing may be skipped.
var emptyKnown = KnownKeys()

func sameExtra(a, b map[string]json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !bytes.Equal(av, bv) {
			return false
		}
	}
	// A nil map and an empty one are different answers: MarshalWithExtra treats
	// both as "nothing to splice", but a caller storing it does not.
	return (a == nil) == (b == nil)
}

func showExtra(m map[string]json.RawMessage) string {
	if m == nil {
		return "<nil>"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%q: %s", k, m[k])
	}
	sb.WriteByte('}')
	return sb.String()
}

// TestSplitExtraValuesDoNotAliasTheBody is the one thing the walk could get
// right on every differential and still be wrong about.
//
// An Extra map outlives the bytes it came from — it is carried through a
// conversion and spliced into the request sent to the backend — so a member
// held as a sub-slice of the caller's buffer is a member that changes when that
// buffer is reused.
func TestSplitExtraValuesDoNotAliasTheBody(t *testing.T) {
	body := []byte(`{"model":"m","vendor":{"keep":"this"}}`)
	extra, err := SplitExtra(body, testKnown)
	if err != nil {
		t.Fatal(err)
	}
	kept := extra["vendor"]
	if len(kept) == 0 {
		t.Fatal("vendor was not kept")
	}
	before := string(kept)
	for i := range body {
		body[i] = 'X'
	}
	if string(kept) != before {
		t.Fatalf("the kept member aliased the body: %q became %q", before, kept)
	}
}

// TestSplitExtraFoldStaysOutOfTheRequestPath restates the security property in
// the only place both halves are visible at once. A folded split on a request
// would readmit the key COMPATIBILITY 2.0's filter just removed.
func TestSplitExtraFoldStaysOutOfTheRequestPath(t *testing.T) {
	body := []byte(`{"model":"m","Model":"other"}`)
	strict, err := SplitExtra(body, testKnown)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := strict["Model"]; !ok {
		t.Fatal("SplitExtra folded a cased key; the strict split must not")
	}
	folded, err := SplitExtraFold(body, testKnown)
	if err != nil {
		t.Fatal(err)
	}
	if len(folded) != 0 {
		t.Fatalf("SplitExtraFold kept %s; the folded split must drop a cased duplicate",
			showExtra(folded))
	}
}

// -----------------------------------------------------------------------------
// Fuzzing
// -----------------------------------------------------------------------------

// fuzzKeys and fuzzValues follow internal/canonical's gate-agreement fuzzer: the
// interesting inputs are one byte away from a real body, so the mutator is given
// the pieces rather than asked to rediscover that an object starts with '{'.
var fuzzKeys = []string{
	`"model"`, `"Model"`, `"MODEL"`, `"moDel"`, `"messages"`, `"Messages"`,
	`"usage"`, `"Usage"`, `"stream"`, `"ſtream"`, `"max_tokens"`, `"id"`,
	`"x"`, `"X"`, `""`, `"\u006dodel"`, `"a\"b"`, `"a\\b"`, `"\u0000"`,
	"\"\x80\"", `"café"`, `"timings"`, `"provider_specific_fields"`,
}

var fuzzValues = []string{
	`null`, `true`, `false`, `1`, `-1.5e10`, `0`, `""`, `"s"`, `"}"`,
	`"a\"b"`, `"\ud800"`, "\"\x90\"", `[]`, `{}`, `[1,2,3]`,
	`{"nested":{"deep":[true,null]}}`, `{"model":"inner"}`,
	`[{"role":"user","content":"hi"}]`, `tru`, `01`, `[1,2,`,
}

func synthObject(seed []byte) []byte {
	b := []byte{'{'}
	for i := 0; i+1 < len(seed) && i < 32; i += 2 {
		if len(b) > 1 {
			b = append(b, ',')
		}
		b = append(b, fuzzKeys[int(seed[i])%len(fuzzKeys)]...)
		b = append(b, ':')
		b = append(b, fuzzValues[int(seed[i+1])%len(fuzzValues)]...)
	}
	return append(b, '}')
}

// FuzzSplitExtra is [TestSplitExtraAgreesWithUnmarshal] with the corpus
// generated instead of written down.
func FuzzSplitExtra(f *testing.F) {
	for _, c := range cases {
		f.Add([]byte(c))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, fold := range []bool{false, true} {
			checkSplitAgreement(t, b, testKnown, fold)
			checkSplitAgreement(t, synthObject(b), testKnown, fold)
			// Claim 3: with nothing modelled the two must agree on these same
			// bytes even when encoding/json rejects them.
			checkSplitAgreement(t, b, emptyKnown, fold)
			checkSplitAgreement(t, synthObject(b), emptyKnown, fold)
		}
	})
}

// -----------------------------------------------------------------------------
// The reason any of this was done
// -----------------------------------------------------------------------------

func benchSplitBody(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"bench-model","stream":false,"max_tokens":256,"messages":[`)
	filler := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 8)
	for i := 0; b.Len() < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"role":"user","content":"%s %d"}`, filler, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// BenchmarkSplitExtra is the walk against the second json.Unmarshal it replaced,
// on a body shaped like a chat request: one large modelled member that the old
// implementation copied out and then deleted.
func BenchmarkSplitExtra(b *testing.B) {
	for _, size := range []int{256, 1 << 10, 4 << 10, 16 << 10} {
		body := benchSplitBody(size)
		b.Run(fmt.Sprintf("walk/%dB", len(body)), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := SplitExtra(body, testKnown); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("unmarshal/%dB", len(body)), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := splitExtraSlow(body, testKnown, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
