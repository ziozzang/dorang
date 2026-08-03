package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/metrics"
	"github.com/ziozzang/dorang/internal/server"
)

// metricsYAML is a minimal gateway with a store on disk, so the ledger, the
// authenticator and the connection pool are all real.
const metricsYAML = `
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`

// metricsReloadYAML is metricsYAML with a second model group, an alias and
// cache-affinity routing turned off — the three things a reload has to be able
// to change in the metrics surface without rebuilding the registry.
const metricsReloadYAML = `
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
  - name: m2
    deployments:
      - {provider: p1, upstream_model: m2-upstream, credentials: [c1]}
aliases: {fast: m1}
routing:
  prefix: {enabled: false}
`

// TestModelLabelAdmissionFollowsAReload is the assembled half of the `model`
// label's bound, and the reload is the whole point of it.
//
// internal/metrics can prove that an unconfigured name folds to __unknown__ and
// that a configured one gets a series; it cannot prove that anything ever tells
// it what the configuration is. That is DESIGN §17.1's dominant defect class —
// an interface satisfied on both ends and connected on neither — and it has a
// second edge here that a start-up-only test would miss: the registry is built
// once and [App.Reload] does not rebuild it, so a set copied in at assembly and
// never again is a bound that a config reload adding a model finds already
// spent. That was the original defect's first symptom, arriving by a different
// door.
//
// It drives [App.recordMetrics], which is the one production observation site:
// server.Event.Model is the caller's own bytes there (internal/server's
// TestMeteredModelIsTheCallersOwnBytes pins that it is), so a fabricated name in
// this test is the same value a fabricated name in a request body would be.
func TestModelLabelAdmissionFollowsAReload(t *testing.T) {
	a := newMetricsApp(t)

	observe := func(model string, status int) {
		a.recordMetrics(&server.Event{
			Model: model, Route: "chat_completions", Status: status,
			DurationNS: int64(time.Millisecond),
			Result:     server.Result{Provider: "p1", Credential: "c1", Deployment: "d1"},
		})
	}

	// The configured model, then far more fabricated names than the default cap
	// has entries.
	observe("m1", 200)
	for i := 0; i < metrics.DefaultMaxModelSeries*4; i++ {
		observe("fabricated-"+strconv.Itoa(i), 404)
	}

	models := scrapedModelLabels(t, a)
	if models["fabricated-7"] {
		t.Errorf("a name out of a request body is a label value: %v", sortedKeys(models))
	}
	if !models[metrics.UnknownModelSentinel] {
		t.Errorf("no %s bucket after 512 unserved model names: %v",
			metrics.UnknownModelSentinel, sortedKeys(models))
	}
	if models[metrics.OverflowSentinel] {
		t.Errorf("the flood folded a family; admission should have kept it out of the "+
			"table entirely: %v", sortedKeys(models))
	}
	if !models["m1"] {
		t.Errorf("the configured model lost its series to the flood: %v", sortedKeys(models))
	}

	// The reload. A model and an alias are added, and the table must have room
	// for both — this is the assertion the pre-fix build could not satisfy at
	// any cap, because the flood had already spent it.
	cfg, err := config.LoadBytes([]byte(metricsReloadYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Storage.SQLite.Path = a.Config().Storage.SQLite.Path
	if err := a.Reload(cfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	observe("m2", 200)
	observe("fast", 200) // the alias, which GET /v1/models publishes

	models = scrapedModelLabels(t, a)
	for _, want := range []string{"m1", "m2", "fast"} {
		if !models[want] {
			t.Errorf("%q has no series of its own after the reload that configured it: %v",
				want, sortedKeys(models))
		}
	}
}

// TestPrefixRatioGateFollowsAReload is the second fact [App.applyMetricsConfig]
// carries, and it was stale for the same reason: copied in at assembly, into a
// registry a reload does not rebuild.
//
// routing.prefix hot-reloads like every other section (§4.1) — the dispatcher's
// prefixOn is swapped on every reload — so turning cache-affinity routing off
// left dorang_prefix_hit_ratio published against a table that had stopped being
// consulted. A ratio of 0.0 from a gateway with no cache is rule 3 of the
// package comment in its exact original form.
func TestPrefixRatioGateFollowsAReload(t *testing.T) {
	a := newMetricsApp(t)
	a.recordMetrics(&server.Event{
		Model: "m1", Route: "chat_completions", Status: 200,
		DurationNS: int64(time.Millisecond),
		Result:     server.Result{Provider: "p1", Credential: "c1", Deployment: "d1"},
	})
	// The TYPE line, not the bare name: dorang_prefix_routed_total's HELP text
	// names the ratio as its denominator, so a substring match on the name alone
	// passes whether or not the family is published.
	const ratioHeader = "# TYPE dorang_prefix_hit_ratio"
	if !strings.Contains(string(a.Metrics.Metrics(nil)), ratioHeader) {
		t.Fatal("cache-affinity routing is on by default and the ratio is absent")
	}

	cfg, err := config.LoadBytes([]byte(metricsReloadYAML)) // prefix: {enabled: false}
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Storage.SQLite.Path = a.Config().Storage.SQLite.Path
	if err := a.Reload(cfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if strings.Contains(string(a.Metrics.Metrics(nil)), ratioHeader) {
		t.Error("the hit ratio is still published after a reload turned cache-affinity " +
			"routing off; it now reports a table nothing consults")
	}
}

// scrapedModelLabels is the set of `model` label values on the assembled
// registry's per-model duration histogram.
func scrapedModelLabels(t *testing.T, a *App) map[string]bool {
	t.Helper()
	fams, err := metrics.Parse(a.Metrics.Metrics(nil))
	if err != nil {
		t.Fatalf("the assembled scrape does not parse: %v", err)
	}
	out := map[string]bool{}
	for _, f := range fams {
		if f.Name != "dorang_request_duration_seconds" {
			continue
		}
		for _, s := range f.Samples {
			out[s.Label("model")] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestAssembledScrapeIsValid builds a whole gateway and requires GET /metrics
// to parse and to satisfy the naming and typing rules.
//
// The unit tests in internal/metrics drive each collector against a fake. This
// is the one that would catch a collector wired to the wrong subsystem, or a
// family that two of them both claim — which is a duplicate TYPE line and
// therefore a scrape Prometheus rejects outright.
func TestAssembledScrapeIsValid(t *testing.T) {
	a := newMetricsApp(t)

	// One real request through the whole surface, so the §12.3 request families
	// have something in them.
	w := httptest.NewRecorder()
	a.Server.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/health answered %d", w.Code)
	}

	w = httptest.NewRecorder()
	a.Server.ServeHTTP(w, adminRequest(httptest.NewRequest(http.MethodGet, "/metrics", nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics answered %d", w.Code)
	}
	body := w.Body.Bytes()

	if _, err := metrics.Parse(body); err != nil {
		t.Fatalf("the assembled scrape does not parse: %v\n%s", err, body)
	}
	for _, e := range metrics.Validate(body) {
		t.Errorf("validation: %v", e)
	}

	// The families that only exist once everything is wired together.
	for _, want := range []string{
		"dorang_build_info",
		"dorang_requests_total",
		"dorang_request_duration_seconds_bucket",
		"dorang_capacity_grants_total",
		"dorang_deployment_health",
		"dorang_meter_recorded_total",
		"dorang_auth_cache_hits_total",
		"dorang_store_pool_open_connections",
		"dorang_ledger_block_size",
		"dorang_coordination_max_overshoot",
		"dorang_ready 1",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("%s is missing from the assembled scrape", want)
		}
	}

	// The built-in block must not also be rendering, or dorang_requests_total
	// carries two TYPE lines and the scrape is rejected.
	if got := strings.Count(string(body), "# TYPE dorang_requests_total"); got != 1 {
		t.Errorf("%d TYPE lines for dorang_requests_total", got)
	}
}

// TestAssembledCountersAgreeWithTheServer is the agreement check at assembly
// scale: the labelled family and the surface's own counter must describe the
// same traffic. A metric that has drifted from the thing it reports is worse
// than no metric, because it is a number someone will act on.
func TestAssembledCountersAgreeWithTheServer(t *testing.T) {
	a := newMetricsApp(t)

	const n = 17
	for i := 0; i < n; i++ {
		w := httptest.NewRecorder()
		a.Server.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/readiness", nil))
	}

	fams, err := metrics.Parse(a.Metrics.Metrics(nil))
	if err != nil {
		t.Fatal(err)
	}
	var scraped float64
	for _, f := range fams {
		if f.Name != "dorang_requests_total" {
			continue
		}
		for _, s := range f.Samples {
			scraped += s.Value
		}
	}
	if st := a.Server.Stats(); uint64(scraped) != st.Requests {
		t.Errorf("dorang_requests_total sums to %v, the server counted %d",
			scraped, st.Requests)
	}
}

// TestScrapeDoesNotBlockAReload keeps the endpoint off the request path's
// critical section. internal/server proves the request path takes no
// configuration lock; this proves the scrape does not either, from the other
// direction.
func TestScrapeDoesNotBlockAReload(t *testing.T) {
	a := newMetricsApp(t)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			a.Metrics.Metrics(nil)
		}
		close(done)
	}()
	for i := 0; i < 20; i++ {
		if err := a.Reload(a.Config()); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	<-done
}

func newMetricsApp(t *testing.T) *App {
	t.Helper()
	isolateState(t)
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	cfg, err := config.LoadBytes([]byte(metricsYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Storage.SQLite.Path = filepath.Join(t.TempDir(), "dorang.db")
	t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
	t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)

	a, err := New(context.Background(), Options{Config: cfg})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if a.Metrics == nil {
		t.Fatal("the assembled app has no metrics registry")
	}
	return a
}
