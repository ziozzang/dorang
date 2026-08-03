package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var importNow = time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

// The source schema is the incumbent's, including the columns dorang refuses
// to carry. key_name is the one that matters: it stores the trailing
// characters of the secret for display.
const sourceDDL = `
CREATE TABLE "LiteLLM_VerificationToken" (
    token                 TEXT PRIMARY KEY,
    key_name              TEXT,
    key_alias             TEXT,
    soft_budget_cooldown  INTEGER,
    spend                 REAL,
    expires               TEXT,
    models                TEXT,
    aliases               TEXT,
    config                TEXT,
    user_id               TEXT,
    team_id               TEXT,
    permissions           TEXT,
    max_parallel_requests INTEGER,
    metadata              TEXT,
    blocked               INTEGER,
    tpm_limit             INTEGER,
    rpm_limit             INTEGER,
    max_budget            REAL,
    budget_duration       TEXT,
    budget_reset_at       TEXT,
    allowed_cache_controls TEXT,
    allowed_routes        TEXT,
    model_spend           TEXT,
    model_max_budget      TEXT,
    budget_id             TEXT,
    organization_id       TEXT,
    object_permission_id  TEXT,
    created_at            TEXT,
    updated_at            TEXT
)`

// The out-of-line permission table. A key's model restriction may live here
// instead of in its own `models` column, which is the shape that fails OPEN if
// the id is carried and never resolved.
const objectPermDDL = `
CREATE TABLE "LiteLLM_ObjectPermissionTable" (
    object_permission_id TEXT PRIMARY KEY,
    models               TEXT,
    mcp_servers          TEXT,
    vector_stores        TEXT
)`

type sourceRow struct {
	token         string
	keyName       string
	keyAlias      string
	spend         float64
	expires       any
	models        string
	allowedRoutes string
	userID        string
	teamID        string
	maxParallel   any
	blocked       int
	tpm, rpm      any
	maxBudget     any
	budgetPeriod  string
	objectPerm    string
}

// permRow is one row of the incumbent's permission table.
type permRow struct {
	id           string
	models       string
	mcpServers   string
	vectorStores string
}

func newSourceDB(t *testing.T, rows []sourceRow, perms ...permRow) *sql.DB {
	t.Helper()
	dsn := sqliteDSN(filepath.Join(t.TempDir(), "source.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(sourceDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(objectPermDDL); err != nil {
		t.Fatal(err)
	}
	for _, p := range perms {
		if _, err := db.Exec(`INSERT INTO "LiteLLM_ObjectPermissionTable"
			(object_permission_id, models, mcp_servers, vector_stores) VALUES (?, ?, ?, ?)`,
			p.id, p.models, p.mcpServers, p.vectorStores); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range rows {
		_, err := db.Exec(`INSERT INTO "LiteLLM_VerificationToken"
			(token, key_name, key_alias, spend, expires, models, allowed_routes, user_id, team_id,
			 max_parallel_requests, blocked, tpm_limit, rpm_limit, max_budget, budget_duration,
			 object_permission_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.token, r.keyName, r.keyAlias, r.spend, r.expires, r.models, r.allowedRoutes,
			r.userID, r.teamID, r.maxParallel, r.blocked, r.tpm, r.rpm, r.maxBudget,
			r.budgetPeriod, r.objectPerm, "2025-01-01 00:00:00", "2025-06-01 00:00:00")
		if err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// TestImportHonoursExpiry is the correction of DESIGN 2.4: a verification
// against a real deployment found the large majority of stored credentials
// already expired, and importing them as live would silently restore revoked
// access.
func TestImportHonoursExpiry(t *testing.T) {
	live := "sk-live-token" // pragma: allowlist secret — test fixture
	dead := "sk-dead-token" // pragma: allowlist secret — test fixture
	src := newSourceDB(t, []sourceRow{
		{token: HashLegacySHA256(live), keyName: "sk-...LIVE", teamID: "team-1",
			expires: "2027-01-01 00:00:00"},
		{token: HashLegacySHA256(dead), keyName: "sk-...DEAD", teamID: "team-1",
			expires: "2025-01-01 00:00:00"},
	})

	eachBackendCfg(t, func(c *Config) {
		c.Now = func() time.Time { return importNow }
		c.Legacy = LegacyAuth{Enabled: true, Until: importNow.AddDate(0, 1, 0)}
	}, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seedTeam(t, s, "team-1")

		rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow})
		if err != nil {
			t.Fatalf("ImportKeys: %v\n%s", err, rep.Summary())
		}
		if rep.Scanned != 2 || rep.Imported != 2 {
			t.Fatalf("scanned %d imported %d, want 2/2\n%s", rep.Scanned, rep.Imported, rep.Summary())
		}
		if rep.Expired != 1 {
			t.Fatalf("Expired = %d, want 1\n%s", rep.Expired, rep.Summary())
		}
		if !strings.Contains(rep.Summary(), "AS EXPIRED") {
			t.Fatalf("the report does not say the expired rows were imported as expired:\n%s", rep.Summary())
		}

		// The expired credential is present -- and refuses to authenticate.
		if _, err := s.AuthenticateKey(ctx, dead); !errors.Is(err, ErrKeyExpired) {
			t.Fatalf("expired credential authenticated: %v", err)
		}
		got, err := s.GetAPIKeyByLookup(ctx, KeyLookup(dead))
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		if !got.ExpiresAt.Equal(want) {
			t.Fatalf("expiry = %s, want %s -- it must be carried, not extended", got.ExpiresAt, want)
		}

		auth, err := s.AuthenticateKey(ctx, live)
		if err != nil {
			t.Fatalf("live credential: %v", err)
		}
		if auth.Key.HashScheme != SchemeLegacySHA256 || !auth.NeedsRehash {
			t.Fatalf("imported key should verify as legacy and ask for a rehash: %+v", auth)
		}
	})
}

// TestImportNeverCopiesTheSecretRevealingColumn: nothing derived from the tail
// of the secret may reach the destination, and the report must say the column
// was refused rather than say nothing.
func TestImportNeverCopiesTheSecretRevealingColumn(t *testing.T) {
	const token = "sk-super-secret-value-Zq7X" // pragma: allowlist secret — test fixture
	const tail = "Zq7X"
	src := newSourceDB(t, []sourceRow{
		{token: HashLegacySHA256(token), keyName: "sk-..." + tail, keyAlias: "reporting-bot"},
	})

	eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
		func(t *testing.T, s *Store) {
			ctx := context.Background()
			rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Imported != 1 {
				t.Fatalf("imported %d\n%s", rep.Imported, rep.Summary())
			}

			var refused bool
			for _, d := range rep.DroppedColumns {
				if strings.EqualFold(d.Column, "key_name") {
					refused = true
					if !strings.Contains(d.Reason, "secret") {
						t.Fatalf("key_name refusal does not say why: %q", d.Reason)
					}
				}
			}
			if !refused {
				t.Fatalf("key_name is not reported as refused:\n%s", rep.Summary())
			}

			// Sweep every stored value: the tail must appear nowhere.
			for _, v := range dumpTable(t, s, "api_keys") {
				if strings.Contains(v, tail) {
					t.Fatalf("a stored value contains the secret's trailing characters: %q", v)
				}
			}

			key, err := s.GetAPIKeyByLookup(ctx, KeyLookup(token))
			if err != nil {
				t.Fatal(err)
			}
			if key.KeyLabel != labelFromLookup(KeyLookup(token)) {
				t.Fatalf("label = %q, want one derived from the digest", key.KeyLabel)
			}
			// The user-chosen alias is not part of the secret and is carried.
			if key.KeyAlias != "reporting-bot" {
				t.Fatalf("key_alias = %q, want it carried", key.KeyAlias)
			}
		})
}

// TestImportCarriesEveryAuthorizationField: DESIGN 2.4 lists these explicitly
// because a field that is not carried is a limit that silently stops existing.
func TestImportCarriesEveryAuthorizationField(t *testing.T) {
	const token = "sk-fully-specified" // pragma: allowlist secret — test fixture
	src := newSourceDB(t, []sourceRow{{
		token:         HashLegacySHA256(token),
		keyName:       "sk-...AAAA",
		keyAlias:      "batch-runner",
		spend:         1.25,
		expires:       "2027-03-04 05:06:07",
		models:        `["model-a","model-b"]`,
		allowedRoutes: `{/v1/chat/completions,/v1/embeddings}`,
		userID:        "user-7",
		teamID:        "team-1",
		maxParallel:   int64(4),
		blocked:       1,
		tpm:           int64(90000),
		rpm:           int64(600),
		maxBudget:     12.5,
		budgetPeriod:  "monthly",
		objectPerm:    "objperm-9",
	}},
		// The permission row exists and restricts nothing, so the key keeps
		// its own model allow-list. An id that resolves to nothing is the
		// only shape in which a carried object_permission_id still imports.
		permRow{id: "objperm-9"})

	eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
		func(t *testing.T, s *Store) {
			ctx := context.Background()
			seedTeam(t, s, "team-1")
			seedUser(t, s, "user-7")

			rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Imported != 1 || rep.Blocked != 1 {
				t.Fatalf("report = %+v\n%s", rep, rep.Summary())
			}

			k, err := s.GetAPIKeyByLookup(ctx, KeyLookup(token))
			if err != nil {
				t.Fatal(err)
			}
			checks := []struct {
				name      string
				got, want any
			}{
				{"blocked", k.Blocked, true},
				{"user", k.UserID, "user-7"},
				{"team", k.TeamID, "team-1"},
				{"object permission", k.ObjectPermissionID, "objperm-9"},
				{"budget period", k.BudgetPeriod, "monthly"},
				{"spend", k.SpendNano, int64(1_250_000_000)},
				{"max budget", *k.MaxBudgetNano, int64(12_500_000_000)},
				{"rpm", *k.RPMLimit, int64(600)},
				{"tpm", *k.TPMLimit, int64(90000)},
				{"max parallel", *k.MaxParallel, int64(4)},
				{"expiry", k.ExpiresAt, time.Date(2027, 3, 4, 5, 6, 7, 0, time.UTC)},
				{"hash scheme", k.HashScheme, SchemeLegacySHA256},
				{"source", k.Source, "litellm"},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
				}
			}
			if strings.Join(k.Models, ",") != "model-a,model-b" {
				t.Errorf("model allow-list = %v", k.Models)
			}
			if strings.Join(k.AllowedRoutes, ",") != "/v1/chat/completions,/v1/embeddings" {
				t.Errorf("route allow-list = %v", k.AllowedRoutes)
			}
		})
}

// TestImportRefusesKeysWhoseTeamIsMissing: an orphaned key runs with no team
// budget, no team rate limit and no team blocked flag -- it fails open.
func TestImportRefusesKeysWhoseTeamIsMissing(t *testing.T) {
	const token = "sk-orphan" // pragma: allowlist secret — test fixture
	src := newSourceDB(t, []sourceRow{
		{token: HashLegacySHA256(token), keyName: "sk-...ORPH", teamID: "team-gone"},
	})

	t.Run("default skips and reports", func(t *testing.T) {
		eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
			func(t *testing.T, s *Store) {
				ctx := context.Background()
				rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow})
				if err != nil {
					t.Fatal(err)
				}
				if rep.MissingTeam != 1 || rep.Imported != 0 || rep.Skipped != 1 {
					t.Fatalf("report = %+v\n%s", rep, rep.Summary())
				}
				if len(rep.NotMigrated) != 1 || rep.NotMigrated[0].Reason != "missing team" {
					t.Fatalf("NotMigrated = %+v", rep.NotMigrated)
				}
				if _, err := s.GetAPIKeyByLookup(ctx, KeyLookup(token)); !errors.Is(err, ErrNotFound) {
					t.Fatalf("the key was written anyway: %v", err)
				}
			})
	})

	t.Run("opt-in orphaning is reported too", func(t *testing.T) {
		eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
			func(t *testing.T, s *Store) {
				ctx := context.Background()
				rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow, OnMissingTeam: MissingTeamOrphan})
				if err != nil {
					t.Fatal(err)
				}
				if rep.MissingTeam != 1 || rep.Imported != 1 {
					t.Fatalf("report = %+v\n%s", rep, rep.Summary())
				}
				if len(rep.Warnings) == 0 {
					t.Fatal("orphaning produced no warning")
				}
				k, err := s.GetAPIKeyByLookup(ctx, KeyLookup(token))
				if err != nil {
					t.Fatal(err)
				}
				if k.TeamID != "" {
					t.Fatalf("team = %q, want it dropped", k.TeamID)
				}
			})
	})
}

// TestImportWarnsAboutTheAdministrativeCredential: a gateway that only reads an
// imported database loses admin auth entirely (DESIGN 2.4).
func TestImportWarnsAboutTheAdministrativeCredential(t *testing.T) {
	src := newSourceDB(t, []sourceRow{
		{token: HashLegacySHA256("sk-ordinary"), keyName: "sk-...ORD"},                       // pragma: allowlist secret — test fixture
		{token: "sk-my-plaintext-master-key", keyName: "sk-...MSTR", keyAlias: "master-key"}, // pragma: allowlist secret — test fixture
	})

	eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
		func(t *testing.T, s *Store) {
			ctx := context.Background()
			rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow, ExpectAdminKey: true})
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(rep.Warnings, "\n")
			if !strings.Contains(joined, "out-of-band") {
				t.Fatalf("no out-of-band warning:\n%s", rep.Summary())
			}
			if !strings.Contains(joined, "master_key_env") {
				t.Fatalf("the warning does not say what to configure instead:\n%s", rep.Summary())
			}
			// The plaintext row is refused, not ingested.
			if rep.Imported != 1 || rep.Skipped != 1 {
				t.Fatalf("report = %+v\n%s", rep, rep.Summary())
			}
			for _, v := range dumpTable(t, s, "api_keys") {
				if strings.Contains(v, "sk-my-plaintext-master-key") { // pragma: allowlist secret — test fixture
					t.Fatal("a plaintext secret was written to the destination")
				}
			}
		})
}

func TestImportAccountsForEveryRowAndColumn(t *testing.T) {
	src := newSourceDB(t, []sourceRow{
		{token: HashLegacySHA256("sk-a"), keyName: "sk-...A"},
		{token: "not-a-digest", keyName: "sk-...B"}, // pragma: allowlist secret — test fixture
		{token: "", keyName: "sk-...C"},
		{token: HashLegacySHA256("sk-d"), keyName: "sk-...D", teamID: "team-missing"},
	})

	eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
		func(t *testing.T, s *Store) {
			rep, err := s.ImportKeys(context.Background(), src, ImportOptions{Now: importNow})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Scanned != 4 {
				t.Fatalf("scanned %d, want 4", rep.Scanned)
			}
			if rep.Imported+rep.Skipped+rep.AlreadyPresent != rep.Scanned {
				t.Fatalf("rows unaccounted for: imported %d + skipped %d + present %d != scanned %d\n%s",
					rep.Imported, rep.Skipped, rep.AlreadyPresent, rep.Scanned, rep.Summary())
			}
			if len(rep.NotMigrated) != rep.Skipped {
				t.Fatalf("%d skipped but %d reasons given", rep.Skipped, len(rep.NotMigrated))
			}
			for _, n := range rep.NotMigrated {
				if n.Reason == "" {
					t.Fatalf("a row was dropped without a reason: %+v", n)
				}
			}

			// Every source column is either carried or reported as not carried.
			carried := 0
			for range importedColumns {
				carried++
			}
			reported := map[string]bool{}
			for _, d := range rep.DroppedColumns {
				reported[strings.ToLower(d.Column)] = true
				if d.Reason == "" {
					t.Fatalf("column %s dropped without a reason", d.Column)
				}
			}
			for _, col := range []string{"key_name", "aliases", "config", "permissions",
				"metadata", "model_spend", "model_max_budget", "budget_id", "organization_id",
				"allowed_cache_controls", "soft_budget_cooldown"} {
				if !reported[col] {
					t.Errorf("source column %s vanished without a report line", col)
				}
			}
		})
}

// ---------------------------------------------------------------------------
// Meaning, not data
//
// Measured against a live incumbent with 54 keys: the import's arithmetic was
// exactly right -- 54 scanned, 4 imported, 50 skipped for a missing team, every
// lookup matching sha256(token)[:32] of the source digest -- and three of the
// incumbent's authorization idioms crossed as data with their meaning gone. The
// report said nothing about any of them. These tests assert the REPORT, because
// the report is the artifact that failed.
// ---------------------------------------------------------------------------

// TestAnObjectPermissionRestrictionArrivesOnTheKey is the fail-open, closed.
//
// The incumbent may keep a key's model restriction in a row of
// LiteLLM_ObjectPermissionTable rather than in the key's own `models` column.
// Carrying the id and never resolving it leaves dorang's `models` EMPTY, and an
// empty allow-list allows every model — so a key that could reach one model
// arrives able to reach all of them, with no line anywhere saying so.
//
// It did not fire on the live database only by luck: the one key carrying an id
// pointed at a row whose `models` was empty. The other row in that same table
// restricted to a single model.
func TestAnObjectPermissionRestrictionArrivesOnTheKey(t *testing.T) {
	const token = "sk-fenced" // pragma: allowlist secret — test fixture
	src := newSourceDB(t, []sourceRow{{
		token: HashLegacySHA256(token), keyName: "sk-...FN",
		// The key's own models column is empty. That is the whole hazard:
		// empty is dorang's spelling for "unrestricted".
		models: "",
		// The restriction lives over here.
		objectPerm: "objperm-fenced",
	}}, permRow{id: "objperm-fenced", models: `{qwen3.5:397b}`})

	eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
		func(t *testing.T, s *Store) {
			ctx := context.Background()
			rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Imported != 1 {
				t.Fatalf("imported %d, want 1\n%s", rep.Imported, rep.Summary())
			}
			if rep.ObjectPermissions != 1 {
				t.Errorf("ObjectPermissions = %d, want 1: the report has to say that a "+
					"restriction was resolved, or an operator cannot tell a key that was "+
					"fenced from one that never was", rep.ObjectPermissions)
			}
			k, err := s.GetAPIKeyByLookup(ctx, KeyLookup(token))
			if err != nil {
				t.Fatal(err)
			}
			if len(k.Models) == 0 {
				t.Fatalf("model allow-list is empty, and empty allows EVERY model: the "+
					"restriction in %s did not cross", DefaultObjectPermissionTable)
			}
			if strings.Join(k.Models, ",") != "qwen3.5:397b" {
				t.Fatalf("model allow-list = %v, want the permission row's [qwen3.5:397b]", k.Models)
			}
		})
}

// TestAnUnresolvableObjectPermissionIsRefusedAndNamed covers every way the
// resolution can fail. Each one is refused rather than imported, because each
// one imports as an empty allow-list and empty allows everything — the failure
// is silent in exactly the cases where the operator most needs to hear it.
func TestAnUnresolvableObjectPermissionIsRefusedAndNamed(t *testing.T) {
	cases := []struct {
		name  string
		perms []permRow
		want  string // a fragment the report must carry
	}{
		{
			name:  "no such row",
			perms: []permRow{{id: "objperm-other", models: `{model-a}`}},
			want:  "no such row",
		},
		{
			name:  "a restriction dorang has no allow-list for",
			perms: []permRow{{id: "objperm-x", models: `{model-a}`, vectorStores: `{store-1}`}},
			want:  "vector_stores",
		},
		{
			name:  "the permission row holds a sentinel of its own",
			perms: []permRow{{id: "objperm-x", models: `{all-team-models}`}},
			want:  "all-team-models",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const token = "sk-unresolvable" // pragma: allowlist secret — test fixture
			src := newSourceDB(t, []sourceRow{{
				token: HashLegacySHA256(token), keyName: "sk-...UN", objectPerm: "objperm-x",
			}}, c.perms...)

			eachBackendCfg(t, func(cfg *Config) { cfg.Now = func() time.Time { return importNow } },
				func(t *testing.T, s *Store) {
					ctx := context.Background()
					rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow})
					if err != nil {
						t.Fatal(err)
					}
					if rep.Imported != 0 {
						t.Fatalf("imported %d: an unresolved object permission imports as an "+
							"EMPTY model allow-list, which allows every model\n%s",
							rep.Imported, rep.Summary())
					}
					// Refusal is not enough on its own: the operator has to be
					// told which key and which id.
					if len(rep.Untranslated) != 1 {
						t.Fatalf("Untranslated = %+v, want one entry naming the id", rep.Untranslated)
					}
					u := rep.Untranslated[0]
					if u.Column != "object_permission_id" || u.Value != "objperm-x" ||
						u.Action != "refused" {
						t.Errorf("report entry = %+v, want object_permission_id/objperm-x refused", u)
					}
					if !strings.Contains(u.Detail, c.want) {
						t.Errorf("the report does not say %q:\n%s", c.want, rep.Summary())
					}
					// The counters stay reconciled.
					if len(rep.NotMigrated) != rep.Skipped || rep.Skipped != 1 {
						t.Errorf("skipped %d with %d reasons, want 1 and 1",
							rep.Skipped, len(rep.NotMigrated))
					}
					// And clearing is NOT available for this one, whatever the
					// operator asks for: a widening is not a flag.
					rep2, err := s.ImportKeys(ctx, src, ImportOptions{
						Now: importNow, OnUntranslatable: UntranslatableClear,
					})
					if err != nil {
						t.Fatal(err)
					}
					if rep2.Imported != 0 {
						t.Fatalf("--on-untranslatable=clear imported an unresolved object "+
							"permission: clearing this one drops a restriction rather than "+
							"an unmatchable literal\n%s", rep2.Summary())
					}
				})
		})
	}
}

// TestARouteGroupOrAModelSentinelIsRefusedByName is the other two idioms. Both
// fail CLOSED — the entry matches nothing, so the key is refused every route or
// every model — which is why they are second priority and not third: three of
// the four importable keys on the live database carried the route group, two
// carried the model sentinel, and every one of them authenticated fine and
// could do nothing.
func TestARouteGroupOrAModelSentinelIsRefusedByName(t *testing.T) {
	const (
		routeToken = "sk-route-group"    // pragma: allowlist secret — test fixture
		modelToken = "sk-model-sentinel" // pragma: allowlist secret — test fixture
	)
	src := newSourceDB(t, []sourceRow{
		{token: HashLegacySHA256(routeToken), keyName: "sk-...RG",
			allowedRoutes: `{llm_api_routes}`},
		{token: HashLegacySHA256(modelToken), keyName: "sk-...MS",
			models: `{all-team-models}`},
	})

	eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
		func(t *testing.T, s *Store) {
			ctx := context.Background()
			rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Imported != 0 || rep.Skipped != 2 {
				t.Fatalf("imported %d, skipped %d, want 0 and 2\n%s",
					rep.Imported, rep.Skipped, rep.Summary())
			}
			named := map[string]Untranslated{}
			for _, u := range rep.Untranslated {
				named[u.Value] = u
			}
			for _, want := range []struct{ value, column string }{
				{"llm_api_routes", "allowed_routes"},
				{"all-team-models", "models"},
			} {
				u, ok := named[want.value]
				if !ok {
					t.Fatalf("the report never names %q; an operator finds out when a "+
						"consumer gets 403 instead\n%s", want.value, rep.Summary())
				}
				if u.Column != want.column || u.Action != "refused" {
					t.Errorf("%q reported as %+v, want %s refused", want.value, u, want.column)
				}
			}
			// The summary is what the operator actually reads.
			sum := rep.Summary()
			for _, want := range []string{"llm_api_routes", "all-team-models", "--on-untranslatable"} {
				if !strings.Contains(sum, want) {
					t.Errorf("the printed summary does not mention %q:\n%s", want, sum)
				}
			}
		})
}

// TestClearingAnUntranslatableAllowListIsOptInAndNamed pins the escape hatch.
//
// Refusing every key with a route group would leave an operator with no way
// forward but hand-editing the incumbent's database, so `clear` exists. It is
// opt-in because it WIDENS — an empty dorang allow-list is unrestricted — and
// every key it touches is named, because a widening nobody is told about is the
// defect this whole file is about.
func TestClearingAnUntranslatableAllowListIsOptInAndNamed(t *testing.T) {
	const token = "sk-cleared" // pragma: allowlist secret — test fixture
	src := newSourceDB(t, []sourceRow{{
		token: HashLegacySHA256(token), keyName: "sk-...CL",
		allowedRoutes: `{llm_api_routes}`,
		// A widening sentinel beside a literal: the union the incumbent
		// read is "every team model", so keeping model-a and dropping the
		// sentinel would leave the key NARROWER than it was.
		models: `["all-team-models","model-a"]`,
	}})

	eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
		func(t *testing.T, s *Store) {
			ctx := context.Background()
			rep, err := s.ImportKeys(ctx, src, ImportOptions{
				Now: importNow, OnUntranslatable: UntranslatableClear,
			})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Imported != 1 {
				t.Fatalf("imported %d, want 1\n%s", rep.Imported, rep.Summary())
			}
			if len(rep.Untranslated) != 2 {
				t.Fatalf("Untranslated = %+v, want both idioms named", rep.Untranslated)
			}
			for _, u := range rep.Untranslated {
				if u.Action != "cleared" {
					t.Errorf("%+v: want action cleared", u)
				}
			}
			k, err := s.GetAPIKeyByLookup(ctx, KeyLookup(token))
			if err != nil {
				t.Fatal(err)
			}
			if len(k.AllowedRoutes) != 0 {
				t.Errorf("route allow-list = %v, want cleared", k.AllowedRoutes)
			}
			if len(k.Models) != 0 {
				t.Errorf("model allow-list = %v: a union containing \"every team model\" is "+
					"every team model, so the literal beside it cannot be kept as the whole "+
					"of the restriction", k.Models)
			}
			if !strings.Contains(rep.Summary(), "cleared") {
				t.Errorf("the summary does not report the widening:\n%s", rep.Summary())
			}
		})
}

func TestImportIsIdempotentAndNeverOverwrites(t *testing.T) {
	const token = "sk-twice" // pragma: allowlist secret — test fixture
	src := newSourceDB(t, []sourceRow{{token: HashLegacySHA256(token), keyName: "sk-...TW"}})

	eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
		func(t *testing.T, s *Store) {
			ctx := context.Background()
			if _, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow}); err != nil {
				t.Fatal(err)
			}
			// An administrator revokes it in dorang afterwards.
			mustExec(t, s, `UPDATE api_keys SET blocked = ? WHERE lookup = ?`, true, KeyLookup(token))

			rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow})
			if err != nil {
				t.Fatal(err)
			}
			if rep.AlreadyPresent != 1 || rep.Imported != 0 {
				t.Fatalf("second import: %+v\n%s", rep, rep.Summary())
			}
			k, err := s.GetAPIKeyByLookup(ctx, KeyLookup(token))
			if err != nil {
				t.Fatal(err)
			}
			if !k.Blocked {
				t.Fatal("re-importing un-revoked a key an administrator had blocked")
			}
		})
}

func TestImportDryRunWritesNothing(t *testing.T) {
	const token = "sk-dry" // pragma: allowlist secret — test fixture
	src := newSourceDB(t, []sourceRow{{token: HashLegacySHA256(token), keyName: "sk-...DR"}})

	eachBackendCfg(t, func(c *Config) { c.Now = func() time.Time { return importNow } },
		func(t *testing.T, s *Store) {
			ctx := context.Background()
			rep, err := s.ImportKeys(ctx, src, ImportOptions{Now: importNow, DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Imported != 1 || !rep.DryRun {
				t.Fatalf("report = %+v", rep)
			}
			if _, err := s.GetAPIKeyByLookup(ctx, KeyLookup(token)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("a dry run wrote a row: %v", err)
			}
		})
}

func TestImportRejectsAnInjectedTableName(t *testing.T) {
	src := newSourceDB(t, nil)
	eachBackend(t, func(t *testing.T, s *Store) {
		_, err := s.ImportKeys(context.Background(), src, ImportOptions{Table: `x"; DROP TABLE api_keys; --`})
		if !errors.Is(err, ErrBadIdentifier) {
			t.Fatalf("got %v, want ErrBadIdentifier", err)
		}
	})
}

func TestForeignValueCoercion(t *testing.T) {
	t.Run("lists", func(t *testing.T) {
		cases := []struct {
			in   any
			want string
		}{
			{`["a","b"]`, "a,b"},
			{`{a,b}`, "a,b"},
			{`{"a b","c"}`, "a b,c"},
			{"single", "single"},
			{nil, ""},
			{"[]", ""},
			{"{}", ""},
		}
		for _, c := range cases {
			if got := strings.Join(asList(c.in), ","); got != c.want {
				t.Errorf("asList(%v) = %q, want %q", c.in, got, c.want)
			}
		}
	})

	t.Run("money", func(t *testing.T) {
		if n, err := nanoFromAny(1.5); err != nil || n != 1_500_000_000 {
			t.Errorf("nanoFromAny(1.5) = %d, %v", n, err)
		}
		if _, err := nanoFromAny(1e30); !errors.Is(err, ErrAmountRange) {
			t.Errorf("nanoFromAny(1e30) = %v, want ErrAmountRange", err)
		}
	})

	t.Run("times", func(t *testing.T) {
		want := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
		for _, in := range []any{
			"2026-04-01T12:00:00Z",
			"2026-04-01 12:00:00",
			want,
			want.Unix(),
			want.UnixMicro(),
		} {
			got, ok := asTime(in)
			if !ok || !got.Equal(want) {
				t.Errorf("asTime(%v) = %s, %v", in, got, ok)
			}
		}
		if _, ok := asTime(nil); ok {
			t.Error("asTime(nil) reported a time")
		}
	})
}

// dumpTable stringifies every value in a table, so a test can assert that a
// forbidden substring appears nowhere at all rather than nowhere it thought to
// look.
func dumpTable(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.query(context.Background(), "SELECT * FROM "+table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, c := range cells {
			out = append(out, asString(c))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
