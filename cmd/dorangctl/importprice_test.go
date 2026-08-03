package main

import (
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/pricing"
	"gopkg.in/yaml.v3"
)

// theIncumbentsCard is a rate card as the file dorang is migrated FROM writes
// one: $2.50 / $10.00 / $0.25 per million tokens, quoted the only way that
// format can quote them — per ONE token.
const theIncumbentsCard = `
model_list:
  - model_name: chat-large
    litellm_params:
      model: gpt-4o
      custom_llm_provider: openai
      api_base: https://api.openai.com/v1
      api_key: os.environ/DORANG_TEST_IMPORT_KEY
      input_cost_per_token: 0.0000025
      output_cost_per_token: 0.00001
      cache_read_input_token_cost: 0.00000025
`

// TestAnImportedCardPricesAKnownRequest is the assertion the importer did not
// have, and its absence is the whole defect.
//
// `input_cost_per_token: 0.0000025` was copied verbatim into `rates.input`,
// which the engine reads per MILLION tokens (CONFIG §13.1c). So every request
// an imported price list priced cost a MILLIONTH of the vendor's figure — a
// flat zero below about two hundred tokens — and nothing an operator can run
// said so. The generated file parses; `dorangctl config lint` answers ok;
// `dorang --check` answers ok; the rule is selected; and `POST
// /spend/calculate` answers `"missing": false`, because `missing` reports that
// no rule matched and one did.
//
// The test that used to guard this asserted the FIELD — that the literal came
// through unchanged — which is what locked the defect in. This one asserts the
// CHARGE, against a figure computed by hand from the vendor's card, through the
// same round trip a migration makes: import, write the file, load it, price a
// request.
func TestAnImportedCardPricesAKnownRequest(t *testing.T) {
	t.Setenv("DORANG_TEST_IMPORT_KEY", "example-key")

	cfg := importAndReload(t, theIncumbentsCard)

	cat, err := app.Pricing(cfg)
	if err != nil {
		t.Fatalf("an imported configuration must compile into a price catalog: %v", err)
	}
	target, ok := app.ResolveTarget(cfg, "chat-large")
	if !ok {
		t.Fatal("the imported configuration does not declare chat-large")
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
	// Against the unconverted import the answer is 34 nanoUSD — a millionth of
	// the figure — and exactly 0 for any request small enough to round down,
	// which is most of them.
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
		t.Fatalf("pricing an imported card failed: %s", x.Err)
	}
	if x.Cost.MarginalNano != wantMarginalNano {
		t.Errorf("an imported card prices this request at %d nanoUSD, want %d.\n"+
			"A figure 1,000,000x too small (or a flat 0) means the importer copied the "+
			"source file's PER-TOKEN rates into dorang's PER-MILLION field without "+
			"converting them (CONFIG §13.1c).", x.Cost.MarginalNano, wantMarginalNano)
	}
	if x.Cost.Missing {
		t.Error("no marginal rule matched the imported model")
	}
}

// TestAnImportedCardIsNotFlaggedAsAUnitError is the second half: the converted
// file has to survive the check that catches the unconverted one. If the
// importer ever rescales twice, or not at all, this fires before an operator
// deploys.
func TestAnImportedCardIsNotFlaggedAsAUnitError(t *testing.T) {
	t.Setenv("DORANG_TEST_IMPORT_KEY", "example-key")
	cfg := importAndReload(t, theIncumbentsCard)

	if got := cfg.Advisories(); len(got) != 0 {
		t.Errorf("`dorangctl config lint` would warn about an imported card: %+v", got)
	}
	rates := map[string]string{}
	for _, r := range cfg.Pricing.Rules {
		for comp, v := range r.Rates {
			rates[comp] = string(v)
		}
	}
	for comp, want := range map[string]string{"input": "2.5", "output": "10", "cached_read": "0.25"} {
		if rates[comp] != want {
			t.Errorf("rates.%s = %q, want %q per MILLION tokens", comp, rates[comp], want)
		}
	}
}

// importAndReload runs the whole migration path: convert the foreign file,
// write dorang's YAML, and load that YAML the way a gateway does. It is the
// round trip rather than the in-memory result because the number has to survive
// being written down.
func importAndReload(t *testing.T, source string) *config.Config {
	t.Helper()
	imported, warnings, err := config.ImportProxyConfig([]byte(source))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	out, err := yaml.Marshal(imported)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cfg, err := config.LoadBytes(out)
	if err != nil {
		t.Fatalf("the generated configuration must load:\n%v\ngenerated:\n%s\nwarnings: %v",
			err, out, warnings)
	}
	return cfg
}

// TestLintAndTheEngineAgreeOnADecimal asserts the agreement in both
// directions: every rate `dorangctl config lint` accepts, the price catalog
// assembles, and every rate it refuses, the catalog refuses too.
//
// They disagreed. internal/config's checkDecimal accepted exponent notation and
// any number of fractional digits; internal/pricing's parser accepts neither.
// A rate card carrying `1.25e-7` therefore linted `ok` and the gateway then
// refused to start — the `rates.images` defect one field over, and again with
// the validator as the more permissive of the two.
//
// The test is written against the ASSEMBLER rather than against a second copy
// of the rules, because two implementations of the same predicate agreeing in a
// table proves only that someone wrote the table twice.
func TestLintAndTheEngineAgreeOnADecimal(t *testing.T) {
	for _, rate := range []string{
		// Accepted by both.
		"2.50", "0", "10", "0.25", "0.000000000001", "1", "0.02",
		// Refused by both: exponent notation, more precision than the engine
		// carries, and text that is not a number at all.
		"1.25e-7", "2E9", "1e-6", "0.0000000000001", "18446744073709551616",
		"abc", "", "1,5", "5%",
	} {
		t.Run("rate="+rate, func(t *testing.T) {
			lintErr := lintRate(t, rate)
			engineErr := assembleRate(t, rate)
			switch {
			case lintErr == nil && engineErr != nil:
				t.Errorf("`config lint` accepts rates.input %q and the gateway then refuses "+
					"to assemble it: %v\nA validator more permissive than its consumer is a "+
					"load error delivered after the deploy.", rate, engineErr)
			case lintErr != nil && engineErr == nil:
				t.Errorf("`config lint` refuses rates.input %q and the engine accepts it: %v\n"+
					"A validator stricter than its consumer refuses a configuration that would "+
					"have worked.", rate, lintErr)
			}
		})
	}
}

// lintRate answers what `dorangctl config lint` would: does the file load.
func lintRate(t *testing.T, rate string) error {
	t.Helper()
	src := "version: 1\npricing:\n  currency: USD\n  rules:\n" +
		"    - {id: r1, class: marginal_usage, rates: {input: \"" + rate + "\"}}\n"
	_, err := config.LoadBytes([]byte(src))
	return err
}

// assembleRate answers what the gateway would: does the rate become a catalog.
// The configuration is built in code rather than parsed, so that the value
// reaches the assembler even when the loader would have refused it — which is
// the only way to ask the second question independently of the first.
func assembleRate(t *testing.T, rate string) error {
	t.Helper()
	cfg := &config.Config{
		Version: config.Version,
		Pricing: config.Pricing{
			Currency: "USD",
			Rules: []config.PricingRule{{
				ID:    "r1",
				Class: config.PricingMarginalUsage,
				Rates: map[string]config.Decimal{"input": config.Decimal(rate)},
			}},
		},
	}
	_, err := app.Pricing(cfg)
	return err
}

// TestConfigLintWarnsAboutAPerTokenRateAndStillSaysOk puts the advisory where
// an operator meets it. `config lint` is what runs before a deploy and what
// answered `ok` while every request priced to zero, so the warning has to come
// out of THIS command — an advisory nothing prints is a comment.
//
// The exit code stays 0 on purpose. A rate that is wrong is a wrong invoice; a
// lint that fails a pipeline over one is a wrong invoice plus an outage.
func TestConfigLintWarnsAboutAPerTokenRateAndStillSaysOk(t *testing.T) {
	dir := t.TempDir()
	perToken := strings.Replace(pricedModel,
		`rates: {input: "3.00", output: "15.00"}`,
		`rates: {input: "0.000003", output: "0.000015"}`, 1)
	path := writeCfg(t, dir, perToken)

	out, errOut, code := invoke("config", "lint", path)
	if code != 0 {
		t.Fatalf("lint exited %d on a legal configuration\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("lint no longer says ok: %s", out)
	}
	if !strings.Contains(errOut, "per MILLION tokens") {
		t.Errorf("lint said ok and nothing else about a rate a millionth of its card:\n%s", errOut)
	}

	// And it is silent on the same card written the way a vendor prints it.
	quiet := writeCfg(t, t.TempDir(), pricedModel)
	_, errOut, code = invoke("config", "lint", quiet)
	if code != 0 {
		t.Fatalf("lint exited %d on the priced fixture: %s", code, errOut)
	}
	if strings.Contains(errOut, "per MILLION tokens") {
		t.Errorf("lint warned about a real rate card:\n%s", errOut)
	}
}

// TestImportConfigReportsTheConversion checks that the command an operator
// actually runs says the numbers changed. A converted rate is not a decision
// they have to make, but it is the one figure in the migration that nothing
// else can verify for them, so it is said once and it names the check.
func TestImportConfigReportsTheConversion(t *testing.T) {
	path := writeSource(t, theIncumbentsCard)
	stdout, stderr, code := invoke("import", "config", path)
	if code != 0 {
		t.Fatalf("import config exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "CONVERTED to dorang's quantities") {
		t.Errorf("the import did not report the conversion:\n%s", stderr)
	}
	if !strings.Contains(stderr, "spend/calculate") {
		t.Errorf("the import did not name the check:\n%s", stderr)
	}
	if !strings.Contains(stdout, "input: \"2.5\"") {
		t.Errorf("the generated configuration does not carry the converted rate:\n%s", stdout)
	}
}
