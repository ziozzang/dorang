package openai

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/wiretest"
)

// These tests assert what the CLIENT RECEIVES, byte for byte, against what the
// upstream sent. That distinction is the whole reason they exist: the defect
// they pin lived in serialization, not in the neutral form, and a test that
// asserts a struct field is populated proves nothing about serialization. Every
// assertion below runs on marshalled bytes.
//
// The upstream capture is a llama.cpp deployment answered through an
// OpenAI-compatible proxy. Four things in it were reproduced as dropped before
// this file existed: usage.prompt_tokens_details.cached_tokens (a MEASURED
// zero), the eleven-field timings object, choices[].provider_specific_fields,
// and the unmodelled members of both usage detail sub-objects.
const upstreamChatCompletion = `{
  "id": "chatcmpl-7cQ2",
  "object": "chat.completion",
  "created": 1750000000,
  "model": "upstream-model-id",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "hello"},
      "finish_reason": "stop",
      "provider_specific_fields": {"llama_slot_id": 3, "vendor_note": "served from slot"}
    }
  ],
  "usage": {
    "prompt_tokens": 11,
    "completion_tokens": 5,
    "total_tokens": 16,
    "prompt_tokens_details": {"cached_tokens": 0, "audio_tokens": 0},
    "completion_tokens_details": {"reasoning_tokens": 3, "accepted_prediction_tokens": 0},
    "cost_usd": 0.000042
  },
  "system_fingerprint": "b4567-abcdef",
  "timings": {
    "cache_n": 0,
    "prompt_n": 11,
    "prompt_ms": 12.5,
    "prompt_per_token_ms": 1.136,
    "prompt_per_second": 880.0,
    "predicted_n": 5,
    "predicted_ms": 40.0,
    "predicted_per_token_ms": 8.0,
    "predicted_per_second": 125.0,
    "draft_n": 0,
    "draft_n_accepted": 0
  }
}`

// TestSameFamilyNonStreamingDropsNothing is the primary assertion: for a
// non-streaming exchange that arrives and leaves in the chat-completions shape,
// EVERY leaf the upstream sent reaches the client with the same literal bytes.
//
// It is deliberately exhaustive rather than a list of the four fields the
// differential harness happened to sample. The harness looked at one deployment
// family; an allow-list of its findings would pass while the next backend's
// fields go missing, which is the failure mode that produced this file.
func TestSameFamilyNonStreamingDropsNothing(t *testing.T) {
	r, err := DecodeResponse([]byte(upstreamChatCompletion), &DecodeOptions{Model: "client-facing-name"})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	got, err := MarshalResponse(r, &ResponseOptions{Model: "client-facing-name"})
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}

	// model is the ONE leaf dorang is contractually required to change: the body
	// carries the name the client asked for, never the upstream id (§7.2).
	assertNoLeafDropped(t, []byte(upstreamChatCompletion), got, "model")

	// And the rewrite actually happened, so the exemption above is not hiding a
	// pass-through of the upstream id.
	if leaf := jsonLeaves(t, got)["model"]; leaf != `"client-facing-name"` {
		t.Errorf("model = %s, want the client-facing name (DESIGN §7.2)", leaf)
	}
}

// TestMeasuredZeroCachedTokensSurvives isolates the zero-versus-absent half of
// the defect, because it is the half that a source reading gets wrong.
//
// A NON-ZERO cached_tokens always survived: the decoder read it and the encoder
// emitted it because it was greater than zero. Only the measured zero was lost,
// and the two cases must be pinned separately or a regression that reinstates
// the `> 0` test passes the non-zero case and ships.
func TestMeasuredZeroCachedTokensSurvives(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cached string
		want   string
	}{
		{"measured zero", "0", "0"},
		{"measured non-zero", "7", "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[],` +
				`"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16,` +
				`"prompt_tokens_details":{"cached_tokens":` + tc.cached + `}}}`
			r, err := DecodeResponse([]byte(body), nil)
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			out, err := MarshalResponse(r, nil)
			if err != nil {
				t.Fatalf("MarshalResponse: %v", err)
			}
			got := jsonLeaves(t, out)["usage.prompt_tokens_details.cached_tokens"]
			if got != tc.want {
				t.Errorf("usage.prompt_tokens_details.cached_tokens = %q, want %q\n%s", got, tc.want, out)
			}
		})
	}
}

// TestSynthesizedUsageReportsNoBreakdown is the other side of that rule, and it
// is why presence is tracked instead of always emitting the detail objects.
//
// A count dorang produced itself — an estimate, an accumulator, a test fixture —
// states nothing about a cache. Emitting prompt_tokens_details: {cached_tokens:
// 0} for it would assert a measurement that was never taken, on every response
// from every backend that has no cache at all.
func TestSynthesizedUsageReportsNoBreakdown(t *testing.T) {
	w := EncodeUsage(canonical.Usage{InputTokens: 11, OutputTokens: 5})
	if w.PromptTokensDetails != nil {
		t.Errorf("prompt_tokens_details = %+v, want absent: nothing measured a cache", w.PromptTokensDetails)
	}
	if w.CompletionTokensDetails != nil {
		t.Errorf("completion_tokens_details = %+v, want absent", w.CompletionTokensDetails)
	}
	if w.CacheCreationInputTokens != nil {
		t.Errorf("cache_creation_input_tokens = %d, want absent", *w.CacheCreationInputTokens)
	}
}

// TestStreamingAndNonStreamingAgreeOnUsage pins the property that was actually
// broken: the SAME upstream usage object, requested both ways, must produce the
// same accounting for the client.
//
// It was not a cosmetic difference. The streaming relay forwards frames, so a
// measured zero survived it; the non-streaming path re-serialized from typed
// structs and dropped the same zero. One request losing billing data or not
// depending on whether the caller passed stream:true is the kind of
// inconsistency nobody debugs successfully, so the equivalence is pinned here
// permanently rather than left to follow from the two paths' implementations.
func TestStreamingAndNonStreamingAgreeOnUsage(t *testing.T) {
	const usageJSON = `{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16,` +
		`"prompt_tokens_details":{"cached_tokens":0},` +
		`"completion_tokens_details":{"reasoning_tokens":3}}`

	// Non-streaming: the whole answer, decoded and re-encoded.
	nonStreamBody := `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":` + usageJSON + `}`
	r, err := DecodeResponse([]byte(nonStreamBody), nil)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	nonStream, err := MarshalResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}

	// Streaming, through the CROSSING path — the one that decodes each frame to
	// neutral events and re-encodes them. The byte relay cannot disagree with
	// anything because it forwards bytes; this path can, and did.
	chunkBody := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
		`"choices":[],"usage":` + usageJSON + `}`
	_, events, err := DecodeChunk([]byte(chunkBody), nil)
	if err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}
	var sse bytes.Buffer
	sw := NewStreamWriter(&sse, StreamConfig{ID: "c1", Created: 1, Model: "m", IncludeUsage: true})
	for _, ev := range events {
		if err := sw.WriteEvent(ev); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	streamed := usageFrameOf(t, sse.Bytes())

	// Paths are compared by name, so the expectation is read under the same
	// "usage." prefix the two documents put it under.
	wantLeaves := subtree(jsonLeaves(t, []byte(`{"usage":`+usageJSON+`}`)), "usage")
	nonStreamLeaves := subtree(jsonLeaves(t, nonStream), "usage")
	streamLeaves := subtree(jsonLeaves(t, streamed), "usage")

	if diff := leafDiff(wantLeaves, nonStreamLeaves); diff != "" {
		t.Errorf("non-streaming usage does not match what the upstream sent:\n%s", diff)
	}
	if diff := leafDiff(wantLeaves, streamLeaves); diff != "" {
		t.Errorf("streamed usage does not match what the upstream sent:\n%s", diff)
	}
	if diff := leafDiff(nonStreamLeaves, streamLeaves); diff != "" {
		t.Errorf("the two paths disagree about usage — the same request bills differently"+
			" depending on stream:true:\n%s", diff)
	}
}

// TestUnknownFieldsDoNotCrossFamilies is the limit on all of the above.
//
// Preserving an unknown member is safe when the answer leaves in the shape it
// arrived in. Splicing one into a CONVERTED answer invents a field the target
// protocol does not have, which is its own compatibility break — a chat
// completion has no `container`, and an Anthropic message has no `timings`.
// DESIGN §10.7 governs a conversion and it carries only what its table names.
func TestUnknownFieldsDoNotCrossFamilies(t *testing.T) {
	r, err := DecodeResponse([]byte(upstreamChatCompletion), nil)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if r.ExtraFamily != canonical.FamilyOpenAIChat {
		t.Fatalf("ExtraFamily = %q, want %q", r.ExtraFamily, canonical.FamilyOpenAIChat)
	}
	if len(r.Extra) == 0 {
		t.Fatal("nothing captured: the rest of this test would prove nothing")
	}

	// The legacy completions shape is a DIFFERENT shape that happens to be
	// served by the same family of backend. Its choices carry `text`, not
	// `message`, and a chat backend's extras have no place in it.
	legacy, err := MarshalCompletionResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalCompletionResponse: %v", err)
	}
	for path := range jsonLeaves(t, legacy) {
		if strings.HasPrefix(path, "timings") {
			t.Errorf("a chat completion's timings reached the text_completion shape at %q:\n%s",
				path, legacy)
		}
	}

	// And a response that never recorded where its members came from splices
	// nowhere at all.
	r.ExtraFamily = canonical.FamilyUnknown
	untagged, err := MarshalResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	for path := range jsonLeaves(t, untagged) {
		if strings.HasPrefix(path, "timings") {
			t.Errorf("untagged members were spliced at %q; only a recorded family may forward them:\n%s",
				path, untagged)
		}
	}
}

// TestNativeFinishReasonJoinsProviderFields asserts that recording dorang's own
// native_finish_reason does not erase the backend's other provider_specific_
// fields members.
//
// Writing a fresh one-key map there is the obvious implementation and it is
// wrong in a chain: the second gateway deletes the first one's context, and the
// loss is invisible because the key it cares about is still there.
func TestNativeFinishReasonJoinsProviderFields(t *testing.T) {
	body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop",` +
		`"provider_specific_fields":{"vendor_note":"kept","native_finish_reason":"eos_token"}}]}`
	r, err := DecodeResponse([]byte(body), nil)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, err := MarshalResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	leaves := jsonLeaves(t, out)
	if got := leaves["choices[0].provider_specific_fields.vendor_note"]; got != `"kept"` {
		t.Errorf("vendor_note = %q, want \"kept\" — writing native_finish_reason must not "+
			"replace the whole object:\n%s", got, out)
	}
	if got := leaves["choices[0].provider_specific_fields.native_finish_reason"]; got != `"eos_token"` {
		t.Errorf("native_finish_reason = %q, want \"eos_token\" (COMPATIBILITY 4.3):\n%s", got, out)
	}
}

// TestModerationAnswerDropsNothing covers a surface the differential harness
// never sampled, and which had the same defect.
//
// The neutral form already HAD canonical.ModerationResponse.Extra and
// canonical.ModerationResult.Extra. Nothing populated them and nothing read
// them, so every unmodelled member of a moderations answer was discarded on a
// surface where the request and the answer are the same shape in and out.
func TestModerationAnswerDropsNothing(t *testing.T) {
	const upstream = `{"id":"modr-1","model":"upstream-model-id","results":[` +
		`{"flagged":false,"categories":{"hate":false},"category_scores":{"hate":0.0001},` +
		`"category_applied_input_types":{"hate":["text"]},"result_note":"policy-v3"}],` +
		`"provider":"self-hosted","policy_version":"2026.1"}`

	r, err := DecodeModerationResponse([]byte(upstream), "client-facing-name")
	if err != nil {
		t.Fatalf("DecodeModerationResponse: %v", err)
	}
	got, err := MarshalModerationResponse(r)
	if err != nil {
		t.Fatalf("MarshalModerationResponse: %v", err)
	}
	assertNoLeafDropped(t, []byte(upstream), got, "model")
}

// TestResponsesSurfaceUsageKeepsAMeasuredZero is the same zero-versus-absent
// rule on the third shape that reports token counts.
//
// It is a live billing path: a /v1/responses client is served from a
// chat-completions backend, so a measured cached_tokens crosses the chat
// decoder and lands in this encoder. The `> 0` test here dropped it exactly the
// way the chat encoder did.
func TestResponsesSurfaceUsageKeepsAMeasuredZero(t *testing.T) {
	const chat = `{"id":"c","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16,` +
		`"prompt_tokens_details":{"cached_tokens":0}}}`
	r, err := DecodeResponse([]byte(chat), nil)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	got, err := MarshalResponsesResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponsesResponse: %v", err)
	}
	if v := jsonLeaves(t, got)["usage.input_tokens_details.cached_tokens"]; v != "0" {
		t.Errorf("usage.input_tokens_details.cached_tokens = %q, want a preserved measured zero:\n%s",
			v, got)
	}
}

// TestCasedKeyIsNotEmittedTwice guards the hazard that arrives WITH the
// pass-through mechanism rather than being fixed by it.
//
// Response bodies are decoded with plain json.Unmarshal, deliberately: a
// differently-cased key from a backend is a vendor quirk, and refusing it would
// turn an upstream cosmetic bug into lost usage counts (see [DecodeResponse]).
// But encoding/json matches field names case-insensitively, so "Usage"
// populates the Usage field — and an exact-match Extra split then keeps "Usage"
// in the map too, so the client receives the object twice under two spellings.
// The split folds case for exactly this reason.
func TestCasedKeyIsNotEmittedTwice(t *testing.T) {
	body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[],` +
		`"Usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`
	r, err := DecodeResponse([]byte(body), nil)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, err := MarshalResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	leaves := jsonLeaves(t, out)
	// The counts arrived, under the canonical spelling.
	if leaves["usage.prompt_tokens"] != "11" {
		t.Errorf("the cased key cost the caller its usage counts:\n%s", out)
	}
	// And not a second time under the vendor's.
	if _, dup := leaves["Usage.prompt_tokens"]; dup {
		t.Errorf("usage was emitted twice, once per spelling:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Leaf comparison
// ---------------------------------------------------------------------------

// jsonLeaves flattens a JSON document to path -> literal. See [wiretest.Leaves]
// for why numbers keep their source text.
func jsonLeaves(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out, err := wiretest.Leaves(b)
	if err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return out
}

func subtree(leaves map[string]string, prefix string) map[string]string {
	return wiretest.Subtree(leaves, prefix)
}

func leafDiff(want, got map[string]string) string { return wiretest.Diff(want, got) }

// assertNoLeafDropped compares every leaf of the upstream document against the
// client's, exempting the paths dorang is required to rewrite.
func assertNoLeafDropped(t *testing.T, upstream, client []byte, rewritten ...string) {
	t.Helper()
	want := jsonLeaves(t, upstream)
	for _, p := range rewritten {
		delete(want, p)
	}
	if diff := leafDiff(want, jsonLeaves(t, client)); diff != "" {
		t.Errorf("the client did not receive what the upstream sent:\n%s\n\nupstream:\n%s\n\nclient:\n%s",
			diff, upstream, client)
	}
}

// usageFrameOf returns the payload of the SSE frame carrying usage.
func usageFrameOf(t *testing.T, sse []byte) []byte {
	t.Helper()
	for _, frame := range strings.Split(string(sse), "\n\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(frame), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		if strings.Contains(payload, `"usage"`) {
			return []byte(payload)
		}
	}
	t.Fatalf("no usage frame in the stream:\n%s", sse)
	return nil
}
