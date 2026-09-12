package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
)

// The Responses-only contract settings are refused on a kind whose adapter
// never reads them, and accepted on the kind the catalog declares responses_only.
//
// Both halves are one test because they are one wiring: app copies the catalog
// kind's `responses_only` into the provider Spec, and NewProvider decides from
// that. Without the copy the flag is false everywhere, the settings are refused
// on the codex kind too, and the refusal test alone would still pass.
func TestTheResponsesContractSettingsFollowTheCatalogFlag(t *testing.T) {
	assemble := func(t *testing.T, kind string) error {
		t.Helper()
		isolateState(t)
		t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
		cfg, err := config.LoadBytes([]byte(`
version: 1
providers:
  - name: p1
    kind: ` + kind + `
    base_url: "https://example.invalid"
    params: {force_stream: true, store_false: true}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - provider: p1
        upstream_model: gpt-5.5
        credentials: [c1]
`))
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		cfg.Storage.SQLite.Path = filepath.Join(t.TempDir(), "dorang.db")
		t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
		t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)
		a, err := New(context.Background(), Options{Config: cfg})
		if a != nil {
			t.Cleanup(func() { _ = a.Close(context.Background()) })
		}
		return err
	}

	t.Run("refused on a chat host", func(t *testing.T) {
		err := assemble(t, "openai")
		if err == nil {
			t.Fatal("force_stream on an openai provider assembled cleanly; the setting loads " +
				"and changes nothing")
		}
		if !strings.Contains(err.Error(), "force_stream") || !strings.Contains(err.Error(), `"p1"`) {
			t.Errorf("the refusal names neither the key nor the provider: %v", err)
		}
	})

	t.Run("accepted on the kind the catalog declares responses_only", func(t *testing.T) {
		// `codex` is the alias the catalog resolves to codex-responses, so this
		// also covers the alias path an operator's file actually takes.
		if err := assemble(t, "codex"); err != nil {
			t.Fatalf("the settings were refused on the one kind they are for, so the catalog's "+
				"responses_only never reached the provider: %v", err)
		}
	})
}
