package scenario

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/store"
)

// DESIGN §14 scenario 13 — legacy credential import.
//
//	expired rows stay expired
//	live rows authenticate
//	rehash-on-use upgrades them
//
// The three are one scenario because getting any of them wrong is the same
// failure with a different sign: importing an expired row as live silently
// restores revoked access, refusing a live row locks the operator out of their
// own migration, and never upgrading makes the unsalted digest permanent — which
// DESIGN §2.4 admits only as a bounded window.

// The incumbent's table, including the columns dorang refuses to carry.
// key_name is the one that matters: it stores trailing characters of the
// secret for display, and nothing derived from it may reach the destination.
const legacySourceDDL = `
CREATE TABLE "LiteLLM_VerificationToken" (
    token                 TEXT PRIMARY KEY,
    key_name              TEXT,
    key_alias             TEXT,
    spend                 REAL,
    expires               TEXT,
    models                TEXT,
    user_id               TEXT,
    team_id               TEXT,
    max_parallel_requests INTEGER,
    blocked               INTEGER,
    tpm_limit             INTEGER,
    rpm_limit             INTEGER,
    max_budget            REAL,
    budget_duration       TEXT,
    created_at            TEXT,
    updated_at            TEXT
)`

type legacyRow struct {
	token   string // the STORED value: sha256 of the full "sk-..." token, hex // pragma: allowlist secret — test fixture
	keyName string
	expires string
	blocked int
}

func newLegacySource(t *testing.T, rows []legacyRow) *sql.DB {
	t.Helper()
	// The store's own driver name; importing internal/store registers it.
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "incumbent.db"))
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(legacySourceDDL); err != nil {
		t.Fatalf("source DDL: %v", err)
	}
	for _, r := range rows {
		_, err := db.Exec(`INSERT INTO "LiteLLM_VerificationToken"
			(token, key_name, expires, blocked, spend, created_at, updated_at)
			VALUES (?, ?, ?, ?, 0, ?, ?)`,
			r.token, r.keyName, nullable(r.expires), r.blocked,
			"2025-01-01 00:00:00", "2025-06-01 00:00:00")
		if err != nil {
			t.Fatalf("seed source: %v", err)
		}
	}
	return db
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func openStore(t *testing.T, now func() time.Time, legacy store.LegacyAuth) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), store.Config{
		Driver: store.DialectSQLite,
		DSN:    filepath.Join(t.TempDir(), "dorang.db"),
		Pepper: []byte("scenario-pepper-not-a-real-secret"),
		Legacy: legacy,
		Now:    now,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestScenario13_LegacyCredentialImport(t *testing.T) {
	const (
		live    = "sk-scenario-live-token"    // pragma: allowlist secret — test fixture
		dead    = "sk-scenario-expired-token" // pragma: allowlist secret — test fixture
		blocked = "sk-scenario-blocked-token" // pragma: allowlist secret — test fixture
	)
	clk := newClock()
	src := newLegacySource(t, []legacyRow{
		{token: store.HashLegacySHA256(live), keyName: "sk-...LIVE", expires: "2027-01-01 00:00:00"}, // pragma: allowlist secret — test fixture
		{token: store.HashLegacySHA256(dead), keyName: "sk-...DEAD", expires: "2025-01-01 00:00:00"}, // pragma: allowlist secret — test fixture
		{token: store.HashLegacySHA256(blocked), keyName: "sk-...BLOK", blocked: 1},                  // pragma: allowlist secret — test fixture
	})

	s := openStore(t, clk.now, store.LegacyAuth{Enabled: true, Until: clk.now().AddDate(0, 1, 0)})
	ctx := context.Background()

	rep, err := s.ImportKeys(ctx, src, store.ImportOptions{Now: clk.now()})
	if err != nil {
		t.Fatalf("ImportKeys: %v\n%s", err, rep.Summary())
	}
	if rep.Scanned != 3 || rep.Imported != 3 {
		t.Fatalf("scanned %d imported %d, want 3/3\n%s", rep.Scanned, rep.Imported, rep.Summary())
	}

	t.Run("expired rows are imported AS EXPIRED, never resurrected", func(t *testing.T) {
		if rep.Expired != 1 {
			t.Fatalf("Expired = %d, want 1\n%s", rep.Expired, rep.Summary())
		}
		if _, err := s.AuthenticateKey(ctx, dead); !errors.Is(err, store.ErrKeyExpired) {
			t.Fatalf("an expired credential authenticated: %v", err)
		}
		// It must be PRESENT and refused as expired, not absent and refused as
		// unknown: the two are indistinguishable to a caller, and only one of
		// them survives an operator asking why their key stopped working.
		row, err := s.GetAPIKeyByLookup(ctx, store.KeyLookup(dead))
		if err != nil {
			t.Fatalf("the expired row is missing entirely: %v", err)
		}
		want := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		if !row.ExpiresAt.Equal(want) {
			t.Fatalf("expiry = %s, want %s carried verbatim", row.ExpiresAt, want)
		}
		if !strings.Contains(rep.Summary(), "AS EXPIRED") {
			t.Errorf("the report must say the rows were imported as expired:\n%s", rep.Summary())
		}
	})

	t.Run("the blocked flag is carried", func(t *testing.T) {
		if rep.Blocked != 1 {
			t.Errorf("Blocked = %d, want 1", rep.Blocked)
		}
		if _, err := s.AuthenticateKey(ctx, blocked); !errors.Is(err, store.ErrKeyBlocked) {
			t.Fatalf("a blocked credential authenticated: %v", err)
		}
	})

	t.Run("the secret-revealing display column is never copied", func(t *testing.T) {
		row, err := s.GetAPIKeyByLookup(ctx, store.KeyLookup(live))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(row.KeyLabel, "LIVE") {
			t.Errorf("the label carries the incumbent's display column: %q", row.KeyLabel)
		}
		var refused bool
		for _, d := range rep.DroppedColumns {
			if d.Column == "key_name" {
				refused = true
			}
		}
		if !refused {
			t.Errorf("the refusal must be reported, not silent:\n%s", rep.Summary())
		}
	})

	t.Run("live rows authenticate, and ask to be upgraded", func(t *testing.T) {
		a, err := s.AuthenticateKey(ctx, live)
		if err != nil {
			t.Fatalf("the live credential did not authenticate: %v", err)
		}
		if a.Key.HashScheme != store.SchemeLegacySHA256 {
			t.Fatalf("scheme = %q, want legacy_sha256 immediately after import", a.Key.HashScheme)
		}
		if !a.NeedsRehash {
			t.Fatal("a legacy verification must ask for an upgrade, or the window never closes")
		}
	})

	t.Run("rehash-on-use upgrades the row, and the credential keeps working", func(t *testing.T) {
		a, err := s.AuthenticateKey(ctx, live)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RehashKey(ctx, a.Key.ID, live); err != nil {
			t.Fatalf("RehashKey: %v", err)
		}
		after, err := s.AuthenticateKey(ctx, live)
		if err != nil {
			t.Fatalf("the credential stopped working after the upgrade: %v", err)
		}
		if after.Key.HashScheme != store.SchemeDorangV1 {
			t.Fatalf("scheme = %q, want dorang_v1 after the upgrade", after.Key.HashScheme)
		}
		if after.NeedsRehash {
			t.Error("an upgraded row must stop asking to be upgraded")
		}
		// The index key is scheme-independent, which is what made a flag day
		// unnecessary: the row is still found by the same lookup.
		if _, err := s.GetAPIKeyByLookup(ctx, store.KeyLookup(live)); err != nil {
			t.Fatalf("the lookup changed with the scheme: %v", err)
		}

		t.Run("inverse: the upgrade is idempotent and does not resurrect legacy", func(t *testing.T) {
			if err := s.RehashKey(ctx, a.Key.ID, live); err != nil {
				t.Fatalf("a repeated upgrade must be a no-op, got %v", err)
			}
			again, err := s.AuthenticateKey(ctx, live)
			if err != nil || again.Key.HashScheme != store.SchemeDorangV1 {
				t.Fatalf("scheme = %q err = %v", again.Key.HashScheme, err)
			}
		})
	})

	t.Run("the closed legacy window refuses what it once admitted", func(t *testing.T) {
		// The window is what makes the unsalted digest a migration rather than a
		// permanent cryptographic constraint. A second store over the same file
		// with the window shut must refuse a row that is still legacy.
		clk := newClock()
		src := newLegacySource(t, []legacyRow{
			{token: store.HashLegacySHA256(live), keyName: "sk-...LIVE", expires: "2027-01-01 00:00:00"}, // pragma: allowlist secret — test fixture
		})
		open := openStore(t, clk.now, store.LegacyAuth{Enabled: true, Until: clk.now().AddDate(0, 1, 0)})
		if _, err := open.ImportKeys(context.Background(), src, store.ImportOptions{Now: clk.now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := open.AuthenticateKey(context.Background(), live); err != nil {
			t.Fatalf("inside the window the credential must work: %v", err)
		}
		// Past the sunset date, the same row and the same token are refused.
		clk.advance(60 * 24 * time.Hour)
		if _, err := open.AuthenticateKey(context.Background(), live); !errors.Is(err, store.ErrLegacyDisabled) {
			t.Fatalf("a closed legacy window still admitted a legacy row: %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// The composed path: internal/auth over internal/store
// -----------------------------------------------------------------------------

// storeAuth adapts *store.Store to the two narrow interfaces internal/auth
// needs. The packages do not know about each other by design (DESIGN §9.1);
// this is the wiring that names both, and it is the shape cmd/dorang uses.
type storeAuth struct{ s *store.Store }

func (a storeAuth) LoadByLookup(ctx context.Context, lookup string) (auth.Record, error) {
	k, err := a.s.GetAPIKeyByLookup(ctx, lookup)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return auth.Record{}, auth.ErrNotFound
		}
		return auth.Record{}, err
	}
	digest, err := auth.ParseDigest(k.TokenHash)
	if err != nil {
		return auth.Record{}, err
	}
	scheme, err := auth.ParseScheme(string(k.HashScheme))
	if err != nil {
		return auth.Record{}, err
	}
	return auth.Record{
		Lookup: k.Lookup,
		Digest: digest,
		Scheme: scheme,
		Principal: auth.Principal{
			KeyID: k.ID, Label: k.KeyLabel, UserID: k.UserID, TeamID: k.TeamID,
			Key: auth.Limits{
				Blocked:   k.Blocked,
				ExpiresAt: k.ExpiresAt,
				Models:    k.Models,
			},
		},
	}, nil
}

func (a storeAuth) Rehash(ctx context.Context, keyID, lookup string, digest auth.Digest) error {
	// The authenticator hands over the already-computed dorang_v1 digest, so
	// the plaintext never leaves the request that verified it.
	_, err := a.s.DB().ExecContext(ctx,
		`UPDATE api_keys SET token_hash = ?, hash_scheme = ? WHERE id = ? AND hash_scheme = ?`,
		digest.Hex(), string(store.SchemeDorangV1), keyID, string(store.SchemeLegacySHA256))
	return err
}

func TestLegacyCredentialUpgradesThroughTheAuthenticator(t *testing.T) {
	const live = "sk-scenario-auth-token" // pragma: allowlist secret — test fixture
	clk := newClock()
	src := newLegacySource(t, []legacyRow{
		{token: store.HashLegacySHA256(live), keyName: "sk-...LIVE", expires: "2027-01-01 00:00:00"}, // pragma: allowlist secret — test fixture
	})
	s := openStore(t, clk.now, store.LegacyAuth{Enabled: true, Until: clk.now().AddDate(0, 1, 0)})
	ctx := context.Background()
	if _, err := s.ImportKeys(ctx, src, store.ImportOptions{Now: clk.now()}); err != nil {
		t.Fatal(err)
	}

	a, err := auth.New(auth.Config{
		Pepper:      "scenario-pepper-not-a-real-secret",
		MasterKey:   "sk-master-not-a-real-secret", // pragma: allowlist secret — test fixture
		Legacy:      auth.LegacyPolicy{Enabled: true, Until: clk.now().AddDate(0, 1, 0)},
		RehashOnUse: true,
		Store:       storeAuth{s: s},
		Now:         clk.now,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(a.Close)

	p, err := a.Authenticate(ctx, live)
	if err != nil {
		t.Fatalf("the imported credential did not authenticate through the authenticator: %v", err)
	}
	if p.KeyID == "" {
		t.Fatal("the principal carries no key id")
	}

	// The upgrade is asynchronous and off the request path, which is the whole
	// point: it must not add latency to the request that triggered it.
	waitFor(t, func() bool { return a.Stats().RehashDone > 0 }, "the asynchronous rehash to land")

	row, err := s.GetAPIKeyByLookup(ctx, store.KeyLookup(live))
	if err != nil {
		t.Fatal(err)
	}
	if row.HashScheme != store.SchemeDorangV1 {
		t.Fatalf("the stored scheme is still %q after rehash-on-use", row.HashScheme)
	}
	// And the credential still works, now through the upgraded scheme.
	a.InvalidateAll()
	if _, err := a.Authenticate(ctx, live); err != nil {
		t.Fatalf("the upgraded credential stopped authenticating: %v", err)
	}
	if got := a.Stats().RehashDropped; got != 0 {
		t.Errorf("%d upgrades were dropped", got)
	}

	t.Run("inverse: a credential without the sk- prefix is refused before any lookup", func(t *testing.T) {
		// The stored rows are hex digests, and hex does not start with "sk-".
		// Without the gate, a leaked digest could be replayed as the credential
		// it stands for.
		if _, err := a.Authenticate(ctx, row.TokenHash); err == nil {
			t.Fatal("a stored digest was accepted as a credential")
		}
	})
}
