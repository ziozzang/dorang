package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// A monthly budget does not start over when the process does.
//
// This is DESIGN risk W9's last line. internal/cluster closed the mechanism —
// durable leased blocks, one store write per block rather than per request, and
// a crash that can only under-spend — but the request path had no budget on it
// at all: the only spend check was auth's comparison against a spend column
// nothing on this path ever incremented. A budget that is never decremented is
// not enforced, and one held only in memory silently resets, which §9.6 calls
// "safe for concurrency, wrong for accounting".
//
// The test spends a credential's budget to exhaustion, restarts the gateway over
// the same database, and requires the very next request to be refused. It also
// pins the terms of the refusal: §6.4 makes an exhausted budget a terminal 400,
// never a 429, because a 429 is a rate-limit signal that would send the request
// down the fallback chain to spend a different subject's budget.
func TestBudgetSpendSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "round-trip-pepper")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamAnswer)
	}))
	defer up.Close()

	cfg := loadRoundTripConfig(t, dir, up.URL)
	ctx := context.Background()

	// 11 input tokens at $3.00/1M plus 3 output at $15.00/1M is 78 000 nano-USD
	// per request, fixed by the fake upstream's usage block. The ceiling is a
	// few of those, and the lease block is smaller than the ceiling so the test
	// exercises a refill rather than a single draw.
	const (
		costPerRequest = 11*3_000 + 3*15_000
		limitNano      = 500_000
		blockNano      = 200_000
	)
	opts := app.Options{Config: cfg, Logf: t.Logf, BudgetBlockNanoUSD: blockNano}

	a, err := app.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}

	const token = "sk-budget-restart-token" // pragma: allowlist secret — test fixture
	limit := int64(limitNano)
	k := &store.APIKey{KeyAlias: "budgeted", MaxBudgetNano: &limit, BudgetPeriod: "monthly"}
	if err := a.Store.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.InsertAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}

	// max_tokens is named so the reservation is a genuine upper bound: §6.4
	// prices output at max_tokens, and a request that names none has no ceiling
	// to price against.
	const body = `{"model":"model-x","max_tokens":4,"messages":[{"role":"user","content":"ping"}]}`

	front := httptest.NewServer(a.Server)
	spend := func(t *testing.T, srv *httptest.Server) response {
		t.Helper()
		resp, err := postErr(srv.URL+"/v1/chat/completions", token, body, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Spend until the budget refuses. The number of requests that fit is left to
	// the arithmetic rather than asserted, because the lease block size is a
	// tuning knob and this test is not about its value.
	served := 0
	var refusal response
	for i := 0; i < 50; i++ {
		resp := spend(t, front)
		if resp.status == http.StatusOK {
			served++
			continue
		}
		refusal = resp
		break
	}
	front.Close()

	if served == 0 {
		t.Fatal("the first request was refused; the ceiling is too small to prove anything")
	}
	if refusal.status == 0 {
		t.Fatalf("%d requests all succeeded against a %d nano-USD budget; the request "+
			"path is not consulting a budget at all", served, limitNano)
	}
	if refusal.status != http.StatusBadRequest {
		t.Errorf("an exhausted budget answered %d, want 400 (§6.4: a 429 would send the "+
			"request down the fallback chain)\n%s", refusal.status, refusal.body)
	}
	if !strings.Contains(refusal.body, "budget") {
		t.Errorf("the refusal does not say it is about a budget:\n%s", refusal.body)
	}

	key := cluster.BudgetKey("key", k.ID, quota.Monthly, time.Now())
	committed, err := a.Ledger.Committed(ctx, key)
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if committed <= 0 {
		t.Fatalf("the durable counter holds %d after %d requests", committed, served)
	}

	// A graceful stop returns every unspent unit, so the counter ends up holding
	// precisely what was spent (§9.6). That is what makes a planned restart cost
	// nothing at all — and it is why the figure below is exact rather than a
	// bound.
	if err := a.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The restart: a second gateway over the same database, sharing nothing in
	// memory with the first.
	b, err := app.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)

	after, err := b.Ledger.Committed(ctx, key)
	if err != nil {
		t.Fatalf("Committed after restart: %v", err)
	}
	if want := int64(served) * costPerRequest; after != want {
		t.Errorf("after the restart the budget records %d nano-USD spent, want %d "+
			"(%d requests at %d each)", after, want, served, costPerRequest)
	}

	front2 := httptest.NewServer(b.Server)
	defer front2.Close()
	resp := spend(t, front2)
	if resp.status == http.StatusOK {
		t.Fatalf("the first request after a restart was served; the monthly budget "+
			"started over (%d nano-USD of a %d ceiling was already spent)", after, limitNano)
	}
	if resp.status != http.StatusBadRequest {
		t.Errorf("after the restart an exhausted budget answered %d, want 400\n%s",
			resp.status, resp.body)
	}
}

// A credential with no configured ceiling is not gated, and a budget with room
// left does not refuse. Both directions matter: a gate that refuses everything
// is as broken as one that refuses nothing, and only one of the two is noticed
// in testing.
func TestUnbudgetedAndUnderBudgetRequestsAreServed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "round-trip-pepper")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamAnswer)
	}))
	defer up.Close()

	cfg := loadRoundTripConfig(t, dir, up.URL)
	ctx := context.Background()
	a, err := app.New(ctx, app.Options{Config: cfg, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)

	unbudgeted := issueKey(t, ctx, a.Store)

	const generous = int64(1_000_000_000) // 1 USD
	limit := generous
	rich := &store.APIKey{KeyAlias: "generous", MaxBudgetNano: &limit, BudgetPeriod: "monthly"}
	const richToken = "sk-generous-budget-token" // pragma: allowlist secret — test fixture
	if err := a.Store.NewAPIKeyFromToken(richToken, rich); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.InsertAPIKey(ctx, rich); err != nil {
		t.Fatal(err)
	}

	front := httptest.NewServer(a.Server)
	defer front.Close()

	for _, tc := range []struct{ name, token string }{
		{"no budget configured", unbudgeted},
		{"budget with room", richToken},
	} {
		for i := 0; i < 3; i++ {
			resp, err := postErr(front.URL+"/v1/chat/completions", tc.token, chatRequest, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if resp.status != http.StatusOK {
				t.Fatalf("%s: request %d answered %d, want 200\n%s",
					tc.name, i, resp.status, resp.body)
			}
		}
	}
}
