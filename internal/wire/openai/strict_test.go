package openai

import (
	"encoding/json"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TestRequestDecodeIsCaseSensitive covers COMPATIBILITY 2.0 at the level the
// authorization gate cares about: a differently-cased key names nothing.
func TestRequestDecodeIsCaseSensitive(t *testing.T) {
	cases := []struct {
		body       string
		model      string
		stream     bool
		maxTokens  bool
		toolChoice canonical.ToolChoiceMode
	}{
		{body: `{"model":"m","stream":true}`, model: "m", stream: true},
		{body: `{"Model":"m","stream":true}`, stream: true},
		{body: `{"MODEL":"m"}`},
		{body: `{"moDel":"m"}`},
		{body: `{"model":"m","Stream":true}`, model: "m"},
		{body: `{"model":"m","streAm":true}`, model: "m"},
		// U+017F LATIN SMALL LETTER LONG S folds to 's' in encoding/json.
		{body: "{\"model\":\"m\",\"ſtream\":true}", model: "m"},
		{body: `{"model":"exact","Model":"folded"}`, model: "exact"},
		{body: `{"Model":"folded","model":"exact"}`, model: "exact"},
		// Duplicate keys, same spelling: last wins, on both sides of the gate.
		{body: `{"model":"first","model":"second"}`, model: "second"},
		{body: `{"model":"m","max_tokens":16}`, model: "m", maxTokens: true},
		{body: `{"model":"m","Max_Tokens":16}`, model: "m"},
		{body: `{"model":"m","tool_choice":"required"}`, model: "m", toolChoice: canonical.ToolChoiceRequired},
		{body: `{"model":"m","Tool_Choice":"required"}`, model: "m"},
	}
	for _, c := range cases {
		r, err := DecodeRequest([]byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if r.Model != c.model {
			t.Fatalf("%s: model = %q, want %q", c.body, r.Model, c.model)
		}
		if r.Stream != c.stream {
			t.Fatalf("%s: stream = %v, want %v", c.body, r.Stream, c.stream)
		}
		if (r.MaxTokens != nil) != c.maxTokens {
			t.Fatalf("%s: max_tokens present = %v, want %v", c.body, r.MaxTokens != nil, c.maxTokens)
		}
		var mode canonical.ToolChoiceMode
		if r.ToolChoice != nil {
			mode = r.ToolChoice.Mode
		}
		if mode != c.toolChoice {
			t.Fatalf("%s: tool_choice = %q, want %q", c.body, mode, c.toolChoice)
		}
	}
}

// TestNestedFieldsAreCaseSensitive: strictness is not only a top-level
// property. Every level a request is decoded at is a level a backend will read
// case-sensitively.
func TestNestedFieldsAreCaseSensitive(t *testing.T) {
	const body = `{"model":"m","messages":[{"Role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","Function":{"name":"f"}}],` +
		`"stream_options":{"Include_Usage":true},"stream":true}`
	r, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 1 || r.Messages[0].Role != "" {
		t.Fatalf("role survived a folded key: %+v", r.Messages)
	}
	if len(r.Tools) != 1 || r.Tools[0].Name != "" {
		t.Fatalf("tool name survived a folded key: %+v", r.Tools)
	}
	if r.StreamOptions == nil || r.StreamOptions.IncludeUsage {
		t.Fatalf("include_usage survived a folded key: %+v", r.StreamOptions)
	}
}

// TestFoldedKeyIsNotRelayed: a dropped key must not reappear in Extra and ride
// on to a backend, where a case-insensitive parser would match it again.
func TestFoldedKeyIsNotRelayed(t *testing.T) {
	const body = `{"model":"cheap","Model":"expensive","messages":[{"role":"user","Content":"hi"}]}`
	var w Request
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.Extra["Model"]; ok {
		t.Fatalf("folded key survived in Extra: %v", w.Extra)
	}
	if _, ok := w.Messages[0].Extra["Content"]; ok {
		t.Fatalf("folded key survived in a message's Extra: %v", w.Messages[0].Extra)
	}
	out, err := Marshal(&w)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"Model"`, `"Content"`} {
		if got := string(out); len(got) > 0 && containsKey(got, k) {
			t.Fatalf("re-encoded request still carries %s: %s", k, out)
		}
	}
}

func containsKey(s, key string) bool {
	for i := 0; i+len(key) <= len(s); i++ {
		if s[i:i+len(key)] == key {
			return true
		}
	}
	return false
}

// TestUnknownKeysStillPassThrough: the filter removes fold-collisions only.
// DESIGN §10.5a's rule — not recognizing something is not a reason to remove
// it — is unaffected.
func TestUnknownKeysStillPassThrough(t *testing.T) {
	const body = `{"model":"m","chat_template_kwargs":{"Enable_Thinking":true},"Frobnicate":1}`
	var w Request
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.Extra["chat_template_kwargs"]; !ok {
		t.Fatalf("unmodelled member was dropped: %v", w.Extra)
	}
	if _, ok := w.Extra["Frobnicate"]; !ok {
		t.Fatalf("unmodelled member was dropped for its capital letter: %v", w.Extra)
	}
	// And its contents are not rewritten: an opaque value is a caller's
	// document, not dorang's.
	if got := string(w.Extra["chat_template_kwargs"]); got != `{"Enable_Thinking":true}` {
		t.Fatalf("opaque value was rewritten: %s", got)
	}
}
