package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
)

func setupTestApp(t *testing.T) *App {
	t.Helper()
	a := newWiringApp(t, pricedYAML, nil)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(pricedYAML), 0600); err != nil {
		t.Fatal(err)
	}
	a.SetConfigControl(path, func() error {
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		return a.Reload(cfg)
	})
	return a
}
func setupSnapshotTest(t *testing.T, a *App) map[string]any {
	t.Helper()
	w := callWith(a, testMasterKey, http.MethodGet, "/admin/setup", "")
	if w.Code != 200 {
		t.Fatalf("snapshot status %d: %s", w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func setupChangeTest(t *testing.T, a *App, in map[string]any) map[string]any {
	t.Helper()
	in["revision"] = setupSnapshotTest(t, a)["revision"]
	raw, _ := json.Marshal(in)
	w := callWith(a, testMasterKey, http.MethodPost, "/admin/setup/change", string(raw))
	if w.Code != 200 {
		t.Fatalf("setup %v status %d: %s", in["action"], w.Code, w.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}
func TestSetupAccountModelAndKeyRotationServeRealRequests(t *testing.T) {
	var mu sync.Mutex
	var observed []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		observed = append(observed, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"discovered-model"},{"id":"second-model"}]}`))
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":"chat-setup","object":"chat.completion","model":"discovered-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer upstream.Close()
	a := setupTestApp(t)
	if out := setupChangeTest(t, a, map[string]any{"action": "provider", "id": "setup-provider", "kind": "openai", "base_url": upstream.URL + "/v1"}); out["status"] != "applied" {
		t.Fatalf("provider not applied: %v", out)
	}
	for i, key := range []string{"setup-secret-before", "setup-secret-after"} {
		out := setupChangeTest(t, a, map[string]any{"action": "credential", "id": "setup-account", "provider": "setup-provider", "auth": "key", "source": "secret", "secret": key, "edit": i > 0})
		if out["status"] != "applied" {
			t.Fatalf("account not applied: %v", out)
		}
		for _, c := range a.Config().Credentials {
			if c.ID == "setup-account" {
				st, err := os.Stat(c.Key.File)
				if err != nil {
					t.Fatal(err)
				}
				if st.Mode().Perm() != 0600 {
					t.Fatal("secret permissions are not private")
				}
			}
		}
		snapshot := setupSnapshotTest(t, a)
		raw, _ := json.Marshal(snapshot)
		disk, _ := os.ReadFile(a.configPath)
		if strings.Contains(string(raw), key) || strings.Contains(string(disk), key) {
			t.Fatal("setup exposed a secret")
		}
		if i == 0 {
			w := callWith(a, testMasterKey, http.MethodPost, "/admin/setup/discover", `{"credential":"setup-account"}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "discovered-model") {
				t.Fatalf("discovery failed: %d %s", w.Code, w.Body)
			}
			setupChangeTest(t, a, map[string]any{"action": "model", "id": "binding", "model": "setup-chat", "provider": "setup-provider", "upstream": "discovered-model", "credential": "setup-account", "weight": 1, "enabled": false})
			w = callWith(a, testMasterKey, http.MethodPost, "/v1/chat/completions", `{"model":"setup-chat","messages":[{"role":"user","content":"hi"}]}`)
			if w.Code == 200 {
				t.Fatal("disabled model served a request")
			}
			w = callWith(a, testMasterKey, http.MethodPost, "/model/deployment/set_enabled", `{"model_group":"setup-chat","provider":"setup-provider","upstream_model":"discovered-model","enabled":true}`)
			if w.Code != 200 {
				t.Fatalf("activation failed: %d %s", w.Code, w.Body)
			}
		}
		w := callWith(a, testMasterKey, http.MethodPost, "/v1/chat/completions", `{"model":"setup-chat","messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != 200 {
			t.Fatalf("model did not serve: %d %s", w.Code, w.Body)
		}
		mu.Lock()
		last := observed[len(observed)-1]
		mu.Unlock()
		if last != "Bearer "+key {
			t.Fatal("serving used the wrong credential after rotation")
		}
	}
}
func TestSetupRefusesStaleRevisionAndLeavesConfigIntact(t *testing.T) {
	a := setupTestApp(t)
	before, _ := os.ReadFile(a.configPath)
	raw, _ := json.Marshal(map[string]any{"action": "provider", "id": "new-provider", "kind": "openai", "revision": strings.Repeat("0", 64)})
	w := callWith(a, testMasterKey, http.MethodPost, "/admin/setup/change", string(raw))
	if w.Code != 409 {
		t.Fatalf("stale write status %d", w.Code)
	}
	after, _ := os.ReadFile(a.configPath)
	if string(before) != string(after) {
		t.Fatal("stale write changed config")
	}
}
func TestSetupOAuthIsSavedPendingWithoutReplacingLiveAccounts(t *testing.T) {
	a := setupTestApp(t)
	out := setupChangeTest(t, a, map[string]any{"action": "credential", "id": "oauth-new", "provider": "p1", "auth": "oauth", "source": "secret", "format": "generic", "secret": `{"access_token":"fixture-oauth-token"}`})
	if out["status"] != "restart_required" {
		t.Fatalf("false application status: %v", out)
	}
	for _, c := range a.Config().Credentials {
		if c.ID == "oauth-new" {
			t.Fatal("pending OAuth account became active")
		}
	}
	if setupSnapshotTest(t, a)["status"] != "restart_required" {
		t.Fatal("pending state was not observable")
	}
}
func TestSetupDiscoveryDoesNotFollowRedirects(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(200) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	a := setupTestApp(t)
	setupChangeTest(t, a, map[string]any{"action": "provider", "id": "p1", "kind": "openai", "base_url": source.URL, "edit": true})
	w := callWith(a, testMasterKey, http.MethodPost, "/admin/setup/discover", `{"credential":"c1"}`)
	if w.Code != 200 || hits != 0 || !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatal("model discovery followed a redirect")
	}
}
