package pricing

import (
	"fmt"
	"testing"
	"time"
)

// hundredDollarPlan is the shape the adversarial review reproduced the defect on: a flat
// plan beside a per-token rule, so every request has a marginal cost to be apportioned by.
const hundredDollarPlan = `
currency: USD
rules:
  - { id: tokens, match: { model: m }, unit: per_1m_tokens, input: "1.00" }
  - id: plan
    class: fixed_subscription
    match: { credential: c1 }
    amount_per_period: "100.00"
    period: monthly
`

const planNano = int64(100_000_000_000) // 100.00 USD

// TestAPeriodAttributesExactlyThePlanCost is the specification of §8.1, asserted as a
// property rather than as a formula.
//
// Whatever a period's traffic looks like, the subscription shares its requests record must
// SUM to the plan cost — not to the plan cost multiplied by something that grows with
// traffic. The formula this replaces gave request i a share of plan_cost x
// (request_marginal / marginal_to_date), which for equal requests is plan_cost/i, and the
// ledger added those up: sum_i plan_cost/i = plan_cost x H_N, the Nth harmonic number. The
// two named rows below are the cases the review reproduced — 519 USD attributed to a 100
// USD plan over 100 requests, 749 USD over 1000 — so the growth cannot come back unseen.
//
// The requests are spread across the period because that is what a period of traffic is;
// the plan cost accrues with the period, and a request records what has accrued since the
// previous one. Evenly spread traffic therefore takes plan_cost/N each, which is the
// per-request figure §8.1 now promises.
func TestAPeriodAttributesExactlyThePlanCost(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
	}{
		{"one request carries the whole plan cost", 1},
		{"two", 2},
		{"three, which is not a whole number of nano", 3},
		{"seven", 7},
		{"the review's case: 100 requests reported 519.00 USD of a 100.00 USD plan", 100},
		{"1000 requests reported 749.00 USD, and it kept growing", 1000},
		{"ten thousand", 10_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mustCatalog(t, hundredDollarPlan)
			start, end := periodBounds(PeriodMonthly, at(t, "2026-07-15T12:00:00Z"), time.UTC)
			span := int64(end.Sub(start))

			var total, firstBreach int64
			var breachAt int
			shares := make([]int64, 0, tc.n)
			for i := 1; i <= tc.n; i++ {
				when := start.Add(time.Duration(span / int64(tc.n) * int64(i)))
				if i == tc.n {
					when = end.Add(-time.Nanosecond) // the period is half-open
				}
				cost, err := c.Settle(Request{Model: "m", Credential: "c1",
					InputTokens: 1_000_000, Requests: 1, At: when})
				if err != nil {
					t.Fatal(err)
				}
				if cost.SubscriptionNano < 0 {
					t.Fatalf("request %d recorded a negative share (%d): a share is an "+
						"increment of the period's attributed total, and the total never falls",
						i, cost.SubscriptionNano)
				}
				total += cost.SubscriptionNano
				if total > planNano && breachAt == 0 {
					breachAt, firstBreach = i, total
				}
				shares = append(shares, cost.SubscriptionNano)
			}

			if total != planNano {
				msg := fmt.Sprintf("%d requests attributed %s of a %s plan (%.2fx the plan cost)",
					tc.n, usd(total), usd(planNano), float64(total)/float64(planNano))
				if breachAt > 0 {
					msg += fmt.Sprintf("; it passed the plan cost at request %d (%s)",
						breachAt, usd(firstBreach))
				}
				t.Fatalf("%s: a period's shares must be increments of one attributed total "+
					"that converges to the plan cost, not repeated estimates of the whole "+
					"that accumulate — summing plan_cost/i over N requests gives "+
					"plan_cost x H_N, which is what this number looks like", msg)
			}
			if breachAt > 0 {
				t.Fatalf("the total reached %s of a %s plan at request %d before ending at "+
					"the plan cost: it must never exceed the plan cost at any point in the period",
					usd(firstBreach), usd(planNano), breachAt)
			}
			// Evenly spread traffic: each request's share is the plan cost over N, to
			// within the nano the exact division cannot express.
			want := planNano / int64(tc.n)
			for i, got := range shares {
				if d := got - want; d > 1 || d < -1 {
					t.Fatalf("request %d of %d recorded %s, want about %s (plan_cost/N)",
						i+1, tc.n, usd(got), usd(want))
				}
			}
		})
	}
}

// TestThePlanCostIsCappedOnEveryEntryPoint puts the bound where it cannot be bypassed.
//
// Price, Settle and Explain all reach the subscription figure through one function, and
// the cap lives inside it rather than at the three call sites — the lesson of the negative
// price, where flooring inside Price/Settle/Explain's shared path caught a third caller
// nobody had named. Once a period has attributed the whole plan cost, every entry point
// reports zero more, for as long as the period lasts.
func TestThePlanCostIsCappedOnEveryEntryPoint(t *testing.T) {
	c := mustCatalog(t, hundredDollarPlan)
	_, end := periodBounds(PeriodMonthly, at(t, "2026-07-15T12:00:00Z"), time.UTC)
	last := end.Add(-time.Nanosecond)
	req := func(when time.Time) Request {
		return Request{Model: "m", Credential: "c1", InputTokens: 1_000_000, Requests: 1, At: when}
	}

	first, err := c.Settle(req(last))
	if err != nil {
		t.Fatal(err)
	}
	if first.SubscriptionNano != planNano {
		t.Fatalf("a single request over a whole period = %s, want the plan cost %s",
			usd(first.SubscriptionNano), usd(planNano))
	}
	for _, e := range []struct {
		name string
		got  func() int64
	}{
		{"Settle", func() int64 {
			cost, err := c.Settle(req(last))
			if err != nil {
				t.Fatal(err)
			}
			return cost.SubscriptionNano
		}},
		{"Price", func() int64 { return mustPrice(t, c, req(last)).SubscriptionNano }},
		{"Explain", func() int64 { return c.Explain(req(last)).Cost.SubscriptionNano }},
	} {
		if got := e.got(); got != 0 {
			t.Fatalf("%s attributed a further %s after the period had already attributed "+
				"its whole plan cost: the cap belongs where all three pass through, not at "+
				"the call sites", e.name, usd(got))
		}
	}
}

// TestAnUnusedPeriodAttributesNothing.
//
// A plan nobody used still costs the operator the plan cost, and attributing it to no
// request is the honest answer: the ledger reports what traffic cost, and there was none.
// The figure that says the plan went unused is the gap between the plan cost and what the
// period attributed, which this rule is what makes visible — an idle month reads as zero
// attributed rather than as a month of traffic that was never sent.
//
// It also has to survive the period roll: a quiet June must not be attributed to July's
// first request.
func TestAnUnusedPeriodAttributesNothing(t *testing.T) {
	c := mustCatalog(t, hundredDollarPlan)
	july := at(t, "2026-07-01T12:00:00Z") // twelve hours into a 31 day month

	cost, err := c.Settle(Request{Model: "m", Credential: "c1",
		InputTokens: 1_000_000, Requests: 1, At: july})
	if err != nil {
		t.Fatal(err)
	}
	// 100.00 x 12h / 31d, and not one nano of the June nobody used.
	const want = int64(1_612_903_226)
	if cost.SubscriptionNano != want {
		t.Fatalf("the first request of a period recorded %s, want %s: an idle period is "+
			"attributed to nobody, not carried into the next period's first request",
			usd(cost.SubscriptionNano), usd(want))
	}
}

// TestAnOutOfOrderRowInsideTheOpenPeriodAttributesNothing.
//
// Rows do not always settle in timestamp order. One that arrives with an earlier instant
// than the period's attributed total already covers has nothing left to accrue, and the
// answer is zero rather than a negative share: a settled row is never restated, and a
// negative share would give back budget and quota exactly as a negative total would.
func TestAnOutOfOrderRowInsideTheOpenPeriodAttributesNothing(t *testing.T) {
	c := mustCatalog(t, hundredDollarPlan)
	req := func(s string) Request {
		return Request{Model: "m", Credential: "c1", InputTokens: 1_000_000, Requests: 1, At: at(t, s)}
	}
	if _, err := c.Settle(req("2026-07-20T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	cost, err := c.Settle(req("2026-07-10T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if cost.SubscriptionNano != 0 {
		t.Fatalf("a row from earlier in the open period recorded %s, want 0",
			usd(cost.SubscriptionNano))
	}
	if cost.MarginalNano != 1_000_000_000 {
		t.Fatalf("marginal = %d: withholding the plan share must not change what the "+
			"request itself cost", cost.MarginalNano)
	}
}

// TestAnAdjustmentOnTheSubscriptionBaseConvergesWithIt.
//
// The classes compose rather than compete, so the property has to survive an adjustment
// aimed at the subscription base. A percentage scales each increment, and increments that
// sum to the plan cost scale to that percentage of the plan cost — where the same discount
// over the formula this replaced scaled plan_cost x H_N and was just as unbounded.
func TestAnAdjustmentOnTheSubscriptionBaseConvergesWithIt(t *testing.T) {
	c := mustCatalog(t, hundredDollarPlan+`
  - { id: plan-discount, class: adjustment, match: {}, op: percent, amount: "-25", applies_to: subscription }
`)
	start, end := periodBounds(PeriodMonthly, at(t, "2026-07-15T12:00:00Z"), time.UTC)
	span := int64(end.Sub(start))
	const n = 50

	var subscription, adjustment int64
	for i := 1; i <= n; i++ {
		when := start.Add(time.Duration(span / n * int64(i)))
		if i == n {
			when = end.Add(-time.Nanosecond)
		}
		cost, err := c.Settle(Request{Model: "m", Credential: "c1",
			InputTokens: 1_000_000, Requests: 1, At: when})
		if err != nil {
			t.Fatal(err)
		}
		subscription += cost.SubscriptionNano
		adjustment += cost.AdjustmentNano
	}
	if subscription != planNano {
		t.Fatalf("the period attributed %s of a %s plan under a discount",
			usd(subscription), usd(planNano))
	}
	if want := -planNano / 4; adjustment != want {
		t.Fatalf("a 25%% discount on the plan came to %s over the period, want %s: an "+
			"adjustment on the subscription base scales a bounded total, not a growing one",
			usd(adjustment), usd(want))
	}
}

// usd renders nano-USD for a failure message. Money in a test failure is unreadable in
// nano and the whole point of these messages is that the overstatement is legible.
func usd(nano int64) string {
	return fmt.Sprintf("%d.%02d USD", nano/1_000_000_000, (nano%1_000_000_000)/10_000_000)
}
