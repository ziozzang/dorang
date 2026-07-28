package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/mask"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/wire/openai"
)

// The end-to-end half of DESIGN §10.5b. Every assertion here is outside the
// package that owns the check: what the *upstream* received, what the *client*
// got back, what is in the database file, what a /metrics scrape says.

// theRRN is the identity number these tests follow through the gateway. If it
// appears anywhere it should not, the test that finds it says so by name.
const theRRN = "900101-1234567"

const theEmail = "hong@example.com"

// pluginPath is the shipped worked example. Pointing the tests at the real file
// means the example cannot rot: a change to the plugin surface that breaks it
// breaks the suite.
func pluginPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../deploy/plugins/pii_mask.lua")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the shipped plugin is missing: %v", err)
	}
	return p
}

func filterYAML(t *testing.T, upstream string) string {
	t.Helper()
	return fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
filters:
  secret: {key_env: DORANG_APP_TEST_FILTER_SECRET}
  plugins:
    - name: pii-mask
      path: %s
      fail: closed
providers:
  - {name: p1, kind: openai, base_url: "%s"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    filters:
      - plugin: pii-mask
        on: [request, response]
        scope: conversation
        patterns: [krrn, email]
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
  - name: plain
    deployments:
      - {provider: p1, upstream_model: plain-upstream, credentials: [c1]}
`, pluginPath(t), upstream)
}

// upstreamSpy records what the gateway sent and answers with what it is told to.
type upstreamSpy struct {
	mu     sync.Mutex
	bodies []string
	reply  func(body string) (status int, contentType, out string)
}

func (u *upstreamSpy) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, string(b))
		u.mu.Unlock()
		status, ct, out := u.reply(string(b))
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (u *upstreamSpy) lastBody(t *testing.T) string {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		t.Fatal("the upstream was never called")
	}
	return u.bodies[len(u.bodies)-1]
}

// placeholderIn extracts the first placeholder from a body.
func placeholderIn(t *testing.T, s string) string {
	t.Helper()
	i := strings.Index(s, "[PII:")
	if i < 0 {
		t.Fatalf("no placeholder in %q", s)
	}
	// 22 bytes: "[PII:" + 16 + "]". The width is mask.PlaceholderLen; it is
	// spelled out here so a change to it fails this test loudly.
	return s[i : i+22]
}

func chatReply(text string) string {
	b, _ := json.Marshal(map[string]any{
		"id":      "cmpl-1",
		"object":  "chat.completion",
		"created": 1,
		"model":   "m1-upstream",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
	})
	return string(b)
}

// newFilterApp assembles a gateway from yaml with a log sink attached. It is a
// sibling of newWiringApp rather than an extension of it, because a filter test
// has to read what the gateway logged and that is not something the other
// wiring tests want.
func newFilterApp(t *testing.T, yaml string, logs *safeLog, mut func(*config.Config)) *App {
	t.Helper()
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Storage.SQLite.Path = filepath.Join(t.TempDir(), "dorang.db")
	if mut != nil {
		mut(cfg)
	}
	t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
	t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)

	a, err := New(context.Background(), Options{Config: cfg, Logf: logs.printf})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

// safeLog collects diagnostics from every goroutine that writes one.
type safeLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *safeLog) printf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, f+"\n", a...)
}

func (l *safeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func filterApp(t *testing.T, spy *upstreamSpy, logs *safeLog) *App {
	t.Helper()
	t.Setenv("DORANG_APP_TEST_FILTER_SECRET", "a-cluster-wide-filter-seed") // pragma: allowlist secret — test fixture
	url := spy.start(t)
	return newFilterApp(t, filterYAML(t, url), logs, nil)
}

// TestMaskRoundTripsThroughTheWholeStack is the motivating case: the upstream
// never sees the identity number and the caller never notices.
func TestMaskRoundTripsThroughTheWholeStack(t *testing.T) {
	var logs safeLog
	spy := &upstreamSpy{}
	spy.reply = func(body string) (int, string, string) {
		// The model echoes the placeholder back, which is what makes the mask
		// reversible rather than merely destructive.
		return http.StatusOK, "application/json", chatReply("확인했습니다: " + placeholderInString(body))
	}
	a := filterApp(t, spy, &logs)
	secret := issueKey(t, a, nil)

	w := callWith(a, secret, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"제 번호는 `+theRRN+` 이고 메일은 `+theEmail+` 입니다"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	up := spy.lastBody(t)
	if strings.Contains(up, theRRN) {
		t.Fatalf("the identity number reached the upstream: %s", up)
	}
	if strings.Contains(up, theEmail) {
		t.Fatalf("the address reached the upstream: %s", up)
	}
	if !strings.Contains(up, "[PII:") {
		t.Fatalf("nothing was masked: %s", up)
	}

	if got := w.Body.String(); !strings.Contains(got, theRRN) {
		t.Fatalf("the caller did not get their own text back: %s", got)
	}
	if strings.Contains(w.Body.String(), "[PII:") {
		t.Fatalf("a placeholder reached the caller: %s", w.Body.String())
	}

	// The unfiltered model is untouched, so the feature costs nothing where it
	// is not configured.
	spy.reply = func(string) (int, string, string) { return http.StatusOK, "application/json", chatReply("ok") }
	w = callWith(a, secret, http.MethodPost, "/v1/chat/completions",
		`{"model":"plain","messages":[{"role":"user","content":"`+theRRN+`"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(spy.lastBody(t), theRRN) {
		t.Fatal("a model with no filter was masked anyway")
	}
}

func placeholderInString(s string) string {
	i := strings.Index(s, "[PII:")
	if i < 0 || i+22 > len(s) {
		return "(none)"
	}
	return s[i : i+22]
}

// TestMaskSurvivesAFrameBoundary: the placeholder is split across two SSE
// frames, which §10.5b calls out as the hard part of streaming.
func TestMaskSurvivesAFrameBoundary(t *testing.T) {
	var logs safeLog
	spy := &upstreamSpy{}
	spy.reply = func(body string) (int, string, string) {
		ph := placeholderInString(body)
		cut := len(ph) / 2
		frame := func(text string) string {
			b, _ := json.Marshal(map[string]any{
				"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "m1-upstream",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}},
			})
			return "data: " + string(b) + "\n\n"
		}
		return http.StatusOK, "text/event-stream",
			frame("당신의 번호는 "+ph[:cut]) + frame(ph[cut:]+" 입니다") +
				"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m1-upstream\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n"
	}
	a := filterApp(t, spy, &logs)
	secret := issueKey(t, a, nil)

	w := callWith(a, secret, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","stream":true,"messages":[{"role":"user","content":"내 번호 `+theRRN+`"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	if strings.Contains(got, "[PII:") {
		t.Fatalf("a placeholder straddling a frame was not reassembled:\n%s", got)
	}
	// The restored text is JSON-encoded in the frame, so the digits are what to
	// look for rather than the exact string.
	if !strings.Contains(got, theRRN) {
		t.Fatalf("the identity number was not restored across the frame boundary:\n%s", got)
	}
}

// TestInventedPlaceholderIsLeftAloneEndToEnd: the model makes one up, and the
// caller sees exactly what the model said.
func TestInventedPlaceholderIsLeftAloneEndToEnd(t *testing.T) {
	var logs safeLog
	spy := &upstreamSpy{}
	spy.reply = func(string) (int, string, string) {
		return http.StatusOK, "application/json", chatReply("here you go: [PII:aaaaaaaaaaaaaaaa]")
	}
	a := filterApp(t, spy, &logs)
	secret := issueKey(t, a, nil)

	w := callWith(a, secret, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"`+theRRN+`"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[PII:aaaaaaaaaaaaaaaa]") {
		t.Fatalf("an invented placeholder was rewritten: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), theRRN) {
		t.Fatalf("an invented placeholder resolved to a real value: %s", w.Body.String())
	}
}

// TestNothingDurableCarriesTheMaskTable is DESIGN §10.5b rule 1, asserted
// against every sink at once: the database file itself, a metrics scrape, and
// the operational log.
func TestNothingDurableCarriesTheMaskTable(t *testing.T) {
	var logs safeLog
	spy := &upstreamSpy{}
	spy.reply = func(body string) (int, string, string) {
		return http.StatusOK, "application/json", chatReply("ok " + placeholderInString(body))
	}

	t.Setenv("DORANG_APP_TEST_FILTER_SECRET", "a-cluster-wide-filter-seed") // pragma: allowlist secret — test fixture
	url := spy.start(t)
	dbPath := filepath.Join(t.TempDir(), "dorang.db")
	a := newFilterApp(t, filterYAML(t, url), &logs, func(c *config.Config) {
		c.Storage.SQLite.Path = dbPath
	})
	secret := issueKey(t, a, nil)

	w := callWith(a, secret, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"`+theRRN+` and `+theEmail+`"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	ph := placeholderIn(t, spy.lastBody(t))

	// A metric scrape: labels are the one place a value could hide in plain
	// sight, and cardinality would make it a memory leak as well as a leak.
	m := callWith(a, testMasterKey, http.MethodGet, "/metrics", "")
	if m.Code != http.StatusOK {
		t.Fatalf("metrics: %d", m.Code)
	}
	for _, bad := range []string{theRRN, theEmail, ph} {
		if strings.Contains(m.Body.String(), bad) {
			t.Fatalf("a metrics label carried %q", bad)
		}
	}
	// …and the counts are there, or "no leak" would be satisfied by exporting
	// nothing at all.
	for _, want := range []string{"dorang_filter_masked_total", "dorang_filter_restored_total"} {
		if !strings.Contains(m.Body.String(), want) {
			t.Fatalf("%s is not exported", want)
		}
	}

	// Flush the ledger and every other durable writer, then read the database
	// file as bytes. This is deliberately blunt: it covers request_logs,
	// request_traces, audit_logs and the Responses store in one assertion, and
	// it cannot be fooled by looking in the wrong table.
	// The error is not asserted: the cleanup closes again, and a second flush of
	// the same ledger rows is a duplicate-key complaint that says nothing about
	// what is on disk.
	_ = a.Close(context.Background())
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(dbPath + suffix)
		if err != nil {
			continue
		}
		for _, bad := range []string{theRRN, theEmail, ph} {
			if strings.Contains(string(b), bad) {
				t.Fatalf("the database file (%s) contains %q", dbPath+suffix, bad)
			}
		}
	}

	for _, bad := range []string{theRRN, theEmail, ph} {
		if strings.Contains(logs.String(), bad) {
			t.Fatalf("the log carried %q:\n%s", bad, logs.String())
		}
	}
}

// TestAFailedMaskStopsTheRequest is §10.5b's inversion of §11.5's fail-open.
func TestAFailedMaskStopsTheRequest(t *testing.T) {
	var logs safeLog
	spy := &upstreamSpy{}
	spy.reply = func(string) (int, string, string) {
		return http.StatusOK, "application/json", chatReply("ok")
	}
	t.Setenv("DORANG_APP_TEST_FILTER_SECRET", "a-cluster-wide-filter-seed") // pragma: allowlist secret — test fixture
	url := spy.start(t)

	// A plugin that cannot complete, declared fail-closed.
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.lua")
	if err := os.WriteFile(broken, []byte(`
		dorang.require_api(1)
		dorang.register("on_filter_request", function(req)
			-- Runs out of instructions before it masks anything.
			while true do end
		end)
	`), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(filterYAML(t, url), pluginPath(t), broken, 1)
	a := newFilterApp(t, yaml, &logs, func(c *config.Config) {
		c.Extensions.Lua.Limits.Instructions = 50_000
	})
	secret := issueKey(t, a, nil)

	w := callWith(a, secret, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"`+theRRN+`"}]}`)
	if w.Code == http.StatusOK {
		t.Fatalf("a request whose mask failed was served: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "filter") {
		t.Fatalf("the refusal does not say why: %s", w.Body.String())
	}
	spy.mu.Lock()
	n := len(spy.bodies)
	spy.mu.Unlock()
	if n != 0 {
		t.Fatalf("the upstream was called %d times despite the failed mask", n)
	}
}

// TestFilteredModelRefusesAnUnfilterableSurface: fail closed rather than send
// text this build cannot reach.
func TestFilteredModelRefusesAnUnfilterableSurface(t *testing.T) {
	var logs safeLog
	spy := &upstreamSpy{}
	spy.reply = func(string) (int, string, string) {
		return http.StatusOK, "application/json", `{"object":"list","data":[],"model":"m1"}`
	}
	a := filterApp(t, spy, &logs)
	secret := issueKey(t, a, nil)

	w := callWith(a, secret, http.MethodPost, "/v1/embeddings",
		`{"model":"m1","input":"`+theRRN+`"}`)
	if w.Code == http.StatusOK {
		t.Fatalf("an embeddings request on a filtered model was served: %s", w.Body.String())
	}
	spy.mu.Lock()
	n := len(spy.bodies)
	spy.mu.Unlock()
	if n != 0 {
		t.Fatal("the upstream saw unfiltered text")
	}
}

// TestMaskedRequestKeepsItsPrefixClaim is the coupling test: the same
// conversation sent twice must produce byte-identical upstream bodies and the
// same digests, and a third turn must extend the chain rather than fork it.
//
// A test that only checked the round trip would pass while caching was dead.
func TestMaskedRequestKeepsItsPrefixClaim(t *testing.T) {
	var logs safeLog
	spy := &upstreamSpy{}
	spy.reply = func(string) (int, string, string) {
		return http.StatusOK, "application/json", chatReply("ok")
	}
	a := filterApp(t, spy, &logs)
	secret := issueKey(t, a, nil)

	turn1 := `{"model":"m1","messages":[{"role":"user","content":"제 번호는 ` + theRRN + ` 입니다"}]}`
	turn2 := `{"model":"m1","messages":[{"role":"user","content":"제 번호는 ` + theRRN + ` 입니다"},` +
		`{"role":"assistant","content":"네"},{"role":"user","content":"다시: ` + theRRN + `"}]}`

	send := func(body string) string {
		if w := callWith(a, secret, http.MethodPost, "/v1/chat/completions", body); w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		return spy.lastBody(t)
	}

	first := send(turn1)
	second := send(turn1)
	if first != second {
		t.Fatalf("the same conversation produced different upstream bytes:\n%s\n%s", first, second)
	}
	third := send(turn2)
	ph := placeholderIn(t, first)
	if strings.Count(third, ph) != 2 {
		t.Fatalf("turn 2 did not reuse turn 1's placeholder (%s):\n%s", ph, third)
	}
}

func decodeChatForTest(raw string) (*canonical.Request, error) {
	return openai.DecodeRequest([]byte(raw))
}

func maskSessionFor(t *testing.T, mf *modelFilter) mask.SessionOptions {
	t.Helper()
	return mask.SessionOptions{Salt: "test-conversation"}
}

// TestMeteringDescribesWhatTheUpstreamWasSent is §10.5b rule 5.
//
// The token counts in the ledger come back from the upstream's own usage object,
// so they already describe the masked text — the end-to-end tests above prove
// the upstream only ever saw the masked text. What did *not* describe it is the
// pre-request estimate that drives the budget hold and the routing decision, and
// that is what this pins: both it and the prefix digest are taken from the
// post-filter image, not from the caller's body.
func TestMeteringDescribesWhatTheUpstreamWasSent(t *testing.T) {
	var logs safeLog
	spy := &upstreamSpy{}
	spy.reply = func(string) (int, string, string) {
		return http.StatusOK, "application/json", chatReply("ok")
	}
	a := filterApp(t, spy, &logs)

	mf := a.dispatch.state().filters.forModel("m1")
	if mf == nil || mf.mask == nil {
		t.Fatal("the model has no filter")
	}
	sess, err := mf.mask.Session(maskSessionFor(t, mf))
	if err != nil {
		t.Fatal(err)
	}

	raw := `{"model":"m1","messages":[{"role":"user","content":"` + theRRN + `"}]}`
	creq, err := decodeChatForTest(raw)
	if err != nil {
		t.Fatal(err)
	}
	c := &call{model: "m1", body: []byte(raw), creq: creq, mask: sess}

	doc, ok := requestDoc(c)
	if !ok {
		t.Fatal("no document")
	}
	for i := 0; i < doc.Len(); i++ {
		out, _, err := sess.Mask(doc.Text(i))
		if err != nil {
			t.Fatal(err)
		}
		doc.SetText(i, out)
	}

	img := prefixImage(c, c.body)
	if strings.Contains(string(img), theRRN) {
		t.Fatalf("the hashed image still carries the caller's text: %s", img)
	}
	if !strings.Contains(string(img), "[PII:") {
		t.Fatalf("the hashed image is not the masked one: %s", img)
	}
	// The estimate the dispatcher uses is structural over the POST-filter
	// canonical request, which is what §10.5b rule 5 asks for: the size the
	// budget holds against is the size the upstream is sent. Stating it needs
	// the two counts to differ, or the assertion cannot tell which one was used.
	unmasked, err := decodeChatForTest(raw)
	if err != nil {
		t.Fatal(err)
	}
	before := (&call{model: "m1", body: []byte(raw), creq: unmasked}).estimate().Tokens
	if c.estimate().Tokens == before {
		t.Fatal("the estimate is the same before and after masking; " +
			"the test cannot tell which one the dispatcher used")
	}
}

// TestPrefixDigestIsPerTenant is a cross-tenant property, not a hashing one:
// two tenants sending byte-identical bodies must not share an affinity entry.
//
// The seed leads with the TENANT — team, else user, else key — rather than with
// the api key alone, because §7.4a's tenant is what a prompt cache belongs to:
// colleagues on one team sharing a cache is the behaviour affinity exists to
// produce, and two teams sharing one is the oracle the review found.
func TestPrefixDigestIsPerTenant(t *testing.T) {
	const body = "the same body, byte for byte"
	a := prefix.Compute("team:alpha", "m1", []byte(body), 4096)
	b := prefix.Compute("team:beta", "m1", []byte(body), 4096)
	if len(a) == 0 || len(b) == 0 {
		t.Fatal("no digests")
	}
	for i := range a {
		if i < len(b) && a[i] == b[i] {
			t.Fatalf("two tenants share affinity entry %d; a cache hit that cannot happen, "+
				"and a confirmation oracle that can", i)
		}
	}
	// The same tenant still matches itself, or affinity is simply off.
	c := prefix.Compute("team:alpha", "m1", []byte(body), 4096)
	for i := range a {
		if a[i] != c[i] {
			t.Fatal("the same tenant and body produced different digests")
		}
	}
	// And the model still separates, as it did before.
	d := prefix.Compute("team:alpha", "m2", []byte(body), 4096)
	if a[0] == d[0] {
		t.Fatal("two models share an affinity entry")
	}
}
