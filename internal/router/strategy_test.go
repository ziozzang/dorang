package router

import (
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/health"
)

// planAndTokens is the shape REVIEW finding 9 is about: a subscription plan
// whose per-token price is cheap, beside a pay-as-you-go deployment whose
// per-token price is dearer. The plan's TOTAL looks far more expensive because
// the sunk plan cost is amortized onto it; its MARGINAL cost is what routing
// must read.
const planAndTokens = `
currency: USD
rules:
  - { id: plan-tokens, match: { deployment: d-plan }, unit: per_1m_tokens, input: "0.10" }
  - { id: api-tokens,  match: { deployment: d-api },  unit: per_1m_tokens, input: "0.20" }
  - id: plan-subscription
    class: fixed_subscription
    match: { deployment: d-plan }
    amount_per_period: "200.00"
    period: monthly
`

func costGroup(order ...string) Config {
	g := Group{Name: "m", Class: "c", Strategy: []Strategy{StrategyLowestCost}}
	for _, id := range order {
		g.Deployments = append(g.Deployments,
			dep(id, "prov-"+id, "openai", "gemma4:31b"))
	}
	return Config{Groups: []Group{g}}
}

// TestCostOrderingUsesMarginalNotTotal is DESIGN §8.1's rule: routing uses
// marginal_usage alone, because a sunk subscription cost must not make a
// saturated plan look cheap. The plan deployment is deliberately placed SECOND
// in configuration order, so winning cannot be an artefact of the tie-break.
func TestCostOrderingUsesMarginalNotTotal(t *testing.T) {
	h := newHarness(t, costGroup("d-api", "d-plan"), harnessOpts{pricing: planAndTokens})

	d := h.route(Request{Model: "m", InputTokens: 1_000_000})
	if d.Deployment != "d-plan" {
		t.Fatalf("lowest_cost must pick the cheaper MARGINAL cost: got %s", d.Deployment)
	}
	if d.Reason != string(StrategyLowestCost) {
		t.Fatalf("want reason %q, got %q", StrategyLowestCost, d.Reason)
	}
	if d.Estimate.SubscriptionNano == 0 {
		t.Fatal("the test is not exercising the hazard: the plan carries no amortized subscription")
	}
	if d.Estimate.TotalNano <= d.Estimate.MarginalNano {
		t.Fatal("the test is not exercising the hazard: the plan's total should look expensive")
	}
	h.ok(d)
}

// TestUnpricedCandidateHasNoOpinionRatherThanCostingZero is the routing
// sibling of §8.3's "an unpriced model costs zero AND increments a counter".
// Zero is the right accounting answer and the wrong ranking answer: read as a
// price it makes the deployment nobody has priced the cheapest one in every
// group, forever, and the misrouting is silent.
//
// The unpriced deployment is placed LAST so that the naive behaviour and the
// correct one produce different winners.
func TestUnpricedCandidateHasNoOpinionRatherThanCostingZero(t *testing.T) {
	h := newHarness(t, costGroup("d-plan", "d-api", "d-unpriced"),
		harnessOpts{pricing: planAndTokens})

	d := h.route(Request{Model: "m", InputTokens: 1_000_000})
	if d.Deployment == "d-unpriced" {
		t.Fatal("an unpriced deployment won lowest_cost on a price of zero")
	}
	if d.Deployment != "d-plan" {
		t.Fatalf("want d-plan, got %s", d.Deployment)
	}
	if h.r.Unpriced() == 0 {
		t.Fatal("§8.3 requires an unpriced model to increment a counter, not pass silently")
	}
	h.ok(d)
}

// TestLowestLatencyIgnoresUnprovenBackends: internal/health returns zero for a
// deployment with no sample, and its own doc says callers must treat that as
// "no opinion" rather than "fastest". An unproven backend must not win on
// ignorance.
func TestLowestLatencyIgnoresUnprovenBackends(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Strategy: []Strategy{StrategyLowestLatency},
		Deployments: []Deployment{
			dep("slow", "p1", "openai", "m"),
			dep("unproven", "p2", "openai", "m"),
		}}}}
	h := newHarness(t, cfg, harnessOpts{})

	// slow has a real, bad sample. unproven has none at all.
	d0 := h.route(Request{Model: "m"})
	h.r.Report(d0, Outcome{TTFT: 900 * time.Millisecond, Total: time.Second})

	d := h.route(Request{Model: "m"})
	if d.Deployment != "slow" {
		t.Fatalf("a deployment with no latency sample must not beat a measured one on silence; got %s",
			d.Deployment)
	}
	h.ok(d)

	// Once it HAS a sample, and a better one, it wins on evidence.
	h.health.Report("unproven", health.Outcome{TTFT: 10 * time.Millisecond, Total: 20 * time.Millisecond})
	d2 := h.route(Request{Model: "m"})
	if d2.Deployment != "unproven" {
		t.Fatalf("a measured, faster deployment must win: got %s", d2.Deployment)
	}
	h.ok(d2)
}

// TestHighestTPSIgnoresUnprovenBackends is the same rule on the generation-rate
// axis, which is a different question from latency: a backend can answer fast
// and generate slowly.
//
// The naive failure is the MIRROR of lowest_latency's. Reading a missing sample
// as zero makes an unproven backend the fastest thing in the group on a
// lower-is-better axis, and the slowest on a higher-is-better one. The second
// is not obviously wrong so it survives review, and it is just as broken: the
// unproven deployment is ranked last forever, never receives a request, and so
// never acquires the sample that would let it compete. Silence must mean
// silence in both directions.
func TestHighestTPSIgnoresUnprovenBackends(t *testing.T) {
	newGroup := func(order ...string) Config {
		g := Group{Name: "m", Strategy: []Strategy{StrategyHighestTPS}}
		for i, id := range order {
			g.Deployments = append(g.Deployments, dep(id, "p"+itoa(i), "openai", "m"))
		}
		return Config{Groups: []Group{g}}
	}
	sample := health.Outcome{TTFT: time.Millisecond, Total: 101 * time.Millisecond, OutputTokens: 10}

	t.Run("an unproven backend is not starved", func(t *testing.T) {
		h := newHarness(t, newGroup("unproven", "measured"), harnessOpts{})
		h.health.Report("measured", sample)
		d := h.route(Request{Model: "m"})
		if d.Deployment != "unproven" {
			t.Fatalf("an unmeasured generation rate is silence, not zero: the config order "+
				"must decide, got %s", d.Deployment)
		}
		h.ok(d)
	})

	t.Run("an unproven backend does not win either", func(t *testing.T) {
		h := newHarness(t, newGroup("measured", "unproven"), harnessOpts{})
		h.health.Report("measured", sample)
		d := h.route(Request{Model: "m"})
		if d.Deployment != "measured" {
			t.Fatalf("silence must not outrank a measured rate: got %s", d.Deployment)
		}
		h.ok(d)
	})
}

// TestLowestLatencyDoesNotStarveAnUnprovenBackend is the other half of the
// mirror: with the unproven backend first in configuration order, silence must
// leave it there rather than sinking it behind the measured one.
func TestLowestLatencyDoesNotStarveAnUnprovenBackend(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Strategy: []Strategy{StrategyLowestLatency},
		Deployments: []Deployment{
			dep("unproven", "p1", "openai", "m"),
			dep("measured", "p2", "openai", "m"),
		}}}}
	h := newHarness(t, cfg, harnessOpts{})
	h.health.Report("measured", health.Outcome{TTFT: 5 * time.Millisecond, Total: 10 * time.Millisecond})

	d := h.route(Request{Model: "m"})
	if d.Deployment != "unproven" {
		t.Fatalf("silence must leave the config order alone: got %s", d.Deployment)
	}
	h.ok(d)
}

// TestLeastBusyIgnoresUncountedAxes is the same rule for occupancy. An axis
// with no configured ceiling is not counted at all by internal/capacity, so it
// reports "unknown" — and reading unknown as zero makes every unlimited
// deployment permanently the idlest thing in the group.
func TestLeastBusyIgnoresUncountedAxes(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Strategy: []Strategy{StrategyLeastBusy},
		Deployments: []Deployment{
			{ID: "counted", Provider: "p1", Kind: "openai", UpstreamModel: "gemma4:31b",
				Credentials: []Credential{{ID: "k1"}}},
			{ID: "uncounted", Provider: "p2", Kind: "openai", UpstreamModel: "gemma4:31b",
				Credentials: []Credential{{ID: "k2"}}},
		}}}}
	cc := capacity.Config{
		SweepInterval: -1,
		Models:        []capacity.ModelLimit{{Provider: "p1", Model: "gemma4:31b", Max: 8}},
	}
	h := newHarness(t, cfg, harnessOpts{capacity: cc})

	// Put three requests on the counted deployment. It is genuinely busier than
	// the uncounted one — but the uncounted one is not known to be idle, it is
	// merely unmeasured, and silence must not beat measurement.
	var held []*Decision
	for i := 0; i < 3; i++ {
		d := h.route(Request{Model: "m"})
		if d.Deployment != "counted" {
			t.Fatalf("setup: expected counted, got %s", d.Deployment)
		}
		held = append(held, d)
	}
	d := h.route(Request{Model: "m"})
	if d.Deployment != "counted" {
		t.Fatalf("an uncounted axis is unknown, not idle; got %s", d.Deployment)
	}
	held = append(held, d)
	for _, x := range held {
		h.ok(x)
	}
}

// TestLeastBusyPrefersTheIdlerWhenBothAreCounted is the control: once both
// axes are counted, the strategy has a real opinion and acts on it.
func TestLeastBusyPrefersTheIdlerWhenBothAreCounted(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Strategy: []Strategy{StrategyLeastBusy},
		Deployments: []Deployment{
			{ID: "busy", Provider: "p1", Kind: "openai", UpstreamModel: "a",
				Credentials: []Credential{{ID: "k1"}}},
			{ID: "idle", Provider: "p2", Kind: "openai", UpstreamModel: "b",
				Credentials: []Credential{{ID: "k2"}}},
		}}}}
	cc := capacity.Config{SweepInterval: -1, Models: []capacity.ModelLimit{
		{Provider: "p1", Model: "a", Max: 8}, {Provider: "p2", Model: "b", Max: 8},
	}}
	h := newHarness(t, cfg, harnessOpts{capacity: cc})

	first := h.route(Request{Model: "m"}) // ties, config order -> busy
	if first.Deployment != "busy" {
		t.Fatalf("setup: got %s", first.Deployment)
	}
	second := h.route(Request{Model: "m"})
	if second.Deployment != "idle" {
		t.Fatalf("least_busy must move to the idler deployment: got %s", second.Deployment)
	}
	h.ok(first)
	h.ok(second)
}

// TestStrategyChainIsEvaluatedInOrder is DESIGN §7.3's composition rule:
// [prefix_sticky, lowest_cost, least_busy] is a tie-break chain, and the first
// strategy with an opinion decides.
func TestStrategyChainIsEvaluatedInOrder(t *testing.T) {
	cfg := costGroup("d-api", "d-plan")
	cfg.Groups[0].Strategy = []Strategy{StrategyPrefixSticky, StrategyLowestCost, StrategyLeastBusy}
	h := newHarness(t, cfg, harnessOpts{pricing: planAndTokens, prefixOn: true})

	// No prefix hit: the second link decides.
	d := h.route(Request{Model: "m", InputTokens: 1_000_000})
	if d.Deployment != "d-plan" || d.Reason != string(StrategyLowestCost) {
		t.Fatalf("with no prefix hit lowest_cost decides: got %s / %s", d.Deployment, d.Reason)
	}
	h.ok(d)

	// Now teach the prefix table that the DEARER deployment holds this
	// conversation. The first link must override the second.
	digests := prefixFor("m", "the quick brown fox")
	h.prefix.Record(digests, h.interner.ID("d-api"))

	d2 := h.route(Request{Model: "m", InputTokens: 1_000_000, Digests: digests})
	if d2.Deployment != "d-api" {
		t.Fatalf("prefix_sticky comes first in the chain and must win: got %s", d2.Deployment)
	}
	if d2.Reason != prefixHitReason(d2.PrefixDepth) {
		t.Fatalf("want a prefix_hit reason, got %q", d2.Reason)
	}
	h.ok(d2)
}

// TestRoundRobinRotatesAndRespectsWeight checks the weighted rotation is a
// rotation rather than a constant, which a naive smooth-WRR implementation that
// commits every step of the ordering silently is not.
func TestRoundRobinRotatesAndRespectsWeight(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Strategy: []Strategy{StrategyRoundRobin},
		Deployments: []Deployment{
			{ID: "heavy", Provider: "p1", Kind: "openai", UpstreamModel: "m", Weight: 3,
				Credentials: []Credential{{ID: "k1"}}},
			{ID: "light", Provider: "p2", Kind: "openai", UpstreamModel: "m", Weight: 1,
				Credentials: []Credential{{ID: "k2"}}},
		}}}}
	h := newHarness(t, cfg, harnessOpts{})

	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		d := h.route(Request{Model: "m"})
		counts[d.Deployment]++
		h.ok(d)
	}
	if counts["light"] == 0 {
		t.Fatal("round_robin never rotated off the first candidate")
	}
	if counts["heavy"] <= counts["light"] {
		t.Fatalf("weight 3 must draw more traffic than weight 1: %v", counts)
	}
	if counts["heavy"] != 30 || counts["light"] != 10 {
		t.Fatalf("weighted round-robin should be exact over 40 requests: %v", counts)
	}
}

// TestWeightedRandomIsDeterministicUnderAnInjectedSource keeps the strategy
// testable: the design permits randomness, not unreproducibility.
func TestWeightedRandomIsDeterministicUnderAnInjectedSource(t *testing.T) {
	seq := []float64{0.9, 0.1, 0.9, 0.1}
	i := 0
	cfg := Config{
		Groups: []Group{{Name: "m", Strategy: []Strategy{StrategyWeightedRandom},
			Deployments: []Deployment{
				dep("a", "p1", "openai", "m"),
				dep("b", "p2", "openai", "m"),
			}}},
		Rand: func() float64 { v := seq[i%len(seq)]; i++; return v },
	}
	h := newHarness(t, cfg, harnessOpts{})
	d := h.route(Request{Model: "m"})
	// -ln(0.9) < -ln(0.1), so a sorts first.
	if d.Deployment != "a" {
		t.Fatalf("the injected source must decide: got %s", d.Deployment)
	}
	h.ok(d)
}

// TestModelNamesAreOpaque is DESIGN §2.1 frozen as a test. Real names use ':'
// three different ways, and a gateway that splits on it in one path and not
// another makes identity, routing, aliasing and prefix affinity all
// non-deterministic for the same model.
func TestModelNamesAreOpaque(t *testing.T) {
	names := []string{"gemma4:31b", "zai:glm-5.1", "deepseek-v4-flash:cloud", "qwen3.5:397b"}
	var groups []Group
	aliases := map[string]string{}
	for i, n := range names {
		id := "d" + itoa(i)
		groups = append(groups, Group{Name: n, Deployments: []Deployment{
			dep(id, "p"+itoa(i), "openai", n+":upstream"),
		}})
		aliases["alias:"+n] = n
	}
	h := newHarness(t, Config{Groups: groups, Aliases: aliases}, harnessOpts{})

	for _, n := range names {
		d := h.route(Request{Model: n})
		if d.Group != n {
			t.Fatalf("group name was not carried whole: want %q, got %q", n, d.Group)
		}
		if d.UpstreamModel != n+":upstream" {
			t.Fatalf("upstream model was not carried byte-identical: want %q, got %q",
				n+":upstream", d.UpstreamModel)
		}
		h.ok(d)

		// The alias resolves to the same group, and the alias's own colons are
		// equally untouched.
		da := h.route(Request{Model: "alias:" + n})
		if da.Group != n {
			t.Fatalf("alias %q resolved to %q, want %q", "alias:"+n, da.Group, n)
		}
		h.ok(da)
	}
}
