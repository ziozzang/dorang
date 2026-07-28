package scenario

import (
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/testing/fake"
)

// DESIGN §14 scenarios 9 and 10 — cache affinity — and §7.4a2's three
// credential-affinity scenarios, which §14 does not number but which the design
// names as scenario tests in their own right.

// twoPricedDeployments makes the second deployment visibly cheaper, so that
// whenever affinity does NOT apply the router prefers d-plan. Every test below
// forces the first decision onto d-api and then watches whether affinity holds
// it there — which is the only way "the same target" is distinguishable from
// "the target it would have picked anyway".
const affinityPricing = `
currency: USD
rules:
  - { id: plan, match: { deployment: d-plan }, unit: per_1m_tokens, input: "0.10" }
  - { id: api,  match: { deployment: d-api },  unit: per_1m_tokens, input: "9.00" }
`

func affinityConfig() router.Config {
	return router.Config{
		Groups: []router.Group{{
			Name: "m", Class: "c",
			Strategy: []router.Strategy{
				router.StrategyPrefixSticky, router.StrategySticky, router.StrategyLowestCost,
			},
			Deployments: []router.Deployment{
				{ID: "d-api", Provider: "prov-api", Kind: "openai", UpstreamModel: "gemma4:31b",
					Credentials: []router.Credential{{ID: "api-1"}}},
				{ID: "d-plan", Provider: "prov-plan", Kind: "openai", UpstreamModel: "gemma4:31b",
					Credentials: []router.Credential{{ID: "plan-1"}}},
			},
		}},
		Sticky:   router.StickyConfig{Enabled: true, TTL: time.Hour},
		Prefix:   router.PrefixConfig{Enabled: true},
		Fallback: router.FallbackConfig{On: router.DefaultChains(), MaxHops: 3},
	}
}

// conversation renders a request body from an ordered list of user turns. The
// bytes are what the prefix chain hashes, so the ORDER of these turns is the
// thing under test.
func conversation(model string, turns ...string) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"` + model + `","messages":[`)
	b.WriteString(`{"role":"system","content":"you are a careful assistant"}`)
	for _, turn := range turns {
		b.WriteString(`,{"role":"user","content":"` + turn + `"}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// -----------------------------------------------------------------------------
// §14.9 — same prefix → same target; reordered messages → free to choose
//         differently
// -----------------------------------------------------------------------------

func TestScenario09_SamePrefixSameTargetReorderedIsFree(t *testing.T) {
	g := newGateway(t, affinityConfig(), rigOpts{
		prefix: true, pricing: affinityPricing,
	}, map[string]fake.Options{
		"d-api":  serving("from api"),
		"d-plan": serving("from plan"),
	})

	body := conversation("m", "what is the capital of Korea", "and its population")
	call := Call{Family: FamilyOpenAI, Body: body, Prefix: true}

	// Force the first decision onto the dearer deployment, so that "stayed on
	// d-api" and "was re-ranked" have different answers.
	g.health.MarkUnavailable("d-plan", 24*time.Hour)
	first, err := g.do(t, call)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	if first.Decision.Deployment != "d-api" {
		t.Fatalf("setup: first request landed on %s", first.Decision.Deployment)
	}
	g.health.MarkHealthy("d-plan")

	// Same bytes, same prefix, same target.
	second, err := g.do(t, call)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	if second.Decision.Deployment != "d-api" {
		t.Fatalf("the same prefix must reach the same target: got %s (reason %q)",
			second.Decision.Deployment, second.Decision.Reason)
	}
	if !strings.HasPrefix(second.Decision.Reason, "prefix_hit") {
		t.Fatalf("reason = %q, want a prefix_hit: without it the target is a coincidence",
			second.Decision.Reason)
	}
	if second.Decision.PrefixDepth == 0 {
		t.Fatal("a prefix hit must report the depth it matched at")
	}

	// Reordered messages: the chain diverges, so nothing holds the request on
	// d-api and the strategy chain is free to choose again — which it does,
	// because d-api is 90× dearer.
	reordered := conversation("m", "and its population", "what is the capital of Korea")
	third, err := g.do(t, Call{Family: FamilyOpenAI, Body: reordered, Prefix: true})
	if err != nil {
		t.Fatalf("reordered request: %v", err)
	}
	if third.Decision.PrefixDepth != 0 {
		t.Fatalf("a reordered conversation matched the original prefix at depth %d: "+
			"the chain is not order-exact", third.Decision.PrefixDepth)
	}
	if third.Decision.Deployment != "d-plan" {
		t.Fatalf("with no prefix hit the strategy chain decides; got %s (reason %q)",
			third.Decision.Deployment, third.Decision.Reason)
	}

	t.Run("the chain is order-exact under permutation, insertion and deletion", func(t *testing.T) {
		// The structural claim behind the routing behaviour, stated directly
		// against the chain rather than inferred from one routing decision.
		// DESIGN §14's property row requires all three to diverge.
		base := prefix.Compute("m", conversation("m", "alpha", "beta", "gamma"), 0)
		cases := map[string][]byte{
			"permutation": conversation("m", "beta", "alpha", "gamma"),
			"insertion":   conversation("m", "alpha", "inserted", "beta", "gamma"),
			"deletion":    conversation("m", "alpha", "gamma"),
		}
		for name, body := range cases {
			got := prefix.Compute("m", body, 0)
			if sameDigests(base, got) {
				t.Errorf("%s produced the same chain as the original: a match would send the "+
					"request to a backend whose cache holds something else", name)
			}
		}

		// The inverse: identical bytes must produce an identical chain, or the
		// three assertions above are satisfied by a chain that never matches
		// anything.
		again := prefix.Compute("m", conversation("m", "alpha", "beta", "gamma"), 0)
		if !sameDigests(base, again) {
			t.Fatal("identical bytes produced different digests; the table can never hit")
		}

		// And a different group must never share an entry with this one.
		other := prefix.Compute("other", conversation("m", "alpha", "beta", "gamma"), 0)
		if sameDigests(base, other) {
			t.Fatal("two groups share a chain: a match no longer implies the same candidate set")
		}
	})
}

func sameDigests(a, b []prefix.Digest) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// §14.10 — sticky TTL elapsed → re-routed
// -----------------------------------------------------------------------------

func TestScenario10_StickyTTLElapsedReRoutes(t *testing.T) {
	r := newRig(t, affinityConfig(), rigOpts{pricing: affinityPricing})
	req := router.Request{Model: "m", Session: "s1", Tenant: "t1", InputTokens: 1_000_000}

	r.health.MarkUnavailable("d-plan", 24*time.Hour)
	d0 := r.route(req)
	if d0.Deployment != "d-api" {
		t.Fatalf("setup: got %s", d0.Deployment)
	}
	r.ok(d0)
	r.health.MarkHealthy("d-plan")

	// Inside the TTL the pin holds, even though d-plan is far cheaper.
	r.clock.advance(50 * time.Minute)
	d1 := r.route(req)
	if d1.Deployment != "d-api" || d1.Reason != router.ReasonSticky {
		t.Fatalf("inside the TTL the pin must hold: got %s / %q", d1.Deployment, d1.Reason)
	}
	r.ok(d1)

	// 61 minutes after CREATION — and only 11 after last use. The TTL is
	// measured from creation because the premise is that the upstream cache is
	// gone by then whether or not the session kept talking; refreshing on use
	// would make the pin permanent for any active conversation.
	r.clock.advance(11 * time.Minute)
	d2 := r.route(req)
	if d2.Deployment != "d-plan" {
		t.Fatalf("an elapsed sticky TTL must re-route: got %s (reason %q)", d2.Deployment, d2.Reason)
	}
	if d2.Reason == router.ReasonSticky {
		t.Fatal("the decision still claims to be sticky after the TTL elapsed")
	}
	r.ok(d2)

	t.Run("inverse: prefix affinity refreshes on use, and that is deliberate", func(t *testing.T) {
		// The opposite clock, on the same schedule. A prefix that keeps being
		// requested keeps the upstream cache warm, so refreshing tracks reality
		// where refreshing a session pin would defeat it. Running the two side
		// by side is what makes the asymmetry a decision rather than an
		// inconsistency.
		r := newRig(t, affinityConfig(), rigOpts{
			pricing: affinityPricing, prefix: true, prefixTL: time.Hour,
		})
		digests := prefix.Compute("m", conversation("m", "hello"), 0)
		req := router.Request{Model: "m", Digests: digests, InputTokens: 1_000_000}

		r.health.MarkUnavailable("d-plan", 24*time.Hour)
		d0 := r.route(req)
		r.ok(d0)
		r.health.MarkHealthy("d-plan")

		r.clock.advance(50 * time.Minute)
		d1 := r.route(req)
		if d1.Deployment != "d-api" {
			t.Fatalf("inside the TTL the prefix entry must hold: got %s", d1.Deployment)
		}
		r.ok(d1) // refreshes from LAST USE

		r.clock.advance(11 * time.Minute) // 61 from creation, 11 from last use
		d2 := r.route(req)
		if d2.Deployment != "d-api" {
			t.Fatalf("prefix affinity expires from last use, so this must still hit: got %s", d2.Deployment)
		}
		r.ok(d2)
	})
}

// -----------------------------------------------------------------------------
// §7.4a2 — credential affinity is a correctness constraint
// -----------------------------------------------------------------------------

// twoAccountsOneProvider is the normal case the design names: several accounts
// on one vendor, each with its own ceiling, and spill configured for the
// unpinned traffic that is most of it.
func twoAccountsOneProvider(limit int) router.Config {
	return router.Config{
		Groups: []router.Group{{Name: "plan", Class: "chat", Deployments: []router.Deployment{{
			ID: "plan-1", Provider: "vendor", Kind: "glm", Family: "openai-chat",
			UpstreamModel: "zai:glm-5.1",
			Credentials: []router.Credential{
				{ID: "acct-1", CapacityGroup: "acct-1", MaxConcurrent: limit},
				{ID: "acct-2", CapacityGroup: "acct-2", MaxConcurrent: limit},
			},
		}}}},
		OnCapacity: capacity.Spill,
		Fallback:   router.FallbackConfig{On: router.DefaultChains(), MaxHops: 3},
	}
}

func TestCredentialAffinityNeverSpillsAStatefulConversation(t *testing.T) {
	r := newRig(t, twoAccountsOneProvider(1), rigOpts{})

	statePin := []router.Pin{{
		Kind: "previous_response_id", Strength: router.Pinned, Credential: "acct-1",
	}}

	// Saturate acct-1 only. acct-2 is wide open, and spill is configured.
	hold := r.route(router.Request{Model: "plan", Pins: statePin})
	if hold.Credential != "acct-1" {
		t.Fatalf("setup: expected acct-1, got %s", hold.Credential)
	}

	e := r.routeErr(router.Request{Model: "plan", Pins: statePin})
	if e.Code != router.CodeCredentialSaturated {
		t.Fatalf("code = %q, want %q", e.Code, router.CodeCredentialSaturated)
	}
	if e.Credential != "acct-1" {
		t.Fatalf("the refusal must name the pinned account: got %q", e.Credential)
	}
	if !e.Terminal() {
		t.Fatal("a pinned request that cannot be served is terminal, not a fallback")
	}
	if e.Pin != "previous_response_id" {
		t.Fatalf("the refusal must name the state that pinned: got %q", e.Pin)
	}

	t.Run("inverse: the identical saturation spills a STATELESS conversation", func(t *testing.T) {
		// Same accounts, same ceiling, same saturation — only the state is
		// gone. If this also failed, the assertion above would be about
		// capacity rather than about the pin.
		r := newRig(t, twoAccountsOneProvider(1), rigOpts{})
		first := r.route(router.Request{Model: "plan"})
		if first.Credential != "acct-1" {
			t.Fatalf("setup: got %s", first.Credential)
		}
		second := r.route(router.Request{Model: "plan"})
		if second.Credential != "acct-2" {
			t.Fatalf("an unpinned request must spill to the free account: got %s", second.Credential)
		}
		r.ok(first)
		r.ok(second)
	})

	r.ok(hold)
}

func TestCredentialAffinityNamesTheAccountAndResetWhenQuotaIsExhausted(t *testing.T) {
	// A real meter rather than a stubbed decision: the reset instant the
	// refusal reports has to come from a window that genuinely exhausted, or
	// the scenario proves only that a struct field is copied.
	clk := newClock()
	rule := quota.Rule{
		Window: mustWindow(t, "rolling:1h"), Metric: quota.MetricRequests,
		Limit: 2, OnExhaust: quota.Cooldown,
	}
	meters := router.Meters{
		"acct-1": mustMeter(t, clk.now, rule),
		"acct-2": mustMeter(t, clk.now, rule),
	}
	r := newRig(t, twoAccountsOneProvider(4), rigOpts{clock: clk, quota: meters})

	for range 2 {
		meters["acct-1"].Record(clk.now(), quota.Usage{Requests: 1})
	}
	want := meters["acct-1"].Check(clk.now())
	if want.Allow {
		t.Fatal("setup: acct-1 should be exhausted")
	}

	e := r.routeErr(router.Request{Model: "plan", Pins: []router.Pin{{
		Kind: "reasoning.encrypted_content", Strength: router.Pinned, Credential: "acct-1",
	}}})

	if e.Code != router.CodeCredentialExhausted {
		t.Fatalf("code = %q, want %q", e.Code, router.CodeCredentialExhausted)
	}
	if e.Status != 429 {
		t.Fatalf("status = %d, want 429", e.Status)
	}
	if !e.Terminal() {
		t.Fatal("the pinned account is the only account that can serve this conversation")
	}
	if e.Credential != "acct-1" {
		t.Fatalf("the refusal must name WHICH account is exhausted: got %q", e.Credential)
	}
	if e.ResetAt.IsZero() || !e.ResetAt.Equal(want.ResetAt) {
		t.Fatalf("the refusal must carry the real reset instant: want %v, got %v",
			want.ResetAt, e.ResetAt)
	}

	t.Run("inverse: unpinned traffic steps aside instead of failing", func(t *testing.T) {
		// Silently continuing elsewhere trades a visible limit for an invisible
		// corruption — but only when there is state to corrupt. Without a pin,
		// stepping aside is exactly right.
		d := r.route(router.Request{Model: "plan"})
		if d.Credential != "acct-2" {
			t.Fatalf("an unpinned request must move to the healthy account: got %s", d.Credential)
		}
		r.ok(d)
	})
}
