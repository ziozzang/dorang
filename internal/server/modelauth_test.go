package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every registered route declares how the model allow-list is enforced for it.
//
// This is the call-graph test the review asked for. It does not check that the
// check works — two unit tests already did that, which is exactly why nobody
// noticed it was never reached. It checks that every route has ANSWERED the
// question, over the real route set the server builds, so a route added later
// cannot reach the mux with the question unanswered.
func TestEveryRouteDeclaresAModelAuthMode(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Passthrough = []PassthroughRoute{
			{Prefix: "/anthropic", Provider: "p", BaseURL: "http://127.0.0.1:1"},
		}
	})
	cfg := s.snap.Load()

	seen := 0
	check := func(rt *Route) {
		seen++
		if rt.ModelAuth == ModelAuthUnset {
			t.Errorf("route %q (%s) does not declare a ModelAuth mode", rt.Name, rt.Pattern)
		}
		if rt.ModelAuth == ModelAuthGate && !rt.NeedsBody {
			t.Errorf("route %q declares ModelAuthGate without a body: the gate would "+
				"scan no model and enforce nothing", rt.Name)
		}
	}
	for _, rt := range cfg.routes.exact {
		check(rt)
	}
	for _, rt := range cfg.routes.pats {
		check(rt)
	}
	if seen == 0 {
		t.Fatal("no routes were inspected")
	}
}

// A route that forgets the declaration cannot be registered at all.
func TestRouteTableRefusesAnUndeclaredModelAuth(t *testing.T) {
	noop := func(http.ResponseWriter, *Request) error { return nil }
	_, err := newRouteTable([]*Route{
		{Pattern: "/x", Methods: MethodPOST, Name: "x", Handler: noop},
	})
	if err != ErrModelAuthUnset {
		t.Fatalf("newRouteTable accepted a route with no ModelAuth mode: %v", err)
	}

	// And ModelAuthGate without a body, which is the shape that made
	// passthrough look authorized while enforcing nothing.
	_, err = newRouteTable([]*Route{
		{Pattern: "/y", Methods: MethodPOST, Name: "y",
			ModelAuth: ModelAuthGate, NeedsBody: false, Handler: noop},
	})
	if err != ErrModelAuthUnset {
		t.Fatalf("newRouteTable accepted ModelAuthGate with no body: %v", err)
	}
}

// A handler that declares it will name its models and then does not fails the
// request rather than serving it unchecked.
//
// This is the post-condition that turns the omission from silent into loud. It
// is the whole reason ModelAuthHandler is safe to offer: the alternative to
// "the handler checks" was "the handler was supposed to check".
func TestModelAuthHandlerRouteThatSkipsTheCheckFails(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Routes = []Route{{
			Pattern:   "/forgetful",
			Methods:   MethodPOST,
			Name:      "forgetful",
			ModelAuth: ModelAuthHandler,
			Handler: func(w http.ResponseWriter, rq *Request) error {
				// Dispatches without ever asking. Exactly the batch-create and
				// passthrough shape.
				return nil
			},
		}}
	})

	r := httptest.NewRequest(http.MethodPost, "/forgetful", strings.NewReader(""))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := do(s, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: a route that skipped the allow-list served the request", w.Code)
	}
	if !strings.Contains(w.Body.String(), "model_auth_missing") {
		t.Errorf("body does not name the missing decision: %s", w.Body.String())
	}
}

// A handler that does name its model is served, and the decision is recorded.
func TestModelAuthHandlerRouteThatChecksIsServed(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Routes = []Route{{
			Pattern:   "/careful",
			Methods:   MethodPOST,
			Name:      "careful",
			ModelAuth: ModelAuthHandler,
			Handler: func(w http.ResponseWriter, rq *Request) error {
				if err := rq.AuthorizeModel("model-x"); err != nil {
					return err
				}
				_, err := io.WriteString(w, `{"ok":true}`)
				return err
			},
		}}
	})

	r := httptest.NewRequest(http.MethodPost, "/careful", strings.NewReader(""))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	if w := do(s, r); w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body.String())
	}
}

// AuthorizeModel refuses a model outside the key's list, from a handler, with
// the same status and code the gate uses: 403 permission_error, COMPATIBILITY
// §11.2.
func TestAuthorizeModelRefusesAModelOutsideTheAllowList(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Auth = restrictedAuth("allowed")
		o.Routes = []Route{{
			Pattern:   "/named",
			Methods:   MethodPOST,
			Name:      "named",
			ModelAuth: ModelAuthHandler,
			Handler: func(w http.ResponseWriter, rq *Request) error {
				return rq.AuthorizeModel("forbidden")
			},
		}}
	})

	r := httptest.NewRequest(http.MethodPost, "/named", strings.NewReader(""))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := do(s, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 (COMPATIBILITY §11.2)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "model_not_allowed") {
		t.Errorf("body %s", w.Body.String())
	}
}

// The allow-list is enforced at the GATE for every ModelAuthGate route, not
// inside one handler.
//
// It used to live in handleInference, which is reached by the six inference
// patterns and by nothing else — so a route that shared the gate's body-scanning
// but not that handler got no allow-list at all. This registers exactly such a
// route: same Family, same NeedsBody, a handler that checks nothing.
func TestGateEnforcesTheAllowListWithoutHelpFromTheHandler(t *testing.T) {
	served := false
	s := newTestServer(t, func(o *Options) {
		o.Auth = restrictedAuth("allowed")
		o.Routes = []Route{{
			Pattern:   "/v1/other/completions",
			Methods:   MethodPOST,
			Name:      "other",
			Family:    FamilyOpenAIChat,
			NeedsBody: true,
			ModelAuth: ModelAuthGate,
			Handler: func(w http.ResponseWriter, rq *Request) error {
				served = true
				return nil
			},
		}}
	})

	w := do(s, post("/v1/other/completions", `{"model":"forbidden"}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: the gate did not enforce the allow-list", w.Code)
	}
	if served {
		t.Error("the handler ran for a model the key may not use")
	}
}

// restrictedAuth builds an authenticator whose one key may use only the given
// models.
func restrictedAuth(models ...string) *fakeAuth {
	return &fakeAuth{keys: map[string]*fakePrincipal{
		"good": {id: "key-1", models: models},
	}}
}
