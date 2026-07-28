package router

import (
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// docGroup holds one deployment that can carry document blocks and one that
// cannot. The one that cannot is placed FIRST and made cheaper, so that
// capability winning cannot be an artefact of the ordering.
func docGroup(withDocs bool) Config {
	deps := []Deployment{
		{ID: "chat-only", Provider: "p-chat", Kind: "openai", UpstreamModel: "gemma4:31b",
			Capabilities: canonical.CapMultiBlockContent | canonical.CapToolCalls,
			Credentials:  []Credential{{ID: "k-chat"}}},
	}
	if withDocs {
		deps = append(deps, Deployment{
			ID: "docs", Provider: "p-docs", Kind: "anthropic", UpstreamModel: "claude-y",
			Capabilities: canonical.CapMultiBlockContent | canonical.CapToolCalls |
				canonical.CapDocumentBlocks | canonical.CapCacheBreakpoints,
			Credentials: []Credential{{ID: "k-docs"}},
		})
	}
	return Config{Groups: []Group{{Name: "m", Class: "c",
		Strategy: []Strategy{StrategyRoundRobin}, Deployments: deps}}}
}

// TestCapabilityFiltersBeforeItRanks is DESIGN §10.1: capability is a routing
// FILTER, so a request using a construct only one protocol family supports is
// routed there when such a deployment exists.
//
// round_robin is the chain, and the incapable deployment is first, so a
// capability that merely nudged the ranking would still lose here.
func TestCapabilityFiltersBeforeItRanks(t *testing.T) {
	h := newHarness(t, docGroup(true), harnessOpts{})

	for i := 0; i < 4; i++ {
		d := h.route(Request{Model: "m", Required: canonical.CapDocumentBlocks})
		if d.Deployment != "docs" {
			t.Fatalf("attempt %d: a request carrying a document must reach the deployment that "+
				"can express it: got %s", i, d.Deployment)
		}
		h.ok(d)
	}

	// Without the construct, the same group rotates normally.
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		d := h.route(Request{Model: "m"})
		seen[d.Deployment] = true
		h.ok(d)
	}
	if !seen["chat-only"] {
		t.Fatal("the filter must not be permanent: a request without the construct rotates freely")
	}
}

// TestCapabilityFailsWithANamedFourHundred is the other half of §10.1: when no
// deployment can express the construct, dorang fails fast with a
// machine-readable body naming it. Returning 200 after silently discarding a
// PDF is worse than an error, because the caller has no way to find out.
func TestCapabilityFailsWithANamedFourHundred(t *testing.T) {
	h := newHarness(t, docGroup(false), harnessOpts{})

	e := h.routeErr(Request{Model: "m", Required: canonical.CapDocumentBlocks})
	if e.Status != 400 {
		t.Fatalf("want 400, got %d", e.Status)
	}
	if e.Code != CodeUnsupportedConstruct {
		t.Fatalf("want %s, got %s", CodeUnsupportedConstruct, e.Code)
	}
	if !containsString(e.Constructs, canonical.ConstructDocumentBlock) {
		t.Fatalf("the refusal must name the construct: got %v", e.Constructs)
	}
	if !e.Terminal() {
		t.Fatal("no deployment can express it, so no hop can; the refusal is terminal")
	}
	if !strings.Contains(e.Message, "silently") {
		t.Fatalf("the message should say the request is not downgraded: %q", e.Message)
	}
}

// TestAllowLossyReachesTheIncapableBackend covers the explicit opt-in: a caller
// who sends x-dorang-allow-lossy accepts the loss, and the construct stops
// being a filter.
func TestAllowLossyReachesTheIncapableBackend(t *testing.T) {
	h := newHarness(t, docGroup(false), harnessOpts{})
	d := h.route(Request{Model: "m",
		Required:   canonical.CapDocumentBlocks,
		AllowLossy: canonical.CapDocumentBlocks})
	if d.Deployment != "chat-only" {
		t.Fatalf("an explicit opt-in must reach the backend: got %s", d.Deployment)
	}
	h.ok(d)
}

// TestDroppableCapabilitiesAreReportedNotFiltered is the split §10.1 insists
// on: a knob the backend does not have is a dropped PARAMETER — the request
// still means what it meant — so it must never remove a candidate.
func TestDroppableCapabilitiesAreReportedNotFiltered(t *testing.T) {
	h := newHarness(t, docGroup(false), harnessOpts{})
	d := h.route(Request{Model: "m", Required: canonical.CapSeed | canonical.CapLogitBias})
	if d.Deployment != "chat-only" {
		t.Fatalf("a droppable capability must not filter: got %s", d.Deployment)
	}
	if !d.Dropped.Has(canonical.CapSeed) || !d.Dropped.Has(canonical.CapLogitBias) {
		t.Fatalf("the decision must report the dropped parameters: got %v", d.Dropped.Names())
	}
	h.ok(d)
}

// TestContextWindowFiltersAndUndeclaredIsNotZero covers §4.3's asymmetry: a
// declared window that the request overflows removes the candidate, while an
// UNDECLARED window (zero) is unknown rather than zero-length and must not.
func TestContextWindowFiltersAndUndeclaredIsNotZero(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Class: "c", Deployments: []Deployment{
		{ID: "small", Provider: "p1", Kind: "openai", UpstreamModel: "m", ContextWindow: 8000,
			Credentials: []Credential{{ID: "k1"}}},
		{ID: "undeclared", Provider: "p2", Kind: "openai", UpstreamModel: "m",
			Credentials: []Credential{{ID: "k2"}}},
	}}}}
	h := newHarness(t, cfg, harnessOpts{})

	d := h.route(Request{Model: "m", InputTokens: 20_000, MaxOutputTokens: 1000})
	if d.Deployment != "undeclared" {
		t.Fatalf("the small window must be filtered out and the undeclared one kept: got %s",
			d.Deployment)
	}
	h.ok(d)

	small := h.route(Request{Model: "m", InputTokens: 100, MaxOutputTokens: 100})
	if small.Deployment != "small" {
		t.Fatalf("a request that fits must take the first candidate: got %s", small.Deployment)
	}
	h.ok(small)
}

// TestNoDeploymentFitsFailsNamingTheLimit is §10.5a's third row: when the
// request fits nowhere in the class, dorang fails with a clear error rather
// than compacting the caller's conversation to make it fit.
func TestNoDeploymentFitsFailsNamingTheLimit(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Class: "c", Deployments: []Deployment{
		{ID: "small", Provider: "p1", Kind: "openai", UpstreamModel: "m", ContextWindow: 8000,
			Credentials: []Credential{{ID: "k1"}}},
	}}}}
	h := newHarness(t, cfg, harnessOpts{})

	e := h.routeErr(Request{Model: "m", InputTokens: 900_000})
	if e.Code != CodeContextWindow || e.Status != 400 {
		t.Fatalf("want a 400 %s, got %d %s", CodeContextWindow, e.Status, e.Code)
	}
}

// TestContextRefusalNamesTheRealLimit: §10.5a's whole justification for
// failing rather than compacting is that the caller learns the real limit,
// which is information they did not have and cannot get elsewhere. A refusal
// that does not carry the number gives up that justification.
func TestContextRefusalNamesTheRealLimit(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Class: "c", Deployments: []Deployment{
		{ID: "small", Provider: "p1", Kind: "openai", UpstreamModel: "m", ContextWindow: 8000,
			Credentials: []Credential{{ID: "k1"}}},
		{ID: "medium", Provider: "p2", Kind: "openai", UpstreamModel: "m", ContextWindow: 32000,
			Credentials: []Credential{{ID: "k2"}}},
	}}}}
	h := newHarness(t, cfg, harnessOpts{})

	e := h.routeErr(Request{Model: "m", InputTokens: 900_000, MaxOutputTokens: 100})
	if !strings.Contains(e.Message, "32000") {
		t.Fatalf("the refusal must name the LARGEST window behind the model, which is the "+
			"limit the caller has to get under: %q", e.Message)
	}
	if strings.Contains(e.Message, "8000;") {
		t.Fatalf("naming the smallest window would be misleading: %q", e.Message)
	}
	if !strings.Contains(e.Message, "900100") {
		t.Fatalf("the refusal should also say what the request needed: %q", e.Message)
	}
}
