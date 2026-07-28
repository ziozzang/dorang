package anthropic

import (
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/openai"
)

// TestUsageBothDirections asserts the normalization in both directions on the
// same numbers, which is the only way to catch an adapter that is
// self-consistent and wrong.
//
// canonical.Usage.InputTokens is the full prompt count, cache included. This
// family reports the part that was neither read from nor written to cache. So
// the wire count is 1000 - 800 - 150 = 50, and decoding it must return 1000.
func TestUsageBothDirections(t *testing.T) {
	in := canonical.Usage{
		InputTokens:      1000,
		OutputTokens:     20,
		CacheReadTokens:  800,
		CacheWriteTokens: 150,
	}

	w := EncodeUsage(in, TotalTokensCompat)
	if *w.InputTokens != 50 {
		t.Errorf("input_tokens = %d, want 50 (1000 - 800 - 150); COMPATIBILITY 6.7", *w.InputTokens)
	}
	if *w.CacheReadInputTokens != 800 || *w.CacheCreationInputTokens != 150 {
		t.Errorf("cache counts = %d/%d, want 800/150", *w.CacheReadInputTokens, *w.CacheCreationInputTokens)
	}
	if *w.OutputTokens != 20 {
		t.Errorf("output_tokens = %d, want 20", *w.OutputTokens)
	}
	// The visible fields must add up to the total, or a client that sums what it
	// sees disagrees with the invoice.
	if sum := *w.InputTokens + *w.CacheReadInputTokens + *w.CacheCreationInputTokens + *w.OutputTokens; sum != *w.TotalTokens {
		t.Errorf("fields sum to %d but total_tokens = %d", sum, *w.TotalTokens)
	}

	back := UsageToCanonical(w)
	// The decode direction learns something the encode direction was not told:
	// WHICH counters the wire stated. `in` was hand-built and states nothing, so
	// the round trip is an identity on the counts and strictly gains presence.
	counts := *back
	counts.Reported = 0
	if counts != in {
		t.Errorf("round trip changed the counts:\n got %+v\nwant %+v", counts, in)
	}
	if want := canonical.UsageInput | canonical.UsageOutput |
		canonical.UsageCacheRead | canonical.UsageCacheWrite; back.Reported != want {
		t.Errorf("decoded presence = %04b, want %04b — every counter this wire stated must be marked",
			back.Reported, want)
	}
}

// TestUsageInclusiveExclusiveAcrossFamilies is the failure DESIGN §10.7 says
// produces no error, only a wrong invoice: one family reports cache reads
// INSIDE the input count and this one reports them beside it.
//
// It drives the real sibling adapter rather than a restatement of it, because
// the mapping is normative across packages — an adapter pair that each
// round-trips itself can still disagree with the other and mis-bill every
// cached request.
func TestUsageInclusiveExclusiveAcrossFamilies(t *testing.T) {
	const body = `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":20,"total_tokens":1020,` +
		`"prompt_tokens_details":{"cached_tokens":800},"cache_creation_input_tokens":150}}` // pragma: allowlist secret — test fixture

	resp, err := openai.DecodeResponse([]byte(body), nil)
	if err != nil {
		t.Fatalf("openai.DecodeResponse: %v", err)
	}
	if resp.Usage.InputTokens != 1000 {
		t.Fatalf("the sibling adapter's convention changed: InputTokens = %d, want the inclusive 1000",
			resp.Usage.InputTokens)
	}

	w := EncodeUsage(*resp.Usage, TotalTokensCompat)
	if *w.InputTokens != 50 {
		t.Errorf("input_tokens = %d, want 50", *w.InputTokens)
	}
	// Neither double-counted nor under-counted: the parts still add to the
	// prompt count the backend billed.
	if got := *w.InputTokens + *w.CacheReadInputTokens + *w.CacheCreationInputTokens; got != 1000 {
		t.Errorf("cache accounting lost %d tokens: parts sum to %d, prompt_tokens was 1000", 1000-got, got)
	}

	// And the way back: an Anthropic-shaped backend's exclusive counts must
	// reach an OpenAI-shaped client as an inclusive prompt count.
	fromWire := UsageToCanonical(&Usage{
		InputTokens:              ptr(50),
		CacheReadInputTokens:     ptr(800),
		CacheCreationInputTokens: ptr(150),
		OutputTokens:             ptr(20),
	})
	ow := openai.EncodeUsage(*fromWire)
	if ow.PromptTokens != 1000 {
		t.Errorf("prompt_tokens = %d, want 1000; the other family counts cache reads inside it", ow.PromptTokens)
	}
	if ow.PromptTokensDetails == nil || ow.PromptTokensDetails.CachedTokens != 800 {
		t.Errorf("cached_tokens lost: %+v", ow.PromptTokensDetails)
	}
}

// TestUsageClampsAtZero. A backend that reports more cached tokens than prompt
// tokens exists; a negative token count on an invoice does not.
func TestUsageClampsAtZero(t *testing.T) {
	w := EncodeUsage(canonical.Usage{InputTokens: 10, CacheReadTokens: 80}, TotalTokensCompat)
	if *w.InputTokens != 0 {
		t.Errorf("input_tokens = %d, want 0 (clamped); COMPATIBILITY 6.7", *w.InputTokens)
	}
}

// TestUsageCacheFieldsOnlyWhenPositive is the rest of 6.7.
func TestUsageCacheFieldsOnlyWhenPositive(t *testing.T) {
	w := EncodeUsage(canonical.Usage{InputTokens: 7, OutputTokens: 3}, TotalTokensCompat)
	if w.CacheReadInputTokens != nil || w.CacheCreationInputTokens != nil {
		t.Error("zero cache counts must be omitted, not emitted as 0")
	}
	if w.InputTokens == nil || w.OutputTokens == nil {
		t.Error("input_tokens and output_tokens are always present")
	}
}

// TestStreamingUsageAccounting: the counts are split across message_start and
// the final message_delta, and the decoder must end up with the same numbers a
// non-streaming response would have produced.
func TestStreamingUsageAccounting(t *testing.T) {
	stream := buildStream(t,
		frame(EventMessageStart, `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":50,"output_tokens":0}}}`),
		frame(EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		frame(EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
		frame(EventContentBlockStop, `{"type":"content_block_stop","index":0}`),
		frame(EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"cache_creation_input_tokens":150,"cache_read_input_tokens":800,"output_tokens":20}}`), // pragma: allowlist secret — test fixture
		frame(EventMessageStop, `{"type":"message_stop"}`),
	)
	events, err := DecodeStream(stream, nil)
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	var last *canonical.Usage
	for i := range events {
		if events[i].Type == canonical.EventUsage {
			last = events[i].Usage
		}
	}
	if last == nil {
		t.Fatal("no usage event; the final message_delta carries it (6.7)")
	}
	want := canonical.Usage{InputTokens: 1000, OutputTokens: 20, CacheReadTokens: 800, CacheWriteTokens: 150}
	if *last != want {
		t.Errorf("streaming usage = %+v, want %+v", *last, want)
	}
	// Reasoning tokens are folded into output here (§10.7's table says so
	// explicitly), so the decoder must not invent a separate count.
	if last.ReasoningTokens != 0 {
		t.Errorf("ReasoningTokens = %d; this family folds them into output_tokens", last.ReasoningTokens)
	}
	if last.OutputTokens < last.ReasoningTokens {
		t.Error("§10.7 invariant violated: OutputTokens >= ReasoningTokens")
	}
}

// TestStreamingOmitsTotalTokensNonStreamingDoesNot is COMPATIBILITY 6.8: the
// two shapes differ by exactly one field, and dorang reproduces that.
func TestStreamingOmitsTotalTokensNonStreamingDoesNot(t *testing.T) {
	u := canonical.Usage{InputTokens: 100, OutputTokens: 25}
	if EncodeUsage(u, TotalTokensCompat).TotalTokens == nil {
		t.Error("the non-streaming shape carries the non-spec total_tokens by default")
	}
	if finalUsage(u).TotalTokens != nil {
		t.Error("the streaming shape must omit total_tokens")
	}
	if seedUsage(u).TotalTokens != nil {
		t.Error("message_start must omit total_tokens")
	}
}
