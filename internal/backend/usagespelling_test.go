package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The vendor's rate card, the same shape and the same numbers as
// internal/pricing's designRateCard, so a figure here can be compared line for
// line against that file.
const backendRateCard = `
currency: USD
rules:
  - id: vendor-list-rate
    class: marginal_usage
    match: { provider: plan-a, model: model-x }
    unit: per_1m_tokens
    input:      "0.85"
    output:     "3.40"
    cache_read: "0.19"
    reasoning:  "6.80"
`

func chargeForUsage(t *testing.T, u canonical.Usage) pricing.Cost {
	t.Helper()
	cat, err := pricing.ParseCatalog([]byte(backendRateCard))
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

func usd9nano(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%09d", sign, n/1e9, n%1e9)
}

// TestEmbeddingUsageIsMeteredUnderTheInputTokensSpelling extends
// [TestEmbeddingUsageIsMeteredWhateverTheVendorCallsIt] with the third spelling.
//
// [scanRelayUsage] read `prompt_tokens` and fell back to `total_tokens`, and an
// embeddings upstream that reports `input_tokens` — the spelling of both the
// Anthropic family and the OpenAI Responses family, and the one a
// Responses-generation embeddings host uses — matched neither. It metered as
// ZERO tokens: priced at nothing, invisible to §11.6's token guard, and absent
// from every per-key budget. Embeddings were already found priced at zero once
// on this path, for a different reason, which is what makes an unmeasured
// spelling here worth a test rather than a comment.
func TestEmbeddingUsageIsMeteredUnderTheInputTokensSpelling(t *testing.T) {
	const vectors = `"data":[{"object":"embedding","index":0,"embedding":[0.5,0.25]}]`

	for _, tc := range []struct {
		name  string
		usage string
		want  int
	}{
		{name: "input_tokens alone", usage: `{"input_tokens":400}`, want: 400},
		{
			name:  "input_tokens beside a total",
			usage: `{"input_tokens":400,"total_tokens":400}`, want: 400,
		},
		{
			// prompt_tokens wins whenever it says something, whichever other
			// spellings are beside it.
			name:  "prompt_tokens still wins",
			usage: `{"prompt_tokens":400,"input_tokens":7,"total_tokens":400}`, want: 400,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, `{"object":"list","model":"upstream-model",`+
				vectors+`,"usage":`+tc.usage+`}`)
			p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
			c := &Call{
				Op: OpEmbeddings, ClientAPI: catalog.APIOpenAIChat, Model: "model-x",
				Body: []byte(`{"model":"model-x","input":"hello"}`),
			}
			res := testBackend("sk").Do(context.Background(), target(p), c, nil)
			if res.Err != nil {
				t.Fatalf("Do: %v", res.Err)
			}
			if res.Usage.InputTokens != tc.want {
				t.Errorf("InputTokens = %d, want %d; this is the only number the "+
					"X-Dorang-Tokens-Input header, internal/meter and pricing ever see",
					res.Usage.InputTokens, tc.want)
			}
			if !res.Usage.Reports(canonical.UsageInput) {
				t.Error("the count is not marked as reported by the backend")
			}
			if res.Usage.TotalTokens() == 0 {
				t.Error("§11.6's token guard cannot see this request at all")
			}
			// 400 x $0.85/1M = $0.000340000.
			cost := chargeForUsage(t, res.Usage)
			const wantNano = 340_000
			if cost.MarginalNano != wantNano {
				t.Errorf("charged %d nano ($%s), the vendor's invoice is %d ($%s)",
					cost.MarginalNano, usd9nano(cost.MarginalNano),
					wantNano, usd9nano(wantNano))
			}
		})
	}
}

// TestAResponsesShapedUpstreamAnswerIsCharged is surface 1 measured through the
// dispatcher rather than through the decoder alone: a provider whose chat route
// answers in the Responses shape, which is what a host serving one API on both
// paths does and what switching openaiAdapter's OpResponses route to
// /v1/responses would make routine.
//
// The prompt count is 120 and inclusive of the 40 the vendor served from cache;
// the completion is 15 and inclusive of the 8 spent reasoning. The invoice, by
// hand, from the rate card above:
//
//	uncached prompt          (120 - 40) x $0.85/1M = $0.000068_0
//	cached prefix                    40 x $0.19/1M = $0.000007_6
//	completion less reasoning  (15 - 8) x $3.40/1M = $0.000023_8
//	reasoning                         8 x $6.80/1M = $0.000054_4
//	                                                 ------------
//	                                                 $0.000153_8
func TestAResponsesShapedUpstreamAnswerIsCharged(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"resp_1","object":"response","status":"completed",`+
		`"model":"upstream-model","created_at":1753660800,`+
		`"output":[{"type":"message","role":"assistant","status":"completed",`+
		`"content":[{"type":"output_text","text":"hi"}]}],`+
		`"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":40},`+ // pragma: allowlist secret — test fixture
		`"output_tokens":15,"output_tokens_details":{"reasoning_tokens":8},`+
		`"total_tokens":135}}`)
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := &Call{
		Op: OpResponses, ClientAPI: catalog.APIOpenAIChat, Model: "model-x",
		Body:    []byte(`{"model":"model-x","input":"hello"}`),
		Request: &canonical.Request{Model: "model-x", Messages: []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hello")}},
	}
	res := testBackend("sk").Do(context.Background(), target(p), c, nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	// The counts are not the only thing under a spelling the chat decoder does
	// not know. This family puts its assistant turn in `output` rather than
	// `choices`, so a chat decode returns a successful answer with no content —
	// which is why the ANSWER's shape selects the decoder rather than the
	// operation dorang addressed. Before that, openai.ResponsesResponseToCanonical
	// was correct, tested, and reachable from nothing but its own test.
	if !bytes.Contains(res.Body, []byte(`hi`)) {
		t.Errorf("the assistant turn did not reach the client: %s", res.Body)
	}
	want := map[string]struct{ qty, nano int64 }{
		"input":      {80, 68_000},
		"cache_read": {40, 7_600},
		"output":     {7, 23_800},
		"reasoning":  {8, 54_400},
	}
	cost := chargeForUsage(t, res.Usage)
	got := map[string]struct{ qty, nano int64 }{}
	for _, comp := range cost.Components {
		got[comp.Name] = struct{ qty, nano int64 }{comp.Quantity, comp.SubtotalNano}
	}
	var sum int64
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s charged %d tokens / %d nano, want %d / %d; decoded usage = %+v",
				name, got[name].qty, got[name].nano, w.qty, w.nano, res.Usage)
		}
		sum += w.nano
	}
	if cost.MarginalNano != sum {
		t.Errorf("charged %d nano ($%s) in total, the vendor's invoice is %d ($%s)",
			cost.MarginalNano, usd9nano(cost.MarginalNano), sum, usd9nano(sum))
	}
}

// TestGeminiUsageRecordsWhatTheBackendStated is a surface beyond the five that
// were reported, found by looking for the same defect rather than the same
// spelling.
//
// [geminiUsageToCanonical] does the two normalizations §10.7 requires and
// documents both — and sets no [canonical.UsageField] at all, exactly like the
// image and audio decoders. The consequence is live and it is the same one
// closed for the chat family: internal/wire/openai's EncodeUsage emits
// prompt_tokens_details only when the cache count is positive OR was reported,
// so a Gemini answer that served nothing from cache reaches an OpenAI client
// with no cache row at all — indistinguishable from a deployment that has no
// cache, which is what a customer's cache accounting is reconciled against.
//
// The fix is not a blanket flag. `cachedContentTokenCount` and
// `thoughtsTokenCount` are ABSENT from this vendor's usage object when they are
// zero, so reporting them unconditionally would invent the measurement instead
// of losing it — the same false zero, pointing the other way.
func TestGeminiUsageRecordsWhatTheBackendStated(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want canonical.UsageField
	}{{
		name: "a cache hit and a thinking turn",
		body: `{"promptTokenCount":100,"candidatesTokenCount":20,` + // pragma: allowlist secret — test fixture
			`"cachedContentTokenCount":40,"thoughtsTokenCount":8,"totalTokenCount":128}`,
		want: canonical.UsageInput | canonical.UsageOutput |
			canonical.UsageCacheRead | canonical.UsageReasoning,
	}, {
		// The measured zero: the vendor stated the fields and both were zero.
		name: "a measured zero on both breakdowns",
		body: `{"promptTokenCount":100,"candidatesTokenCount":20,` + // pragma: allowlist secret — test fixture
			`"cachedContentTokenCount":0,"thoughtsTokenCount":0,"totalTokenCount":120}`,
		want: canonical.UsageInput | canonical.UsageOutput |
			canonical.UsageCacheRead | canonical.UsageReasoning,
	}, {
		// Neither breakdown was mentioned. Reporting one here would say the
		// cache returned nothing on a deployment that never mentioned a cache.
		name: "no breakdown stated at all",
		body: `{"promptTokenCount":100,"candidatesTokenCount":20,"totalTokenCount":120}`, // pragma: allowlist secret — test fixture
		want: canonical.UsageInput | canonical.UsageOutput,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var w geminiUsage
			if err := json.Unmarshal([]byte(tc.body), &w); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			u := geminiUsageToCanonical(&w)
			if u == nil {
				t.Fatal("a stated usageMetadata decoded to no usage")
			}
			if u.Reported != tc.want {
				t.Errorf("Reported = %b, want %b; usage = %+v", u.Reported, tc.want, u)
			}
		})
	}
}
