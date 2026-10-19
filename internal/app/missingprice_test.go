package app

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
)

func TestMissingPriceWarningDoesNotFloodOrSkipAccounting(t *testing.T) {
	const catalog = `
currency: USD
rules:
  - id: plan
    class: fixed_subscription
    match: { credential: cred-1 }
    amount_per_period: "100.00"
    period: monthly
`
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	h := newSettleHarness(t, catalog, now)
	st := &dispatchState{pricing: mustPriceCatalog(t, catalog), quota: &quotaSet{meters: router.Meters{"cred-1": h.meter}}}
	var warnings atomic.Int64
	d := &dispatcher{now: func() time.Time { return now }, logf: func(format string, args ...any) {
		if strings.Contains(format, "no marginal price rule matched") {
			warnings.Add(1)
		}
	}}
	dec := &router.Decision{Provider: "p", UpstreamModel: "m", Credential: "cred-1"}
	var total atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rq := &server.Request{}
			d.settle(st, &call{}, dec, rq, result{usage: canonical.Usage{InputTokens: 100, OutputTokens: 10}})
			total.Add(rq.Result.CostNanoUSD)
			if rq.Result.Tokens.Input != 100 || rq.Result.Tokens.Output != 10 {
				t.Error("suppressing a warning lost token accounting")
			}
		}()
	}
	wg.Wait()
	if warnings.Load() != 1 {
		t.Fatalf("warnings = %d, want 1 across concurrent requests", warnings.Load())
	}
	if total.Load() != 32_258_064_516 || h.charged() != total.Load() {
		t.Fatalf("ledger = %d, quota = %d; subscription accounting changed", total.Load(), h.charged())
	}
	other := *dec
	other.UpstreamModel = "another-model"
	d.settle(st, &call{}, &other, &server.Request{}, result{})
	if warnings.Load() != 2 {
		t.Fatal("another model must retain its diagnostic")
	}
	// Reload creates a fresh generation: a previously diagnosed problem must
	// become visible again after an operator changes pricing configuration.
	reloaded := &dispatchState{pricing: mustPriceCatalog(t, catalog)}
	d.settle(reloaded, &call{}, dec, &server.Request{}, result{})
	if warnings.Load() != 3 {
		t.Fatal("configuration reload must re-enable the diagnostic")
	}
}
