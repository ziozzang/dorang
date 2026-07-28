package pricing

import (
	"strings"
	"testing"
	"time"
)

// A credit that is smaller than the request it credits is an ordinary discount
// and must survive untouched: the floor exists to stop a payout, not to stop
// credits.
func TestACreditWithinTheCostIsNotFloored(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: usage
    match: { model: m }
    unit: per_1m_tokens
    input: "1.0"
  - id: credit
    class: adjustment
    match: { model: m }
    op: add
    amount: "-0.000001"
`)
	// 1000 input tokens at 1.00/1M = 0.001 USD, less a 0.000001 USD credit.
	cost := mustPrice(t, c, Request{Provider: "p", Model: "m", InputTokens: 1000})
	if cost.Floored {
		t.Fatal("a credit smaller than the cost was floored")
	}
	if want := int64(999_000); cost.TotalNano != want {
		t.Fatalf("total = %d, want %d", cost.TotalNano, want)
	}
	if cost.AdjustmentNano != -1000 {
		t.Fatalf("adjustment = %d, want -1000", cost.AdjustmentNano)
	}
}

// TestACreditLargerThanTheRequestCannotPayTheCaller is the floor itself. The
// clamp lands in compute, which Price, Settle and Explain all pass through, so
// no caller can reach an un-floored total and no new caller can forget to.
func TestACreditLargerThanTheRequestCannotPayTheCaller(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: usage
    match: { model: m }
    unit: per_1m_tokens
    input: "1.0"
  - id: rebate
    class: adjustment
    match: { model: m }
    op: add
    amount: "-1"
`)
	req := Request{Provider: "p", Model: "m", InputTokens: 10}

	for _, tc := range []struct {
		name string
		cost Cost
	}{
		{"Price", mustPrice(t, c, req)},
		{"Settle", func() Cost {
			got, err := c.Settle(req)
			if err != nil {
				t.Fatalf("Settle: %v", err)
			}
			return got
		}()},
		{"Explain", c.Explain(req).Cost},
	} {
		if tc.cost.TotalNano < 0 {
			t.Errorf("%s: total = %d nano, want >= 0", tc.name, tc.cost.TotalNano)
		}
		if !tc.cost.Floored {
			t.Errorf("%s: Floored is false, so a clamped total is indistinguishable "+
				"from a free request", tc.name)
		}
		// The reported classes must still sum to the reported total, or
		// reconciliation against the component breakdown drifts.
		sum := tc.cost.MarginalNano + tc.cost.SubscriptionNano + tc.cost.AdjustmentNano
		if sum != tc.cost.TotalNano {
			t.Errorf("%s: %d + %d + %d = %d, want TotalNano = %d",
				tc.name, tc.cost.MarginalNano, tc.cost.SubscriptionNano,
				tc.cost.AdjustmentNano, sum, tc.cost.TotalNano)
		}
	}

	// The clamp is visible in the explanation rather than silent.
	ex := c.Explain(req)
	found := false
	for _, n := range ex.Notes {
		if strings.Contains(n, "clamped to zero") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Explain does not report the clamp: notes = %v", ex.Notes)
	}
}

// A percent discount deeper than the whole is rejected where it is written,
// which is the one bound on a negative adjustment that can be checked without
// knowing the request.
func TestADiscountDeeperThanTheWholeIsRejected(t *testing.T) {
	for _, amount := range []string{"-100.0001", "-150", "-1000"} {
		_, err := ParseCatalog([]byte(`
currency: USD
rules:
  - id: over
    class: adjustment
    match: { model: m }
    op: percent
    amount: "` + amount + `"
`))
		if err == nil {
			t.Fatalf("amount %q was accepted: a discount over 100%% is a payout", amount)
		}
	}
	// Exactly -100% is a full discount, which is legitimate and stays legal.
	c := mustCatalog(t, `
currency: USD
rules:
  - id: usage
    match: { model: m }
    unit: per_1m_tokens
    input: "1.0"
  - id: full
    class: adjustment
    match: { model: m }
    op: percent
    amount: "-100"
`)
	cost := mustPrice(t, c, Request{Provider: "p", Model: "m", InputTokens: 1000})
	if cost.TotalNano != 0 {
		t.Fatalf("total = %d, want 0 for a 100%% discount", cost.TotalNano)
	}
	if cost.Floored {
		t.Fatal("a 100% discount reaches zero exactly; it must not be reported as clamped")
	}
}

// TestABackfilledSettlementDoesNotResetThePeriodAccumulator.
//
// The subscription accumulator is keyed by the period the settled instant falls
// in, and it was replaced outright on every Settle. One row with a timestamp in
// last month — a backfill, a replay, a clock that stepped back — therefore moved
// the recorded period backwards, and every subsequent row in the open period
// found a period that did not match its own, read a to-date of zero, and took
// the entire plan cost as its share.
func TestABackfilledSettlementDoesNotResetThePeriodAccumulator(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: usage
    match: { model: m }
    unit: per_1m_tokens
    output: "1000.0"
  - id: plan
    class: fixed_subscription
    match: { model: m }
    amount_per_period: "100.00"
    period: monthly
`)
	const (
		nano     = int64(1_000_000_000)
		planNano = 100 * nano
	)
	now := at(t, "2026-07-15T12:00:00Z")
	req := func(when time.Time) Request {
		return Request{Provider: "p", Model: "m", Credential: "c1",
			OutputTokens: 1000, Requests: 1, At: when}
	}

	for i := 0; i < 4; i++ {
		if _, err := c.Settle(req(now)); err != nil {
			t.Fatalf("Settle %d: %v", i, err)
		}
	}
	// A row from the closed period arrives late.
	if _, err := c.Settle(req(at(t, "2026-06-30T23:59:00Z"))); err != nil {
		t.Fatalf("backfilled Settle: %v", err)
	}

	got, err := c.Settle(req(now))
	if err != nil {
		t.Fatalf("Settle after backfill: %v", err)
	}
	// Four requests of 1 USD are already in this period, so the fifth takes a
	// fifth of the plan. A reset accumulator would hand it the whole 100.
	if want := planNano / 5; got.SubscriptionNano != want {
		t.Fatalf("share after a backfilled row = %d, want %d: the late row reset the "+
			"open period's accumulator", got.SubscriptionNano, want)
	}
}
