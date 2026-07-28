package tokenest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// bytesOverThree is the estimator this package replaced: the raw body length
// divided by three. Several tests below are stated against it rather than
// against a literal, because the property that matters is a direction — the new
// estimate must be lower where the old one was catastrophically high, and higher
// where it had no margin at all.
func bytesOverThree(n int) int64 { return int64((n + 2) / 3) }

// TestAnImageCostsWhatAnImageCostsNotWhatItsEncodingWeighs is the whole point.
//
// A 2 MB photograph base64-encodes to ~2.7 MB of request body. Divided by three
// that is ~900,000 tokens, which exceeds every context window in existence, so
// the request is refused everywhere rather than routed anywhere. The real cost
// is on the order of 1,600.
func TestAnImageCostsWhatAnImageCostsNotWhatItsEncodingWeighs(t *testing.T) {
	const encoded = 2_700_000
	r := &canonical.Request{
		Messages: []canonical.Message{{
			Role: canonical.RoleUser,
			Content: canonical.Content{
				canonical.TextBlock("what is in this photo?"),
				canonical.ImageBlock("image/jpeg", strings.Repeat("A", encoded)),
			},
		}},
	}
	got := Request(r).Tokens
	if got > 4_000 {
		t.Errorf("a single image estimates at %d tokens; it costs about %d",
			got, int64(imageTokens))
	}
	if old := bytesOverThree(encoded); got >= old/100 {
		t.Errorf("estimate %d is not meaningfully below the byte rule's %d", got, old)
	}
	// The bound must not depend on how large the encoding is. Ten times the
	// payload is the same image.
	big := &canonical.Request{Messages: []canonical.Message{{
		Role:    canonical.RoleUser,
		Content: canonical.Content{canonical.ImageBlock("image/jpeg", strings.Repeat("A", encoded*10))},
	}}}
	if a, b := Request(big).Tokens, Request(r).Tokens; a > b {
		t.Errorf("a ten-times-larger encoding estimated higher (%d > %d): the count still reads the payload", a, b)
	}
}

// TestLowDetailImagesAreCheaperBecauseTheProtocolSaysSo. detail:"low" is a
// promise about the request, not a guess: the image is downsampled to one tile
// whatever it was.
func TestLowDetailImagesAreCheaperBecauseTheProtocolSaysSo(t *testing.T) {
	low := canonical.Block{Kind: canonical.KindImage,
		Source: &canonical.Source{Kind: canonical.SourceBase64, Data: "AAAA", Detail: "low"}}
	high := canonical.ImageBlock("image/png", "AAAA")
	if a, b := block(&low, 0), block(&high, 0); a >= b {
		t.Errorf("low detail (%d) is not cheaper than the default (%d)", a, b)
	}
}

// TestKoreanIsNotUnderCounted. DESIGN §10.5a rejects bytes/4 for undercounting
// CJK. bytes/3 is no better where it matters: internal/server/json.go fixes the
// serializer at raw UTF-8, so Korean arrives at three bytes per syllable against
// roughly one token per syllable — exactly the ratio, with no margin at all.
func TestKoreanIsNotUnderCounted(t *testing.T) {
	const ko = "안녕하세요. 오늘 날씨가 참 좋네요. 자세히 설명해 주세요."
	if got, old := Text(ko), bytesOverThree(len(ko)); got <= old {
		t.Errorf("Korean estimated at %d tokens, the byte rule gives %d; the estimate has no margin", got, old)
	}
	// The byte fallback, which the relayed surfaces still use, has to point the
	// same way.
	if got, old := Bytes([]byte(ko)).Tokens, bytesOverThree(len(ko)); got <= old {
		t.Errorf("the byte fallback gives %d for Korean, the old rule gives %d", got, old)
	}
}

// TestAsciiProseIsBiasedHigh. Over-estimating prose is the affordable direction
// and the one §10.5a asks for; the check is that it is above the ~4 bytes/token
// a real tokenizer achieves, not that it is close.
func TestAsciiProseIsBiasedHigh(t *testing.T) {
	const en = "The quick brown fox jumps over the lazy dog, twice, and then rests."
	real := int64(len(en) / 4)
	if got := Text(en); got <= real {
		t.Errorf("English prose estimated at %d tokens, below the ~%d a tokenizer produces", got, real)
	}
}

// TestEmojiCostMoreThanOneToken. A four-byte sequence is charged two, because
// emoji routinely tokenize as a pair.
func TestEmojiCostMoreThanOneToken(t *testing.T) {
	if got := Text("🙂"); got != 2 {
		t.Errorf("one emoji estimated at %d tokens, want 2", got)
	}
}

// TestToolsAndSchemasArePrompt asserts that the declarations a caller does not
// think of as prompt are counted: they reach the model and they occupy the
// window.
func TestToolsAndSchemasArePrompt(t *testing.T) {
	bare := &canonical.Request{Messages: []canonical.Message{
		canonical.TextMessage(canonical.RoleUser, "hi")}}
	withTools := &canonical.Request{
		Messages: bare.Messages,
		Tools: []canonical.Tool{{
			Type: "function", Name: "search",
			Description: "search the corpus for a phrase and return the matches",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
	}
	if a, b := Request(withTools).Tokens, Request(bare).Tokens; a <= b {
		t.Errorf("declaring a tool did not raise the estimate (%d vs %d)", a, b)
	}
}

// TestTheEstimateSaysItIsAnEstimate. Nothing here tokenizes, so nothing here may
// claim to have counted.
func TestTheEstimateSaysItIsAnEstimate(t *testing.T) {
	cases := map[string]Estimate{
		"request": Request(&canonical.Request{}),
		"bytes":   Bytes([]byte("hello")),
		"rerank":  Rerank(&canonical.RerankRequest{Query: "q"}),
	}
	for name, e := range cases {
		if e.Exact {
			t.Errorf("%s claims to be exact", name)
		}
		if !e.Estimated() {
			t.Errorf("%s does not report itself as estimated", name)
		}
		if e.Method == "" {
			t.Errorf("%s carries no method", name)
		}
	}
	if got := Request(nil); got.Method != MethodNone || got.Tokens != 0 {
		t.Errorf("a nil request estimated as %+v", got)
	}
}

// TestUploadedAudioIsNotCountedAsPromptBytes. A transcription's file is audio.
// Counting its bytes as prompt is the image defect with a different media type:
// a 20 MB upload is minutes of speech, not seven million tokens.
func TestUploadedAudioIsNotCountedAsPromptBytes(t *testing.T) {
	const size = 20 << 20
	r := &canonical.TranscriptionRequest{
		File: canonical.File{Field: "file", Name: "a.wav", Data: make([]byte, size)},
	}
	got := Request(nil).Tokens + Transcription(r).Tokens
	if got > 100_000 {
		t.Errorf("a %d-byte upload estimates at %d tokens", size, got)
	}
	if old := bytesOverThree(size); got >= old/10 {
		t.Errorf("estimate %d is not meaningfully below the byte rule's %d", got, old)
	}
}

// TestNoAllocations. This runs once per request on the hot path (§15.2.2).
func TestNoAllocations(t *testing.T) {
	r := &canonical.Request{
		System: canonical.Content{canonical.TextBlock("you are a helpful assistant")},
		Messages: []canonical.Message{
			canonical.TextMessage(canonical.RoleUser, "설명해 주세요, please."),
			{Role: canonical.RoleAssistant, Content: canonical.Content{
				canonical.ToolUseBlock("call_1", "search", json.RawMessage(`{"q":"x"}`)),
			}},
			{Role: canonical.RoleUser, Content: canonical.Content{
				canonical.ImageBlock("image/png", strings.Repeat("A", 4096)),
			}},
		},
		Tools: []canonical.Tool{{Type: "function", Name: "search",
			Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	if n := testing.AllocsPerRun(100, func() { _ = Request(r) }); n != 0 {
		t.Errorf("Request allocates %v times per call", n)
	}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	if n := testing.AllocsPerRun(100, func() { _ = Bytes(body) }); n != 0 {
		t.Errorf("Bytes allocates %v times per call", n)
	}
}

// TestNestedToolResultsTerminate. A tool result carries blocks and a block can
// be another tool result, so the shape is a caller-supplied tree on the request
// path. The bound is a refusal to recurse, not a stack overflow.
func TestNestedToolResultsTerminate(t *testing.T) {
	inner := canonical.ToolResultBlock("t0", canonical.TextBlock("leaf"))
	for range 64 {
		inner = canonical.ToolResultBlock("t", inner)
	}
	r := &canonical.Request{Messages: []canonical.Message{{
		Role: canonical.RoleTool, Content: canonical.Content{inner}}}}
	if got := Request(r).Tokens; got <= 0 {
		t.Errorf("a deeply nested tool result estimated at %d", got)
	}
}
