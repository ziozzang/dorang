package server

import (
	"context"
	"io"
	"net/http"
	"testing"
)

// unpricedDispatcher answers normally and reports a routing decision, but no
// price: Priced stays false, which is what every model in a deployment with no
// `pricing:` block looks like.
func unpricedDispatcher() Dispatcher {
	return DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
		rq.Result.Provider = "prov-1"
		rq.Result.Credential = "cred-1"
		rq.Result.Deployment = "dep-1"
		rq.Result.UpstreamModel = "upstream-" + rq.Model
		rq.Result.Attempt = 1
		rq.Result.Tokens = Usage{Input: 11, Output: 22, Total: 33}
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"ok":true}`)
		return err
	})
}

// TestUnpricedRequestStillMirrorsTheCostHeaderAsZero is COMPATIBILITY §7.7a's
// own justification applied to the case it did not cover.
//
// The mirror exists because "an exporter reading x-litellm-response-cost does
// not error when the header stops arriving, it reports zero". Gating the mirror
// on Priced reproduced exactly that: a deployment with no `pricing:` block — the
// state of every model in one — emitted the call id, the model id and the
// duration, and no cost header at all. An exporter could not tell "no price
// configured" from "the gateway did not answer", and neither could an operator.
//
// dorang's OWN header keeps the distinction, and the pair is what carries it:
// legacy header 0 with x-dorang-cost-usd absent means unpriced; both present
// means priced, and 0 there means genuinely free.
func TestUnpricedRequestStillMirrorsTheCostHeaderAsZero(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.LegacyHeaders = true
		o.Dispatcher = unpricedDispatcher()
	})
	h := do(s, post("/v1/chat/completions", `{"model":"model-x"}`)).Header()

	got, ok := h[http.CanonicalHeaderKey(LegacyHeaderResponseCost)]
	if !ok {
		t.Fatalf("%s absent on an unpriced request: an exporter built on it reports "+
			"zero silently, which is the failure the mirror exists to prevent",
			LegacyHeaderResponseCost)
	}
	if len(got) != 1 || !isZeroUSD(got[0]) {
		t.Errorf("%s = %q, want an explicit zero", LegacyHeaderResponseCost, got)
	}
	if v := h.Get(HeaderCostUSD); v != "" {
		t.Errorf("%s = %q on an unpriced request: dorang's own name must stay absent, "+
			"because it is what distinguishes unpriced from free", HeaderCostUSD, v)
	}
}

// TestPricedRequestMirrorsTheCostHeaderExactly keeps the mirror a mirror: when
// there IS a price, the two names carry one answer.
func TestPricedRequestMirrorsTheCostHeaderExactly(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.LegacyHeaders = true })
	h := do(s, post("/v1/chat/completions", `{"model":"model-x"}`)).Header()

	native := h.Get(HeaderCostUSD)
	if native == "" {
		t.Fatal("the fixture reported no cost, so the mirror proves nothing")
	}
	if got := h.Get(LegacyHeaderResponseCost); got != native {
		t.Errorf("%s = %q, want %q (the value of %s)",
			LegacyHeaderResponseCost, got, native, HeaderCostUSD)
	}
}

// deferredCostDispatcher answers with an event stream and leaves the cost unset,
// which is what the real dispatcher does: a stream is settled after its last
// frame, and these headers go out before its first.
func deferredCostDispatcher() Dispatcher {
	return DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
		rq.Result.Provider = "prov-1"
		rq.Result.Deployment = "dep-1"
		rq.Result.UpstreamModel = "upstream-" + rq.Model
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: {}\n\ndata: [DONE]\n\n"); err != nil {
			return err
		}
		// Settlement happens here, after the client already has the headers.
		rq.Result.Tokens = Usage{Input: 120, Output: 15, Total: 135}
		rq.Result.CostNanoUSD = 126_600
		rq.Result.Priced = true
		return nil
	})
}

// TestStreamedRequestDoesNotPublishACostOfZero is the regression the previous fix
// introduced. Before it the legacy header was absent on a stream and claimed
// nothing; after it the header was present and said 0 — and §7.7a reads
// "legacy 0 + x-dorang-cost-usd absent" as NOT PRICED. Every streamed request
// therefore told a cost exporter it had no price, while its ledger row carried
// one, and in an agent deployment every turn streams.
//
// Absent and honest beats present and wrong. The number for a stream is on
// §10.4's usage event and in the ledger, joined by x-dorang-request-id.
func TestStreamedRequestDoesNotPublishACostOfZero(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.LegacyHeaders = true
		o.Dispatcher = deferredCostDispatcher()
	})
	rec := do(s, post("/v1/chat/completions", `{"model":"model-x","stream":true}`))
	h := rec.Header()

	if got := h.Get(LegacyHeaderResponseCost); got != "" {
		if isZeroUSD(got) {
			t.Fatalf("%s = %q on a streamed request whose settled cost was %d nano: "+
				"the header is written before the stream and the cost is known after it, "+
				"so this is a zero nobody measured — and §7.7a reads it, beside an absent "+
				"%s, as \"this model has no price rule\"",
				LegacyHeaderResponseCost, got, 126_600, HeaderCostUSD)
		}
		t.Fatalf("%s = %q, want absent: nothing can know the cost at header time",
			LegacyHeaderResponseCost, got)
	}
	// The other mirrors are unaffected: they carry values that ARE known before
	// the first frame, and dropping them would lose a cutover its join key.
	if h.Get(LegacyHeaderCallID) == "" || h.Get(LegacyHeaderModelID) == "" {
		t.Errorf("the identifying mirrors went missing with the cost one: %v", h)
	}
}

// TestTheDiscriminatorStillDistinguishesUnpricedFromPriced pins §7.7a's table as
// a table: the three states a reader must be able to tell apart, asserted
// together so that fixing one cannot quietly collapse another into it.
func TestTheDiscriminatorStillDistinguishesUnpricedFromPriced(t *testing.T) {
	cases := []struct {
		name        string
		dispatcher  Dispatcher
		body        string
		wantLegacy  string // "" means the header must be absent
		wantNative  string
		explanation string
	}{
		{
			name: "no price rule matched", dispatcher: unpricedDispatcher(),
			body: `{"model":"model-x"}`, wantLegacy: "zero", wantNative: "absent",
			explanation: "the cost is unknown, not zero",
		},
		{
			name: "priced", dispatcher: nil, // the fixture dispatcher prices
			body: `{"model":"model-x"}`, wantLegacy: "value", wantNative: "value",
			explanation: "both names carry one answer",
		},
		{
			name: "streamed, not yet priced", dispatcher: deferredCostDispatcher(),
			body: `{"model":"model-x","stream":true}`, wantLegacy: "absent", wantNative: "absent",
			explanation: "the cost is not decidable at header time; read the usage event or the ledger",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t, func(o *Options) {
				o.LegacyHeaders = true
				if c.dispatcher != nil {
					o.Dispatcher = c.dispatcher
				}
			})
			h := do(s, post("/v1/chat/completions", c.body)).Header()
			legacy, native := h.Get(LegacyHeaderResponseCost), h.Get(HeaderCostUSD)

			check := func(name, want, got string) {
				switch want {
				case "absent":
					if got != "" {
						t.Errorf("%s = %q, want absent (%s)", name, got, c.explanation)
					}
				case "zero":
					if got == "" || !isZeroUSD(got) {
						t.Errorf("%s = %q, want an explicit zero (%s)", name, got, c.explanation)
					}
				case "value":
					if got == "" || isZeroUSD(got) {
						t.Errorf("%s = %q, want a real amount (%s)", name, got, c.explanation)
					}
				}
			}
			check(LegacyHeaderResponseCost, c.wantLegacy, legacy)
			check(HeaderCostUSD, c.wantNative, native)
			if c.wantLegacy == "value" && legacy != native {
				t.Errorf("%s = %q but %s = %q: the mirror is not a mirror",
					LegacyHeaderResponseCost, legacy, HeaderCostUSD, native)
			}
		})
	}
}

// TestNotionalHeaderIsAbsentWhenThereIsNoListRate is DESIGN §8.5 rule 5 on the
// header surface: a missing list-rate equivalent is REPORTED as missing, never
// as zero, because "silently returning zero would make a subscription look
// infinitely efficient". The header was gated on Priced rather than on
// NotionalPriced, so a billed request with no notional_rate rule — the normal
// case on a subscription — published a list-rate of $0.00, while internal/app's
// metric for the same value already honoured the flag.
func TestNotionalHeaderIsAbsentWhenThereIsNoListRate(t *testing.T) {
	s := newTestServer(t, nil) // the fixture prices, and sets no notional
	r := post("/v1/chat/completions", `{"model":"model-x"}`)
	r.Header.Set(HeaderDetail, "full")
	h := do(s, r).Header()

	if h.Get(HeaderCostUSD) == "" {
		t.Fatal("the fixture reported no cost, so this proves nothing about the notional gate")
	}
	if v := h.Get(HeaderNotionalUSD); v != "" {
		t.Errorf("%s = %q with no notional rate configured, want absent", HeaderNotionalUSD, v)
	}
}

// isZeroUSD reports whether a rendered USD amount is zero, whatever precision it
// was written at.
func isZeroUSD(s string) bool {
	for _, c := range s {
		if c != '0' && c != '.' && c != '-' && c != '+' {
			return false
		}
	}
	return s != ""
}
