package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/pricing"
)

// TestShippedExamplePricesAKnownRequest is the assertion the shipped example
// did not have, and its absence is the whole defect.
//
// deploy/config.example.yaml carried per-TOKEN rates — `input: "0.0000025"` —
// into a field the engine reads per MILLION tokens, so every request it priced
// cost a MILLIONTH of what it should have — which rounds to a flat zero for all
// but the largest. Nothing an operator can run said so. The file parses;
// `dorangctl config lint` answers ok; `dorang --check` answers ok; the rule is
// selected; and
// `POST /spend/calculate` answers `"missing": false`, because `missing` reports
// that no rule matched and one did.
//
// So the test cannot be "the example parses" or "a rule matched" — both of
// those passed throughout. It has to be a NUMBER, computed by hand from the
// rates in the file, against the code that actually prices requests.
func TestShippedExamplePricesAKnownRequest(t *testing.T) {
	cfg := loadShippedExample(t)

	cat, err := app.Pricing(cfg)
	if err != nil {
		t.Fatalf("the shipped example must compile into a price catalog: %v", err)
	}
	target, ok := app.ResolveTarget(cfg, "model-x")
	if !ok {
		t.Fatal("the shipped example no longer declares model-x")
	}

	// 12,000 prompt tokens of which 2,000 came from cache, and 800 completion
	// tokens. The carve-out of CONFIG §13.1a charges 10,000 at the input rate.
	//
	//   10,000 / 1,000,000 x 2.50  = 0.025
	//    2,000 / 1,000,000 x 0.25  = 0.0005
	//      800 / 1,000,000 x 10.00 = 0.008
	//                                ------
	//                                0.0335 USD = 33,500,000 nanoUSD
	//
	// Against the per-token spelling of the same card the answer is 34 nanoUSD
	// — a millionth of the figure — and exactly 0 for any request small enough
	// to round down, which is most of them.
	const wantMarginalNano = 33_500_000

	x := cat.Explain(pricing.Request{
		Provider:        target.Provider,
		Model:           target.UpstreamModel,
		Credential:      target.Credential,
		Deployment:      target.Deployment,
		InputTokens:     12_000,
		CacheReadTokens: 2_000,
		OutputTokens:    800,
		Requests:        1,
		At:              time.Now(),
	})
	if x.Err != "" {
		t.Fatalf("pricing the shipped example failed: %s", x.Err)
	}

	// Marginal, and only marginal. The subscription share is the plan cost
	// accrued since the previous settlement, so it moves with the clock; the
	// adjustment is a percentage of a figure that includes it. Marginal is the
	// figure routing compares and the one a rate card can be checked against.
	if x.Cost.MarginalNano != wantMarginalNano {
		t.Errorf("the shipped example prices this request at %d nanoUSD, want %d.\n"+
			"A figure 1,000,000x too small (or a flat 0) means the rates are "+
			"per-token in a per-million field (CONFIG §13.1c); one 1,000,000x "+
			"too large means the reverse.",
			x.Cost.MarginalNano, wantMarginalNano)
	}

	// `missing` is not the check, and this pins why. It was false the whole
	// time the example was wrong.
	if x.Cost.Missing {
		t.Error("no marginal rule matched the shipped example's own model")
	}
}

// TestShippedExampleRatesAreOnTheScaleOfARateCard guards the file itself rather
// than one request through it. A rate three orders of magnitude below any real
// card is a unit error, and the arithmetic cannot tell — it multiplies whatever
// it is given.
func TestShippedExampleRatesAreOnTheScaleOfARateCard(t *testing.T) {
	cfg := loadShippedExample(t)

	// Per million tokens, the cheapest models on the market are around $0.02
	// and the most expensive well over $100. A rate below a thousandth of a
	// cent per million tokens is not a price, it is a per-token figure that
	// landed in a per-million field.
	const implausiblyCheap = 0.00001

	found := 0
	for _, r := range cfg.Pricing.Rules {
		for name, rate := range r.Rates {
			if u, ok := config.PricingUnit([]string{name}); !ok || u != "per_1m_tokens" {
				continue
			}
			found++
			v, err := parseRate(string(rate))
			if err != nil {
				t.Fatalf("rules[%s].rates.%s = %q: %v", r.ID, name, rate, err)
			}
			if v > 0 && v < implausiblyCheap {
				t.Errorf("rules[%s].rates.%s = %q is %g per MILLION tokens, which is "+
					"three orders of magnitude below any real rate card. Token rates "+
					"are per million: $2.50/Mtok is \"2.50\", never \"0.0000025\" "+
					"(CONFIG §13.1c).", r.ID, name, rate, v)
			}
		}
	}
	if found == 0 {
		t.Fatal("the shipped example declares no token rates: this test proves nothing")
	}
}

// loadShippedExample loads deploy/config.example.yaml with only its secret
// references redirected — the pricing block is the file's own bytes.
func loadShippedExample(t *testing.T) *config.Config {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "config.example.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("example configuration not present: %v", err)
	}

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "cloud-a-2.key")
	if err := os.WriteFile(keyFile, []byte("example-key-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(data), "/etc/dorang/secrets/cloud-a-2.key", keyFile)

	t.Setenv("DORANG_EXAMPLE_PLAN_A_KEY", "example-key-plan-a")
	t.Setenv("DORANG_EXAMPLE_CLOUD_A_KEY_1", "example-key-1")
	t.Setenv("DORANG_EXAMPLE_FILTER_SECRET", "example-filter-seed")

	cfg, err := config.LoadBytes([]byte(src))
	if err != nil {
		t.Fatalf("the shipped example must load:\n%v", err)
	}
	return cfg
}

// parseRate reads a decimal rate the way a human reads a rate card. It is
// deliberately not the engine's parser: this test is asking whether the number
// is on the scale of a price, not whether the engine can consume it.
func parseRate(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	return f, err
}
