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
// row) but it advances the subscription period accumulator, which is the
// denominator every later request's amortized share is divided by. Nine
// requests' worth of imaginary usage per real request makes every subsequent
// share wrong.
//
// The accumulator is not exported, so it is measured where it is spent: the
// amortized share of a probe settle is plan_cost * marginal / (accumulator +
// marginal), which inverts to the accumulator exactly.
func TestThePreflightEstimateDoesNotSettle(t *testing.T) {
	const (
		nano     = int64(1_000_000_000)
		planNano = 100 * nano // amount_per_period: 100.00
		reqNano  = 1 * nano   // 1000 output tokens at 0.001/token
		requests = 9
	)
	cat := mustPriceCatalog(t, `
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

	now := time.Unix(1_700_000_000, 0).UTC()
	g := &budgetGate{now: func() time.Time { return now }}
	st := &dispatchState{pricing: cat}
	dec := &router.Decision{Provider: "p", UpstreamModel: "m", Credential: "cred-1"}
	maxTokens := 9000
	c := &call{creq: &canonical.Request{MaxTokens: &maxTokens}}

	for i := 0; i < requests; i++ {
		// The gate quotes the pessimistic upper bound...
		if est := g.estimate(st, c, dec); est <= 0 {
			t.Fatalf("estimate %d = %d, want a positive upper bound", i, est)
		}
		// ...and then the request settles at what it actually generated.
		if _, err := cat.Settle(pricing.Request{Provider: "p", Model: "m",
			Credential: "cred-1", OutputTokens: 1000, Requests: 1, At: now}); err != nil {
			t.Fatalf("Settle %d: %v", i, err)
		}
	}

	probe, err := cat.Settle(pricing.Request{Provider: "p", Model: "m",
		Credential: "cred-1", OutputTokens: 1000, Requests: 1, At: now})
	if err != nil {
		t.Fatalf("probe Settle: %v", err)
	}
	share := probe.SubscriptionNano
	if share <= 0 {
		t.Fatalf("probe subscription share = %d, want > 0", share)
	}
	// share = plan * req / (accumulator + req), so an accumulator of exactly N
	// requests' marginal cost — one per request, not one per request plus one
	// nine-times-larger quote — makes the probe's share plan/(N+1).
	if want := planNano / (requests + 1); share != want {
		// Inverting the same identity says what the accumulator actually holds.
		acc := float64(reqNano) * float64(planNano-share) / float64(share)
		t.Fatalf("probe subscription share = %d, want %d: the accumulator holds "+
			"%.0f nano-USD after %d requests of %d (%.1fx), so the pre-flight "+
			"estimate is settling as well as quoting",
			share, want, acc, requests, reqNano,
			acc/float64(int64(requests)*reqNano))
	}
}
