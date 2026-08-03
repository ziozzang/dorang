package app

import (
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
)

// Three counters read one finished request: the plan accumulator inside
// internal/pricing, the quota counter, and the ledger row. `settle` is where the
// last two are written, and it used to read the Cost twice — the ledger row
// inside the switch, the quota counter from a line outside it — so two arms of
// that switch recorded nothing in the ledger while the line below charged quota
// anyway.
//
// These tests assert the AGREEMENT rather than either number: whatever a request
// costs, the figure the ledger row carries and the figure the quota counter takes
// are the same figure.

// settleHarness runs one finished request through `settle` and reports what each
// of the two counters ended up holding.
type settleHarness struct {
	rq    *server.Request
	meter *quota.Meter
}

func newSettleHarness(t *testing.T, catalog string, now time.Time) *settleHarness {
	t.Helper()
	m, err := quota.NewMeter(quota.MeterConfig{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	return &settleHarness{rq: &server.Request{}, meter: m}
}

func (h *settleHarness) run(t *testing.T, catalog string, now time.Time, u canonical.Usage) {
	t.Helper()
	st := &dispatchState{
		pricing: mustPriceCatalog(t, catalog),
		quota:   &quotaSet{meters: router.Meters{"cred-1": h.meter}},
	}
	d := &dispatcher{logf: func(string, ...any) {}, now: func() time.Time { return now }}
	dec := &router.Decision{Provider: "p", UpstreamModel: "m", Credential: "cred-1"}
	d.settle(st, &call{}, dec, h.rq, result{usage: u, total: 8 * time.Second})
}

// ledger is the figure the ledger row carries.
func (h *settleHarness) ledger() int64 { return h.rq.Result.CostNanoUSD }

// charged is the figure the quota counter took.
func (h *settleHarness) charged() int64 { return h.meter.Cumulative(quota.MetricCostUSD) }

// TestAnUnpriceableRequestChargesNeitherCounter is the defect at the point where
// two of the three counters are written.
//
// The rule matches and cannot be applied — it prices seconds of recording and the
// request carries none — so the ledger records nothing. The quota counter used to
// take cost.TotalNano regardless, and cost.TotalNano carried the plan share the
// subscription class had just accrued: 32.26 USD of a 100.00 USD plan, ten days
// into the period, charged to a customer's quota against a ledger row of 0.00.
func TestAnUnpriceableRequestChargesNeitherCounter(t *testing.T) {
	const catalog = `
currency: USD
rules:
  - id: plan
    class: fixed_subscription
    match: { credential: cred-1 }
    amount_per_period: "100.00"
    period: monthly
  - id: transcription
    class: marginal_usage
    match: { credential: cred-1 }
    unit: per_audio_second
    audio_seconds: "0.0001"
`
	// Ten days into a 31 day July.
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	h := newSettleHarness(t, catalog, now)
	// A request with no recording: the rule matched and cannot price it.
	h.run(t, catalog, now, canonical.Usage{InputTokens: 100, OutputTokens: 10})

	if h.ledger() != 0 {
		t.Fatalf("the ledger row carries %d nano on an unpriceable request, want 0", h.ledger())
	}
	if h.charged() != h.ledger() {
		t.Fatalf("quota took %d nano and the ledger row carries %d. They are the same "+
			"request and must be the same number: quota charged for a row that records "+
			"no cost is money the customer cannot see, cannot dispute and cannot "+
			"reconcile against anything", h.charged(), h.ledger())
	}
	if h.rq.Result.Priced {
		t.Error("Priced marks the cost fields as meaningful; nothing was priced")
	}
}

// TestAFlatPlanRecordsItsShareOnBothCounters is the other half, and the reason
// the fix is not "charge nothing whenever the marginal class is silent".
//
// A flat plan has no marginal_usage rule at all (DESIGN §8.1), so every one of its
// requests reports Missing — and the plan share pricing attributes is the whole of
// what the row costs. Recording nothing would leave `subscription_spend` without a
// producer for exactly the plans it exists for, while the period's accumulator
// advanced on every request: the plan cost apportioned across rows that all report
// zero, which is the same money going missing by the opposite route.
func TestAFlatPlanRecordsItsShareOnBothCounters(t *testing.T) {
	const catalog = `
currency: USD
rules:
  - id: plan
    class: fixed_subscription
    match: { credential: cred-1 }
    amount_per_period: "100.00"
    period: monthly
`
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	h := newSettleHarness(t, catalog, now)
	h.run(t, catalog, now, canonical.Usage{InputTokens: 100, OutputTokens: 10})

	// 100.00 x 10/31 days.
	const want = 32_258_064_516
	if h.ledger() != want {
		t.Fatalf("the ledger row carries %d nano, want %d: a flat plan's share is the "+
			"whole of this row's cost and it has nowhere else to be recorded",
			h.ledger(), want)
	}
	if h.rq.Result.SubscriptionNanoUSD != want {
		t.Errorf("subscription_spend = %d, want %d: §8.1's two fields are separate and "+
			"the plan share belongs in this one", h.rq.Result.SubscriptionNanoUSD, want)
	}
	if h.rq.Result.MarginalNanoUSD != 0 {
		t.Errorf("marginal_spend = %d, want 0: there is no marginal rule to have priced "+
			"anything, and conflating the two is what §8.1 forbids",
			h.rq.Result.MarginalNanoUSD)
	}
	if h.charged() != h.ledger() {
		t.Fatalf("quota took %d nano and the ledger row carries %d; they are one request",
			h.charged(), h.ledger())
	}
}
