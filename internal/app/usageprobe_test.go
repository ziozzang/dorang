package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/quota"
)

// probeYAML is a gateway whose one provider publishes its own quota, with a
// tpm rule for the reported figure to gate and an allowance declaring how large
// the provider's window is.
//
// The rule matters as much as the probe: quota is keyed by (window, metric), so
// a provider figure filed under a key no rule uses is inert. `tpm: 1000`
// becomes Rolling(1m)/tokens_total, which is the key the allowance below files
// z.ai's TOKENS_LIMIT under.
const probeYAML = `
version: 1
providers:
  - name: p1
    kind: openai
    base_url: "https://example.invalid"
    usage_probe:
      enabled: true
      fetcher: zai
      interval: 60s
      allowances:
        - {label: "tokens_limit:1m", window: 1m, metric: tokens_total, limit: 1000}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - provider: p1
        upstream_model: m1-upstream
        credentials: [c1]
        limits:
          - {metric: tpm, value: 1000}
`

// zaiPayload is one TOKENS_LIMIT window: `percent` consumed of a one-minute
// allowance, resetting at `reset`.
func zaiPayload(percent int, reset time.Time) string {
	b, _ := json.Marshal(map[string]any{
		"code":    200,
		"success": true,
		"data": map[string]any{
			"limits": []map[string]any{{
				"type":          "TOKENS_LIMIT",
				"percentage":    percent,
				"unit":          5, // minutes
				"number":        1,
				"nextResetTime": reset.UnixMilli(),
			}},
		},
	})
	return string(b)
}

// fakeZAI intercepts the prober's requests to z.ai's documented host and counts
// them. Everything else is refused loudly rather than 404'd, so a request going
// somewhere unexpected fails the test instead of looking like a provider error.
type fakeZAI struct {
	calls  atomic.Int64
	body   atomic.Pointer[string]
	status atomic.Int64
	// token records the bearer the prober presented, which is the assertion
	// that §11.2b's resolution happened at probe time.
	token atomic.Pointer[string]
}

func (f *fakeZAI) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.z.ai" {
		return nil, fmt.Errorf("unexpected probe host %q", r.URL.Host)
	}
	f.calls.Add(1)
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.token.Store(&auth)
	status := int(f.status.Load())
	if status == 0 {
		status = http.StatusOK
	}
	body := ""
	if p := f.body.Load(); p != nil {
		body = *p
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

func (f *fakeZAI) serve(body string) { f.body.Store(&body) }

// probeClock is a movable clock. The prober rate-limits itself — a second read
// of one credential inside `usage_probe.interval` replays the last snapshot
// rather than spending a request — so a test that wants a SECOND reading has to
// move time rather than sleep through it. That floor is rule 6 of internal/probe
// and is not a detail to route around: polling an account-status endpoint hard
// enough to get the account limited is precisely the failure the probe exists to
// prevent.
type probeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newProbeClock() *probeClock {
	// A real instant, not an epoch: reset instants are compared against wall
	// time in several places and a clock decades out would exercise those
	// rather than the probe.
	return &probeClock{t: time.Now().UTC().Truncate(time.Second)}
}

func (c *probeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *probeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newProbeApp assembles a gateway whose HTTP client answers z.ai from f.
func newProbeApp(t *testing.T, yaml string, f *fakeZAI, clk *probeClock) *App {
	t.Helper()
	isolateState(t)
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Storage.SQLite.Path = filepath.Join(t.TempDir(), "dorang.db")
	t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
	t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)

	a, err := New(context.Background(), Options{
		Config:   cfg,
		Upstream: &http.Client{Transport: f},
		Now:      clk.now,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

// TestProviderReportedQuotaReachesTheMeter is DESIGN §6.2, wired.
//
// internal/probe had no importer anywhere in the tree and
// `providers[].usage_probe` was on internal/config's knownUnwired ledger, so
// quota was only ever dorang's own view of what dorang itself had sent. §6.2
// exists because that view is incomplete by construction: a key shared with
// another tool, or a plan consumed by something outside the gateway, is spend
// the local counter cannot see, and a credential reads as having room while the
// account is exhausted.
//
// The assertion is on an observable the app package cannot produce by itself —
// the credential's own meter refusing a request on a figure NO LOCAL TRAFFIC
// produced. Nothing in this test sends a request through the gateway; every
// token the meter counts came from the provider.
func TestProviderReportedQuotaReachesTheMeter(t *testing.T) {
	f := &fakeZAI{}
	clk := newProbeClock()
	// 90% of a 1,000-token minute is 900, against a limit of 1,000. Under the
	// limit, so the credential still serves.
	f.serve(zaiPayload(90, clk.now().Add(30*time.Second)))
	a := newProbeApp(t, probeYAML, f, clk)

	m := a.quota.meter("c1")
	if m == nil {
		t.Fatal("credential c1 has no quota meter, so a probe would have nothing to gate")
	}
	// Before any poll, local metering is all there is and nothing has been sent.
	if d := m.Check(a.now()); !d.Allow {
		t.Fatalf("the credential is refused before any traffic or any poll: %+v", d)
	}

	a.pollProbes(context.Background())
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("the probe made %d reads, want exactly 1", n)
	}
	if tok := f.token.Load(); tok == nil || *tok != testUpstreamKey {
		t.Fatalf("the prober presented %q, want the credential's own secret", derefOr(tok))
	}
	rule := quota.Rule{Window: quota.Rolling(time.Minute), Metric: quota.MetricTokensTotal, Limit: 1000}
	if used := m.Used(a.now(), rule); used != 900 {
		t.Fatalf("effective used = %d, want the provider's 900 (90%% of the declared "+
			"1000-token allowance). Zero means the percentage never became a figure in "+
			"the metric's units — see usage_probe.allowances", used)
	}
	if d := m.Check(a.now()); !d.Allow {
		t.Fatalf("900 of 1000 refused the credential: %+v", d)
	}

	// The provider now says the window is spent. dorang has still sent nothing
	// through this credential, so this figure is entirely out-of-band usage —
	// which is precisely the traffic §6.2 exists to make visible.
	//
	// Time moves past `usage_probe.interval` first: inside it the prober
	// replays its last snapshot rather than spending a request, which is rule 6
	// and is asserted below by the read count.
	clk.advance(2 * time.Minute)
	reset := clk.now().Add(45 * time.Second)
	f.serve(zaiPayload(100, reset))
	a.pollProbes(context.Background())
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("the probe has made %d reads, want 2: the second poll did not reach "+
			"the provider", n)
	}

	d := m.Check(a.now())
	if d.Allow {
		t.Fatal("the provider reports the window exhausted and the credential still " +
			"serves: local metering alone cannot see usage that did not go through dorang")
	}
	if !d.FromProvider {
		t.Errorf("the refusal is not attributed to the provider's figure: %+v", d)
	}
	// §7.5a(c)'s load-bearing field. A rolling window has no reset instant of
	// its own; the provider's is the only one there is, and without it the
	// cooldown is dorang's own guess at when to come back.
	if !d.ResetAt.Truncate(time.Second).Equal(reset.UTC().Truncate(time.Second)) {
		t.Errorf("cooldown until %v, want the provider's reset at %v", d.ResetAt, reset.UTC())
	}
}

// TestAFailedProbeNeverDisablesACredential is §6.2's first rule, and it is the
// one whose absence turns a provider outage into an outage here.
func TestAFailedProbeNeverDisablesACredential(t *testing.T) {
	f := &fakeZAI{}
	clk := newProbeClock()
	f.serve(zaiPayload(10, clk.now().Add(30*time.Second)))
	a := newProbeApp(t, probeYAML, f, clk)

	a.pollProbes(context.Background())
	m := a.quota.meter("c1")
	if d := m.Check(a.now()); !d.Allow {
		t.Fatalf("a healthy probe refused the credential: %+v", d)
	}

	// The provider goes dark. A failed read is not an exhausted quota: the last
	// good snapshot is retained and its staleness grows, and the credential
	// keeps serving.
	f.status.Store(http.StatusInternalServerError)
	f.serve(`{"error":"upstream is down"}`)
	for i := 0; i < 3; i++ {
		clk.advance(2 * time.Minute)
		a.pollProbes(context.Background())
	}
	if d := m.Check(a.now()); !d.Allow {
		t.Fatalf("a failing probe disabled the credential: %+v", d)
	}
	rule := quota.Rule{Window: quota.Rolling(time.Minute), Metric: quota.MetricTokensTotal, Limit: 1000}
	if used := m.Used(a.now(), rule); used != 100 {
		t.Errorf("effective used = %d after the provider went dark, want the last good "+
			"figure of 100 — a failed read must not erase what was read", used)
	}
}

// TestAFetcherWithNoProberIsRefusedByName. `fetcher:` is free text in the
// schema and internal/config cannot check it without importing a sibling
// package, so a wrong one used to be a provider that silently never reported —
// the exact state this wiring exists to end. CONFIG's own worked example said
// `fetcher: openai`, for which there is deliberately no prober.
func TestAFetcherWithNoProberIsRefusedByName(t *testing.T) {
	cases := []struct{ name, fetcher, want string }{
		{"examined and publishes nothing", "openai", "no quota endpoint"},
		{"never looked at", "not-a-provider", "not a fetcher this build has"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolateState(t)
			t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
			cfg, err := config.LoadBytes([]byte(
				strings.Replace(probeYAML, "fetcher: zai", "fetcher: "+c.fetcher, 1)))
			if err != nil {
				t.Fatalf("config: %v", err)
			}
			cfg.Storage.SQLite.Path = filepath.Join(t.TempDir(), "dorang.db")
			t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
			t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)

			a, err := New(context.Background(), Options{Config: cfg})
			if a != nil {
				_ = a.Close(context.Background())
			}
			if err == nil {
				t.Fatal("a fetcher with no prober started the gateway; it would report nothing, forever")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say why: %v", err)
			}
			// A refusal that does not name the working values is a wall rather
			// than a diagnosis.
			if !strings.Contains(err.Error(), "zai") {
				t.Errorf("the refusal does not list the fetchers that exist: %v", err)
			}
		})
	}
}

// TestReloadRebuildsTheProbeTrackers is why the probe set is rebuilt rather
// than carried.
//
// A tracker differences the provider's figure against the meter it was built
// with. A reload builds new meters; a carried tracker would keep differencing
// the old one, whose cumulative is frozen, which makes the provider's figure
// authoritative — the revision-1 behaviour §6.2 was corrected away from.
func TestReloadRebuildsTheProbeTrackers(t *testing.T) {
	f := &fakeZAI{}
	clk := newProbeClock()
	f.serve(zaiPayload(50, clk.now().Add(30*time.Second)))
	a := newProbeApp(t, probeYAML, f, clk)

	a.pollProbes(context.Background())
	rule := quota.Rule{Window: quota.Rolling(time.Minute), Metric: quota.MetricTokensTotal, Limit: 1000}
	if used := a.quota.meter("c1").Used(a.now(), rule); used != 500 {
		t.Fatalf("before the reload, effective used = %d, want 500", used)
	}

	cfg, err := config.LoadBytes([]byte(probeYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.SQLite.Path = a.opts.Config.Storage.SQLite.Path
	if err := a.Reload(cfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	m := a.quota.meter("c1")
	if m == nil {
		t.Fatal("the reload left the credential with no meter")
	}
	// The new meter starts with no adopted figure at all, which is the correct
	// state: the provider has not been asked since it was built.
	if used := m.Used(a.now(), rule); used != 0 {
		t.Fatalf("a freshly built meter already reports %d used", used)
	}
	// A fresh prober was built with the set, so its own read floor starts over
	// and this poll reaches the provider without moving time.
	a.pollProbes(context.Background())
	if used := m.Used(a.now(), rule); used != 500 {
		t.Fatalf("after the reload, effective used = %d, want the provider's 500. Zero "+
			"means the tracker was not re-attached to the meter the reload built", used)
	}
}

// TestProbeAllowanceVocabularyMatchesQuota bounds the one copy this wiring
// forced.
//
// internal/config imports no sibling package (§1), so it checks
// `usage_probe.allowances[].window` and `.metric` against its own copy of
// quota's vocabulary. Two lists that must agree is the defect shape this
// codebase has been bitten by repeatedly; this is the assertion that keeps them
// agreeing, and it runs in the package that can see both.
func TestProbeAllowanceVocabularyMatchesQuota(t *testing.T) {
	for _, m := range []string{"cost_usd", "tokens_total", "tokens_input", "tokens_output", "requests"} {
		if _, err := quota.ParseMetric(m); err != nil {
			t.Errorf("internal/config accepts metric %q and quota does not: %v", m, err)
		}
	}
	for _, w := range []string{"daily", "weekly", "monthly", "5h", "1m"} {
		if _, err := quota.ParseWindow(w); err != nil {
			t.Errorf("internal/config accepts window %q and quota does not: %v", w, err)
		}
	}
	// The floor as well as the vocabulary. quota's counter is a minute-bucket
	// ring, and a validator that accepted a shorter window would turn a
	// configuration mistake into a start-up crash instead of a field error.
	if _, err := quota.ParseWindow("30s"); err == nil {
		t.Error("quota now accepts a sub-minute window; internal/config still refuses one")
	}

	// And the reverse direction, so a value quota gained does not sit
	// unreachable behind a stale validator.
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	for _, m := range []string{"cost_usd", "tokens_total", "tokens_input", "tokens_output", "requests"} {
		yaml := strings.Replace(probeYAML, "metric: tokens_total", "metric: "+m, 1)
		if _, err := config.LoadBytes([]byte(yaml)); err != nil {
			t.Errorf("quota parses metric %q and internal/config refuses it: %v", m, err)
		}
	}
	for _, w := range []string{"daily", "weekly", "monthly", "5h", "1m"} {
		yaml := strings.Replace(probeYAML, "window: 1m", "window: "+w, 1)
		if _, err := config.LoadBytes([]byte(yaml)); err != nil {
			t.Errorf("quota parses window %q and internal/config refuses it: %v", w, err)
		}
	}
	if _, err := config.LoadBytes([]byte(
		strings.Replace(probeYAML, "window: 1m", "window: 30s", 1))); err == nil {
		t.Error("internal/config accepted a sub-minute window that quota refuses")
	}
}

func derefOr(s *string) string {
	if s == nil {
		return "<nothing>"
	}
	return *s
}
