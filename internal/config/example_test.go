package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// examplePath is the shipped example, one directory up from the package.
const examplePath = "../../deploy/config.example.yaml"

// TestExampleConfigLoads keeps the shipped example honest: it must load through
// the same path a running gateway uses, secrets included.
func TestExampleConfigLoads(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "cloud-a-2.key")
	if err := os.WriteFile(keyFile, []byte("example-key-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	// The example points at /etc; the test supplies a readable file instead.
	src := strings.ReplaceAll(string(data), "/etc/dorang/secrets/cloud-a-2.key", keyFile)

	t.Setenv("DORANG_EXAMPLE_PLAN_A_KEY", "example-key-plan-a")
	t.Setenv("DORANG_EXAMPLE_CLOUD_A_KEY_1", "example-key-1")

	c, err := LoadBytes([]byte(src))
	if err != nil {
		t.Fatalf("the shipped example must load:\n%v", err)
	}

	// Spot-check that it exercises the whole schema rather than a corner of it.
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"two providers", len(c.Providers) == 2},
		{"three credentials", len(c.Credentials) == 3},
		{"two model groups", len(c.Models) == 2},
		{"aliases", len(c.Aliases) == 2},
		{"classes", len(c.Classes) == 2},
		{"capacity groups", len(c.Capacity.ProviderGroups) == 1 && len(c.Capacity.CredentialGroups) == 3},
		{"model axis", len(c.Capacity.Models) == 2},
		{"key rotation", len(c.KeyRotation.Providers["cloud-a"].Keys) == 2},
		{"pricing rules", len(c.Pricing.Rules) == 3},
		{"passthrough routes", len(c.Passthrough.Routes) == 2},
		{"priority emit", len(c.PriorityMapping.Emit.Backends) == 2},
		{"notification events", len(c.Notifications.Events) == 7},
	} {
		if !tc.ok {
			t.Errorf("the example does not cover %s", tc.name)
		}
	}

	// Secrets resolved from both an environment variable and a file.
	if cred, ok := c.Credential("plan-a-1"); ok {
		if v, _ := cred.Key.Value(); v != "example-key-plan-a" {
			t.Error("plan-a-1 did not resolve from the environment")
		}
	} else {
		t.Error("credential plan-a-1 is missing")
	}
	if cred, ok := c.Credential("acct-2"); ok {
		if v, _ := cred.Key.Value(); v != "example-key-2" {
			t.Error("acct-2 did not resolve from its key file, newline trimmed")
		}
	} else {
		t.Error("credential acct-2 is missing")
	}
}

// TestExampleConfigMatchesDefaults checks that the commented values really are
// the defaults they claim to be: loading the example and loading an almost
// empty file must agree on every documented default.
func TestExampleConfigMatchesDefaults(t *testing.T) {
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	example, err := decodeConfig(data, examplePath)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	bare := &Config{}
	bare.ApplyDefaults()

	for _, tc := range []struct {
		name      string
		got, want any
	}{
		{"server.listen", example.Server.Listen, bare.Server.Listen},
		{"server.env", example.Server.Env, bare.Server.Env},
		{"server.master_key_env", example.Server.MasterKeyEnv, bare.Server.MasterKeyEnv},
		{"server.key_pepper_env", example.Server.KeyPepperEnv, bare.Server.KeyPepperEnv},
		{"server.request_timeout", example.Server.RequestTimeout, bare.Server.RequestTimeout},
		{"server.shutdown_grace", example.Server.ShutdownGrace, bare.Server.ShutdownGrace},
		{"storage.driver", example.Storage.Driver, bare.Storage.Driver},
		{"storage.sqlite.path", example.Storage.SQLite.Path, bare.Storage.SQLite.Path},
		{"storage.postgres.url_env", example.Storage.Postgres.URLEnv, bare.Storage.Postgres.URLEnv},
		{"storage.postgres.max_conns", example.Storage.Postgres.MaxConns, bare.Storage.Postgres.MaxConns},
		{"cluster.capacity_mode", example.Cluster.CapacityMode, bare.Cluster.CapacityMode},
		{"cluster.min_leasable", example.Cluster.MinLeasable, bare.Cluster.MinLeasable},
		{"capacity.interactive_reserve", example.Capacity.Reserve(), bare.Capacity.Reserve()},
		{"routing.sticky.ttl", example.Routing.Sticky.TTL, bare.Routing.Sticky.TTL},
		{"routing.sticky.purge_interval", example.Routing.Sticky.PurgeInterval, bare.Routing.Sticky.PurgeInterval},
		{"routing.prefix.chunk_bytes", example.Routing.Prefix.ChunkBytes, bare.Routing.Prefix.ChunkBytes},
		{"routing.prefix.checkpoints", example.Routing.Prefix.Checkpoints, bare.Routing.Prefix.Checkpoints},
		{"routing.prefix.max_bytes", example.Routing.Prefix.MaxBytes, bare.Routing.Prefix.MaxBytes},
		{"routing.prefix.ttl", example.Routing.Prefix.TTL, bare.Routing.Prefix.TTL},
		{"fallbacks.max_hops", example.Fallbacks.MaxHops, bare.Fallbacks.MaxHops},
		{"fallbacks.budget_ms", example.Fallbacks.BudgetMS, bare.Fallbacks.BudgetMS},
		{"pricing.currency", example.Pricing.Currency, bare.Pricing.Currency},
		{"metering.trace.store_messages", example.Metering.Trace.StoreMessages, bare.Metering.Trace.StoreMessages},
		{"metering.trace.truncate_chars", example.Metering.Trace.TruncateChars, bare.Metering.Trace.TruncateChars},
		{"metering.trace.sample_rate", example.Metering.Trace.Rate(), bare.Metering.Trace.Rate()},
		{"metering.trace.daily_byte_budget", example.Metering.Trace.DailyByteBudget, bare.Metering.Trace.DailyByteBudget},
		{"metering.spool.dir", example.Metering.Spool.Dir, bare.Metering.Spool.Dir},
		{"metering.spool.max_bytes", example.Metering.Spool.MaxBytes, bare.Metering.Spool.MaxBytes},
		{"metering.flush_interval", example.Metering.FlushInterval, bare.Metering.FlushInterval},
		{"observability.log_level", example.Observability.LogLevel, bare.Observability.LogLevel},
		{"observability.log_format", example.Observability.LogFormat, bare.Observability.LogFormat},
		{"extensions.lua.dir", example.Extensions.Lua.Dir, bare.Extensions.Lua.Dir},
		{"extensions.lua.limits", example.Extensions.Lua.Limits, bare.Extensions.Lua.Limits},
		{"passthrough.default.auth", example.Passthrough.Default.Auth, bare.Passthrough.Default.Auth},
		{"passthrough.default.timeout", example.Passthrough.Default.Timeout, bare.Passthrough.Default.Timeout},
		{"shadow.mode", example.Shadow.Mode, bare.Shadow.Mode},
		{"notifications.email.driver", example.Notifications.Email.Driver, bare.Notifications.Email.Driver},
		{"priority_mapping.emit.header", example.PriorityMapping.Emit.Header, bare.PriorityMapping.Emit.Header},
	} {
		if tc.got != tc.want {
			t.Errorf("the example writes %s = %v, but the default is %v", tc.name, tc.got, tc.want)
		}
	}
}
