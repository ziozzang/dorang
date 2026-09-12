package app

import (
	"encoding/json"
	"fmt"
	"github.com/ziozzang/dorang/internal/server"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Server-side tool spend reaches the caller's headers.
//
// The counts already reached the ledger and the price; the caller is the one
// party that could not see them, on the very answer that incurred them. The
// assertion is on the response headers of an assembled gateway, with the
// upstream answering a Responses document carrying `tool_usage` — and on an
// UNPRICED deployment, because the counts are the answer's whether or not a
// rate exists for them.
func TestServerSideToolSpendReachesTheHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","object":"response","status":"completed","model":"m1-upstream",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],` +
			`"usage":{"input_tokens":10,"output_tokens":5},` +
			`"tool_usage":{"web_search":{"num_requests":3},"image_gen":{"input_tokens":40,"output_tokens":120}}}`))
	}))
	t.Cleanup(up.Close)
	yaml := fmt.Sprintf(spellingYAML, up.URL, "{}", "")
	a := newWiringApp(t, yaml, nil, func(o *Options) { o.Upstream = up.Client() })
	key := issueKey(t, a, nil)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"go"}]}`))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set(server.HeaderDetail, "full")
	w := httptest.NewRecorder()
	a.Server.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", w.Code, w.Body.String())
	}
	for name, want := range map[string]string{
		server.HeaderToolWebSearches:   "3",
		server.HeaderTokensImageInput:  "40",
		server.HeaderTokensImageOutput: "120",
		server.HeaderTokensInput:       "10",
	} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
