package catalog

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// writeOverlay writes an operator overlay to a temp file and returns its path.
func writeOverlay(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	return p
}

func loadWith(t *testing.T, body string) *Catalog {
	t.Helper()
	c, err := Load(writeOverlay(t, "overlay.yaml", body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func mustLoadFail(t *testing.T, body, wantSubstr string) {
	t.Helper()
	_, err := Load(writeOverlay(t, "overlay.yaml", body))
	if err == nil {
		t.Fatalf("Load succeeded, want an error mentioning %q", wantSubstr)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("error %v\ndoes not mention %q", err, wantSubstr)
	}
}

// ---------------------------------------------------------------------------
// Embedded data
// ---------------------------------------------------------------------------

// TestDefaultParses is the "the embedded YAML is not broken" guard: Default
// panics on invalid embedded data, so reaching the assertions is most of the
// test.
func TestDefaultParses(t *testing.T) {
	c := Default()

	if got := len(c.Models()); got == 0 {
		t.Fatal("Default() has no models")
	}
	if got := len(c.Kinds()); got == 0 {
		t.Fatal("Default() has no kinds")
	}

	// The 2026-07-28 fetch. A change here should be a deliberate data edit.
	perKind := map[string]int{}
	for _, ref := range c.Models() {
		perKind[ref.Kind]++
	}
	want := map[string]int{
		"ollama-cloud":    19,
		"glm":             8,
		"qwen":            2,
		"xai":             2,
		"codex-responses": 4,
		"jina":            3,
	}
	for kind, n := range want {
		if perKind[kind] != n {
			t.Errorf("kind %q has %d models, want %d", kind, perKind[kind], n)
		}
	}
	sum := 0
	for _, n := range want {
		sum += n
	}
	if total := len(c.Models()); total != sum {
		t.Errorf("catalog has %d models, want %d", total, sum)
	}

	// Retired upstream, so deliberately absent. Re-adding it needs a fetch,
	// not a memory.
	if c.Model("ollama-cloud", "deepseek-v3.2").ModelKnown {
		t.Error("deepseek-v3.2 is present; the provider no longer offers it")
	}
}

// TestDefaultIsShared checks that Default hands back one immutable snapshot
// rather than reparsing, and that Load does not disturb it.
func TestDefaultIsShared(t *testing.T) {
	if Default() != Default() {
		t.Error("Default() returned different catalogs")
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if loaded == Default() {
		t.Error("Load() returned the shared default; overlays could mutate it")
	}
	if len(loaded.Models()) != len(Default().Models()) {
		t.Error("Load() with no paths differs from Default()")
	}
}

// ---------------------------------------------------------------------------
// Kinds and aliases
// ---------------------------------------------------------------------------

func TestEveryKindResolves(t *testing.T) {
	c := Default()

	// Every kind the design names in §4.3, plus the kinds the live providers
	// need. Each must resolve and declare a wire adapter.
	required := []string{
		"openai", "openai-responses", "anthropic", "glm", "qwen", "deepseek",
		"moonshot", "minimax", "mistral", "xai", "google", "ollama",
		"openrouter", "cohere", "jina", "vllm", "bedrock", "vertex", "azure",
		"echo", "ollama-cloud", "codex-responses",
	}
	for _, name := range required {
		kd, ok := c.Kind(name)
		if !ok {
			t.Errorf("kind %q does not resolve", name)
			continue
		}
		if kd.Name != name {
			t.Errorf("kind %q resolved to %q", name, kd.Name)
		}
		if kd.API == "" {
			t.Errorf("kind %q declares no api", name)
		}
		if !validAPIs[kd.API] {
			t.Errorf("kind %q declares unknown api %q", name, kd.API)
		}
		if !validCaches[kd.Cache] {
			t.Errorf("kind %q declares unknown cache %q", name, kd.Cache)
		}
	}

	for _, name := range c.Kinds() {
		if !slices.Contains(required, name) {
			t.Errorf("undeclared-in-test kind %q; update the required list", name)
		}
	}

	if _, ok := c.Kind("no-such-kind"); ok {
		t.Error("an unknown kind resolved")
	}
}

func TestKindAliasesResolve(t *testing.T) {
	c := Default()

	for alias, target := range c.KindAliases() {
		kd, ok := c.Kind(alias)
		if !ok {
			t.Errorf("alias %q does not resolve", alias)
			continue
		}
		if kd.Name != target {
			t.Errorf("alias %q resolved to %q, want %q", alias, kd.Name, target)
		}
		if _, isKind := c.kinds[alias]; isKind {
			t.Errorf("alias %q shadows a declared kind", alias)
		}
	}

	for alias, want := range map[string]string{
		"dashscope":  "qwen",
		"minimax-cn": "minimax",
		"zai":        "glm",
		"z.ai":       "glm", // chained: z.ai -> zai -> glm
		"codex":      "codex-responses",
	} {
		kd, ok := c.Kind(alias)
		if !ok || kd.Name != want {
			t.Errorf("Kind(%q) = %q, %v; want %q, true", alias, kd.Name, ok, want)
		}
	}

	// An alias is a kind-level name from configuration. Looking a model up
	// through one must give the same answer as through the canonical kind.
	viaAlias := c.Model("dashscope", "qwen3.8-max-preview")
	direct := c.Model("qwen", "qwen3.8-max-preview")
	if viaAlias.Kind != direct.Kind || viaAlias.ContextWindow != direct.ContextWindow {
		t.Errorf("alias lookup %+v differs from direct %+v", viaAlias, direct)
	}
}

func TestKindAliasCycleRejected(t *testing.T) {
	mustLoadFail(t, `
version: 1
kind_aliases:
  loop-a: loop-b
  loop-b: loop-a
`, "loop-a")
}

func TestKindAliasToUnknownRejected(t *testing.T) {
	mustLoadFail(t, `
version: 1
kind_aliases:
  ghost: not-a-kind
`, "not a declared kind")
}

// ---------------------------------------------------------------------------
// Opaque model names (REVIEW C3, DESIGN §2.1)
// ---------------------------------------------------------------------------

// TestOpaqueModelNamesRoundTrip freezes the real colon-bearing names. A name
// goes in and comes back byte-identical, whatever the colon happens to mean in
// it: a family tag, a vendor prefix, or a deployment variant.
func TestOpaqueModelNamesRoundTrip(t *testing.T) {
	c := Default()

	golden := []struct {
		kind  string
		model string
		known bool
	}{
		{"ollama-cloud", "gemma4:31b", true},               // family tag
		{"ollama-cloud", "qwen3.5:397b", true},             // family tag
		{"ollama-cloud", "gpt-oss:120b", true},             // family tag
		{"ollama-cloud", "mistral-large-3:675b", true},     // family tag
		{"ollama-cloud", "nemotron-3-nano:30b", true},      // family tag
		{"glm", "zai:glm-5.1", false},                      // vendor prefix
		{"ollama-cloud", "deepseek-v4-flash:cloud", false}, // deployment variant
	}

	for _, g := range golden {
		got := c.Model(g.kind, g.model)
		if got.Model != g.model {
			t.Errorf("Model(%q, %q).Model = %q; the name was rewritten",
				g.kind, g.model, got.Model)
		}
		if got.ModelKnown != g.known {
			t.Errorf("Model(%q, %q).ModelKnown = %v, want %v",
				g.kind, g.model, got.ModelKnown, g.known)
		}
	}

	// The decisive pair: a name that literally contains a catalogued name plus
	// a suffix must not be treated as that catalogued model. Splitting on ':'
	// anywhere would collapse these two.
	base := c.Model("ollama-cloud", "deepseek-v4-flash")
	variant := c.Model("ollama-cloud", "deepseek-v4-flash:cloud")
	if !base.ModelKnown {
		t.Fatal("deepseek-v4-flash should be catalogued")
	}
	if variant.ModelKnown {
		t.Error("deepseek-v4-flash:cloud resolved to the deepseek-v4-flash entry")
	}
	if variant.Verified != "" {
		t.Error("an uncatalogued variant inherited a verification date")
	}

	// A vendor-looking prefix inside a name is not a kind. `zai` resolves as a
	// kind alias, and that must not make `zai:glm-5.1` mean `glm-5.1` on glm.
	if _, ok := c.Kind("zai"); !ok {
		t.Fatal("zai should be a kind alias, to make this test meaningful")
	}
	if c.Model("glm", "zai:glm-5.1").ModelKnown {
		t.Error("zai:glm-5.1 was parsed into a vendor prefix and a model")
	}
	if !c.Model("glm", "glm-5.1").ModelKnown {
		t.Error("glm-5.1 should be catalogued on glm")
	}
}

// TestSameNameDifferentKinds: the key is (kind, model). A name says nothing
// about which provider serves it.
func TestSameNameDifferentKinds(t *testing.T) {
	c := Default()
	onOllama := c.Model("ollama-cloud", "glm-5.1")
	onZai := c.Model("glm", "glm-5.1")

	if !onOllama.ModelKnown || !onZai.ModelKnown {
		t.Fatal("glm-5.1 should be catalogued on both ollama-cloud and glm")
	}
	if onOllama.Kind == onZai.Kind {
		t.Fatal("test setup: kinds should differ")
	}
	if onOllama.Cache == onZai.Cache {
		t.Errorf("both resolved to cache %q; the kind layer was ignored", onOllama.Cache)
	}
}

// ---------------------------------------------------------------------------
// Layer composition (DESIGN §4.3)
// ---------------------------------------------------------------------------

func TestLayerComposition(t *testing.T) {
	// Overlay adds two overlapping prefix rules and one model entry, so all
	// three layers and their precedence are exercised on data this test owns.
	c := loadWith(t, `
version: 1
prefix_rules:
  - kind: ollama-cloud
    prefix: "nemotron-3-"
    context_window: 111000
    max_output_tokens: 11000
  - kind: ollama-cloud
    prefix: "nemotron-3-nano"
    context_window: 222000
models:
  - kind: ollama-cloud
    model: "nemotron-3-nano:30b"
    context_window: 333000
    verified: 2026-07-28
`)

	t.Run("kind only", func(t *testing.T) {
		got := c.Model("ollama-cloud", "no-rule-matches-this")
		if want := []string{"kind"}; !slices.Equal(got.Layers, want) {
			t.Errorf("Layers = %v, want %v", got.Layers, want)
		}
		if got.API != APIOpenAIChat || got.Cache != CacheNone {
			t.Errorf("kind defaults not applied: %+v", got)
		}
		if got.ContextWindow != 0 {
			t.Errorf("ContextWindow = %d; undeclared must stay 0, never a guess",
				got.ContextWindow)
		}
	})

	t.Run("kind then prefix", func(t *testing.T) {
		// Not a catalogued model, so only two layers apply.
		got := c.Model("ollama-cloud", "nemotron-3-mega")
		if want := []string{"kind", "prefix"}; !slices.Equal(got.Layers, want) {
			t.Errorf("Layers = %v, want %v", got.Layers, want)
		}
		if got.MatchedPrefix != "nemotron-3-" {
			t.Errorf("MatchedPrefix = %q, want %q", got.MatchedPrefix, "nemotron-3-")
		}
		if got.ContextWindow != 111000 {
			t.Errorf("ContextWindow = %d, want 111000 from the prefix layer", got.ContextWindow)
		}
		if got.API != APIOpenAIChat {
			t.Errorf("API = %q; the kind layer should still show through", got.API)
		}
	})

	t.Run("longest prefix wins", func(t *testing.T) {
		got := c.Model("ollama-cloud", "nemotron-3-nano-preview")
		if got.MatchedPrefix != "nemotron-3-nano" {
			t.Errorf("MatchedPrefix = %q, want the longer %q",
				got.MatchedPrefix, "nemotron-3-nano")
		}
		if got.ContextWindow != 222000 {
			t.Errorf("ContextWindow = %d, want 222000", got.ContextWindow)
		}
		// Exactly one rule applies, so a field only the shorter rule sets is
		// not inherited from it.
		if got.MaxOutputTokens != 0 {
			t.Errorf("MaxOutputTokens = %d; only the longest rule applies", got.MaxOutputTokens)
		}
	})

	t.Run("model overrides prefix overrides kind", func(t *testing.T) {
		got := c.Model("ollama-cloud", "nemotron-3-nano:30b")
		if want := []string{"kind", "prefix", "model"}; !slices.Equal(got.Layers, want) {
			t.Errorf("Layers = %v, want %v", got.Layers, want)
		}
		if got.ContextWindow != 333000 {
			t.Errorf("ContextWindow = %d, want 333000 from the model layer", got.ContextWindow)
		}
		if got.MatchedPrefix != "nemotron-3-nano" {
			t.Errorf("MatchedPrefix = %q, want %q", got.MatchedPrefix, "nemotron-3-nano")
		}
		if got.Cache != CacheNone || got.API != APIOpenAIChat {
			t.Errorf("kind layer lost: %+v", got)
		}
	})
}

// TestPrefixRuleNeverChangesIdentity is the C3 guard on the middle layer: a
// prefix rule may supply capability defaults and nothing else.
func TestPrefixRuleNeverChangesIdentity(t *testing.T) {
	c := Default()

	// grok-4.9 is not catalogued; the grok-4. rule still supplies defaults.
	got := c.Model("xai", "grok-4.9")
	if got.MatchedPrefix != "grok-4." {
		t.Fatalf("MatchedPrefix = %q, want %q", got.MatchedPrefix, "grok-4.")
	}
	if got.ContextWindow != 256000 || got.MaxOutputTokens != 64000 {
		t.Errorf("prefix rule supplied no capability defaults: %+v", got)
	}
	if got.Model != "grok-4.9" {
		t.Errorf("Model = %q, want %q", got.Model, "grok-4.9")
	}
	if got.ModelKnown {
		t.Error("ModelKnown is true; a prefix match is not catalogue membership")
	}
	if got.Verified != "" {
		t.Error("an uncatalogued model inherited a verification date from a prefix rule")
	}
	if got.Reasoning.Effective() != ReasoningUnknown {
		t.Errorf("Reasoning = %s; prefix rules must not supply reasoning (C5)", got.Reasoning)
	}
}

// TestPrefixRulesCannotDeclareReasoning: the schema has no such field, so an
// overlay that tries is rejected rather than silently ignored.
func TestPrefixRulesCannotDeclareReasoning(t *testing.T) {
	mustLoadFail(t, `
version: 1
prefix_rules:
  - kind: glm
    prefix: "glm-5"
    reasoning:
      capability: thinking_flag
      verified: 2026-07-28
`, "reasoning")
}

func TestKindScopedRuleBeatsGlobalOfEqualLength(t *testing.T) {
	c := loadWith(t, `
version: 1
prefix_rules:
  - prefix: "kimi-k2."
    context_window: 1000
  - kind: ollama-cloud
    prefix: "kimi-k2."
    context_window: 2000
`)

	if got := c.Model("ollama-cloud", "kimi-k2.6").ContextWindow; got != 2000 {
		t.Errorf("ContextWindow = %d, want 2000 from the kind-scoped rule", got)
	}
	// The kind-agnostic rule still applies to other kinds.
	if got := c.Model("glm", "kimi-k2.6").ContextWindow; got != 1000 {
		t.Errorf("ContextWindow = %d, want 1000 from the kind-agnostic rule", got)
	}
}

func TestUnknownKindIsNotAnError(t *testing.T) {
	c := Default()
	got := c.Model("not-a-kind", "some:model")

	if got.KindKnown || got.ModelKnown {
		t.Errorf("unknown kind reported as known: %+v", got)
	}
	if got.Kind != "not-a-kind" || got.Model != "some:model" {
		t.Errorf("Kind/Model = %q/%q; both should come back as given", got.Kind, got.Model)
	}
	if got.Reasoning.Effective() != ReasoningUnknown {
		t.Errorf("Reasoning = %s, want unknown", got.Reasoning)
	}
	if len(got.Layers) != 0 {
		t.Errorf("Layers = %v, want none", got.Layers)
	}
}

// ---------------------------------------------------------------------------
// Reasoning capability (REVIEW C5, DESIGN §10.2)
// ---------------------------------------------------------------------------

func TestReasoningIsKeyedByModelNotKind(t *testing.T) {
	c := Default()

	// The one verified capability in the embedded catalog.
	got := c.Reasoning("qwen", "qwen3.8-max-preview")
	if got.Effective() != ReasoningEffortScale {
		t.Fatalf("capability = %s, want %s", got, ReasoningEffortScale)
	}
	if want := []string{"low", "high", "xhigh"}; !slices.Equal(got.Levels, want) {
		t.Errorf("Levels = %v, want %v", got.Levels, want)
	}
	if got.DefaultLevel != "xhigh" {
		t.Errorf("DefaultLevel = %q, want %q", got.DefaultLevel, "xhigh")
	}
	if got.Verified != "2026-07-28" {
		t.Errorf("Verified = %q, want 2026-07-28", got.Verified)
	}
	if s := got.String(); s != "effort_scale(low,high,xhigh)" {
		t.Errorf("String() = %q", s)
	}

	// The same kind, a different model: the capability does NOT carry across.
	// Revision 1 generalized one model version's scale to the whole family;
	// this assertion is what stops that from coming back.
	sibling := c.Reasoning("qwen", "qwen3.7-max")
	if sibling.Effective() != ReasoningUnknown {
		t.Errorf("qwen3.7-max reasoning = %s; a sibling's verified scale leaked "+
			"across the family (REVIEW C5)", sibling)
	}
	if sibling.Known() {
		t.Error("qwen3.7-max reports a known capability without a probe")
	}

	// The kind's shape hint is not the model's capability, and here it does
	// not even agree with the one model that was verified.
	kd, _ := c.Kind("qwen")
	if kd.ReasoningHint != ReasoningEnableThinking {
		t.Errorf("qwen ReasoningHint = %q, want %q", kd.ReasoningHint, ReasoningEnableThinking)
	}
	if ReasoningCapability(kd.ReasoningHint) == got.Effective() {
		t.Error("test setup: hint and verified capability should differ here")
	}
}

// TestReasoningUnknownUnlessVerified sweeps the catalogue: nothing claims a
// capability without a date, and everything undated reads unknown.
func TestReasoningUnknownUnlessVerified(t *testing.T) {
	c := Default()

	verified := 0
	for _, ref := range c.Models() {
		r := c.Reasoning(ref.Kind, ref.Model)
		switch {
		case r.Effective() == ReasoningUnknown:
			if r.Verified != "" {
				t.Errorf("%s/%s: unknown capability carries date %q",
					ref.Kind, ref.Model, r.Verified)
			}
			if r.Known() {
				t.Errorf("%s/%s: unknown capability reports Known()", ref.Kind, ref.Model)
			}
		default:
			if r.Verified == "" {
				t.Errorf("%s/%s: claims %s with no verification date",
					ref.Kind, ref.Model, r.Effective())
			}
			verified++
		}
	}
	if verified != 1 {
		t.Errorf("%d models claim a concrete capability; the 2026-07-28 pass "+
			"probed exactly one", verified)
	}

	// Every kind with no model entries answers unknown for any model.
	for _, kind := range c.Kinds() {
		if r := c.Reasoning(kind, "a-model-nobody-has-probed"); r.Effective() != ReasoningUnknown {
			t.Errorf("kind %q answered %s for an unprobed model", kind, r)
		}
	}
}

func TestReasoningLevelsAreCopied(t *testing.T) {
	c := Default()
	first := c.Reasoning("qwen", "qwen3.8-max-preview")
	if len(first.Levels) == 0 {
		t.Fatal("no levels to mutate")
	}
	first.Levels[0] = "clobbered"

	second := c.Reasoning("qwen", "qwen3.8-max-preview")
	if second.Levels[0] != "low" {
		t.Errorf("Levels[0] = %q; a caller mutated the shared catalog", second.Levels[0])
	}
}

func TestZeroReasoningReadsUnknown(t *testing.T) {
	var r Reasoning
	if r.Effective() != ReasoningUnknown {
		t.Errorf("zero Reasoning reads %q, want %q", r.Effective(), ReasoningUnknown)
	}
	if r.Known() {
		t.Error("zero Reasoning reports Known(); the safe branch must be the default")
	}
	if r.String() != "unknown" {
		t.Errorf("String() = %q", r.String())
	}
}

// ---------------------------------------------------------------------------
// UnverifiedModels
// ---------------------------------------------------------------------------

func TestUnverifiedModels(t *testing.T) {
	c := Default()
	got := c.UnverifiedModels()

	if len(got) == 0 {
		t.Fatal("UnverifiedModels is empty; only one capability was probed on 2026-07-28")
	}
	if want := len(c.Models()) - 1; len(got) != want {
		t.Errorf("UnverifiedModels has %d entries, want %d", len(got), want)
	}

	// Accurate in both directions: every listed entry is genuinely unverified,
	// and every unverified entry is listed.
	listed := map[string]bool{}
	for _, line := range got {
		listed[line] = true
	}
	for _, ref := range c.Models() {
		line := "kind=" + ref.Kind + " model=" + ref.Model
		unverified := !c.Reasoning(ref.Kind, ref.Model).Known()
		if unverified != listed[line] {
			t.Errorf("%s: unverified=%v but listed=%v", line, unverified, listed[line])
		}
	}

	// The one probed model is absent.
	for _, line := range got {
		if strings.Contains(line, "qwen3.8-max-preview") {
			t.Errorf("verified model still listed: %q", line)
		}
	}

	// Stable across calls, so an operator can diff two runs.
	if !slices.Equal(got, c.UnverifiedModels()) {
		t.Error("UnverifiedModels is not deterministic")
	}
}

func TestUnverifiedModelsShrinksAsCapabilitiesAreFilledIn(t *testing.T) {
	c := loadWith(t, `
version: 1
models:
  - kind: qwen
    model: "qwen3.7-max"
    reasoning:
      capability: effort_scale
      levels: [low, high, xhigh]
      default_level: xhigh
      verified: 2026-07-28
      note: operator probed this endpoint
`)

	if before, after := len(Default().UnverifiedModels()), len(c.UnverifiedModels()); after != before-1 {
		t.Errorf("unverified count went %d -> %d, want one fewer", before, after)
	}
	for _, line := range c.UnverifiedModels() {
		if strings.Contains(line, "qwen3.7-max") {
			t.Errorf("still listed after being verified: %q", line)
		}
	}
}

// ---------------------------------------------------------------------------
// Operator overlay
// ---------------------------------------------------------------------------

func TestOperatorOverlayOverridesEmbedded(t *testing.T) {
	c := loadWith(t, `
version: 1
kind_aliases:
  acme-cloud: acme
kinds:
  ollama-cloud:
    cache: openai_cache_key
  acme:
    api: openai-chat
    cache: none
    supports_tools: true
    supports_streaming: true
models:
  - kind: ollama-cloud
    model: "gemma4:31b"
    context_window: 131072
    reasoning:
      capability: thinking_flag
      verified: 2026-07-28
  - kind: acme
    model: "acme-1:preview"
    verified: 2026-07-28
`)

	t.Run("field-level kind merge", func(t *testing.T) {
		kd, ok := c.Kind("ollama-cloud")
		if !ok {
			t.Fatal("ollama-cloud vanished")
		}
		if kd.Cache != CacheOpenAIKey {
			t.Errorf("Cache = %q, want the overlay value", kd.Cache)
		}
		if kd.API != APIOpenAIChat {
			t.Errorf("API = %q; untouched embedded fields must survive", kd.API)
		}
		if !kd.SupportsStreaming {
			t.Error("SupportsStreaming lost; the overlay only set cache")
		}
	})

	t.Run("new kind and alias", func(t *testing.T) {
		kd, ok := c.Kind("acme-cloud")
		if !ok || kd.Name != "acme" {
			t.Fatalf("Kind(acme-cloud) = %q, %v", kd.Name, ok)
		}
		if !c.Model("acme-cloud", "acme-1:preview").ModelKnown {
			t.Error("model added on a new kind is not found through its alias")
		}
	})

	t.Run("model override", func(t *testing.T) {
		got := c.Model("ollama-cloud", "gemma4:31b")
		if got.ContextWindow != 131072 {
			t.Errorf("ContextWindow = %d, want the overlay value", got.ContextWindow)
		}
		if got.Verified != "2026-07-28" {
			t.Errorf("Verified = %q; the embedded date should survive a partial override",
				got.Verified)
		}
		if got.Reasoning.Effective() != ReasoningThinkingFlag {
			t.Errorf("Reasoning = %s, want thinking_flag", got.Reasoning)
		}
	})

	t.Run("embedded default untouched", func(t *testing.T) {
		d := Default()
		if kd, _ := d.Kind("ollama-cloud"); kd.Cache != CacheNone {
			t.Errorf("embedded ollama-cloud cache is now %q; an overlay mutated the default", kd.Cache)
		}
		if d.Reasoning("ollama-cloud", "gemma4:31b").Effective() != ReasoningUnknown {
			t.Error("embedded gemma4:31b reasoning changed; an overlay mutated the default")
		}
		if _, ok := d.Kind("acme"); ok {
			t.Error("overlay kind leaked into the default catalog")
		}
	})
}

func TestOverlayOrderLastWins(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.yaml")
	second := filepath.Join(dir, "b.yaml")
	for path, body := range map[string]string{
		first:  "version: 1\nkinds:\n  ollama-cloud:\n    cache: openai_cache_key\n",
		second: "version: 1\nkinds:\n  ollama-cloud:\n    cache: openrouter_cache\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	c, err := Load(first, second)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if kd, _ := c.Kind("ollama-cloud"); kd.Cache != CacheOpenRouter {
		t.Errorf("Cache = %q, want the last file's value", kd.Cache)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load of a missing file succeeded")
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestCapabilityWithoutVerifiedDateIsRejected(t *testing.T) {
	mustLoadFail(t, `
version: 1
models:
  - kind: glm
    model: "glm-5.2"
    reasoning:
      capability: thinking_flag
`, "needs a verified date")
}

func TestUnknownCapabilityWithDateIsRejected(t *testing.T) {
	mustLoadFail(t, `
version: 1
models:
  - kind: glm
    model: "glm-5.2"
    reasoning:
      capability: unknown
      verified: 2026-07-28
`, "nothing was")
}

func TestEffortScaleNeedsLevels(t *testing.T) {
	mustLoadFail(t, `
version: 1
models:
  - kind: glm
    model: "glm-5.2"
    reasoning:
      capability: effort_scale
      verified: 2026-07-28
`, "levels")
}

func TestDefaultLevelMustBeDeclared(t *testing.T) {
	mustLoadFail(t, `
version: 1
models:
  - kind: glm
    model: "glm-5.2"
    reasoning:
      capability: effort_scale
      levels: [low, high]
      default_level: xhigh
      verified: 2026-07-28
`, "default_level")
}

func TestLevelsOnlyForEffortScale(t *testing.T) {
	mustLoadFail(t, `
version: 1
models:
  - kind: glm
    model: "glm-5.2"
    reasoning:
      capability: thinking_flag
      levels: [low, high]
      verified: 2026-07-28
`, "levels are only meaningful")
}

func TestValidationRejectsBadData(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"unknown kind on a model", `
version: 1
models:
  - { kind: no-such-kind, model: "m" }
`, "unknown kind"},
		{"unknown api", `
version: 1
kinds:
  weird: { api: carrier-pigeon }
`, "unknown api"},
		{"unknown cache", `
version: 1
kinds:
  weird: { api: echo, cache: magic }
`, "unknown cache"},
		{"unknown reasoning shape", `
version: 1
kinds:
  weird: { api: echo, reasoning: vibes }
`, "unknown reasoning shape"},
		{"api missing", `
version: 1
kinds:
  weird: { cache: none }
`, "api is required"},
		{"misspelled field", `
version: 1
kinds:
  glm: { contxt_window: 100 }
`, "contxt_window"},
		{"bad date", `
version: 1
models:
  - { kind: glm, model: "glm-5.2", verified: "yesterday" }
`, "YYYY-MM-DD"},
		{"unsupported version", `
version: 99
`, "unsupported version"},
		{"model without a kind", `
version: 1
models:
  - { model: "orphan" }
`, "needs both kind and model"},
		{"alias shadows a kind", `
version: 1
kind_aliases: { glm: openai }
`, "also a declared kind"},
		{"bad price", `
version: 1
models:
  - kind: glm
    model: "glm-5.2"
    pricing: { currency: USD, input_per_mtok: "1.2.3" }
`, "not a decimal"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { mustLoadFail(t, tc.body, tc.want) })
	}
}

// TestValidationReportsEveryProblem: an operator fixing a file should see the
// whole list, not one defect per attempt.
func TestValidationReportsEveryProblem(t *testing.T) {
	_, err := Load(writeOverlay(t, "overlay.yaml", `
version: 1
models:
  - { kind: no-such-kind, model: "a" }
  - { kind: another-missing-kind, model: "b" }
`))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "no-such-kind") ||
		!strings.Contains(err.Error(), "another-missing-kind") {
		t.Errorf("error reports only some problems: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Pricing hints
// ---------------------------------------------------------------------------

func TestPricingHintsAreExactAndAbsentByDefault(t *testing.T) {
	d := Default()
	for _, ref := range d.Models() {
		if p := d.Model(ref.Kind, ref.Model).Pricing; !p.IsZero() {
			t.Errorf("%s/%s carries a price hint, but no prices were verified "+
				"in the 2026-07-28 pass", ref.Kind, ref.Model)
		}
	}

	// Unquoted decimals keep their literal text: 0.10 must not become "0.1".
	c := loadWith(t, `
version: 1
models:
  - kind: glm
    model: "glm-5.2"
    pricing:
      currency: USD
      input_per_mtok: 0.10
      output_per_mtok: "2.20"
      verified: 2026-07-28
`)
	p := c.Model("glm", "glm-5.2").Pricing
	if p.InputPerMTok != "0.10" {
		t.Errorf("InputPerMTok = %q, want the literal %q", p.InputPerMTok, "0.10")
	}
	if p.OutputPerMTok != "2.20" {
		t.Errorf("OutputPerMTok = %q, want %q", p.OutputPerMTok, "2.20")
	}
	if p.Currency != "USD" {
		t.Errorf("Currency = %q", p.Currency)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// TestConcurrentUse: a catalog is read from every request path, so a data race
// here would be a race everywhere.
func TestConcurrentUse(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := Default()
			for j := 0; j < 100; j++ {
				c.Kind("dashscope")
				c.Kinds()
				c.Model("ollama-cloud", "gemma4:31b")
				c.Model("xai", "grok-4.9")
				r := c.Reasoning("qwen", "qwen3.8-max-preview")
				if len(r.Levels) > 0 {
					r.Levels[0] = "mutate a copy, not the catalog"
				}
				c.UnverifiedModels()
			}
		}()
	}
	wg.Wait()

	if got := Default().Reasoning("qwen", "qwen3.8-max-preview").Levels[0]; got != "low" {
		t.Errorf("Levels[0] = %q after concurrent readers", got)
	}
}
