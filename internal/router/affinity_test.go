package router

import (
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/prefix"
)

// affinityConfig prices d-plan below d-api, so that whenever affinity does NOT
// apply the router visibly prefers d-plan. Every test below forces the first
// decision onto d-api and then watches whether affinity holds it there.
func affinityConfig() Config {
	return Config{
		Groups: []Group{{Name: "m", Class: "c",
			Strategy: []Strategy{StrategyPrefixSticky, StrategySticky, StrategyLowestCost},
			Deployments: []Deployment{
				dep("d-api", "prov-api", "openai", "gemma4:31b"),
				dep("d-plan", "prov-plan", "openai", "gemma4:31b"),
			}}},
		Sticky:   StickyConfig{Enabled: true, TTL: time.Hour},
		Prefix:   PrefixConfig{Enabled: true},
		Fallback: FallbackConfig{On: DefaultChains(), MaxHops: 3},
	}
}

// TestScenario10StickyTTLExpiresFromCreationNotLastUse is DESIGN §14 scenario
// 10 — "Sticky TTL elapsed → re-routed" — and, at the same time, §7.4a's
// reason for measuring it from creation.
//
// The pin is USED at 50 minutes, well inside the hour. Under last-use expiry
// that would push its deadline out to 110 minutes and the session would stay
// pinned indefinitely as long as it kept talking. The premise of the TTL is
// that the upstream cache is gone after an hour whether or not anyone kept
// asking, so refreshing on use would defeat the point.
func TestScenario10StickyTTLExpiresFromCreationNotLastUse(t *testing.T) {
	h := newHarness(t, affinityConfig(), harnessOpts{pricing: planAndTokens})

	// Force the first decision onto the dearer deployment so that "still
	// pinned" and "re-ranked" have different answers.
	h.health.MarkUnavailable("d-plan", 24*time.Hour)
	d0 := h.route(Request{Model: "m", Session: "s1", Tenant: "t1", InputTokens: 1000})
	if d0.Deployment != "d-api" {
		t.Fatalf("setup: got %s", d0.Deployment)
	}
	h.ok(d0)
	h.health.MarkHealthy("d-plan")

	h.clock.advance(50 * time.Minute)
	d1 := h.route(Request{Model: "m", Session: "s1", Tenant: "t1", InputTokens: 1000})
	if d1.Deployment != "d-api" || d1.Reason != ReasonSticky {
		t.Fatalf("inside the TTL the pin must hold: got %s / %s", d1.Deployment, d1.Reason)
	}
	h.ok(d1)

	// 61 minutes after CREATION, 11 after last use.
	h.clock.advance(11 * time.Minute)
	d2 := h.route(Request{Model: "m", Session: "s1", Tenant: "t1", InputTokens: 1000})
	if d2.Deployment != "d-plan" {
		t.Fatalf("the pin expires from creation, so the session must be re-routed: got %s",
			d2.Deployment)
	}
	if d2.Reason != string(StrategyLowestCost) {
		t.Fatalf("after expiry the strategy chain decides again: got %s", d2.Reason)
	}
	h.ok(d2)
}

// TestPrefixTTLExpiresFromLastUse is the deliberate opposite (§7.4b). A prefix
// that keeps being requested keeps the upstream cache warm, so refreshing on
// use tracks reality rather than defeating it.
//
// The same 50-then-11-minute schedule as the sticky test above, and the
// opposite answer. That is the whole point of running them side by side.
func TestPrefixTTLExpiresFromLastUse(t *testing.T) {
	h := newHarness(t, affinityConfig(),
		harnessOpts{pricing: planAndTokens, prefixOn: true, prefixTL: time.Hour})

	digests := prefixFor("m", "system: you are a helpful assistant\nuser: hello")

	h.health.MarkUnavailable("d-plan", 24*time.Hour)
	d0 := h.route(Request{Model: "m", Digests: digests, InputTokens: 1000})
	if d0.Deployment != "d-api" {
		t.Fatalf("setup: got %s", d0.Deployment)
	}
	h.ok(d0)
	h.health.MarkHealthy("d-plan")

	h.clock.advance(50 * time.Minute)
	d1 := h.route(Request{Model: "m", Digests: digests, InputTokens: 1000})
	if d1.Deployment != "d-api" {
		t.Fatalf("inside the TTL the prefix must hold: got %s", d1.Deployment)
	}
	h.ok(d1)

	h.clock.advance(11 * time.Minute)
	d2 := h.route(Request{Model: "m", Digests: digests, InputTokens: 1000})
	if d2.Deployment != "d-api" {
		t.Fatalf("prefix affinity expires from LAST USE, so 11 minutes after a hit it is "+
			"still live: got %s", d2.Deployment)
	}
	if d2.Reason != prefixHitReason(d2.PrefixDepth) {
		t.Fatalf("want a prefix_hit reason, got %q", d2.Reason)
	}
	h.ok(d2)

	// And it does eventually expire, from the last use rather than the first.
	h.clock.advance(61 * time.Minute)
	d3 := h.route(Request{Model: "m", Digests: digests, InputTokens: 1000})
	if d3.Deployment != "d-plan" {
		t.Fatalf("an hour after the last use the entry is gone: got %s", d3.Deployment)
	}
	h.ok(d3)
}

// TestTheTwoTTLsDisagreeOnPurpose states the asymmetry directly on the two
// stores, so a future change that "harmonizes" them fails here with the reason
// attached rather than somewhere downstream.
func TestTheTwoTTLsDisagreeOnPurpose(t *testing.T) {
	c := newClock()
	ttl := time.Hour

	sticky := newStickyStore(ttl, c.now)
	table := prefix.NewTable(prefix.Options{TTL: ttl, Now: c.now})
	digests := prefixFor("g", "same bytes both sides")

	key := stickyKey{tenant: "t", group: "g", session: "s"}
	sticky.put(key, "d1", "k1")
	table.Record(digests, 7)

	c.advance(50 * time.Minute)
	if _, ok := sticky.get(key); !ok {
		t.Fatal("the session pin should still be live at 50 minutes")
	}
	if _, _, ok := table.Lookup(digests, nil); !ok {
		t.Fatal("the prefix entry should still be live at 50 minutes")
	}

	c.advance(20 * time.Minute) // 70 from creation, 20 from last use
	if _, ok := sticky.get(key); ok {
		t.Fatal("session stickiness expires from CREATION: it must be gone at 70 minutes")
	}
	if _, _, ok := table.Lookup(digests, nil); !ok {
		t.Fatal("prefix affinity expires from LAST USE: it must still be live 20 minutes after a hit")
	}
}

// TestScenario9SamePrefixSameTargetReorderedFree is DESIGN §14 scenario 9:
// "Same prefix → same target; reordered messages → free to choose differently."
//
// The hash chain is what makes the second half honest: reordering the messages
// diverges at the first differing byte, so the router has no affinity to obey
// and the strategy chain picks on the merits.
func TestScenario9SamePrefixSameTargetReorderedFree(t *testing.T) {
	h := newHarness(t, affinityConfig(),
		harnessOpts{pricing: planAndTokens, prefixOn: true})

	forward := prefixFor("m", "user: alpha\nassistant: one\nuser: beta")
	reordered := prefixFor("m", "user: beta\nassistant: one\nuser: alpha")
	if digestsEqual(forward, reordered) {
		t.Fatal("the chain must diverge on reordering; the test is not exercising anything")
	}

	h.health.MarkUnavailable("d-plan", 24*time.Hour)
	d0 := h.route(Request{Model: "m", Digests: forward, InputTokens: 1000})
	if d0.Deployment != "d-api" {
		t.Fatalf("setup: got %s", d0.Deployment)
	}
	h.ok(d0)
	h.health.MarkHealthy("d-plan")

	same := h.route(Request{Model: "m", Digests: forward, InputTokens: 1000})
	if same.Deployment != "d-api" {
		t.Fatalf("the same prefix must reach the same target: got %s", same.Deployment)
	}
	if same.PrefixDepth == 0 {
		t.Fatal("the decision must record the matched depth")
	}
	h.ok(same)

	free := h.route(Request{Model: "m", Digests: reordered, InputTokens: 1000})
	if free.Deployment != "d-plan" {
		t.Fatalf("a reordered conversation carries no affinity and the chain is free to "+
			"choose the cheaper deployment: got %s", free.Deployment)
	}
	if free.Reason != string(StrategyLowestCost) {
		t.Fatalf("want lowest_cost, got %q", free.Reason)
	}
	h.ok(free)
}

// TestPrefixHitDoesNotResurrectAnIneligibleTarget guards the predicate
// internal/prefix takes for exactly this reason: cache affinity must not bring
// back a deployment that health, quota or capability has ruled out.
func TestPrefixHitDoesNotResurrectAnIneligibleTarget(t *testing.T) {
	cfg := affinityConfig()
	// Give only d-plan the ability to carry a document.
	cfg.Groups[0].Deployments[0].Capabilities = canonical.CapToolCalls
	cfg.Groups[0].Deployments[1].Capabilities = canonical.CapToolCalls | canonical.CapDocumentBlocks
	h := newHarness(t, cfg, harnessOpts{pricing: planAndTokens, prefixOn: true})

	digests := prefixFor("m", "a long shared system prompt")
	h.prefix.Record(digests, h.interner.ID("d-api"))

	d := h.route(Request{Model: "m", Digests: digests, Required: canonical.CapDocumentBlocks})
	if d.Deployment != "d-plan" {
		t.Fatalf("a warm but ineligible target must not be resurrected: got %s", d.Deployment)
	}
	if d.Reason == prefixHitReason(d.PrefixDepth) {
		t.Fatalf("the hit must not be reported: %q", d.Reason)
	}
	h.ok(d)
}

// TestUnhealthyTargetDiscardsTheSessionPin is §7.4a's last sentence. Leaving
// the pin in place would route every remaining turn of the session at a backend
// that has just proved it cannot serve them.
func TestUnhealthyTargetDiscardsTheSessionPin(t *testing.T) {
	h := newHarness(t, affinityConfig(), harnessOpts{pricing: planAndTokens})

	h.health.MarkUnavailable("d-plan", 24*time.Hour)
	d0 := h.route(Request{Model: "m", Session: "s", Tenant: "t", InputTokens: 1000})
	h.ok(d0)
	h.health.MarkHealthy("d-plan")
	if h.r.sticky.len() != 1 {
		t.Fatal("setup: expected a pin")
	}

	d1 := h.route(Request{Model: "m", Session: "s", Tenant: "t", InputTokens: 1000})
	h.fail(d1, CauseUpstream5xx)
	if h.r.sticky.len() != 0 {
		t.Fatal("an unhealthy target must discard the pin immediately, not wait out the TTL")
	}
}

// TestStickyKeyIsTenantLeading is §7.4a's key shape: two tenants using the same
// session id must never share a pin.
func TestStickyKeyIsTenantLeading(t *testing.T) {
	h := newHarness(t, affinityConfig(), harnessOpts{pricing: planAndTokens})

	h.health.MarkUnavailable("d-plan", 24*time.Hour)
	a := h.route(Request{Model: "m", Tenant: "tenant-a", Session: "shared", InputTokens: 1000})
	h.ok(a)
	h.health.MarkHealthy("d-plan")

	b := h.route(Request{Model: "m", Tenant: "tenant-b", Session: "shared", InputTokens: 1000})
	if b.Deployment == "d-api" && b.Reason == ReasonSticky {
		t.Fatal("tenant-b inherited tenant-a's pin through a shared session id")
	}
	h.ok(b)
	if h.r.sticky.len() != 2 {
		t.Fatalf("two tenants must hold two distinct pins: got %d", h.r.sticky.len())
	}
}

// TestPurgeDropsExpiredPins keeps the periodic sweep honest.
func TestPurgeDropsExpiredPins(t *testing.T) {
	h := newHarness(t, affinityConfig(), harnessOpts{pricing: planAndTokens})
	d := h.route(Request{Model: "m", Tenant: "t", Session: "s", InputTokens: 1000})
	h.ok(d)
	if h.r.Purge() != 0 {
		t.Fatal("a live pin must not be purged")
	}
	h.clock.advance(2 * time.Hour)
	if n := h.r.Purge(); n != 1 {
		t.Fatalf("want 1 purged pin, got %d", n)
	}
}

func digestsEqual(a, b []prefix.Digest) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
