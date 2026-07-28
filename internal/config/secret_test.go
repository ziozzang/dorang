package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const testSecret = "sk-SUPERSECRET-do-not-print-me" // pragma: allowlist secret — fixture, asserts redaction

// TestSecretSources resolves a key from each of the three references, and from
// an inline literal under development.
func TestSecretSources(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "provider.key")
	if err := os.WriteFile(keyFile, []byte(testSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	crlfFile := filepath.Join(dir, "provider.crlf.key")
	if err := os.WriteFile(crlfFile, []byte(testSecret+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DORANG_TEST_KEY", testSecret)

	cases := []struct {
		name         string
		cred         string
		env          string
		wantValue    string
		wantResolved bool
		wantSource   string
	}{
		{
			name:         "key_env",
			cred:         "{id: c1, provider: p1, key_env: DORANG_TEST_KEY}",
			wantValue:    testSecret,
			wantResolved: true,
			wantSource:   "key_env:DORANG_TEST_KEY",
		},
		{
			name:         "key_file trims the trailing newline",
			cred:         fmt.Sprintf("{id: c1, provider: p1, key_file: %q}", keyFile),
			wantValue:    testSecret,
			wantResolved: true,
			wantSource:   "key_file:" + keyFile,
		},
		{
			name:         "key_file trims a CRLF ending",
			cred:         fmt.Sprintf("{id: c1, provider: p1, key_file: %q}", crlfFile),
			wantValue:    testSecret,
			wantResolved: true,
			wantSource:   "key_file:" + crlfFile,
		},
		{
			name:         "inline literal under development",
			cred:         fmt.Sprintf("{id: c1, provider: p1, key: %q}", testSecret),
			env:          "development",
			wantValue:    testSecret,
			wantResolved: true,
			wantSource:   "key:(inline literal)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			if env == "" {
				env = "production"
			}
			c, err := LoadBytes([]byte(fmt.Sprintf(`
version: 1
server: {env: %s}
providers: [{name: p1, kind: openai}]
credentials:
  - %s
models: [{name: m1, deployments: [{provider: p1, upstream_model: u, credentials: [c1]}]}]
`, env, tc.cred)))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			cred, ok := c.Credential("c1")
			if !ok {
				t.Fatal("credential c1 is missing")
			}
			got, resolved := cred.Key.Value()
			if resolved != tc.wantResolved {
				t.Errorf("Resolved() = %v, want %v", resolved, tc.wantResolved)
			}
			if got != tc.wantValue {
				t.Errorf("resolved value is not the secret from the source")
			}
			if src := cred.Key.Source(); src != tc.wantSource {
				t.Errorf("Source() = %q, want %q", src, tc.wantSource)
			}
			if tc.wantValue != "" && strings.Contains(cred.Key.Source(), tc.wantValue) {
				t.Error("Source() leaks the secret")
			}
			// The inline literal must not survive resolution in the struct.
			if cred.Key.Inline != "" {
				t.Error("the inline literal is still in the exported field after resolution")
			}
		})
	}
}

// TestInlineSecretRefusedInProduction is design §4.1: an inline literal is
// accepted only in development.
func TestInlineSecretRefusedInProduction(t *testing.T) {
	src := fmt.Sprintf(`
version: 1
providers: [{name: p1, kind: openai}]
credentials:
  - {id: c1, provider: p1, key: %q}
models: [{name: m1, deployments: [{provider: p1, upstream_model: u, credentials: [c1]}]}]
`, testSecret)

	_, err := LoadBytes([]byte(src))
	if err == nil {
		t.Fatal("an inline literal must be refused when server.env is production")
	}
	if !hasProblem(err, "credentials[0].key", "accepted only when server.env") {
		t.Fatalf("unexpected problem set: %v", err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatal("the error message contains the secret")
	}
}

// TestSecretsNeverAppearInErrors is the property that matters most: whatever
// else goes wrong, no error string carries a key.
func TestSecretsNeverAppearInErrors(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "k")
	if err := os.WriteFile(keyFile, []byte(testSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DORANG_TEST_KEY", testSecret)

	// Every credential resolves, and everything else is wrong.
	src := fmt.Sprintf(`
version: 1
server: {env: development}
cluster: {enabled: true, capacity_mode: local}
providers: [{name: p1, kind: openai}]
credentials:
  - {id: c1, provider: p1, key_env: DORANG_TEST_KEY}
  - {id: c2, provider: p1, key_file: %q}
  - {id: c3, provider: p1, key: %q}
  - {id: c4, provider: nope, key_env: DORANG_TEST_KEY}
models:
  - name: m1
    deployments: [{provider: p1, upstream_model: u, credentials: [c1, c2, c3, missing]}]
aliases: {a: nowhere}
`, keyFile, testSecret)

	_, err := LoadBytes([]byte(src))
	if err == nil {
		t.Fatal("want problems")
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatalf("an error message carries a secret:\n%v", err)
	}
	for _, p := range Problems(err) {
		if strings.Contains(p.Message, testSecret) {
			t.Errorf("problem at %s carries a secret", p.Path)
		}
	}
}

// TestSecretResolutionFailures covers the ways a reference can fail to resolve.
func TestSecretResolutionFailures(t *testing.T) {
	cases := []struct {
		name string
		cred string
		path string
		want string
	}{
		{
			name: "environment variable is not set",
			cred: "{id: c1, provider: p1, key_env: DORANG_TEST_ABSENT}",
			path: "credentials[0].key_env",
			want: "environment variable DORANG_TEST_ABSENT is not set",
		},
		{
			name: "secret file is missing",
			cred: "{id: c1, provider: p1, key_file: /nonexistent/dorang.key}",
			path: "credentials[0].key_file",
			want: "no such file",
		},
		{
			name: "no source at all",
			cred: "{id: c1, provider: p1}",
			path: "credentials[0]",
			want: "no secret source",
		},
		{
			name: "two sources",
			cred: "{id: c1, provider: p1, key_env: A, key_ref: \"vault:x\"}",
			path: "credentials[0]",
			want: "set exactly one of",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(fmt.Sprintf(`
version: 1
providers: [{name: p1, kind: openai}]
credentials:
  - %s
models: [{name: m1, deployments: [{provider: p1, upstream_model: u}]}]
`, tc.cred)))
			if !hasProblem(err, tc.path, tc.want) {
				t.Errorf("want %s: %q, got %v", tc.path, tc.want, err)
			}
		})
	}
}

// TestRotationKeysResolve checks the second place secrets live.
func TestRotationKeysResolve(t *testing.T) {
	t.Setenv("DORANG_TEST_KEY_1", testSecret+"-1")
	t.Setenv("DORANG_TEST_KEY_2", testSecret+"-2")
	c, err := LoadBytes([]byte(`
version: 1
providers: [{name: p1, kind: openai}]
credentials:
  - {id: k1, provider: p1, key_env: DORANG_TEST_KEY_1}
  - {id: k2, provider: p1, key_env: DORANG_TEST_KEY_2}
models: [{name: m1, deployments: [{provider: p1, upstream_model: u, credentials: [k1, k2]}]}]
key_rotation:
  strategy: least_used
  providers:
    p1:
      affinity_group: p1-accounts
      stickiness: {scope: session, on_capacity: spill}
      keys:
        - {id: k1, key_env: DORANG_TEST_KEY_1, max_concurrency: 3}
        - {id: k2, key_env: DORANG_TEST_KEY_2, max_concurrency: 3}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	keys := c.KeyRotation.Providers["p1"].Keys
	for i, want := range []string{testSecret + "-1", testSecret + "-2"} {
		got, ok := keys[i].Key.Value()
		if !ok || got != want {
			t.Errorf("rotation key %d did not resolve", i)
		}
	}
}

// TestSecretRefRedaction checks every rendering path.
func TestSecretRefRedaction(t *testing.T) {
	t.Setenv("DORANG_TEST_KEY", testSecret)
	c, err := LoadBytes([]byte(`
version: 1
providers: [{name: p1, kind: openai}]
credentials: [{id: c1, provider: p1, key_env: DORANG_TEST_KEY}]
models: [{name: m1, deployments: [{provider: p1, upstream_model: u, credentials: [c1]}]}]
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cred, _ := c.Credential("c1")
	if v, ok := cred.Key.Value(); !ok || v != testSecret {
		t.Fatal("the key did not resolve")
	}
	for name, rendered := range map[string]string{
		"%v":  fmt.Sprintf("%v", cred.Key),
		"%s":  fmt.Sprintf("%s", cred.Key),
		"%#v": fmt.Sprintf("%#v", cred.Key),
		"%+v": fmt.Sprintf("%+v", *cred),
	} {
		if strings.Contains(rendered, testSecret) {
			t.Errorf("%s leaks the secret: %s", name, rendered)
		}
	}
	out, err := yaml.Marshal(cred)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), testSecret) {
		t.Errorf("YAML marshaling leaks the secret:\n%s", out)
	}
	if !strings.Contains(string(out), "DORANG_TEST_KEY") {
		t.Errorf("YAML marshaling lost the reference:\n%s", out)
	}
	j, err := json.Marshal(cred.Key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(j), testSecret) {
		t.Errorf("JSON marshaling leaks the secret: %s", j)
	}
}

// TestDevelopmentInlineIsNotWrittenBack checks that a literal accepted in
// development cannot be re-serialized into a file.
func TestDevelopmentInlineIsNotWrittenBack(t *testing.T) {
	c, err := LoadBytes([]byte(fmt.Sprintf(`
version: 1
server: {env: development}
providers: [{name: p1, kind: openai}]
credentials: [{id: c1, provider: p1, key: %q}]
models: [{name: m1, deployments: [{provider: p1, upstream_model: u, credentials: [c1]}]}]
`, testSecret)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), testSecret) {
		t.Errorf("the literal was written back:\n%s", out)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := ExpandPath("~/.dorang/dorang.db"); got != filepath.Join(home, ".dorang/dorang.db") {
		t.Errorf("ExpandPath(~/.dorang/dorang.db) = %q", got)
	}
	if got := ExpandPath("/etc/dorang.yaml"); got != "/etc/dorang.yaml" {
		t.Errorf("an absolute path must not change: %q", got)
	}
	if got := ExpandPath("~user/x"); got != "~user/x" {
		t.Errorf("only a leading ~/ is expanded: %q", got)
	}
}

// TestSecretRefAccessors covers the small surface other packages read.
func TestSecretRefAccessors(t *testing.T) {
	var zero SecretRef
	if !zero.IsZero() {
		t.Error("a zero SecretRef must report IsZero")
	}
	if zero.Source() != "(unset)" {
		t.Errorf("Source() = %q", zero.Source())
	}
	if _, ok := zero.Value(); ok {
		t.Error("a zero SecretRef must not report a value")
	}

	ref := SecretRef{Ref: "vault:secret/data/x#key"}
	if ref.IsZero() {
		t.Error("a key_ref is a source")
	}
	if ref.Resolved() {
		t.Error("a key_ref is not resolved by this package")
	}
	if ref.Reference() != "vault:secret/data/x#key" {
		t.Errorf("Reference() = %q", ref.Reference())
	}

	// Marshaling a SecretRef on its own writes back only the reference.
	for _, tc := range []struct {
		in   SecretRef
		want string
	}{
		{SecretRef{Env: "NAME"}, "key_env: NAME"},
		{SecretRef{File: "/p"}, "key_file: /p"},
		{SecretRef{Ref: "vault:x"}, "key_ref: vault:x"},
	} {
		out, err := yaml.Marshal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(out)) != tc.want {
			t.Errorf("marshal(%v) = %q, want %q", tc.in.Source(), out, tc.want)
		}
	}
	// A resolved inline literal marshals to nothing at all.
	resolved := SecretRef{value: testSecret, resolved: true}
	out, err := yaml.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), testSecret) {
		t.Errorf("marshaling a resolved reference leaked the secret: %s", out)
	}
}
