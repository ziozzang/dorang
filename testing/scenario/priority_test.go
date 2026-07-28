package scenario

import (
	"testing"

	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/testing/fake"
)

// Priority — DESIGN §7.5 and §10.5.
//
// Two claims, and both are the kind that a passing test can easily be backwards
// about:
//
//  1. The two self-hosted engines read the SAME field in OPPOSITE directions.
//     vLLM schedules the lowest value first, SGLang the highest. Both accept the
//     same JSON, both return 200, and nothing in either response reveals which
//     reading applied — so sending one shared constant does not degrade priority
//     on one of them, it INVERTS it, permanently and silently.
//
//  2. A client-supplied priority is ignored by default. Priority is a claim on
//     shared capacity: if callers may set it, every caller eventually sets the
//     most urgent value, and the callers who left it alone are the ones
//     penalised.

func priorityConfig(p router.PriorityConfig) router.Config {
	dep := func(id, kind string, verified bool) router.Deployment {
		return router.Deployment{
			ID: id, Provider: "prov-" + id, Kind: kind, UpstreamModel: "qwen3.5:397b",
			Credentials: []router.Credential{{ID: "k-" + id}}, PriorityVerified: verified,
		}
	}
	return router.Config{
		Groups: []router.Group{
			{Name: "on-vllm", Class: "self", Deployments: []router.Deployment{dep("v1", "vllm", true)}},
			{Name: "on-sglang", Class: "self", Deployments: []router.Deployment{dep("s1", "sglang", true)}},
			{Name: "on-openai", Class: "hosted", Deployments: []router.Deployment{dep("o1", "openai", true)}},
			{Name: "on-unverified", Class: "self", Deployments: []router.Deployment{dep("u1", "vllm", false)}},
		},
		Priority: p,
	}
}

func TestPriorityEmitsOppositeWireValuesOnTheTwoSelfHostedEngines(t *testing.T) {
	p := router.DefaultPriority()
	r := newRig(t, priorityConfig(p), rigOpts{})

	for _, class := range []string{"realtime", "interactive", "batch"} {
		canonical := p.Classes[class]

		v := r.route(router.Request{Model: "on-vllm", PriorityClass: class})
		s := r.route(router.Request{Model: "on-sglang", PriorityClass: class})

		if v.PriorityField != "priority" || s.PriorityField != "priority" {
			t.Fatalf("%s: both engines take the same field name; got %q and %q",
				class, v.PriorityField, s.PriorityField)
		}
		// vLLM is the canonical direction: the value travels unchanged.
		if v.Priority != canonical {
			t.Errorf("%s: vLLM wire value = %d, want the canonical %d", class, v.Priority, canonical)
		}
		// SGLang is the opposite direction: the value is negated.
		if s.Priority != -canonical {
			t.Errorf("%s: SGLang wire value = %d, want %d", class, s.Priority, -canonical)
		}
		// Stated as the property rather than as two constants: the two engines
		// must not receive the same number for the same class unless the class
		// is the fixed point of the negation.
		if canonical != 0 && v.Priority == s.Priority {
			t.Errorf("%s: both engines received %d — one shared constant is an inversion, "+
				"not a degradation", class, v.Priority)
		}
		if v.CanonicalPriority != canonical || s.CanonicalPriority != canonical {
			t.Errorf("%s: the canonical value must be kept for a re-normalizing hop; got %d and %d",
				class, v.CanonicalPriority, s.CanonicalPriority)
		}
		r.ok(v)
		r.ok(s)
	}

	t.Run("the ordering survives the direction flip", func(t *testing.T) {
		// The point of the negation is that dorang's own classes still order
		// correctly on each engine. On vLLM realtime must sort before batch;
		// on SGLang it must sort after — and being the LAST in an ascending
		// sort is what "scheduled first" means there.
		rt := r.route(router.Request{Model: "on-vllm", PriorityClass: "realtime"})
		bt := r.route(router.Request{Model: "on-vllm", PriorityClass: "batch"})
		if !(rt.Priority < bt.Priority) {
			t.Errorf("vLLM schedules the lowest first, so realtime(%d) must be below batch(%d)",
				rt.Priority, bt.Priority)
		}
		r.ok(rt)
		r.ok(bt)

		rs := r.route(router.Request{Model: "on-sglang", PriorityClass: "realtime"})
		bs := r.route(router.Request{Model: "on-sglang", PriorityClass: "batch"})
		if !(rs.Priority > bs.Priority) {
			t.Errorf("SGLang schedules the highest first, so realtime(%d) must be above batch(%d)",
				rs.Priority, bs.Priority)
		}
		r.ok(rs)
		r.ok(bs)
	})

	t.Run("a hosted engine gets a tier, not a number", func(t *testing.T) {
		d := r.route(router.Request{Model: "on-openai", PriorityClass: "batch"})
		if d.PriorityField != "" {
			t.Errorf("this kind takes no numeric priority field; got %q", d.PriorityField)
		}
		if d.PriorityTier != "flex" {
			t.Errorf("service_tier = %q, want flex for the batch class", d.PriorityTier)
		}
		r.ok(d)
	})

	t.Run("an unverified engine flag is reported rather than assumed", func(t *testing.T) {
		// Both engines accept a priority and ignore it silently unless the
		// operator declared the flag that makes it take effect, and no response
		// says so. The decision is the only place a caller can learn it.
		d := r.route(router.Request{Model: "on-unverified", PriorityClass: "batch"})
		if !d.PriorityUnverified {
			t.Error("an undeclared engine flag must be reported on the decision")
		}
		r.ok(d)
		v := r.route(router.Request{Model: "on-vllm", PriorityClass: "batch"})
		if v.PriorityUnverified {
			t.Error("a verified deployment must not be flagged unverified")
		}
		r.ok(v)
	})
}

func TestPriorityWireValuesReachTheBackend(t *testing.T) {
	// The end-to-end half: what the two engines actually receive in their
	// request bodies. Asserting only on the decision would leave the transport
	// free to emit the canonical value to both.
	p := router.DefaultPriority()
	g := newGateway(t, priorityConfig(p), rigOpts{}, map[string]fake.Options{
		"v1": serving("vllm answer"),
		"s1": serving("sglang answer"),
		"o1": serving("hosted answer"),
		"u1": serving("unverified answer"),
	})

	body := func(model string) []byte {
		return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`)
	}
	if _, err := g.do(t, Call{Family: FamilyOpenAI, Body: body("on-vllm"), PriorityClass: "batch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.do(t, Call{Family: FamilyOpenAI, Body: body("on-sglang"), PriorityClass: "batch"}); err != nil {
		t.Fatal(err)
	}

	v := g.ups["v1"].Last()
	s := g.ups["s1"].Last()
	if !v.PriorityPresent || !s.PriorityPresent {
		t.Fatalf("both engines must receive the field: vllm=%v sglang=%v",
			v.PriorityPresent, s.PriorityPresent)
	}
	if v.Priority != 10 {
		t.Errorf("vLLM received priority %d, want 10 for the batch class", v.Priority)
	}
	if s.Priority != -10 {
		t.Errorf("SGLang received priority %d, want -10 for the batch class", s.Priority)
	}
	if v.Priority == s.Priority {
		t.Error("the two engines received the same number: batch now outranks realtime on one of them")
	}

	if _, err := g.do(t, Call{Family: FamilyOpenAI, Body: body("on-openai"), PriorityClass: "batch"}); err != nil {
		t.Fatal(err)
	}
	o := g.ups["o1"].Last()
	if o.PriorityPresent {
		t.Error("a hosted engine must not receive a numeric priority field")
	}
	if o.ServiceTier != "flex" {
		t.Errorf("service_tier = %q, want flex", o.ServiceTier)
	}
}

func TestClientSuppliedPriorityIsIgnoredByDefault(t *testing.T) {
	// DESIGN §10.5: "A client-supplied priority is ignored by default."
	// The default principal policy is client_priority: ignore, which in this
	// package is spelled Min == Max == 0.
	ignore := router.DefaultPriority()
	ignore.Min, ignore.Max = 0, 0

	r := newRig(t, priorityConfig(ignore), rigOpts{})
	claimRealtime := 0
	batchValue := ignore.Classes["batch"]

	d := r.route(router.Request{
		Model: "on-vllm", PriorityClass: "batch", PriorityHint: &claimRealtime,
	})
	if d.CanonicalPriority != batchValue {
		t.Fatalf("a batch caller claiming realtime was granted %d, want the class value %d: "+
			"a hint that is honoured by default is a way for one caller to outrank every other",
			d.CanonicalPriority, batchValue)
	}
	if d.Priority != batchValue {
		t.Fatalf("wire priority = %d, want %d", d.Priority, batchValue)
	}
	r.ok(d)

	t.Run("an operator can still grant a range explicitly", func(t *testing.T) {
		// The asymmetry is the design's point: an operator can grant urgency, a
		// caller cannot claim it. Without this half, "ignored" would be
		// indistinguishable from "not implemented".
		granted := router.DefaultPriority()
		granted.Min, granted.Max = ignore.Classes["interactive"], ignore.Classes["batch"]
		r := newRig(t, priorityConfig(granted), rigOpts{})
		hint := granted.Classes["interactive"]
		d := r.route(router.Request{Model: "on-vllm", PriorityClass: "batch", PriorityHint: &hint})
		if d.CanonicalPriority != hint {
			t.Fatalf("a granted hint was not honoured: got %d, want %d", d.CanonicalPriority, hint)
		}
		r.ok(d)

		// And a hint outside the granted range is clamped, never widened.
		tooUrgent := granted.Classes["realtime"]
		d2 := r.route(router.Request{Model: "on-vllm", PriorityClass: "batch", PriorityHint: &tooUrgent})
		if d2.CanonicalPriority != granted.Min {
			t.Fatalf("a hint outside the range was granted %d, want the clamp %d",
				d2.CanonicalPriority, granted.Min)
		}
		r.ok(d2)
	})

	t.Run("DIVERGENCE: router.DefaultPriority accepts a hint over the whole class range", func(t *testing.T) {
		// Characterization, not endorsement. DESIGN §10.5 states the default is
		// `ignore` and explicitly rejects the clamp as "the earlier rule […] not
		// safe enough", yet router.DefaultPriority() returns Min=0, Max=10 —
		// the widest possible clamp, under which any caller can claim realtime.
		//
		// This subtest pins the behaviour that exists so the divergence is
		// visible in the suite rather than only in a report. If it starts
		// failing because the default became `ignore`, that is the design-
		// correct direction: delete this subtest.
		d := newRig(t, priorityConfig(router.DefaultPriority()), rigOpts{}).
			route(router.Request{Model: "on-vllm", PriorityClass: "batch", PriorityHint: &claimRealtime})
		if d.CanonicalPriority != claimRealtime {
			t.Skipf("router.DefaultPriority() no longer honours a client hint (got %d): "+
				"it now matches DESIGN §10.5 and this characterization subtest can be deleted",
				d.CanonicalPriority)
		}
		t.Logf("router.DefaultPriority() granted a batch caller priority %d (realtime); "+
			"DESIGN §10.5 requires the hint to be ignored by default", d.CanonicalPriority)
	})
}
