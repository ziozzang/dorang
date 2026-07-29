package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/store"
)

// planClock is a fixed instant nine tenths of the way through a month, which is
// where the live measurement of the restart defect was taken: at that point a
// 100.00 USD monthly plan has accrued about 90 USD, and a process that starts
// with an empty accumulator attributes all of it to whichever request arrives
// first.
//
// Fixed rather than derived from the wall clock. On the first minute of a month
// the elapsed share is a fraction of a cent, and a test whose subject is
// "90 USD landed on one request" must not depend on the day the suite runs.
var planClock = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// planYAML is a gateway on a flat plan with a real upstream behind it: the shape
// where a restart is a billing event if the accumulator is not written down.
func planYAML(upstreamURL string) string {
	return fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
providers:
  - {name: p1, kind: openai, base_url: %q}
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
`, upstreamURL)
}

// planUpstream answers one fixed chat completion with a usage object.
func planUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m1-upstream",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},`+
			`"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`)
	}))
	t.Cleanup(up.Close)
	return up
}

// restartableApp builds an App against a FIXED store path and a fixed clock, so
// a test can close one process and start another over the same durable state.
// That is what "survives a restart" means in a single test binary, and it is the
// boundary [newWiringApp]'s per-test TempDir deliberately hides.
func restartableApp(t *testing.T, yaml, dbPath string, up *httptest.Server) *App {
	t.Helper()
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Storage.SQLite.Path = dbPath
	t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
	t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)

	a, err := New(context.Background(), Options{
		Config:   cfg,
		Upstream: up.Client(),
		Now:      func() time.Time { return planClock },
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	return a
}

const planChat = `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`

// TestARestartDoesNotRefuseAFreshKeyOnItsFirstRequest is the client-visible half
// of the restart defect, asserted where the client sees it: an HTTP status.
//
// The accumulator that bounds a subscription period's attribution lived in the
// loaded price catalog and nowhere else. A reload carried it across
// (TestReloadDoesNotRestartSubscriptionAttribution); a process restart did not,
// so the open period started attributing from zero again and the whole elapsed
// share — 90.23 USD of a 100.00 USD plan, measured — landed on the first request
// after the restart. internal/app reserves the settled cost against the
// REQUESTING key's budget, so:
//
//	fresh process, brand-new key, max_budget: 1.00, never used
//	POST /v1/chat/completions  ->  400 {"code":"budget_exceeded", …}
//
// A key that has never spent anything, refused on its first ever request,
// because a plan share it has no relationship to was booked against it.
//
// Both arms run. The control arm empties `subscription_state` between the two
// processes, which is exactly the state of the world before the table existed,
// and it must still produce the 400 — otherwise this test cannot tell the two
// apart and proves nothing.
func TestARestartDoesNotRefuseAFreshKeyOnItsFirstRequest(t *testing.T) {
	up := planUpstream(t)
	yaml := planYAML(up.URL)

	run := func(t *testing.T, wipe bool) int {
		dbPath := filepath.Join(t.TempDir(), "dorang.db")

		// The first process serves one request, which attributes the period's
		// elapsed share to it. Its key carries no ceiling, so nothing refuses it.
		first := restartableApp(t, yaml, dbPath, up)
		unbudgeted := issueKey(t, first, nil)
		if w := callWith(first, unbudgeted, http.MethodPost, "/v1/chat/completions", planChat); w.Code != http.StatusOK {
			t.Fatalf("the first process's request answered %d: %s", w.Code, w.Body.String())
		}
		if err := first.Close(context.Background()); err != nil {
			t.Fatalf("closing the first process: %v", err)
		}

		if wipe {
			// The world before the accumulator was written down.
			second := openTestStoreAt(t, dbPath, nil)
			if _, err := second.DB().Exec(`DELETE FROM subscription_state`); err != nil {
				t.Fatalf("clearing the accumulator: %v", err)
			}
		}

		// The restart, and a key that has never sent anything.
		next := restartableApp(t, yaml, dbPath, up)
		t.Cleanup(func() { _ = next.Close(context.Background()) })
		fresh := issueKey(t, next, func(k *store.APIKey) {
			one := int64(1_000_000_000) // 1.00 USD
			k.MaxBudgetNano = &one
			k.BudgetPeriod = "monthly"
		})
		w := callWith(next, fresh, http.MethodPost, "/v1/chat/completions", planChat)
		if w.Code == http.StatusBadRequest && !strings.Contains(w.Body.String(), "budget_exceeded") {
			t.Fatalf("a 400 that is not a budget refusal: %s", w.Body.String())
		}
		return w.Code
	}

	t.Run("control: nothing carried across the restart", func(t *testing.T) {
		if got := run(t, true); got != http.StatusBadRequest {
			t.Fatalf("a brand-new key answered %d after a restart that carried nothing: "+
				"the re-attributed plan share no longer reaches a budget, so this test "+
				"can no longer tell the two arms apart", got)
		}
	})
	t.Run("the accumulator crosses the process boundary", func(t *testing.T) {
		if got := run(t, false); got != http.StatusOK {
			t.Fatalf("a brand-new key with a 1.00 USD ceiling answered %d on its first "+
				"ever request: the restart re-attributed the period's elapsed share and "+
				"internal/app reserved it against that key's budget", got)
		}
	})
}

// TestAPlanShareIsRecordedAsSubscriptionSpend asserts the value that reaches the
// client, not the field it came from.
//
// `subscription_spend` had a column, a reader in /spend/logs and no producer.
// internal/app wrote `MarginalCostNano: t.CostNano` — the WHOLE cost — because
// meter.Trace carried one number and pricing.Cost's three-way split was dropped
// at that boundary, so a flat plan's share was filed under `marginal_spend` on
// every row. DESIGN §8.1: "The two are separate fields, never conflated."
//
// Pre-existing since `cb898a1` and unobservable until a configuration declared a
// plan, which no configuration in the parity suite did.
func TestAPlanShareIsRecordedAsSubscriptionSpend(t *testing.T) {
	up := planUpstream(t)
	a := restartableApp(t, planYAML(up.URL), filepath.Join(t.TempDir(), "dorang.db"), up)
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	secret := issueKey(t, a, nil)
	if w := callWith(a, secret, http.MethodPost, "/v1/chat/completions", planChat); w.Code != http.StatusOK {
		t.Fatalf("request: %d %s", w.Code, w.Body.String())
	}
	flushLedger(t, a, 1)

	rows := spendLogs(t, a, "key-"+secret)
	if len(rows) != 1 {
		t.Fatalf("/spend/logs returned %d rows, want 1", len(rows))
	}
	r := rows[0]
	spend := money(t, r, "spend")
	marginal := money(t, r, "marginal_spend")
	subscription := money(t, r, "subscription_spend")

	if subscription <= 0 {
		t.Fatalf("/spend/logs reports subscription_spend=%v for a request on a "+
			"fixed_subscription plan whose total spend is %v: the column has a reader and "+
			"no producer, and the plan share was filed under marginal_spend=%v",
			subscription, spend, marginal)
	}
	if marginal >= subscription {
		t.Errorf("marginal_spend=%v is not smaller than the plan share %v; a 110-token "+
			"request at 1.00 USD per million tokens costs a fraction of a cent, so a "+
			"marginal figure this large is still carrying the plan", marginal, subscription)
	}
	// The decomposition must reconcile with the total the same row reports:
	// there is no adjustment rule in this fixture, so the two classes are all of
	// it. A split that does not add up is a second wrong answer, not a fix.
	//
	// The tolerance is one nano — the surface renders these as decimal currency,
	// and the assertion is about the accounting rather than about float64.
	if got := marginal + subscription - spend; got > 1e-9 || got < -1e-9 {
		t.Errorf("marginal_spend (%v) + subscription_spend (%v) = %v, but spend = %v",
			marginal, subscription, marginal+subscription, spend)
	}

	// And the aggregate route agrees with the row it aggregates. Both reported
	// the plan share as `marginal_spend`, and a report route that still did
	// would be the same defect one materialization up — the one an operator
	// compares models by.
	report := adminJSON(t, a, "/global/spend/report?group_by=key&"+planWindow())
	total, ok := report["total"].(map[string]any)
	if !ok {
		t.Fatalf("the report has no total: %v", report)
	}
	for _, f := range []string{"spend", "marginal_spend", "subscription_spend"} {
		if got, want := money(t, total, f), money(t, r, f); got != want {
			t.Errorf("/global/spend/report reports %s=%v where the single ledger row it "+
				"aggregates reports %v: the rollups carry one cost column and the report "+
				"answers the decomposition from the total", f, got, want)
		}
	}
}

// TestKeyInfoReportsTheSpendTheLedgerRecorded is defect N3, asserted against the
// number a client can reach by another route.
//
// `/key/info` rendered `spend` from `api_keys.spend_nano`, which is written by
// the importer and by nothing on the request path — budget enforcement lives in
// the durable counter, which is why enforcement worked and this column did not.
// The result was a **silent zero on the endpoint a per-key spend dashboard is
// most likely to read**, while the same key's ledger rows, its own response
// header and /global/spend/report all agreed on a different number.
//
// The assertion is that the two routes agree. Asserting a literal would pass
// against a re-point to any other constant.
func TestKeyInfoReportsTheSpendTheLedgerRecorded(t *testing.T) {
	up := planUpstream(t)
	a := restartableApp(t, planYAML(up.URL), filepath.Join(t.TempDir(), "dorang.db"), up)
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	secret := issueKey(t, a, func(k *store.APIKey) { k.BudgetPeriod = "monthly" })
	const requests = 3
	for i := 0; i < requests; i++ {
		if w := callWith(a, secret, http.MethodPost, "/v1/chat/completions", planChat); w.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	flushLedger(t, a, requests)

	// What the ledger says, through the route that already reported it.
	report := adminJSON(t, a, "/global/spend/report?group_by=key&"+planWindow())
	total, ok := report["total"].(map[string]any)
	if !ok {
		t.Fatalf("the report has no total: %v", report)
	}
	want := money(t, total, "spend")
	if want <= 0 {
		t.Fatalf("the ledger reports %v spend for %d priced requests; the fixture proves "+
			"nothing", want, requests)
	}

	info := adminJSON(t, a, "/key/info?key_id=key-"+secret)
	key, ok := info["key"].(map[string]any)
	if !ok {
		t.Fatalf("/key/info has no key object: %v", info)
	}
	if got := money(t, key, "spend"); got != want {
		t.Fatalf("/key/info reports spend=%v where the ledger and "+
			"/global/spend/report both report %v: the field is rendered from "+
			"api_keys.spend_nano, which nothing on the request path writes",
			got, want)
	}
}

// zeroRatedYAML prices m1 at nothing and m2 at something. Both are PRICED — a
// rule matched — which is the case the header contract is about: "unpriced" is
// already reported by omitting the cost header entirely.
func zeroRatedYAML(upstreamURL string) string {
	return fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
providers:
  - {name: p1, kind: openai, base_url: %q}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
  - name: m2
    deployments:
      - {provider: p1, upstream_model: m2-upstream, credentials: [c1]}
pricing:
  currency: USD
  rules:
    - {id: free, class: marginal_usage, match: {model: m1-upstream}, rates: {input: "0"}}
    - {id: paid, class: marginal_usage, match: {provider: p1}, rates: {input: "1000.00"}}
`, upstreamURL)
}

// TestTheFirstZeroCostRequestDoesNotClaimAZeroSpend is defect N4, and the
// question it answers is the one the streamed cost header already answered:
// **is "not yet loaded" allowed to read as zero on the wire?** It is not.
//
// The budget hold is hydrated from the durable counter by the RESERVATION. A
// request whose estimated cost is zero takes no reservation, so nothing draws a
// block and Consumed answers from an empty one. Measured immediately after a
// process start, on a key whose true spend was 0.001889198 of a 0.01 ceiling:
//
//	x-dorang-spend-usd: 0        x-dorang-budget-remaining-usd: 0.01
//
// Three requests, because two of them are the discriminator: a header that is
// simply never emitted would pass an assertion about the first one alone.
func TestTheFirstZeroCostRequestDoesNotClaimAZeroSpend(t *testing.T) {
	up := planUpstream(t)
	a := restartableApp(t, zeroRatedYAML(up.URL), filepath.Join(t.TempDir(), "dorang.db"), up)
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	secret := issueKey(t, a, func(k *store.APIKey) {
		ten := int64(10_000_000_000) // 10.00 USD
		k.MaxBudgetNano = &ten
		k.BudgetPeriod = "monthly"
	})
	post := func(model string) http.Header {
		t.Helper()
		w := callWith(a, secret, http.MethodPost, "/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", model, w.Code, w.Body.String())
		}
		return w.Header()
	}

	// 1. The first zero-rated request of the process. The ceiling is known — it
	//    comes from the authorization snapshot — and the spend is not.
	h := post("m1")
	if h.Get(server.HeaderCostUSD) == "" {
		t.Fatal("the zero-rated request was not priced at all, so this test is about a " +
			"different case than the one it names")
	}
	if got := h.Get(server.HeaderSpendUSD); got != "" {
		t.Errorf("%s = %q on the first zero-cost request of a process: no reservation was "+
			"taken, so nothing hydrated the budget hold and the figure behind this header "+
			"is an empty block. \"Not loaded\" is not \"zero\"",
			server.HeaderSpendUSD, got)
	}
	if got := h.Get(server.HeaderBudgetRemainingUSD); got != "" {
		t.Errorf("%s = %q, derived from a spend nobody looked up",
			server.HeaderBudgetRemainingUSD, got)
	}
	if h.Get(server.HeaderBudgetUSD) == "" {
		t.Errorf("%s is absent: the CEILING is known — it is on the authorization "+
			"snapshot — and withholding it makes the fix a different defect",
			server.HeaderBudgetUSD)
	}

	// 2. A priced request reserves, which hydrates the hold.
	if got := post("m2").Get(server.HeaderSpendUSD); got == "" {
		t.Fatalf("%s is absent on a request that took a reservation: the header is now "+
			"never emitted, which is not a fix", server.HeaderSpendUSD)
	}

	// 3. And a zero-rated request AFTER that reports the real figure, because by
	//    then there is one. This is the row the measurement got right.
	if got := post("m1").Get(server.HeaderSpendUSD); got == "" {
		t.Errorf("%s is absent on a zero-rated request against an already-hydrated hold: "+
			"the value is known and is being withheld", server.HeaderSpendUSD)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// flushLedger pushes the meter's buffers into the store and waits for the rows.
func flushLedger(t *testing.T, a *App, want int) {
	t.Helper()
	if err := a.Meter.Flush(context.Background()); err != nil {
		t.Fatalf("meter flush: %v", err)
	}
	waitFor(t, "the traces to reach the ledger", func() bool {
		return a.Meter.Stats().TracesFlushed >= int64(want)
	})
}

// planWindow is a bounded range around the fixed clock, in the form §9.3 requires.
func planWindow() string {
	return "start_date=" + planClock.Add(-time.Hour).Format(time.RFC3339) +
		"&end_date=" + planClock.Add(time.Hour).Format(time.RFC3339)
}

// adminJSON calls an administration route with the master credential.
func adminJSON(t *testing.T, a *App, path string) map[string]any {
	t.Helper()
	w := callWith(a, testMasterKey, http.MethodGet, path, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s: %v (%s)", path, err, w.Body.String())
	}
	return out
}

// spendLogs returns the ledger rows one key produced, as the client sees them.
func spendLogs(t *testing.T, a *App, keyID string) []map[string]any {
	t.Helper()
	body := adminJSON(t, a, "/spend/logs?api_key="+keyID+"&"+planWindow())
	raw, _ := body["logs"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// money reads one of the surface's amount fields. They are rendered as JSON
// numbers in the catalog currency, so the comparison is on the wire value and
// not on an internal nano count.
func money(t *testing.T, m map[string]any, field string) float64 {
	t.Helper()
	v, ok := m[field]
	if !ok {
		t.Fatalf("no %q in %v", field, m)
	}
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil {
			t.Fatalf("%q = %q: %v", field, n, err)
		}
		return f
	case nil:
		t.Fatalf("%q is null", field)
	}
	t.Fatalf("%q is %T, not a number", field, v)
	return 0
}
