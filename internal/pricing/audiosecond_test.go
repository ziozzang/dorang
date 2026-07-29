package pricing

import (
	"strings"
	"testing"
)

// The vendor's published rate card for speech-to-text, copied field for field.
//
// $0.006 per minute of audio is what every hosted transcription service charges to
// within a factor of two, and it is quoted PER MINUTE OF RECORDING. dorang prices per
// second, so the rate is the same number divided by sixty and the axis is named in the
// rate's own spelling — which is the whole of the convention this file pins.
const transcriptionRateCard = `
currency: USD
rules:
  - id: vendor-transcription
    class: marginal_usage
    match: { provider: speech-co, model: transcribe-1 }
    unit: per_audio_second
    audio_seconds: "0.0001"
`

// TestATranscriptIsChargedForTheRecordingAndNotForTheLatency is the defect, asserted the
// only way that can settle it: dorang's charged amount against a figure computed by hand
// from the vendor's rate card and the length of the recording.
//
// The two durations are deliberately three orders of magnitude apart. A ten-MINUTE
// recording that a fast model transcribes in eight SECONDS is the ordinary case, not a
// contrived one — that ratio is what a transcription service is for — and it is what
// makes the substitution impossible to pass by accident: any implementation that still
// reads the request's wall time bills 8 seconds where the vendor bills 600, which is
// 1.3% of the invoice.
//
// It compares against a LITERAL rather than against another function in this package, for
// the reason [TestChargedAmountMatchesTheVendorInvoice] gives: the defect was a
// convention error and not an arithmetic one. Every function agreed with every other, and
// the number they agreed on was dorang's own latency.
func TestATranscriptIsChargedForTheRecordingAndNotForTheLatency(t *testing.T) {
	c := mustCatalog(t, transcriptionRateCard)

	// A ten-minute recording. The vendor billed 600 seconds of audio and said so
	// (usage.type: duration); the request itself completed in eight seconds.
	req := Request{
		Provider: "speech-co", Model: "transcribe-1",
		AudioSeconds: 600,
		Seconds:      8,
		Billed:       BilledDuration,
		At:           at(t, "2026-07-29T12:00:00Z"),
	}

	// The invoice, by hand, from the vendor's rate card:
	//
	//	600 s of audio x $0.0001/s = $0.0600000
	const (
		vendorNano = 60_000_000 // $0.060000000
		// What dorang charged before: the same rate against the request's wall
		// time, because one field served both quantities.
		latencyBilledNano = 800_000 // $0.000800000, 1.33% of the invoice
	)

	cost := mustPrice(t, c, req)
	if cost.MarginalNano == latencyBilledNano {
		t.Fatalf("charged %d nano ($%s) against the vendor's %d nano ($%s): the rate was "+
			"applied to the request's wall time (%.0fs) instead of the recording's length "+
			"(%.0fs), so a ten-minute transcript was billed as eight seconds",
			cost.MarginalNano, usd9(cost.MarginalNano), vendorNano, usd9(vendorNano),
			req.Seconds, req.AudioSeconds)
	}
	if cost.MarginalNano != vendorNano {
		t.Fatalf("charged %d nano ($%s), the vendor's invoice is %d nano ($%s)",
			cost.MarginalNano, usd9(cost.MarginalNano), vendorNano, usd9(vendorNano))
	}
	if cost.TotalNano != vendorNano {
		t.Fatalf("TotalNano = %d, want %d", cost.TotalNano, vendorNano)
	}
	if cost.NoPrice != NoPriceNone {
		t.Fatalf("NoPrice = %d (%s): the request carries exactly what the rule prices",
			cost.NoPrice, cost.NoPrice.Why())
	}

	// The breakdown is what an operator reconciles against the invoice, so it has to
	// carry the recording's length and name the axis it was charged on.
	if len(cost.Components) != 1 {
		t.Fatalf("components = %+v, want one line", cost.Components)
	}
	comp := cost.Components[0]
	if comp.Name != "audio_seconds" {
		t.Errorf("component = %q, want audio_seconds", comp.Name)
	}
	if comp.Unit != UnitPerAudioSecond {
		t.Errorf("component unit = %s, want per_audio_second", comp.Unit)
	}
	// Micro-seconds: Scale 6, so 600 s is 600,000,000 of them.
	if comp.Quantity != 600_000_000 || comp.Scale != 6 {
		t.Errorf("component quantity = %d at scale %d, want 600_000_000 at scale 6 "+
			"(the recording, not the 8 s the request took)", comp.Quantity, comp.Scale)
	}
}

// TestAComputeSecondRateStillPricesWallTime is the other half, and the reason this was
// not fixed by simply renaming the field. A GPU-second rate is a real thing: a
// self-hosted deployment billed by occupancy is priced on how long the request held the
// machine, and for THAT rate the request's own duration is the correct input.
//
// The same request as above, priced by a rule that names the other axis, must charge the
// eight seconds — so the two rules are not two spellings of one behaviour.
func TestAComputeSecondRateStillPricesWallTime(t *testing.T) {
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
		AudioSeconds: 600, Seconds: 8,
		At: at(t, "2026-07-29T12:00:00Z"),
	})
	// 8 s of wall time x $0.0001/s = $0.0008.
	const wantNano = 800_000
	if cost.MarginalNano != wantNano {
		t.Fatalf("charged %d nano ($%s), want %d ($%s): a per_compute_second rate prices "+
			"the request's own duration and must not reach for the recording's",
			cost.MarginalNano, usd9(cost.MarginalNano), wantNano, usd9(wantNano))
	}
	if cost.Components[0].Name != "compute_seconds" {
		t.Errorf("component = %q, want compute_seconds", cost.Components[0].Name)
	}
}

// TestAnAudioRateAgainstNoRecordingIsUnpricedAndNotFree is what makes the separation safe
// to ship. Splitting one field into two turns the old wrong answer into a zero, and a
// zero is the worse of the two: a wrong invoice gets disputed and a free request does
// not. The rule matched, so Missing is false, and something else has to say so.
func TestAnAudioRateAgainstNoRecordingIsUnpricedAndNotFree(t *testing.T) {
	c := mustCatalog(t, transcriptionRateCard)
	cost := mustPrice(t, c, Request{
		Provider: "speech-co", Model: "transcribe-1",
		Seconds: 8, // the request took eight seconds and reported no duration
		At:      at(t, "2026-07-29T12:00:00Z"),
	})
	if cost.MarginalNano != 0 {
		t.Fatalf("marginal = %d: with no recorded duration there is nothing to apply the "+
			"rate to, and the request's wall time is not a substitute", cost.MarginalNano)
	}
	if cost.Missing {
		t.Error("Missing is for a catalog that says NOTHING about this model; a rule matched")
	}
	if cost.NoPrice != NoPriceAudioNotMeasured {
		t.Fatalf("NoPrice = %d, want NoPriceAudioNotMeasured: a zero nobody is told about "+
			"is a free request in the ledger", cost.NoPrice)
	}
	if cost.NoPriceRule != "vendor-transcription" || cost.NoPriceQuantity != "audio_seconds" {
		t.Errorf("NoPrice names rule %q quantity %q, want vendor-transcription/audio_seconds",
			cost.NoPriceRule, cost.NoPriceQuantity)
	}
}

// TestARuleMayNotPriceTheUnitTheVendorDidNotBillIn is §10.7's rule one level up. The
// audio surface states its billing unit on the wire, so "this vendor billed tokens" is a
// fact dorang has rather than one it infers — and a duration rate applied to a
// token-billed transcript is the same error as a duration rate applied to wall time,
// except that the arithmetic succeeds and produces a plausible figure.
func TestARuleMayNotPriceTheUnitTheVendorDidNotBillIn(t *testing.T) {
	audio := mustCatalog(t, transcriptionRateCard)
	// The backend reported a duration AND said it billed in tokens. Both numbers are
	// real; only one of them is on the invoice.
	cost := mustPrice(t, audio, Request{
		Provider: "speech-co", Model: "transcribe-1",
		AudioSeconds: 600, Seconds: 8, InputTokens: 4_000,
		Billed: BilledTokens,
		At:     at(t, "2026-07-29T12:00:00Z"),
	})
	if cost.MarginalNano != 0 || cost.NoPrice != NoPriceVendorBilledTokens {
		t.Fatalf("marginal = %d, NoPrice = %d: a per_audio_second rate against a "+
			"token-billed request charges a rate against a quantity nobody was invoiced for",
			cost.MarginalNano, cost.NoPrice)
	}

	// And the other direction, which is the one an operator reaches by writing the
	// ordinary token rule for a model that bills by the second: the token counts on
	// such a response are absent or synthesized, so the rule charges a confident zero.
	tokens := mustCatalog(t, `
currency: USD
rules:
  - id: token-rule
    class: marginal_usage
    match: { model: transcribe-1 }
    unit: per_1m_tokens
    input: "6.00"
`)
	cost = mustPrice(t, tokens, Request{
		Model: "transcribe-1", AudioSeconds: 600, Seconds: 8,
		Billed: BilledDuration,
		At:     at(t, "2026-07-29T12:00:00Z"),
	})
	if cost.NoPrice != NoPriceVendorBilledDuration {
		t.Fatalf("NoPrice = %d, want NoPriceVendorBilledDuration: the vendor billed a "+
			"duration and this rule prices tokens", cost.NoPrice)
	}
}

// TestTheAmbiguousSecondSpellingIsALoadError is the half of the fix a catalog author
// meets. The rule is decided once and written where they read it, rather than left to a
// per-request knob: there is no `per_second` and no `seconds:`, so a rate cannot be
// declared without saying which quantity it prices.
//
// The refusal has to NAME both replacements. An operator hitting it is not making a typo
// — they are writing the only spelling this catalog used to have — so "unknown unit" is
// the wrong diagnosis and would send them looking for one.
func TestTheAmbiguousSecondSpellingIsALoadError(t *testing.T) {
	cases := []struct{ name, yaml string }{
		{"unit", `
rules:
  - { id: r, match: { model: m }, unit: per_second, compute_seconds: "0.006" }
`},
		{"rate", `
rules:
  - { id: r, match: { model: m }, unit: per_compute_second, seconds: "0.006" }
`},
		{"rate in a tier", `
rules:
  - id: r
    match: { model: m }
    unit: per_compute_second
    tiers:
      - { up_to_input_tokens: 100, seconds: "0.006" }
      - { seconds: "0.004" }
`},
		{"rate on a class that prices nothing", `
rules:
  - { id: r, class: fixed_subscription, match: {}, amount_per_period: "20.00", seconds: "0.006" }
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseCatalog([]byte(c.yaml))
			if err == nil {
				t.Fatal("a per-second rate that does not say which second it prices loaded")
			}
			// Both axes, in whichever spelling the refusal is about — the unit
			// (`per_audio_second`) or the rate (`audio_seconds`).
			for _, want := range []string{"compute_second", "audio_second"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %s, so it does not say what to "+
						"write instead: %v", want, err)
				}
			}
		})
	}
}

// TestTheTwoSecondAxesAreNotInterchangeableInARate pins the other load error: a rate that
// belongs to one axis may not be declared under the other's unit. Without it the unit and
// the rate could disagree, and one of the two would silently win.
func TestTheTwoSecondAxesAreNotInterchangeableInARate(t *testing.T) {
	_, err := ParseCatalog([]byte(`
rules:
  - { id: r, match: { model: m }, unit: per_audio_second, compute_seconds: "0.006" }
`))
	if err == nil {
		t.Fatal("a compute_seconds rate loaded under unit per_audio_second")
	}
	if !strings.Contains(err.Error(), "per_compute_second") {
		t.Errorf("the refusal does not say which unit the rate belongs to: %v", err)
	}
}

// TestANotionalAudioRateIsUnavailableAndNotZero applies the same rule to §8.5's class.
// A notional figure invented from a quantity nobody measured would make a subscription
// look efficient against a rate that was never charged, and it would look like a
// measurement — which is exactly what NotionalMissing exists to prevent.
func TestANotionalAudioRateIsUnavailableAndNotZero(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - { id: billed, class: marginal_usage, match: { model: m }, unit: per_1m_tokens, input: "1.00" }
  - id: list-rate
    class: notional_rate
    match: { model: m }
    unit: per_audio_second
    audio_seconds: "0.0001"
    source: vendor public price page
    as_of: "2026-07-01"
`)
	cost := mustPrice(t, c, Request{
		Model: "m", InputTokens: 1_000_000, Seconds: 8,
		At: at(t, "2026-07-29T12:00:00Z"),
	})
	if cost.MarginalNano != 1_000_000_000 {
		t.Fatalf("marginal = %d: the billed rule prices tokens and is unaffected", cost.MarginalNano)
	}
	if !cost.NotionalMissing || cost.NotionalNano != 0 {
		t.Fatalf("notional = %d, missing = %v: a list rate quoted per second of audio "+
			"cannot be estimated for a request that carries none",
			cost.NotionalNano, cost.NotionalMissing)
	}
}

// TestExplainNamesTheRuleThatCouldNotPrice keeps the §8.4 surfaces — the preview
// endpoint, the admin calculator and the CLI — able to say what an operator has to fix.
// "Unpriced" without the rule id sends them reading the whole catalog.
func TestExplainNamesTheRuleThatCouldNotPrice(t *testing.T) {
	c := mustCatalog(t, transcriptionRateCard)
	ex := c.Explain(Request{
		Provider: "speech-co", Model: "transcribe-1", Seconds: 8,
		At: at(t, "2026-07-29T12:00:00Z"),
	})
	joined := strings.Join(ex.Notes, "\n")
	for _, want := range []string{"vendor-transcription", "audio_seconds", "unpriced, not free"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the explanation does not mention %q:\n%s", want, joined)
		}
	}
}
