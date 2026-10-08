package pricing

import (
	"strings"
	"testing"
)

// The figures are the vendors' own: DeepInfra stated 3.4e-07 USD (340 nano) for a
// 14-token chat on a model listed at $0.02 / $0.03 per million, and OpenRouter
// 8.8e-07 USD (880 nano) for 35 tokens.
const reportedRateCard = `
currency: USD
rules:
  - id: deepinfra-reported
    class: marginal_usage
    match: { provider: deepinfra }
    reported_cost: "1"
    input: "0.02"
    output: "0.03"
  - id: openrouter-reported-only
    class: marginal_usage
    match: { provider: openrouter }
    reported_cost: "1"
  - id: marked-up
    class: marginal_usage
    match: { provider: resold }
    reported_cost: "1.1"
  - id: whisper
    class: marginal_usage
    match: { provider: deepinfra, model: whisper }
    unit: per_audio_second
    audio_seconds: "0.00000333"
    reported_cost: "1"
`

func TestAStatedCostIsTheWholePrice(t *testing.T) {
	c := mustCatalog(t, reportedRateCard)
	cost := mustPrice(t, c, Request{
		Provider: "deepinfra", Model: "llama", InputTokens: 11, OutputTokens: 3,
		ReportedCostNano: 340, ReportedCostStated: true,
	})
	if cost.MarginalNano != 340 || cost.TotalNano != 340 {
		t.Fatalf("charged %d (total %d), the vendor stated 340", cost.MarginalNano, cost.TotalNano)
	}
	if !cost.ReportedPriced || cost.ReportedFallback {
		t.Errorf("ReportedPriced=%v ReportedFallback=%v, want true/false", cost.ReportedPriced, cost.ReportedFallback)
	}
	// One line, the vendor's: the token rates the rule also carries are its
	// fallback and must not be added on top.
	if len(cost.Components) != 1 || cost.Components[0].Name != "reported_cost" {
		t.Fatalf("components = %+v, want the one reported_cost line", cost.Components)
	}
}

func TestAnUnstatedCostFallsBackToTheRulesRates(t *testing.T) {
	c := mustCatalog(t, reportedRateCard)
	cost := mustPrice(t, c, Request{
		Provider: "deepinfra", Model: "llama", InputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	// $0.02 + $0.03 at the rule's own per-million rates.
	if cost.MarginalNano != 50_000_000 {
		t.Fatalf("charged %d, want the fallback 50_000_000", cost.MarginalNano)
	}
	if cost.ReportedPriced || !cost.ReportedFallback {
		t.Errorf("ReportedPriced=%v ReportedFallback=%v, want false/true", cost.ReportedPriced, cost.ReportedFallback)
	}
	for _, comp := range cost.Components {
		if comp.Name == "reported_cost" {
			t.Errorf("a figure nobody stated was billed: %+v", comp)
		}
	}
}

// TestAReportedOnlyRuleWithNoFigureIsUnpricedNotFree: a vendor that stated nothing
// under a rule with no fallback is a request this catalog cannot price, which is a
// different ledger fact from one the vendor said was free.
func TestAReportedOnlyRuleWithNoFigureIsUnpricedNotFree(t *testing.T) {
	c := mustCatalog(t, reportedRateCard)
	cost := mustPrice(t, c, Request{Provider: "openrouter", Model: "m", InputTokens: 10})
	if cost.NoPrice != NoPriceReportedNotStated {
		t.Fatalf("NoPrice = %v, want NoPriceReportedNotStated", cost.NoPrice)
	}
	if cost.TotalNano != 0 {
		t.Errorf("an unpriced request charged %d", cost.TotalNano)
	}
	if !strings.Contains(cost.NoPrice.Why(), "reported_cost") {
		t.Errorf("Why() does not name the rate: %q", cost.NoPrice.Why())
	}

	free := mustPrice(t, c, Request{Provider: "openrouter", Model: "m:free",
		ReportedCostNano: 0, ReportedCostStated: true})
	if free.NoPrice != NoPriceNone || free.TotalNano != 0 || !free.ReportedPriced {
		t.Errorf("a stated zero is a price of zero: NoPrice=%v total=%d priced=%v",
			free.NoPrice, free.TotalNano, free.ReportedPriced)
	}
}

func TestTheMultiplierIsExact(t *testing.T) {
	c := mustCatalog(t, reportedRateCard)
	cost := mustPrice(t, c, Request{Provider: "resold", Model: "m",
		ReportedCostNano: 880, ReportedCostStated: true})
	if cost.MarginalNano != 968 {
		t.Fatalf("1.1 x 880 = %d, want 968", cost.MarginalNano)
	}
}

// TestAStatedCostNeedsNoOtherQuantity: the fallback's axis is not measured when the
// figure is stated, so a rule that falls back to per_audio_second must not refuse a
// request that carries no recording length but a stated cost.
func TestAStatedCostNeedsNoOtherQuantity(t *testing.T) {
	c := mustCatalog(t, reportedRateCard)
	stated := mustPrice(t, c, Request{Provider: "deepinfra", Model: "whisper",
		ReportedCostNano: 13_347, ReportedCostStated: true, Billed: BilledTokens})
	if stated.NoPrice != NoPriceNone || stated.MarginalNano != 13_347 {
		t.Fatalf("NoPrice=%v charged %d, want the stated 13347", stated.NoPrice, stated.MarginalNano)
	}
	fallback := mustPrice(t, c, Request{Provider: "deepinfra", Model: "whisper"})
	if fallback.NoPrice != NoPriceAudioNotMeasured {
		t.Fatalf("NoPrice = %v, want the fallback's own refusal", fallback.NoPrice)
	}
	measured := mustPrice(t, c, Request{Provider: "deepinfra", Model: "whisper", AudioSeconds: 4.008})
	if measured.MarginalNano != 13_347 {
		t.Errorf("4.008 s x $0.00000333 = %d nano, want 13347", measured.MarginalNano)
	}
}

func TestReportedCostRulesThatCannotLoad(t *testing.T) {
	for name, card := range map[string]string{
		"on a notional rule": `
currency: USD
rules:
  - id: n
    class: notional_rate
    match: { provider: p }
    source: list
    as_of: "2026-10-01"
    reported_cost: "1"
`,
		"a negative multiplier": `
currency: USD
rules:
  - id: m
    class: marginal_usage
    match: { provider: p }
    reported_cost: "-1"
`,
		"ten fractional digits": `
currency: USD
rules:
  - id: m
    class: marginal_usage
    match: { provider: p }
    reported_cost: "1.0000000001"
`,
	} {
		if _, err := ParseCatalog([]byte(card)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// TestUtilizationDoesNotScaleAStatedCost: a vendor's stated figure is its bill, and
// the operator's occupancy factor applies to the rule's own fallback rates only.
func TestUtilizationDoesNotScaleAStatedCost(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: u
    class: marginal_usage
    match: { provider: p }
    reported_cost: "1"
    input: "1"
    utilization:
      slope: "1.0"
      max_multiplier: "2.0"
`)
	stated := mustPrice(t, c, Request{Provider: "p", Model: "m", InputTokens: 1_000_000,
		ReportedCostNano: 1_000_000_000, ReportedCostStated: true,
		UtilizationPPM: 1_000_000, UtilizationObserved: true})
	if stated.MarginalNano != 1_000_000_000 || stated.UtilizationPriced {
		t.Errorf("a stated $1 was charged %d nano (utilization priced: %v)", stated.MarginalNano, stated.UtilizationPriced)
	}
	// The fallback still moves with the machine: at full occupancy, slope 1.0
	// doubles the $1 input rate.
	fallback := mustPrice(t, c, Request{Provider: "p", Model: "m", InputTokens: 1_000_000,
		UtilizationPPM: 1_000_000, UtilizationObserved: true})
	if fallback.MarginalNano != 2_000_000_000 || !fallback.UtilizationPriced {
		t.Errorf("the fallback was charged %d nano (utilization priced: %v), want 2e9",
			fallback.MarginalNano, fallback.UtilizationPriced)
	}
}
