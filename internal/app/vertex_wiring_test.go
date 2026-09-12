package app

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The whole Vertex path from an operator's file: a service-account key file
// as the credential, a token minted from it at a (fake) Google endpoint, and
// the request landing on the project-scoped route with that bearer.
func TestVertexFromTheFileMintsAndAddressesTheProject(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var minted int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		minted++
		mu.Unlock()
		if !strings.Contains(r.Form.Get("grant_type"), "jwt-bearer") || r.Form.Get("assertion") == "" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ya29.wired","expires_in":3600,"token_type":"Bearer"}`))
	}))
	t.Cleanup(tokenSrv.Close)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	sa, _ := json.Marshal(map[string]string{
		"type": "service_account", "private_key_id": "kid", "client_email": "svc@proj.iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":   tokenSrv.URL,
	})
	saPath := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(saPath, sa, 0o600); err != nil {
		t.Fatal(err)
	}

	var seenPath, seenAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		seenPath, seenAuth = r.URL.Path, r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1}}`))
	}))
	t.Cleanup(up.Close)

	yaml := `
version: 1
providers:
  - {name: vx, kind: vertex, base_url: "` + up.URL + `", params: {project: proj, location: us-central1}}
credentials:
  - id: c1
    provider: vx
    auth: oauth
    oauth:
      source: file
      path: "` + saPath + `"
      format: gcp-service-account
      refresh: {token_url: "` + tokenSrv.URL + `"}
models:
  - name: m1
    deployments:
      - {provider: vx, upstream_model: gemini-2.5-flash, credentials: [c1]}
`
	a := newWiringApp(t, yaml, nil, func(o *Options) { o.Upstream = up.Client() })
	k := issueKey(t, a, nil)
	w := callWith(a, k, http.MethodPost, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if seenPath != "/v1/projects/proj/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent" {
		t.Errorf("host saw %q", seenPath)
	}
	if seenAuth != "Bearer ya29.wired" {
		t.Errorf("authorization = %q; the minted token did not reach the route", seenAuth)
	}
	if minted != 1 {
		t.Errorf("token minted %d times, want 1", minted)
	}
}
