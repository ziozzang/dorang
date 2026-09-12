package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeServiceAccount renders a key file in Google's shape for a fresh key.
func writeServiceAccount(t *testing.T, key *rsa.PrivateKey, tokenURL string) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	doc := map[string]string{
		"type": "service_account", "project_id": "proj", "private_key_id": "kid-1",
		"private_key": pemText, "client_email": "svc@proj.iam.gserviceaccount.com",
		"token_uri": tokenURL,
	}
	b, _ := json.Marshal(doc)
	p := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeGoogleToken verifies the assertion the way Google would — signature,
// issuer, audience, scope — and answers a token when it holds.
func fakeGoogleToken(t *testing.T, pub *rsa.PublicKey, calls *int) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}
		parts := strings.Split(r.Form.Get("assertion"), ".")
		if len(parts) != 3 {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
			http.Error(w, `{"error":"invalid_grant","error_description":"bad signature"}`, http.StatusBadRequest)
			return
		}
		claimsRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		_ = json.Unmarshal(claimsRaw, &claims)
		if claims["iss"] != "svc@proj.iam.gserviceaccount.com" || claims["aud"] != srv.URL ||
			claims["scope"] != DefaultServiceAccountScope {
			http.Error(w, `{"error":"invalid_grant","error_description":"claims"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ya29.minted","expires_in":3599,"token_type":"Bearer"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A service-account key mints a bearer that the token endpoint accepts.
//
// The endpoint here checks what Google checks: an RS256 signature by the key
// in the file, the issuer, the audience and the scope. What comes back is a
// token with the expiry the endpoint stated, and the account is the key's
// client email.
func TestAServiceAccountKeyMintsABearer(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	srv := fakeGoogleToken(t, &key.PublicKey, &calls)
	path := writeServiceAccount(t, key, srv.URL)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	r, err := NewServiceAccountRefresher(ServiceAccountConfig{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := r.Refresh(context.Background(), Token{Refresh: "kid-1"})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.Access != "ya29.minted" || tok.AccountID != "svc@proj.iam.gserviceaccount.com" || tok.Refresh != "kid-1" {
		t.Errorf("token = %+v", tok)
	}
	if !tok.ExpiresAt.Equal(now.Add(3599 * time.Second)) {
		t.Errorf("ExpiresAt = %v, want the endpoint's expires_in from now", tok.ExpiresAt)
	}
	if calls != 1 {
		t.Errorf("token endpoint called %d times, want 1", calls)
	}
}

// Through the OAuth credential: the key file is the store, it loads empty,
// the first use mints, and the file is never written back.
func TestAServiceAccountCredentialMintsOnFirstUseAndNeverRewritesTheKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	srv := fakeGoogleToken(t, &key.PublicKey, &calls)
	path := writeServiceAccount(t, key, srv.URL)
	before, _ := os.ReadFile(path)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	rf, err := NewServiceAccountRefresher(ServiceAccountConfig{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewOAuthCredential(OAuthConfig{
		ID: "vertex-1", Provider: "vertex", Source: SourceFile, Path: path,
		StoreFormat: FormatGCPServiceAccount, Fields: FormatGCPServiceAccount.Fields(TokenFields{}),
		Now: func() time.Time { return now },
	}, rf)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if err := c.RefreshIfDue(context.Background()); err != nil {
		t.Fatalf("RefreshIfDue: %v", err)
	}
	got, err := c.AccessToken()
	if err != nil || got != "ya29.minted" {
		t.Fatalf("AccessToken = %q, %v", got, err)
	}
	h := http.Header{}
	if err := c.Apply(h); err != nil || h.Get("Authorization") != "Bearer ya29.minted" {
		t.Errorf("Apply: %v %q", err, h.Get("Authorization"))
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("the service-account key file was rewritten by a token refresh")
	}
	if calls != 1 {
		t.Errorf("token endpoint called %d times, want 1 mint", calls)
	}
}
