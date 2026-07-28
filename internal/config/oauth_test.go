package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Before this, `grep -ri oauth internal/config` returned nothing. internal/auth
// implemented a credential that refreshes itself, internal/backend declared the
// seam it satisfies, and there was no way to DECLARE one — so the whole
// subsystem was unreachable from a running binary, which is DESIGN §17.1's
// defect class.

const oauthYAML = `
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - id: plan-oauth-1
    provider: p1
    auth: oauth
    oauth:
      source: file
      path: /var/lib/dorang/fixture-auth.json
      format: codex
      account_header: chatgpt-account-id
      refresh_margin: 7m
      poll_interval: 20s
      refresh:
        token_url: https://auth.example.invalid/oauth/token
        client_id: fixture-client-id
        encoding: form
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: u1, credentials: [plan-oauth-1]}
`

// TestAnOAuthCredentialIsDeclarable is the floor: the schema exists and a file
// that uses it loads.
func TestAnOAuthCredentialIsDeclarable(t *testing.T) {
	c, err := LoadBytes([]byte(oauthYAML))
	if err != nil {
		t.Fatalf("a configuration declaring an oauth credential does not load: %v", err)
	}
	cr, ok := c.Credential("plan-oauth-1")
	if !ok {
		t.Fatal("the credential is not in the index")
	}
	if !cr.IsOAuth() {
		t.Fatal("the credential does not report itself as oauth")
	}
	if cr.OAuth == nil {
		t.Fatal("the oauth block is nil")
	}
	if cr.OAuth.Format != "codex" || cr.OAuth.Source != "file" {
		t.Errorf("format/source = %q/%q", cr.OAuth.Format, cr.OAuth.Source)
	}
	if cr.OAuth.AccountHeader != "chatgpt-account-id" {
		t.Errorf("account_header = %q", cr.OAuth.AccountHeader)
	}
	if cr.OAuth.RefreshMargin.Duration() != 7*time.Minute {
		t.Errorf("refresh_margin = %v", cr.OAuth.RefreshMargin)
	}
	if cr.OAuth.PollInterval.Duration() != 20*time.Second {
		t.Errorf("poll_interval = %v", cr.OAuth.PollInterval)
	}
	if cr.OAuth.Refresh.TokenURL == "" || cr.OAuth.Refresh.ClientID == "" {
		t.Errorf("the refresh block did not decode: %+v", cr.OAuth.Refresh)
	}
}

// TestAnOAuthCredentialAndAStaticKeyAreAlternatives. Two answers to the question
// the upstream call asks once, resolved by precedence, is how a deployment sends
// the wrong credential with nothing in the file to explain it.
func TestAnOAuthCredentialAndAStaticKeyAreAlternatives(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{
			name: "both spellings",
			yaml: `
  - id: c1
    provider: p1
    auth: oauth
    key_env: DORANG_TEST_OAUTH_UNUSED
    oauth: {source: file, path: /tmp/fixture-auth.json}`,
			want: "alternatives",
		},
		{
			name: "oauth block with no auth: oauth",
			yaml: `
  - id: c1
    provider: p1
    key_env: DORANG_TEST_OAUTH_UNUSED
    oauth: {source: file, path: /tmp/fixture-auth.json}`,
			want: "read by nothing",
		},
		{
			name: "auth: oauth with no block",
			yaml: `
  - {id: c1, provider: p1, auth: oauth}`,
			want: "needs an oauth block",
		},
		{
			name: "unknown auth kind",
			yaml: `
  - {id: c1, provider: p1, auth: mtls, key_env: DORANG_TEST_OAUTH_UNUSED}`,
			want: "unknown value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DORANG_TEST_OAUTH_UNUSED", "fixture-static-key")
			_, err := LoadBytes([]byte(credentialsOnly(tc.yaml)))
			if err == nil {
				t.Fatalf("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q: %v", tc.want, err)
			}
		})
	}
}

// TestAnOAuthBlockIsCheckedAgainstItsSource. Every one of these loads and does
// nothing if it is not checked — a command with `source: file`, an env_var with
// no name, a format nothing understands.
func TestAnOAuthBlockIsCheckedAgainstItsSource(t *testing.T) {
	for _, tc := range []struct {
		name, block, want string
	}{
		{"file needs a path", `{source: file}`, "needs the path"},
		{"exec needs a command", `{source: exec}`, "needs a command"},
		{"env needs a name", `{source: env}`, "needs the name"},
		{"unknown source", `{source: keychain}`, "unknown source"},
		{"unknown format", `{source: file, path: /tmp/a.json, format: openai}`, "unknown token store format"},
		{
			"command with source file",
			`{source: file, path: /tmp/a.json, command: [cat, /tmp/a.json]}`,
			"read by nothing",
		},
		{
			"unknown encoding",
			`{source: file, path: /tmp/a.json, refresh: {token_url: "https://a.invalid/t", client_id: c, encoding: xml}}`,
			"unknown encoding",
		},
		{
			"plaintext token endpoint",
			`{source: file, path: /tmp/a.json, refresh: {token_url: "http://auth.invalid/t", client_id: c}}`,
			"must be https",
		},
		{
			"refresh with no client_id",
			`{source: file, path: /tmp/a.json, refresh: {token_url: "https://a.invalid/t"}}`,
			"client_id",
		},
		{
			// The exchange would spend a refresh token and have nowhere to
			// record the successor: §11.2b's token_store_write_failed, arranged
			// in advance by the configuration.
			"refresh against a read-only source",
			`{source: exec, command: [gettoken], refresh: {token_url: "https://a.invalid/t", client_id: c}}`,
			"needs a writable store",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(credentialsOnly(
				"\n  - {id: c1, provider: p1, auth: oauth, oauth: " + tc.block + "}")))
			if err == nil {
				t.Fatalf("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q: %v", tc.want, err)
			}
		})
	}
}

// TestAnOAuthClientSecretIsASecretLikeAnyOther: a reference in the file, a value
// only in memory, resolved by the same code as every other one (§4.1).
func TestAnOAuthClientSecretIsASecretLikeAnyOther(t *testing.T) {
	t.Setenv("DORANG_TEST_OAUTH_CLIENT_SECRET", "fixture-client-secret-value")
	src := credentialsOnly(`
  - id: c1
    provider: p1
    auth: oauth
    oauth:
      source: file
      path: /tmp/fixture-auth.json
      refresh:
        token_url: https://auth.example.invalid/token
        client_id: fixture-client
        key_env: DORANG_TEST_OAUTH_CLIENT_SECRET`)
	c, err := LoadBytes([]byte(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cr, _ := c.Credential("c1")
	v, ok := cr.OAuth.Refresh.ClientSecret.Value()
	if !ok || v != "fixture-client-secret-value" {
		t.Fatalf("the client secret did not resolve: ok=%t", ok)
	}
	// And it is not printable, in either direction.
	for _, s := range []string{
		cr.OAuth.Refresh.ClientSecret.String(),
		cr.OAuth.Refresh.ClientSecret.GoString(),
	} {
		if strings.Contains(s, "fixture-client-secret-value") {
			t.Errorf("the client secret is printable: %s", s)
		}
	}
}

// TestAnInlineClientSecretIsNotWrittenBack. `yaml:",inline"` flattens an
// embedded SecretRef field by field and never calls its own MarshalYAML — the
// exact defect that once wrote every literal api_key to stdout. The refresh
// block has the same shape, so it needs the same structural answer: a marshalled
// form with no field a literal can land in.
func TestAnInlineClientSecretIsNotWrittenBack(t *testing.T) {
	src := `
version: 1
server: {env: development}
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - id: c1
    provider: p1
    auth: oauth
    oauth:
      source: file
      path: /tmp/fixture-auth.json
      refresh:
        token_url: https://auth.example.invalid/token
        client_id: fixture-client
        key: fixture-inline-client-secret
`
	c, err := LoadBytes([]byte(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "fixture-inline-client-secret") {
		t.Fatalf("a literal client secret was written back:\n%s", out)
	}
	// The reference spelling still survives, so the generated file is usable.
	if !strings.Contains(string(out), "token_url") {
		t.Errorf("the refresh block did not survive marshalling:\n%s", out)
	}
}

// credentialsOnly wraps a credentials[] fragment in the smallest valid file.
func credentialsOnly(creds string) string {
	return `
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:` + creds + "\n"
}
