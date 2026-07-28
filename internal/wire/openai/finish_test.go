package openai

import (
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TestFinishReasonTableCoversContract asserts every value COMPATIBILITY 4.1
// names is in the table, and that each maps to a value inside OpenAI's closed
// enumeration.
func TestFinishReasonTableCoversContract(t *testing.T) {
	required := []string{
		"end_turn", "stop_sequence", "max_tokens", "tool_use", "refusal",
		"COMPLETE", "ERROR", "ERROR_TOXIC", "eos_token", "eos", "STOP",
		"SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "network_error", "sensitive", "guardrail_intervened",
	}
	if len(required) != 20 {
		t.Fatalf("the contract lists 20 values, the test lists %d", len(required))
	}
	allowed := map[string]bool{
		FinishStop: true, FinishLength: true, FinishToolCalls: true,
		FinishContentFilter: true, FinishFunctionCall: true, "": true,
	}
	for _, native := range required {
		if _, ok := LookupStopReason(native); !ok {
			t.Errorf("%q is not in the normalization table (COMPATIBILITY 4.1)", native)
			continue
		}
		var warned []Warning
		finish, _ := NormalizeFinishReason(native, func(w Warning) { warned = append(warned, w) })
		if !allowed[finish] {
			t.Errorf("%q normalized to %q, which is outside OpenAI's enumeration", native, finish)
		}
		if len(warned) != 0 {
			t.Errorf("%q is in the table but warned: %+v", native, warned)
		}
	}
}

// TestFinishReasonSemantics pins the specific mappings that are choices rather
// than mechanics, so a change to any of them is deliberate.
func TestFinishReasonSemantics(t *testing.T) {
	cases := map[string]string{
		"end_turn":             FinishStop,
		"stop_sequence":        FinishStop,
		"max_tokens":           FinishLength,
		"tool_use":             FinishToolCalls,
		"refusal":              FinishContentFilter,
		"COMPLETE":             FinishStop,
		"ERROR":                FinishStop,
		"ERROR_TOXIC":          FinishContentFilter,
		"eos_token":            FinishStop,
		"eos":                  FinishStop,
		"STOP":                 FinishStop,
		"SAFETY":               FinishContentFilter,
		"RECITATION":           FinishContentFilter,
		"BLOCKLIST":            FinishContentFilter,
		"PROHIBITED_CONTENT":   FinishContentFilter,
		"SPII":                 FinishContentFilter,
		"IMAGE_SAFETY":         FinishContentFilter,
		"network_error":        FinishStop,
		"sensitive":            FinishContentFilter,
		"guardrail_intervened": FinishContentFilter,
	}
	for native, want := range cases {
		got, _ := NormalizeFinishReason(native, nil)
		if got != want {
			t.Errorf("%q -> %q, want %q", native, got, want)
		}
	}
}

// TestUnmappedFinishReasonWarnsAndBecomesStop covers COMPATIBILITY 4.2.
func TestUnmappedFinishReasonWarnsAndBecomesStop(t *testing.T) {
	var warned []Warning
	finish, preserve := NormalizeFinishReason("SOME_NEW_PROVIDER_VALUE",
		func(w Warning) { warned = append(warned, w) })
	if finish != FinishStop {
		t.Errorf("unmapped value became %q, want %q", finish, FinishStop)
	}
	if finish == "SOME_NEW_PROVIDER_VALUE" {
		t.Error("an unmapped value was passed through raw")
	}
	if preserve != "SOME_NEW_PROVIDER_VALUE" {
		t.Errorf("the original was not preserved: %q", preserve)
	}
	if !hasWarning(warned, WarnUnmappedFinishReason) {
		t.Fatalf("no warning was logged: %+v", warned)
	}
	if warned[0].Detail != "SOME_NEW_PROVIDER_VALUE" {
		t.Errorf("warning did not carry the offending value: %+v", warned[0])
	}
}

// TestNativeFinishReasonPreserved covers COMPATIBILITY 4.3 end to end: the
// original survives a full response encode and comes back on decode.
func TestNativeFinishReasonPreserved(t *testing.T) {
	resp := &canonical.Response{
		ID: testID, Model: testModel, Created: testCreated,
		Choices: []canonical.Choice{{
			Message:          canonical.TextMessage(canonical.RoleAssistant, "no"),
			StopReason:       canonical.StopRefusal,
			NativeStopReason: "refusal",
		}},
	}
	b, err := MarshalResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"finish_reason":"content_filter"`) {
		t.Errorf("refusal did not collapse to content_filter: %s", b)
	}
	if !strings.Contains(string(b), `"native_finish_reason":"refusal"`) {
		t.Fatalf("the native reason was not preserved out of band: %s", b)
	}

	back, err := DecodeResponse(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if back.Choices[0].StopReason != canonical.StopRefusal {
		t.Errorf("second hop lost the rich reason: %q", back.Choices[0].StopReason)
	}
}

// TestRichStopReasonIsAStructuralLoss checks that the collapse is reported when
// the client's view cannot express it.
func TestRichStopReasonIsAStructuralLoss(t *testing.T) {
	resp := &canonical.Response{
		Choices: []canonical.Choice{{
			Message:    canonical.TextMessage(canonical.RoleAssistant, ""),
			StopReason: canonical.StopSafety,
		}},
	}
	if !resp.RequiredCapabilities().Has(canonical.CapRichStopReasons) {
		t.Fatal("a safety stop must require CapRichStopReasons")
	}
	var loss canonical.LossReport
	if _, err := EncodeResponse(resp, &ResponseOptions{
		Capabilities: StrictCapabilities,
		Loss:         &loss,
	}); err != nil {
		t.Fatal(err)
	}
	if !contains(loss.Constructs(), canonical.ConstructRichStopReason) {
		t.Fatalf("the collapse was not reported: %+v", loss.Downgrades)
	}
}

// TestFinishReasonNeverEscapesTheEnumeration is the property behind 4.2.
func TestFinishReasonNeverEscapesTheEnumeration(t *testing.T) {
	allowed := map[string]bool{
		FinishStop: true, FinishLength: true, FinishToolCalls: true,
		FinishContentFilter: true, FinishFunctionCall: true,
	}
	for native := range finishTable {
		got, _ := NormalizeFinishReason(native, nil)
		if got != "" && !allowed[got] {
			t.Errorf("table entry %q produces %q", native, got)
		}
	}
	for _, r := range []canonical.StopReason{
		canonical.StopUnspecified, canonical.StopEndTurn, canonical.StopMaxTokens,
		canonical.StopToolUse, canonical.StopStopSequence, canonical.StopContentFilter,
		canonical.StopRefusal, canonical.StopSafety, canonical.StopRecitation,
		canonical.StopError, canonical.StopFunctionCall, canonical.StopPauseTurn,
		canonical.StopReason("something-invented"),
	} {
		if got := FinishReasonOf(r); !allowed[got] {
			t.Errorf("FinishReasonOf(%q) = %q", r, got)
		}
	}
}
