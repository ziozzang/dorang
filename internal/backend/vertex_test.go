package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ziozzang/dorang/pkg/catalog"
)

type vertexHostRec struct {
	mu                 sync.Mutex
	path, q, key, auth string
}

func serveVertex(t *testing.T, f *fakeUpstream) *vertexHostRec {
	t.Helper()
	h := &vertexHostRec{}
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.path, h.q, h.key, h.auth = r.URL.Path, r.URL.RawQuery, r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization")
		h.mu.Unlock()
		if strings.Contains(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1}}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1}}`))
	})
	return h
}

func vertexProvider(t *testing.T, f *fakeUpstream) *Provider {
	t.Helper()
	p, err := NewProvider(Spec{Name: "vx", Kind: "vertex", API: catalog.APIVertex, BaseURL: f.srv.URL,
		VertexProject: "proj", VertexLocation: "us-central1"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

type bearerApplier struct{ tok string }

func (a bearerApplier) Apply(h http.Header) error {
	h.Set("Authorization", "Bearer "+a.tok)
	return nil
}

// Gemini on Vertex is addressed under the project and location, with the
// credential Google reads there.
func TestVertexIsAddressedUnderTheProjectAndLocation(t *testing.T) {
	const want = "/v1/projects/proj/locations/us-central1/publishers/google/models/upstream-model"

	t.Run("static key as x-goog-api-key", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveVertex(t, f)
		b := testBackend("sk-test")
		res := b.Do(context.Background(), target(vertexProvider(t, f)), chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		if host.path != want+":generateContent" || host.q != "" {
			t.Errorf("host saw %s?%s", host.path, host.q)
		}
		if host.key != "sk-test" || host.auth != "" {
			t.Errorf("x-goog-api-key=%q authorization=%q", host.key, host.auth)
		}
		if !strings.Contains(string(res.Body), `"ok"`) {
			t.Errorf("body:\n%s", res.Body)
		}
	})

	t.Run("oauth bearer from a service account", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveVertex(t, f)
		b := New(Options{Credentials: staticCredentials{oauth: map[string]Applier{"c1": bearerApplier{"ya29.minted"}}}, Client: NewClient()})
		res := b.Do(context.Background(), target(vertexProvider(t, f)), chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		if host.auth != "Bearer ya29.minted" || host.key != "" {
			t.Errorf("authorization=%q x-goog-api-key=%q", host.auth, host.key)
		}
	})

	t.Run("streaming takes the streaming method with alt=sse", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveVertex(t, f)
		b := testBackend("sk-test")
		c := chatCall(catalog.APIOpenAIChat)
		c.Stream, c.Request.Stream = true, true
		rec := httptest.NewRecorder()
		res := b.Do(context.Background(), target(vertexProvider(t, f)), c, rec)
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		if host.path != want+":streamGenerateContent" || host.q != "alt=sse" {
			t.Errorf("host saw %s?%s", host.path, host.q)
		}
		if !strings.Contains(rec.Body.String(), "ok") {
			t.Errorf("client stream:\n%s", rec.Body.String())
		}
	})

	t.Run("the location names the host when no base_url is given", func(t *testing.T) {
		p, err := NewProvider(Spec{Kind: "vertex", API: catalog.APIVertex, VertexProject: "proj", VertexLocation: "europe-west4"})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		if p.BaseURL() != "https://europe-west4-aiplatform.googleapis.com" {
			t.Errorf("base = %q", p.BaseURL())
		}
		g, err := NewProvider(Spec{Kind: "vertex", API: catalog.APIVertex, VertexProject: "proj", VertexLocation: "global"})
		if err != nil || g.BaseURL() != "https://aiplatform.googleapis.com" {
			t.Errorf("global base = %q %v", g.BaseURL(), err)
		}
	})

	t.Run("project and location are required, and refused elsewhere", func(t *testing.T) {
		if _, err := NewProvider(Spec{Kind: "vertex", API: catalog.APIVertex, BaseURL: "https://example.invalid", VertexProject: "proj"}); err == nil {
			t.Error("a vertex provider without a location was accepted; its route cannot be built")
		}
		if _, err := NewProvider(Spec{Kind: "openai", API: catalog.APIOpenAIChat, BaseURL: "https://example.invalid", VertexProject: "proj"}); err == nil || !strings.Contains(err.Error(), "project") {
			t.Errorf("params.project on an openai provider was accepted: %v", err)
		}
	})
}
