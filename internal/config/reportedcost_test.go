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

func TestNotificationDriversAndRemainingLow(t *testing.T) {
	ok := "\nnotifications:\n  email:\n    drivers: [smtp, http]\n    from: dorang@example.com\n" +
		"    to: [ops@example.com]\n    smtp: {addr: \"mail.example.com:25\"}\n" +
		"    http: {url: \"https://hooks.example.com/x\", key_env: DORANG_TEST_FIXTURE_KEY, format: slack}\n" +
		"  remaining_low: {percent: 5, balance_floor_usd: \"2\", interval: 1m, remind: 6h}\n"
	c, err := loadYAML(t, ok)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Notifications.Email.Driver != "" {
		t.Errorf("driver defaulted to %q beside drivers", c.Notifications.Email.Driver)
	}
	const head = "\nnotifications:\n  email:\n"
	mustRefuse(t, loadWith(t, head+"    driver: smtp\n    drivers: [http]\n"),
		"notifications.email.driver", "exclusive")
	mustRefuse(t, loadWith(t, head+"    drivers: [http, http]\n    http: {url: \"https://h.example.com\", key_env: X}\n"),
		"notifications.email.drivers[1]", "twice")
	mustRefuse(t, loadWith(t, head+"    drivers: [http]\n"), "notifications.email.http.url", "must be set")
	mustRefuse(t, loadWith(t, head+"    drivers: [http]\n    http: {url: \"https://h.example.com\", key_env: X, format: teams}\n"),
		"notifications.email.http.format", "")
	mustRefuse(t, loadWith(t, "\nnotifications:\n  remaining_low: {percent: 100}\n"),
		"notifications.remaining_low.percent", "between 0 and 100")
}
