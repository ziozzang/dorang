package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// refusingLedger mirrors adminLedger's contract, which the shared fakeStore
// does not: an unfiltered window is refused (the real store has no all-rows
// index — DESIGN §9.3), while the errors-only window is served from its own
// partial index. The monitoring screen's whole reason to read ErrorsOnly is
// that this refusal is real, so its test must be run against a ledger that
// enforces it — a permissive fake would let the screen ship the unfiltered
// query that failed live and never notice.
type refusingLedger struct{ rows []LogRow }

func (l refusingLedger) ListRequests(_ context.Context, q LogQuery) (LogPage, error) {
	if q.Range.Start.IsZero() || q.Range.End.IsZero() || !q.Range.End.After(q.Range.Start) {
		return LogPage{}, ErrUnboundedRange
	}
	// The store errors on a page above its maximum rather than clamping it, so
	// the fake does too: a monitoring read that asks for more than the store
	// allows must fail here, not be silently served, or the limit regression
	// that shipped the "could not be read" banner would pass the test again.
	if q.Limit > DefaultMaxPageSize {
		return LogPage{}, ErrUnsupported
	}
	if !q.ErrorsOnly && q.KeyID == "" && q.TeamID == "" && q.TraceID == "" && q.Tag == "" && q.UserID == "" {
		return LogPage{}, ErrUnsupported
	}
	var out []LogRow
	for _, r := range l.rows {
		if r.TS.Before(q.Range.Start) || !r.TS.Before(q.Range.End) {
			continue
		}
		if q.ErrorsOnly && r.Status < 400 {
			continue
		}
		out = append(out, r)
	}
	return LogPage{Rows: out}, nil
}

func (l refusingLedger) Report(context.Context, ReportQuery) (Report, error) {
	return Report{}, ErrUnsupported
}

type fakeSurface struct{ s Surface }

func (f fakeSurface) Surface(context.Context) (Surface, error) { return f.s, nil }

// The real-time pulse renders this node's live HTTP counters — the digest of
// GET /metrics — including the node id it belongs to and a pointer to the
// scrape endpoint. Without the node id the numbers are meaningless behind a
// balancer, so it is part of what the section must show.
func TestMonitoringShowsThisNodeLivePulse(t *testing.T) {
	sf := fakeSurface{s: Surface{
		NodeID: "dorang-7", Requests: 4210, Class2xx: 4000, Class4xx: 200, Class5xx: 10,
		InFlight: 3, AvgLatencyMS: 42, UptimeSeconds: 3*3600 + 12*60, Ready: true, MetricsPath: "/metrics",
	}}
	h := newHarness(t, func(c *Config) { c.Surface = sf })
	seedAdminSession(h)
	c, _ := signInUI(t, h, adminToken)
	rec := uiGet(h, "/ui/monitoring", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/monitoring = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Live", "dorang-7", "4,210", "3h 12m", "/metrics"} {
		if !strings.Contains(body, want) {
			t.Errorf("the live pulse is missing %q", want)
		}
	}
}

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
	for _, want := range []string{"Monitoring", `data-live="10"`, "Recent failures — last 5m", `href="/ui/monitoring"`} {
		if !strings.Contains(body, want) {
			t.Errorf("monitoring screen missing %q", want)
		}
	}
}

// A failure inside the window renders, and a success in the same window does
// not — the view is the errors-only read, the one windowed ledger query a
// no-filter "what is happening now" page can make.
//
// Revert check: point the handler back at an unfiltered ListRequests and the
// fake refuses it the way the real store does (ErrUnsupported). The live
// region then shows "could not be read" and none of these rows render, so the
// assertions below fail — which is the §17.1 consequence this test observes.
func TestMonitoringShowsRecentFailures(t *testing.T) {
	led := refusingLedger{rows: []LogRow{
		{ID: "f1", TS: testNow.Add(-2 * time.Minute),
			ModelGroup: "gpt-4o", ProviderID: "openai", Endpoint: "chat_completions",
			Status: 502, LatencyMS: 1200},
		{ID: "ok1", TS: testNow.Add(-1 * time.Minute),
			ModelGroup: "claude-sonnet", ProviderID: "anthropic", Endpoint: "messages",
			Status: 200, LatencyMS: 80},
	}}
	h := newHarness(t, func(c *Config) { c.Ledger = led })
	seedAdminSession(h)
	c, _ := signInUI(t, h, adminToken)
	rec := uiGet(h, "/ui/monitoring", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/monitoring = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "could not be read") {
		t.Fatalf("the failures ledger read errored:\n%s", body)
	}
	for _, want := range []string{"gpt-4o", "openai", "502"} {
		if !strings.Contains(body, want) {
			t.Errorf("the in-window failure did not render: missing %q", want)
		}
	}
	if strings.Contains(body, "claude-sonnet") {
		t.Error("a successful request appeared on the failures-only view")
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
		if strings.Contains(rec.Body.String(), "Recent failures — last") {
			t.Errorf("a team-scoped viewer saw the deployment monitor")
		}
	}
}
