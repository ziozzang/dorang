package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/store"
)

// An imported legacy credential is rewritten as dorang_v1 the first time it is
// used, so the migration window of DESIGN §2.4 narrows on its own instead of
// ending in a flag day.
//
// The assertion is on the STORED ROW, not on a counter: rehash-on-use is only
// real if the row a second process would read has changed scheme. Before the
// adapter implemented auth.Rehasher, auth.New left the upgrade path switched
// off entirely and the row stayed legacy_sha256 forever.
func TestLegacyKeyIsUpgradedInPlaceOnSecondUse(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, func(c *store.Config) {
		c.Legacy = store.LegacyAuth{Enabled: true, Until: time.Now().Add(24 * time.Hour)}
	})

	// An imported row: the incumbent's unsalted digest, and a lookup derived
	// from it exactly as an importer that never sees the plaintext would.
	const token = "sk-legacy-import-token" // pragma: allowlist secret — test fixture
	legacy := &store.APIKey{
		ID:         "key-legacy",
		Lookup:     store.KeyLookup(token),
		TokenHash:  store.HashLegacySHA256(token),
		HashScheme: store.SchemeLegacySHA256,
		Source:     "imported",
	}
	if err := st.InsertAPIKey(ctx, legacy); err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}

	adapter := &authStore{st: st}
	if _, ok := any(adapter).(auth.Rehasher); !ok {
		t.Fatal("the store adapter does not implement auth.Rehasher, so auth.New " +
			"never enables rehash-on-use and DESIGN §2.4's migration never completes")
	}

	authn, err := auth.New(auth.Config{
		Pepper:      testPepper,
		NoMasterKey: true,
		Legacy:      auth.LegacyPolicy{Enabled: true, Until: time.Now().Add(24 * time.Hour)},
		RehashOnUse: true,
		Store:       adapter,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	defer authn.Close()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)

	// First use: the row is still legacy, and it verifies under legacy_sha256.
	if _, err := authn.AuthenticateHeader(ctx, hdr); err != nil {
		t.Fatalf("first authentication: %v", err)
	}

	waitFor(t, "the legacy row to be upgraded", func() bool {
		k, err := st.GetAPIKey(ctx, legacy.ID)
		return err == nil && k.HashScheme == store.SchemeDorangV1
	})

	row, err := st.GetAPIKey(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	wantDigest, err := store.HashDorangV1([]byte(testPepper), token)
	if err != nil {
		t.Fatal(err)
	}
	if row.TokenHash != wantDigest {
		t.Errorf("upgraded digest = %s, want HMAC-SHA256(pepper, token) = %s",
			row.TokenHash, wantDigest)
	}
	if row.Lookup != store.KeyLookup(token) {
		t.Errorf("the lookup changed under the upgrade: %s", row.Lookup)
	}

	// Second use: the row is dorang_v1 now and the same token still
	// authenticates. An upgrade that broke verification would be worse than no
	// upgrade at all.
	if _, err := authn.AuthenticateHeader(ctx, hdr); err != nil {
		t.Fatalf("second authentication, after the upgrade: %v", err)
	}

	// And the proof that the window has actually narrowed: with legacy
	// verification refused outright, the credential still works.
	strict, err := auth.New(auth.Config{
		Pepper:      testPepper,
		NoMasterKey: true,
		Legacy:      auth.LegacyPolicy{},
		Store:       &authStore{st: st},
	})
	if err != nil {
		t.Fatalf("auth.New (legacy disabled): %v", err)
	}
	defer strict.Close()
	if _, err := strict.AuthenticateHeader(ctx, hdr); err != nil {
		t.Fatalf("the upgraded credential does not verify with legacy disabled: %v", err)
	}
}

// The upgrade primitive refuses anything but the upgrade direction. A general
// digest setter would let a caller overwrite a live dorang_v1 credential.
func TestSetKeyDigestOnlyUpgrades(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, nil)

	const token = "sk-native-token" // pragma: allowlist secret — test fixture
	k := &store.APIKey{ID: "key-native"}
	if err := st.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}

	other, err := store.HashDorangV1([]byte(testPepper), "sk-a-different-token") // pragma: allowlist secret — test fixture
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKeyDigest(ctx, k.ID, k.Lookup, other, store.SchemeDorangV1); err != nil {
		t.Fatalf("SetKeyDigest on a dorang_v1 row: %v", err)
	}
	row, err := st.GetAPIKey(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.TokenHash != k.TokenHash {
		t.Error("a dorang_v1 row's digest was overwritten; the upgrade is not one-way")
	}

	if err := st.SetKeyDigest(ctx, k.ID, k.Lookup, other, store.SchemeLegacySHA256); err == nil {
		t.Error("SetKeyDigest accepted a downgrade to legacy_sha256")
	}
	if err := st.SetKeyDigest(ctx, k.ID, k.Lookup, "not-hex", store.SchemeDorangV1); err == nil {
		t.Error("SetKeyDigest accepted a digest that is not a hex sha256")
	}
}
