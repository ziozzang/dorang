package backend

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The three constructs Gemini accepts and this encoder used to withhold.
//
// [GeminiCapabilities]'s comment carried a group headed "expressible by the
// protocol, not by this encoder", and the rule beside it: "the set has to
// describe the code that converts, not the protocol somebody could convert to.
// Adding the field is what earns the bit, in that order." This is that order's
// second half for logprobs and the two penalties.
//
// CapLogprobs is [canonical.Material], so while the field went unwritten dorang
// REFUSED requests Gemini can serve — and told the caller the model could not do
// it, which was never true.
func TestGeminiWritesTheFieldsItsProtocolDefines(t *testing.T) {
	f := 0.7
	p := -0.3
	yes := true
	three := 3
	req := &canonical.Request{
		Model:            "gemini-x",
		Messages:         []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")},
		FrequencyPenalty: &f,
		PresencePenalty:  &p,
		Logprobs:         &yes,
		TopLogprobs:      &three,
	}
	gc := geminiConfig(req)
	if gc == nil {
		t.Fatal("generationConfig came out empty for a request carrying four of its fields")
	}
	if gc.FrequencyPenalty == nil || *gc.FrequencyPenalty != f {
		t.Errorf("frequencyPenalty = %v, want %v — the protocol names it exactly", gc.FrequencyPenalty, f)
	}
	if gc.PresencePenalty == nil || *gc.PresencePenalty != p {
		t.Errorf("presencePenalty = %v, want %v", gc.PresencePenalty, p)
	}
	if gc.ResponseLogprobs == nil || !*gc.ResponseLogprobs {
		t.Error("responseLogprobs was not turned on for a request that asked for logprobs")
	}
	if gc.Logprobs == nil || *gc.Logprobs != three {
		t.Errorf("logprobs = %v, want %d — this field is HOW MANY alternatives, which is "+
			"OpenAI's top_logprobs", gc.Logprobs, three)
	}

	b, err := json.Marshal(gc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{"frequencyPenalty", "presencePenalty", "responseLogprobs", "logprobs"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s never reached the wire:\n%s", want, b)
		}
	}
}

// Asking only for logprobs turns them on without claiming a count.
//
// The two halves are separate fields here, and inventing a count for a caller
// who did not ask for one would be dorang choosing how many alternatives they
// are billed for.
func TestGeminiLogprobsWithoutACountSendsNoCount(t *testing.T) {
	yes := true
	gc := geminiConfig(&canonical.Request{
		Model:    "gemini-x",
		Messages: []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")},
		Logprobs: &yes,
	})
	if gc == nil || gc.ResponseLogprobs == nil || !*gc.ResponseLogprobs {
		t.Fatal("responseLogprobs was not set")
	}
	if gc.Logprobs != nil {
		t.Errorf("logprobs = %v for a caller who asked for no count; the number of "+
			"alternatives is not dorang's to choose", *gc.Logprobs)
	}
}

// The capability bits follow the fields, so the gate stops refusing.
//
// A bit claimed without the field is the failure this adapter's own comment
// warns about; a field written without the bit is this one — §10.1 refuses a
// request the adapter would have converted correctly.
func TestGeminiCapabilitiesMatchWhatTheEncoderWrites(t *testing.T) {
	for _, c := range []struct {
		bit  canonical.Capability
		name string
	}{
		{canonical.CapLogprobs, "CapLogprobs"},
		{canonical.CapPenalties, "CapPenalties"},
	} {
		if GeminiCapabilities&c.bit == 0 {
			t.Errorf("%s is withheld while geminiConfig writes its field, so §10.1 refuses "+
				"a request this adapter converts correctly", c.name)
		}
	}
}

// An empty request still produces no generationConfig.
//
// The new fields must not make the object appear on every request: an empty
// generationConfig is a change to the bytes on the wire, and the bytes are what
// a prompt cache keys on.
func TestGeminiConfigStaysAbsentWhenNothingWasAsked(t *testing.T) {
	if gc := geminiConfig(&canonical.Request{
		Model:    "gemini-x",
		Messages: []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")},
	}); gc != nil {
		t.Errorf("generationConfig = %+v for a request that set none of it", gc)
	}
}
