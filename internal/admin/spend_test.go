package admin

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func seedLedger(h *harness) {
	base := testNow.Add(-48 * time.Hour)
	rows := []LogRow{
		{
			ID: "r1", TS: base, APIKeyID: "k1", UserID: "u1", TeamID: "t1",
			ProviderID: "prov-a", ModelGroup: "chat", UpstreamModel: "a/model",
			Endpoint: "/v1/chat/completions", Status: 200,
			PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200,
			CostNano: 0, MarginalCostNano: 0, SubscriptionCostNano: 0,
			NotionalNano: 1_190_000_000, NotionalKnown: true,
			LatencyMS: 250, Tags: []string{"team-a", "batch"},
		},
		{
			ID: "r2", TS: base.Add(time.Hour), APIKeyID: "k1", UserID: "u1", TeamID: "t1",
			ProviderID: "prov-a", ModelGroup: "chat", Status: 500, ErrorClass: "upstream",
			PromptTokens: 10, TotalTokens: 10,
			CostNano: 0, NotionalNano: 0, NotionalKnown: true,
			Tags: []string{"team-a"},
		},
		{
			ID: "r3", TS: base.Add(25 * time.Hour), APIKeyID: "k2", UserID: "u2", TeamID: "t2",
			ProviderID: "prov-b", ModelGroup: "embed", Status: 200,
			PromptTokens: 500, TotalTokens: 500,
			CostNano: 2_500_000_000, MarginalCostNano: 2_500_000_000,
			// No notional rule matched for this model.
			NotionalKnown: false,
			Tags:          []string{"team-b"},
		},
	}
	for _, r := range rows {
		h.store.addLog(r)
	}
}

// The rule that shapes every ledger endpoint: no range, no answer.
func TestUnboundedLedgerQueryIsRefused(t *testing.T) {
	h := newHarness(t)
	seedLedger(h)

	for _, p := range []string{
		"/spend/logs",
		"/global/spend/report",
		"/user/daily/activity",
		"/team/daily/activity",
		"/tag/daily/activity",
	} {
		rec := h.do(http.MethodGet, p, nil)
		body := h.expectFault(rec, http.StatusBadRequest, CodeUnboundedRange)
		msg := body["error"].(map[string]any)["message"].(string)
		if !strings.Contains(msg, "start_date") {
			t.Errorf("%s: refusal does not say what is missing: %q", p, msg)
		}
		detail := body["error"].(map[string]any)["detail"].(map[string]any)
		missing, _ := detail["missing"].([]any)
		if len(missing) != 2 {
			t.Errorf("%s: refusal does not enumerate the missing bounds: %v", p, detail)
		}
	}
	if h.api.Metrics().RangeRefusals == 0 {
		t.Error("range refusals were not counted")
	}
}

// Half a range is still unbounded.
func TestHalfARangeIsRefused(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodGet, "/spend/logs?start_date=2026-07-01", nil),
		http.StatusBadRequest, CodeUnboundedRange)
	h.expectFault(h.do(http.MethodGet, "/spend/logs?end_date=2026-07-01", nil),
		http.StatusBadRequest, CodeUnboundedRange)
}

func TestInvertedRangeIsRefused(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodGet,
		"/spend/logs?start_date=2026-07-10&end_date=2026-07-01", nil),
		http.StatusBadRequest, CodeUnboundedRange)
}

// A too-wide range is refused, not capped. A capped query returns a partial
// answer that looks complete, which is the worse of the two failures.
func TestTooWideRangeIsRefusedNotCapped(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxTimeRange = 48 * time.Hour })
	seedLedger(h)

	rec := h.do(http.MethodGet, "/spend/logs?start_date=2026-01-01&end_date=2026-07-01", nil)
	body := h.expectFault(rec, http.StatusBadRequest, CodeRangeTooWide)
	detail := body["error"].(map[string]any)["detail"].(map[string]any)
	if detail["max_range_hours"] != 48.0 {
		t.Errorf("refusal does not state the maximum: %v", detail)
	}
	if !strings.Contains(rec.Body.String(), "narrow it") {
		t.Errorf("refusal does not tell the caller what to do: %s", rec.Body.String())
	}
}

// A backend that refuses an unbounded range must reach the caller as a refusal,
// not as a 500. The handler validates first, so this drives the store's own
// refusal through the mapping.
func TestStoreSideRefusalIsSurfaced(t *testing.T) {
	h := newHarness(t)
	c := &call{r: mustRequest("/x"), a: h.api}
	if err := c.ledgerError(ErrUnboundedRange); !isFaultCode(err, CodeUnboundedRange) {
		t.Fatalf("store refusal mapped to %v", faultFor(err).Code)
	}
	if err := c.ledgerError(ErrRangeTooWide); !isFaultCode(err, CodeRangeTooWide) {
		t.Fatalf("store refusal mapped to %v", faultFor(err).Code)
	}
}

func isFaultCode(err error, code string) bool { return faultFor(err).Code == code }

func mustRequest(path string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, path, nil)
	return r
}

func TestSpendLogsPaginatesWithAnOpaqueCursor(t *testing.T) {
	h := newHarness(t)
	seedLedger(h)
	q := "?start_date=2026-07-01&end_date=2026-07-29&limit=1"

	body := h.expectStatus(h.do(http.MethodGet, "/spend/logs"+q, nil), http.StatusOK)
	if len(body["logs"].([]any)) != 1 {
		t.Fatalf("limit was not honoured: %v", body["logs"])
	}
	if body["has_more"] != true {
		t.Fatal("has_more should be true on a full page")
	}
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("no cursor on a full page")
	}
	if strings.Contains(cursor, ":") {
		t.Error("the cursor is not opaque")
	}

	seen := map[string]bool{}
	for i := 0; i < 5 && cursor != ""; i++ {
		body = h.expectStatus(h.do(http.MethodGet, "/spend/logs"+q+"&cursor="+cursor, nil), http.StatusOK)
		for _, l := range body["logs"].([]any) {
			id := l.(map[string]any)["request_id"].(string)
			if seen[id] {
				t.Fatalf("row %s returned twice", id)
			}
			seen[id] = true
		}
		cursor, _ = body["next_cursor"].(string)
	}
	if len(seen) != 2 {
		t.Fatalf("paged through %d rows after the first, want 2", len(seen))
	}
}

func TestBadCursorIsRefused(t *testing.T) {
	h := newHarness(t)
	// Not base64 at all, and well-formed base64 that is not a cursor: both are
	// refused, because a cursor the caller invented is a pagination contract
	// nobody agreed to.
	for _, bad := range []string{"!!!!", "YWJjZA"} {
		h.expectFault(h.do(http.MethodGet,
			"/spend/logs?start_date=2026-07-01&end_date=2026-07-29&cursor="+bad, nil),
			http.StatusBadRequest, CodeInvalidRequest)
	}
}

func TestSpendLogsFilters(t *testing.T) {
	h := newHarness(t)
	seedLedger(h)
	base := "/spend/logs?start_date=2026-07-01&end_date=2026-07-29"

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"&api_key=k1", 2},
		{"&team_id=t2", 1},
		{"&user_id=u2", 1},
		{"&tag=team-a", 2},
		{"&errors_only=true", 1},
	} {
		body := h.expectStatus(h.do(http.MethodGet, base+tc.query, nil), http.StatusOK)
		if got := len(body["logs"].([]any)); got != tc.want {
			t.Errorf("%s returned %d rows, want %d", tc.query, got, tc.want)
		}
	}
}

// §8.5: a missing notional figure is reported as unavailable, never as zero.
func TestNotionalIsNullWhenMissingNotZero(t *testing.T) {
	h := newHarness(t)
	seedLedger(h)

	body := h.expectStatus(h.do(http.MethodGet,
		"/spend/logs?start_date=2026-07-01&end_date=2026-07-29", nil), http.StatusOK)
	var sawNull, sawValue bool
	for _, l := range body["logs"].([]any) {
		row := l.(map[string]any)
		v, present := row["notional_spend"]
		if !present {
			t.Fatal("notional_spend must always be present, even when unavailable")
		}
		if v == nil {
			sawNull = true
		} else {
			sawValue = true
		}
	}
	if !sawNull {
		t.Error("a row with no notional rule rendered a number instead of null")
	}
	if !sawValue {
		t.Error("a row with a notional rule rendered null")
	}
}

// An aggregate whose parts are not all known is not claimed as a total.
func TestNotionalTotalIsUnavailableIfAnyPartIs(t *testing.T) {
	h := newHarness(t)
	seedLedger(h)

	body := h.expectStatus(h.do(http.MethodGet,
		"/global/spend/report?start_date=2026-07-01&end_date=2026-07-29", nil), http.StatusOK)
	total := body["total"].(map[string]any)
	if total["notional_available"] != false {
		t.Fatalf("total claims a notional figure while one model has no rule: %v", total)
	}
	if total["notional_spend"] != nil {
		t.Fatalf("an unavailable notional total rendered a number: %v", total["notional_spend"])
	}
	if h.api.Metrics().NotionalMissing == 0 {
		t.Error("a missing notional figure was not counted")
	}
}

func TestNotionalTotalIsReportedWhenEveryPartIsKnown(t *testing.T) {
	h := newHarness(t)
	h.store.addLog(LogRow{
		ID: "only", TS: testNow.Add(-time.Hour), ModelGroup: "chat", Status: 200,
		CostNano: 1_000_000_000, NotionalNano: 4_000_000_000, NotionalKnown: true,
	})
	body := h.expectStatus(h.do(http.MethodGet,
		"/global/spend/report?start_date=2026-07-27&end_date=2026-07-29", nil), http.StatusOK)
	total := body["total"].(map[string]any)
	if total["notional_available"] != true || total["notional_spend"] != 4.0 {
		t.Fatalf("total = %v", total)
	}
}

func TestGlobalSpendReportGroupBy(t *testing.T) {
	h := newHarness(t)
	seedLedger(h)
	base := "/global/spend/report?start_date=2026-07-01&end_date=2026-07-29"

	body := h.expectStatus(h.do(http.MethodGet, base+"&group_by=model", nil), http.StatusOK)
	rows := body["results"].([]any)
	if len(rows) != 2 {
		t.Fatalf("group_by=model returned %d rows, want 2", len(rows))
	}
	h.expectFault(h.do(http.MethodGet, base+"&group_by=phase-of-moon", nil),
		http.StatusBadRequest, CodeInvalidRequest)
}

func TestDailyActivityShapes(t *testing.T) {
	h := newHarness(t)
	seedLedger(h)
	q := "?start_date=2026-07-01&end_date=2026-07-29"

	for _, tc := range []struct{ path, idField string }{
		{"/user/daily/activity", "user_id"},
		{"/team/daily/activity", "team_id"},
		{"/tag/daily/activity", "tag_id"},
	} {
		body := h.expectStatus(h.do(http.MethodGet, tc.path+q, nil), http.StatusOK)
		results := body["results"].([]any)
		if len(results) == 0 {
			t.Fatalf("%s returned no rows", tc.path)
		}
		row := results[0].(map[string]any)
		if _, ok := row[tc.idField]; !ok {
			t.Errorf("%s row has no %s: %v", tc.path, tc.idField, row)
		}
		if _, ok := row["date"]; !ok {
			t.Errorf("%s row has no date", tc.path)
		}
		metrics := row["metrics"].(map[string]any)
		for _, f := range []string{"spend", "notional_spend", "notional_available", "api_requests"} {
			if _, ok := metrics[f]; !ok {
				t.Errorf("%s metrics missing %s", tc.path, f)
			}
		}
		if _, ok := row["breakdown"].(map[string]any)["models"]; !ok {
			t.Errorf("%s row has no model breakdown", tc.path)
		}
		md := body["metadata"].(map[string]any)
		if md["dimension"] == nil || md["total"] == nil {
			t.Errorf("%s metadata = %v", tc.path, md)
		}
	}
}

func TestDailyActivityFiltersByID(t *testing.T) {
	h := newHarness(t)
	seedLedger(h)
	body := h.expectStatus(h.do(http.MethodGet,
		"/team/daily/activity?start_date=2026-07-01&end_date=2026-07-29&team_id=t2", nil),
		http.StatusOK)
	for _, r := range body["results"].([]any) {
		if r.(map[string]any)["team_id"] != "t2" {
			t.Fatalf("filter leaked another team: %v", r)
		}
	}
}

// A bare end date includes that whole day. Half-open arithmetic on a date the
// caller wrote inclusively is a silent off-by-one-day in every daily report.
func TestBareEndDateIncludesThatDay(t *testing.T) {
	h := newHarness(t)
	h.store.addLog(LogRow{
		ID: "late", TS: time.Date(2026, 7, 26, 23, 30, 0, 0, time.UTC),
		ModelGroup: "chat", Status: 200, NotionalKnown: true,
	})
	body := h.expectStatus(h.do(http.MethodGet,
		"/spend/logs?start_date=2026-07-26&end_date=2026-07-26", nil), http.StatusOK)
	if len(body["logs"].([]any)) != 1 {
		t.Fatalf("a request at 23:30 on the end date was excluded: %v", body["logs"])
	}
}

func TestSpendCalculateUsesThePricingEngine(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Pricing = fakePricer{ex: PriceExplanation{
			Currency: "USD", MarginalNano: 1_500_000_000, TotalNano: 1_500_000_000,
			Notional: PriceNotional{Nano: 3_000_000_000, RuleID: "list", Source: "vendor page", AsOf: "2026-07-01"},
		}}
	})
	body := h.expectStatus(h.do(http.MethodPost, "/spend/calculate", map[string]any{
		"model": "chat", "prompt_tokens": 1000, "completion_tokens": 100,
	}), http.StatusOK)
	if body["total"] != 1.5 {
		t.Fatalf("total = %v", body["total"])
	}
	n := body["notional"].(map[string]any)
	if n["amount"] != 3.0 || n["available"] != true || n["source"] != "vendor page" {
		t.Fatalf("notional = %v", n)
	}
}

// Sending messages instead of token counts gets an explanation, not a
// confidently wrong number.
func TestSpendCalculateRefusesMessagesWithACode(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Pricing = fakePricer{} })
	rec := h.do(http.MethodPost, "/spend/calculate", map[string]any{
		"model":    "chat",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	h.expectFault(rec, http.StatusNotImplemented, "tokenizer_unavailable")
}

func TestSpendCalculateWithoutPricingEngine(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodPost, "/spend/calculate", map[string]any{"model": "x"}),
		http.StatusNotImplemented, CodeDependencyOff)
}
