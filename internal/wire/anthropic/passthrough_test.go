package anthropic

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/internal/wire/wiretest"
)

// The same pass-through property internal/wire/openai asserts, on this family's
// shape. It is asserted separately rather than being assumed to follow, because
// this family's usage object is where the vendor keeps adding fields: the
// per-TTL cache_creation breakdown and server_tool_use's web-search request
// count are both billed and neither is modelled here.
const upstreamMessage = `{
  "id": "msg_01Abc",
  "type": "message",
  "role": "assistant",
  "model": "upstream-model-id",
  "content": [{"type": "text", "text": "hello"}],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 50,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0,
    "output_tokens": 12,
    "cache_creation": {"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": 0},
    "server_tool_use": {"web_search_requests": 2},
    "service_tier": "standard"
  },
  "container": {"id": "container_01", "expires_at": "2026-01-01T00:00:00Z"}
}`

// TestSameFamilyMessageDropsNothing asserts what the client receives against
// what the upstream sent, leaf by leaf, for a non-streaming same-family
// exchange.
func TestSameFamilyMessageDropsNothing(t *testing.T) {
	r, err := DecodeResponse([]byte(upstreamMessage), &DecodeOptions{Model: "client-facing-name"})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	got, err := MarshalResponse(r, &ResponseOptions{Model: "client-facing-name"})
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}

	want := leaves(t, []byte(upstreamMessage))
	// model is rewritten to the client-facing name (DESIGN §7.2). total_tokens
	// is ADDED by this encoder under COMPATIBILITY 6.8, which Diff already
	// tolerates: adding a field is not losing one.
	delete(want, "model")

	if diff := wiretest.Diff(want, leaves(t, got)); diff != "" {
		t.Errorf("the client did not receive what the upstream sent:\n%s\n\nclient:\n%s", diff, got)
	}
}

// TestMeasuredZeroCacheCountsSurvive is COMPATIBILITY 6.7's unresolved half.
//
// 6.7 records that cache fields appear "only when > 0", and in the same
// paragraph that the vendor itself emits explicit zeros, so a golden captured
// from the vendor will not match one captured from a proxy. Those cannot both
// be the rule. The reading that survives contact with a billing system is: a
// count the BACKEND stated is emitted as stated, and the ">0" rule governs a
// count dorang produced itself, which states nothing.
func TestMeasuredZeroCacheCountsSurvive(t *testing.T) {
	body := `{"id":"m","type":"message","role":"assistant","model":"m",` +
		`"content":[],"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":50,"output_tokens":12,` +
		`"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	r, err := DecodeResponse([]byte(body), nil)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	got, err := MarshalResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	l := leaves(t, got)
	if l["usage.cache_read_input_tokens"] != "0" {
		t.Errorf("cache_read_input_tokens = %q, want a preserved measured zero:\n%s",
			l["usage.cache_read_input_tokens"], got)
	}
	if l["usage.cache_creation_input_tokens"] != "0" {
		t.Errorf("cache_creation_input_tokens = %q, want a preserved measured zero:\n%s",
			l["usage.cache_creation_input_tokens"], got)
	}

	// And a count dorang built itself still reports no cache at all.
	w := EncodeUsage(canonical.Usage{InputTokens: 50, OutputTokens: 12}, TotalTokensOmit)
	if w.CacheReadInputTokens != nil || w.CacheCreationInputTokens != nil {
		t.Error("a synthesized usage asserted a cache measurement that was never taken")
	}
}

// TestStreamingAndNonStreamingAgreeOnCacheCounts is this family's half of the
// equivalence the OpenAI adapter pins on whole documents.
//
// The terminal message_delta is the frame COMPATIBILITY 6.7 calls the one that
// "carries the real values", so it and the non-streaming encoder must make the
// same decision about a measured zero. If they diverge, the same exchange bills
// differently depending on whether the caller passed stream:true — which is the
// defect, not an implementation detail of two functions.
//
// message_start is deliberately NOT included: 6.7 specifies that it omits the
// cache fields rather than zero-seeding them, and it is sent before the counts
// are known.
func TestStreamingAndNonStreamingAgreeOnCacheCounts(t *testing.T) {
	for _, u := range []canonical.Usage{
		{InputTokens: 50, OutputTokens: 12}, // synthesized: reports nothing
		{InputTokens: 50, OutputTokens: 12,
			Reported: canonical.UsageCacheRead | canonical.UsageCacheWrite}, // measured zeros
		{InputTokens: 900, OutputTokens: 12, CacheReadTokens: 800, CacheWriteTokens: 50,
			Reported: canonical.UsageCacheRead | canonical.UsageCacheWrite},
	} {
		nonStream := EncodeUsage(u, TotalTokensOmit)
		streamed := finalUsage(u)
		if !samePtr(nonStream.CacheReadInputTokens, streamed.CacheReadInputTokens) {
			t.Errorf("cache_read_input_tokens differs by path for %+v: non-streaming %v, streamed %v",
				u, show(nonStream.CacheReadInputTokens), show(streamed.CacheReadInputTokens))
		}
		if !samePtr(nonStream.CacheCreationInputTokens, streamed.CacheCreationInputTokens) {
			t.Errorf("cache_creation_input_tokens differs by path for %+v: non-streaming %v, streamed %v",
				u, show(nonStream.CacheCreationInputTokens), show(streamed.CacheCreationInputTokens))
		}
	}
}

func samePtr(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func show(p *int) string {
	if p == nil {
		return "absent"
	}
	return strconv.Itoa(*p)
}

// TestChatExtrasDoNotReachAMessage is the limit on the above.
//
// A chat completion's unmodelled members have no meaning in this shape, and
// inventing them here is the compatibility break DESIGN §10.7 exists to
// prevent. The conversion carries what §10.7's table names and nothing else.
func TestChatExtrasDoNotReachAMessage(t *testing.T) {
	const chat = `{"id":"c","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16},` +
		`"timings":{"predicted_per_second":125.0}}`
	r, err := openai.DecodeResponse([]byte(chat), nil)
	if err != nil {
		t.Fatalf("openai.DecodeResponse: %v", err)
	}
	if len(r.Extra) == 0 {
		t.Fatal("the sibling adapter captured nothing; this test would prove nothing")
	}
	got, err := MarshalResponse(r, nil)
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	for path := range leaves(t, got) {
		if strings.HasPrefix(path, "timings") {
			t.Errorf("a chat completion's %q was invented on a message:\n%s", path, got)
		}
	}
}

func leaves(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out, err := wiretest.Leaves(b)
	if err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return out
}
