package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestKeyLookupIsDerivableFromTheLegacyDigest(t *testing.T) {
	// This identity is what lets an import carry a credential without ever
	// holding the plaintext, and what lets two hash schemes share one index:
	// lookup is a prefix of the legacy digest.
	const token = "sk-dorang-example-token" // pragma: allowlist secret — test fixture
	digest := HashLegacySHA256(token)
	if got, want := KeyLookup(token), digest[:32]; got != want {
		t.Fatalf("KeyLookup = %s, want the first 32 hex characters of sha256: %s", got, want)
	}
	sum := sha256.Sum256([]byte(token))
	if KeyLookup(token) != hex.EncodeToString(sum[:16]) {
		t.Fatal("KeyLookup is not sha256(token)[:16]")
	}
}

// TestLookupIsAlwaysOneQuery is the DESIGN 2.4 claim measured rather than
// asserted: whichever scheme a credential is stored under, resolving a bearer
// token costs exactly one statement.
func TestLookupIsAlwaysOneQuery(t *testing.T) {
	eachBackendCfg(t, func(c *Config) {
		c.Legacy = LegacyAuth{Enabled: true, Until: time.Now().AddDate(1, 0, 0)}
	}, func(t *testing.T, s *Store) {
		ctx := context.Background()

		const nativeToken = "sk-native-0123456789" // pragma: allowlist secret — test fixture
		native := &APIKey{ID: "k-native"}
		if err := s.NewAPIKeyFromToken(nativeToken, native); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertAPIKey(ctx, native); err != nil {
			t.Fatal(err)
		}

		const legacyToken = "sk-legacy-9876543210" // pragma: allowlist secret — test fixture
		legacy := &APIKey{
			ID:         "k-legacy",
			Lookup:     KeyLookup(legacyToken),
			TokenHash:  HashLegacySHA256(legacyToken),
			HashScheme: SchemeLegacySHA256,
		}
		if err := s.InsertAPIKey(ctx, legacy); err != nil {
			t.Fatal(err)
		}

		for _, tc := range []struct {
			name       string
			token      string
			wantScheme HashScheme
			wantRehash bool
		}{
			{"dorang_v1", nativeToken, SchemeDorangV1, false},
			{"legacy_sha256", legacyToken, SchemeLegacySHA256, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				before := s.StatementCount()
				auth, err := s.AuthenticateKey(ctx, tc.token)
				if err != nil {
					t.Fatalf("AuthenticateKey: %v", err)
				}
				if n := s.StatementCount() - before; n != 1 {
					t.Fatalf("AuthenticateKey issued %d statements, want exactly 1", n)
				}
				if auth.Key.HashScheme != tc.wantScheme {
					t.Fatalf("scheme = %s, want %s", auth.Key.HashScheme, tc.wantScheme)
				}
				if auth.NeedsRehash != tc.wantRehash {
					t.Fatalf("NeedsRehash = %v, want %v", auth.NeedsRehash, tc.wantRehash)
				}
			})
		}

		if _, err := s.AuthenticateKey(ctx, "sk-not-a-key"); !errors.Is(err, ErrBadCredential) { // pragma: allowlist secret — test fixture
			t.Fatalf("unknown token: %v, want ErrBadCredential", err)
		}
		if _, err := s.AuthenticateKey(ctx, "sk-native-0123456780"); !errors.Is(err, ErrBadCredential) { // pragma: allowlist secret — test fixture
			t.Fatalf("wrong token: %v, want ErrBadCredential", err)
		}
	})
}

func TestRehashOnUseUpgradesWithoutAFlagDay(t *testing.T) {
	eachBackendCfg(t, func(c *Config) {
		c.Legacy = LegacyAuth{Enabled: true, Until: time.Now().AddDate(1, 0, 0)}
	}, func(t *testing.T, s *Store) {
		ctx := context.Background()
		const token = "sk-upgrade-me" // pragma: allowlist secret — test fixture
		lookup := KeyLookup(token)
		if err := s.InsertAPIKey(ctx, &APIKey{
			ID: "k1", Lookup: lookup, TokenHash: HashLegacySHA256(token), HashScheme: SchemeLegacySHA256,
		}); err != nil {
			t.Fatal(err)
		}

		auth, err := s.AuthenticateKey(ctx, token)
		if err != nil || !auth.NeedsRehash {
			t.Fatalf("first auth: %v, needsRehash=%v", err, auth.NeedsRehash)
		}
		if err := s.RehashKey(ctx, "k1", token); err != nil {
			t.Fatal(err)
		}

		// The lookup value is unchanged, so nothing else has to move.
		after, err := s.GetAPIKey(ctx, "k1")
		if err != nil {
			t.Fatal(err)
		}
		if after.HashScheme != SchemeDorangV1 {
			t.Fatalf("scheme after rehash = %s", after.HashScheme)
		}
		if after.Lookup != lookup {
			t.Fatalf("lookup changed from %s to %s", lookup, after.Lookup)
		}
		want, _ := HashDorangV1(testPepper, token)
		if after.TokenHash != want {
			t.Fatal("token_hash is not the dorang_v1 HMAC")
		}

		auth, err = s.AuthenticateKey(ctx, token)
		if err != nil {
			t.Fatalf("auth after rehash: %v", err)
		}
		if auth.NeedsRehash {
			t.Fatal("still asking for a rehash after one")
		}

		// Repeating it is a no-op, not a race or an error.
		if err := s.RehashKey(ctx, "k1", token); err != nil {
			t.Fatalf("second RehashKey: %v", err)
		}
	})
}

func TestLegacyVerificationNeedsAnUnexpiredWindow(t *testing.T) {
	const token = "sk-legacy-window" // pragma: allowlist secret — test fixture
	cases := []struct {
		name   string
		legacy LegacyAuth
	}{
		{"disabled", LegacyAuth{}},
		{"enabled without a sunset date", LegacyAuth{Enabled: true}},
		{"past its sunset date", LegacyAuth{Enabled: true, Until: time.Now().AddDate(0, 0, -1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eachBackendCfg(t, func(c *Config) { c.Legacy = tc.legacy }, func(t *testing.T, s *Store) {
				ctx := context.Background()
				if err := s.InsertAPIKey(ctx, &APIKey{
					ID: "k1", Lookup: KeyLookup(token),
					TokenHash: HashLegacySHA256(token), HashScheme: SchemeLegacySHA256,
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AuthenticateKey(ctx, token); !errors.Is(err, ErrLegacyDisabled) {
					t.Fatalf("got %v, want ErrLegacyDisabled", err)
				}
			})
		})
	}
}

func TestExpiredAndBlockedKeysDoNotAuthenticate(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		mk := func(id, token string, mutate func(*APIKey)) {
			k := &APIKey{ID: id}
			if err := s.NewAPIKeyFromToken(token, k); err != nil {
				t.Fatal(err)
			}
			mutate(k)
			if err := s.InsertAPIKey(ctx, k); err != nil {
				t.Fatal(err)
			}
		}
		mk("expired", "sk-expired", func(k *APIKey) { k.ExpiresAt = s.now().Add(-time.Hour) }) // pragma: allowlist secret — test fixture
		mk("blocked", "sk-blocked", func(k *APIKey) { k.Blocked = true })                      // pragma: allowlist secret — test fixture
		mk("live", "sk-live", func(k *APIKey) { k.ExpiresAt = s.now().Add(time.Hour) })        // pragma: allowlist secret — test fixture

		if _, err := s.AuthenticateKey(ctx, "sk-expired"); !errors.Is(err, ErrKeyExpired) { // pragma: allowlist secret — test fixture
			t.Fatalf("expired key: %v, want ErrKeyExpired", err)
		}
		if _, err := s.AuthenticateKey(ctx, "sk-blocked"); !errors.Is(err, ErrKeyBlocked) { // pragma: allowlist secret — test fixture
			t.Fatalf("blocked key: %v, want ErrKeyBlocked", err)
		}
		if _, err := s.AuthenticateKey(ctx, "sk-live"); err != nil { // pragma: allowlist secret — test fixture
			t.Fatalf("live key: %v", err)
		}
	})
}

func TestDorangV1NeedsAPepper(t *testing.T) {
	if _, err := HashDorangV1(nil, "sk-x"); !errors.Is(err, ErrNoPepper) {
		t.Fatalf("got %v, want ErrNoPepper", err)
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			s := openStore(t, b, b.env(t), func(c *Config) { c.Pepper = nil })
			k := &APIKey{ID: "k1"}
			if err := s.NewAPIKeyFromToken("sk-x", k); !errors.Is(err, ErrNoPepper) {
				t.Fatalf("got %v, want ErrNoPepper", err)
			}
		})
	}
}

func TestKeyLabelRevealsNothingAboutTheSecret(t *testing.T) {
	const token = "sk-abcdefghijklmnopqrstuvwxyz-TAIL9Z" // pragma: allowlist secret — test fixture
	label := LabelFor(token)
	if strings.Contains(token, strings.TrimPrefix(label, "key-")) {
		t.Fatalf("label %q is a substring of the token", label)
	}
	for n := 3; n <= 8; n++ {
		tail := token[len(token)-n:]
		if strings.Contains(label, tail) {
			t.Fatalf("label %q contains the last %d characters of the token", label, n)
		}
	}
	if !strings.HasPrefix(label, "key-") || len(label) != len("key-")+8 {
		t.Fatalf("unexpected label shape: %q", label)
	}
}
