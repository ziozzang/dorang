package backend

import (
	"context"
	"net/http"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// TestEmbeddingUsageIsMeteredWhateverTheVendorCallsIt.
//
// D3. [Result.Usage] is the single source for the X-Dorang-Tokens-* headers,
// for internal/meter and for pricing, and §11.6's token guard triggers only on
// Input+Output+Reasoning > 0. An embeddings answer that reported its count as
// total_tokens was read as zero, so the request was priced at zero AND was
// invisible to the guard — and nothing failed, so nothing alerted.
//
// Both bodies here are real. Jina direct answers with total_tokens alone; the
// same model behind LiteLLM answers with an explicit prompt_tokens of 0 beside
// a real total.
func TestEmbeddingUsageIsMeteredWhateverTheVendorCallsIt(t *testing.T) {
	const vectors = `"data":[{"object":"embedding","index":0,"embedding":[0.5,0.25]}]`

	for _, tc := range []struct {
		name  string
		usage string
		want  int
		kind  catalog.API
	}{
		{
			name: "jina direct states only a total", kind: catalog.APIJina,
			usage: `{"total_tokens":4}`, want: 4,
		},
		{
			name: "the same model through litellm states a zero prompt count",
			kind: catalog.APIOpenAIChat,
			// prompt_tokens is PRESENT and zero. The total is the measurement.
			usage: `{"prompt_tokens":0,"total_tokens":4}`, want: 4,
		},
		{
			name: "an openai-shaped vendor states both", kind: catalog.APIOpenAIChat,
			usage: `{"prompt_tokens":9,"total_tokens":9}`, want: 9,
		},
		{
			// prompt_tokens wins when it is non-zero: a vendor that states both
			// and means different things by them is stating the prompt count in
			// the field named for it.
			name: "prompt_tokens is preferred when it says something", kind: catalog.APIOpenAIChat,
			usage: `{"prompt_tokens":7,"total_tokens":11}`, want: 7,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, `{"object":"list","model":"upstream-model",`+
				vectors+`,"usage":`+tc.usage+`}`)
			p := testProvider(t, f, "openai", tc.kind)
			c := &Call{
				Op: OpEmbeddings, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
				Body: []byte(`{"model":"client-model","input":"hello"}`),
			}

			res := testBackend("sk").Do(context.Background(), target(p), c, nil)
			if res.Err != nil {
				t.Fatalf("Do: %v", res.Err)
			}
			if res.Usage.InputTokens != tc.want {
				t.Errorf("InputTokens = %d, want %d; this is the only number the "+
					"X-Dorang-Tokens-Input header, the meter and pricing ever see",
					res.Usage.InputTokens, tc.want)
			}
			if !res.Usage.Reports(canonical.UsageInput) {
				t.Error("the count is not marked as reported by the backend, so a downstream " +
					"encoder cannot tell a measured zero from an unmeasured one")
			}
			// §11.6's guard is gated on the token total being positive, and the
			// total is input plus output — reasoning is already inside output.
			if sum := res.Usage.TotalTokens(); sum == 0 {
				t.Error("the token guard of §11.6 cannot see this request at all")
			}
		})
	}
}
