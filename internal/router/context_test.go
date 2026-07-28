package router

import (
	"strings"
	"testing"
	"time"
)

// The context-window fit check of DESIGN §10.5a, on the two halves that had no
// consumer: the deployment's own output ceiling, and the provenance of the size
// the refusal quotes.

// windowed is one group whose deployments declare both numbers.
func windowed(ctx, out int) Config {
	return Config{
		Groups: []Group{{Name: "m", Class: "c", Deployments: []Deployment{
			func() Deployment {
				d := dep("d1", "p1", "openai", "m-upstream")
				d.ContextWindow, d.MaxOutputTokens = ctx, out
				return d
			}(),
		}}},
		Fallback: FallbackConfig{On: DefaultChains(), MaxHops: 2, Budget: time.Minute},
	}
}

// TestACallerWhoNamedNoCeilingStillReservesOutput.
//
// max_tokens is optional in the OpenAI family, so the common request reserves
// nothing — and the fit check then admits a prompt that fills the whole window
// and overflows on the first generated token. Deployment.MaxOutputTokens is the
// declared ceiling that says what will be generated when nobody asked; before
// this it was compiled onto every deployment and read by nothing on the routing
// path (§17.1).
func TestACallerWhoNamedNoCeilingStillReservesOutput(t *testing.T) {
	h := newHarness(t, windowed(8_000, 2_000), harnessOpts{})

	// 7,900 of an 8,000 window, with 2,000 reserved for the answer: it does not
	// fit, and the caller did not have to say so for that to be true.
	e := h.routeErr(Request{Model: "m", InputTokens: 7_900})
	if e.Code != CodeContextWindow {
		t.Fatalf("code = %q, want %q", e.Code, CodeContextWindow)
	}

	// The same request with room for the reserve is admitted, so the refusal
	// above is about the reserve and not about the window alone.
	d := h.route(Request{Model: "m", InputTokens: 5_000})
	h.ok(d)

	// A caller who named their own ceiling gets exactly it: 7,900 + 50 fits.
	d = h.route(Request{Model: "m", InputTokens: 7_900, MaxOutputTokens: 50})
	h.ok(d)
}

// TestTheOutputReserveNeverEatsTheWindow.
//
// The catalogued ceilings are enormous next to the windows they sit in —
// 200,000 context against 100,000 max output is an ordinary entry. Reserving the
// whole of one for a caller who asked for nothing would refuse a 150,000-token
// prompt that routes and completes today, which is the same defect this check
// exists to fix with its sign flipped.
func TestTheOutputReserveNeverEatsTheWindow(t *testing.T) {
	h := newHarness(t, windowed(200_000, 100_000), harnessOpts{})
	d := h.route(Request{Model: "m", InputTokens: 150_000})
	h.ok(d)

	if got := outputReserve(0, 100_000, 200_000); got != 50_000 {
		t.Errorf("reserve = %d, want a quarter of the window", got)
	}
	if got := outputReserve(0, 4_096, 128_000); got != 4_096 {
		t.Errorf("reserve = %d, want the declared ceiling, which is under the cap", got)
	}
	if got := outputReserve(0, 4_096, 0); got != 4_096 {
		t.Errorf("reserve = %d: an undeclared window does not bound the ceiling", got)
	}
	if got := outputReserve(0, 0, 128_000); got != 0 {
		t.Errorf("reserve = %d, want nothing: neither side declared a ceiling", got)
	}
	if got := outputReserve(77, 100_000, 200_000); got != 77 {
		t.Errorf("reserve = %d, want the caller's own number", got)
	}
}

// TestTheRefusalSaysTheSizeWasEstimated. A client refused for exceeding a
// context window is being refused on the strength of a number. Whether that
// number was measured decides what they should do about it, and dorang has never
// measured one — there is no tokenizer on this path (§15.5).
func TestTheRefusalSaysTheSizeWasEstimated(t *testing.T) {
	h := newHarness(t, windowed(8_000, 1_000), harnessOpts{})
	e := h.routeErr(Request{Model: "m", InputTokens: 900_000,
		InputTokensMethod: "structural"})

	if !e.Estimated {
		t.Error("the refusal presents an estimate as a measurement")
	}
	if e.EstimateMethod != "structural" {
		t.Errorf("EstimateMethod = %q, want the method the frontend used", e.EstimateMethod)
	}
	for _, want := range []string{"estimated", "structural", "901000", "8000"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("the message does not carry %q: %q", want, e.Message)
		}
	}

	// The inverse: a measured size says so. Nothing produces one today, which is
	// why the field exists rather than the assumption.
	e = h.routeErr(Request{Model: "m", InputTokens: 900_000, InputTokensExact: true})
	if e.Estimated {
		t.Error("a measured size was reported as an estimate")
	}
	if !strings.Contains(e.Message, "the request needs ") {
		t.Errorf("a measured size still hedges: %q", e.Message)
	}
}
