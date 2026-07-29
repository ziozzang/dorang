package rerank

import (
	"fmt"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/pricing"
)

// A rerank rate card. Rerank has no completion half — the whole count is the
// prompt — so the input rate is the only token rate a vendor quotes for it.
const rerankRateCard = `
currency: USD
rules:
  - id: vendor-list-rate
    class: marginal_usage
    match: { provider: plan-a, model: model-x }
    unit: per_1m_tokens
    input: "0.85"
`

func chargeForRerank(t *testing.T, u *canonical.Usage) pricing.Cost {
	t.Helper()
	if u == nil {
		t.Fatal("the decoder produced no usage at all")
	}
	cat, err := pricing.ParseCatalog([]byte(rerankRateCard))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	cost, err := cat.Price(pricing.Request{
		Provider: "plan-a", Model: "model-x",
		InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens),
		Requests: 1,
		At:       time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	return cost
}

// TestARerankIsMeteredWhateverTheVendorSpellsItsPromptCount.
//
// [BilledUnits]'s own comment names `input_tokens` and `output_tokens` as
// members this package sees and does not model, and [Usage] read neither: a
// rerank backend that reports its prompt count under that spelling metered as
// ZERO tokens and was priced at nothing.
//
// Zero is not a harmless number on this path. The count is the single source for
// the X-Dorang-Tokens-* headers, for internal/meter and for pricing, and §11.6's
// token guard triggers on a positive token total — so a rerank priced at zero is
// also a rerank the guard cannot see. It is the same defect internal/backend's
// scanRelayUsage closed for embeddings, on the surface that was cited there as
// the one that metered correctly.
func TestARerankIsMeteredWhateverTheVendorSpellsItsPromptCount(t *testing.T) {
	const results = `"results":[{"index":0,"relevance_score":0.9}]`

	for _, tc := range []struct {
		name  string
		usage string
		want  int
	}{{
		// The generic/Jina spelling, which this package always read.
		name: "prompt_tokens", usage: `{"prompt_tokens":400,"total_tokens":400}`, want: 400,
	}, {
		// A total alone. An embedding and a rerank have no completion half, so
		// the total IS the prompt count.
		name: "total_tokens alone", usage: `{"total_tokens":400}`, want: 400,
	}, {
		// The spelling [BilledUnits] names and [Usage] did not model.
		name: "input_tokens", usage: `{"input_tokens":400}`, want: 400,
	}, {
		name:  "input_tokens with a total",
		usage: `{"input_tokens":400,"total_tokens":400}`, want: 400,
	}, {
		// A reranker that scores rather than generates still reports an output
		// half on some hosts. It is a real count and belongs in the neutral form.
		name:  "input_tokens beside output_tokens",
		usage: `{"input_tokens":400,"output_tokens":0,"total_tokens":400}`, want: 400,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"id":"rr_1","model":"upstream-id",` + results + `,"usage":` + tc.usage + `}`)
			out, err := DecodeResponse(body, FlavorGeneric, "model-x")
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			if out.Usage == nil {
				t.Fatal("a stated usage block decoded to no usage at all")
			}
			if out.Usage.InputTokens != tc.want {
				t.Errorf("InputTokens = %d, want %d; this is the only number the "+
					"X-Dorang-Tokens-Input header, internal/meter and pricing ever see "+
					"for this request", out.Usage.InputTokens, tc.want)
			}
			if !out.Usage.Reports(canonical.UsageInput) {
				t.Error("the count is not marked as reported by the backend, so the " +
					"encoder cannot tell a measured zero from an unmeasured one")
			}
			// 400 x $0.85/1M = $0.000340000.
			cost := chargeForRerank(t, out.Usage)
			const wantNano = 340_000
			if cost.MarginalNano != wantNano {
				t.Errorf("charged %d nano ($%s), the vendor's invoice is %d ($%s)",
					cost.MarginalNano, rerankNano9(cost.MarginalNano), wantNano,
					rerankNano9(wantNano))
			}
		})
	}
}

// TestARerankOutputCountIsNotFoldedIntoThePrompt. `output_tokens` is a second
// quantity, priced by a second rate, and folding it into the prompt count would
// bill generation at the input rate — the mirror of the mistake §10.7 records
// for cache counts.
//
// The second case is the one that decides the rule: with no prompt count under
// any spelling, the total is the only number left, and a total that CONTAINS the
// stated output half is not a prompt count until the output half comes off it.
func TestARerankOutputCountIsNotFoldedIntoThePrompt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		usage     string
		in, outTk int
	}{{
		name:  "a stated prompt count beside a stated output count",
		usage: `{"input_tokens":400,"output_tokens":7,"total_tokens":407}`,
		in:    400, outTk: 7,
	}, {
		name:  "a total that contains the output half, and no prompt count",
		usage: `{"output_tokens":7,"total_tokens":407}`,
		in:    400, outTk: 7,
	}, {
		// A total smaller than the output half it is supposed to contain is a
		// contradiction. Zero, never a negative: a negative count hands back
		// budget and quota nobody paid for.
		name:  "a contradictory total does not credit",
		usage: `{"output_tokens":7,"total_tokens":3}`,
		in:    0, outTk: 7,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"id":"rr_1","model":"upstream-id","results":[],"usage":` +
				tc.usage + `}`)
			out, err := DecodeResponse(body, FlavorGeneric, "model-x")
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			if out.Usage.InputTokens != tc.in {
				t.Errorf("InputTokens = %d, want %d; charging the prompt rate on a total "+
					"that already contains the output half bills generation as prompt",
					out.Usage.InputTokens, tc.in)
			}
			if out.Usage.OutputTokens != tc.outTk {
				t.Errorf("OutputTokens = %d, want %d", out.Usage.OutputTokens, tc.outTk)
			}
		})
	}
}

// TestARerankUsageBreakdownSurvivesTheCrossing. Whatever this package does not
// model rides in Extra rather than being deleted for not being interesting to
// the router — that is what [BilledUnits] says about search units and what
// [Usage] must equally hold for the token block.
func TestARerankUsageBreakdownSurvivesTheCrossing(t *testing.T) {
	body := []byte(`{"id":"rr_1","model":"upstream-id","results":[],` +
		`"usage":{"input_tokens":400,"classifications":3,"total_tokens":400}}`) // pragma: allowlist secret — test fixture
	out, err := DecodeResponse(body, FlavorGeneric, "model-x")
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if _, ok := out.UsageExtra["classifications"]; !ok {
		t.Errorf("the unmodelled `classifications` line item was dropped: %v", out.UsageExtra)
	}
	if _, ok := out.UsageExtra["input_tokens"]; ok {
		t.Error("input_tokens is modelled now and must not ALSO ride in Extra, or the " +
			"encoder emits the same count twice under two names")
	}
}

func rerankNano9(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%09d", sign, n/1e9, n%1e9)
}
