package pricing

// Utilization pricing: a marginal rate scaled by how contended the backend was.
//
// # Why this belongs to the marginal class and not to an adjustment
//
// On hardware the operator OWNS there is no vendor invoice to reconcile against. The
// real cost of a request is the fraction of the machine it occupied for as long as it
// occupied it, and a token rate on such a deployment is already a proxy for that — it is
// a per-compute-second rate with the seconds estimated from the token count. A busy
// GPU-second and an idle GPU-second are not the same quantity of the thing being sold,
// so scaling the rate by occupancy CORRECTS the marginal cost rather than adding a
// margin on top of it. That is why the factor multiplies [Cost.MarginalNano] and does
// not land in [Cost.AdjustmentNano], where a discount, a margin or a tax lands.
//
// It is the same argument that gave `per_compute_second` its own axis (§10.7): a
// wall-clock second and an audio second are different units, and a contended
// GPU-second and an idle one are different amounts of the same unit.
//
// # Three properties, and each one is load-bearing
//
//  1. **The factor is never below 1.0 and never above the declared ceiling.** `slope`
//     is non-negative and `max_multiplier` is required, so the range of the factor is
//     exactly [1.0, max_multiplier] and both ends are numbers the operator wrote down.
//     The consequence that matters is at the bottom: the fallback — the factor applied
//     when nothing was observed — is 1.0, which is the LOWEST charge the rule can
//     produce. A broken observation therefore costs the operator their premium and can
//     never overcharge a caller. See [utilFactor].
//
//  2. **A missing observation is a refusal to apply the factor, never a zero.** Zero
//     utilization and unobserved utilization are the same float and mean opposite
//     things — VLLM.md §3.1 says so about `--disable-log-stats`, where `/metrics`
//     answers 200 with no series at all and a naive reader sees an idle machine. For
//     routing that misdirects traffic; here it would bill at the base rate while
//     reporting a measurement of "idle". So the observation carries a separate
//     [Request.UtilizationObserved] flag, the absence is reported as
//     [UtilFallbackNotObserved], and it reaches the response header and the ledger row.
//
//  3. **A quote carries no observation, so routing never sees the factor.** The
//     utilization a request will run at is not knowable before it runs, exactly as its
//     wall time is not ([NoPriceComputeNotMeasured] says so in as many words). So
//     [Catalog.Price], which is what cost-based routing calls, prices at 1.0 and the
//     feedback loop between "route away from load" and "charge more for load" has no
//     gain in it at all. This is a property of the DATA, not of the entry point, which
//     is why [Catalog.Explain] can still show an operator what a given occupancy would
//     cost. TestRoutingQuotesNeverCarryTheMultiplier pins it.

import (
	"errors"
	"fmt"
	"strconv"
)

// OnePPM is the factor that changes nothing, in parts per million. Every multiplier in
// this file is an integer at this scale, so the price path stays exact.
const OnePPM = 1_000_000

// MaxDeclarableMultiplierPPM is the hard ceiling on `max_multiplier`, refused at load.
//
// It is a bound on the blast radius of a METRIC GLITCH, not an opinion about what an
// operator may charge. DESIGN §5.6 requires every mode of a bounded-error mechanism to
// publish its maximum error as a number rather than as "approximately accurate", and
// this is that number for pricing:
//
//	worst case charge  =  base rate  x  max_multiplier
//	                   <= base rate  x  4.000
//
// so a mis-scaled gauge, a `/metrics` endpoint that started answering in percent, or a
// catalog typo can turn a $100 day into at most a $400 day, and cannot turn it into a
// $10,000 day. An operator who genuinely wants more gets a load error naming this
// constant, which is a conversation rather than a surprise on an invoice.
const MaxDeclarableMultiplierPPM = 4 * OnePPM

// UtilFallback says why a rule that declares a utilization factor charged 1.0x anyway.
//
// It is reported for the same reason [Cost.Missing] is: the alternative is a price that
// moved for a reason nobody recorded. A caller disputing an invoice has to be able to
// ask "was this request charged at a premium, and if not, why not", and both answers
// have to be in the row.
type UtilFallback uint8

const (
	// UtilFallbackNone: an observation was present and the factor was applied.
	UtilFallbackNone UtilFallback = iota
	// UtilFallbackNotObserved: no occupancy was observed for this request, so the base
	// rate was charged. It covers every way an observation can fail to arrive — the
	// backend did not report one, the response was streamed, the reading was out of
	// range — and the specific one is recorded by the layer that made the observation,
	// because this package does not speak HTTP.
	//
	// It is NOT "the backend was idle". Idle is UtilFallbackNone with a utilization of
	// zero, and telling the two apart is the whole reason this enum exists.
	UtilFallbackNotObserved
)

func (f UtilFallback) String() string {
	if f == UtilFallbackNotObserved {
		return "not_observed"
	}
	return "none"
}

// Why renders the fallback in words, for the preview endpoint and the operator's log.
func (f UtilFallback) Why() string {
	if f == UtilFallbackNotObserved {
		return "no backend occupancy was observed for this request, so the base rate was " +
			"charged with no utilization factor; an unobserved backend is not an idle one " +
			"and is never priced as if it were"
	}
	return ""
}

// utilization is the compiled factor a marginal rule declares.
//
// Both fields are parts per million so the factor is computed in integers. slope is the
// factor's whole travel across the occupancy range: at occupancy 1.0 the factor is
// 1 + slope, before the ceiling.
type utilization struct {
	slopePPM   int64
	ceilingPPM int64
	// slopeText and ceilingText are the decimals exactly as the catalog wrote them, so
	// an explanation quotes the operator's own numbers back.
	slopeText   string
	ceilingText string
}

// declared reports whether the rule prices on utilization at all.
func (u *utilization) declared() bool { return u != nil && u.ceilingPPM > 0 }

// factor returns the multiplier for an occupancy, in parts per million, and whether the
// ceiling bound it.
//
//	factor = 1 + slope x utilization,  capped at max_multiplier
//
// Stated in integers at ppm scale, which is where the arithmetic actually happens:
//
//	ppm = 1_000_000 + slopePPM * utilPPM / 1_000_000,  capped at ceilingPPM
//
// # Why the product cannot overflow, stated because it is not obvious
//
// Both factors are bounded at load: slopePPM by [MaxDeclarableMultiplierPPM] and utilPPM
// by int32. The largest product is therefore 4e6 x 2.1e9 = 8.4e15, comfortably inside
// int64 — and the slope bound is there for THIS reason as much as for pricing. Without
// it a catalog could write `slope: "1000000000000"` and the multiply would wrap, which
// turns a typo into a negative charge rather than a large one.
//
// A NEGATIVE utilPPM is refused by the caller rather than clamped here, so the result is
// always in [1_000_000, ceilingPPM]. See [applyUtilization].
func (u *utilization) factor(utilPPM int32) (ppm int64, atCeiling bool) {
	ppm = OnePPM + u.slopePPM*int64(utilPPM)/OnePPM
	if ppm >= u.ceilingPPM {
		return u.ceilingPPM, true
	}
	return ppm, false
}

// applyUtilization scales a rule's marginal amount by the occupancy this request ran at.
//
// It returns the scaled amount, the factor that was applied and why it was 1.0 when it
// was. The delta is appended to the component breakdown as its own line, so the
// breakdown still sums to the charge and an operator reading it sees the base lines and
// the factor separately — which is the shape of an invoice a dispute can be settled
// against, rather than a set of rates that do not multiply out to the total.
func applyUtilization(r *rule, req *Request, base amt, comps *[]Component) (amt, int64, bool, UtilFallback, error) {
	u := r.util
	if !u.declared() {
		return base, 0, false, UtilFallbackNone, nil
	}
	if !req.UtilizationObserved || req.UtilizationPPM < 0 {
		// A negative occupancy is not a measurement of anything, and it is the one
		// input that could point the factor DOWNWARDS and take the charge below the
		// base rate — which is the property the whole design rests on not happening.
		// It resolves the way every degenerate case in this feature resolves: against
		// applying the factor.
		//
		// It is refused rather than clamped to zero because a clamp would silently
		// accept a reading that is definitely wrong, and the fallback's charge is the
		// same as a clamp's would be. The only difference is that the row says so.
		// The base rate, recorded as the base rate. §8.3's rule about an unpriced
		// request applies in miniature: the figure is right, the claim that it was
		// measured is not, and only one of the two is safe to make without evidence.
		return base, OnePPM, false, UtilFallbackNotObserved, nil
	}
	ppm, atCeiling := u.factor(req.UtilizationPPM)
	scaled, ok := mulDivAmt(base, uint64(ppm), OnePPM)
	if !ok {
		return amt{}, 0, false, UtilFallbackNone, ErrOverflow
	}
	if comps != nil {
		delta, ok := addAmt(scaled, base.negate())
		if !ok {
			return amt{}, 0, false, UtilFallbackNone, ErrOverflow
		}
		nano, _, err := roundToNano(delta, 0)
		if err != nil {
			return amt{}, 0, false, UtilFallbackNone, err
		}
		*comps = append(*comps, Component{
			Name:   "utilization",
			RuleID: r.id,
			// The factor that was applied, as a decimal, beside the occupancy it
			// came from. Quantity/Scale is this package's existing way of carrying
			// a fractional measured quantity (compute seconds are micro-seconds at
			// scale 6); an occupancy in ppm is the same shape.
			Rate:         formatPPM(ppm),
			Unit:         UnitNone,
			Quantity:     int64(req.UtilizationPPM),
			Scale:        6,
			SubtotalNano: nano,
		})
	}
	return scaled, ppm, atCeiling, UtilFallbackNone, nil
}

// formatPPM renders a ppm factor as a fixed six-place decimal without touching a float.
func formatPPM(ppm int64) string {
	if ppm < 0 {
		// Unreachable for a compiled rule; a defensive zero rather than a string of
		// malformed digits, because this renders into an operator-facing message.
		ppm = 0
	}
	var b [24]byte
	out := strconv.AppendInt(b[:0], ppm/OnePPM, 10)
	out = append(out, '.')
	frac := ppm % OnePPM
	for div := int64(100_000); div > 0; div /= 10 {
		out = append(out, byte('0'+(frac/div)%10))
	}
	return string(out)
}

// rawUtilization is the catalog's `utilization:` block.
type rawUtilization struct {
	Slope         string `yaml:"slope"`
	MaxMultiplier string `yaml:"max_multiplier"`
}

// errUtilizationClass refuses the block on the three classes it cannot mean anything on.
const errUtilizationClass = "utilization belongs to a marginal_usage rule: it corrects " +
	"what a request COST on hardware the operator owns, which a fixed plan (no per-request " +
	"quantity to scale), an adjustment (already a multiply, and one that applies to a total " +
	"the factor has already scaled) and a notional_rate (a VENDOR's published list price, " +
	"which does not move with your GPU) each have no version of"

// compileUtilization compiles and validates the block.
//
// Every refusal here is a refusal to load, not a warning, and each one closes a way for
// the factor to become unbounded or to point downwards.
func compileUtilization(raw *rawUtilization, r *rule) error {
	if raw == nil {
		return nil
	}
	if r.class != ClassMarginal {
		return errors.New(errUtilizationClass)
	}
	if raw.MaxMultiplier == "" {
		return errors.New("utilization.max_multiplier is required: a price that moves with " +
			"a measurement must state the most it can move to, as a number, before it is " +
			"allowed to move at all (DESIGN §5.6). Write the ceiling you would defend on " +
			"an invoice, for example max_multiplier: \"2.0\"")
	}
	slope := decimal{}
	if raw.Slope != "" {
		d, err := parseDecimal(raw.Slope)
		if err != nil {
			return fmt.Errorf("utilization.slope: %w", err)
		}
		if d.neg {
			return errors.New("utilization.slope must not be negative: the factor's range " +
				"is [1.0, max_multiplier] precisely so that an unobserved backend is charged " +
				"the LOWEST price the rule can produce, and a negative slope would make a " +
				"failed observation the most expensive outcome. To discount an idle backend, " +
				"lower the base rate and keep the slope positive")
		}
		slope = d
	}
	ceiling, err := parseDecimal(raw.MaxMultiplier)
	if err != nil {
		return fmt.Errorf("utilization.max_multiplier: %w", err)
	}
	if ceiling.neg {
		return errors.New("utilization.max_multiplier must not be negative")
	}
	slopePPM, ok := slope.scaled(6)
	if !ok {
		return errors.New("utilization.slope: at most six fractional digits (parts per million)")
	}
	ceilingPPM, ok := ceiling.scaled(6)
	if !ok {
		return errors.New("utilization.max_multiplier: at most six fractional digits (parts per million)")
	}
	if ceilingPPM < OnePPM {
		return fmt.Errorf("utilization.max_multiplier is %s, below 1.0: the factor never "+
			"goes below 1.0, so a ceiling under it would clamp every request to a discount "+
			"and make the base rate unreachable", raw.MaxMultiplier)
	}
	if ceilingPPM > MaxDeclarableMultiplierPPM {
		return fmt.Errorf("utilization.max_multiplier is %s, above the %s this build will "+
			"apply. The cap is not an opinion about your prices: it bounds what a MIS-SCALED "+
			"metric can do to an invoice. vLLM's kv_cache_usage_perc is a fraction whose name "+
			"says percent (VLLM.md §3.1), and a reading a hundred times too large multiplied "+
			"by an unbounded ceiling is a hundred-fold bill. Raise the base rate instead",
			raw.MaxMultiplier, formatPPM(MaxDeclarableMultiplierPPM))
	}
	if slopePPM > MaxDeclarableMultiplierPPM {
		// The same bound as the ceiling, and it is a bound on the ARITHMETIC as much
		// as on the price: the factor is slope x occupancy, and an unbounded slope
		// multiplied by an out-of-range reading is an integer overflow, which turns a
		// mis-scaled metric into a wrapped — possibly negative — charge instead of a
		// large one. A steeper curve than this is expressible by lowering the ceiling,
		// which reaches the same maximum sooner and states it.
		return fmt.Errorf("utilization.slope is %s, above %s. The factor is "+
			"slope x occupancy and an unbounded slope makes a mis-scaled reading an "+
			"arithmetic overflow rather than a large number; to reach the ceiling at a "+
			"lower occupancy, lower max_multiplier",
			raw.Slope, formatPPM(MaxDeclarableMultiplierPPM))
	}
	if slopePPM == 0 {
		return errors.New("utilization.slope is zero, so the factor is 1.0 at every " +
			"occupancy and the block does nothing. Remove it, or give it a slope: a " +
			"setting that validates and has no effect is the defect class DESIGN §17.1 " +
			"names as this project's most common")
	}
	r.util = &utilization{
		slopePPM:    int64(slopePPM),
		ceilingPPM:  int64(ceilingPPM),
		slopeText:   slope.text,
		ceilingText: ceiling.text,
	}
	return nil
}

// PublishedCeiling renders the bound a rule's utilization factor cannot exceed, with the
// arithmetic, in the form DESIGN §5.6 requires of a bounded-error mechanism.
//
// It is exported so the admin calculator and `dorangctl` can print the same sentence the
// catalog's own error messages use, rather than each inventing one.
func (c *Catalog) PublishedCeiling(ruleID string) (string, bool) {
	for _, r := range c.rules {
		if r.id != ruleID || !r.util.declared() {
			continue
		}
		u := r.util
		reached, _ := u.factor(OnePPM)
		return fmt.Sprintf(
			"rule %s: factor = 1 + %s x utilization, capped at %s. "+
				"At full occupancy the factor is %s, so the most this rule can charge is "+
				"%s x its base rate. A missing or refused observation charges 1.000000 x, "+
				"which is the least it can charge.",
			r.id, u.slopeText, u.ceilingText, formatPPM(reached), formatPPM(reached)), true
	}
	return "", false
}
