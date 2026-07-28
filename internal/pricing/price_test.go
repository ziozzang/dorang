package pricing

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const planAndTokens = `
currency: USD
rules:
  - id: plan-a-model-x
    class: marginal_usage
    match: { provider: plan-a, model: model-x }
    unit: per_1m_tokens
    input:  "0.85"
    output: "3.40"
  - id: plan-a-subscription
    class: fixed_subscription
    match: { credential: plan-a-1 }
    unit: subscription
    amount_per_period: "20.00"
    period: monthly
`

// TestSubscriptionDoesNotZeroTokenCost is the regression test for REVIEW.md finding 9.
//
// The credential-scoped subscription rule is strictly more specific than the model-scoped
// token rule. Under revision 1's single-winner model it therefore won outright and the
// token cost vanished. Classes must keep both: the token cost is unchanged by the presence
// of the subscription, and the subscription is reported separately.
func TestSubscriptionDoesNotZeroTokenCost(t *testing.T) {
	req := Request{
		Provider: "plan-a", Model: "model-x", Credential: "plan-a-1",
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
		At: at(t, "2026-07-15T12:00:00Z"),
	}

	withPlan := mustPrice(t, mustCatalog(t, planAndTokens), req)

	// The same catalog with the subscription rule removed must produce the same marginal.
	tokensOnly := mustPrice(t, mustCatalog(t, planAndTokens[:strings.Index(planAndTokens, "  - id: plan-a-subscription")]), req)

	const wantMarginal = 4_250_000_000 // 0.85 + 3.40 USD
	if tokensOnly.MarginalNano != wantMarginal {
		t.Fatalf("token-only marginal = %d, want %d", tokensOnly.MarginalNano, wantMarginal)
	}
	if withPlan.MarginalNano != wantMarginal {
		t.Fatalf("a credential-scoped subscription zeroed the model-scoped token cost: "+
			"marginal = %d, want %d", withPlan.MarginalNano, wantMarginal)
	}
	// 20.00 x 14.5 days of a 31 day month: what the plan has accrued by this instant,
	// reported separately from the token cost rather than replacing it.
	if withPlan.SubscriptionNano != 9_354_838_710 {
		t.Fatalf("subscription = %d, want 9_354_838_710", withPlan.SubscriptionNano)
	}
	if withPlan.TotalNano != withPlan.MarginalNano+withPlan.SubscriptionNano+withPlan.AdjustmentNano {
		t.Fatalf("total %d is not the sum of its parts %+v", withPlan.TotalNano, withPlan)
	}
	if got := ruleIDs(withPlan, ClassMarginal); len(got) != 1 || got[0] != "plan-a-model-x" {
		t.Fatalf("marginal rules = %v", got)
	}
	if got := ruleIDs(withPlan, ClassSubscription); len(got) != 1 || got[0] != "plan-a-subscription" {
		t.Fatalf("subscription rules = %v", got)
	}
	if withPlan.Missing {
		t.Fatal("Missing must be false when a marginal rule matched")
	}
}

// TestRoutingSeesOnlyMarginal states the property routing depends on: a sunk plan cost
// never enters the number routing compares.
func TestRoutingSeesOnlyMarginal(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - { id: cheap-tokens, match: { deployment: dep-plan }, unit: per_1m_tokens, input: "0.10" }
  - { id: dear-tokens,  match: { deployment: dep-api },  unit: per_1m_tokens, input: "0.20" }
  - id: plan
    class: fixed_subscription
    match: { deployment: dep-plan }
    amount_per_period: "200.00"
`)
	base := Request{Model: "m", InputTokens: 1_000_000, At: at(t, "2026-07-15T12:00:00Z")}
	plan, api := base, base
	plan.Deployment, api.Deployment = "dep-plan", "dep-api"

	planCost, apiCost := mustPrice(t, c, plan), mustPrice(t, c, api)
	if planCost.MarginalNano >= apiCost.MarginalNano {
		t.Fatalf("routing comparison broken: plan %d, api %d", planCost.MarginalNano, apiCost.MarginalNano)
	}
	if planCost.TotalNano <= apiCost.TotalNano {
		t.Fatal("the test is not exercising the hazard: the plan's total should look expensive")
	}
	if planCost.SubscriptionNano == 0 {
		t.Fatal("expected an amortized subscription share")
	}
}

// TestSubscriptionSharesAreScopedToTheirPeriod: what a period has attributed belongs to
// that period. A denser burst inside it takes smaller shares rather than a larger total,
// and the next period starts from zero rather than from where the last one stopped.
//
// The convergence property itself — that a period's shares sum to the plan cost — is
// TestAPeriodAttributesExactlyThePlanCost.
func TestSubscriptionSharesAreScopedToTheirPeriod(t *testing.T) {
	c := mustCatalog(t, planAndTokens)
	req := Request{
		Provider: "plan-a", Model: "model-x", Credential: "plan-a-1",
		InputTokens: 1_000_000, At: at(t, "2026-07-15T12:00:00Z"),
	}
	// Five settlements one second apart, deep inside a 31 day month.
	var july int64
	for i := 1; i <= 5; i++ {
		cost, err := c.Settle(req)
		if err != nil {
			t.Fatal(err)
		}
		july += cost.SubscriptionNano
		if cost.MarginalNano != 850_000_000 {
			t.Fatalf("settlement %d: marginal = %d", i, cost.MarginalNano)
		}
		req.At = req.At.Add(time.Second)
	}
	// 20.00 x 14.5 days of 31 (9.354838710), plus the four seconds the burst spans
	// (29868 nano), and not a cent more however many requests shared it.
	if want := int64(9_354_838_710 + 29_868); july != want {
		t.Fatalf("five settlements attributed %d over four seconds of a month, want %d",
			july, want)
	}
	// A request in the next period starts over: it takes what August has accrued, not
	// what July had left.
	next := req
	next.At = at(t, "2026-08-02T00:00:00Z") // one day into a 31 day August
	cost, err := c.Settle(next)
	if err != nil {
		t.Fatal(err)
	}
	// 20.00 x 1 day of 31, plus the sub-nano remainder July carried into it.
	if want := int64(645_161_291); cost.SubscriptionNano != want {
		t.Fatalf("new period: subscription = %d, want %d", cost.SubscriptionNano, want)
	}
}

// TestSubscriptionAttributionIsUnchangedWithoutAMarginalRule states that the shape of the
// traffic does not change how a plan cost is attributed: a plan with no marginal_usage
// rule at all — the usual shape of a flat plan — accrues exactly as one beside a token
// rule does. This used to be a separate fallback path taken only when the period's
// marginal usage was zero; it is now the one rule, because a denominator built from
// usage-to-date cannot bound the total (§8.1).
func TestSubscriptionAttributionIsUnchangedWithoutAMarginalRule(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - id: plan
    class: fixed_subscription
    match: { credential: c1 }
    amount_per_period: "31.00"
    period: monthly
`)
	// A month with no marginal usage at all: each share is what the plan accrued since
	// the previous settlement, so a whole period's settlements sum to the plan cost.
	req := Request{Credential: "c1", At: at(t, "2026-07-11T00:00:00Z")}
	cost, err := c.Settle(req)
	if err != nil {
		t.Fatal(err)
	}
	// 10 days of a 31 day month.
	if want := int64(10_000_000_000); cost.SubscriptionNano != want {
		t.Fatalf("subscription = %d, want %d", cost.SubscriptionNano, want)
	}
	if !cost.Missing {
		t.Fatal("no marginal rule matched, so Missing must be set")
	}
	req.At = at(t, "2026-07-21T00:00:00Z")
	cost, err = c.Settle(req)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(10_000_000_000); cost.SubscriptionNano != want {
		t.Fatalf("second settlement = %d, want %d", cost.SubscriptionNano, want)
	}
}

func TestThresholdAndGraduatedTiering(t *testing.T) {
	const tiers = `
rules:
  - id: tiered
    match: { model: t1 }
    unit: per_1m_tokens
    tier_mode: MODE
    tiers:
      - { up_to_input_tokens: 200000, input: "1.25", output: "10.00" }
      - { up_to_input_tokens: null,   input: "2.50", output: "15.00" }
`
	req := Request{Model: "t1", InputTokens: 300_000, OutputTokens: 1_000, At: at(t, "2026-07-15T12:00:00Z")}

	threshold := mustPrice(t, mustCatalog(t, strings.Replace(tiers, "MODE", "threshold", 1)), req)
	// The whole request is priced in the reached tier: 300k x 2.50/1M + 1k x 15.00/1M.
	if want := int64(750_000_000 + 15_000_000); threshold.MarginalNano != want {
		t.Fatalf("threshold marginal = %d, want %d", threshold.MarginalNano, want)
	}

	graduated := mustPrice(t, mustCatalog(t, strings.Replace(tiers, "MODE", "graduated", 1)), req)
	// 200k at 1.25 plus 100k at 2.50, output at the reached tier's rate.
	if want := int64(250_000_000 + 250_000_000 + 15_000_000); graduated.MarginalNano != want {
		t.Fatalf("graduated marginal = %d, want %d", graduated.MarginalNano, want)
	}
	if len(graduated.Components) != 3 {
		t.Fatalf("graduated components = %+v, want one per bracket plus output", graduated.Components)
	}
	if graduated.Components[0].Quantity != 200_000 || graduated.Components[0].Rate != "1.25" {
		t.Fatalf("first bracket = %+v", graduated.Components[0])
	}
	if graduated.Components[1].Quantity != 100_000 || graduated.Components[1].Rate != "2.50" {
		t.Fatalf("second bracket = %+v", graduated.Components[1])
	}

	// A small request lands in the first tier under either mode, identically.
	small := req
	small.InputTokens = 100_000
	a := mustPrice(t, mustCatalog(t, strings.Replace(tiers, "MODE", "threshold", 1)), small)
	b := mustPrice(t, mustCatalog(t, strings.Replace(tiers, "MODE", "graduated", 1)), small)
	if a.MarginalNano != b.MarginalNano || a.MarginalNano != 125_000_000+10_000_000 {
		t.Fatalf("first-tier request: threshold %d, graduated %d", a.MarginalNano, b.MarginalNano)
	}
}

func TestTierInheritsRulesRates(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - id: tiered
    match: { model: t1 }
    unit: per_1m_tokens
    output: "9.00"
    tiers:
      - { up_to_input_tokens: 100, input: "1.00" }
      - { input: "2.00" }
`)
	cost := mustPrice(t, c, Request{Model: "t1", InputTokens: 1_000_000, OutputTokens: 1_000_000,
		At: at(t, "2026-07-15T12:00:00Z")})
	if want := int64(2_000_000_000 + 9_000_000_000); cost.MarginalNano != want {
		t.Fatalf("marginal = %d, want %d", cost.MarginalNano, want)
	}
}

func TestAdjustmentsApplyInOrderAndCompose(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "10.00" }
  - { id: discount, class: adjustment, match: { credential: c1 }, op: percent, amount: "-10", order: 10 }
  - { id: margin,   class: adjustment, match: {},                 op: percent, amount: "20",  order: 20 }
  - { id: tax,      class: adjustment, match: {},                 op: percent, amount: "10",  order: 30 }
`)
	req := Request{Model: "m", Credential: "c1", InputTokens: 1_000_000, At: at(t, "2026-07-15T12:00:00Z")}
	cost := mustPrice(t, c, req)

	// 10.00 -> -10% -> 9.00 -> +20% -> 10.80 -> +10% -> 11.88
	if cost.MarginalNano != 10_000_000_000 {
		t.Fatalf("marginal = %d", cost.MarginalNano)
	}
	if want := int64(1_880_000_000); cost.AdjustmentNano != want {
		t.Fatalf("adjustment = %d, want %d", cost.AdjustmentNano, want)
	}
	if want := int64(11_880_000_000); cost.TotalNano != want {
		t.Fatalf("total = %d, want %d", cost.TotalNano, want)
	}
	if got := ruleIDs(cost, ClassAdjustment); len(got) != 3 ||
		got[0] != "discount" || got[1] != "margin" || got[2] != "tax" {
		t.Fatalf("adjustment order = %v", got)
	}
	if cost.AppliedRules[1].Why() == "" || !strings.Contains(cost.AppliedRules[1].Why(), "every matching") {
		t.Fatalf("adjustment Why() = %q", cost.AppliedRules[1].Why())
	}

	// Order matters, and it is the declared order, not the file order or the specificity.
	reordered := mustCatalog(t, `
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "10.00" }
  - { id: discount, class: adjustment, match: { credential: c1 }, op: percent, amount: "-10", order: 30 }
  - { id: margin,   class: adjustment, match: {},                 op: percent, amount: "20",  order: 20 }
  - { id: tax,      class: adjustment, match: {},                 op: percent, amount: "10",  order: 10 }
`)
	// 10.00 -> +10% -> 11.00 -> +20% -> 13.20 -> -10% -> 11.88; same here, so use a
	// non-commuting shape to prove ordering: an absolute add between two percentages.
	other := mustPrice(t, reordered, req)
	if got := ruleIDs(other, ClassAdjustment); got[0] != "tax" || got[2] != "discount" {
		t.Fatalf("reordered adjustment order = %v", got)
	}

	noncommuting := mustCatalog(t, `
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "10.00" }
  - { id: fee,    class: adjustment, match: {}, op: add,     amount: "5.00", order: 10 }
  - { id: markup, class: adjustment, match: {}, op: percent, amount: "50",   order: 20 }
`)
	first := mustPrice(t, noncommuting, req)
	if want := int64(22_500_000_000); first.TotalNano != want { // (10+5) * 1.5
		t.Fatalf("fee-then-markup total = %d, want %d", first.TotalNano, want)
	}
	swapped := mustCatalog(t, strings.Replace(strings.Replace(
		`
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "10.00" }
  - { id: fee,    class: adjustment, match: {}, op: add,     amount: "5.00", order: 10 }
  - { id: markup, class: adjustment, match: {}, op: percent, amount: "50",   order: 20 }
`, "order: 10", "order: 99", 1), "order: 20", "order: 1", 1))
	second := mustPrice(t, swapped, req)
	if want := int64(20_000_000_000); second.TotalNano != want { // 10*1.5 + 5
		t.Fatalf("markup-then-fee total = %d, want %d", second.TotalNano, want)
	}
}

func TestAdjustmentAppliesToSelectedBase(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "10.00" }
  - id: plan
    class: fixed_subscription
    match: { credential: c1 }
    amount_per_period: "20.00"
  - { id: on-marginal, class: adjustment, match: {}, op: percent, amount: "-50", applies_to: marginal }
`)
	req := Request{Model: "m", Credential: "c1", InputTokens: 1_000_000, At: at(t, "2026-07-15T12:00:00Z")}
	cost := mustPrice(t, c, req)
	if cost.AdjustmentNano != -5_000_000_000 {
		t.Fatalf("adjustment = %d, want -5e9 (half of the marginal only)", cost.AdjustmentNano)
	}
	const plan = int64(9_354_838_710) // 20.00 x 14.5 days of a 31 day month
	if cost.SubscriptionNano != plan {
		t.Fatalf("subscription = %d, want %d", cost.SubscriptionNano, plan)
	}
	if want := 10_000_000_000 + plan - 5_000_000_000; cost.TotalNano != want {
		t.Fatalf("total = %d, want %d", cost.TotalNano, want)
	}
}

func TestAdjustmentMultiply(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "10.00" }
  - { id: retail, class: adjustment, match: {}, op: multiply, amount: "1.30" }
`)
	cost := mustPrice(t, c, Request{Model: "m", InputTokens: 1_000_000, At: at(t, "2026-07-15T12:00:00Z")})
	if want := int64(3_000_000_000); cost.AdjustmentNano != want {
		t.Fatalf("adjustment = %d, want %d", cost.AdjustmentNano, want)
	}
	if want := int64(13_000_000_000); cost.TotalNano != want {
		t.Fatalf("total = %d, want %d", cost.TotalNano, want)
	}
}

func TestUnitsOtherThanTokens(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: per-req,   match: { model: r }, unit: per_request,       request:    "0.002" }
  - { id: per-char,  match: { model: c }, unit: per_1k_characters, characters: "0.030" }
  - { id: per-sec,   match: { model: s }, unit: per_second,        seconds:    "0.006" }
`)
	now := at(t, "2026-07-15T12:00:00Z")

	// Requests defaults to one, because Price prices one request.
	cost := mustPrice(t, c, Request{Model: "r", At: now})
	if cost.MarginalNano != 2_000_000 {
		t.Fatalf("per_request default = %d, want 2_000_000", cost.MarginalNano)
	}
	cost = mustPrice(t, c, Request{Model: "r", Requests: 7, At: now})
	if cost.MarginalNano != 14_000_000 {
		t.Fatalf("per_request x7 = %d", cost.MarginalNano)
	}

	cost = mustPrice(t, c, Request{Model: "c", Characters: 2_500, At: now})
	if cost.MarginalNano != 75_000_000 {
		t.Fatalf("per_1k_characters = %d, want 75_000_000", cost.MarginalNano)
	}

	cost = mustPrice(t, c, Request{Model: "s", Seconds: 12.5, At: now})
	if cost.MarginalNano != 75_000_000 {
		t.Fatalf("per_second = %d, want 75_000_000", cost.MarginalNano)
	}
	if cost.Components[0].Scale != 6 || cost.Components[0].Quantity != 12_500_000 {
		t.Fatalf("seconds component = %+v, want micro-seconds", cost.Components[0])
	}

	if _, err := c.Price(Request{Model: "s", Seconds: -1, At: now}); err == nil {
		t.Fatal("negative seconds must be an error")
	}
	if _, err := c.Price(Request{Model: "r", Requests: -3, At: now}); err == nil {
		t.Fatal("a negative quantity must be an error")
	}
}

func TestCachedAndReasoningComponents(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - id: full
    match: { model: m }
    unit: per_1m_tokens
    input: "0.85"
    output: "3.40"
    cache_read: "0.19"
    cache_write: "0"
    reasoning: "0"
`)
	cost := mustPrice(t, c, Request{
		Model: "m", InputTokens: 1_000_000, OutputTokens: 1_000_000,
		CacheReadTokens: 1_000_000, CacheWriteTokens: 1_000_000, ReasoningTokens: 1_000_000,
		At: at(t, "2026-07-15T12:00:00Z"),
	})
	if want := int64(4_440_000_000); cost.MarginalNano != want {
		t.Fatalf("marginal = %d, want %d", cost.MarginalNano, want)
	}
	if len(cost.Components) != 5 {
		t.Fatalf("components = %d, want 5 (a rate of zero is still an explicit price)", len(cost.Components))
	}
	var sum int64
	for _, comp := range cost.Components {
		sum += comp.SubtotalNano
	}
	if sum != cost.MarginalNano {
		t.Fatalf("component subtotals %d != marginal %d", sum, cost.MarginalNano)
	}
}

func TestOverflowIsAnErrorNotANegativeCost(t *testing.T) {
	cases := []struct {
		name, yaml string
		req        Request
	}{
		{
			"nano range", `
rules:
  - { id: huge, match: { model: m }, unit: per_request, request: "1000000" }
`, Request{Model: "m", Requests: 10_000_000_000_000},
		},
		{
			"128-bit product", `
rules:
  - { id: huge, match: { model: m }, unit: per_1m_tokens, input: "1000000000000000000" }
`, Request{Model: "m", InputTokens: 1_000_000_000_000_000_000},
		},
		{
			"adjustment blow-up", `
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "1000000" }
  - { id: silly,  class: adjustment, match: {}, op: multiply, amount: "999999999999" }
`, Request{Model: "m", InputTokens: 1_000_000_000_000},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustCatalog(t, tc.yaml)
			tc.req.At = at(t, "2026-07-15T12:00:00Z")
			cost, err := c.Price(tc.req)
			if err == nil {
				t.Fatalf("expected an overflow error, got %+v", cost)
			}
			if !errors.Is(err, ErrOverflow) {
				t.Fatalf("error = %v, want ErrOverflow", err)
			}
			if cost.TotalNano != 0 || cost.MarginalNano < 0 || cost.TotalNano < 0 {
				t.Fatalf("overflow produced a cost instead of an error: %+v", cost)
			}
		})
	}
}

func TestMissingIsDetectable(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: known, match: { model: known-model }, unit: per_1m_tokens, input: "1.00" }
`)
	cost := mustPrice(t, c, Request{Model: "unpriced-model", InputTokens: 1_000_000,
		At: at(t, "2026-07-15T12:00:00Z")})
	if !cost.Missing {
		t.Fatal("an unpriced model must be reported as Missing")
	}
	if cost.TotalNano != 0 || len(cost.AppliedRules) != 0 || len(cost.Components) != 0 {
		t.Fatalf("unpriced cost = %+v", cost)
	}
	ex := c.Explain(Request{Model: "unpriced-model", InputTokens: 1_000_000, At: at(t, "2026-07-15T12:00:00Z")})
	if len(ex.Notes) == 0 || !strings.Contains(ex.Notes[0], "unpriced") {
		t.Fatalf("Explain notes = %v", ex.Notes)
	}
}

func TestExplainReportsTheChainAndWhy(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - { id: night, match: { model: m }, when: { time_of_day: "22:00-08:00" }, unit: per_1m_tokens, input: "1.00", priority: 10 }
  - { id: day,   match: { model: m }, unit: per_1m_tokens, input: "5.00" }
  - { id: other, match: { model: z }, unit: per_1m_tokens, input: "9.00" }
  - id: plan
    class: fixed_subscription
    match: { credential: c1 }
    amount_per_period: "20.00"
  - { id: tax, class: adjustment, match: {}, op: percent, amount: "10" }
`)
	ex := c.Explain(Request{Model: "m", Credential: "c1", InputTokens: 1_000_000,
		At: at(t, "2026-07-15T12:00:00Z")}) // 12:00 UTC, outside the night window
	if ex.Err != "" {
		t.Fatalf("Explain error: %s", ex.Err)
	}
	if ex.Currency != "USD" {
		t.Fatalf("currency = %q", ex.Currency)
	}
	if ex.Cost.MarginalNano != 5_000_000_000 {
		t.Fatalf("marginal = %d", ex.Cost.MarginalNano)
	}
	marginalTrace := ex.Classes[ClassMarginal]
	var sawNightRejected, sawDaySelected bool
	for _, cd := range marginalTrace.Considered {
		switch cd.RuleID {
		case "night":
			sawNightRejected = !cd.Eligible && strings.Contains(cd.Reason, "time predicate")
		case "day":
			sawDaySelected = cd.Selected && strings.Contains(cd.Reason, "most specific")
		case "other":
			t.Fatal("the index let a rule for a different model be considered")
		}
	}
	if !sawNightRejected {
		t.Fatalf("night rule not reported as time-rejected: %+v", marginalTrace.Considered)
	}
	if !sawDaySelected {
		t.Fatalf("day rule not reported as selected: %+v", marginalTrace.Considered)
	}
	if len(ex.Classes[ClassSubscription].Considered) != 1 || !ex.Classes[ClassSubscription].Considered[0].Selected {
		t.Fatalf("subscription trace = %+v", ex.Classes[ClassSubscription].Considered)
	}
	if len(ex.Classes[ClassAdjustment].Considered) != 1 || !ex.Classes[ClassAdjustment].Considered[0].Selected {
		t.Fatalf("adjustment trace = %+v", ex.Classes[ClassAdjustment].Considered)
	}
	if len(ex.Notes) == 0 {
		t.Fatal("expected a note that the subscription share is accounting-only")
	}
}

func TestPriceDoesNotMutateAndSettleDoes(t *testing.T) {
	c := mustCatalog(t, planAndTokens)
	req := Request{Provider: "plan-a", Model: "model-x", Credential: "plan-a-1",
		InputTokens: 1_000_000, At: at(t, "2026-07-15T12:00:00Z")}

	first := mustPrice(t, c, req)
	for i := 0; i < 10; i++ {
		again := mustPrice(t, c, req)
		if !sameAmounts(again, first) {
			t.Fatalf("Price is not idempotent: %+v then %+v", first, again)
		}
	}
	if _, err := c.Settle(req); err != nil {
		t.Fatal(err)
	}
	after := mustPrice(t, c, req)
	if after.SubscriptionNano >= first.SubscriptionNano {
		t.Fatalf("Settle did not advance the period accumulator: %d then %d",
			first.SubscriptionNano, after.SubscriptionNano)
	}
}

// sameAmounts compares the money in two costs, ignoring slice identity.
func sameAmounts(a, b Cost) bool {
	return a.MarginalNano == b.MarginalNano && a.SubscriptionNano == b.SubscriptionNano &&
		a.AdjustmentNano == b.AdjustmentNano && a.TotalNano == b.TotalNano
}

func TestConcurrentPriceAndSettle(t *testing.T) {
	c := mustCatalog(t, planAndTokens)
	req := Request{Provider: "plan-a", Model: "model-x", Credential: "plan-a-1",
		InputTokens: 1_000_000, At: at(t, "2026-07-15T12:00:00Z")}
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			for j := 0; j < 200; j++ {
				var err error
				if i%2 == 0 {
					_, err = c.Price(req)
				} else {
					_, err = c.Settle(req)
				}
				if err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}(i)
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestZeroAtMeansNow(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: always, match: { model: m }, unit: per_1m_tokens, input: "1.00" }
`)
	cost := mustPrice(t, c, Request{Model: "m", InputTokens: 1_000_000})
	if cost.MarginalNano != 1_000_000_000 {
		t.Fatalf("marginal = %d", cost.MarginalNano)
	}
	_ = time.Now
}
