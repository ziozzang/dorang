package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/luaext"
	"github.com/ziozzang/dorang/internal/store"
)

// End-to-end tests for DESIGN §11.5 through the real request path.
//
// The package-level tests in internal/luaext and internal/notify prove the
// mechanisms. These prove the wiring: that the hook actually runs where the
// design says it does, that its refusal reaches the client, that its view is
// built from a request which really is carrying a credential, and that a budget
// alert leaves the gateway once rather than once per request.

const hookBody = `{"model":"model-x","max_tokens":4,"messages":[{"role":"user","content":"ping"}]}`

// extConfig renders the round-trip configuration with extra sections spliced in.
func extConfig(t *testing.T, dir, upstreamURL, extra string) *config.Config {
	t.Helper()
	src := fmt.Sprintf(`version: 1
server:
  listen: 127.0.0.1:0
  env: development
storage:
  driver: sqlite
  sqlite:
    path: %s/dorang.db
metering:
  flush_interval: 20ms
  spool:
    dir: %s/spool
providers:
  - name: fake
    kind: openai
    base_url: %s/v1
    max_concurrency: 4
credentials:
  - id: fake-1
    provider: fake
    key: upstream-secret
classes:
  chat: [model-x]
models:
  - name: model-x
    class: chat
    deployments:
      - provider: fake
        upstream_model: upstream-x
        credentials: [fake-1]
pricing:
  currency: USD
  rules:
    - id: fake-tokens
      class: marginal_usage
      match: {provider: fake}
      rates: {input: "3.00", output: "15.00"}
%s`, dir, dir, upstreamURL, extra)

	path := filepath.Join(dir, "extensions.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("configuration:\n%v", err)
	}
	return cfg
}

func extUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamAnswer)
	}))
	t.Cleanup(up.Close)
	return up
}

// waitForCond polls until cond holds. Delivery is asynchronous by construction
// — that is the requirement, not an implementation detail — so a test that
// asserts on it has to wait for it rather than sleep a guessed interval.
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func policyDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func startGateway(t *testing.T, cfg *config.Config, opts ...func(*app.Options)) (*app.App, *httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	o := app.Options{Config: cfg, Logf: t.Logf}
	for _, fn := range opts {
		fn(&o)
	}
	a, err := app.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = a.Close(cctx)
	})
	front := httptest.NewServer(a.Server)
	t.Cleanup(front.Close)
	return a, front, issueKey(t, ctx, a.Store)
}

// TestHooksOffCostNothing: the default configuration builds no engine at all,
// so the hot path's cost is a nil check on a pointer that is nil.
func TestHooksOffCostNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "extensions-pepper")

	up := extUpstream(t)
	a, front, token := startGateway(t, extConfig(t, dir, up.URL, ""))

	if a.Hooks != nil {
		t.Fatalf("the default configuration must build no extension engine, got %#v", a.Hooks)
	}
	if a.Notify != nil {
		t.Fatalf("the default configuration must build no notifier, got %#v", a.Notify)
	}
	for h := luaext.Hook(0); h <= luaext.HookEmail; h++ {
		if a.Hooks.Enabled(h) {
			t.Errorf("%s reported enabled with no engine", h)
		}
	}
	if resp := post(t, front.URL+"/v1/chat/completions", token, hookBody); resp.status != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.status, resp.body)
	}
}

// TestOnRequestDenyReachesTheClient is §11.5's fail-closed half through the
// whole stack: a policy file on disk refuses a request, and the caller is told.
func TestOnRequestDenyReachesTheClient(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "extensions-pepper")

	up := extUpstream(t)
	pdir := policyDir(t, map[string]string{
		"on_request.policy": "deny \"model-x is not available through this gateway\" if model == \"model-x\"\n",
	})
	cfg := extConfig(t, dir, up.URL, fmt.Sprintf(`extensions:
  lua:
    enabled: true
    dir: %s
    limits: {instructions: 100000, memory_mb: 4, timeout: 2s}
`, pdir))
	_, front, token := startGateway(t, cfg)

	resp := post(t, front.URL+"/v1/chat/completions", token, hookBody)
	if resp.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.status, resp.body)
	}
	if !strings.Contains(resp.body, "model-x is not available through this gateway") {
		t.Errorf("the extension's reason did not reach the caller: %s", resp.body)
	}
	if !strings.Contains(resp.body, luaext.DefaultDenyCode) {
		t.Errorf("the refusal carries no code: %s", resp.body)
	}
}

// TestOnRouteDenyReachesTheClient: the same, one stage later, where the chosen
// deployment is in view.
func TestOnRouteDenyReachesTheClient(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "extensions-pepper")

	up := extUpstream(t)
	pdir := policyDir(t, map[string]string{
		"on_route.policy": "deny \"this team may not use fake\" if provider == \"fake\"\n",
	})
	cfg := extConfig(t, dir, up.URL, fmt.Sprintf(`extensions:
  lua:
    enabled: true
    dir: %s
    limits: {instructions: 100000, memory_mb: 4, timeout: 2s}
`, pdir))
	_, front, token := startGateway(t, cfg)

	resp := post(t, front.URL+"/v1/chat/completions", token, hookBody)
	if resp.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.status, resp.body)
	}
	if !strings.Contains(resp.body, "this team may not use fake") {
		t.Errorf("body = %s", resp.body)
	}
}

// TestABrokenHookDoesNotBreakTheGateway is the fail-open half, with all three
// ways a hook can fail exercised on a real request: it panics, it ignores its
// deadline, and it tries to refuse from inside the wreckage. The request is
// served every time.
func TestABrokenHookDoesNotBreakTheGateway(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "extensions-pepper")

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	up := extUpstream(t)
	cfg := extConfig(t, dir, up.URL, `extensions:
  lua:
    limits: {instructions: 100000, memory_mb: 4, timeout: 50ms}
`)
	a, front, token := startGateway(t, cfg, func(o *app.Options) {
		o.Extensions = []luaext.Native{
			{
				Name: "crasher",
				Request: func(_ context.Context, _ *luaext.RequestView, d *luaext.RequestDecision) {
					d.Denied = true
					d.Reason = "should never be seen"
					panic("the extension is broken")
				},
			},
			{
				Name: "wedged",
				Route: func(_ context.Context, _ *luaext.RouteView, d *luaext.RouteDecision) {
					<-release // ignores the context entirely
					d.Denied = true
				},
			},
		}
	})
	if a.Hooks == nil {
		t.Fatal("a registered native must build an engine")
	}

	start := time.Now()
	resp := post(t, front.URL+"/v1/chat/completions", token, hookBody)
	if resp.status != http.StatusOK {
		t.Fatalf("a broken extension must not refuse a request: %d %s", resp.status, resp.body)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the request waited %v on a wedged hook", elapsed)
	}
	st := a.Hooks.Stats()
	if st.Panics == 0 {
		t.Error("the panic was not counted")
	}
	if st.Timeouts == 0 {
		t.Error("the abandoned invocation was not counted")
	}
	if st.Denies != 0 {
		t.Errorf("a broken hook produced %d denials", st.Denies)
	}
}

// TestNoHookSeesTheBearerToken is the secret guarantee where it matters: a real
// request, authenticated with a real token, against a deployment holding a real
// upstream credential. Every hook records everything it can reach, and neither
// secret is in any of it.
func TestNoHookSeesTheBearerToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "extensions-pepper")

	var mu sync.Mutex
	var seen []string
	record := func(vals ...string) {
		mu.Lock()
		seen = append(seen, vals...)
		mu.Unlock()
	}

	up := extUpstream(t)
	cfg := extConfig(t, dir, up.URL, `extensions:
  lua:
    limits: {instructions: 100000, memory_mb: 4, timeout: 2s}
`)
	_, front, token := startGateway(t, cfg, func(o *app.Options) {
		o.Extensions = []luaext.Native{{
			Name: "exfiltrator",
			Request: func(_ context.Context, v *luaext.RequestView, _ *luaext.RequestDecision) {
				record(v.RequestID, v.Method, v.Path, v.Route, v.Model, v.KeyID,
					v.KeyName, v.UserID, v.TeamID, v.Priority)
			},
			Route: func(_ context.Context, v *luaext.RouteView, _ *luaext.RouteDecision) {
				record(v.Model, v.KeyID, v.UserID, v.TeamID, v.Provider,
					v.Deployment, v.Kind, v.UpstreamModel, v.Priority)
			},
			Response: func(_ context.Context, v *luaext.ResponseView, _ *luaext.ResponseDecision) {
				record(v.RequestID, v.Model, v.KeyID, v.UserID, v.TeamID,
					v.Provider, v.Deployment, v.UpstreamModel, v.ErrorCode)
			},
		}}
	})

	if resp := post(t, front.URL+"/v1/chat/completions", token, hookBody); resp.status != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.status, resp.body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no hook ran")
	}
	// The caller's bearer token and the upstream credential from the
	// configuration. Neither has a field on any view that could carry it.
	for _, secret := range []string{token, "upstream-secret", "Bearer "} {
		for _, s := range seen {
			if s != "" && strings.Contains(s, secret) {
				t.Fatalf("a hook read %q, which contains %q", s, secret)
			}
		}
	}
}

// TestBudget80AlertsOncePerPeriod is the notification requirement end to end.
//
// The threshold is crossed on a request and re-crossed on every request after
// it. One alert leaves the gateway. It leaves on a worker rather than on the
// request path — which is observable here as the request that crossed the line
// completing normally — and it carries no token.
func TestBudget80AlertsOncePerPeriod(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "extensions-pepper")

	var mu sync.Mutex
	var hooks []string
	var unsigned int
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hooks = append(hooks, string(b))
		// §11.5 rule 1: every delivery is signed and carries an idempotency key.
		if r.Header.Get("X-Dorang-Signature") == "" ||
			r.Header.Get("X-Dorang-Idempotency-Key") == "" {
			unsigned++
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()

	up := extUpstream(t)
	t.Setenv("DORANG_TEST_WEBHOOK_SECRET", "budget-alert-signing-secret")
	cfg := extConfig(t, dir, up.URL, fmt.Sprintf(`notifications:
  email:
    driver: http
    http:
      url: %s
      key_env: DORANG_TEST_WEBHOOK_SECRET
  events: [budget_80pct, budget_exceeded]
  dedup_period: 1h
`, webhook.URL))

	ctx := context.Background()
	a, err := app.New(ctx, app.Options{Config: cfg, Logf: t.Logf, BudgetBlockNanoUSD: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = a.Close(cctx)
	})
	if a.Notify == nil {
		t.Fatal("a configured driver must build a notifier")
	}

	const token = "sk-budget-alert-token" // pragma: allowlist secret — test fixture
	limit := int64(500_000)
	k := &store.APIKey{KeyAlias: "budgeted", MaxBudgetNano: &limit, BudgetPeriod: "monthly"}
	if err := a.Store.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.InsertAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}

	front := httptest.NewServer(a.Server)
	defer front.Close()

	// Spend until the budget refuses, then keep going: every refused request
	// re-raises budget_exceeded, and every served request past 80% re-raises
	// budget_80pct. Both must still be one message.
	refusals := 0
	for i := 0; i < 40; i++ {
		resp := post(t, front.URL+"/v1/chat/completions", token, hookBody)
		if resp.status != http.StatusOK {
			refusals++
		}
	}
	if refusals == 0 {
		t.Fatal("the budget never refused; the test proves nothing")
	}

	// Delivery is asynchronous by construction, so drain rather than assert
	// immediately — and then assert the count does not grow.
	waitForCond(t, "the alerts to be delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(hooks) >= 2
	})
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	counts := map[string]int{}
	for _, body := range hooks {
		switch {
		case strings.Contains(body, `"event":"budget_80pct"`):
			counts["budget_80pct"]++
		case strings.Contains(body, `"event":"budget_exceeded"`):
			counts["budget_exceeded"]++
		default:
			t.Errorf("an unexpected notification: %s", body)
		}
		if strings.Contains(body, token) || strings.Contains(body, "upstream-secret") {
			t.Fatalf("a notification carried a secret: %s", body)
		}
	}
	for _, ev := range []string{"budget_80pct", "budget_exceeded"} {
		if counts[ev] != 1 {
			t.Errorf("%s fired %d times across %d requests, want exactly 1 per period",
				ev, counts[ev], 40)
		}
	}
	if unsigned != 0 {
		t.Errorf("%d deliveries arrived unsigned or without an idempotency key", unsigned)
	}
	if st := a.Notify.Stats(); st.Suppressed == 0 {
		t.Error("the repeats must be counted as suppressed, not merely absent")
	}
}

// TestLuaSourceRefusesToStart is the promise that the configuration surface does
// not accept Lua that never runs. This build has no interpreter; a .lua file is
// a startup failure that says so.
func TestLuaSourceRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "extensions-pepper")

	up := extUpstream(t)
	pdir := policyDir(t, map[string]string{
		"on_request.lua": "function on_request(r) return false end\n",
	})
	cfg := extConfig(t, dir, up.URL, fmt.Sprintf(`extensions:
  lua:
    enabled: true
    dir: %s
    limits: {instructions: 100000, memory_mb: 4, timeout: 2s}
`, pdir))

	_, err := app.New(context.Background(), app.Options{Config: cfg, Logf: t.Logf})
	if err == nil {
		t.Fatal("a .lua file must not be silently ignored")
	}
	if !strings.Contains(err.Error(), "Lua source is not executable") {
		t.Errorf("the error must say why: %v", err)
	}
}
