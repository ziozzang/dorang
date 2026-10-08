package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMonthRangeIsTheUTCCalendarMonth(t *testing.T) {
	s, e, err := monthRange("2026-10", time.Time{})
	if err != nil || !s.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) ||
		!e.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("monthRange(2026-10) = %v, %v, %v", s, e, err)
	}
	now := time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)
	if s, e, _ := monthRange("", now); s.Month() != 12 || e.Year() != 2027 {
		t.Errorf("the current month of %v is %v..%v", now, s, e)
	}
	if _, _, err := monthRange("2026.10", now); err == nil {
		t.Error("a DeepInfra-style period was accepted as YYYY-MM")
	}
}

// TestDeepInfraUsageIsReadInCents uses the shape the endpoint answered on
// 2026-10-09: money in US cents, `cost` rounded to a whole cent, so the exact
// figure is units x rate.
func TestDeepInfraUsageIsReadInCents(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		if r.URL.Path != "/payment/usage" || r.Header.Get("Authorization") != "Bearer sk" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"months":[{"period":"2026.09","items":[
			{"model":{"model_name":"openai/whisper-large-v3-turbo"},"units":79362,"rate":0.000333,"cost":26,"pricing_type":"input_length"},
			{"model":{"model_name":"meta-llama/Meta-Llama-3.1-8B-Instruct-Turbo"},"units":11,"rate":2e-06,"cost":0,"pricing_type":"input_tokens"},
			{"model":{"model_name":"meta-llama/Meta-Llama-3.1-8B-Instruct-Turbo"},"units":3,"rate":4e-06,"cost":0,"pricing_type":"output_tokens"}]}]}`)
	}))
	defer srv.Close()

	a := &account{api: "deepinfra", base: srv.URL + "/v1/openai", secret: "sk"}
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	v, err := fetchDeepInfra(context.Background(), srv.Client(), a, start, start.AddDate(0, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "from=1788220800") || !strings.Contains(query, "to=1790812800") {
		t.Errorf("asked for %q, want the UTC month in epoch seconds", query)
	}
	// 79362 x 0.000333 + 11 x 0.000002 + 3 x 0.000004 cents = 26.42758 cents.
	if got := usd(v.totalUSD); got != "0.264275800" {
		t.Errorf("total = %s USD, want 0.264275800 (units x rate, not the rounded cost)", got)
	}
	// One line per model, every axis kept: input and output tokens are two items.
	if len(v.models) != 2 {
		t.Fatalf("models = %+v, want one line per model", v.models)
	}
	llama := v.models[1]
	if strings.Join(llama.units, " + ") != "11 input_tokens + 3 output_tokens" || usd(llama.usd) != "0.000000340" {
		t.Errorf("llama = %+v, want both axes summed to 0.000000340", llama)
	}
}

func TestOpenRouterStatesOnlyTheCurrentMonth(t *testing.T) {
	a := &account{api: "openrouter", base: "https://openrouter.invalid/api/v1", secret: "sk"}
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := fetchOpenRouter(context.Background(), http.DefaultClient, a, start, start.AddDate(0, 1, 0)); err == nil ||
		!strings.Contains(err.Error(), "current month") {
		t.Errorf("a past month was not refused: %v", err)
	}
}

func TestNanoUSD(t *testing.T) {
	for n, want := range map[int64]string{0: "0.000000000", 340: "0.000000340",
		1_500_000_000: "1.500000000", -2: "-0.000000002"} {
		if got := nanoUSD(n); got != want {
			t.Errorf("nanoUSD(%d) = %s, want %s", n, got, want)
		}
	}
}
