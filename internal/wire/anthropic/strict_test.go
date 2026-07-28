package anthropic

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestRequestDecodeIsCaseSensitive covers COMPATIBILITY 2.0 at the level the
// authorization gate cares about: a differently-cased key names nothing.
func TestRequestDecodeIsCaseSensitive(t *testing.T) {
	cases := []struct {
		body      string
		wantModel string
		wantMax   bool
	}{
		{`{"model":"m","max_tokens":16,"messages":[]}`, "m", true},
		{`{"Model":"m","max_tokens":16,"messages":[]}`, "", true},
		{`{"MODEL":"m","max_tokens":16,"messages":[]}`, "", true},
		{`{"moDel":"m","max_tokens":16,"messages":[]}`, "", true},
		{`{"model":"exact","Model":"folded","max_tokens":16,"messages":[]}`, "exact", true},
		// max_tokens is required, and "Max_Tokens" does not supply it.
		{`{"model":"m","Max_Tokens":16,"messages":[]}`, "m", false},
	}
	for _, c := range cases {
		var w Request
		if err := json.Unmarshal([]byte(c.body), &w); err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if w.Model != c.wantModel {
			t.Fatalf("%s: model = %q, want %q", c.body, w.Model, c.wantModel)
		}
		if (w.MaxTokens != nil) != c.wantMax {
			t.Fatalf("%s: max_tokens present = %v, want %v", c.body, w.MaxTokens != nil, c.wantMax)
		}
		if _, err := DecodeRequest([]byte(c.body)); (err == nil) != c.wantMax {
			t.Fatalf("%s: DecodeRequest error = %v, want required-field error = %v", c.body, err, !c.wantMax)
		}
	}
}

// TestFoldedKeyIsNotRelayed: a dropped key must not reappear in Extra and ride
// on to a backend, where a case-insensitive parser would match it again.
func TestFoldedKeyIsNotRelayed(t *testing.T) {
	const body = `{"model":"cheap","Model":"expensive","max_tokens":16,"messages":[]}`
	var w Request
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.Extra["Model"]; ok {
		t.Fatalf("folded key survived in Extra: %v", w.Extra)
	}
	out, err := json.Marshal(&w)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(`"Model"`)) {
		t.Fatalf("re-encoded request still carries the folded key: %s", out)
	}
}

// TestBlockDiscriminatorIsCaseSensitive pins the one place where strictness had
// to be applied in two steps: the block type decides whether the rest of the
// block is modelled at all.
func TestBlockDiscriminatorIsCaseSensitive(t *testing.T) {
	var modelled ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"text","text":"hi"}`), &modelled); err != nil {
		t.Fatal(err)
	}
	if modelled.Type != BlockText || modelled.Text == nil || *modelled.Text != "hi" {
		t.Fatalf("modelled block decoded as %+v", modelled)
	}

	// "Type" is not the discriminator, so this is an untyped, opaque block —
	// which is exactly what a case-sensitive backend sees.
	var folded ContentBlock
	if err := json.Unmarshal([]byte(`{"Type":"text","text":"hi"}`), &folded); err != nil {
		t.Fatal(err)
	}
	if folded.Type != "" {
		t.Fatalf("folded discriminator was honoured: %+v", folded)
	}
	if _, ok := folded.Extra["Type"]; ok {
		t.Fatalf("folded discriminator survived in Extra: %v", folded.Extra)
	}
	if _, ok := folded.Extra["text"]; !ok {
		t.Fatalf("opaque block lost a member it should relay: %v", folded.Extra)
	}

	// An opaque block's OWN members are none of the filter's business, even
	// when they are spelled like a modelled field: only the discriminator is
	// reserved (see ContentBlock.UnmarshalJSON).
	var opaque ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"search_result","Source":"u","source":"v"}`), &opaque); err != nil {
		t.Fatal(err)
	}
	if _, ok := opaque.Extra["Source"]; !ok {
		t.Fatalf("opaque block lost a member: %v", opaque.Extra)
	}
}

// TestNestedFieldsAreCaseSensitive: strictness is not only a top-level
// property. A message whose role is spelled "Role" has no role here and none at
// the backend either.
func TestNestedFieldsAreCaseSensitive(t *testing.T) {
	const body = `{"model":"m","max_tokens":16,"messages":[{"Role":"user","content":"hi"}]}`
	r, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 1 {
		t.Fatalf("messages = %d", len(r.Messages))
	}
	if r.Messages[0].Role != "" {
		t.Fatalf("role = %q, want empty", r.Messages[0].Role)
	}
}
