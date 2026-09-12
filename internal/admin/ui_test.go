package admin

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/ui"
)

// The embedded UI must actually parse. Parsing happens in the constructor, so a
// broken template is a startup failure rather than a 500 discovered by an
// operator mid-incident — and this test is what makes that claim true.
func TestEmbeddedTemplatesParse(t *testing.T) {
	h := newHarness(t)
	if h.api.ui == nil {
		t.Fatal("no UI server was built")
	}
	for _, name := range ui.Pages() {
		if _, ok := h.api.ui.pages[name]; !ok {
			t.Errorf("page %q was not parsed", name)
		}
	}
}

func TestEmbeddedAssetsAreSelfContained(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ path, contentType, must string }{
		{"/ui/assets/style.css", "text/css", "prefers-color-scheme"},
		{"/ui/assets/app.js", "text/javascript", "data-theme"},
	} {
		rec := h.do(http.MethodGet, tc.path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
			t.Errorf("%s: Content-Type = %q", tc.path, ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, tc.must) {
			t.Errorf("%s does not contain %q", tc.path, tc.must)
		}
		// §11.3: works offline, no external assets. An asset that reaches for
		// a URL is an external asset however it is spelled.
		for _, bad := range []string{"http://", "https://", "//cdn.", "@import"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s references %q; the UI must work offline", tc.path, bad)
			}
		}
	}
}

// Assets revalidate instead of caching blind: each carries an ETag and a
// conditional request matching it gets a 304. This is what makes a redeploy's
// new stylesheet show at once rather than after the old cache window — the
// defect that made a deploy look like it had not taken.
func TestAssetsRevalidateWithAnETag(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/ui/assets/style.css", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("style.css: status %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("style.css carries no ETag, so a browser cannot tell a redeploy from a repeat")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q; without revalidation a deploy is invisible for the cache window", cc)
	}
	// A conditional request with the current ETag is answered 304 (cheap), not
	// a full body.
	rec2 := h.do(http.MethodGet, "/ui/assets/style.css", nil, header("If-None-Match", etag))
	if rec2.Code != http.StatusNotModified {
		t.Errorf("If-None-Match with the current ETag = %d, want 304", rec2.Code)
	}
}

func TestThreeScreensRender(t *testing.T) {
	h := newHarness(t, withReporters)
	seedLedger(h)
	h.newKey(map[string]any{"key_alias": "ci", "max_budget": 10, "tags": []string{"prod"}})
	newDeployment(t, h, "chat", "prov-a", "a/model")
	h.store.aliases = []Alias{{Alias: "chat-latest", ModelGroup: "chat"}}

	for _, tc := range []struct {
		path string
		must []string
	}{
		{"/ui/keys", []string{"<title>Keys", ">ci<", "dk-", "aria-current"}},
		{"/ui/models", []string{"Models &amp; deployments", "a/model", "chat-latest", "cred-a", "prov-a"}},
		{"/ui/usage", []string{"cost (billed)", "notional (list rate)", "leverage", "By model group"}},
	} {
		rec := h.do(http.MethodGet, tc.path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d\n%s", tc.path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.HasPrefix(strings.TrimSpace(body), "<!DOCTYPE html>") {
			t.Errorf("%s did not render a document", tc.path)
		}
		for _, want := range tc.must {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not contain %q", tc.path, want)
			}
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type = %q", tc.path, ct)
		}
	}
}

// ---------------------------------------------------------------------------
// Reading the rendered page
// ---------------------------------------------------------------------------
//
// The tests below parse the HTML and compare a CELL against the source of
// truth. That is deliberate and it is the lesson of the defect they cover: the
// keys screen answered 200, rendered every row, and printed 0 in the spend
// column for every key while the ledger, /key/list and /global/spend/report all
// agreed on another figure. A test that asserted the status, or that an
// enrichment function had been called, would have passed throughout.

// The keys table's columns, in the order keys.html renders them.
const (
	colKeyLabel = iota
	colKeyAlias
	colKeyID
	colKeyUser
	colKeyTeam
	colKeyModels
	colKeyTags
	colKeySpend
	colKeyBudget
	colKeyState
	colKeyExpires
	colKeyCreated
	numKeyCols
)

var (
	reCell = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	reTag  = regexp.MustCompile(`(?s)<[^>]*>`)
	reWS   = regexp.MustCompile(`\s+`)
)

// htmlCells returns the cells of the first table row whose rendered text
// contains match, as plain text: tags stripped, entities decoded, whitespace
// collapsed. It is deliberately a text extractor rather than a DOM: what is
// being asserted is what an operator READS.
func htmlCells(t *testing.T, body, match string) []string {
	t.Helper()
	for _, row := range strings.Split(body, "<tr") {
		end := strings.Index(row, "</tr>")
		if end < 0 {
			continue
		}
		row = row[:end]
		var cells []string
		for _, m := range reCell.FindAllStringSubmatch(row, -1) {
			cells = append(cells, htmlText(m[1]))
		}
		for _, c := range cells {
			if c == match {
				return cells
			}
		}
	}
	t.Fatalf("no rendered row has a cell reading %q", match)
	return nil
}

func htmlText(s string) string {
	return strings.TrimSpace(reWS.ReplaceAllString(html.UnescapeString(reTag.ReplaceAllString(s, " ")), " "))
}

// apiSpend reads one key's `spend` out of a /key/list response as the DIGITS it
// published.
//
// json.Number rather than a float64: the whole point of Money is that an exact
// decimal never passes through binary floating point (§8.3), so decoding to a
// float and formatting it again would compare the screen against a third
// answer rather than against the API's.
func apiSpend(t *testing.T, rec *httptest.ResponseRecorder, keyID string) string {
	t.Helper()
	var body struct {
		Keys []struct {
			TokenID string      `json:"token_id"`
			Spend   json.Number `json:"spend"`
		} `json:"keys"`
	}
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("/key/list is not the expected shape: %v\n%s", err, rec.Body.String())
	}
	for _, k := range body.Keys {
		if k.TokenID == keyID {
			return k.Spend.String()
		}
	}
	t.Fatalf("/key/list does not carry key %s:\n%s", keyID, rec.Body.String())
	return ""
}

// A spend dashboard reading zero is the failure this project has now found
// three times, and the third time it was on the screen whose whole subject is
// spend. The column must carry the LEDGER's figure — the one /key/list reports
// and /global/spend/report totals — and not `api_keys.spend_nano`, which no
// request-path writer touches and which is therefore 0 on every deployment.
func TestKeysScreenSpendColumnIsTheLedgersFigure(t *testing.T) {
	h := newHarness(t, withSpend)
	id, _ := h.newKey(map[string]any{"key_alias": "dogfood", "max_budget": 5})
	other, _ := h.newKey(map[string]any{"key_alias": "probe"})

	// Two priced requests against one key, one against the other. These rows
	// are the ledger; every figure asserted below is computed from them.
	h.store.addLog(LogRow{ID: "r1", TS: testNow.Add(-2 * time.Hour), APIKeyID: id,
		ModelGroup: "chat", Status: 200, CostNano: 600_000_000})
	h.store.addLog(LogRow{ID: "r2", TS: testNow.Add(-time.Hour), APIKeyID: id,
		ModelGroup: "chat", Status: 200, CostNano: 687_300})
	h.store.addLog(LogRow{ID: "r3", TS: testNow.Add(-time.Hour), APIKeyID: other,
		ModelGroup: "chat", Status: 200, CostNano: 2_777_300})

	// The stored column stays at zero, which is why a raw read is a zero rather
	// than a stale number — and why nobody noticed.
	if got := h.store.keys[id].SpendNano; got != 0 {
		t.Fatalf("the fixture writes api_keys.spend_nano = %d; the defect needs it at 0", got)
	}

	want := formatNano(600_687_300)
	rec := h.do(http.MethodGet, "/ui/keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	cells := htmlCells(t, body, id)
	if len(cells) != numKeyCols {
		t.Fatalf("the keys row renders %d cells, want %d: %q", len(cells), numKeyCols, cells)
	}
	if got := cells[colKeySpend]; got != want {
		t.Errorf("the spend column shows %q; the ledger says %q", got, want)
	}
	// The budget beside it is the stored ceiling and must not have moved.
	if got := cells[colKeyBudget]; got != "5" {
		t.Errorf("the budget column shows %q, want %q", got, "5")
	}
	// The second key's own figure, so that one hydrated row cannot stand in
	// for the page.
	if got := htmlCells(t, body, other)[colKeySpend]; got != formatNano(2_777_300) {
		t.Errorf("the second key's spend shows %q; the ledger says %q", got, formatNano(2_777_300))
	}

	// And the screen equals the API — read from both, compared as the digits
	// each one publishes. The footer's claim is that this page, the API and the
	// CLI are one answer rather than three; until a test reads two of them and
	// compares, that sentence is a hope, and it was printed under a column that
	// disagreed with /key/list about the same key from the same store.
	//
	// The comparison is on TEXT. Money renders through integer formatting on
	// both paths (§8.3: prices never pass through binary floating point), so a
	// float64 that survived a JSON round trip would be a third answer.
	api := h.do(http.MethodGet, "/key/list", nil)
	if api.Code != http.StatusOK {
		t.Fatalf("/key/list status %d", api.Code)
	}
	for id, screen := range map[string]string{
		id:    cells[colKeySpend],
		other: htmlCells(t, body, other)[colKeySpend],
	} {
		if got := apiSpend(t, api, id); got != screen {
			t.Errorf("key %s: /ui/keys shows %q and /key/list reports %q — the screen and "+
				"the API are not one answer", id, screen, got)
		}
	}

	// One batched lookup for the page, not one per row: a per-row query is how
	// an operator screen becomes a store outage at the page size operators use.
	if n := h.api.cfg.Spend.(*fakeSpend).calls.Load(); n != 2 {
		t.Errorf("the screen and the API made %d spend lookups, want 2 (one each)", n)
	}
}

// A reporter that cannot answer must not blank the screen: describing a
// credential is the incident-response job, and an accounting figure is not
// worth refusing it over.
func TestKeysScreenSurvivesASpendReporterThatFails(t *testing.T) {
	h := newHarness(t, withSpend)
	h.api.cfg.Spend.(*fakeSpend).err = errors.New("counter unavailable")
	id, _ := h.newKey(nil)
	rec := h.do(http.MethodGet, "/ui/keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := htmlCells(t, rec.Body.String(), id)[colKeySpend]; got != "0" {
		t.Errorf("spend column = %q, want the unhydrated stored value", got)
	}
}

// §11.6's pend is a refusal. A screen that renders a pended key as "active"
// describes a credential the gateway is turning away as one it is serving.
func TestKeysScreenShowsAPendedKey(t *testing.T) {
	h := newHarness(t)
	id, _ := h.newKey(nil)
	h.store.keys[id].PendedAt = testNow.Add(-time.Minute)
	h.store.keys[id].PendReason = "token rate 8x baseline"

	cells := htmlCells(t, h.do(http.MethodGet, "/ui/keys", nil).Body.String(), id)
	if got := cells[colKeyState]; got != "pended" {
		t.Errorf("state column = %q for a pended key, want %q", got, "pended")
	}
	// A block is an operator's decision and outranks a statistical one.
	h.store.keys[id].Blocked = true
	cells = htmlCells(t, h.do(http.MethodGet, "/ui/keys", nil).Body.String(), id)
	if got := cells[colKeyState]; got != "blocked" {
		t.Errorf("state column = %q for a blocked and pended key, want %q", got, "blocked")
	}
}

// The keys screen must not render a secret. It cannot, because it renders the
// same label-only type the API does — but the screen is where an operator would
// notice a regression last, so it is checked.
func TestKeysScreenShowsNoSecret(t *testing.T) {
	h := newHarness(t)
	_, token := h.newKey(nil)
	rec := h.do(http.MethodGet, "/ui/keys", nil)
	assertNoSecret(t, "ui/keys", rec.Body.String(), token)
}

// §8.5 exists so that a subscription's value is visible, which means cost and
// the notional figure appear together on the same screen rather than one being
// a click away.
func TestUsageScreenShowsCostAndNotionalTogether(t *testing.T) {
	h := newHarness(t)
	h.store.addLog(LogRow{
		ID: "a", TS: testNow.Add(-time.Hour), ModelGroup: "chat", TeamID: "t1", Status: 200,
		CostNano: 1_000_000_000, NotionalNano: 4_000_000_000, NotionalKnown: true,
	})
	rec := h.do(http.MethodGet, "/ui/usage", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, body)
	}
	cost := strings.Index(body, "cost (billed)")
	notional := strings.Index(body, "notional (list rate)")
	if cost < 0 || notional < 0 {
		t.Fatal("the usage screen does not show both figures")
	}
	if notional < cost {
		t.Error("the notional figure is rendered before the billed cost")
	}
	if !strings.Contains(body, "4.00×") {
		t.Errorf("leverage was not computed: %s", body[max(0, notional-200):min(len(body), notional+900)])
	}
	if !strings.Contains(body, "never billed") {
		t.Error("the notional figure is not labelled as an estimate")
	}
}

func TestUsageScreenReportsMissingNotional(t *testing.T) {
	h := newHarness(t)
	h.store.addLog(LogRow{ID: "a", TS: testNow.Add(-time.Hour), ModelGroup: "chat",
		Status: 200, CostNano: 1_000_000_000, NotionalKnown: false})
	rec := h.do(http.MethodGet, "/ui/usage", nil)
	body := rec.Body.String()
	if !strings.Contains(body, "unavailable") {
		t.Error("a missing notional figure was not shown as unavailable")
	}
	// The unavailability must be inside the notional tile, not merely somewhere
	// on the page: a zero in that tile is exactly the flattering answer §8.5
	// forbids.
	i := strings.Index(body, "notional (list rate)")
	if i < 0 {
		t.Fatal("no notional tile")
	}
	tile := body[i:min(len(body), i+400)]
	if !strings.Contains(tile, "unavailable") {
		t.Errorf("the notional tile does not report unavailability: %s", tile)
	}
	if strings.Contains(tile, `class="value">0`) {
		t.Errorf("a missing notional figure was rendered as zero: %s", tile)
	}
}

func TestUsageScreenRefusesATooWideWindow(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxTimeRange = 48 * time.Hour })
	rec := h.do(http.MethodGet, "/ui/usage?start_date=2026-01-01&end_date=2026-07-01", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "wider than this deployment allows") {
		t.Errorf("no explanation: %s", rec.Body.String())
	}
}

// The models screen is a VIEW, and it is served from what this process actually
// routes on. A navigation item that answers 501 on every click in every
// deployment — which is what an unset registry made of it — tells an operator
// less than the table it was withholding.
func TestModelsScreenIsServedFromTheRoutingTable(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		// No editable registry in this process: exactly the shipped wiring.
		c.Routing = c.Models
		c.Models = nil
	})
	if err := h.store.CreateDeployment(context.Background(), &Deployment{
		ID: "chat|prov-a|a/model", ModelGroup: "chat", ProviderID: "prov-a",
		UpstreamModel: "a/model", Weight: 3, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	h.store.aliases = []Alias{{Alias: "chat-latest", ModelGroup: "chat"}}

	rec := h.do(http.MethodGet, "/ui/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"chat|prov-a|a/model", "a/model", "chat-latest", "compiled"} {
		if !strings.Contains(body, want) {
			t.Errorf("the models screen does not mention %q", want)
		}
	}
	// The deployment id the screen shows is the one the ledger records, which
	// is the whole reason an operator reads this column.
	cells := htmlCells(t, body, "chat|prov-a|a/model")
	if cells[0] != "chat" || cells[2] != "prov-a" || cells[3] != "a/model" {
		t.Errorf("deployment row = %q", cells)
	}

	// And the WRITE routes still refuse, by name. Being able to see the routing
	// table is not being able to edit it.
	for _, p := range []string{"/model/new", "/model/update", "/model/delete"} {
		rec := h.do(http.MethodPost, p, map[string]any{"model_name": "x", "id": "y"})
		h.expectFault(rec, http.StatusNotImplemented, CodeDependencyOff)
	}
	h.expectFault(h.do(http.MethodGet, "/model/info", nil),
		http.StatusNotImplemented, CodeDependencyOff)
}

// With neither source configured the screen says so, rather than rendering an
// empty table that reads as "this gateway routes nothing".
func TestModelsScreenWithNeitherSource(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Models = nil })
	rec := h.do(http.MethodGet, "/ui/models", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Error("the screen does not say what is missing")
	}
}

// A navigation item that cannot answer is not a disclosure that a dependency is
// optional; it is a broken product. The nav therefore lists what this process
// can serve THIS viewer, and the screens' own guards stay exactly as they were
// — a link is not an authorization decision.
// The sidebar is grouped into labelled sections rather than one flat column,
// and grouping drops no link: every screen the flat nav offered still has its
// href, now under a section title.
func TestNavIsGroupedIntoSections(t *testing.T) {
	h := newHarness(t)
	body := h.do(http.MethodGet, "/ui/keys", nil).Body.String()
	for _, title := range []string{`nav-group-title">access`, `nav-group-title">observability`} {
		if !strings.Contains(body, title) {
			t.Errorf("the nav is not grouped: missing %q", title)
		}
	}
	for _, href := range []string{`href="/ui/keys"`, `href="/ui/usage"`, `href="/ui/monitoring"`} {
		if !strings.Contains(body, href) {
			t.Errorf("grouping dropped a link: %s", href)
		}
	}
}

func TestNavAdvertisesOnlyScreensThatAnswer(t *testing.T) {
	t.Run("no model catalog", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.Models = nil })
		body := h.do(http.MethodGet, "/ui/keys", nil).Body.String()
		if strings.Contains(body, `href="/ui/models"`) {
			t.Error("the nav advertises the models screen in a process that cannot serve it")
		}
		for _, want := range []string{`href="/ui/keys"`, `href="/ui/usage"`} {
			if !strings.Contains(body, want) {
				t.Errorf("the nav dropped %s, which this process serves", want)
			}
		}
		// The guard is unchanged: typing the URL still gets the named refusal.
		if got := h.do(http.MethodGet, "/ui/models", nil).Code; got != http.StatusNotImplemented {
			t.Errorf("/ui/models = %d, want 501", got)
		}
	})

	t.Run("team-scoped administrator", func(t *testing.T) {
		h := newHarness(t)
		body := h.do(http.MethodGet, "/ui/keys", nil, asToken(teamAToken)).Body.String()
		if !strings.Contains(body, `href="/ui/keys"`) {
			t.Error("a team administrator is not offered the keys screen, which answers for their team")
		}
		for _, screen := range []string{"models", "usage"} {
			if strings.Contains(body, `href="/ui/`+screen+`"`) {
				t.Errorf("a team administrator is offered %s, which is deployment-wide and "+
					"can only answer them with a 403", screen)
			}
		}
	})

	t.Run("a refusal offers a screen that works", func(t *testing.T) {
		h := newHarness(t)
		body := h.do(http.MethodGet, "/ui/batches", nil).Body.String()
		if !strings.Contains(body, `href="/ui/keys"`) {
			t.Error("the refusal page offers no way out")
		}
		// Nowhere to go is said by saying nothing, rather than by sending an
		// operator from a screen that failed to one that cannot answer either.
		h2 := newHarness(t, func(c *Config) { c.Keys, c.Models, c.Ledger, c.Directory = nil, nil, nil, nil })
		body = h2.do(http.MethodGet, "/ui/batches", nil).Body.String()
		if strings.Contains(body, "go to") {
			t.Errorf("a process that serves no screen still offers one: %s", body)
		}
	})
}

// The footer used to assert, on every page including the ones with no figures
// at all, that "every figure comes from the same engine the API and the CLI use,
// so there is one answer rather than three" — while the keys screen showed a
// spend column that disagreed with /key/list about the same key. A claim that
// strong, citing the section that makes it, is what stops a reader from
// cross-checking, so it now belongs to the screens that earn it.
func TestProvenanceIsClaimedOnlyWhereThereAreFigures(t *testing.T) {
	h := newHarness(t, withSpend)
	h.newKey(nil)

	if body := h.do(http.MethodGet, "/ui/keys", nil).Body.String(); !strings.Contains(
		body, "spend is the §9.4 rollup /key/list reports") {
		t.Error("the keys screen does not say where its spend column comes from")
	}
	if body := h.do(http.MethodGet, "/ui/usage", nil).Body.String(); !strings.Contains(
		body, "/global/spend/report aggregates") {
		t.Error("the usage screen does not say where its figures come from")
	}

	// A page that produced no figure claims nothing about figures.
	body := h.do(http.MethodGet, "/ui/batches", nil).Body.String()
	if !strings.Contains(body, "Read-only operator view") {
		t.Fatal("the refusal page lost its footer entirely")
	}
	for _, claim := range []string{"one answer rather than three", "comes from", "§9.4"} {
		if strings.Contains(body, claim) {
			t.Errorf("a page with no figures claims %q", claim)
		}
	}
}

// The screens themselves are still read-only. The UI mutates now, and it does
// so through exactly one URL — see [uiActionPath] and the tests in
// uikeys_test.go — so a POST at a SCREEN is refused whoever sends it.
//
// This test used to assert that the whole UI refused every POST, which was the
// old security model in one line. It is kept, narrowed to what is still true,
// rather than deleted: an operator's browser posting to /ui/keys and getting a
// mutation would be the same defect wearing a different path.
func TestTheScreensThemselvesAreReadOnly(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/ui/keys", "/ui/models", "/ui/usage"} {
		rec := h.do(http.MethodPost, p, map[string]any{})
		// A header-authenticated caller cannot mutate the UI at all, so this is
		// the forgery refusal rather than a method one. Either way: not 2xx and
		// not a mutation.
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s = %d, want 403; a screen must not mutate", p, rec.Code)
		}
	}
}

func TestUISecurityHeaders(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/ui/keys", nil)
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP = %q, want it to contain %q", csp, want)
		}
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("no nosniff header")
	}
}

func TestUIUnknownScreenIs501(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/ui/batches", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestUIRootRedirects(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/ui", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/keys" {
		t.Fatalf("status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
}

// A browser cannot send a bearer header on a navigation, so an unauthenticated
// visitor is sent to a sign-in form rather than to a bare 401.
func TestUnauthenticatedUIRedirectsToLogin(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/ui/keys", nil, asToken(""))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/login") {
		t.Fatalf("Location = %q", loc)
	}
}

func TestUILoginIssuesASessionAndLogoutRevokesIt(t *testing.T) {
	h := newHarness(t)

	form := strings.NewReader("key=" + masterToken + "&next=/ui/usage")
	req := httptest.NewRequest(http.MethodPost, "/ui/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/usage" {
		t.Errorf("Location = %q, want the requested screen", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie")
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/ui" {
		t.Errorf("session cookie is not locked down: %+v", c)
	}

	// The cookie authorizes the UI...
	rec2 := h.do(http.MethodGet, "/ui/keys", nil, asToken(""), func(r *http.Request) {
		r.AddCookie(c)
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("cookie did not authorize the UI: %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "sign out") {
		t.Error("a cookie session should offer a sign-out control")
	}

	// ...and nothing else. The administration API never accepts it, which is
	// what makes the read-only UI free of cross-site request forgery risk.
	rec3 := h.do(http.MethodGet, "/key/list", nil, asToken(""), func(r *http.Request) {
		r.AddCookie(c)
	})
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("the API accepted a UI cookie: %d", rec3.Code)
	}

	// Signing out revokes it immediately.
	out := h.do(http.MethodPost, "/ui/logout", nil, asToken(""), func(r *http.Request) {
		r.AddCookie(c)
	})
	if out.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d", out.Code)
	}
	rec4 := h.do(http.MethodGet, "/ui/keys", nil, asToken(""), func(r *http.Request) {
		r.AddCookie(c)
	})
	if rec4.Code != http.StatusSeeOther {
		t.Fatalf("a revoked session still authorized: %d", rec4.Code)
	}
}

func TestUILoginRejectsNonAdminCredential(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/ui/login",
		strings.NewReader("key="+userToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a session was issued to a non-administrative credential")
	}
}

// An open redirect through ?next= would turn the sign-in page into a phishing
// hop, so anything outside the UI's own mount is discarded.
func TestLoginNextIsConfinedToTheUI(t *testing.T) {
	for _, next := range []string{
		"https://evil.example/", "//evil.example/", "/key/list", "/ui/keys\nX", "",
	} {
		if got := safeNext("/ui", next); !strings.HasPrefix(got, "/ui/") || got == "/ui//" {
			t.Errorf("safeNext(%q) = %q", next, got)
		}
	}
	if got := safeNext("/ui", "/ui/usage?start_date=2026-07-01"); got != "/ui/usage?start_date=2026-07-01" {
		t.Errorf("safeNext discarded a legitimate destination: %q", got)
	}
}

// signIn exchanges a credential for a session cookie, as a browser would.
func signIn(t *testing.T, h *harness, token string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader("key="+token))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d\n%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie")
	}
	return cookies[0]
}

// seedKey puts a raw key row in the store, for the credentials the fake
// authenticator already knows about.
func seedKey(h *harness, k *Key) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.keys[k.ID] = k
	h.store.keyOrder = append(h.store.keyOrder, k.ID)
}

// An administrative session that outlives the credential that created it is the
// same gap the invalidation work closed on the request path — and worse here,
// because this is the surface the key was revoked FROM. The bound is the next
// request, not the session TTL.
func TestRevokingAKeyEndsItsUISession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(h *harness)
	}{
		{"blocked", func(h *harness) {
			rec := h.do(http.MethodPost, "/key/block", map[string]any{"key_id": "key-admin"})
			if rec.Code != http.StatusOK {
				h.t.Fatalf("/key/block status %d: %s", rec.Code, rec.Body.String())
			}
		}},
		{"deleted", func(h *harness) {
			rec := h.do(http.MethodPost, "/key/delete", map[string]any{"keys": []string{"key-admin"}})
			if rec.Code != http.StatusOK {
				h.t.Fatalf("/key/delete status %d: %s", rec.Code, rec.Body.String())
			}
		}},
		{"pended", func(h *harness) {
			h.store.mu.Lock()
			h.store.keys["key-admin"].PendedAt = testNow
			h.store.mu.Unlock()
		}},
		{"expired", func(h *harness) {
			h.store.mu.Lock()
			h.store.keys["key-admin"].ExpiresAt = testNow.Add(-time.Minute)
			h.store.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			seedKey(h, &Key{ID: "key-admin", KeyLabel: "dk-admin", CreatedAt: testNow})
			c := signIn(t, h, adminToken)

			get := func() int {
				return h.do(http.MethodGet, "/ui/keys", nil, asToken(""),
					func(r *http.Request) { r.AddCookie(c) }).Code
			}
			if got := get(); got != http.StatusOK {
				t.Fatalf("the session did not authorize the screen: %d", got)
			}
			tc.revoke(h)
			if got := get(); got != http.StatusSeeOther {
				t.Fatalf("a revoked credential still authorized the screen: %d — "+
					"the session TTL is not a revocation", got)
			}
			// And the session is gone from the table rather than merely refused,
			// so a second tab does not have to discover this again.
			if _, live := h.api.ui.lookupSession(c.Value); live {
				t.Error("the session survived in the table")
			}
		})
	}
}

// The scope a session carries was derived from the key's team. Moving the key
// changes that derivation, and a session holding the old answer is ended rather
// than left administering a team its credential has left.
func TestMovingAKeysTeamEndsItsUISession(t *testing.T) {
	h := newHarness(t)
	seedKey(h, &Key{ID: "key-team-a", KeyLabel: "dk-team-a", TeamID: "team-a", CreatedAt: testNow})
	c := signIn(t, h, teamAToken)

	get := func() int {
		return h.do(http.MethodGet, "/ui/keys", nil, asToken(""),
			func(r *http.Request) { r.AddCookie(c) }).Code
	}
	if got := get(); got != http.StatusOK {
		t.Fatalf("the team session did not authorize the keys screen: %d", got)
	}
	h.store.mu.Lock()
	h.store.keys["key-team-a"].TeamID = "team-b"
	h.store.mu.Unlock()
	if got := get(); got != http.StatusSeeOther {
		t.Errorf("a session scoped to team-a still authorized after its key moved to team-b: %d", got)
	}
}

// The master credential has no row to check (§2.4), so it is not checked — and
// must not be refused for having nothing to find.
func TestMasterSessionIsNotRefusedForHavingNoKeyRow(t *testing.T) {
	h := newHarness(t)
	c := signIn(t, h, masterToken)
	rec := h.do(http.MethodGet, "/ui/keys", nil, asToken(""), func(r *http.Request) { r.AddCookie(c) })
	if rec.Code != http.StatusOK {
		t.Fatalf("the master credential's session was refused: %d", rec.Code)
	}
}

// Behind a reverse proxy holding the certificate, dorang sees plaintext. The
// cookie must still be marked Secure, or an administrative session ships in the
// clear on the next plaintext request to the same host.
func TestSessionCookieIsSecureBehindATerminatingProxy(t *testing.T) {
	for _, tc := range []struct {
		name, header, value string
		want                bool
	}{
		{"plain http", "", "", false},
		{"x-forwarded-proto", "X-Forwarded-Proto", "https", true},
		{"x-forwarded-proto chain", "X-Forwarded-Proto", "https, http", true},
		{"x-forwarded-proto http", "X-Forwarded-Proto", "http", false},
		{"rfc 7239", "Forwarded", `for=192.0.2.1;proto=https`, true},
		{"rfc 7239 quoted", "Forwarded", `proto="https";for=192.0.2.1`, true},
		{"rfc 7239 http", "Forwarded", `for=192.0.2.1;proto=http`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			req := httptest.NewRequest(http.MethodPost, "/ui/login",
				strings.NewReader("key="+masterToken))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.header != "" {
				req.Header.Set(tc.header, tc.value)
			}
			rec := httptest.NewRecorder()
			h.api.ServeHTTP(rec, req)
			cookies := rec.Result().Cookies()
			if len(cookies) == 0 {
				t.Fatalf("no cookie: %d", rec.Code)
			}
			if got := cookies[0].Secure; got != tc.want {
				t.Errorf("Secure = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSessionExpires(t *testing.T) {
	now := testNow
	h := newHarness(t, func(c *Config) {
		c.SessionTTL = time.Minute
		c.Now = func() time.Time { return now }
	})
	id := h.api.ui.newSession(fakePrincipal{kind: "master", admin: true})
	if _, ok := h.api.ui.lookupSession(id); !ok {
		t.Fatal("a fresh session did not resolve")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := h.api.ui.lookupSession(id); ok {
		t.Fatal("an expired session still resolved")
	}
}

func TestUICanBeDisabled(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.UIPrefix = "-" })
	if h.api.ui != nil {
		t.Fatal("the UI was built despite being disabled")
	}
	rec := h.do(http.MethodGet, "/ui/keys", nil)
	h.expectFault(rec, http.StatusNotImplemented, CodeNotImplemented)
}

func TestUIWithoutSessionsRequiresAHeader(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DisableUISessions = true })
	rec := h.do(http.MethodGet, "/ui/keys", nil, asToken(""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	rec = h.do(http.MethodGet, "/ui/keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("header auth did not work: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sign out") {
		t.Error("a header-authenticated view offered a sign-out control")
	}
}

func TestScreensDegradeWhenADependencyIsAbsent(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Keys = nil
		c.Models = nil
		c.Ledger = nil
	})
	for _, p := range []string{"/ui/keys", "/ui/models", "/ui/usage"} {
		rec := h.do(http.MethodGet, p, nil)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s = %d, want 501", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "not configured") {
			t.Errorf("%s does not say what is missing", p)
		}
	}
}

func TestHumanInt(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 12: "12", 1234: "1,234", 1234567: "1,234,567", -4321: "-4,321",
	} {
		if got := humanInt(in); got != want {
			t.Errorf("humanInt(%d) = %q, want %q", in, got, want)
		}
	}
}
