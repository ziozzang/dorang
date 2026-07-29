package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/store"
)

// The incumbent's credential table, cut down to the columns this test needs.
// The importer enumerates the source's columns and selects only the ones it
// carries, so an abbreviated source is a legitimate one.
const importSourceDDL = `
CREATE TABLE "LiteLLM_VerificationToken" (
    token           TEXT PRIMARY KEY,
    key_name        TEXT,
    key_alias       TEXT,
    expires         TEXT,
    blocked         INTEGER,
    team_id         TEXT,
    created_at      TEXT,
    updated_at      TEXT
)`

// writeSourceDB builds a foreign database at path and returns it.
func writeSourceDB(t *testing.T, path string, tokens map[string]string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(importSourceDDL); err != nil {
		t.Fatal(err)
	}
	for token, alias := range tokens {
		sum := sha256.Sum256([]byte(token))
		if _, err := db.Exec(`INSERT INTO "LiteLLM_VerificationToken"
			(token, key_name, key_alias, expires, blocked, team_id, created_at, updated_at)
			VALUES (?, ?, ?, NULL, 0, '', '2025-01-01 00:00:00', '2025-06-01 00:00:00')`,
			hex.EncodeToString(sum[:]), "sk-..."+token[len(token)-4:], alias); err != nil {
			t.Fatal(err)
		}
	}
}

// TestImportKeysHasACLIEntryPoint is the closing of MIGRATION.md §3.5.
//
// `store.ImportKeys` was implemented, tested end to end, and reachable from
// nothing an operator can type: no subcommand, no administrative endpoint. The
// migration document existed partly to warn a reader planning a cutover that
// the credential path they were counting on could not be invoked, and to plan
// on reissuing instead.
//
// The assertion is on the OBSERVABLE an operator gets — the exit code, the
// report, and whether a token from the incumbent's database authenticates
// against this deployment afterwards — and not on the store method, which
// passed throughout the defect.
func TestImportKeysHasACLIEntryPoint(t *testing.T) {
	dir := t.TempDir()
	cfg := writeCfg(t, dir, "")
	srcPath := filepath.Join(dir, "incumbent.db")

	const migrated = "sk-incumbent-token-AB12" // pragma: allowlist secret — test fixture
	const untouched = "sk-never-imported-CD34" // pragma: allowlist secret — test fixture
	writeSourceDB(t, srcPath, map[string]string{migrated: "reporting-bot"})

	// Reading and reporting is the default, and it writes nothing.
	out, errb, code := invoke("import", "keys", "--config", cfg, "--from", srcPath)
	if code != 0 {
		t.Fatalf("dry run exited %d: %s%s", code, out, errb)
	}
	for _, want := range []string{"scanned 1", "imported 1", "dry run", "--commit"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry-run report does not mention %q:\n%s", want, out)
		}
	}
	if authenticates(t, cfg, migrated) {
		t.Fatal("the default run wrote a credential; --commit exists so that it cannot")
	}

	// And committing actually grants the incumbent's credential access here,
	// without anyone ever holding its plaintext: the source stores a digest and
	// dorang's index key is derived from that same digest.
	out, errb, code = invoke("import", "keys", "--config", cfg, "--from", srcPath, "--commit")
	if code != 0 {
		t.Fatalf("commit exited %d: %s%s", code, out, errb)
	}
	if strings.Contains(out, "dry run") {
		t.Errorf("--commit still reported a dry run:\n%s", out)
	}
	if !authenticates(t, cfg, migrated) {
		t.Fatalf("the imported credential does not authenticate after --commit:\n%s", out)
	}
	if authenticates(t, cfg, untouched) {
		t.Fatal("a token that was never in the source authenticates: the import is not selective")
	}

	// Re-running is safe. An operator re-runs a cutover step, and overwriting
	// would undo any revocation made in between.
	out, _, code = invoke("import", "keys", "--config", cfg, "--from", srcPath, "--commit")
	if code != 0 {
		t.Fatalf("second commit exited %d: %s", code, out)
	}
	if !strings.Contains(out, "already present") {
		t.Errorf("a re-import does not report the rows it left alone:\n%s", out)
	}
}

// TestImportKeysRefusesWhatItCannotDo covers the three ways the invocation is
// wrong, because each one has an answer better than a stack trace.
func TestImportKeysRefusesWhatItCannotDo(t *testing.T) {
	dir := t.TempDir()
	cfg := writeCfg(t, dir, "")

	if _, errb, code := invoke("import", "keys", "--config", cfg); code != 2 ||
		!strings.Contains(errb, "--from") {
		t.Errorf("a missing --from exits %d saying %q, want 2 naming --from", code, errb)
	}
	if _, errb, code := invoke("import", "keys", "--config", cfg,
		"--from", "x.db", "--on-missing-team", "invent"); code == 0 ||
		!strings.Contains(errb, "orphan") {
		t.Errorf("an unknown --on-missing-team policy exits %d saying %q, want a "+
			"refusal naming the two policies", code, errb)
	}
	// A source that is not there is a diagnosis, not a panic — and the DSN is
	// the one string in an import that routinely carries a password, so the
	// message must name the driver rather than echo it.
	_, errb, code := invoke("import", "keys", "--config", cfg,
		"--from", "postgres://user:hunter2@127.0.0.1:1/none")
	if code == 0 {
		t.Error("an unreachable source exited 0")
	}
	if strings.Contains(errb, "hunter2") {
		t.Errorf("the failure echoed the source DSN's password:\n%s", errb)
	}
}

// TestImportSubcommandsAreDistinct holds the boundary between the two verbs.
// `import config` writes YAML to stdout; `import keys` writes credentials into
// a live store. Conflating them grants access by accident.
func TestImportSubcommandsAreDistinct(t *testing.T) {
	_, errb, code := invoke("import")
	if code != 2 {
		t.Fatalf("bare `import` exited %d, want 2", code)
	}
	for _, want := range []string{"import config", "import keys"} {
		if !strings.Contains(errb, want) {
			t.Errorf("the usage line does not offer %q:\n%s", want, errb)
		}
	}
	if _, errb, code := invoke("import", "nonsense"); code != 2 ||
		!strings.Contains(errb, "import keys") {
		t.Errorf("an unknown import subcommand exits %d saying %q", code, errb)
	}
}

// TestUsageListsEveryImplementedSubcommand is the guard the health check
// already has, generalized: a command an operator reads about in `--help` and
// cannot run is the same defect as a store method with no entry point, which is
// what this file exists to close.
func TestUsageListsEveryImplementedSubcommand(t *testing.T) {
	out, _, code := invoke("help")
	if code != 0 {
		t.Fatalf("help exited %d", code)
	}
	for _, want := range []string{
		"config lint", "catalog explain", "price <model>",
		"import config", "import keys", "key create", "migrate", "health",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage does not list %q:\n%s", want, out)
		}
	}
}

// authenticates reports whether a token resolves against the deployment the
// configuration names. It opens the store the way the gateway does, so the
// answer is the gateway's.
func authenticates(t *testing.T, cfgPath, token string) bool {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := app.OpenStore(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.GetAPIKeyByLookup(ctx, store.KeyLookup(token))
	return err == nil
}
