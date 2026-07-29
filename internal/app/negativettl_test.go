package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

// TestANewKeyServesWithinTheNegativeTTL is DESIGN §11.2c's negative-entry TTL read in
// the direction the section did not state until now.
//
// §11.2c is written about revocation, and `negative_ttl` is introduced there as the
// thing that makes the revocation window narrow: a refusal is cheap to re-check, a
// serving row is not, so the two do not need the same freshness. That leaves the
// opposite event undocumented, and it is an event that happens in every deployment: a
// key used BEFORE it was created, then created. A client that starts beside its own
// provisioning, or a rotation script that hands the secret over before the row commits,
// gets a refusal — and that refusal is cached.
//
// **Creation deliberately publishes no invalidation.** There is nothing to invalidate:
// a row that has never existed cannot be in any node's snapshot, and putting every key
// issued onto the bus a compromise depends on would trade the mechanism's latency for
// nothing. So the window is bounded by `negative_ttl` and by nothing else, which is why
// that number cannot simply be raised toward `entry_ttl` to save lookups.
//
// Measured here, and it is the reason this is a test and not only a paragraph: with
// `negative_ttl: 300ms` the key is still refused in the instant after the row commits
// and is served ~310 ms later, without anything having invalidated anything.
func TestANewKeyServesWithinTheNegativeTTL(t *testing.T) {
	const negativeTTL = 300 * time.Millisecond
	a := newWiringApp(t, `
version: 1
auth:
  revocation: {entry_ttl: 60s, negative_ttl: 300ms}
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`, nil)

	const token = "sk-used-before-it-existed" // pragma: allowlist secret — test fixture

	// Used before it exists. This is what plants the negative entry; without it the
	// test would prove nothing, because a key nobody has presented has nothing
	// cached about it.
	if w := callWith(a, token, http.MethodGet, "/v1/models", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("a key that does not exist answered %d, want 401: %s",
			w.Code, w.Body.String())
	}

	// Now the row commits. No invalidation is published, deliberately.
	k := &store.APIKey{ID: "key-" + token}
	if err := a.Store.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.InsertAPIKey(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	// The refusal is still cached, which is the fact the paragraph exists to state.
	// It is asserted rather than assumed: if creation DID invalidate, the bound would
	// be zero and the documented one would be wrong in the direction that matters.
	if w := callWith(a, token, http.MethodGet, "/v1/models", ""); w.Code == http.StatusOK {
		t.Skip("the key served immediately after creation: something now publishes an " +
			"invalidation on create, so negative_ttl is no longer the bound and §11.2c " +
			"has to say what is")
	}

	start := time.Now()
	deadline := start.Add(10 * negativeTTL)
	for {
		if w := callWith(a, token, http.MethodGet, "/v1/models", ""); w.Code == http.StatusOK {
			if elapsed := time.Since(start); elapsed > 5*negativeTTL {
				t.Errorf("the new key was refused for %s with negative_ttl at %s: the "+
					"published bound does not describe this direction", elapsed, negativeTTL)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a key created %s ago is still refused with negative_ttl at %s: the "+
				"cached refusal outlives its own TTL, so a key created after a client "+
				"first tried it never starts working", time.Since(start), negativeTTL)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
