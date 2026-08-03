package config

import (
	"strings"
	"testing"
	"time"
)

func warningsContain(ws []Warning, path, want string) bool {
	for _, w := range ws {
		if (path == "" || w.Path == path) && strings.Contains(w.Message, want) {
			return true
		}
	}
	return false
}

func warningStrings(ws []Warning) string {
	var b strings.Builder
	for _, w := range ws {
		b.WriteString("  - " + w.String() + "\n")
	}
	return b.String()
}

// TestImportMapsTheDocumentedShape walks the mapping table of design §2.2.
func TestImportMapsTheDocumentedShape(t *testing.T) {
	src := `
model_list:
  - model_name: chat-large
    litellm_params:
      model: openai/gpt-4o
      custom_llm_provider: openai
      api_base: https://api.openai.com/v1
      api_key: os.environ/OPENAI_KEY_1
      rpm: 100
      tpm: 100000
      max_parallel_requests: 5
      weight: 10
      timeout: 600
      stream_timeout: 60
      input_cost_per_token: 0.0000025
      output_cost_per_token: 0.00001
  - model_name: chat-large
    litellm_params:
      model: gpt-4o
      custom_llm_provider: azure
      api_base: https://eu.azure.invalid
      api_key: os.environ/AZURE_KEY
      weight: 5
  - model_name: chat-small
    litellm_params:
      model: openai/gpt-4o-mini
      custom_llm_provider: openai
      api_base: https://api.openai.com/v1
      api_key: os.environ/OPENAI_KEY_1
litellm_settings:
  drop_params: true
router_settings:
  routing_strategy: least-busy
  num_retries: 3
general_settings:
  master_key: os.environ/PROXY_MASTER_KEY
  database_url: os.environ/DATABASE_URL
  database_connection_pool_limit: 20
`
	c, warnings, err := ImportProxyConfig([]byte(src))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// Repeated model_name entries form one load-balanced group.
	if len(c.Models) != 2 {
		t.Fatalf("models = %d, want 2: %+v", len(c.Models), c.Models)
	}
	group, ok := c.Model("chat-large")
	if !ok {
		t.Fatal("model group chat-large is missing")
	}
	if len(group.Deployments) != 2 {
		t.Fatalf("chat-large deployments = %d, want 2", len(group.Deployments))
	}

	d := group.Deployments[0]
	if d.Provider != "openai" || d.UpstreamModel != "gpt-4o" {
		t.Errorf("deployment 0 = (%q, %q), want (openai, gpt-4o)", d.Provider, d.UpstreamModel)
	}
	if d.Weight != 10 {
		t.Errorf("weight = %d, want 10", d.Weight)
	}
	if d.Timeout.Duration() != 600*time.Second || d.StreamTimeout.Duration() != 60*time.Second {
		t.Errorf("timeouts = (%v, %v), want (600s, 60s)", d.Timeout, d.StreamTimeout)
	}
	wantLimits := []Limit{{MetricRPM, 100}, {MetricTPM, 100000}, {MetricMaxConcurrent, 5}}
	if len(d.Limits) != len(wantLimits) {
		t.Fatalf("limits = %+v, want %+v", d.Limits, wantLimits)
	}
	for i, l := range wantLimits {
		if d.Limits[i] != l {
			t.Errorf("limits[%d] = %+v, want %+v", i, d.Limits[i], l)
		}
	}
	if len(d.Credentials) != 1 {
		t.Fatalf("credentials = %v", d.Credentials)
	}
	cred, ok := c.Credential(d.Credentials[0])
	if !ok {
		t.Fatalf("credential %q is missing", d.Credentials[0])
	}
	if cred.Key.Env != "OPENAI_KEY_1" || cred.Provider != "openai" {
		t.Errorf("credential = %+v, want a key_env of OPENAI_KEY_1 on openai", cred)
	}

	// The same api_key on the same provider is one credential, not two.
	small, _ := c.Model("chat-small")
	if small.Deployments[0].Credentials[0] != cred.ID {
		t.Errorf("the same key produced two credentials: %q and %q",
			cred.ID, small.Deployments[0].Credentials[0])
	}

	// Providers come from the declared hints, with base URLs attached.
	p, ok := c.Provider("openai")
	if !ok || p.BaseURL != "https://api.openai.com/v1" || p.Kind != "openai" {
		t.Errorf("provider openai = %+v", p)
	}
	az, ok := c.Provider("azure")
	if !ok || az.Kind != "azure" || az.BaseURL != "https://eu.azure.invalid" {
		t.Errorf("provider azure = %+v", az)
	}

	// Strategy, retries, drop_params, master key and database.
	if got := group.Strategy; len(got) != 1 || got[0] != "least_busy" {
		t.Errorf("strategy = %v, want [least_busy]", got)
	}
	if p.Retry.MaxAttempts != 3 {
		t.Errorf("retry.max_attempts = %d, want 3", p.Retry.MaxAttempts)
	}
	if !p.Params.DropsUnsupported() {
		t.Error("drop_params was not carried over to params.drop_unsupported")
	}
	if c.Server.MasterKeyEnv != "PROXY_MASTER_KEY" {
		t.Errorf("master_key_env = %q", c.Server.MasterKeyEnv)
	}
	if c.Storage.Driver != "postgres" || c.Storage.Postgres.URLEnv != "DATABASE_URL" {
		t.Errorf("storage = %+v", c.Storage)
	}
	if c.Storage.Postgres.MaxConns != 20 {
		t.Errorf("max_conns = %d, want 20", c.Storage.Postgres.MaxConns)
	}

	// Per-TOKEN costs become per-MILLION-token rates. The source file quotes
	// $2.50 and $10.00 per million the only way it can — as a figure per token —
	// and dorang's rates.input is per million (§13.1c), so the literal cannot
	// come through unchanged. This assertion used to demand that it did.
	if len(c.Pricing.Rules) != 1 {
		t.Fatalf("pricing rules = %+v", c.Pricing.Rules)
	}
	r := c.Pricing.Rules[0]
	if r.Class != PricingMarginalUsage || r.Match.Provider != "openai" || r.Match.Model != "gpt-4o" {
		t.Errorf("pricing rule = %+v", r)
	}
	if r.Rates["input"] != "2.5" || r.Rates["output"] != "10" {
		t.Errorf("rates = %+v, want input 2.5 and output 10 per MILLION tokens. "+
			"The literals from the file (0.0000025, 0.00001) are the same prices per ONE "+
			"token, and in this field they price every request to a millionth (§13.1c)",
			r.Rates)
	}
	if !warningsContain(warnings, "model_list", "were CONVERTED to dorang's quantities") {
		t.Errorf("the rescaling was not reported:\n%s", warningStrings(warnings))
	}

	// The result is a configuration that validates.
	if err := c.Validate(); err != nil {
		t.Errorf("the imported configuration does not validate: %v\nwarnings:\n%s",
			err, warningStrings(warnings))
	}
}

// TestImportKeepsAnUnresolvableProviderPrefix is design §2.1 rule 5: a model
// string is never split to guess a provider.
func TestImportKeepsAnUnresolvableProviderPrefix(t *testing.T) {
	src := `
model_list:
  - model_name: claude
    litellm_params:
      model: bedrock/anthropic.claude-v2:1
      api_key: os.environ/AWS_KEY
`
	c, warnings, err := ImportProxyConfig([]byte(src))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	m, ok := c.Model("claude")
	if !ok {
		t.Fatal("model claude is missing")
	}
	d := m.Deployments[0]
	if d.UpstreamModel != "bedrock/anthropic.claude-v2:1" {
		t.Errorf("upstream_model = %q, want the whole string kept", d.UpstreamModel)
	}
	if d.Provider != "imported" {
		t.Errorf("provider = %q, want the placeholder provider", d.Provider)
	}
	if !warningsContain(warnings, "", "no declared provider matches model") {
		t.Errorf("the unresolved prefix was not reported:\n%s", warningStrings(warnings))
	}
	if !warningsContain(warnings, "", "never split") {
		t.Errorf("the warning does not explain the rule:\n%s", warningStrings(warnings))
	}
	if c.Providers[0].Name != "imported" {
		t.Errorf("providers = %+v", c.Providers)
	}
}

// TestImportResolvesAgainstDeclaredProviders checks the positive case: the
// prefix is stripped only because a provider by that name was declared.
func TestImportResolvesAgainstDeclaredProviders(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		wantProvider string
		wantUpstream string
	}{
		{
			name: "declared by the same entry",
			src: `
model_list:
  - model_name: g
    litellm_params: {model: azure/gpt-4o, custom_llm_provider: azure, api_key: os.environ/K}
`,
			wantProvider: "azure",
			wantUpstream: "gpt-4o",
		},
		{
			name: "declared by another entry",
			src: `
model_list:
  - model_name: a
    litellm_params: {model: anything, custom_llm_provider: vertex_ai, api_key: os.environ/K}
  - model_name: g
    litellm_params: {model: vertex_ai/gemini-2.0, api_key: os.environ/K}
`,
			wantProvider: "vertex_ai",
			wantUpstream: "gemini-2.0",
		},
		{
			name: "no separator at all",
			src: `
model_list:
  - model_name: g
    litellm_params: {model: gpt-4o, custom_llm_provider: openai, api_key: os.environ/K}
`,
			wantProvider: "openai",
			wantUpstream: "gpt-4o",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, err := ImportProxyConfig([]byte(tc.src))
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			m, ok := c.Model("g")
			if !ok {
				t.Fatal("model g is missing")
			}
			d := m.Deployments[0]
			if d.Provider != tc.wantProvider || d.UpstreamModel != tc.wantUpstream {
				t.Errorf("= (%q, %q), want (%q, %q)",
					d.Provider, d.UpstreamModel, tc.wantProvider, tc.wantUpstream)
			}
		})
	}
}

// TestImportNeverSplitsOnColon is the golden set of §2.1, seen through import.
func TestImportNeverSplitsOnColon(t *testing.T) {
	src := `
model_list:
  - model_name: "gemma4:31b"
    litellm_params: {model: "gemma4:31b", custom_llm_provider: ollama, api_base: "http://127.0.0.1:11434"}
  - model_name: "zai:glm-5.1"
    litellm_params: {model: "zai:glm-5.1", custom_llm_provider: glm, api_key: os.environ/GLM_KEY}
  - model_name: "deepseek-v4-flash:cloud"
    litellm_params: {model: "deepseek-v4-flash:cloud", custom_llm_provider: deepseek, api_key: os.environ/DS_KEY}
  - model_name: "qwen3.5:397b"
    litellm_params: {model: "qwen3.5:397b", custom_llm_provider: dashscope, api_key: os.environ/QWEN_KEY}
`
	c, _, err := ImportProxyConfig([]byte(src))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, name := range []string{"gemma4:31b", "zai:glm-5.1", "deepseek-v4-flash:cloud", "qwen3.5:397b"} {
		m, ok := c.Model(name)
		if !ok {
			t.Errorf("model %q is missing", name)
			continue
		}
		if got := m.Deployments[0].UpstreamModel; got != name {
			t.Errorf("upstream_model = %q, want %q", got, name)
		}
	}
	// "zai" is not a provider, and glm was declared: nothing may have been
	// stripped from the front of the name.
	if _, ok := c.Provider("zai"); ok {
		t.Error("a provider was invented by splitting a model name on ':'")
	}
}

// TestImportWarnsAboutALiteralKey checks that a secret in the source file is
// reported rather than quietly carried into a file dorang refuses to load.
func TestImportWarnsAboutALiteralKey(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: g
    litellm_params: {model: gpt-4o, custom_llm_provider: openai, api_key: sk-literal-secret} # pragma: allowlist secret — fixture
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !warningsContain(warnings, "", "literal secret") {
		t.Errorf("a literal api_key was not reported:\n%s", warningStrings(warnings))
	}
	// The literal is not carried at all. What lands in the generated
	// configuration is a key_env reference the operator has to fill in — the
	// importer's output is a file, and DESIGN §4.1 says a file does not hold
	// secrets.
	if len(c.Credentials) != 1 {
		t.Fatalf("credentials = %d, want 1", len(c.Credentials))
	}
	cred := c.Credentials[0]
	if cred.Key.Inline != "" {
		t.Errorf("the literal secret was carried into the imported config: %q", cred.Key.Inline)
	}
	if cred.Key.Env == "" {
		t.Error("no key_env reference was substituted for the literal")
	}
	for _, w := range warnings {
		if strings.Contains(w.Message, "sk-literal-secret") {
			t.Errorf("the warning echoes the secret: %s", w.Message)
		}
	}
}

// TestImportWarnsAboutEverythingItCannotRepresent is the "never a silent drop"
// rule of §2.2.
func TestImportWarnsAboutEverythingItCannotRepresent(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: g
    litellm_params:
      model: gpt-4o
      custom_llm_provider: openai
      api_key: os.environ/K
      api_version: "2024-02-01"
      mock_response: hello
    model_info: {id: abc123, mode: chat}
    tags: [beta]
litellm_settings:
  success_callback: [langfuse]
  cache: true
  max_budget: 100
router_settings:
  allowed_fails: 3
  cooldown_time: 5
  redis_host: localhost
  enable_pre_call_checks: true
general_settings:
  master_key: sk-1234
  alerting: [slack]
  store_model_in_db: true
environment_variables:
  SOME: thing
unknown_section:
  a: b
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, want := range []struct{ path, msg string }{
		{"model_list[0].litellm_params.api_version", "no equivalent"},
		{"model_list[0].litellm_params.mock_response", "no equivalent"},
		{"model_list[0].model_info.id", "not imported"},
		{"model_list[0].model_info.mode", "not imported"},
		{"model_list[0].tags", "not imported"},
		{"litellm_settings.success_callback", "callback integrations"},
		{"litellm_settings.cache", "caching"},
		{"litellm_settings.max_budget", "budgets are configured"},
		{"router_settings.allowed_fails", "circuit-breaker"},
		{"router_settings.cooldown_time", "circuit-breaker"},
		{"router_settings.redis_host", "cluster.redis_url_env"},
		{"router_settings.enable_pre_call_checks", "no equivalent"},
		{"general_settings.master_key", "never stores it in the file"},
		{"general_settings.alerting", "notifications"},
		{"general_settings.store_model_in_db", "no equivalent"},
		{"environment_variables", "process environment"},
		{"unknown_section", "not part of dorang's schema"},
	} {
		if !warningsContain(warnings, want.path, want.msg) {
			t.Errorf("no warning at %s about %q\ngot:\n%s", want.path, want.msg, warningStrings(warnings))
		}
	}
	// A literal master key is not carried over.
	if c.Server.MasterKeyEnv != "DORANG_MASTER_KEY" {
		t.Errorf("a literal master key leaked into the configuration: %q", c.Server.MasterKeyEnv)
	}
}

// TestImportConvertsEveryRateItCarries walks the conversion table of
// priceFieldByKey: every rate key the source format can express, against the
// component and the quantity dorang prices in.
//
// One field copied verbatim across a unit boundary meant the table had never
// been written down, so the test is the table: a key that is added without a
// scale fails here rather than in an operator's ledger.
func TestImportConvertsEveryRateItCarries(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: chat
    litellm_params:
      model: gpt-4o
      custom_llm_provider: openai
      api_key: os.environ/K
      input_cost_per_token: 0.0000025
      output_cost_per_token: 0.00001
      cache_read_input_token_cost: 0.00000025
      cache_creation_input_token_cost: 0.000003125
      output_cost_per_reasoning_token: 0.00002
  - model_name: chars
    litellm_params:
      model: gemini-1.5-pro
      custom_llm_provider: gemini
      api_key: os.environ/G
      input_cost_per_character: 0.000000125
  - model_name: gpu
    litellm_params:
      model: whisper-1
      custom_llm_provider: vllm
      api_base: https://gpu.invalid
      api_key: os.environ/V
      input_cost_per_second: "0.0001"
      input_cost_per_request: "0.002"
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// Per token → per MILLION tokens: $2.50, $10.00, $0.25, $3.125, $20.00.
	want := map[string]map[string]string{
		"openai/gpt-4o": {
			"input":       "2.5",
			"output":      "10",
			"cached_read": "0.25",
			"cache_write": "3.125",
			"reasoning":   "20",
		},
		// Per character → per 1,000 characters: $0.125 per million characters
		// is $0.000125 per thousand.
		"gemini/gemini-1.5-pro": {"characters": "0.000125"},
		// Per second and per request are dorang's own quantities: unchanged.
		"vllm/whisper-1": {"compute_seconds": "0.0001", "request": "0.002"},
	}
	got := map[string]map[string]string{}
	for _, r := range c.Pricing.Rules {
		key := r.Match.Provider + "/" + r.Match.Model
		if got[key] == nil {
			got[key] = map[string]string{}
		}
		for comp, rate := range r.Rates {
			got[key][comp] = string(rate)
		}
	}
	for model, rates := range want {
		for comp, rate := range rates {
			if got[model][comp] != rate {
				t.Errorf("%s rates.%s = %q, want %q\nall: %+v",
					model, comp, got[model][comp], rate, got[model])
			}
		}
	}

	// A rule carries one unit, so the per-second and per-request rates of one
	// model are two rules rather than one that cannot be assembled.
	for _, r := range c.Pricing.Rules {
		if _, ok := PricingUnit(sortedKeys(r.Rates)); !ok {
			t.Errorf("rule %s mixes units: %+v", r.ID, r.Rates)
		}
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the imported configuration does not validate: %v\nwarnings:\n%s",
			err, warningStrings(warnings))
	}
	// And nothing it converted is small enough to look like a per-token figure.
	if a := c.Advisories(); len(a) != 0 {
		t.Errorf("the converted rates still look like per-token figures: %+v", a)
	}
}

// TestImportReadsAFloatPrintersExponent is the shape a rate actually arrives
// in. The source file is written by a program that formats floats, so "2.5e-06"
// is as common as "0.0000025" — and it is a decimal dorang refuses to STORE
// (§13.1c). The importer is the one place that reads it, and it writes the
// digits out.
func TestImportReadsAFloatPrintersExponent(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: chat
    litellm_params:
      model: gpt-4o
      custom_llm_provider: openai
      api_key: os.environ/K
      input_cost_per_token: 2.5e-06
      output_cost_per_token: 1E-5
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	r := c.Pricing.Rules[0]
	if r.Rates["input"] != "2.5" || r.Rates["output"] != "10" {
		t.Errorf("rates = %+v, want input 2.5 and output 10", r.Rates)
	}
	for comp, rate := range r.Rates {
		if err := checkDecimal(string(rate)); err != nil {
			t.Errorf("rates.%s = %q, which this package will not load: %v", comp, rate, err)
		}
	}
	if err := c.Validate(); err != nil {
		t.Errorf("does not validate: %v\n%s", err, warningStrings(warnings))
	}
}

// TestImportWillNotSilentlyHalveAPerSecondRate covers the one pair of keys the
// source format adds together: it multiplies input_cost_per_second AND
// output_cost_per_second by the same elapsed time. dorang prices wall time
// once, so keeping the first quietly would be a rate that is plausible and too
// low — this section's own defect, one field over.
func TestImportWillNotSilentlyHalveAPerSecondRate(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: gpu
    litellm_params:
      model: llama-3
      custom_llm_provider: vllm
      api_base: https://gpu.invalid
      api_key: os.environ/V
      input_cost_per_second: "0.0001"
      output_cost_per_second: "0.0002"
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := string(c.Pricing.Rules[0].Rates["compute_seconds"]); got != "0.0001" {
		t.Errorf("compute_seconds = %q, want the first rate kept", got)
	}
	if !warningsContain(warnings, "", "write their SUM") {
		t.Errorf("the second per-second rate was dropped without saying so:\n%s",
			warningStrings(warnings))
	}
}

// TestImportNamesTheRatesItCannotPrice is the other half of the conversion
// table: a rate with no axis here is reported as a RATE, not as an unknown
// parameter. "This parameter has no equivalent" about a price reads as "nothing
// was lost", and something was.
func TestImportNamesTheRatesItCannotPrice(t *testing.T) {
	_, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: m
    litellm_params:
      model: gpt-4o
      custom_llm_provider: openai
      api_key: os.environ/K
      input_cost_per_image: "0.001"
      input_cost_per_video_per_second: "0.002"
      input_cost_per_audio_token: "0.0000001"
      input_cost_per_token_batches: "0.00000125"
      input_cost_per_token_above_128k_tokens: "0.000005"
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, want := range []struct{ path, msg string }{
		{"model_list[0].litellm_params.input_cost_per_image", "no per-image unit"},
		{"model_list[0].litellm_params.input_cost_per_video_per_second", "second of video"},
		{"model_list[0].litellm_params.input_cost_per_audio_token", "charged at the input rate"},
		{"model_list[0].litellm_params.input_cost_per_token_batches", "no batch rate card"},
		{"model_list[0].litellm_params.input_cost_per_token_above_128k_tokens", "TIER"},
	} {
		if !warningsContain(warnings, want.path, want.msg) {
			t.Errorf("no warning at %s about %q\ngot:\n%s",
				want.path, want.msg, warningStrings(warnings))
		}
	}
}

// TestImportFallbacksBecomeClasses covers the §7.6 normalization: dorang
// delegates by class, not by a named per-model list.
func TestImportFallbacksBecomeClasses(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: big
    litellm_params: {model: gpt-4o, custom_llm_provider: openai, api_key: os.environ/K}
  - model_name: small
    litellm_params: {model: gpt-4o-mini, custom_llm_provider: openai, api_key: os.environ/K}
router_settings:
  fallbacks: [{big: [small]}]
  context_window_fallbacks: [{small: [big]}]
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	class := "imported-fallback-big"
	members := c.ClassMembers(class)
	if len(members) != 2 || members[0] != "big" || members[1] != "small" {
		t.Errorf("classes[%s] = %v, want [big small]", class, members)
	}
	if m, _ := c.Model("big"); m.Class != class {
		t.Errorf("big.class = %q, want %q", m.Class, class)
	}
	if got := c.Fallbacks.On[CauseRateLimit]; len(got) != 2 {
		t.Errorf("fallbacks.on.rate_limit = %v", got)
	}
	if got := c.Fallbacks.On[CauseContextWindow]; len(got) != 1 || got[0] != TargetSameClassLarger {
		t.Errorf("fallbacks.on.context_window = %v", got)
	}
	if !warningsContain(warnings, "router_settings.fallbacks", "became class") {
		t.Errorf("the class normalization was not reported:\n%s", warningStrings(warnings))
	}
	// small is already in big's class, so the second list cannot also claim it.
	if !warningsContain(warnings, "router_settings.context_window_fallbacks", "already in class") {
		t.Errorf("the class conflict was not reported:\n%s", warningStrings(warnings))
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the imported configuration does not validate: %v", err)
	}
}

func TestImportModelGroupAlias(t *testing.T) {
	c, _, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: gpt-4o
    litellm_params: {model: gpt-4o, custom_llm_provider: openai, api_key: os.environ/K}
router_settings:
  model_group_alias: {gpt-4: gpt-4o}
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := c.ResolveAlias("gpt-4"); got != "gpt-4o" {
		t.Errorf("ResolveAlias(gpt-4) = %q", got)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func TestImportUnknownProviderHintIsWarnedNotGuessed(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: g
    litellm_params: {model: some-model, custom_llm_provider: brand_new_vendor, api_key: os.environ/K}
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	p, ok := c.Provider("brand_new_vendor")
	if !ok {
		t.Fatal("the provider was not declared")
	}
	if p.Kind != "openai" {
		t.Errorf("kind = %q, want the OpenAI-compatible default", p.Kind)
	}
	if !warningsContain(warnings, "", "is not a known kind") {
		t.Errorf("the assumption was not reported:\n%s", warningStrings(warnings))
	}
}

func TestImportProviderFromBaseURLOnly(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: local
    litellm_params: {model: llama-3, api_base: "http://gpu-1.internal:8000/v1"}
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, ok := c.Provider("gpu-1-internal"); !ok {
		t.Errorf("provider was not derived from the base URL: %+v", c.Providers)
	}
	if !warningsContain(warnings, "", "no provider hint") {
		t.Errorf("the assumed kind was not reported:\n%s", warningStrings(warnings))
	}
}

func TestImportSameHintDifferentBaseURLs(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: a
    litellm_params: {model: m, custom_llm_provider: openai, api_base: "https://one.invalid"}
  - model_name: b
    litellm_params: {model: m, custom_llm_provider: openai, api_base: "https://two.invalid"}
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(c.Providers) != 2 {
		t.Fatalf("providers = %+v, want two", c.Providers)
	}
	if c.Providers[1].Name != "openai-2" {
		t.Errorf("second provider = %q, want openai-2", c.Providers[1].Name)
	}
	if !warningsContain(warnings, "", "already declared with a different base URL") {
		t.Errorf("the split was not reported:\n%s", warningStrings(warnings))
	}
}

func TestImportRoutingStrategies(t *testing.T) {
	for src, want := range map[string]string{
		"simple-shuffle":         "weighted_random",
		"least-busy":             "least_busy",
		"latency-based-routing":  "lowest_latency",
		"cost-based-routing":     "lowest_cost",
		"usage-based-routing-v2": "least_busy",
	} {
		c, _, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: g
    litellm_params: {model: m, custom_llm_provider: openai, api_key: os.environ/K}
router_settings: {routing_strategy: ` + src + `}
`))
		if err != nil {
			t.Fatalf("import %s: %v", src, err)
		}
		got := c.Models[0].Strategy
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s mapped to %v, want [%s]", src, got, want)
		}
	}
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: g
    litellm_params: {model: m, custom_llm_provider: openai, api_key: os.environ/K}
router_settings: {routing_strategy: vibes}
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(c.Models[0].Strategy) != 0 {
		t.Errorf("an unknown strategy was invented: %v", c.Models[0].Strategy)
	}
	if !warningsContain(warnings, "router_settings.routing_strategy", "not known") {
		t.Errorf("the unknown strategy was not reported:\n%s", warningStrings(warnings))
	}
}

func TestImportDropParamsList(t *testing.T) {
	c, _, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: g
    litellm_params:
      model: m
      custom_llm_provider: openai
      api_key: os.environ/K
      additional_drop_params: [logprobs, top_logprobs]
litellm_settings:
  drop_params: false
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	p, _ := c.Provider("openai")
	if p.Params.DropsUnsupported() {
		t.Error("drop_params: false was not carried over")
	}
	if len(p.Params.Drop) != 2 || p.Params.Drop[0] != "logprobs" {
		t.Errorf("params.drop = %v", p.Params.Drop)
	}
}

func TestImportRejectsNonsense(t *testing.T) {
	if _, _, err := ImportProxyConfig([]byte("- a\n- b\n")); err == nil {
		t.Error("a sequence at the top level must be rejected")
	}
	if _, _, err := ImportProxyConfig([]byte("model_list: {}\n")); err == nil {
		t.Error("a model_list that is not a sequence must be rejected")
	}
	if _, _, err := ImportProxyConfig([]byte(": : :")); err == nil {
		t.Error("unparseable YAML must be rejected")
	}
	_, warnings, err := ImportProxyConfig([]byte("router_settings: {num_retries: 2}\n"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !warningsContain(warnings, "model_list", "nothing to route") {
		t.Errorf("an empty model list was not reported:\n%s", warningStrings(warnings))
	}
}

func TestImportEntryProblemsAreReported(t *testing.T) {
	_, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: no-params
  - litellm_params: {model: m}
  - model_name: no-model
    litellm_params: {api_key: os.environ/K}
  - "just a string"
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, want := range []string{
		"no litellm_params", "no model_name", "has no model", "not a mapping",
	} {
		if !warningsContain(warnings, "", want) {
			t.Errorf("no warning about %q:\n%s", want, warningStrings(warnings))
		}
	}
}

func TestWarningString(t *testing.T) {
	if got := (Warning{Path: "a.b", Message: "no"}).String(); got != "a.b: no" {
		t.Errorf("String() = %q", got)
	}
	if got := (Warning{Message: "no"}).String(); got != "no" {
		t.Errorf("String() = %q", got)
	}
}
