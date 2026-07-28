package server

import (
	"net/http"
	"testing"
)

// TestRouteSpecificityBeatsRegistrationOrder is COMPATIBILITY §7.5's golden
// case, and it is here because the failure mode is silent: a prefix router
// registers /openai/{rest...} first, the catch-all matches
// /openai/deployments/gpt-4o/chat/completions, and every Azure-style client
// gets a passthrough relay instead of the deployment route. Nothing errors. The
// requests just go somewhere else.
func TestRouteSpecificityBeatsRegistrationOrder(t *testing.T) {
	hit := func(name string) Handler {
		return func(w http.ResponseWriter, rq *Request) error {
			rq.Result.RouteReason = name
			return nil
		}
	}
	// Registered catch-all first, on purpose.
	table, err := newRouteTable([]*Route{
		{Pattern: "/openai/{rest...}", Methods: MethodPOST, Name: "catchall", Handler: hit("catchall")},
		{Pattern: "/openai/deployments/{model}/chat/completions", Methods: MethodPOST, Name: "deployment", Handler: hit("deployment")},
		{Pattern: "/openai/deployments/{model}/{rest...}", Methods: MethodPOST, Name: "deployment-any", Handler: hit("deployment-any")},
	})
	if err != nil {
		t.Fatalf("newRouteTable: %v", err)
	}

	cases := []struct {
		path, want, wantModel string
	}{
		{"/openai/deployments/gpt-4o/chat/completions", "deployment", "gpt-4o"},
		{"/openai/deployments/gpt-4o/embeddings", "deployment-any", "gpt-4o"},
		{"/openai/v1/anything/at/all", "catchall", ""},
		{"/openai", "catchall", ""},
		{"/openai/", "catchall", ""},
	}
	for _, c := range cases {
		var params [maxParams]Param
		rt, np, res := table.lookup(http.MethodPost, c.path, &params)
		if res != lookupHit {
			t.Fatalf("%s: lookup result %v, want hit", c.path, res)
		}
		if rt.Name != c.want {
			t.Errorf("%s: matched %q, want %q", c.path, rt.Name, c.want)
		}
		if c.wantModel != "" {
			var got string
			for i := 0; i < np; i++ {
				if params[i].Name == "model" {
					got = params[i].Value
				}
			}
			if got != c.wantModel {
				t.Errorf("%s: model param %q, want %q", c.path, got, c.wantModel)
			}
		}
	}
}

// TestRouteSpecificityIsIndependentOfOrder registers the same set in both
// orders and requires the same answer. Specificity ordering that happens to
// work because of how the table was built is not specificity ordering.
func TestRouteSpecificityIsIndependentOfOrder(t *testing.T) {
	mk := func(p, n string) *Route {
		return &Route{Pattern: p, Methods: MethodGET, Name: n,
			Handler: func(http.ResponseWriter, *Request) error { return nil }}
	}
	a := []*Route{
		mk("/a/{x}/c", "param"),
		mk("/a/b/c", "literal"),
		mk("/a/{rest...}", "wild"),
	}
	b := []*Route{
		mk("/a/{rest...}", "wild"),
		mk("/a/b/c", "literal"),
		mk("/a/{x}/c", "param"),
	}
	for _, set := range [][]*Route{a, b} {
		table, err := newRouteTable(set)
		if err != nil {
			t.Fatalf("newRouteTable: %v", err)
		}
		var params [maxParams]Param
		// "/a/b/c" is exact, so it never reaches the pattern scan; the
		// interesting case is that /a/z/c prefers the parameter over the
		// wildcard.
		rt, _, res := table.lookup(http.MethodGet, "/a/z/c", &params)
		if res != lookupHit || rt.Name != "param" {
			t.Fatalf("/a/z/c matched %v/%v, want param", res, routeName(rt))
		}
		rt, _, res = table.lookup(http.MethodGet, "/a/z/d/e", &params)
		if res != lookupHit || rt.Name != "wild" {
			t.Fatalf("/a/z/d/e matched %v/%v, want wild", res, routeName(rt))
		}
	}
}

func routeName(rt *Route) string {
	if rt == nil {
		return "<nil>"
	}
	return rt.Name
}

// TestT0RoutesAreRegistered checks the eleven paths of COMPATIBILITY §0, with
// the methods each one has to answer.
func TestT0RoutesAreRegistered(t *testing.T) {
	s := newTestServer(t, nil)
	table := s.snap.Load().routes

	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodPost, "/chat/completions"},
		{http.MethodPost, "/v1/embeddings"},
		{http.MethodPost, "/embeddings"},
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/models"},
		{http.MethodGet, "/health/liveliness"},
		{http.MethodOptions, "/health/liveliness"},
		{http.MethodGet, "/health/liveness"},
		{http.MethodOptions, "/health/liveness"},
		{http.MethodGet, "/health/readiness"},
		{http.MethodOptions, "/health/readiness"},
		{http.MethodPost, "/v1/messages"},
		{http.MethodPost, "/v1/messages/count_tokens"},
		{http.MethodGet, "/metrics"},
	}
	for _, c := range cases {
		var params [maxParams]Param
		_, _, res := table.lookup(c.method, c.path, &params)
		if res != lookupHit {
			t.Errorf("%s %s: %v, want hit", c.method, c.path, res)
		}
	}
}

// TestMethodMismatchIs405 keeps a wrong method distinguishable from a missing
// route. A 501 there would tell a client the endpoint does not exist, which is
// a different bug to chase.
func TestMethodMismatchIs405(t *testing.T) {
	s := newTestServer(t, nil)
	w := do(s, get("/v1/chat/completions"))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "POST" {
		t.Errorf("Allow %q, want POST", allow)
	}
}

// TestTrailingSlashTolerated: a single trailing slash resolves rather than
// answering 501, and does so without a redirect — a 307 on a POST costs a round
// trip and some clients drop the body on the retry.
func TestTrailingSlashTolerated(t *testing.T) {
	s := newTestServer(t, nil)
	w := do(s, get("/v1/models/"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want 200", w.Code, w.Body.String())
	}
}

// TestCompileRejectsBadPatterns covers the shapes that would silently misroute.
func TestCompileRejectsBadPatterns(t *testing.T) {
	bad := []string{
		"",                     // empty
		"v1/models",            // no leading slash
		"/a/{rest...}/b",       // non-terminal wildcard
		"/a/{b",                // unbalanced
		"/{a}/{b}/{c}/{d}/{e}", // more params than the pooled array holds
	}
	for _, p := range bad {
		if _, err := compile(p); err == nil {
			t.Errorf("compile(%q) succeeded, want error", p)
		}
	}
}

// TestDuplicateExactRouteRejected: merging two handlers on one path would make
// one of them unreachable, which is a configuration bug worth failing on.
func TestDuplicateExactRouteRejected(t *testing.T) {
	h := func(http.ResponseWriter, *Request) error { return nil }
	_, err := newRouteTable([]*Route{
		{Pattern: "/x", Methods: MethodGET, Handler: h},
		{Pattern: "/x", Methods: MethodPOST, Handler: h},
	})
	if err == nil {
		t.Fatal("duplicate exact route accepted")
	}
}
