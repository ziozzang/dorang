package openai

import (
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The legacy text-completions surface.
//
// Every assertion here is a byte comparison rather than a decode-and-compare,
// for the reason COMPATIBILITY 2.1a gives: "byte-for-byte" is meaningless
// without naming the serializer, and a struct that decodes its own output can
// still emit a shape no other server emits.

func TestCompletionRequestGolden(t *testing.T) {
	body := []byte(`{"model":"qwen3.5:397b","prompt":"Once upon","max_tokens":16,"temperature":0.2,"stop":["\n"],"echo":true}`)
	req, err := DecodeCompletionRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "qwen3.5:397b" {
		t.Errorf("model %q — a colon-bearing name must survive whole (DESIGN §2.1)", req.Model)
	}
	if req.Prompt == nil {
		t.Fatal("the prompt did not reach the neutral form")
	}
	if text, exact := req.Prompt.Text(); text != "Once upon" || !exact {
		t.Errorf("prompt %q exact=%v", text, exact)
	}
	// The prompt is ALSO a user turn, which is what makes a legacy request
	// routable to a family with no completions surface at all.
	if len(req.Messages) != 1 || req.Messages[0].Content.Flatten() != "Once upon" {
		t.Errorf("messages %+v — the prompt must also be a user turn", req.Messages)
	}
	// echo has no neutral field and must survive as a pass-through member.
	if _, ok := req.Extra["echo"]; !ok {
		t.Errorf("echo was dropped rather than carried in Extra: %v", req.Extra)
	}

	got, err := MarshalCompletionRequest(req, &EncodeOptions{Model: "upstream-id"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"upstream-id","prompt":"Once upon","max_tokens":16,"temperature":0.2,"stop":"\n","echo":true}`
	if string(got) != want {
		t.Fatalf("request bytes\n got: %s\nwant: %s", got, want)
	}
}

// TestCompletionPromptFormsRoundTrip is the reason canonical.Prompt exists.
//
// The field is four things on the wire and normalizing them to one changes what
// the backend generates from. A single-element array must stay an array and a
// bare string must stay a string.
func TestCompletionPromptFormsRoundTrip(t *testing.T) {
	for _, wire := range []string{
		`"one"`,
		`["one"]`,
		`["one","two"]`,
		`[15496,995]`,
		`[[15496,995],[1,2]]`,
		`[]`,
	} {
		body := []byte(`{"model":"m","prompt":` + wire + `}`)
		req, err := DecodeCompletionRequest(body)
		if err != nil {
			t.Fatalf("%s: %v", wire, err)
		}
		got, err := MarshalCompletionRequest(req, nil)
		if err != nil {
			t.Fatalf("%s: %v", wire, err)
		}
		want := `{"model":"m","prompt":` + wire + `}`
		if string(got) != want {
			t.Errorf("prompt round trip\n got: %s\nwant: %s", got, want)
		}
	}
}

func TestCompletionResponseGolden(t *testing.T) {
	upstream := []byte(`{"id":"cmpl-1","object":"text_completion","created":1753660800,` +
		`"model":"upstream-id","choices":[{"index":0,"text":" a time","finish_reason":"length"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,` +
		`"prompt_tokens_details":{"cached_tokens":2}}}`) // pragma: allowlist secret — test fixture
	resp, err := DecodeCompletionResponse(upstream, &DecodeOptions{Model: "qwen3.5:397b"})
	if err != nil {
		t.Fatal(err)
	}
	// DESIGN §10.7: input counts are INCLUSIVE of cache reads on this wire, and
	// the neutral form keeps them that way. Reading them as exclusive is the
	// inversion that bills a cached request ~1.8x over.
	if resp.Usage.InputTokens != 3 || resp.Usage.CacheReadTokens != 2 {
		t.Errorf("usage %+v — input must stay inclusive of cache reads", resp.Usage)
	}
	if resp.Choices[0].StopReason != canonical.StopMaxTokens {
		t.Errorf("stop reason %q", resp.Choices[0].StopReason)
	}

	got, err := MarshalCompletionResponse(resp, &ResponseOptions{Model: "qwen3.5:397b"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"cmpl-1","object":"text_completion","created":1753660800,"model":"qwen3.5:397b",` +
		`"choices":[{"index":0,"text":" a time","finish_reason":"length"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,` +
		`"prompt_tokens_details":{"cached_tokens":2}}}` // pragma: allowlist secret — test fixture
	if string(got) != want {
		t.Fatalf("response bytes\n got: %s\nwant: %s", got, want)
	}
}

// TestCompletionCrossFamily is the whole point of decoding to the neutral form:
// a /v1/completions request reaches an Anthropic-shaped deployment, and its
// answer comes back in the text_completion shape the caller expects.
func TestCompletionCrossFamily(t *testing.T) {
	req, err := DecodeCompletionRequest([]byte(`{"model":"m","prompt":"Hello","max_tokens":8}`))
	if err != nil {
		t.Fatal(err)
	}
	// The neutral request is complete on its own: messages, not just a prompt.
	if len(req.Messages) != 1 || req.Messages[0].Role != canonical.RoleUser {
		t.Fatalf("a legacy request did not produce a routable conversation: %+v", req.Messages)
	}
	// And a chat-shaped answer renders back as text_completion.
	resp := &canonical.Response{
		ID: "id", Created: 1, Model: "m",
		Choices: []canonical.Choice{{
			Message:    canonical.TextMessage(canonical.RoleAssistant, "there"),
			StopReason: canonical.StopEndTurn,
		}},
	}
	got, err := MarshalCompletionResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"id","object":"text_completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"text":"there","finish_reason":"stop"}]}`
	if string(got) != want {
		t.Fatalf("cross-family bytes\n got: %s\nwant: %s", got, want)
	}
}

// TestCompletionDecodeIsCaseSensitive is COMPATIBILITY 2.0 on this surface.
//
// The authorization gate scanned these bytes with an exact key comparison. An
// adapter that resolved a model from "Model" would dispatch a request the
// allow-list never saw — the W10 bypass, one route over.
func TestCompletionDecodeIsCaseSensitive(t *testing.T) {
	req, err := DecodeCompletionRequest([]byte(`{"Model":"expensive","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "" {
		t.Errorf("model %q resolved from a differently-cased key", req.Model)
	}
	if _, leaked := req.Extra["Model"]; leaked {
		t.Error("the colliding key survived into Extra and would be relayed to the next hop")
	}
}
