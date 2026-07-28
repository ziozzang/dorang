package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The literal used in these tests. It is fabricated and matches nothing.
const fakeLiteral = "sk-FAKE-NOT-A-REAL-KEY-000000000000000000" // pragma: allowlist secret — fabricated

// Marshalling a configuration that holds an unresolved inline secret never
// emits the plaintext.
//
// SecretRef.MarshalYAML exists and deliberately emits only the reference — and
// it never ran. Both uses of SecretRef embed it with `yaml:",inline"`, and
// gopkg.in/yaml.v3 flattens an inline-embedded struct's exported fields rather
// than calling its MarshalYAML. So `dorangctl import config old.yaml >
// dorang.yaml` wrote every literal api_key from the source file to stdout as a
// plaintext `key:` value: into the new config file, into terminal scrollback,
// and into CI logs if the migration was scripted. DESIGN §4.1 says secrets
// never appear in configuration, and this is the one tool whose job is to
// produce one.
//
// The server-startup path was safe only by accident of ordering — resolve()
// moves the literal out of the exported field before anything could marshal it
// — and the import path is precisely the one that never calls resolve.
func TestMarshalingNeverEmitsAPlaintextSecret(t *testing.T) {
	cfg := &Config{
		Credentials: []Credential{{
			ID: "openai-key-1", Provider: "openai",
			Key: SecretRef{Inline: fakeLiteral},
		}},
		KeyRotation: KeyRotation{Providers: map[string]KeyRotationProvider{
			"openai": {Keys: []RotationKey{{
				ID: "acct-1", Key: SecretRef{Inline: fakeLiteral},
			}}},
		}},
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), fakeLiteral) {
		t.Fatalf("the plaintext secret was written to the generated configuration:\n%s", out)
	}
	// And not by dropping the field into an obviously broken shape either — a
	// `key:` line at all is the thing that must not be there.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "key:") {
			t.Errorf("a bare `key:` line survives marshalling: %q", line)
		}
	}
}

// A reference is still written, so a generated configuration is usable.
func TestMarshalingKeepsTheReference(t *testing.T) {
	cases := []struct {
		name string
		ref  SecretRef
		want string
	}{
		{"env", SecretRef{Env: "OPENAI_API_KEY"}, "key_env: OPENAI_API_KEY"},
		{"file", SecretRef{File: "/run/secrets/openai"}, "key_file: /run/secrets/openai"},
		{"ref", SecretRef{Ref: "vault:secret/data/x#key"}, "key_ref: vault:secret/data/x#key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := yaml.Marshal(&Config{Credentials: []Credential{{
				ID: "c1", Provider: "openai", Key: c.ref,
			}}})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !strings.Contains(string(out), c.want) {
				t.Errorf("the reference was lost:\n%s", out)
			}
		})
	}
}

// A RESOLVED secret does not leak either: resolve moves the literal into an
// unexported field, and marshalling must not find it there.
func TestMarshalingAResolvedSecretEmitsNothing(t *testing.T) {
	t.Setenv("DORANG_TEST_KEY", fakeLiteral)
	ref := SecretRef{Env: "DORANG_TEST_KEY"}
	c := &collector{}
	ref.resolve(EnvProduction, "credentials[0]", c)
	if v, ok := ref.Value(); !ok || v != fakeLiteral {
		t.Fatalf("the secret did not resolve: %q %v", v, ok)
	}

	out, err := yaml.Marshal(&Config{Credentials: []Credential{{
		ID: "c1", Provider: "openai", Key: ref,
	}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), fakeLiteral) {
		t.Fatalf("a resolved secret was marshalled:\n%s", out)
	}
}
