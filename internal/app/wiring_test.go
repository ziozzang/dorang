package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/server"
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
	isolateState(t)
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
//
// The tokens come from a REAL finished request through a real upstream, not
// from a direct call into the counter. A test that feeds the counter itself
// proves the comparison works and says nothing about whether anything in
// production feeds it — which is precisely the topology that let rpm_limit and
// tpm_limit ship enforcing nothing, unit-tested, for the life of the project.
func TestTPMLimitCountsFinishedTokens(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m1-upstream",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},`+
			`"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":400,"completion_tokens":400,"total_tokens":800}}`)
	}))
	defer up.Close()

	yaml := fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
providers:
  - {name: p1, kind: openai, base_url: %q}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`, up.URL)
	a := newWiringApp(t, yaml, nil, func(o *Options) { o.Upstream = up.Client() })
	secret := issueKey(t, a, func(k *store.APIKey) { k.TPMLimit = int64Ptr(100) })

	const chat = `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`

	// Nothing has been spent, so the first request is admitted and served.
	if w := callWith(a, secret, http.MethodPost, "/v1/chat/completions", chat); w.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", w.Code, w.Body.String())
	}

	// 800 tokens against a ceiling of 100. The count exists only at settlement,
	// so the ceiling bounds the NEXT request — which is the whole of what a
	// post-hoc counter can do and is what tpm means everywhere it is published.
	w := callWith(a, secret, http.MethodPost, "/v1/chat/completions", chat)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("a key past tpm_limit answered %d, want 429: the settled token count "+
			"reaches no counter\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rate") {
		t.Errorf("the refusal does not say it is a rate limit: %s", w.Body.String())
	}
}

// The settled token count lands on EVERY subject of the request, not only on
// the api key.
//
// auth.Access carried ONE observed pair for three subjects and the window was
// keyed by api key alone, so a team's tpm_limit was compared against one key's
// count — a team ceiling multiplied by the number of keys under the team, the
// same N-multiplication as the budget defect in the rate dimension. This drives
// App.recordMetrics, which is the one production settlement site, and then asks
// the gate about a DIFFERENT key on the same team.
//
// It is a separate test from the one above because `teams` has no Go code in
// internal/store yet (docs/OPERATIONS.md §12), so a team ceiling cannot be put
// on a stored row and reached over HTTP. The half that can be joined is joined:
// the real settlement site feeds the real gate.
func TestSettledTokensReachEverySubjectOfTheRequest(t *testing.T) {
	a := newWiringApp(t, wiringYAML, nil)

	a.recordMetrics(&server.Event{
		KeyID: "key-1", UserID: "user-1", TeamID: "team-a",
		Result: server.Result{Tokens: server.Usage{Input: 400, Output: 400, Total: 800}},
	})

	// A different key, same team, and the ceiling is the TEAM's.
	other := &principal{
		p: &auth.Principal{
			KeyID: "key-2", TeamID: "team-a",
			Team: &auth.Limits{TPMLimit: auth.Limit(100)},
		},
		now:   a.now,
		rates: a.rates,
	}
	if err := other.Authorize(server.Access{}); err == nil {
		t.Fatal("a second key under a team that has already spent 800 tokens against a " +
			"tpm_limit of 100 was admitted: the settled tokens reached only the api key, " +
			"so the team ceiling is enforced per key")
	}

	// And a key on a different team is unaffected, so the counter separates
	// rather than simply refusing everything.
	elsewhere := &principal{
		p: &auth.Principal{
			KeyID: "key-3", TeamID: "team-b",
			Team: &auth.Limits{TPMLimit: auth.Limit(100)},
		},
		now:   a.now,
		rates: a.rates,
	}
	if err := elsewhere.Authorize(server.Access{}); err != nil {
		t.Fatalf("another team's key was refused by team-a's spend: %v", err)
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

// TestRequestLedgerHoldsOnlyThisTestsRequests counts ledger rows with no
// predicate on them, which is the one shape of assertion the package's shared
// state directory was waiting for.
//
// DESIGN §12.1 splits metering in two so the trace payload may be dropped and
// the counters may not, and the trace half's durability is a disk spool: an
// append log with a read cursor, replayed at open so a crash costs at most one
// batch. Every replayed record becomes a `request_logs` row — one ledger row per
// metered request. Replay is scoped by DIRECTORY and by nothing else, so two
// gateways pointed at one directory are one spool, and the rows the first one
// still owed are written into the second one's database as its own.
//
// Before [isolateState], that directory was the whole package's: the default
// spool path is `~/.dorang/spool`, `~` resolved to the single temporary
// directory TestMain sets, and roughly ninety-seven tests shared it. Nothing
// failed, because every test overrode the SQLite path to its own t.TempDir() and
// every assertion that read the ledger read it through a filter — by key, by
// team, by trace id — and a filtered read cannot see another test's rows. That
// is a property of the tests that happened to exist, not of the harness.
//
// The position is deliberate, and it is worth saying why, because once the fix
// is in it stops mattering entirely. A backlog only exists when the gateway
// before this one left records unshipped, and it lands on whichever gateway
// opens the directory NEXT — which then adopts it, writes it down as its own,
// and acknowledges it, so the evidence is gone by the test after that. This test
// therefore runs immediately behind the one gateway in the package that is made
// to fall behind on purpose: TestMeteringDegradedReachesHealth pushes a hundred
// thousand events through a meter to make it report a loss. Measured with the
// two of them sharing a directory: 212 KB of spooled records with the cursor
// still at the first one, all of them landing here.
//
// That is also the whole reason the defect read as nothing for so long. The
// backlog goes to a neighbour, the neighbour does not count, and moving one test
// makes it appear somewhere else. It would have arrived as a flake.
func TestRequestLedgerHoldsOnlyThisTestsRequests(t *testing.T) {
	ctx := context.Background()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","object":"chat.completion","model":"m1-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
			"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	t.Cleanup(up.Close)

	yaml := fmt.Sprintf(`
version: 1
providers:
  - {name: p1, kind: openai, base_url: %q}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`, up.URL)

	a := newWiringApp(t, yaml, nil, func(o *Options) { o.Upstream = up.Client() })
	dsn := a.Config().Storage.SQLite.Path
	secret := issueKey(t, a, nil)

	const requests = 3
	for i := 0; i < requests; i++ {
		w := callWith(a, secret, http.MethodPost, "/v1/chat/completions",
			`{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d answered %d: %s", i, w.Code, w.Body.String())
		}
	}

	// Close is the flush. Meter.Close drains the ring to the spool, ships the
	// spool to the sink and writes the numeric half, and it runs before the store
	// it writes into closes — so afterwards the file holds everything this
	// gateway ever metered and nothing is still in flight.
	if err := a.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := countLedgerRows(t, dsn); got != requests {
		t.Fatalf("this gateway served %d requests and its ledger holds %d rows. The "+
			"difference was metered by another test and replayed into this database out "+
			"of a shared trace spool; give every test its own state directory "+
			"(isolateState)", requests, got)
	}
}

// countLedgerRows counts `request_logs` in a closed SQLite database.
//
// It goes to the file rather than through internal/store because the point is to
// count EVERYTHING, and every ledger read internal/store offers is scoped to a
// key, a team, a trace or a status — which is precisely why rows belonging to
// another test could sit there unnoticed. The gateway is closed first, so the
// meter has drained, shipped and flushed, and the file is nobody's.
func countLedgerRows(t *testing.T, dsn string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&n); err != nil {
		t.Fatalf("count request_logs: %v", err)
	}
	return n
}

// TestGrantedClientPriorityIsHonouredAndADropIsReported is §10.5 end to end: the
// hint is honoured for a key an operator granted it to, and REPORTED as dropped
// for one that was not.
//
// The name used to promise more than the body delivered: it asserted the joint
// (the configured grant reaching router.PriorityConfig) and then called
// CanonicalFor directly, which is the priority clamp's own unit test wearing an
// end-to-end name. §10.5's requirement is that a dropped hint be reported in
// x-dorang-dropped-params, and that header is stamped by the dispatcher — so
// asserting it needs a request that reaches an upstream, which is what this now
// does.
func TestGrantedClientPriorityIsHonouredAndADropIsReported(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m1-upstream",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	yaml := strings.Replace(wiringYAML,
		`base_url: "https://example.invalid"`, fmt.Sprintf("base_url: %q", up.URL), 1)
	a := newWiringApp(t, yaml, nil, func(o *Options) { o.Upstream = up.Client() })
	ungranted := issueKey(t, a, nil)

	const chat = `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chat))
	r.Header.Set("Authorization", "Bearer "+ungranted)
	r.Header.Set(HeaderClientPriority, "0")
	w := httptest.NewRecorder()
	a.Server.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	dropped := w.Header().Get(server.HeaderDroppedParams)
	if !strings.Contains(dropped, HeaderClientPriority) {
		t.Fatalf("an ungranted priority hint was dropped silently; "+
			"%s = %q, want it to name %s (§10.5)",
			server.HeaderDroppedParams, dropped, HeaderClientPriority)
	}

	// And the joint underneath it: the configured grant reaches the router's
	// priority config, and a principal with no grant does not acquire one.
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
