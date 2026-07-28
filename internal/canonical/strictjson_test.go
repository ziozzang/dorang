package canonical

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"unicode/utf8"
)

// probe stands in for a wire request: modelled scalars, a nested modelled
// object, a raw pass-through blob and a raw member map.
type probe struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Kind     string            `json:"kind,omitempty"`
	Messages []probeMessage    `json:"messages,omitempty"`
	Tools    []probeTool       `json:"tools,omitempty"`
	Format   *probeFormat      `json:"response_format,omitempty"`
	Bias     map[string]int    `json:"logit_bias,omitempty"`
	Choice   json.RawMessage   `json:"tool_choice,omitempty"`
	Ignored  string            `json:"-"`
	unseen   string            // exercises the unexported-field skip
	Nested   map[string]*probe `json:"nested,omitempty"`
}

func (p *probe) hidden() string { return p.unseen }

type probeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type probeTool struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"parameters,omitempty"`
}

type probeFormat struct {
	Type string `json:"type"`
}

func TestStrictBytes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"exact", `{"model":"m"}`, `{"model":"m"}`},
		{"capital", `{"Model":"m"}`, `{}`},
		{"upper", `{"MODEL":"m"}`, `{}`},
		{"mixed", `{"moDel":"m"}`, `{}`},
		{"first of two", `{"Model":"a","messages":[]}`, `{"messages":[]}`},
		{"last of two", `{"messages":[],"Model":"a"}`, `{"messages":[]}`},
		{"middle of three", `{"stream":true,"Model":"a","model":"b"}`, `{"stream":true,"model":"b"}`},
		{"both dropped", `{"Model":"a","MODEL":"b"}`, `{}`},
		{"exact then folded", `{"model":"a","MODEL":"b"}`, `{"model":"a"}`},
		{"folded then exact", `{"MODEL":"b","model":"a"}`, `{"model":"a"}`},
		{"duplicate exact kept", `{"model":"a","model":"b"}`, `{"model":"a","model":"b"}`},
		{"spacing preserved", `{ "model" : "a" , "Stream" : true }`, `{ "model" : "a"  }`},
		{"unknown key kept", `{"model":"a","frobnicate":1}`, `{"model":"a","frobnicate":1}`},
		{"decoy inside a string", `{"messages":[{"role":"user","content":"{\"Model\":\"x\"}"}]}`,
			`{"messages":[{"role":"user","content":"{\"Model\":\"x\"}"}]}`},
		{"nested field folded", `{"messages":[{"Role":"user","content":"hi"}]}`,
			`{"messages":[{"content":"hi"}]}`},
		{"caller schema untouched", `{"tools":[{"name":"t","parameters":{"Type":"object","Name":"x"}}]}`,
			`{"tools":[{"name":"t","parameters":{"Type":"object","Name":"x"}}]}`},
		{"raw member untouched", `{"tool_choice":{"Type":"function"}}`, `{"tool_choice":{"Type":"function"}}`},
		{"map keys never dropped", `{"logit_bias":{"Model":3,"MODEL":4}}`, `{"logit_bias":{"Model":3,"MODEL":4}}`},
		{"map values descended", `{"nested":{"a":{"Model":"x","model":"y"}}}`, `{"nested":{"a":{"model":"y"}}}`},
		{"nested modelled object", `{"response_format":{"Type":"json_object"}}`, `{"response_format":{}}`},
		{"long s folds onto stream", "{\"ſtream\":true}", `{}`},
		{"not an object", `[1,2,3]`, `[1,2,3]`},
		{"empty", ``, ``},
		{"unterminated", `{"Model":"a"`, `{"Model":"a"`},
		{"invalid utf8 value survives", "{\"model\":\"\x90\"}", "{\"model\":\"\x90\"}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(StrictBytes([]byte(c.in), new(probe)))
			if got != c.want {
				t.Fatalf("StrictBytes(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// escapeFirst rewrites a key's first rune as a \uXXXX escape and returns the
// quoted literal. "model" becomes "model", which is the SAME key to any
// conforming parser and a different byte string to a scanner that does not
// unescape.
func escapeFirst(name string) string {
	r, n := utf8.DecodeRuneInString(name)
	return fmt.Sprintf(`"\u%04x%s"`, r, name[n:])
}

// TestStrictBytesEscapedKeys covers the spelling the gate keeps a slow path
// for. An escaped key is the key it spells, so the escaped form of "model"
// must survive and the escaped form of "Model" must not.
func TestStrictBytesEscapedKeys(t *testing.T) {
	cases := []struct {
		key  string
		keep bool
	}{
		{"model", true},      // the same key, spelled with an escape
		{"kind", true},       // ditto
		{"frobnicate", true}, // unknown to the type, so a pass-through member
		{"Model", false},     // folds onto model
		{"MODEL", false},     // folds onto model
		{"ſtream", false},    // U+017F LONG S folds onto stream
		{"ſTREAM", false},    // and so does its upper-cased spelling
		{"Kind", false},      // U+212A KELVIN SIGN folds onto kind
	}
	for _, c := range cases {
		lit := escapeFirst(c.key)
		in := `{` + lit + `:1}`
		want := `{}`
		if c.keep {
			want = in
		}
		got := string(StrictBytes([]byte(in), new(probe)))
		if got != want {
			t.Fatalf("key %q as %s: StrictBytes = %q, want %q", c.key, lit, got, want)
		}
	}
}

// TestStrictBytesReturnsInputWhenClean pins the fast path: a body with nothing
// to remove comes back as the very same bytes, having allocated nothing.
func TestStrictBytesReturnsInputWhenClean(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	p := new(probe)
	out := StrictBytes(in, p)
	if &out[0] != &in[0] {
		t.Fatalf("clean body was copied")
	}
	if n := testing.AllocsPerRun(100, func() { StrictBytes(in, p) }); n != 0 {
		t.Fatalf("clean scan allocated %v times", n)
	}
}

// TestStrictUnmarshalMatchesExactKeySemantics is the property that closes W10:
// after filtering, a struct decode resolves exactly what an exact-key map
// decode resolves, which is what the gate and every Python backend resolve.
func TestStrictUnmarshalMatchesExactKeySemantics(t *testing.T) {
	bodies := []string{
		`{"model":"m"}`, `{"Model":"m"}`, `{"MODEL":"m"}`, `{"moDel":"m"}`,
		`{"model":"a","MODEL":"b"}`, `{"MODEL":"b","model":"a"}`,
		`{"model":"a","model":"b"}`,
		"{\"ſtream\":true,\"model\":\"m\"}",
		`{"stream":true}`, `{"Stream":true}`, `{"streAm":true}`,
		escapedBody("model", "m"), escapedBody("Model", "m"),
	}
	for _, body := range bodies {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatalf("bad test body %q: %v", body, err)
		}
		var want probe
		if v, ok := raw["model"]; ok {
			if err := json.Unmarshal(v, &want.Model); err != nil {
				t.Fatalf("bad test body %q: %v", body, err)
			}
		}
		if v, ok := raw["stream"]; ok {
			if err := json.Unmarshal(v, &want.Stream); err != nil {
				t.Fatalf("bad test body %q: %v", body, err)
			}
		}
		var got probe
		if err := StrictUnmarshal([]byte(body), &got); err != nil {
			t.Fatalf("StrictUnmarshal(%q): %v", body, err)
		}
		if got.Model != want.Model || got.Stream != want.Stream {
			t.Fatalf("body %q: strict decode gave model=%q stream=%v, exact-key decode gives model=%q stream=%v",
				body, got.Model, got.Stream, want.Model, want.Stream)
		}
	}
}

func escapedBody(key, value string) string {
	return `{` + escapeFirst(key) + `:"` + value + `"}`
}

// foldProbe has string fields only, so a sentinel value lands wherever
// encoding/json decides to put it instead of failing a type check on the way.
type foldProbe struct {
	Model  string `json:"model"`
	Stream string `json:"stream"`
	Kind   string `json:"kind"`
	Role   string `json:"role"`
}

// TestFoldDetectionAgreesWithEncodingJSON validates the reimplementation of
// encoding/json's foldName against encoding/json itself. If the standard
// library ever folds a spelling this does not, that key survives the filter and
// is matched anyway — which is W10, back again.
func TestFoldDetectionAgreesWithEncodingJSON(t *testing.T) {
	exact := map[string]bool{"model": true, "stream": true, "kind": true, "role": true}
	keys := []string{
		"model", "Model", "MODEL", "moDel", "modeL", "mode", "models", "_model",
		"m0del", "MoDeL", "stream", "Stream", "streAm", "STREAM", "ſtream",
		"ſTREAM", "kind", "Kind", "KIND", "Kind", "KIND", "role", "Role",
		"rôle", "модель", "ＭＯＤＥＬ", "", "モデル", "kınd",
	}
	ti := infoOf(reflect.TypeOf(&foldProbe{}))
	for _, k := range keys {
		lit, err := json.Marshal(k)
		if err != nil {
			t.Fatal(err)
		}
		_, collides := ti.member(lit, false)

		var v foldProbe
		_ = json.Unmarshal([]byte(`{`+string(lit)+`:"sentinel"}`), &v)
		matched := v.Model == "sentinel" || v.Stream == "sentinel" ||
			v.Kind == "sentinel" || v.Role == "sentinel"

		if want := matched && !exact[k]; collides != want {
			t.Fatalf("key %q: filter says collides=%v, encoding/json matched=%v", k, collides, matched)
		}
	}
}
