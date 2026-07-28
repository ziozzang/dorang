package admin

import (
	"net/http"
	"strings"
	"testing"
)

// shapeCompatiblePaths is DESIGN §2.3's list, written out so that a route
// silently disappearing is a test failure rather than a support ticket.
var shapeCompatiblePaths = []string{
	"/key/generate", "/key/info", "/key/update", "/key/delete",
	"/key/list", "/key/block", "/key/unblock", "/key/regenerate",
	"/user/new", "/user/info", "/user/update", "/user/delete", "/user/list",
	"/team/new", "/team/info", "/team/update", "/team/delete", "/team/list",
	"/team/member_add", "/team/member_delete",
	"/model/new", "/model/info", "/model/update", "/model/delete",
	"/model_group/info",
	"/budget/new", "/budget/info", "/budget/update", "/budget/delete", "/budget/list",
	"/spend/logs", "/spend/calculate",
	"/global/spend/report",
	"/user/daily/activity", "/team/daily/activity", "/tag/daily/activity",
	"/health/history",
}

func TestEveryShapeCompatiblePathIsRegistered(t *testing.T) {
	h := newHarness(t)
	for _, p := range shapeCompatiblePaths {
		if _, ok := h.api.routes[p]; !ok {
			t.Errorf("DESIGN §2.3 path %q is not registered", p)
		}
	}
}

// A route dorang has not built answers 501 with a code — never a silent 404
// (DESIGN §0.2). This is the difference between "we have not implemented that"
// and "you typed the URL wrong", and only the code carries it.
func TestUnknownRouteIs501NotFound(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/nope", "/key/nonsense", "/v2/key/list", "/"} {
		rec := h.do(http.MethodPost, p, nil)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s answered 404; §0.2 requires 501 with a reason", p)
		}
		h.expectFault(rec, http.StatusNotImplemented, CodeNotImplemented)
	}
}

// The named stubs carry their own code, so a caller learns which concept is
// missing rather than only that something is.
func TestStubbedSurfacesCarrySpecificCodes(t *testing.T) {
	h := newHarness(t)
	for path, code := range map[string]string{
		"/organization/new":   "organizations_unsupported",
		"/organization/list":  "organizations_unsupported",
		"/customer/info":      "customers_unsupported",
		"/key/health":         "key_health_unsupported",
		"/global/spend/reset": "spend_reset_unsupported",
		"/spend/tags":         "spend_tags_unsupported",
		"/model/settings":     "model_settings_unsupported",
		"/cache/flushall":     "cache_admin_unsupported",
		"/budget/settings":    "budget_settings_unsupported",
		"/audit/list":         "audit_read_unsupported",
	} {
		rec := h.do(http.MethodPost, path, nil)
		body := h.expectFault(rec, http.StatusNotImplemented, code)
		msg, _ := body["error"].(map[string]any)["message"].(string)
		if strings.TrimSpace(msg) == "" {
			t.Errorf("%s: 501 with no reason", path)
		}
	}
}

// A stub is still authorized: a caller with no credential must not be able to
// enumerate which surfaces exist.
func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/key/list", "/user/list", "/spend/logs", "/admin/status"} {
		rec := h.do(http.MethodGet, p, nil, asToken(""))
		h.expectFault(rec, http.StatusUnauthorized, CodeUnauthorized)
	}
}

// A valid non-administrative credential is a 403, not a 401: the caller is who
// they say they are and still may not do this.
func TestNonAdminCredentialIsForbidden(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/key/list", "/key/generate", "/user/list", "/admin/capacity"} {
		rec := h.do(http.MethodPost, p, nil, asToken(userToken))
		h.expectFault(rec, http.StatusForbidden, CodeForbidden)
	}
}

func TestAdminRoleKeyIsAccepted(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/key/list", nil, asToken(adminToken))
	h.expectStatus(rec, http.StatusOK)
}

func TestMethodNotAllowedCarriesAllow(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodDelete, "/key/generate", nil)
	h.expectFault(rec, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("Allow = %q, want it to mention POST", allow)
	}
}

func TestTrailingSlashIsTheSameRoute(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/key/list/", nil)
	h.expectStatus(rec, http.StatusOK)
}

func TestOptionsReportsAllowedMethods(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodOptions, "/key/info", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	for _, m := range []string{"GET", "POST", "OPTIONS"} {
		if !strings.Contains(allow, m) {
			t.Errorf("Allow = %q, want it to mention %s", allow, m)
		}
	}
}

// An absent optional dependency answers 501 naming the dependency, rather than
// panicking or returning an empty result that reads like "there is nothing
// there".
func TestAbsentDependencyIs501WithName(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Keys = nil
		c.Ledger = nil
		c.Capacity = nil
	})
	for _, p := range []string{"/key/list", "/spend/logs", "/admin/capacity"} {
		rec := h.do(http.MethodGet, p+"?start_date=2026-07-01&end_date=2026-07-02", nil)
		body := h.expectFault(rec, http.StatusNotImplemented, CodeDependencyOff)
		detail, _ := body["error"].(map[string]any)["detail"].(map[string]any)
		if detail["dependency"] == nil {
			t.Errorf("%s: 501 does not name the missing dependency", p)
		}
	}
}

// Without an auditor every mutation is refused before it happens, so the
// promise that everything is audited cannot be quietly broken by a
// misconfiguration.
func TestMutationsRefusedWithoutAuditor(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Audit = nil })
	for _, p := range []string{"/key/generate", "/user/new", "/team/new", "/model/new", "/budget/new"} {
		rec := h.do(http.MethodPost, p, map[string]any{})
		h.expectFault(rec, http.StatusNotImplemented, CodeDependencyOff)
	}
	if len(h.store.keys) != 0 {
		t.Errorf("a key was created despite the audit refusal")
	}
}

func TestConfigRequiresAuth(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted a Config with no Authenticator")
	}
}

func TestMalformedBodyIsRefused(t *testing.T) {
	h := newHarness(t)
	req := h.do(http.MethodPost, "/user/new", nil, func(r *http.Request) {
		r.Body = http.NoBody
		r.Header.Set("Content-Type", "application/json")
		r.ContentLength = 3
		r.Body = readCloser("{,,")
	})
	h.expectFault(req, http.StatusBadRequest, CodeInvalidRequest)
}

func readCloser(s string) interface {
	Read([]byte) (int, error)
	Close() error
} {
	return struct {
		*strings.Reader
		closer
	}{strings.NewReader(s), closer{}}
}

type closer struct{}

func (closer) Close() error { return nil }
