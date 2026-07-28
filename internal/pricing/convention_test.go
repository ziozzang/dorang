package pricing

import (
	"fmt"
	"testing"
)

// The rate card of DESIGN §8.5, copied field for field, as a marginal rule.
//
// It is the fixture on purpose: the design's own example is the one rate table a reader is
// most likely to transcribe, and the defect this file pins made the design's own example
// bill 27% over the vendor it was copied from.
const designRateCard = `
currency: USD
rules:
  - id: vendor-list-rate
    class: marginal_usage
    match: { provider: plan-a, model: model-x }
    unit: per_1m_tokens
    input:  "0.85"
    output: "3.40"
    cache_read: "0.19"
`

// TestChargedAmountMatchesTheVendorInvoice is the whole of the cache/reasoning double-charge,
// asserted the only way that can settle it: dorang's charged amount against a figure computed
// by hand from the vendor's published rate card and the request's own counts.
//
// It deliberately compares against a LITERAL and not against another function in this
// package. The defect was a convention error, not an arithmetic error — every internal
// function agreed with every other one, and the number they agreed on was not the invoice.
// Two of this package's own tests passed throughout, because their fixtures set the cache
// count equal to the whole prompt, which is the one shape under which the two conventions
// are hardest to tell apart.
//
// The measurement that found it: a live cutover priced a 120-input / 15-output request with
// a 40-token cached prefix at $0.0001606 against the vendor's $0.0001266.
func TestChargedAmountMatchesTheVendorInvoice(t *testing.T) {
	c := mustCatalog(t, designRateCard)

	// The counts as dorang carries them (§10.7, inclusive): the prompt is 120 tokens, 40 of
	// which the vendor served from cache. The completion is 15 tokens.
	req := Request{
		Provider: "plan-a", Model: "model-x",
		InputTokens: 120, CacheReadTokens: 40, OutputTokens: 15,
		At: at(t, "2026-07-29T12:00:00Z"),
	}

	// The invoice, by hand, from the vendor's rate card:
	//
	//	uncached prompt   (120 - 40) x $0.85/1M = $0.0000680
	//	cached prefix             40 x $0.19/1M = $0.0000076
	//	completion                15 x $3.40/1M = $0.0000510
	//	                                          ----------
	//	                                          $0.0001266
	const (
		vendorNano = 126_600 // $0.0001266
		// What dorang charged before: the input rate applied to the whole inclusive
		// prompt, and cache_read applied to the cached part of it a second time.
		doubleChargedNano = 160_600 // $0.0001606, 26.9% over
	)

	cost := mustPrice(t, c, req)
	if cost.MarginalNano == doubleChargedNano {
		t.Fatalf("charged %d nano ($%s) against the vendor's %d nano ($%s): the input rate "+
			"was applied to the whole inclusive prompt and cache_read to the cached prefix "+
			"again, so every cached token was billed twice",
			cost.MarginalNano, usd9(cost.MarginalNano), vendorNano, usd9(vendorNano))
	}
	if cost.MarginalNano != vendorNano {
		t.Fatalf("charged %d nano ($%s), the vendor's invoice is %d nano ($%s)",
			cost.MarginalNano, usd9(cost.MarginalNano), vendorNano, usd9(vendorNano))
	}
	if cost.TotalNano != vendorNano {
		t.Fatalf("TotalNano = %d, want %d", cost.TotalNano, vendorNano)
	}

	// The component breakdown is what an operator reads to check the bill, so it has to
	// carry the CHARGED quantity and not the measured one. A breakdown that says "input:
	// 120 tokens" beside a subtotal for 80 is a bill nobody can reconcile.
	want := map[string]struct{ qty, nano int64 }{
		"input":      {80, 68_000},
		"output":     {15, 51_000},
		"cache_read": {40, 7_600},
	}
	if len(cost.Components) != len(want) {
		t.Fatalf("components = %+v, want %d lines", cost.Components, len(want))
	}
	var sum int64
	for _, comp := range cost.Components {
		w, ok := want[comp.Name]
		if !ok {
			t.Fatalf("unexpected component %q", comp.Name)
		}
		if comp.Quantity != w.qty || comp.SubtotalNano != w.nano {
			t.Errorf("component %s = %d tokens / %d nano, want %d / %d",
				comp.Name, comp.Quantity, comp.SubtotalNano, w.qty, w.nano)
		}
		sum += comp.SubtotalNano
	}
	if sum != cost.MarginalNano {
		t.Errorf("component subtotals sum to %d, marginal is %d", sum, cost.MarginalNano)
	}
}

// TestAnAgenticCacheHitIsNotBilledAsAFreshPrompt is the same defect at the magnitude that
// matters. An agent turn re-sends its whole conversation and the provider serves nearly all
// of it from cache; that is the workload a gateway is deployed for, and it is where charging
// the input rate against the cached prefix stops being a rounding error.
//
// The rate card is the real shape: a cached read costs a tenth of a fresh input token.
func TestAnAgenticCacheHitIsNotBilledAsAFreshPrompt(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: sonnet-shaped
    class: marginal_usage
    match: { model: m }
    unit: per_1m_tokens
    input:      "3.00"
    cache_read: "0.30"
    cache_write: "3.75"
    output:     "15.00"
`)
	// 100k of context, 90% of it a cache hit, and the turn produced nothing yet.
	cost := mustPrice(t, c, Request{
		Model: "m", InputTokens: 100_000, CacheReadTokens: 90_000,
		At: at(t, "2026-07-29T12:00:00Z"),
	})
	// 10,000 x $3.00/1M + 90,000 x $0.30/1M = $0.030 + $0.027 = $0.057.
	// The additive rule charged 100,000 x $3.00/1M + $0.027 = $0.327 — 5.74x the invoice.
	const wantNano = 57_000_000
	if cost.MarginalNano != wantNano {
		t.Fatalf("a 90%%-cached turn charged %d nano ($%s), want %d ($%s); the additive rule "+
			"charged 327_000_000 ($0.327000000), 5.74x over",
			cost.MarginalNano, usd9(cost.MarginalNano), wantNano, usd9(wantNano))
	}
}

// TestReasoningIsNotBilledAsOutputAndAgainAsReasoning is the output half of the same rule.
// §10.7 requires ReasoningTokens to be reported separately AND to be contained in
// OutputTokens — "so cost never adds them twice" — and the arithmetic added them twice.
func TestReasoningIsNotBilledAsOutputAndAgainAsReasoning(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: reasoning-priced-apart
    class: marginal_usage
    match: { model: m }
    unit: per_1m_tokens
    input:     "1.00"
    output:    "4.00"
    reasoning: "8.00"
`)
	// 1,000 completion tokens, 400 of which the model spent thinking.
	cost := mustPrice(t, c, Request{
		Model: "m", InputTokens: 0, OutputTokens: 1_000, ReasoningTokens: 400,
		At: at(t, "2026-07-29T12:00:00Z"),
	})
	// 600 x $4.00/1M + 400 x $8.00/1M = $0.0024 + $0.0032 = $0.0056.
	const wantNano = 5_600_000
	if cost.MarginalNano != wantNano {
		t.Fatalf("charged %d nano ($%s), want %d ($%s); charging the output rate on all 1,000 "+
			"tokens and the reasoning rate on 400 of them again gives 7_200_000",
			cost.MarginalNano, usd9(cost.MarginalNano), wantNano, usd9(wantNano))
	}
}

// TestAnUndeclaredSubRateLeavesItsTokensWithTheParent is the other half of the convention,
// and the half that makes it safe to state as one rule rather than as a per-catalog knob:
// a rate table that says nothing about cache is a table whose input price covers cached
// tokens too, which is exactly what a vendor with no cache discount charges.
func TestAnUndeclaredSubRateLeavesItsTokensWithTheParent(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: no-cache-discount
    class: marginal_usage
    match: { model: m }
    unit: per_1m_tokens
    input:  "0.85"
    output: "3.40"
`)
	cost := mustPrice(t, c, Request{
		Model: "m", InputTokens: 120, CacheReadTokens: 40, CacheWriteTokens: 10,
		OutputTokens: 15, ReasoningTokens: 5,
		At: at(t, "2026-07-29T12:00:00Z"),
	})
	// The whole prompt at the input rate and the whole completion at the output rate:
	// 120 x $0.85/1M + 15 x $3.40/1M.
	const wantNano = 102_000 + 51_000
	if cost.MarginalNano != wantNano {
		t.Fatalf("charged %d nano, want %d: an undeclared cache_read must not carve tokens "+
			"out of a rate that is the only one covering them", cost.MarginalNano, wantNano)
	}
}

// TestACacheCountLargerThanThePromptDoesNotCredit pins the clamp. A backend that reports
// more cached tokens than prompt tokens has contradicted the inclusive form; the charge for
// the parent falls to zero and never below it, because a negative component would hand back
// budget and quota that nobody paid for.
func TestACacheCountLargerThanThePromptDoesNotCredit(t *testing.T) {
	c := mustCatalog(t, designRateCard)
	cost := mustPrice(t, c, Request{
		Provider: "plan-a", Model: "model-x",
		InputTokens: 100, CacheReadTokens: 400, OutputTokens: 0,
		At: at(t, "2026-07-29T12:00:00Z"),
	})
	// Only the cache_read line: 400 x $0.19/1M.
	if want := int64(76_000); cost.MarginalNano != want {
		t.Fatalf("marginal = %d, want %d", cost.MarginalNano, want)
	}
	if cost.MarginalNano < 0 || cost.TotalNano < 0 {
		t.Fatalf("a contradictory usage report produced a negative charge: %+v", cost)
	}
}

// TestGraduatedBracketsChargeTheCachedPrefixOnce covers the one place the carve-out is not a
// single subtraction: graduated tiers walk the whole prompt to place the brackets, and the
// cached prefix comes off the front of the walk because a cache hit IS a prefix.
func TestGraduatedBracketsChargeTheCachedPrefixOnce(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - id: graded
    class: marginal_usage
    match: { model: m }
    unit: per_1m_tokens
    tier_mode: graduated
    cache_read: "0.10"
    tiers:
      - up_to_input_tokens: 100000
        input: "1.00"
      - input: "2.00"
`)
	// 150,000 prompt tokens, the first 100,000 of them served from cache. The cached
	// prefix fills the first bracket exactly, so nothing is left in it to charge and the
	// remaining 50,000 are charged in the second.
	cost := mustPrice(t, c, Request{
		Model: "m", InputTokens: 150_000, CacheReadTokens: 100_000,
		At: at(t, "2026-07-29T12:00:00Z"),
	})
	// 50,000 x $2.00/1M + 100,000 x $0.10/1M = $0.100 + $0.010.
	if want := int64(110_000_000); cost.MarginalNano != want {
		t.Fatalf("marginal = %d, want %d", cost.MarginalNano, want)
	}
}

// usd9 renders a nano amount in whole currency units, so a failure prints the figure an
// operator would have compared against an invoice rather than a count of nano.
func usd9(nano int64) string {
	sign := ""
	if nano < 0 {
		sign, nano = "-", -nano
	}
	return fmt.Sprintf("%s%d.%09d", sign, nano/1e9, nano%1e9)
}
