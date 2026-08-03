package pricing

import (
	"testing"
)

// An unpriceable request is one where a marginal_usage rule MATCHED and could not
// be applied — the catalog says the wrong thing about this model rather than
// nothing. Three counters read that request: the subscription accumulator inside
// this package, the quota counter in internal/app, and the ledger row.
//
// They used to disagree. `compute` reached the NoPrice arm and `break`s the
// switch rather than compute, so the subscription class still ran with
// settle=true and advanced the accumulator; internal/app recorded nothing on that
// arm and then handed cost.TotalNano to the quota counter from a line outside the
// switch. The result on one request: quota +32.26 USD, ledger 0.00, and 32.26 USD
// of a 100.00 USD plan gone — the accumulator only ever moves forward, so nothing
// could give it back.
//
// The decision: an unpriceable request charges NOTHING to any of the three, and
// the plan share it did not take is still there for the next priceable row of the
// period. That last part is what makes withholding free: the share is a function
// of elapsed time, not of requests.

// unpriceablePlan is the fixture the 32.26 figure comes from: a 100.00 USD
// monthly plan, and a marginal rule quoted per second of audio for a model whose
// requests carry no recording. The rule matches, so this is NoPrice and not
// Missing.
const unpriceablePlan = `
currency: USD
rules:
  - id: plan
    class: fixed_subscription
    match: { credential: c1 }
    amount_per_period: "100.00"
    period: monthly
  - id: transcription
    class: marginal_usage
    match: { credential: c1 }
    unit: per_audio_second
    audio_seconds: "0.0001"
`

// TestAnUnpriceableRequestChargesNothingAndBurnsNoPlan is the defect, asserted on
// the observable that settles it: the plan accumulator, read through the next
// request that CAN be priced.
func TestAnUnpriceableRequestChargesNothingAndBurnsNoPlan(t *testing.T) {
	c := mustCatalog(t, unpriceablePlan)

	// Ten days into a 31-day July, one request the matched rule cannot price:
	// it carries no recording, and the request's own wall time is not a
	// substitute for one.
	bad, err := c.Settle(Request{
		Credential: "c1", Seconds: 8,
		At: at(t, "2026-07-11T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if bad.NoPrice != NoPriceAudioNotMeasured {
		t.Fatalf("NoPrice = %d (%s), want NoPriceAudioNotMeasured: the fixture is not "+
			"exercising the arm this test is about", bad.NoPrice, bad.NoPrice.Why())
	}

	// 100.00 x 10/31 = 32.258064516..., which is what the accumulator used to
	// take on this one request.
	const burned = 32_258_064_516

	// 1. The ledger row. internal/app records nothing on this arm.
	// 2. The quota counter, which takes TotalNano.
	// The two agree only if TotalNano is zero, and it is the SAME number, so
	// asserting it once asserts both.
	if bad.SubscriptionNano != 0 {
		t.Errorf("subscription = %d ($%s) on a request that produces no billable row: "+
			"a plan share attributed to a row the ledger records as unpriced is an "+
			"estimate nobody can reconcile", bad.SubscriptionNano, usd9(bad.SubscriptionNano))
	}
	if bad.TotalNano != 0 {
		t.Fatalf("TotalNano = %d ($%s), want 0. internal/app records nothing for this "+
			"request and hands this figure to the quota counter, so any non-zero value "+
			"here is quota charged against a ledger row of 0.00",
			bad.TotalNano, usd9(bad.TotalNano))
	}
	if bad.MarginalNano != 0 || bad.AdjustmentNano != 0 {
		t.Errorf("marginal = %d, adjustment = %d: an unpriceable request is unpriced in "+
			"every class", bad.MarginalNano, bad.AdjustmentNano)
	}

	// 3. The accrual. The next request that CAN be priced, at the same instant,
	// must find the whole ten days still unattributed. This is the assertion
	// that the withholding deferred the share rather than destroying it — and
	// the one that fails loudly if the accumulator moved.
	good, err := c.Settle(Request{
		Credential: "c1", AudioSeconds: 600, Billed: BilledDuration,
		At: at(t, "2026-07-11T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if good.SubscriptionNano != burned {
		t.Fatalf("the first priceable row of the period took %d ($%s), want %d ($%s). "+
			"The unpriceable request before it advanced the plan accumulator, and the "+
			"accumulator only moves forward — so that share is not merely misattributed, "+
			"it is gone", good.SubscriptionNano, usd9(good.SubscriptionNano),
			burned, usd9(burned))
	}
	// And it really is a priced row: 600 s x $0.0001 = $0.06 of marginal on top.
	if good.MarginalNano != 60_000_000 {
		t.Errorf("marginal = %d, want 60000000", good.MarginalNano)
	}
	if good.TotalNano != burned+60_000_000 {
		t.Errorf("total = %d, want %d", good.TotalNano, burned+60_000_000)
	}
}

// TestAnUnpriceableRequestTakesNoAdjustmentEither closes the arithmetic the fix
// would otherwise leave open. An `add` adjustment does not scale its base, so it
// produces a non-zero TotalNano on a request with no marginal cost and no plan
// share — and internal/app would hand THAT to the quota counter while the ledger
// row recorded nothing. Same disagreement, different sign.
func TestAnUnpriceableRequestTakesNoAdjustmentEither(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: transcription
    class: marginal_usage
    match: { credential: c1 }
    unit: per_audio_second
    audio_seconds: "0.0001"
  - id: platform-fee
    class: adjustment
    match: { credential: c1 }
    op: add
    amount: "0.25"
`)
	cost, err := c.Settle(Request{Credential: "c1", Seconds: 8, At: at(t, "2026-07-11T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if cost.NoPrice == NoPriceNone {
		t.Fatal("the fixture no longer produces an unpriceable request")
	}
	if cost.TotalNano != 0 {
		t.Fatalf("TotalNano = %d ($%s) on an unpriceable request: a flat adjustment "+
			"charged a request that produced no billable row, and the quota counter "+
			"takes this number while the ledger row takes nothing",
			cost.TotalNano, usd9(cost.TotalNano))
	}
}

// TestAPlanWithNoMarginalRuleStillAccrues is the line this fix must not cross.
//
// `Missing` and `NoPrice` are not the same fact. A flat plan is a catalog with no
// marginal_usage rule AT ALL (§8.1), so every one of its requests is Missing —
// and withholding the plan share from those would stop a flat plan from ever
// attributing anything. The refusal is for a rule that matched and could not be
// applied, which is a catalog error, and only for that.
func TestAPlanWithNoMarginalRuleStillAccrues(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: plan
    class: fixed_subscription
    match: { credential: c1 }
    amount_per_period: "100.00"
    period: monthly
`)
	cost, err := c.Settle(Request{Credential: "c1", At: at(t, "2026-07-11T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if !cost.Missing {
		t.Fatal("no marginal rule matched, so Missing must be set")
	}
	if want := int64(32_258_064_516); cost.SubscriptionNano != want {
		t.Fatalf("subscription = %d, want %d: a flat plan has no marginal_usage rule by "+
			"construction, and refusing to attribute its cost would leave the plan "+
			"unbilled forever", cost.SubscriptionNano, want)
	}
	if cost.TotalNano != cost.SubscriptionNano {
		t.Fatalf("total = %d, subscription = %d: internal/app records this row and the "+
			"quota counter takes the same figure, so they must be the same figure",
			cost.TotalNano, cost.SubscriptionNano)
	}
}

// ---------------------------------------------------------------------------
// The second axis of the unit guard
// ---------------------------------------------------------------------------

// TestAComputeSecondRateMayNotPriceAVendorBilledRecording is the direction the
// two-axis split left unguarded.
//
// [TestATranscriptIsChargedForTheRecordingAndNotForTheLatency] pins the original
// defect: a ten-minute recording billed as eight seconds. The split was built so
// that no unqualified second could be picked wrongly — and the check it shipped
// with asked only whether the rule priced a TOKEN count, so a rule quoting
// compute seconds against a request the vendor billed by DURATION was refused by
// neither half. The same wrong answer, through the axis that was supposed to have
// separated them.
//
// The two durations are three orders of magnitude apart, deliberately, exactly as
// in the test this one mirrors.
func TestAComputeSecondRateMayNotPriceAVendorBilledRecording(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: gpu-seconds
    class: marginal_usage
    match: { provider: speech-co, model: transcribe-1 }
    unit: per_compute_second
    compute_seconds: "0.0001"
`)
	cost := mustPrice(t, c, Request{
		Provider: "speech-co", Model: "transcribe-1",
		// A ten-minute recording, transcribed in eight seconds, and the
		// vendor said which of the two it billed.
		AudioSeconds: 600,
		Seconds:      8,
		Billed:       BilledDuration,
		At:           at(t, "2026-07-29T12:00:00Z"),
	})

	// What the unguarded direction charged: the same 8 seconds of dorang's own
	// wall time, against a 600-second invoice worth $0.06.
	const (
		latencyBilledNano = 800_000    // $0.000800000
		vendorNano        = 60_000_000 // $0.060000000
	)
	if cost.MarginalNano == latencyBilledNano {
		t.Fatalf("charged %d nano ($%s) against the vendor's %d nano ($%s): the rule "+
			"prices seconds of COMPUTE and the vendor billed 600 s of RECORDING, so "+
			"the rate was applied to dorang's own 8 s of latency — 1.33%% of the "+
			"invoice, and a plausible number rather than an error",
			cost.MarginalNano, usd9(cost.MarginalNano), vendorNano, usd9(vendorNano))
	}
	if cost.MarginalNano != 0 {
		t.Fatalf("charged %d nano ($%s): a rate on an axis the vendor did not bill "+
			"prices nothing", cost.MarginalNano, usd9(cost.MarginalNano))
	}
	if cost.NoPrice != NoPriceVendorBilledDuration {
		t.Fatalf("NoPrice = %d (%s), want NoPriceVendorBilledDuration: a zero nobody is "+
			"told about is a free request in the ledger", cost.NoPrice, cost.NoPrice.Why())
	}
	if cost.NoPriceQuantity != "compute_seconds" {
		t.Errorf("NoPrice names quantity %q, want compute_seconds — an operator has to "+
			"be told which rate to move", cost.NoPriceQuantity)
	}
	if cost.Missing {
		t.Error("Missing is for a catalog that says NOTHING about this model; a rule matched")
	}
}

// TestAComputeSecondRateMayNotPriceATokenBilledRequest is the mirror, and it is
// here because the assumption that the split was complete had already failed
// once. Asking the question as a whitelist — which quantities does this stated
// billing unit put on the invoice — settles both directions at once, and settles
// the ones nobody has thought of yet in the safe direction.
func TestAComputeSecondRateMayNotPriceATokenBilledRequest(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: gpu-seconds
    class: marginal_usage
    match: { model: m }
    unit: per_compute_second
    compute_seconds: "0.0001"
`)
	cost := mustPrice(t, c, Request{
		Model: "m", InputTokens: 4_000, Seconds: 8,
		Billed: BilledTokens,
		At:     at(t, "2026-07-29T12:00:00Z"),
	})
	if cost.MarginalNano != 0 || cost.NoPrice != NoPriceVendorBilledTokens {
		t.Fatalf("marginal = %d, NoPrice = %d: the vendor billed tokens and this rule "+
			"charges dorang's wall clock, which is not a quantity anyone was invoiced for",
			cost.MarginalNano, cost.NoPrice)
	}
}

// TestACharactersRateMayNotPriceAVendorBilledRecording is the third component the
// old blacklist did not name. It is not a hypothetical shape: a per-1000-character
// rate is how text-to-speech is quoted, and it meets a duration-billed response on
// the same audio surface.
func TestACharactersRateMayNotPriceAVendorBilledRecording(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: tts
    class: marginal_usage
    match: { model: m }
    unit: per_1k_characters
    characters: "0.030"
`)
	cost := mustPrice(t, c, Request{
		Model: "m", Characters: 5_000, AudioSeconds: 600,
		Billed: BilledDuration,
		At:     at(t, "2026-07-29T12:00:00Z"),
	})
	if cost.MarginalNano != 0 || cost.NoPrice != NoPriceVendorBilledDuration {
		t.Fatalf("marginal = %d, NoPrice = %d: the vendor billed a duration and this "+
			"rule prices characters", cost.MarginalNano, cost.NoPrice)
	}
}

// TestAPerRequestRateSurvivesEveryStatedBillingUnit is the boundary of the guard,
// and the reason it is a whitelist of METERED quantities rather than of all of
// them. A per-request fee prices the existence of the call, not a quantity of it,
// so it makes no claim about which axis the vendor metered and cannot be applied
// to the wrong one. Refusing it would refuse the ordinary catalog — a default
// rule carrying `request:` beside everything else — on every request whose vendor
// merely stated its billing unit.
func TestAPerRequestRateSurvivesEveryStatedBillingUnit(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: per-call
    class: marginal_usage
    match: { model: m }
    unit: per_request
    request: "0.002"
`)
	for _, billed := range []BilledUnit{BilledUnstated, BilledTokens, BilledDuration} {
		cost := mustPrice(t, c, Request{
			Model: "m", InputTokens: 100, AudioSeconds: 600, Seconds: 8,
			Billed: billed,
			At:     at(t, "2026-07-29T12:00:00Z"),
		})
		if cost.NoPrice != NoPriceNone || cost.MarginalNano != 2_000_000 {
			t.Errorf("Billed=%v: marginal = %d, NoPrice = %d; a per-request fee is not "+
				"quoted against any measured quantity and is on the invoice whatever "+
				"the vendor metered", billed, cost.MarginalNano, cost.NoPrice)
		}
	}
}
