package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/store"
)

// `/key/regenerate` actually stops the old secret from authenticating.
//
// This is the incident path — an operator has a leaked `sk-` key in hand — and
// it had a shape that made it worse than useless. The store method behind it
// wrote `api_keys.lookup`, `.token_hash` and `.hash_scheme` directly, and those
// three columns are the DENORMALIZED copy: authentication resolves through
// api_key_secrets (store.resolveKeyQuery). So the leaked secret went on
// authenticating, the freshly minted one was unknown to the gateway, and the
// operator had a 200 telling them the credential had been replaced.
//
// The assertion is therefore about the LOOKUP PATH and not about the columns:
// what matters is what a request presenting each token resolves to.
func TestRegeneratingAKeyRetiresTheOldSecret(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, nil)
	ks := &adminKeyStore{st: st}

	const leaked = "sk-leaked-secret-0000" // pragma: allowlist secret — test fixture
	k := &store.APIKey{KeyAlias: "leaked"}
	if err := st.NewAPIKeyFromToken(leaked, k); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, sec, err := st.ResolveKeyByLookup(ctx, store.KeyLookup(leaked)); err != nil {
		t.Fatalf("the leaked key does not authenticate before the regeneration: %v", err)
	} else if sec.Retired(time.Now()) {
		t.Fatal("the leaked key was already retired before the regeneration")
	}

	const replacement = "sk-replacement-secret-1" // pragma: allowlist secret — test fixture
	fresh := &store.APIKey{}
	if err := st.NewAPIKeyFromToken(replacement, fresh); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err := ks.ReplaceVerifier(ctx, k.ID, admin.Verifier{
		Lookup:     fresh.Lookup,
		TokenHash:  fresh.TokenHash,
		HashScheme: string(fresh.HashScheme),
	}, fresh.KeyLabel, now)
	if err != nil {
		t.Fatalf("ReplaceVerifier: %v", err)
	}

	// The leaked secret: either its row is gone or it is retired. Anything else
	// means the regeneration regenerated nothing.
	_, old, err := st.ResolveKeyByLookup(ctx, store.KeyLookup(leaked))
	switch {
	case err == nil && !old.Retired(time.Now()):
		t.Fatal("the leaked secret still authenticates after /key/regenerate: " +
			"the verifier was written to the denormalized copy on api_keys and the " +
			"row authentication actually reads was never touched")
	case err != nil && !errors.Is(err, store.ErrNotFound):
		t.Fatalf("resolving the leaked lookup: %v", err)
	}

	// And the replacement authenticates, against the same durable key id, with
	// every authorization field intact — that is the point of rotating rather
	// than reissuing.
	got, sec, err := st.ResolveKeyByLookup(ctx, store.KeyLookup(replacement))
	if err != nil {
		t.Fatalf("the replacement secret does not authenticate: %v", err)
	}
	if got.ID != k.ID {
		t.Errorf("the replacement resolved to key %q, want the durable id %q", got.ID, k.ID)
	}
	if sec.Retired(time.Now()) {
		t.Error("the replacement secret was minted already retired")
	}
	if !sec.Current {
		t.Error("the replacement secret is not the current one")
	}
	if sec.Generation <= 1 {
		t.Errorf("generation = %d, want a rotation past the issued secret", sec.Generation)
	}
}

// A planned rotation keeps the old secret alive for its grace period, which is
// the whole difference from the regeneration above — and it is a parameter,
// not a second write path.
func TestRotatingAKeyKeepsTheOldSecretForItsGrace(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, nil)
	ks := &adminKeyStore{st: st}

	const first = "sk-first-secret-000000" // pragma: allowlist secret — test fixture
	k := &store.APIKey{KeyAlias: "rolling"}
	if err := st.NewAPIKeyFromToken(first, k); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}

	const second = "sk-second-secret-00000" // pragma: allowlist secret — test fixture
	fresh := &store.APIKey{}
	if err := st.NewAPIKeyFromToken(second, fresh); err != nil {
		t.Fatal(err)
	}
	res, err := ks.Rotate(ctx, k.ID, admin.Verifier{
		Lookup:     fresh.Lookup,
		TokenHash:  fresh.TokenHash,
		HashScheme: string(fresh.HashScheme),
	}, fresh.KeyLabel, time.Hour, time.Now().UTC())
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if res.PreviousExpiresAt.IsZero() {
		t.Error("the caller was not told when the old secret stops working, " +
			"which is the one number it cannot infer")
	}

	_, old, err := st.ResolveKeyByLookup(ctx, store.KeyLookup(first))
	if err != nil {
		t.Fatalf("the previous secret was cut by a rotation with an hour of grace: %v", err)
	}
	if old.Retired(time.Now()) {
		t.Fatal("the previous secret was retired immediately despite an hour of grace: " +
			"a client that has not rolled yet is cut off")
	}

	// EndGrace is how an operator closes the window early.
	if n, err := ks.EndGrace(ctx, k.ID, time.Now().UTC()); err != nil {
		t.Fatalf("EndGrace: %v", err)
	} else if n == 0 {
		t.Fatal("EndGrace cut nothing")
	}
	if _, old, err := st.ResolveKeyByLookup(ctx, store.KeyLookup(first)); err == nil &&
		!old.Retired(time.Now()) {
		t.Fatal("the previous secret survived EndGrace")
	}
	if secrets, err := ks.ListSecrets(ctx, k.ID); err != nil {
		t.Fatalf("ListSecrets: %v", err)
	} else if len(secrets) < 2 {
		t.Errorf("ListSecrets returned %d secrets; the retired one has to stay "+
			"visible or an operator cannot answer \"did the client roll?\"", len(secrets))
	}
}

// A pend is refused-and-reversible, and it reaches the authorization envelope
// the request path enforces (§11.6).
func TestPendAndReleaseReachTheAuthorizationEnvelope(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, nil)
	ks := &adminKeyStore{st: st}

	const token = "sk-pendable-key-00000" // pragma: allowlist secret — test fixture
	k := &store.APIKey{KeyAlias: "pendable"}
	if err := st.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}

	if err := ks.Pend(ctx, k.ID, "token guard: burst", time.Now().UTC()); err != nil {
		t.Fatalf("Pend: %v", err)
	}
	row, err := st.GetAPIKey(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Pended() {
		t.Fatal("the key was not pended")
	}
	if row.Blocked {
		t.Error("a pend set blocked: the two are different judgements and a caller " +
			"who cannot tell them apart cannot tell an outage from a policy")
	}

	if err := ks.Release(ctx, k.ID, time.Now().UTC()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	row, err = st.GetAPIKey(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Pended() {
		t.Fatal("the pend survived a release")
	}
}
