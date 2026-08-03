package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// utilizationDispatcher answers a priced request whose rule prices on backend occupancy
// (DESIGN §8.6). streamed selects the branch where the price does not exist yet.
func utilizationDispatcher(mulPPM, ceilPPM int64, occPPM int32, source string, streamed bool) Dispatcher {
	return DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
		rq.Result.Provider = "local"
		rq.Result.Deployment = "h100-a"
		rq.Result.UpstreamModel = "llama-70b"
		rq.Result.Attempt = 1
		// A streamed answer is priced AFTER its last frame, so Priced is still
		// false when the header block is written — which is exactly what makes
		// the cost deferred. The utilization fields are set here anyway, because
		// the guard being tested has to hold even if a later edit populates them
		// before the price they belong to exists.
		if !streamed {
			rq.Result.Priced = true
			rq.Result.CostNanoUSD = 42_000_000
			rq.Result.MarginalNanoUSD = 42_000_000
		}
		rq.Result.UtilizationPriced = true
		rq.Result.UtilizationMultiplierPPM = mulPPM
		rq.Result.UtilizationCeilingPPM = ceilPPM
		rq.Result.UtilizationPPM = occPPM
		rq.Result.UtilizationSource = source
		if streamed {
			w.Header().Set("Content-Type", "text/event-stream")
			_, err := io.WriteString(w, "data: [DONE]\n\n")
			return err
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"ok":true}`)
		return err
	})
}

// TestTheAppliedFactorIsPublishedBesideTheCost.
//
// §8.5 says a notional figure exists "for traceability and prediction". A price that MOVES
// is the case where that obligation actually bites: a caller receiving x-dorang-cost-usd
// on a variable-price deployment cannot reproduce the number from any rate card unless the
// factor travels with it, and cannot bound their exposure unless the ceiling does too.
//
// Both are in the always-on set rather than behind x-dorang-detail. §10.4 bounds that set
// to identification and cost, and a factor the charge was multiplied by is not telemetry
// ABOUT the cost — it is a term OF it.
func TestTheAppliedFactorIsPublishedBesideTheCost(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = utilizationDispatcher(1_750_000, 2_000_000, 750_000, "observed", false)
	})
	h := do(s, post("/v1/chat/completions", `{"model":"llama-70b"}`)).Header()

	if got := h.Get(HeaderUtilizationMultiplier); got != "1.750000" {
		t.Errorf("%s = %q, want %q", HeaderUtilizationMultiplier, got, "1.750000")
	}
	if got := h.Get(HeaderUtilizationCeiling); got != "2.000000" {
		t.Errorf("%s = %q, want %q; a price that moves has to publish the most it can "+
			"move to, as a number", HeaderUtilizationCeiling, got, "2.000000")
	}
	if got := h.Get(HeaderUtilizationSource); got != "observed" {
		t.Errorf("%s = %q, want %q", HeaderUtilizationSource, got, "observed")
	}
	if h.Get(HeaderCostUSD) == "" {
		t.Error("the factor was published without the cost it is a term of")
	}
	// The occupancy itself is a statement about the operator's fleet rather than about
	// the caller's request, so it waits for x-dorang-detail.
	if got := h.Get(HeaderUtilization); got != "" {
		t.Errorf("%s = %q on a request that did not ask for detail: how full the "+
			"operator's machines are is not on every tenant's response",
			HeaderUtilization, got)
	}
	rq := post("/v1/chat/completions", `{"model":"llama-70b"}`)
	rq.Header.Set(HeaderDetail, "full")
	if got := do(s, rq).Header().Get(HeaderUtilization); got != "0.750000" {
		t.Errorf("%s = %q under detail, want %q", HeaderUtilization, got, "0.750000")
	}
}

// TestAFallbackPublishesOneTimesAndSaysWhy is the disclosure half of the silent-zero rule.
//
// A response that carried the factor only when the price moved would leave a caller unable
// to tell "the backend was idle" from "nobody observed it" — the two facts VLLM.md §3.1
// says must never be confused — because both charge 1.000000x. So the headers are emitted
// on the fallback too, and the source names which fallback fired.
func TestAFallbackPublishesOneTimesAndSaysWhy(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = utilizationDispatcher(1_000_000, 2_000_000, 0, "no_load_header", false)
	})
	h := do(s, post("/v1/chat/completions", `{"model":"llama-70b"}`)).Header()

	if got := h.Get(HeaderUtilizationMultiplier); got != "1.000000" {
		t.Fatalf("%s = %q on a fallback, want 1.000000", HeaderUtilizationMultiplier, got)
	}
	if got := h.Get(HeaderUtilizationSource); got != "no_load_header" {
		t.Fatalf("%s = %q, want the refusal that fired; without it a 1.000000x is "+
			"unattributable and an invoice dispute has nothing to turn on",
			HeaderUtilizationSource, got)
	}
	// And no occupancy, even under detail: rendering 0.000000 beside a source that says
	// nobody looked is a measurement nobody made.
	rq := post("/v1/chat/completions", `{"model":"llama-70b"}`)
	rq.Header.Set(HeaderDetail, "full")
	if got := do(s, rq).Header().Get(HeaderUtilization); got != "" {
		t.Errorf("%s = %q on a fallback: an unobserved backend has no occupancy to report",
			HeaderUtilization, got)
	}
}

// TestAnOrdinaryRateCardPublishesNothing. Off by default is the absence of a block in the
// price catalog, and the absence has to reach the wire: a deployment that does not price
// on occupancy must not emit a header claiming its price is variable and pinned at 1.0x.
func TestAnOrdinaryRateCardPublishesNothing(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Dispatcher = unpricedDispatcher() })
	h := do(s, post("/v1/chat/completions", `{"model":"model-x"}`)).Header()
	for _, name := range []string{
		HeaderUtilizationMultiplier, HeaderUtilizationCeiling,
		HeaderUtilizationSource, HeaderUtilization,
	} {
		if got := h.Get(name); got != "" {
			t.Errorf("%s = %q on a deployment that does not price on utilization", name, got)
		}
	}
}

// TestAStreamedAnswerOmitsTheFactorRatherThanClaimingOne.
//
// A stream is priced after its last frame and these headers went out before its first, so
// the factor does not exist when they are written. Publishing 1.000000 there would say
// "this request was charged at the base rate", for every streamed request — which in an
// agent deployment is all of them — and it would be the identical string a genuine
// fallback produces. That is exactly the failure LegacyHeaderResponseCost documents for
// the cost mirror, so the answer is the same: omit, and let the number travel in the
// ledger joined by x-dorang-request-id.
func TestAStreamedAnswerOmitsTheFactorRatherThanClaimingOne(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = utilizationDispatcher(1_000_000, 2_000_000, 0, "streamed", true)
	})
	h := do(s, post("/v1/chat/completions", `{"model":"llama-70b","stream":true}`)).Header()
	for _, name := range []string{
		HeaderUtilizationMultiplier, HeaderUtilizationCeiling, HeaderUtilizationSource,
	} {
		if got := h.Get(name); got != "" {
			t.Errorf("%s = %q on a stream: the price is settled after the last frame and "+
				"these headers were written before the first", name, got)
		}
	}
}

// TestPPMDecimalRendersAFactorAsANumber. A multiplier on an invoice reads 1.450000, never
// 1450000, and it never carries a character that would need quoting in a header value.
func TestPPMDecimalRendersAFactorAsANumber(t *testing.T) {
	for _, tc := range []struct {
		ppm  int64
		want string
	}{
		{0, "0.000000"},
		{1_000_000, "1.000000"},
		{1_450_000, "1.450000"},
		{2_000_000, "2.000000"},
		{4_000_000, "4.000000"},
		{1_000_001, "1.000001"},
		{999_999, "0.999999"},
		{-5, "0.000000"}, // never a negative factor on the wire
	} {
		if got := ppmDecimal(tc.ppm); got != tc.want {
			t.Errorf("ppmDecimal(%d) = %q, want %q", tc.ppm, got, tc.want)
		}
	}
}

// TestUtilizationHeadersMatchTheLoadSignalVocabulary.
//
// internal/server imports no sibling of internal/app's, so the token for "a reading was
// actually taken" is a literal here and a String() method in internal/loadsignal. Two
// spellings of one value is how a header comes to disagree with the ledger column beside
// it, so the copy is checked rather than trusted.
//
// The check is a literal comparison and not an import, on purpose: importing would make
// the two one value and there would be nothing left to check, which is fine — but the
// dependency direction §1 fixes does not allow it.
func TestUtilizationHeadersMatchTheLoadSignalVocabulary(t *testing.T) {
	// The exact string internal/loadsignal's RefusalNone.String() returns.
	if utilizationObserved != "observed" {
		t.Fatalf("utilizationObserved = %q; internal/loadsignal spells it %q, and the "+
			"detail-gated occupancy header is gated on this value matching",
			utilizationObserved, "observed")
	}
	for _, name := range []string{
		HeaderUtilizationMultiplier, HeaderUtilizationCeiling,
		HeaderUtilizationSource, HeaderUtilization,
	} {
		if !strings.HasPrefix(name, "X-Dorang-") {
			t.Errorf("%q is not in dorang's own header namespace", name)
		}
	}
}
