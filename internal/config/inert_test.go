package config

import (
	"strings"
	"testing"
)

// A setting that validates and does nothing is worse than no setting: an
// operator who believes they capped a rate has capped nothing and finds out from
// the provider's bill. Everything here either became load-bearing or became a
// load error, and these tests hold that line from the configuration side.

// loadWith builds baseYAML plus a fragment and returns the load error.
func loadWith(t *testing.T, extra string) error {
	t.Helper()
	_, err := LoadBytes([]byte(baseYAML + extra))
	return err
}

// mustRefuse requires a load error naming path and containing want.
func mustRefuse(t *testing.T, err error, path, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal naming %s", path)
	}
	for _, p := range Problems(err) {
		if p.Path == path && strings.Contains(p.Message, want) {
			return
		}
	}
	t.Fatalf("no problem at %s containing %q; got: %v", path, want, err)
}

// TestKeyRefIsRefused. It used to load, validate, and leave the credential with
// no usable secret: internal/app discarded the "resolved" flag, applyCredential
// returned early on an empty string, and the request went upstream with no
// Authorization header at all. The failure arrived as a 401 from the provider,
// with nothing in the configuration to explain it.
func TestKeyRefIsRefused(t *testing.T) {
	_, err := LoadBytes([]byte(`
version: 1
providers: [{name: p1, kind: openai}]
credentials:
  - {id: c1, provider: p1, key_ref: "vault:kv/p1#key"}
models: [{name: m, deployments: [{provider: p1, upstream_model: u, credentials: [c1]}]}]
`))
	mustRefuse(t, err, "credentials[0].key_ref", "not resolved by this build")
	// The refusal has to say what to use instead, or it is a wall rather than a
	// diagnosis.
	if !strings.Contains(err.Error(), "key_env or key_file") {
		t.Errorf("the refusal does not name a working alternative: %v", err)
	}
}

// TestCapacityRateCeilingsAreRefused. capacity.*.rpm and .tpm were validated for
// sign and then never read: the broker counts concurrent reservations, which are
// released when a request finishes, and has no time window to count a rate over.
func TestCapacityRateCeilingsAreRefused(t *testing.T) {
	cases := []struct{ name, yaml, path, want string }{
		{
			"provider group rpm",
			"\ncapacity:\n  provider_groups: {pg: {max_concurrency: 4, rpm: 600}}\n",
			"capacity.provider_groups[\"pg\"].rpm", "models[].deployments[].limits[]",
		},
		{
			"credential group tpm",
			"\ncapacity:\n  credential_groups: {cg: {tpm: 400000}}\n",
			"capacity.credential_groups[\"cg\"].tpm", "models[].deployments[].limits[]",
		},
		{
			"global rpm",
			"\ncapacity:\n  global: {max_concurrency: 100, rpm: 10}\n",
			"capacity.global.rpm", "models[].deployments[].limits[]",
		},
		{
			"principal rpm names the api key instead",
			"\ncapacity:\n  principals: {default: {max_concurrent: 4, rpm: 60}}\n",
			"capacity.principals[\"default\"].rpm", "dorangctl key create --rpm",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mustRefuse(t, loadWith(t, c.yaml), c.path, c.want)
		})
	}
}

// TestCapacityQueueCeilingsAreAccepted is the other half: max_queue and
// max_queue_wait are wired now, so they must still load.
func TestCapacityQueueCeilingsAreAccepted(t *testing.T) {
	if err := loadWith(t, "\ncapacity:\n"+
		"  global: {max_concurrency: 100, max_queue: 50}\n"+
		"  provider_groups: {pg: {max_concurrency: 4, max_queue: 8}}\n"+
		"  principals: {default: {max_concurrent: 4, max_queue: 16, max_queue_wait: 10s}}\n"); err != nil {
		t.Fatalf("a wired queue ceiling must load: %v", err)
	}
}

// TestClientPriorityGrantIsExpressible is §10.5, which had no configuration
// surface at all: `client_priority` was an unknown field, so the design's own
// example was not loadable by the build that ships it.
func TestClientPriorityGrantIsExpressible(t *testing.T) {
	c, err := LoadBytes([]byte(baseYAML + "\ncapacity:\n  principals:\n" +
		"    default: {client_priority: ignore}\n" +
		"    batch-pipeline: {client_priority: allow, range: [batch, interactive]}\n"))
	if err != nil {
		t.Fatalf("§10.5's own example must load: %v", err)
	}
	if !c.Capacity.Principals["batch-pipeline"].GrantsClientPriority() {
		t.Error("the grant did not survive loading")
	}
	if c.Capacity.Principals["default"].GrantsClientPriority() {
		t.Error("ignore was read as a grant")
	}
}

// TestClientPriorityGrantIsChecked: a grant with no range, a range with no
// grant, and a range naming a class that does not exist are all refused. A
// half-written grant is the shape this sweep exists to remove.
func TestClientPriorityGrantIsChecked(t *testing.T) {
	cases := []struct{ name, yaml, path, want string }{
		{
			"allow without a range",
			"\ncapacity:\n  principals: {p: {client_priority: allow}}\n",
			"capacity.principals[\"p\"].range", "requires a range",
		},
		{
			"range without allow",
			"\ncapacity:\n  principals: {p: {range: [batch, interactive]}}\n",
			"capacity.principals[\"p\"].range", "only meaningful with client_priority: allow",
		},
		{
			"range naming an undeclared class",
			"\ncapacity:\n  principals: {p: {client_priority: allow, range: [batch, urgent]}}\n",
			"capacity.principals[\"p\"].range[1]", "not a declared priority class",
		},
		{
			"unknown mode",
			"\ncapacity:\n  principals: {p: {client_priority: maybe}}\n",
			"capacity.principals[\"p\"].client_priority", "is not a known value",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mustRefuse(t, loadWith(t, c.yaml), c.path, c.want)
		})
	}
}

// TestQuotaUrgencyIsAnAcceptedStrategy is §7.5a(c). The comparator was designed
// and its arithmetic implemented; the name was in neither the config validator's
// list nor the router's, so the feature could not be selected from a file.
func TestQuotaUrgencyIsAnAcceptedStrategy(t *testing.T) {
	_, err := LoadBytes([]byte(`
version: 1
providers: [{name: p1, kind: openai}]
credentials: [{id: c1, provider: p1, key_env: DORANG_TEST_FIXTURE_KEY}]
models:
  - name: m
    strategy: [lowest_cost, quota_urgency]
    deployments: [{provider: p1, upstream_model: u, credentials: [c1]}]
`))
	if err != nil {
		t.Fatalf("quota_urgency must be an accepted strategy: %v", err)
	}
}

// TestNotionalRateIsExpressibleInline is §8.5. It was catalog-only, so a
// deployment with no external catalog file could not record what its
// subscription traffic was worth — the one figure that says whether the
// subscription is worth renewing.
func TestNotionalRateIsExpressibleInline(t *testing.T) {
	c, err := LoadBytes([]byte(baseYAML + "\npricing:\n  rules:\n" +
		"    - id: notional-1\n      class: notional_rate\n" +
		"      match: {provider: p1}\n" +
		"      rates: {input: \"0.000003\", cache_read: \"0.00000019\"}\n" +
		"      source: \"vendor list price page\"\n      as_of: \"2026-07-28\"\n"))
	if err != nil {
		t.Fatalf("an inline notional_rate rule must load: %v", err)
	}
	if got := c.Pricing.Rules[0].Class; got != PricingNotionalRate {
		t.Fatalf("class = %q", got)
	}
	if c.Pricing.Rules[0].Source == "" || c.Pricing.Rules[0].AsOf == "" {
		t.Fatal("provenance did not survive loading")
	}
}

// TestNotionalProvenanceIsRequired: §8.5 makes source and as_of load errors
// rather than warnings, because an estimate without provenance is a guess
// wearing a currency symbol and a rate with no date cannot be judged stale.
func TestNotionalProvenanceIsRequired(t *testing.T) {
	base := "\npricing:\n  rules:\n    - id: n1\n      class: notional_rate\n" +
		"      rates: {input: \"0.000003\"}\n"
	mustRefuse(t, loadWith(t, base), "pricing.rules[0].source", "not auditable")
	mustRefuse(t, loadWith(t, base+"      source: vendor\n"),
		"pricing.rules[0].as_of", "judged stale")
	// And provenance on a rule that is not notional is refused in the other
	// direction: a marginal rule carrying a source looks audited and is not.
	mustRefuse(t, loadWith(t, "\npricing:\n  rules:\n    - id: m1\n      class: marginal_usage\n"+
		"      rates: {input: \"0.000003\"}\n      source: vendor\n"),
		"pricing.rules[0].source", "belong to a notional_rate rule")
}

// TestBothCachedReadSpellingsPrice: the main file has always written
// `cached_read` and the price catalog `cache_read`. A rule moved between the two
// files should not stop pricing because of an underscore.
func TestBothCachedReadSpellingsPrice(t *testing.T) {
	for _, name := range []string{"cached_read", "cache_read"} {
		if err := loadWith(t, "\npricing:\n  rules:\n    - id: r1\n      class: marginal_usage\n"+
			"      rates: {"+name+": \"0.00000019\"}\n"); err != nil {
			t.Errorf("%s must be a priced component: %v", name, err)
		}
	}
	// The same component twice under two names is a rule that prices one thing
	// twice, which is a mistake rather than a preference.
	mustRefuse(t, loadWith(t, "\npricing:\n  rules:\n    - id: r1\n      class: marginal_usage\n"+
		"      rates: {cache_read: \"0.1\", cached_read: \"0.2\"}\n"),
		"pricing.rules[0].rates[\"cached_read\"]", "priced twice")
}

// TestImagesIsRefusedAtLoad. It used to pass validation and then fail at
// assembly — a load error one layer too late, so `dorangctl config lint` said
// the file was good and the server refused to start.
func TestImagesIsRefusedAtLoad(t *testing.T) {
	mustRefuse(t, loadWith(t, "\npricing:\n  rules:\n    - id: r1\n      class: marginal_usage\n"+
		"      rates: {images: \"0.01\"}\n"),
		"pricing.rules[0].rates[\"images\"]", "not priced by this build")
}

// TestMetricsPublicIsExpressible: observability.prometheus was never read and
// /metrics was unconditional and unauthenticated. Opening it is now a written
// decision rather than the default.
func TestMetricsAccessIsExpressible(t *testing.T) {
	c, err := LoadBytes([]byte(baseYAML + "\nobservability:\n  prometheus: false\n  metrics: {public: true}\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Observability.PrometheusEnabled() {
		t.Error("prometheus: false did not survive loading")
	}
	if !c.Observability.MetricsPublic() {
		t.Error("metrics.public did not survive loading")
	}
	// And the default is the restrictive one.
	d, err := LoadBytes([]byte(baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if d.Observability.MetricsPublic() {
		t.Error("/metrics is public by default")
	}
	if !d.Observability.PrometheusEnabled() {
		t.Error("/metrics is off by default")
	}
}

// TestPrefixTTLIsPerBackend. One global hour modelled something that is not
// global: how long the BACKEND still holds the KV blocks. It is minutes for a
// hosted service and not a duration at all for vLLM or SGLang.
func TestPrefixTTLIsPerBackend(t *testing.T) {
	c, err := LoadBytes([]byte(`
version: 1
routing: {prefix: {ttl: 30m}}
providers:
  - {name: hosted, kind: openai, base_url: "https://example.invalid", prefix_ttl: 5m}
  - {name: selfhosted, kind: vllm, base_url: "https://example.invalid", prefix_ttl: until_evicted}
  - {name: plain, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: hosted, key_env: DORANG_TEST_FIXTURE_KEY}
  - {id: c2, provider: selfhosted, key_env: DORANG_TEST_FIXTURE_KEY}
  - {id: c3, provider: plain, key_env: DORANG_TEST_FIXTURE_KEY}
models:
  - name: m
    deployments:
      - {provider: hosted, upstream_model: u, credentials: [c1]}
      - {provider: selfhosted, upstream_model: u, credentials: [c2]}
      - {provider: plain, upstream_model: u, credentials: [c3]}
      - {provider: hosted, upstream_model: u2, credentials: [c1], prefix_ttl: 1h}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	deps := c.Models[0].Deployments
	for _, tc := range []struct {
		what string
		got  CacheTTL
		want string
	}{
		{"provider override", c.PrefixTTLFor("hosted", &deps[0]), "5m0s"},
		{"until_evicted", c.PrefixTTLFor("selfhosted", &deps[1]), UntilEvicted},
		{"global default", c.PrefixTTLFor("plain", &deps[2]), "30m0s"},
		{"deployment beats provider", c.PrefixTTLFor("hosted", &deps[3]), "1h0m0s"},
	} {
		if got := tc.got.String(); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.what, got, tc.want)
		}
	}
	// until_evicted has to survive as something internal/prefix can act on, and
	// zero is already spent on "use the default".
	if got := c.PrefixTTLFor("selfhosted", &deps[1]).TableTTL(); got >= 0 {
		t.Errorf("until_evicted rendered as %v, which internal/prefix reads as a duration", got)
	}
}

// TestPrefixTTLRejectsNonsense keeps the new spelling from becoming another
// accepted-and-inert setting in its own right.
func TestPrefixTTLRejectsNonsense(t *testing.T) {
	_, err := LoadBytes([]byte(baseYAML + "\nrouting: {prefix: {ttl: forever}}\n"))
	if err == nil {
		t.Fatal("an unparseable cache lifetime must be refused")
	}
	if !strings.Contains(err.Error(), UntilEvicted) {
		t.Errorf("the refusal does not name the alternative spelling: %v", err)
	}
}

// TestStateDirRelocatesTheDefaults. DORANG_STATE_DIR is set by the container
// image and was read by nothing, so every container wrote its database, its
// spool and its generated key pepper outside the declared volume — into the
// writable layer, lost on restart. Losing the pepper makes every issued api key
// unverifiable.
func TestStateDirRelocatesTheDefaults(t *testing.T) {
	t.Setenv(EnvStateDir, "/var/lib/dorang")
	if got := ExpandPath("~/.dorang/dorang.db"); got != "/var/lib/dorang/.dorang/dorang.db" {
		t.Errorf("ExpandPath = %q, want it under the state directory", got)
	}
	if got := StateDir(); got != "/var/lib/dorang" {
		t.Errorf("StateDir = %q", got)
	}
	// An absolute path in the file is an instruction, not a default: it must not
	// be silently re-rooted.
	if got := ExpandPath("/srv/dorang.db"); got != "/srv/dorang.db" {
		t.Errorf("an absolute path was re-rooted to %q", got)
	}
}
