package app

import (
	"encoding/json"
	"errors"
	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/server"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// Opt-in, localhost-only browser fixture assembled with the real app, SQLite,
// session auth, metrics registry and administrative handlers. No provider calls.
func TestConsoleBrowserFixture(t *testing.T) {
	path := os.Getenv("DORANG_CONSOLE_FIXTURE")
	if path == "" {
		t.Skip("browser fixture is opt-in")
	}
	a := setupTestApp(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer browser-key-1" || r.Header.Get("Authorization") == "Bearer browser-key-2" {
			_, _ = w.Write([]byte(`{"data":[{"id":"browser-model"}]}`))
		} else {
			w.WriteHeader(401)
		}
	}))
	defer upstream.Close()
	var seq int
	emit := func() {
		seq++
		a.recordMetrics(&server.Event{RequestID: "fixture-request", Model: "m1", Route: "chat_completions", Status: 200, DurationNS: 120_000_000, Result: server.Result{Provider: "p1", Credential: "c1", Deployment: "fixture-deployment", TTFTNS: 40_000_000, Tokens: server.Usage{Input: 100, Output: 20, CacheRead: 60, CacheWrite: 5, Reasoning: 8}, Priced: true, CostNanoUSD: 320000}})
	}
	emit()
	srv := httptest.NewServer(a.Server)
	defer srv.Close()
	raw, _ := json.Marshal(map[string]string{"url": srv.URL, "token": testMasterKey, "upstream_url": upstream.URL + "/v1"})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	done := time.NewTimer(10 * time.Minute)
	defer done.Stop()
	for {
		select {
		case <-ticker.C:
			emit()
		case <-done.C:
			return
		}
	}
}

func TestTelemetryIsWiredToAppMetricsAndRequests(t *testing.T) {
	a := newWiringApp(t, pricedYAML, nil)
	a.recordMetrics(&server.Event{RequestID: "cached-real-observer", Model: "m1", Route: "chat_completions", Status: 200, Result: server.Result{Provider: "p1", Credential: "c1", Deployment: "d1", Tokens: server.Usage{Input: 100, Output: 20, CacheRead: 60, CacheWrite: 5, Reasoning: 8}}})
	w := callWith(a, testMasterKey, http.MethodGet, "/admin/telemetry", "")
	if w.Code != 200 {
		t.Fatalf("telemetry=%d", w.Code)
	}
	var data struct {
		Prometheus string `json:"prometheus"`
		Requests   []struct {
			CacheRead  int64 `json:"cache_read"`
			CacheWrite int64 `json:"cache_write"`
			Reasoning  int64 `json:"reasoning"`
		} `json:"requests"`
		Credentials []any `json:"credentials"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Requests) != 1 || data.Requests[0].CacheRead != 60 || data.Requests[0].CacheWrite != 5 || data.Requests[0].Reasoning != 8 || len(data.Credentials) != 1 {
		t.Fatalf("lost observer metadata: %s", w.Body.String())
	}
}

func TestTelemetryNodeIdentityDoesNotChangeBetweenSamples(t *testing.T) {
	a := newWiringApp(t, pricedYAML, nil)
	reporter := &adminSurface{a: a}
	first, err := reporter.Surface(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := reporter.Surface(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.NodeID == "" || first.NodeID != second.NodeID || first.NodeID != a.Node.ID() {
		t.Fatalf("unstable telemetry node: %q -> %q", first.NodeID, second.NodeID)
	}
}

func TestTelemetryBudgetCapabilitiesMatchSchema(t *testing.T) {
	a := newWiringApp(t, pricedYAML, nil)
	b := &adminBudgets{st: a.Store, now: a.now}
	soft := int64(10)
	err := b.SetBudget(t.Context(), admin.Budget{Subject: admin.BudgetSubject{Kind: "user", ID: "no-user"}, SoftBudgetNano: &soft})
	if !errors.Is(err, admin.ErrUnsupported) {
		t.Fatalf("unsupported soft limit reported as %v", err)
	}
}
