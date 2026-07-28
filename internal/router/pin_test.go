package router

import (
	"errors"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/quota"
)

// twoFamilies is one class holding two model groups on two protocol families,
// which is the shape EXTENSIONS §B.3 is about: a conversation that started on
// one family carries state the other cannot decrypt.
func twoFamilies() Config {
	return Config{
		Groups: []Group{
			{Name: "resp", Class: "chat-large", Deployments: []Deployment{
				{ID: "resp-1", Provider: "p-openai", Kind: "openai-responses",
					Family: "openai-responses", UpstreamModel: "gpt-x",
					Credentials: []Credential{{ID: "openai-a"}, {ID: "openai-b"}}},
			}},
			{Name: "msgs", Class: "chat-large", Deployments: []Deployment{
				{ID: "msgs-1", Provider: "p-anthropic", Kind: "anthropic",
					Family: "anthropic-messages", UpstreamModel: "claude-y",
					Credentials: []Credential{{ID: "anthropic-a"}}},
			}},
		},
		Fallback: FallbackConfig{On: DefaultChains(), MaxHops: 3},
	}
}

// TestOpaqueStateIsAHardPinNotAPreference is EXTENSIONS §B.3 and DESIGN §7.6's
// warning box: a conversation carrying family-scoped opaque state must fail
// IMMEDIATELY rather than spend its hop budget arriving at the identical 400.
//
// The receiving family classifies foreign state as never-retryable — grok's own
// error text says "Never retryable — the user must start a new session" — so a
// fallback across the boundary cannot succeed, and every hop it spends makes
// the same failure slower.
func TestOpaqueStateIsAHardPinNotAPreference(t *testing.T) {
	h := newHarness(t, twoFamilies(), harnessOpts{})

	// The conversation began on the Anthropic family and echoes a signed
	// thinking block. The caller asks the responses-shaped group.
	e := h.routeErr(Request{
		Model: "resp",
		Pins: []Pin{{
			Kind:     "reasoning.encrypted_content",
			Strength: Pinned,
			Family:   "anthropic-messages",
		}},
	})

	if e.Code != CodeStatePinUnroutable {
		t.Fatalf("want %s, got %s (%s)", CodeStatePinUnroutable, e.Code, e.Message)
	}
	if e.Status != 400 {
		t.Fatalf("a request that can never be served anywhere is a 400: got %d", e.Status)
	}
	if !e.Terminal() || !IsTerminal(e) || !quota.IsTerminal(e) {
		t.Fatal("a pin refusal must be terminal under every terminality predicate")
	}
	if e.Pin != "reasoning.encrypted_content" {
		t.Fatalf("the refusal must name the state that pinned: got %q", e.Pin)
	}
	if e.Family != "anthropic-messages" {
		t.Fatalf("the refusal must name the family required: got %q", e.Family)
	}
	if e.Attempt != 1 {
		t.Fatalf("the refusal must land on the FIRST attempt, spending no hops: got attempt %d", e.Attempt)
	}
}

// TestFamilyPinRoutesToTheFamilyThatCanAcceptIt is the other half of §10.1:
// capability routing prefers a backend that CAN express the request, so the pin
// is a filter with a positive answer whenever one exists.
func TestFamilyPinRoutesToTheFamilyThatCanAcceptIt(t *testing.T) {
	h := newHarness(t, twoFamilies(), harnessOpts{})
	d := h.route(Request{
		Model: "msgs",
		Pins:  []Pin{{Kind: "thinking.signature", Strength: Pinned, Family: "anthropic-messages"}},
	})
	if d.Deployment != "msgs-1" {
		t.Fatalf("want msgs-1, got %s", d.Deployment)
	}
	if d.PinnedTo != "thinking.signature" {
		t.Fatalf("the decision must record the pin it honoured: got %q", d.PinnedTo)
	}
	h.ok(d)
}

// TestFallbackNeverCrossesTheFamilyPin is the routing consequence §7.6 spells
// out: once the in-family deployment has failed, the class sibling on another
// family is NOT a fallback target, and the session ends rather than hopping.
func TestFallbackNeverCrossesTheFamilyPin(t *testing.T) {
	h := newHarness(t, twoFamilies(), harnessOpts{})
	pin := []Pin{{Kind: "previous_response_id", Strength: Pinned, Family: "openai-responses"}}

	d := h.route(Request{Model: "resp", Pins: pin})
	if d.Deployment != "resp-1" {
		t.Fatalf("want resp-1, got %s", d.Deployment)
	}
	h.fail(d, CauseUpstream5xx)

	e := h.routeErr(Request{Model: "resp", Pins: pin, Previous: d})
	if e.Code != CodeStatePinUnroutable {
		t.Fatalf("the hop must refuse rather than cross the family: got %s (%s)", e.Code, e.Message)
	}
	if !e.Terminal() {
		t.Fatal("crossing the pin cannot succeed, so the refusal is terminal")
	}
	if e.Attempt != 2 {
		t.Fatalf("the refusal is on hop 2: got %d", e.Attempt)
	}
}

// twoAccounts is DESIGN §7.4a2's normal case: several accounts on one vendor,
// each with its own concurrency ceiling.
func twoAccounts(limit int) (Config, capacity.Config) {
	cfg := Config{
		Groups: []Group{{Name: "plan", Class: "chat", Deployments: []Deployment{
			{ID: "plan-1", Provider: "vendor", Kind: "glm", Family: "openai-chat",
				UpstreamModel: "zai:glm-5.1", Credentials: []Credential{
					{ID: "acct-1", CapacityGroup: "acct-1", MaxConcurrent: limit},
					{ID: "acct-2", CapacityGroup: "acct-2", MaxConcurrent: limit},
				}},
		}}},
		OnCapacity: capacity.Spill,
		Fallback:   FallbackConfig{On: DefaultChains(), MaxHops: 3},
	}
	return cfg, capacity.Config{SweepInterval: -1}
}

// TestStatefulConversationNeverSpillsToAnotherAccount is scenario one of
// §7.4a2: two accounts on one provider, a stateful conversation, the preferred
// account saturated. The request waits or fails; it never lands elsewhere.
//
// Spilling here is not a slower answer, it is a wrong one: a server-side
// response handle does not resolve on a sibling account, and the failure mode
// that matters is the one that returns a plausible answer to a different
// question.
func TestStatefulConversationNeverSpillsToAnotherAccount(t *testing.T) {
	cfg, cc := twoAccounts(1)
	h := newHarness(t, cfg, harnessOpts{capacity: cc})

	// Saturate acct-1 only. acct-2 is wide open.
	hold := h.route(Request{Model: "plan", Pins: []Pin{
		{Kind: "previous_response_id", Strength: Pinned, Credential: "acct-1"}}})
	if hold.Credential != "acct-1" {
		t.Fatalf("setup: expected acct-1, got %s", hold.Credential)
	}

	e := h.routeErr(Request{Model: "plan", Pins: []Pin{
		{Kind: "previous_response_id", Strength: Pinned, Credential: "acct-1"}}})
	if e.Code != CodeCredentialSaturated {
		t.Fatalf("want %s, got %s (%s)", CodeCredentialSaturated, e.Code, e.Message)
	}
	if !e.Terminal() {
		t.Fatal("a pinned request that cannot be served is terminal, not a fallback")
	}
	if e.Credential != "acct-1" {
		t.Fatalf("the refusal must name the pinned account: got %q", e.Credential)
	}
	h.ok(hold)
}

// TestStatelessConversationSpillsCleanly is scenario two: the same saturation,
// no state, so the spill to another account is exactly right — a cold cache is
// a cost, not a corruption.
func TestStatelessConversationSpillsCleanly(t *testing.T) {
	cfg, cc := twoAccounts(1)
	h := newHarness(t, cfg, harnessOpts{capacity: cc})

	first := h.route(Request{Model: "plan"})
	if first.Credential != "acct-1" {
		t.Fatalf("setup: expected acct-1 first, got %s", first.Credential)
	}
	second := h.route(Request{Model: "plan"})
	if second.Credential != "acct-2" {
		t.Fatalf("an unpinned request must spill to the free account: got %s", second.Credential)
	}
	h.ok(first)
	h.ok(second)
}

// TestPinnedAccountWithExhaustedQuotaFailsAndNamesTheReset is scenario three.
// dorang cannot move the conversation to a healthy account without breaking it,
// so the honest outcome is to fail and say WHICH account is exhausted and when
// it resets, letting the caller choose between waiting and starting fresh.
// Silently continuing elsewhere trades a visible limit for an invisible
// corruption.
func TestPinnedAccountWithExhaustedQuotaFailsAndNamesTheReset(t *testing.T) {
	cfg, cc := twoAccounts(4)
	h := newHarness(t, cfg, harnessOpts{capacity: cc})

	reset := h.clock.now().Add(37 * time.Minute)
	h.quota["acct-1"] = quota.Decision{Allow: false, ResetAt: reset, Used: 1000, Limit: 1000}

	e := h.routeErr(Request{Model: "plan", Pins: []Pin{
		{Kind: "reasoning.encrypted_content", Strength: Pinned, Credential: "acct-1"}}})

	if e.Code != CodeCredentialExhausted {
		t.Fatalf("want %s, got %s (%s)", CodeCredentialExhausted, e.Code, e.Message)
	}
	if e.Status != 429 {
		t.Fatalf("an exhausted quota is a 429: got %d", e.Status)
	}
	if !e.Terminal() {
		t.Fatal("the pinned account is the only account; failing is the correct outcome")
	}
	if e.Credential != "acct-1" {
		t.Fatalf("the refusal must name the exhausted account: got %q", e.Credential)
	}
	if !e.ResetAt.Equal(reset) {
		t.Fatalf("the refusal must carry the reset instant: want %v, got %v", reset, e.ResetAt)
	}

	// And the control: without the pin, the same exhausted account simply steps
	// aside and traffic continues on the other one (DESIGN §6.1, scenario 4).
	d := h.route(Request{Model: "plan"})
	if d.Credential != "acct-2" {
		t.Fatalf("an unpinned request must step aside to acct-2: got %s", d.Credential)
	}
	h.ok(d)
}

// TestPinIsInferredNotConfigured guards the sentence that makes §7.4a2 work:
// "dorang infers the pin rather than trusting configuration for it." Spill is
// configured, and a stateful request pins anyway, because the setting expresses
// a preference about cost and this is not a question about cost.
func TestPinIsInferredNotConfigured(t *testing.T) {
	cfg, cc := twoAccounts(1)
	cfg.OnCapacity = capacity.Spill // configuration says: spill freely
	h := newHarness(t, cfg, harnessOpts{capacity: cc})

	hold := h.route(Request{Model: "plan"}) // takes acct-1
	if hold.Credential != "acct-1" {
		t.Fatalf("setup: got %s", hold.Credential)
	}
	e := h.routeErr(Request{Model: "plan", Pins: []Pin{
		{Kind: "x-codex-turn-state", Strength: Pinned, Credential: "acct-1"}}})
	if e.Code != CodeCredentialSaturated {
		t.Fatalf("the inferred pin must override the configured spill: got %s", e.Code)
	}
	h.ok(hold)
}

// TestPinnedRequestWaitsWhenGivenABudget covers the "waits for the pinned
// credential" half of §7.4a2: with a wait budget the request blocks on its own
// account rather than landing on another one.
func TestPinnedRequestWaitsWhenGivenABudget(t *testing.T) {
	cfg, cc := twoAccounts(1)
	cfg.PinnedWait = 500 * time.Millisecond
	h := newHarness(t, cfg, harnessOpts{capacity: cc})

	hold := h.route(Request{Model: "plan"})
	done := make(chan *Decision, 1)
	go func() {
		d, err := h.r.Route(t.Context(), Request{Model: "plan", Pins: []Pin{
			{Kind: "previous_response_id", Strength: Pinned, Credential: "acct-1"}}})
		if err != nil {
			done <- nil
			return
		}
		done <- d
	}()

	time.Sleep(20 * time.Millisecond)
	h.ok(hold) // releases acct-1

	d := <-done
	if d == nil {
		t.Fatal("the waiter should have been admitted once the pinned account freed a slot")
	}
	if d.Credential != "acct-1" {
		t.Fatalf("the waiter must land on the pinned account: got %s", d.Credential)
	}
	h.ok(d)
}

// TestPreferredAffinityIsNotAPin is the counterpart: a Preferred pin is a cache
// hint and spills, which is right for stateless traffic and is most of it.
func TestPreferredAffinityIsNotAPin(t *testing.T) {
	cfg, cc := twoAccounts(1)
	h := newHarness(t, cfg, harnessOpts{capacity: cc})

	hold := h.route(Request{Model: "plan"})
	d, err := h.r.Route(t.Context(), Request{Model: "plan", Pins: []Pin{
		{Kind: "prompt_cache_key", Strength: Preferred, Credential: "acct-1"}}})
	if err != nil {
		t.Fatalf("a preferred affinity must spill rather than refuse: %v", err)
	}
	if d.Credential != "acct-2" {
		t.Fatalf("expected a spill to acct-2: got %s", d.Credential)
	}
	h.ok(hold)
	h.ok(d)
}

func TestErrorsIsMatchesRoutingErrors(t *testing.T) {
	h := newHarness(t, twoFamilies(), harnessOpts{})
	_, err := h.r.Route(t.Context(), Request{Model: "no-such-model"})
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("a routing refusal must match ErrNoRoute: %v", err)
	}
}

// TestPinnedCandidatesDoNotShareACredentialSlice is the regression test for a
// bug this package had: the one-element credential list a credential pin
// produces was cut from a buffer shared by every candidate, so each candidate
// overwrote the last one's provider and upstream model. Every pinned candidate
// then reserved capacity on the LAST deployment's axes while reporting the
// first deployment's identity — a mis-accounted reservation that no assertion
// about the decision itself can see.
//
// The two deployments share a credential id and each caps it at one, so the
// key axis is (provider, credential) and the two axes are distinct. If the
// slices alias, both candidates contend for one axis and the second request is
// refused instead of landing on the free deployment.
func TestPinnedCandidatesDoNotShareACredentialSlice(t *testing.T) {
	cfg := Config{
		Groups: []Group{{Name: "m", Class: "c", Deployments: []Deployment{
			{ID: "d1", Provider: "prov-1", Kind: "openai", UpstreamModel: "shared-model",
				Credentials: []Credential{{ID: "shared", MaxConcurrent: 1}}},
			{ID: "d2", Provider: "prov-2", Kind: "openai", UpstreamModel: "shared-model",
				Credentials: []Credential{{ID: "shared", MaxConcurrent: 1}}},
		}}},
		Fallback: FallbackConfig{On: DefaultChains(), MaxHops: 3},
	}
	h := newHarness(t, cfg, harnessOpts{})
	pin := []Pin{{Kind: "previous_response_id", Strength: Pinned, Credential: "shared"}}

	first := h.route(Request{Model: "m", Pins: pin})
	if first.Deployment != "d1" {
		t.Fatalf("setup: got %s", first.Deployment)
	}
	second := h.route(Request{Model: "m", Pins: pin})
	if second.Deployment != "d2" {
		t.Fatalf("the second pinned request must reach the other deployment's own key axis: "+
			"got %s", second.Deployment)
	}
	if second.Provider != "prov-2" {
		t.Fatalf("want prov-2, got %s", second.Provider)
	}
	h.ok(first)
	h.ok(second)
}

// TestQuotaFilteredCandidatesDoNotShareACredentialSlice is the same hazard on
// the unpinned path, where a partially exhausted credential pool produces a
// filtered list per deployment.
func TestQuotaFilteredCandidatesDoNotShareACredentialSlice(t *testing.T) {
	cfg := Config{
		Groups: []Group{{Name: "m", Deployments: []Deployment{
			{ID: "d1", Provider: "prov-1", Kind: "openai", UpstreamModel: "shared-model",
				Credentials: []Credential{
					{ID: "dead", MaxConcurrent: 4}, {ID: "live", MaxConcurrent: 1}}},
			{ID: "d2", Provider: "prov-2", Kind: "openai", UpstreamModel: "shared-model",
				Credentials: []Credential{
					{ID: "dead", MaxConcurrent: 4}, {ID: "live", MaxConcurrent: 1}}},
		}}},
	}
	h := newHarness(t, cfg, harnessOpts{})
	h.quota["dead"] = quota.Decision{Allow: false, ResetAt: h.clock.now().Add(time.Hour)}

	first := h.route(Request{Model: "m"})
	if first.Deployment != "d1" || first.Credential != "live" {
		t.Fatalf("setup: got %s / %s", first.Deployment, first.Credential)
	}
	second := h.route(Request{Model: "m"})
	if second.Deployment != "d2" || second.Provider != "prov-2" {
		t.Fatalf("the filtered list must carry d2's own provider: got %s / %s",
			second.Deployment, second.Provider)
	}
	h.ok(first)
	h.ok(second)
}
