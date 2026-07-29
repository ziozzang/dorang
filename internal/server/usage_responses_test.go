package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/pricing"
)

// The vendor's rate card, quoted the way a vendor quotes one: a list rate for
// the prompt and the completion, and a lower rate for the part of the prompt it
// served from cache and a separate one for the reasoning it produced.
//
// It is the same shape as internal/pricing's own designRateCard, and the input
// half carries the same three numbers, so the figure below can be compared line
// for line against TestChargedAmountMatchesTheVendorInvoice. That comparison is
// the point of this file: the SAME request, priced through the Responses-API
// spelling of its usage block, must cost what it costs through the chat one.
const responsesRateCard = `
currency: USD
rules:
  - id: vendor-list-rate
    class: marginal_usage
    match: { provider: plan-a, model: model-x }
    unit: per_1m_tokens
    input:  "0.85"
    output: "3.40"
    cache_read: "0.19"
    reasoning: "6.80"
`

// A Responses-API answer, as a native /v1/responses upstream writes it.
//
// The prompt count is 120 and INCLUSIVE of the 40 the vendor served from cache;
// the completion is 15 and inclusive of the 8 it spent reasoning. Both
// breakdowns live one level down, under spellings the chat family does not use:
// input_tokens_details.cached_tokens and output_tokens_details.reasoning_tokens.
const responsesUsageBody = `{"id":"resp_1","object":"response","status":"completed",` +
	`"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":40},` + // pragma: allowlist secret — test fixture
	`"output_tokens":15,"output_tokens_details":{"reasoning_tokens":8},` +
	`"total_tokens":135}}`

// TestResponsesCachedRequestIsChargedTheVendorFigure prices one relayed
// Responses-API answer and compares the charge against the vendor's invoice,
// computed by hand from the vendor's own rate card.
//
// Asserting that the decoded struct carries a cached count would not settle
// this. The defect is not that a field is empty; it is that the bill is wrong,
// and the bill is wrong in the direction the customer pays. §8.5's convention is
// that a declared sub-rate CARVES its quantity out of its parent's rate: with
// `cache_read` declared, the `input` rate is charged on InputTokens −
// CacheReadTokens. A CacheReadTokens of zero therefore does not merely lose a
// line of the breakdown — it charges the whole inclusive prompt at the full
// uncached rate, which is exactly the overcharge closed for the chat family in
// internal/pricing/convention_test.go, still live on this surface because one
// decoder knew a spelling the other did not.
//
// The counts travel from [scanUsage] to [pricing.Request] field for field, as
// internal/app does at dispatch.go's settle: Input→InputTokens,
// CacheRead→CacheReadTokens, Output→OutputTokens, Reasoning→ReasoningTokens.
func TestResponsesCachedRequestIsChargedTheVendorFigure(t *testing.T) {
	cat, err := pricing.ParseCatalog([]byte(responsesRateCard))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}

	u, ok := scanUsage([]byte(responsesUsageBody))
	if !ok {
		t.Fatal("scanUsage found no usage object in a Responses-API answer")
	}

	cost, err := cat.Price(pricing.Request{
		Provider: "plan-a", Model: "model-x",
		InputTokens:     u.Input,
		OutputTokens:    u.Output,
		CacheReadTokens: u.CacheRead,
		ReasoningTokens: u.Reasoning,
		Requests:        1,
		At:              time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}

	// The invoice, by hand, from the vendor's rate card:
	//
	//	uncached prompt      (120 - 40) x $0.85/1M = $0.000068_0
	//	cached prefix                40 x $0.19/1M = $0.000007_6
	//	completion less reasoning (15 - 8) x $3.40/1M = $0.000023_8
	//	reasoning                     8 x $6.80/1M = $0.000054_4
	//	                                             ------------
	//	                                             $0.000153_8
	const (
		vendorNano = 153_800 // $0.0001538
		// What dorang charges when the two details objects are dropped: the
		// input rate against the whole inclusive prompt, and the output rate
		// against the whole inclusive completion, because both sub-rates carve
		// out a quantity that decoded as zero.
		//
		//	whole prompt   120 x $0.85/1M = $0.000102_0
		//	whole output    15 x $3.40/1M = $0.000051_0
		//	                                ------------
		//	                                $0.000153_0
		blindNano = 153_000
	)

	if cost.MarginalNano == blindNano {
		t.Fatalf("charged %d nano ($%s) against the vendor's %d nano ($%s): the "+
			"Responses-API usage block decoded with CacheRead = %d and Reasoning = %d, "+
			"so `cache_read` carved nothing out of `input` and `reasoning` carved "+
			"nothing out of `output` -- the 40 cached prompt tokens were billed at "+
			"the full uncached rate and the 8 reasoning tokens at the plain output "+
			"rate. input_tokens_details.cached_tokens and "+
			"output_tokens_details.reasoning_tokens are the spellings this family uses",
			cost.MarginalNano, nanoUSD(cost.MarginalNano), vendorNano, nanoUSD(vendorNano),
			u.CacheRead, u.Reasoning)
	}
	if cost.MarginalNano != vendorNano {
		t.Fatalf("charged %d nano ($%s), the vendor's invoice is %d nano ($%s); "+
			"scanned usage = %+v", cost.MarginalNano, nanoUSD(cost.MarginalNano),
			vendorNano, nanoUSD(vendorNano), u)
	}
	if cost.TotalNano != vendorNano {
		t.Fatalf("TotalNano = %d, want %d", cost.TotalNano, vendorNano)
	}

	// The two halves err in OPPOSITE directions on this rate card — the lost
	// cached count overcharges the prompt, the lost reasoning count undercharges
	// the completion — and on a card whose reasoning rate happened to sit at the
	// right multiple of its output rate they would cancel in the total. So the
	// total is not the whole assertion: each line is pinned to the quantity it
	// was CHARGED for, which is also what an operator reconciles a bill against.
	//
	//	input      120 - 40 cached    =  80 x $0.85/1M
	//	cache_read              40    =  40 x $0.19/1M
	//	output      15 - 8 reasoning  =   7 x $3.40/1M
	//	reasoning                8    =   8 x $6.80/1M
	want := map[string]struct{ qty, nano int64 }{
		"input":      {80, 68_000},
		"cache_read": {40, 7_600},
		"output":     {7, 23_800},
		"reasoning":  {8, 54_400},
	}
	if len(cost.Components) != len(want) {
		t.Fatalf("components = %+v, want %d lines", cost.Components, len(want))
	}
	var sum int64
	for _, comp := range cost.Components {
		w, ok := want[comp.Name]
		if !ok {
			t.Fatalf("unexpected component %q", comp.Name)
		}
		if comp.Quantity != w.qty || comp.SubtotalNano != w.nano {
			t.Errorf("component %s = %d tokens / %d nano, want %d / %d",
				comp.Name, comp.Quantity, comp.SubtotalNano, w.qty, w.nano)
		}
		sum += comp.SubtotalNano
	}
	if sum != cost.MarginalNano {
		t.Errorf("component subtotals sum to %d, marginal is %d", sum, cost.MarginalNano)
	}
}

// TestResponsesUsageIsNotDoubleCounted pins the half of this that a fix could
// easily get backwards.
//
// `input_tokens` is spelled identically by two families that mean OPPOSITE
// things by it: Anthropic's EXCLUDES the cached prefix, and the OpenAI Responses
// API's INCLUDES it (internal/wire/openai's ResponsesUsage says so, and records
// that reading it as exclusive bills a cached request about 1.8x over). So
// teaching the scanner the two details spellings without also teaching it that
// they settle the family would trade a lost cached count for an inflated prompt:
// 120 + 40 = 160 input tokens on a request whose own body said 120.
func TestResponsesUsageIsNotDoubleCounted(t *testing.T) {
	u, ok := scanUsage([]byte(responsesUsageBody))
	if !ok {
		t.Fatal("scanUsage found no usage object in a Responses-API answer")
	}
	want := Usage{Input: 120, Output: 15, CacheRead: 40, Reasoning: 8, Total: 135}
	if u != want {
		t.Errorf("scanUsage(Responses answer) = %+v, want %+v", u, want)
	}
}

// TestResponsesShapeDoesNotDisturbAnthropic guards the other side of the same
// decision: the Anthropic family keeps its exclusive reading, whichever order
// its keys arrive in.
func TestResponsesShapeDoesNotDisturbAnthropic(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Usage
	}{{
		// input_tokens EXCLUDES the 40 cached, so the inclusive prompt is 160.
		name: "anthropic",
		body: `{"usage":{"input_tokens":120,"cache_read_input_tokens":40,` + // pragma: allowlist secret — test fixture
			`"cache_creation_input_tokens":10,"output_tokens":15}}`,
		want: Usage{Input: 170, Output: 15, CacheRead: 40, CacheWrite: 10, Total: 185},
	}, {
		// The chat family, unchanged: prompt_tokens already contains the cached
		// prefix and the details objects are spelled prompt_/completion_.
		name: "chat",
		body: `{"usage":{"prompt_tokens":120,"completion_tokens":15,"total_tokens":135,` + // pragma: allowlist secret — test fixture
			`"prompt_tokens_details":{"cached_tokens":40},` +
			`"completion_tokens_details":{"reasoning_tokens":8}}}`,
		want: Usage{Input: 120, Output: 15, CacheRead: 40, Reasoning: 8, Total: 135},
	}, {
		// The details object arriving BEFORE the count it breaks down. JSON
		// members are unordered, so the family cannot be settled by whichever
		// key the scanner happened to reach first.
		name: "responses-details-first",
		body: `{"usage":{"input_tokens_details":{"cached_tokens":40},"input_tokens":120,` + // pragma: allowlist secret — test fixture
			`"output_tokens_details":{"reasoning_tokens":8},"output_tokens":15}}`,
		want: Usage{Input: 120, Output: 15, CacheRead: 40, Reasoning: 8, Total: 135},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scanUsage([]byte(tc.body))
			if !ok {
				t.Fatal("scanUsage found no usage object")
			}
			if got != tc.want {
				t.Errorf("scanUsage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// nanoUSD renders a nano-USD amount as a decimal, for failure messages.
func nanoUSD(nano int64) string {
	sign := ""
	if nano < 0 {
		sign, nano = "-", -nano
	}
	return fmt.Sprintf("%s%d.%09d", sign, nano/1e9, nano%1e9)
}
