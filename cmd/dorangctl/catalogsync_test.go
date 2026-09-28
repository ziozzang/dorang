package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ziozzang/dorang/pkg/catalog"
)

const syncTestKey = "sync-test-key-7f3a9c"

// fakeOllama serves a /v1/models listing and /api/show, and counts the metadata
// reads so a test can hold sync to "only ask for what is missing".
type fakeOllama struct {
	srv   *httptest.Server
	shows atomic.Int64
}

func newFakeOllama(t *testing.T) *fakeOllama {
	t.Helper()
	f := &fakeOllama{}
	listed := []string{"deepseek-v4.1-flash", "glm-5.2", "glm-5.3", "gpt-oss:120b", "gpt-oss:20b", "newcomer-7"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+syncTestKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			data := make([]map[string]string, 0, len(listed))
			for _, m := range listed {
				data = append(data, map[string]string{"id": m, "object": "model"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case r.Method == http.MethodPost && r.URL.Path == "/api/show":
			f.shows.Add(1)
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch body.Model {
			case "deepseek-v4-flash:0731":
				http.Error(w, `{"error":"model retired"}`, http.StatusGone)
			case "ghost-model":
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			case "newcomer-7":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"model_info":   map[string]any{"newarch.context_length": 131072},
					"capabilities": []string{"completion", "tools"},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"model_info":   map[string]any{"arch.context_length": 262144},
					"capabilities": []string{"completion"},
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// syncFixture writes a configuration routing to the fake provider and a
// models.dev registry file matched to it by URL.
func syncFixture(t *testing.T, f *fakeOllama) (cfgPath, registry string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SYNC_TEST_KEY", syncTestKey)
	cfgPath = writeCfg(t, dir, fmt.Sprintf(`providers:
  - {name: ollama, kind: ollama-cloud, base_url: "%s/v1"}
credentials:
  - {id: ollama-1, provider: ollama, key_env: SYNC_TEST_KEY}
models:
  - name: a
    deployments:
      - {provider: ollama, upstream_model: "deepseek-v4.1-flash", credentials: [ollama-1]}
  - name: b
    deployments:
      - {provider: ollama, upstream_model: "deepseek-v4-flash:0731", credentials: [ollama-1]}
  - name: c
    deployments:
      - {provider: ollama, upstream_model: "deepseek-v4-pro", credentials: [ollama-1]}
  - name: d
    deployments:
      - {provider: ollama, upstream_model: "ghost-model", credentials: [ollama-1], enabled: false}
  - name: e
    deployments:
      - {provider: ollama, upstream_model: "glm-5.2", credentials: [ollama-1]}
  - name: f
    deployments:
      - {provider: ollama, upstream_model: "gpt-oss:20b", credentials: [ollama-1]}
`, f.srv.URL))

	reg := map[string]any{
		"ollama-cloud": map[string]any{
			"api": f.srv.URL + "/v1/",
			"models": map[string]any{
				"newcomer-7": map[string]any{"limit": map[string]int{"context": 1, "output": 8192}},
				"glm-5.3":    map[string]any{"limit": map[string]int{"context": 999, "output": 777}, "tool_call": true},
			},
		},
		// Same vendor, different endpoint: must not be matched.
		"ollama-other": map[string]any{
			"api":    "https://elsewhere.example/v1",
			"models": map[string]any{"newcomer-7": map[string]any{"limit": map[string]int{"output": 1}}},
		},
	}
	registry = filepath.Join(dir, "models.dev.json")
	b, _ := json.Marshal(reg)
	if err := os.WriteFile(registry, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, registry
}

// reportLine returns the report row for one configured model.
func reportLine(t *testing.T, out, model string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(model) + `(\s|\s\(disabled\)).*$`)
	line := re.FindString(out)
	if line == "" {
		t.Fatalf("no report row for %q:\n%s", model, out)
	}
	return line
}

// TestCatalogSyncReportsWhatTheProviderSays is the operator's view: which of
// the configured models are retired, which are unlisted but still served, which
// have a newer version listed — the manual pass of 2026-09-28, as one command.
func TestCatalogSyncReportsWhatTheProviderSays(t *testing.T) {
	f := newFakeOllama(t)
	cfg, reg := syncFixture(t, f)

	out, errOut, code := invoke("catalog", "sync", "--config", cfg, "--models-dev", reg)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	cases := []struct{ model, want string }{
		{"deepseek-v4.1-flash", "listed"},
		// Retired by the provider's own answer, with the successor it lists.
		{"deepseek-v4-flash:0731", "RETIRED"},
		{"deepseek-v4-flash:0731", "newer listed: deepseek-v4.1-flash"},
		// Off the listing but still answering: absence from a listing is not
		// retirement.
		{"deepseek-v4-pro", "unlisted, still served"},
		{"ghost-model", "absent"},
		{"ghost-model", "(disabled)"},
		{"glm-5.2", "newer listed: glm-5.3"},
	}
	for _, c := range cases {
		if line := reportLine(t, out, c.model); !strings.Contains(line, c.want) {
			t.Errorf("row for %s lacks %q: %q", c.model, c.want, line)
		}
	}
	// A size tag is not a version: 120b is not the successor of 20b.
	if line := reportLine(t, out, "gpt-oss:20b"); strings.Contains(line, "newer") {
		t.Errorf("a size tag was read as a version: %q", line)
	}
	if !strings.Contains(out, "listed but not configured: ") || !strings.Contains(out, "newcomer-7") {
		t.Errorf("a listed, unconfigured model is not reported:\n%s", out)
	}
	if !strings.Contains(out, "2 configured model(s) are retired or absent") {
		t.Errorf("the retirement summary is missing:\n%s", out)
	}
	if strings.Contains(out+errOut, syncTestKey) {
		t.Fatal("the credential appeared in the output")
	}
}

// TestCatalogSyncOverlayFillsOnlyWhatIsMissing: new models become entries,
// provider-published metadata wins over the registry, the registry fills the
// rest, a value declared at the model layer is never overwritten, and a
// retired model is never written.
//
// Revert check: drop the missingFields gate in buildSyncOverlay and glm-5.3's
// pinned window is overwritten with the registry's 999; swap the
// provider/registry precedence and the newcomer-7 context window becomes 1.
func TestCatalogSyncOverlayFillsOnlyWhatIsMissing(t *testing.T) {
	f := newFakeOllama(t)
	cfg, reg := syncFixture(t, f)
	dir := t.TempDir()

	// An operator overlay that already declares gpt-oss:120b fully, and only
	// glm-5.3's window: the partial case is where a fill can overwrite.
	pinned := filepath.Join(dir, "pinned.yaml")
	if err := os.WriteFile(pinned, []byte(`version: 1
models:
  - {kind: ollama-cloud, model: "gpt-oss:120b", context_window: 4242, max_output_tokens: 11, supports_tools: true}
  - {kind: ollama-cloud, model: "glm-5.3", context_window: 5555, note: "a human wrote this"}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "sync.yaml")
	out, errOut, code := invoke("catalog", "sync", "--config", cfg, "--models-dev", reg,
		"--catalog", pinned, "--include-private", "--write", outPath)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "deepseek-v4-flash:0731") {
		t.Error("a retired model was written to the overlay")
	}
	if regexp.MustCompile(`(?m)^\s+verified:`).MatchString(text) {
		t.Error("sync wrote verified:, which only asking can establish")
	}

	var doc syncOverlayDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, m := range doc.Models {
		if m.Model == "gpt-oss:120b" {
			t.Errorf("an entry declared at the model layer was rewritten: %+v", m)
		}
	}

	// The written file is a catalog layer the gateway's loader accepts.
	cat, err := catalog.Load(pinned, outPath)
	if err != nil {
		t.Fatalf("the overlay does not load: %v", err)
	}
	n := cat.Model("ollama-cloud", "newcomer-7")
	if !n.ModelKnown {
		t.Fatal("a newly listed model did not become a catalog entry")
	}
	if n.ContextWindow != 131072 {
		t.Errorf("newcomer-7 context %d: the provider's own value must win over models.dev (1)", n.ContextWindow)
	}
	if n.MaxOutputTokens != 8192 || !n.SupportsTools {
		t.Errorf("newcomer-7 = %+v; want the registry's output ceiling and the provider's tools flag", n)
	}
	if cat.Verification("ollama-cloud", "newcomer-7") == catalog.VerificationVerified {
		t.Error("a listed model came out verified")
	}
	g := cat.Model("ollama-cloud", "glm-5.3")
	if g.ContextWindow != 5555 {
		t.Errorf("glm-5.3 context %d: a window declared at the model layer was overwritten", g.ContextWindow)
	}
	if g.MaxOutputTokens != 777 || !g.SupportsTools {
		t.Errorf("glm-5.3 = out %d tools %v; the undeclared fields should be filled from the registry",
			g.MaxOutputTokens, g.SupportsTools)
	}
	if g.Note != "a human wrote this" {
		t.Errorf("glm-5.3 note %q: sync's provenance replaced a note a human wrote", g.Note)
	}
	d := cat.Model("ollama-cloud", "deepseek-v4.1-flash")
	if d.ContextWindow != 262144 {
		t.Errorf("deepseek-v4.1-flash context %d; want the provider's /api/show value", d.ContextWindow)
	}
	if !strings.Contains(text, "models.dev (citation") {
		t.Error("registry values are not marked as a citation")
	}
	if p := cat.Model("ollama-cloud", "gpt-oss:120b"); p.ContextWindow != 4242 {
		t.Errorf("the operator's pinned window was lost: %d", p.ContextWindow)
	}

	// A second run over the synced catalog asks for no metadata it already has:
	// only the three configured-but-unlisted models are shown again.
	before := f.shows.Load()
	_, errOut, code = invoke("catalog", "sync", "--config", cfg, "--models-dev", reg,
		"--catalog", pinned+","+outPath, "--include-private")
	if code != 0 {
		t.Fatalf("second run exit %d: %s", code, errOut)
	}
	if got := f.shows.Load() - before; got != 3 {
		t.Errorf("second run made %d /api/show calls, want 3 (status checks only)", got)
	}
}

// TestCatalogSyncLeavesPrivateEndpointsOutOfTheOverlay: a LAN backend's model
// names are the operator's local aliases, and writing them under a public kind
// would describe that kind wrongly. The report still covers it.
func TestCatalogSyncLeavesPrivateEndpointsOutOfTheOverlay(t *testing.T) {
	f := newFakeOllama(t) // httptest listens on loopback
	cfg, reg := syncFixture(t, f)
	outPath := filepath.Join(t.TempDir(), "sync.yaml")
	out, errOut, code := invoke("catalog", "sync", "--config", cfg, "--models-dev", reg, "--write", outPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "private endpoint (report only)") {
		t.Errorf("the report does not say the endpoint is private:\n%s", out)
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Error("an overlay was written for a private endpoint without --include-private")
	}
}

// TestCatalogSyncFailsWhenNothingCanBeListed: a rejected credential is not an
// empty provider, and "nothing to write" would be the wrong answer.
func TestCatalogSyncFailsWhenNothingCanBeListed(t *testing.T) {
	f := newFakeOllama(t)
	cfg, reg := syncFixture(t, f)
	t.Setenv("SYNC_TEST_KEY", "wrong-key")
	out, errOut, code := invoke("catalog", "sync", "--config", cfg, "--models-dev", reg)
	if code == 0 {
		t.Fatalf("exit 0 with every listing refused\n%s", out)
	}
	if !strings.Contains(out, "listing failed") || !strings.Contains(errOut, "no provider's model listing could be read") {
		t.Errorf("the failure is not explained\nstdout: %s\nstderr: %s", out, errOut)
	}
}

// TestCatalogSyncUsesTheKindEndpointWhenTheProviderNamesNone: the gateway
// falls back to the kind's catalogued base_url, so sync must too — the live
// openrouter provider declares none and was silently missing from the report.
func TestCatalogSyncUsesTheKindEndpointWhenTheProviderNamesNone(t *testing.T) {
	f := newFakeOllama(t)
	dir := t.TempDir()
	t.Setenv("SYNC_TEST_KEY", syncTestKey)
	cfg := writeCfg(t, dir, `providers:
  - {name: ollama, kind: ollama-cloud}
credentials:
  - {id: ollama-1, provider: ollama, key_env: SYNC_TEST_KEY}
models:
  - name: a
    deployments:
      - {provider: ollama, upstream_model: "deepseek-v4.1-flash", credentials: [ollama-1]}
`)
	kinds := filepath.Join(dir, "kinds.yaml")
	if err := os.WriteFile(kinds, []byte(fmt.Sprintf(`version: 1
kinds:
  ollama-cloud: {base_url: "%s/v1"}
`, f.srv.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := invoke("catalog", "sync", "--config", cfg, "--catalog", kinds, "--models-dev", "off")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "provider ollama (kind ollama-cloud)  "+f.srv.URL+"/v1") {
		t.Errorf("the provider was not synced against its kind's endpoint:\n%s", out)
	}
	if line := reportLine(t, out, "deepseek-v4.1-flash"); !strings.Contains(line, "listed") {
		t.Errorf("row: %q", line)
	}
}

// TestCatalogSyncTakesOnlyFreeOpenRouterModels is the operator's rule: OpenRouter
// is not opened up, so of its hundreds of listed models only the zero-priced
// ones are taken in. A paid model the configuration already routes is still
// reported as listed — the rule narrows what is ADDED, not what is checked.
//
// Revert check: drop the free-only narrowing in syncProvider and the paid and
// negative-priced models appear as new and in the overlay.
func TestCatalogSyncTakesOnlyFreeOpenRouterModels(t *testing.T) {
	type priced struct{ id, prompt, completion string }
	models := []priced{
		{"vendor/free-a:free", "0", "0"},
		{"vendor/zero-b", "0", "0"},
		{"vendor/paid-c", "0.000001", "0.000002"},
		{"openrouter/auto", "-1", "-1"}, // a router: "whatever the chosen model costs"
		{"vendor/paid-configured", "0.000003", "0.000004"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		data := []map[string]any{}
		for _, m := range models {
			data = append(data, map[string]any{"id": m.id,
				"pricing": map[string]string{"prompt": m.prompt, "completion": m.completion}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	t.Setenv("OR_TEST_KEY", "or-test-key")
	cfg := writeCfg(t, dir, fmt.Sprintf(`providers:
  - {name: openrouter, kind: openrouter, base_url: "%s/api/v1"}
credentials:
  - {id: or-1, provider: openrouter, key_env: OR_TEST_KEY}
models:
  - name: p
    deployments:
      - {provider: openrouter, upstream_model: "vendor/paid-configured", credentials: [or-1]}
`, srv.URL))
	outPath := filepath.Join(dir, "sync.yaml")
	out, errOut, code := invoke("catalog", "sync", "--config", cfg, "--models-dev", "off",
		"--include-private", "--write", outPath)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "listed: 2 free model(s) of 5") {
		t.Errorf("the free-only narrowing is not reported:\n%s", out)
	}
	if line := reportLine(t, out, "vendor/paid-configured"); !strings.Contains(line, "listed") {
		t.Errorf("a configured paid model must still be checked against the full listing: %q", line)
	}
	for _, paid := range []string{"vendor/paid-c", "openrouter/auto"} {
		if strings.Contains(out, "not configured: ") && strings.Contains(out[strings.Index(out, "not configured: "):], paid) {
			t.Errorf("paid model %s was suggested:\n%s", paid, out)
		}
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"vendor/free-a:free", "vendor/zero-b"} {
		if !strings.Contains(text, want) {
			t.Errorf("free model %s missing from the overlay", want)
		}
	}
	for _, paid := range []string{"vendor/paid-c", "openrouter/auto", "vendor/paid-configured"} {
		if strings.Contains(text, paid) {
			t.Errorf("paid model %s was written to the overlay", paid)
		}
	}

	// The operator can opt in to the whole listing.
	out, _, code = invoke("catalog", "sync", "--config", cfg, "--models-dev", "off", "--openrouter-paid")
	if code != 0 || !strings.Contains(out, "listed: 5 model(s)") {
		t.Errorf("--openrouter-paid did not take the full listing (exit %d):\n%s", code, out)
	}
}

func TestIsFreePricing(t *testing.T) {
	cases := []struct {
		p    map[string]any
		want bool
	}{
		{map[string]any{"prompt": "0", "completion": "0"}, true},
		{map[string]any{"prompt": "0", "completion": "0", "request": ""}, true},
		{map[string]any{"prompt": 0.0, "completion": 0.0}, true},
		{map[string]any{"prompt": "0", "completion": "0.000001"}, false},
		{map[string]any{"prompt": "-1", "completion": "-1"}, false},
		{map[string]any{"prompt": "free"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isFreePricing(c.p); got != c.want {
			t.Errorf("isFreePricing(%v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestVersionShapeAndNewerListed(t *testing.T) {
	shapes := []struct {
		name, skel string
		ver        []int
	}{
		{"qwen3.8-flash", "qwen#-flash", []int{3, 8}},
		{"deepseek-v4.1-flash", "deepseek-v#-flash", []int{4, 1}},
		{"deepseek-v4-flash:0731", "deepseek-v#-flash", []int{4}},
		{"gpt-oss:120b", "gpt-oss", nil},
		{"kimi-k2.7-code", "kimi-k#-code", []int{2, 7}},
	}
	for _, s := range shapes {
		skel, ver := versionShape(s.name)
		if skel != s.skel || fmt.Sprint(ver) != fmt.Sprint(s.ver) {
			t.Errorf("versionShape(%q) = %q %v, want %q %v", s.name, skel, ver, s.skel, s.ver)
		}
	}

	listed := []string{"glm-5.3", "glm-5.3-flash", "kimi-k3", "kimi-k2.7-code", "minimax-m3",
		"qwen3.8-flash", "qwen3.8-max", "gpt-oss:120b", "gpt-oss:20b", "deepseek-v4-pro", "deepseek-v4-pro:0813"}
	cases := map[string]string{
		"glm-5.2":         "glm-5.3", // not glm-5.3-flash: a different family
		"minimax-m2.7":    "minimax-m3",
		"kimi-k2.6":       "kimi-k3", // not kimi-k2.7-code
		"qwen3.6-flash":   "qwen3.8-flash",
		"qwen3.7-max":     "qwen3.8-max",
		"qwen3.7-plus":    "", // no newer plus
		"gpt-oss:20b":     "", // sizes are not versions
		"glm-5.3":         "", // already newest
		"deepseek-v4-pro": "", // a dated tag of the same version is not newer
	}
	for name, want := range cases {
		if got := newerListed(name, listed); got != want {
			t.Errorf("newerListed(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestIsPrivateEndpoint(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://10.2.2.10:28080/v1":           true,
		"http://127.0.0.1:1234/v1":            true,
		"http://localhost:1234":               true,
		"http://gpu-box:8000/v1":              true, // a bare LAN name
		"https://ollama.com/v1":               false,
		"https://api.z.ai/api/coding/paas/v4": false,
	} {
		if got := isPrivateEndpoint(raw); got != want {
			t.Errorf("isPrivateEndpoint(%q) = %v, want %v", raw, got, want)
		}
	}
}
