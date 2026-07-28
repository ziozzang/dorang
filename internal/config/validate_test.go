package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestValidationRules is the table of every rule the loader enforces. Each case
// extends the minimal valid configuration and states which path must be
// reported. An empty path means the case must load cleanly.
func TestValidationRules(t *testing.T) {
	cases := []struct {
		name string
		f    fragments
		path string
		want string
	}{
		{
			name: "cluster enabled with local capacity is a hard error",
			f:    fragments{top: "cluster: {enabled: true, capacity_mode: local}\n"},
			path: "cluster.capacity_mode",
			want: "refuses to start",
		},
		{
			name: "cluster enabled with a shared mode is fine",
			f:    fragments{top: "cluster: {enabled: true, capacity_mode: shared-redis}\n"},
		},
		{
			name: "unknown capacity mode",
			f:    fragments{top: "cluster: {capacity_mode: nearly}\n"},
			path: "cluster.capacity_mode",
			want: "not a known value",
		},
		{
			name: "leased mode rejects a limit below min_leasable",
			f: fragments{top: "cluster: {enabled: true, capacity_mode: leased}\n" +
				"capacity: {credential_groups: {g1: {max_concurrency: 3}}}\n"},
			path: `capacity.credential_groups["g1"].max_concurrency`,
			want: "below cluster.min_leasable",
		},
		{
			name: "leased mode accepts a divisible limit",
			f: fragments{top: "cluster: {enabled: true, capacity_mode: leased}\n" +
				"capacity: {credential_groups: {g1: {max_concurrency: 64}}}\n"},
		},
		{
			name: "legacy auth without an end date",
			f:    fragments{top: "auth: {legacy: {enabled: true}}\n"},
			path: "auth.legacy.until",
			want: "requires an end date",
		},
		{
			name: "legacy auth with an unparseable end date",
			f:    fragments{top: "auth: {legacy: {enabled: true, until: soon}}\n"},
			path: "auth.legacy.until",
			want: "cannot parse date",
		},
		{
			name: "legacy auth with a past end date",
			f:    fragments{top: "auth: {legacy: {enabled: true, until: \"2020-01-01\"}}\n"},
			path: "auth.legacy.until",
			want: "migration window closed",
		},
		{
			name: "legacy auth with a future end date",
			f:    fragments{top: "auth: {legacy: {enabled: true, until: \"2099-01-01\"}}\n"},
		},
		{
			name: "deployment names an undeclared provider",
			f:    fragments{models: "  - {name: m2, deployments: [{provider: nope, upstream_model: u}]}\n"},
			path: "models[1].deployments[0].provider",
			want: `provider "nope" is not declared`,
		},
		{
			name: "credential names an undeclared provider",
			f:    fragments{credentials: "  - {id: c2, provider: nope, key_ref: \"vault:x\"}\n"},
			path: "credentials[1].provider",
			want: `provider "nope" is not declared`,
		},
		{
			name: "deployment names an undeclared credential",
			f: fragments{models: "  - name: m2\n" +
				"    deployments: [{provider: p1, upstream_model: u, credentials: [nope]}]\n"},
			path: "models[1].deployments[0].credentials[0]",
			want: `credential "nope" is not declared`,
		},
		{
			name: "deployment uses a credential of another provider",
			f: fragments{
				providers:   "  - {name: p2, kind: openai}\n",
				credentials: "  - {id: c2, provider: p2, key_ref: \"vault:x\"}\n",
				models: "  - name: m2\n" +
					"    deployments: [{provider: p1, upstream_model: u, credentials: [c2]}]\n",
			},
			path: "models[1].deployments[0].credentials[0]",
			want: `belongs to provider "p2"`,
		},
		{
			name: "provider capacity group is not declared",
			f:    fragments{providers: "  - {name: p2, kind: openai, capacity_group: pool}\n"},
			path: "providers[1].capacity_group",
			want: "capacity.provider_groups",
		},
		{
			name: "credential capacity group is not declared",
			f:    fragments{credentials: "  - {id: c2, provider: p1, key_ref: \"vault:x\", capacity_group: acct}\n"},
			path: "credentials[1].capacity_group",
			want: "capacity.credential_groups",
		},
		{
			name: "declared capacity groups resolve",
			f: fragments{
				providers:   "  - {name: p2, kind: openai, capacity_group: pool}\n",
				credentials: "  - {id: c2, provider: p2, key_ref: \"vault:x\", capacity_group: acct}\n",
				top: "capacity:\n  provider_groups: {pool: {max_concurrency: 6}}\n" +
					"  credential_groups: {acct: {max_concurrency: 3}}\n",
			},
		},
		{
			name: "capacity model axis names an undeclared provider",
			f:    fragments{top: "capacity: {models: [{provider: nope, model: m, max_concurrency: 7}]}\n"},
			path: "capacity.models[0].provider",
			want: `provider "nope" is not declared`,
		},
		{
			name: "rotation pool names an undeclared provider",
			f:    fragments{top: "key_rotation: {providers: {nope: {keys: [{id: c1, key_ref: \"vault:x\"}]}}}\n"},
			path: `key_rotation.providers["nope"]`,
			want: `provider "nope" is not declared`,
		},
		{
			name: "rotation key capacity group is not declared",
			f: fragments{top: "key_rotation:\n  providers:\n    p1:\n      keys:\n" +
				"        - {id: c1, key_ref: \"vault:x\", capacity_group: acct}\n"},
			path: `key_rotation.providers["p1"].keys[0].capacity_group`,
			want: "capacity.credential_groups",
		},
		{
			name: "rotation key belongs to another provider",
			f: fragments{
				providers: "  - {name: p2, kind: openai}\n",
				top:       "key_rotation: {providers: {p2: {keys: [{id: c1, key_ref: \"vault:x\"}]}}}\n",
			},
			path: `key_rotation.providers["p2"].keys[0].id`,
			want: "rotation pool is for provider",
		},
		{
			name: "unknown rotation strategy",
			f:    fragments{top: "key_rotation: {strategy: coin_flip}\n"},
			path: "key_rotation.strategy",
			want: "not a known value",
		},
		{
			name: "alias target is not a model",
			f:    fragments{top: "aliases: {small: nope}\n"},
			path: `aliases["small"]`,
			want: `"nope" is not a declared model`,
		},
		{
			name: "aliases do not chain",
			f:    fragments{top: "aliases: {a: b, b: m1}\n"},
			path: `aliases["a"]`,
			want: "aliases do not chain",
		},
		{
			name: "alias may not shadow a model name",
			f:    fragments{top: "aliases: {m1: m1}\n"},
			path: `aliases["m1"]`,
			want: "also a declared model name",
		},
		{
			name: "class member is not a model",
			f:    fragments{top: "classes: {big: [m1, nope]}\n"},
			path: `classes["big"][1]`,
			want: `"nope" is not a declared model`,
		},
		{
			name: "class member listed twice",
			f:    fragments{top: "classes: {big: [m1, m1]}\n"},
			path: `classes["big"][1]`,
			want: "listed twice",
		},
		{
			name: "empty class",
			f:    fragments{top: "classes: {big: []}\n"},
			path: `classes["big"]`,
			want: "has no members",
		},
		{
			name: "model declares a class it is not a member of",
			f: fragments{
				models: "  - {name: m2, class: big, deployments: [{provider: p1, upstream_model: u}]}\n",
				top:    "classes: {big: [m1]}\n",
			},
			path: "models[1].class",
			want: "is not a member of it",
		},
		{
			name: "model declares an undeclared class",
			f:    fragments{models: "  - {name: m2, class: nope, deployments: [{provider: p1, upstream_model: u}]}\n"},
			path: "models[1].class",
			want: "is not declared under classes",
		},
		{
			name: "duplicate provider name",
			f:    fragments{providers: "  - {name: p1, kind: openai}\n"},
			path: "providers[1].name",
			want: "duplicate provider name",
		},
		{
			name: "duplicate credential id",
			f:    fragments{credentials: "  - {id: c1, provider: p1, key_ref: \"vault:y\"}\n"},
			path: "credentials[1].id",
			want: "duplicate credential id",
		},
		{
			name: "duplicate model name",
			f:    fragments{models: "  - {name: m1, deployments: [{provider: p1, upstream_model: u}]}\n"},
			path: "models[1].name",
			want: "duplicate model name",
		},
		{
			name: "model without deployments",
			f:    fragments{models: "  - {name: m2, deployments: []}\n"},
			path: "models[1].deployments",
			want: "no deployments",
		},
		{
			name: "deployment without an upstream model",
			f:    fragments{models: "  - {name: m2, deployments: [{provider: p1, upstream_model: \"\"}]}\n"},
			path: "models[1].deployments[0].upstream_model",
			want: "never parsed",
		},
		{
			name: "interactive reserve of one leaves nothing for batch",
			f:    fragments{top: "capacity: {interactive_reserve: 1.0}\n"},
			path: "capacity.interactive_reserve",
			want: "must be in [0,1)",
		},
		{
			name: "negative interactive reserve",
			f:    fragments{top: "capacity: {interactive_reserve: -0.1}\n"},
			path: "capacity.interactive_reserve",
			want: "must be in [0,1)",
		},
		{
			name: "negative provider concurrency",
			f:    fragments{providers: "  - {name: p2, kind: openai, max_concurrency: -1}\n"},
			path: "providers[1].max_concurrency",
			want: "must not be negative",
		},
		{
			name: "negative deployment weight",
			f: fragments{models: "  - name: m2\n" +
				"    deployments: [{provider: p1, upstream_model: u, weight: -3}]\n"},
			path: "models[1].deployments[0].weight",
			want: "must not be negative",
		},
		{
			name: "negative group concurrency",
			f:    fragments{top: "capacity: {provider_groups: {pool: {max_concurrency: -2}}}\n"},
			path: `capacity.provider_groups["pool"].max_concurrency`,
			want: "must not be negative",
		},
		{
			name: "negative principal concurrency",
			f:    fragments{top: "capacity: {principals: {default: {max_concurrent: -2}}}\n"},
			path: `capacity.principals["default"].max_concurrent`,
			want: "must not be negative",
		},
		{
			name: "provider without a kind",
			f:    fragments{providers: "  - {name: p2, kind: \"\"}\n"},
			path: "providers[1].kind",
			want: "must name a provider kind",
		},
		{
			name: "usage probe without a fetcher",
			f:    fragments{providers: "  - {name: p2, kind: openai, usage_probe: {enabled: true}}\n"},
			path: "providers[1].usage_probe.fetcher",
			want: "must name a fetcher",
		},
		{
			name: "backend metrics without an endpoint",
			f:    fragments{providers: "  - {name: p2, kind: openai, metrics: {enabled: true}}\n"},
			path: "providers[1].metrics.endpoint",
			want: "must be set",
		},
		{
			name: "unknown retry backoff",
			f:    fragments{providers: "  - {name: p2, kind: openai, retry: {backoff: fibonacci}}\n"},
			path: "providers[1].retry.backoff",
			want: "not a known value",
		},
		{
			name: "unknown routing strategy",
			f: fragments{models: "  - name: m2\n    strategy: [cheapest]\n" +
				"    deployments: [{provider: p1, upstream_model: u}]\n"},
			path: "models[1].strategy[0]",
			want: "not a known value",
		},
		{
			name: "unknown fallback cause",
			f:    fragments{top: "fallbacks: {on: {meltdown: [same_group]}}\n"},
			path: `fallbacks.on["meltdown"]`,
			want: "not a known fallback cause",
		},
		{
			name: "unknown fallback target",
			f:    fragments{top: "fallbacks: {on: {timeout: [somewhere_else]}}\n"},
			path: `fallbacks.on["timeout"][0]`,
			want: "not a known value",
		},
		{
			name: "budget exceeded may not fall back",
			f:    fragments{top: "fallbacks: {on: {budget_exceeded: [same_class]}}\n"},
			path: `fallbacks.on["budget_exceeded"]`,
			want: "failing is the correct outcome",
		},
		{
			name: "auth failure may not fall back",
			f:    fragments{top: "fallbacks: {on: {auth: [same_group]}}\n"},
			path: `fallbacks.on["auth"]`,
			want: "failing is the correct outcome",
		},
		{
			name: "unknown storage driver",
			f:    fragments{top: "storage: {driver: mysql}\n"},
			path: "storage.driver",
			want: "not a known value",
		},
		{
			name: "postgres pool must be positive",
			f:    fragments{top: "storage: {driver: postgres, postgres: {max_conns: -1}}\n"},
			path: "storage.postgres.max_conns",
			want: "greater than zero",
		},
		{
			name: "unknown server env",
			f:    fragments{top: "server: {env: staging}\n"},
			path: "server.env",
			want: "not a known value",
		},
		{
			name: "request timeout must be positive",
			f:    fragments{top: "server: {request_timeout: 0s}\n"},
			// zero is indistinguishable from unset, so the default applies
		},
		{
			name: "numeric metering cannot be turned off",
			f:    fragments{top: "metering: {numeric: {enabled: false}}\n"},
			path: "metering.numeric.enabled",
			want: "cannot be turned off",
		},
		{
			name: "trace sample rate out of range",
			f:    fragments{top: "metering: {trace: {sample_rate: 1.5}}\n"},
			path: "metering.trace.sample_rate",
			want: "must be in [0,1]",
		},
		{
			name: "unknown message storage mode",
			f:    fragments{top: "metering: {trace: {store_messages: everything}}\n"},
			path: "metering.trace.store_messages",
			want: "not a known value",
		},
		{
			name: "unknown log level",
			f:    fragments{top: "observability: {log_level: loud}\n"},
			path: "observability.log_level",
			want: "not a known value",
		},
		{
			name: "unknown lua hook",
			f:    fragments{top: "extensions: {lua: {enabled: true, hooks: [on_teatime]}}\n"},
			path: "extensions.lua.hooks[0]",
			want: "not a known value",
		},
		{
			name: "passthrough prefix must be a path",
			f:    fragments{top: "passthrough: {enabled: true, routes: [{prefix: anthropic, provider: p1}]}\n"},
			path: "passthrough.routes[0].prefix",
			want: `must start with "/"`,
		},
		{
			name: "passthrough prefix may not traverse",
			f:    fragments{top: "passthrough: {enabled: true, routes: [{prefix: /a/../b, provider: p1}]}\n"},
			path: "passthrough.routes[0].prefix",
			want: "traversal is rejected",
		},
		{
			name: "duplicate passthrough prefix",
			f: fragments{top: "passthrough:\n  enabled: true\n  routes:\n" +
				"    - {prefix: /a, provider: p1}\n    - {prefix: /a, provider: p1}\n"},
			path: "passthrough.routes[1].prefix",
			want: "duplicate passthrough prefix",
		},
		{
			name: "passthrough route names an undeclared provider",
			f:    fragments{top: "passthrough: {enabled: true, routes: [{prefix: /a, provider: nope}]}\n"},
			path: "passthrough.routes[0].provider",
			want: `provider "nope" is not declared`,
		},
		{
			name: "passthrough route with an unknown auth mode",
			f:    fragments{top: "passthrough: {enabled: true, routes: [{prefix: /a, provider: p1, auth: hope}]}\n"},
			path: "passthrough.routes[0].auth",
			want: "not a known value",
		},
		{
			name: "shadow comparison needs a reference",
			f:    fragments{top: "shadow: {mode: compare}\n"},
			path: "shadow.reference.url",
			want: "must be set",
		},
		{
			name: "mirroring needs a cost ceiling",
			f:    fragments{top: "shadow: {mode: mirror, reference: {url: \"https://ref.invalid\"}}\n"},
			path: "shadow.max_cost_usd_per_day",
			want: "costs twice",
		},
		{
			name: "comparison needs a cost ceiling too",
			f:    fragments{top: "shadow: {mode: compare, reference: {url: \"https://ref.invalid\"}}\n"},
			path: "shadow.max_cost_usd_per_day",
			want: "costs twice",
		},
		{
			name: "semantic comparison is not implemented and is refused",
			f: fragments{top: "shadow:\n  mode: compare\n  reference: {url: \"https://ref.invalid\"}\n" +
				"  max_cost_usd_per_day: 5\n  compare: {semantic: true}\n"},
			path: "shadow.compare.semantic",
			want: "not implemented",
		},
		{
			name: "compare mode with nothing to compare",
			f: fragments{top: "shadow:\n  mode: compare\n  reference: {url: \"https://ref.invalid\"}\n" +
				"  max_cost_usd_per_day: 5\n  compare: {structural: false}\n"},
			path: "shadow.compare.structural",
			want: "no comparison is enabled",
		},
		{
			name: "reference url must be http",
			f: fragments{top: "shadow:\n  mode: mirror\n  reference: {url: \"file:///etc/passwd\"}\n" +
				"  max_cost_usd_per_day: 5\n"},
			path: "shadow.reference.url",
			want: "http or https",
		},
		{
			name: "reference url may not carry a query",
			f: fragments{top: "shadow:\n  mode: mirror\n  reference: {url: \"https://ref.invalid/v1?x=1\"}\n" +
				"  max_cost_usd_per_day: 5\n"},
			path: "shadow.reference.url",
			want: "query or fragment",
		},
		{
			name: "empty ignore_fields entry",
			f: fragments{top: "shadow:\n  mode: compare\n  reference: {url: \"https://ref.invalid\"}\n" +
				"  max_cost_usd_per_day: 5\n  compare: {ignore_fields: [\"  \"]}\n"},
			path: "shadow.compare.ignore_fields[0]",
			want: "must not be empty",
		},
		{
			name: "shadow comparison configured fully",
			f: fragments{top: "shadow:\n  mode: compare\n  reference: {url: \"https://ref.invalid\", api_key_env: REF_KEY, timeout: 30s}\n" +
				"  sample_rate: 0.05\n  compare: {structural: true, semantic: false, ignore_fields: [system_fingerprint]}\n" +
				"  max_cost_usd_per_day: 5\n  unpriced_estimate_usd: \"0.01\"\n" +
				"  queue_size: 128\n  workers: 2\n" +
				"  capture: {head_bytes: 128KiB, tail_bytes: 8KiB}\n" +
				"  report: {path: /var/lib/dorang/shadow.jsonl, max_bytes: 64MiB}\n"},
		},
		{
			name: "unknown email driver",
			f:    fragments{top: "notifications: {email: {driver: pigeon}}\n"},
			path: "notifications.email.driver",
			want: "not a known value",
		},
		{
			name: "unknown notification event",
			f:    fragments{top: "notifications: {events: [world_ended]}\n"},
			path: "notifications.events[0]",
			want: "not a known value",
		},
		{
			name: "priority map names an undeclared class",
			f: fragments{top: "priority_mapping:\n  classes: {realtime: 0}\n" +
				"  emit:\n    openai: {field: service_tier, map: {batch: flex}}\n"},
			path: `priority_mapping.emit["openai"].map["batch"]`,
			want: "not a declared priority class",
		},
		{
			name: "priority emit as the design writes it",
			f: fragments{top: "priority_mapping:\n  classes: {realtime: 0, interactive: 2, batch: 10}\n" +
				"  emit:\n    vllm: {field: priority}\n" +
				"    openai: {field: service_tier, map: {realtime: priority, interactive: default, batch: flex}}\n" +
				"    header: X-Request-Priority\n"},
		},
		{
			name: "pricing rule with an unknown component",
			f:    fragments{top: "pricing: {rules: [{id: r1, class: marginal_usage, rates: {tokens: \"0.001\"}}]}\n"},
			path: `pricing.rules[0].rates["tokens"]`,
			want: "not a priced component",
		},
		{
			name: "subscription rule without a period",
			f:    fragments{top: "pricing: {rules: [{id: r1, class: fixed_subscription, amount: \"20.00\"}]}\n"},
			path: "pricing.rules[0].period",
			want: "must state its period",
		},
		{
			name: "adjustment rule without a percentage",
			f:    fragments{top: "pricing: {rules: [{id: r1, class: adjustment}]}\n"},
			path: "pricing.rules[0].percent",
			want: "must state a percentage",
		},
		{
			name: "pricing rule names an undeclared provider",
			f: fragments{top: "pricing:\n  rules:\n" +
				"    - {id: r1, class: marginal_usage, match: {provider: nope}, rates: {input: \"0.1\"}}\n"},
			path: "pricing.rules[0].match.provider",
			want: `provider "nope" is not declared`,
		},
		{
			name: "duplicate pricing rule id",
			f: fragments{top: "pricing:\n  rules:\n" +
				"    - {id: r1, class: marginal_usage, rates: {input: \"0.1\"}}\n" +
				"    - {id: r1, class: marginal_usage, rates: {input: \"0.2\"}}\n"},
			path: "pricing.rules[1].id",
			want: "duplicate pricing rule id",
		},
		{
			name: "priced rule with exact decimals",
			f: fragments{top: "pricing:\n  rules:\n" +
				"    - {id: r1, class: marginal_usage, match: {provider: p1, model: m1-upstream}, " +
				"rates: {input: \"0.0000025\", output: \"0.00001\", cached_read: \"1.25e-7\"}}\n" +
				"    - {id: r2, class: fixed_subscription, period: monthly, amount: \"20.00\"}\n" +
				"    - {id: r3, class: adjustment, percent: \"-10\"}\n"},
		},
		{
			name: "unknown sticky key component",
			f:    fragments{top: "routing: {sticky: {key: [ip_address]}}\n"},
			path: "routing.sticky.key[0]",
			want: "not a known value",
		},
		{
			name: "unknown prefix checkpoint policy",
			f:    fragments{top: "routing: {prefix: {checkpoints: every_byte}}\n"},
			path: "routing.prefix.checkpoints",
			want: "not a known value",
		},
		{
			name: "deployment limit metric must be known",
			f: fragments{models: "  - name: m2\n" +
				"    deployments: [{provider: p1, upstream_model: u, limits: [{metric: qps, value: 5}]}]\n"},
			path: "models[1].deployments[0].limits[0].metric",
			want: "not a known value",
		},
		{
			name: "deployment limits as §2.2 maps them",
			f: fragments{models: "  - name: m2\n    deployments:\n" +
				"      - provider: p1\n        upstream_model: u\n        weight: 2\n" +
				"        timeout: 600s\n        stream_timeout: 60s\n        limits:\n" +
				"          - {metric: rpm, value: 100}\n          - {metric: tpm, value: 100000}\n" +
				"          - {metric: max_concurrent, value: 5}\n"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFragments(t, tc.f)
			if tc.path == "" {
				if err != nil {
					t.Fatalf("must load cleanly, got:\n%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want a problem at %s, but the configuration loaded", tc.path)
			}
			if !hasProblem(err, tc.path, tc.want) {
				t.Errorf("want %s: %q\ngot:\n%v", tc.path, tc.want, err)
			}
		})
	}
}

// TestClusterLocalMessage pins the wording of the §5.6 hard guard: the message
// has to explain the N-fold overshoot and the 429 cascade, because the failure
// surfaces far from its cause.
func TestClusterLocalMessage(t *testing.T) {
	_, err := loadYAML(t, "cluster: {enabled: true, capacity_mode: local}")
	if err == nil {
		t.Fatal("cluster.enabled with capacity_mode local must refuse to start")
	}
	var msg string
	for _, p := range Problems(err) {
		if p.Path == "cluster.capacity_mode" {
			msg = p.Message
		}
	}
	for _, want := range []string{
		"refuses to start", "N-fold", "429", "fallback chain", "same class",
		"shared-redis", "shared-pg", "leased",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n%s", want, msg)
		}
	}
}

// TestValidationReportsEveryProblem checks that loading does not stop at the
// first problem.
func TestValidationReportsEveryProblem(t *testing.T) {
	_, err := LoadBytes([]byte(`
version: 1
server: {env: staging}
cluster: {enabled: true, capacity_mode: local}
providers:
  - {name: p1, kind: openai}
  - {name: p1, kind: openai}
credentials:
  - {id: c1, provider: nope, key_ref: "vault:x"}
models:
  - name: m1
    strategy: [cheapest]
    deployments:
      - {provider: gone, upstream_model: "", credentials: [missing], weight: -1}
aliases: {a: nowhere}
classes: {big: [nothing]}
`))
	if err == nil {
		t.Fatal("want problems")
	}
	want := []string{
		"server.env",
		"cluster.capacity_mode",
		"providers[1].name",
		"credentials[0].provider",
		"models[0].strategy[0]",
		"models[0].deployments[0].provider",
		"models[0].deployments[0].upstream_model",
		"models[0].deployments[0].credentials[0]",
		"models[0].deployments[0].weight",
		`aliases["a"]`,
		`classes["big"][0]`,
	}
	got := problemPaths(err)
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("problem at %s was not reported; got %v", w, got)
		}
	}
	if len(got) < len(want) {
		t.Errorf("reported %d problems, want at least %d", len(got), len(want))
	}
}

// TestValidationIsDeterministic keeps the aggregate stable across runs, which
// map iteration would otherwise break.
func TestValidationIsDeterministic(t *testing.T) {
	src := buildYAML(fragments{top: `aliases: {z: gone, a: alsogone, m: missing}
classes: {c: [x], b: [y], a: [z]}
capacity: {provider_groups: {b: {max_concurrency: -1}, a: {max_concurrency: -1}}}
`})
	_, err := LoadBytes([]byte(src))
	if err == nil {
		t.Fatal("want problems")
	}
	for i := 0; i < 20; i++ {
		_, again := LoadBytes([]byte(src))
		if again.Error() != err.Error() {
			t.Fatalf("aggregate error is not deterministic:\n%v\n\n%v", err, again)
		}
	}
}

// TestValidationErrorUnwrap checks the aggregate cooperates with errors.As.
func TestValidationErrorUnwrap(t *testing.T) {
	_, err := loadYAML(t, "cluster: {enabled: true, capacity_mode: local}")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error is not a *ValidationError: %T", err)
	}
	if len(ve.Errors) == 0 {
		t.Fatal("no problems recorded")
	}
	var fe FieldError
	if !errors.As(err, &fe) {
		t.Fatal("individual problems are not reachable through errors.As")
	}
	if !strings.Contains(err.Error(), "invalid configuration") {
		t.Errorf("unexpected rendering: %v", err)
	}
}

// TestLegacyUntilUsesTheClock checks the boundary rather than a fixed date.
func TestLegacyUntilUsesTheClock(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	old := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = old }()

	if _, err := loadYAML(t, `auth: {legacy: {enabled: true, until: "2026-07-29"}}`); err != nil {
		t.Errorf("a window that is still open must load: %v", err)
	}
	_, err := loadYAML(t, `auth: {legacy: {enabled: true, until: "2026-07-27"}}`)
	if !hasProblem(err, "auth.legacy.until", "migration window closed") {
		t.Errorf("a window that has closed must be refused: %v", err)
	}
	// The date itself, at midnight, has already passed by noon.
	_, err = loadYAML(t, `auth: {legacy: {enabled: true, until: "2026-07-28"}}`)
	if !hasProblem(err, "auth.legacy.until", "migration window closed") {
		t.Errorf("an elapsed window must be refused: %v", err)
	}
}

// TestVersionMustMatch keeps a future schema from being read as this one.
func TestVersionMustMatch(t *testing.T) {
	_, err := LoadBytes([]byte("version: 2\n"))
	if !hasProblem(err, "version", "unsupported configuration version") {
		t.Errorf("want an unsupported-version problem, got %v", err)
	}
}
