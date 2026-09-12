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

// azureHost answers the chat route in OpenAI's shape and records how it was
// addressed: path, query and the two credential headers.
type azureHost struct {
	mu   sync.Mutex
	path string
	q    string
	key  string
	auth string
}

func serveAzure(t *testing.T, f *fakeUpstream) *azureHost {
	t.Helper()
	h := &azureHost{}
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.path, h.q, h.key, h.auth = r.URL.Path, r.URL.RawQuery, r.Header.Get("api-key"), r.Header.Get("Authorization")
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	})
	return h
}

func azureProvider(t *testing.T, f *fakeUpstream, apiVersion string) *Provider {
	t.Helper()
	p, err := NewProvider(Spec{Name: "az", Kind: "azure", API: catalog.APIAzureOpenAI,
		BaseURL: f.srv.URL, AzureAPIVersion: apiVersion})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

// An Azure resource is addressed where Azure serves the OpenAI shape, with the
// credential spelled the way Azure reads it.
//
// The kind used to be refused outright ("no adapter in this build"). What it
// needs is exactly two things beside the OpenAI adapter: the route — the
// unified /openai/v1 mount by default, the legacy per-deployment path with its
// mandatory api-version when the operator names one — and the api-key header.
// Asserted on what the host received, not on the adapter chosen.
func TestAnAzureResourceIsAddressedWhereAzureServesTheOpenAIShape(t *testing.T) {
	t.Run("unified surface by default", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveAzure(t, f)
		b := testBackend("sk-test")
		res := b.Do(context.Background(), target(azureProvider(t, f, "")), chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		if host.path != "/openai/v1/chat/completions" || host.q != "" {
			t.Errorf("host saw %s?%s, want the unified route with no query", host.path, host.q)
		}
		if host.key != "sk-test" || host.auth != "" {
			t.Errorf("api-key=%q authorization=%q: the static key goes in the header Azure documents, and nowhere else", host.key, host.auth)
		}
		if !strings.Contains(string(res.Body), `"ok"`) {
			t.Errorf("body:\n%s", res.Body)
		}
	})

	t.Run("legacy surface when api_version is set", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveAzure(t, f)
		b := testBackend("sk-test")
		res := b.Do(context.Background(), target(azureProvider(t, f, "2024-10-21")), chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		if host.path != "/openai/deployments/upstream-model/chat/completions" {
			t.Errorf("host saw %q, want the deployment in the path", host.path)
		}
		if host.q != "api-version=2024-10-21" {
			t.Errorf("query = %q, want the mandatory api-version", host.q)
		}
	})

	t.Run("a base that already carries the mount is not doubled", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveAzure(t, f)
		p, err := NewProvider(Spec{Name: "az", Kind: "azure", API: catalog.APIAzureOpenAI, BaseURL: f.srv.URL + "/openai/v1"})
		if err != nil {
			t.Fatal(err)
		}
		b := testBackend("sk-test")
		if res := b.Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), httptest.NewRecorder()); res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		if host.path != "/openai/v1/chat/completions" {
			t.Errorf("host saw %q", host.path)
		}
	})

	t.Run("api_version is refused off the azure kind", func(t *testing.T) {
		_, err := NewProvider(Spec{Kind: "openai", API: catalog.APIOpenAIChat, BaseURL: "https://example.invalid", AzureAPIVersion: "2024-10-21"})
		if err == nil || !strings.Contains(err.Error(), "api_version") {
			t.Errorf("a setting only the Azure adapter reads was accepted on an openai provider: %v", err)
		}
	})
}
