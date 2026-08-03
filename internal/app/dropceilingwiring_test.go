package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/openai"
)

// `providers[].params.drop` and `models[].deployments[].max_output_tokens`,
// asserted from YAML TEXT through the assembled gateway.
//
// Both features were built, validated and covered by consequence tests inside
// the package that owns them — internal/canonical and internal/backend for the
// drop list, internal/router for the ceiling — and neither could be reached
// from a configuration file, because internal/app never copied the parsed value
// out of config.Config. That is DESIGN §17.1's dominant class in its plainest
// form: an interface satisfied on both ends and connected on neither.
//
// The reason these are not struct-field assertions is §17.1 generalization (c).
// A test that reads router.Deployment.MaxOutputTokens back after buildRouter
// observes the value in the same place the code sets it, so it cannot fail for
// the reason the defect existed: the config value never arriving. These start
// at yaml, go in at a.Server.ServeHTTP, and read the answer off the wire — the
// body a real socket received, and the status and code a client is given.

// upstreamProbe is a fake upstream that records the request bodies it was given
// and answers a minimal chat completion.
//
// The recorded body is the whole point of the drop half: the only place
// "temperature was removed" is observable is in the bytes that left the
// process. Reading it back off canonical.Request would be reading dorang's own
// copy of the answer.
type upstreamProbe struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (p *upstreamProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.bodies = append(p.bodies, b)
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":1,"model":"m1-upstream",
		"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
		"finish_reason":"stop"}],
		"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
}

// calls reports how many requests reached the upstream. Zero is an assertion in
// its own right: the output-ceiling refusal is a REFUSAL, and a refusal that
// still spends an upstream call is a different product.
func (p *upstreamProbe) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bodies)
}

func (p *upstreamProbe) lastBody(t *testing.T) map[string]any {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.bodies) == 0 {
		t.Fatal("no request reached the upstream at all")
	}
	var m map[string]any
	if err := json.Unmarshal(p.bodies[len(p.bodies)-1], &m); err != nil {
		t.Fatalf("the upstream request is not JSON: %v\n%s", err, p.bodies[len(p.bodies)-1])
	}
	return m
}

// probeApp assembles a gateway from yamlf — a format string whose single %q is
// the fake upstream's URL — and returns the probe and an issued key.
func probeApp(t *testing.T, yamlf string) (*App, *upstreamProbe, string) {
	t.Helper()
	p := &upstreamProbe{}
	up := httptest.NewServer(p)
	t.Cleanup(up.Close)

	a := newWiringApp(t, fmt.Sprintf(yamlf, up.URL), nil,
		func(o *Options) { o.Upstream = up.Client() })
	return a, p, issueKey(t, a, nil)
}

// dropYAML names one parameter the upstream is said to reject. `temperature` is
// a MODELLED field, so a copy of the drop list that reached the encoder but not
// canonical.Request would still remove it — which is why the assertion below is
// on the encoded body rather than on the neutral request.
const dropYAML = `
version: 1
observability: {always_full_headers: true}
providers:
  - name: p1
    kind: openai
    base_url: %q
    params:
      drop: [temperature]
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`

// TestParamsDropReachesTheUpstreamFromYAML is the consequence half of
// `providers[].params.drop`.
//
// The assignment it pins is one field in a struct literal —
// backend.Spec.DropParams — and removing it leaves every test in
// internal/backend and internal/canonical green while no configuration file on
// earth can drop a parameter. The observable that separates the two states is
// the body the upstream received, so that is what this reads.
func TestParamsDropReachesTheUpstreamFromYAML(t *testing.T) {
	a, probe, key := probeApp(t, dropYAML)

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","temperature":0.9,"messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}

	body := probe.lastBody(t)
	if v, ok := body["temperature"]; ok {
		t.Errorf("the upstream received temperature=%v — providers[].params.drop names it, "+
			"and the endpoint that rejects it still got it", v)
	}
	// The removal is reported, which is the difference between this and the
	// operator editing every client they own (§10.3). A drop nobody is told
	// about is a silently different request.
	if got := w.Header().Get(server.HeaderDroppedParams); !strings.Contains(got, "temperature") {
		t.Errorf("%s = %q, want it to name temperature", server.HeaderDroppedParams, got)
	}
}

// TestParamsDropDoesNotRemoveWhatItWasNotAskedTo is the negative half, so the
// test above cannot be satisfied by an encoder that drops everything.
func TestParamsDropDoesNotRemoveWhatItWasNotAskedTo(t *testing.T) {
	a, probe, key := probeApp(t, dropYAML)

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","top_p":0.5,"messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	if _, ok := probe.lastBody(t)["top_p"]; !ok {
		t.Error("top_p did not reach the upstream, and no configuration asked for it to be dropped")
	}
}

// ceilingYAML sets an operator's output ceiling on the only deployment behind
// m1. 256 is well under what the request below asks for and is not a figure any
// catalog would supply, so a refusal can only come from the configured value.
const ceilingYAML = `
version: 1
observability: {always_full_headers: true}
providers:
  - {name: p1, kind: openai, base_url: %q}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - provider: p1
        upstream_model: m1-upstream
        credentials: [c1]
        max_output_tokens: 256
`

// TestMaxOutputTokensRefusesFromYAML is the consequence half of
// `models[].deployments[].max_output_tokens`.
//
// internal/router refuses correctly and has for as long as the field has
// existed; what did not exist was any way for an operator's number to reach it,
// so every deployment ran with the field undeclared and the ceiling was the
// catalog's DESCRIPTION rather than the operator's DECISION. The two behave
// oppositely — a description reserves and never refuses — so the state before
// the assignment is not "a weaker ceiling", it is no ceiling at all.
func TestMaxOutputTokensRefusesFromYAML(t *testing.T) {
	a, probe, key := probeApp(t, ceilingYAML)

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","max_tokens":8000,"messages":[{"role":"user","content":"go"}]}`)

	if w.Code == http.StatusOK {
		t.Fatalf("a request asking for 8000 output tokens was SERVED under "+
			"max_output_tokens: 256\n%s", w.Body.String())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	if body.Error.Code != router.CodeOutputCeiling {
		t.Fatalf("code = %q, want %q (status %d): the refusal has to say the ANSWER "+
			"asked for is too long, not that the conversation is",
			body.Error.Code, router.CodeOutputCeiling, w.Code)
	}
	// The number the caller has to get under is the operator's, and it is only
	// in the message if the operator's value actually arrived.
	if !strings.Contains(body.Error.Message, "256") {
		t.Errorf("the refusal does not name the configured ceiling: %q", body.Error.Message)
	}
	if n := probe.calls(); n != 0 {
		t.Errorf("%d request(s) reached the upstream: a refusal that still spends the "+
			"call is not a ceiling", n)
	}
}

// TestMaxOutputTokensAdmitsWhatFitsFromYAML keeps the test above from being
// satisfied by a gateway that refuses everything.
func TestMaxOutputTokensAdmitsWhatFitsFromYAML(t *testing.T) {
	a, probe, key := probeApp(t, ceilingYAML)

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","max_tokens":128,"messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("a request for 128 output tokens under a ceiling of 256 answered %d\n%s",
			w.Code, w.Body.String())
	}
	if probe.calls() != 1 {
		t.Errorf("the upstream saw %d requests, want 1", probe.calls())
	}
}

// ---------------------------------------------------------------------------
// max_tokens vs max_completion_tokens — COMPATIBILITY §5.5
// ---------------------------------------------------------------------------

// spellingYAML parameterises the two levels the choice can be written at. The
// %q is the upstream URL; %s are the provider default and the deployment
// override, each an empty string or a `max_tokens_field:` line.
const spellingYAML = `
version: 1
observability: {always_full_headers: true}
providers:
  - name: p1
    kind: openai
    base_url: %q
    params:
      %s
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - provider: p1
        upstream_model: m1-upstream
        credentials: [c1]
        %s
`

// spellingApp assembles a gateway whose provider and deployment carry the given
// `max_tokens_field` lines, sends one request with a ceiling on it, and returns
// the body the upstream received.
func spellingApp(t *testing.T, provider, deployment string) map[string]any {
	t.Helper()
	p := &upstreamProbe{}
	up := httptest.NewServer(p)
	t.Cleanup(up.Close)

	yaml := fmt.Sprintf(spellingYAML, up.URL, provider, deployment)
	a := newWiringApp(t, yaml, nil, func(o *Options) { o.Upstream = up.Client() })
	key := issueKey(t, a, nil)

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	return p.lastBody(t)
}

// TestMaxTokensFieldSelectsTheSpellingOnTheWire is COMPATIBILITY §5.5 through
// the assembled gateway, at both levels it can be written and in both values.
//
// The observable is which KEY the upstream body carries, because that is the
// entire content of the setting and there is nowhere else it shows. It is also
// where the defect this closes was measured: a token-plan endpoint given
// `max_tokens: 16` billed 116 completion tokens and reported
// `finish_reason: length`, so the response says the caller's limit was honoured
// while a larger, silently substituted ceiling did the work. There is no status,
// no header and no error to assert on — only the bytes that left.
func TestMaxTokensFieldSelectsTheSpellingOnTheWire(t *testing.T) {
	for _, c := range []struct {
		name, provider, deployment string
		want, absent               string
	}{
		{
			name: "unset anywhere keeps max_tokens",
			want: "max_tokens", absent: "max_completion_tokens",
		},
		{
			name:     "the deployment selects max_completion_tokens",
			provider: "{}", deployment: "max_tokens_field: max_completion_tokens",
			want: "max_completion_tokens", absent: "max_tokens",
		},
		{
			name:     "the provider supplies the default for its deployments",
			provider: "max_tokens_field: max_completion_tokens",
			want:     "max_completion_tokens", absent: "max_tokens",
		},
		{
			// The whole reason §5.5 says per-DEPLOYMENT: one base URL fronts
			// endpoints that disagree, so the more specific level has to win.
			name:       "the deployment overrides its provider",
			provider:   "max_tokens_field: max_completion_tokens",
			deployment: "max_tokens_field: max_tokens",
			want:       "max_tokens", absent: "max_completion_tokens",
		},
		{
			name:     "the deployment can select max_tokens explicitly",
			provider: "{}", deployment: "max_tokens_field: max_tokens",
			want: "max_tokens", absent: "max_completion_tokens",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := spellingApp(t, c.provider, c.deployment)
			v, ok := body[c.want]
			if !ok {
				t.Fatalf("the upstream request carries no %s at all: %v", c.want, body)
			}
			if n, isNum := v.(float64); !isNum || int(n) != 16 {
				t.Errorf("%s = %v, want 16 — the ceiling changed value as well as spelling",
					c.want, v)
			}
			if v, ok := body[c.absent]; ok {
				t.Errorf("the upstream request carries BOTH spellings (%s = %v): an upstream "+
					"that validates strictly refuses that, and one that does not picks for itself",
					c.absent, v)
			}
		})
	}
}

// TestMaxTokensFieldIsRefusedWhereTheShapeHasNoChoice.
//
// Anthropic's messages request spells the ceiling `max_tokens`, requires it, and
// offers no alternative, so the key can only mislead there. CONFIG §23.2's rule
// is that a key which loads and does nothing is worse than one that is refused,
// because the operator who wrote it believes it took effect — and this is
// exactly the state `providers[].params.drop` and `max_output_tokens` were both
// in until an hour ago.
func TestMaxTokensFieldIsRefusedWhereTheShapeHasNoChoice(t *testing.T) {
	isolateState(t)
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	cfg, err := config.LoadBytes([]byte(`
version: 1
providers:
  - {name: p1, kind: anthropic, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - provider: p1
        upstream_model: m1-upstream
        credentials: [c1]
        max_tokens_field: max_completion_tokens
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Storage.SQLite.Path = filepath.Join(t.TempDir(), "dorang.db")
	t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
	t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)

	a, err := New(context.Background(), Options{Config: cfg})
	if a != nil {
		t.Cleanup(func() { _ = a.Close(context.Background()) })
	}
	if err == nil {
		t.Fatal("a max_tokens_field on an anthropic-shaped deployment assembled cleanly, " +
			"so the setting loads and changes nothing")
	}
	if !strings.Contains(err.Error(), "max_tokens_field") {
		t.Errorf("the refusal does not name the key: %v", err)
	}
}

// TestMaxTokensFieldRefusesAnUnknownSpelling. Only two values exist, and a typo
// falling back to max_tokens is the silent half of the same defect: the operator
// reads their own file as saying the opposite of what the wire carries.
func TestMaxTokensFieldRefusesAnUnknownSpelling(t *testing.T) {
	_, err := config.LoadBytes([]byte(`
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - provider: p1
        upstream_model: m1-upstream
        credentials: [c1]
        max_tokens_field: max_output_tokens
`))
	if err == nil {
		t.Fatal("max_tokens_field: max_output_tokens loaded")
	}
	// The refusal has to list what does work, or the operator guesses twice.
	if !strings.Contains(err.Error(), "max_completion_tokens") {
		t.Errorf("the refusal does not name the accepted values: %v", err)
	}
}

// TestConfigMaxTokensFieldSpellingsMatchTheEncoders pins the one duplicated
// pair of literals.
//
// internal/config cannot import internal/wire/openai — it is the schema, and it
// depends on internal/canonical alone so a config file stays loadable by a tool
// that builds no wire stack — so the two spellings exist twice. This package
// imports both and is the only place they can be compared. A rename on either
// side without the other would leave `max_tokens_field` accepting a value the
// encoder does not recognise, which falls back to max_tokens in silence.
func TestConfigMaxTokensFieldSpellingsMatchTheEncoders(t *testing.T) {
	if config.MaxTokensFieldMaxTokens != openai.FieldMaxTokens {
		t.Errorf("config %q != encoder %q",
			config.MaxTokensFieldMaxTokens, openai.FieldMaxTokens)
	}
	if config.MaxTokensFieldMaxCompletionTokens != openai.FieldMaxCompletionTokens {
		t.Errorf("config %q != encoder %q",
			config.MaxTokensFieldMaxCompletionTokens, openai.FieldMaxCompletionTokens)
	}
}

// TestMaxTokensFieldOnAnAlibabaShapedProvider is the operator's own shape: a
// `kind: alibaba` provider with an overridden `base_url`, which is what a qwen
// token plan looks like in a real file.
//
// It exists because the key carries a START-UP refusal for wire shapes that have
// no such choice, and a refusal that fired on the deployment the key was written
// for would be worse than no key at all. `alibaba` resolves to `openai-chat`
// through pkg/catalog, so it is admitted — and this asserts that through the
// same assembly a `dorang serve` performs, rather than by reading the catalog
// file and agreeing with it.
func TestMaxTokensFieldOnAnAlibabaShapedProvider(t *testing.T) {
	p := &upstreamProbe{}
	up := httptest.NewServer(p)
	t.Cleanup(up.Close)

	a := newWiringApp(t, fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
providers:
  - name: qwen-token-plan
    kind: alibaba
    base_url: %q
    params:
      max_tokens_field: max_completion_tokens
credentials:
  - {id: c1, provider: qwen-token-plan, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: qwen-token-plan, upstream_model: m1-upstream, credentials: [c1]}
`, up.URL), nil, func(o *Options) { o.Upstream = up.Client() })

	key := issueKey(t, a, nil)
	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	body := p.lastBody(t)
	if _, ok := body["max_completion_tokens"]; !ok {
		t.Errorf("the plan received %v, and the whole reason the key exists is that this "+
			"endpoint ignores max_tokens and substitutes a larger ceiling — 116 completion "+
			"tokens billed against a request for 16", body)
	}
	if _, ok := body["max_tokens"]; ok {
		t.Error("max_tokens is still on the request")
	}
}
