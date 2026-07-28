package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/ui"
)

// The embedded UI must actually parse. Parsing happens in the constructor, so a
// broken template is a startup failure rather than a 500 discovered by an
// operator mid-incident — and this test is what makes that claim true.
func TestEmbeddedTemplatesParse(t *testing.T) {
	h := newHarness(t)
	if h.api.ui == nil {
		t.Fatal("no UI server was built")
	}
	for _, name := range ui.Pages() {
		if _, ok := h.api.ui.pages[name]; !ok {
			t.Errorf("page %q was not parsed", name)
		}
	}
}

func TestEmbeddedAssetsAreSelfContained(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ path, contentType, must string }{
		{"/ui/assets/style.css", "text/css", "prefers-color-scheme"},
		{"/ui/assets/app.js", "text/javascript", "data-theme"},
	} {
		rec := h.do(http.MethodGet, tc.path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
			t.Errorf("%s: Content-Type = %q", tc.path, ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, tc.must) {
			t.Errorf("%s does not contain %q", tc.path, tc.must)
		}
		// §11.3: works offline, no external assets. An asset that reaches for
		// a URL is an external asset however it is spelled.
		for _, bad := range []string{"http://", "https://", "//cdn.", "@import"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s references %q; the UI must work offline", tc.path, bad)
			}
		}
	}
}

func TestThreeScreensRender(t *testing.T) {
	h := newHarness(t, withReporters)
	seedLedger(h)
	h.newKey(map[string]any{"key_alias": "ci", "max_budget": 10, "tags": []string{"prod"}})
	newDeployment(t, h, "chat", "prov-a", "a/model")
	h.store.aliases = []Alias{{Alias: "chat-latest", ModelGroup: "chat"}}

	for _, tc := range []struct {
		path string
		must []string
	}{
		{"/ui/keys", []string{"<title>Keys", ">ci<", "dk-", "aria-current"}},
		{"/ui/models", []string{"Models &amp; deployments", "a/model", "chat-latest", "cred-a", "prov-a"}},
		{"/ui/usage", []string{"cost (billed)", "notional (list rate)", "leverage", "By model group"}},
	} {
		rec := h.do(http.MethodGet, tc.path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d\n%s", tc.path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.HasPrefix(strings.TrimSpace(body), "<!DOCTYPE html>") {
			t.Errorf("%s did not render a document", tc.path)
		}
		for _, want := range tc.must {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not contain %q", tc.path, want)
			}
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type = %q", tc.path, ct)
		}
	}
}

// The keys screen must not render a secret. It cannot, because it renders the
// same label-only type the API does — but the screen is where an operator would
// notice a regression last, so it is checked.
func TestKeysScreenShowsNoSecret(t *testing.T) {
	h := newHarness(t)
	_, token := h.newKey(nil)
	rec := h.do(http.MethodGet, "/ui/keys", nil)
	assertNoSecret(t, "ui/keys", rec.Body.String(), token)
}

// §8.5 exists so that a subscription's value is visible, which means cost and
// the notional figure appear together on the same screen rather than one being
// a click away.
func TestUsageScreenShowsCostAndNotionalTogether(t *testing.T) {
	h := newHarness(t)
	h.store.addLog(LogRow{
		ID: "a", TS: testNow.Add(-time.Hour), ModelGroup: "chat", TeamID: "t1", Status: 200,
		CostNano: 1_000_000_000, NotionalNano: 4_000_000_000, NotionalKnown: true,
	})
	rec := h.do(http.MethodGet, "/ui/usage", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, body)
	}
	cost := strings.Index(body, "cost (billed)")
	notional := strings.Index(body, "notional (list rate)")
	if cost < 0 || notional < 0 {
		t.Fatal("the usage screen does not show both figures")
	}
	if notional < cost {
		t.Error("the notional figure is rendered before the billed cost")
	}
	if !strings.Contains(body, "4.00×") {
		t.Errorf("leverage was not computed: %s", body[max(0, notional-200):min(len(body), notional+900)])
	}
	if !strings.Contains(body, "never billed") {
		t.Error("the notional figure is not labelled as an estimate")
	}
}

func TestUsageScreenReportsMissingNotional(t *testing.T) {
	h := newHarness(t)
	h.store.addLog(LogRow{ID: "a", TS: testNow.Add(-time.Hour), ModelGroup: "chat",
		Status: 200, CostNano: 1_000_000_000, NotionalKnown: false})
	rec := h.do(http.MethodGet, "/ui/usage", nil)
	body := rec.Body.String()
	if !strings.Contains(body, "unavailable") {
		t.Error("a missing notional figure was not shown as unavailable")
	}
	// The unavailability must be inside the notional tile, not merely somewhere
	// on the page: a zero in that tile is exactly the flattering answer §8.5
	// forbids.
	i := strings.Index(body, "notional (list rate)")
	if i < 0 {
		t.Fatal("no notional tile")
	}
	tile := body[i:min(len(body), i+400)]
	if !strings.Contains(tile, "unavailable") {
		t.Errorf("the notional tile does not report unavailability: %s", tile)
	}
	if strings.Contains(tile, `class="value">0`) {
		t.Errorf("a missing notional figure was rendered as zero: %s", tile)
	}
}

func TestUsageScreenRefusesATooWideWindow(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxTimeRange = 48 * time.Hour })
	rec := h.do(http.MethodGet, "/ui/usage?start_date=2026-01-01&end_date=2026-07-01", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "wider than this deployment allows") {
		t.Errorf("no explanation: %s", rec.Body.String())
	}
}

func TestUIIsReadOnly(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/ui/keys", "/ui/models", "/ui/usage"} {
		rec := h.do(http.MethodPost, p, map[string]any{})
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405; the UI must not mutate", p, rec.Code)
		}
	}
}

func TestUISecurityHeaders(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/ui/keys", nil)
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP = %q, want it to contain %q", csp, want)
		}
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("no nosniff header")
	}
}

func TestUIUnknownScreenIs501(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/ui/batches", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestUIRootRedirects(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/ui", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/keys" {
		t.Fatalf("status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
}

// A browser cannot send a bearer header on a navigation, so an unauthenticated
// visitor is sent to a sign-in form rather than to a bare 401.
func TestUnauthenticatedUIRedirectsToLogin(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/ui/keys", nil, asToken(""))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/login") {
		t.Fatalf("Location = %q", loc)
	}
}

func TestUILoginIssuesASessionAndLogoutRevokesIt(t *testing.T) {
	h := newHarness(t)

	form := strings.NewReader("key=" + masterToken + "&next=/ui/usage")
	req := httptest.NewRequest(http.MethodPost, "/ui/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/usage" {
		t.Errorf("Location = %q, want the requested screen", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie")
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/ui" {
		t.Errorf("session cookie is not locked down: %+v", c)
	}

	// The cookie authorizes the UI...
	rec2 := h.do(http.MethodGet, "/ui/keys", nil, asToken(""), func(r *http.Request) {
		r.AddCookie(c)
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("cookie did not authorize the UI: %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "sign out") {
		t.Error("a cookie session should offer a sign-out control")
	}

	// ...and nothing else. The administration API never accepts it, which is
	// what makes the read-only UI free of cross-site request forgery risk.
	rec3 := h.do(http.MethodGet, "/key/list", nil, asToken(""), func(r *http.Request) {
		r.AddCookie(c)
	})
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("the API accepted a UI cookie: %d", rec3.Code)
	}

	// Signing out revokes it immediately.
	out := h.do(http.MethodPost, "/ui/logout", nil, asToken(""), func(r *http.Request) {
		r.AddCookie(c)
	})
	if out.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d", out.Code)
	}
	rec4 := h.do(http.MethodGet, "/ui/keys", nil, asToken(""), func(r *http.Request) {
		r.AddCookie(c)
	})
	if rec4.Code != http.StatusSeeOther {
		t.Fatalf("a revoked session still authorized: %d", rec4.Code)
	}
}

func TestUILoginRejectsNonAdminCredential(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/ui/login",
		strings.NewReader("key="+userToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a session was issued to a non-administrative credential")
	}
}

// An open redirect through ?next= would turn the sign-in page into a phishing
// hop, so anything outside the UI's own mount is discarded.
func TestLoginNextIsConfinedToTheUI(t *testing.T) {
	for _, next := range []string{
		"https://evil.example/", "//evil.example/", "/key/list", "/ui/keys\nX", "",
	} {
		if got := safeNext("/ui", next); !strings.HasPrefix(got, "/ui/") || got == "/ui//" {
			t.Errorf("safeNext(%q) = %q", next, got)
		}
	}
	if got := safeNext("/ui", "/ui/usage?start_date=2026-07-01"); got != "/ui/usage?start_date=2026-07-01" {
		t.Errorf("safeNext discarded a legitimate destination: %q", got)
	}
}

func TestSessionExpires(t *testing.T) {
	now := testNow
	h := newHarness(t, func(c *Config) {
		c.SessionTTL = time.Minute
		c.Now = func() time.Time { return now }
	})
	id := h.api.ui.newSession(fakePrincipal{kind: "master", admin: true})
	if _, ok := h.api.ui.lookupSession(id); !ok {
		t.Fatal("a fresh session did not resolve")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := h.api.ui.lookupSession(id); ok {
		t.Fatal("an expired session still resolved")
	}
}

func TestUICanBeDisabled(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.UIPrefix = "-" })
	if h.api.ui != nil {
		t.Fatal("the UI was built despite being disabled")
	}
	rec := h.do(http.MethodGet, "/ui/keys", nil)
	h.expectFault(rec, http.StatusNotImplemented, CodeNotImplemented)
}

func TestUIWithoutSessionsRequiresAHeader(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DisableUISessions = true })
	rec := h.do(http.MethodGet, "/ui/keys", nil, asToken(""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	rec = h.do(http.MethodGet, "/ui/keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("header auth did not work: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sign out") {
		t.Error("a header-authenticated view offered a sign-out control")
	}
}

func TestScreensDegradeWhenADependencyIsAbsent(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Keys = nil
		c.Models = nil
		c.Ledger = nil
	})
	for _, p := range []string{"/ui/keys", "/ui/models", "/ui/usage"} {
		rec := h.do(http.MethodGet, p, nil)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s = %d, want 501", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "not configured") {
			t.Errorf("%s does not say what is missing", p)
		}
	}
}

func TestHumanInt(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 12: "12", 1234: "1,234", 1234567: "1,234,567", -4321: "-4,321",
	} {
		if got := humanInt(in); got != want {
			t.Errorf("humanInt(%d) = %q, want %q", in, got, want)
		}
	}
}
