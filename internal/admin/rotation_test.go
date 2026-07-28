package admin

import (
	"net/http"
	"testing"
)

// DESIGN §11.2c and §11.6 on the wire.

// rotationHarness issues one key and returns the harness, the key id and its
// plaintext.
func rotationHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	h := newHarness(t)
	rec := h.do(http.MethodPost, "/key/generate", map[string]any{
		"key_alias": "the-key", "max_budget": 10, "tier": "commercial",
		"rpm_limit": 60, "models": []string{"chat"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body.String())
	}
	body := h.decode(rec)
	return h, body["token_id"].(string), body["key"].(string)
}

func TestRotateReturnsTheNewSecretOnceAndReportsTheOldOnesExpiry(t *testing.T) {
	h, id, first := rotationHarness(t)

	rec := h.do(http.MethodPost, "/key/rotate", map[string]any{"key_id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	body := h.decode(rec)

	second, _ := body["key"].(string)
	if second == "" {
		t.Fatal("the rotation returned no secret")
	}
	if second == first {
		t.Fatal("the rotation returned the same secret")
	}
	if body["token_id"] != id {
		t.Errorf("the key id changed to %v; the identity is the durable half", body["token_id"])
	}
	// The expiry of the OLD secret is the point of the response.
	if body["previous_expires_at"] == nil {
		t.Fatal("the response does not say when the old secret expires; a caller would " +
			"have to infer it from a policy plus a clock")
	}
	if body["previous_secret_id"] == "" {
		t.Error("the response does not name the secret that was replaced, so it cannot be " +
			"matched against the ledger")
	}
	if got, _ := body["generation"].(float64); got != 2 {
		t.Errorf("generation = %v, want 2", body["generation"])
	}

	// Returned ONCE. Nothing else on the surface will show it again.
	for _, path := range []string{"/key/info?key_id=" + id, "/key/list", "/key/secrets?key_id=" + id} {
		r := h.do(http.MethodGet, path, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, r.Code)
		}
		if bodyContains(r.Body.String(), second) {
			t.Fatalf("%s echoed the secret back", path)
		}
	}
	// And the audit trail records the rotation without the secret.
	for _, e := range h.store.auditLog() {
		if bodyContains(e.Before, second) || bodyContains(e.After, second) ||
			bodyContains(e.Before, first) || bodyContains(e.After, first) {
			t.Fatal("the audit trail recorded a plaintext credential")
		}
	}
}

func TestRotationDoesNotResetAnyLimit(t *testing.T) {
	// A rotation that also reset the limits would be a re-provisioning, and an
	// operator facing that will put it off — which is how a five-year-old
	// secret happens.
	h, id, _ := rotationHarness(t)
	before := h.decode(h.do(http.MethodGet, "/key/info?key_id="+id, nil))["key"].(map[string]any)

	if rec := h.do(http.MethodPost, "/key/rotate", map[string]any{"key_id": id}); rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	after := h.decode(h.do(http.MethodGet, "/key/info?key_id="+id, nil))["key"].(map[string]any)

	// key_name and updated_at legitimately change: the label is derived from
	// the secret, and the row was written.
	for _, field := range []string{
		"token_id", "key_alias", "tier", "max_budget", "soft_budget", "budget_duration",
		"rpm_limit", "tpm_limit", "max_parallel_requests", "priority_class",
		"models", "allowed_routes", "tags", "spend", "user_id", "team_id",
		"blocked", "expires", "created_at",
	} {
		if !jsonEqual(before[field], after[field]) {
			t.Errorf("rotation changed %s: %v -> %v", field, before[field], after[field])
		}
	}
	if before["key_name"] == after["key_name"] {
		t.Error("the label did not change; nothing was rotated")
	}
}

func TestSecretsListShowsBothDuringTheGraceAndSaysWhichIsCurrent(t *testing.T) {
	h, id, _ := rotationHarness(t)
	if rec := h.do(http.MethodPost, "/key/rotate",
		map[string]any{"key_id": id, "grace": "24h"}); rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}

	body := h.decode(h.do(http.MethodGet, "/key/secrets?key_id="+id, nil))
	secrets, _ := body["secrets"].([]any)
	if len(secrets) != 2 {
		t.Fatalf("the key has %d secrets during a grace period, want 2", len(secrets))
	}
	var current, grace map[string]any
	for _, s := range secrets {
		m := s.(map[string]any)
		if m["current"] == true {
			current = m
		} else {
			grace = m
		}
	}
	if current == nil || grace == nil {
		t.Fatalf("the list does not distinguish the current secret: %v", secrets)
	}
	if current["status"] != "current" || grace["status"] != "grace" {
		t.Errorf("statuses are %v/%v, want current/grace", current["status"], grace["status"])
	}
	if grace["expires_at"] == nil {
		t.Error("the superseded secret does not report when it stops working")
	}
	if current["expires_at"] != nil {
		t.Error("the current secret was given an expiry of its own")
	}
	// Nothing in the list is a verifier.
	for _, s := range secrets {
		m := s.(map[string]any)
		for _, forbidden := range []string{"lookup", "token_hash", "digest", "key", "secret"} {
			if _, present := m[forbidden]; present {
				t.Errorf("the secret list exposes %q", forbidden)
			}
		}
	}
}

func TestCuttingTheGraceIsImmediateAndSparesTheCurrentSecret(t *testing.T) {
	h, id, _ := rotationHarness(t)
	if rec := h.do(http.MethodPost, "/key/rotate",
		map[string]any{"key_id": id, "grace": "30d"}); rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d", rec.Code)
	}

	rec := h.do(http.MethodPost, "/key/rotate/cut", map[string]any{"key_id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("cut: %d %s", rec.Code, rec.Body.String())
	}
	body := h.decode(rec)
	if got, _ := body["cut"].(float64); got != 1 {
		t.Fatalf("cut %v secrets, want 1", body["cut"])
	}
	secrets, _ := body["secrets"].([]any)
	var revoked, current int
	for _, s := range secrets {
		m := s.(map[string]any)
		switch m["status"] {
		case "revoked":
			revoked++
			if m["revoked_at"] == nil {
				t.Error("a cut secret does not say when it was cut; 'the grace ran out' and " +
					"'an operator ended it' have to stay distinguishable")
			}
		case "current":
			current++
		}
	}
	if revoked != 1 || current != 1 {
		t.Fatalf("after the cut: %d revoked, %d current; want 1 and 1", revoked, current)
	}
}

func TestARotationGraceOfZeroCutsImmediately(t *testing.T) {
	h, id, _ := rotationHarness(t)
	rec := h.do(http.MethodPost, "/key/rotate", map[string]any{"key_id": id, "grace": "0"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	body := h.decode(rec)
	secrets, _ := body["secrets"].([]any)
	for _, s := range secrets {
		m := s.(map[string]any)
		if m["current"] == true {
			continue
		}
		if m["status"] != "expired" {
			t.Errorf("with grace 0 the old secret is %q, want expired", m["status"])
		}
	}
}

func TestRotateRefusesAPlaintextCredentialAsAParameter(t *testing.T) {
	h, _, tok := rotationHarness(t)
	rec := h.do(http.MethodPost, "/key/rotate", map[string]any{"key": tok})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := h.decode(rec)["error"].(map[string]any)["code"]; code != "secret_in_request" {
		t.Errorf("code = %v, want secret_in_request", code)
	}
}

func TestPendIsDistinctFromBlockAndReleasedInOneCall(t *testing.T) {
	h, id, _ := rotationHarness(t)

	rec := h.do(http.MethodPost, "/key/pend", map[string]any{"key_id": id, "reason": "token guard"})
	if rec.Code != http.StatusOK {
		t.Fatalf("pend: %d %s", rec.Code, rec.Body.String())
	}
	k := h.decode(rec)["key"].(map[string]any)
	if k["pended"] != true {
		t.Fatal("the key does not report itself as pended")
	}
	if k["blocked"] == true {
		t.Error("a pend set the blocked flag; the reversible control has to stay distinguishable")
	}
	if k["pend_reason"] != "token guard" {
		t.Errorf("pend_reason = %v", k["pend_reason"])
	}
	if k["pended_at"] == nil {
		t.Error("the pend recorded no instant")
	}

	// ONE call, and one argument.
	rec = h.do(http.MethodPost, "/key/release", map[string]any{"key_id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("release: %d %s", rec.Code, rec.Body.String())
	}
	k = h.decode(rec)["key"].(map[string]any)
	if k["pended"] != false || k["pended_at"] != nil {
		t.Fatalf("the key is still pended after the release: %v", k)
	}
}

func TestTheTierIsSettableOnlyThroughTheOperatorSurface(t *testing.T) {
	// §10.5, applied to tiers: an operator can grant one, a caller cannot claim
	// one. Every route in this package is behind the administrative credential,
	// so the assertion that matters is that the tier is REACHABLE here and
	// carried, and that an unauthenticated caller reaches none of it.
	h, id, _ := rotationHarness(t)

	k := h.decode(h.do(http.MethodGet, "/key/info?key_id="+id, nil))["key"].(map[string]any)
	if k["tier"] != "commercial" {
		t.Fatalf("tier = %v, want the one the operator assigned", k["tier"])
	}

	rec := h.do(http.MethodPost, "/key/update", map[string]any{"key_id": id, "tier": "unlimited"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if got := h.decode(rec)["key"].(map[string]any)["tier"]; got != "unlimited" {
		t.Errorf("tier = %v after an operator granted one", got)
	}

	// Without the administrative credential, none of it is reachable.
	rec = h.do(http.MethodPost, "/key/update",
		map[string]any{"key_id": id, "tier": "unlimited"}, asToken(""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated tier grant answered %d, want 401", rec.Code)
	}
}

func TestRotationRoutesAnswer501WhenTheStoreCannotRotate(t *testing.T) {
	// A named 501 rather than a regeneration: answering /key/rotate with an
	// outright replacement would cut the caller off at the instant the operator
	// asked for a grace period, under the name of the route that promised one.
	h := newHarness(t, func(c *Config) { c.Keys = plainKeyStore{c.Keys} })
	for _, path := range []string{"/key/rotate", "/key/rotate/cut", "/key/pend", "/key/release"} {
		rec := h.do(http.MethodPost, path, map[string]any{"key_id": "k1"})
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s answered %d, want 501", path, rec.Code)
		}
		if code := h.decode(rec)["error"].(map[string]any)["code"]; code != CodeDependencyOff {
			t.Errorf("%s: code = %v, want %s", path, code, CodeDependencyOff)
		}
	}
}

// plainKeyStore is a KeyStore that implements neither optional half, which is
// what a deployment predating rotation looks like.
type plainKeyStore struct{ KeyStore }

func bodyContains(haystack, needle string) bool {
	return needle != "" && len(needle) > 4 && contains(haystack, needle)
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

// jsonEqual compares two decoded JSON values structurally.
func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k := range av {
			if !jsonEqual(av[k], bv[k]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
