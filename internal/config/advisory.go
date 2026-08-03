package config

import "fmt"

// implausibleTokenRateExp is the power of ten a per-million-token rate has to
// fall below before it is reported. 10^-5 is three orders of magnitude under
// the cheapest card on the market — around $0.02 per million tokens — so
// nothing a vendor publishes lands here, and it is the same line
// cmd/dorangctl's shipped-example test draws.
//
// It is deliberately conservative, and the cost of that is a band it does not
// judge: the per-token spelling of a rate above about $10 per million lands
// between 10^-5 and the cheapest real card, where it is indistinguishable from
// a deliberate near-zero internal price. Widening this to 10^-3 would catch
// those too and would start reporting genuine self-hosted amortizations; a
// warning nobody believes is worse than one that fires less often. The
// arithmetic in cmd/dorangctl, which prices a known request against a
// hand-computed vendor figure, is what covers that band.
const implausibleTokenRateExp = -5

// Advisories returns what is legal, deployable, and almost certainly a mistake.
//
// It is deliberately not part of [Config.Validate], and the split is the point.
// Validate decides whether a gateway may run; an advisory decides whether
// somebody should look. Pricing must never be what stops a gateway from
// serving — a wrong rate is a wrong invoice, an outage is a wrong invoice for
// everybody upstream at once — so a suspicious rate cannot be a load error. But
// `dorangctl config lint` and `dorang --check` already run before a deploy and
// already answer `ok`, and an `ok` is exactly where a silent zero should stop
// being silent.
//
// The list is ordered by the file's own order, and it is safe to ignore: a
// caller that prints nothing behaves as it did before.
func (c *Config) Advisories() []Warning {
	// A nil configuration is what a checker holds when the file was readable and
	// its secrets were not (app.CheckConfig), and a caller in that state should
	// not have to remember which of its two values it has.
	if c == nil {
		return nil
	}
	var out []Warning
	out = c.pricingAdvisories(out)
	return out
}

// pricingAdvisories reports token rates that are three orders of magnitude
// below any real rate card.
//
// This is the hand-written arrival of the same defect the importer used to
// produce (§13.1c): `input: "0.0000025"` is the per-TOKEN spelling of $2.50 per
// million, and in a per-million field it prices every request to a millionth of
// the truth — exactly zero below about two hundred tokens. Every other signal
// says the configuration is right. It parses; lint says ok; the rule is
// selected; `POST /spend/calculate` answers `"missing": false`, because
// `missing` reports that no rule matched and one did. The only symptom is a
// ledger of zeroes, which reads as a gateway that is not metering rather than
// as a rate that is wrong.
//
// Refusing would be the wrong instrument. A genuinely cheap model exists — a
// small open-weights model served for $0.02 per million tokens is a real line
// on a real card — and refusing to start over it would trade an invoice for an
// outage. So this warns, names the figure, and says what the fix is.
func (c *Config) pricingAdvisories(out []Warning) []Warning {
	for i := range c.Pricing.Rules {
		r := &c.Pricing.Rules[i]
		for _, comp := range sortedKeys(r.Rates) {
			if pricingComponentUnit[CanonicalComponent(comp)] != "per_1m_tokens" {
				continue
			}
			lit := string(r.Rates[comp])
			if !decimalBelowPow10(lit, implausibleTokenRateExp) {
				continue
			}
			path := fmt.Sprintf("pricing.rules[%d].rates[%q]", i, comp)
			if r.ID != "" {
				path = fmt.Sprintf("pricing.rules[%s].rates[%q]", r.ID, comp)
			}
			out = append(out, Warning{
				Path: path,
				Message: fmt.Sprintf("%s is a rate per MILLION tokens (§13.1c), and %s per million "+
					"is three orders of magnitude below the cheapest model on the market. This is "+
					"almost always a per-TOKEN figure in a per-million field, which prices every "+
					"request to about a millionth of the truth — and nothing else reports it: the "+
					"rule still matches and /spend/calculate still answers \"missing\": false. If "+
					"the vendor's card says $2.50 per million, write \"2.50\". If this model really "+
					"is that cheap, the rate is correct and this is only a warning", lit, lit),
			})
		}
	}
	return out
}
