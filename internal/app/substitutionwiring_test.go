package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ziozzang/dorang/internal/metrics"
	"github.com/ziozzang/dorang/internal/server"
)

// A 200 that was served by a different model, asserted through the assembled
// gateway.
//
// DESIGN §17.1 says why this test has to be here rather than in the package
// that owns the comparison. The check is spread over four layers by
// construction — internal/wire reads the name out of the answer,
// internal/backend classifies it, internal/router decides whether the catalog
// can prove anything, and internal/app is the only one holding the catalog to
// finish with — so there is no package whose own tests can fail if any one link
// is not connected. That is §17.1's dominant defect class exactly: an interface
// satisfied on both ends and connected on neither.
//
// It also satisfies §17.1 generalization (c). The two values compared are read
// in genuinely different places — `upstream_model` comes out of the YAML text
// below, and the served name comes off a socket — so the assertion can fail for
// the reason the defect exists.

// substProbe answers a chat completion naming whatever model it is told to.
type substProbe struct {
	mu    sync.Mutex
	model string
	calls int
}

func (p *substProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	p.mu.Lock()
	m := p.model
	p.calls++
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":%q,
		"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
		"finish_reason":"stop"}],
		"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`, m)
}

// substYAML routes `m1` at a z.ai-shaped deployment asking for `glm-5.1`, which
// is a model pkg/catalog holds figures for. `%s` is the fake upstream's URL.
const substYAML = `
version: 1
observability: {always_full_headers: true}
providers:
  - {name: zai, kind: glm, base_url: %q}
credentials:
  - {id: c1, provider: zai, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: zai, upstream_model: "glm-5.1", credentials: [c1]}
`

// substLocalYAML is the operator's own alias for a self-hosted OpenAI-shaped
// server. `local` is not a catalog entry and neither is anything the server
// answers with, which is the case that must stay silent.
const substLocalYAML = `
version: 1
observability: {always_full_headers: true}
providers:
  - {name: box, kind: openai, base_url: %q}
credentials:
  - {id: c1, provider: box, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: box, upstream_model: "local", credentials: [c1]}
`

func substApp(t *testing.T, yamlf, answersAs string) (*App, *substProbe, string) {
	t.Helper()
	p := &substProbe{model: answersAs}
	up := httptest.NewServer(p)
	t.Cleanup(up.Close)

	a := newWiringApp(t, fmt.Sprintf(yamlf, up.URL), nil,
		func(o *Options) { o.Upstream = up.Client() })
	return a, p, issueKey(t, a, nil)
}

// The measured failure, end to end: dorang sends `glm-5.1`, the endpoint
// answers 200 with `"model":"glm-5.2"`, and the client is told it got `m1`.
func TestSubstitutedModelIsCountedAndReported(t *testing.T) {
	a, _, key := substApp(t, substYAML, "glm-5.2")

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the substitution arrives as a success\n%s",
			w.Code, w.Body.String())
	}

	// 1. The caller's contract. §7.2 puts the client's own name in the body, so
	//    the header is the only place the disagreement survives to the caller.
	if got := w.Header().Get(server.HeaderServedModel); got != "glm-5.2" {
		t.Errorf("%s = %q, want %q", server.HeaderServedModel, got, "glm-5.2")
	}
	if got := w.Header().Get(server.HeaderUpstreamModel); got != "glm-5.1" {
		t.Errorf("%s = %q, want %q — the two headers are the disagreement",
			server.HeaderUpstreamModel, got, "glm-5.1")
	}

	// 2. The counter an operator alerts on.
	if n := substitutionCount(t, a, "glm-5.2"); n != 1 {
		t.Errorf("dorang_model_substitutions_total{served=\"glm-5.2\"} = %d, want 1", n)
	}

	// 3. Detection is not policy. Nothing rerouted, nothing was refused, and
	//    the answer still went to the client.
	if !strings.Contains(w.Body.String(), `"model":"m1"`) {
		t.Errorf("the client body does not carry the requested name: %s", w.Body.String())
	}
}

// The same deployment answering honestly emits nothing at all. The family is
// absent rather than zero, which is what makes its presence a signal.
func TestHonestAnswerEmitsNoSubstitutionSeries(t *testing.T) {
	a, _, key := substApp(t, substYAML, "glm-5.1")

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(server.HeaderServedModel); got != "" {
		t.Errorf("%s = %q on an honest answer, want absent", server.HeaderServedModel, got)
	}
	if n := substitutionCount(t, a, ""); n != 0 {
		t.Errorf("dorang_model_substitutions_total has %d samples on honest traffic, want 0", n)
	}
}

// Case B from the operator's live configuration, and the one that kills any
// purely textual rule: `local` is the operator's own alias and the server
// resolved it to the gguf it had loaded. The two names share nothing — not a
// prefix, not a token, not a substring — and this is entirely correct
// behaviour. Firing here would mean firing on every request that deployment
// serves, which is how a control gets switched off.
func TestAnAliasResolvedByTheUpstreamIsNotASubstitution(t *testing.T) {
	a, _, key := substApp(t, substLocalYAML,
		"Gemma-4-Garnet-V2-31B-it-ultra-uncensored-heretic.i1-Q6_K.gguf")

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(server.HeaderServedModel); got != "" {
		t.Errorf("%s = %q, want absent: neither name is a model this catalog has "+
			"figures for, so nothing was mispriced against anything",
			server.HeaderServedModel, got)
	}
	if n := substitutionCount(t, a, ""); n != 0 {
		t.Errorf("dorang_model_substitutions_total has %d samples, want 0", n)
	}
}

// Case A from the same configuration: OpenRouter drops the `:free` suffix and
// adds a `private/openrouter/` prefix, so the returned name is neither a
// prefix, a suffix nor a superstring of what was asked.
func TestADecoratedNameIsNotASubstitution(t *testing.T) {
	const yamlf = `
version: 1
observability: {always_full_headers: true}
providers:
  - {name: orouter, kind: openai, base_url: %q}
credentials:
  - {id: c1, provider: orouter, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: orouter, upstream_model: "nvidia/llama-nemotron-embed-vl-1b-v2:free", credentials: [c1]}
`
	a, _, key := substApp(t, yamlf, "private/openrouter/nvidia/llama-nemotron-embed-vl-1b-v2")

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(server.HeaderServedModel); got != "" {
		t.Errorf("%s = %q, want absent — a decorated name is not a different model",
			server.HeaderServedModel, got)
	}
}

// The case the app-side gate exists for, and the only one that reaches it.
//
// Here the ASKED name IS a catalog entry, so internal/backend arms the reading
// and copies the returned name out — and the returned name is not an entry at
// all. Nothing was mispriced against anything, because dorang holds no second
// set of figures it should have used instead, so the correct answer is silence.
//
// Without this test the app-side catalog lookup is a control nothing reaches:
// the two cases from the operator's live configuration are both stopped one
// layer earlier, by [router.Decision.ModelKnown], because neither of THEIR
// asked names is an entry either.
func TestAnUnknownReturnedNameIsNotASubstitution(t *testing.T) {
	a, _, key := substApp(t, substYAML, "glm-9-experimental-not-in-any-catalog")

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(server.HeaderServedModel); got != "" {
		t.Errorf("%s = %q, want absent: the returned name is not a model this catalog "+
			"has figures for, so no figure can be shown to have been wrong",
			server.HeaderServedModel, got)
	}
	if n := substitutionCount(t, a, ""); n != 0 {
		t.Errorf("dorang_model_substitutions_total has %d samples, want 0", n)
	}
}

// A variant is chosen, not stamped, so `glm-4.5` answered by `glm-4.5-air` is a
// different model — and both are entries this catalog holds separate figures
// for, which is precisely the harm.
func TestAVariantIsASubstitution(t *testing.T) {
	const yamlf = `
version: 1
observability: {always_full_headers: true}
providers:
  - {name: zai, kind: glm, base_url: %q}
credentials:
  - {id: c1, provider: zai, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: zai, upstream_model: "glm-4.5", credentials: [c1]}
`
	a, _, key := substApp(t, yamlf, "glm-4.5-air")

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(server.HeaderServedModel); got != "glm-4.5-air" {
		t.Errorf("%s = %q, want %q", server.HeaderServedModel, got, "glm-4.5-air")
	}
}

// The line is deduplicated and the COUNT is not, which is the whole division of
// labour between them.
//
// The measured endpoint substitutes on every request, so a line per occurrence
// would be a line per request for the life of the deployment — a real signal
// filtered out of a log pipeline and then out of an operator's attention. A
// counter that stopped at one would be worse: it would say a substitution
// happened once when it is happening continuously.
func TestRepeatedSubstitutionsAreCountedButLoggedOnce(t *testing.T) {
	a, _, key := substApp(t, substYAML, "glm-5.2")

	for i := 0; i < 4; i++ {
		w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
			`{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d\n%s", i, w.Code, w.Body.String())
		}
		if got := w.Header().Get(server.HeaderServedModel); got != "glm-5.2" {
			t.Errorf("request %d: %s = %q, want it on EVERY response",
				i, server.HeaderServedModel, got)
		}
	}
	if n := substitutionCount(t, a, "glm-5.2"); n != 4 {
		t.Errorf("dorang_model_substitutions_total = %d, want 4", n)
	}
	if !a.dispatch.subs.first("other-deployment", "glm-5.2") {
		t.Error("a different deployment is suppressed by the first one's line")
	}
	if a.dispatch.subs.first("zai-m1-1", "glm-5.2") == a.dispatch.subs.first("zai-m1-1", "glm-5.2") {
		t.Error("substitutionLog.first is not idempotent on the second call")
	}
}

func TestSubstitutionLogIsBounded(t *testing.T) {
	var s substitutionLog
	for i := 0; i < maxLoggedSubstitutions; i++ {
		if !s.first("d"+strconv.Itoa(i), "served") {
			t.Fatalf("pair %d was suppressed below the cap", i)
		}
	}
	if s.first("one-too-many", "served") {
		t.Error("the log set grew past its cap; a set bounded only by what an upstream " +
			"chooses to say is not bounded")
	}
	// Past the cap the COUNTER still advances — only the line stops.
	if len(s.seen) != maxLoggedSubstitutions {
		t.Errorf("%d entries retained, want %d", len(s.seen), maxLoggedSubstitutions)
	}
}

// The streaming half, through the assembled gateway.
//
// A stream is where the substitution is hardest to see and where the counter is
// the ONLY surface that can carry it: the header block closes on the first
// write, and on a stream the first write is the very frame the reading is taken
// from. So this asserts the counter, and asserts the absence of the header as
// the documented consequence rather than as an oversight.
func TestSubstitutedModelIsCountedOnAStream(t *testing.T) {
	p := &substStreamProbe{model: "glm-5.2"}
	up := httptest.NewServer(p)
	t.Cleanup(up.Close)
	a := newWiringApp(t, fmt.Sprintf(substYAML, up.URL), nil,
		func(o *Options) { o.Upstream = up.Client() })
	key := issueKey(t, a, nil)

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","stream":true,"messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	if n := substitutionCount(t, a, "glm-5.2"); n != 1 {
		t.Errorf("dorang_model_substitutions_total{served=\"glm-5.2\"} = %d, want 1 — a "+
			"stream is the case the header cannot cover, so the counter has to", n)
	}
	// §7.2 rewrote every frame on the way out, which is exactly why the count
	// is the only record: the bytes the client received name `m1` throughout.
	if body := w.Body.String(); strings.Contains(body, "glm-5.2") {
		t.Errorf("the relayed stream still names the upstream model: %s", body)
	}
}

type substStreamProbe struct {
	mu    sync.Mutex
	model string
}

func (p *substStreamProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	p.mu.Lock()
	m := p.model
	p.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,"model":%q,`+
		`"choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n", m)
	fmt.Fprintf(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,"model":%q,`+
		`"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`+"\n\n", m)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// substitutionCount reads the assembled scrape. served == "" counts every
// sample of the family, which is how "the family is absent" is asserted.
func substitutionCount(t *testing.T, a *App, served string) int64 {
	t.Helper()
	fams, err := metrics.Parse(a.Metrics.Metrics(nil))
	if err != nil {
		t.Fatalf("the assembled scrape does not parse: %v", err)
	}
	var n int64
	for _, f := range fams {
		if f.Name != "dorang_model_substitutions_total" {
			continue
		}
		for _, s := range f.Samples {
			if served == "" || s.Labels["served"] == served {
				n += int64(s.Value)
			}
		}
	}
	return n
}
