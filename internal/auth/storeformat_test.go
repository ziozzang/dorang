package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Three token stores exist on a developer machine that runs the vendors' own
// CLIs, and none of them has the shape internal/auth was written against: two
// nest their tokens one level down and one carries no expiry field at all. A
// credential that cannot read them is a credential nobody can point at anything,
// which is the same "unreachable subsystem" this whole change is about.
//
// The fixtures here are SHAPES, not copies. No value from a real store is in
// this repository and no path to one is either: what is asserted is the key
// layout, which is what the code has to understand.

// jwtWithExp builds a JWT-shaped string whose payload carries exp. The signature
// is nonsense on purpose — nothing verifies it, and a test that supplied a real
// one would be asserting something dorang deliberately does not do.
func jwtWithExp(t *testing.T, exp time.Time) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	head := enc([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body := enc([]byte(fmt.Sprintf(`{"sub":"fixture","exp":%d}`, exp.Unix())))
	return head + "." + body + ".not-a-signature"
}

// TestCodexStoreShapeIsReadable covers ~/.codex/auth.json's layout: tokens
// nested under "tokens", no expiry field anywhere, and an OPENAI_API_KEY of null
// — that last one being why the account is OAuth-only and why this mattered.
func TestCodexStoreShapeIsReadable(t *testing.T) {
	exp := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	access := jwtWithExp(t, exp)
	store := fmt.Sprintf(`{
	  "auth_mode": "chatgpt",
	  "OPENAI_API_KEY": null,
	  "tokens": {
	    "id_token": "test-fixture-id-token",
	    "access_token": %q,
	    "refresh_token": "test-fixture-codex-refresh-token",
	    "account_id": "test-fixture-account-id"
	  },
	  "last_refresh": "2026-07-27T00:00:00.000000000Z"
	}`, access)

	f := FormatCodex.Fields(TokenFields{})
	tok, err := decodeToken([]byte(store), f)
	if err != nil {
		t.Fatalf("decodeToken: %v", err)
	}
	if tok.Access != access {
		t.Errorf("access token not read out of tokens.access_token")
	}
	if tok.Refresh != "test-fixture-codex-refresh-token" {
		t.Errorf("refresh token not read out of tokens.refresh_token")
	}
	if tok.AccountID != "test-fixture-account-id" {
		t.Errorf("account id = %q, want it read out of tokens.account_id", tok.AccountID)
	}
	// This is the load-bearing one. The store has no expiry field, so without
	// the JWT claim this credential is never due for refresh and the whole of
	// §11.2b's "ahead of expiry, not on failure" reduces to the 401 fallback.
	if !tok.ExpiresAt.Equal(exp) {
		t.Fatalf("expiry = %v, want %v read from the access token's exp claim", tok.ExpiresAt, exp)
	}
	if tok.NeedsRefresh(exp.Add(-2*time.Minute), 5*time.Minute) != true {
		t.Errorf("a codex token two minutes inside a five-minute margin is not due")
	}
}

// TestClaudeStoreShapeIsReadable covers ~/.claude/.credentials.json: one nested
// object, camel-cased keys, expiry in unix milliseconds.
func TestClaudeStoreShapeIsReadable(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	store := fmt.Sprintf(`{
	  "claudeAiOauth": {
	    "accessToken": "test-fixture-claude-access-token",
	    "refreshToken": "test-fixture-claude-refresh-token",
	    "expiresAt": %d,
	    "refreshTokenExpiresAt": %d,
	    "scopes": ["user:inference", "user:profile"],
	    "subscriptionType": "max",
	    "rateLimitTier": "default_claude_max_20x"
	  }
	}`, exp.UnixMilli(), exp.Add(720*time.Hour).UnixMilli())

	f := FormatClaude.Fields(TokenFields{})
	tok, err := decodeToken([]byte(store), f)
	if err != nil {
		t.Fatalf("decodeToken: %v", err)
	}
	if tok.Access != "test-fixture-claude-access-token" || tok.Refresh != "test-fixture-claude-refresh-token" {
		t.Fatalf("tokens not read out of the nested object")
	}
	if !tok.ExpiresAt.Equal(exp) {
		t.Errorf("expiry = %v, want %v", tok.ExpiresAt, exp)
	}
	if tok.AccountID != "" {
		t.Errorf("account id = %q, want empty: this store carries none", tok.AccountID)
	}
}

// TestGeminiStoreShapeIsReadable covers ~/.gemini/oauth_creds.json: a Google
// token response written to disk verbatim, so flat and standard except that the
// expiry is expiry_date in unix milliseconds.
func TestGeminiStoreShapeIsReadable(t *testing.T) {
	exp := time.Now().Add(45 * time.Minute).UTC().Truncate(time.Millisecond)
	store := fmt.Sprintf(`{
	  "access_token": "test-fixture-gemini-access-token",
	  "refresh_token": "test-fixture-gemini-refresh-token",
	  "scope": "https://www.googleapis.com/auth/cloud-platform",
	  "token_type": "Bearer",
	  "id_token": "test-fixture-gemini-id-token",
	  "expiry_date": %d
	}`, exp.UnixMilli())

	f := FormatGemini.Fields(TokenFields{})
	tok, err := decodeToken([]byte(store), f)
	if err != nil {
		t.Fatalf("decodeToken: %v", err)
	}
	if tok.Access != "test-fixture-gemini-access-token" || tok.Refresh != "test-fixture-gemini-refresh-token" {
		t.Fatalf("tokens not read")
	}
	if !tok.ExpiresAt.Equal(exp) {
		t.Errorf("expiry = %v, want %v read from expiry_date", tok.ExpiresAt, exp)
	}
}

// TestAWriteBackPreservesEveryKeyTheVendorOwns is DESIGN §11.2b's "the store is
// shared with the vendor's own CLI, so writes must not corrupt it", asserted at
// the depth the real stores actually nest to. A flat implementation passes the
// old round-trip test and silently deletes `id_token` and `auth_mode` here.
func TestAWriteBackPreservesEveryKeyTheVendorOwns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	oldExp := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	original := fmt.Sprintf(`{
	  "auth_mode": "chatgpt",
	  "OPENAI_API_KEY": null,
	  "tokens": {
	    "id_token": "test-fixture-id-token",
	    "access_token": %q,
	    "refresh_token": "test-fixture-old-refresh",
	    "account_id": "test-fixture-account-id"
	  },
	  "last_refresh": "2026-07-27T00:00:00.000000000Z"
	}`, jwtWithExp(t, oldExp))
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &fileStore{path: path, fields: FormatCodex.Fields(TokenFields{})}
	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	newExp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	next := Token{
		Access:    jwtWithExp(t, newExp),
		Refresh:   "test-fixture-new-refresh",
		ExpiresAt: newExp,
	}.merge(loaded)
	if err := s.Save(next); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the rewritten store is not JSON: %v", err)
	}
	for _, key := range []string{"auth_mode", "OPENAI_API_KEY", "last_refresh", "tokens"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the write dropped top-level key %q, which the vendor's CLI reads", key)
		}
	}
	tokens, _ := got["tokens"].(map[string]any)
	if tokens == nil {
		t.Fatalf("tokens is no longer an object")
	}
	if tokens["id_token"] != "test-fixture-id-token" {
		t.Errorf("the write dropped tokens.id_token")
	}
	if tokens["account_id"] != "test-fixture-account-id" {
		t.Errorf("the write dropped tokens.account_id")
	}
	if tokens["access_token"] != next.Access {
		t.Errorf("the new access token was not written to tokens.access_token")
	}
	if tokens["refresh_token"] != "test-fixture-new-refresh" {
		t.Errorf("the new refresh token was not written")
	}
	// The expiry came out of the access token. Writing a field of its own would
	// add a key the vendor's CLI never wrote, and the next reader would have two
	// answers to one question.
	if _, ok := got["expires_at"]; ok {
		t.Errorf("a jwt-derived expiry was written back as a field of its own")
	}
	if _, ok := tokens["expires_at"]; ok {
		t.Errorf("a jwt-derived expiry was written back inside tokens")
	}

	// And it reads back as what was written.
	back, err := s.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if back.Access != next.Access || back.Refresh != "test-fixture-new-refresh" {
		t.Errorf("the store does not read back what was written")
	}
	if !back.ExpiresAt.Equal(newExp) {
		t.Errorf("expiry after reload = %v, want %v", back.ExpiresAt, newExp)
	}
}

// TestAMalformedJWTIsNoExpiryRatherThanAnError: a store whose access token is
// not a JWT is a store with no expiry, which [Token.NeedsRefresh] already
// answers. Reporting it would mean reporting on the shape of a token, and the
// report is the one place a token must never be.
func TestAMalformedJWTIsNoExpiryRatherThanAnError(t *testing.T) {
	const planted = "not-a-jwt-but-still-a-secret-token"
	f := FormatCodex.Fields(TokenFields{})
	tok, err := decodeToken([]byte(`{"tokens":{"access_token":"`+planted+`"}}`), f)
	if err != nil {
		t.Fatalf("decodeToken: %v", err)
	}
	if !tok.ExpiresAt.IsZero() {
		t.Errorf("expiry = %v, want none", tok.ExpiresAt)
	}
	if tok.Access != planted {
		t.Errorf("the access token was not read")
	}
	if tok.NeedsRefresh(time.Now(), time.Hour) {
		t.Errorf("a token with no expiry is due for refresh, which is a refresh loop")
	}
}

// TestAnOverrideBeatsTheFormat: a vendor renaming a key in a point release must
// not require a new dorang build.
func TestAnOverrideBeatsTheFormat(t *testing.T) {
	f := FormatGemini.Fields(TokenFields{AccessToken: "token.value"})
	if f.AccessToken != "token.value" {
		t.Fatalf("access token field = %q, want the override", f.AccessToken)
	}
	if f.ExpiresAt != "expiry_date" {
		t.Errorf("expires_at field = %q, want the format's own to survive", f.ExpiresAt)
	}
	tok, err := decodeToken([]byte(`{"token":{"value":"test-fixture-access"},"expiry_date":0}`), f)
	if err != nil {
		t.Fatalf("decodeToken: %v", err)
	}
	if tok.Access != "test-fixture-access" {
		t.Errorf("a dotted override was not followed")
	}
}

// TestUnknownFormatNamesTheOnesThatExist. A configuration error that does not
// say what the valid values are sends an operator to the source.
func TestUnknownFormatNamesTheOnesThatExist(t *testing.T) {
	_, err := ParseStoreFormat("openai")
	if err == nil {
		t.Fatal("an unknown format was accepted")
	}
	for _, want := range StoreFormats() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
	if f, err := ParseStoreFormat(""); err != nil || f != FormatGeneric {
		t.Errorf("empty = (%q, %v), want the generic format", f, err)
	}
}
