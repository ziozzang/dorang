package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The operator UI's mutating surface.
//
// Every assertion below reads what the BROWSER got or what the STORE holds. A
// handler that returned 200 was never the thing in doubt: the questions are
// whether a request from another site is refused, whether a secret can be made
// to appear twice, and whether a key the screen says is deleted has stopped
// serving.

// ---------------------------------------------------------------------------
// Browser-shaped helpers
// ---------------------------------------------------------------------------

// signInUI exchanges a credential for a session and returns the cookie together
// with the token that session's forms carry.
//
// The token is read from the session table rather than scraped, so that a test
// about forgery is not also a test about template rendering.
// TestTheCreateFormCarriesTheSessionsOwnToken asserts the page and the table
// agree, which is the other half.
func signInUI(t *testing.T, h *harness, token string) (*http.Cookie, string) {
	t.Helper()
	c := signIn(t, h, token)
	h.api.ui.mu.Lock()
	defer h.api.ui.mu.Unlock()
	sess, ok := h.api.ui.sessions[c.Value]
	if !ok {
		t.Fatal("the session was not in the table after sign-in")
	}
	if sess.csrf == "" {
		t.Fatal("the session carries no token; every mutation would refuse")
	}
	return c, sess.csrf
}

// uiPost submits a form the way a browser does: urlencoded, in the body, with
// the session cookie attached and no bearer header anywhere.
func uiPost(h *harness, path string, form url.Values, c *http.Cookie,
	opts ...func(*http.Request)) *httptest.ResponseRecorder {

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "operator-browser")
	if c != nil {
		req.AddCookie(c)
	}
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	return rec
}

func uiGet(h *harness, path string, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	return rec
}

func header(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

// form builds a submission. The token is the caller's business, which is the
// point of most of the tests below.
func form(pairs ...string) url.Values {
	f := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		f.Set(pairs[i], pairs[i+1])
	}
	return f
}

// authenticates reports whether a plaintext secret resolves to a live key, the
// way the gateway's authenticator does it: the lookup selects the row and the
// digest verifies it.
//
// It reads the STORE. "The response said 200" and "this credential now works"
// are different claims, and only the second one is what an operator is about to
// hand to a client.
func authenticates(h *harness, secret string) (string, bool) {
	hh := fakeHasher{}
	lookup := hh.Lookup(secret)
	digest, scheme, err := hh.Hash(secret)
	if err != nil {
		return "", false
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	for id, v := range h.store.verifiers {
		if v.Lookup != lookup || v.TokenHash != digest || v.HashScheme != scheme {
			continue
		}
		if _, live := h.store.keys[id]; !live {
			// The verifier outlived its row. A key whose row is gone does not
			// serve, whatever is left in the index.
			return "", false
		}
		return id, true
	}
	return "", false
}

var reSecretValue = regexp.MustCompile(`id="secret-value" value="([^"]+)"`)

func secretOnPage(t *testing.T, body string) string {
	t.Helper()
	m := reSecretValue.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the page shows no secret field:\n%s", body)
	}
	return m[1]
}

// auditCount is how many rows the trail holds, for the assertions that a
// refused request changed NOTHING — including nothing anybody would have to
// reconcile later.
func auditCount(h *harness) int {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return len(h.store.audits)
}

// seedAdminSession puts the row behind `adminToken` in the store, so that the
// per-request re-check of the session's key has something to find.
func seedAdminSession(h *harness) {
	seedKey(h, &Key{ID: "key-admin", KeyLabel: "dk-admin", CreatedAt: testNow})
}

// ---------------------------------------------------------------------------
// Cross-site request forgery
// ---------------------------------------------------------------------------

// A forged cross-site POST carrying a valid session cookie is refused — for
// every action, because the table this ranges over is the one the dispatcher
// reads. An action added to uiActions is covered here on the same commit; that
// is the whole reason there is a table rather than eight handlers.
//
// Three shapes, and the first is the one that matters: a cross-site form can
// make a browser attach the cookie and can put anything it likes in the body,
// and the one thing it cannot do is know this session's token.
func TestForgedCrossSitePostIsRefusedForEveryAction(t *testing.T) {
	for name := range uiActions {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			seedAdminSession(h)
			id, _ := h.newKey(map[string]any{"key_alias": "victim"})
			c, token := signInUI(t, h, adminToken)
			before := auditCount(h)

			for _, tc := range []struct {
				what string
				form url.Values
				opts []func(*http.Request)
			}{
				{
					what: "no token at all — what a cross-site form can send",
					form: form("action", name, "key_id", id),
				},
				{
					what: "a guessed token",
					form: form("action", name, "key_id", id, csrfField, "not-the-token"),
				},
				{
					what: "the right token from another origin",
					form: form("action", name, "key_id", id, csrfField, token),
					opts: []func(*http.Request){header("Origin", "https://evil.example")},
				},
				{
					what: "the right token with the browser saying it is cross-site",
					form: form("action", name, "key_id", id, csrfField, token),
					opts: []func(*http.Request){header("Sec-Fetch-Site", "cross-site")},
				},
			} {
				rec := uiPost(h, "/ui"+uiActionPath, tc.form, c, tc.opts...)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s: status %d, want 403\n%s", tc.what, rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), CodeCrossSiteRefused) {
					t.Errorf("%s: the refusal does not carry %q", tc.what, CodeCrossSiteRefused)
				}
			}

			// And the store is exactly where it was: the key is present,
			// serving, unpended, and no trail row was written for something
			// that did not happen.
			h.store.mu.Lock()
			k, live := h.store.keys[id]
			h.store.mu.Unlock()
			if !live {
				t.Fatal("a forged request deleted the key")
			}
			if k.Blocked || !k.PendedAt.IsZero() {
				t.Errorf("a forged request changed the key's state: blocked=%v pended=%v",
					k.Blocked, k.PendedAt)
			}
			if got := auditCount(h); got != before {
				t.Errorf("a forged request wrote %d audit row(s)", got-before)
			}
			if got := h.api.Metrics().UIMutations; got != 0 {
				t.Errorf("UIMutations = %d after four refusals", got)
			}
			if got := h.api.Metrics().UIForgeries; got != 4 {
				t.Errorf("UIForgeries = %d, want 4", got)
			}
		})
	}
}

// The same session's own token works, so the test above is refusing forgery
// rather than refusing everything. A CSRF defence that also refuses the
// operator is indistinguishable from a broken feature and would hide one.
func TestTheSessionsOwnTokenIsAccepted(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	id, _ := h.newKey(map[string]any{"key_alias": "victim"})
	c, token := signInUI(t, h, adminToken)

	rec := uiPost(h, "/ui"+uiActionPath,
		form("action", "block", "key_id", id, csrfField, token), c)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	h.store.mu.Lock()
	blocked := h.store.keys[id].Blocked
	h.store.mu.Unlock()
	if !blocked {
		t.Fatal("the key was not blocked")
	}
}

// The token a browser would actually send is the one in the page, and it is the
// one the session holds. Reading it out of the session table (as the helpers
// above do) would be a hermetic test of nothing if the template emitted
// something else.
func TestTheCreateFormCarriesTheSessionsOwnToken(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, token := signInUI(t, h, adminToken)

	rec := uiGet(h, "/ui/keys/new", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d\n%s", rec.Code, rec.Body.String())
	}
	want := `name="csrf" value="` + token + `"`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the create form does not carry the session's token")
	}
	// And the token is nowhere in a URL on the page: a token in a link travels
	// into history, into Referer and into every access log on the way.
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.Contains(line, "href=") && strings.Contains(line, token) {
			t.Errorf("the token appears in a link: %s", strings.TrimSpace(line))
		}
	}
}

// Two sessions, two tokens. A token is proof that THIS session sent the
// request, and one that authorized any session would be a shared secret rather
// than a per-session one.
func TestOneSessionsTokenDoesNotAuthorizeAnother(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	id, _ := h.newKey(nil)
	victim, _ := signInUI(t, h, adminToken)
	_, otherToken := signInUI(t, h, masterToken)

	rec := uiPost(h, "/ui"+uiActionPath,
		form("action", "block", "key_id", id, csrfField, otherToken), victim)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: another session's token authorized a mutation", rec.Code)
	}
}

// A header-authenticated view reads and does not act. There is no browser
// client for a bearer header, so the only thing this path could add is a
// forgeable surface for a proxy whose cookie dorang cannot see.
func TestHeaderAuthenticatedViewCannotMutate(t *testing.T) {
	h := newHarness(t)
	h.newKey(nil)

	rec := h.do(http.MethodPost, "/ui"+uiActionPath, nil, func(r *http.Request) {
		r.Body = http.NoBody
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403\n%s", rec.Code, rec.Body.String())
	}
	// And the screen offers it no controls, so it is not a refusal an operator
	// discovers by clicking.
	page := h.do(http.MethodGet, "/ui/keys", nil).Body.String()
	if strings.Contains(page, uiActionPath) {
		t.Error("a read-only view rendered a mutating form")
	}
	if strings.Contains(page, "keys/confirm") {
		t.Error("a read-only view rendered a destructive control")
	}
	if !strings.Contains(page, "read-only") {
		t.Error("a read-only view does not say it is one")
	}
}

// There is one mutating URL. Everything else in the UI answers a method
// refusal, which is what makes the forgery test above complete rather than
// merely thorough.
func TestTheUIHasExactlyOneMutatingURL(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, token := signInUI(t, h, adminToken)

	for _, p := range []string{"/ui/keys", "/ui/models", "/ui/usage", "/ui/keys/new",
		"/ui/keys/confirm", "/ui/keys/secret"} {
		rec := uiPost(h, p, form(csrfField, token, "action", "delete"), c)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405 — only %s accepts a mutation",
				p, rec.Code, uiActionPath)
		}
	}
}

// A method that is neither safe nor POST is refused before anything reads it.
func TestUIRefusesOtherMethods(t *testing.T) {
	h := newHarness(t)
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := h.do(m, "/ui/keys", nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /ui/keys = %d, want 405", m, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// The action table and the routes it names
// ---------------------------------------------------------------------------

// Every action names a route this build actually serves, with POST mounted.
// The table is the UI's claim about the administration surface, and a row
// naming a path that does not exist is a control that 501s on click.
func TestEveryUIActionNamesAServedRoute(t *testing.T) {
	h := newHarness(t)
	for name, a := range uiActions {
		rt := h.api.routes[cleanPath(a.api)]
		if rt == nil {
			t.Errorf("action %q names %q, which this build does not serve", name, a.api)
			continue
		}
		if rt.methods[http.MethodPost] == nil {
			t.Errorf("action %q names %q, which does not accept POST", name, a.api)
		}
		if a.body == nil {
			t.Errorf("action %q builds no request body", name)
		}
		if a.verb == "" || a.past == "" {
			t.Errorf("action %q has no words for what it does", name)
		}
		if a.confirm && a.consequence == "" {
			t.Errorf("action %q asks for a confirmation and says nothing about what it does", name)
		}
	}
	// The lifecycle the operator asked for, in full. A missing row here is a
	// control that quietly went away.
	for _, want := range []string{"create", "delete", "rotate", "cut", "block", "unblock",
		"pend", "release"} {
		if _, ok := uiActions[want]; !ok {
			t.Errorf("the UI cannot %s", want)
		}
	}
}

// Anything that stops a credential working, or cannot be undone, goes through a
// page that names the key. The two that RESTORE service do not: §11.6 requires a
// pended key to be released in one action, and an interstitial is not one.
func TestDestructiveActionsConfirmAndRestoringOnesDoNot(t *testing.T) {
	for name, a := range uiActions {
		switch name {
		case "delete", "block", "cut", "rotate", "pend":
			if !a.confirm {
				t.Errorf("%q does not confirm, and it stops a credential working", name)
			}
		case "unblock", "release":
			if a.confirm {
				t.Errorf("%q confirms; restoring service is one action (§11.6)", name)
			}
		case "create":
			if a.confirm {
				t.Errorf("%q confirms; minting is not destructive and the page after it is the warning", name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Confirmation
// ---------------------------------------------------------------------------

// The confirmation page names the key, says what will happen to it, and is the
// only thing that carries a token — the control in the row is a link, so a
// mis-click one pixel above the intended row costs a page load and not a
// credential.
func TestConfirmationNamesTheKeyBeforeADestructiveAction(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	id, _ := h.newKey(map[string]any{"key_alias": "payments", "team_id": "", "max_budget": 250})
	c, token := signInUI(t, h, adminToken)

	// The row's destructive controls are links to the confirmation, not forms.
	rows := uiGet(h, "/ui/keys", c).Body.String()
	for _, want := range []string{
		`href="/ui/keys/confirm?action=delete&amp;key_id=` + id + `"`,
		`href="/ui/keys/confirm?action=block&amp;key_id=` + id + `"`,
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("the keys screen does not offer %s", want)
		}
	}

	rec := uiGet(h, "/ui/keys/confirm?action=delete&key_id="+id, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		id,                       // which key
		"payments",               // by the name the operator knows it by
		"cannot be brought back", // what this does
		`value="delete"`,         // and the form that will do it
		`name="csrf" value="` + token + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the confirmation page does not contain %q", want)
		}
	}

	// The page changed nothing: it is a GET, so a reload re-reads the key.
	if _, live := authenticatesByID(h, id); !live {
		t.Error("opening the confirmation page deleted the key")
	}
}

func authenticatesByID(h *harness, id string) (*Key, bool) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	k, ok := h.store.keys[id]
	return k, ok
}

// A confirmation page is a read through the same handler as everything else, so
// it cannot show an operator a key they may not act on.
func TestConfirmationRespectsScope(t *testing.T) {
	h := newHarness(t)
	seedKey(h, &Key{ID: "key-team-a", KeyLabel: "dk-team-a", TeamID: "team-a", CreatedAt: testNow})
	other, _ := h.newKey(map[string]any{"key_alias": "somebody-elses", "team_id": "team-b"})
	c, token := signInUI(t, h, teamAToken)

	rec := uiGet(h, "/ui/keys/confirm?action=delete&key_id="+other, c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a team-a administrator saw team-b's key: %d\n%s", rec.Code, rec.Body.String())
	}
	// And posting the action anyway, with a valid token, is refused the same
	// way — the confirmation page is a courtesy and the handler is the check.
	rec = uiPost(h, "/ui"+uiActionPath,
		form("action", "delete", "key_id", other, csrfField, token), c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a team-a administrator deleted team-b's key: %d", rec.Code)
	}
	if _, live := authenticatesByID(h, other); !live {
		t.Fatal("the key is gone")
	}
}

// ---------------------------------------------------------------------------
// The one-time secret
// ---------------------------------------------------------------------------

// The secret appears exactly once, a reload does not reproduce it, and the key
// it belongs to authenticates.
//
// This is the property the operator's choice was made against: §2.4's one-time
// secret is now pasted into a browser tab, and the least dorang can do is make
// "once" literal.
func TestTheSecretIsShownOnceAndAReloadDoesNotReproduceIt(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, token := signInUI(t, h, adminToken)

	post := uiPost(h, "/ui"+uiActionPath,
		form("action", "create", csrfField, token, "key_alias", "fresh", "duration", "30d"), c)
	if post.Code != http.StatusSeeOther {
		t.Fatalf("create status %d\n%s", post.Code, post.Body.String())
	}
	loc := post.Header().Get("Location")
	if loc != "/ui/keys/secret" {
		t.Fatalf("Location = %q, want the static secret path", loc)
	}
	// Nothing that could be a credential is in the redirect, and the POST's own
	// body carries none either.
	if strings.Contains(loc, keyPrefix) || strings.Contains(post.Body.String(), keyPrefix) {
		t.Fatalf("the mint response leaked a secret: %q / %q", loc, post.Body.String())
	}

	first := uiGet(h, loc, c)
	if first.Code != http.StatusOK {
		t.Fatalf("the secret page answered %d\n%s", first.Code, first.Body.String())
	}
	secret := secretOnPage(t, first.Body.String())
	if !strings.HasPrefix(secret, keyPrefix) {
		t.Fatalf("that is not a dorang credential: %q", secret)
	}
	// Once on the page, not once per element.
	if n := strings.Count(first.Body.String(), secret); n != 1 {
		t.Errorf("the secret appears %d times on the page, want 1", n)
	}

	// The page says what the operator's browser now knows, at the moment it
	// knows it, rather than in a document.
	for _, want := range []string{
		"only time", "DOM", "session restore", "screenshot", "cannot show",
	} {
		if !strings.Contains(first.Body.String(), want) {
			t.Errorf("the secret page does not mention %q", want)
		}
	}

	// A created key authenticates.
	id, ok := authenticates(h, secret)
	if !ok {
		t.Fatal("the secret the page showed does not resolve to a key")
	}

	// A reload does not reproduce it, and says why rather than 404ing.
	again := uiGet(h, loc, c)
	if again.Code != http.StatusGone {
		t.Fatalf("a reload of the secret page answered %d, want 410", again.Code)
	}
	if strings.Contains(again.Body.String(), secret) {
		t.Fatal("a reload reproduced the secret")
	}
	if !strings.Contains(again.Body.String(), "rotate the key") {
		t.Error("the exhausted page does not say what to do instead")
	}

	// Nor is it anywhere else: not on the screen that lists the key, not in the
	// audit trail that recorded its creation.
	if body := uiGet(h, "/ui/keys", c).Body.String(); strings.Contains(body, secret) {
		t.Fatal("the keys screen renders the secret")
	}
	h.store.mu.Lock()
	audits := append([]AuditEntry(nil), h.store.audits...)
	h.store.mu.Unlock()
	minted := false
	for _, e := range audits {
		if strings.Contains(e.Before, secret) || strings.Contains(e.After, secret) {
			t.Fatalf("audit row %q carries the secret", e.Action)
		}
		if e.Action == "key.generate" && e.ObjectID == id {
			minted = true
			if e.ActorKind != "key" || e.ActorID != "key-admin" {
				t.Errorf("the audit row names actor %s/%s, not the signed-in operator",
					e.ActorKind, e.ActorID)
			}
			if e.UserAgent != "operator-browser" {
				t.Errorf("the audit row's user agent is %q, not the browser's", e.UserAgent)
			}
		}
	}
	if !minted {
		t.Error("the UI minted a key and wrote no audit row naming it")
	}
}

// A HEAD writes no body, so serving one would consume a secret nobody could
// read — and a prefetcher makes that happen without an operator clicking.
func TestHeadDoesNotConsumeTheSecret(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, token := signInUI(t, h, adminToken)
	if rec := uiPost(h, "/ui"+uiActionPath,
		form("action", "create", csrfField, token), c); rec.Code != http.StatusSeeOther {
		t.Fatalf("create status %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodHead, "/ui/keys/secret", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD on the secret page = %d, want 405", rec.Code)
	}
	if got := uiGet(h, "/ui/keys/secret", c); got.Code != http.StatusOK {
		t.Fatalf("the HEAD consumed the secret: the GET after it answered %d", got.Code)
	}
}

// Signing out takes an unread secret with it, and a second create replaces the
// first rather than leaving it in memory for a page nobody will open.
func TestAnUnreadSecretDoesNotLinger(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, token := signInUI(t, h, adminToken)

	mint := func() {
		if rec := uiPost(h, "/ui"+uiActionPath,
			form("action", "create", csrfField, token), c); rec.Code != http.StatusSeeOther {
			t.Fatalf("create status %d", rec.Code)
		}
	}
	mint()
	mint()
	// One slot, so the first secret is not still sitting there.
	h.api.ui.mu.Lock()
	n := 0
	for _, sess := range h.api.ui.sessions {
		if sess.reveal != nil {
			n++
		}
	}
	h.api.ui.mu.Unlock()
	if n != 1 {
		t.Errorf("%d unread secrets are held in memory, want 1", n)
	}

	// And signing out drops it.
	if rec := uiPost(h, "/ui/logout", form(csrfField, token), c); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout status %d", rec.Code)
	}
	h.api.ui.mu.Lock()
	held := len(h.api.ui.sessions)
	h.api.ui.mu.Unlock()
	if held != 0 {
		t.Errorf("%d session(s) survived a sign-out, and each one may be holding a secret", held)
	}
	// The page it would have been shown on has nothing left to show.
	if rec := uiGet(h, "/ui/keys/secret", c); rec.Code == http.StatusOK {
		t.Error("a signed-out session still produced its unread secret")
	}
}

// ---------------------------------------------------------------------------
// The lifecycle, end to end against the store
// ---------------------------------------------------------------------------

// A created key authenticates; a deleted one stops serving. Both are read off
// the store, and the deletion is announced to the fleet on the same path
// /key/delete announces on — because it IS /key/delete.
func TestCreatedKeyAuthenticatesAndDeletedKeyStops(t *testing.T) {
	h, inv := newInvalidatingHarness(t)
	seedAdminSession(h)
	c, token := signInUI(t, h, adminToken)

	if rec := uiPost(h, "/ui"+uiActionPath,
		form("action", "create", csrfField, token, "key_alias", "doomed"), c); rec.Code != http.StatusSeeOther {
		t.Fatalf("create status %d", rec.Code)
	}
	secret := secretOnPage(t, uiGet(h, "/ui/keys/secret", c).Body.String())
	id, ok := authenticates(h, secret)
	if !ok {
		t.Fatal("the created key does not authenticate")
	}
	inv.taken() // the create deliberately announces nothing (§11.2c)

	rec := uiPost(h, "/ui"+uiActionPath,
		form("action", "delete", "key_id", id, csrfField, token), c)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete status %d\n%s", rec.Code, rec.Body.String())
	}
	if _, still := authenticates(h, secret); still {
		t.Fatal("a deleted key still authenticates")
	}
	msgs := inv.taken()
	if len(msgs) != 1 || msgs[0].keyID != id || msgs[0].cause != CauseRevoked {
		t.Errorf("the deletion announced %+v; the UI must publish what the API publishes", msgs)
	}

	// The operator is told, on the page they land on, and once.
	page := uiGet(h, "/ui/keys", c)
	if !strings.Contains(page.Body.String(), "is deleted") {
		t.Error("the keys screen does not report what just happened")
	}
	if strings.Contains(uiGet(h, "/ui/keys", c).Body.String(), "is deleted") {
		t.Error("the notice survived a refresh; a stale outcome beside a live table is a lie")
	}
}

// Rotation from the UI keeps both secrets alive for the grace, and cutting the
// grace short ends the old one. The states are read from /key/secrets, which is
// the same list an operator reads to answer "did the client roll?".
func TestRotateFromTheUIKeepsBothSecretsForTheGrace(t *testing.T) {
	h, inv := newInvalidatingHarness(t)
	seedAdminSession(h)
	id, first := h.newKey(map[string]any{"key_alias": "rolling"})
	c, token := signInUI(t, h, adminToken)
	inv.taken()

	rec := uiPost(h, "/ui"+uiActionPath,
		form("action", "rotate", "key_id", id, csrfField, token, "grace", "24h"), c)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("rotate status %d\n%s", rec.Code, rec.Body.String())
	}
	page := uiGet(h, "/ui/keys/secret", c)
	second := secretOnPage(t, page.Body.String())
	if second == first {
		t.Fatal("the rotation returned the same secret")
	}
	// The page says when the OLD secret stops, which is the fact the operator
	// has to act on before it does.
	for _, want := range []string{"OLD secret stops working", "grace 24h", id} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("the rotation's secret page does not say %q", want)
		}
	}
	if msgs := inv.taken(); len(msgs) != 1 || msgs[0].cause != CauseRotated {
		t.Errorf("the rotation announced %+v", msgs)
	}

	// Both secrets are attached to the key: the previous one in its grace
	// window, the current one without an expiry of its own.
	secrets := listSecrets(t, h, id)
	if len(secrets) != 2 {
		t.Fatalf("the key has %d secrets, want 2", len(secrets))
	}
	byStatus := map[string]secretView{}
	for _, s := range secrets {
		byStatus[s.Status] = s
	}
	grace, hasGrace := byStatus["grace"]
	current, hasCurrent := byStatus["current"]
	if !hasGrace || !hasCurrent {
		t.Fatalf("the key's secrets are %+v; want one current and one in grace", secrets)
	}
	if want := testNow.Add(24 * time.Hour); !grace.ExpiresAt.Time().Equal(want) {
		t.Errorf("the old secret expires at %v, want %v", grace.ExpiresAt.Time(), want)
	}
	if current.Generation != 2 {
		t.Errorf("the new secret is generation %d, want 2", current.Generation)
	}

	// Cutting the grace ends the old one now, and does not touch the new one:
	// a client that has already rolled must not be locked out by the control
	// that is protecting it.
	rec = uiPost(h, "/ui"+uiActionPath,
		form("action", "cut", "key_id", id, csrfField, token), c)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("cut status %d\n%s", rec.Code, rec.Body.String())
	}
	if msgs := inv.taken(); len(msgs) != 1 || msgs[0].cause != CauseGraceCut {
		t.Errorf("the cut announced %+v", msgs)
	}
	after := listSecrets(t, h, id)
	states := map[string]int{}
	for _, s := range after {
		states[s.Status]++
	}
	if states["revoked"] != 1 || states["current"] != 1 {
		t.Errorf("after the cut the secrets are %+v; want the superseded one revoked and "+
			"the current one untouched", after)
	}
	if _, ok := authenticates(h, second); !ok {
		t.Error("the current secret stopped working when the grace was cut")
	}
}

func listSecrets(t *testing.T, h *harness, id string) []secretView {
	t.Helper()
	rs := h.api.cfg.Keys.(RotatingKeyStore)
	list, err := rs.ListSecrets(context.Background(), id)
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	out := make([]secretView, 0, len(list))
	for _, s := range list {
		out = append(out, viewSecret(s, testNow))
	}
	return out
}

// Block, unblock, pend and release all reach the API's own handlers, so each one
// publishes what that handler publishes. The pair that restores service is one
// click and the pair that withdraws it is two.
func TestTheReversiblePairsRoundTripFromTheUI(t *testing.T) {
	h, inv := newInvalidatingHarness(t)
	seedAdminSession(h)
	id, _ := h.newKey(nil)
	c, token := signInUI(t, h, adminToken)
	inv.taken()

	do := func(action string) {
		t.Helper()
		rec := uiPost(h, "/ui"+uiActionPath,
			form("action", action, "key_id", id, csrfField, token), c)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s status %d\n%s", action, rec.Code, rec.Body.String())
		}
	}
	state := func() (blocked bool, pended bool) {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		k := h.store.keys[id]
		return k.Blocked, !k.PendedAt.IsZero()
	}

	do("block")
	if b, _ := state(); !b {
		t.Fatal("block did nothing")
	}
	if msgs := inv.taken(); len(msgs) != 1 || msgs[0].cause != CauseRevoked {
		t.Errorf("block announced %+v", msgs)
	}
	do("unblock")
	if b, _ := state(); b {
		t.Fatal("unblock did nothing")
	}
	if msgs := inv.taken(); len(msgs) != 1 || msgs[0].cause != CauseUpdated {
		t.Errorf("unblock announced %+v", msgs)
	}

	do("pend")
	if _, p := state(); !p {
		t.Fatal("pend did nothing")
	}
	if msgs := inv.taken(); len(msgs) != 1 || msgs[0].cause != CausePended {
		t.Errorf("pend announced %+v", msgs)
	}
	do("release")
	if _, p := state(); p {
		t.Fatal("release did nothing")
	}
	if msgs := inv.taken(); len(msgs) != 1 || msgs[0].cause != CauseReleased {
		t.Errorf("release announced %+v", msgs)
	}
}

// A refusal from the administration surface is RENDERED, with its own status,
// rather than redirected: 404 and 403 and 501 are different answers and a 303
// would flatten all of them.
func TestAFailedActionRendersItsOwnRefusal(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, token := signInUI(t, h, adminToken)

	rec := uiPost(h, "/ui"+uiActionPath,
		form("action", "block", "key_id", "no-such-key", csrfField, token), c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), CodeNotFound) {
		t.Error("the failure page does not carry the code")
	}
}

// A control for an optional half of the key store is not offered where there is
// nothing behind it. An operator clicking a permanent 501 learns that the
// product is broken, not that a dependency is optional.
func TestControlsAreNotOfferedForAbsentDependencies(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Hasher = nil
		c.Keys = plainKeyStore{c.Keys}
	})
	seedAdminSession(h)
	seedKey(h, &Key{ID: "k1", KeyLabel: "dk-plain", CreatedAt: testNow})
	c, token := signInUI(t, h, adminToken)

	body := uiGet(h, "/ui/keys", c).Body.String()
	for _, absent := range []string{"new key", ">rotate<", "cut&nbsp;grace", ">pend<"} {
		if strings.Contains(body, absent) {
			t.Errorf("the screen offers %q against a store that cannot do it", absent)
		}
	}
	// Block and delete are the two the plain store can always do.
	if !strings.Contains(body, "action=delete") {
		t.Error("the screen dropped the controls that DO work")
	}
	// And asking anyway is refused by name rather than by a panic or a 500.
	rec := uiPost(h, "/ui"+uiActionPath, form("action", "rotate", "key_id", "k1", csrfField, token), c)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("rotate against a non-rotating store = %d, want 501", rec.Code)
	}
	rec = uiGet(h, "/ui/keys/new", c)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("the create form with no pepper = %d, want 501", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// What a browser found in the API's own handlers
// ---------------------------------------------------------------------------

// A body the surface will not read is REFUSED, not ignored.
//
// This is the one thing giving the UI mutations turned up in the administration
// handlers themselves, and it is here rather than in keys_test.go because a
// browser is what found it: a script posts JSON and a browser posts
// `application/x-www-form-urlencoded`, and `decodeOptionalBody` used to return
// nil for anything that was not JSON. The sharp case is the second one below —
// the id resolves from the query string, so nothing is missing, the body's
// `grace: 0` is discarded, and an operator who asked for an immediate cut on the
// incident path of OPERATIONS §3.1 gets 200 and a 24-hour grace.
//
// The UI never posts a form to a handler — [uiServer.invoke] builds JSON — so
// this is a hazard closed rather than a defect that shipped. It stays closed
// because of this test.
func TestANonJSONBodyIsRefusedRatherThanIgnored(t *testing.T) {
	h := newHarness(t)
	id, _ := h.newKey(nil)

	for _, tc := range []struct{ name, path, body string }{
		{"a form body where the id is missing without it", "/key/block", "key_id=" + id},
		{"a form body whose grace would silently not apply", "/key/rotate?key_id=" + id, "grace=0"},
		{"a form body on the route that mints", "/key/generate", "key_alias=x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Authorization", "Bearer "+masterToken)
			rec := httptest.NewRecorder()
			h.api.ServeHTTP(rec, req)
			h.expectFault(rec, http.StatusUnsupportedMediaType, CodeUnsupportedMedia)
		})
	}

	// The leniency this replaced is kept where it was actually for: a POST whose
	// parameters are all in the query string, with no body at all, still works
	// whatever Content-Type it declares.
	req := httptest.NewRequest(http.MethodPost, "/key/block?key_id="+id, nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+masterToken)
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a bodyless query-string call = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// The half of the model that did not change
// ---------------------------------------------------------------------------

// cookieCredulousAuth is an authenticator that WOULD accept the UI's session
// cookie, if it were ever given one.
//
// It exists so that "the API never accepts the cookie" is tested rather than
// assumed. With the ordinary fake — which reads only Authorization — the
// assertion would pass on an API that handed the cookie straight through, and
// that is now the only structural half of the security model left.
type cookieCredulousAuth struct{ cookie string }

func (a cookieCredulousAuth) AuthenticateHeader(ctx context.Context, h http.Header) (Principal, error) {
	for _, v := range h.Values("Cookie") {
		if a.cookie != "" && strings.Contains(v, a.cookie) {
			return fakePrincipal{kind: "master", admin: true, scope: GlobalScope()}, nil
		}
	}
	return fakeAuth{}.AuthenticateHeader(ctx, h)
}

// The API refuses the UI's cookie, on every route, including the ones that mint
// and the ones that destroy.
//
// Revert the header copy in [API.authenticate] and this fails loudly: the
// authenticator underneath is one that accepts the cookie happily.
func TestTheAPIStillRefusesTheUICookie(t *testing.T) {
	var credulous cookieCredulousAuth
	h := newHarness(t, func(c *Config) { c.Auth = &credulous })
	c := signIn(t, h, masterToken)
	credulous.cookie = c.Value

	// The premise: this authenticator really would accept it.
	if p, err := credulous.AuthenticateHeader(context.Background(),
		http.Header{"Cookie": {c.Name + "=" + c.Value}}); err != nil || p == nil {
		t.Fatal("the test's own authenticator does not accept the cookie; it proves nothing")
	}

	for _, path := range []string{
		"/key/list", "/key/info", "/key/generate", "/key/delete", "/key/block",
		"/key/rotate", "/key/rotate/cut", "/key/pend", "/key/release", "/admin/status",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := httptest.NewRequest(method, path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(c)
			rec := httptest.NewRecorder()
			h.api.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with only the UI cookie = %d, want 401 — the API must "+
					"never accept the cookie", method, path, rec.Code)
			}
		}
	}

	// And the same cookie still works where it is supposed to.
	if rec := uiGet(h, "/ui/keys", c); rec.Code != http.StatusOK {
		t.Fatalf("the cookie stopped working on the UI: %d", rec.Code)
	}
}

// The UI's own fallback to header authentication goes through the same
// stripping, so a cookie cannot authenticate a screen either.
func TestTheUIsHeaderFallbackDoesNotAcceptTheCookie(t *testing.T) {
	var credulous cookieCredulousAuth
	h := newHarness(t, func(c *Config) {
		c.Auth = &credulous
		c.DisableUISessions = true
	})
	// A cookie value the credulous authenticator would take, with sessions off
	// so the only path left is the header one.
	credulous.cookie = "sentinel"
	req := httptest.NewRequest(http.MethodGet, "/ui/keys", nil)
	req.AddCookie(&http.Cookie{Name: DefaultSessionCookie, Value: "sentinel"})
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a cookie authorized a screen through the header path: %d", rec.Code)
	}
}
