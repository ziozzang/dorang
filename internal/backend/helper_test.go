package backend

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// fakeUpstream is a provider that records what it was asked.
type fakeUpstream struct {
	srv *httptest.Server

	mu   sync.Mutex
	reqs []recorded

	// handler answers one request. Nil answers 200 with body.
	handler http.HandlerFunc
	status  int
	body    string
	header  http.Header
}

type recorded struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{status: http.StatusOK, body: `{}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			header: r.Header.Clone(), body: body,
		})
		h := f.handler
		status, out, extra := f.status, f.body, f.header
		f.mu.Unlock()

		if h != nil {
			h(w, r)
			return
		}
		for k, vs := range extra {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) answer(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeUpstream) setHandler(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
}

func (f *fakeUpstream) last() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return recorded{}
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// staticCredentials is the test's credential table.
type staticCredentials struct {
	secrets map[string]string
	oauth   map[string]Applier
}

func (s staticCredentials) Credential(id string) (string, Applier) {
	if a, ok := s.oauth[id]; ok {
		return "", a
	}
	return s.secrets[id], nil
}

// failingApplier is an OAuth credential whose token cannot be produced, and
// whose error carries something that must never travel.
type failingApplier struct{ leak string }

func (a failingApplier) Apply(http.Header) error { return &leakyError{secret: a.leak} }

type leakyError struct{ secret string }

func (e *leakyError) Error() string { return "refresh failed with token " + e.secret }

// testProvider builds a provider pointed at a fake upstream.
func testProvider(t *testing.T, f *fakeUpstream, kind string, api catalog.API) *Provider {
	t.Helper()
	p, err := NewProvider(Spec{Name: "p1", Kind: kind, API: api, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

// testBackend builds a backend with one credential.
func testBackend(secret string) *Backend {
	return New(Options{
		Credentials: staticCredentials{secrets: map[string]string{"c1": secret}},
		Client:      NewClient(),
		Now:         time.Now,
	})
}

// chatCall is a minimal neutral chat request.
func chatCall(clientAPI catalog.API) *Call {
	max := 64
	return &Call{
		Op:        OpChat,
		ClientAPI: clientAPI,
		Model:     "client-model",
		Request: &canonical.Request{
			Model:     "client-model",
			Messages:  []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hello")},
			MaxTokens: &max,
		},
		DefaultMaxTokens: 1024,
	}
}

func target(p *Provider) Target {
	return Target{Provider: p, Credential: "c1", UpstreamModel: "upstream-model"}
}
