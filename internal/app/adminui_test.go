package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/store"
)

// The UI toggle edits the real config file through the writer and the change
// takes effect: the file records the flag and the model stops routing. This
// exercises the whole path — endpoint, ConfigWriter, the locked in-place edit,
// and the reload — against a real file, which the unit tests with a fake
// ConfigWriter cannot.
//
// Revert check: make writeConfig/EditLocked a no-op (or unwire ConfigWriter)
// and the file is unchanged and the model still serves — both assertions fail.
func TestToggleDeploymentEditsConfigAndReroutes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(adminUIYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newWiringApp(t, adminUIYAML, nil)
	// Stand in for the start-up wiring main.go does after building the watcher.
	a.SetConfigControl(path, func() error {
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		return a.Reload(cfg)
	})

	body := `{"model_group":"chat","provider":"p1","upstream_model":"vendor/chat-2026","enabled":false}`
	w := callWith(a, testMasterKey, http.MethodPost, "/model/deployment/set_enabled", body)
	if w.Code != http.StatusOK {
		t.Fatalf("set_enabled = %d: %s", w.Code, w.Body.String())
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "enabled: false") {
		t.Errorf("the config file was not edited to disable the deployment:\n%s", after)
	}
	// The reload applied: chat's only deployment is disabled, so it no longer
	// serves.
	cw := callWith(a, testMasterKey, http.MethodPost, "/v1/chat/completions",
		`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`)
	if cw.Code == http.StatusOK {
		t.Errorf("chat still served after its only deployment was disabled: %s", cw.Body.String())
	}
}

// A deployment marked enabled:false is defined but not routed: buildRouter
// skips it, so it is absent from the compiled routing the models screen shows,
// while its enabled sibling remains. This is the routing half of the
// enable/disable control.
//
// Revert check: drop the Enabled skip in buildRouter and the disabled
// deployment is routed and listed — the first assertion fails.
func TestDisabledDeploymentIsNotRouted(t *testing.T) {
	disabled := false
	a := newWiringApp(t, adminUIYAML, func(cfg *config.Config) {
		for i := range cfg.Models {
			if cfg.Models[i].Name == "chat" {
				for j := range cfg.Models[i].Deployments {
					cfg.Models[i].Deployments[j].Enabled = &disabled
				}
			}
		}
	})
	// The model's only deployment is disabled, so the group is empty and a
	// request to it is refused as unroutable rather than served.
	body := `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`
	w := callWith(a, testMasterKey, http.MethodPost, "/v1/chat/completions", body)
	if w.Code == http.StatusOK {
		t.Fatalf("a model whose only deployment is disabled served a request:\n%s", w.Body.String())
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "deployment") {
		t.Errorf("expected a no-deployment routing refusal, got %d: %s", w.Code, w.Body.String())
	}
	// The disabled deployment is still shown on the models screen — marked
	// disabled, not hidden — so an operator can see and re-enable it.
	m := callWith(a, testMasterKey, http.MethodGet, "/ui/models", "")
	if !strings.Contains(m.Body.String(), "vendor/chat-2026") {
		t.Errorf("the disabled deployment vanished from the models screen instead of showing as disabled")
	}
}

// The operator UI, against an assembled gateway.
//
// Every assertion here reads the RENDERED PAGE. That is the lesson of the
// defects this file covers: all three screens answered 200 throughout — one
// printed 0 in a spend column the ledger disagreed with, one advertised a link
// that answered 501 on every click, and one carried two tiles that could not
// hold a value in any deployment. A status code was never the thing that was
// wrong.

var (
	reUICell = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	reUITag  = regexp.MustCompile(`(?s)<[^>]*>`)
	reUIWS   = regexp.MustCompile(`\s+`)
)

// uiRow returns the cells of the rendered table row that has a cell reading
// match, as plain text.
func uiRow(t *testing.T, body, match string) []string {
	t.Helper()
	for _, row := range strings.Split(body, "<tr") {
		end := strings.Index(row, "</tr>")
		if end < 0 {
			continue
		}
		var cells []string
		for _, m := range reUICell.FindAllStringSubmatch(row[:end], -1) {
			cells = append(cells, uiText(m[1]))
		}
		for _, c := range cells {
			if c == match {
				return cells
			}
		}
	}
	t.Fatalf("no rendered row has a cell reading %q\n%s", match, body)
	return nil
}

// uiTile returns the value a named tile on the usage screen shows.
func uiTile(t *testing.T, body, label string) string {
	t.Helper()
	i := strings.Index(body, ">"+label+"<")
	if i < 0 {
		t.Fatalf("no tile labelled %q", label)
	}
	rest := body[i:]
	j := strings.Index(rest, `class="value"`)
	if j < 0 {
		t.Fatalf("tile %q has no value", label)
	}
	rest = rest[j:]
	k := strings.Index(rest, "</div>")
	if k < 0 {
		t.Fatalf("tile %q is not closed", label)
	}
	return uiText(rest[len(`class="value">`):k])
}

func uiText(s string) string {
	return strings.TrimSpace(reUIWS.ReplaceAllString(reUITag.ReplaceAllString(s, " "), " "))
}

const adminUIYAML = `
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid", timeout: 30s}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
aliases:
  chat-latest: chat
models:
  - name: chat
    deployments:
      - provider: p1
        upstream_model: vendor/chat-2026
        credentials: [c1]
        weight: 3
        priority: 1
        limits:
          - {metric: rpm, value: 600}
  - name: embed
    deployments:
      - {provider: p1, upstream_model: vendor/embed, credentials: [c1]}
`

// The models screen was advertised on all three pages and answered 501 in every
// deployment, because admin.Config.Models was never assigned and nothing else
// answered the question. dorang knows its model set — this asserts the screen
// shows it, with the SAME deployment id the ledger records.
func TestModelsScreenShowsTheCompiledRoutingTable(t *testing.T) {
	a := newWiringApp(t, adminUIYAML, nil)

	w := callWith(a, testMasterKey, http.MethodGet, "/ui/models", "")
	if w.Code != http.StatusOK {
		t.Fatalf("/ui/models = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The id is the one buildRouter compiles, which is the one stamped into
	// x-dorang-deployment and written to request_logs.deployment_id.
	id := deploymentID("chat", "p1", "vendor/chat-2026")
	cells := uiRow(t, body, id)
	// group, deployment, provider, upstream, weight, priority, rpm, tpm, parallel, enabled
	for i, want := range []string{"chat", id, "p1", "vendor/chat-2026", "3", "1", "600", "—", "—", "yes"} {
		if cells[i] != want {
			t.Errorf("deployment cell %d = %q, want %q (row %q)", i, cells[i], want, cells)
		}
	}
	// A deployment that declares no weight routes as weight 1, so the column
	// says 1 rather than describing the file's silence.
	if got := uiRow(t, body, deploymentID("embed", "p1", "vendor/embed"))[4]; got != "1" {
		t.Errorf("an unweighted deployment renders weight %q, want %q", got, "1")
	}
	if !strings.Contains(body, "chat-latest") {
		t.Error("the alias table is empty; aliases: is configured")
	}

	// The screen says which table it is showing, because "what you configured"
	// and "what is running" are different sentences.
	if !strings.Contains(body, "compiled") {
		t.Error("the screen does not say the rows are the compiled routing table")
	}

	// And the write surface still refuses, by name: being able to SEE the
	// routing table is not being able to edit it.
	w = callWith(a, testMasterKey, http.MethodPost, "/model/new",
		`{"model_name":"m","litellm_params":{"provider":"p1","model":"x"}}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("/model/new = %d, want 501 — a deployment row nothing routes on "+
			"must not be writable: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model registry") {
		t.Errorf("/model/new does not name the missing piece: %s", w.Body.String())
	}
}

// Every link the navigation advertises must answer.
//
// The links are read OFF the rendered page rather than listed here, because the
// defect was precisely a nav that advertised a screen nothing was wired behind:
// a list in the test would have been the same list as the one in the template,
// agreeing with itself. This follows what the operator's browser would follow.
func TestEveryAdvertisedScreenAnswers(t *testing.T) {
	a := newWiringApp(t, adminUIYAML, nil)
	// The nav element carries attributes (aria-label) since the app-shell
	// layout, so the open tag is matched up to its own '>' rather than assumed
	// to be bare.
	nav := regexp.MustCompile(`<nav class="screens"[^>]*>(?s)(.*?)</nav>`)
	href := regexp.MustCompile(`href="([^"]+)"`)

	seen := map[string]bool{}
	for _, p := range []string{"/ui/keys", "/ui/models", "/ui/usage"} {
		w := callWith(a, testMasterKey, http.MethodGet, p, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200: %s", p, w.Code, w.Body.String())
		}
		m := nav.FindStringSubmatch(w.Body.String())
		if m == nil {
			t.Fatalf("%s renders no navigation", p)
		}
		for _, h := range href.FindAllStringSubmatch(m[1], -1) {
			seen[h[1]] = true
		}
	}
	// The exact count is deliberately not asserted: screens are advertised as
	// their dependencies are wired (users when a directory is configured,
	// monitoring when a ledger, credential reporter or capacity broker is), so
	// pinning a number here would just be a second copy of the nav's own
	// conditions. What must hold is that the core three are always present and
	// that every advertised link answers — the loop below is the real test.
	for _, want := range []string{"/ui/keys", "/ui/models", "/ui/usage"} {
		if !seen[want] {
			t.Errorf("the nav does not advertise %s; it lists %v", want, seen)
		}
	}
	for link := range seen {
		w := callWith(a, testMasterKey, http.MethodGet, link, "")
		if w.Code != http.StatusOK {
			t.Errorf("the navigation advertises %s and it answers %d — an operator "+
				"clicking a permanent refusal learns the product is broken, not that a "+
				"dependency is optional", link, w.Code)
		}
	}
}

// The monitoring screen reads the ledger live, through the real store, within
// the page the store accepts.
//
// A 200 is not enough to know the live region works: the screen returns 200
// even when its ledger read fails, rendering "could not be read" in place of
// the traffic. The two ways that read failed in practice were both queries the
// real store refuses — an unfiltered window (no all-rows index, DESIGN §9.3)
// and a page above MaxPageSize (store.pageLimit ERRORS rather than clamps) —
// and a fake ledger answered both, so the defect reached production while the
// handler test stayed green. This runs the real adminLedger over the real
// store and asserts the banner is ABSENT, which is the consequence §17.1 asks
// for: revert the window read to either refused shape and this fails.
func TestMonitoringScreenReadsTheLedgerLive(t *testing.T) {
	a := newWiringApp(t, adminUIYAML, nil)
	w := callWith(a, testMasterKey, http.MethodGet, "/ui/monitoring", "")
	if w.Code != http.StatusOK {
		t.Fatalf("/ui/monitoring = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Recent failures") {
		t.Errorf("the monitoring screen has no failures section:\n%s", body)
	}
	if strings.Contains(body, "could not be read") {
		t.Errorf("the monitoring screen could not read the ledger live — its "+
			"window query is one the real store refuses:\n%s", body)
	}
}

// The monitoring screen shows this node's live pulse from the real server's own
// counters — the digest of what GET /metrics exposes — wired through App.
//
// A fresh process's counters are zero, which is the honest reading and not an
// error; what must hold is that the section renders and points an operator at
// the scrape endpoint. Revert check: unwire Config.Surface in buildAdmin and
// the section is gone — "Live" and the /metrics pointer both vanish and this
// fails.
func TestMonitoringShowsLivePulseFromTheServer(t *testing.T) {
	a := newWiringApp(t, adminUIYAML, nil)
	w := callWith(a, testMasterKey, http.MethodGet, "/ui/monitoring", "")
	if w.Code != http.StatusOK {
		t.Fatalf("/ui/monitoring = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"Live", "/metrics"} {
		if !strings.Contains(body, want) {
			t.Errorf("the live pulse from the server is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "could not be read") {
		t.Errorf("this node's live counters could not be read — the surface reporter "+
			"is not wired to the server:\n%s", body)
	}
}

// Two of the six tiles on the usage screen — notional and leverage — could not
// hold a value in any deployment: the rollup adapter hard-wired NotionalKnown
// to false, so §8.5's whole point (a subscription's value is visible) rendered
// as `unavailable` forever. `unavailable` is honest and a tile that can never
// be anything else is furniture.
func TestUsageScreenNotionalTilesHaveAProducer(t *testing.T) {
	a := newWiringApp(t, adminUIYAML, nil)
	ctx := context.Background()
	hour := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Hour)

	b := store.NewRollupBatch()
	k := store.ModelHourKey{Hour: hour, ModelGroup: "chat"}
	e := b.ModelHour[k]
	e.Add(store.UsageDelta{
		Requests: 4, TotalTokens: 900, CostNano: 250_000_000,
		// Four requests, four list rates: the sum is whole.
		NotionalNano: 1_000_000_000, NotionalRequests: 4,
	})
	b.ModelHour[k] = e
	if _, err := a.Store.MergeRollups(ctx, b); err != nil {
		t.Fatal(err)
	}

	w := callWith(a, testMasterKey, http.MethodGet, "/ui/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("/ui/usage = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if got := uiTile(t, body, "cost (billed)"); got != "0.25" {
		t.Errorf("the billed tile shows %q, want %q", got, "0.25")
	}
	if got := uiTile(t, body, "notional (list rate)"); got != "1" {
		t.Errorf("the notional tile shows %q; the rollup holds 1 (list rate)", got)
	}
	// notional ÷ billed, over the window. 1.0 / 0.25 = 4.00×.
	if got := uiTile(t, body, "leverage"); got != "4.00×" {
		t.Errorf("the leverage tile shows %q, want %q", got, "4.00×")
	}

	// And the same figure through the API, so the screen and the report cannot
	// disagree about the same window.
	api := callWith(a, testMasterKey, http.MethodGet,
		"/global/spend/report?start_date="+hour.Add(-24*time.Hour).Format("2006-01-02")+
			"&end_date="+hour.Add(48*time.Hour).Format("2006-01-02"), "")
	if !strings.Contains(api.Body.String(), `"notional_spend":1`) {
		t.Errorf("/global/spend/report does not carry the notional figure: %s", api.Body.String())
	}
}

// The other half of §8.5 rule 5, and the half that must never regress: one
// request that reached pricing without a list rate makes the window's notional
// sum an understatement, and an understatement presented as a total is the
// flattering answer the rule exists to forbid.
func TestUsageScreenReportsAnIncompleteNotionalAsUnavailable(t *testing.T) {
	a := newWiringApp(t, adminUIYAML, nil)
	ctx := context.Background()
	hour := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Hour)

	b := store.NewRollupBatch()
	k := store.ModelHourKey{Hour: hour, ModelGroup: "chat"}
	e := b.ModelHour[k]
	e.Add(store.UsageDelta{
		Requests: 4, TotalTokens: 900, CostNano: 250_000_000,
		NotionalNano: 750_000_000, NotionalRequests: 3, NotionalMissing: 1,
	})
	b.ModelHour[k] = e
	if _, err := a.Store.MergeRollups(ctx, b); err != nil {
		t.Fatal(err)
	}

	body := callWith(a, testMasterKey, http.MethodGet, "/ui/usage", "").Body.String()
	if got := uiTile(t, body, "notional (list rate)"); got != "unavailable" {
		t.Errorf("the notional tile shows %q for a window missing one request's "+
			"list rate; a sum short by an unknown amount must not be shown as a total", got)
	}
	if got := uiTile(t, body, "leverage"); got != "—" {
		t.Errorf("leverage = %q, want it withheld with the figure it is computed from", got)
	}
}

// The producer, end to end: a real request, priced by the real engine, metered
// through the real spool, into the columns migration 0002 created and nothing
// had ever written.
//
// This is the test that makes the notional tile a figure rather than a shape on
// a page. Everything above it renders what the store holds; this asserts the
// store comes to hold it because a request happened.
func TestANotionalPricedRequestReachesTheLedgerAndTheRollup(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","object":"chat.completion","model":"m1-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
			"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`)
	}))
	t.Cleanup(up.Close)

	yaml := fmt.Sprintf(`
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
  rules:
    - id: marginal
      class: marginal_usage
      match: {provider: p1}
      rates: {input: "1.00", output: "2.00"}
    - id: notional
      class: notional_rate
      match: {provider: p1}
      rates: {input: "3.00", output: "6.00"}
      source: "vendor list price page"
      as_of: "2026-07-28"
`, up.URL)

	a := newWiringApp(t, yaml, nil, func(o *Options) { o.Upstream = up.Client() })
	dsn := a.Config().Storage.SQLite.Path
	secret := issueKey(t, a, nil)

	w := callWith(a, secret, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("request answered %d: %s", w.Code, w.Body.String())
	}
	// The same figure the response publishes, which is where §8.5's number was
	// visible all along — and the only place it was.
	header := w.Header().Get("X-Dorang-Notional-Usd")
	if header == "" {
		t.Fatal("no notional header: the request was not priced at list rate")
	}

	// Close is the flush: the ring drains to the spool, the spool ships to the
	// sink, and the numeric half is written.
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// The catalog quotes per million tokens: 1000 input at 3.00 and 500 output
	// at 6.00 is 0.003 + 0.003 = 0.006 USD at list rate, against 0.002 billed.
	const wantNano = 6_000_000
	var (
		rowNano  int64
		rowKnown bool
	)
	if err := db.QueryRow(
		`SELECT notional_nano, notional_known FROM request_logs`).Scan(&rowNano, &rowKnown); err != nil {
		t.Fatalf("read the ledger row: %v", err)
	}
	if rowNano != wantNano || !rowKnown {
		t.Errorf("request_logs.notional = (%d, %v), want (%d, true) — the columns have had "+
			"a schema since migration 0002 and needed a writer", rowNano, rowKnown, wantNano)
	}

	var (
		sum, known, missing int64
	)
	if err := db.QueryRow(
		`SELECT notional_nano, notional_requests, notional_missing FROM usage_by_model_hour`).
		Scan(&sum, &known, &missing); err != nil {
		t.Fatalf("read the rollup: %v", err)
	}
	if sum != wantNano || known != 1 || missing != 0 {
		t.Errorf("usage_by_model_hour notional = (sum %d, priced %d, missing %d), "+
			"want (%d, 1, 0)", sum, known, missing, wantNano)
	}
	// The row and the rollup are two materializations of one fact and must not
	// disagree; a screen reading the second while an invoice is reconciled
	// against the first is how they would be discovered to.
	if sum != rowNano {
		t.Errorf("the rollup (%d) and the ledger row (%d) disagree about the same request",
			sum, rowNano)
	}
}

// The keys screen's spend column, end to end: a rollup written to a real store,
// read back through the real adapter, rendered by the real template.
func TestKeysScreenSpendComesFromTheRollup(t *testing.T) {
	a := newWiringApp(t, adminUIYAML, nil)
	ctx := context.Background()
	secret := issueKey(t, a, func(k *store.APIKey) { k.KeyAlias = "dogfood" })
	id := lookupKeyID(t, a, secret)

	// Two hours of one key's traffic, in the materialization §9.4 keeps for it.
	b := store.NewRollupBatch()
	now := time.Now().UTC()
	for _, r := range []struct {
		at   time.Time
		cost int64
	}{
		{now.Add(-90 * time.Minute), 600_000_000},
		{now.Add(-10 * time.Minute), 687_300},
	} {
		k := store.KeyHourKey{Hour: r.at.Truncate(time.Hour), APIKeyID: id}
		e := b.KeyHour[k]
		e.Add(store.UsageDelta{Requests: 1, CostNano: r.cost})
		b.KeyHour[k] = e
	}
	if _, err := a.Store.MergeRollups(ctx, b); err != nil {
		t.Fatal(err)
	}

	// api_keys.spend_nano is 0 — nothing on the request path writes it — so a
	// raw read of the column is a zero rather than a stale number.
	row, err := a.Store.GetAPIKey(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.SpendNano != 0 {
		t.Fatalf("api_keys.spend_nano = %d; the defect needs it at 0", row.SpendNano)
	}

	w := callWith(a, testMasterKey, http.MethodGet, "/ui/keys", "")
	if w.Code != http.StatusOK {
		t.Fatalf("/ui/keys = %d: %s", w.Code, w.Body.String())
	}
	// Columns: label, alias, id, user, team, models, tags, spend, budget, …
	if got := uiRow(t, w.Body.String(), id)[7]; got != "0.6006873" {
		t.Errorf("the spend column shows %q; the rollup holds 0.6006873", got)
	}

	// The API reports the same figure through the same reporter.
	api := callWith(a, testMasterKey, http.MethodGet, "/key/info?key_id="+id, "")
	if !strings.Contains(api.Body.String(), `"spend":0.6006873`) {
		t.Errorf("/key/info disagrees with the screen: %s", api.Body.String())
	}
}
