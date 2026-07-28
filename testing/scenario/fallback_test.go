package scenario

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/testing/fake"
)

// DESIGN §14 scenarios 6 and 7 — fail-back, and the boundary it must not cross.
//
// These run through [Gateway]: a real socket to a fake backend, real wire
// conversion, and a real routing session. internal/router already proves the
// chain in isolation; what is proven here is that the composition honours it,
// including the part no unit test can see — that the bytes the client received
// are not duplicated when a hop happens.

// twoGroupsOneClass is a group with two deployments and a class sibling with
// one: the shape scenario 6 needs.
func twoGroupsOneClass() router.Config {
	return router.Config{
		Groups: []router.Group{
			{Name: "chat-large", Class: "large", Deployments: []router.Deployment{
				{ID: "g1", Provider: "p1", Kind: "openai", UpstreamModel: "zai:glm-5.1",
					Credentials: []router.Credential{{ID: "k1"}}},
				{ID: "g2", Provider: "p2", Kind: "openai", UpstreamModel: "zai:glm-5.1",
					Credentials: []router.Credential{{ID: "k2"}}},
			}},
			{Name: "chat-large-alt", Class: "large", Deployments: []router.Deployment{
				{ID: "h1", Provider: "p3", Kind: "openai", UpstreamModel: "qwen3.5:397b",
					Credentials: []router.Credential{{ID: "k3"}}},
			}},
		},
		Fallback: router.FallbackConfig{
			On: router.DefaultChains(), MaxHops: 3, Budget: time.Minute,
		},
		// Configuration order is the final tie-break, so g1 is always first.
		Strategy: []router.Strategy{router.StrategyRoundRobin},
	}
}

func failing(status int) fake.Options {
	return fake.Options{
		Shape:     fake.ShapeOpenAI,
		Behaviour: func(*fake.Recorded) fake.Behaviour { return fake.Behaviour{Status: status} },
	}
}

func serving(text string) fake.Options {
	return fake.Options{
		Shape: fake.ShapeOpenAI,
		Script: func(r *fake.Recorded) fake.Script {
			return fake.Script{
				Text: text, TextChunks: 3,
				Usage: fake.Usage{InputTokens: 11, OutputTokens: 4},
			}
		},
	}
}

// -----------------------------------------------------------------------------
// §14.6 — model failure → same-group fallback → whole group down → same class
// -----------------------------------------------------------------------------

func TestScenario06_GroupThenClassDelegation(t *testing.T) {
	g := newGateway(t, twoGroupsOneClass(), rigOpts{}, map[string]fake.Options{
		"g1": failing(503),
		"g2": failing(503),
		"h1": serving("delegated answer"),
	})

	rep, err := g.do(t, Call{Family: FamilyOpenAI, Principal: "team-a",
		Body: []byte(`{"model":"chat-large","messages":[{"role":"user","content":"hi"}]}`)})
	if err != nil {
		t.Fatalf("the request should have succeeded on the class sibling: %v", err)
	}
	if rep.Status != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rep.Status, rep.Body)
	}

	want := []string{"g1", "g2", "h1"}
	if len(rep.Attempts) != len(want) {
		t.Fatalf("attempts = %v, want %v", rep.Attempts, want)
	}
	for i, id := range want {
		if rep.Attempts[i] != id {
			t.Fatalf("attempt %d went to %s, want %s (full chain %v)", i+1, rep.Attempts[i], id, rep.Attempts)
		}
	}

	// Delegation is to a DIFFERENT model of equivalent class, and the upstream
	// must have received that model's real id.
	if rep.UpstreamModel != "qwen3.5:397b" {
		t.Errorf("upstream model = %q, want qwen3.5:397b", rep.UpstreamModel)
	}
	last := g.ups["h1"].Last()
	if last == nil {
		t.Fatal("the class sibling never received a request")
	}
	if last.Model != "qwen3.5:397b" {
		t.Errorf("the sibling was sent %q; upstream always receives the real id (§7.2)", last.Model)
	}
	// The body the client sees carries the name the client asked for.
	if !strings.Contains(string(rep.Body), `"model":"chat-large"`) {
		t.Errorf("the response body must carry the requested name: %s", rep.Body)
	}
	if !strings.Contains(string(rep.Body), "delegated answer") {
		t.Errorf("the delegated answer did not reach the client: %s", rep.Body)
	}

	t.Run("inverse: a cause with no class link stays in the group", func(t *testing.T) {
		// timeout's chain is [same_group] alone. If the assertion above were
		// satisfied by "fall back to anything", this would delegate too.
		cfg := twoGroupsOneClass()
		r := newRig(t, cfg, rigOpts{})
		d1 := r.route(router.Request{Model: "chat-large"})
		r.fail(d1, router.CauseTimeout)
		d2 := r.route(router.Request{Model: "chat-large", Previous: d1})
		if d2.Group != "chat-large" {
			t.Fatalf("timeout left the group and landed in %s", d2.Group)
		}
		r.fail(d2, router.CauseTimeout)
		re := r.routeErr(router.Request{Model: "chat-large", Previous: d2})
		if re.Code != router.CodeFallbackExhausted {
			t.Fatalf("code = %q, want %q: timeout must not reach the class sibling", re.Code, router.CodeFallbackExhausted)
		}
	})

	t.Run("inverse: budget_exceeded and auth never fall back at all", func(t *testing.T) {
		// DESIGN §7.6 gives them empty chains deliberately: no other deployment
		// makes the money reappear or the credential valid.
		for _, cause := range []router.Cause{router.CauseBudgetExceeded, router.CauseAuth} {
			if cause.Chainable() {
				t.Errorf("%s must not be chainable", cause)
			}
			r := newRig(t, twoGroupsOneClass(), rigOpts{})
			d1 := r.route(router.Request{Model: "chat-large"})
			r.fail(d1, cause)
			re := r.routeErr(router.Request{Model: "chat-large", Previous: d1})
			if re.Code != router.CodeNotChainable {
				t.Errorf("%s produced code %q, want %q", cause, re.Code, router.CodeNotChainable)
			}
		}
	})
}

// -----------------------------------------------------------------------------
// §7.6 — what a 4xx is evidence of
// -----------------------------------------------------------------------------

// oneDeploymentGroup is a model served by a single backend. That is the shape
// that makes a circuit breaker load-bearing: standing this deployment down is
// not a degradation, it is the model going away.
func oneDeploymentGroup() router.Config {
	return router.Config{
		Groups: []router.Group{{Name: "chat", Class: "large", Deployments: []router.Deployment{
			{ID: "d1", Provider: "p1", Kind: "openai", UpstreamModel: "zai:glm-5.1",
				Credentials: []router.Credential{{ID: "k1"}}},
		}}},
		Fallback: router.FallbackConfig{On: router.DefaultChains(), MaxHops: 3, Budget: time.Minute},
	}
}

// TestMalformedRequestsFromOneTenantDoNotRemoveTheDeploymentFromTheOthers is the
// cross-tenant denial of service, end to end.
//
// The circuit breaker is per DEPLOYMENT and shared by every tenant of it. If a
// 400 counts against availability, then any caller holding any key can take a
// working deployment away from everybody else by sending a handful of bodies
// the backend refuses — no privilege, no volume, no timing.
//
// The backend here is deliberately healthy throughout: it answers the malformed
// bodies with a 400 immediately and correctly, and it serves everything else.
// Nothing about it is failing, which is the whole point.
func TestMalformedRequestsFromOneTenantDoNotRemoveTheDeploymentFromTheOthers(t *testing.T) {
	const marker = "TEMPERATURE-OUT-OF-RANGE"
	g := newGateway(t, oneDeploymentGroup(), rigOpts{}, map[string]fake.Options{
		"d1": {
			Shape: fake.ShapeOpenAI,
			Behaviour: func(r *fake.Recorded) fake.Behaviour {
				if bytes.Contains(r.Body, []byte(marker)) {
					return fake.Behaviour{Status: 400, ErrorType: "BadRequestError",
						ErrorMessage: "1 validation error for ChatCompletionRequest: " +
							"temperature must be <= 2"}
				}
				return fake.Behaviour{}
			},
			Script: func(*fake.Recorded) fake.Script {
				return fake.Script{Text: "served the well-formed request",
					Usage: fake.Usage{InputTokens: 9, OutputTokens: 4}}
			},
		},
	})

	bad := []byte(`{"model":"chat","messages":[{"role":"user","content":"` + marker + `"}]}`)
	for i := range 20 {
		rep, err := g.do(t, Call{Family: FamilyOpenAI, Principal: "attacker", Tenant: "attacker",
			Body: bad})
		if err == nil {
			t.Fatalf("request %d: the upstream served a body it is configured to refuse", i)
		}
		if rep.Status != 400 {
			t.Fatalf("request %d: status = %d, want 400 — the CALLER's request is what was refused",
				i, rep.Status)
		}
		if len(rep.Attempts) != 1 {
			t.Fatalf("request %d: attempts = %v; a malformed body is malformed on every "+
				"backend, so it must not tour the class", i, rep.Attempts)
		}
	}

	// A different tenant, a well-formed request, the same deployment.
	rep, err := g.do(t, Call{Family: FamilyOpenAI, Principal: "victim", Tenant: "victim",
		Body: []byte(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`)})
	if err != nil {
		t.Fatalf("another tenant's malformed requests took the deployment away from this one: %v", err)
	}
	if rep.Status != 200 || !strings.Contains(string(rep.Body), "served the well-formed request") {
		t.Fatalf("status = %d, body = %s", rep.Status, rep.Body)
	}

	if st := g.health.Stats("d1"); st.State != health.Closed || st.Failures != 0 || st.Opens != 0 {
		t.Fatalf("a backend that answered every request correctly was charged %d failures "+
			"and %d opens, and is %v", st.Failures, st.Opens, st.State)
	}
	// And it really was the same backend serving both, rather than a second one
	// quietly covering for a stood-down first.
	if n := g.ups["d1"].Count(); n != 21 {
		t.Errorf("the deployment received %d requests, want 21", n)
	}
}

// -----------------------------------------------------------------------------
// §14.7 — a stream falls back only before the first byte,
//         and there is zero duplicated output after it
// -----------------------------------------------------------------------------

func TestScenario07_StreamFallsBackOnlyBeforeTheFirstByte(t *testing.T) {
	const streamBody = `{"model":"chat-large","messages":[{"role":"user","content":"hi"}],` +
		`"stream":true,"stream_options":{"include_usage":true}}`

	t.Run("before the first byte a hop is allowed", func(t *testing.T) {
		// The upstream refuses with a status, so nothing has been written to the
		// client and the status is still ours to choose.
		g := newGateway(t, twoGroupsOneClass(), rigOpts{}, map[string]fake.Options{
			"g1": failing(503),
			"g2": serving("alpha beta gamma"),
			"h1": serving("never reached"),
		})
		rep, err := g.do(t, Call{Family: FamilyOpenAI, Body: []byte(streamBody)})
		if err != nil {
			t.Fatalf("the stream should have been served by g2: %v", err)
		}
		if len(rep.Attempts) != 2 || rep.Attempts[1] != "g2" {
			t.Fatalf("attempts = %v, want [g1 g2]", rep.Attempts)
		}
		body := string(rep.Body)
		if !strings.HasSuffix(body, "data: [DONE]\n\n") {
			t.Fatalf("the relayed stream is not terminated:\n%s", body)
		}

		// Zero duplication: the whole answer arrived exactly once, and no
		// fragment of the abandoned attempt is in it.
		for _, frag := range []string{"alpha", "beta", "gamma"} {
			if n := strings.Count(body, frag); n != 1 {
				t.Errorf("%q appears %d times in the client's stream, want 1:\n%s", frag, n, body)
			}
		}
		if strings.Contains(body, "never reached") {
			t.Error("output from a deployment that was never selected reached the client")
		}
	})

	t.Run("after the first byte no hop is permitted and nothing is duplicated", func(t *testing.T) {
		// g1 emits two content frames and then fails in band. The HTTP status is
		// already 200 and cannot change, so the only honest outcome is to end
		// the stream — DESIGN §7.6's boundary, and the whole point of the
		// scenario.
		g := newGateway(t, twoGroupsOneClass(), rigOpts{}, map[string]fake.Options{
			"g1": {
				Shape: fake.ShapeOpenAI,
				Script: func(*fake.Recorded) fake.Script {
					return fake.Script{Text: "alpha beta gamma", TextChunks: 3}
				},
				Behaviour: func(*fake.Recorded) fake.Behaviour {
					return fake.Behaviour{FailAfter: 2}
				},
			},
			"g2": serving("the answer that must not appear"),
			"h1": serving("nor this one"),
		})

		rep, _ := g.do(t, Call{Family: FamilyOpenAI, Body: []byte(streamBody)})
		if len(rep.Attempts) != 1 {
			t.Fatalf("attempts = %v: a committed stream must not hop", rep.Attempts)
		}
		if rep.Status != 200 {
			t.Errorf("status = %d; once a byte is out the response is already 200", rep.Status)
		}
		if !rep.FirstByteSent {
			t.Error("the relay did not report that the client had already seen output")
		}

		body := string(rep.Body)
		// Zero duplicated output: the two frames that did go out appear once,
		// and no byte of any other deployment's answer is present.
		for _, frag := range []string{"alpha", "beta"} {
			if n := strings.Count(body, frag); n != 1 {
				t.Errorf("%q appears %d times, want 1:\n%s", frag, n, body)
			}
		}
		for _, forbidden := range []string{"must not appear", "nor this one"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("a second deployment's output was appended to a committed stream:\n%s", body)
			}
		}
		// The client is told the stream failed, in band (COMPATIBILITY 1.3).
		if !strings.Contains(body, `"error"`) {
			t.Errorf("a mid-stream failure must be delivered in band:\n%s", body)
		}
		if g.ups["g2"].Count() != 0 || g.ups["h1"].Count() != 0 {
			t.Errorf("a fallback was dispatched after the first byte: g2=%d h1=%d",
				g.ups["g2"].Count(), g.ups["h1"].Count())
		}

		t.Run("DIVERGENCE: an in-band upstream failure is reported to the router as a success",
			func(t *testing.T) {
				// Characterization, not endorsement, and it appeared the moment
				// this harness stopped relaying streams itself: the harness used
				// to scan the relayed bytes for an error frame and hand
				// internal/router a failed outcome, which is a verdict about the
				// exchange — something the system under test is responsible for
				// producing (DESIGN §17.1). internal/backend does not produce it.
				// Its relay copies an upstream error frame through and returns a
				// nil error, so every consumer of the outcome is told the attempt
				// succeeded:
				//
				//   - internal/health never counts it, so a deployment that fails
				//     every stream after the first frame keeps its full share of
				//     traffic forever;
				//   - the meter records a success;
				//   - §7.6's stream_committed refusal is unreachable from this
				//     condition, so the ONLY end-to-end proof of the boundary is
				//     that no hop was dispatched above. The rule itself is proved
				//     in internal/router's TestStreamCommitted.
				//
				// Deleting this subtest is the correct move the day the relay
				// surfaces an in-band error; see the report accompanying this
				// change for where.
				rep, err := g.do(t, Call{Family: FamilyOpenAI, Body: []byte(streamBody)})
				if err != nil || rep.RouteError != nil {
					t.Skipf("the relay now surfaces the in-band failure (err=%v, route=%v): "+
						"restore the stream_committed assertion and delete this subtest",
						err, rep.RouteError)
				}
				t.Log("internal/backend relayed an upstream in-band error frame and reported " +
					"the attempt as a success; the failure reaches neither internal/health " +
					"nor the fail-back boundary")
			})
	})

	t.Run("inverse: the same failure BEFORE any frame does hop", func(t *testing.T) {
		// The distinction is the first byte, not the failure. With FailAfter
		// removed and the same 5xx delivered as a status, the request moves on
		// — which is what makes the assertion above about the boundary rather
		// than about mid-stream failures being unrecoverable in general.
		g := newGateway(t, twoGroupsOneClass(), rigOpts{}, map[string]fake.Options{
			"g1": failing(500),
			"g2": serving("alpha beta gamma"),
			"h1": serving("unused"),
		})
		rep, err := g.do(t, Call{Family: FamilyOpenAI, Body: []byte(streamBody)})
		if err != nil {
			t.Fatalf("a pre-first-byte 500 must be recoverable: %v", err)
		}
		if len(rep.Attempts) != 2 {
			t.Fatalf("attempts = %v, want two", rep.Attempts)
		}
		if !strings.Contains(string(rep.Body), "alpha") {
			t.Errorf("the second attempt's output did not reach the client: %s", rep.Body)
		}
	})
}
