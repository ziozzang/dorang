package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type consoleMetrics string

func (m consoleMetrics) Metrics(dst []byte) []byte { return append(dst, string(m)...) }

func TestTrafficBufferBoundsAndCacheMetadata(t *testing.T) {
	b := &TrafficBuffer{}
	for i := 0; i < 300; i++ {
		b.Record(LiveRequest{At: testNow, ID: strings.Repeat("x", 2000), Model: "chat", Input: 100, Output: 20, CacheRead: 60, CacheWrite: 5, Reasoning: 8})
	}
	rows := b.Recent(testNow)
	if len(rows) != 256 || rows[0].Sequence != 300 || rows[255].Sequence != 45 || len(rows[0].ID) != 256 {
		t.Fatalf("unbounded or unordered buffer: %d", len(rows))
	}
	if rows[0].Input != 100 || rows[0].CacheRead != 60 || rows[0].CacheWrite != 5 || rows[0].Reasoning != 8 {
		t.Fatal("token dimensions lost")
	}
	if len(b.Recent(testNow.Add(16*time.Minute))) != 0 {
		t.Fatal("expired metadata retained in response")
	}
}

func TestConsoleViewsHonorScopeAndHeaderReads(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Prometheus = consoleMetrics("# TYPE test gauge\ntest 3\n")
		c.Traffic = &TrafficBuffer{}
		c.Pricing = fakePricer{}
		c.Catalog = fakeCatalog{}
		c.Setup = &fakeSetup{}
	})
	for screen := range consoleTitles {
		if got := h.do(http.MethodGet, "/ui/"+screen, nil); got.Code != 200 {
			t.Errorf("%s=%d %s", screen, got.Code, got.Body.String())
		}
		if got := h.do(http.MethodGet, "/ui/"+screen, nil, asToken(teamAToken)); got.Code != 403 {
			t.Errorf("scoped %s=%d", screen, got.Code)
		}
	}
	for _, path := range []string{"/ui/events", "/admin/telemetry", "/admin/requests/recent"} {
		if got := h.do(http.MethodGet, path, nil, asToken(teamAToken)); got.Code != 403 {
			t.Errorf("scoped %s=%d", path, got.Code)
		}
	}
}

func TestTelemetrySSEExpiresAndDoesNotAcceptAnonymous(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Prometheus = consoleMetrics("# TYPE cache_read counter\ncache_read 60\n")
		c.Traffic = &TrafficBuffer{}
	})
	cookie, _ := signInUI(t, h, masterToken)
	h.api.cfg.Traffic.Record(LiveRequest{At: testNow, ID: "req-cached", Input: 100, CacheRead: 60, CacheWrite: 5, Reasoning: 8})
	srv := httptest.NewServer(h.api)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/ui/events", nil)
	req.AddCookie(cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal("not an unbuffered SSE stream")
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 4096), 5<<20)
	gotFrame := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var snap telemetrySnapshot
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &snap); err != nil {
				t.Fatal(err)
			}
			if len(snap.Requests) != 1 || snap.Requests[0].CacheRead != 60 || !strings.Contains(snap.Prometheus, "cache_read 60") {
				t.Fatal("SSE lost live data")
			}
			gotFrame = true
			break
		}
	}
	if !gotFrame {
		t.Fatal("no initial frame")
	}
	h.api.ui.dropSession(cookie.Value)
	expired := false
	for scanner.Scan() {
		if scanner.Text() == "event: expired" {
			expired = true
			break
		}
	}
	if !expired {
		t.Fatal("revoked session kept streaming")
	}
}

func TestBudgetConsoleMutationIsAuditedAndCSRFProtected(t *testing.T) {
	h := newHarness(t)
	keyID, _ := h.newKey(nil)
	cookie, csrf := signInUI(t, h, masterToken)
	form := url.Values{"action": {"budget_create"}, "subject_kind": {"key"}, "subject_id": {keyID}, "max_budget": {"12.34"}, "soft_budget": {"10"}, "budget_duration": {"monthly"}, "return": {"/ui/budgets"}}
	if got := uiPost(h, "/ui"+uiActionPath, form, cookie); got.Code != 403 {
		t.Fatalf("forgery=%d", got.Code)
	}
	form.Set("csrf", csrf)
	rec := uiPost(h, "/ui"+uiActionPath, form, cookie)
	if rec.Code != 303 {
		t.Fatalf("budget write=%d %s", rec.Code, rec.Body.String())
	}
	body := h.do(http.MethodGet, "/budget/info?subject_kind=key&subject_id="+keyID, nil)
	if !strings.Contains(body.Body.String(), `"max_budget":12.34`) {
		t.Fatalf("budget not applied: %s", body.Body.String())
	}
}

// The parser tests above use fixture values; this checks anonymous requests
// never receive the real stream, even when the HTTP client follows redirects.
func TestAnonymousTelemetryDoesNotStream(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(h.api)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/ui/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.Header.Get("Content-Type") == "text/event-stream" || !strings.Contains(string(body), "Sign in") {
		t.Fatal("anonymous telemetry access")
	}
}
