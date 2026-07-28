package catalog

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// writeFile drops one file into dir and returns its path.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// loadNoEnv loads paths without the environment layer, so a developer with
// DORANG_CATALOG_PATH set in their shell does not change what a test means.
func loadNoEnv(t *testing.T, paths ...string) *Catalog {
	t.Helper()
	c, err := Loader{Paths: paths}.Load()
	if err != nil {
		t.Fatalf("Load(%v): %v", paths, err)
	}
	return c
}

// findOrigin returns the reported origin of one field, or fails.
func findOrigin(t *testing.T, origins []FieldOrigin, field string) FieldOrigin {
	t.Helper()
	for _, o := range origins {
		if o.Field == field {
			return o
		}
	}
	t.Fatalf("Explain reported no entry for %q; got %v", field, origins)
	return FieldOrigin{}
}

// ---------------------------------------------------------------------------
// Directory loading
// ---------------------------------------------------------------------------

// TestLoadDirectorySortedByName is the contract an operator relies on when
// they name files 10-, 20-, 30-: the order is the one a directory listing
// shows, not the order the filesystem happened to hand back.
func TestLoadDirectorySortedByName(t *testing.T) {
	dir := t.TempDir()
	// Written out of order on purpose.
	writeFile(t, dir, "30-third.yaml", `
version: 1
models:
  - { kind: echo, model: "layered", context_window: 3000 }
`)
	writeFile(t, dir, "10-first.yaml", `
version: 1
models:
  - { kind: echo, model: "layered", context_window: 1000, max_output_tokens: 111 }
`)
	writeFile(t, dir, "20-second.yml", `
version: 1
models:
  - { kind: echo, model: "layered", context_window: 2000 }
`)

	c := loadNoEnv(t, dir)
	info := c.Model("echo", "layered")
	if info.ContextWindow != 3000 {
		t.Errorf("context window %d, want 3000 from the last file by name", info.ContextWindow)
	}
	// The .yml extension counts, and its field survived because nothing later
	// restated it.
	if info.MaxOutputTokens != 111 {
		t.Errorf("max output %d, want 111 merged from the first file", info.MaxOutputTokens)
	}
}

// TestLoadDirectoryIgnoresNonYAML keeps a directory usable as a working
// directory: notes, backups and subdirectories must not break a load.
func TestLoadDirectoryIgnoresNonYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "provider.yaml", `
version: 1
models:
  - { kind: echo, model: "kept", context_window: 1234 }
`)
	writeFile(t, dir, "README.md", "not yaml, and not parseable as yaml either: [")
	writeFile(t, dir, "provider.yaml.bak", "this: is: not: valid: yaml")
	if err := os.Mkdir(filepath.Join(dir, "disabled"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "disabled"), "off.yaml", "version: 99999")

	c := loadNoEnv(t, dir)
	if got := c.Model("echo", "kept").ContextWindow; got != 1234 {
		t.Errorf("context window %d, want 1234", got)
	}
}

// TestLoadDirectoryList checks that several directories compose in argument
// order, which is what lets a deployment layer site defaults under host
// overrides.
func TestLoadDirectoryList(t *testing.T) {
	base, over := t.TempDir(), t.TempDir()
	writeFile(t, base, "a.yaml", `
version: 1
models:
  - { kind: echo, model: "two-dirs", context_window: 1000, max_output_tokens: 100 }
`)
	writeFile(t, over, "a.yaml", `
version: 1
models:
  - { kind: echo, model: "two-dirs", context_window: 9000 }
`)

	c := loadNoEnv(t, base, over)
	info := c.Model("echo", "two-dirs")
	if info.ContextWindow != 9000 || info.MaxOutputTokens != 100 {
		t.Errorf("got window=%d max=%d, want 9000 and 100",
			info.ContextWindow, info.MaxOutputTokens)
	}
}

func TestLoadMixesFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "in-dir.yaml", `
version: 1
models:
  - { kind: echo, model: "mixed", context_window: 100 }
`)
	file := writeFile(t, t.TempDir(), "standalone.yaml", `
version: 1
models:
  - { kind: echo, model: "mixed", context_window: 200 }
`)

	if got := loadNoEnv(t, dir, file).Model("echo", "mixed").ContextWindow; got != 200 {
		t.Errorf("file after directory gave %d, want 200", got)
	}
	if got := loadNoEnv(t, file, dir).Model("echo", "mixed").ContextWindow; got != 100 {
		t.Errorf("directory after file gave %d, want 100", got)
	}
}

func TestLoadMissingDirectoryIsAnError(t *testing.T) {
	_, err := Loader{Paths: []string{filepath.Join(t.TempDir(), "nope")}}.Load()
	if err == nil {
		t.Fatal("a missing path loaded without error")
	}
}

// TestLoadEmptyDirectory: nothing to say is not a problem.
func TestLoadEmptyDirectory(t *testing.T) {
	c := loadNoEnv(t, t.TempDir())
	if len(c.Models()) != len(Default().Models()) {
		t.Error("an empty directory changed the catalog")
	}
}

// ---------------------------------------------------------------------------
// The environment layer
// ---------------------------------------------------------------------------

// TestEnvIsTheLastLayer pins the precedence the whole feature exists for: an
// operator must be able to correct a running deployment without editing the
// configuration file that deployment was started with.
func TestEnvIsTheLastLayer(t *testing.T) {
	cfgDir := t.TempDir()
	writeFile(t, cfgDir, "cfg.yaml", `
version: 1
models:
  - { kind: echo, model: "precedence", context_window: 1000, max_output_tokens: 10 }
`)
	envDir := t.TempDir()
	writeFile(t, envDir, "env.yaml", `
version: 1
models:
  - { kind: echo, model: "precedence", context_window: 2000 }
`)

	t.Setenv(EnvCatalogPath, envDir)
	c, err := Load(cfgDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	info := c.Model("echo", "precedence")
	if info.ContextWindow != 2000 {
		t.Errorf("context window %d, want 2000 from the env layer", info.ContextWindow)
	}
	if info.MaxOutputTokens != 10 {
		t.Errorf("max output %d, want 10 still merged from the config layer", info.MaxOutputTokens)
	}
}

// TestEnvSplitsLikePATH covers the list form, including the empty segments a
// shell leaves behind when a variable is built by concatenation.
func TestEnvSplitsLikePATH(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeFile(t, first, "a.yaml", `
version: 1
models:
  - { kind: echo, model: "env-list", context_window: 1, max_output_tokens: 7 }
`)
	writeFile(t, second, "b.yaml", `
version: 1
models:
  - { kind: echo, model: "env-list", context_window: 2 }
`)

	sep := string(os.PathListSeparator)
	t.Setenv(EnvCatalogPath, sep+first+sep+sep+second+sep)

	if got := EnvPaths(); !slices.Equal(got, []string{first, second}) {
		t.Errorf("EnvPaths() = %v, want the two directories with empties dropped", got)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	info := c.Model("echo", "env-list")
	if info.ContextWindow != 2 || info.MaxOutputTokens != 7 {
		t.Errorf("got window=%d max=%d, want 2 and 7", info.ContextWindow, info.MaxOutputTokens)
	}
}

// TestEnvMissingPathIsAnError: a layer that is silently skipped is the exact
// question this package exists to answer, with no answer.
func TestEnvMissingPathIsAnError(t *testing.T) {
	t.Setenv(EnvCatalogPath, filepath.Join(t.TempDir(), "absent.yaml"))
	if _, err := Load(); err == nil {
		t.Fatal("a missing env path loaded without error")
	} else if !strings.Contains(err.Error(), EnvCatalogPath) {
		t.Errorf("error %v does not name %s", err, EnvCatalogPath)
	}
}

func TestLoaderWithoutEnvVarIgnoresTheEnvironment(t *testing.T) {
	envDir := t.TempDir()
	writeFile(t, envDir, "env.yaml", `
version: 1
models:
  - { kind: echo, model: "ignored", context_window: 5000 }
`)
	t.Setenv(EnvCatalogPath, envDir)

	c, err := Loader{}.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Model("echo", "ignored").ModelKnown {
		t.Error("Loader with no EnvVar still read the environment")
	}
}

// ---------------------------------------------------------------------------
// merge: replace
// ---------------------------------------------------------------------------

// TestMergeReplaceRemovesAField is the reason the mode exists. Under merge,
// an absent key and an unchanged key are the same input, so a wrong inherited
// value cannot be taken back out — only overwritten with another wrong value.
func TestMergeReplaceRemovesAField(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-base.yaml", `
version: 1
models:
  - { kind: echo, model: "removal", context_window: 1000, max_output_tokens: 500 }
`)
	writeFile(t, dir, "20-fix.yaml", `
version: 1
models:
  - kind: echo
    model: "removal"
    merge: replace
    context_window: 900
`)

	info := loadNoEnv(t, dir).Model("echo", "removal")
	if info.ContextWindow != 900 {
		t.Errorf("context window %d, want 900", info.ContextWindow)
	}
	if info.MaxOutputTokens != 0 {
		t.Errorf("max output %d, want 0 (removed); replace did not discard the inherited field",
			info.MaxOutputTokens)
	}
}

// TestMergeIsTheDefault states the other half of the contract explicitly.
func TestMergeIsTheDefault(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-base.yaml", `
version: 1
models:
  - { kind: echo, model: "kept", context_window: 1000, max_output_tokens: 500 }
`)
	writeFile(t, dir, "20-implicit.yaml", `
version: 1
models:
  - { kind: echo, model: "kept", context_window: 900 }
`)
	writeFile(t, dir, "30-explicit.yaml", `
version: 1
models:
  - { kind: echo, model: "kept", merge: merge, context_window: 800 }
`)

	info := loadNoEnv(t, dir).Model("echo", "kept")
	if info.ContextWindow != 800 || info.MaxOutputTokens != 500 {
		t.Errorf("got window=%d max=%d, want 800 and 500", info.ContextWindow, info.MaxOutputTokens)
	}
}

// TestExplicitNullIsNotRemoval: there is one way to remove a value, and it is
// a keyword, not a punctuation mark.
func TestExplicitNullIsNotRemoval(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-base.yaml", `
version: 1
models:
  - { kind: echo, model: "nulled", context_window: 1000, max_output_tokens: 500 }
`)
	writeFile(t, dir, "20-null.yaml", `
version: 1
models:
  - kind: echo
    model: "nulled"
    max_output_tokens: null
`)

	if got := loadNoEnv(t, dir).Model("echo", "nulled").MaxOutputTokens; got != 500 {
		t.Errorf("max output %d, want 500; an explicit null must read as 'not stated'", got)
	}
}

func TestMergeReplaceOnAKind(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-base.yaml", `
version: 1
kinds:
  replaceable:
    api: openai-chat
    cache: openai_cache_key
    max_output_tokens: 4096
    supports_tools: true
`)
	writeFile(t, dir, "20-fix.yaml", `
version: 1
kinds:
  replaceable:
    merge: replace
    api: openai-chat
`)

	kd, ok := loadNoEnv(t, dir).Kind("replaceable")
	if !ok {
		t.Fatal("kind vanished")
	}
	if kd.MaxOutputTokens != 0 {
		t.Errorf("max output %d, want 0 after replace", kd.MaxOutputTokens)
	}
	if kd.SupportsTools {
		t.Error("supports_tools survived a replace")
	}
	if kd.Cache != CacheNone {
		t.Errorf("cache %q, want the package default after replace", kd.Cache)
	}
}

// TestMergeReplaceOnAKindMustRestateAPI: replace really does discard
// everything, including the fields that are required.
func TestMergeReplaceOnAKindMustRestateAPI(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-base.yaml", `
version: 1
kinds:
  headless:
    api: openai-chat
`)
	writeFile(t, dir, "20-broken.yaml", `
version: 1
kinds:
  headless:
    merge: replace
    note: forgot the api
`)
	_, err := Loader{Paths: []string{dir}}.Load()
	if err == nil || !strings.Contains(err.Error(), "api is required") {
		t.Fatalf("error = %v, want a complaint that api is required", err)
	}
}

func TestDocumentLevelMergeMode(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-base.yaml", `
version: 1
models:
  - { kind: echo, model: "doc-a", context_window: 1000, max_output_tokens: 500 }
  - { kind: echo, model: "doc-b", context_window: 1000, max_output_tokens: 500 }
`)
	writeFile(t, dir, "20-doc-replace.yaml", `
version: 1
merge: replace
models:
  - { kind: echo, model: "doc-a", context_window: 900 }
  - { kind: echo, model: "doc-b", merge: merge, context_window: 900 }
`)

	c := loadNoEnv(t, dir)
	if got := c.Model("echo", "doc-a").MaxOutputTokens; got != 0 {
		t.Errorf("doc-a max output %d, want 0 from the document default", got)
	}
	if got := c.Model("echo", "doc-b").MaxOutputTokens; got != 500 {
		t.Errorf("doc-b max output %d, want 500; the entry opted back into merge", got)
	}
}

func TestUnknownMergeModeIsRejected(t *testing.T) {
	mustLoadFail(t, `
version: 1
models:
  - { kind: echo, model: "bad-mode", merge: clobber }
`, "merge must be")
}

// ---------------------------------------------------------------------------
// Overlays add, they do not only patch
// ---------------------------------------------------------------------------

// TestOverlayAddsAnUnknownKindAndModel is the headline claim: an operator can
// run dorang against a provider and a model this build has never heard of,
// using configuration alone.
func TestOverlayAddsAnUnknownKindAndModel(t *testing.T) {
	c := loadNoEnv(t, writeOverlay(t, "new-provider.yaml", `
version: 1
kind_aliases:
  nebula-ai: nebula
kinds:
  nebula:
    api: anthropic-messages
    base_url: https://api.nebula.example/v1
    cache: anthropic_cache_control
    category: chat
    supports_tools: true
    supports_streaming: true
prefix_rules:
  - kind: nebula
    prefix: "nebula-"
    context_window: 64000
models:
  - kind: nebula
    model: "nebula-quasar:1m"
    context_window: 1000000
    max_output_tokens: 65536
    reasoning:
      capability: thinking_budget
      min_budget_tokens: 1024
      max_budget_tokens: 32000
      verified: 2026-07-28
`))

	if _, ok := Default().Kind("nebula"); ok {
		t.Fatal("the embedded data already knows this kind; the test proves nothing")
	}

	kd, ok := c.Kind("nebula")
	if !ok {
		t.Fatal("overlay-declared kind does not resolve")
	}
	if kd.API != APIAnthropicMessages || kd.BaseURL != "https://api.nebula.example/v1" {
		t.Errorf("kind = %+v, want the overlay's api and base URL", kd)
	}

	// The alias the overlay declared resolves too.
	if kd, ok := c.Kind("nebula-ai"); !ok || kd.Name != "nebula" {
		t.Errorf("Kind(\"nebula-ai\") = %q, %v; want nebula, true", kd.Name, ok)
	}

	// The listed model composes across all three layers.
	info := c.Model("nebula", "nebula-quasar:1m")
	if !info.KindKnown || !info.ModelKnown {
		t.Errorf("layers = %v, want kind and model both known", info.Layers)
	}
	if info.Model != "nebula-quasar:1m" {
		t.Errorf("model name came back as %q", info.Model)
	}
	if info.ContextWindow != 1000000 || info.MaxOutputTokens != 65536 {
		t.Errorf("got window=%d max=%d", info.ContextWindow, info.MaxOutputTokens)
	}
	if info.Reasoning.Effective() != ReasoningThinkingBudget || !info.Reasoning.Known() {
		t.Errorf("reasoning = %v, want a verified thinking_budget", info.Reasoning)
	}

	// An unlisted member of the family gets the overlay's prefix defaults and
	// stays unlisted, exactly as an embedded prefix rule would behave.
	unlisted := c.Model("nebula-ai", "nebula-pulsar-3")
	if unlisted.ModelKnown {
		t.Error("a prefix rule made an unlisted model count as listed")
	}
	if unlisted.ContextWindow != 64000 {
		t.Errorf("prefix default window %d, want 64000", unlisted.ContextWindow)
	}
	if unlisted.Reasoning.Effective() != ReasoningUnknown {
		t.Error("an overlay prefix rule supplied a reasoning capability")
	}

	// And the embedded data is untouched.
	if _, ok := Default().Kind("nebula"); ok {
		t.Error("the overlay leaked into the shared default catalog")
	}
}

func TestOverlayAddsAModelToAnEmbeddedKind(t *testing.T) {
	c := loadNoEnv(t, writeOverlay(t, "extra.yaml", `
version: 1
models:
  - { kind: ollama-cloud, model: "brand-new-model:cloud", context_window: 42000 }
`))
	info := c.Model("ollama-cloud", "brand-new-model:cloud")
	if !info.ModelKnown || info.ContextWindow != 42000 {
		t.Errorf("overlay model = %+v", info)
	}
	if info.API != APIOpenAIChat {
		t.Errorf("api = %q; the embedded kind defaults did not apply", info.API)
	}
}

// ---------------------------------------------------------------------------
// Provenance
// ---------------------------------------------------------------------------

// TestExplainAcrossThreeLayers is the "did my overlay apply?" question, asked
// of a stack deep enough that guessing would not work: embedded data, a file,
// and the environment, with each winning a different field.
func TestExplainAcrossThreeLayers(t *testing.T) {
	fileDir := t.TempDir()
	filePath := writeFile(t, fileDir, "site.yaml", `
version: 1
models:
  - { kind: qwen, model: "qwen3.8-max-preview", max_output_tokens: 40000 }
`)
	envDir := t.TempDir()
	envPath := writeFile(t, envDir, "hotfix.yaml", `
version: 1
models:
  - { kind: qwen, model: "qwen3.8-max-preview", context_window: 700000 }
`)

	t.Setenv(EnvCatalogPath, envDir)
	c, err := Load(filePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	origins := c.Explain("qwen", "qwen3.8-max-preview")

	ctx := findOrigin(t, origins, FieldContextWindow)
	if ctx.Origin != OriginEnv || ctx.Source != envPath || ctx.Layer != LayerModel {
		t.Errorf("context_window origin = %+v, want the env file at the model layer", ctx)
	}
	if ctx.Value != "700000" {
		t.Errorf("context_window value = %q, want 700000", ctx.Value)
	}

	max := findOrigin(t, origins, FieldMaxOutputTokens)
	if max.Origin != OriginFile || max.Source != filePath {
		t.Errorf("max_output_tokens origin = %+v, want the config file", max)
	}

	api := findOrigin(t, origins, FieldAPI)
	if api.Origin != OriginEmbedded || api.Source != "provider_defaults.yaml" || api.Layer != LayerKind {
		t.Errorf("api origin = %+v, want the embedded kind defaults", api)
	}

	reasoning := findOrigin(t, origins, FieldReasoning)
	if reasoning.Origin != OriginEmbedded || reasoning.Source != "model_catalog.yaml" {
		t.Errorf("reasoning origin = %+v, want the embedded model catalog", reasoning)
	}

	pricing := findOrigin(t, origins, FieldPricing)
	if pricing.Declared() {
		t.Errorf("pricing origin = %+v, want undeclared", pricing)
	}
}

// TestExplainReportsUndeclaredDistinctly: zero and undeclared are different
// answers, and an operator debugging a context window needs to tell them
// apart (DESIGN §4.3).
func TestExplainReportsUndeclaredDistinctly(t *testing.T) {
	c := Default()
	origins := c.Explain("echo", "never-heard-of-it")

	ctx := findOrigin(t, origins, FieldContextWindow)
	if ctx.Declared() {
		t.Errorf("context_window = %+v, want undeclared for an unlisted model", ctx)
	}
	if !strings.Contains(ctx.String(), "undeclared") {
		t.Errorf("String() = %q, want it to say undeclared", ctx.String())
	}

	// The kind layer still answers for the fields it owns.
	if api := findOrigin(t, origins, FieldAPI); !api.Declared() {
		t.Errorf("api = %+v, want the kind's declaration", api)
	}
}

// TestExplainOnPrefixLayer covers the middle layer, which is the one an
// operator is least likely to suspect.
func TestExplainOnPrefixLayer(t *testing.T) {
	c := Default()
	origins := c.Explain("xai", "grok-4.9-does-not-exist-yet")

	ctx := findOrigin(t, origins, FieldContextWindow)
	if ctx.Layer != LayerPrefix {
		t.Errorf("context_window layer = %q, want %q", ctx.Layer, LayerPrefix)
	}
	if ctx.Origin != OriginEmbedded || ctx.Source != "provider_defaults.yaml" {
		t.Errorf("context_window origin = %+v, want the embedded prefix rule", ctx)
	}
}

func TestExplainCoversEveryModelInfoField(t *testing.T) {
	got := Default().Explain("qwen", "qwen3.8-max-preview")
	if len(got) != len(modelInfoFields) {
		t.Fatalf("Explain returned %d fields, want %d", len(got), len(modelInfoFields))
	}
	for i, f := range modelInfoFields {
		if got[i].Field != f {
			t.Errorf("field %d = %q, want %q; the order is part of the contract", i, got[i].Field, f)
		}
	}
}

func TestExplainKindAndAliasOrigin(t *testing.T) {
	c := Default()

	origins, ok := c.ExplainKind("dashscope") // an alias
	if !ok {
		t.Fatal("ExplainKind did not resolve an alias")
	}
	api := findOrigin(t, origins, FieldAPI)
	if api.Origin != OriginEmbedded || api.Layer != LayerKind {
		t.Errorf("api origin = %+v", api)
	}

	// The package default is distinguishable from a file that says the same
	// thing: the echo kind never declares a base URL.
	base := findOrigin(t, origins, FieldBaseURL)
	if base.Source == "" && base.Origin != OriginNone {
		t.Errorf("base_url origin = %+v", base)
	}

	if _, ok := c.ExplainKind("no-such-kind"); ok {
		t.Error("ExplainKind resolved an unknown kind")
	}

	target, origin, ok := c.AliasOrigin("dashscope")
	if !ok || target != "qwen" {
		t.Errorf("AliasOrigin(dashscope) = %q, %v", target, ok)
	}
	if origin.Origin != OriginEmbedded || origin.Source != "provider_defaults.yaml" {
		t.Errorf("alias origin = %+v", origin)
	}
	if _, _, ok := c.AliasOrigin("qwen"); ok {
		t.Error("AliasOrigin treated a declared kind as an alias")
	}
}

// TestPackageDefaultIsNotAFile: "dorang assumed chat" and "my file says chat"
// must not read the same.
func TestPackageDefaultIsNotAFile(t *testing.T) {
	c := loadNoEnv(t, writeOverlay(t, "minimal.yaml", `
version: 1
kinds:
  bare:
    api: openai-chat
`))
	origins, ok := c.ExplainKind("bare")
	if !ok {
		t.Fatal("kind missing")
	}
	cache := findOrigin(t, origins, FieldCache)
	if cache.Origin != OriginDefault {
		t.Errorf("cache origin = %+v, want %q", cache, OriginDefault)
	}
	if cache.Source != "" {
		t.Errorf("a package default names a file: %q", cache.Source)
	}
	if !cache.Declared() {
		t.Error("a package default should still count as declared")
	}
}

// ---------------------------------------------------------------------------
// Validate and LintFiles
// ---------------------------------------------------------------------------

// TestEmbeddedDataValidatesClean is a standing check on the shipped data: the
// rules Validate enforces are ones the embedded files already satisfy, so a
// non-empty result here is a data regression.
func TestEmbeddedDataValidatesClean(t *testing.T) {
	for _, p := range Default().Validate() {
		t.Errorf("embedded data: %s", p)
	}
}

func TestExampleOverlaysValidateClean(t *testing.T) {
	for _, path := range []string{
		"model_catalog.overlay.example.yaml",
		"catalog.d.example",
	} {
		for _, p := range LintFiles(path) {
			t.Errorf("%s: %s", path, p)
		}
	}
}

// TestValidateReportsEveryProblemAtOnce: one pass, the whole list. An
// operator fixing a catalog should not have to discover its faults one
// restart at a time.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	c := loadNoEnv(t, writeOverlay(t, "suspect.yaml", `
version: 1
kinds:
  suspect:
    api: openai-chat
    context_window: 1000
    max_output_tokens: 5000
    verified: 2099-01-01
  suspect-embed:
    api: jina
    category: embedding
    supports_tools: true
prefix_rules:
  - prefix: "cross-provider-"
    context_window: 123456
models:
  - kind: suspect
    model: "priced"
    pricing:
      input_per_mtok: "1.00"
      output_per_mtok: "2.00"
`))

	ps := c.Validate()
	if !HasErrors(ps) {
		t.Fatalf("no errors reported; got %v", ps)
	}

	var msgs []string
	for _, p := range ps {
		msgs = append(msgs, p.String())
	}
	joined := strings.Join(msgs, "\n")

	for _, want := range []string{
		"exceeds context_window",       // kind: max output > window
		"is in the future",             // kind: verified date
		"applies to every kind",        // kind-agnostic prefix rule with a limit
		"kind declares context_window", // kind-wide window warning
		"no currency",                  // pricing amounts with no currency
		"carries no verified date",     // pricing with no date
		"neither is meaningful",        // tools on an embedding surface
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Validate did not report %q; got:\n%s", want, joined)
		}
	}

	// Errors sort ahead of warnings, so a truncated report still shows the
	// findings that stop a deployment.
	seenWarning := false
	for _, p := range ps {
		if p.Severity == SeverityWarning {
			seenWarning = true
		} else if seenWarning {
			t.Error("an error was reported after a warning")
		}
	}
}

// TestValidateCatchesTheComposedContradiction: the numbers that reach a
// request come from three layers, so the check has to look at the composition
// and not at any one entry.
func TestValidateCatchesTheComposedContradiction(t *testing.T) {
	c := loadNoEnv(t, writeOverlay(t, "composed.yaml", `
version: 1
kinds:
  wide-output:
    api: openai-chat
    max_output_tokens: 500000
models:
  - { kind: wide-output, model: "small-window", context_window: 8000 }
`))

	var found bool
	for _, p := range c.Validate() {
		if p.Model == "small-window" && strings.Contains(p.Message, "resolved max_output_tokens") {
			found = true
		}
	}
	if !found {
		t.Errorf("the composed contradiction went unreported; got %v", c.Validate())
	}
}

func TestLintFilesReportsAMalformedFileWithoutPanicking(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-good.yaml", `
version: 1
models:
  - { kind: echo, model: "fine" }
`)
	writeFile(t, dir, "20-broken.yaml", "kinds: [this is not a mapping\n")
	writeFile(t, dir, "30-unknown-key.yaml", `
version: 1
kinds:
  typo:
    api: openai-chat
    contxt_window: 1000
`)
	writeFile(t, dir, "40-bad-kind.yaml", `
version: 1
models:
  - { kind: no-such-kind-anywhere, model: "orphan" }
`)

	ps := LintFiles(dir)
	if !HasErrors(ps) {
		t.Fatal("a directory with three broken files linted clean")
	}

	var joined strings.Builder
	for _, p := range ps {
		joined.WriteString(p.String())
		joined.WriteString("\n")
	}
	out := joined.String()

	if !strings.Contains(out, "20-broken.yaml") {
		t.Errorf("the unparseable file was not named:\n%s", out)
	}
	if !strings.Contains(out, "contxt_window") {
		t.Errorf("the misspelled key was not named:\n%s", out)
	}
	if !strings.Contains(out, "no-such-kind-anywhere") {
		t.Errorf("the orphaned model was not reported:\n%s", out)
	}
}

func TestLintFilesReportsAMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.yaml")
	ps := LintFiles(missing)
	if !HasErrors(ps) {
		t.Fatal("a missing path linted clean")
	}
	if ps[0].Source != missing {
		t.Errorf("problem source = %q, want %q", ps[0].Source, missing)
	}
}

func TestLintFilesOnCleanDataIsSilent(t *testing.T) {
	p := writeOverlay(t, "clean.yaml", `
version: 1
models:
  - { kind: echo, model: "quiet", context_window: 1000, max_output_tokens: 100 }
`)
	if ps := LintFiles(p); len(ps) != 0 {
		t.Errorf("clean data produced %v", ps)
	}
}

// TestLintFilesSplitsJoinedErrors keeps a report readable: one finding per
// defect, not one wall of text per load.
func TestLintFilesSplitsJoinedErrors(t *testing.T) {
	p := writeOverlay(t, "many.yaml", `
version: 1
models:
  - { kind: ghost-one, model: "a" }
  - { kind: ghost-two, model: "b" }
  - { kind: ghost-three, model: "c" }
`)
	ps := LintFiles(p)
	if len(ps) < 3 {
		t.Errorf("got %d findings for three defects: %v", len(ps), ps)
	}
}

func TestProblemStringLabelsRatherThanJoins(t *testing.T) {
	p := Problem{
		Severity: SeverityError,
		Source:   "/etc/dorang/x.yaml",
		Kind:     "ollama-cloud",
		Model:    "deepseek-v4-flash:cloud",
		Field:    FieldContextWindow,
		Message:  "something",
	}
	got := p.String()
	for _, want := range []string{"kind=ollama-cloud", "model=deepseek-v4-flash:cloud"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Opaque names, through every new path
// ---------------------------------------------------------------------------

// TestOpaqueNamesSurviveEveryPath runs the four shapes that have historically
// tempted a parser — an Ollama tag, a vendor prefix with a colon, a colon in
// the middle of a name, and a slash — through directory loading, the
// environment layer, merge:replace, Explain, Validate and LintFiles.
func TestOpaqueNamesSurviveEveryPath(t *testing.T) {
	names := []string{
		"gemma4:31b",
		"zai:glm-5.1",
		"deepseek-v4-flash:cloud",
		"vendor/model-1.5",
		"k3[1m]",
		"hf:zai-org/GLM-5.2",
		"accounts/fireworks/models/kimi-k2p6",
	}

	dir := t.TempDir()
	var base, fix strings.Builder
	base.WriteString("version: 1\nkinds:\n  opaque:\n    api: openai-chat\nmodels:\n")
	fix.WriteString("version: 1\nmodels:\n")
	for _, n := range names {
		base.WriteString("  - { kind: opaque, model: \"" + n + "\", context_window: 1000, max_output_tokens: 100 }\n")
		fix.WriteString("  - { kind: opaque, model: \"" + n + "\", merge: replace, context_window: 2000 }\n")
	}
	writeFile(t, dir, "10-base.yaml", base.String())

	envDir := t.TempDir()
	writeFile(t, envDir, "20-fix.yaml", fix.String())
	t.Setenv(EnvCatalogPath, envDir)

	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, n := range names {
		info := c.Model("opaque", n)
		if info.Model != n {
			t.Errorf("model name came back as %q, want %q", info.Model, n)
		}
		if !info.ModelKnown {
			t.Errorf("model %q was not found; something split the name", n)
		}
		if info.ContextWindow != 2000 {
			t.Errorf("model %q window = %d, want 2000 from the env layer", n, info.ContextWindow)
		}
		if info.MaxOutputTokens != 0 {
			t.Errorf("model %q max output = %d, want 0; merge:replace did not apply", n, info.MaxOutputTokens)
		}

		ctx := findOrigin(t, c.Explain("opaque", n), FieldContextWindow)
		if ctx.Origin != OriginEnv {
			t.Errorf("model %q context_window origin = %+v, want the env layer", n, ctx)
		}
	}

	// The names survive the report surfaces too.
	report := strings.Join(c.UnverifiedModels(), "\n")
	for _, n := range names {
		if !strings.Contains(report, "model="+n) {
			t.Errorf("UnverifiedModels lost %q:\n%s", n, report)
		}
	}

	if ps := LintFiles(dir, envDir); len(ps) != 0 {
		t.Errorf("linting opaque names produced %v", ps)
	}

	// Listing keeps them byte-identical, and none of them collides.
	seen := map[string]bool{}
	for _, ref := range c.Models() {
		if ref.Kind != "opaque" {
			continue
		}
		if seen[ref.Model] {
			t.Errorf("duplicate ref for %q", ref.Model)
		}
		seen[ref.Model] = true
	}
	for _, n := range names {
		if !seen[n] {
			t.Errorf("Models() lost %q", n)
		}
	}
}

// TestOpaqueNamesInTheEmbeddedCatalog freezes the real ones that arrived with
// the absorbed data, so a future "normalization" has to break a test first.
func TestOpaqueNamesInTheEmbeddedCatalog(t *testing.T) {
	c := Default()
	for _, tc := range []ModelRef{
		{Kind: "ollama-cloud", Model: "gemma4:31b"},
		{Kind: "ollama-cloud", Model: "deepseek-v4-flash"},
		{Kind: "kimi-coding", Model: "k3[1m]"},
		{Kind: "synthetic", Model: "hf:zai-org/GLM-5.2"},
		{Kind: "fireworks", Model: "accounts/fireworks/models/kimi-k2p6"},
		{Kind: "nvidia", Model: "nvidia/nemotron-3-ultra-550b-a55b"},
		{Kind: "groq", Model: "groq/compound"},
	} {
		info := c.Model(tc.Kind, tc.Model)
		if !info.ModelKnown {
			t.Errorf("kind=%s model=%s is missing", tc.Kind, tc.Model)
		}
		if info.Model != tc.Model {
			t.Errorf("kind=%s model came back as %q", tc.Kind, info.Model)
		}
	}
}

// TestAbsorbedDataCarriesNoReasoning is the C5 guard applied to the mining
// pass: the sources state reasoning shapes per model in abundance, they
// contradict each other, and none of it was written in.
func TestAbsorbedDataCarriesNoReasoning(t *testing.T) {
	c := Default()
	for _, ref := range c.Models() {
		info := c.Model(ref.Kind, ref.Model)
		if info.Reasoning.Effective() == ReasoningUnknown {
			continue
		}
		// Exactly one entry may claim a capability: the one an operator has
		// in production use against that endpoint and that model.
		if ref.Kind != "qwen" || ref.Model != "qwen3.8-max-preview" {
			t.Errorf("kind=%s model=%s claims %v; nothing absorbed may",
				ref.Kind, ref.Model, info.Reasoning)
		}
	}
}

// TestAbsorbedDataCarriesNoPricing: openclaw's manifests carry a full cost
// block for every model, and none of it was taken.
func TestAbsorbedDataCarriesNoPricing(t *testing.T) {
	c := Default()
	for _, ref := range c.Models() {
		if p := c.Model(ref.Kind, ref.Model).Pricing; !p.IsZero() {
			t.Errorf("kind=%s model=%s carries a price hint %+v", ref.Kind, ref.Model, p)
		}
	}
}

// TestNoAbsorbedMaxOutputExceedsItsWindow is the DESIGN §4.3 direction check
// applied across the whole absorbed set at once.
func TestNoAbsorbedMaxOutputExceedsItsWindow(t *testing.T) {
	c := Default()
	for _, ref := range c.Models() {
		info := c.Model(ref.Kind, ref.Model)
		if info.ContextWindow > 0 && info.MaxOutputTokens > info.ContextWindow {
			t.Errorf("kind=%s model=%s: max output %d exceeds window %d",
				ref.Kind, ref.Model, info.MaxOutputTokens, info.ContextWindow)
		}
	}
}

// TestConcurrentLoadAndExplain runs the new read paths under -race alongside
// the old ones; a Catalog is shared and nothing may mutate after construction.
func TestConcurrentLoadAndExplain(t *testing.T) {
	c := loadNoEnv(t, "catalog.d.example")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = c.Explain("acme", "acme-lodestar-2")
				_, _ = c.ExplainKind("acme")
				_ = c.Validate()
				_ = c.Model("acme", "acme-lodestar-2:turbo")
				_ = c.UnverifiedModels()
			}
		}()
	}
	wg.Wait()
}
