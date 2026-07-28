// Differential test: the authorization gate and every wire adapter must
// resolve the SAME model from the same bytes.
//
// DESIGN §18 W10 / COMPATIBILITY 2.0. The gate
// (internal/server/peek.go) decides whether an API key is allowed to use the
// model a body names; the adapter decides which model is actually dispatched.
// If those two disagree, the allow-list is checked against a name that is not
// the name that gets used, which is an authorization bypass and not a
// formatting inconsistency. This file feeds one corpus to both and asserts they
// agree, and fuzzes the same property.
//
// The gate's scanner is unexported, so it is MIRRORED below, verbatim, and
// TestGateMirrorIsCurrent fails if internal/server/peek.go's code ever drifts
// from the copy. A differential test against a stale copy of one side proves
// nothing.
package canonical_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
)

// ---------------------------------------------------------------------------
// The corpus
// ---------------------------------------------------------------------------

// gateCorpus is every spelling that has ever been able to separate a scanner
// from a parser: exact, case-varied, duplicated, nested, escaped, and invalid
// UTF-8.
var gateCorpus = []string{
	// The ordinary case.
	`{"model":"m","max_tokens":16}`,
	`{"model":"m","stream":true,"max_tokens":16}`,
	`{ "model" : "m" , "stream" : true }`,

	// The defect: encoding/json fills a `json:"model"` field from all of these.
	`{"Model":"expensive-model"}`,
	`{"MODEL":"expensive-model"}`,
	`{"moDel":"expensive-model"}`,
	`{"MoDeL":"expensive-model"}`,
	`{"Model":"expensive-model","max_tokens":16}`,
	`{"Stream":true,"model":"m"}`,
	`{"streAm":true,"model":"m"}`,
	`{"STREAM":true,"model":"m"}`,

	// Unicode simple folding, which is what encoding/json actually does. U+017F
	// LATIN SMALL LETTER LONG S folds to 's', so this is "stream" to it.
	"{\"ſtream\":true,\"model\":\"m\"}",

	// Duplicate keys, same case and different case, in one object.
	`{"model":"first","model":"second"}`,
	`{"model":"exact","Model":"folded"}`,
	`{"Model":"folded","model":"exact"}`,
	`{"Model":"a","MODEL":"b","moDel":"c"}`,
	`{"model":"a","Model":"b","model":"c"}`,
	`{"stream":true,"Stream":false}`,
	`{"Stream":false,"stream":true}`,

	// The key inside a nested object, where it must be ignored by both.
	`{"messages":[{"role":"user","content":"hi","model":"nested"}],"model":"real"}`,
	`{"messages":[{"role":"user","content":"{\"model\":\"decoy\"}"}],"model":"real"}`,
	`{"metadata":{"model":"nested"}}`,
	`{"tools":[{"type":"function","function":{"name":"f","parameters":{"model":"schema"}}}],"model":"real"}`,
	`{"response_format":{"type":"json_schema","json_schema":{"name":"n","schema":{"Model":"x"}}},"model":"real"}`,

	// Escapes and invalid UTF-8 in the VALUE, where the gate deliberately hands
	// off to encoding/json so that both sides see the same string.
	`{"model":"😀"}`,
	`{"model":"\ud800"}`,
	"{\"model\":\"\x90\"}",
	"{\"model\":\"\xed\xa0\x80\"}",
	"{\"model\":\"\xff\xfe\"}",

	// Shapes that are not a model name at all.
	`{}`,
	`{"model":""}`,
	`{"model":null}`,
	`{"stream":false}`,
	`{"a":[[[[]]]],"model":"deep"}`,
	`{"a":null,"b":-1.5e10,"model":"m","stream":true}`,
}

// escaped spells key with its first rune written as a \uXXXX escape. To any
// conforming parser it is the same key; to a scanner that compares raw bytes it
// is a different one, which is why the gate keeps a slow path for it.
func escaped(key string) string {
	r, n := utf8.DecodeRuneInString(key)
	return fmt.Sprintf(`"\u%04x%s"`, r, key[n:])
}

// escapedKeyCorpus is kept separate because these bodies are where the gate and
// the adapters still differ; see TestGateEscapedKeyDivergence.
var escapedKeyCorpus = []string{
	`{` + escaped("model") + `:"escaped-exact"}`,
	`{` + escaped("Model") + `:"escaped-folded"}`,
	`{` + escaped("MODEL") + `:"escaped-folded"}`,
	`{` + escaped("stream") + `:true,"model":"m"}`,
	`{` + escaped("model") + `:"esc","model":"exact"}`,
	`{"model":"exact",` + escaped("model") + `:"esc"}`,
	`{"model":"exact",` + escaped("Model") + `:"esc"}`,
	`{` + escaped("abc") + `:1,"model":"m"}`,
}

func TestGateAndAdaptersAgree(t *testing.T) {
	for _, body := range append(append([]string{}, gateCorpus...), escapedKeyCorpus...) {
		t.Run(body, func(t *testing.T) { checkAgreement(t, []byte(body)) })
	}
	for name, seed := range serverFuzzSeeds(t) {
		t.Run("seed/"+name, func(t *testing.T) { checkAgreement(t, seed) })
	}
}

// TestFoldedKeyIsNotRelayed pins the other half of the rule: a key that the
// filter removed must not survive in the pass-through Extra map and be handed
// to the next hop, which would re-create the bypass one link down the chain.
func TestFoldedKeyIsNotRelayed(t *testing.T) {
	const body = `{"Model":"expensive-model","messages":[],"model":"cheap"}`
	var req openai.Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Model != "cheap" {
		t.Fatalf("model = %q, want %q", req.Model, "cheap")
	}
	if _, ok := req.Extra["Model"]; ok {
		t.Fatalf("dropped key was kept in Extra and would be relayed: %v", req.Extra)
	}
	out, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if bytes.Contains(out, []byte(`"Model"`)) {
		t.Fatalf("re-encoded request still carries the folded key: %s", out)
	}
}

// checkAgreement is the property, stated once.
func checkAgreement(t *testing.T, body []byte) {
	t.Helper()

	gateModel, gateStream, ok := peekRequest(body)
	if !ok {
		// The server answers 400 and no adapter ever sees these bytes.
		return
	}

	type probe struct {
		name   string
		decode func([]byte) (model string, stream bool, err error)
	}
	probes := []probe{
		{"openai", func(b []byte) (string, bool, error) {
			r, err := openai.DecodeRequest(b)
			if err != nil {
				return "", false, err
			}
			return r.Model, r.Stream, nil
		}},
		{"anthropic", func(b []byte) (string, bool, error) {
			// count_tokens decodes the same Request type without the
			// max_tokens precondition, so the corpus does not have to carry
			// one member purely to get past a check this test is not about.
			r, err := anthropic.DecodeCountTokensRequest(b)
			if err != nil {
				return "", false, err
			}
			return r.Model, r.Stream, nil
		}},
		{"anthropic/messages", func(b []byte) (string, bool, error) {
			r, err := anthropic.DecodeRequest(b)
			if err != nil {
				return "", false, err
			}
			return r.Model, r.Stream, nil
		}},
	}

	// A body with an ESCAPED top-level key is the one shape where the two sides
	// still disagree, and the disagreement is the gate's, not the adapter's:
	// peekRequest compares raw key bytes and only unescapes on a fallback that
	// runs when no model was found. TestGateEscapedKeyDivergence pins exactly
	// what that costs. The adapter is still held to the exact-key oracle below,
	// so the carve-out gives up the gate comparison and nothing else.
	compareToGate := !escapedTopLevelKey(body)

	// The oracle for the adapter itself is a MAP decode. encoding/json matches
	// map keys exactly, so it is the semantics of the gate, of every Python
	// backend, and of the JSON spec — and it is the oracle internal/server's own
	// FuzzPeekRequest holds the scanner to. Holding the adapter to the same one
	// is what makes "the gate and the adapter agree" a property rather than a
	// coincidence of two hand-written walks.
	oracleModel, haveModel, oracleStream, haveStream := exactKeyFields(body)

	// A REPEATED stream key is the second shape the two sides read differently,
	// and again it is the gate that departs from every other implementation:
	// its scanner ORs the flag rather than assigning it, so an earlier true is
	// never undone by a later false. See TestGateStreamFlagIsOredNotAssigned.
	repeatedStream := countTopLevelKey(body, "stream") > 1

	for _, p := range probes {
		model, stream, err := p.decode(body)
		if err != nil {
			// The adapter refused the body outright. Nothing is dispatched, so
			// there is nothing to disagree about.
			continue
		}
		if haveModel && model != oracleModel {
			t.Fatalf("%s: adapter resolved model %q, exact-key decode says %q, body %q",
				p.name, model, oracleModel, body)
		}
		if haveStream && stream != oracleStream {
			t.Fatalf("%s: adapter resolved stream=%v, exact-key decode says %v, body %q",
				p.name, stream, oracleStream, body)
		}
		if !compareToGate {
			continue
		}
		if model != gateModel {
			t.Fatalf("%s: gate authorized model %q, adapter dispatches %q, body %q",
				p.name, gateModel, model, body)
		}
		if stream != gateStream && !repeatedStream {
			t.Fatalf("%s: gate metered stream=%v, adapter streams=%v, body %q",
				p.name, gateStream, stream, body)
		}
	}
}

// exactKeyFields reads model and stream with json.Decoder's TOKENIZER, which is
// an implementation of exact-key matching that shares no code with either the
// gate's scanner or struct decoding: it unescapes each key and hands members
// back in order, duplicates included.
//
// THE DUPLICATE-KEY RULE, stated once and applied here, at the gate and in the
// adapters: the LAST member wins, except that a JSON null never overwrites an
// earlier value. The exception is encoding/json's ("unmarshaling a JSON null
// into any other Go type has no effect on the value"), and the gate follows it
// too — its `model` case requires a string value and skips anything else, so an
// earlier name survives a later null there as well. It is worth knowing that a
// Python backend does NOT follow it: json.loads keeps the last member whatever
// it is, so {"model":"a","model":null} is model "a" to dorang and no model at
// all upstream. That is a fidelity bug, not an authorization one — the gate and
// the adapter still resolve the same name — and it is out of W10's scope.
//
// The "have" results are false when a member's value is not of the type the
// field takes, which is a body the adapter refuses outright.
func exactKeyFields(body []byte) (model string, haveModel bool, stream, haveStream bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return "", false, false, false
	}
	haveModel, haveStream = true, true
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", false, false, false
		}
		key, _ := keyTok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", false, false, false
		}
		if string(bytes.TrimSpace(raw)) == "null" {
			continue
		}
		switch key {
		case "model":
			if err := json.Unmarshal(raw, &model); err != nil {
				haveModel = false
			}
		case "stream":
			if err := json.Unmarshal(raw, &stream); err != nil {
				haveStream = false
			}
		}
	}
	return model, haveModel, stream, haveStream
}

// countTopLevelKey counts members of the top-level object whose key is exactly
// key, unescaped — which is the comparison the gate makes.
func countTopLevelKey(b []byte, key string) int {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return 0
	}
	i++
	n := 0
	for {
		i = skipSpace(b, i)
		if i >= len(b) || b[i] == '}' {
			return n
		}
		if b[i] == ',' {
			i++
			continue
		}
		if b[i] != '"' {
			return n
		}
		start := i + 1
		end, esc, ok := scanString(b, i)
		if !ok {
			return n
		}
		if !esc && string(b[start:end-1]) == key {
			n++
		}
		i = skipSpace(b, end)
		if i >= len(b) || b[i] != ':' {
			return n
		}
		i = skipValue(b, skipSpace(b, i+1))
		if i < 0 {
			return n
		}
	}
}

// TestGateStreamFlagIsOredNotAssigned records the second residual divergence,
// found by FuzzGateAdapterAgreement and, like the first, living in the gate.
//
// The duplicate-key rule everywhere else in dorang is LAST-WINS: encoding/json
// overwrites the field on every matching member, and peekRequest assigns to
// `model` on every matching member, so both resolve {"model":"a","model":"b"}
// to "b". The stream flag breaks that rule on one side only —
//
//	case !esc && string(key) == "stream":
//		if hasPrefixAt(b, i, "true") {
//			stream = true
//		}
//
// — which sets but never clears. {"stream":true,"stream":false} is therefore a
// streaming request to the gate and a non-streaming request to every adapter
// and every backend. It is not an authorization bypass: nothing is authorized
// against the flag. It is a response-shape and metering mismatch — the server
// prepares an SSE response, including COMPATIBILITY 1.3's in-band error path,
// for a call that will come back whole. The fix is in peekRequest: assign
// `stream = hasPrefixAt(b, i, "true")` instead of OR-ing into it.
// TestGateStreamFlagIsAssignedNotOred pins the fix: a later duplicate must
// clear an earlier true, because every adapter and backend takes the last one.
// Previously the gate OR-ed, so it prepared an SSE response — in-band error
// path and all — for a call that comes back whole.
func TestGateStreamFlagIsAssignedNotOred(t *testing.T) {
	const body = `{"model":"m","stream":true,"stream":false}`
	gateModel, gateStream, ok := peekRequest([]byte(body))
	if !ok {
		t.Fatalf("gate rejected %q", body)
	}
	r, err := openai.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("adapter refused %q: %v", body, err)
	}
	if gateStream != r.Stream || gateModel != r.Model {
		t.Fatalf("gate and adapter disagree: gate=(%q,%v) adapter=(%q,%v)",
			gateModel, gateStream, r.Model, r.Stream)
	}
	if gateStream {
		t.Fatal("stream is still OR-ed: a later false did not clear an earlier true")
	}
}

// TestGateEscapedKeyDivergence records what is left after W10 is closed, and it
// is NOT in the adapters.
//
// internal/server/peek.go compares key bytes without unescaping, and unescapes
// only on a fallback that runs when the fast path found no model
// (`if model == "" && sawEscapedKey`). That fallback is a json.Unmarshal into a
// tagged struct, which is case-INSENSITIVE. Three consequences, in increasing
// severity:
//
//  1. "Model" — an escaped spelling of "Model" — is read as a model BY THE
//     GATE and as nothing by the adapter. Fails closed: the allow-list is
//     checked against a name that is then not dispatched.
//
//  2. {"stream":true,"model":"m"} names a model on the fast path, so the
//     fallback never runs and the escaped stream flag is invisible to the gate
//     while the adapter honours it. The request is metered as non-streaming and
//     streamed.
//
//  3. {"model":"y","model":"x"} — an exact key followed by an escaped
//     spelling of the SAME key — resolves to "y" at the gate and to "x"
//     everywhere else, because everywhere else unescapes the key and takes the
//     last of two duplicates. THIS FAILS OPEN: the allow-list is checked
//     against "y" and "x" is dispatched. It is the same class of bug as W10 and
//     it lives in the gate, not in the adapters, so it cannot be closed from
//     here. The fix is one line of peekRequest: unescape the key before
//     comparing it, or take the slow path whenever sawEscapedKey is set rather
//     than only when no model was found.
//
// The assertions below describe today's behaviour so that fixing peekRequest
// fails this test and retires it, rather than passing silently.
// TestGateAgreesWithAdapterOnEscapedKeys covers the three ways the gate used to
// disagree with the adapters. Each one authorized one model and dispatched
// another; the last failed OPEN, since the key's allow-list was checked against
// a name that was never sent.
//
// This test asserted the broken behaviour on purpose, so that a fix to peek.go
// would fail loudly rather than pass in silence. The fix landed; it now asserts
// agreement, which is the property that was wanted all along.
func TestGateAgreesWithAdapterOnEscapedKeys(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantModel  string
		wantStream bool
	}{
		{
			// The gate's fallback used a tagged struct, so encoding/json's
			// case-insensitive fold resolved this — W10's own defect, living
			// inside the gate. Neither side accepts it now.
			name:      "a folded escaped key is not the model",
			body:      `{` + escaped("Model") + `:"expensive-model"}`,
			wantModel: "",
		},
		{
			// The gate never decoded escaped keys, so this streamed for the
			// adapter and not for the gate.
			name:       "an escaped stream key is seen by both",
			body:       `{` + escaped("stream") + `:true,"model":"m"}`,
			wantModel:  "m",
			wantStream: true,
		},
		{
			// The sharpest one: the gate stopped at the first spelling while
			// every adapter unescapes and takes the last.
			name:      "an escaped duplicate overtakes the exact key on both sides",
			body:      `{"model":"authorized",` + escaped("model") + `:"dispatched"}`,
			wantModel: "dispatched",
		},
		{
			// Found by fuzzing: the gate OR-ed the flag instead of assigning,
			// so it prepared an SSE response for a call that comes back whole.
			name:       "a later stream:false clears an earlier true on both sides",
			body:       `{"model":"m","stream":true,"stream":false}`,
			wantModel:  "m",
			wantStream: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model, stream, ok := peekRequest([]byte(c.body))
			if !ok {
				t.Fatalf("gate rejected %q", c.body)
			}
			r, err := openai.DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("adapter refused %q: %v", c.body, err)
			}
			if model != r.Model || stream != r.Stream {
				t.Fatalf("gate and adapter disagree on %s\n  gate:    model=%q stream=%v\n  adapter: model=%q stream=%v",
					c.body, model, stream, r.Model, r.Stream)
			}
			if model != c.wantModel || stream != c.wantStream {
				t.Fatalf("both resolved model=%q stream=%v, want model=%q stream=%v",
					model, stream, c.wantModel, c.wantStream)
			}
		})
	}
}

// escapedTopLevelKey reports whether any member key of the top-level object is
// written with a backslash escape, which is the trigger for the gate's slow
// path.
func escapedTopLevelKey(b []byte) bool {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return false
	}
	i++
	for {
		i = skipSpace(b, i)
		if i >= len(b) || b[i] == '}' {
			return false
		}
		if b[i] == ',' {
			i++
			continue
		}
		if b[i] != '"' {
			return false
		}
		end, esc, ok := scanString(b, i)
		if !ok {
			return false
		}
		if esc {
			return true
		}
		i = skipSpace(b, end)
		if i >= len(b) || b[i] != ':' {
			return false
		}
		i = skipValue(b, skipSpace(b, i+1))
		if i < 0 {
			return false
		}
	}
}

// ---------------------------------------------------------------------------
// Fuzzing
// ---------------------------------------------------------------------------

// FuzzGateAdapterAgreement is TestGateAndAdaptersAgree with the corpus
// generated instead of written down.
//
// The mutator is given real request bodies to start from, plus the server's own
// FuzzPeekRequest corpus, because the interesting inputs are one byte away from
// a valid request — a flipped case bit in a key is exactly what this is looking
// for and exactly what a mutator produces from those seeds.
func FuzzGateAdapterAgreement(f *testing.F) {
	for _, s := range gateCorpus {
		f.Add([]byte(s))
	}
	for _, seed := range serverFuzzSeeds(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkAgreement(t, body)
		// The same bytes read as a program that BUILDS an object, so that the
		// mutator spends its budget on member spellings and member order
		// instead of on rediscovering that a body has to start with '{'.
		checkAgreement(t, synthObject(body))
	})
}

// fuzzKeys are quoted key literals: every spelling that has ever mattered here,
// plus a few that must keep not mattering.
var fuzzKeys = []string{
	`"model"`, `"Model"`, `"MODEL"`, `"moDel"`, `"modeL"`,
	escaped("model"), escaped("Model"), escaped("MODEL"),
	`"stream"`, `"Stream"`, `"streAm"`, escaped("stream"),
	"\"ſtream\"", "\"ſTREAM\"",
	`"max_tokens"`, `"Max_Tokens"`, `"messages"`, `"Messages"`,
	`"metadata"`, `"tools"`, `"Tools"`, `"tool_choice"`, `"system"`,
	`"a"`, `"A"`, `""`, escaped("abc"),
}

// fuzzValues are value literals, including the two the gate hands to
// encoding/json rather than reading itself: an escape and invalid UTF-8.
var fuzzValues = []string{
	`"m"`, `"M"`, `""`, `"a:b:c"`, `null`, `true`, `false`, `1`, `-1.5e10`,
	`"A"`, `"\ud800"`, "\"\x90\"", `"😀"`, `[]`, `{}`,
	`{"model":"nested"}`, `[{"role":"user","content":"hi"}]`, `16`,
	`"true"`, `"model"`,
}

// synthObject reads seed as a sequence of (key, value) selectors and emits the
// object they name. An empty seed still produces a valid object, so every
// execution tests something.
func synthObject(seed []byte) []byte {
	var b []byte
	b = append(b, '{')
	for i := 0; i+1 < len(seed) && i < 24; i += 2 {
		if len(b) > 1 {
			b = append(b, ',')
		}
		b = append(b, fuzzKeys[int(seed[i])%len(fuzzKeys)]...)
		b = append(b, ':')
		b = append(b, fuzzValues[int(seed[i+1])%len(fuzzValues)]...)
	}
	return append(b, '}')
}

// serverFuzzSeeds reads internal/server's committed FuzzPeekRequest corpus.
//
// It is read rather than copied so that a seed the server's fuzzer found —
// every one of them is a body that already separated the scanner from
// encoding/json once — is exercised here too, and stays exercised when that
// corpus grows.
func serverFuzzSeeds(tb testing.TB) map[string][]byte {
	tb.Helper()
	dir := filepath.Join("..", "server", "testdata", "fuzz", "FuzzPeekRequest")
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatalf("reading the gate's fuzz corpus: %v", err)
	}
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			tb.Fatalf("reading %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			const prefix = `[]byte("`
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			s, err := strconv.Unquote(strings.TrimPrefix(strings.TrimSuffix(line, ")"), "[]byte("))
			if err != nil {
				tb.Fatalf("corpus entry %s: %v", e.Name(), err)
			}
			out[e.Name()] = []byte(s)
		}
	}
	if len(out) == 0 {
		tb.Fatal("the gate's fuzz corpus is empty; the differential test would prove less than it claims")
	}
	return out
}

// ---------------------------------------------------------------------------
// The mirror, and its drift check
// ---------------------------------------------------------------------------

// TestGateMirrorIsCurrent compares the mirrored scanner below with the real one
// in internal/server/peek.go, function by function, ignoring comments and
// formatting. It fails the moment the gate's behaviour is edited without the
// mirror following, because a differential test against a stale copy is a test
// of nothing.
func TestGateMirrorIsCurrent(t *testing.T) {
	names := []string{"peekRequest", "hasPrefixAt", "skipSpace", "scanString", "skipValue"} // pragma: allowlist secret — test fixture
	gate := funcSource(t, filepath.Join("..", "server", "peek.go"), names)
	mirror := funcSource(t, "gatediff_test.go", names)
	for _, n := range names {
		if gate[n] == "" {
			t.Fatalf("%s is gone from internal/server/peek.go; the gate changed shape", n)
		}
		if gate[n] != mirror[n] {
			t.Fatalf("mirror of %s has drifted from internal/server/peek.go.\n"+
				"gate:\n%s\n\nmirror:\n%s", n, gate[n], mirror[n])
		}
	}
}

func funcSource(t *testing.T, path string, names []string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0) // no ParseComments: comments may differ
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := make(map[string]string, len(names))
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !want[fn.Name.Name] {
			continue
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, fn); err != nil {
			t.Fatalf("printing %s: %v", fn.Name.Name, err)
		}
		// Blank lines survive comment removal, and a comment is exactly what
		// the two copies are allowed to differ in.
		var code []string
		for _, line := range strings.Split(buf.String(), "\n") {
			if line = strings.TrimRight(line, " \t"); line != "" {
				code = append(code, line)
			}
		}
		out[fn.Name.Name] = strings.Join(code, "\n")
	}
	return out
}

// ---------------------------------------------------------------------------
// MIRROR OF internal/server/peek.go — do not edit except to re-sync.
// ---------------------------------------------------------------------------

func peekRequest(b []byte) (model string, stream, ok bool) {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return "", false, false
	}
	i++
	sawEscapedKey := false
	for {
		i = skipSpace(b, i)
		if i >= len(b) {
			return model, stream, false
		}
		if b[i] == '}' {
			break
		}
		if b[i] == ',' {
			i++
			continue
		}
		if b[i] != '"' {
			return model, stream, false
		}
		keyStart := i + 1
		keyEnd, esc, ok2 := scanString(b, i)
		if !ok2 {
			return model, stream, false
		}
		key := b[keyStart : keyEnd-1]
		if esc {
			sawEscapedKey = true
		}
		i = skipSpace(b, keyEnd)
		if i >= len(b) || b[i] != ':' {
			return model, stream, false
		}
		i = skipSpace(b, i+1)
		if i >= len(b) {
			return model, stream, false
		}
		switch {
		case !esc && string(key) == "model" && b[i] == '"':
			vEnd, vEsc, ok3 := scanString(b, i)
			if !ok3 {
				return model, stream, false
			}
			raw := b[i+1 : vEnd-1]
			if vEsc || !utf8.Valid(raw) {
				// Two cases where the bytes on the wire are not the string:
				// a JSON escape, and invalid UTF-8, which encoding/json
				// replaces with U+FFFD on decode. Both go through the real
				// decoder so that the name the gate authorizes is byte-identical
				// to the name a later full parse will see. A model name is an
				// opaque string (DESIGN §2.1) and two spellings of it are two
				// models — the fuzzer found this by disagreeing with
				// encoding/json on one high byte.
				var s string
				if err := json.Unmarshal(b[i:vEnd], &s); err == nil {
					model = s
				}
			} else {
				model = string(raw)
			}
			i = vEnd
		case !esc && string(key) == "stream":
			// Only the literal true. COMPATIBILITY §3.1 makes the same point
			// about stream_options.include_usage: truthy is not enough, and a
			// gateway that accepts "1" or "yes" here streams a response the
			// client's parser is not expecting.
			// Assign, never OR. A later `"stream": false` must clear an earlier
			// true, because every adapter and backend takes the last duplicate.
			// A gate that disagrees prepares an SSE response — including
			// COMPATIBILITY 1.3's in-band error path — for a call that comes
			// back whole. The fuzzer found this; reading the code did not.
			stream = hasPrefixAt(b, i, "true")
			i = skipValue(b, i)
		default:
			i = skipValue(b, i)
		}
		if i < 0 {
			return model, stream, false
		}
	}
	if sawEscapedKey {
		// An escaped key can spell "model" — it is the same key to every
		// conforming parser, and the fast path deliberately does not decode keys.
		// Two things this must get right, both of which an earlier version did not:
		//
		// The fallback runs whenever an escaped key was seen, NOT only when no
		// model was found. Given a body naming the model twice, once plainly and
		// once with an escaped key, the fast path stops at the first while every
		// adapter unescapes and takes the last — so the gate authorized one model
		// and the backend served another. That failed OPEN: the key's allow-list
		// was checked against a name that was never dispatched.
		//
		// And it decodes into a map, not a tagged struct. encoding/json matches
		// tags case-insensitively, with Unicode folding, which is the very defect
		// W10 exists to close — using a struct here would put it back inside the
		// gate. A map yields unescaped keys and exact matching, and takes the last
		// duplicate, which is what the adapters do.
		var slow map[string]json.RawMessage
		if err := json.Unmarshal(b, &slow); err == nil {
			m, st := model, stream
			if raw, found := slow["model"]; found {
				var v string
				if json.Unmarshal(raw, &v) == nil {
					m = v
				}
			}
			if raw, found := slow["stream"]; found {
				var v bool
				if json.Unmarshal(raw, &v) == nil {
					st = v
				}
			}
			return m, st, true
		}
	}
	return model, stream, true
}

func hasPrefixAt(b []byte, i int, s string) bool {
	if i+len(s) > len(b) {
		return false
	}
	return string(b[i:i+len(s)]) == s
}

func skipSpace(b []byte, i int) int {
	if i < 0 {
		return i
	}
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func scanString(b []byte, i int) (end int, escaped, ok bool) {
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

func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		end, _, ok := scanString(b, i)
		if !ok {
			return -1
		}
		return end
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				end, _, ok := scanString(b, i)
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
