package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func withReporters(c *Config) {
	c.Credentials = fakeCredentials{list: []CredentialStatus{
		{
			ID: "cred-a", ProviderID: "prov-a", Health: "healthy",
			Requests: 100, Failures: 2, TTFTMS: 180, TokensPerSec: 42.5,
			Quota: []QuotaWindow{
				{Window: "minute", Metric: "requests", Used: 30, Limit: 60, UsedPct: 50, Source: "combined"},
				{Window: "day", Metric: "tokens", Used: 900, Limit: 1000, UsedPct: 90, Source: "provider", Stale: true},
			},
		},
		{
			ID: "cred-b", ProviderID: "prov-b", Health: "unavailable",
			UnavailableUntil: testNow.Add(time.Minute), ConsecutiveFailures: 5,
		},
	}}
	c.Capacity = fakeCapacity{occ: CapacityOccupancy{
		Axes: []AxisOccupancy{
			{Axis: "model", Key: "prov-a|a/model", InUse: 3, Limit: 10, Waiting: 1},
			{Axis: "global", Key: "global", InUse: 3, Limit: 0},
		},
		Waiting: 1, Reservations: 3, Grants: 97, Wakeups: 12, Expired: 1,
	}}
	c.Catalog = fakeCatalog{
		explanation: ModelExplanation{
			Fields: []FieldOrigin{
				{Field: "context_window", Value: "200000", Layer: "model", Origin: "file", Source: "catalog.yaml"},
			},
			Layers: []string{"kind", "model"}, Verified: "2026-07-01", KindKnown: true, ModelKnown: true,
		},
		unverified: []string{"vendor/unlisted-b", "vendor/unlisted-a"},
	}
	c.Pricing = fakePricer{ex: PriceExplanation{
		Currency:         "USD",
		MarginalNano:     0,
		SubscriptionNano: 660_000_000,
		TotalNano:        660_000_000,
		Applied:          []PriceRule{{RuleID: "plan-a-subscription", Class: "fixed_subscription", Level: "credential", Why: "most specific"}},
		Classes: []PriceClassTrace{{Class: "marginal_usage", Considered: []PriceConsidered{
			{RuleID: "none", Level: "default", Eligible: false, Reason: "no rule matched the provider"},
		}}},
		Missing: true,
		Notional: PriceNotional{
			Nano: 4_250_000_000, RuleID: "plan-a-list-rate",
			Source: "vendor public price page", AsOf: "2026-07-28", AgeSeconds: 3600,
			Components: []PriceComponent{{Name: "input", RuleID: "plan-a-list-rate", Rate: "0.85",
				Unit: "per_1m_tokens", Quantity: 5_000_000, SubtotalNano: 4_250_000_000}},
		},
	}}
	c.Reloader = fakeReloader{res: ReloadResult{
		Version: "v7", LoadedAt: testNow, Changed: []string{"providers"},
	}}
	c.Health = NewMemoryHealthHistory(16, testNow)
}

func TestAdminCredentialHealth(t *testing.T) {
	h := newHarness(t, withReporters)
	body := h.expectStatus(h.do(http.MethodGet, "/admin/credentials/health", nil), http.StatusOK)
	if body["unhealthy"] != 1.0 {
		t.Fatalf("unhealthy = %v", body["unhealthy"])
	}
	creds := body["credentials"].([]any)
	first := creds[0].(map[string]any)
	if first["credential_id"] != "cred-a" {
		t.Fatalf("credentials are not sorted: %v", first)
	}
	q := first["quota"].([]any)[1].(map[string]any)
	if q["used_pct"] != 90.0 || q["stale"] != true || q["source"] != "provider" {
		t.Fatalf("quota window = %v", q)
	}
	// No credential material may appear anywhere in this answer: §11.2b says
	// the only thing that leaves that subsystem is an opaque id and a health.
	rendered := strings.ToLower(mustJSON(t, body))
	for _, bad := range []string{"secret", "token_hash", "api_key", "bearer"} {
		if strings.Contains(rendered, bad) {
			t.Errorf("credential health mentions %q", bad)
		}
	}
}

func TestAdminQuotaIsSortedByPressure(t *testing.T) {
	h := newHarness(t, withReporters)
	body := h.expectStatus(h.do(http.MethodGet, "/admin/quota", nil), http.StatusOK)
	windows := body["windows"].([]any)
	if len(windows) != 2 {
		t.Fatalf("windows = %v", windows)
	}
	if windows[0].(map[string]any)["used_pct"] != 90.0 {
		t.Fatalf("the most pressured window is not first: %v", windows)
	}
	if windows[0].(map[string]any)["credential_id"] != "cred-a" {
		t.Fatalf("window is not attributed to a credential: %v", windows[0])
	}
}

func TestAdminCapacityReportsOccupancy(t *testing.T) {
	h := newHarness(t, withReporters)
	body := h.expectStatus(h.do(http.MethodGet, "/admin/capacity", nil), http.StatusOK)
	axes := body["axes"].([]any)
	if axes[0].(map[string]any)["used_pct"] != 30.0 {
		t.Fatalf("used_pct = %v", axes[0])
	}
	// An axis with no ceiling must not claim 0% used: "nothing in use" and "no
	// ceiling" are different facts.
	if axes[1].(map[string]any)["used_pct"] != nil {
		t.Fatalf("an unlimited axis reported a percentage: %v", axes[1])
	}
	if body["waiting"] != 1.0 || body["grants"] != 97.0 {
		t.Fatalf("counters = %v", body)
	}
}

func TestAdminCatalogExplainAndUnverified(t *testing.T) {
	h := newHarness(t, withReporters)

	body := h.expectStatus(h.do(http.MethodGet,
		"/admin/catalog/explain?kind=openai-compatible&model=vendor/model-x", nil), http.StatusOK)
	if body["model"] != "vendor/model-x" {
		t.Fatalf("model = %v", body["model"])
	}
	f := body["fields"].([]any)[0].(map[string]any)
	if f["origin"] != "file" || f["layer"] != "model" {
		t.Fatalf("field provenance = %v", f)
	}

	h.expectFault(h.do(http.MethodGet, "/admin/catalog/explain", nil),
		http.StatusBadRequest, CodeInvalidRequest)

	body = h.expectStatus(h.do(http.MethodGet, "/admin/catalog/unverified", nil), http.StatusOK)
	models := body["models"].([]any)
	if len(models) != 2 || models[0] != "vendor/unlisted-a" {
		t.Fatalf("unverified list is not sorted: %v", models)
	}
}

// §8.4 and §8.5: the preview returns the rule chain, the components, why each
// rule was selected, and the notional figure with its provenance.
func TestAdminPricingPreviewCarriesNotionalWithProvenance(t *testing.T) {
	h := newHarness(t, withReporters)
	body := h.expectStatus(h.do(http.MethodPost, "/admin/pricing/preview", map[string]any{
		"model": "model-x", "provider": "plan-a", "credential": "plan-a-1",
		"prompt_tokens": 5000000,
	}), http.StatusOK)

	if body["total"] != 0.66 {
		t.Fatalf("total = %v", body["total"])
	}
	if body["missing"] != true {
		t.Errorf("an unpriced marginal must be reported as missing, not as free traffic")
	}
	applied := body["applied_rules"].([]any)
	if len(applied) != 1 || applied[0].(map[string]any)["why"] == "" {
		t.Fatalf("applied rules do not say why: %v", applied)
	}
	if len(body["class_traces"].([]any)) != 1 {
		t.Fatalf("class traces = %v", body["class_traces"])
	}

	n := body["notional"].(map[string]any)
	if n["amount"] != 4.25 || n["available"] != true {
		t.Fatalf("notional = %v", n)
	}
	if n["source"] != "vendor public price page" || n["as_of"] != "2026-07-28" {
		t.Fatalf("notional has no provenance: %v", n)
	}
	// §8.5 rule 4: notional lines stay out of the cost component breakdown, so
	// a caller who sums components still gets the billed figure.
	for _, c := range body["components"].([]any) {
		if c.(map[string]any)["rule_id"] == "plan-a-list-rate" {
			t.Fatal("a notional component leaked into the billed component breakdown")
		}
	}
}

func TestAdminPricingPreviewReportsMissingNotional(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Pricing = fakePricer{ex: PriceExplanation{
			Currency: "USD", MarginalNano: 1_000_000_000, TotalNano: 1_000_000_000,
			Notional: PriceNotional{Missing: true},
		}}
	})
	body := h.expectStatus(h.do(http.MethodPost, "/admin/pricing/preview",
		map[string]any{"model": "x"}), http.StatusOK)
	n := body["notional"].(map[string]any)
	if n["amount"] != nil {
		t.Fatalf("a missing notional figure rendered as a number: %v", n["amount"])
	}
	if n["available"] != false {
		t.Fatalf("available = %v", n["available"])
	}
	if !strings.Contains(n["note"].(string), "unavailable rather than zero") {
		t.Errorf("note = %v", n["note"])
	}
	if h.api.Metrics().NotionalMissing != 1 {
		t.Error("a missing notional figure was not counted")
	}
}

func TestAdminConfigReloadIsAudited(t *testing.T) {
	h := newHarness(t, withReporters)
	body := h.expectStatus(h.do(http.MethodPost, "/admin/config/reload", nil), http.StatusOK)
	if body["version"] != "v7" {
		t.Fatalf("version = %v", body)
	}
	e, _ := h.store.lastAudit()
	if e.Action != "config.reload" || e.ObjectID != "v7" {
		t.Fatalf("audit = %+v", e)
	}
	if e.Before == "" || e.After == "" {
		t.Fatalf("reload audit is missing a state: %+v", e)
	}
}

func TestAdminStatusNamesWhatIsWired(t *testing.T) {
	h := newHarness(t)
	body := h.expectStatus(h.do(http.MethodGet, "/admin/status", nil), http.StatusOK)
	deps := body["dependencies"].(map[string]any)
	if deps["keys"] != true || deps["capacity"] != false {
		t.Fatalf("dependencies = %v", deps)
	}
	if body["metrics"].(map[string]any)["requests"] == nil {
		t.Fatal("status carries no metrics")
	}
}

// /health/history has no table behind it in §9.2. Without a recorder it says so
// with a code; it must never return an empty list, which reads as "nothing ever
// failed".
func TestHealthHistoryWithoutRecorderIs501(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/health/history?start_date=2026-07-27&end_date=2026-07-29", nil)
	body := h.expectFault(rec, http.StatusNotImplemented, CodeDependencyOff)
	detail := body["error"].(map[string]any)["detail"].(map[string]any)
	if !strings.Contains(detail["reason"].(string), "credential_state") {
		t.Errorf("the 501 does not explain the schema gap: %v", detail)
	}
}

func TestHealthHistoryWithRecorder(t *testing.T) {
	hist := NewMemoryHealthHistory(8, testNow)
	h := newHarness(t, func(c *Config) { c.Health = hist })

	hist.Record(HealthEvent{TS: testNow.Add(-2 * time.Hour), SubjectKind: "credential",
		Subject: "cred-a", State: "unavailable", Reason: "429", Status: 429})
	hist.Record(HealthEvent{TS: testNow.Add(-time.Hour), SubjectKind: "credential",
		Subject: "cred-a", State: "healthy"})
	hist.Record(HealthEvent{TS: testNow.Add(-30 * time.Minute), SubjectKind: "credential",
		Subject: "cred-b", State: "unavailable"})

	body := h.expectStatus(h.do(http.MethodGet,
		"/health/history?start_date=2026-07-27&end_date=2026-07-29", nil), http.StatusOK)
	events := body["history"].([]any)
	if len(events) != 3 {
		t.Fatalf("history = %v", events)
	}
	// Newest first, like every other ledger read here.
	if events[0].(map[string]any)["subject"] != "cred-b" {
		t.Fatalf("history is not newest-first: %v", events)
	}

	body = h.expectStatus(h.do(http.MethodGet,
		"/health/history?start_date=2026-07-27&end_date=2026-07-29&subject=cred-a", nil), http.StatusOK)
	if len(body["history"].([]any)) != 2 {
		t.Fatalf("subject filter = %v", body["history"])
	}
}

// A history query is a ledger query and takes the same bounded range: §9.3's
// own correction is that exempting one read contradicts the rule.
func TestHealthHistoryRequiresARange(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Health = NewMemoryHealthHistory(4, testNow) })
	h.expectFault(h.do(http.MethodGet, "/health/history", nil),
		http.StatusBadRequest, CodeUnboundedRange)
}

func TestMemoryHealthHistoryIsBounded(t *testing.T) {
	hist := NewMemoryHealthHistory(3, testNow)
	for i := 0; i < 10; i++ {
		hist.Record(HealthEvent{TS: testNow.Add(time.Duration(i) * time.Minute), Subject: "c"})
	}
	got, err := hist.History(context.Background(), HealthQuery{
		Range: Range{Start: testNow.Add(-time.Hour), End: testNow.Add(time.Hour)}, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ring kept %d events, want 3", len(got))
	}
	if !got[0].TS.Equal(testNow.Add(9 * time.Minute)) {
		t.Fatalf("the ring dropped the wrong end: %v", got[0].TS)
	}
}

func TestMemoryHealthHistoryRefusesUnboundedRange(t *testing.T) {
	hist := NewMemoryHealthHistory(4, testNow)
	if _, err := hist.History(context.Background(), HealthQuery{}); err == nil {
		t.Fatal("an unbounded history query was accepted")
	}
}
