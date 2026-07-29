package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// invoke runs one command in process and returns stdout, stderr and the exit
// code. The CLI takes its streams as parameters for exactly this reason: no
// subcommand writes to os.Stdout directly, so every one of them is testable.
func invoke(args ...string) (stdout, stderr string, code int) {
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return out.String(), errb.String(), code
}

// writeCfg writes a self-contained configuration into dir and returns its path.
func writeCfg(t *testing.T, dir, extra string) string {
	t.Helper()
	src := fmt.Sprintf(`version: 1
server:
  env: development
storage:
  driver: sqlite
  sqlite:
    path: %s/dorang.db
metering:
  spool:
    dir: %s/spool
%s`, dir, dir, extra)
	path := filepath.Join(dir, "dorang.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const pricedModel = `providers:
  - name: fake
    kind: openai
    base_url: https://example.invalid/v1
credentials:
  - id: fake-1
    provider: fake
    key: dev-secret
models:
  - name: model-x
    deployments:
      - provider: fake
        upstream_model: upstream-x
        credentials: [fake-1]
aliases:
  model-large: model-x
pricing:
  currency: USD
  rules:
    - id: fake-tokens
      class: marginal_usage
      match: {provider: fake}
      rates: {input: "3.00", output: "15.00"}
`

func TestConfigLintAcceptsAValidFile(t *testing.T) {
	dir := t.TempDir()
	path := writeCfg(t, dir, pricedModel)
	out, errOut, code := invoke("config", "lint", path)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("lint said nothing about the outcome: %s", out)
	}
}

// TestConfigLintRefusesUnsafeConfigurations is the CLI half of the server's
// refuse-to-start contract: the two must agree, or an operator can lint a
// configuration clean and then watch it fail to start.
func TestConfigLintRefusesUnsafeConfigurations(t *testing.T) {
	cases := []struct {
		name, yaml, expect string
	}{
		{
			"cluster with local capacity accounting",
			"version: 1\ncluster: {enabled: true, capacity_mode: local}\n",
			"capacity_mode",
		},
		{
			"inline literal secret outside development",
			"version: 1\nserver: {env: production}\n" +
				"providers:\n  - {name: p, kind: openai, base_url: \"https://example.invalid/v1\"}\n" +
				"credentials:\n  - {id: c, provider: p, key: \"a-literal\"}\n",
			"inline literal secret",
		},
		{
			"legacy hashing with no expiry",
			"version: 1\nauth:\n  legacy: {enabled: true}\n",
			"auth.legacy.until",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bad.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			out, errOut, code := invoke("config", "lint", path)
			if code == 0 {
				t.Fatalf("lint accepted an unsafe configuration\nstdout: %s", out)
			}
			if !strings.Contains(errOut, tc.expect) {
				t.Errorf("lint did not name %q\nstderr: %s", tc.expect, errOut)
			}
		})
	}
}

// TestConfigLintOnTheShippedExample checks the file operators copy. Its secret
// references point at an environment this machine does not have, which is a
// warning rather than a failure.
func TestConfigLintOnTheShippedExample(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "config.example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example configuration not present: %v", err)
	}
	out, errOut, code := invoke("config", "lint", path)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("no verdict printed: %s", out)
	}
}

func TestCatalogExplainReportsProvenance(t *testing.T) {
	out, errOut, code := invoke("catalog", "explain", "anthropic", "claude-sonnet-4-5")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, errOut)
	}
	for _, want := range []string{"api", "anthropic-messages", "embedded", "provider_defaults.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("the provenance view did not mention %q:\n%s", want, out)
		}
	}
	// An undeclared field must read as undeclared, not as zero (DESIGN §4.3).
	if !strings.Contains(out, "undeclared") {
		t.Errorf("undeclared fields were rendered as zeros:\n%s", out)
	}
}

func TestCatalogUnverifiedListsTheProbeList(t *testing.T) {
	out, errOut, code := invoke("catalog", "unverified")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, errOut)
	}
	if !strings.Contains(out, "probe") && !strings.Contains(out, "verification date") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

// TestPricePreviewShowsTheChain is DESIGN §8.4: the applied rule chain per
// class, each component's rate and quantity, the final amount, and why each rule
// was selected.
func TestPricePreviewShowsTheChain(t *testing.T) {
	dir := t.TempDir()
	path := writeCfg(t, dir, pricedModel)
	out, errOut, code := invoke("price", "model-x", "--config", path,
		"--input", "1000000", "--output", "1000000")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, errOut)
	}
	for _, want := range []string{
		"upstream-x",     // the target, resolved
		"fake-tokens",    // the rule that won
		"marginal_usage", // the class trace
		"per_1m_tokens",  // the unit the rate is quoted in
		"TOTAL",          // the final amount
		"notional",       // the §8.5 figure, present either way
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the preview did not mention %q:\n%s", want, out)
		}
	}
	// $3 per 1M input plus $15 per 1M output, one million of each.
	if !strings.Contains(out, "18.00") {
		t.Errorf("the total is not 18.00:\n%s", out)
	}
	// The alias resolves to the same answer.
	aliasOut, _, aliasCode := invoke("price", "model-large", "--config", path,
		"--input", "1000000", "--output", "1000000")
	if aliasCode != 0 || !strings.Contains(aliasOut, "18.00") {
		t.Errorf("the alias priced differently:\n%s", aliasOut)
	}
}

// TestPriceReportsAnUnpricedModel covers §8.3's rule that an unpriced model
// warns rather than costing zero in silence.
func TestPriceReportsAnUnpricedModel(t *testing.T) {
	dir := t.TempDir()
	path := writeCfg(t, dir, `providers:
  - name: fake
    kind: openai
    base_url: https://example.invalid/v1
credentials:
  - id: fake-1
    provider: fake
    key: dev-secret
models:
  - name: model-x
    deployments:
      - provider: fake
        upstream_model: upstream-x
        credentials: [fake-1]
`)
	out, errOut, code := invoke("price", "model-x", "--config", path, "--input", "100")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, errOut)
	}
	if !strings.Contains(errOut, "UNPRICED") {
		t.Errorf("an unpriced model was reported as costing zero:\nstdout: %s\nstderr: %s", out, errOut)
	}
}

func TestPriceRejectsAnUnknownModel(t *testing.T) {
	dir := t.TempDir()
	path := writeCfg(t, dir, pricedModel)
	_, errOut, code := invoke("price", "nope", "--config", path, "--input", "1")
	if code == 0 {
		t.Fatal("pricing an unknown model must fail")
	}
	if !strings.Contains(errOut, "nope") {
		t.Errorf("the message did not name the model: %s", errOut)
	}
}

// TestImportPrintsWarningsAndYAML covers the credential-import path: the
// converted configuration goes to stdout so it can be redirected into a file,
// and everything the import could not resolve goes to stderr.
func TestImportPrintsWarningsAndYAML(t *testing.T) {
	dir := t.TempDir()
	src := `model_list:
  - model_name: gpt-4o
    litellm_params:
      model: openai/gpt-4o
      api_key: os.environ/OPENAI_API_KEY
      api_base: https://api.openai.com/v1
  - model_name: mystery
    litellm_params:
      model: some-vendor/some-model
      api_key: os.environ/MYSTERY_KEY
`
	path := filepath.Join(dir, "foreign.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := invoke("import", "config", path)
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, errOut)
	}
	if !strings.Contains(out, "version: 1") || !strings.Contains(out, "gpt-4o") {
		t.Errorf("the converted configuration is not on stdout:\n%s", out)
	}
	if !strings.Contains(errOut, "warning:") {
		t.Errorf("an unresolvable entry produced no warning:\n%s", errOut)
	}
	// A model name is opaque and is never split to guess a provider (§2.1).
	if !strings.Contains(out, "some-vendor/some-model") {
		t.Errorf("the opaque model name did not survive the import:\n%s", out)
	}
}

// TestKeyLifecycle exercises migrate, create, list and revoke against a real
// SQLite database.
func TestKeyLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "cli-test-pepper")
	path := writeCfg(t, dir, "")

	if out, errOut, code := invoke("migrate", "--config", path); code != 0 {
		t.Fatalf("migrate exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	} else if !strings.Contains(out, "migration") {
		t.Errorf("migrate said nothing: %s", out)
	}
	// Migrations are idempotent, so a second run is a no-op rather than a
	// failure.
	if out, _, code := invoke("migrate", "--config", path); code != 0 {
		t.Fatalf("second migrate exit %d: %s", code, out)
	}

	token, errOut, code := invoke("key", "create", "--config", path,
		"--alias", "cli-test", "--rpm", "60", "--budget-usd", "5")
	if code != 0 {
		t.Fatalf("key create exit %d\nstderr: %s", code, errOut)
	}
	token = strings.TrimSpace(token) // pragma: allowlist secret — test fixture
	if !strings.HasPrefix(token, "sk-") {
		t.Fatalf("issued token %q does not carry the mandatory sk- prefix", token)
	}
	if !strings.Contains(errOut, "shown once") {
		t.Errorf("the CLI did not warn that the token is not recoverable: %s", errOut)
	}

	out, _, code := invoke("key", "list", "--config", path)
	if code != 0 {
		t.Fatalf("key list exit %d", code)
	}
	if !strings.Contains(out, "cli-test") || !strings.Contains(out, "active") {
		t.Fatalf("the new key is not in the listing:\n%s", out)
	}
	// The token must not be recoverable from the listing.
	if strings.Contains(out, token) {
		t.Error("the listing leaked the token")
	}

	id := listedID(t, out)
	if _, errOut, code := invoke("key", "revoke", id, "--config", path); code != 0 {
		t.Fatalf("key revoke exit %d\nstderr: %s", code, errOut)
	}
	out, _, _ = invoke("key", "list", "--config", path)
	if !strings.Contains(out, "blocked") {
		t.Errorf("the revoked key is not blocked:\n%s", out)
	}
	// And the revocation is PUBLISHED, which is the difference between a
	// running fleet honouring it within auth.revocation.poll and honouring it
	// when each node's credential cache happens to expire. This is a CLI: it has
	// no snapshot of its own to drop, so the durable message is its whole half
	// of DESIGN §11.2c, and asserting the row is the only way to observe it from
	// here.
	assertRevocationPublished(t, filepath.Join(dir, "dorang.db"), id)
	if _, _, code := invoke("key", "revoke", "no-such-id", "--config", path); code == 0 {
		t.Error("revoking an unknown id must fail")
	}
}

// assertRevocationPublished checks that `key revoke` left a message on the
// invalidation bus, not only a flag on the row.
func assertRevocationPublished(t *testing.T, dbPath, keyID string) {
	t.Helper()
	// The store's own driver name; importing internal/store registers it.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()

	var (
		n       int
		cause   string
		lookups string
	)
	row := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(cause), ''), COALESCE(MAX(lookups), '')
		 FROM key_invalidations WHERE key_id = ?`, keyID)
	if err := row.Scan(&n, &cause, &lookups); err != nil {
		t.Fatalf("read key_invalidations: %v", err)
	}
	if n != 1 {
		t.Fatalf("key revoke published %d invalidations for %s, want 1: a revocation that is "+
			"only a row is one the fleet honours a cache TTL later", n, keyID)
	}
	if cause != "revoked" {
		t.Errorf("published cause = %q, want %q", cause, "revoked")
	}
	if strings.TrimSpace(lookups) == "" {
		t.Error("the message names no index keys; the key id alone still works, but the drop " +
			"is a scan rather than a lookup and a negative entry is not caught at all")
	}
}

func listedID(t *testing.T, listing string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(listing), "\n")
	if len(lines) < 2 {
		t.Fatalf("no rows in the listing:\n%s", listing)
	}
	fields := strings.Fields(lines[1])
	if len(fields) == 0 {
		t.Fatalf("unreadable listing row: %q", lines[1])
	}
	return fields[0]
}

func TestUsageAndUnknownCommand(t *testing.T) {
	if out, _, code := invoke("--help"); code != 0 || !strings.Contains(out, "dorangctl") {
		t.Errorf("--help exit %d: %s", code, out)
	}
	if _, errOut, code := invoke("nonsense"); code == 0 || !strings.Contains(errOut, "nonsense") {
		t.Errorf("an unknown command must fail and name itself: %s", errOut)
	}
	if out, _, code := invoke("--version"); code != 0 || strings.TrimSpace(out) == "" {
		t.Errorf("--version exit %d: %q", code, out)
	}
}
