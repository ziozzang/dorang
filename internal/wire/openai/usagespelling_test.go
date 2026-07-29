package openai

import (
	"fmt"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/pricing"
)

// The vendor's rate card, quoted the way a vendor quotes one: a list rate for
// the prompt and the completion, and lower rates for the part of the prompt it
// served from cache and the part of the completion it spent reasoning.
//
// It is the same shape and the same three input numbers as internal/pricing's
// designRateCard and internal/server's responsesRateCard, so a figure computed
// here can be compared line for line against those files. That comparison is the
// point: the SAME request must cost the same through every decoder in this
// package, and the defects below are all one decoder knowing a spelling another
// did not.
const usageSpellingRateCard = `
currency: USD
rules:
  - id: vendor-list-rate
    class: marginal_usage
    match: { provider: plan-a, model: model-x }
    unit: per_1m_tokens
    input:       "0.85"
    output:      "3.40"
    cache_read:  "0.19"
    cache_write: "1.06"
    reasoning:   "6.80"
`

// chargeFor prices one decoded usage the way internal/app's settle does: the
// canonical counters copied into a pricing.Request field for field.
func chargeFor(t *testing.T, u *canonical.Usage) pricing.Cost {
	t.Helper()
	if u == nil {
		t.Fatal("the decoder produced no usage at all")
	}
	cat, err := pricing.ParseCatalog([]byte(usageSpellingRateCard))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	cost, err := cat.Price(pricing.Request{
		Provider: "plan-a", Model: "model-x",
		InputTokens:      int64(u.InputTokens),
		OutputTokens:     int64(u.OutputTokens),
		CacheReadTokens:  int64(u.CacheReadTokens),
		CacheWriteTokens: int64(u.CacheWriteTokens),
		ReasoningTokens:  int64(u.ReasoningTokens),
		Requests:         1,
		At:               time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	return cost
}

// wantCharge asserts the charge line by line and only then in total.
//
// Pinning the total alone is what let the Responses defect through the first
// time: a lost cached count overcharges the prompt and a lost reasoning count
// undercharges the completion, and on a plausible rate card the two nearly
// cancel. Each component is pinned to the quantity it was CHARGED for, which is
// also what an operator reconciles a bill against.
func wantCharge(t *testing.T, cost pricing.Cost, want map[string]struct{ qty, nano int64 }) {
	t.Helper()
	got := map[string]struct{ qty, nano int64 }{}
	for _, c := range cost.Components {
		got[c.Name] = struct{ qty, nano int64 }{c.Quantity, c.SubtotalNano}
	}
	var sum int64
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("no %s line was charged at all; charged %+v", name, cost.Components)
			continue
		}
		if g != w {
			t.Errorf("%s charged %d tokens / %d nano ($%s), want %d / %d ($%s)",
				name, g.qty, g.nano, nano9(g.nano), w.qty, w.nano, nano9(w.nano))
		}
		sum += w.nano
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected %s line: %+v", name, got[name])
		}
	}
	if cost.MarginalNano != sum {
		t.Errorf("charged %d nano ($%s) in total, the vendor's invoice is %d ($%s)",
			cost.MarginalNano, nano9(cost.MarginalNano), sum, nano9(sum))
	}
}

func nano9(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%09d", sign, n/1e9, n%1e9)
}

// ---------------------------------------------------------------------------
// The Responses spelling, through the decoder internal/backend actually calls
// ---------------------------------------------------------------------------

// A native /v1/responses answer, as that upstream writes it.
//
// The prompt count is 120 and INCLUSIVE of the 40 the vendor served from cache;
// the completion is 15 and inclusive of the 8 it spent reasoning. Both
// breakdowns live one level down, under spellings the chat family does not use.
const responsesAnswerBody = `{"id":"resp_1","object":"response","status":"completed",` +
	`"model":"upstream-id","created_at":1753660800,` +
	`"output":[{"type":"message","role":"assistant","status":"completed",` +
	`"content":[{"type":"output_text","text":"hi"}]}],` +
	`"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":40},` + // pragma: allowlist secret — test fixture
	`"output_tokens":15,"output_tokens_details":{"reasoning_tokens":8},` +
	`"total_tokens":135}}`

// The invoice for that answer, by hand, from the rate card above:
//
//	uncached prompt          (120 - 40) x $0.85/1M = $0.000068_0
//	cached prefix                    40 x $0.19/1M = $0.000007_6
//	completion less reasoning  (15 - 8) x $3.40/1M = $0.000023_8
//	reasoning                         8 x $6.80/1M = $0.000054_4
//	                                                 ------------
//	                                                 $0.000153_8
var responsesInvoice = map[string]struct{ qty, nano int64 }{
	"input":      {80, 68_000},
	"cache_read": {40, 7_600},
	"output":     {7, 23_800},
	"reasoning":  {8, 54_400},
}

// TestAResponsesAnswerIsChargedTheVendorFigure prices a Responses-API answer
// through [DecodeResponse] — the function internal/backend's openaiAdapter calls
// for OpResponses, and therefore the one that decodes this body the day a
// deployment points that operation at a native /v1/responses upstream.
//
// Asserting that the decoded struct carries a cached count would not settle
// this. The defect is not that a field is empty; it is that the bill is wrong,
// and wrong in the direction the customer pays. DESIGN §8.5 prices a declared
// sub-rate by CARVING its quantity out of its parent's rate: with `cache_read`
// declared, `input` is charged on InputTokens − CacheReadTokens. A decoder that
// reads the whole usage block as zero therefore does not merely lose a line of
// the breakdown — it charges nothing at all, and one that reads the prompt but
// not the cached prefix charges the whole inclusive prompt at the full uncached
// rate.
func TestAResponsesAnswerIsChargedTheVendorFigure(t *testing.T) {
	resp, err := DecodeResponse([]byte(responsesAnswerBody), &DecodeOptions{Model: "model-x"})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage == nil {
		t.Fatal("no usage decoded from a Responses answer")
	}
	if resp.Usage.InputTokens == 0 {
		t.Errorf("the prompt count decoded as zero from a body that said 120: "+
			"usage = %+v. input_tokens/input_tokens_details.cached_tokens and "+
			"output_tokens/output_tokens_details.reasoning_tokens are the spellings "+
			"this family uses, and this decoder knows only prompt_tokens", resp.Usage)
	}
	wantCharge(t, chargeFor(t, resp.Usage), responsesInvoice)
}

// TestAResponsesAnswerCarriesItsContent is the other half of the same routing
// question: the usage is not the only thing a Responses body carries under
// spellings the chat decoder does not know. Its assistant turn is in `output`,
// not `choices`, so a chat decode returns a successful answer with no content
// at all — the shape ErrNotAResponse exists to refuse, arriving through a
// decoder that accepted it.
func TestAResponsesAnswerCarriesItsContent(t *testing.T) {
	resp, err := DecodeResponse([]byte(responsesAnswerBody), &DecodeOptions{Model: "model-x"})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("a Responses answer decoded to no choices at all: %+v", resp)
	}
	if got := canonical.Content(resp.Choices[0].Message.Content).Flatten(); got != "hi" {
		t.Errorf("assistant text = %q, want %q; the turn is in `output`, "+
			"which the chat decoder does not read", got, "hi")
	}
}

// TestChatShapedAnswerWithResponsesUsageIsCharged is the same spelling arriving
// on a body that IS a chat completion — a `choices` array with a Responses-
// spelled usage block beside it, which is what a translating proxy in front of a
// Responses-native model produces. Shape dispatch cannot help here: the body's
// own shape says chat, and only the usage block says otherwise.
func TestChatShapedAnswerWithResponsesUsageIsCharged(t *testing.T) {
	body := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-id",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":40},` + // pragma: allowlist secret — test fixture
		`"output_tokens":15,"output_tokens_details":{"reasoning_tokens":8},"total_tokens":135}}`
	resp, err := DecodeResponse([]byte(body), &DecodeOptions{Model: "model-x"})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	wantCharge(t, chargeFor(t, resp.Usage), responsesInvoice)
}

// TestAnExclusiveUsageIsNotReadAsInclusive pins the half of this that a fix gets
// backwards.
//
// `input_tokens` is spelled identically by two families that mean OPPOSITE
// things by it: the Anthropic family's EXCLUDES the cached prefix and the
// cache-creation count, and the Responses family's INCLUDES the cached prefix
// ([ResponsesUsage] says so, and records that reading it as exclusive bills a
// cached request about 1.8x over). Teaching this decoder the input key without
// also teaching it that the BREAKDOWN OBJECT settles the family would read a
// prompt of 170 as one of 120 and charge 70 uncached tokens where the vendor
// charged 120.
//
// The counts arrive here through a chat-shaped body because that is the only way
// they can: an Anthropic-native upstream is served by internal/wire/anthropic and
// never reaches this decoder. What does reach it is a proxy that renders an
// Anthropic answer in a chat envelope and leaves the usage block in the vendor's
// own spelling.
func TestAnExclusiveUsageIsNotReadAsInclusive(t *testing.T) {
	body := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-id",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"input_tokens":120,"cache_read_input_tokens":40,` + // pragma: allowlist secret — test fixture
		`"cache_creation_input_tokens":10,"output_tokens":15}}`
	resp, err := DecodeResponse([]byte(body), &DecodeOptions{Model: "model-x"})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage.InputTokens != 170 {
		t.Errorf("InputTokens = %d, want 170: this family reports input_tokens EXCLUSIVE of "+
			"the 40 read from cache and the 10 written to it, and dorang normalizes to "+
			"inclusive (DESIGN §10.7); usage = %+v", resp.Usage.InputTokens, resp.Usage)
	}
	//	uncached prompt   (170 - 40 - 10) x $0.85/1M = $0.000102_0
	//	cached prefix                 40 x $0.19/1M  = $0.000007_6
	//	cache write                   10 x $1.06/1M  = $0.000010_6
	//	completion                    15 x $3.40/1M  = $0.000051_0
	wantCharge(t, chargeFor(t, resp.Usage), map[string]struct{ qty, nano int64 }{
		"input":       {120, 102_000},
		"cache_read":  {40, 7_600},
		"cache_write": {10, 10_600},
		"output":      {15, 51_000},
	})
}

// TestTheChatSpellingIsUnchanged is the regression guard on the other side. The
// chat family's own spelling is unambiguous and must decode exactly as it did
// before the two new keys were understood.
func TestTheChatSpellingIsUnchanged(t *testing.T) {
	body := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-id",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":120,"completion_tokens":15,"total_tokens":135,` + // pragma: allowlist secret — test fixture
		`"prompt_tokens_details":{"cached_tokens":40},` +
		`"completion_tokens_details":{"reasoning_tokens":8}}}`
	resp, err := DecodeResponse([]byte(body), &DecodeOptions{Model: "model-x"})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	wantCharge(t, chargeFor(t, resp.Usage), responsesInvoice)
}

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

// TestImageCachedPrefixIsNotChargedAtTheFullRate is the image surface's version
// of the same billing rule.
//
// [ImageUsage] declares an `input_tokens_details` member, which reads like this
// surface already knows the Responses breakdown. It does not: the sub-shape it
// declares is text_tokens/image_tokens, and the decode block copied NEITHER the
// details nor anything else about them into the neutral form. A `cached_tokens`
// beside them — which is what an image model with a cached prompt reports — had
// no field to land in and no map to ride in, so it was dropped before anything
// could price it.
func TestImageCachedPrefixIsNotChargedAtTheFullRate(t *testing.T) {
	body := []byte(`{"created":1753660800,"data":[{"b64_json":"AAA"}],` +
		`"usage":{"input_tokens":120,"output_tokens":1500,"total_tokens":1620,` + // pragma: allowlist secret — test fixture
		`"input_tokens_details":{"text_tokens":20,"image_tokens":100,"cached_tokens":40}}}`)
	resp, err := DecodeImageResponse(body)
	if err != nil {
		t.Fatalf("DecodeImageResponse: %v", err)
	}
	if resp.Usage.CacheReadTokens != 40 {
		t.Errorf("CacheReadTokens = %d, want 40: the vendor stated a cached prefix in "+
			"input_tokens_details and this decoder never reads that object at all, so "+
			"§8.5 carves nothing out of the input rate and all 120 prompt tokens are "+
			"charged uncached", resp.Usage.CacheReadTokens)
	}
	//	uncached prompt   (120 - 40) x $0.85/1M = $0.000068_0
	//	cached prefix             40 x $0.19/1M = $0.000007_6
	//	rendered image         1,500 x $3.40/1M = $0.005100_0
	wantCharge(t, chargeFor(t, resp.Usage), map[string]struct{ qty, nano int64 }{
		"input":      {80, 68_000},
		"cache_read": {40, 7_600},
		"output":     {1500, 5_100_000},
	})
}

// TestImageUsageBreakdownSurvivesTheCrossing is the fidelity half. text_tokens
// and image_tokens are line items on somebody's invoice — the two halves of an
// image prompt are priced differently by every vendor that reports them — and
// dorang has no canonical counter for either, which is exactly the case Extra
// exists for. Neither [ImageUsage] nor the private breakdown struct it used to
// declare had one, so the whole thing was deleted on the way through.
func TestImageUsageBreakdownSurvivesTheCrossing(t *testing.T) {
	body := []byte(`{"created":1753660800,"data":[{"b64_json":"AAA"}],` +
		`"usage":{"input_tokens":9,"output_tokens":1500,"total_tokens":1509,` + // pragma: allowlist secret — test fixture
		`"input_tokens_details":{"text_tokens":9,"image_tokens":0}}}`)
	resp, err := DecodeImageResponse(body)
	if err != nil {
		t.Fatalf("DecodeImageResponse: %v", err)
	}
	got, err := MarshalImageResponse(resp)
	if err != nil {
		t.Fatalf("MarshalImageResponse: %v", err)
	}
	leaves := jsonLeaves(t, got)
	if leaves["usage.input_tokens_details.text_tokens"] != "9" {
		t.Errorf("usage.input_tokens_details.text_tokens = %q, want 9:\n%s",
			leaves["usage.input_tokens_details.text_tokens"], got)
	}
	if leaves["usage.input_tokens_details.image_tokens"] != "0" {
		t.Errorf("usage.input_tokens_details.image_tokens = %q, want a preserved "+
			"measured zero:\n%s", leaves["usage.input_tokens_details.image_tokens"], got)
	}
}

// TestImageUsageRecordsWhatTheBackendStated. A measured zero and an unmeasured
// count are different facts and only one of them is a measurement; see
// [canonical.UsageField]. Every other decoder in this package records which
// counters the backend actually stated, and the image decoder recorded none.
func TestImageUsageRecordsWhatTheBackendStated(t *testing.T) {
	body := []byte(`{"created":1,"data":[],"usage":{"input_tokens":0,"output_tokens":0,` +
		`"total_tokens":0,"input_tokens_details":{"text_tokens":0,"image_tokens":0,` + // pragma: allowlist secret — test fixture
		`"cached_tokens":0}}}`)
	resp, err := DecodeImageResponse(body)
	if err != nil {
		t.Fatalf("DecodeImageResponse: %v", err)
	}
	want := canonical.UsageInput | canonical.UsageOutput | canonical.UsageCacheRead
	if !resp.Usage.Reports(want) {
		t.Errorf("Reported = %b, want %b: the backend stated three counters and measured "+
			"zero on each, and nothing downstream can tell that from an answer that "+
			"reported no usage at all", resp.Usage.Reported, want)
	}
}

// ---------------------------------------------------------------------------
// Audio
// ---------------------------------------------------------------------------

// TestTranscriptionUsageRecordsWhatTheBackendStated is the audio surface's
// version of the same distinction, and the one this surface got wrong: it set no
// [canonical.UsageField] at all, so a transcript that the backend measured at
// zero tokens is indistinguishable from one that carried no usage block.
//
// That distinction is load-bearing — it was the whole of the `cached_tokens`
// defect closed for the chat family, where an encoder with only the integer to
// look at omitted a measured zero and a customer's cache accounting lost the row
// saying the cache had been consulted.
func TestTranscriptionUsageRecordsWhatTheBackendStated(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want canonical.UsageField
	}{{
		name: "a token-billed transcript",
		body: `{"text":"hi","usage":{"type":"tokens","input_tokens":14,` + // pragma: allowlist secret — test fixture
			`"output_tokens":4,"total_tokens":18}}`,
		want: canonical.UsageInput | canonical.UsageOutput,
	}, {
		// The measured zero. A backend that says "this transcript cost nothing"
		// has measured; one that sends no usage object has not.
		name: "a measured zero",
		body: `{"text":"","usage":{"type":"tokens","input_tokens":0,"output_tokens":0,` + // pragma: allowlist secret — test fixture
			`"total_tokens":0}}`,
		want: canonical.UsageInput | canonical.UsageOutput,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := DecodeTranscriptionResponse([]byte(tc.body), "application/json")
			if err != nil {
				t.Fatalf("DecodeTranscriptionResponse: %v", err)
			}
			if resp.Usage == nil {
				t.Fatal("a stated usage block decoded to no usage")
			}
			if !resp.Usage.Reports(tc.want) {
				t.Errorf("Reported = %b, want %b: this decoder is the only one in the "+
					"package that records nothing, so a measured zero here is "+
					"indistinguishable from an absent count",
					resp.Usage.Reported, tc.want)
			}
		})
	}
}

// TestATranscriptIsNotRelabelledAsTokenBilled is a defect found beside the one
// that was reported, on the same object.
//
// [TranscriptionUsage] models two billing units — tokens for a token-billed
// model, seconds for a duration-billed one — and its doc comment says "both are
// read". Neither `type` nor `seconds` was read on the way in, and the encoder
// wrote `"type":"tokens"` unconditionally on the way out. So a duration-billed
// transcript reached the client relabelled as token-billed, with a token count
// of zero and the billed duration deleted: an answer that states a unit it was
// not billed in and a quantity nobody charged.
func TestATranscriptIsNotRelabelledAsTokenBilled(t *testing.T) {
	body := []byte(`{"text":"hi","usage":{"type":"duration","seconds":12.5}}`)
	resp, err := DecodeTranscriptionResponse(body, "application/json")
	if err != nil {
		t.Fatalf("DecodeTranscriptionResponse: %v", err)
	}
	got, _, err := MarshalTranscriptionResponse(resp)
	if err != nil {
		t.Fatalf("MarshalTranscriptionResponse: %v", err)
	}
	leaves := jsonLeaves(t, got)
	if leaves["usage.type"] != `"duration"` {
		t.Errorf("usage.type = %q, want \"duration\": this transcript was billed by the "+
			"second and left saying it was billed by the token:\n%s", leaves["usage.type"], got)
	}
	if leaves["usage.seconds"] != "12.5" {
		t.Errorf("usage.seconds = %q, want 12.5; the billed quantity was dropped:\n%s",
			leaves["usage.seconds"], got)
	}
}

// TestTranscriptionBreakdownSurvivesTheCrossing. This surface's own breakdown
// object is `input_token_details` — singular `token`, a fourth spelling — and it
// splits an audio prompt into text and audio tokens, which are priced
// differently. dorang models no counter for either, which is what Extra is for,
// and [TranscriptionUsage] had none: every unmodelled member of this usage
// object was deleted on the way through.
func TestTranscriptionBreakdownSurvivesTheCrossing(t *testing.T) {
	body := []byte(`{"text":"hi","usage":{"type":"tokens","input_tokens":14,` + // pragma: allowlist secret — test fixture
		`"input_token_details":{"text_tokens":0,"audio_tokens":14},` +
		`"output_tokens":4,"total_tokens":18}}`)
	resp, err := DecodeTranscriptionResponse(body, "application/json")
	if err != nil {
		t.Fatalf("DecodeTranscriptionResponse: %v", err)
	}
	got, _, err := MarshalTranscriptionResponse(resp)
	if err != nil {
		t.Fatalf("MarshalTranscriptionResponse: %v", err)
	}
	leaves := jsonLeaves(t, got)
	if leaves["usage.input_token_details.audio_tokens"] != "14" {
		t.Errorf("usage.input_token_details.audio_tokens = %q, want 14:\n%s",
			leaves["usage.input_token_details.audio_tokens"], got)
	}
}
