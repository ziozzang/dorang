package pricing

import (
	"errors"
	"strings"
	"testing"
)

// ownedGPU is the rate card of a self-hosted node, and it is a per_compute_second rule on
// purpose: on hardware the operator owns, the billable quantity is time on the machine.
//
//	$0.000800 / compute-second  =  $2.88 / hour
//	factor = 1 + 1.0 x utilization, capped at 2.0
const ownedGPU = `
currency: USD
rules:
  - id: owned-h100
    class: marginal_usage
    match: { provider: local, model: llama-70b }
    unit: per_compute_second
    compute_seconds: "0.000800"
    utilization:
      slope: "1.0"
      max_multiplier: "2.0"
`

// TestChargedAmountAtTwoUtilizations is the whole feature asserted the only way a price
// can be settled: dorang's charged amount against a figure computed by hand from the rate
// card, the request's own duration and the occupancy it ran at.
//
// It compares against LITERALS and not against another function in this package, for the
// same reason [TestChargedAmountMatchesTheVendorInvoice] does. A multiplier is a
// convention — which number multiplies which, and at what scale — and a convention error
// is one every internal function agrees on. The two utilizations are here because one of
// them cannot distinguish a factor from a constant.
func TestChargedAmountAtTwoUtilizations(t *testing.T) {
	c := mustCatalog(t, ownedGPU)

	// A 30-second request. The base charge, by hand:
	//
	//	30 s x $0.000800/s = $0.0240000
	const baseNano = 24_000_000

	for _, tc := range []struct {
		name     string
		fraction float64
		// factor and want, by hand from the rate card above.
		factorPPM int64
		wantNano  int64
	}{
		// A quarter-full KV cache: 1 + 1.0 x 0.25 = 1.25.
		//	$0.0240000 x 1.25 = $0.0300000
		{"quarter full", 0.25, 1_250_000, 30_000_000},
		// Three-quarters full: 1 + 1.0 x 0.75 = 1.75.
		//	$0.0240000 x 1.75 = $0.0420000
		{"three quarters full", 0.75, 1_750_000, 42_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ppm, err := UtilizationFromFraction(tc.fraction)
			if err != nil {
				t.Fatalf("UtilizationFromFraction(%v): %v", tc.fraction, err)
			}
			cost := mustPrice(t, c, Request{
				Provider: "local", Model: "llama-70b", Seconds: 30,
				UtilizationPPM: ppm, UtilizationObserved: true,
				At: at(t, "2026-08-03T12:00:00Z"),
			})
			if cost.MarginalNano != tc.wantNano {
				t.Fatalf("at %.0f%% occupancy charged %d nano ($%s), want %d ($%s); the base "+
					"charge is %d ($%s) and the factor should be %sx",
					tc.fraction*100, cost.MarginalNano, usd9(cost.MarginalNano),
					tc.wantNano, usd9(tc.wantNano), baseNano, usd9(baseNano),
					formatPPM(tc.factorPPM))
			}
			if cost.TotalNano != tc.wantNano {
				t.Errorf("TotalNano = %d, want %d", cost.TotalNano, tc.wantNano)
			}
			if cost.UtilizationMultiplierPPM != tc.factorPPM {
				t.Errorf("published factor = %s, want %s (the charge was right; the number "+
					"the invoice shows for it was not)",
					formatPPM(cost.UtilizationMultiplierPPM), formatPPM(tc.factorPPM))
			}
			if cost.UtilizationFallback != UtilFallbackNone {
				t.Errorf("fallback = %v on an observed request", cost.UtilizationFallback)
			}
			if cost.UtilizationPPM != ppm {
				t.Errorf("published occupancy = %d ppm, want %d", cost.UtilizationPPM, ppm)
			}
			if cost.UtilizationCeiling {
				t.Errorf("the ceiling is 2.0x and the factor is %s; it must not report bound",
					formatPPM(cost.UtilizationMultiplierPPM))
			}

			// The breakdown an operator reads to check the bill has to multiply out.
			// A set of base rates beside a total that is 25% larger, with nothing
			// saying why, is a bill nobody can reconcile — which is exactly the
			// dispute this feature creates if the factor is not a line of its own.
			var sum int64
			var sawFactor bool
			for _, comp := range cost.Components {
				sum += comp.SubtotalNano
				if comp.Name != "utilization" {
					continue
				}
				sawFactor = true
				if comp.Rate != formatPPM(tc.factorPPM) {
					t.Errorf("factor line rate = %q, want %q", comp.Rate, formatPPM(tc.factorPPM))
				}
				if comp.Quantity != int64(ppm) || comp.Scale != 6 {
					t.Errorf("factor line carries %d at scale %d, want the occupancy %d at scale 6",
						comp.Quantity, comp.Scale, ppm)
				}
				if want := tc.wantNano - baseNano; comp.SubtotalNano != want {
					t.Errorf("factor line = %d nano, want %d (the amount the factor ADDED)",
						comp.SubtotalNano, want)
				}
			}
			if !sawFactor {
				t.Fatalf("no `utilization` component line: components = %+v", cost.Components)
			}
			if sum != cost.MarginalNano {
				t.Errorf("component subtotals sum to %d, marginal is %d", sum, cost.MarginalNano)
			}
		})
	}
}

// TestAnUnobservedBackendIsChargedTheBaseRateAndSaysSo is the silent-zero failure in the
// form this feature can take it.
//
// A vLLM started with `--disable-log-stats` answers `/metrics` with 200 and ZERO `vllm:`
// series (VLLM.md §3.1), and a backend that reports no load header on its response is the
// same thing one layer up: a successful call carrying no signal. Read as a number that is
// what a completely idle machine looks like, and the whole point of §3.1's warning is that
// the two are indistinguishable and mean opposite things.
//
// For routing, mistaking one for the other misdirects traffic. Here it would mean the
// request is billed at the base rate while the invoice CLAIMS it ran on an empty machine —
// and if the arithmetic had been written the obvious way (factor = 1 + slope x
// utilization, with utilization defaulting to zero), the claim and the charge would agree
// and nothing would ever look wrong.
//
// So the assertion is in two halves, and the second is the one that matters: the charge is
// the base rate, and the row says WHY it is the base rate.
func TestAnUnobservedBackendIsChargedTheBaseRateAndSaysSo(t *testing.T) {
	c := mustCatalog(t, ownedGPU)
	const baseNano = 24_000_000

	unobserved := mustPrice(t, c, Request{
		Provider: "local", Model: "llama-70b", Seconds: 30,
		// No UtilizationObserved. This is a stream, or a backend that sent no load
		// header, or a reading that was refused as out of range.
		At: at(t, "2026-08-03T12:00:00Z"),
	})
	if unobserved.MarginalNano == 0 {
		t.Fatalf("an unobserved backend was charged nothing: a missing measurement must " +
			"fall back to the base rate, never multiply the rate by an absent number")
	}
	if unobserved.MarginalNano != baseNano {
		t.Fatalf("charged %d nano ($%s) with no observation, want the base rate %d ($%s)",
			unobserved.MarginalNano, usd9(unobserved.MarginalNano), baseNano, usd9(baseNano))
	}
	if !unobserved.UtilizationPriced {
		t.Fatalf("the rule declares a utilization factor, so the fallback must be reported; " +
			"a row with no utilization fields at all is indistinguishable from a rule that " +
			"never had one")
	}
	if unobserved.UtilizationFallback != UtilFallbackNotObserved {
		t.Fatalf("fallback = %v, want %v; the response and the ledger have to say WHICH "+
			"fallback fired or an invoice dispute is unresolvable",
			unobserved.UtilizationFallback, UtilFallbackNotObserved)
	}
	if unobserved.UtilizationMultiplierPPM != OnePPM {
		t.Fatalf("published factor = %s on a fallback, want 1.000000",
			formatPPM(unobserved.UtilizationMultiplierPPM))
	}

	// And the fact that makes the two answerable apart: a genuinely idle backend is a
	// DIFFERENT row. Same charge, different provenance.
	idle := mustPrice(t, c, Request{
		Provider: "local", Model: "llama-70b", Seconds: 30,
		UtilizationPPM: 0, UtilizationObserved: true,
		At: at(t, "2026-08-03T12:00:00Z"),
	})
	if idle.MarginalNano != baseNano {
		t.Fatalf("an idle backend charged %d, want the base rate %d", idle.MarginalNano, baseNano)
	}
	if idle.UtilizationFallback != UtilFallbackNone {
		t.Fatalf("an OBSERVED occupancy of zero reported fallback %v; zero is a measurement "+
			"and must not be filed as an absence", idle.UtilizationFallback)
	}
	if idle.MarginalNano != unobserved.MarginalNano {
		t.Fatalf("the two charges differ (%d vs %d); they are the same amount by design",
			idle.MarginalNano, unobserved.MarginalNano)
	}
	// The whole assertion: same money, and the row still distinguishes them.
	if idle.UtilizationFallback == unobserved.UtilizationFallback {
		t.Fatalf("an idle backend and an unobserved one produce identical rows; they are " +
			"the two facts VLLM.md §3.1 says must never be confused")
	}
}

// TestAFractionReadAsAPercentageIsRefused pins the trap the metric's own NAME sets.
//
// `vllm:kv_cache_usage_perc` is a fraction: 1.0 means 100% full. A reader that takes the
// name at its word and hands over 45.0 for a 45%-full cache is 100x out, and every step
// after that succeeds — the multiply, the rounding, the ledger insert, the invoice.
//
// Two independent guards have to hold, because one of them is a range check that a future
// engine or a Ray-renamed metric could route around:
//
//  1. the conversion at the float boundary refuses the value outright, and
//  2. even if a reading got past it, max_multiplier bounds what it can charge.
func TestAFractionReadAsAPercentageIsRefused(t *testing.T) {
	// A 45%-full KV cache. The fraction is 0.45; the percentage is 45.0; they differ by
	// exactly the 100x this test exists for.
	const (
		asFraction  = 0.45
		asPercent   = 45.0
		theMistake  = 100 // asPercent / asFraction
		wantFactorX = "1.450000"
	)

	ppm, err := UtilizationFromFraction(asFraction)
	if err != nil {
		t.Fatalf("the correct reading was refused: %v", err)
	}
	if ppm != 450_000 {
		t.Fatalf("0.45 converted to %d ppm, want 450000", ppm)
	}

	// Guard 1: the boundary refuses the percentage.
	if _, err := UtilizationFromFraction(asPercent); err == nil {
		t.Fatalf("%.1f was accepted as a utilization fraction. It is %dx the real value "+
			"(%.2f), because kv_cache_usage_perc is a FRACTION whose name says percent "+
			"(VLLM.md §3.1) — and nothing downstream of here can tell the two apart",
			asPercent, theMistake, asFraction)
	} else if !errors.Is(err, ErrUtilizationNotAFraction) {
		t.Fatalf("refused with the wrong error: %v", err)
	} else if !strings.Contains(err.Error(), "100x") {
		t.Fatalf("the refusal does not name the 100x, so an operator reading it in a log "+
			"cannot tell a scale error from a bad number: %v", err)
	}

	c := mustCatalog(t, ownedGPU)
	correct := mustPrice(t, c, Request{
		Provider: "local", Model: "llama-70b", Seconds: 30,
		UtilizationPPM: ppm, UtilizationObserved: true,
		At: at(t, "2026-08-03T12:00:00Z"),
	})
	if got := formatPPM(correct.UtilizationMultiplierPPM); got != wantFactorX {
		t.Fatalf("factor at 45%% = %s, want %s", got, wantFactorX)
	}

	// Guard 2, independent of guard 1: hand the price path the mis-scaled value
	// directly, as though the range check had been bypassed by a metric this build has
	// never seen. The factor at 45.0 would be 1 + 1.0 x 45 = 46.0x — a $0.024 request
	// billed at $1.10. The ceiling has to bring it back to 2.0x on its own.
	bypassed := mustPrice(t, c, Request{
		Provider: "local", Model: "llama-70b", Seconds: 30,
		UtilizationPPM: 45 * OnePPM, UtilizationObserved: true,
		At: at(t, "2026-08-03T12:00:00Z"),
	})
	if !bypassed.UtilizationCeiling {
		t.Fatalf("a 45.0 reading did not report hitting the ceiling")
	}
	const ceilingNano = 48_000_000 // 24_000_000 x 2.0
	if bypassed.MarginalNano != ceilingNano {
		t.Fatalf("a 45.0 reading charged %d nano ($%s); max_multiplier is 2.0 so the most "+
			"chargeable is %d ($%s). Unbounded it would be 46.0x = %d ($%s)",
			bypassed.MarginalNano, usd9(bypassed.MarginalNano),
			ceilingNano, usd9(ceilingNano), int64(46*24_000_000), usd9(46*24_000_000))
	}
}

// TestTheFactorIsChargedAtTheCeilingAndNotBeyond drives the ceiling from the direction an
// operator reaches it legitimately: a steep slope on a genuinely busy machine.
func TestTheFactorIsChargedAtTheCeilingAndNotBeyond(t *testing.T) {
	// factor = 1 + 1.5 x utilization, capped at 2.0.
	c := mustCatalog(t, `
currency: USD
rules:
  - id: steep
    class: marginal_usage
    match: { model: m }
    unit: per_compute_second
    compute_seconds: "0.000800"
    utilization:
      slope: "1.5"
      max_multiplier: "2.0"
`)
	price := func(fraction float64) Cost {
		t.Helper()
		ppm, err := UtilizationFromFraction(fraction)
		if err != nil {
			t.Fatal(err)
		}
		return mustPrice(t, c, Request{
			Model: "m", Seconds: 30, UtilizationPPM: ppm, UtilizationObserved: true,
			At: at(t, "2026-08-03T12:00:00Z"),
		})
	}
	// Below the ceiling: 1 + 1.5 x 0.50 = 1.75. $0.024 x 1.75 = $0.042.
	below := price(0.50)
	if below.MarginalNano != 42_000_000 || below.UtilizationCeiling {
		t.Fatalf("at 50%%: charged %d (want 42000000), ceiling=%v (want false)",
			below.MarginalNano, below.UtilizationCeiling)
	}
	// At the ceiling: 1 + 1.5 x 0.75 = 2.125, capped to 2.000. $0.024 x 2.0 = $0.048,
	// NOT $0.051.
	atCap := price(0.75)
	if !atCap.UtilizationCeiling {
		t.Fatalf("at 75%% the uncapped factor is 2.125 and the ceiling is 2.0; the cap was " +
			"not reported")
	}
	if atCap.MarginalNano != 48_000_000 {
		t.Fatalf("at 75%% charged %d nano ($%s), want the ceiling %d ($%s); uncapped it "+
			"would be %d ($%s)",
			atCap.MarginalNano, usd9(atCap.MarginalNano), int64(48_000_000), usd9(48_000_000),
			int64(51_000_000), usd9(51_000_000))
	}
	if atCap.UtilizationMultiplierPPM != 2*OnePPM {
		t.Fatalf("published factor = %s, want 2.000000",
			formatPPM(atCap.UtilizationMultiplierPPM))
	}
	// And past it the price stops moving, which is the property the ceiling flag exists
	// to disclose: two different measurements, one charge.
	full := price(1.0)
	if full.MarginalNano != atCap.MarginalNano {
		t.Fatalf("75%% and 100%% charged %d and %d; above the ceiling they must agree",
			atCap.MarginalNano, full.MarginalNano)
	}
	if !full.UtilizationCeiling {
		t.Fatalf("at 100%% the ceiling was not reported, so a caller comparing the two " +
			"charges cannot tell that one of the measurements stopped mattering")
	}
	if full.UtilizationCeilingPPM != 2*OnePPM {
		t.Fatalf("published ceiling = %s, want 2.000000 — DESIGN §5.6 requires the bound "+
			"to travel as a number", formatPPM(full.UtilizationCeilingPPM))
	}
}

// TestRoutingQuotesNeverCarryTheMultiplier is the feedback loop, closed structurally
// rather than damped.
//
// `least_busy` routes AWAY from occupancy and this charges MORE for it. If a cost-based
// router could see the factor, the two would be one loop: traffic moves to the cheap
// backend, the cheap backend fills, its price rises, traffic moves back. The usual fix is
// damping and jitter, and internal/quota has both written for exactly this shape.
//
// Neither is needed here, because the loop has no gain to damp. A quote is priced BEFORE
// the request runs and the occupancy it will run at is not knowable then — the same
// argument [NoPriceComputeNotMeasured] already makes about the same instant for wall time.
// So a routing quote carries no observation, prices at 1.0x, and every candidate is
// compared on its base rate.
func TestRoutingQuotesNeverCarryTheMultiplier(t *testing.T) {
	c := mustCatalog(t, ownedGPU)
	// The shape internal/app builds for a routing quote: identity and estimated
	// quantities, no measured duration and no observation.
	quote, err := c.Price(Request{
		Provider: "local", Model: "llama-70b", Seconds: 30,
		At: at(t, "2026-08-03T12:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if quote.UtilizationFallback != UtilFallbackNotObserved {
		t.Fatalf("a quote reported fallback %v; it cannot have observed anything",
			quote.UtilizationFallback)
	}
	if quote.MarginalNano != 24_000_000 {
		t.Fatalf("a quote priced at %d, want the base rate 24000000: a router comparing "+
			"candidates must compare base rates, or the price it reads is a function of "+
			"the load it is trying to route around", quote.MarginalNano)
	}
	// The router reads MarginalNano and only MarginalNano (§8.1). The same request
	// SETTLED at 75% costs 75% more, and that difference must appear only after the
	// request has run.
	settled := mustPrice(t, c, Request{
		Provider: "local", Model: "llama-70b", Seconds: 30,
		UtilizationPPM: 750_000, UtilizationObserved: true,
		At: at(t, "2026-08-03T12:00:00Z"),
	})
	if settled.MarginalNano <= quote.MarginalNano {
		t.Fatalf("settlement (%d) did not exceed the quote (%d); the factor is not reaching "+
			"the charge at all", settled.MarginalNano, quote.MarginalNano)
	}
}

// TestTheFactorScalesTheMarginalCostAndNothingElse pins the three classes it must not
// touch. A plan share accrues with elapsed time and has no occupancy; a notional rate is a
// VENDOR's published list price and does not move with the operator's own GPU (§8.5).
func TestTheFactorScalesTheMarginalCostAndNothingElse(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: owned
    class: marginal_usage
    match: { model: m }
    unit: per_compute_second
    compute_seconds: "0.000800"
    utilization:
      slope: "1.0"
      max_multiplier: "2.0"
  - id: list-rate
    class: notional_rate
    match: { model: m }
    unit: per_compute_second
    compute_seconds: "0.000800"
    source: "vendor price page"
    as_of: "2026-07-01"
`)
	req := Request{
		Model: "m", Seconds: 30, UtilizationPPM: OnePPM, UtilizationObserved: true,
		At: at(t, "2026-08-03T12:00:00Z"),
	}
	cost := mustPrice(t, c, req)
	if cost.MarginalNano != 48_000_000 {
		t.Fatalf("marginal = %d, want 48000000 (the base 24000000 at 2.0x)", cost.MarginalNano)
	}
	if cost.NotionalNano != 24_000_000 {
		t.Fatalf("notional = %d, want the unscaled list rate 24000000: a vendor's published "+
			"price does not move with the operator's own occupancy, and §8.5 exists so an "+
			"operator can compare what they charge against what the market charges — a "+
			"comparison that is meaningless if the factor is applied to both sides",
			cost.NotionalNano)
	}
}

// TestUtilizationBlockRefusals covers every way the block fails to load. Each one closes a
// path by which the factor becomes unbounded, points downwards, or does nothing.
func TestUtilizationBlockRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
		want  string
	}{{
		name:  "no ceiling",
		block: "      slope: \"1.0\"\n",
		want:  "max_multiplier is required",
	}, {
		name:  "ceiling above the build's cap",
		block: "      slope: \"1.0\"\n      max_multiplier: \"10.0\"\n",
		want:  "above the 4.000000 this build will apply",
	}, {
		name:  "ceiling below 1.0",
		block: "      slope: \"1.0\"\n      max_multiplier: \"0.5\"\n",
		want:  "below 1.0",
	}, {
		name:  "negative slope",
		block: "      slope: \"-1.0\"\n      max_multiplier: \"2.0\"\n",
		want:  "must not be negative",
	}, {
		name:  "slope steeper than the build will multiply",
		block: "      slope: \"10.0\"\n      max_multiplier: \"2.0\"\n",
		want:  "above 4.000000",
	}, {
		name:  "zero slope does nothing",
		block: "      slope: \"0\"\n      max_multiplier: \"2.0\"\n",
		want:  "the block does nothing",
	}, {
		name:  "no slope does nothing",
		block: "      max_multiplier: \"2.0\"\n",
		want:  "the block does nothing",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCatalog([]byte(`
currency: USD
rules:
  - id: r
    class: marginal_usage
    match: { model: m }
    unit: per_compute_second
    compute_seconds: "0.0008"
    utilization:
` + tc.block))
			if err == nil {
				t.Fatalf("loaded without error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestUtilizationIsRefusedOnEveryOtherClass. A plan has no per-request quantity to scale,
// an adjustment is already a multiply against a total the factor has scaled once, and a
// notional rate is somebody else's published price.
func TestUtilizationIsRefusedOnEveryOtherClass(t *testing.T) {
	for _, tc := range []struct{ name, rule string }{{
		"subscription", `
  - id: r
    class: fixed_subscription
    match: { model: m }
    amount_per_period: "100.00"
    period: monthly
    utilization: { slope: "1.0", max_multiplier: "2.0" }`,
	}, {
		"adjustment", `
  - id: r
    class: adjustment
    match: { model: m }
    op: percent
    amount: "10"
    utilization: { slope: "1.0", max_multiplier: "2.0" }`,
	}, {
		"notional", `
  - id: r
    class: notional_rate
    match: { model: m }
    unit: per_compute_second
    compute_seconds: "0.0008"
    source: s
    as_of: "2026-07-01"
    utilization: { slope: "1.0", max_multiplier: "2.0" }`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCatalog([]byte("currency: USD\nrules:" + tc.rule + "\n"))
			if err == nil {
				t.Fatalf("loaded without error")
			}
			if !strings.Contains(err.Error(), "utilization belongs to a marginal_usage rule") {
				t.Fatalf("error %q does not name the class rule", err)
			}
		})
	}
}

// TestAnOrdinaryCatalogReportsNoUtilizationAtAll. Off by default is not a config flag
// here, it is the absence of a block: a rule that declares no factor produces a Cost with
// UtilizationPriced clear, so nothing downstream emits a header or a column claiming a
// price moved when it did not.
func TestAnOrdinaryCatalogReportsNoUtilizationAtAll(t *testing.T) {
	c := mustCatalog(t, designRateCard)
	cost := mustPrice(t, c, Request{
		Provider: "plan-a", Model: "model-x", InputTokens: 120, OutputTokens: 15,
		// An observation is present and must be ignored: the operator did not ask for
		// a variable price, so they do not get one.
		UtilizationPPM: OnePPM, UtilizationObserved: true,
		At: at(t, "2026-08-03T12:00:00Z"),
	})
	if cost.UtilizationPriced {
		t.Fatalf("a rule with no utilization block reported a factor")
	}
	if cost.UtilizationMultiplierPPM != 0 {
		t.Fatalf("factor = %d on a rule that declares none", cost.UtilizationMultiplierPPM)
	}
	// 120 x $0.85/1M + 15 x $3.40/1M, unchanged by the observation.
	if want := int64(102_000 + 51_000); cost.MarginalNano != want {
		t.Fatalf("marginal = %d, want %d: an observation must not move a price whose rule "+
			"never opted into moving", cost.MarginalNano, want)
	}
}

// TestTheFactorCannotPointDownwardsOrWrap is the adversarial half of the range property.
//
// [1.0, max_multiplier] is asserted everywhere else from inputs a working observation
// layer produces. This drives it from the inputs a BROKEN one produces, because the
// property that makes the fallback defensible — that 1.0x is the least the rule can charge
// — has to hold against a bad number and not only against a missing one.
func TestTheFactorCannotPointDownwardsOrWrap(t *testing.T) {
	c := mustCatalog(t, ownedGPU)
	const baseNano = 24_000_000
	price := func(ppm int32) Cost {
		t.Helper()
		return mustPrice(t, c, Request{
			Provider: "local", Model: "llama-70b", Seconds: 30,
			UtilizationPPM: ppm, UtilizationObserved: true,
			At: at(t, "2026-08-03T12:00:00Z"),
		})
	}
	// A negative occupancy is the one input that could take a charge BELOW the base
	// rate. It is refused rather than clamped: the charge is the same either way, and
	// only the refusal puts the fact in the row.
	for _, ppm := range []int32{-1, -1_000_000, -2_147_483_648} {
		got := price(ppm)
		if got.MarginalNano != baseNano {
			t.Fatalf("occupancy %d charged %d nano, want the base rate %d", ppm,
				got.MarginalNano, baseNano)
		}
		if got.UtilizationFallback != UtilFallbackNotObserved {
			t.Fatalf("occupancy %d reported fallback %v; a negative reading is not a "+
				"measurement and must not be filed as one", ppm, got.UtilizationFallback)
		}
	}
	// And the largest positive an int32 can carry — a reading no scale error could
	// exceed — still lands on the ceiling rather than wrapping.
	huge := price(2_147_483_647)
	if huge.MarginalNano != 2*baseNano {
		t.Fatalf("a saturating reading charged %d nano ($%s), want the ceiling %d ($%s)",
			huge.MarginalNano, usd9(huge.MarginalNano), int64(2*baseNano), usd9(2*baseNano))
	}
	if !huge.UtilizationCeiling {
		t.Fatal("a saturating reading did not report hitting the ceiling")
	}
	// The invariant, stated once over the whole reachable range: never below the base
	// rate, never above the published ceiling.
	for ppm := int32(0); ppm <= 1_000_000; ppm += 1_237 {
		got := price(ppm)
		if got.MarginalNano < baseNano || got.MarginalNano > 2*baseNano {
			t.Fatalf("occupancy %d charged %d, outside [%d, %d]",
				ppm, got.MarginalNano, baseNano, 2*baseNano)
		}
	}
}

// TestPublishedCeilingStatesTheArithmetic. DESIGN §5.6's rule for a bounded-error
// mechanism is that it publishes its maximum as a number rather than as "approximately
// accurate", and the same rule applies to a price that moves.
func TestPublishedCeilingStatesTheArithmetic(t *testing.T) {
	c := mustCatalog(t, ownedGPU)
	got, ok := c.PublishedCeiling("owned-h100")
	if !ok {
		t.Fatal("no published ceiling for a rule that declares one")
	}
	for _, want := range []string{"1 + 1.0 x utilization", "2.0", "2.000000", "1.000000"} {
		if !strings.Contains(got, want) {
			t.Errorf("published ceiling %q does not contain %q", got, want)
		}
	}
	if _, ok := c.PublishedCeiling("nope"); ok {
		t.Error("a rule that declares no factor published a ceiling")
	}
}

// TestExplainReportsTheFactorAndTheFallback. §8.4: one engine, one answer — the preview
// endpoint, the admin calculator and the CLI all read this.
func TestExplainReportsTheFactorAndTheFallback(t *testing.T) {
	c := mustCatalog(t, ownedGPU)
	base := Request{
		Provider: "local", Model: "llama-70b", Seconds: 30,
		At: at(t, "2026-08-03T12:00:00Z"),
	}
	observed := base
	observed.UtilizationPPM, observed.UtilizationObserved = 750_000, true

	ex := c.Explain(observed)
	if !strings.Contains(strings.Join(ex.Notes, "\n"), "scaled by 1.750000x") {
		t.Errorf("notes do not state the applied factor: %v", ex.Notes)
	}
	ex = c.Explain(base)
	joined := strings.Join(ex.Notes, "\n")
	if !strings.Contains(joined, "NOT applied") || !strings.Contains(joined, "not an idle one") {
		t.Errorf("notes do not explain the fallback: %v", ex.Notes)
	}
}

// TestTheFactorRoundsOnceWithEveryOtherClass. The factor multiplies the exact unrounded
// marginal amount, not the rounded one, so a stream of sub-nano requests still accumulates
// exactly through the carried remainder (§8.3).
func TestTheFactorRoundsOnceWithEveryOtherClass(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: tiny
    class: marginal_usage
    match: { model: m }
    unit: per_1m_tokens
    input: "0.000001"
    utilization:
      slope: "1.0"
      max_multiplier: "2.0"
`)
	// 1 input token at $0.000001/1M is 1e-12 USD = 0.001 nano. At 1.0x that rounds to
	// zero every time; the carried remainder is what makes a thousand of them come to
	// one nano. The factor must not break that by rounding before it multiplies.
	var total int64
	for i := 0; i < 1000; i++ {
		cost, err := c.Settle(Request{
			Model: "m", InputTokens: 1, UtilizationPPM: 0, UtilizationObserved: true,
			At: at(t, "2026-08-03T12:00:00Z"), Settlement: "b",
		})
		if err != nil {
			t.Fatal(err)
		}
		total += cost.MarginalNano
	}
	if total != 1 {
		t.Fatalf("a thousand 0.001-nano requests at 1.0x summed to %d, want 1", total)
	}
}
