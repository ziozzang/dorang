package router

import (
	"context"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
)

// Everything in this file was configured and never applied: key_rotation's four
// strategy names, §7.5a(c)'s quota_urgency, and §10.5's per-principal priority
// grant. Each test asserts an OUTCOME the router produces — which credential was
// chosen, which candidate won, what priority went on the decision — rather than
// that a helper computes the right number when called. That distinction is the
// whole point: internal/quota's urgency arithmetic has always been correct and
// tested, and nothing has ever called it.

// stubUrgency is a per-credential urgency the test states directly.
type stubUrgency map[string]float64

func (s stubUrgency) Urgency(cred string, _ float64, _ time.Time) float64 { return s[cred] }

// rotationHarness builds one group whose single deployment has three accounts.
func rotationHarness(t *testing.T, rot Rotation) *harness {
	t.Helper()
	d := dep("d1", "p", "openai", "m", "acct-1", "acct-2", "acct-3")
	for i := range d.Credentials {
		d.Credentials[i].MaxConcurrent = 8
	}
	return newHarness(t, Config{
		Rotation: rot,
		Groups:   []Group{{Name: "m", Deployments: []Deployment{d}}},
	}, harnessOpts{})
}

// TestRotationFailoverStaysOnTheFirstAccount pins the behaviour every strategy
// used to get, so the other cases are a change from something rather than from
// nothing.
func TestRotationFailoverStaysOnTheFirstAccount(t *testing.T) {
	h := rotationHarness(t, RotationFailover)
	for i := 0; i < 5; i++ {
		d := h.route(Request{Model: "m"})
		if d.Credential != "acct-1" {
			t.Fatalf("attempt %d chose %q, want acct-1 under failover", i, d.Credential)
		}
		h.ok(d)
		d.Reservation.Release()
	}
}

// TestRotationRoundRobinAdvances: `key_rotation.strategy: round_robin` was
// accepted by the validator and then every request went to the first account.
func TestRotationRoundRobinAdvances(t *testing.T) {
	h := rotationHarness(t, RotationRoundRobin)
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		d := h.route(Request{Model: "m"})
		seen[d.Credential]++
		h.ok(d)
		d.Reservation.Release()
	}
	if len(seen) != 3 {
		t.Fatalf("round_robin used %d of 3 accounts: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 3 {
			t.Errorf("account %s served %d of 9 requests, want an even 3: %v", id, n, seen)
		}
	}
}

// TestRotationLeastUsedPrefersTheIdlestAccount. It is the DEFAULT whenever a
// rotation pool is configured, which is what made its being unwired expensive:
// every deployment that configured a pool concentrated on one account.
func TestRotationLeastUsedPrefersTheIdlestAccount(t *testing.T) {
	h := rotationHarness(t, RotationLeastUsed)

	// Hold one on the first account — in flight, NOT reported, since Report
	// releases the reservation and an account with nothing outstanding is not
	// busy. The next request must not pick it.
	first := h.route(Request{Model: "m"})
	if first.Credential != "acct-1" {
		t.Fatalf("the first request chose %q on an idle pool, want acct-1", first.Credential)
	}

	second := h.route(Request{Model: "m"})
	if second.Credential == "acct-1" {
		t.Fatal("least_used chose the only busy account")
	}
	h.ok(first)
	h.ok(second)
}

// TestRotationNeverOverridesACredentialPin: a rotation is a statement about
// load, a pin is a statement about this conversation, and the pin wins. A
// rotation that could move a pinned conversation to another account would break
// it rather than slow it (§7.4a2).
func TestRotationNeverOverridesACredentialPin(t *testing.T) {
	h := rotationHarness(t, RotationRoundRobin)
	for i := 0; i < 4; i++ {
		d := h.route(Request{Model: "m", Pins: []Pin{{Kind: "previous_response_id", Strength: Pinned, Credential: "acct-3"}}})
		if d.Credential != "acct-3" {
			t.Fatalf("attempt %d left the pinned account for %q", i, d.Credential)
		}
		h.ok(d)
		d.Reservation.Release()
	}
}

// TestQuotaUrgencyRanksByExpiringAllowance is §7.5a(c). The comparator, the
// damping, the jitter and the Ranker facade all existed; quota_urgency was not
// an accepted strategy name in either list, so none of it could be selected.
func TestQuotaUrgencyRanksByExpiringAllowance(t *testing.T) {
	cfg := Config{
		Groups: []Group{{
			Name:     "m",
			Strategy: []Strategy{StrategyQuotaUrgency},
			Deployments: []Deployment{
				dep("cold", "p", "openai", "m", "acct-cold"),
				dep("expiring", "p", "openai", "m", "acct-expiring"),
			},
		}},
	}
	h := newHarness(t, cfg, harnessOpts{})
	// The harness builds its own Deps, so the urgency source is installed after
	// construction — which is also the shape internal/app uses.
	h.r.deps.Urgency = stubUrgency{"acct-cold": 0.5, "acct-expiring": 8.0}

	d := h.route(Request{Model: "m"})
	if d.Deployment != "expiring" {
		t.Fatalf("quota_urgency chose %q, want the allowance about to be discarded", d.Deployment)
	}
	h.ok(d)
	d.Reservation.Release()
}

// TestQuotaUrgencyIsSilentWithoutASource: a chain that names the strategy on a
// deployment with no resetting window must fall through to the next rule rather
// than order on zeroes. Silence, not "least urgent" — the same rule lowest_cost
// follows for an unpriced candidate.
func TestQuotaUrgencyIsSilentWithoutASource(t *testing.T) {
	cfg := Config{
		Groups: []Group{{
			Name:     "m",
			Strategy: []Strategy{StrategyQuotaUrgency, StrategyPriority},
			Deployments: []Deployment{
				{ID: "low", Provider: "p", Kind: "openai", UpstreamModel: "m", Priority: 9,
					Credentials: []Credential{{ID: "a"}}},
				{ID: "high", Provider: "p", Kind: "openai", UpstreamModel: "m", Priority: 1,
					Credentials: []Credential{{ID: "b"}}},
			},
		}},
	}
	h := newHarness(t, cfg, harnessOpts{})
	d := h.route(Request{Model: "m"})
	if d.Deployment != "high" {
		t.Fatalf("chose %q: an unscored quota_urgency must let the next rule decide", d.Deployment)
	}
	h.ok(d)
	d.Reservation.Release()
}

// TestQuotaUrgencyIsAnAcceptedStrategyName guards the two independent lists.
// internal/config validates one and internal/router parses another, with nothing
// holding them together, which is exactly how quota_urgency came to be missing
// from both.
func TestQuotaUrgencyIsAnAcceptedStrategyName(t *testing.T) {
	if _, ok := ParseStrategy("quota_urgency"); !ok {
		t.Fatal("the router does not accept quota_urgency")
	}
}

// TestClientPriorityGrantIsPerPrincipal is §10.5. The clamp arithmetic was
// finished and tested; the grant that arms it could not be expressed, and
// PriorityConfig carried one fleet-wide Min/Max that nothing ever set.
func TestClientPriorityGrantIsPerPrincipal(t *testing.T) {
	pc := DefaultPriority()
	pc.Grants = map[string]PriorityGrant{
		// [batch, interactive] on the canonical scale is [2, 10].
		"batch-pipeline": {Min: 2, Max: 10},
	}
	hint := 2

	if got := pc.CanonicalFor("batch-pipeline", "batch", &hint); got != 2 {
		t.Errorf("a granted principal's hint = %d, want 2", got)
	}
	if got := pc.CanonicalFor("someone-else", "batch", &hint); got != 10 {
		t.Errorf("an ungranted principal's hint = %d, want the class value 10", got)
	}
	// The grant is a budget, not a blank cheque: realtime is outside the band.
	realtime := 0
	if got := pc.CanonicalFor("batch-pipeline", "batch", &realtime); got != 2 {
		t.Errorf("a hint past the grant = %d, want the clamp at 2", got)
	}
	if !pc.GrantsHint("batch-pipeline") {
		t.Error("GrantsHint is false for a granted principal")
	}
	if pc.GrantsHint("someone-else") {
		t.Error("GrantsHint is true for a principal with no grant")
	}
}

// TestClientPriorityDefaultsToIgnore keeps the shipped behaviour: a caller
// cannot claim urgency, and the absence of configuration is not a grant.
func TestClientPriorityDefaultsToIgnore(t *testing.T) {
	pc := DefaultPriority()
	hint := 0
	if got := pc.CanonicalFor("anyone", "batch", &hint); got != 10 {
		t.Fatalf("an unconfigured deployment honoured a client hint: got %d, want 10", got)
	}
	if pc.GrantsHint("anyone") {
		t.Fatal("an unconfigured deployment reports a grant")
	}
}

// TestDroppedPriorityHintIsReported: §10.5 requires the drop to be visible,
// because §10.3's whole rule is that silently discarding what a caller sent is
// worse than refusing it.
func TestDroppedPriorityHintIsReported(t *testing.T) {
	pc := DefaultPriority()
	pc.Grants = map[string]PriorityGrant{"granted": {Min: 0, Max: 10}}
	cfg := Config{
		Priority: pc,
		Groups: []Group{{Name: "m", Deployments: []Deployment{
			dep("d1", "p", "openai", "m"),
		}}},
	}
	h := newHarness(t, cfg, harnessOpts{})
	hint := 0

	d := h.route(Request{Model: "m", Principal: "ungranted", PriorityHint: &hint})
	if !d.PriorityHintDropped {
		t.Error("an ignored hint was not reported as dropped")
	}
	h.ok(d)
	d.Reservation.Release()

	d = h.route(Request{Model: "m", Principal: "granted", PriorityHint: &hint})
	if d.PriorityHintDropped {
		t.Error("an honoured hint was reported as dropped")
	}
	if d.CanonicalPriority != 0 {
		t.Errorf("a granted hint of 0 produced priority %d", d.CanonicalPriority)
	}
	h.ok(d)
	d.Reservation.Release()
}

// TestPrincipalMaxReachesTheBroker joins the two halves of the key's
// max_parallel_requests: the router has to carry it onto capacity.Request or the
// broker's enforcement is unreachable.
func TestPrincipalMaxReachesTheBroker(t *testing.T) {
	h := newHarness(t, Config{Groups: []Group{{Name: "m", Deployments: []Deployment{
		dep("d1", "p", "openai", "m"),
	}}}}, harnessOpts{})

	req := Request{Model: "m", Principal: "k1", PrincipalMax: 1}
	// Held in flight rather than reported: Report releases the reservation, and
	// a concurrency ceiling is about what is outstanding right now.
	first := h.route(req)
	defer h.ok(first)

	if _, err := h.r.Route(context.Background(), req); err == nil {
		t.Fatal("a second concurrent request was admitted past max_parallel_requests: 1")
	}

	// And the slot comes back.
	h.ok(first)
	again, err := h.r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("the slot was not returned: %v", err)
	}
	h.ok(again)
}

// TestUrgencySourceIsTheQuotaRanker is a compile-time join: internal/quota's
// Ranker must satisfy the interface internal/router declares, or the app's
// wiring would need an adapter nobody would notice was missing.
func TestUrgencySourceIsTheQuotaRanker(t *testing.T) {
	var _ UrgencySource = (*quota.Ranker)(nil)
}
