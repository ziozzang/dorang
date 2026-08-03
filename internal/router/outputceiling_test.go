package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// models[].deployments[].max_output_tokens — an operator's ceiling on what a
// deployment may be asked to generate.
//
// The incumbent carries a per-deployment max_tokens on real rows (65536,
// 65000) and dorang had no field for it, so the importer lost it. The field
// exists now, and these tests assert the two decisions it embodies: that it
// REFUSES rather than clamping, and that only a ceiling an OPERATOR wrote is
// enforced.

// ceilings is one group whose deployments declare operator ceilings.
func ceilings(window int, outs ...int) Config {
	g := Group{Name: "m", Class: "c"}
	for i, out := range outs {
		d := dep(string(rune('a'+i)), "p1", "openai", "m-upstream")
		d.ContextWindow, d.MaxOutputTokens = window, out
		g.Deployments = append(g.Deployments, d)
	}
	return Config{
		Groups:   []Group{g},
		Fallback: FallbackConfig{On: DefaultChains(), MaxHops: 2, Budget: time.Minute},
	}
}

// TestAnOperatorsOutputCeilingRefusesRatherThanClamping.
//
// Clamping is the other defensible product and it is not the one dorang ships.
// A clamp answers 200 with a truncated completion and the only signal is
// finish_reason, which most clients do not branch on — so the caller reads a
// cut-off answer as a complete one. The refusal names the ceiling instead.
func TestAnOperatorsOutputCeilingRefusesRatherThanClamping(t *testing.T) {
	h := newHarness(t, ceilings(200_000, 4_096), harnessOpts{})

	e := h.routeErr(Request{Model: "m", InputTokens: 100, MaxOutputTokens: 8_000})
	if e.Code != CodeOutputCeiling {
		t.Fatalf("code = %q, want %q", e.Code, CodeOutputCeiling)
	}
	if e.Status != 400 {
		t.Errorf("status = %d, want 400", e.Status)
	}
	// The number the caller has to get under has to be IN the refusal, or they
	// are guessing at a limit only the operator can see.
	if !strings.Contains(e.Message, "4096") {
		t.Errorf("the refusal does not name the ceiling: %q", e.Message)
	}
	// It is the OUTPUT refusal, not the context one. The two facts are
	// different and the fixes are opposite: shorten the conversation versus
	// ask for a shorter answer.
	if e.Code == CodeContextWindow {
		t.Error("an output-ceiling refusal was reported as a context-window refusal")
	}
	// It carries the context_window CAUSE all the same, so an operator who
	// wrote one fallback chain for "this model is too small for this request"
	// wrote one chain and not two.
	if e.Cause != CauseContextWindow {
		t.Errorf("cause = %v, want %v so §7.6's chain applies", e.Cause, CauseContextWindow)
	}

	// Exactly at the ceiling is served: it is a ceiling, not a strict bound.
	h.ok(h.route(Request{Model: "m", InputTokens: 100, MaxOutputTokens: 4_096}))

	// A caller who named NO ceiling is unaffected. The field constrains callers
	// who ask for a number; it is not a clamp applied to everyone.
	h.ok(h.route(Request{Model: "m", InputTokens: 100}))
}

// TestTheCeilingIsRoutableWhereAClampCouldNotBe is the argument for refusing,
// stated as a behaviour: a request over one deployment's ceiling is served by
// the sibling with a higher one. A clamp succeeds locally and can never fail
// back, so the request would have been silently truncated on the small
// deployment while a deployment that could answer it in full sat idle.
func TestTheCeilingIsRoutableWhereAClampCouldNotBe(t *testing.T) {
	h := newHarness(t, ceilings(200_000, 4_096, 65_536), harnessOpts{})

	d := h.route(Request{Model: "m", InputTokens: 100, MaxOutputTokens: 8_000})
	if d.Deployment != "b" {
		t.Fatalf("deployment = %q, want the sibling whose ceiling admits the request", d.Deployment)
	}
	h.ok(d)

	// Past BOTH ceilings the refusal quotes the LARGEST one, because any
	// smaller number is not the limit the caller is up against.
	e := h.routeErr(Request{Model: "m", InputTokens: 100, MaxOutputTokens: 100_000})
	if !strings.Contains(e.Message, "65536") {
		t.Errorf("the refusal quotes a ceiling that is not the largest one: %q", e.Message)
	}
}

// TestACataloguedCeilingNeverRefuses is the other half of the decision, and the
// reason MaxOutputTokens carries a provenance flag rather than being one number.
//
// The catalog is a description of a model that drifts — the entry above this
// commit moved three models in six days. Enforcing a figure dorang READ would
// turn a documentation lag into an outage: a model whose real ceiling rose to
// 128k would keep refusing at the stale 8k, on a deployment where no operator
// ever asked for a limit.
func TestACataloguedCeilingNeverRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "10-ceiling.yaml")
	if err := os.WriteFile(path, []byte(`
version: 1
models:
  - { kind: openai, model: "m-upstream", context_window: 200000, max_output_tokens: 4096 }
`), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	cat, err := catalog.Loader{Paths: []string{path}}.Load()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if got := cat.Model("openai", "m-upstream").MaxOutputTokens; got != 4096 {
		t.Fatalf("the fixture did not take: catalogued ceiling is %d", got)
	}

	// The deployment declares NOTHING, so the 4096 above is the only ceiling in
	// play and it arrives from the catalog.
	cfg := ceilings(0, 0)
	cfg.Groups[0].Deployments[0].ContextWindow = 0
	cfg.Groups[0].Deployments[0].MaxOutputTokens = 0

	h := newHarnessWithCatalog(t, cfg, cat)

	// Twenty times the catalogued ceiling. It must ROUTE: dorang did not read
	// that number from an operator, so it is not dorang's to refuse on.
	d := h.route(Request{Model: "m", InputTokens: 100, MaxOutputTokens: 80_000})
	h.ok(d)

	// And the fill did happen, so the test is not passing because the catalog
	// was ignored altogether.
	if got := h.r.byID["a"].MaxOutputTokens; got != 4096 {
		t.Fatalf("the catalogued ceiling was not compiled onto the deployment: %d", got)
	}
	if h.r.byID["a"].enforceOut {
		t.Error("a catalogued ceiling was marked enforceable; a stale entry is now an outage")
	}
}
