package anthropic

import (
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TestMaterialLossIsADowngradeNotADroppedParam pins which SIDE of §10.1's split
// each parameter lands on, at the layer that records it.
//
// The classification and the recording have to agree or the split does nothing:
// a construct filed as structural but recorded with DropParam produces a 200
// with a header, which is the behaviour the classification was changed to stop.
// This asserts both directions at once — the four material parameters appear in
// Downgrades and NOT in Dropped, and the sampling knobs the other way round.
func TestMaterialLossIsADowngradeNotADroppedParam(t *testing.T) {
	req := &canonical.Request{
		Model: "m", MaxTokens: ptr(16),
		Messages: []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")},

		// Material: this family has none of these fields.
		N:           ptr(4),
		Logprobs:    ptr(true),
		ServiceTier: "flex",

		// Droppable: priors over sampling, which this family also lacks.
		Seed:             ptr(int64(9)),
		LogitBias:        map[string]float64{"1734": -100},
		FrequencyPenalty: ptr(0.5),
	}
	var loss canonical.LossReport
	if _, err := EncodeRequest(req, &EncodeOptions{Loss: &loss}); err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	if !loss.HasStructural() {
		t.Fatal("n, logprobs and service_tier were all recorded as knobs dorang did not apply; " +
			"each of them changes the answer or the price, so each is a refusable downgrade")
	}
	byConstruct := map[string]string{}
	for _, d := range loss.Downgrades {
		byConstruct[d.Construct] = d.Detail
	}
	for construct, wantDetail := range map[string]string{
		canonical.ConstructMultipleChoices: "4",
		canonical.ConstructLogprobs:        "logprobs",
		canonical.ConstructServiceTier:     "flex",
	} {
		detail, ok := byConstruct[construct]
		if !ok {
			t.Errorf("%q was not reported as a downgrade: %+v", construct, loss.Downgrades)
			continue
		}
		if !strings.Contains(detail, wantDetail) {
			t.Errorf("%q detail = %q, does not carry the value the caller wrote", construct, detail)
		}
	}
	for _, name := range []string{"n", "logprobs", "service_tier"} {
		if contains(loss.Dropped, name) {
			t.Errorf("%q is listed as a dropped parameter; x-dorang-dropped-params is for knobs "+
				"that did not change the request's meaning, and this one did", name)
		}
	}

	// The other side of the split, unchanged and asserted so it stays that way.
	for _, name := range []string{"seed", "logit_bias", "frequency_penalty"} {
		if !contains(loss.Dropped, name) {
			t.Errorf("%q was dropped without being named: %v", name, loss.Dropped)
		}
	}
	for _, d := range loss.Downgrades {
		switch d.Construct {
		case "seed", "logit_bias", "penalties", "top_k":
			t.Errorf("%q was reported as a refusable downgrade; a prior over sampling promises "+
				"no distribution and its absence is indistinguishable from a re-draw", d.Construct)
		}
	}
}

// TestStopSequencesAreExpressibleHereAndReportedWhenTheyAreNot.
//
// The Messages family HAS stop_sequences, so the everyday crossing loses
// nothing and must report nothing — a classification that refused it here would
// refuse traffic that works. The loss only exists against a deployment that
// expresses less than the shape it is addressed as, and there it is a
// downgrade, because a stop sequence states when the text stops.
func TestStopSequencesAreExpressibleHereAndReportedWhenTheyAreNot(t *testing.T) {
	req := &canonical.Request{
		Model: "m", MaxTokens: ptr(16),
		Messages: []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")},
		Stop:     []string{"\n\nObservation:"},
	}

	var full canonical.LossReport
	w, err := EncodeRequest(req, &EncodeOptions{Loss: &full})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if len(w.StopSequences) != 1 || w.StopSequences[0] != "\n\nObservation:" {
		t.Fatalf("stop_sequences = %v; this family has the field", w.StopSequences)
	}
	if full.Lossy() {
		t.Fatalf("an expressible stop sequence was reported as a loss: %+v", full)
	}

	var narrow canonical.LossReport
	w2, err := EncodeRequest(req, &EncodeOptions{
		Capabilities: DefaultCapabilities &^ canonical.CapStopSequences,
		Loss:         &narrow,
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if len(w2.StopSequences) != 0 {
		t.Fatalf("stop_sequences reached a target that cannot express them: %v", w2.StopSequences)
	}
	if !narrow.HasStructural() {
		t.Fatalf("a dropped stop sequence was reported as a knob: %+v", narrow)
	}
	if got := narrow.Constructs(); len(got) != 1 || got[0] != canonical.ConstructStopSequences {
		t.Fatalf("constructs = %v, want just %q", got, canonical.ConstructStopSequences)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
