package backend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// providers[].params.drop was documented as a key that "loads and does
// nothing", and the importer wrote a real incumbent setting into it and
// reported success. An operator whose upstream answers 400 to max_tokens
// migrated, saw no warning, and found out in production.
//
// These tests assert the CONSEQUENCE. A test that loads a configuration and
// reads the field back proves only that YAML decoding works, which is exactly
// what was true for the whole time the field did nothing.

// sentBody returns the JSON object the upstream actually received.
func sentBody(t *testing.T, f *fakeUpstream) map[string]json.RawMessage {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(f.last().body, &got); err != nil {
		t.Fatalf("upstream body is not a JSON object: %v (%s)", err, f.last().body)
	}
	return got
}

// TestOperatorDroppedParamNeverReachesTheUpstream is the incumbent's own case:
// four model groups carrying additional_drop_params: [max_tokens,
// max_completion_tokens], on an upstream that refuses the field.
func TestOperatorDroppedParamNeverReachesTheUpstream(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(200, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)

	p, err := NewProvider(Spec{
		Name: "p1", Kind: "openai", API: catalog.APIOpenAIChat, BaseURL: f.srv.URL,
		DropParams: []string{"max_tokens", "temperature"},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	c := chatCall(catalog.APIOpenAIChat)
	temp := 0.7
	c.Request.Temperature = &temp
	seed := int64(9)
	c.Request.Seed = &seed

	res, loss := runWithAccepted(t, c, p)
	if res.Err != nil {
		t.Fatalf("call: %v", res.Err)
	}

	got := sentBody(t, f)
	for _, name := range []string{"max_tokens", "max_completion_tokens", "temperature"} {
		if _, present := got[name]; present {
			t.Errorf("the operator asked to drop %q and it reached the upstream: %s", name, f.last().body)
		}
	}
	// A parameter NOT on the list is untouched, or the mechanism is a mute
	// button rather than a drop list.
	if _, present := got["seed"]; !present {
		t.Errorf("seed was not on the drop list and did not reach the upstream: %s", f.last().body)
	}

	// §10.3: every removal is reported. A drop nobody can see is the same
	// silence this closes, moved one layer down.
	dropped := loss.dropped
	if !hasAll(dropped, "max_tokens", "temperature") {
		t.Errorf("the removals were not reported: x-dorang-dropped-params would carry %v", dropped)
	}
	if hasAll(dropped, "max_completion_tokens") {
		t.Errorf("max_completion_tokens was reported dropped, and the caller never sent it: %v", dropped)
	}
}

// TestDroppingAMaterialParamServesInsteadOfRefusing. stop, n, logprobs and
// service_tier are canonical.Material: a deployment that cannot express them
// answers 400 rather than losing them quietly. An operator who has NAMED one in
// the drop list has already answered that question for this endpoint, so the
// request must be served with the field removed and the removal reported —
// never refused by the gate the operator was overriding.
func TestDroppingAMaterialParamServesInsteadOfRefusing(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(200, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)

	p, err := NewProvider(Spec{
		Name: "p1", Kind: "openai", API: catalog.APIOpenAIChat, BaseURL: f.srv.URL,
		DropParams: []string{"stop"},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	c := chatCall(catalog.APIOpenAIChat)
	c.Request.Stop = []string{"END"}

	// A deployment whose capability set does not include stop sequences: without
	// the drop this is the 400 that refuseMaterialLoss exists to produce.
	tg := target(p)
	tg.Capabilities = openaiCapsWithout(canonical.CapStopSequences)

	got := &acceptedLoss{}
	c.Accepted = func(l *canonical.LossReport) {
		got.dropped = append([]string(nil), l.Dropped...)
	}
	res := testBackend("k").Do(context.Background(), tg, c, newCountingWriter())
	if res.Err != nil {
		t.Fatalf("the operator's own drop produced a refusal: %v", res.Err)
	}
	if _, present := sentBody(t, f)["stop"]; present {
		t.Errorf("stop reached the upstream: %s", f.last().body)
	}
	if !hasAll(got.dropped, "stop") {
		t.Errorf("the removal was not reported: %v", got.dropped)
	}
}

// TestDropReachesAnUnmodelledPassThroughField. The incumbent's lists are full of
// vendor knobs dorang has no struct field for. They cross a same-family hop in
// canonical.Request.Extra, so a drop that only covered dorang's own vocabulary
// would forward exactly the field an operator is most likely to be fighting.
func TestDropReachesAnUnmodelledPassThroughField(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(200, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)

	p, err := NewProvider(Spec{
		Name: "p1", Kind: "openai", API: catalog.APIOpenAIChat, BaseURL: f.srv.URL,
		DropParams: []string{"repetition_penalty"},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	c := chatCall(catalog.APIOpenAIChat)
	c.Request.Extra = map[string]json.RawMessage{
		"repetition_penalty": json.RawMessage("1.1"),
		"guided_regex":       json.RawMessage(`"a+"`),
	}

	res, loss := runWithAccepted(t, c, p)
	if res.Err != nil {
		t.Fatalf("call: %v", res.Err)
	}
	got := sentBody(t, f)
	if _, present := got["repetition_penalty"]; present {
		t.Errorf("the dropped pass-through field reached the upstream: %s", f.last().body)
	}
	if _, present := got["guided_regex"]; !present {
		t.Errorf("an undropped pass-through field was lost: %s", f.last().body)
	}
	if !hasAll(loss.dropped, "repetition_penalty") {
		t.Errorf("the removal was not reported: %v", loss.dropped)
	}
	// The caller's own map must survive: a fail-back hop re-derives from it.
	if _, still := c.Request.Extra["repetition_penalty"]; !still {
		t.Error("the drop edited the caller's request in place; the next fail-back hop would " +
			"inherit a request the caller did not send")
	}
}

// TestDropOfARequiredFieldIsRefusedAtStartUp. Anthropic's messages API requires
// max_tokens, and internal/wire/anthropic refills it from the model catalog when
// the caller named none. So the drop would apply and the field would come
// straight back — a setting that loads, looks applied, and changes nothing,
// which is the disposition this whole mechanism exists to stop being.
func TestDropOfARequiredFieldIsRefusedAtStartUp(t *testing.T) {
	f := newFakeUpstream(t)
	for _, name := range []string{"max_tokens", "max_completion_tokens"} {
		_, err := NewProvider(Spec{
			Name: "p1", Kind: "anthropic", API: catalog.APIAnthropicMessages, BaseURL: f.srv.URL,
			DropParams: []string{name},
		})
		if err == nil {
			t.Fatalf("dropping %q on an anthropic provider was accepted, and it takes effect on nothing", name)
		}
		// The refusal has to name the working alternative or it is a wall.
		if !strings.Contains(err.Error(), "max_output_tokens") {
			t.Errorf("the refusal names no alternative: %v", err)
		}
	}
	// The same name on a shape that does not require the field is fine.
	if _, err := NewProvider(Spec{
		Name: "p1", Kind: "openai", API: catalog.APIOpenAIChat, BaseURL: f.srv.URL,
		DropParams: []string{"max_tokens"},
	}); err != nil {
		t.Errorf("dropping max_tokens on an OpenAI-shaped provider was refused: %v", err)
	}
}

// TestUndroppableNamesAreRefusedAtStartUp. §10.1 already answers "this
// deployment cannot express the construct" — it refuses, and
// x-dorang-allow-lossy is how a caller consents. An operator setting that could
// delete the caller's tools and answer 200 would be a second, disagreeing answer
// to the same question.
func TestUndroppableNamesAreRefusedAtStartUp(t *testing.T) {
	f := newFakeUpstream(t)
	for _, name := range []string{"tools", "tool_choice", "messages", "model", "stream", "response_format"} {
		_, err := NewProvider(Spec{
			Name: "p1", Kind: "openai", API: catalog.APIOpenAIChat, BaseURL: f.srv.URL,
			DropParams: []string{name},
		})
		if err == nil {
			t.Errorf("params.drop accepted %q", name)
		}
	}
}

// openaiCapsWithout is the OpenAI wire set minus one bit.
func openaiCapsWithout(c canonical.Capability) canonical.Capability {
	return CapabilitiesForAPI(catalog.APIOpenAIChat) &^ c
}

func hasAll(list []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, v := range list {
			if v == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
