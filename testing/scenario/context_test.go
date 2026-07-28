package scenario

import (
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/tokenest"
	"github.com/ziozzang/dorang/testing/fake"
)

// DESIGN §10.5a — the context window is used for routing and refusal.
//
// Two properties, and neither of them is a property of the estimator alone. An
// estimator test asserting a better number would pass while the routing filter
// still refused the request, so both of these are stated as what the CLIENT
// gets: a multimodal request is served, and an upstream overflow lands on a
// larger model instead of on the caller.

// visionGroup is one deployment that declares a real window and can express
// image blocks — the ordinary hosted-vision shape.
func visionGroup() router.Config {
	return router.Config{
		Groups: []router.Group{{Name: "vision", Class: "large", Deployments: []router.Deployment{
			{ID: "v1", Provider: "p1", Kind: "openai", UpstreamModel: "vision-1",
				ContextWindow: 128_000, MaxOutputTokens: 4_096,
				Capabilities: canonical.Structural,
				Credentials:  []router.Credential{{ID: "k1"}}},
		}}},
		Fallback: router.FallbackConfig{On: router.DefaultChains(), MaxHops: 2, Budget: time.Minute},
		Strategy: []router.Strategy{router.StrategyRoundRobin},
	}
}

// multimodalBody is a chat request carrying one inline image of encoded bytes.
// It is a data: URL because that is how every SDK sends a local file, and it is
// what puts the image into the request body rather than behind a fetch.
func multimodalBody(model string, encoded int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"`)
	b.WriteString(model)
	b.WriteString(`","messages":[{"role":"user","content":[`)
	b.WriteString(`{"type":"text","text":"what is in this photo?"},`)
	b.WriteString(`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,`)
	b.WriteString(strings.Repeat("A", encoded))
	b.WriteString(`"}}]}]}`)
	return []byte(b.String())
}

// TestAPhotographDoesNotExhaustTheContextWindow.
//
// A 450 KB photograph base64-encodes to ~600 KB of request body. Estimated at
// three bytes per token that is ~200,000 tokens, which is more than the 128,000
// this deployment declares — so the routing filter removes the only candidate
// and the caller is refused with `context_length_exceeded` and a number that is
// wrong by more than a hundred times. The image costs about 1,600.
//
// dorang models images first-class (canonical.KindImage, CapImageBlocks), so
// this is a supported input and being refused is not a limitation, it is a bug.
func TestAPhotographDoesNotExhaustTheContextWindow(t *testing.T) {
	g := newGateway(t, visionGroup(), rigOpts{}, map[string]fake.Options{
		"v1": serving("a cat asleep on a sofa"),
	})

	rep, err := g.do(t, Call{Family: FamilyOpenAI, Body: multimodalBody("vision", 600_000)})
	if err != nil {
		if rep.RouteError != nil {
			t.Fatalf("the request was refused before it was ever dispatched: %s %s",
				rep.RouteError.Code, rep.RouteError.Message)
		}
		t.Fatalf("the multimodal request failed: %v", err)
	}
	if rep.Status != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rep.Status, rep.Body)
	}
	if g.ups["v1"].Count() != 1 {
		t.Fatalf("the deployment received %d requests, want 1", g.ups["v1"].Count())
	}
	if !strings.Contains(string(rep.Body), "a cat asleep on a sofa") {
		t.Errorf("the answer did not reach the client: %s", rep.Body)
	}

	// The inverse: an estimate is still a filter. A conversation that really is
	// larger than the window is still refused, and the refusal still says so.
	t.Run("inverse: real overflow is still refused", func(t *testing.T) {
		g := newGateway(t, visionGroup(), rigOpts{}, map[string]fake.Options{
			"v1": serving("never reached"),
		})
		huge := `{"model":"vision","messages":[{"role":"user","content":"` +
			strings.Repeat("word ", 200_000) + `"}]}`
		rep, err := g.do(t, Call{Family: FamilyOpenAI, Body: []byte(huge)})
		if err == nil {
			t.Fatalf("a million-token prompt was admitted; status %d", rep.Status)
		}
		if rep.RouteError == nil || rep.RouteError.Code != router.CodeContextWindow {
			t.Fatalf("refusal = %+v, want %s", rep.RouteError, router.CodeContextWindow)
		}
		// §10.5a's third row: the caller learns the real limit. And because the
		// size is dorang's own estimate, the refusal says that too — a client
		// told "needs N tokens" cannot otherwise tell a guess from a count.
		if !rep.RouteError.Estimated {
			t.Error("the refusal presents an estimate as a measurement")
		}
		if !strings.Contains(rep.RouteError.Message, "estimated") {
			t.Errorf("the message does not say the size was estimated: %q", rep.RouteError.Message)
		}
		if !strings.Contains(rep.RouteError.Message, tokenest.MethodStructural) {
			t.Errorf("the message does not name the estimate's method: %q", rep.RouteError.Message)
		}
		if rep.RouteError.EstimateMethod != tokenest.MethodStructural {
			t.Errorf("EstimateMethod = %q, want %q",
				rep.RouteError.EstimateMethod, tokenest.MethodStructural)
		}
		if !strings.Contains(rep.RouteError.Message, "128000") {
			t.Errorf("the message does not name the real limit: %q", rep.RouteError.Message)
		}
		if g.ups["v1"].Count() != 0 {
			t.Errorf("a refused request still reached the upstream %d times", g.ups["v1"].Count())
		}
	})
}

// undeclaredThenLarger is the self-hosted shape docs/VLLM.md and docs/SGLANG.md
// target: a deployment that declares no context window at all, with a class
// sibling that declares a large one. Nothing dorang computes before dispatch can
// refuse the first one, so the upstream's own 400 is the only overflow signal
// that exists.
func undeclaredThenLarger() router.Config {
	return router.Config{
		Groups: []router.Group{
			{Name: "local", Class: "large", Deployments: []router.Deployment{
				{ID: "vllm", Provider: "p1", Kind: "openai", UpstreamModel: "qwen3.5:32b",
					Capabilities: canonical.Structural,
					Credentials:  []router.Credential{{ID: "k1"}}},
			}},
			{Name: "local-xl", Class: "large", Deployments: []router.Deployment{
				{ID: "vllm-xl", Provider: "p2", Kind: "openai", UpstreamModel: "qwen3.5:397b",
					ContextWindow: 200_000, MaxOutputTokens: 8_192,
					Capabilities: canonical.Structural,
					Credentials:  []router.Credential{{ID: "k2"}}},
			}},
		},
		Fallback: router.FallbackConfig{On: router.DefaultChains(), MaxHops: 3, Budget: time.Minute},
		Strategy: []router.Strategy{router.StrategyRoundRobin},
	}
}

// overflowing answers the 400 a backend sends when the prompt did not fit. The
// wording is vLLM's; the point of matching on it is that the code field is not
// reliable — SGLANG.md §6.2 records four spellings of `type` from one process —
// so the message has to be enough on its own.
func overflowing(message string) fake.Options {
	return fake.Options{
		Shape: fake.ShapeOpenAI,
		Behaviour: func(*fake.Recorded) fake.Behaviour {
			return fake.Behaviour{Status: 400, ErrorType: "BadRequestError", ErrorMessage: message}
		},
	}
}

// TestAnUpstreamOverflowRoutesToALargerWindow.
//
// §10.5a's second row, reached from the only direction that works when the
// deployment declares no window: the backend says the prompt did not fit, and
// dorang moves to a deployment whose window is larger than the one that failed.
//
// Before this, nothing inspected an error body, so every 400 classified as
// CauseNone and the context_window chain could not be entered by an upstream
// signal at all.
func TestAnUpstreamOverflowRoutesToALargerWindow(t *testing.T) {
	g := newGateway(t, undeclaredThenLarger(), rigOpts{}, map[string]fake.Options{
		"vllm": overflowing("This model's maximum context length is 32768 tokens. " +
			"However, you requested 41000 tokens. Please reduce the length of the messages."),
		"vllm-xl": serving("answered on the larger window"),
	})

	rep, err := g.do(t, Call{Family: FamilyOpenAI, Principal: "team-a",
		Body: []byte(`{"model":"local","messages":[{"role":"user","content":"a long conversation"}]}`)})
	if err != nil {
		t.Fatalf("the overflow was not recovered from: %v (attempts %v)", err, rep.Attempts)
	}
	if rep.Status != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rep.Status, rep.Body)
	}
	want := []string{"vllm", "vllm-xl"}
	if len(rep.Attempts) != len(want) || rep.Attempts[0] != want[0] || rep.Attempts[1] != want[1] {
		t.Fatalf("attempts = %v, want %v", rep.Attempts, want)
	}
	if rep.Decision.Reason != "fallback:context_window" {
		t.Errorf("route reason = %q, want the context_window chain", rep.Decision.Reason)
	}
	if !strings.Contains(string(rep.Body), "answered on the larger window") {
		t.Errorf("the larger deployment's answer did not reach the client: %s", rep.Body)
	}

	// The inverse: only an overflow signature enters that chain. An ordinary
	// 400 — a malformed field, a rejected parameter — is terminal, because
	// retrying it on another deployment burns capacity to reach the identical
	// refusal.
	t.Run("inverse: an ordinary 400 does not fall back", func(t *testing.T) {
		g := newGateway(t, undeclaredThenLarger(), rigOpts{}, map[string]fake.Options{
			"vllm":    overflowing("1 validation error for ChatCompletionRequest: temperature must be <= 2"),
			"vllm-xl": serving("must not be reached"),
		})
		rep, err := g.do(t, Call{Family: FamilyOpenAI,
			Body: []byte(`{"model":"local","messages":[{"role":"user","content":"hi"}]}`)})
		if err == nil {
			t.Fatalf("a validation 400 was retried into a success: attempts %v", rep.Attempts)
		}
		if len(rep.Attempts) != 1 {
			t.Errorf("attempts = %v, want one: an unrecognised 400 is terminal", rep.Attempts)
		}
		if g.ups["vllm-xl"].Count() != 0 {
			t.Errorf("the class sibling was tried %d times for a malformed request",
				g.ups["vllm-xl"].Count())
		}
	})
}
