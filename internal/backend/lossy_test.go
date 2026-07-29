package backend

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The answer every fake upstream in this file gives back. One choice, whatever
// was asked for — which is the point of the `n` case.
const anthropicAnswer = `{"id":"msg-1","type":"message","role":"assistant","model":"upstream-model",
	"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
	"usage":{"input_tokens":7,"output_tokens":2}}`

// lossyCall is an OpenAI-shaped client request aimed at an Anthropic-shaped
// deployment — the crossing where every construct in this file is actually
// lost — with one material parameter set by the caller.
func lossyCall(mutate func(*canonical.Request)) *Call {
	c := chatCall(catalog.APIOpenAIChat)
	mutate(c.Request)
	return c
}

func intp(v int) *int    { return &v }
func boolp(v bool) *bool { return &v }

// materialCases is DESIGN §10.1's material set, one row each, with the target
// that cannot express it.
//
// Three of the four are unexpressible by the Messages family outright, so those
// rows use the family's own declared set and are what a real deployment does
// today. Stop sequences DO exist in both families, so that row declares a
// reduced deployment — which is the case the classification exists for: a
// self-hosted engine, or a family added later, that expresses less than the
// shape it is addressed as.
var materialCases = []struct {
	name      string
	mutate    func(*canonical.Request)
	construct string
	param     string
	// caps overrides the deployment's declared set; zero means the family's.
	caps canonical.Capability
	// absent is a key that must not appear in the body sent upstream once the
	// caller has consented to the loss.
	absent string
}{
	{
		name:      "stop sequences decide WHEN generation ends",
		mutate:    func(r *canonical.Request) { r.Stop = []string{"\n\nObservation:"} },
		construct: canonical.ConstructStopSequences,
		param:     "stop",
		caps:      anthropic.DefaultCapabilities &^ canonical.CapStopSequences,
		absent:    "stop_sequences",
	},
	{
		name:      "n decides how many choices come back",
		mutate:    func(r *canonical.Request) { r.N = intp(4) },
		construct: canonical.ConstructMultipleChoices,
		param:     "n",
		absent:    "n",
	},
	{
		name:      "logprobs is a member of the response that was asked for",
		mutate:    func(r *canonical.Request) { r.Logprobs = boolp(true) },
		construct: canonical.ConstructLogprobs,
		param:     "logprobs",
		absent:    "logprobs",
	},
	{
		name:      "service_tier decides the price band",
		mutate:    func(r *canonical.Request) { r.ServiceTier = "flex" },
		construct: canonical.ConstructServiceTier,
		param:     "service_tier",
		absent:    "service_tier",
	},
}

// TestMaterialLossIsRefusedAndTheRefusalNamesTheConstruct is the whole point of
// DESIGN §10.1, asserted on what the CLIENT gets rather than on a capability
// bit.
//
// Each of these four used to return 200 with the parameter listed in
// x-dorang-dropped-params, which is not consent — it is a header nobody parses,
// carrying news that the answer is not the one that was asked for. The caller
// paid for tokens past a stop sequence, indexed choices[3] on a one-choice
// answer, read a null logprobs member, or was billed in a band they did not
// select, and in every case the exchange looked like a success.
func TestMaterialLossIsRefusedAndTheRefusalNamesTheConstruct(t *testing.T) {
	for _, tc := range materialCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, anthropicAnswer)
			p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)
			tgt := target(p)
			tgt.Capabilities = tc.caps

			res := testBackend("sk-ant").Do(context.Background(), tgt, lossyCall(tc.mutate), nil)

			if res.Err == nil {
				t.Fatalf("the client got a %d and body %s: dropping %s changes the answer or the "+
					"price, and a 200 says it did not", res.Status, res.Body, tc.param)
			}
			if res.Err.Status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", res.Err.Status)
			}
			if res.Err.Code != CodeUnsupportedConstruct {
				t.Errorf("code = %q, want %q: a client branches on the code, not on the sentence",
					res.Err.Code, CodeUnsupportedConstruct)
			}
			// The construct id is the retry vocabulary. A refusal that does not
			// carry it leaves the caller with an error and no way to act on it.
			if !strings.Contains(res.Err.Message, tc.construct) {
				t.Errorf("the refusal does not name the construct %q: %q", tc.construct, res.Err.Message)
			}
			if !strings.Contains(res.Err.Message, "x-dorang-allow-lossy") {
				t.Errorf("the refusal does not name the opt-in that would let it through: %q", res.Err.Message)
			}
			if res.Err.Param == nil || *res.Err.Param != tc.param {
				t.Errorf("param = %v, want %q: this half of the structural set IS a request field, "+
					"unlike thinking_block, so a field-locating SDK can point at it", res.Err.Param, tc.param)
			}
			// And nothing was spent finding out.
			if f.count() != 0 {
				t.Errorf("the upstream was called %d times for a request dorang already knew it "+
					"could not express", f.count())
			}
			if res.Body != nil {
				t.Errorf("a refused request still produced an answer body: %s", res.Body)
			}
		})
	}
}

// TestAllowLossyTurnsTheRefusalBackIntoTheOldBehaviour is the other half: the
// opt-in of §10.1 exists so a caller who knows can proceed, and it is per
// construct, so consent is never blanket.
//
// This is also where the classification's cost is paid honestly: with the
// header, the caller gets exactly what they got before — the request upstream
// without the parameter, and a 200 — and the only difference is that they said
// so.
func TestAllowLossyTurnsTheRefusalBackIntoTheOldBehaviour(t *testing.T) {
	for _, tc := range materialCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, anthropicAnswer)
			p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)
			tgt := target(p)
			tgt.Capabilities = tc.caps

			bit, ok := canonical.ParseCapability(tc.construct)
			if !ok {
				t.Fatalf("x-dorang-allow-lossy cannot name %q, so the refusal is a dead end", tc.construct)
			}
			c := lossyCall(tc.mutate)
			c.AllowLossy = bit

			res := testBackend("sk-ant").Do(context.Background(), tgt, c, nil)
			if res.Err != nil {
				t.Fatalf("an explicit opt-in was still refused: %+v", res.Err)
			}
			if f.count() != 1 {
				t.Fatalf("the upstream was called %d times, want 1", f.count())
			}
			sent := decodeJSON(t, f.last().body)
			if _, present := sent[tc.absent]; present {
				t.Errorf("%q reached the upstream, which cannot express it: %s", tc.absent, f.last().body)
			}
			// The caller consented to a loss and got an answer, so the answer
			// must actually be one.
			out := decodeJSON(t, res.Body)
			if out["object"] != "chat.completion" {
				t.Errorf("answer is not rendered in the caller's protocol: %s", res.Body)
			}
		})
	}
}

// TestConsentedMultipleChoicesReturnsTheOneChoiceItCanNames what the opt-in
// actually buys, for the row where it is visible: a caller who asked for four
// choices and consented gets one.
//
// It is the concrete form of the thing the header used to claim without saying:
// `n: 4` in, `len(choices) == 1` out. A client that consented can handle it; a
// client that did not is the one this classification protects.
func TestConsentedMultipleChoicesReturnsTheOneChoiceItCan(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, anthropicAnswer)
	p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)

	c := lossyCall(func(r *canonical.Request) { r.N = intp(4) })
	c.AllowLossy = canonical.CapMultipleChoices
	res := testBackend("sk-ant").Do(context.Background(), target(p), c, nil)
	if res.Err != nil {
		t.Fatalf("Do: %+v", res.Err)
	}
	out := decodeJSON(t, res.Body)
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %d; the deployment has no n and cannot invent three more", len(choices))
	}
}

// TestServiceTierAutoIsNotAMaterialLoss is the honest limit of the price-band
// argument, and the reason the classification is not a blanket refusal on the
// field.
//
// "auto" delegates the band to the provider. Omitting the field delegates the
// band to the provider. Refusing a request because dorang would do exactly what
// the caller asked for would be a false refusal, and false refusals are how a
// safety mechanism becomes noise callers route around.
func TestServiceTierAutoIsNotAMaterialLoss(t *testing.T) {
	for _, tier := range []string{"auto", "AUTO"} {
		f := newFakeUpstream(t)
		f.answer(http.StatusOK, anthropicAnswer)
		p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)

		res := testBackend("sk-ant").Do(context.Background(), target(p),
			lossyCall(func(r *canonical.Request) { r.ServiceTier = tier }), nil)
		if res.Err != nil {
			t.Fatalf("service_tier %q was refused, but it selects nothing: %+v", tier, res.Err)
		}
		if f.count() != 1 {
			t.Fatalf("service_tier %q: upstream calls = %d, want 1", tier, f.count())
		}
	}
}

// TestSingleChoiceIsNotAMaterialLoss is the same limit for `n`, and it is the
// specific cost the reclassification was weighed against: a client that always
// sends `n: 1` — which is most of them, because SDKs fill the field — must not
// start getting 400s for a parameter that changed nothing.
func TestSingleChoiceIsNotAMaterialLoss(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, anthropicAnswer)
	p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)

	res := testBackend("sk-ant").Do(context.Background(), target(p),
		lossyCall(func(r *canonical.Request) { r.N = intp(1) }), nil)
	if res.Err != nil {
		t.Fatalf("n: 1 was refused, but one choice is what the target returns anyway: %+v", res.Err)
	}
	if f.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", f.count())
	}
}

// TestLogitBiasStaysDroppableBecauseTheAnswerIsTheSameKind is the harder half of
// the review: the case for LEAVING a capability droppable, asserted rather than
// asserted-by-omission.
//
// logit_bias, seed and the penalties are priors over sampling. No vendor
// promises a distribution — the same request with the same bias returns
// different text on every call — so there is no postcondition for a dropped
// bias to violate, and the answer that comes back is indistinguishable from an
// ordinary re-draw of the request that carried it. What the caller is entitled
// to is that nothing they STATED about the answer changed, and that they are
// told.
//
// Both halves are asserted here:
//
//  1. the body that reaches the upstream is byte-identical to the body for the
//     same request with the knob removed — dorang neither approximated the
//     prior nor compensated for it elsewhere, which is the failure that would
//     make "materially the same" false;
//  2. every constraint the caller stated — the messages, the output ceiling,
//     the temperature, the stop sequence, the tool set — is still on the wire;
//  3. the parameter is named to the caller, by the same expression
//     internal/router fills x-dorang-dropped-params from.
//
// Reclassifying these would also commit dorang to refusing top_k on every
// crossing into the OpenAI family, which is an everyday, harmless conversion.
func TestLogitBiasStaysDroppableBecauseTheAnswerIsTheSameKind(t *testing.T) {
	// Everything the caller STATED about the answer, carried on every case.
	constraints := func(r *canonical.Request) {
		r.Stop = []string{"\n\nObservation:"}
		r.Temperature = float64p(0.2)
		r.Tools = []canonical.Tool{{Name: "lookup", Description: "d"}}
	}
	cases := []struct {
		name  string
		knob  func(*canonical.Request)
		param string
	}{
		{"logit_bias", func(r *canonical.Request) {
			r.LogitBias = map[string]float64{"1734": -100, "99": 0.5}
		}, "logit_bias"},
		{"seed", func(r *canonical.Request) { r.Seed = int64p(7) }, "seed"},
		{"penalties", func(r *canonical.Request) {
			r.FrequencyPenalty, r.PresencePenalty = float64p(0.5), float64p(0.1)
		}, "frequency_penalty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withKnob := sendAndCapture(t, func(r *canonical.Request) { constraints(r); tc.knob(r) })
			without := sendAndCapture(t, constraints)

			if !bytes.Equal(withKnob, without) {
				t.Fatalf("dropping %s changed the request in some OTHER way, so \"the answer is "+
					"materially the same\" is not established:\n with: %s\nwithout: %s",
					tc.param, withKnob, without)
			}

			// The constraints the caller stated all survive.
			sent := decodeJSON(t, withKnob)
			for _, key := range []string{"messages", "max_tokens", "temperature", "stop_sequences", "tools"} {
				if _, ok := sent[key]; !ok {
					t.Errorf("%q did not reach the upstream: dropping a sampling prior must not "+
						"drop a stated constraint with it: %s", key, withKnob)
				}
			}

			// And the caller is told. This is the exact expression internal/router
			// builds x-dorang-dropped-params from (§10.4).
			req := lossyCall(func(r *canonical.Request) { constraints(r); tc.knob(r) }).Request
			dropped := anthropic.DefaultCapabilities.Missing(req.RequiredCapabilities()).Droppable().Params()
			if !containsStr(dropped, tc.param) {
				t.Errorf("x-dorang-dropped-params would not name %q: got %v", tc.param, dropped)
			}
		})
	}
}

// sendAndCapture runs one chat call against a Messages-shaped fake and returns
// the bytes the upstream received.
func sendAndCapture(t *testing.T, mutate func(*canonical.Request)) []byte {
	t.Helper()
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, anthropicAnswer)
	p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)

	res := testBackend("sk-ant").Do(context.Background(), target(p), lossyCall(mutate), nil)
	if res.Err != nil {
		t.Fatalf("a droppable knob must never refuse the request: %+v", res.Err)
	}
	if f.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", f.count())
	}
	return f.last().body
}

func float64p(v float64) *float64 { return &v }
func int64p(v int64) *int64       { return &v }

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
