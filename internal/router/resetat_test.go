package router

import (
	"testing"
	"time"
)

// A rate limit whose reset the provider named is a cooldown of that length.
//
// The configured RateLimitCooldown is a guess made before the provider had
// said anything; a 429 carrying the instant its window recovers has said
// exactly how long. The deployment must stay out until that instant — past
// the configured guess — and come back at it.
func TestAProviderNamedResetOutlivesTheConfiguredCooldown(t *testing.T) {
	cfg := Config{
		Groups: []Group{{Name: "g", Deployments: []Deployment{dep("g1", "p1", "openai", "model-x")}}},
		Fallback: FallbackConfig{On: DefaultChains(), MaxHops: 3, Budget: 2 * time.Minute,
			RateLimitCooldown: 10 * time.Second},
	}
	h := newHarness(t, cfg, harnessOpts{})
	d := h.route(Request{Model: "g"})
	h.r.Report(d, Outcome{Err: errFake, Status: 429, Cause: CauseRateLimit,
		Total: 5 * time.Millisecond, ResetAt: h.clock.now().Add(90 * time.Second)})

	if e := h.routeErr(Request{Model: "g"}); e == nil {
		t.Fatal("the deployment was offered again immediately after a 429 naming a 90s reset")
	}
	h.clock.advance(11 * time.Second)
	if e := h.routeErr(Request{Model: "g"}); e == nil {
		t.Fatal("the deployment came back at the configured 10s guess although the provider said 90s")
	}
	h.clock.advance(80 * time.Second)
	if d := h.route(Request{Model: "g"}); d == nil || d.Deployment != "g1" {
		t.Fatalf("the deployment did not come back at the instant the provider named: %+v", d)
	}
}
