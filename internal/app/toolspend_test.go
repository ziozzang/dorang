package app

import (
	"encoding/json"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Server-side tool spend reaches the pricing engine, not just the ledger.
//
// Carrying a count and never charging it is the defect this repository
// catalogues most often — a value that is read, stored, and consulted by
// nothing. `tool_usage` was being preserved through the wire, the backend and
// the dispatcher, and pricing could not see it: a web search is billed PER
// SEARCH and a generated image's tokens at a rate unrelated to chat tokens, so
// a deployment using either was billing its callers for neither.
func TestToolSpendReachesPricing(t *testing.T) {
	extra := &canonical.UsageExtra{ToolUsage: map[string]json.RawMessage{
		"web_search": json.RawMessage(`{"num_requests":3}`),
		"image_gen":  json.RawMessage(`{"input_tokens":40,"output_tokens":1200}`),
	}}
	// The WIRING, not the reader. A test that called toolCount directly would
	// prove the reader works and say nothing about whether the dispatcher hands
	// the numbers to pricing — which is the failure this change closes.
	searches, imgIn, imgOut := toolQuantities(extra)
	if searches != 3 {
		t.Errorf("web searches = %d, want 3 — billed per search and priced at none", searches)
	}
	if imgIn != 40 {
		t.Errorf("image input tokens = %d, want 40", imgIn)
	}
	if imgOut != 1200 {
		t.Errorf("image output tokens = %d, want 1200", imgOut)
	}
}

// A count dorang cannot read is not a count it may invent.
//
// The report's shape is the vendor's and grows without a version bump. Every
// unreadable case yields zero rather than a guess, because a fabricated
// quantity produces a plausible total that no invoice matches and nothing
// fails.
func TestAnUnreadableToolCountIsZeroRatherThanAGuess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra *canonical.UsageExtra
	}{
		{"nil", nil},
		{"no tool usage", &canonical.UsageExtra{}},
		{"tool absent", &canonical.UsageExtra{ToolUsage: map[string]json.RawMessage{
			"image_gen": json.RawMessage(`{"output_tokens":5}`)}}},
		{"member absent", &canonical.UsageExtra{ToolUsage: map[string]json.RawMessage{
			"web_search": json.RawMessage(`{"something_else":1}`)}}},
		{"not a number", &canonical.UsageExtra{ToolUsage: map[string]json.RawMessage{
			"web_search": json.RawMessage(`{"num_requests":"many"}`)}}},
		{"negative", &canonical.UsageExtra{ToolUsage: map[string]json.RawMessage{
			"web_search": json.RawMessage(`{"num_requests":-2}`)}}},
		{"not an object", &canonical.UsageExtra{ToolUsage: map[string]json.RawMessage{
			"web_search": json.RawMessage(`7`)}}},
	} {
		if got, _, _ := toolQuantities(tc.extra); got != 0 {
			t.Errorf("%s: got %d, want 0 — an unreadable charge must not become an "+
				"invented one", tc.name, got)
		}
	}
}
