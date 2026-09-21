package config

import (
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

func TestSetupEditPreservesUnownedProviderAndOAuthFields(t *testing.T) {
	raw := []byte("# operator notes\nproviders:\n  - name: p\n    kind: openai\n    params:\n      drop_unsupported: true # keep me\n      api_version: old\ncredentials:\n  - id: account\n    provider: p\n    key_env: OLD_KEY\nmodels: []\n")
	out, err := EditSetup(raw, SetupEdit{Section: "providers", IDKey: "name", ID: "p", Fields: map[string]any{"base_url": "https://example.test/v1"}, Parameters: map[string]any{"api_version": "new"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# operator notes", "drop_unsupported: true # keep me", "api_version: new", "key_env: OLD_KEY"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("lost unrelated field/comment %q", want)
		}
	}
	out, err = EditSetup(out, SetupEdit{Section: "credentials", IDKey: "id", ID: "account", Fields: map[string]any{"key_file": "/secrets/new"}, Remove: []string{"key_env"}})
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "key_env") {
		t.Fatal("credential rotation kept conflicting old source")
	}
}
