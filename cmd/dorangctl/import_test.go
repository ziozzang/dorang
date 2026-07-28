package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
)

// theSecret is the value that must not appear anywhere. It is shaped like a
// real credential so that a scrubber keyed on a prefix cannot pass by accident,
// and it is unmistakably fake so a match in a log is unambiguous.
const theSecret = "sk-FAKE-NOT-A-REAL-KEY-000000000000000000"

// secondSecret exists so the tests can tell "one credential is handled" from
// "credentials are handled".
const secondSecret = "sk-FAKE-SECOND-KEY-1111111111111111111111"

// assertNoSecret is the property every test in this file asserts: the bytes of
// a secret appear in NEITHER stream.
//
// It is written against both streams together because the two are one output as
// far as an operator's terminal, a CI log, or `2>&1 | tee` is concerned. A test
// that only checked the happy path's formatting on stdout would have passed
// against the version that printed the key on stdout with a warning on stderr,
// and against every version that moves the leak from one stream to the other.
func assertNoSecret(t *testing.T, what, stdout, stderr string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(stdout, s) {
			t.Errorf("%s: the secret is on stdout:\n%s", what, stdout)
		}
		if strings.Contains(stderr, s) {
			t.Errorf("%s: the secret is on stderr:\n%s", what, stderr)
		}
	}
}

// writeSource writes a foreign proxy configuration to import.
func writeSource(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestImportConfigNeverPrintsTheSecret is the finding: `dorangctl import config`
// re-emitted every literal api_key from the source file as a plaintext `key:`
// on stdout — into the generated configuration, into terminal scrollback, and
// into the CI log of a scripted migration. DESIGN §4.1 is the rule the rest of
// the codebase follows; this is the one tool whose job is to produce a
// configuration, and it was writing secrets into one.
func TestImportConfigNeverPrintsTheSecret(t *testing.T) {
	path := writeSource(t, `
model_list:
  - model_name: gpt-4o
    litellm_params:
      model: openai/gpt-4o
      api_key: `+theSecret+`
`)
	stdout, stderr, code := invoke("import", "config", path)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	assertNoSecret(t, "import config", stdout, stderr, theSecret)

	// What the operator gets instead: a reference in the generated file, and the
	// name of the variable to put the value in. That is the whole of what §4.1
	// lets leave the subsystem, and it is enough to finish the migration.
	if !strings.Contains(stdout, "key_env: DORANG_IMPORTED_API_KEY") {
		t.Errorf("the generated configuration names no environment variable:\n%s", stdout)
	}
	if !strings.Contains(stderr, "DORANG_IMPORTED_API_KEY") {
		t.Errorf("stderr never tells the operator which variable to set:\n%s", stderr)
	}
	// No fragment of the secret rides along either — a fingerprint is an
	// identifier, not an encoding.
	if strings.Contains(stdout+stderr, theSecret[3:11]) {
		t.Errorf("a slice of the secret was printed:\n%s\n%s", stdout, stderr)
	}
	// And nothing was scrubbed on the way out, which would mean the redaction
	// missed a copy and the backstop caught it.
	if strings.Contains(stdout, "<redacted>") {
		t.Errorf("a secret reached the encoder and had to be scrubbed:\n%s", stdout)
	}
}

// TestRedactImportedCredentialsMovesTheLiteralOut tests the CLI's own redaction
// against a configuration that holds literals, whatever the importer did.
//
// It is separate from the end-to-end test on purpose. internal/config was
// changed to stop CREATING inline literals, which is the better place for it and
// which means the end-to-end path no longer reaches this code with a literal in
// hand. That makes the end-to-end test a proof about the importer, not about the
// writer — and the writer is the thing that must never emit a secret no matter
// where the configuration came from. A `Config` built by hand is the only way to
// keep asserting that.
func TestRedactImportedCredentialsMovesTheLiteralOut(t *testing.T) {
	cfg := &config.Config{Credentials: []config.Credential{
		{ID: "openai-key-1", Provider: "openai", Key: config.SecretRef{Inline: theSecret}},
		{ID: "openai-key-2", Provider: "openai", Key: config.SecretRef{Inline: secondSecret}},
		{ID: "azure-key-1", Provider: "azure eu/west", Key: config.SecretRef{Env: "ALREADY_SET"}},
	}}
	creds, secrets := redactImportedCredentials(cfg)

	if len(creds) != 2 {
		t.Fatalf("reported %d credentials, want 2: %+v", len(creds), creds)
	}
	if len(secrets) != 2 {
		t.Fatalf("collected %d literals, want 2", len(secrets))
	}
	for i, c := range cfg.Credentials {
		if c.Key.Inline != "" {
			t.Errorf("credential %d still holds a literal after redaction", i)
		}
	}
	if a, b := cfg.Credentials[0].Key.Env, cfg.Credentials[1].Key.Env; a == b {
		t.Errorf("two credentials of one provider share the variable %q", a)
	}
	if got := cfg.Credentials[2].Key.Env; got != "ALREADY_SET" {
		t.Errorf("an existing reference was rewritten to %q", got)
	}
	for _, c := range creds {
		if c.id == "" || c.provider == "" || c.env == "" {
			t.Errorf("an incomplete report leaves the operator nothing to act on: %+v", c)
		}
		if !strings.HasPrefix(c.fingerprint, "sha256:") {
			t.Errorf("fingerprint = %q, want a sha256 prefix", c.fingerprint)
		}
		if strings.Contains(c.fingerprint, theSecret) || strings.Contains(c.fingerprint, secondSecret) {
			t.Errorf("the report carries the secret: %+v", c)
		}
	}

	// A provider name is free-form and a variable name is not.
	if got := envIdent("azure eu/west"); got != "AZURE_EU_WEST" {
		t.Errorf("envIdent = %q, want AZURE_EU_WEST", got)
	}
	if got := envIdent(""); got != "IMPORTED" {
		t.Errorf("envIdent(\"\") = %q, want IMPORTED", got)
	}
}

// TestImportConfigSeparatesTwoSecrets checks that two credentials do not both
// land in one variable, which would be a silent misconfiguration wearing the
// costume of a redaction.
func TestImportConfigSeparatesTwoSecrets(t *testing.T) {
	path := writeSource(t, `
model_list:
  - model_name: a
    litellm_params:
      model: a
      custom_llm_provider: openai
      api_key: `+theSecret+`
  - model_name: b
    litellm_params:
      model: b
      custom_llm_provider: openai
      api_key: `+secondSecret+`
`)
	stdout, stderr, code := invoke("import", "config", path)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	assertNoSecret(t, "two credentials", stdout, stderr, theSecret, secondSecret)

	// Two distinct references, not one variable holding two providers' keys —
	// which would be a silent misconfiguration wearing the costume of a
	// redaction. The names themselves are not pinned; that they differ is.
	seen := map[string]bool{}
	for _, line := range strings.Split(stdout, "\n") {
		if _, ok := strings.CutPrefix(strings.TrimSpace(line), "key_env:"); ok {
			seen[strings.TrimSpace(line)] = true
		}
	}
	if len(seen) != 2 {
		t.Errorf("two credentials produced %d distinct key_env references, want 2:\n%s",
			len(seen), stdout)
	}
}

// TestImportConfigErrorPathsNeverPrintTheSecret is the half a happy-path test
// misses. An error message that helpfully includes the value it could not parse
// is the usual way this defect comes back, and every one of these files carries
// the secret on the line the importer chokes on.
func TestImportConfigErrorPathsNeverPrintTheSecret(t *testing.T) {
	cases := map[string]string{
		"top level is a sequence": "- api_key: " + theSecret + "\n",
		"unterminated flow mapping": "model_list:\n  - litellm_params: {model: x, api_key: " +
			theSecret + "\n",
		"tab where yaml forbids one": "model_list:\n\t- api_key: " + theSecret + "\n",
		"a scalar document":          theSecret + "\n",
		"a duplicate key": "general_settings:\n  master_key: " + theSecret +
			"\n  master_key: " + theSecret + "\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := invoke("import", "config", writeSource(t, src))
			assertNoSecret(t, "error path", stdout, stderr, theSecret)
			_ = code // some of these are refusals and some are imports; neither may leak
		})
	}

	// The path that does not exist, and the path that is a directory: both go
	// through e.fail with the operator's own string.
	stdout, stderr, _ := invoke("import", "config", filepath.Join(t.TempDir(), "absent.yaml"))
	assertNoSecret(t, "missing file", stdout, stderr, theSecret)
}

// TestImportConfigWarnsWithoutEchoingTheSecret pins the specific regression the
// review names: the fix for "a secret ended up somewhere it should not" is not
// to print it somewhere else. Every warning goes through one scrubber.
func TestImportConfigWarnsWithoutEchoingTheSecret(t *testing.T) {
	// A master_key literal and a database_url literal are both warned about and
	// neither is imported; a webhook URL with a token in it rides along in a
	// section that has no dorang equivalent.
	path := writeSource(t, `
general_settings:
  master_key: `+theSecret+`
  database_url: postgres://u:`+secondSecret+`@db/x
model_list:
  - model_name: m
    litellm_params:
      model: m
      custom_llm_provider: openai
      api_key: `+theSecret+`
`)
	stdout, stderr, _ := invoke("import", "config", path)
	assertNoSecret(t, "warnings", stdout, stderr, theSecret, secondSecret)
	if !strings.Contains(stderr, "warning:") {
		t.Errorf("no warnings were emitted at all, so nothing was proved:\n%s", stderr)
	}
}

// TestOtherSubcommandsDoNotPrintAConfiguredSecret covers the rest of the CLI
// against the same property. These commands load a configuration that holds an
// inline development secret and report on it; none of them may render it.
func TestOtherSubcommandsDoNotPrintAConfiguredSecret(t *testing.T) {
	dir := t.TempDir()
	path := writeCfg(t, dir, strings.Replace(pricedModel, "key: dev-secret", "key: "+theSecret, 1))

	for _, args := range [][]string{
		{"config", "lint", path},
		{"config", "check", path},
		{"price", "model-x", "--config", path, "--input", "1000", "--output", "1000"},
		{"price", "no-such-model", "--config", path},
	} {
		name := strings.Join(args[:2], " ")
		stdout, stderr, _ := invoke(args...)
		assertNoSecret(t, name, stdout, stderr, theSecret)
	}
}

// TestFingerprintIdentifiesWithoutDisclosing pins the two properties the report
// line depends on: the same secret always fingerprints the same way, and two
// different ones do not collide into the same line.
func TestFingerprintIdentifiesWithoutDisclosing(t *testing.T) {
	if fingerprint(theSecret) != fingerprint(theSecret) {
		t.Error("the fingerprint is not stable")
	}
	if fingerprint(theSecret) == fingerprint(secondSecret) {
		t.Error("two secrets fingerprint identically")
	}
	if strings.Contains(fingerprint(theSecret), theSecret[:8]) {
		t.Error("the fingerprint carries the secret")
	}
}

// TestScrubBytesIsTheBackstop exercises the guard directly, because on a correct
// run it never fires and an untested guard is the defect this whole exercise is
// about.
func TestScrubBytesIsTheBackstop(t *testing.T) {
	out, acted := scrubBytes([]byte("key: "+theSecret+"\n"), []string{theSecret})
	if !acted {
		t.Fatal("a secret in the encoder's output was not detected")
	}
	if strings.Contains(string(out), theSecret) {
		t.Errorf("the secret survived scrubbing: %s", out)
	}
	if out, acted := scrubBytes([]byte("key_env: X\n"), []string{theSecret}); acted {
		t.Errorf("clean output was reported as leaking: %s", out)
	}
	// Something too short to be a credential is left alone rather than being
	// replaced everywhere it incidentally occurs.
	if _, acted := scrubBytes([]byte("a: b\n"), []string{"b"}); acted {
		t.Error("a one-character value was treated as a secret")
	}
}
