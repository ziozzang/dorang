package app

import (
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
)

func mustPriceCatalog(t *testing.T, y string) *pricing.Catalog {
	t.Helper()
	c, err := pricing.ParseCatalog([]byte(y))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	return c
}

// TestANegativeAdjustmentNeitherCreditsTheLedgerNorManufacturesQuota.
//
// `op: add` with a negative amount passes validation, and it should: a credit is
// a real thing. What must not follow is a request that costs less than nothing.
// The number this produces is not merely a wrong invoice line — it is written to
// the ledger row and then handed to the credential's quota meter, where a
// negative cost does not under-count, it GIVES BACK window capacity that no
// reset granted.
//
// The assertions are therefore on the ledger row and on the meter, not on the
// arithmetic: a test that checked only that a price is non-negative would pass
// against a guard installed at one of the three call sites, which is exactly the
// state this defect was found in.
func TestANegativeAdjustmentNeitherCreditsTheLedgerNorManufacturesQuota(t *testing.T) {
	cat := mustPriceCatalog(t, `
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

	now := time.Unix(1_700_000_000, 0).UTC()
	rule := quota.Rule{Window: quota.Rolling(time.Hour), Metric: quota.MetricCostUSD,
		Limit: quota.NanoUSD(5)}
	meter, err := quota.NewMeter(quota.MeterConfig{Rules: []quota.Rule{rule},
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	// Spend two real dollars first. Manufactured quota is then observable as the
	// counter running BACKWARDS, which is the thing that matters, rather than as
	// a negative number, which is only how it looks on the first request.
	meter.Record(now, quota.Usage{CostNanoUSD: quota.NanoUSD(2), Requests: 1})
	before := meter.Used(now, rule)

	d := &dispatcher{logf: func(string, ...any) {}, now: func() time.Time { return now }}
	st := &dispatchState{
		pricing: cat,
		quota:   &quotaSet{meters: router.Meters{"cred-1": meter}},
	}
	dec := &router.Decision{Provider: "p", UpstreamModel: "m", Credential: "cred-1",
		Deployment: "dep-1"}

	for i := 0; i < 3; i++ {
		rq := &server.Request{}
		d.settle(st, &call{}, dec, rq, result{usage: canonical.Usage{InputTokens: 10}})

		// Errorf, not Fatalf: the quota assertion below is the other half of this
		// defect and it has to be reached even when the ledger row is already
		// wrong. Guarding one of the two is how this survived.
		if rq.Result.CostNanoUSD < 0 {
			t.Errorf("ledger row %d = %d nano-USD: a request was billed a negative amount",
				i, rq.Result.CostNanoUSD)
		}
	}

	if after := meter.Used(now, rule); after < before {
		t.Fatalf("quota used went from %d to %d nano-USD: a negative cost manufactured "+
			"%d nano-USD of allowance nobody paid for", before, after, before-after)
	}
	if got := meter.Cumulative(quota.MetricCostUSD); got < 0 {
		t.Fatalf("cumulative cost = %d, want >= 0", got)
	}
	if d := meter.Check(now); !d.Allow {
		t.Fatalf("meter refused after three near-free requests: %+v", d)
	}
}

// TestThePreflightEstimateDoesNotSettle.
//
// [pricing.Catalog.Settle] is documented "at most once per request, from the
// accounting path; routing must call Price", and the pre-flight budget estimate
// called it — priced at max_tokens, which is nine times the generation this test
// produces. Settling is not a double charge (the second settle wins the ledger
// row) but it is state: it consumes the carried sub-nano remainder, and it
// advances the subscription period's attributed total. A plan share taken by a
// pre-flight quote is attributed to a row that is never written, and the real row
// that follows it then finds the period already up to date and records nothing.
// The period's attribution does not overshoot — it silently goes missing.
//
// The accumulator is not exported, so it is measured where it is spent: an
// identical catalog settled once at the last instant says what the period had
// accrued, and the rows of the catalog under test must add up to exactly that.
func TestThePreflightEstimateDoesNotSettle(t *testing.T) {
	const (
		catalog = `
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
`
		requests = 9
	)
	cat := mustPriceCatalog(t, catalog)

	start := time.Unix(1_700_000_000, 0).UTC()
	now := start
	g := &budgetGate{now: func() time.Time { return now }}
	st := &dispatchState{pricing: cat}
	dec := &router.Decision{Provider: "p", UpstreamModel: "m", Credential: "cred-1"}
	maxTokens := 9000
	c := &call{creq: &canonical.Request{MaxTokens: &maxTokens}}
	settled := func(cat *pricing.Catalog, at time.Time) pricing.Cost {
		t.Helper()
		cost, err := cat.Settle(pricing.Request{Provider: "p", Model: "m",
			Credential: "cred-1", OutputTokens: 1000, Requests: 1, At: at})
		if err != nil {
			t.Fatalf("Settle: %v", err)
		}
		return cost
	}

	var attributed int64
	for i := 0; i < requests; i++ {
		now = start.Add(time.Duration(i) * time.Minute)
		// The gate quotes the pessimistic upper bound...
		if est := g.estimate(st, c, dec); est <= 0 {
			t.Fatalf("estimate %d = %d, want a positive upper bound", i, est)
		}
		// ...and then the request settles at what it actually generated.
		attributed += settled(cat, now).SubscriptionNano
	}

	// The same traffic against a catalog no estimate has touched.
	want := settled(mustPriceCatalog(t, catalog), now).SubscriptionNano
	if want <= 0 {
		t.Fatalf("the test is not exercising the hazard: the period accrued %d", want)
	}
	if attributed != want {
		t.Fatalf("the settled rows attribute %d nano-USD of the %d this period accrued "+
			"(%.0f%%): the pre-flight estimate is settling as well as quoting, so the "+
			"plan share it took belongs to a row that was never written",
			attributed, want, 100*float64(attributed)/float64(want))
	}
}
