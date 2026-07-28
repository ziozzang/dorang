package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// TestAliasSendsTheRealModelUpstream is the routing half of DESIGN §14
// scenario 8. §7.2's contract is that upstream always receives the REAL model
// id; the requested name goes back in the response body, which is the
// frontend's job and not this package's.
func TestAliasSendsTheRealModelUpstream(t *testing.T) {
	h := newHarness(t, Config{
		Groups: []Group{
			{Name: "model-y", Deployments: []Deployment{
				dep("d1", "plan-a", "glm", "qwen3.5:397b"),
			}},
		},
		Aliases: map[string]string{"model-small": "model-y"},
	}, harnessOpts{})

	d := h.route(Request{Model: "model-small"})
	if d.UpstreamModel != "qwen3.5:397b" {
		t.Fatalf("upstream must receive the real model id: got %q", d.UpstreamModel)
	}
	if d.Group != "model-y" {
		t.Fatalf("the alias must resolve to its group: got %q", d.Group)
	}
	h.ok(d)

	if got, ok := h.r.Resolve("model-small"); !ok || got != "model-y" {
		t.Fatalf("Resolve: got %q ok=%v", got, ok)
	}
	if got, ok := h.r.Resolve("model-y"); !ok || got != "model-y" {
		t.Fatalf("a non-alias resolves to itself: got %q ok=%v", got, ok)
	}
	if _, ok := h.r.Resolve("nothing"); ok {
		t.Fatal("an unknown name must not resolve")
	}
}

// TestUnknownModelIsANamedTerminalRefusal: a name nothing knows is a 404 that
// no fail-back chain can repair.
func TestUnknownModelIsANamedTerminalRefusal(t *testing.T) {
	h := newHarness(t, Config{Groups: []Group{{Name: "m",
		Deployments: []Deployment{dep("d1", "p", "openai", "m")}}}}, harnessOpts{})
	e := h.routeErr(Request{Model: "who?"})
	if e.Status != 404 || e.Code != CodeModelNotFound || !e.Terminal() {
		t.Fatalf("want a terminal 404 %s, got %d %s terminal=%v",
			CodeModelNotFound, e.Status, e.Code, e.Terminal())
	}
}

// TestNewRejectsDanglingReferences: an alias, a class member or a duplicate id
// is a configuration defect, and finding it at load rather than on the first
// request is the difference between a startup failure and an outage.
func TestNewRejectsDanglingReferences(t *testing.T) {
	base := func() Config {
		return Config{Groups: []Group{{Name: "m", Class: "c", Deployments: []Deployment{
			dep("d1", "p", "openai", "m"),
		}}}}
	}
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"alias to nowhere", func(c *Config) { c.Aliases = map[string]string{"a": "nope"} }},
		{"alias shadows a group", func(c *Config) { c.Aliases = map[string]string{"m": "m"} }},
		{"class member missing", func(c *Config) { c.Classes = map[string][]string{"c": {"nope"}} }},
		{"duplicate group", func(c *Config) { c.Groups = append(c.Groups, c.Groups[0]) }},
		{"deployment without an id", func(c *Config) { c.Groups[0].Deployments[0].ID = "" }},
		{"group without a name", func(c *Config) { c.Groups[0].Name = "" }},
		{"credential without an id", func(c *Config) {
			c.Groups[0].Deployments[0].Credentials[0].ID = ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mut(&cfg)
			if _, err := New(cfg, Deps{}); err == nil {
				t.Fatal("expected New to refuse")
			}
		})
	}
}

// TestDuplicateDeploymentIDIsRefused separately, because a shared id would
// silently merge two deployments' health, prefix affinity and circuit state.
func TestDuplicateDeploymentIDIsRefused(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "a", Deployments: []Deployment{dep("same", "p1", "openai", "m")}},
		{Name: "b", Deployments: []Deployment{dep("same", "p2", "openai", "m")}},
	}}
	if _, err := New(cfg, Deps{}); err == nil {
		t.Fatal("two deployments sharing an id must be refused: they would share a circuit")
	}
}

// TestCatalogSuppliesFamilyAndContextWindow checks the pkg/catalog composition:
// configuration that states neither still gets a family for the opaque-state
// pin and a window for the context filter, and the model name goes through
// byte-identical.
func TestCatalogSuppliesFamilyAndContextWindow(t *testing.T) {
	cat := catalog.Default()
	cfg := Config{Groups: []Group{{Name: "m", Deployments: []Deployment{
		{ID: "vllm-1", Provider: "p", Kind: "vllm", UpstreamModel: "qwen3.5:397b",
			Credentials: []Credential{{ID: "k"}}},
		{ID: "anth-1", Provider: "q", Kind: "anthropic", UpstreamModel: "claude-y",
			Credentials: []Credential{{ID: "k2"}}},
	}}}}
	r, err := New(cfg, Deps{Catalog: cat})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vllm, anth := r.byID["vllm-1"], r.byID["anth-1"]
	if vllm.Family == "" || anth.Family == "" {
		t.Fatalf("both families must be derived: %q, %q", vllm.Family, anth.Family)
	}
	if vllm.Family == anth.Family {
		t.Fatalf("an openai-chat engine and an anthropic-messages one are different families "+
			"for opaque state; both came out %q", vllm.Family)
	}
	if vllm.UpstreamModel != "qwen3.5:397b" {
		t.Fatalf("the model name must survive the catalog lookup byte-identical: %q",
			vllm.UpstreamModel)
	}
}

// TestFamiliesDoNotMergeWhenUndeclared: two deployments that merely both failed
// to declare a family must not end up sharing one, because that is a silent
// crossing of exactly the boundary §B forbids.
func TestFamiliesDoNotMergeWhenUndeclared(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Deployments: []Deployment{
		dep("a", "p1", "", "m"),
		dep("b", "p2", "", "m"),
	}}}}
	r, err := New(cfg, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.byID["a"].Family == r.byID["b"].Family {
		t.Fatalf("two undeclared families must not merge: both are %q", r.byID["a"].Family)
	}
}

// TestQuotaExhaustedCredentialStepsAside is the routing half of DESIGN §14
// scenario 4: the exhausted credential steps aside, traffic continues on
// another, and it comes back after the reset.
func TestQuotaExhaustedCredentialStepsAside(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Deployments: []Deployment{
		{ID: "d1", Provider: "p", Kind: "openai", UpstreamModel: "m", Credentials: []Credential{
			{ID: "c1"}, {ID: "c2"},
		}},
	}}}}
	h := newHarness(t, cfg, harnessOpts{})

	reset := h.clock.now().Add(10 * time.Minute)
	h.quota["c1"] = quota.Decision{Allow: false, ResetAt: reset}

	d := h.route(Request{Model: "m"})
	if d.Credential != "c2" {
		t.Fatalf("the exhausted credential must step aside: got %s", d.Credential)
	}
	h.ok(d)

	delete(h.quota, "c1")
	d2 := h.route(Request{Model: "m"})
	if d2.Credential != "c1" {
		t.Fatalf("after the reset the preferred credential comes back: got %s", d2.Credential)
	}
	h.ok(d2)
}

// TestEveryCredentialExhaustedIsA429NamingTheReset: when nothing is left, the
// refusal carries the reset instant rather than a bare "unavailable".
func TestEveryCredentialExhaustedIsA429NamingTheReset(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Deployments: []Deployment{
		{ID: "d1", Provider: "p", Kind: "openai", UpstreamModel: "m",
			Credentials: []Credential{{ID: "c1"}}},
	}}}}
	h := newHarness(t, cfg, harnessOpts{})
	reset := h.clock.now().Add(3 * time.Minute)
	h.quota["c1"] = quota.Decision{Allow: false, ResetAt: reset}

	e := h.routeErr(Request{Model: "m"})
	if e.Status != 429 || e.Code != CodeQuotaExhausted {
		t.Fatalf("want a 429 %s, got %d %s", CodeQuotaExhausted, e.Status, e.Code)
	}
	if !e.ResetAt.Equal(reset) {
		t.Fatalf("want reset %v, got %v", reset, e.ResetAt)
	}
	if e.Cause != CauseQuotaExhausted {
		t.Fatalf("the refusal must classify itself so a chain can pick it up: %s", e.Cause)
	}
}

// TestNilQuotaSourceAllowsEveryCredential: an unmetered credential is
// unlimited, not exhausted. Getting this backwards makes a notebook profile
// refuse every request.
func TestNilQuotaSourceAllowsEveryCredential(t *testing.T) {
	var m Meters
	if d := m.Check("never-seen", time.Now()); !d.Allow {
		t.Fatal("a credential with no meter must be allowed")
	}
}

// TestSingleCandidateReason keeps the explanation honest when there was no
// choice to make.
func TestSingleCandidateReason(t *testing.T) {
	h := newHarness(t, Config{Groups: []Group{{Name: "m",
		Deployments: []Deployment{dep("d1", "p", "openai", "m")}}}}, harnessOpts{})
	d := h.route(Request{Model: "m"})
	if d.Reason != ReasonOnlyCandidate {
		t.Fatalf("want %q, got %q", ReasonOnlyCandidate, d.Reason)
	}
	h.ok(d)
}

// TestClassifyMapsStatusesToCauses covers the classification the router does on
// its own. It deliberately does not sniff bodies: context_window and
// content_policy are 400s with a vendor-specific signature, and §15.5 forbids
// regular expressions here, so the frontend sets those.
func TestClassifyMapsStatusesToCauses(t *testing.T) {
	cases := []struct {
		status int
		err    error
		want   Cause
	}{
		{429, errFake, CauseRateLimit},
		{500, errFake, CauseUpstream5xx},
		{503, errFake, CauseUpstream5xx},
		{401, errFake, CauseAuth},
		{403, errFake, CauseAuth},
		{408, errFake, CauseTimeout},
		{504, errFake, CauseTimeout},
		{0, context.DeadlineExceeded, CauseTimeout},
		{0, &quota.BudgetError{}, CauseBudgetExceeded},
		{200, nil, CauseNone},
	}
	for _, tc := range cases {
		if got := Classify(tc.status, tc.err); got != tc.want {
			t.Errorf("Classify(%d, %v) = %s, want %s", tc.status, tc.err, got, tc.want)
		}
	}
}

// TestVocabularyMatchesTheConfigurationValidator guards the seam between this
// package and internal/config. A strategy the validator accepts but the router
// cannot parse would pass validation and then rank nothing, and a fallback
// cause spelled differently on the two sides would silently lose its chain.
func TestVocabularyMatchesTheConfigurationValidator(t *testing.T) {
	causes := []string{
		config.CauseRateLimit, config.CauseQuotaExhausted, config.CauseContextWindow,
		config.CauseContentPolicy, config.CauseUpstream5xx, config.CauseTimeout,
		config.CauseBudgetExceeded, config.CauseAuth,
	}
	for _, name := range causes {
		c, ok := ParseCause(name)
		if !ok {
			t.Errorf("the router cannot parse the configured cause %q", name)
			continue
		}
		if c.String() != name {
			t.Errorf("cause %q round-trips as %q", name, c.String())
		}
	}
	// The two causes internal/config refuses to give a chain must be the two
	// this package refuses to chain.
	for _, name := range []string{config.CauseBudgetExceeded, config.CauseAuth} {
		c, _ := ParseCause(name)
		if c.Chainable() {
			t.Errorf("%q must not be chainable", name)
		}
	}

	targets := []string{config.TargetSameGroup, config.TargetSameClass, config.TargetSameClassLarger}
	for _, name := range targets {
		tg, ok := ParseTarget(name)
		if !ok || tg.String() != name {
			t.Errorf("target %q does not round-trip (ok=%v, got %q)", name, ok, tg.String())
		}
	}

	for _, name := range []string{
		"round_robin", "least_busy", "lowest_cost", "lowest_latency", "highest_tps",
		"sticky", "prefix_sticky", "priority", "weighted_random",
	} {
		if _, ok := ParseStrategy(name); !ok {
			t.Errorf("the router cannot parse the configured strategy %q", name)
		}
	}
	if _, ok := ParseStrategy("least_used"); ok {
		t.Error("least_used is a key_rotation strategy, not a routing one")
	}
}

// TestDefaultChainsMatchTheDesignTable freezes §7.6's table so a change to it
// has to be deliberate.
func TestDefaultChainsMatchTheDesignTable(t *testing.T) {
	want := map[Cause][]Target{
		CauseRateLimit:      {TargetSameGroup, TargetSameClass},
		CauseQuotaExhausted: {TargetSameGroup, TargetSameClass},
		CauseContextWindow:  {TargetSameClassLarger},
		CauseContentPolicy:  {TargetSameClass},
		CauseUpstream5xx:    {TargetSameGroup, TargetSameClass},
		CauseTimeout:        {TargetSameGroup},
		CauseBudgetExceeded: {},
		CauseAuth:           {},
	}
	got := DefaultChains()
	if len(got) != len(want) {
		t.Fatalf("want %d causes, got %d", len(want), len(got))
	}
	for c, chain := range want {
		g, ok := got[c]
		if !ok {
			t.Errorf("%s is missing", c)
			continue
		}
		if len(g) != len(chain) {
			t.Errorf("%s: want %v, got %v", c, chain, g)
			continue
		}
		for i := range chain {
			if g[i] != chain[i] {
				t.Errorf("%s: want %v, got %v", c, chain, g)
				break
			}
		}
	}
}

// TestReportOnANilDecisionIsSafe: the caller's error paths are messy and the
// router must not be the thing that panics in one.
func TestReportOnANilDecisionIsSafe(t *testing.T) {
	h := newHarness(t, Config{Groups: []Group{{Name: "m",
		Deployments: []Deployment{dep("d1", "p", "openai", "m")}}}}, harnessOpts{})
	h.r.Report(nil, Outcome{})
}

// TestRouteHonoursContextCancellation: a client that hung up must not have
// capacity blocked on its behalf.
func TestRouteHonoursContextCancellation(t *testing.T) {
	cfg, cc := twoAccounts(1)
	cfg.PinnedWait = 5 * time.Second
	h := newHarness(t, cfg, harnessOpts{capacity: cc})

	hold := h.route(Request{Model: "plan"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()

	_, err := h.r.Route(ctx, Request{Model: "plan", Pins: []Pin{
		{Kind: "previous_response_id", Strength: Pinned, Credential: "acct-1"}}})
	if err == nil {
		t.Fatal("a cancelled context must not produce a decision")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrNoRoute) {
		t.Fatalf("unexpected error: %v", err)
	}
	h.ok(hold)
}
