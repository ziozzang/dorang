package scenario

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/store"
)

// The import carries the incumbent's authorization columns exactly. What it
// could not do was translate MEANING, and one of the three idioms it left
// untranslated failed OPEN: a key whose model restriction lived in
// LiteLLM_ObjectPermissionTable arrived with an empty `models` list, and an
// empty allow-list allows every model.
//
// The store's own tests assert the report. This one asserts the consequence,
// which is the only assertion that can settle it: the imported key, converted
// by the same adapter the gateway uses, refused a model the incumbent had
// fenced it off from. A test that only reads the `models` column back would
// pass against an implementation that stored the restriction and never
// enforced it — which is precisely what the defect was.

func newPermSource(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "incumbent.db"))
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE "LiteLLM_VerificationToken" (
			token TEXT PRIMARY KEY, key_name TEXT, models TEXT, allowed_routes TEXT,
			object_permission_id TEXT, spend REAL, created_at TEXT, updated_at TEXT)`,
		`CREATE TABLE "LiteLLM_ObjectPermissionTable" (
			object_permission_id TEXT PRIMARY KEY, models TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("source DDL: %v", err)
		}
	}
	return db
}

// TestAnImportedKeyCannotReachAModelItsPermissionRowFenced.
//
// The fixture is the live database's own shape: two rows in the permission
// table, one restricting to a single model and one restricting nothing. The key
// points at the restricting one — which on the live database was the row no key
// happened to point at, and the only reason the fail-open did not fire.
func TestAnImportedKeyCannotReachAModelItsPermissionRowFenced(t *testing.T) {
	const token = "sk-fenced-by-object-permission" // pragma: allowlist secret — test fixture
	const (
		fenced = "qwen3.5:397b"
		other  = "gpt-4o"
	)

	src := newPermSource(t)
	if _, err := src.Exec(`INSERT INTO "LiteLLM_ObjectPermissionTable"
		(object_permission_id, models) VALUES (?, ?), (?, ?)`,
		"objperm-restricting", `{`+fenced+`}`,
		"objperm-empty", ``); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`INSERT INTO "LiteLLM_VerificationToken"
		(token, key_name, models, allowed_routes, object_permission_id, spend, created_at, updated_at)
		VALUES (?, ?, '', '', ?, 0, ?, ?)`,
		store.HashLegacySHA256(token), "sk-...FN", "objperm-restricting",
		"2025-01-01 00:00:00", "2025-06-01 00:00:00"); err != nil {
		t.Fatal(err)
	}

	now := func() time.Time { return time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC) }
	s := openStore(t, now, store.LegacyAuth{})
	ctx := context.Background()

	rep, err := s.ImportKeys(ctx, src, store.ImportOptions{})
	if err != nil {
		t.Fatalf("ImportKeys: %v", err)
	}
	if rep.Imported != 1 {
		t.Fatalf("imported %d, want 1\n%s", rep.Imported, rep.Summary())
	}

	k, err := s.GetAPIKeyByLookup(ctx, store.KeyLookup(token))
	if err != nil {
		t.Fatalf("GetAPIKeyByLookup: %v", err)
	}

	// The same adapter the gateway builds its principals with. Reading
	// k.Models directly would assert the column and not the fence.
	p, err := cluster.AuthPrincipal(k, nil)
	if err != nil {
		t.Fatalf("AuthPrincipal: %v", err)
	}

	if err := p.Authorize(auth.Access{Now: now(), Model: fenced}); err != nil {
		t.Fatalf("the key was refused %s, which its permission row allows: %v", fenced, err)
	}
	err = p.Authorize(auth.Access{Now: now(), Model: other})
	if err == nil {
		t.Fatalf("the imported key reached %s. Its restriction lived in "+
			"LiteLLM_ObjectPermissionTable, and a key that arrives with an EMPTY model "+
			"allow-list may reach every model — the one failure mode that grants access "+
			"nobody granted, with no line in the import report", other)
	}
	if !errors.Is(err, auth.ErrModelNotAllowed) {
		t.Fatalf("refused %s with %v, want a model-not-allowed refusal", other, err)
	}
}
