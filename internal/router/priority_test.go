package router

import (
	"testing"
	"time"
)

// TestPriorityDirectionIsOppositeOnTheTwoEngines is the test DESIGN §7.5 asks
// for by name: "a scenario test asserts that the same canonical class produces
// opposite wire values on the two engines — because a test asserting only 'a
// number was sent' would pass while the behaviour is backwards."
//
// The load-bearing assertion is the ORDER, not the value. vLLM schedules the
// lowest number first and SGLang the highest, so realtime must compare BELOW
// batch on vLLM and ABOVE it on SGLang. Sending one shared constant to both
// makes batch outrank realtime on SGLang, which is an inversion rather than a
// degradation, and both engines answer 200 either way.
func TestPriorityDirectionIsOppositeOnTheTwoEngines(t *testing.T) {
	p := DefaultPriority()

	rt := p.Canonical("realtime", nil)
	bt := p.Canonical("batch", nil)
	if rt >= bt {
		t.Fatalf("canonical scale is not lower-is-more-urgent: realtime %d, batch %d", rt, bt)
	}

	vRT, vField, vOK := p.Wire("vllm", rt)
	vBT, _, _ := p.Wire("vllm", bt)
	sRT, sField, sOK := p.Wire("sglang", rt)
	sBT, _, _ := p.Wire("sglang", bt)

	if !vOK || !sOK {
		t.Fatal("both self-hosted engines take a native priority field")
	}
	if vField != "priority" || sField != "priority" {
		t.Fatalf("the two engines use the SAME field name, which is the whole hazard: %q, %q",
			vField, sField)
	}

	if !(vRT < vBT) {
		t.Fatalf("vLLM is ascending: realtime must be the LOWER number, got realtime=%d batch=%d",
			vRT, vBT)
	}
	if !(sRT > sBT) {
		t.Fatalf("SGLang is descending: realtime must be the HIGHER number, got realtime=%d batch=%d",
			sRT, sBT)
	}

	// The direct statement of the defect: the batch class cannot come out the
	// same on both engines, because equal numbers on opposite scales mean
	// opposite scheduling.
	if vBT == sBT {
		t.Fatalf("the same wire value %d reached both engines; one of them is scheduling backwards", vBT)
	}
	if sBT != -bt {
		t.Fatalf("descending emission must negate the canonical value: want %d, got %d", -bt, sBT)
	}
	if vBT != bt {
		t.Fatalf("ascending emission must pass the canonical value through: want %d, got %d", bt, vBT)
	}
}

// TestPriorityBandsAreSpacedForResponsesToolLoops guards VLLM.md §1.2's
// constraint: /v1/responses decrements priority by one after every built-in
// tool round trip, so bands closer than two let turn 2 of a tool loop cross
// into the band above the one the caller was granted.
func TestPriorityBandsAreSpacedForResponsesToolLoops(t *testing.T) {
	p := DefaultPriority()
	vals := []int{p.Canonical("realtime", nil), p.Canonical("interactive", nil), p.Canonical("batch", nil)}
	for i := 1; i < len(vals); i++ {
		if gap := vals[i] - vals[i-1]; gap < 2 {
			t.Fatalf("priority bands %d and %d are %d apart; must be >= 2 (VLLM.md §1.2)",
				vals[i-1], vals[i], gap)
		}
	}
}

// TestPriorityHintIsClampedNotHonoured checks §10.5: a client hint is clamped
// to the principal's permitted range. An unclamped hint is how one caller
// outranks every other one on a shared engine.
func TestPriorityHintIsClampedNotHonoured(t *testing.T) {
	p := DefaultPriority() // Min 0, Max 10
	over := -50
	under := 500
	if got := p.Canonical("batch", &over); got != 0 {
		t.Fatalf("a hint below the permitted range must clamp to Min: got %d", got)
	}
	if got := p.Canonical("batch", &under); got != 10 {
		t.Fatalf("a hint above the permitted range must clamp to Max: got %d", got)
	}

	// A configuration that states no range ignores hints entirely rather than
	// accepting them unbounded.
	noRange := PriorityConfig{Classes: map[string]int{"batch": 10}, Default: "batch"}
	if got := noRange.Canonical("batch", &over); got != 10 {
		t.Fatalf("with no permitted range the class alone decides: got %d", got)
	}
}

// TestPriorityTierIsNeverInvented guards VLLM.md §1.2's warning that vLLM
// accepts service_tier and has zero consumers for it: it must never be used as
// a priority fallback for a kind that did not declare a mapping.
func TestPriorityTierIsNeverInvented(t *testing.T) {
	p := DefaultPriority()
	if tier := p.Tier("openai", "batch"); tier != "flex" {
		t.Fatalf("openai declares a tier mapping: got %q", tier)
	}
	if tier := p.Tier("vllm", "batch"); tier != "" {
		t.Fatalf("vllm declares no tier mapping and must get none: got %q", tier)
	}
	if tier := p.Tier("unknown-kind", "batch"); tier != "" {
		t.Fatalf("an unknown backend receives only the header: got %q", tier)
	}
}

// TestDecisionPriorityIsNormalizedForTheChosenEngine is the same inversion
// asserted through the whole router, because §7.5's hazard is only real once a
// decision carries the number.
func TestDecisionPriorityIsNormalizedForTheChosenEngine(t *testing.T) {
	h := newHarness(t, Config{
		Groups: []Group{
			{Name: "local", Strategy: []Strategy{StrategyPriority}, Deployments: []Deployment{
				{ID: "on-vllm", Provider: "p-vllm", Kind: "vllm", UpstreamModel: "qwen3.5:397b",
					Priority: 0, PriorityVerified: true,
					Credentials: []Credential{{ID: "k1"}}},
				{ID: "on-sglang", Provider: "p-sglang", Kind: "sglang", UpstreamModel: "qwen3.5:397b",
					Priority: 1, PriorityVerified: true,
					Credentials: []Credential{{ID: "k2"}}},
			}},
		},
	}, harnessOpts{})

	// Same canonical class, two engines, opposite wire values.
	dv := h.route(Request{Model: "local", PriorityClass: "batch"})
	if dv.Deployment != "on-vllm" {
		t.Fatalf("expected the vllm deployment first: got %s", dv.Deployment)
	}
	h.ok(dv)

	// Take vLLM out so the sglang deployment is chosen for the identical class.
	h.health.MarkUnavailable("on-vllm", time.Hour)
	ds := h.route(Request{Model: "local", PriorityClass: "batch"})
	if ds.Deployment != "on-sglang" {
		t.Fatalf("expected the sglang deployment: got %s", ds.Deployment)
	}
	h.ok(ds)

	if dv.CanonicalPriority != ds.CanonicalPriority {
		t.Fatalf("the canonical class must be identical: %d vs %d",
			dv.CanonicalPriority, ds.CanonicalPriority)
	}
	if dv.Priority == ds.Priority {
		t.Fatalf("the same wire value %d reached both engines; one is scheduling backwards",
			dv.Priority)
	}
	if dv.Priority != dv.CanonicalPriority {
		t.Fatalf("vllm is ascending and takes the canonical value unchanged: %d vs %d",
			dv.Priority, dv.CanonicalPriority)
	}
	if ds.Priority != -ds.CanonicalPriority {
		t.Fatalf("sglang is descending and takes the negation: %d vs %d",
			ds.Priority, -ds.CanonicalPriority)
	}
}

// TestUnverifiedPriorityIsFlagged records VLLM.md §1.2 and SGLANG.md §3.4: both
// engines accept a priority and ignore it silently unless the operator set the
// scheduling flag, and no response reveals that. The decision is the only place
// a caller can learn it.
func TestUnverifiedPriorityIsFlagged(t *testing.T) {
	h := newHarness(t, Config{
		Groups: []Group{{Name: "m", Deployments: []Deployment{
			{ID: "d1", Provider: "p", Kind: "vllm", UpstreamModel: "m",
				Credentials: []Credential{{ID: "k"}}}, // PriorityVerified deliberately false
		}}},
	}, harnessOpts{})

	d := h.route(Request{Model: "m", PriorityClass: "realtime"})
	if !d.PriorityUnverified {
		t.Fatal("a deployment that has not declared the scheduling flag must be flagged unverified")
	}
	h.ok(d)
}
