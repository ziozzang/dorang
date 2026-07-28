package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The fakes in this file are the whole point of the narrow interfaces in
// deps.go: the HTTP surface is exercised end to end, including a benchmark,
// without a router, an authenticator, a store or a provider existing.

// fakePrincipal is an authenticated caller with a model allow-list.
type fakePrincipal struct {
	id      string
	user    string
	team    string
	models  []string // nil allows everything
	refuse  error
	nRoutes []string
}

func (p *fakePrincipal) KeyID() string  { return p.id }
func (p *fakePrincipal) UserID() string { return p.user }
func (p *fakePrincipal) TeamID() string { return p.team }

func (p *fakePrincipal) Authorize(a Access) error {
	if p.refuse != nil {
		return p.refuse
	}
	if len(p.nRoutes) > 0 {
		for _, r := range p.nRoutes {
			if r == a.Route {
				return NewError(http.StatusForbidden, TypePermission,
					"route not allowed for this key").WithCode("route_not_allowed")
			}
		}
	}
	return nil
}

func (p *fakePrincipal) AllowsModel(m string) bool {
	if p.models == nil {
		return true
	}
	for _, v := range p.models {
		if v == m {
			return true
		}
	}
	return false
}

// fakeAuth accepts any of the six headers and resolves the token through a map.
type fakeAuth struct {
	keys map[string]*fakePrincipal
	// seen records the header name that carried the credential.
	mu   sync.Mutex
	seen []string
}

func newFakeAuth(tokens ...string) *fakeAuth {
	a := &fakeAuth{keys: map[string]*fakePrincipal{}}
	for _, t := range tokens {
		a.keys[t] = &fakePrincipal{id: "key-" + t}
	}
	return a
}

func (a *fakeAuth) AuthenticateHeader(_ context.Context, h http.Header) (Principal, error) {
	tok, name := extractToken(h)
	if name != "" {
		a.mu.Lock()
		a.seen = append(a.seen, name)
		a.mu.Unlock()
	}
	if tok == "" {
		return nil, NewError(http.StatusUnauthorized, TypeAuthentication,
			"no credential presented").WithCode("no_credential")
	}
	p, ok := a.keys[tok]
	if !ok {
		return nil, NewError(http.StatusUnauthorized, TypeAuthentication,
			"unknown key").WithCode("invalid_api_key")
	}
	return p, nil
}

// extractToken mirrors what a real authenticator does with the six accepted
// header names, so the fake exercises the same acceptance surface.
func extractToken(h http.Header) (token, header string) {
	for _, name := range authHeaders {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if name == HeaderAuthorization {
			if len(v) >= 7 && strings.EqualFold(v[:7], "bearer ") {
				return strings.TrimSpace(v[7:]), name
			}
			if !strings.ContainsAny(v, " \t") {
				return v, name
			}
			continue
		}
		return v, name
	}
	return "", ""
}

// fakeDispatcher writes whatever the test told it to.
type fakeDispatcher struct {
	fn func(ctx context.Context, rq *Request, w http.ResponseWriter) error
}

func (d fakeDispatcher) Dispatch(ctx context.Context, rq *Request, w http.ResponseWriter) error {
	if d.fn == nil {
		return nil
	}
	return d.fn(ctx, rq, w)
}

// jsonDispatcher answers a fixed non-streaming JSON body and reports a routing
// decision, which is what the extension-header contract needs.
func jsonDispatcher(body string) Dispatcher {
	return DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
		rq.Result.Provider = "prov-1"
		rq.Result.Credential = "cred-1"
		rq.Result.Deployment = "dep-1"
		rq.Result.UpstreamModel = "upstream-" + rq.Model
		rq.Result.Attempt = 1
		rq.Result.RouteReason = "least_busy"
		rq.Result.Tokens = Usage{Input: 11, Output: 22, Total: 33}
		rq.Result.CostNanoUSD = 123456
		rq.Result.Priced = true
		rq.Result.LatencyNS = 5_000_000
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, body)
		return err
	})
}

// recordingMeter keeps every event.
type recordingMeter struct {
	mu     sync.Mutex
	events []Event
}

func (m *recordingMeter) Record(ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
}

func (m *recordingMeter) last(t *testing.T) Event {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.events) == 0 {
		t.Fatal("no metering event was recorded")
	}
	return m.events[len(m.events)-1]
}

// panicMeter proves that metering failure never fails the request.
type panicMeter struct{}

func (panicMeter) Record(Event) { panic("meter exploded") }

// newTestServer builds a server with the usual fakes.
func newTestServer(t *testing.T, mut func(*Options)) *Server {
	t.Helper()
	opts := Options{
		Auth:       newFakeAuth("good"),
		Dispatcher: jsonDispatcher(`{"ok":true}`),
		Models:     ModelSlice{{ID: "model-x"}, {ID: "model-y"}},
	}
	if mut != nil {
		mut(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// do runs one request against the handler and returns the recorder.
func do(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// post builds an authenticated POST.
func post(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	return r
}

// get builds an authenticated GET.
func get(path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set(HeaderAuthorization, "Bearer good")
	return r
}
