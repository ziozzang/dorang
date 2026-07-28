package backend

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// TestEndpointDerivation covers the rule that has to hold for every catalogued
// kind at once: a base URL arrives as a bare host, as a host already carrying
// its version segment, or as a host plus a path with no version at all, and all
// three are correct. dorang supplies the version unless the base already
// contains one.
func TestEndpointDerivation(t *testing.T) {
	cases := []struct {
		name string
		kind string
		api  catalog.API
		base string
		op   Operation
		want string
	}{
		{"bare host gets the version", "openai", catalog.APIOpenAIChat,
			"https://api.example.com", OpChat, "https://api.example.com/v1/chat/completions"},
		{"versioned host keeps its own", "openai", catalog.APIOpenAIChat,
			"https://api.example.com/v1", OpChat, "https://api.example.com/v1/chat/completions"},
		{"a non-v1 version segment survives", "glm", catalog.APIOpenAIChat,
			"https://api.z.ai/api/coding/paas/v4", OpChat,
			"https://api.z.ai/api/coding/paas/v4/chat/completions"},
		{"a trailing slash does not double", "openai", catalog.APIOpenAIChat,
			"https://api.example.com/v1/", OpChat, "https://api.example.com/v1/chat/completions"},
		{"a base that already names the route", "openai", catalog.APIOpenAIChat,
			"https://api.example.com/v1/chat/completions", OpChat,
			"https://api.example.com/v1/chat/completions"},
		{"embeddings", "openai", catalog.APIOpenAIChat,
			"https://api.example.com", OpEmbeddings, "https://api.example.com/v1/embeddings"},
		{"messages", "anthropic", catalog.APIAnthropicMessages,
			"https://api.anthropic.com", OpChat, "https://api.anthropic.com/v1/messages"},
		{"count tokens", "anthropic", catalog.APIAnthropicMessages,
			"https://api.anthropic.com", OpCountTokens,
			"https://api.anthropic.com/v1/messages/count_tokens"},
		// D1. A path prefix with NO version segment gets one. Both of the
		// Anthropic-compatible coding plans this project exists to aggregate hand
		// the operator exactly this shape — it is what a Claude-Code-shaped client
		// puts in ANTHROPIC_BASE_URL, and that client appends "/v1/messages"
		// itself — and dorang used to append nothing and address a route that does
		// not exist.
		{"a vendor path prefix keeps its version", "minimax", catalog.APIAnthropicMessages,
			"https://api.minimax.io/anthropic", OpChat, "https://api.minimax.io/anthropic/v1/messages"},
		{"an anthropic coding plan on a path", "anthropic", catalog.APIAnthropicMessages,
			"https://api.z.ai.invalid/api/anthropic", OpChat,
			"https://api.z.ai.invalid/api/anthropic/v1/messages"},
		{"an anthropic coding plan on a different path", "anthropic", catalog.APIAnthropicMessages,
			"https://dashscope.invalid/apps/anthropic", OpChat,
			"https://dashscope.invalid/apps/anthropic/v1/messages"},
		{"count_tokens on a path base", "anthropic", catalog.APIAnthropicMessages,
			"https://api.z.ai.invalid/api/anthropic", OpCountTokens,
			"https://api.z.ai.invalid/api/anthropic/v1/messages/count_tokens"},
		// An operator who already wrote the version segment must not get a second
		// one, whichever spelling the vendor uses.
		{"an operator-supplied version is not doubled", "anthropic", catalog.APIAnthropicMessages,
			"https://api.z.ai.invalid/api/anthropic/v1", OpChat,
			"https://api.z.ai.invalid/api/anthropic/v1/messages"},
		// The version is not always the LAST segment. deepinfra's catalogued base
		// is /v1/openai and its route is /v1/openai/chat/completions; a rule keyed
		// on the final segment would address /v1/openai/v1/chat/completions.
		{"a version that is not the last segment", "deepinfra", catalog.APIOpenAIChat,
			"https://api.deepinfra.com/v1/openai", OpChat,
			"https://api.deepinfra.com/v1/openai/chat/completions"},
		{"a non-numeric v-word is not a version", "openai", catalog.APIOpenAIChat,
			"https://api.venice.ai/api", OpChat, "https://api.venice.ai/api/v1/chat/completions"},
		// The two rerank spellings side by side. The vendor's own route lives at
		// /v2 on a bare host, so the "/v1" every other OpenAI-shaped route takes
		// would address /v1/v2/rerank, which 404s.
		{"self-hosted rerank is an ordinary v1 route", "vllm", catalog.APIOpenAIChat,
			"https://engine.example.test", OpRerank, "https://engine.example.test/v1/rerank"},
		{"cohere versions per route", "cohere", catalog.APICohere,
			"https://api.cohere.com", OpRerank, "https://api.cohere.com/v2/rerank"},
		{"cohere base already at v2", "cohere", catalog.APICohere,
			"https://api.cohere.com/v2", OpRerank, "https://api.cohere.com/v2/rerank"},
		{"jina rerank", "jina", catalog.APIJina,
			"https://api.jina.ai", OpRerank, "https://api.jina.ai/v1/rerank"},
		{"jina embeddings", "jina", catalog.APIJina,
			"https://api.jina.ai/v1", OpEmbeddings, "https://api.jina.ai/v1/embeddings"},
		{"gemini puts the model in the path", "google", catalog.APIGemini,
			"https://generativelanguage.googleapis.com/v1beta", OpChat,
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:generateContent"},
		{"gemini bare host", "google", catalog.APIGemini,
			"https://generativelanguage.googleapis.com", OpChat,
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:generateContent"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewProvider(Spec{Name: "p", Kind: tc.kind, API: tc.api, BaseURL: tc.base})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			got, err := p.Endpoint(tc.op, "gemini-2.5-pro", false)
			if err != nil {
				t.Fatalf("Endpoint: %v", err)
			}
			if got != tc.want {
				t.Errorf("Endpoint = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCatalogueBasesThatGainAVersion pins the blast radius of D1's rule change,
// by name, for every catalogued kind it moves.
//
// The rule it replaced supplied the version segment only for a BARE HOST, and
// its doc comment claimed that "leaves every catalogued kind pointing at a real
// endpoint". That claim was false for five kinds, three of which are the
// Anthropic-compatible coding plans this project exists to aggregate: dorang
// addressed `<base>/messages` where the route is `<base>/v1/messages`.
//
// Two of the five are OpenAI-shaped and are the judgement call in this fix. The
// catalog records their base URLs from a plugin manifest consumed by an SDK that
// appends `/chat/completions` with no version of its own, so a reading exists
// under which they wanted nothing appended. They are listed here so that reading
// is one edit away and cannot be lost: an operator who needs the unversioned
// route writes the full route as `base_url`, which [joinVersioned] returns
// verbatim.
func TestCatalogueBasesThatGainAVersion(t *testing.T) {
	// kind -> the endpoint dorang now addresses for that kind's own operation.
	want := map[string]string{
		// Anthropic-shaped coding plans. A Claude-Code-shaped client is handed
		// exactly this base and appends "/v1/messages" itself, which is why the
		// vendor documents the base without a version.
		"minimax":     "https://api.minimax.io/anthropic/v1/messages",
		"kimi-coding": "https://api.kimi.com/coding/v1/messages",
		"synthetic":   "https://api.synthetic.new/anthropic/v1/messages",
		// OpenAI-shaped, and the judgement call described above.
		"longcat":  "https://api.longcat.chat/openai/v1/chat/completions",
		"kilocode": "https://api.kilo.ai/api/gateway/v1/chat/completions",
	}

	cat := catalog.Default()
	got := make(map[string]string)
	for _, name := range cat.Kinds() {
		kd, ok := cat.Kind(name)
		if !ok || kd.BaseURL == "" {
			continue
		}
		// The two filters are spelled out here rather than borrowed from
		// endpoint.go on purpose: a test that selects its own subjects with the
		// function under test stops selecting them the moment that function
		// regresses, and reports an empty set instead of a wrong URL.
		if trimBase(kd.BaseURL) == baseHost(t, kd.BaseURL) {
			continue // a bare host: it got the version under the old rule too
		}
		if pathNamesAVersion(t, kd.BaseURL) {
			continue // already versioned: nothing was ever appended
		}
		p, err := NewProvider(Spec{Name: "p", Kind: name, API: kd.API, BaseURL: kd.BaseURL})
		if err != nil {
			t.Fatalf("%s: NewProvider: %v", name, err)
		}
		ep, err := p.Endpoint(OpChat, "m", false)
		if err != nil {
			t.Fatalf("%s: Endpoint: %v", name, err)
		}
		got[name] = ep
	}

	for name, url := range want {
		if got[name] != url {
			t.Errorf("kind %q addresses %q, want %q", name, got[name], url)
		}
		delete(got, name)
	}
	for name, url := range got {
		t.Errorf("kind %q now addresses %q and is not in this test's list; a catalogued base "+
			"whose route moves has to be named here, not discovered in production", name, url)
	}
}

// baseHost returns the scheme and host of a base URL, with no path.
func baseHost(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %q: %v", base, err)
	}
	return u.Scheme + "://" + u.Host
}

// pathNamesAVersion is the test's own reading of "this base is already
// versioned". See the comment at its call site for why it is not
// [hasVersionSegment].
func pathNamesAVersion(t *testing.T, base string) bool {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %q: %v", base, err)
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if len(seg) >= 2 && seg[0] == 'v' && seg[1] >= '0' && seg[1] <= '9' {
			return true
		}
	}
	return false
}

func TestGeminiStreamEndpoint(t *testing.T) {
	p, err := NewProvider(Spec{Name: "g", Kind: "google", API: catalog.APIGemini,
		BaseURL: "https://generativelanguage.googleapis.com/v1beta"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Endpoint(OpChat, "gemini-2.5-flash", true)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse"
	if got != want {
		t.Errorf("streaming endpoint = %q, want %q", got, want)
	}
}

// TestModelNamesAreEscapedInThePath: a model name is opaque (§2.1) and may hold
// characters a path segment reserves. Nothing splits it, and nothing may let it
// alter the URL's structure.
func TestModelNamesAreEscapedInThePath(t *testing.T) {
	p, err := NewProvider(Spec{Name: "g", Kind: "google", API: catalog.APIGemini,
		BaseURL: "https://example.com/v1beta"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Endpoint(OpChat, "../../admin/models/evil", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com/v1beta/models/..%2F..%2Fadmin%2Fmodels%2Fevil:generateContent" {
		t.Errorf("endpoint = %q; a model name must not be able to traverse the path", got)
	}
}

func TestProviderRequiresABaseURL(t *testing.T) {
	if _, err := NewProvider(Spec{Name: "p", Kind: "openai", API: catalog.APIOpenAIChat}); err == nil {
		t.Fatal("a provider with no endpoint must fail at construction, not on the first request")
	} else if !errors.Is(err, ErrNoBaseURL) {
		t.Errorf("err = %v, want ErrNoBaseURL", err)
	}
}

// TestProviderTimeoutIsApplied: providers[].timeout is configuration an operator
// wrote down, and a timeout that is parsed and never applied is a promise the
// gateway does not keep.
func TestProviderTimeoutIsApplied(t *testing.T) {
	f := newFakeUpstream(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})

	p, err := NewProvider(Spec{Name: "p", Kind: "openai", API: catalog.APIOpenAIChat,
		BaseURL: f.srv.URL, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := testBackend("k").Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil)
	if res.Err == nil {
		t.Fatal("want a timeout")
	}
	if !res.Timeout {
		t.Errorf("Timeout = false for %+v; the router cannot classify a deadline without it", res.Err)
	}
	if res.Err.Status != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", res.Err.Status)
	}
	if res.Err.Code != CodeTimeout {
		t.Errorf("code = %q, want %q", res.Err.Code, CodeTimeout)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the request ran for %v; the provider timeout was not applied", elapsed)
	}
}

// TestRetryOnlyRepeatsAConnectionThatWasNeverMade.
//
// A chat completion is not idempotent and carries no idempotency key, so an
// attempt that MAY have been executed is never repeated: the caller would be
// billed twice for an answer they receive once, and no response says which
// happened.
func TestRetryOnlyRepeatsAConnectionThatWasNeverMade(t *testing.T) {
	t.Run("a 500 is not repeated here", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.answer(http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
		p, err := NewProvider(Spec{Name: "p", Kind: "openai", API: catalog.APIOpenAIChat,
			BaseURL: f.srv.URL, Retry: Policy{MaxAttempts: 3, Backoff: BackoffConstant}})
		if err != nil {
			t.Fatal(err)
		}
		res := testBackend("k").Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil)
		if res.Err == nil {
			t.Fatal("want an error")
		}
		if f.count() != 1 {
			t.Errorf("the upstream saw %d requests; a POST that may have been executed must not "+
				"be repeated — the fallback chain owns this decision", f.count())
		}
		if !res.Retryable {
			t.Error("a 5xx must still be offered to the fallback chain")
		}
	})

	t.Run("a refused connection is repeated", func(t *testing.T) {
		p, err := NewProvider(Spec{Name: "p", Kind: "openai", API: catalog.APIOpenAIChat,
			BaseURL: "http://127.0.0.1:1", Retry: Policy{MaxAttempts: 3}})
		if err != nil {
			t.Fatal(err)
		}
		res := testBackend("k").Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil)
		if res.Err == nil {
			t.Fatal("want an error")
		}
		if res.Attempts != 3 {
			t.Errorf("attempts = %d, want 3: a connection that was never established is the one "+
				"failure that provably never reached the model", res.Attempts)
		}
		if !res.Transport {
			t.Error("Transport must be set when no HTTP response arrived")
		}
	})
}

func TestPolicyBackoff(t *testing.T) {
	p := Policy{Base: 100 * time.Millisecond, Backoff: BackoffExponential}
	for n, want := range map[int]time.Duration{1: 100, 2: 200, 3: 400} {
		if got := p.wait(n); got != want*time.Millisecond {
			t.Errorf("exponential wait(%d) = %v, want %v", n, got, want*time.Millisecond)
		}
	}
	if got := (Policy{Base: time.Second, Backoff: BackoffConstant}).wait(4); got != time.Second {
		t.Errorf("constant wait = %v", got)
	}
	if got := (Policy{Base: time.Second, Backoff: BackoffLinear}).wait(3); got != 3*time.Second {
		t.Errorf("linear wait = %v", got)
	}
	if got := (Policy{Backoff: BackoffExponential}).wait(1); got != 0 {
		t.Errorf("a zero base must not sleep, got %v", got)
	}
	// A long chain must not overflow into a negative duration.
	if got := (Policy{Base: time.Second, Backoff: BackoffExponential}).wait(64); got <= 0 {
		t.Errorf("wait(64) = %v, want a positive bound", got)
	}
}

// TestNoCredentialIsNotAnError: an unauthenticated deployment is ordinary — a
// vLLM on a private network is the common case — and manufacturing a 401 for one
// would refuse a request the upstream would have served.
func TestNoCredentialIsNotAnError(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"1","object":"chat.completion","model":"m","choices":[]}`)
	p := testProvider(t, f, "vllm", catalog.APIOpenAIChat)

	be := New(Options{Credentials: staticCredentials{}})
	tg := target(p)
	tg.Credential = ""
	if res := be.Do(context.Background(), tg, chatCall(catalog.APIOpenAIChat), nil); res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if got := f.last().header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want nothing sent", got)
	}
}
