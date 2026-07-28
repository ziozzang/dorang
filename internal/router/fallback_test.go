package router

import (
	"context"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/quota"
)

// classConfig is two model groups of one class: group "g" with two deployments
// and group "h" with one. It is the shape scenario 6 needs — a group to exhaust
// and a class sibling to delegate to.
func classConfig() Config {
	return Config{
		Groups: []Group{
			{Name: "g", Class: "chat-large", Deployments: []Deployment{
				dep("g1", "p1", "openai", "model-x"),
				dep("g2", "p2", "openai", "model-x"),
			}},
			{Name: "h", Class: "chat-large", Deployments: []Deployment{
				dep("h1", "p3", "openai", "model-z"),
			}},
		},
		Fallback: FallbackConfig{On: DefaultChains(), MaxHops: 3, Budget: 2 * time.Minute},
	}
}

// TestScenario6GroupThenClassDelegation is DESIGN §14 scenario 6: "Model
// failure → same-group fallback → whole group down → same-class delegation".
func TestScenario6GroupThenClassDelegation(t *testing.T) {
	h := newHarness(t, classConfig(), harnessOpts{})

	d1 := h.route(Request{Model: "g"})
	if d1.Deployment != "g1" || d1.Attempt != 1 {
		t.Fatalf("first attempt: got %s attempt %d", d1.Deployment, d1.Attempt)
	}
	h.fail(d1, CauseUpstream5xx)

	d2 := h.route(Request{Model: "g", Previous: d1})
	if d2.Deployment != "g2" {
		t.Fatalf("same_group is the first link: got %s", d2.Deployment)
	}
	if d2.Attempt != 2 {
		t.Fatalf("want attempt 2, got %d", d2.Attempt)
	}
	if d2.Reason != fallbackReason(CauseUpstream5xx) {
		t.Fatalf("want reason %q, got %q", fallbackReason(CauseUpstream5xx), d2.Reason)
	}
	if d2.Group != "g" {
		t.Fatalf("same_group must stay in the group: got %s", d2.Group)
	}
	h.fail(d2, CauseUpstream5xx)

	d3 := h.route(Request{Model: "g", Previous: d2})
	if d3.Deployment != "h1" {
		t.Fatalf("the whole group is down; same_class must delegate: got %s", d3.Deployment)
	}
	if d3.Group != "h" || d3.Class != "chat-large" {
		t.Fatalf("delegation must land in the same class: got group %s class %s", d3.Group, d3.Class)
	}
	if d3.UpstreamModel != "model-z" {
		t.Fatalf("same_class is delegation to a DIFFERENT model of equivalent class: got %s",
			d3.UpstreamModel)
	}
	h.ok(d3)
}

// TestFallbackChainsAreDistinctPerCause is §7.6's table read as a
// specification. The chains differ, and the difference must be observable:
// timeout stays inside the group where rate_limit is allowed to leave it.
func TestFallbackChainsAreDistinctPerCause(t *testing.T) {
	t.Run("timeout never leaves the group", func(t *testing.T) {
		h := newHarness(t, classConfig(), harnessOpts{})
		d1 := h.route(Request{Model: "g"})
		h.fail(d1, CauseTimeout)
		d2 := h.route(Request{Model: "g", Previous: d1})
		if d2.Group != "g" {
			t.Fatalf("timeout's chain is [same_group]: got group %s", d2.Group)
		}
		h.fail(d2, CauseTimeout)
		// The group is now exhausted, and timeout has nowhere else to go.
		e := h.routeErr(Request{Model: "g", Previous: d2})
		if e.Code != CodeFallbackExhausted {
			t.Fatalf("timeout must not reach the class sibling: got %s -> %s", e.Code, e.Message)
		}
	})

	t.Run("rate_limit leaves the group", func(t *testing.T) {
		h := newHarness(t, classConfig(), harnessOpts{})
		d1 := h.route(Request{Model: "g"})
		h.fail(d1, CauseRateLimit)
		d2 := h.route(Request{Model: "g", Previous: d1})
		h.fail(d2, CauseRateLimit)
		d3 := h.route(Request{Model: "g", Previous: d2})
		if d3.Group != "h" {
			t.Fatalf("rate_limit's chain reaches same_class: got group %s", d3.Group)
		}
		h.ok(d3)
	})

	t.Run("content_policy goes straight to the class", func(t *testing.T) {
		h := newHarness(t, classConfig(), harnessOpts{})
		d1 := h.route(Request{Model: "g"})
		h.fail(d1, CauseContentPolicy)
		d2 := h.route(Request{Model: "g", Previous: d1})
		// content_policy's chain is [same_class] with no same_group link, so
		// hop 1 already sees the whole class.
		if d2.Deployment == "g1" {
			t.Fatal("the failed deployment must be excluded")
		}
		h.ok(d2)
	})
}

// TestBudgetExceededIsTerminal is §7.6's empty chain, and the reason it is
// empty: no other deployment makes the money reappear. internal/quota already
// models this and the router must not re-derive it.
func TestBudgetExceededIsTerminal(t *testing.T) {
	h := newHarness(t, classConfig(), harnessOpts{})
	d := h.route(Request{Model: "g"})

	budgetErr := &quota.BudgetError{Subject: quota.Global, Limit: 100, Spent: 100, Requested: 5}
	if !quota.IsTerminal(budgetErr) {
		t.Fatal("internal/quota must already classify a budget refusal as terminal")
	}
	h.r.Report(d, Outcome{Err: budgetErr})

	e := h.routeErr(Request{Model: "g", Previous: d})
	if e.Code != CodeNotChainable {
		t.Fatalf("want %s, got %s (%s)", CodeNotChainable, e.Code, e.Message)
	}
	if e.Cause != CauseBudgetExceeded {
		t.Fatalf("the router must classify a quota.BudgetError as budget_exceeded: got %s", e.Cause)
	}
	if !IsTerminal(e) {
		t.Fatal("the refusal itself must be terminal")
	}
}

// TestAuthIsTerminalAndMarksTheCredential is the other empty chain. §7.6 also
// requires the credential to be marked exhausted, because a wrong key stays
// wrong and sending it round the whole class just multiplies the 401.
func TestAuthIsTerminalAndMarksTheCredential(t *testing.T) {
	h := newHarness(t, classConfig(), harnessOpts{})
	d := h.route(Request{Model: "g"})
	h.r.Report(d, Outcome{Err: errFake, Status: 401})

	if got := h.health.Stats("g1").State; got != health.Open {
		t.Fatalf("an authentication failure must take the credential out of service: state %v", got)
	}
	e := h.routeErr(Request{Model: "g", Previous: d})
	if e.Code != CodeNotChainable || e.Cause != CauseAuth {
		t.Fatalf("want a terminal auth refusal, got %s / %s", e.Code, e.Cause)
	}
	if !IsTerminal(e) {
		t.Fatal("auth has no chain, so the refusal is terminal")
	}
}

// TestContextWindowFallsBackToALargerWindow is §7.6's same_class_larger, and
// §10.5a's second row: route to a same-class deployment with a bigger window
// rather than rewriting the caller's conversation.
func TestContextWindowFallsBackToALargerWindow(t *testing.T) {
	cfg := Config{
		Groups: []Group{
			{Name: "g", Class: "c", Deployments: []Deployment{
				{ID: "g-small", Provider: "p1", Kind: "openai", UpstreamModel: "m",
					ContextWindow: 32_000, Credentials: []Credential{{ID: "k1"}}},
			}},
			{Name: "h", Class: "c", Deployments: []Deployment{
				{ID: "h-same", Provider: "p2", Kind: "openai", UpstreamModel: "m2",
					ContextWindow: 32_000, Credentials: []Credential{{ID: "k2"}}},
				{ID: "h-undeclared", Provider: "p3", Kind: "openai", UpstreamModel: "m3",
					Credentials: []Credential{{ID: "k3"}}},
				{ID: "h-large", Provider: "p4", Kind: "openai", UpstreamModel: "m4",
					ContextWindow: 200_000, Credentials: []Credential{{ID: "k4"}}},
			}},
		},
		Fallback: FallbackConfig{On: DefaultChains(), MaxHops: 3},
	}
	h := newHarness(t, cfg, harnessOpts{})

	d := h.route(Request{Model: "g", InputTokens: 1000})
	h.fail(d, CauseContextWindow)

	d2 := h.route(Request{Model: "g", Previous: d, InputTokens: 1000})
	if d2.Deployment != "h-large" {
		t.Fatalf("same_class_larger must pick the strictly larger window and skip the "+
			"equal one and the UNDECLARED one: got %s", d2.Deployment)
	}
	h.ok(d2)
}

// TestContextWindowWithNoLargerDeploymentFailsNamingTheLimit is §10.5a's third
// row. Failing is the correct outcome: the caller learns the real limit, which
// is information they did not have and cannot get elsewhere.
func TestContextWindowWithNoLargerDeploymentFailsNamingTheLimit(t *testing.T) {
	cfg := Config{
		Groups: []Group{{Name: "g", Class: "c", Deployments: []Deployment{
			{ID: "only", Provider: "p1", Kind: "openai", UpstreamModel: "m",
				ContextWindow: 32_000, Credentials: []Credential{{ID: "k1"}}},
		}}},
		Fallback: FallbackConfig{On: DefaultChains(), MaxHops: 3},
	}
	h := newHarness(t, cfg, harnessOpts{})
	d := h.route(Request{Model: "g"})
	h.fail(d, CauseContextWindow)

	e := h.routeErr(Request{Model: "g", Previous: d})
	if e.Code != CodeContextWindow || !e.Terminal() {
		t.Fatalf("want a terminal context-window refusal: got %s terminal=%v", e.Code, e.Terminal())
	}
	if !contains(e.Message, "32000") {
		t.Fatalf("the refusal must name the real limit: %q", e.Message)
	}
	if !contains(e.Message, "compact") {
		t.Fatalf("the refusal should say dorang does not compact (§10.5a): %q", e.Message)
	}
}

// TestHopBudgetIsBounded is §7.6's max_hops.
func TestHopBudgetIsBounded(t *testing.T) {
	cfg := classConfig()
	cfg.Fallback.MaxHops = 1
	h := newHarness(t, cfg, harnessOpts{})

	d1 := h.route(Request{Model: "g"})
	h.fail(d1, CauseUpstream5xx)
	d2 := h.route(Request{Model: "g", Previous: d1})
	if d2.Attempt != 2 {
		t.Fatalf("want attempt 2, got %d", d2.Attempt)
	}
	h.fail(d2, CauseUpstream5xx)

	e := h.routeErr(Request{Model: "g", Previous: d2})
	if e.Code != CodeHopsExhausted {
		t.Fatalf("want %s, got %s", CodeHopsExhausted, e.Code)
	}
	if !e.Terminal() {
		t.Fatal("an exhausted hop budget is terminal")
	}
}

// TestFallbackIsDisabledWhenNoHopsAreAllowed: max_hops of zero means no
// fail-back at all, which must be a clear refusal rather than an accidental
// retry.
func TestFallbackIsDisabledWhenNoHopsAreAllowed(t *testing.T) {
	cfg := classConfig()
	cfg.Fallback.MaxHops = 0
	h := newHarness(t, cfg, harnessOpts{})
	d := h.route(Request{Model: "g"})
	h.fail(d, CauseUpstream5xx)
	e := h.routeErr(Request{Model: "g", Previous: d})
	if e.Code != CodeHopsExhausted {
		t.Fatalf("want %s, got %s", CodeHopsExhausted, e.Code)
	}
}

// TestWallClockBudgetIsBounded is the other bound of §7.6. A chain that is
// within its hop budget but has already spent the caller's patience must stop:
// three more hops of a slow failure is worse than one fast one.
func TestWallClockBudgetIsBounded(t *testing.T) {
	cfg := classConfig()
	cfg.Fallback.MaxHops = 5
	cfg.Fallback.Budget = 100 * time.Millisecond
	h := newHarness(t, cfg, harnessOpts{})

	d := h.route(Request{Model: "g"})
	h.fail(d, CauseUpstream5xx)
	h.clock.advance(150 * time.Millisecond)

	e := h.routeErr(Request{Model: "g", Previous: d})
	if e.Code != CodeBudgetElapsed {
		t.Fatalf("want %s, got %s", CodeBudgetElapsed, e.Code)
	}
	if !e.Terminal() {
		t.Fatal("an elapsed wall-clock budget is terminal")
	}
}

// TestScenario7StreamingFallbackBoundary is DESIGN §14 scenario 7: "Stream
// interrupted → fallback only before first byte; zero duplicated output after".
func TestScenario7StreamingFallbackBoundary(t *testing.T) {
	h := newHarness(t, classConfig(), harnessOpts{})

	t.Run("before the first byte a stream may hop", func(t *testing.T) {
		d := h.route(Request{Model: "g", Stream: true})
		if !d.Stream {
			t.Fatal("the decision must carry the streaming flag")
		}
		h.r.Report(d, Outcome{Err: errFake, Status: 500, FirstByteSent: false})
		d2 := h.route(Request{Model: "g", Stream: true, Previous: d})
		if d2.Deployment == d.Deployment {
			t.Fatal("the hop must move off the failed deployment")
		}
		h.ok(d2)
	})

	t.Run("after the first byte it must not", func(t *testing.T) {
		d := h.route(Request{Model: "g", Stream: true})
		h.r.Report(d, Outcome{Err: errFake, Status: 500, FirstByteSent: true})
		e := h.routeErr(Request{Model: "g", Stream: true, Previous: d})
		if e.Code != CodeStreamCommitted {
			t.Fatalf("want %s, got %s (%s)", CodeStreamCommitted, e.Code, e.Message)
		}
		if !e.Terminal() {
			t.Fatal("duplicated output is worse than a visible failure; the refusal is terminal")
		}
	})

	t.Run("the boundary is sticky once crossed", func(t *testing.T) {
		// A byte reaching the client on hop 1 closes fail-back for the whole
		// session, not only for that hop.
		d := h.route(Request{Model: "g", Stream: true})
		h.r.Report(d, Outcome{Err: errFake, Status: 500, FirstByteSent: true})
		e := h.routeErr(Request{Model: "g", Stream: true, Previous: d})
		if e.Code != CodeStreamCommitted {
			t.Fatalf("want %s, got %s", CodeStreamCommitted, e.Code)
		}
	})
}

// TestReportIsRequiredBeforeAHop keeps the loop honest: a caller that never
// reported the outcome cannot ask for a fallback, because nothing knows what
// happened or which chain applies.
func TestReportIsRequiredBeforeAHop(t *testing.T) {
	h := newHarness(t, classConfig(), harnessOpts{})
	d := h.route(Request{Model: "g"})
	e := h.routeErr(Request{Model: "g", Previous: d})
	if e.Code != "attempt_not_reported" {
		t.Fatalf("want attempt_not_reported, got %s", e.Code)
	}
	h.ok(d)
}

// TestSuccessCannotFallBack: a decision that succeeded is not a fallback
// candidate, whatever the caller does with it afterwards.
func TestSuccessCannotFallBack(t *testing.T) {
	h := newHarness(t, classConfig(), harnessOpts{})
	d := h.route(Request{Model: "g"})
	h.ok(d)
	e := h.routeErr(Request{Model: "g", Previous: d})
	if e.Code != "attempt_succeeded" {
		t.Fatalf("want attempt_succeeded, got %s", e.Code)
	}
}

// TestRateLimitDoesNotCountAgainstAvailability is the distinction
// health.Outcome.Failure exists for: a 429 is an error to the caller and says
// nothing about whether the deployment is alive. Counting it would open the
// circuit on a backend that is answering perfectly.
func TestRateLimitDoesNotCountAgainstAvailability(t *testing.T) {
	h := newHarness(t, classConfig(), harnessOpts{})
	for i := 0; i < 5; i++ {
		d := h.route(Request{Model: "g"})
		h.r.Report(d, Outcome{Err: errFake, Status: 429})
	}
	if got := h.health.Stats("g1").Failures; got != 0 {
		t.Fatalf("a 429 must not count against availability: %d failures recorded", got)
	}
	if got := h.health.Stats("g1").State; got != health.Closed {
		t.Fatalf("the circuit must stay closed after repeated 429s: %v", got)
	}
}

// TestMalformedRequestsCannotStandDownAHealthyDeployment is the denial of
// service the 4xx boundary exists to close, exercised as the attack rather than
// as a classification.
//
// A 400 is the REQUEST being wrong. The deployment that produced it read the
// body, judged it correctly and answered immediately — it is working. Counting
// that against availability opened a circuit that is shared by every tenant of
// the deployment, so any caller holding any key could take a healthy backend
// away from everyone else for the price of three requests.
//
// The group here holds ONE deployment on purpose. With two, the victim would be
// quietly moved to the sibling and the test would pass while the attack still
// worked; with one, losing the deployment is losing the model.
func TestMalformedRequestsCannotStandDownAHealthyDeployment(t *testing.T) {
	soloGroup := Config{
		Groups: []Group{{Name: "g", Class: "chat-large", Deployments: []Deployment{
			dep("g1", "p1", "openai", "model-x"),
		}}},
		Fallback: FallbackConfig{On: DefaultChains(), MaxHops: 3, Budget: 2 * time.Minute},
	}
	h := newHarness(t, soloGroup, harnessOpts{})

	// The attack: one key, a stream of malformed bodies. Far more than the
	// three consecutive failures that open a circuit. A refusal here is not the
	// assertion — the attacker losing the deployment is only the attacker's
	// problem — so the loop stops rather than failing, and the victim below is
	// what the test is about.
	for range 20 {
		d, err := h.r.Route(context.Background(), Request{
			Model: "g", Principal: "attacker", Tenant: "attacker"})
		if err != nil {
			break
		}
		h.r.Report(d, Outcome{Err: errFake, Status: 400, Total: time.Millisecond})
	}

	// The victim: a different tenant, a well-formed request. The circuit is per
	// deployment and not per tenant, so if the attack opened it this route has
	// nowhere left to go.
	d, err := h.r.Route(context.Background(), Request{
		Model: "g", Principal: "victim", Tenant: "victim"})
	if err != nil {
		t.Fatalf("malformed requests from one caller took the deployment away from "+
			"every other tenant: %v", err)
	}
	if d.Deployment != "g1" {
		t.Fatalf("the well-formed request landed on %s", d.Deployment)
	}
	h.ok(d)

	if st := h.health.Stats("g1"); st.Failures != 0 || st.Opens != 0 || st.State != health.Closed {
		t.Fatalf("a working backend was charged with %d failures and %d opens, state %v",
			st.Failures, st.Opens, st.State)
	}

	t.Run("one bad body does not tour the class either", func(t *testing.T) {
		// The same misclassification also handed the request upstream_5xx's
		// chain, so a single malformed body was retried on the group and then on
		// every class sibling — opening a circuit at each stop. It is terminal
		// where it happened.
		h := newHarness(t, classConfig(), harnessOpts{})
		d := h.route(Request{Model: "g"})
		h.r.Report(d, Outcome{Err: errFake, Status: 400, Total: time.Millisecond})

		re := h.routeErr(Request{Model: "g", Previous: d})
		if re.Code != CodeNotChainable {
			t.Fatalf("code = %q, want %q: a malformed body is malformed on every backend",
				re.Code, CodeNotChainable)
		}
		if re.Status != 400 {
			t.Errorf("status = %d, want 400: the caller's request is what was refused", re.Status)
		}
		for _, id := range []string{"g1", "g2", "h1"} {
			if st := h.health.Stats(id).Failures; st != 0 {
				t.Errorf("%s was charged %d failures for another tenant's malformed body", id, st)
			}
		}
	})
}

// TestUpstream5xxOpensTheCircuit is the control: a 5xx is exactly the signal
// availability tracking exists for.
func TestUpstream5xxOpensTheCircuit(t *testing.T) {
	h := newHarness(t, classConfig(), harnessOpts{})
	for i := 0; i < 3; i++ {
		d := h.route(Request{Model: "g"})
		if d.Deployment != "g1" {
			break
		}
		h.r.Report(d, Outcome{Err: errFake, Status: 500})
	}
	if got := h.health.Stats("g1").State; got != health.Open {
		t.Fatalf("three consecutive 5xx must open the circuit: %v", got)
	}
	d := h.route(Request{Model: "g"})
	if d.Deployment == "g1" {
		t.Fatal("an open circuit must not be selected")
	}
	h.ok(d)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOfSub(s, sub) >= 0)
}

func indexOfSub(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
