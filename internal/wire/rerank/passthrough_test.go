package rerank

import (
	"testing"

	"github.com/ziozzang/dorang/internal/wire/wiretest"
)

// The rerank answer has TWO billing blocks and dorang modelled one number out
// of each. meta.billed_units.total_tokens was reproduced as dropped; it is one
// of several, and the assertion below is exhaustive rather than a list of the
// ones that were noticed.
//
// Rerank needs no family gate. There is exactly one client-facing rerank shape
// — the union of both vendors' billing blocks — so an answer never leaves in a
// shape other than the one it was decoded into.
const upstreamRerank = `{
  "id": "rr_01",
  "model": "upstream-model-id",
  "results": [
    {"index": 1, "relevance_score": 0.91},
    {"index": 0, "relevance_score": 0.42}
  ],
  "usage": {"total_tokens": 320, "prompt_tokens": 320, "billed_characters": 1180},
  "meta": {
    "api_version": {"version": "2", "is_experimental": false},
    "billed_units": {"search_units": 0, "total_tokens": 320, "input_tokens": 300, "output_tokens": 20},
    "warnings": ["default_model_deprecated"]
  }
}`

// TestRerankAnswerDropsNothing asserts what the client receives against what
// the upstream sent, leaf by leaf.
func TestRerankAnswerDropsNothing(t *testing.T) {
	r, err := DecodeResponse([]byte(upstreamRerank), FlavorCohere, "client-facing-name")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	got, err := MarshalResponse(r)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}

	want := leaves(t, []byte(upstreamRerank))
	// model is rewritten to the client-facing name (DESIGN §7.2).
	delete(want, "model")

	if diff := wiretest.Diff(want, leaves(t, got)); diff != "" {
		t.Errorf("the client did not receive what the upstream sent:\n%s\n\nclient:\n%s", diff, got)
	}
}

// TestMeasuredZeroSearchUnitsSurvives is the same zero-versus-absent rule the
// token counters follow, on the field this surface bills from.
//
// The [Response] doc comment already said the rule — "a block is emitted only
// when the backend actually reported it — a zeroed billing block is worse than
// an absent one, since it reads as 'this was free'" — and an `> 0` test cannot
// express it: it collapses "the vendor charged nothing for this call" into "the
// vendor said nothing", which are different rows on an invoice.
func TestMeasuredZeroSearchUnitsSurvives(t *testing.T) {
	body := `{"id":"r","model":"m","results":[],"meta":{"billed_units":{"search_units":0}}}`
	r, err := DecodeResponse([]byte(body), FlavorCohere, "")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	got, err := MarshalResponse(r)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	if v := leaves(t, got)["meta.billed_units.search_units"]; v != "0" {
		t.Errorf("meta.billed_units.search_units = %q, want a preserved measured zero:\n%s", v, got)
	}

	// A response dorang built itself reports nothing and still emits no block,
	// so a backend that does not bill in search units gains no billing object.
	silent := `{"id":"r","model":"m","results":[]}`
	r, err = DecodeResponse([]byte(silent), FlavorGeneric, "")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	got, err = MarshalResponse(r)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	if _, ok := leaves(t, got)["meta.billed_units.search_units"]; ok {
		t.Errorf("a billing block was invented for a backend that reported none:\n%s", got)
	}
}

func leaves(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out, err := wiretest.Leaves(b)
	if err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return out
}
