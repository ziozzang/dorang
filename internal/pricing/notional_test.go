package pricing

import (
	"strings"
	"testing"
	"time"
)

// designExample is §8.5's catalog, copied verbatim, including the unquoted as_of date.
const designExample = `
currency: USD
rules:
  - id: plan-a-subscription
    class: fixed_subscription
    match: { credential: plan-a-1 }
    unit: subscription
    amount_per_period: "20.00"
    period: monthly

  - id: plan-a-list-rate
    class: notional_rate                     # never billed, never budgeted
    match: { provider: plan-a, model: model-x }
    unit: per_1m_tokens
    input:  "0.85"
    output: "3.40"
    cache_read: "0.19"
    source: "vendor public price page"       # required
    as_of: 2026-07-28
`

func TestDesignExampleParsesAndPrices(t *testing.T) {
	c := mustCatalog(t, designExample)
	req := Request{
		Provider: "plan-a", Model: "model-x", Credential: "plan-a-1",
		// 40% of the prompt was served from cache. A fixture whose prompt is
		// ENTIRELY cache — which this was — cannot tell the exclusive rate
		// convention from the additive one that double-charged the prefix.
		InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 400_000,
		At: at(t, "2026-07-29T12:00:00Z"),
	}
	cost := mustPrice(t, c, req)

	// A plan bills flat: nothing is marginal, and that is exactly the blind spot §8.5
	// exists to fill.
	if cost.MarginalNano != 0 || !cost.Missing {
		t.Fatalf("a flat plan has no marginal rule: %+v", cost)
	}
	// 0.85 x 0.6M + 3.40 x 1M + 0.19 x 0.4M. The input rate is charged on the part of
	// the prompt the cache did NOT serve; charging it on the whole inclusive count and
	// cache_read on the prefix again gives 4_326_000_000, which is the defect.
	if want := int64(3_986_000_000); cost.NotionalNano != want {
		t.Fatalf("notional = %d, want %d", cost.NotionalNano, want)
	}
	if cost.NotionalMissing {
		t.Fatal("a notional rule matched, so NotionalMissing must be false")
	}
	// The plan has no marginal rule at all, so amortization takes the elapsed-fraction
	// fallback: 28.5 days into a 31 day month of a 20.00 plan.
	if want := int64(18_387_096_774); cost.SubscriptionNano != want {
		t.Fatalf("subscription = %d, want %d", cost.SubscriptionNano, want)
	}
	if cost.TotalNano != cost.MarginalNano+cost.SubscriptionNano+cost.AdjustmentNano {
		t.Fatalf("TotalNano %d is not the sum of the billing classes: %+v", cost.TotalNano, cost)
	}
	if cost.TotalNano == cost.NotionalNano+cost.SubscriptionNano {
		t.Fatal("the notional figure leaked into TotalNano")
	}
}

// TestNotionalRequiresProvenance covers §8.5's first rule: no source or no as_of is a
// validation error at load, not a warning.
func TestNotionalRequiresProvenance(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"no source", `
rules:
  - { id: n, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "1", as_of: 2026-07-28 }
`, "source is required"},
		{"blank source", `
rules:
  - { id: n, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "1", source: "   ", as_of: 2026-07-28 }
`, "source is required"},
		{"no as_of", `
rules:
  - { id: n, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "1", source: "vendor page" }
`, "as_of is required"},
		{"neither", `
rules:
  - { id: n, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "1" }
`, "source is required"},
		{"as_of is not a date", `
rules:
  - { id: n, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "1", source: s, as_of: "last tuesday" }
`, "is not a date"},
		{"provenance on a marginal rule", `
rules:
  - { id: m, match: { model: m }, unit: per_1m_tokens, input: "1", source: s, as_of: 2026-07-28 }
`, "belong to a notional_rate rule"},
		{"provenance on a subscription rule", `
rules:
  - { id: s, class: fixed_subscription, match: { credential: c }, amount_per_period: "1", source: s, as_of: 2026-07-28 }
`, "belong to a notional_rate rule"},
		{"provenance on an adjustment rule", `
rules:
  - { id: a, class: adjustment, match: {}, op: percent, amount: "10", source: s, as_of: 2026-07-28 }
`, "belong to a notional_rate rule"},
		{"notional rules still validate their rates", `
rules:
  - { id: n, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "-1", source: s, as_of: 2026-07-28 }
`, "may not be negative"},
		{"a notional rule cannot be a subscription", `
rules:
  - { id: n, class: notional_rate, match: { model: m }, unit: subscription, source: s, as_of: 2026-07-28 }
`, "unit subscription requires class fixed_subscription"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCatalog([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected a load error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}

	// An RFC3339 as_of is accepted too, for a rate captured at a known instant.
	c := mustCatalog(t, `
rules:
  - { id: n, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "1", source: s, as_of: "2026-07-28T09:30:00Z" }
`)
	cost := mustPrice(t, c, Request{Model: "m", InputTokens: 1_000_000, At: at(t, "2026-07-28T12:00:00Z")})
	if cost.NotionalNano != 1_000_000_000 {
		t.Fatalf("notional = %d", cost.NotionalNano)
	}
}

const withAndWithoutNotional = `
currency: USD
rules:
  - { id: tokens,   match: { model: m }, unit: per_1m_tokens, input: "1.00" }
  - id: plan
    class: fixed_subscription
    match: { credential: c1 }
    amount_per_period: "20.00"
  - { id: discount, class: adjustment, match: {}, op: percent, amount: "-50" }
`

// TestNotionalNeverChangesTheBill is §8.5's second rule. The same catalog with and without
// notional rules must bill identically, down to the nano.
func TestNotionalNeverChangesTheBill(t *testing.T) {
	req := Request{Model: "m", Credential: "c1", InputTokens: 1_000_000,
		At: at(t, "2026-07-15T12:00:00Z")}

	plain := mustPrice(t, mustCatalog(t, withAndWithoutNotional), req)
	withNotional := mustPrice(t, mustCatalog(t, withAndWithoutNotional+`
  - { id: list, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "99.00", source: "vendor page", as_of: 2026-07-28 }
`), req)

	if !sameAmounts(plain, withNotional) {
		t.Fatalf("adding a notional rule changed the bill:\n without: %+v\n with:    %+v",
			plain, withNotional)
	}
	if plain.NotionalNano != 0 || !plain.NotionalMissing {
		t.Fatalf("a catalog with no notional rule must report the estimate unavailable: %+v", plain)
	}
	if withNotional.NotionalNano != 99_000_000_000 {
		t.Fatalf("notional = %d, want 99e9", withNotional.NotionalNano)
	}
	// An adjustment is a billing construct: a 50% discount on the bill must not discount
	// what the traffic is worth at list rates. The discount is half of the 1.00 of tokens
	// plus the 9.354838710 the 20.00 plan has accrued by mid-month, and the 99.00 estimate
	// is untouched by it.
	if withNotional.AdjustmentNano != -5_177_419_355 {
		t.Fatalf("adjustment = %d", withNotional.AdjustmentNano)
	}
	if withNotional.TotalNano != 5_177_419_355 {
		t.Fatalf("total = %d, want 5_177_419_355", withNotional.TotalNano)
	}

	// Settling must not move the notional figure into the ledger's billed total either.
	c := mustCatalog(t, withAndWithoutNotional+`
  - { id: list, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "99.00", source: "vendor page", as_of: 2026-07-28 }
`)
	settled, err := c.Settle(req)
	if err != nil {
		t.Fatal(err)
	}
	if settled.TotalNano != settled.MarginalNano+settled.SubscriptionNano+settled.AdjustmentNano {
		t.Fatalf("settled total %d is not the sum of the billing classes: %+v", settled.TotalNano, settled)
	}
	if settled.NotionalNano != 99_000_000_000 {
		t.Fatalf("settled notional = %d", settled.NotionalNano)
	}
}

// TestRoutingIgnoresNotional states the property §8.5 rule 2 protects: what traffic would
// cost elsewhere says nothing about the cost of the choice in front of the router.
func TestRoutingIgnoresNotional(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: plan-tokens, match: { deployment: dep-plan }, unit: per_1m_tokens, input: "0" }
  - { id: api-tokens,  match: { deployment: dep-api },  unit: per_1m_tokens, input: "0.20" }
  - { id: plan-list, class: notional_rate, match: { deployment: dep-plan }, unit: per_1m_tokens, input: "500.00", source: "vendor page", as_of: 2026-07-28 }
`)
	base := Request{Model: "m", InputTokens: 1_000_000, At: at(t, "2026-07-15T12:00:00Z")}
	plan, api := base, base
	plan.Deployment, api.Deployment = "dep-plan", "dep-api"

	planCost, apiCost := mustPrice(t, c, plan), mustPrice(t, c, api)
	if planCost.MarginalNano != 0 || apiCost.MarginalNano != 200_000_000 {
		t.Fatalf("marginals: plan %d, api %d", planCost.MarginalNano, apiCost.MarginalNano)
	}
	if planCost.MarginalNano >= apiCost.MarginalNano {
		t.Fatal("a huge notional figure made the plan look expensive to the router")
	}
	if planCost.NotionalNano != 500_000_000_000 {
		t.Fatalf("notional = %d", planCost.NotionalNano)
	}
}

// TestNotionalMissingIsSeparateFromMissing covers §8.5's third rule: the two failures have
// different consequences, so they are different flags.
func TestNotionalMissingIsSeparateFromMissing(t *testing.T) {
	billableOnly := mustCatalog(t, `
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "1.00" }
`)
	estimateOnly := mustCatalog(t, `
rules:
  - { id: list, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "1.00", source: s, as_of: 2026-07-28 }
`)
	req := Request{Model: "m", InputTokens: 1_000_000, At: at(t, "2026-07-15T12:00:00Z")}

	cost := mustPrice(t, billableOnly, req)
	if cost.Missing || !cost.NotionalMissing {
		t.Fatalf("billable but not estimable: Missing=%v NotionalMissing=%v", cost.Missing, cost.NotionalMissing)
	}
	if cost.NotionalNano != 0 {
		t.Fatalf("notional = %d; the flag, not the number, reports unavailability", cost.NotionalNano)
	}

	cost = mustPrice(t, estimateOnly, req)
	if !cost.Missing || cost.NotionalMissing {
		t.Fatalf("estimable but not billable: Missing=%v NotionalMissing=%v", cost.Missing, cost.NotionalMissing)
	}
	if cost.TotalNano != 0 || cost.NotionalNano != 1_000_000_000 {
		t.Fatalf("cost = %+v", cost)
	}

	// A model outside both catalogs is unavailable on both counts.
	other := req
	other.Model = "unknown"
	cost = mustPrice(t, estimateOnly, other)
	if !cost.Missing || !cost.NotionalMissing {
		t.Fatalf("cost = %+v", cost)
	}
}

func TestNotionalComponentsStayOutOfTheBilledBreakdown(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "1.00" }
  - id: list
    class: notional_rate
    match: { model: m }
    unit: per_1m_tokens
    input:  "2.00"
    output: "3.00"
    source: "vendor page"
    as_of: 2026-07-28
`)
	req := Request{Model: "m", InputTokens: 1_000_000, OutputTokens: 1_000_000,
		At: at(t, "2026-07-15T12:00:00Z")}
	cost := mustPrice(t, c, req)

	var sum int64
	for _, comp := range cost.Components {
		if comp.RuleID != "tokens" {
			t.Fatalf("a notional component reached Cost.Components: %+v", comp)
		}
		sum += comp.SubtotalNano
	}
	if sum != cost.MarginalNano {
		t.Fatalf("component subtotals %d != marginal %d", sum, cost.MarginalNano)
	}
	if cost.NotionalNano != 5_000_000_000 {
		t.Fatalf("notional = %d", cost.NotionalNano)
	}
	// The rule that produced it is still identified, so the number is attributable.
	if got := ruleIDs(cost, ClassNotional); len(got) != 1 || got[0] != "list" {
		t.Fatalf("notional applied rules = %v", got)
	}
}

func TestNotionalSpecificityAndTieBreaking(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: n-credential, class: notional_rate, match: { credential: c1 },          unit: per_1m_tokens, input: "7", source: s, as_of: 2026-07-28 }
  - { id: n-model,      class: notional_rate, match: { model: m1 },               unit: per_1m_tokens, input: "4", source: s, as_of: 2026-07-28 }
  - { id: n-prefix,     class: notional_rate, match: { model_prefix: "m" },       unit: per_1m_tokens, input: "3", source: s, as_of: 2026-07-28 }
  - { id: n-default,    class: notional_rate, match: {},                          unit: per_1m_tokens, input: "1", source: s, as_of: 2026-07-28 }
  - { id: aaa-tie,      class: notional_rate, match: { model: m2 },               unit: per_1m_tokens, input: "8", source: s, as_of: 2026-07-28 }
  - { id: bbb-tie,      class: notional_rate, match: { model: m2 },               unit: per_1m_tokens, input: "9", source: s, as_of: 2026-07-28 }
  - { id: ccc-priority, class: notional_rate, match: { model: m2 },               unit: per_1m_tokens, input: "6", source: s, as_of: 2026-07-28, priority: 5 }
`)
	now := at(t, "2026-07-28T12:00:00Z")
	cases := []struct {
		req  Request
		want string
	}{
		{Request{Credential: "c1", Model: "m1"}, "n-credential"},
		{Request{Model: "m1"}, "n-model"},
		{Request{Model: "mzz"}, "n-prefix"},
		{Request{Model: "zzz"}, "n-default"},
		{Request{Model: "m2"}, "ccc-priority"}, // priority wins inside a level
	}
	for _, tc := range cases {
		tc.req.InputTokens, tc.req.At = 1_000_000, now
		cost := mustPrice(t, c, tc.req)
		if got := ruleIDs(cost, ClassNotional); len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%+v selected %v, want %s", tc.req, got, tc.want)
		}
	}
	// With priority removed the lexicographically smaller id wins, deterministically.
	c2 := mustCatalog(t, strings.ReplaceAll(`
rules:
  - { id: aaa-tie, class: notional_rate, match: { model: m2 }, unit: per_1m_tokens, input: "8", source: s, as_of: 2026-07-28 }
  - { id: bbb-tie, class: notional_rate, match: { model: m2 }, unit: per_1m_tokens, input: "9", source: s, as_of: 2026-07-28 }
`, "\t", ""))
	for i := 0; i < 25; i++ {
		cost := mustPrice(t, c2, Request{Model: "m2", InputTokens: 1_000_000, At: now})
		if got := ruleIDs(cost, ClassNotional)[0]; got != "aaa-tie" {
			t.Fatalf("iteration %d selected %s", i, got)
		}
	}
}

func TestNotionalHonoursTimeWindowsAndTiers(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - id: night-list
    class: notional_rate
    match: { model: m }
    when: { time_of_day: "22:00-08:00", tz: Asia/Seoul }
    unit: per_1m_tokens
    input: "1.00"
    priority: 10
    source: "vendor page, night rate"
    as_of: 2026-07-28
  - id: day-list
    class: notional_rate
    match: { model: m }
    unit: per_1m_tokens
    tier_mode: graduated
    tiers:
      - { up_to_input_tokens: 200000, input: "1.25" }
      - { input: "2.50" }
    source: "vendor page"
    as_of: 2026-07-28
`)
	night := mustPrice(t, c, Request{Model: "m", InputTokens: 300_000, At: at(t, "2026-07-28T17:00:00Z")})
	if got := ruleIDs(night, ClassNotional)[0]; got != "night-list" {
		t.Fatalf("selected %s at 02:00 Seoul", got)
	}
	if night.NotionalNano != 300_000_000 {
		t.Fatalf("night notional = %d", night.NotionalNano)
	}
	day := mustPrice(t, c, Request{Model: "m", InputTokens: 300_000, At: at(t, "2026-07-28T03:00:00Z")})
	if want := int64(250_000_000 + 250_000_000); day.NotionalNano != want {
		t.Fatalf("graduated notional = %d, want %d", day.NotionalNano, want)
	}
}

func TestNotionalCarriesItsOwnRemainder(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "1.00" }
  - { id: list, class: notional_rate, match: { model: m }, unit: per_1m_tokens, input: "0.0005", source: s, as_of: 2026-07-28 }
`)
	req := Request{Model: "m", Credential: "acct", InputTokens: 1, At: at(t, "2026-07-15T12:00:00Z")}
	var total int64
	for i := 0; i < 2000; i++ {
		cost, err := c.Settle(req)
		if err != nil {
			t.Fatal(err)
		}
		total += cost.NotionalNano
	}
	if total != 1000 {
		t.Fatalf("2000 single-token estimates summed to %d nano, want 1000", total)
	}
}

func TestExplainAuditsTheNotionalFigure(t *testing.T) {
	c := mustCatalog(t, designExample)
	req := Request{
		Provider: "plan-a", Model: "model-x", Credential: "plan-a-1",
		InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 400_000,
		At: at(t, "2026-08-27T12:00:00Z"),
	}
	ex := c.Explain(req)
	if ex.Err != "" {
		t.Fatalf("Explain error: %s", ex.Err)
	}
	n := ex.Notional
	if n.RuleID != "plan-a-list-rate" {
		t.Fatalf("rule id = %q", n.RuleID)
	}
	if n.Source != "vendor public price page" {
		t.Fatalf("source = %q", n.Source)
	}
	if n.AsOfText != "2026-07-28" || !n.AsOf.Equal(time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("as_of = %q / %v", n.AsOfText, n.AsOf)
	}
	if want := 30 * 24 * time.Hour; n.Age < want {
		t.Fatalf("age = %s, want at least %s so staleness is visible", n.Age, want)
	}
	if n.Missing || n.Nano != 3_986_000_000 {
		t.Fatalf("notional detail = %+v", n)
	}
	if len(n.Components) != 3 {
		t.Fatalf("components = %+v, want one per priced field", n.Components)
	}
	for _, comp := range n.Components {
		if comp.RuleID != "plan-a-list-rate" {
			t.Fatalf("component from the wrong rule: %+v", comp)
		}
	}
	trace := ex.Classes[ClassNotional]
	if len(trace.Considered) != 1 || !trace.Considered[0].Selected {
		t.Fatalf("notional trace = %+v", trace.Considered)
	}
	var sawProvenance bool
	for _, note := range ex.Notes {
		if strings.Contains(note, "vendor public price page") && strings.Contains(note, "2026-07-28") &&
			strings.Contains(note, "excluded from TotalNano") {
			sawProvenance = true
		}
	}
	if !sawProvenance {
		t.Fatalf("notes do not state the estimate's provenance and exclusion: %v", ex.Notes)
	}

	// An unavailable estimate says so, and says it separately from an unpriced model.
	ex = c.Explain(Request{Provider: "plan-a", Model: "other", Credential: "plan-a-1",
		InputTokens: 1, At: req.At})
	if !ex.Notional.Missing || ex.Notional.RuleID != "" {
		t.Fatalf("notional detail = %+v", ex.Notional)
	}
	var sawUnavailable bool
	for _, note := range ex.Notes {
		if strings.Contains(note, "unavailable, not zero") {
			sawUnavailable = true
		}
	}
	if !sawUnavailable {
		t.Fatalf("notes = %v", ex.Notes)
	}
}

// TestNotionalLeverage exercises the arithmetic §8.5 says the feature buys: notional over
// amortized subscription is the plan's realized leverage.
func TestNotionalLeverage(t *testing.T) {
	c := mustCatalog(t, designExample)
	req := Request{
		Provider: "plan-a", Model: "model-x", Credential: "plan-a-1",
		InputTokens: 10_000_000, OutputTokens: 10_000_000,
		At: at(t, "2026-07-15T12:00:00Z"),
	}
	var notional, subscription int64
	for i := 0; i < 100; i++ {
		cost, err := c.Settle(req)
		if err != nil {
			t.Fatal(err)
		}
		notional += cost.NotionalNano
		subscription += cost.SubscriptionNano
	}
	if want := int64(100 * (8_500_000_000 + 34_000_000_000)); notional != want {
		t.Fatalf("notional over the period = %d, want %d", notional, want)
	}
	// The plan bills 20.00 for the month however the elapsed-fraction fallback apportions
	// it, and the traffic is worth 4250.00 at list rates: leverage well above one.
	if subscription <= 0 || notional/subscription < 100 {
		t.Fatalf("leverage = %d / %d", notional, subscription)
	}
}
