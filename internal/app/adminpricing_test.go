package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
)

// pricedYAML is a gateway with a marginal rule, a notional rule and a discount,
// so a preview has something to explain in three of the four classes.
const pricedYAML = `
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
    - {id: tokens, class: marginal_usage, match: {provider: p1}, rates: {input: "2.00", output: "6.00"}}
    - {id: list, class: notional_rate, match: {provider: p1}, source: "vendor price page", as_of: "2026-01-01", rates: {input: "4.00", output: "12.00"}}
`

// TestPricingPreviewIsServedByTheAssembledGateway is §8.4's "one engine, one
// answer" for the two surfaces that could not reach the engine.
//
// `admin.Config.Pricing` was never set by this package, so `/admin/pricing/
// preview` and `/spend/calculate` answered 501 `dependency_unavailable` in every
// deployment — a complete implementation in internal/admin behind a dependency
// with no injector, while `dorangctl price` and the request path priced through
// the same catalog perfectly well.
//
// The assertion is on the STATUS and the BODY of an HTTP response from an
// assembled gateway, not on the adapter: the adapter was never the missing
// piece, the Config field was, and a unit test of the adapter would pass with
// the field still unset. Reverting the one line in buildAdmin turns both of
// these into 501.
func TestPricingPreviewIsServedByTheAssembledGateway(t *testing.T) {
	a := newWiringApp(t, pricedYAML, nil)

	for _, path := range []string{"/admin/pricing/preview", "/spend/calculate"} {
		t.Run(path, func(t *testing.T) {
			body := `{"provider":"p1","model":"m1-upstream","credential":"c1",
			          "prompt_tokens":1000000,"completion_tokens":500000}`
			r := adminRequest(httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			a.Server.ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("%s answered %d: %s", path, w.Code, w.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding %s: %v (%s)", path, err, w.Body.String())
			}

			// 2.00/M input on 1M plus 6.00/M output on 0.5M is 5.00 USD, and it
			// has to be the number the ledger and `dorangctl price` produce
			// from the same catalog — that is the whole of §8.4.
			if want, got := "5", jsonPath(t, got, "total"); got != want {
				t.Errorf("%s priced the request at %v, want %v", path, got, want)
			}
			// The engine's trace has to survive the mapping, or the endpoint is
			// a calculator rather than an explanation. A component names the
			// rule that produced it.
			comps, _ := got["components"].([]any)
			if len(comps) == 0 {
				t.Fatalf("%s returned no priced components: %s", path, w.Body.String())
			}
			if id := comps[0].(map[string]any)["rule_id"]; id != "tokens" {
				t.Errorf("%s attributed the first component to rule %v, want \"tokens\"", path, id)
			}
			// §8.5: the notional figure travels with its provenance and is not
			// a term in the total.
			if src := jsonPath(t, got, "notional", "source"); src != "vendor price page" {
				t.Errorf("%s lost the notional provenance: %v", path, src)
			}
		})
	}
}

// TestPricingPreviewFollowsAReload holds the reason the adapter reads the
// catalog off the dispatch state instead of capturing one.
//
// The price catalog is immutable by construction and swapped by pointer on
// SIGHUP (App.Reload), and picking up an edited catalog is the main reason an
// operator sends one. A preview that answers from the catalog the process
// started with is the surface they would check it on, telling them the edit did
// not take.
func TestPricingPreviewFollowsAReload(t *testing.T) {
	a := newWiringApp(t, pricedYAML, nil)

	price := func() string {
		t.Helper()
		body := `{"provider":"p1","model":"m1-upstream","prompt_tokens":1000000}`
		r := adminRequest(httptest.NewRequest(http.MethodPost, "/admin/pricing/preview",
			strings.NewReader(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.Server.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("preview answered %d: %s", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return jsonPath(t, got, "total")
	}

	if got := price(); got != "2" {
		t.Fatalf("before the reload the preview quotes %v, want 2", got)
	}

	edited := strings.Replace(pricedYAML,
		`rates: {input: "2.00", output: "6.00"}`,
		`rates: {input: "3.00", output: "6.00"}`, 1)
	cfg, err := config.LoadBytes([]byte(edited))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	// Same database: Reload deliberately does not swap the store, so handing it
	// a different path would be testing something else.
	cfg.Storage.SQLite.Path = a.opts.Config.Storage.SQLite.Path
	if err := a.Reload(cfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := price(); got != "3" {
		t.Errorf("after the reload the preview quotes %v, want 3: the adapter is "+
			"holding the catalog the process started with", got)
	}
}

// jsonPath reads a dotted path out of a decoded body and renders it as a
// string, so a money field can be compared without deciding here whether the
// wire form is a number or a decimal string.
func jsonPath(t *testing.T, m map[string]any, path ...string) string {
	t.Helper()
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v is not an object at %q", cur, p)
		}
		cur, ok = obj[p]
		if !ok {
			t.Fatalf("no %q in %v", p, obj)
		}
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return strings.TrimRight(strings.TrimRight(
			jsonNumber(v), "0"), ".")
	}
	return ""
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	s := string(b)
	if !strings.Contains(s, ".") {
		s += "."
	}
	return s
}
