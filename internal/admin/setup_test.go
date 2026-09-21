package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type fakeSetup struct{ changes int }

func (f *fakeSetup) Snapshot(context.Context) (map[string]any, error) {
	return map[string]any{"revision": strings.Repeat("a", 64), "status": "applied", "providers": []any{}, "credentials": []any{}, "models": []any{}, "kinds": []any{}}, nil
}
func (f *fakeSetup) Change(context.Context, SetupChange) (map[string]any, error) {
	f.changes++
	return map[string]any{"id": "account", "operation": "credential", "status": "applied", "secret_updated": true}, nil
}
func (f *fakeSetup) Discover(context.Context, string) (map[string]any, error) {
	return map[string]any{"status": "ready", "models": []string{"test-model"}}, nil
}
func TestSetupRequiresGlobalScopeCSRFAndRedactsAudit(t *testing.T) {
	f := &fakeSetup{}
	h := newHarness(t, func(c *Config) { c.Setup = f })
	cookie, csrf := signInUI(t, h, masterToken)
	form := url.Values{"action": {"setup_save"}, "operation": {"credential"}, "secret": {"never-audit-this-secret"}, "return": {"/ui/setup"}}
	if w := uiPost(h, "/ui"+uiActionPath, form, cookie); w.Code != 403 {
		t.Fatalf("missing CSRF status %d", w.Code)
	}
	if f.changes != 0 {
		t.Fatal("forged form changed config")
	}
	form.Set("csrf", csrf)
	w := uiPost(h, "/ui"+uiActionPath, form, cookie)
	if w.Code != 303 {
		t.Fatalf("authorized form status %d: %s", w.Code, w.Body)
	}
	raw, _ := json.Marshal(h.store.auditLog())
	if strings.Contains(string(raw), "never-audit-this-secret") {
		t.Fatal("secret leaked to audit")
	}
	if !strings.Contains(string(raw), "setup.credential") {
		t.Fatal("missing setup audit")
	}
	for _, path := range []string{"/admin/setup", "/admin/setup/change", "/admin/setup/discover"} {
		w := h.do(http.MethodPost, path, map[string]any{}, asToken(teamAToken))
		if w.Code != 403 {
			t.Fatalf("team could use %s: %d", path, w.Code)
		}
	}
	form = url.Values{"action": {"setup_discover"}, "credential": {"account"}, "csrf": {csrf}}
	w = uiPost(h, "/ui"+uiActionPath, form, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "test-model") {
		t.Fatal("session model discovery is not wired")
	}
}
