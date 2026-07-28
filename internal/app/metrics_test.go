package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/metrics"
)

// metricsYAML is a minimal gateway with a store on disk, so the ledger, the
// authenticator and the connection pool are all real.
const metricsYAML = `
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_ref: "vault:kv/p1#key"}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`

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
	a.Server.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
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
	cfg, err := config.LoadBytes([]byte(metricsYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Storage.SQLite.Path = filepath.Join(t.TempDir(), "dorang.db")
	t.Setenv(cfg.Server.KeyPepperEnv, testPepper)

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
