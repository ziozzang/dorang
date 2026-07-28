package pricing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// The timezone tests must not depend on the host having a zoneinfo database.
	_ "time/tzdata"
)

// farFuture is what "now" means to a catalog built by [mustCatalog].
//
// A settlement stamped ahead of the present is clamped to it, and every fixture in this
// package settles at a hand-written instant. Left on the wall clock, whether a fixture is
// "in the future" would depend on the day the suite runs — a fixture dated 2026-07-31 is
// in the past today and was in the future when it was written, so the same test would pass
// and fail on different dates. Pinning the clock past every fixture makes the clamp
// something a test opts into with [mustCatalogAt] rather than something the calendar
// decides.
var farFuture = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)

func mustCatalog(t *testing.T, y string) *Catalog {
	return mustCatalogAt(t, y, farFuture)
}

// mustCatalogAt builds a catalog whose present is now.
func mustCatalogAt(t *testing.T, y string, now time.Time) *Catalog {
	t.Helper()
	c, err := ParseCatalog([]byte(y))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	c.SetClock(func() time.Time { return now })
	return c
}

func mustPrice(t *testing.T, c *Catalog, req Request) Cost {
	t.Helper()
	cost, err := c.Price(req)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	return cost
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return v
}

func ruleIDs(cost Cost, class Class) []string {
	var out []string
	for _, a := range cost.AppliedRules {
		if a.Class == class {
			out = append(out, a.RuleID)
		}
	}
	return out
}

func TestLoadCatalogFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	body := `
currency: EUR
rules:
  - id: base
    match: { model: m }
    unit: per_1m_tokens
    input: "1.00"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCatalog(path)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if c.Currency != "EUR" {
		t.Fatalf("currency = %q, want EUR", c.Currency)
	}
	if _, err := LoadCatalog(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("expected an error for a missing catalog")
	}
}

func TestEmptyCatalogPricesNothingAndSaysSo(t *testing.T) {
	c := mustCatalog(t, "")
	cost := mustPrice(t, c, Request{Provider: "p", Model: "m", At: time.Unix(0, 0)})
	if !cost.Missing {
		t.Fatal("an empty catalog must report Missing, not a silent zero")
	}
	if cost.TotalNano != 0 || cost.MarginalNano != 0 {
		t.Fatalf("unpriced request cost %+v", cost)
	}
}

func TestCatalogRejectsBadConfiguration(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"unknown field", "rules:\n  - id: a\n    matches: {}\n", "field matches not found"},
		{"unknown class", "rules:\n  - id: a\n    class: freebie\n", "unknown class"},
		{"unknown unit", "rules:\n  - id: a\n    unit: per_furlong\n    input: \"1\"\n", "unknown unit"},
		{"missing id", "rules:\n  - unit: per_request\n    request: \"1\"\n", "id is required"},
		{"duplicate id", "rules:\n  - id: a\n    request: \"1\"\n    unit: per_request\n  - id: a\n    request: \"2\"\n    unit: per_request\n", "duplicate rule id"},
		{"no rates", "rules:\n  - id: a\n    unit: per_1m_tokens\n", "no rates declared"},
		{"rate for the wrong unit", "rules:\n  - id: a\n    unit: per_request\n    input: \"1\"\n", "does not belong to unit"},
		{"negative rate", "rules:\n  - id: a\n    unit: per_1m_tokens\n    input: \"-1\"\n", "may not be negative"},
		{"exponent price", "rules:\n  - id: a\n    unit: per_1m_tokens\n    input: \"1e-6\"\n", "exponent notation"},
		{"too many digits", "rules:\n  - id: a\n    unit: per_1m_tokens\n    input: \"0.0000000000001\"\n", "at most 12 fractional digits"},
		{"subscription without amount", "rules:\n  - id: a\n    class: fixed_subscription\n", "amount_per_period is required"},
		{"subscription with rates", "rules:\n  - id: a\n    class: fixed_subscription\n    amount_per_period: \"1\"\n    input: \"1\"\n", "prices no components"},
		{"bad period", "rules:\n  - id: a\n    class: fixed_subscription\n    amount_per_period: \"1\"\n    period: fortnightly\n", "unknown period"},
		{"adjustment without amount", "rules:\n  - id: a\n    class: adjustment\n    op: percent\n", "amount is required"},
		{"bad time window", "rules:\n  - id: a\n    unit: per_request\n    request: \"1\"\n    when: { time_of_day: \"25:00-08:00\" }\n", "bad hour"},
		{"empty when", "rules:\n  - id: a\n    unit: per_request\n    request: \"1\"\n    when: {}\n", "no predicate declared"},
		{"bad tz", "rules:\n  - id: a\n    unit: per_request\n    request: \"1\"\n    when: { tz: Mars/Olympus, weekday: [mon] }\n", "when.tz"},
		{"bounded last tier", "rules:\n  - id: a\n    unit: per_1m_tokens\n    input: \"1\"\n    tiers:\n      - { up_to_input_tokens: 10, input: \"1\" }\n", "last tier must be unbounded"},
		{"descending tiers", "rules:\n  - id: a\n    unit: per_1m_tokens\n    input: \"1\"\n    tiers:\n      - { up_to_input_tokens: 100, input: \"1\" }\n      - { up_to_input_tokens: 10, input: \"2\" }\n      - { input: \"3\" }\n", "must increase"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCatalog([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

const sevenLevels = `
currency: USD
rules:
  - { id: l7-credential,   match: { credential: c1 },              unit: per_1m_tokens, input: "7" }
  - { id: l6-deployment,   match: { deployment: d1 },              unit: per_1m_tokens, input: "6" }
  - { id: l5-provmodel,    match: { provider: p1, model: m1 },     unit: per_1m_tokens, input: "5" }
  - { id: l4-model,        match: { model: m1 },                   unit: per_1m_tokens, input: "4" }
  - { id: l3-prefix,       match: { model_prefix: "m" },           unit: per_1m_tokens, input: "3" }
  - { id: l2-provider,     match: { provider: p1 },                unit: per_1m_tokens, input: "2" }
  - { id: l1-default,      match: {},                              unit: per_1m_tokens, input: "1" }
`

// TestSpecificityOrder walks all seven levels of §8.2 by removing, one at a time, the
// dimension the current winner matches on.
func TestSpecificityOrder(t *testing.T) {
	c := mustCatalog(t, sevenLevels)
	base := Request{Provider: "p1", Model: "m1", Credential: "c1", Deployment: "d1",
		InputTokens: 1_000_000, At: at(t, "2026-07-28T12:00:00Z")}

	steps := []struct {
		mutate func(r *Request)
		want   string
		nano   int64
		level  Level
	}{
		{func(r *Request) {}, "l7-credential", 7_000_000_000, LevelCredential},
		{func(r *Request) { r.Credential = "" }, "l6-deployment", 6_000_000_000, LevelDeployment},
		{func(r *Request) { r.Deployment = "" }, "l5-provmodel", 5_000_000_000, LevelProviderModel},
		{func(r *Request) { r.Provider = "other" }, "l4-model", 4_000_000_000, LevelModel},
		{func(r *Request) { r.Model = "mzzz" }, "l3-prefix", 3_000_000_000, LevelModelPrefix},
		{func(r *Request) { r.Provider = "p1"; r.Model = "zzz" }, "l2-provider", 2_000_000_000, LevelProvider},
		{func(r *Request) { r.Provider = "nope" }, "l1-default", 1_000_000_000, LevelDefault},
	}
	req := base
	for _, s := range steps {
		s.mutate(&req)
		cost := mustPrice(t, c, req)
		got := ruleIDs(cost, ClassMarginal)
		if len(got) != 1 || got[0] != s.want {
			t.Fatalf("req %+v selected %v, want %s", req, got, s.want)
		}
		if cost.MarginalNano != s.nano {
			t.Fatalf("%s: marginal = %d, want %d", s.want, cost.MarginalNano, s.nano)
		}
		if cost.AppliedRules[0].Level != s.level {
			t.Fatalf("%s: level = %s, want %s", s.want, cost.AppliedRules[0].Level, s.level)
		}
		if why := cost.AppliedRules[0].Why(); !strings.Contains(why, s.level.String()) {
			t.Fatalf("%s: Why() = %q, want it to mention %s", s.want, why, s.level)
		}
	}
}

func TestTieBreakIsDeterministic(t *testing.T) {
	// Same level, same match: priority wins first, then the rule id, always.
	y := `
rules:
  - { id: bbb, match: { model: m }, unit: per_1m_tokens, input: "1", priority: 10 }
  - { id: aaa, match: { model: m }, unit: per_1m_tokens, input: "2", priority: 10 }
  - { id: ccc, match: { model: m }, unit: per_1m_tokens, input: "3", priority: 99 }
`
	req := Request{Model: "m", InputTokens: 1_000_000, At: at(t, "2026-07-28T12:00:00Z")}
	for i := 0; i < 50; i++ {
		c := mustCatalog(t, y)
		cost := mustPrice(t, c, req)
		if got := ruleIDs(cost, ClassMarginal)[0]; got != "ccc" {
			t.Fatalf("iteration %d: priority ignored, got %s", i, got)
		}
	}
	// With priority equal, the lexicographically smaller id wins, every time.
	y2 := strings.ReplaceAll(y, "priority: 99", "priority: 10")
	for i := 0; i < 50; i++ {
		c := mustCatalog(t, y2)
		cost := mustPrice(t, c, req)
		if got := ruleIDs(cost, ClassMarginal)[0]; got != "aaa" {
			t.Fatalf("iteration %d: id tie-break not deterministic, got %s", i, got)
		}
	}
}

func TestLongerPrefixIsMoreSpecific(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: short, match: { model_prefix: "g" },       unit: per_1m_tokens, input: "1" }
  - { id: long,  match: { model_prefix: "gemma4:" }, unit: per_1m_tokens, input: "2" }
`)
	cost := mustPrice(t, c, Request{Model: "gemma4:31b", InputTokens: 1_000_000, At: at(t, "2026-07-28T12:00:00Z")})
	if got := ruleIDs(cost, ClassMarginal)[0]; got != "long" {
		t.Fatalf("selected %s, want long", got)
	}
	cost = mustPrice(t, c, Request{Model: "gpt-ish", InputTokens: 1_000_000, At: at(t, "2026-07-28T12:00:00Z")})
	if got := ruleIDs(cost, ClassMarginal)[0]; got != "short" {
		t.Fatalf("selected %s, want short", got)
	}
}

// TestModelNamesAreOpaque freezes §2.1: a model name is never split on ':' or '/',
// and model_prefix is a literal prefix test on the whole name.
func TestModelNamesAreOpaque(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: family,   match: { model_prefix: "gemma4:" },            unit: per_1m_tokens, input: "1" }
  - { id: vendor,   match: { model_prefix: "zai:glm-5" },          unit: per_1m_tokens, input: "2" }
  - { id: variant,  match: { model: "deepseek-v4-flash:cloud" },   unit: per_1m_tokens, input: "3" }
  - { id: bare,     match: { model: "gemma4" },                    unit: per_1m_tokens, input: "9" }
  - { id: slashed,  match: { model: "vendor/model-x" },            unit: per_1m_tokens, input: "4" }
  - { id: fallback, match: {},                                     unit: per_1m_tokens, input: "0" }
`)
	cases := []struct{ model, want string }{
		{"gemma4:31b", "family"},
		{"gemma4", "bare"}, // an exact match, not a prefix of the family rule's key
		{"zai:glm-5.1", "vendor"},
		{"deepseek-v4-flash:cloud", "variant"},
		{"deepseek-v4-flash", "fallback"}, // splitting on ':' would have matched "variant"
		{"vendor/model-x", "slashed"},
		{"vendor", "fallback"}, // splitting on '/' would have matched "slashed"
		{"model-x", "fallback"},
	}
	for _, tc := range cases {
		cost := mustPrice(t, c, Request{Model: tc.model, InputTokens: 1_000_000, At: at(t, "2026-07-28T12:00:00Z")})
		if got := ruleIDs(cost, ClassMarginal)[0]; got != tc.want {
			t.Fatalf("model %q selected %s, want %s", tc.model, got, tc.want)
		}
	}
}

func TestTimeWindowAcrossTimezoneAndMidnight(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: night
    match: { model: m }
    when: { time_of_day: "22:00-08:00", tz: Asia/Seoul }
    unit: per_1m_tokens
    input: "1"
    priority: 10
  - id: day
    match: { model: m }
    unit: per_1m_tokens
    input: "5"
`)
	cases := []struct {
		utc  string
		want string
		note string
	}{
		{"2026-07-28T14:00:00Z", "night", "23:00 Seoul, inside the window before midnight"},
		{"2026-07-28T17:00:00Z", "night", "02:00 Seoul next day, the window wraps midnight"},
		{"2026-07-28T22:30:00Z", "night", "07:30 Seoul, still inside"},
		{"2026-07-28T23:00:00Z", "day", "08:00 Seoul, the end of the window is exclusive"},
		{"2026-07-28T03:00:00Z", "day", "12:00 Seoul, outside"},
		{"2026-07-28T12:59:00Z", "day", "21:59 Seoul, one minute before the window"},
		{"2026-07-28T13:00:00Z", "night", "22:00 Seoul, the start is inclusive"},
	}
	for _, tc := range cases {
		req := Request{Model: "m", InputTokens: 1_000_000, At: at(t, tc.utc)}
		cost := mustPrice(t, c, req)
		if got := ruleIDs(cost, ClassMarginal)[0]; got != tc.want {
			t.Fatalf("%s (%s): selected %s, want %s", tc.utc, tc.note, got, tc.want)
		}
	}
}

func TestWeekdayAndDateRange(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - id: weekday-promo
    match: { model: m }
    when: { weekday: [mon, tue, wed, thu, fri], date_range: "2026-07-01..2026-07-31", tz: Asia/Seoul }
    unit: per_1m_tokens
    input: "1"
    priority: 10
  - id: standard
    match: { model: m }
    unit: per_1m_tokens
    input: "5"
`)
	cases := []struct{ utc, want string }{
		{"2026-07-28T03:00:00Z", "weekday-promo"}, // Tuesday noon in Seoul
		{"2026-07-25T03:00:00Z", "standard"},      // Saturday
		{"2026-08-04T03:00:00Z", "standard"},      // outside the date range
		{"2026-06-30T03:00:00Z", "standard"},      // before the date range
		{"2026-07-31T14:00:00Z", "weekday-promo"}, // 23:00 on the last day, inclusive
		{"2026-07-31T15:00:00Z", "standard"},      // 00:00 on 1 Aug in Seoul
	}
	for _, tc := range cases {
		cost := mustPrice(t, c, Request{Model: "m", InputTokens: 1_000_000, At: at(t, tc.utc)})
		if got := ruleIDs(cost, ClassMarginal)[0]; got != tc.want {
			t.Fatalf("%s: selected %s, want %s", tc.utc, got, tc.want)
		}
	}
}

// TestIndexedRuleStillChecksItsOtherDimensions covers the half of §8.2 that the index
// alone cannot do: a rule is filed under its most specific dimension, but every other
// dimension it constrains still has to match.
func TestIndexedRuleStillChecksItsOtherDimensions(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: narrow,   match: { credential: c1, model: "m1", provider: p1 }, unit: per_1m_tokens, input: "1" }
  - { id: bydep,    match: { deployment: d1, model: "m1" },               unit: per_1m_tokens, input: "2" }
  - { id: byprefix, match: { model_prefix: "m", provider: p1 },           unit: per_1m_tokens, input: "3" }
  - { id: fallback, match: {},                                            unit: per_1m_tokens, input: "9" }
`)
	now := at(t, "2026-07-28T12:00:00Z")
	cases := []struct {
		req  Request
		want string
	}{
		{Request{Credential: "c1", Provider: "p1", Model: "m1"}, "narrow"},
		{Request{Credential: "c1", Provider: "p1", Model: "m2"}, "byprefix"}, // model rules out narrow
		{Request{Credential: "c1", Provider: "p2", Model: "m1"}, "fallback"}, // provider rules out both
		{Request{Deployment: "d1", Model: "m1"}, "bydep"},
		{Request{Deployment: "d1", Model: "zz"}, "fallback"}, // model rules out bydep
		{Request{Provider: "p2", Model: "m1"}, "fallback"},   // provider rules out byprefix
	}
	for _, tc := range cases {
		tc.req.InputTokens, tc.req.At = 1_000_000, now
		cost := mustPrice(t, c, tc.req)
		if got := ruleIDs(cost, ClassMarginal)[0]; got != tc.want {
			t.Fatalf("%+v selected %s, want %s", tc.req, got, tc.want)
		}
	}

	// Explain must say which dimension rejected a rule.
	ex := c.Explain(Request{Credential: "c1", Provider: "p1", Model: "m2", InputTokens: 1, At: now})
	var reason string
	for _, cd := range ex.Classes[ClassMarginal].Considered {
		if cd.RuleID == "narrow" {
			reason = cd.Reason
		}
	}
	if !strings.Contains(reason, "model m1 != m2") {
		t.Fatalf("Explain reason for the rejected rule = %q", reason)
	}
}

// TestSubscriptionPeriods checks that every period length places its own boundaries and
// starts its own attribution over: two requests, one at the half-way mark and one at the
// last instant, take half the plan cost each and sum to exactly the plan cost, and the
// next period does the same again rather than continuing the last one (§8.1).
func TestSubscriptionPeriods(t *testing.T) {
	for _, tc := range []struct {
		period string
		p      Period
		inside string // any instant inside the period under test
	}{
		{"daily", PeriodDaily, "2026-07-28T01:00:00Z"},
		{"weekly", PeriodWeekly, "2026-07-27T01:00:00Z"}, // Monday to Sunday
		{"monthly", PeriodMonthly, "2026-07-01T01:00:00Z"},
		{"yearly", PeriodYearly, "2026-01-01T01:00:00Z"},
	} {
		t.Run(tc.period, func(t *testing.T) {
			c := mustCatalog(t, `
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "1.00" }
  - id: plan
    class: fixed_subscription
    match: { credential: c1 }
    amount_per_period: "10.00"
    period: `+tc.period+`
`)
			const plan, half = int64(10_000_000_000), int64(5_000_000_000)
			req := Request{Model: "m", Credential: "c1", InputTokens: 1_000_000}
			settle := func(when time.Time) int64 {
				t.Helper()
				req.At = when
				cost, err := c.Settle(req)
				if err != nil {
					t.Fatal(err)
				}
				return cost.SubscriptionNano
			}

			start, end := periodBounds(tc.p, at(t, tc.inside), time.UTC)
			var total int64
			if got := settle(start.Add(end.Sub(start) / 2)); got != half {
				t.Fatalf("half way through the period = %d, want half the plan cost %d", got, half)
			} else {
				total += got
			}
			if got := settle(end.Add(-time.Nanosecond)); got != half {
				t.Fatalf("at the end of the period = %d, want the remaining %d", got, half)
			} else {
				total += got
			}
			if total != plan {
				t.Fatalf("the period attributed %d, want exactly the plan cost %d", total, plan)
			}
			nextStart, nextEnd := periodBounds(tc.p, end, time.UTC)
			if got := settle(nextStart.Add(nextEnd.Sub(nextStart) / 2)); got != half {
				t.Fatalf("half way through the NEXT period = %d, want %d: each period "+
					"attributes its own plan cost from zero", got, half)
			}
		})
	}
}

func TestCatalogTimezoneAppliesToPeriodsAndWindows(t *testing.T) {
	c := mustCatalog(t, `
currency: KRW
tz: Asia/Seoul
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "1.00" }
  - id: plan
    class: fixed_subscription
    match: { credential: c1 }
    amount_per_period: "10.00"
    period: daily
`)
	req := Request{Model: "m", Credential: "c1", InputTokens: 1_000_000}
	// 2026-07-28T16:00Z is 2026-07-29T01:00 in Seoul: a new day there, the same day in UTC.
	req.At = at(t, "2026-07-28T10:00:00Z") // 19:00 Seoul on the 28th: 19/24 of that day
	if _, err := c.Settle(req); err != nil {
		t.Fatal(err)
	}
	req.At = at(t, "2026-07-28T16:00:00Z") // 01:00 Seoul on the 29th: a fresh period
	cost, err := c.Settle(req)
	if err != nil {
		t.Fatal(err)
	}
	// One hour into a fresh Seoul day: 10.00 x 1/24, less the third of a nano the first
	// settlement of the previous day carried. Placed in UTC instead, this instant would
	// fall six hours later in the SAME period and take 10.00 x 6/24 = 2.50.
	if want := int64(416_666_666); cost.SubscriptionNano != want {
		t.Fatalf("the catalog timezone was not used to place the period: %d, want %d",
			cost.SubscriptionNano, want)
	}
}
