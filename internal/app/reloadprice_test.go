package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/pricing"
)

// subscriptionYAML is a gateway on a flat plan: the shape where a reload is a
// billing event if the catalog swap drops the period's accumulator.
const subscriptionYAML = `
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
pricing:
  currency: USD
  rules:
    - {id: tokens, class: marginal_usage, match: {provider: p1}, rates: {input: "1.00"}}
    - id: plan
      class: fixed_subscription
      match: {credential: c1}
      period: monthly
      amount: "100.00"
`

// TestReloadDoesNotRestartSubscriptionAttribution is the wiring half of the
// reload residual: internal/pricing can carry a period's attributed total across
// a catalog swap, and this asserts that App.Reload actually asks it to.
//
// The assertion is time-independent by construction. Two settlements at the SAME
// instant, with a reload between them, must attribute a share and then nothing:
// no plan cost has accrued in between, and the second row is an increment of a
// running total, not a fresh estimate of it. A reload that hands the new catalog
// an empty accumulator makes the second row equal to the first — the whole of
// the period so far, attributed twice.
//
// Measured before the carry-over existed: twenty reloads attributed 1,030 USD of
// a 100 USD plan, and one reload late in a period put 91.00 USD on one request,
// which budget.reserve then holds against that request's own budget. A SIGHUP
// against an unchanged file did it, because a SIGHUP re-applies unconditionally
// on purpose — it is how an operator picks up an edited external price catalog
// or a rotated key_file secret.
func TestReloadDoesNotRestartSubscriptionAttribution(t *testing.T) {
	isolateState(t)
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	cfg, err := config.LoadBytes([]byte(subscriptionYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Storage.SQLite.Path = filepath.Join(t.TempDir(), "dorang.db")
	t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
	t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)

	ctx := context.Background()
	a, err := New(ctx, Options{Config: cfg})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })

	// An instant safely inside the open period and safely behind the clock, so
	// neither the backfill guard nor the future clamp is what is being measured.
	when := a.now().Add(-time.Hour)
	req := pricing.Request{Provider: "p1", Model: "m1-upstream", Credential: "c1",
		InputTokens: 1_000_000, Requests: 1, At: when}

	first, err := a.dispatch.state().pricing.Settle(req)
	if err != nil {
		t.Fatalf("first settlement: %v", err)
	}
	if first.SubscriptionNano <= 0 {
		t.Fatalf("the first settlement attributed %d of the plan; the fixture is not on a "+
			"subscription and proves nothing", first.SubscriptionNano)
	}

	before := a.dispatch.state().pricing
	if err := a.Reload(a.Config()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	after := a.dispatch.state().pricing
	if after == before {
		t.Fatal("the reload did not rebuild the price catalog, so it cannot show whether " +
			"the accumulator survives being rebuilt")
	}

	second, err := after.Settle(req)
	if err != nil {
		t.Fatalf("settlement after reload: %v", err)
	}
	if second.SubscriptionNano != 0 {
		t.Fatalf("a settlement at the same instant attributed a further %d nano after a "+
			"reload (the first attributed %d): the period's attributed total did not "+
			"survive the catalog swap, so the open period attributes the whole of itself "+
			"again — once per reload, and a SIGHUP against an unchanged file is a reload",
			second.SubscriptionNano, first.SubscriptionNano)
	}
	// The marginal figure is not state and must be unchanged by any of this.
	if second.MarginalNano != first.MarginalNano {
		t.Errorf("marginal cost changed across a reload: %d then %d",
			first.MarginalNano, second.MarginalNano)
	}
}
