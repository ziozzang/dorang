package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// TestPriorityOppositeDirections is the test DESIGN §7.5 asks for by name.
//
// "A test asserting only that a number was sent would pass while the behaviour
// is backwards", so this asserts the ORDER the two engines will schedule in.
// vLLM schedules the lowest value first, SGLang the highest, using the same
// field name and type, and both answer 200 either way — so the only observable
// that distinguishes correct from inverted is the relation between the values
// emitted for two different classes on the same engine.
//
// The canonical values come from internal/router, not from arithmetic repeated
// here: a second implementation of the negation is the defect itself.
func TestPriorityOppositeDirections(t *testing.T) {
	pc := router.DefaultPriority()

	realtime := pc.Canonical("realtime", nil)
	batch := pc.Canonical("batch", nil)
	if realtime >= batch {
		t.Fatalf("the canonical scale is not lower-is-more-urgent: realtime=%d batch=%d",
			realtime, batch)
	}

	emitted := func(kind string, class string) float64 {
		t.Helper()
		value, field, ok := pc.Wire(kind, pc.Canonical(class, nil))
		if !ok {
			t.Fatalf("%s: no priority field", kind)
		}
		f := newFakeUpstream(t)
		f.answer(http.StatusOK, `{"id":"1","object":"chat.completion","model":"m","choices":[]}`)
		p := testProvider(t, f, kind, catalog.APIOpenAIChat)
		tg := target(p)
		tg.PriorityField, tg.Priority = field, value

		if res := testBackend("k").Do(context.Background(), tg, chatCall(catalog.APIOpenAIChat), nil); res.Err != nil {
			t.Fatalf("%s: %v", kind, res.Err)
		}
		sent := decodeJSON(t, f.last().body)
		n, ok := sent["priority"].(float64)
		if !ok {
			t.Fatalf("%s: no priority field on the wire: %v", kind, sent)
		}
		return n
	}

	vllmRealtime, vllmBatch := emitted("vllm", "realtime"), emitted("vllm", "batch")
	sglangRealtime, sglangBatch := emitted("sglang", "realtime"), emitted("sglang", "batch")

	// vLLM: lowest first, so the urgent class must carry the SMALLER number.
	if !(vllmRealtime < vllmBatch) {
		t.Errorf("vLLM: realtime=%v batch=%v — vLLM schedules the lowest value first, so this "+
			"ordering makes batch outrank realtime", vllmRealtime, vllmBatch)
	}
	// SGLang: highest first, so the urgent class must carry the LARGER number.
	if !(sglangRealtime > sglangBatch) {
		t.Errorf("sglang: realtime=%v batch=%v — SGLang schedules the highest value first by "+
			"default, so this ordering makes batch outrank realtime", sglangRealtime, sglangBatch)
	}
	// And the two engines must disagree, which is the whole point: one shared
	// constant sent to both is an inversion on one of them.
	if (vllmRealtime < vllmBatch) == (sglangRealtime < sglangBatch) {
		t.Error("both engines received the same ordering; they read the field in opposite directions")
	}
}

// TestServiceTierNeverReachesASelfHostedEngine covers both directions of the
// rule: the caller's own field and dorang's class fold.
func TestServiceTierNeverReachesASelfHostedEngine(t *testing.T) {
	for _, kind := range []string{"vllm", "sglang"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, `{"id":"1","object":"chat.completion","model":"m","choices":[]}`)
			p := testProvider(t, f, kind, catalog.APIOpenAIChat)

			c := chatCall(catalog.APIOpenAIChat)
			c.Request.ServiceTier = "priority"
			tg := target(p)
			// Even if an operator configured a tier fold for this kind, it is
			// refused: the field has zero consumers on vLLM and is not a field
			// at all on SGLang's chat route.
			tg.PriorityTier = "flex"

			if res := testBackend("k").Do(context.Background(), tg, c, nil); res.Err != nil {
				t.Fatalf("Do: %v", res.Err)
			}
			sent := decodeJSON(t, f.last().body)
			if v, ok := sent["service_tier"]; ok {
				t.Errorf("service_tier = %v was sent to %s, where it is accepted and ignored", v, kind)
			}
		})
	}
}

// TestServiceTierFoldOnAHostedProvider is the other half: the fold of §7.5 is
// emitted where it means something.
func TestServiceTierFoldOnAHostedProvider(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"1","object":"chat.completion","model":"m","choices":[]}`)
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

	tg := target(p)
	tg.PriorityTier = router.DefaultPriority().Tier("openai", "batch")
	if tg.PriorityTier == "" {
		t.Fatal("the default mapping has no tier for the batch class on kind openai")
	}
	c := chatCall(catalog.APIOpenAIChat)
	// A caller's own claim does not win: §10.5 makes priority an operator grant.
	c.Request.ServiceTier = "priority"

	if res := testBackend("k").Do(context.Background(), tg, c, nil); res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	sent := decodeJSON(t, f.last().body)
	if sent["service_tier"] != tg.PriorityTier {
		t.Errorf("service_tier = %v, want %q — the principal's class, not the caller's claim",
			sent["service_tier"], tg.PriorityTier)
	}
}

// TestToolChoiceNullNormalized is VLLM.md §2.1: an explicit null alongside
// tools passes validation, skips the auto-injection that would have set "auto",
// and then takes a branch that never invokes the tool parser. The client
// receives plain content, no tool calls, and no error.
func TestToolChoiceNullNormalized(t *testing.T) {
	for _, tc := range []struct {
		kind     string
		wantAuto bool
	}{
		{"vllm", true},
		{"sglang", true},
		// A hosted provider is left alone: omitting the key is correct there and
		// the normalization is motivated by an engine defect, not by taste.
		{"openai", false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, `{"id":"1","object":"chat.completion","model":"m","choices":[]}`)
			p := testProvider(t, f, tc.kind, catalog.APIOpenAIChat)

			c := chatCall(catalog.APIOpenAIChat)
			c.Request.Tools = []canonical.Tool{{
				Type: "function", Name: "f", Parameters: json.RawMessage(`{"type":"object"}`),
			}}
			c.Request.ToolChoice = nil // what an explicit null decodes to

			if res := testBackend("k").Do(context.Background(), target(p), c, nil); res.Err != nil {
				t.Fatalf("Do: %v", res.Err)
			}
			sent := decodeJSON(t, f.last().body)
			got, present := sent["tool_choice"]
			if tc.wantAuto {
				if !present || got != "auto" {
					t.Errorf("tool_choice = %v (present=%v), want \"auto\": an absent or null value "+
						"silently disables tool parsing on this engine", got, present)
				}
				return
			}
			if present && got == nil {
				t.Error("an explicit null must never be forwarded")
			}
		})
	}
}

// TestToolChoiceNoneIsLeftAlone guards the normalization's boundary. On SGLang
// "none" strips the tool schemas from the prompt entirely, which is a real
// semantic difference a caller may be relying on.
func TestToolChoiceNoneIsLeftAlone(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"1","object":"chat.completion","model":"m","choices":[]}`)
	p := testProvider(t, f, "sglang", catalog.APIOpenAIChat)

	c := chatCall(catalog.APIOpenAIChat)
	c.Request.Tools = []canonical.Tool{{Type: "function", Name: "f",
		Parameters: json.RawMessage(`{"type":"object"}`)}}
	c.Request.ToolChoice = &canonical.ToolChoice{Mode: canonical.ToolChoiceNone}

	if res := testBackend("k").Do(context.Background(), target(p), c, nil); res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	sent := decodeJSON(t, f.last().body)
	if sent["tool_choice"] != "none" {
		t.Errorf("tool_choice = %v, want none preserved", sent["tool_choice"])
	}
}

// TestEngineReasoningFieldIsRead is VLLM.md §2.5: the response field is
// `reasoning`, not `reasoning_content`. internal/wire/openai models the latter,
// so without the adoption the reasoning text a deployment was configured to
// produce reaches the caller as nothing at all.
func TestEngineReasoningFieldIsRead(t *testing.T) {
	body := `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,
		"message":{"role":"assistant","content":"4","reasoning":"two plus two"},
		"finish_reason":"stop"}]}`

	t.Run("vllm to an anthropic client", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.answer(http.StatusOK, body)
		p := testProvider(t, f, "vllm", catalog.APIOpenAIChat)

		res := testBackend("k").Do(context.Background(), target(p),
			chatCall(catalog.APIAnthropicMessages), nil)
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		out := decodeJSON(t, res.Body)
		content, _ := out["content"].([]any)
		found := false
		for _, b := range content {
			blk, _ := b.(map[string]any)
			if blk["type"] == "thinking" && blk["thinking"] == "two plus two" {
				found = true
			}
		}
		if !found {
			t.Errorf("the engine's reasoning did not reach the caller: %s", res.Body)
		}
	})

	t.Run("vllm to an openai client", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.answer(http.StatusOK, body)
		p := testProvider(t, f, "vllm", catalog.APIOpenAIChat)

		res := testBackend("k").Do(context.Background(), target(p),
			chatCall(catalog.APIOpenAIChat), nil)
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		out := decodeJSON(t, res.Body)
		choices, _ := out["choices"].([]any)
		choice, _ := choices[0].(map[string]any)
		msg, _ := choice["message"].(map[string]any)
		if msg["reasoning_content"] != "two plus two" {
			t.Errorf("reasoning_content = %v, want the engine's reasoning under the name "+
				"clients read", msg["reasoning_content"])
		}
		if _, dup := msg["reasoning"]; dup {
			t.Error("the engine's own spelling was relayed beside the normalized one; a client " +
				"would see the same reasoning twice under two names")
		}
	})

	t.Run("a hosted provider is not reinterpreted", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.answer(http.StatusOK, body)
		p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

		res := testBackend("k").Do(context.Background(), target(p),
			chatCall(catalog.APIOpenAIChat), nil)
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		out := decodeJSON(t, res.Body)
		choices, _ := out["choices"].([]any)
		choice, _ := choices[0].(map[string]any)
		msg, _ := choice["message"].(map[string]any)
		if msg["reasoning_content"] != nil {
			t.Error("an unchecked provider's `reasoning` field was adopted: not recognising " +
				"something is not a reason to reinterpret it")
		}
		if msg["reasoning"] != "two plus two" {
			t.Errorf("the unrecognised field was dropped rather than relayed: %v", msg)
		}
	})
}

// TestSGLangReasoningContentStillWorks: the two engines use each other's
// spelling, so adopting one must not break the other.
func TestSGLangReasoningContentStillWorks(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"1","object":"chat.completion","created":1,"model":"m",
		"choices":[{"index":0,"message":{"role":"assistant","content":"4",
		"reasoning_content":"two plus two"},"finish_reason":"stop"}]}`)
	p := testProvider(t, f, "sglang", catalog.APIOpenAIChat)

	res := testBackend("k").Do(context.Background(), target(p),
		chatCall(catalog.APIOpenAIChat), nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	out := decodeJSON(t, res.Body)
	choices, _ := out["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg["reasoning_content"] != "two plus two" {
		t.Errorf("reasoning_content = %v", msg["reasoning_content"])
	}
}

// TestEngineForKind pins the profile lookup, which is what every normalization
// above is gated on.
func TestEngineForKind(t *testing.T) {
	for kind, want := range map[string]Engine{
		"vllm": EngineVLLM, "sglang": EngineSGLang,
		"openai": EngineNone, "anthropic": EngineNone, "": EngineNone,
	} {
		if got := EngineForKind(kind); got != want {
			t.Errorf("EngineForKind(%q) = %v, want %v", kind, got, want)
		}
	}
}
