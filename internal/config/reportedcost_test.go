package config

import "testing"

// TestReportedCostLoads: the one component that belongs to no unit may sit beside
// a rule's own rates, which are its fallback.
func TestReportedCostLoads(t *testing.T) {
	for _, rates := range []string{
		`{reported_cost: "1"}`,
		`{reported_cost: "1.1", input: "0.02", output: "0.03"}`,
		`{reported_cost: "1", audio_seconds: "0.00000333"}`,
	} {
		if _, err := loadYAML(t, "\npricing:\n  currency: USD\n  rules:\n    - id: r1\n"+
			"      class: marginal_usage\n      rates: "+rates+"\n"); err != nil {
			t.Errorf("rates %s: %v", rates, err)
		}
	}
	if u, ok := PricingUnit([]string{"reported_cost", "audio_seconds"}); !ok || u != "per_audio_second" {
		t.Errorf("PricingUnit = %q, %v; reported_cost must not decide the unit", u, ok)
	}
}

func TestReportedCostRefusals(t *testing.T) {
	const at = "pricing.rules[0].rates[\"reported_cost\"]"
	mustRefuse(t, loadWith(t, "\npricing:\n  currency: USD\n  rules:\n    - id: r1\n"+
		"      class: notional_rate\n      source: list\n      as_of: \"2026-10-01\"\n"+
		"      rates: {reported_cost: \"1\"}\n"),
		at, "notional_rate")
	mustRefuse(t, loadWith(t, "\npricing:\n  currency: EUR\n  rules:\n    - id: r1\n"+
		"      class: marginal_usage\n      rates: {reported_cost: \"1\"}\n"),
		at, "does not convert currencies")
	mustRefuse(t, loadWith(t, "\npricing:\n  currency: USD\n  rules:\n    - id: r1\n"+
		"      class: marginal_usage\n      rates: {reported_cost: \"1.0000000001\"}\n"),
		at, "9 fractional digits")
}
