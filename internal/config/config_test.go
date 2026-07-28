package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// baseYAML is a minimal configuration that loads and validates cleanly. Tests
// append the section they exercise. It uses key_ref so that loading it touches
// neither the environment nor the filesystem.
const baseYAML = `
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_ref: "vault:kv/p1#key"}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`

func loadYAML(t *testing.T, extra ...string) (*Config, error) {
	t.Helper()
	return LoadBytes([]byte(baseYAML + strings.Join(extra, "\n")))
}

// fragments extends the minimal configuration. YAML forbids a repeated
// top-level key, so extra list items are spliced into the section they belong
// to rather than appended as a second block.
type fragments struct {
	providers   string // extra items under providers:
	credentials string // extra items under credentials:
	models      string // extra items under models:
	top         string // extra top-level sections
}

func buildYAML(f fragments) string {
	var b strings.Builder
	b.WriteString("version: 1\nproviders:\n")
	b.WriteString("  - {name: p1, kind: openai, base_url: \"https://example.invalid\"}\n")
	b.WriteString(f.providers)
	b.WriteString("credentials:\n")
	b.WriteString("  - {id: c1, provider: p1, key_ref: \"vault:kv/p1#key\"}\n")
	b.WriteString(f.credentials)
	b.WriteString("models:\n  - name: m1\n    deployments:\n")
	b.WriteString("      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}\n")
	b.WriteString(f.models)
	b.WriteString(f.top)
	return b.String()
}

func loadFragments(t *testing.T, f fragments) (*Config, error) {
	t.Helper()
	return LoadBytes([]byte(buildYAML(f)))
}

func mustLoad(t *testing.T, extra ...string) *Config {
	t.Helper()
	c, err := loadYAML(t, extra...)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return c
}

// hasProblem reports whether err carries a problem at path whose message
// contains want.
func hasProblem(err error, path, want string) bool {
	for _, p := range Problems(err) {
		if p.Path == path && strings.Contains(p.Message, want) {
			return true
		}
	}
	return false
}

func problemPaths(err error) []string {
	var out []string
	for _, p := range Problems(err) {
		out = append(out, p.Path)
	}
	return out
}

// --- §4.2 ------------------------------------------------------------------

// TestDesignShapeRoundTrip loads the YAML of design §4.2 verbatim and checks
// every section against what the design writes.
func TestDesignShapeRoundTrip(t *testing.T) {
	data, err := os.ReadFile("testdata/design_4_2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := decodeConfig(data, "design_4_2.yaml")
	if err != nil {
		t.Fatalf("the §4.2 shape must decode: %v", err)
	}

	eq := func(name string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", name, got, want)
		}
	}

	eq("version", c.Version, 1)

	eq("server.listen", c.Server.Listen, ":4100")
	eq("server.env", c.Server.Env, "production")
	eq("server.master_key_env", c.Server.MasterKeyEnv, "DORANG_MASTER_KEY")
	eq("server.key_pepper_env", c.Server.KeyPepperEnv, "DORANG_KEY_PEPPER")
	eq("server.request_timeout", c.Server.RequestTimeout.Duration(), 600*time.Second)
	eq("server.shutdown_grace", c.Server.ShutdownGrace.Duration(), 30*time.Second)

	eq("storage.driver", c.Storage.Driver, "sqlite")
	eq("storage.sqlite.path", c.Storage.SQLite.Path, "~/.dorang/dorang.db")
	eq("storage.postgres.url_env", c.Storage.Postgres.URLEnv, "DORANG_DATABASE_URL")
	eq("storage.postgres.max_conns", c.Storage.Postgres.MaxConns, 32)

	eq("cluster.enabled", c.Cluster.Enabled, false)
	eq("cluster.node_id", c.Cluster.NodeID, "")
	eq("cluster.redis_url_env", c.Cluster.RedisURLEnv, "DORANG_REDIS_URL")
	eq("cluster.capacity_mode", c.Cluster.CapacityMode, "local")

	eq("auth.legacy.enabled", c.Auth.Legacy.Enabled, false)
	eq("auth.legacy.until", c.Auth.Legacy.Until, "")
	eq("auth.rehash_on_use", *c.Auth.RehashOnUse, true)

	if len(c.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(c.Providers))
	}
	p := c.Providers[0]
	eq("providers[0].name", p.Name, "plan-a")
	eq("providers[0].kind", p.Kind, "glm")
	eq("providers[0].timeout", p.Timeout.Duration(), 180*time.Second)
	eq("providers[0].max_concurrency", p.MaxConcurrency, 20)
	eq("providers[0].capacity_group", p.CapacityGroup, "plan-a-pool")
	eq("providers[0].params.drop_unsupported", *p.Params.DropUnsupported, true)
	eq("providers[0].params.drop", p.Params.Drop, []string{})
	eq("providers[0].retry.max_attempts", p.Retry.MaxAttempts, 2)
	eq("providers[0].retry.backoff", p.Retry.Backoff, "exponential")
	eq("providers[0].retry.base", p.Retry.Base.Duration(), 500*time.Millisecond)
	eq("providers[0].usage_probe", p.UsageProbe,
		UsageProbe{Enabled: true, Fetcher: "glm", Interval: Duration(60 * time.Second)})

	if len(c.Credentials) != 2 {
		t.Fatalf("credentials = %d, want 2", len(c.Credentials))
	}
	eq("credentials[0].id", c.Credentials[0].ID, "acct-1")
	eq("credentials[0].provider", c.Credentials[0].Provider, "cloud-a")
	eq("credentials[0].key_env", c.Credentials[0].Key.Env, "CLOUD_A_KEY_1")
	eq("credentials[0].capacity_group", c.Credentials[0].CapacityGroup, "acct-1")
	eq("credentials[1].key_env", c.Credentials[1].Key.Env, "CLOUD_A_KEY_2")

	eq("capacity.provider_groups", c.Capacity.ProviderGroups,
		map[string]CapacityLimits{"cloud-a-pool": {MaxConcurrency: 6}})
	eq("capacity.credential_groups", c.Capacity.CredentialGroups,
		map[string]CapacityLimits{"acct-1": {MaxConcurrency: 3}, "acct-2": {MaxConcurrency: 3}})
	eq("capacity.models", c.Capacity.Models, []ModelCapacity{
		{Provider: "plan-a", Model: "model-x", Limits: CapacityLimits{MaxConcurrency: 7}},
		{Provider: "plan-a", Model: "model-y", Limits: CapacityLimits{MaxConcurrency: 7}},
	})
	eq("capacity.principals", c.Capacity.Principals, map[string]PrincipalLimits{
		"default": {MaxConcurrent: 32, MaxQueueWait: Duration(30 * time.Second)},
	})
	eq("capacity.interactive_reserve", *c.Capacity.InteractiveReserve, 0.3)

	eq("key_rotation.strategy", c.KeyRotation.Strategy, "least_used")
	kr := c.KeyRotation.Providers["cloud-a"]
	eq("key_rotation.providers[cloud-a].affinity_group", kr.AffinityGroup, "cloud-a-accounts")
	eq("key_rotation.providers[cloud-a].stickiness", kr.Stickiness,
		Stickiness{Scope: "session", OnCapacity: "spill"})
	eq("key_rotation.providers[cloud-a].keys", kr.Keys, []RotationKey{
		{ID: "acct-1", Key: SecretRef{Env: "CLOUD_A_KEY_1"}, MaxConcurrency: 3, CapacityGroup: "acct-1"},
		{ID: "acct-2", Key: SecretRef{Env: "CLOUD_A_KEY_2"}, MaxConcurrency: 3, CapacityGroup: "acct-2"},
	})

	if len(c.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(c.Models))
	}
	m := c.Models[0]
	eq("models[0].name", m.Name, "model-x")
	eq("models[0].class", m.Class, "chat-large")
	eq("models[0].strategy", m.Strategy, []string{"prefix_sticky", "lowest_cost", "least_busy"})
	eq("models[0].deployments", m.Deployments, []Deployment{
		{Provider: "plan-a", UpstreamModel: "model-x", Credentials: []string{"plan-a-1"}, Weight: 10, Priority: 0},
		{Provider: "cloud-a", UpstreamModel: "model-x:cloud", Credentials: []string{"acct-1", "acct-2"}, Weight: 5, Priority: 1},
	})

	eq("aliases", c.Aliases, map[string]string{"model-small": "model-y", "model-large": "model-x"})
	eq("classes", c.Classes, map[string][]string{
		"chat-large": {"model-x", "model-z"}, "chat-small": {"model-y"}})

	eq("routing.sticky", c.Routing.Sticky, StickyRouting{
		Enabled: boolPtr(true), TTL: Duration(time.Hour), PurgeInterval: Duration(5 * time.Minute),
		Key: []string{"api_key", "session_id"},
	})
	eq("routing.prefix", c.Routing.Prefix, PrefixRouting{
		Enabled: boolPtr(true), ChunkBytes: 4096, Checkpoints: "logarithmic",
		MaxBytes: 64 << 20, TTL: Duration(time.Hour),
	})

	eq("fallbacks.on", c.Fallbacks.On, map[string][]string{
		"rate_limit":      {"same_group", "same_class"},
		"quota_exhausted": {"same_group", "same_class"},
		"context_window":  {"same_class_larger"},
		"content_policy":  {"same_class"},
		"upstream_5xx":    {"same_group", "same_class"},
		"timeout":         {"same_group"},
		"budget_exceeded": {},
		"auth":            {},
	})
	eq("fallbacks.max_hops", c.Fallbacks.MaxHops, 3)
	eq("fallbacks.budget_ms", c.Fallbacks.BudgetMS, 120000)

	eq("pricing", c.Pricing, Pricing{Catalog: "/etc/dorang/pricing.yaml", Currency: "USD"})

	eq("metering.numeric.enabled", *c.Metering.Numeric.Enabled, true)
	eq("metering.trace.store_messages", c.Metering.Trace.StoreMessages, "truncated")
	eq("metering.trace.truncate_chars", c.Metering.Trace.TruncateChars, 512)
	eq("metering.trace.sample_rate", *c.Metering.Trace.SampleRate, 1.0)
	eq("metering.trace.daily_byte_budget", c.Metering.Trace.DailyByteBudget.Bytes(), int64(8<<30))
	eq("metering.spool.dir", c.Metering.Spool.Dir, "~/.dorang/spool")
	eq("metering.spool.max_bytes", c.Metering.Spool.MaxBytes.Bytes(), int64(2<<30))
	eq("metering.flush_interval", c.Metering.FlushInterval.Duration(), 250*time.Millisecond)

	eq("observability", c.Observability, Observability{
		Prometheus: boolPtr(true), OTLPEndpoint: "", LogLevel: "info", LogFormat: "json"})

	eq("extensions.lua.enabled", c.Extensions.Lua.Enabled, false)
	eq("extensions.lua.dir", c.Extensions.Lua.Dir, "/etc/dorang/lua")
	eq("extensions.lua.hooks", c.Extensions.Lua.Hooks, luaHooks)
	eq("extensions.lua.limits", c.Extensions.Lua.Limits, LuaLimits{
		Instructions: 5000000, MemoryMB: 32, Timeout: Duration(200 * time.Millisecond)})

	// Marshaling, decoding and marshaling again must be stable: the file makes
	// the round trip without losing or inventing anything.
	c.ApplyDefaults()
	out, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again, err := decodeConfig(out, "design_4_2.yaml")
	if err != nil {
		t.Fatalf("re-decode of marshaled configuration: %v\n%s", err, out)
	}
	again.ApplyDefaults()
	out2, err := yaml.Marshal(again)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != string(out2) {
		t.Errorf("round trip changed the configuration\nfirst:\n%s\nsecond:\n%s", out, out2)
	}
}

// TestDesignShapeCrossReferences pins the cross-reference defects in the §4.2
// example itself: it names providers, credentials and models it never declares.
// Every one of them is reported, in one aggregate.
func TestDesignShapeCrossReferences(t *testing.T) {
	data, err := os.ReadFile("testdata/design_4_2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadBytes(data)
	if err == nil {
		t.Fatal("the §4.2 example is internally inconsistent; it must not validate")
	}
	for _, want := range []struct{ path, msg string }{
		{"providers[0].capacity_group", `"plan-a-pool" is not declared`},
		{"credentials[0].provider", `provider "cloud-a" is not declared`},
		{"credentials[1].provider", `provider "cloud-a" is not declared`},
		{`key_rotation.providers["cloud-a"]`, `provider "cloud-a" is not declared`},
		{"models[0].deployments[0].credentials[0]", `credential "plan-a-1" is not declared`},
		{"models[0].deployments[1].provider", `provider "cloud-a" is not declared`},
		{`aliases["model-small"]`, `alias target "model-y" is not a declared model`},
		{`classes["chat-large"][1]`, `class member "model-z" is not a declared model`},
		{`classes["chat-small"][0]`, `class member "model-y" is not a declared model`},
	} {
		if !hasProblem(err, want.path, want.msg) {
			t.Errorf("missing problem at %s (%s); got %v", want.path, want.msg, problemPaths(err))
		}
	}
}

// --- defaults ---------------------------------------------------------------

func TestDefaults(t *testing.T) {
	c := mustLoad(t)

	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"version", c.Version, 1},
		{"server.listen", c.Server.Listen, ":4100"},
		{"server.env", c.Server.Env, "production"},
		{"server.master_key_env", c.Server.MasterKeyEnv, "DORANG_MASTER_KEY"},
		{"server.key_pepper_env", c.Server.KeyPepperEnv, "DORANG_KEY_PEPPER"},
		{"server.request_timeout", c.Server.RequestTimeout.Duration(), 600 * time.Second},
		{"server.shutdown_grace", c.Server.ShutdownGrace.Duration(), 30 * time.Second},
		{"storage.driver", c.Storage.Driver, "sqlite"},
		{"storage.sqlite.path", c.Storage.SQLite.Path, "~/.dorang/dorang.db"},
		{"storage.postgres.url_env", c.Storage.Postgres.URLEnv, "DORANG_DATABASE_URL"},
		{"storage.postgres.max_conns", c.Storage.Postgres.MaxConns, 32},
		{"cluster.enabled", c.Cluster.Enabled, false},
		{"cluster.capacity_mode", c.Cluster.CapacityMode, "local"},
		{"cluster.redis_url_env", c.Cluster.RedisURLEnv, "DORANG_REDIS_URL"},
		{"cluster.min_leasable", c.Cluster.MinLeasable, 16},
		{"auth.legacy.enabled", c.Auth.Legacy.Enabled, false},
		{"auth.rehash_on_use", c.Auth.RehashesOnUse(), true},
		{"providers[0].params.drop_unsupported", c.Providers[0].Params.DropsUnsupported(), true},
		{"providers[0].retry.max_attempts", c.Providers[0].Retry.MaxAttempts, 2},
		{"providers[0].retry.backoff", c.Providers[0].Retry.Backoff, "exponential"},
		{"providers[0].retry.base", c.Providers[0].Retry.Base.Duration(), 500 * time.Millisecond},
		{"capacity.interactive_reserve", c.Capacity.Reserve(), 0.3},
		{"capacity.principals[default].max_concurrent", c.Capacity.Principals["default"].MaxConcurrent, 32},
		{"capacity.principals[default].max_queue_wait",
			c.Capacity.Principals["default"].MaxQueueWait.Duration(), 30 * time.Second},
		{"routing.sticky.enabled", c.Routing.Sticky.IsEnabled(), true},
		{"routing.sticky.ttl", c.Routing.Sticky.TTL.Duration(), time.Hour},
		{"routing.sticky.purge_interval", c.Routing.Sticky.PurgeInterval.Duration(), 5 * time.Minute},
		{"routing.sticky.key", c.Routing.Sticky.Key, []string{"api_key", "session_id"}},
		{"routing.prefix.chunk_bytes", c.Routing.Prefix.ChunkBytes.Bytes(), int64(4096)},
		{"routing.prefix.checkpoints", c.Routing.Prefix.Checkpoints, "logarithmic"},
		{"routing.prefix.max_bytes", c.Routing.Prefix.MaxBytes.Bytes(), int64(64 << 20)},
		{"routing.prefix.ttl", c.Routing.Prefix.TTL.Duration(), time.Hour},
		{"fallbacks.on[rate_limit]", c.Fallbacks.On["rate_limit"], []string{"same_group", "same_class"}},
		{"fallbacks.on[context_window]", c.Fallbacks.On["context_window"], []string{"same_class_larger"}},
		{"fallbacks.on[timeout]", c.Fallbacks.On["timeout"], []string{"same_group"}},
		{"fallbacks.on[budget_exceeded]", c.Fallbacks.On["budget_exceeded"], []string{}},
		{"fallbacks.on[auth]", c.Fallbacks.On["auth"], []string{}},
		{"fallbacks.max_hops", c.Fallbacks.MaxHops, 3},
		{"fallbacks.budget_ms", c.Fallbacks.BudgetMS, 120000},
		{"pricing.currency", c.Pricing.Currency, "USD"},
		{"metering.numeric.enabled", c.Metering.Numeric.IsEnabled(), true},
		{"metering.trace.store_messages", c.Metering.Trace.StoreMessages, "truncated"},
		{"metering.trace.truncate_chars", c.Metering.Trace.TruncateChars, 512},
		{"metering.trace.sample_rate", c.Metering.Trace.Rate(), 1.0},
		{"metering.trace.daily_byte_budget", c.Metering.Trace.DailyByteBudget.Bytes(), int64(8 << 30)},
		{"metering.spool.dir", c.Metering.Spool.Dir, "~/.dorang/spool"},
		{"metering.spool.max_bytes", c.Metering.Spool.MaxBytes.Bytes(), int64(2 << 30)},
		{"metering.flush_interval", c.Metering.FlushInterval.Duration(), 250 * time.Millisecond},
		{"observability.prometheus", c.Observability.PrometheusEnabled(), true},
		{"observability.log_level", c.Observability.LogLevel, "info"},
		{"observability.log_format", c.Observability.LogFormat, "json"},
		{"extensions.lua.enabled", c.Extensions.Lua.Enabled, false},
		{"extensions.lua.dir", c.Extensions.Lua.Dir, "/etc/dorang/lua"},
		{"extensions.lua.limits.instructions", c.Extensions.Lua.Limits.Instructions, int64(5000000)},
		{"extensions.lua.limits.memory_mb", c.Extensions.Lua.Limits.MemoryMB, 32},
		{"extensions.lua.limits.timeout", c.Extensions.Lua.Limits.Timeout.Duration(), 200 * time.Millisecond},
		{"passthrough.default.auth", c.Passthrough.Default.Auth, "dorang"},
		{"passthrough.default.meter", *c.Passthrough.Default.Meter, true},
		{"passthrough.default.timeout", c.Passthrough.Default.Timeout.Duration(), 600 * time.Second},
		{"shadow.mode", c.Shadow.Mode, "off"},
		{"shadow.compare.structural", c.Shadow.Compare.IsStructural(), true},
		{"shadow.reference.timeout", c.Shadow.Reference.Timeout.Duration(), 60 * time.Second},
		{"shadow.queue_size", c.Shadow.QueueSize, 256},
		{"shadow.workers", c.Shadow.Workers, 4},
		{"shadow.capture.head_bytes", c.Shadow.Capture.HeadBytes.Bytes(), int64(256 << 10)},
		{"shadow.capture.tail_bytes", c.Shadow.Capture.TailBytes.Bytes(), int64(16 << 10)},
		{"shadow.report.path", c.Shadow.Report.Path, "~/.dorang/shadow.jsonl"},
		{"shadow.report.max_bytes", c.Shadow.Report.MaxBytes.Bytes(), int64(256 << 20)},
		{"shadow.unpriced_estimate_usd", string(c.Shadow.UnpricedEstimateUSD), "0.01"},
		{"notifications.email.driver", c.Notifications.Email.Driver, "none"},
		{"priority_mapping.classes", c.PriorityMapping.Classes,
			map[string]int{"realtime": 0, "interactive": 2, "batch": 10}},
		{"priority_mapping.emit.header", c.PriorityMapping.Emit.Header, "X-Request-Priority"},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s = %#v, want %#v", tc.name, tc.got, tc.want)
		}
	}
}

// TestDefaultsDoNotOverrideExplicitZero pins the fields where an explicit false
// or zero has to survive defaulting.
func TestDefaultsDoNotOverrideExplicitZero(t *testing.T) {
	c := mustLoad(t, `
capacity: {interactive_reserve: 0}
auth: {rehash_on_use: false}
routing:
  sticky: {enabled: false}
  prefix: {enabled: false}
metering:
  trace: {sample_rate: 0}
observability: {prometheus: false}
`)
	if got := c.Capacity.Reserve(); got != 0 {
		t.Errorf("interactive_reserve = %v, want 0", got)
	}
	if c.Auth.RehashesOnUse() {
		t.Error("rehash_on_use = true, want false")
	}
	if c.Routing.Sticky.IsEnabled() || c.Routing.Prefix.IsEnabled() {
		t.Error("routing affinity stayed enabled")
	}
	if got := c.Metering.Trace.Rate(); got != 0 {
		t.Errorf("trace sample_rate = %v, want 0", got)
	}
	if c.Observability.PrometheusEnabled() {
		t.Error("prometheus = true, want false")
	}
	// drop_unsupported false must survive too.
	c2 := mustLoad(t, `
`)
	_ = c2
	c3, err := LoadBytes([]byte(`
version: 1
providers:
  - {name: p1, kind: openai, params: {drop_unsupported: false}}
credentials: [{id: c1, provider: p1, key_ref: "vault:x"}]
models: [{name: m1, deployments: [{provider: p1, upstream_model: u1}]}]
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c3.Providers[0].Params.DropsUnsupported() {
		t.Error("drop_unsupported = true, want false")
	}
}

// --- opaque model names ------------------------------------------------------

// TestOpaqueModelNames is the §2.1 golden set: real model names use ':' as a
// family tag, as a vendor prefix and as a deployment variant, and '/' as a
// vendor separator. None of them is ever split.
func TestOpaqueModelNames(t *testing.T) {
	names := []string{
		"gemma4:31b",
		"zai:glm-5.1",
		"deepseek-v4-flash:cloud",
		"qwen3.5:397b",
		"vendor/model-1.5",
	}

	var b strings.Builder
	b.WriteString("version: 1\nproviders:\n  - {name: p1, kind: openai}\n")
	b.WriteString("credentials:\n  - {id: c1, provider: p1, key_ref: \"vault:x\"}\n")
	b.WriteString("models:\n")
	for _, n := range names {
		b.WriteString("  - name: \"" + n + "\"\n    class: everything\n")
		b.WriteString("    deployments:\n      - {provider: p1, upstream_model: \"" + n +
			"\", credentials: [c1]}\n")
	}
	b.WriteString("classes:\n  everything: [")
	for i, n := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("\"" + n + "\"")
	}
	b.WriteString("]\naliases:\n")
	for _, n := range names {
		b.WriteString("  \"alias-of-" + n + "\": \"" + n + "\"\n")
	}
	b.WriteString("capacity:\n  models:\n")
	for _, n := range names {
		b.WriteString("    - {provider: p1, model: \"" + n + "\", max_concurrency: 7}\n")
	}

	c, err := LoadBytes([]byte(b.String()))
	if err != nil {
		t.Fatalf("opaque names must load: %v", err)
	}
	for i, n := range names {
		if got := c.Models[i].Name; got != n {
			t.Errorf("models[%d].name = %q, want %q", i, got, n)
		}
		if got := c.Models[i].Deployments[0].UpstreamModel; got != n {
			t.Errorf("models[%d].deployments[0].upstream_model = %q, want %q", i, got, n)
		}
		m, ok := c.Model(n)
		if !ok {
			t.Fatalf("Model(%q) not found", n)
		}
		if m.Name != n {
			t.Errorf("Model(%q).Name = %q", n, m.Name)
		}
		if got := c.ResolveAlias("alias-of" + "-" + n); got != n {
			t.Errorf("ResolveAlias(alias-of-%s) = %q, want %q", n, got, n)
		}
		am, ok := c.ResolveModel("alias-of-" + n)
		if !ok || am.Name != n {
			t.Errorf("ResolveModel(alias-of-%s) = %v, %v", n, am, ok)
		}
		if got := c.Capacity.Models[i].Model; got != n {
			t.Errorf("capacity.models[%d].model = %q, want %q", i, got, n)
		}
	}
	if got := c.ClassMembers("everything"); !reflect.DeepEqual(got, names) {
		t.Errorf("class members = %v, want %v", got, names)
	}
	// A name that is not declared resolves to nothing rather than to a prefix
	// of itself.
	if _, ok := c.Model("gemma4"); ok {
		t.Error(`Model("gemma4") resolved: a model name was split on ':'`)
	}
	if _, ok := c.Model("vendor"); ok {
		t.Error(`Model("vendor") resolved: a model name was split on '/'`)
	}
}

// --- decoding ---------------------------------------------------------------

func TestDecodeRejectsUnknownField(t *testing.T) {
	_, err := loadYAML(t, "storage: {driver: sqlite, sqlite: {pth: /x}}")
	if err == nil {
		t.Fatal("an unknown key must be reported")
	}
	if !strings.Contains(err.Error(), "pth") {
		t.Errorf("error does not name the unknown key: %v", err)
	}
}

func TestDecodeRejectsDuplicateAliasKey(t *testing.T) {
	_, err := LoadBytes([]byte(baseYAML + "\naliases: {a: m1, a: m1}\n"))
	if err == nil {
		t.Fatal("a duplicate alias key must be reported")
	}
	if !strings.Contains(err.Error(), "already defined") {
		t.Errorf("error does not report the duplicate: %v", err)
	}
}

func TestEmptyFileIsAllDefaults(t *testing.T) {
	c, err := LoadBytes(nil)
	if err != nil {
		t.Fatalf("an empty file must load: %v", err)
	}
	if c.Server.Listen != ":4100" || c.Storage.Driver != "sqlite" {
		t.Errorf("empty file did not take defaults: %+v", c.Server)
	}
}

func TestLoadFileNotFound(t *testing.T) {
	if _, err := Load("testdata/does-not-exist.yaml"); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestLoadReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/dorang.yaml"
	if err := os.WriteFile(path, []byte(baseYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Source() != path {
		t.Errorf("Source() = %q, want %q", c.Source(), path)
	}
}

// TestConfigAccessors covers the lookup surface other packages use.
func TestConfigAccessors(t *testing.T) {
	c := mustLoad(t, "aliases: {small: m1}")
	if got := c.ModelNames(); len(got) != 1 || got[0] != "m1" {
		t.Errorf("ModelNames() = %v", got)
	}
	if c.IsDevelopment() {
		t.Error("IsDevelopment() = true for the default environment")
	}
	dev := mustLoad(t, "server: {env: development}")
	if !dev.IsDevelopment() {
		t.Error("IsDevelopment() = false for server.env development")
	}
	if _, ok := c.Provider("nope"); ok {
		t.Error("Provider(nope) resolved")
	}
	if _, ok := c.Credential("nope"); ok {
		t.Error("Credential(nope) resolved")
	}
	if _, ok := c.Model("nope"); ok {
		t.Error("Model(nope) resolved")
	}
	if got := c.ResolveAlias("m1"); got != "m1" {
		t.Errorf("ResolveAlias of a plain model name = %q", got)
	}

	// The same lookups work on a configuration that was never indexed, so a
	// hand-built or imported one behaves identically.
	bare := &Config{
		Providers:   []Provider{{Name: "p1"}},
		Credentials: []Credential{{ID: "c1", Provider: "p1"}},
		Models:      []Model{{Name: "m1"}},
	}
	if _, ok := bare.Provider("p1"); !ok {
		t.Error("Provider lookup failed without an index")
	}
	if _, ok := bare.Credential("c1"); !ok {
		t.Error("Credential lookup failed without an index")
	}
	if _, ok := bare.Model("m1"); !ok {
		t.Error("Model lookup failed without an index")
	}
	if _, ok := bare.Provider("nope"); ok {
		t.Error("Provider(nope) resolved without an index")
	}
}
