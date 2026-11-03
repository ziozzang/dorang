package router

import (
	"strings"
	"testing"
	"time"
)

func twoOllama() Config {
	return Config{Groups: []Group{{Name: "m", Deployments: []Deployment{
		dep("o1", "ollama", "openai", "u"),
		dep("o2", "ollama", "openai", "u"),
	}}}}
}

// The client hanging up is not the deployment failing (board #1623). Ten
// cancelled streams in a row must leave both accounts selectable.
func TestClientHangUpsDoNotStandDownDeployments(t *testing.T) {
	h := newHarness(t, twoOllama(), harnessOpts{})
	for i := 0; i < 10; i++ {
		d := h.route(Request{Model: "m"})
		h.r.Report(d, Outcome{Err: errFake, Cause: CauseAborted, Total: time.Minute})
	}
	h.route(Request{Model: "m"}) // must not be refused
	for _, id := range []string{"o1", "o2"} {
		if s := h.health.Stats(id); s.Failures != 0 || s.Opens != 0 {
			t.Errorf("%s was counted against: %+v", id, s)
		}
	}
}

func TestAbortedIsNotAFallbackCondition(t *testing.T) {
	if CauseAborted.Chainable() {
		t.Error("a hung-up client must not tour the fallback chain")
	}
	if _, ok := ParseCause("aborted"); ok {
		t.Error("aborted must not be a configurable fallback key")
	}
	if countsAgainstAvailability(CauseAborted) {
		t.Error("aborted counted against availability")
	}
}

// When every candidate is resting on a short guess, one request is let through
// the one that recovers soonest instead of being refused for certain.
func TestEveryCandidateRestingStillTriesTheSoonestOnce(t *testing.T) {
	h := newHarness(t, twoOllama(), harnessOpts{})
	h.health.MarkUnavailable("o1", 20*time.Second)
	h.health.MarkUnavailable("o2", 8*time.Second)

	d := h.route(Request{Model: "m"})
	if d.Deployment != "o2" {
		t.Fatalf("last resort chose %s, want the soonest to recover (o2)", d.Deployment)
	}
	// Served: the stand-down ends for everyone.
	h.ok(d)
	if s := h.health.Stats("o2"); s.State != 0 {
		t.Fatalf("a served last-resort request left o2 %v", s.State)
	}
	if d2 := h.route(Request{Model: "m"}); d2.Deployment != "o2" {
		t.Fatalf("o2 not selectable after proving itself: %s", d2.Deployment)
	}
}

func TestLastResortIsRateLimitedPerDeployment(t *testing.T) {
	h := newHarness(t, twoOllama(), harnessOpts{})
	h.health.MarkUnavailable("o1", 20*time.Second)
	h.health.MarkUnavailable("o2", 20*time.Second)
	h.route(Request{Model: "m"}) // o1 (config order) or o2
	h.route(Request{Model: "m"}) // the other
	e := h.routeErr(Request{Model: "m"})
	if e.Code != CodeNoCandidate {
		t.Fatalf("code %q", e.Code)
	}
}

// A long stand-down is somebody saying "not until then"; it is not overridden.
func TestLastResortLeavesProviderNamedLongWaitsAlone(t *testing.T) {
	h := newHarness(t, twoOllama(), harnessOpts{})
	h.health.MarkUnavailable("o1", 10*time.Minute)
	h.health.MarkUnavailable("o2", 10*time.Minute)
	h.routeErr(Request{Model: "m"})
}

// The refusal says why and for how long.
func TestNoHealthyDeploymentNamesTheReasonAndTheWait(t *testing.T) {
	h := newHarness(t, twoOllama(), harnessOpts{})
	h.health.MarkUnavailable("o1", 10*time.Minute)
	h.health.SetReason("o1", "quota_exhausted, upstream Retry-After 10m0s")
	h.health.MarkUnavailable("o2", 5*time.Minute)
	h.health.SetReason("o2", "auth_failed (upstream 401)")
	e := h.routeErr(Request{Model: "m"})
	for _, want := range []string{"o1: quota_exhausted", "o2: auth_failed"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q lacks %q", e.Message, want)
		}
	}
	if got := e.ResetAt.Sub(h.clock.now()); got != 5*time.Minute {
		t.Errorf("ResetAt in %v, want the soonest recovery (5m)", got)
	}
}

// A 429 is remembered with its reason, and a short Retry-After rests that
// account for that long and no longer.
func TestRateLimitedAccountRestsOnlyForRetryAfter(t *testing.T) {
	h := newHarness(t, twoOllama(), harnessOpts{})
	d := h.route(Request{Model: "m"})
	first := d.Deployment
	h.r.Report(d, Outcome{Err: errFake, Status: 429, Cause: CauseRateLimit, RetryAfter: time.Second})
	// The sibling serves meanwhile.
	if d2 := h.route(Request{Model: "m"}); d2.Deployment == first {
		t.Fatalf("the 429'd account %s was reselected at once", first)
	}
	h.clock.advance(1100 * time.Millisecond)
	found := false
	for _, ds := range h.health.Details() {
		if ds.ID == first && ds.Remaining == 0 {
			found = true
		}
		if ds.ID == first && ds.Reason == "" {
			t.Errorf("no reason recorded for %s", ds.ID)
		}
	}
	if !found {
		t.Fatalf("%s still resting after its 1s Retry-After", first)
	}
}
