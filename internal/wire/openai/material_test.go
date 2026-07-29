package openai

import (
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TestMaterialLossIsADowngradeNotADroppedParam is the OpenAI-side half of the
// same invariant the anthropic package pins.
//
// The chat-completions shape HAS all four of these fields, so the everyday
// crossing loses nothing — asserted first, because a classification that
// refused traffic the target serves correctly would be a worse defect than the
// one it fixes. The loss exists only against a deployment that expresses less
// than the shape it is addressed as, and there each one is a downgrade rather
// than a dropped knob.
func TestMaterialLossIsADowngradeNotADroppedParam(t *testing.T) {
	req := &canonical.Request{
		Model: "m", MaxTokens: ptr(16),
		Messages:    []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")},
		Stop:        []string{"\n\nObservation:"},
		N:           ptr(4),
		Logprobs:    ptr(true),
		ServiceTier: "flex",
	}

	var full canonical.LossReport
	w, err := EncodeRequest(req, &EncodeOptions{Loss: &full})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if full.Lossy() {
		t.Fatalf("this family expresses all four; nothing should be reported: %+v", full)
	}
	if len(w.Stop) != 1 || w.N == nil || *w.N != 4 || w.Logprobs == nil || w.ServiceTier != "flex" {
		t.Fatalf("a parameter the target has did not reach the wire: %+v", w)
	}

	// A deployment declaring less than its family — a self-hosted engine, a
	// gateway in front of one.
	narrowCaps := DefaultCapabilities &^ (canonical.CapStopSequences | canonical.CapMultipleChoices |
		canonical.CapLogprobs | canonical.CapServiceTier)
	var narrow canonical.LossReport
	w2, err := EncodeRequest(req, &EncodeOptions{Capabilities: narrowCaps, Loss: &narrow})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if len(w2.Stop) != 0 || w2.N != nil || w2.Logprobs != nil || w2.ServiceTier != "" {
		t.Fatalf("a parameter reached a target that cannot express it: %+v", w2)
	}
	if !narrow.HasStructural() {
		t.Fatalf("four parameters that change the answer or the price were recorded as knobs: %+v", narrow)
	}
	want := map[string]bool{
		canonical.ConstructStopSequences:   false,
		canonical.ConstructMultipleChoices: false,
		canonical.ConstructLogprobs:        false,
		canonical.ConstructServiceTier:     false,
	}
	for _, d := range narrow.Downgrades {
		if _, ok := want[d.Construct]; ok {
			want[d.Construct] = true
		}
		if d.Detail == "" {
			t.Errorf("%q was reported with no detail; the caller needs the value they wrote", d.Construct)
		}
	}
	for construct, seen := range want {
		if !seen {
			t.Errorf("%q was not reported as a downgrade: %+v", construct, narrow.Downgrades)
		}
	}
	for _, name := range []string{"stop", "n", "logprobs", "service_tier"} {
		if contains(narrow.Dropped, name) {
			t.Errorf("%q is on x-dorang-dropped-params, which is for knobs that did not change "+
				"what the request means", name)
		}
	}
}

// TestSamplingPriorsStayDroppableHere is the counterpart claim, on the family
// where top_k is the everyday case: a knob the target does not have is dropped,
// named, and the request continues.
//
// top_k is the one that makes the argument concrete. It is Anthropic-native and
// absent here, so every Anthropic-shaped request crossing into this family
// loses it. Refusing on "the distribution is not the one requested" would refuse
// that crossing, and it is the same argument that would be made for logit_bias.
func TestSamplingPriorsStayDroppableHere(t *testing.T) {
	req := &canonical.Request{
		Model: "m", MaxTokens: ptr(16),
		Messages:  []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")},
		TopK:      ptr(40),
		Stop:      []string{"\n\nObservation:"},
		LogitBias: map[string]float64{"1734": -100},
	}
	var loss canonical.LossReport
	w, err := EncodeRequest(req, &EncodeOptions{
		Capabilities: DefaultCapabilities &^ canonical.CapLogitBias,
		Loss:         &loss,
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if loss.HasStructural() {
		t.Fatalf("a sampling prior was made refusable: %+v", loss.Downgrades)
	}
	for _, name := range []string{"top_k", "logit_bias"} {
		if !contains(loss.Dropped, name) {
			t.Errorf("%q was dropped without being named: %v", name, loss.Dropped)
		}
	}
	// The constraints the caller stated are untouched. Dropping a prior must
	// never take a postcondition with it.
	if len(w.Stop) != 1 {
		t.Errorf("the stop sequence went with the sampling knobs: %+v", w)
	}
	if w.MaxTokens == nil || *w.MaxTokens != 16 {
		t.Errorf("the output ceiling went with the sampling knobs: %+v", w)
	}
}
