package admin

import (
	"net/http"
	"strings"
	"testing"
)

// The monitoring screen renders and marks a live region for auto-refresh.
func TestMonitoringScreenRenders(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, _ := signInUI(t, h, adminToken)
	rec := uiGet(h, "/ui/monitoring", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/monitoring = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Monitoring", `data-live="10"`, "Traffic — last 5m", `href="/ui/monitoring"`} {
		if !strings.Contains(body, want) {
			t.Errorf("monitoring screen missing %q", want)
		}
	}
}

// The auto-refresh fetch of the same URL returns the live region, which is
// what app.js swaps in — so a poll is a normal authorized GET, not a special
// endpoint.
func TestMonitoringRefetchReturnsTheLiveRegion(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, _ := signInUI(t, h, adminToken)
	rec := uiGet(h, "/ui/monitoring", c)
	if !strings.Contains(rec.Body.String(), `data-live`) {
		t.Fatal("no live region to refetch")
	}
}

// A team-scoped session cannot see the deployment-wide monitor.
func TestMonitoringIsGlobalOnly(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/ui/monitoring", nil, asToken(teamAToken))
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusSeeOther {
		// a team viewer is either refused (403) or bounced to login depending
		// on how the token authorizes; either way it must not render the page.
		if strings.Contains(rec.Body.String(), "Traffic — last") {
			t.Errorf("a team-scoped viewer saw the deployment monitor")
		}
	}
}
