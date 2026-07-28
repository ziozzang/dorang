package rerank

import (
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The rerank surface.
//
// The dialects disagree about one thing only — how the answer reports its
// billing — and that disagreement is the reason the neutral form has two
// distinct fields for it. A search unit priced as a token is DESIGN §10.7's
// "wrong invoice, no error" defect with the units swapped.

func TestRerankRequestGolden(t *testing.T) {
	body := []byte(`{"model":"rerank:v3.5","query":"capital of Korea",` +
		`"documents":["Seoul is the capital.","Busan is a port."],"top_n":1,"return_documents":true}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "rerank:v3.5" {
		t.Errorf("model %q — the name is opaque and nothing splits it (DESIGN §2.1)", req.Model)
	}
	if len(req.Documents) != 2 || req.Documents[0].Text != "Seoul is the capital." {
		t.Fatalf("documents %+v", req.Documents)
	}
	got, err := MarshalRequest(req, &EncodeOptions{Model: "upstream-id", Flavor: FlavorGeneric})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"upstream-id","query":"capital of Korea",` +
		`"documents":["Seoul is the capital.","Busan is a port."],"top_n":1,"return_documents":true}`
	if string(got) != want {
		t.Fatalf("request bytes\n got: %s\nwant: %s", got, want)
	}
}

// TestRerankStructuredDocumentsSurvive: the object form carries named members a
// rank_fields-aware backend ranks on. Collapsing it to its text would silently
// change what is being ranked.
func TestRerankStructuredDocumentsSurvive(t *testing.T) {
	body := []byte(`{"model":"m","query":"q","documents":[{"title":"T","text":"B"}],"rank_fields":["title","text"]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Documents[0].IsStructured() || req.Documents[0].Text != "B" {
		t.Fatalf("document %+v", req.Documents[0])
	}
	got, err := MarshalRequest(req, &EncodeOptions{Flavor: FlavorJina})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `{"title":"T","text":"B"}`) {
		t.Fatalf("the structured form did not survive: %s", got)
	}

	// The vendor's v2 surface takes strings unless rank_fields names the members
	// to rank on. Without them, the text form is sent rather than a body that
	// 400s for a reason the caller has never seen.
	plain, err := DecodeRequest([]byte(`{"model":"m","query":"q","documents":[{"title":"T","text":"B"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err = MarshalRequest(plain, &EncodeOptions{Flavor: FlavorCohere})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"documents":["B"]`) {
		t.Fatalf("the v2 fallback did not apply: %s", got)
	}
}

// TestRerankBillingIsNotConflated is the one that would produce a wrong invoice.
func TestRerankBillingIsNotConflated(t *testing.T) {
	t.Run("tokens", func(t *testing.T) {
		body := []byte(`{"model":"upstream-id","usage":{"total_tokens":815},` +
			`"results":[{"index":0,"relevance_score":0.87,"document":{"text":"Seoul"}}]}`)
		resp, err := DecodeResponse(body, FlavorJina, "rerank:v3.5")
		if err != nil {
			t.Fatal(err)
		}
		if resp.Usage == nil || resp.Usage.InputTokens != 815 {
			t.Fatalf("usage %+v — a token count must be read as tokens", resp.Usage)
		}
		if resp.SearchUnits != 0 {
			t.Errorf("search units %d were invented from a token count", resp.SearchUnits)
		}
		if resp.Model != "rerank:v3.5" {
			t.Errorf("model %q — §7.2 puts the CLIENT's name in the body", resp.Model)
		}
	})
	t.Run("search units", func(t *testing.T) {
		body := []byte(`{"id":"r1","results":[{"index":2,"relevance_score":0.98}],` +
			`"meta":{"api_version":{"version":"2"},"billed_units":{"search_units":1}}}`)
		resp, err := DecodeResponse(body, FlavorCohere, "rerank:v3.5")
		if err != nil {
			t.Fatal(err)
		}
		if resp.SearchUnits != 1 {
			t.Fatalf("search units %d", resp.SearchUnits)
		}
		if resp.Usage != nil {
			t.Errorf("a token count was invented from a search unit: %+v", resp.Usage)
		}
	})
}

// TestRerankResponseGolden pins the one client-facing answer shape. It is the
// union of the two vendor shapes because both are read in the field, and a
// block is emitted only when the backend reported it — a zeroed billing block
// reads as "this was free".
func TestRerankResponseGolden(t *testing.T) {
	resp := &canonical.RerankResponse{
		ID:    "r1",
		Model: "rerank:v3.5",
		Results: []canonical.RerankResult{
			{Index: 2, RelevanceScore: 0.98, Document: &canonical.RerankDocument{Text: "Seoul"}},
			{Index: 0, RelevanceScore: 0.12},
		},
		Usage:       &canonical.Usage{InputTokens: 815},
		SearchUnits: 1,
	}
	got, err := MarshalResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"r1","model":"rerank:v3.5","results":[` +
		`{"index":2,"relevance_score":0.98,"document":"Seoul"},` +
		`{"index":0,"relevance_score":0.12}],` +
		`"usage":{"total_tokens":815,"prompt_tokens":815},` +
		`"meta":{"billed_units":{"search_units":1}}}`
	if string(got) != want {
		t.Fatalf("response bytes\n got: %s\nwant: %s", got, want)
	}

	// And with nothing billed, neither block appears.
	bare, err := MarshalResponse(&canonical.RerankResponse{
		Results: []canonical.RerankResult{{Index: 0, RelevanceScore: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "usage") || strings.Contains(string(bare), "meta") {
		t.Fatalf("an unbilled answer carried a zeroed billing block: %s", bare)
	}
}

// TestRerankDecodeIsCaseSensitive is COMPATIBILITY 2.0 on this surface.
func TestRerankDecodeIsCaseSensitive(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"Model":"expensive","query":"q","documents":["a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "" {
		t.Errorf("model %q resolved from a differently-cased key", req.Model)
	}
	if _, leaked := req.Extra["Model"]; leaked {
		t.Error("the colliding key survived into Extra and would be relayed to the next hop")
	}
}

// TestRerankExtraPassesThrough keeps a backend knob dorang does not model.
func TestRerankExtraPassesThrough(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","query":"q","documents":["a"],"truncate":"END"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"truncate":"END"`) {
		t.Fatalf("an unmodelled member was filtered out: %s", got)
	}
}
