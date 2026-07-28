package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The central promise of §2.4: a generated key is shown exactly once, and no
// later read can produce it or any part of it.
func TestGeneratedKeyIsReturnedOnceAndNeverAgain(t *testing.T) {
	h := newHarness(t)
	id, token := h.newKey(map[string]any{"key_alias": "ci", "max_budget": 25})

	if !strings.HasPrefix(token, keyPrefix) {
		t.Fatalf("issued token %q lacks the conventional prefix", token)
	}

	// Every subsequent read of this key, through every endpoint that returns
	// one, must be free of the secret — and free of any suffix of it, which is
	// the specific mistake §2.4 records.
	reads := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"info", http.MethodPost, "/key/info", map[string]any{"key_id": id}},
		{"info-get", http.MethodGet, "/key/info?key_id=" + id, nil},
		{"list", http.MethodGet, "/key/list", nil},
		{"update", http.MethodPost, "/key/update", map[string]any{"key_id": id, "key_alias": "ci2"}},
		{"block", http.MethodPost, "/key/block", map[string]any{"key_id": id}},
		{"unblock", http.MethodPost, "/key/unblock", map[string]any{"key_id": id}},
	}
	for _, r := range reads {
		rec := h.do(r.method, r.path, r.body)
		h.expectStatus(rec, http.StatusOK)
		assertNoSecret(t, r.name, rec.Body.String(), token)
	}

	// The audit trail is written from the same view type, so it cannot carry
	// what the API would not.
	for _, e := range h.store.auditLog() {
		assertNoSecret(t, "audit.before", e.Before, token)
		assertNoSecret(t, "audit.after", e.After, token)
	}

	// And the store never received the plaintext: only a lookup and a digest.
	v := h.store.verifierFor(id)
	if v.Lookup == "" || v.TokenHash == "" {
		t.Fatalf("no verifier stored for %s", id)
	}
	if strings.Contains(v.Lookup, token) || strings.Contains(v.TokenHash, token) {
		t.Fatalf("the plaintext key was stored")
	}
}

// assertNoSecret fails if a response carries the token or a meaningful suffix
// of it. The suffix check is the point: §2.4 records a foreign system whose
// display column stored the key's trailing characters.
func assertNoSecret(t *testing.T, where, body, token string) {
	t.Helper()
	if strings.Contains(body, token) {
		t.Fatalf("%s leaked the whole key", where)
	}
	if len(token) > 8 {
		if suffix := token[len(token)-8:]; strings.Contains(body, suffix) {
			t.Fatalf("%s leaked the key's trailing characters (%q) — the exact pattern §2.4 forbids",
				where, suffix)
		}
	}
}

// The response type that carries a secret is a separate type, and the shared
// view type has no field that could hold one. That is checked structurally, so
// that a later edit adding such a field fails here rather than in production.
func TestKeyViewHasNoSecretBearingField(t *testing.T) {
	buf, err := json.Marshal(viewKey(&Key{ID: "k", KeyLabel: "dk-abcdef01"}))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"key", "token", "token_hash", "lookup", "secret", "hashed_token"} {
		if _, ok := m[forbidden]; ok {
			t.Errorf("keyView exposes %q; the shared view must not be able to carry a credential", forbidden)
		}
	}
	if _, ok := m["key_name"]; !ok {
		t.Error("keyView must carry the non-reversible display label as key_name")
	}
}

func TestKeyGenerateWritesAuditWithBeforeAndAfter(t *testing.T) {
	h := newHarness(t)
	id, _ := h.newKey(map[string]any{"key_alias": "ci"})

	e, ok := h.store.lastAudit()
	if !ok {
		t.Fatal("no audit row for key.generate")
	}
	if e.Action != "key.generate" || e.ObjectKind != "key" || e.ObjectID != id {
		t.Fatalf("audit row = %+v", e)
	}
	if e.ActorKind != "master" {
		t.Errorf("actor_kind = %q, want master", e.ActorKind)
	}
	if e.Before != "" {
		t.Errorf("creation recorded a before state: %q", e.Before)
	}
	if !strings.Contains(e.After, id) {
		t.Errorf("after state does not describe the created key: %q", e.After)
	}
	if e.UserAgent != "admin-test" {
		t.Errorf("user_agent = %q", e.UserAgent)
	}
}

func TestKeyUpdateAuditCarriesBothStates(t *testing.T) {
	h := newHarness(t)
	id, _ := h.newKey(map[string]any{"key_alias": "before-alias"})

	rec := h.do(http.MethodPost, "/key/update", map[string]any{
		"key_id": id, "key_alias": "after-alias", "max_budget": "12.5",
	})
	h.expectStatus(rec, http.StatusOK)

	e, _ := h.store.lastAudit()
	if e.Action != "key.update" {
		t.Fatalf("action = %q", e.Action)
	}
	if !strings.Contains(e.Before, "before-alias") {
		t.Errorf("before state missing the prior alias: %s", e.Before)
	}
	if !strings.Contains(e.After, "after-alias") {
		t.Errorf("after state missing the new alias: %s", e.After)
	}
	if !strings.Contains(e.After, "12.5") {
		t.Errorf("after state missing the new budget: %s", e.After)
	}
}

// A mutation that could not be audited is reported as exactly that, with its
// own code, because the caller's correct response is to reconcile rather than
// to retry.
func TestAuditFailureIsReportedDistinctly(t *testing.T) {
	h := newHarness(t)
	h.store.auditErr = errNoAudit{}

	rec := h.do(http.MethodPost, "/key/generate", map[string]any{})
	body := h.expectFault(rec, http.StatusInternalServerError, CodeAuditWriteFailed)
	msg := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "do not retry") {
		t.Errorf("message does not tell the caller not to retry: %q", msg)
	}
	if h.api.Metrics().AuditFailures != 1 {
		t.Errorf("audit failure was not counted")
	}
}

type errNoAudit struct{}

func (errNoAudit) Error() string { return "audit unavailable" }

func TestKeyRegenerateRotatesSecretAndKeepsAuthorization(t *testing.T) {
	h := newHarness(t)
	id, first := h.newKey(map[string]any{
		"key_alias":  "rotate-me",
		"models":     []string{"gpt-x"},
		"max_budget": 5,
	})
	beforeVerifier := h.store.verifierFor(id)

	rec := h.do(http.MethodPost, "/key/regenerate", map[string]any{"key_id": id})
	body := h.expectStatus(rec, http.StatusOK)

	second, _ := body["key"].(string)
	if second == "" || second == first {
		t.Fatalf("regenerate did not mint a new secret (%q -> %q)", first, second)
	}
	assertNoSecret(t, "regenerate", mustJSON(t, body["info"]), first)

	afterVerifier := h.store.verifierFor(id)
	if afterVerifier.Lookup == beforeVerifier.Lookup {
		t.Error("the lookup did not change, so the old secret still resolves")
	}

	// Rotation must not widen what the credential can do.
	info := h.expectStatus(h.do(http.MethodGet, "/key/info?key_id="+id, nil), http.StatusOK)
	key := info["key"].(map[string]any)
	models, _ := key["models"].([]any)
	if len(models) != 1 || models[0] != "gpt-x" {
		t.Errorf("model allow-list changed across rotation: %v", key["models"])
	}
	if key["max_budget"] != 5.0 {
		t.Errorf("budget changed across rotation: %v", key["max_budget"])
	}
}

func TestKeyBlockUnblockRoundTrip(t *testing.T) {
	h := newHarness(t)
	id, _ := h.newKey(nil)

	body := h.expectStatus(h.do(http.MethodPost, "/key/block", map[string]any{"key_id": id}), http.StatusOK)
	if body["key"].(map[string]any)["blocked"] != true {
		t.Fatal("block did not block")
	}
	body = h.expectStatus(h.do(http.MethodPost, "/key/unblock", map[string]any{"key_id": id}), http.StatusOK)
	if body["key"].(map[string]any)["blocked"] != false {
		t.Fatal("unblock did not unblock")
	}

	actions := []string{}
	for _, e := range h.store.auditLog() {
		actions = append(actions, e.Action)
	}
	want := []string{"key.generate", "key.block", "key.unblock"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Errorf("audit actions = %v, want %v", actions, want)
	}
}

func TestKeyDeleteRecordsWhatWasRemoved(t *testing.T) {
	h := newHarness(t)
	id, _ := h.newKey(map[string]any{"key_alias": "doomed"})

	body := h.expectStatus(h.do(http.MethodPost, "/key/delete",
		map[string]any{"keys": []string{id}}), http.StatusOK)
	if body["deleted"] != 1.0 {
		t.Fatalf("deleted = %v, want 1", body["deleted"])
	}
	e, _ := h.store.lastAudit()
	if !strings.Contains(e.Before, "doomed") {
		t.Errorf("the audit row does not record what was deleted: %s", e.Before)
	}
	h.expectFault(h.do(http.MethodGet, "/key/info?key_id="+id, nil), http.StatusNotFound, CodeNotFound)
}

// A plaintext credential as a request parameter is refused, with its own code.
// The incumbent identifies keys by their own secret; doing that puts live
// credentials into URLs and access logs.
func TestPlaintextKeyAsParameterIsRefused(t *testing.T) {
	h := newHarness(t)
	_, token := h.newKey(nil)

	rec := h.do(http.MethodPost, "/key/info", map[string]any{"key": token})
	h.expectFault(rec, http.StatusBadRequest, "secret_in_request")

	rec = h.do(http.MethodGet, "/key/info?key="+token, nil)
	h.expectFault(rec, http.StatusBadRequest, "secret_in_request")

	rec = h.do(http.MethodPost, "/key/delete", map[string]any{"keys": []string{token}})
	h.expectFault(rec, http.StatusBadRequest, "secret_in_request")
}

func TestKeyInfoRequiresAnIdentifier(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodGet, "/key/info", nil), http.StatusBadRequest, CodeInvalidRequest)
}

func TestKeyNotFoundIs404NotSilentEmpty(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodGet, "/key/info?key_id=missing", nil),
		http.StatusNotFound, CodeNotFound)
}

// Clearing a nullable limit is explicit. Null and absent are indistinguishable
// after decoding into a pointer, so the API does not pretend otherwise.
func TestClearRemovesALimitAndTyposAreRefused(t *testing.T) {
	h := newHarness(t)
	id, _ := h.newKey(map[string]any{"max_budget": 10, "rpm_limit": 60})

	body := h.expectStatus(h.do(http.MethodPost, "/key/update",
		map[string]any{"key_id": id, "clear": []string{"max_budget"}}), http.StatusOK)
	if body["key"].(map[string]any)["max_budget"] != nil {
		t.Fatalf("max_budget survived clear: %v", body["key"].(map[string]any)["max_budget"])
	}
	if body["key"].(map[string]any)["rpm_limit"] != 60.0 {
		t.Errorf("clear removed a limit it was not asked to remove")
	}

	h.expectFault(h.do(http.MethodPost, "/key/update",
		map[string]any{"key_id": id, "clear": []string{"max_bugdet"}}),
		http.StatusBadRequest, CodeInvalidRequest)
}

func TestDurationAndExpiresAreMutuallyExclusive(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodPost, "/key/generate", map[string]any{
		"duration": "30d", "expires": "2026-09-01T00:00:00Z",
	})
	h.expectFault(rec, http.StatusBadRequest, CodeInvalidRequest)
}

func TestDurationInDays(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodPost, "/key/generate", map[string]any{"duration": "30d"})
	body := h.expectStatus(rec, http.StatusOK)
	exp, _ := body["expires"].(string)
	if !strings.HasPrefix(exp, "2026-08-27") {
		t.Fatalf("expires = %q, want 30 days after the fixed clock", exp)
	}
}

func TestSoftBudgetMustNotExceedMax(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodPost, "/key/generate", map[string]any{
		"max_budget": 5, "soft_budget": 10,
	})
	h.expectFault(rec, http.StatusBadRequest, CodeInvalidRequest)
}

// Money is rendered from integer nano-units, so an amount that float64 cannot
// hold exactly still round-trips.
func TestMoneyIsExact(t *testing.T) {
	h := newHarness(t)
	id, _ := h.newKey(map[string]any{"max_budget": "0.123456789"})
	rec := h.do(http.MethodGet, "/key/info?key_id="+id, nil)
	if !strings.Contains(rec.Body.String(), `"max_budget":0.123456789`) {
		t.Fatalf("budget was not rendered exactly: %s", rec.Body.String())
	}
}

func TestMoneyBeyondNanoPrecisionIsRefused(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodPost, "/key/generate", map[string]any{"max_budget": "0.1234567891"})
	h.expectFault(rec, http.StatusBadRequest, CodeInvalidRequest)
}

func TestIssuingWithoutAPepperIsRefusedNotFudged(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Hasher = fakeHasher{broken: true} })
	rec := h.do(http.MethodPost, "/key/generate", map[string]any{})
	h.expectFault(rec, http.StatusNotImplemented, CodeDependencyOff)
	if len(h.store.keys) != 0 {
		t.Error("a key row was written despite hashing failing")
	}
}

func TestKeyListFilters(t *testing.T) {
	h := newHarness(t)
	h.newKey(map[string]any{"user_id": "u1"})
	h.newKey(map[string]any{"user_id": "u2"})

	body := h.expectStatus(h.do(http.MethodGet, "/key/list?user_id=u1", nil), http.StatusOK)
	keys := body["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("user filter returned %d keys, want 1", len(keys))
	}
	h.expectFault(h.do(http.MethodGet, "/key/list?limit=0", nil),
		http.StatusBadRequest, CodeInvalidRequest)
	h.expectFault(h.do(http.MethodGet, "/key/list?limit=99999", nil),
		http.StatusBadRequest, CodeInvalidRequest)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
