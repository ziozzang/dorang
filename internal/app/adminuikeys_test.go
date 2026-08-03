package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/store"
)

// The operator UI's credential lifecycle, against an assembled gateway.
//
// internal/admin proves these properties against fakes. This file proves the
// ones that only exist once the real hasher, the real store, the real
// authenticator and the real invalidation path are in the same process — which
// is where the question an operator actually has lives: *does the key I just
// made from a browser work, and does the one I just deleted stop?*
//
// Every assertion is a status the gateway returned to a credential, or a row the
// store holds. None of them is "the handler answered 200".

// uiCall drives the UI the way a browser does: form-encoded, with a cookie, and
// with no bearer header anywhere.
func uiCall(a *App, method, path string, form url.Values, c *http.Cookie,
	opts ...func(*http.Request)) *httptest.ResponseRecorder {

	var r *http.Request
	if form == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c != nil {
		r.AddCookie(c)
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	a.Server.ServeHTTP(w, r)
	return w
}

var (
	reCSRF = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	// The capture group is what reads the one-time secret back out of the
	// rendered page; the pattern itself holds nothing.
	reSecret    = regexp.MustCompile(`id="secret-value" value="([^"]+)"`) // pragma: allowlist secret
	reRevealKey = regexp.MustCompile(`<dt>key id</dt><dd class="mono">([^<]+)</dd>`)
)

// uiSignIn exchanges the master credential for a session and reads the token off
// a rendered page — the same way the browser gets it, so that a template that
// stopped emitting one would fail here rather than pass on a value scraped out
// of the server's own memory.
func uiSignIn(t *testing.T, a *App) (*http.Cookie, string) {
	t.Helper()
	w := uiCall(a, http.MethodPost, "/ui/login", url.Values{"key": {testMasterKey}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("sign-in = %d: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("sign-in issued no session cookie")
	}
	c := cookies[0]

	page := uiCall(a, http.MethodGet, "/ui/keys/new", nil, c)
	if page.Code != http.StatusOK {
		t.Fatalf("the create form = %d: %s", page.Code, page.Body.String())
	}
	m := reCSRF.FindStringSubmatch(page.Body.String())
	if m == nil {
		t.Fatal("the create form carries no token; every mutation would refuse")
	}
	return c, m[1]
}

// uiAct performs one action and returns the page the operator lands on, having
// followed the redirect a successful mutation answers with.
func uiAct(t *testing.T, a *App, c *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	w := uiCall(a, http.MethodPost, "/ui/keys/action", form, c)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("%s = %d, want 303: %s", form.Get("action"), w.Code, w.Body.String())
	}
	return uiCall(a, http.MethodGet, w.Header().Get("Location"), nil, c)
}

// The whole lifecycle from a browser, asserted against the credential.
//
// Create, use, rotate, use both, cut the grace, use one, delete, use none. The
// gateway's answer to the secret is the assertion at every step, because "the
// screen said so" and "the credential works" have been different things in this
// codebase before.
func TestTheUILifecycleWorksAgainstARealGateway(t *testing.T) {
	a := newWiringApp(t, adminUIYAML, nil)
	ctx := context.Background()
	c, token := uiSignIn(t, a)

	// --- create ---------------------------------------------------------
	page := uiAct(t, a, c, url.Values{
		"csrf": {token}, "action": {"create"},
		"key_alias": {"browser-made"}, "models": {"chat"}, "duration": {"30d"},
	})
	if page.Code != http.StatusOK {
		t.Fatalf("the secret page = %d: %s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	m := reSecret.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no secret on the page:\n%s", body)
	}
	secret := m[1]
	km := reRevealKey.FindStringSubmatch(body)
	if km == nil {
		t.Fatal("the secret page does not name the key it belongs to")
	}
	keyID := km[1]

	// It authenticates, through the real hasher and the real store.
	p, err := a.Auth.Authenticate(ctx, secret)
	if err != nil {
		t.Fatalf("the key the UI minted does not authenticate: %v", err)
	}
	if p.KeyID != keyID {
		t.Errorf("the secret resolves to key %q; the page said %q", p.KeyID, keyID)
	}
	// And it serves, through the whole stack.
	if w := callWith(a, secret, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("a key made from the UI does not serve: %d %s", w.Code, w.Body.String())
	}
	// The allow-list the form carried is on the key rather than decoration.
	row, err := a.Store.GetAPIKey(ctx, keyID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if row.KeyAlias != "browser-made" || len(row.Models) != 1 || row.Models[0] != "chat" {
		t.Errorf("the form's fields did not reach the row: alias=%q models=%v",
			row.KeyAlias, row.Models)
	}
	if row.ExpiresAt.IsZero() {
		t.Error("the form asked for a 30d expiry and the row has none")
	}

	// The secret is shown once. A reload does not reproduce it, here as in the
	// unit tests — asserted again through the assembled server, because this is
	// the process an operator's browser talks to.
	if again := uiCall(a, http.MethodGet, "/ui/keys/secret", nil, c); again.Code != http.StatusGone {
		t.Fatalf("a reload of the secret page = %d, want 410", again.Code)
	}

	// --- rotate, and honour both secrets for the grace -------------------
	page = uiAct(t, a, c, url.Values{
		"csrf": {token}, "action": {"rotate"}, "key_id": {keyID}, "grace": {"24h"},
	})
	m = reSecret.FindStringSubmatch(page.Body.String())
	if m == nil {
		t.Fatalf("the rotation showed no secret:\n%s", page.Body.String())
	}
	rotated := m[1]
	if rotated == secret {
		t.Fatal("the rotation returned the same secret")
	}

	// Both authenticate, which is the whole point of a grace window.
	oldP, err := a.Auth.Authenticate(ctx, secret)
	if err != nil {
		t.Fatalf("the superseded secret stopped working inside its grace: %v", err)
	}
	newP, err := a.Auth.Authenticate(ctx, rotated)
	if err != nil {
		t.Fatalf("the new secret does not authenticate: %v", err)
	}
	if oldP.KeyID != keyID || newP.KeyID != keyID {
		t.Errorf("rotation changed the key id: %q / %q, want %q", oldP.KeyID, newP.KeyID, keyID)
	}
	// And the gateway serves both.
	for name, s := range map[string]string{"superseded": secret, "current": rotated} {
		if w := callWith(a, s, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
			t.Errorf("the %s secret does not serve during the grace: %d", name, w.Code)
		}
	}

	// The ledger records WHICH secret was used, and this is the value it
	// records: internal/server stamps Principal.SecretID into request_logs
	// (see server.go's principalSecret), and internal/store's secrets_test
	// asserts the column round-trips. What is asserted here is that the two
	// secrets are distinguishable at that point at all — a rotation whose
	// secrets both reported the same id would make the column useless without
	// making any test fail.
	if oldP.SecretID == "" || newP.SecretID == "" {
		t.Fatalf("a secret authenticated with no secret id: %q / %q", oldP.SecretID, newP.SecretID)
	}
	if oldP.SecretID == newP.SecretID {
		t.Fatalf("both secrets report secret id %q; the ledger could not tell them apart",
			oldP.SecretID)
	}
	secrets, err := a.Store.ListKeySecrets(ctx, keyID)
	if err != nil {
		t.Fatalf("ListKeySecrets: %v", err)
	}
	if len(secrets) != 2 {
		t.Fatalf("the key has %d secrets after one rotation, want 2", len(secrets))
	}
	byID := map[string]bool{}
	for _, s := range secrets {
		byID[s.ID] = true
	}
	for _, id := range []string{oldP.SecretID, newP.SecretID} {
		if !byID[id] {
			t.Errorf("secret id %q is not one of the key's rows", id)
		}
	}

	// --- cut the grace short ---------------------------------------------
	uiAct(t, a, c, url.Values{"csrf": {token}, "action": {"cut"}, "key_id": {keyID}})
	if w := callWith(a, secret, http.MethodGet, "/v1/models", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the superseded secret still serves after the grace was cut: %d", w.Code)
	}
	if w := callWith(a, rotated, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Errorf("cutting the grace locked out the client that HAD rolled: %d", w.Code)
	}

	// --- block, unblock ---------------------------------------------------
	uiAct(t, a, c, url.Values{"csrf": {token}, "action": {"block"}, "key_id": {keyID}})
	// 403 and not 401: a blocked key is present and refused AS SUCH, which is
	// the distinction OPERATIONS §3 draws between blocking and deleting. The
	// status is the assertion — it is how the caller learns which happened.
	if w := callWith(a, rotated, http.MethodGet, "/v1/models", ""); w.Code != http.StatusForbidden {
		t.Errorf("a key blocked from the UI answered %d, want 403", w.Code)
	}
	uiAct(t, a, c, url.Values{"csrf": {token}, "action": {"unblock"}, "key_id": {keyID}})
	if w := callWith(a, rotated, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Errorf("a key unblocked from the UI does not serve: %d", w.Code)
	}

	// --- delete ------------------------------------------------------------
	uiAct(t, a, c, url.Values{"csrf": {token}, "action": {"delete"}, "key_id": {keyID}})
	if w := callWith(a, rotated, http.MethodGet, "/v1/models", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a key deleted from the UI still serves: %d", w.Code)
	}
	if _, err := a.Store.GetAPIKey(ctx, keyID); err == nil {
		t.Error("the row survived the delete")
	}

	// Every one of those wrote a trail row naming the operator, because each one
	// went through the same handler a script reaches.
	var actions []string
	rows, err := a.Store.DB().QueryContext(ctx,
		`SELECT action FROM audit_logs ORDER BY ts, id`)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		actions = append(actions, s)
	}
	for _, want := range []string{
		"key.generate", "key.rotate", "key.rotate.cut", "key.block", "key.unblock", "key.delete",
	} {
		found := false
		for _, got := range actions {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no audit row for %s; the trail is %v", want, actions)
		}
	}
}

// A forged cross-site POST at the assembled gateway is refused, and the key it
// named is untouched.
//
// The unit test in internal/admin covers every action; this one covers the
// mount: the UI route is Public in the gateway's sense (it has to serve a
// sign-in form), so it is worth asserting that "public" did not become
// "unauthenticated mutation" somewhere between server.Route and the handler.
func TestForgedPostAtTheAssembledGatewayIsRefused(t *testing.T) {
	a := newWiringApp(t, adminUIYAML, nil)
	ctx := context.Background()
	c, token := uiSignIn(t, a)
	victim := issueKey(t, a, func(k *store.APIKey) { k.KeyAlias = "victim" })
	row := keyRowFor(t, a, "victim")

	for _, tc := range []struct {
		what string
		form url.Values
		opts []func(*http.Request)
	}{
		{"no token", url.Values{"action": {"delete"}, "key_id": {row}}, nil},
		{"a guess", url.Values{"action": {"delete"}, "key_id": {row}, "csrf": {"guessed"}}, nil},
		{"the token, from another origin",
			url.Values{"action": {"delete"}, "key_id": {row}, "csrf": {token}},
			[]func(*http.Request){func(r *http.Request) {
				r.Header.Set("Origin", "https://evil.example")
			}}},
	} {
		w := uiCall(a, http.MethodPost, "/ui/keys/action", tc.form, c, tc.opts...)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403\n%s", tc.what, w.Code, w.Body.String())
		}
	}

	if _, err := a.Store.GetAPIKey(ctx, row); err != nil {
		t.Fatalf("a forged request removed the key: %v", err)
	}
	if w := callWith(a, victim, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Errorf("a forged request stopped the key serving: %d", w.Code)
	}
}

// keyRowFor finds the id of the key with an alias, so a test can act on a key it
// created through the store.
func keyRowFor(t *testing.T, a *App, alias string) string {
	t.Helper()
	keys, err := a.Store.ListAPIKeys(context.Background(), store.APIKeyFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	for _, k := range keys {
		if k.KeyAlias == alias {
			return k.ID
		}
	}
	t.Fatalf("no key with alias %q", alias)
	return ""
}

// The administration API refuses the UI's session cookie on the assembled
// gateway, on the routes that mint and the routes that destroy.
//
// This is the half of the security model that did not change when the UI was
// given mutations, and it is therefore the only structural half left. It is
// asserted here as well as in internal/admin because the two answer different
// questions: there, that admin.API strips the header; here, that nothing
// between server.Route and that call puts it back.
func TestTheAdministrationAPIRefusesTheUICookieOnTheGateway(t *testing.T) {
	a := newWiringApp(t, adminUIYAML, nil)
	c, _ := uiSignIn(t, a)

	for _, path := range []string{"/key/list", "/key/generate", "/key/delete", "/key/block"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(c)
		w := httptest.NewRecorder()
		a.Server.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("POST %s with the UI cookie = %d, want 401: the API must never accept it",
				path, w.Code)
		}
	}
	// And the same cookie is still what authorizes the UI.
	if w := uiCall(a, http.MethodGet, "/ui/keys", nil, c); w.Code != http.StatusOK {
		t.Fatalf("the cookie stopped authorizing the UI: %d", w.Code)
	}
}
