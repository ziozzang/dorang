package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/store"
)

// These are the assembled-stack half of the sweep. Every one of them asserts
// something OUTSIDE the package that owns the check — an HTTP status, a header,
// a health body — because the checks themselves have always been correct and
// have never been called. DESIGN §17.1: a package test can prove a check works
// while nothing calls it.

const wiringYAML = `
version: 1
observability: {always_full_headers: true}
priority_mapping:
  classes: {realtime: 0, interactive: 2, batch: 10}
capacity:
  principals:
    default: {max_concurrent: 32}
    granted: {client_priority: allow, range: [batch, interactive]}
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`

// newWiringApp assembles a gateway from yaml, with a master key set. The
// optional Options mutators run last, so a test can supply a Logf or an upstream
// client without a second assembly path existing.
func newWiringApp(t *testing.T, yaml string, mut func(*config.Config), opts ...func(*Options)) *App {
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

	o := Options{Config: cfg}
	for _, f := range opts {
		f(&o)
	}
	a, err := New(context.Background(), o)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

// issueKey inserts an api key and returns its secret.
var issuedKeys atomic.Int64

func issueKey(t *testing.T, a *App, mut func(*store.APIKey)) string {
	t.Helper()
	token := "sk-wiring-" + strconv.FormatInt(issuedKeys.Add(1), 10) // pragma: allowlist secret — test fixture
	k := &store.APIKey{ID: "key-" + token}
	if mut != nil {
		mut(k)
	}
	if err := a.Store.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatalf("NewAPIKeyFromToken: %v", err)
	}
	if err := a.Store.InsertAPIKey(context.Background(), k); err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	return token
}

func callWith(a *App, secret, method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	w := httptest.NewRecorder()
	a.Server.ServeHTTP(w, r)
	return w
}

// TestRPMLimitRefusesThroughTheWholeStack is the joined form of the per-key rate
// ceiling. internal/auth's comparison has always been right and has always been
// handed a zero, so a key with rpm_limit: 2 was unlimited. The assertion is an
// HTTP status, which is the only place the defect was visible.
func TestRPMLimitRefusesThroughTheWholeStack(t *testing.T) {
	a := newWiringApp(t, wiringYAML, nil)
	secret := issueKey(t, a, func(k *store.APIKey) { k.RPMLimit = int64Ptr(2) })

	for i := 1; i <= 2; i++ {
		if w := callWith(a, secret, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d body %s", i, w.Code, w.Body.String())
		}
	}
	w := callWith(a, secret, http.MethodGet, "/v1/models", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the third request under rpm_limit: 2 answered %d, want 429\n%s",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rate") {
		t.Errorf("the refusal does not say it is a rate limit: %s", w.Body.String())
	}
}

// TestNoRPMLimitIsUnlimited: the counter must not become a limit of its own. An
// absent ceiling and a ceiling of zero are different answers, which is why the
// column is a pointer.
func TestNoRPMLimitIsUnlimited(t *testing.T) {
	a := newWiringApp(t, wiringYAML, nil)
	secret := issueKey(t, a, nil)
	for i := 0; i < 20; i++ {
		if w := callWith(a, secret, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
			t.Fatalf("request %d on an unlimited key: %d %s", i, w.Code, w.Body.String())
		}
	}
}

// TestTPMLimitCountsFinishedTokens closes the other half: the request count is
// taken at the gate, the token count when the request settles.
func TestTPMLimitCountsFinishedTokens(t *testing.T) {
	a := newWiringApp(t, wiringYAML, nil)
	secret := issueKey(t, a, func(k *store.APIKey) { k.TPMLimit = int64Ptr(100) })

	// Nothing has been spent, so the key is admitted.
	if w := callWith(a, secret, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("first request: %d", w.Code)
	}
	// Settle a request that consumed more than the whole minute's allowance.
	keyID := lookupKeyID(t, a, secret)
	a.rates.record(keyID, 500)

	if w := callWith(a, secret, http.MethodGet, "/v1/models", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a key past tpm_limit answered %d, want 429\n%s", w.Code, w.Body.String())
	}
}

// TestMasterCredentialIsNotRateCounted: the administrative credential has no
// stored row and no limits, and counting it would let a probe consume a tenant's
// window under a shared id.
func TestMasterCredentialIsNotRateCounted(t *testing.T) {
	a := newWiringApp(t, wiringYAML, nil)
	for i := 0; i < 50; i++ {
		if w := callWith(a, testMasterKey, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
			t.Fatalf("master request %d: %d", i, w.Code)
		}
	}
}

// TestMetricsRequiresAnAdminCredential. /metrics was Public alongside the
// container probes, which put per-key spend, per-credential quota state and the
// whole configured model list on an unauthenticated port.
func TestMetricsRequiresAnAdminCredential(t *testing.T) {
	a := newWiringApp(t, wiringYAML, nil)
	tenant := issueKey(t, a, nil)

	if w := callWith(a, "", http.MethodGet, "/metrics", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated scrape answered %d, want 401", w.Code)
	}
	if w := callWith(a, tenant, http.MethodGet, "/metrics", ""); w.Code != http.StatusForbidden {
		t.Errorf("an ordinary key read the scrape: %d, want 403", w.Code)
	}
	w := callWith(a, testMasterKey, http.MethodGet, "/metrics", "")
	if w.Code != http.StatusOK {
		t.Fatalf("the master credential was refused the scrape: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dorang_build_info") {
		t.Error("the scrape is empty")
	}
}

// TestMetricsPublicOpensItDeliberately: opening the endpoint is a written
// decision, and the setting that writes it has to work.
func TestMetricsPublicOpensItDeliberately(t *testing.T) {
	a := newWiringApp(t, strings.Replace(wiringYAML,
		"observability: {always_full_headers: true}",
		"observability: {always_full_headers: true, metrics: {public: true}}", 1), nil)
	if w := callWith(a, "", http.MethodGet, "/metrics", ""); w.Code != http.StatusOK {
		t.Fatalf("metrics.public: true still refused an anonymous scrape: %d", w.Code)
	}
}

// TestPrometheusFalseRemovesTheRoute. The flag was never read: setting it
// changed nothing at all.
func TestPrometheusFalseRemovesTheRoute(t *testing.T) {
	a := newWiringApp(t, wiringYAML, func(c *config.Config) {
		f := false
		c.Observability.Prometheus = &f
	})
	w := callWith(a, testMasterKey, http.MethodGet, "/metrics", "")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("prometheus: false answered %d, want 501: a disabled route is not a 200 "+
			"with an empty body\n%s", w.Code, w.Body.String())
	}
}

// TestMeteringDegradedReachesHealth is DESIGN §12.1's "a drop is never silent".
// The meter has tracked five degradation reasons with hysteresis since it was
// written, and until now the only reader was its own test suite.
func TestMeteringDegradedReachesHealth(t *testing.T) {
	a := newWiringApp(t, wiringYAML, nil)

	w := callWith(a, "", http.MethodGet, "/health", "")
	if w.Code != http.StatusOK {
		t.Fatalf("/health answered %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("/health is not JSON: %v\n%s", err, w.Body.String())
	}
	m, ok := body["metering"].(map[string]any)
	if !ok {
		t.Fatalf("no metering object in the health body: %s", w.Body.String())
	}
	if m["degraded"] != false {
		t.Errorf("a healthy meter reports degraded=%v", m["degraded"])
	}
	if m["reason"] != "none" {
		t.Errorf("reason = %v on a healthy meter", m["reason"])
	}

	// Now make it lose data and require the health body to say so. Reporting
	// only on failure would leave an operator unable to tell "not degraded" from
	// "this build does not report it".
	degradeMeter(t, a.Meter)
	w = callWith(a, "", http.MethodGet, "/health", "")
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("/health is not JSON: %v", err)
	}
	m = body["metering"].(map[string]any)
	if m["degraded"] != true {
		t.Fatalf("metering is losing data and /health says degraded=%v: %s",
			m["degraded"], w.Body.String())
	}
	if m["reason"] == "none" {
		t.Error("degraded with no reason: the five reasons exist to be reported")
	}
	// Losing trace payloads is a data-quality failure, not a serving failure: it
	// must not take the pod out of rotation.
	if w.Code != http.StatusOK {
		t.Errorf("degraded metering took the pod out of rotation: /health answered %d", w.Code)
	}
}

// TestGrantedClientPriorityIsHonouredAndADropIsReported is §10.5 end to end: the
// grant comes from the file, the hint from the request, and a hint that was not
// granted says so on the way out.
func TestGrantedClientPriorityIsHonouredAndADropIsReported(t *testing.T) {
	a := newWiringApp(t, wiringYAML, nil)
	ungranted := issueKey(t, a, nil)

	w := callWith(a, ungranted, http.MethodGet, "/v1/models", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}

	// The header path is exercised through the router directly, because the
	// dropped-params header is stamped by the dispatcher and an inference
	// request needs an upstream. What must hold here is the joint: the config
	// grant reached router.PriorityConfig.
	cfg := a.Config()
	if !cfg.Capacity.Principals["granted"].GrantsClientPriority() {
		t.Fatal("the fixture lost its grant")
	}
	pc := priorityConfig(cfg)
	if !pc.GrantsHint("granted") {
		t.Fatal("a configured client_priority: allow did not reach the router's priority config")
	}
	if pc.GrantsHint("someone-else") {
		t.Fatal("a principal with no grant was given one")
	}
	hint := 2
	if got := pc.CanonicalFor("granted", "batch", &hint); got != 2 {
		t.Errorf("a granted hint produced priority %d, want 2", got)
	}
	if got := pc.CanonicalFor("someone-else", "batch", &hint); got != 10 {
		t.Errorf("an ungranted hint was honoured: %d", got)
	}
}

// TestClientPriorityHeaderIsRead: the hint had no reader anywhere, so §10.5's
// allow branch had no input even once a range could be configured.
func TestClientPriorityHeaderIsRead(t *testing.T) {
	h := http.Header{}
	if clientPriorityHint(h) != nil {
		t.Error("an absent header produced a hint")
	}
	h.Set(HeaderClientPriority, "3")
	got := clientPriorityHint(h)
	if got == nil || *got != 3 {
		t.Fatalf("hint = %v, want 3", got)
	}
	h.Set(HeaderClientPriority, "urgent-please")
	if clientPriorityHint(h) != nil {
		t.Error("an unparseable advisory header must be nil, not an error")
	}
}

// TestKeyRotationStrategyReachesTheRouter joins the config value to the router.
// It was validated against four names and every one of them behaved as failover.
func TestKeyRotationStrategyReachesTheRouter(t *testing.T) {
	yaml := strings.Replace(wiringYAML,
		"credentials:\n  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}",
		"credentials:\n  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}\n"+
			"  - {id: c2, provider: p1, key_env: DORANG_APP_TEST_KEY}\n"+
			"key_rotation:\n  strategy: round_robin\n"+
			"  providers:\n    p1:\n      keys:\n"+
			"        - {id: c1, key_env: DORANG_APP_TEST_KEY}\n"+
			"        - {id: c2, key_env: DORANG_APP_TEST_KEY}", 1)
	yaml = strings.Replace(yaml, "credentials: [c1]}", "credentials: [c1, c2]}", 1)

	a := newWiringApp(t, yaml, nil)
	if got := a.Config().KeyRotation.Strategy; got != "round_robin" {
		t.Fatalf("the fixture lost its strategy: %q", got)
	}
	// The assembled router carries it, which is the edge that did not exist.
	st := a.dispatch.state()
	if st == nil || st.router == nil {
		t.Fatal("no router was assembled")
	}
	if got := st.router.Rotation(); got != "round_robin" {
		t.Fatalf("the router's rotation is %q: the configured strategy did not reach it", got)
	}
}

// TestBrokerQueueCeilingsReachTheBroker is the same joint for max_queue and
// max_queue_wait, which were defaulted per principal and thrown away.
func TestBrokerQueueCeilingsReachTheBroker(t *testing.T) {
	yaml := strings.Replace(wiringYAML,
		"    default: {max_concurrent: 32}",
		"    default: {max_concurrent: 32, max_queue: 4, max_queue_wait: 7s}", 1)
	a := newWiringApp(t, yaml, nil)
	if got := a.Broker.WaitBudget("anyone"); got.String() != "7s" {
		t.Fatalf("the broker's wait budget is %s: max_queue_wait did not reach it", got)
	}
}

// TestInlineNotionalRuleReachesThePricingCatalog: §8.5's class was catalog-only,
// and adding the name alone would not have been a fix — internal/pricing refuses
// a notional rule with no provenance, so the bridge had to carry it too.
func TestInlineNotionalRuleReachesThePricingCatalog(t *testing.T) {
	yaml := wiringYAML + `
pricing:
  rules:
    - id: marginal
      class: marginal_usage
      match: {provider: p1}
      rates: {input: "0.000001", output: "0.000002"}
    - id: notional
      class: notional_rate
      match: {provider: p1}
      rates: {input: "0.000003", cached_read: "0.00000019"}
      source: "vendor list price page"
      as_of: "2026-07-28"
`
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cat, err := Pricing(cfg)
	if err != nil {
		t.Fatalf("the inline notional rule did not compile: %v", err)
	}
	cost, err := cat.Price(pricing.Request{
		Provider: "p1", Model: "m1-upstream", Credential: "c1",
		InputTokens: 1000, OutputTokens: 500, Requests: 1,
	})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if cost.NotionalMissing {
		t.Fatal("the notional figure is missing: an inline rule did not reach the engine")
	}
	if cost.NotionalNano == 0 {
		t.Error("the notional figure is zero, which §8.5 says must never stand in for missing")
	}
	if cost.MarginalNano == 0 {
		t.Error("the marginal rule stopped pricing")
	}
}

// TestPassthroughCounterReachesTheScrape. The counter is incremented in
// internal/server and rendered by internal/metrics from a different struct; the
// documentation said it had no increment site, and nothing asserted otherwise.
func TestPassthroughCounterReachesTheScrape(t *testing.T) {
	yaml := wiringYAML + `
passthrough:
  enabled: true
  routes:
    - {prefix: /vendor, provider: p1, auth: none}
`
	a := newWiringApp(t, yaml, nil)
	before := a.Server.Stats().Passthrough

	// The upstream does not exist, which is fine: the counter counts attempts,
	// and a relay that cannot dial is still a passthrough request.
	callWith(a, "", http.MethodGet, "/vendor/anything", "")
	if got := a.Server.Stats().Passthrough; got != before+1 {
		t.Fatalf("Passthrough = %d, want %d", got, before+1)
	}
	w := callWith(a, testMasterKey, http.MethodGet, "/metrics", "")
	if !strings.Contains(w.Body.String(), "dorang_passthrough_requests_total 1") {
		t.Errorf("the counter did not reach the scrape:\n%s", grepLines(w.Body.String(), "passthrough"))
	}
}

func grepLines(body, want string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, want) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func int64Ptr(v int64) *int64 { return &v }

// lookupKeyID resolves a secret to the key id the rate window is keyed by.
func lookupKeyID(t *testing.T, a *App, secret string) string {
	t.Helper()
	p, err := a.Auth.AuthenticateHeader(context.Background(),
		http.Header{"Authorization": []string{"Bearer " + secret}})
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return p.KeyID
}

// degradeMeter overruns the trace queue until the meter reports a loss.
func degradeMeter(t *testing.T, m *meter.Meter) {
	t.Helper()
	for i := 0; i < 100_000; i++ {
		m.Record(meter.Event{
			APIKeyID: "k", ModelGroup: "m1", Provider: "p1", Status: 200,
			Trace: meter.TraceInfo{RequestID: "r", UpstreamModel: "m1-upstream"},
		})
		if deg, _ := m.Degraded(); deg {
			return
		}
	}
	t.Fatal("could not make the meter drop a trace, so this test proves nothing")
}
